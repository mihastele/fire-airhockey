// Hub: client registry, room registry, JSON message routing and the
// per-room simulation loops.
//
// Wire protocol (JSON text frames, field "t" discriminates):
//
//	client -> server:
//	  {"t":"hello","name":"Ada","room":"AB12CD"}  join lobby (or a room link)
//	  {"t":"create","title":"...","public":true,"cpu":false}
//	  {"t":"join","room":"AB12CD"}
//	  {"t":"leave"}
//	  {"t":"paddle","x":0..1,"y":0..1}   own-view coords, self at the bottom
//	  {"t":"rematch"}
//	  {"t":"addcpu"}
//
//	server -> client:
//	  {"t":"welcome","id":"...","name":"..."}
//	  {"t":"lobby","rooms":[...]}
//	  {"t":"room", ...}    room meta + your seat
//	  {"t":"snap", ...}    30 Hz canonical match snapshot
//	  {"t":"error","msg":"..."}
package main

import (
	"crypto/rand"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"
)

const botName = "CPU"

// Hub owns all clients and rooms.
type Hub struct {
	mu      sync.Mutex
	clients map[*Client]struct{}
	rooms   map[string]*Room
}

// Client is one WebSocket connection: either in the lobby (room == nil) or
// seated/spectating in a room.
type Client struct {
	hub  *Hub
	ws   *WSConn
	id   string
	name string

	mu   sync.Mutex // guards room/seat
	room *Room
	seat int // -2 lobby/unseated, -1 spectator, 0/1 player seat
}

const seatLobby = -2
const seatSpectator = -1

var clientSeq = struct {
	mu sync.Mutex
	n  int64
}{}

// NewHub builds an empty hub.
func NewHub() *Hub {
	return &Hub{clients: map[*Client]struct{}{}, rooms: map[string]*Room{}}
}

// ServeWS upgrades the HTTP request and pumps messages until disconnect.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	ws, err := HijackWS(w, r)
	if err != nil {
		return
	}
	log.Printf("ws connect from %s", r.RemoteAddr)
	h.serveConn(ws)
}

// serveConn runs the lifecycle of one accepted socket.
func (h *Hub) serveConn(ws *WSConn) {
	clientSeq.mu.Lock()
	clientSeq.n++
	n := clientSeq.n
	clientSeq.mu.Unlock()
	c := &Client{hub: h, ws: ws, seat: seatLobby}
	c.id = randomID(12, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")
	_ = n
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	defer h.disconnect(c)

	// Keep middleboxes happy; browsers answer pings on their own.
	pingStop := make(chan struct{})
	defer close(pingStop)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := ws.Ping(); err != nil {
					return
				}
			case <-pingStop:
				return
			}
		}
	}()

	for {
		raw, err := ws.ReadMessage()
		if err != nil {
			return
		}
		c.handle(raw)
	}
}

// disconnect frees the seat (forfeiting live matches) and refreshes everyone.
func (h *Hub) disconnect(c *Client) {
	c.mu.Lock()
	room, seat := c.room, c.seat
	c.room, c.seat = nil, seatLobby
	c.mu.Unlock()

	if room != nil && seat >= 0 {
		room.mu.Lock()
		name := ""
		if p := room.players[seat]; p != nil {
			name = p.Name
		}
		room.dropSeat(seat)
		room.mu.Unlock()
		log.Printf("client %q left room %s (seat %d name %q)", c.name, room.ID, seat, name)
		h.broadcastRoom(room)
	}
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	_ = c.ws.Close()
	h.broadcastLobby()
}

// ---- inbound messages ----

type inbound struct {
	T      string  `json:"t"`
	Name   string  `json:"name"`
	Room   string  `json:"room"`
	Title  string  `json:"title"`
	Public *bool   `json:"public"`
	CPU    bool    `json:"cpu"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
}

func (c *Client) handle(raw []byte) {
	var in inbound
	if err := json.Unmarshal(raw, &in); err != nil {
		return
	}
	h := c.hub
	switch in.T {
	case "hello":
		name := cleanName(in.Name)
		if name == "" {
			c.sendError("pick a nickname first")
			return
		}
		c.mu.Lock()
		c.name = name
		c.mu.Unlock()
		c.sendJSON(map[string]any{"t": "welcome", "id": c.id, "name": name})
		if in.Room != "" {
			c.joinRoom(in.Room)
		} else {
			h.broadcastLobbyTo(c)
		}
	case "create":
		name := c.currentName()
		if name == "" {
			c.sendError("pick a nickname first")
			return
		}
		public := true
		if in.Public != nil {
			public = *in.Public
		}
		title := cleanTitle(in.Title)
		if title == "" {
			title = name + "'s game"
		}
		if in.CPU {
			public = false // practice tables stay out of the public list
		}
		c.mu.Lock()
		seated := c.room != nil
		c.mu.Unlock()
		if seated {
			c.leaveRoom() // abandon the old table first: no ghost players
		}
		room := h.newRoom(title, public)
		seat := -1
		room.mu.Lock()
		seat = room.addPlayer(name)
		if in.CPU {
			room.addBot()
		}
		room.mu.Unlock()
		c.enterRoom(room, seat)
		h.broadcastLobby()
	case "join":
		c.joinRoom(in.Room)
	case "leave":
		c.leaveRoom()
	case "paddle":
		c.mu.Lock()
		room, seat := c.room, c.seat
		c.mu.Unlock()
		if room == nil || seat < 0 {
			return
		}
		x := clamp(in.X, 0, 1) * TableW
		var y float64
		if seat == 0 {
			y = clamp(in.Y, 0, 1) * TableH
		} else {
			y = (1 - clamp(in.Y, 0, 1)) * TableH
		}
		room.mu.Lock()
		room.setTarget(seat, x, y)
		room.mu.Unlock()
	case "rematch":
		c.mu.Lock()
		room, seat := c.room, c.seat
		c.mu.Unlock()
		if room == nil || seat < 0 {
			return
		}
		room.mu.Lock()
		if room.phase == PhaseOver && room.players[seat] != nil && !room.players[seat].Bot {
			room.rematch[seat] = true
			other := 1 - seat
			switch {
			case room.players[other] == nil:
				// Opponent is gone: reset to a fresh table awaiting the
				// next challenger instead of a dead rematch button.
				room.resetToWait()
			case room.players[other].Bot || room.rematch[other]:
				if room.bothPresent() {
					room.startCountdown()
				}
			}
		}
		room.mu.Unlock()
		h.broadcastRoom(room)
	case "addcpu":
		c.mu.Lock()
		room, seat := c.room, c.seat
		c.mu.Unlock()
		if room == nil || seat < 0 {
			return
		}
		room.mu.Lock()
		if room.players[1-seat] == nil {
			room.addBot()
		}
		room.mu.Unlock()
		h.broadcastRoom(room)
		h.broadcastLobby()
	}
}

func (c *Client) currentName() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.name
}

// cleanTitle trims a table name, collapses inner whitespace and caps length
// (in runes, so multi-byte names are not sliced mid-character). Reports ""
// for blank input so callers can fall back to a default.
func cleanTitle(s string) string {
	out := make([]rune, 0, 48)
	space := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if len(out) > 0 {
				space = true
			}
			continue
		}
		if len(out) >= 48 {
			break
		}
		if space {
			out = append(out, ' ')
			space = false
		}
		out = append(out, r)
	}
	return string(out)
}

func cleanName(s string) string {
	// Trim, collapse spaces, cap length. Keep it simple and safe for HTML
	// (the frontend also injects names via textContent).
	out := make([]rune, 0, 20)
	space := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' {
			if len(out) > 0 {
				space = true
			}
			continue
		}
		if len(out) >= 16 {
			break
		}
		if space {
			out = append(out, ' ')
			space = false
		}
		out = append(out, r)
	}
	return string(out)
}

// ---- room membership ----

func (h *Hub) newRoom(title string, public bool) *Room {
	for {
		id := randomID(6, "ABCDEFGHJKMNPQRSTUVWXYZ23456789")
		h.mu.Lock()
		if _, taken := h.rooms[id]; taken {
			h.mu.Unlock()
			continue
		}
		r := newRoom(id, title, public)
		h.rooms[id] = r
		h.mu.Unlock()
		go h.roomLoop(r)
		return r
	}
}

func (h *Hub) findRoom(id string) *Room {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rooms[id]
}

func (c *Client) joinRoom(id string) {
	h := c.hub
	room := h.findRoom(id)
	if room == nil {
		c.sendError("game not found — it may have ended")
		h.broadcastLobbyTo(c)
		return
	}
	name := c.currentName()
	if name == "" {
		c.sendError("pick a nickname first")
		return
	}
	c.mu.Lock()
	cur := c.room
	c.mu.Unlock()
	if cur != nil {
		if cur.ID == id {
			h.broadcastRoom(cur) // already here: just refresh state
			return
		}
		c.leaveRoom() // free the old seat first: no ghost players
	}
	room.mu.Lock()
	dup := room.hasHuman(name)
	seat := -1
	if !dup {
		seat = room.addPlayer(name)
	}
	room.mu.Unlock()
	if dup {
		// Same nickname already seated at this table (e.g. a second tab
		// sharing the stored nickname): joining would mean playing against
		// yourself, so refuse with a message that says how to proceed.
		c.sendError("that nickname is already at this table — change nicknames to join")
		return
	}
	if seat < 0 {
		c.enterRoom(room, seatSpectator)
	} else {
		c.enterRoom(room, seat)
	}
	h.broadcastLobby()
}

func (c *Client) enterRoom(room *Room, seat int) {
	c.mu.Lock()
	c.room, c.seat = room, seat
	c.mu.Unlock()
	c.hub.broadcastRoom(room)
}

func (c *Client) leaveRoom() {
	c.mu.Lock()
	room, seat := c.room, c.seat
	c.room, c.seat = nil, seatLobby
	c.mu.Unlock()
	if room != nil && seat >= 0 {
		room.mu.Lock()
		room.dropSeat(seat)
		room.mu.Unlock()
		c.hub.broadcastRoom(room)
	}
	c.hub.broadcastLobbyTo(c)
	c.hub.broadcastLobby()
}

// ---- loops and broadcasts ----

// roomLoop ticks the simulation and pushes snapshots until the room expires.
func (h *Hub) roomLoop(r *Room) {
	t := time.NewTicker(time.Second / TickHz)
	defer t.Stop()
	var snapTick int
	for range t.C {
		r.mu.Lock()
		out := r.tick(1.0 / TickHz)
		phase := r.phase
		empty := h.countRoomClients(r) == 0
		if empty {
			if r.emptySince.IsZero() {
				r.emptySince = time.Now()
			}
		} else {
			r.emptySince = time.Time{}
		}
		expired := empty && time.Since(r.emptySince) > EmptyRoomTTL
		r.mu.Unlock()
		if expired {
			h.mu.Lock()
			delete(h.rooms, r.ID)
			h.mu.Unlock()
			h.broadcastLobby()
			return
		}
		snapTick++
		if out {
			h.broadcastRoom(r)
			h.broadcastLobby()
		}
		// Live phases stream snapshots at ~30 Hz even when nothing
		// "changed": countdowns tick and goal pauses animate. Phase
		// transitions always set out, so they arrive immediately.
		if out || ((phase == PhasePlay || phase == PhaseGoal || phase == PhaseCount) && snapTick%SnapshotEvery == 0) {
			h.broadcastSnap(r)
		}
	}
}

func (h *Hub) countRoomClients(r *Room) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for c := range h.clients {
		c.mu.Lock()
		if c.room == r {
			n++
		}
		c.mu.Unlock()
	}
	return n
}

// roomClients snapshots the client list of a room.
func (h *Hub) roomClients(r *Room) []*Client {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*Client
	for c := range h.clients {
		c.mu.Lock()
		in := c.room == r
		c.mu.Unlock()
		if in {
			out = append(out, c)
		}
	}
	return out
}

// broadcastRoom sends room meta to everyone inside the room.
func (h *Hub) broadcastRoom(r *Room) {
	for _, c := range h.roomClients(r) {
		c.mu.Lock()
		seat := c.seat
		c.mu.Unlock()
		c.sendJSON(roomMsg(r, seat))
	}
}

// broadcastSnap sends the live snapshot to everyone inside the room.
func (h *Hub) broadcastSnap(r *Room) {
	msg := snapMsg(r)
	for _, c := range h.roomClients(r) {
		c.sendJSON(msg)
	}
}

// lobbyList builds the public room list.
func (h *Hub) lobbyList() []any {
	h.mu.Lock()
	defer h.mu.Unlock()
	rooms := []any{}
	for _, r := range h.rooms {
		if !r.Public {
			continue
		}
		r.mu.Lock()
		entry := map[string]any{
			"id":      r.ID,
			"title":   r.Title,
			"host":    hostName(r),
			"players": r.humanCount() + r.botCount(),
			"playing": r.phase == PhasePlay || r.phase == PhaseCount || r.phase == PhaseGoal,
			// open tells the lobby whether Join would actually seat a
			// human: a free seat, or a CPU seat the joiner takes over.
			// A table full of humans is watch-only (spectator).
			"open": r.players[0] == nil || r.players[1] == nil || r.players[0].Bot || r.players[1].Bot,
		}
		r.mu.Unlock()
		rooms = append(rooms, entry)
	}
	return rooms
}

func hostName(r *Room) string {
	for _, p := range r.players {
		if p != nil && !p.Bot {
			return p.Name
		}
	}
	return "—"
}

// broadcastLobby pushes the public list to all lobby clients.
func (h *Hub) broadcastLobby() {
	msg := map[string]any{"t": "lobby", "rooms": h.lobbyList()}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		c.mu.Lock()
		lobby := c.room == nil
		c.mu.Unlock()
		if lobby {
			c.sendJSON(msg)
		}
	}
}

// broadcastLobbyTo pushes the public list to one client.
func (h *Hub) broadcastLobbyTo(c *Client) {
	c.sendJSON(map[string]any{"t": "lobby", "rooms": h.lobbyList()})
}

// roomMsg describes the room for a client with the given seat.
func roomMsg(r *Room, seat int) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	players := []any{}
	for i := 0; i < 2; i++ {
		if r.players[i] == nil {
			players = append(players, nil)
		} else {
			players = append(players, map[string]any{"name": r.players[i].Name, "bot": r.players[i].Bot, "score": r.players[i].Score})
		}
	}
	return map[string]any{
		"t":       "room",
		"id":      r.ID,
		"title":   r.Title,
		"public":  r.Public,
		"players": players,
		"you":     seat,
		"phase":   string(r.phase),
		"count":   r.countdown,
		"winner":  r.winner,
		"reason":  r.winReason,
		"scorer":  r.scorer,
	}
}

// snapMsg is the canonical live snapshot; clients mirror seat 1 locally.
func snapMsg(r *Room) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	pads := []any{}
	for i := 0; i < 2; i++ {
		if r.players[i] == nil {
			pads = append(pads, nil)
		} else {
			pads = append(pads, []float64{round1(r.players[i].PX), round1(r.players[i].PY)})
		}
	}
	sc := []int{0, 0}
	for i := 0; i < 2; i++ {
		if r.players[i] != nil {
			sc[i] = r.players[i].Score
		}
	}
	return map[string]any{
		"t":      "snap",
		"tno":    r.tickNo,
		"puck":   []float64{round1(r.puckX), round1(r.puckY), round1(r.puckVX), round1(r.puckVY)},
		"pads":   pads,
		"sc":     sc,
		"ph":     string(r.phase),
		"count":  round1(r.countdown),
		"scorer": r.scorer,
		"winner": r.winner,
	}
}

func round1(v float64) float64 {
	if v >= 0 {
		return float64(int(v*10+0.5)) / 10
	}
	return float64(int(v*10-0.5)) / 10
}

func randomID(n int, alphabet string) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	for i := range buf {
		buf[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	return string(buf)
}

// sendJSON writes one message; a failed write disconnects lazily on next read.
func (c *Client) sendJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_ = c.ws.WriteMessage(b)
}

func (c *Client) sendError(msg string) {
	c.sendJSON(map[string]any{"t": "error", "msg": msg})
}
