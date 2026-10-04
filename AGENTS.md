# stackchan-server

Go relay server for Stackchan Embody Mode. See [readme.md](readme.md) for how to run it and for the protocol.

- Workspace notes for the whole Stackchan effort (firmware, build and flash, design) are in `../stackchan-mj/AGENTS.md`. Read them first.
- Go only. Use the standard library, plus gorilla/websocket, go-oidc and x/oauth2 (sign-in) and go.bug.st/serial (`stackchan-usb`).
- Protocol change: the spec in `home-w42-eu` `docs/wire-protocol.md` first, then `wire/wire.go`, then the Commands table in `readme.md` (frames and binary types are only linked from there).
- The firmware must be able to parse frames with cJSON: flat JSON objects, one frame per message from the server to the robot.
- Before committing, run `gofmt -l .`, `go vet ./...` and `go test -race ./...`.
- Test end to end without hardware with `go run ./cmd/fake-robot`.
