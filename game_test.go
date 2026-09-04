package main

import (
	"math"
	"testing"
)

func twoPlayerRoom() *Room {
	r := newRoom("T1", "test", true)
	// Park both mallets in the corners (with matching targets so they stay
	// there) so straight test shots fly unobstructed.
	r.players[0] = &Player{Name: "a", PX: 10, PY: TableH - 10, TX: 10, TY: TableH - 10}
	r.players[1] = &Player{Name: "b", PX: 10, PY: 10, TX: 10, TY: 10}
	r.phase = PhasePlay
	return r
}

// A puck fired straight into the top goal mouth must score for seat 0.
func TestGoalTopScoresSeat0(t *testing.T) {
	r := twoPlayerRoom()
	r.puckX, r.puckY = TableW/2, 40
	r.puckVX, r.puckVY = 0, -160
	for i := 0; i < 120 && r.phase != PhaseGoal; i++ {
		r.stepPlay(1.0 / TickHz)
	}
	if r.phase != PhaseGoal {
		t.Fatalf("expected goal phase, got %q (puck %v,%v)", r.phase, r.puckX, r.puckY)
	}
	if r.scorer != 0 || r.players[0].Score != 1 {
		t.Fatalf("expected seat 0 to score, scorer=%d scores=%v", r.scorer, []int{r.players[0].Score, r.players[1].Score})
	}
}

// A puck fired straight into the bottom goal mouth must score for seat 1.
func TestGoalBottomScoresSeat1(t *testing.T) {
	r := twoPlayerRoom()
	r.puckX, r.puckY = TableW/2, TableH-40
	r.puckVX, r.puckVY = 0, 160
	for i := 0; i < 120 && r.phase != PhaseGoal; i++ {
		r.stepPlay(1.0 / TickHz)
	}
	if r.phase != PhaseGoal || r.scorer != 1 || r.players[1].Score != 1 {
		t.Fatalf("expected seat 1 goal, phase=%q scorer=%d", r.phase, r.scorer)
	}
}

// Outside the mouth the end wall must bounce, not score.
func TestEndWallBouncesOutsideMouth(t *testing.T) {
	r := twoPlayerRoom()
	r.puckX, r.puckY = 5, 40 // far from the centered mouth
	r.puckVX, r.puckVY = 0, -160
	for i := 0; i < 120; i++ {
		r.stepPlay(1.0 / TickHz)
	}
	if r.phase != PhasePlay {
		t.Fatalf("expected play to continue, got %q", r.phase)
	}
	if r.players[0].Score+r.players[1].Score != 0 {
		t.Fatalf("no goal should have been scored")
	}
	if r.puckVY <= 0 {
		t.Fatalf("puck should have bounced back down, vy=%v", r.puckVY)
	}
}

// A puck overlapping a still paddle must be pushed out and reflected.
func TestPaddleCollisionReflects(t *testing.T) {
	r := twoPlayerRoom()
	p := r.players[0]
	p.PX, p.PY, p.TX, p.TY = TableW/2, 150, TableW/2, 150
	r.puckX, r.puckY = TableW/2, 150-(PuckR+PadR)+0.5 // slight overlap
	r.puckVX, r.puckVY = 0, 80                        // moving down into the paddle
	r.collidePaddle(p)
	dist := math.Hypot(r.puckX-p.PX, r.puckY-p.PY)
	if dist < PuckR+PadR-1e-9 {
		t.Fatalf("puck still overlapping paddle, dist=%v", dist)
	}
	if r.puckVY >= 0 {
		t.Fatalf("puck should leave upward after hitting paddle from above, vy=%v", r.puckVY)
	}
}

// Winning the 7th point must end the match through the full tick path.
func TestWinAtSevenEndsMatch(t *testing.T) {
	r := twoPlayerRoom()
	r.players[0].Score = WinScore - 1
	r.puckX, r.puckY = TableW/2, 40
	r.puckVX, r.puckVY = 0, -160
	for i := 0; i < 60 && r.phase == PhasePlay; i++ {
		r.tick(1.0 / TickHz)
	}
	if r.phase != PhaseGoal {
		t.Fatalf("expected goal phase, got %q", r.phase)
	}
	for i := 0; i < int(GoalPauseSecs*TickHz)+10 && r.phase == PhaseGoal; i++ {
		r.tick(1.0 / TickHz)
	}
	if r.phase != PhaseOver || r.winner != 0 {
		t.Fatalf("expected seat 0 win, phase=%q winner=%d", r.phase, r.winner)
	}
}

// Waiting rooms start the countdown as soon as both seats fill.
func TestCountdownStartsWithTwoPlayers(t *testing.T) {
	r := newRoom("T2", "test", true)
	if r.addPlayer("a") != 0 || r.addPlayer("b") != 1 {
		t.Fatalf("expected seats 0 and 1")
	}
	if r.tick(1.0 / TickHz); r.phase != PhaseCount {
		t.Fatalf("expected countdown, got %q", r.phase)
	}
}

// Paddle targets must stay on the owner's half.
func TestTargetsClampedToOwnHalf(t *testing.T) {
	r := twoPlayerRoom()
	r.setTarget(0, -50, -50)
	if p := r.players[0]; p.TX < PadR || p.TY < TableH/2 {
		t.Fatalf("seat 0 escaped its half: %v,%v", p.TX, p.TY)
	}
	r.setTarget(1, 999, 999)
	if p := r.players[1]; p.TX > TableW-PadR || p.TY > TableH/2 {
		t.Fatalf("seat 1 escaped its half: %v,%v", p.TX, p.TY)
	}
}

// Joining a room whose second seat is a bot must evict the bot, not spectate.
func TestJoinEvictsBot(t *testing.T) {
	r := newRoom("T3", "test", true)
	if r.addPlayer("human") != 0 {
		t.Fatalf("expected seat 0")
	}
	if r.addBot() != 1 {
		t.Fatalf("expected bot on seat 1")
	}
	if seat := r.addPlayer("second"); seat != 1 {
		t.Fatalf("expected human to take seat 1, got %d", seat)
	}
	if r.players[1].Bot {
		t.Fatalf("bot was not evicted")
	}
}
