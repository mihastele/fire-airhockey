package main

import (
	"bufio"
	"net"
	"testing"
)

func newTestClient(t *testing.T, h *Hub) *Client {
	t.Helper()
	a, b := net.Pipe()
	go func() {
		buf := make([]byte, 8192)
		for {
			if _, err := b.Read(buf); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { a.Close(); b.Close() })
	return &Client{hub: h, ws: &WSConn{conn: a, br: bufio.NewReader(a)}, seat: seatLobby}
}

func (c *Client) testRoom() *Room {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.room
}

func (c *Client) testSeat() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seat
}

// A drop during countdown (game never started) must not crown a winner.
func TestCountdownDropBackToWait(t *testing.T) {
	r := twoPlayerRoom()
	r.phase = PhaseCount
	r.dropSeat(0)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.phase != PhaseWait {
		t.Fatalf("expected wait, got %q", r.phase)
	}
	if r.winner != -1 {
		t.Fatalf("nobody should win an unstarted game, winner=%d", r.winner)
	}
}

// A drop during live play concedes to whoever remains.
func TestPlayDropForfeits(t *testing.T) {
	r := twoPlayerRoom()
	r.phase = PhasePlay
	r.dropSeat(0)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.phase != PhaseOver || r.winner != 1 {
		t.Fatalf("expected seat 1 to win by forfeit, phase=%q winner=%d", r.phase, r.winner)
	}
}

// Rematch against the CPU must restart on the human's request alone.
func TestRematchVsBotRestarts(t *testing.T) {
	h := NewHub()
	c := newTestClient(t, h)
	c.handle([]byte(`{"t":"hello","name":"Solo"}`))
	c.handle([]byte(`{"t":"create","title":"p","cpu":true}`))
	r := c.testRoom()
	if r == nil {
		t.Fatal("no room created")
	}
	r.mu.Lock()
	r.phase = PhaseOver
	r.winner = 1
	r.players[0].Score = 3
	r.players[1].Score = 7
	r.mu.Unlock()
	c.handle([]byte(`{"t":"rematch"}`))
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.phase != PhaseCount {
		t.Fatalf("expected countdown after bot rematch, got %q", r.phase)
	}
	if r.players[0].Score+r.players[1].Score != 0 {
		t.Fatalf("scores should reset, got %d+%d", r.players[0].Score, r.players[1].Score)
	}
}

// Mutual rematch after a played match must start a fresh countdown.
func TestMutualRematchRestarts(t *testing.T) {
	h := NewHub()
	a := newTestClient(t, h)
	b := newTestClient(t, h)
	a.handle([]byte(`{"t":"hello","name":"Ann"}`))
	a.handle([]byte(`{"t":"create","title":"x","public":true}`))
	r := a.testRoom()
	b.handle([]byte(`{"t":"hello","name":"Bob","room":"` + r.ID + `"}`))
	r.mu.Lock()
	r.phase = PhaseOver
	r.winner = 0
	r.players[0].Score = 7
	r.players[1].Score = 4
	r.mu.Unlock()
	a.handle([]byte(`{"t":"rematch"}`))
	r.mu.Lock()
	stillOver := r.phase == PhaseOver
	r.mu.Unlock()
	if !stillOver {
		t.Fatal("one rematch request must not restart alone")
	}
	b.handle([]byte(`{"t":"rematch"}`))
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.phase != PhaseCount {
		t.Fatalf("expected countdown after mutual rematch, got %q", r.phase)
	}
	if r.players[0].Score+r.players[1].Score != 0 {
		t.Fatalf("scores should reset")
	}
}

// Rematch with no opponent left must reset to a fresh waiting table.
func TestRematchAloneResetsToWait(t *testing.T) {
	h := NewHub()
	a := newTestClient(t, h)
	b := newTestClient(t, h)
	a.handle([]byte(`{"t":"hello","name":"Ann"}`))
	a.handle([]byte(`{"t":"create","title":"x","public":true}`))
	r := a.testRoom()
	b.handle([]byte(`{"t":"hello","name":"Bob","room":"` + r.ID + `"}`))
	if b.testSeat() != 1 {
		t.Fatalf("expected Bob on seat 1, got %d", b.testSeat())
	}
	r.mu.Lock()
	r.phase = PhaseOver
	r.winner = 0
	r.mu.Unlock()
	// Bob abandons the finished match, Ann asks for a rematch alone.
	b.handle([]byte(`{"t":"leave"}`))
	a.handle([]byte(`{"t":"rematch"}`))
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.phase != PhaseWait {
		t.Fatalf("expected fresh waiting table, got %q", r.phase)
	}
	if r.winner != -1 || r.players[0].Score != 0 {
		t.Fatalf("expected clean slate, winner=%d score=%d", r.winner, r.players[0].Score)
	}
}

// Joining a table where your nickname is already seated must be refused:
// otherwise a second tab (which shares the stored nickname) ends up playing
// against itself.
func TestSameNickCannotJoinOwnTable(t *testing.T) {
	h := NewHub()
	a := newTestClient(t, h)
	b := newTestClient(t, h)
	a.handle([]byte(`{"t":"hello","name":"Ann"}`))
	a.handle([]byte(`{"t":"create","title":"Solo Table","public":true}`))
	r := a.testRoom()
	if r == nil {
		t.Fatal("no room created")
	}
	b.handle([]byte(`{"t":"hello","name":"Ann"}`))
	b.handle([]byte(`{"t":"join","room":"` + r.ID + `"}`))
	if got := b.testRoom(); got != nil {
		t.Fatal("same-nick second connection must not enter the table")
	}
	if got := b.testSeat(); got != seatLobby {
		t.Fatalf("refused joiner must stay in the lobby, seat=%d", got)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, p := range r.players {
		if p != nil && !p.Bot {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one human seated, got %d", n)
	}
}

// The same rule applies to invite links: hello with a room id while another
// connection already holds that nickname there must not seat a second copy.
func TestSameNickInviteLinkRefused(t *testing.T) {
	h := NewHub()
	a := newTestClient(t, h)
	a.handle([]byte(`{"t":"hello","name":"Ann"}`))
	a.handle([]byte(`{"t":"create","title":"Solo Table","public":false}`))
	r := a.testRoom()
	if r == nil {
		t.Fatal("no room created")
	}
	b := newTestClient(t, h)
	b.handle([]byte(`{"t":"hello","name":"Ann","room":"` + r.ID + `"}`))
	if got := b.testRoom(); got != nil {
		t.Fatal("same-nick invite join must not enter the table")
	}
}

// Blank table names fall back to "<nick>'s game"; long names are capped.
func TestCreateTitleCleaning(t *testing.T) {
	h := NewHub()
	c := newTestClient(t, h)
	c.handle([]byte(`{"t":"hello","name":"Ann"}`))
	c.handle([]byte(`{"t":"create","title":"   ","public":true}`))
	r := c.testRoom()
	if r == nil {
		t.Fatal("no room created")
	}
	r.mu.Lock()
	title := r.Title
	r.mu.Unlock()
	if title != "Ann's game" {
		t.Fatalf("expected default title, got %q", title)
	}
	long := ""
	for i := 0; i < 60; i++ {
		long += "x"
	}
	c.handle([]byte(`{"t":"create","title":"` + long + `","public":true}`))
	r2 := c.testRoom()
	r2.mu.Lock()
	defer r2.mu.Unlock()
	if n := len([]rune(r2.Title)); n != 48 {
		t.Fatalf("expected title capped at 48 runes, got %d", n)
	}
}

// Lobby entries report open=false once two humans hold the seats, so the UI
// can offer Watch (spectate) instead of a Join that would not seat anyone.
func TestLobbyOpenFlag(t *testing.T) {
	h := NewHub()
	a := newTestClient(t, h)
	a.handle([]byte(`{"t":"hello","name":"Ann"}`))
	a.handle([]byte(`{"t":"create","title":"x","public":true}`))
	r := a.testRoom()
	open := lobbyOpen(t, h, r.ID)
	if !open {
		t.Fatal("one-player table must be joinable")
	}
	b := newTestClient(t, h)
	b.handle([]byte(`{"t":"hello","name":"Bob","room":"` + r.ID + `"}`))
	if lobbyOpen(t, h, r.ID) {
		t.Fatal("two-human table must be reported as not open")
	}
}

func lobbyOpen(t *testing.T, h *Hub, id string) bool {
	t.Helper()
	for _, e := range h.lobbyList() {
		m, ok := e.(map[string]any)
		if !ok || m["id"] != id {
			continue
		}
		open, _ := m["open"].(bool)
		return open
	}
	t.Fatalf("room %s missing from lobby", id)
	return false
}

// A different nickname may still take over the CPU seat.
func TestOtherNickTakesBotSeat(t *testing.T) {
	h := NewHub()
	a := newTestClient(t, h)
	a.handle([]byte(`{"t":"hello","name":"Solo"}`))
	a.handle([]byte(`{"t":"create","title":"p","cpu":true}`))
	r := a.testRoom()
	if r == nil {
		t.Fatal("no room created")
	}
	b := newTestClient(t, h)
	b.handle([]byte(`{"t":"hello","name":"Bob","room":"` + r.ID + `"}`))
	if b.testSeat() != 1 {
		t.Fatalf("expected Bob to take the CPU seat, got %d", b.testSeat())
	}
}

// Creating a second table must free the seat in the first (no ghosts).
func TestCreateLeavesPreviousRoom(t *testing.T) {
	h := NewHub()
	c := newTestClient(t, h)
	c.handle([]byte(`{"t":"hello","name":"Ann"}`))
	c.handle([]byte(`{"t":"create","title":"one","public":true}`))
	first := c.testRoom()
	c.handle([]byte(`{"t":"create","title":"two","public":true}`))
	second := c.testRoom()
	if first == second {
		t.Fatal("expected a new room")
	}
	first.mu.Lock()
	occupied := first.players[0] != nil || first.players[1] != nil
	first.mu.Unlock()
	if occupied {
		t.Fatal("ghost seat left behind in the first room")
	}
}

// Joining the room you are already in must not take a second seat.
func TestRejoinSameRoomKeepsSeat(t *testing.T) {
	h := NewHub()
	c := newTestClient(t, h)
	c.handle([]byte(`{"t":"hello","name":"Ann"}`))
	c.handle([]byte(`{"t":"create","title":"one","public":true}`))
	r := c.testRoom()
	seat := c.testSeat()
	c.handle([]byte(`{"t":"join","room":"` + r.ID + `"}`))
	if c.testSeat() != seat {
		t.Fatalf("seat changed %d -> %d", seat, c.testSeat())
	}
	r.mu.Lock()
	n := 0
	for _, p := range r.players {
		if p != nil {
			n++
		}
	}
	r.mu.Unlock()
	if n != 1 {
		t.Fatalf("expected exactly one seated player, got %d", n)
	}
}
