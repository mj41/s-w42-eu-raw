# stackchan-server

Go relay server for Stack-chan Embody Mode. See [readme.md](readme.md) for how to run it and for the protocol.

- Workspace notes for the whole Stack-chan effort (firmware, build and flash, design, prior work) are in `~/work-stai/stackchan-mj/AGENTS.md`. Read them first.
- Go only. Use the standard library, plus gorilla/websocket (same as stai-yolovm).
- Keep the frame envelope and handshake compatible with yolovm-pilot (`~/work-stai/stai-yolovm/docs/yolovm-pilot/wire-protocol.md`).
- The firmware must be able to parse frames with cJSON: flat JSON objects, one frame per message from the server to the robot.
- When the protocol changes, update the table in `readme.md` and `internal/wire/wire.go` together.
- Before committing, run `gofmt -l .`, `go vet ./...` and `go test -race ./...`.
- Test end to end without hardware with `go run ./cmd/fake-robot`.
