// Package statestore keeps one app's state document: a file at home, a row in Postgres in the
// cluster (mj41-linode docs/pg-state.md). The apps keep their state in memory and save the whole
// document; this package only says where it goes.
//
// Postgres: one table per database, a row per app, the last saves kept for a rollback, and a
// session advisory lock held while the app runs, so a second copy waits instead of writing over
// the first. When the connection is lost, so is the lock: Lost() closes, and the app must stop.
package statestore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Store keeps one app's state document.
type Store interface {
	// Load returns the saved document, or nil when there is none yet.
	Load(ctx context.Context) ([]byte, error)
	// Save replaces the document (and, in Postgres, keeps the previous ones).
	Save(ctx context.Context, doc []byte) error
	// Lost is closed when the store can no longer be trusted to be this app's alone (Postgres:
	// the connection, and with it the lock, is gone). Never closed for a file.
	Lost() <-chan struct{}
	Close() error
	// Where says where the state is, for logs (no password).
	Where() string
}

// IsDatabase: target is a Postgres URL rather than a file path.
func IsDatabase(target string) bool {
	return strings.HasPrefix(target, "postgres://") || strings.HasPrefix(target, "postgresql://")
}

// --- a file --------------------------------------------------------------------------------------

// File is the state in one JSON file, written atomically and readable only by this user (it holds
// session ids, which are credentials).
type File struct{ Path string }

func (f *File) Load(context.Context) ([]byte, error) {
	b, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

func (f *File) Save(_ context.Context, doc []byte) error {
	dir := filepath.Dir(f.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(doc); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), f.Path)
}

func (f *File) Lost() <-chan struct{} { return nil }
func (f *File) Close() error          { return nil }
func (f *File) Where() string         { return f.Path }

// --- Postgres -----------------------------------------------------------------------------------

// Keep is how many earlier saves Postgres keeps per app (state_history).
const Keep = 50

// PingEvery is how often the connection (and so the lock) is checked.
var PingEvery = 10 * time.Second

const schema = `
CREATE TABLE IF NOT EXISTS state (
    app      text PRIMARY KEY,
    doc      jsonb NOT NULL,
    saved_at timestamptz NOT NULL,
    version  bigint NOT NULL
);
CREATE TABLE IF NOT EXISTS state_history (
    app      text NOT NULL,
    version  bigint NOT NULL,
    doc      jsonb NOT NULL,
    saved_at timestamptz NOT NULL,
    PRIMARY KEY (app, version)
);`

// Postgres is the state as a row of the database's state table. One connection, held for the
// app's life: the advisory lock belongs to it.
type Postgres struct {
	app   string
	where string
	mu    sync.Mutex // one statement at a time on the connection
	conn  *pgx.Conn
	lost  chan struct{}
	once  sync.Once
	stop  chan struct{}
	every time.Duration // PingEvery when it opened
}

// OpenPostgres connects (the password may come from PGPASSWORD or PGPASSFILE rather than the
// URL), makes the tables, and waits for the app's lock: while another copy runs, it waits here.
func OpenPostgres(ctx context.Context, url, app string) (*Postgres, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("statestore: %w", redact(err, url))
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("statestore: connect %s@%s/%s: %w", cfg.User, cfg.Host, cfg.Database, redact(err, url))
	}
	p := &Postgres{app: app, conn: conn, lost: make(chan struct{}), stop: make(chan struct{}), every: PingEvery,
		where: fmt.Sprintf("postgres %s@%s/%s (app %s)", cfg.User, cfg.Host, cfg.Database, app)}
	if _, err := conn.Exec(ctx, schema); err != nil {
		conn.Close(ctx)
		return nil, fmt.Errorf("statestore: schema: %w", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext('statestore:' || $1))", app); err != nil {
		conn.Close(ctx)
		return nil, fmt.Errorf("statestore: lock: %w", err)
	}
	go p.watch()
	return p, nil
}

// redact keeps a password that was in the URL out of an error.
func redact(err error, url string) error {
	msg := err.Error()
	if i := strings.Index(url, "://"); i >= 0 {
		rest := url[i+3:]
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			if colon := strings.Index(rest[:at], ":"); colon >= 0 {
				if pw := rest[colon+1 : at]; pw != "" {
					msg = strings.ReplaceAll(msg, pw, "…")
				}
			}
		}
	}
	return errors.New(msg)
}

// watch pings the connection; a failed ping means the lock may be gone: Lost.
func (p *Postgres) watch() {
	t := time.NewTicker(p.every)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			p.mu.Lock()
			err := p.conn.Ping(ctx)
			p.mu.Unlock()
			cancel()
			if err != nil {
				p.once.Do(func() { close(p.lost) })
				return
			}
		}
	}
}

func (p *Postgres) Load(ctx context.Context) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var doc []byte
	err := p.conn.QueryRow(ctx, "SELECT doc::text FROM state WHERE app = $1", p.app).Scan(&doc)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return doc, err
}

func (p *Postgres) Save(ctx context.Context, doc []byte) error {
	select {
	case <-p.lost:
		return errors.New("statestore: the connection (and the lock) is lost")
	default:
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	tx, err := p.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var version int64
	err = tx.QueryRow(ctx, `INSERT INTO state (app, doc, saved_at, version) VALUES ($1, $2::text::jsonb, now(), 1)
		ON CONFLICT (app) DO UPDATE SET doc = EXCLUDED.doc, saved_at = EXCLUDED.saved_at, version = state.version + 1
		RETURNING version`, p.app, string(doc)).Scan(&version)
	if err != nil {
		return fmt.Errorf("statestore: save: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO state_history (app, version, doc, saved_at) VALUES ($1, $2, $3::text::jsonb, now())`,
		p.app, version, string(doc)); err != nil {
		return fmt.Errorf("statestore: history: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM state_history WHERE app = $1 AND version <= $2`, p.app, version-Keep); err != nil {
		return fmt.Errorf("statestore: history: %w", err)
	}
	return tx.Commit(ctx)
}

func (p *Postgres) Lost() <-chan struct{} { return p.lost }

// pid is the server process of the connection (tests).
func (p *Postgres) pid() uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn.PgConn().PID()
}
func (p *Postgres) Where() string { return p.where }

// Close releases the lock (with the connection).
func (p *Postgres) Close() error {
	select {
	case <-p.stop:
		return nil
	default:
		close(p.stop)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn.Close(ctx)
}

// Open is a file store for a path, or a Postgres store for a postgres:// URL. importFile, for a
// database that has no state for app yet: that file's state is saved into it first (the move
// from a volume), and left in place.
func Open(ctx context.Context, target, app, importFile string) (Store, error) {
	if !IsDatabase(target) {
		return &File{Path: target}, nil
	}
	p, err := OpenPostgres(ctx, target, app)
	if err != nil {
		return nil, err
	}
	if importFile != "" {
		have, err := p.Load(ctx)
		if err != nil {
			p.Close()
			return nil, err
		}
		if have == nil {
			b, err := os.ReadFile(importFile)
			switch {
			case errors.Is(err, os.ErrNotExist): // nothing to move
			case err != nil:
				p.Close()
				return nil, fmt.Errorf("statestore: import: %w", err)
			default:
				if err := p.Save(ctx, b); err != nil {
					p.Close()
					return nil, fmt.Errorf("statestore: import %s: %w", importFile, err)
				}
			}
		}
	}
	return p, nil
}
