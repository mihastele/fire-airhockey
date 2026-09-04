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
