package export

import (
	"sync"
	"time"
)

// An export reads all of a person's data and holds a database connection and a goroutine for as
// long as the person's connection is slow, so it is limited three ways: one at a time per
// person, a few per hour per person, and a few at a time on the whole server.
const (
	defaultPerPerson = 3
	defaultPerWindow = time.Hour
	defaultAtOnce    = 4
	// busyRetryAfter is what a person is told to wait when the server's exports are all busy.
	busyRetryAfter = 30 * time.Second
)

// why a person is turned away.
type refusal int

const (
	allowed refusal = iota
	// running: the person has an export running already.
	running
	// tooMany: the person started their share of exports this hour.
	tooMany
	// busy: the server's exports are all busy.
	busy
)

// gate limits exports. It is in memory: a restart forgets it, which is fine for a courtesy to
// the server's resources, not a security boundary.
type gate struct {
	perPerson int
	window    time.Duration
	atOnce    int
	now       func() time.Time

	mu      sync.Mutex
	started map[int64][]time.Time // each person's starts within the window
	active  map[int64]bool
	total   int
}

func newGate(perPerson int, window time.Duration, atOnce int, now func() time.Time) *gate {
	if perPerson == 0 {
		perPerson = defaultPerPerson
	}
	if window == 0 {
		window = defaultPerWindow
	}
	if atOnce == 0 {
		atOnce = defaultAtOnce
	}
	if now == nil {
		now = time.Now
	}
	return &gate{perPerson: perPerson, window: window, atOnce: atOnce, now: now,
		started: map[int64][]time.Time{}, active: map[int64]bool{}}
}

// enter lets user start an export, and returns the function that ends it. Otherwise it says why
// not, and how long until trying again can work. A refused attempt isn't counted. A negative
// perPerson or atOnce, for tests, is no limit of that kind.
func (g *gate) enter(user int64) (leave func(), why refusal, wait time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if g.active[user] {
		return nil, running, busyRetryAfter
	}
	recent := g.started[user][:0:0]
	for _, at := range g.started[user] {
		if now.Sub(at) < g.window {
			recent = append(recent, at)
		}
	}
	if g.perPerson > 0 && len(recent) >= g.perPerson {
		g.started[user] = recent
		return nil, tooMany, g.window - now.Sub(recent[0])
	}
	if g.atOnce > 0 && g.total >= g.atOnce {
		g.started[user] = recent
		return nil, busy, busyRetryAfter
	}
	for u, times := range g.started { // forget the people whose starts have all expired
		if len(times) > 0 && now.Sub(times[len(times)-1]) >= g.window {
			delete(g.started, u)
		}
	}
	g.started[user] = append(recent, now)
	g.active[user] = true
	g.total++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			delete(g.active, user)
			g.total--
		})
	}, allowed, 0
}
