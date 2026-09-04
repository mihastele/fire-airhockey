# Fire Air Hockey

Real-time multiplayer air hockey. Pure HTML + CSS + JavaScript frontend, Go
backend (standard library only — no dependencies). Open the game in two
browser windows and play against each other.

- **Nickname on entry**, remembered in the browser.
- **Public tables**: create one, anyone in the lobby can join.
- **Private tables**: hidden from the lobby, joinable only via invite link.
  Every table — public or private — has a shareable link (`/r/ABC123`).
- **Mirrored view**: you always play from the bottom; your opponent sees the
  other side. Subtle depth scaling gives a light perspective feel while the
  physics stay on a fair top-down plane.
- **Practice vs CPU** if no opponent is around. Spectators can watch.
- Server-authoritative physics at 60 Hz, snapshots at 30 Hz with client-side
  puck interpolation. First to 7 wins, rematch included.

## Run it

You need Go 1.24+ (no other dependencies — the WebSocket layer is stdlib-only).

```sh
go run .                 # serves on http://localhost:8080
go run . -addr :9090     # custom address
```

Then open the printed address in **two browser windows** (or send one player
the invite link), pick nicknames, and share a table.

Build a single self-contained binary (frontend is embedded):

```sh
go build -o airhockey .
./airhockey
```

## How it works

```
browser  <--websocket JSON-->  Go hub  -->  room loop (60 Hz physics)
```

- `main.go` — HTTP server, embedded `static/` frontend, `/api/rooms` lobby API.
- `hub.go` — clients, rooms, matchmaking messages, snapshots.
- `game.go` — table constants, puck/paddle physics, bot AI, match state machine.
- `ws.go` — minimal RFC 6455 WebSocket framing (stdlib only).
- `static/` — dependency-free frontend (`index.html`, `style.css`, `app.js`).

Coordinates are canonical table units (x 0–100, y 0–200; seat 0 defends
y=200). Each client renders itself at the bottom: seat 1 mirrors y locally,
and its paddle targets are un-mirrored by the server.

## Tests

```sh
go test ./...
```

Covers goal/wall/paddle physics, win detection, seat clamping, bot eviction
and WebSocket frame encoding. `go vet` is clean.
