package export

import (
	"sync"
	"time"
)

const (
	defaultPerPerson = 3
	defaultPerWindow = time.Hour
	defaultAtOnce    = 4
	busyRetryAfter   = 30 * time.Second
)

type refusal int

const (
	allowed refusal = iota
	running
	tooMany
	busy
)

type gate struct {
	perPerson int
	window    time.Duration
	atOnce    int
	now       func() time.Time

	mu      sync.Mutex
	started map[int64][]time.Time
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

func (g *gate) keep(user int64, recent []time.Time) {
	if len(recent) == 0 {
		delete(g.started, user)
		return
	}
	g.started[user] = recent
}

func (g *gate) refund(user int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if n := len(g.started[user]); n > 0 {
		g.keep(user, g.started[user][:n-1])
	}
}

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
		g.keep(user, recent)
		return nil, tooMany, g.window - now.Sub(recent[0])
	}
	if g.atOnce > 0 && g.total >= g.atOnce {
		g.keep(user, recent)
		return nil, busy, busyRetryAfter
	}
	for u, times := range g.started {
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
