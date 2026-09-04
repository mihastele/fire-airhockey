// Game core: table constants, authoritative puck/paddle physics and the room
// state machine. All coordinates are canonical table units:
//
//	x in [0, TableW], y in [0, TableH]
//	seat 0 defends the y=TableH goal, seat 1 defends the y=0 goal.
//
// Each client plays in its own view with itself at the bottom; the hub maps
// seat-1 positions by flipping y (x is shared so left/right match on both
// screens).
package main

import (
	"math"
	"sync"
	"time"
)

const (
	TableW   = 100.0
	TableH   = 200.0
	PuckR    = 3.0
	PadR     = 5.5
	GoalHalf = 15.0 // half width of the goal mouth

	TickHz         = 60
	WinScore       = 7
	CountdownSecs  = 3.0
	GoalPauseSecs  = 1.5
	PuckMaxSpeed   = 230.0
	PuckFriction   = 0.35 // exponential damping, per second
	PuckStop       = 0.6
	PadMaxSpeed    = 340.0
	PadHitBoost    = 0.45 // fraction of paddle velocity added to the puck
	PuckRestWall   = 0.85
	PuckRestPaddle = 0.92
	EmptyRoomTTL   = 45 * time.Second
	SnapshotEvery  = 2 // broadcast a snapshot every N ticks (30 Hz)
)

// Phase is the room's match state.
type Phase string

const (
	PhaseWait  Phase = "wait"  // need a second player
	PhaseCount Phase = "count" // countdown before serve
	PhasePlay  Phase = "play"  // puck live
	PhaseGoal  Phase = "goal"  // brief pause after a goal
	PhaseOver  Phase = "over"  // match decided
)

// Player is one seat in a room. Bot seats are driven by room.botAI.
type Player struct {
	Name  string
	Bot   bool
	Score int
	PX    float64 // paddle center, canonical
	PY    float64
	TX    float64 // paddle target, canonical
	TY    float64
	VX    float64 // actual paddle velocity (units/s), for hit transfer
	VY    float64
}

// Room hosts one match plus its spectators.
type Room struct {
	ID      string
	Title   string
	Public  bool
	Created time.Time

	mu         sync.Mutex
	players    [2]*Player
	puckX      float64
	puckY      float64
	puckVX     float64
	puckVY     float64
	phase      Phase
	phaseT     float64 // seconds left in count/goal phases
	countdown  float64 // countdown display value
	scorer     int
	winner     int // -1 while undecided
	winReason  string
	rematch    [2]bool
	emptySince time.Time // set while the room has no clients
	tickNo     uint64
	changed    bool // room meta changed -> hub should push lobby + room msgs
}

// newRoom makes a room with the puck and paddles parked at home.
func newRoom(id, title string, public bool) *Room {
	r := &Room{ID: id, Title: title, Public: public, Created: time.Now(), winner: -1}
	r.resetPositions(0)
	r.phase = PhaseWait
	return r
}

func homeY(seat int) float64 {
	if seat == 0 {
		return TableH - 24
	}
	return 24
}

// resetPositions parks the puck in the middle (optionally served toward one
// side) and both paddles at home.
func (r *Room) resetPositions(serveDir float64) {
	r.puckX, r.puckY = TableW/2, TableH/2
	r.puckVX, r.puckVY = 0, serveDir*70
	for seat := 0; seat < 2; seat++ {
		if p := r.players[seat]; p != nil {
			p.PX, p.TX = TableW/2, TableW/2
			p.PY, p.TY = homeY(seat), homeY(seat)
			p.VX, p.VY = 0, 0
		}
	}
}

// humanCount reports seats held by humans.
func (r *Room) humanCount() int {
	n := 0
	for _, p := range r.players {
		if p != nil && !p.Bot {
			n++
		}
	}
	return n
}

// hasHuman reports whether a human seat already holds name. Bots are
// excluded: a player may always take over a CPU seat.
// Call with the room lock held.
func (r *Room) hasHuman(name string) bool {
	for _, p := range r.players {
		if p != nil && !p.Bot && p.Name == name {
			return true
		}
	}
	return false
}

// botCount reports seats held by bots.
func (r *Room) botCount() int {
	n := 0
	for _, p := range r.players {
		if p != nil && p.Bot {
			n++
		}
	}
	return n
}

// addPlayer seats a human, preferring an empty seat, then a bot seat (the bot
// is evicted). Returns the seat or -1 when the room is full of humans.
func (r *Room) addPlayer(name string) int {
	for seat := 0; seat < 2; seat++ {
		if r.players[seat] == nil {
			r.players[seat] = &Player{Name: name, PX: TableW / 2, PY: homeY(seat), TX: TableW / 2, TY: homeY(seat)}
			r.rematch[seat] = false
			r.changed = true
			return seat
		}
	}
	for seat := 0; seat < 2; seat++ {
		if r.players[seat].Bot {
			r.players[seat] = &Player{Name: name, PX: TableW / 2, PY: homeY(seat), TX: TableW / 2, TY: homeY(seat)}
			r.rematch[seat] = false
			r.changed = true
			return seat
		}
	}
	return -1
}

// addBot seats a CPU opponent if a seat is free. Returns the seat or -1.
func (r *Room) addBot() int {
	for seat := 0; seat < 2; seat++ {
		if r.players[seat] == nil {
			r.players[seat] = &Player{Name: "CPU", Bot: true, PX: TableW / 2, PY: homeY(seat), TX: TableW / 2, TY: homeY(seat)}
			r.rematch[seat] = false
			r.changed = true
			return seat
		}
	}
	return -1
}

// removeSeat frees a seat. Called with the room lock held.
func (r *Room) removeSeat(seat int) {
	if seat < 0 || seat > 1 {
		return
	}
	r.players[seat] = nil
	r.rematch[seat] = false
	r.changed = true
}

// bothPresent reports whether both seats are filled (human or bot).
func (r *Room) bothPresent() bool {
	return r.players[0] != nil && r.players[1] != nil
}

// startCountdown begins (or restarts) a match.
func (r *Room) startCountdown() {
	for _, p := range r.players {
		if p != nil {
			p.Score = 0
		}
	}
	r.rematch = [2]bool{}
	r.winner = -1
	r.winReason = ""
	r.resetPositions(0)
	r.phase = PhaseCount
	r.countdown = CountdownSecs
	r.phaseT = CountdownSecs
	r.changed = true
}

// setTarget records a seat's desired paddle position, clamped to its own half.
// Called with the room lock held. Coordinates are canonical.
func (r *Room) setTarget(seat int, x, y float64) {
	p := r.players[seat]
	if p == nil || p.Bot {
		return
	}
	p.TX = clamp(x, PadR, TableW-PadR)
	if seat == 0 {
		p.TY = clamp(y, TableH/2+PadR*0.4, TableH-PadR)
	} else {
		p.TY = clamp(y, PadR, TableH/2-PadR*0.4)
	}
}

// forfeit ends a live match because seat left; the other seat wins.
func (r *Room) forfeit(seat int) {
	if r.phase != PhasePlay && r.phase != PhaseGoal {
		return
	}
	other := 1 - seat
	if r.players[other] == nil {
		r.phase = PhaseWait
	} else {
		r.phase = PhaseOver
		r.winner = other
		r.winReason = "opponent left"
	}
	r.changed = true
}

// dropSeat frees a seat after a leave or disconnect. A drop during live play
// concedes the match to whoever remains; a drop during countdown (the game
// never started) or any idle phase just sends the room back to waiting —
// nobody "wins" a game that never began.
func (r *Room) dropSeat(seat int) {
	if seat < 0 || seat > 1 {
		return
	}
	counting := r.phase == PhaseCount
	r.removeSeat(seat)
	switch {
	case counting:
		r.phase = PhaseWait
		r.changed = true
	case r.phase == PhasePlay || r.phase == PhaseGoal:
		if r.players[1-seat] != nil {
			r.forfeit(seat)
		} else {
			r.phase = PhaseWait
			r.changed = true
		}
	}
}

// resetToWait clears a finished match so the table is fresh for the next
// challenger. The seated player stays seated.
func (r *Room) resetToWait() {
	for _, p := range r.players {
		if p != nil {
			p.Score = 0
		}
	}
	r.rematch = [2]bool{}
	r.winner = -1
	r.winReason = ""
	r.resetPositions(0)
	r.phase = PhaseWait
	r.changed = true
}

// tick advances the simulation by dt seconds. Called at TickHz by the room
// loop. Returns true when callers should broadcast fresh room/snapshot data.
func (r *Room) tick(dt float64) bool {
	r.tickNo++
	out := false
	if r.changed {
		r.changed = false
		out = true
	}
	switch r.phase {
	case PhaseWait:
		if r.bothPresent() {
			r.startCountdown()
			out = true
		}
	case PhaseCount:
		r.phaseT -= dt
		r.countdown = r.phaseT
		if r.phaseT <= 0 {
			r.phase = PhasePlay
			r.resetPositions(0)
			// Opening serve drifts toward a random side.
			if r.tickNo%2 == 0 {
				r.puckVY = 55
			} else {
				r.puckVY = -55
			}
			out = true
		}
	case PhasePlay:
		r.stepPlay(dt)
		// No out=true here: 30 Hz snapshots carry live play. Room meta is
		// only rebroadcast when something actually changed (goals set it).
	case PhaseGoal:
		r.phaseT -= dt
		r.stepPaddles(dt) // let mallets glide home during the pause
		if r.phaseT <= 0 {
			if r.players[0] != nil && r.players[0].Score >= WinScore ||
				r.players[1] != nil && r.players[1].Score >= WinScore {
				r.phase = PhaseOver
				r.winner = 0
				if r.players[1] != nil && r.players[1].Score >= WinScore {
					r.winner = 1
				}
				r.winReason = ""
			} else {
				r.phase = PhasePlay
				// Serve toward the player who was scored on.
				if r.scorer == 0 {
					r.resetPositions(-1)
				} else {
					r.resetPositions(1)
				}
			}
			out = true
		}
	case PhaseOver:
		// idle; rematch requests are handled by hub handlers
	}
	return out
}

// stepPlay moves paddles (humans chase targets, bots run AI), then the puck.
func (r *Room) stepPlay(dt float64) {
	r.stepPaddles(dt)

	// Friction. Squared-speed compares avoid a sqrt on the hot path; the
	// single Exp per tick is the only transcendental left here.
	damp := math.Exp(-PuckFriction * dt)
	r.puckVX *= damp
	r.puckVY *= damp
	if r.puckVX*r.puckVX+r.puckVY*r.puckVY < PuckStop*PuckStop {
		r.puckVX, r.puckVY = 0, 0
	}
	r.puckX += r.puckVX * dt
	r.puckY += r.puckVY * dt

	// Paddle collisions.
	for seat := 0; seat < 2; seat++ {
		if p := r.players[seat]; p != nil {
			r.collidePaddle(p)
		}
	}

	// Side walls.
	if r.puckX < PuckR {
		r.puckX = PuckR
		r.puckVX = -r.puckVX * PuckRestWall
	} else if r.puckX > TableW-PuckR {
		r.puckX = TableW - PuckR
		r.puckVX = -r.puckVX * PuckRestWall
	}

	// End walls and goals. The mouth is centered; outside it the wall bounces.
	inMouth := math.Abs(r.puckX-TableW/2) < GoalHalf-PuckR*0.5
	if r.puckY < PuckR {
		if inMouth && r.puckVY < 0 {
			r.goal(0)
			return
		}
		r.puckY = PuckR
		r.puckVY = -r.puckVY * PuckRestWall
	} else if r.puckY > TableH-PuckR {
		if inMouth && r.puckVY > 0 {
			r.goal(1)
			return
		}
		r.puckY = TableH - PuckR
		r.puckVY = -r.puckVY * PuckRestWall
	}

	// Clamp runaway speed. Only the over-limit case pays for a sqrt.
	if spSq := r.puckVX*r.puckVX + r.puckVY*r.puckVY; spSq > PuckMaxSpeed*PuckMaxSpeed {
		scale := PuckMaxSpeed / math.Sqrt(spSq)
		r.puckVX *= scale
		r.puckVY *= scale
	}
}

// goal records a goal by seat (seat 0 attacks the y=0 goal).
func (r *Room) goal(by int) {
	if p := r.players[by]; p != nil {
		p.Score++
	}
	r.scorer = by
	r.phase = PhaseGoal
	r.phaseT = GoalPauseSecs
	r.changed = true
}

// stepPaddles drives every paddle toward its target with a speed cap so
// inputs stay smooth and teleporting across the table is impossible.
//
// Hot path (60 Hz per room): idle paddles (target reached, the common case)
// exit on a cheap compare with no sqrt; paddles that reach their target this
// tick snap without any divide; only a genuinely capped stride pays for one
// sqrt via a single reciprocal. Behavior matches the old formulation exactly:
// velocity is dx/dt when the target is reached, capped-stride velocity
// otherwise, with the same 1e-6 dead zone.
func (r *Room) stepPaddles(dt float64) {
	maxStep := PadMaxSpeed * dt
	maxStepSq := maxStep * maxStep
	invDt := 1 / dt
	for seat := 0; seat < 2; seat++ {
		p := r.players[seat]
		if p == nil {
			continue
		}
		if p.Bot {
			r.botAI(seat, p)
		}
		dx, dy := p.TX-p.PX, p.TY-p.PY
		distSq := dx*dx + dy*dy
		if distSq < 1e-12 {
			p.VX, p.VY = 0, 0
			continue
		}
		if distSq <= maxStepSq {
			p.PX, p.PY = p.TX, p.TY
			p.VX, p.VY = dx*invDt, dy*invDt
			continue
		}
		inv := 1 / math.Sqrt(distSq)
		p.PX += dx * inv * maxStep
		p.PY += dy * inv * maxStep
		p.VX = dx * inv * PadMaxSpeed
		p.VY = dy * inv * PadMaxSpeed
	}
}

// botAI steers a bot paddle: chase the puck when it is on (or heading to)
// the bot's half, otherwise drift home. Speed-capped like human paddles,
// just with a slightly lower effective pace via a damped target.
func (r *Room) botAI(seat int, p *Player) {
	ownGoalY := 0.0
	if seat == 0 {
		ownGoalY = TableH
	}
	onMyHalf := (seat == 0 && r.puckY > TableH/2) || (seat == 1 && r.puckY < TableH/2)
	coming := (seat == 0 && r.puckVY > 30) || (seat == 1 && r.puckVY < -30)
	tx, ty := TableW/2, homeY(seat)
	if onMyHalf || coming {
		// Guard the goal line mouth while tracking the puck's x.
		tx = clamp(r.puckX+r.puckVX*0.12, PadR, TableW-PadR)
		if onMyHalf {
			ty = clamp(r.puckY, PadR, TableH-PadR)
			if seat == 0 {
				ty = math.Max(ty, TableH/2+PadR*0.4)
				ty = math.Min(ty, TableH-PadR)
			} else {
				ty = math.Min(ty, TableH/2-PadR*0.4)
				ty = math.Max(ty, PadR)
			}
			// Lunge slightly toward the puck to strike rather than block.
			ty += (r.puckY - ty) * 0.25
		} else {
			ty = ownGoalY
			if seat == 0 {
				ty = TableH - 14
			} else {
				ty = 14
			}
		}
	}
	// Damp the target so the bot is beatable, then clamp to its half.
	p.TX += (tx - p.TX) * 0.35
	p.TY += (ty - p.TY) * 0.35
	if seat == 0 {
		p.TY = clamp(p.TY, TableH/2+PadR*0.4, TableH-PadR)
	} else {
		p.TY = clamp(p.TY, PadR, TableH/2-PadR*0.4)
	}
	p.TX = clamp(p.TX, PadR, TableW-PadR)
}

// collidePaddle resolves puck/paddle overlap: positional separation plus a
// velocity reflection that inherits part of the paddle's motion.
func (r *Room) collidePaddle(p *Player) {
	dx, dy := r.puckX-p.PX, r.puckY-p.PY
	minDist := PuckR + PadR
	// Squared early-out: non-overlapping pairs (the common case) skip the
	// sqrt entirely. Equivalent to the old dist>=minDist||dist<1e-6 test.
	distSq := dx*dx + dy*dy
	if distSq >= minDist*minDist || distSq < 1e-12 {
		return
	}
	dist := math.Sqrt(distSq)
	nx, ny := dx/dist, dy/dist
	// Separate.
	r.puckX = p.PX + nx*minDist
	r.puckY = p.PY + ny*minDist
	// Relative velocity along the normal.
	rvx, rvy := r.puckVX-p.VX, r.puckVY-p.VY
	vn := rvx*nx + rvy*ny
	if vn < 0 {
		r.puckVX -= (1 + PuckRestPaddle) * vn * nx
		r.puckVY -= (1 + PuckRestPaddle) * vn * ny
	}
	// Inherit some paddle motion for smash shots.
	r.puckVX += p.VX * PadHitBoost
	r.puckVY += p.VY * PadHitBoost
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
