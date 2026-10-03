# From prototype to v1

**Status:** plan, 2026-10-02.

## Where it stands

`stackchan-server` works and is used: one robot, every day, on a home network since
2026-09-29, with phones and laptops pairing by QR; the public instance at
`chan.w42.eu` runs the same code. It is a **prototype** in these ways:

| Area | Today | Why it matters |
|---|---|---|
| Robot identity | one shared token for the owner's robots, compiled into the firmware; other people's robots each get their own invite token | reading one of the owner's robots' flash gives the token for all of them |
| Permissions | a paired browser may use every command and see all data | no guests, no "camera only for parents", no time limits |
| Network | plain `ws://` and `http://` on the LAN; TLS only behind a proxy | anyone on the network can listen |
| State | in memory, plus a JSON snapshot on restart; one instance only | a crash or a deploy loses live state |
| Audio | raw PCM, about 48 KB/s | too heavy for mobile networks |
| Quality checks | tests run on the developer's machine; CI only builds release images | the v0.2.0 image build broke without anyone noticing before the tag |
| Abuse | join requests, failed robot logins and wrong pairing codes are limited per address; invited robots have send budgets; browsers' streams are not limited yet | a public instance needs more |

## Stages

Each stage leaves a release that is better than the one before, and has a clear
**done when**.

### 1. Solid on a home network

- **CI on every push:** gofmt, vet, race tests, and a container build, so a broken image
  is found before a tag.
- **Fuzz tests** for everything robots and browsers send: `wire.Parse`, frame bodies,
  binary messages (media, files), with size limits checked.
- **A browser security pass:** the session cookie's flags, origin checks on every
  WebSocket and state-changing request, request size limits, and a content security
  policy for the dashboard.
- **The version everywhere:** in `/api/info`, on the dashboard, and in the robot's
  `Register`, so a mismatch between firmware and server is visible.
- **Restart behaviour tested:** a server restart, a robot that reconnects with backoff,
  and a browser that resumes its event stream, all covered by tests.

**Done when:** someone else can follow the robot setup guide on their own network and it
works, CI is green, and the fuzzers have run without findings.

### 2. Each robot its own identity

**First step, done 2026-10-03:** per-robot invite tokens (`-robot-tokens-file`,
`stackchan-server invite`): each invited robot has its own token, bound to its id, revoked
by deleting one line; the server keeps only the tokens' hashes. They are still bearer
tokens in the firmware, so the steps below remain.

- An **owner key** and a small tool that signs robot certificates and configuration.
- A **device key per robot** (in its storage first, in the ESP32-S3's secure key
  peripheral later), proved with a challenge instead of the shared token.
- **Revoking one robot** without touching the others.

**Done when:** reading one robot's flash compromises only that robot, and the shared
token is gone.

### 3. Permissions per person, robot, capability and time

- Permission entries: who, which robot, which commands or data (camera, microphone,
  movement…), and for how long; enforced by the server and checked again by the robot.
- **Guests:** a QR scan on the robot gives a small set of rights for an hour.
- **An audit view:** who used which robot, which commands, and who watched the camera, in
  plain words.

**Done when:** a parent can let a babysitter see one robot's camera for an evening, and
nothing else, and can read afterwards what was used.

### 4. Safe on the internet

- **End-to-end encryption through the public instance:** `chan.w42.eu` becomes a
  rendezvous and relay that cannot read robot or browser traffic; TLS ends on the
  owner's own server.
- **Compressed audio** (Opus) for the microphone and the speaker.
- **Abuse limits:** connection and pairing rate limits per address, size and frequency
  limits on every message, and short log retention on the public instance. *Started
  2026-10-03:* failed robot logins and wrong pairing codes are limited per address (with
  `-trusted-proxies` so a forged `X-Forwarded-For` cannot dodge it), and invited robots
  have message and byte budgets. chan.w42.eu sees real client addresses since
  2026-10-03 (the PROXY protocol from its load balancer), so all of them apply there.
- **An external security review** of the protocol and the server.

**Done when:** the public instance can be offered to people we do not know without
reading or storing their traffic, and the review's findings are fixed.

### 5. Version 1.0

- **Wire protocol v2** frozen, with the compatibility rule written down: which firmware
  versions a server accepts, and for how long.
- **Releases** with signed container images and a software bill of materials; the robot
  firmware built in a pinned container, reproducibly, so anyone can compare hashes.
- **Documentation** for owners (setup, everyday use, privacy) and for app developers (the
  protocol, the command catalog, an example app).

**Done when:** a 1.0 tag, and an upgrade from it to the next minor version works without
re-pairing.

## Not planned

- Accounts on our servers: owners keep their own keys; the public instance does not need
  to know who they are.
- Recording by default: media streams only while someone watches or listens, as today.
