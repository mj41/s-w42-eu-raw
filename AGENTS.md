# stackchan-server

Go relay server for Stackchan Embody Mode. See [readme.md](readme.md) for how to run it and for the protocol.

- Workspace notes for the whole Stackchan effort (firmware, build and flash, design) are in `../stackchan-mj/AGENTS.md`. Read them first.
- Go only. Use the standard library, plus gorilla/websocket.
- The wire protocol's reference is `docs/wire-protocol.md` in the `home-w42-eu` repo. Change the spec there first, then `wire/wire.go` and this readme.
- The firmware must be able to parse frames with cJSON: flat JSON objects, one frame per message from the server to the robot.
- When the protocol changes, update the table in `readme.md` and `wire/wire.go` together.
- Before committing, run `gofmt -l .`, `go vet ./...` and `go test -race ./...`.
- Test end to end without hardware with `go run ./cmd/fake-robot`.
