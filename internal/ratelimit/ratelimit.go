// Package ratelimit is the server's in-memory attempt counter: so many attempts per key in a
// fixed window. The login, the reset and invitation limits of internal/auth and the webhook
// route of internal/integrations use it. A restart forgets the counts, which is fine for slowing
// down guessing and floods.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter counts attempts per key in fixed windows, in memory.
type Limiter struct {
	max    int
	window time.Duration
	now    func() time.Time

	mu   sync.Mutex
	hits map[string]*window
}

type window struct {
	count int
	ends  time.Time
}

// SweepAt is the number of keys above which Take drops the expired windows.
const SweepAt = 10_000

// New returns a limiter that allows maxAttempts per key in each window of length per, reading the
// time from now.
func New(maxAttempts int, per time.Duration, now func() time.Time) *Limiter {
	return &Limiter{max: maxAttempts, window: per, now: now, hits: map[string]*window{}}
}

// Take counts an attempt for key. It reports false, and how long until the window ends, if key
// has used up its attempts; that attempt isn't counted.
func (l *Limiter) Take(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w := l.hits[key]
	if w == nil || !now.Before(w.ends) {
		if len(l.hits) >= SweepAt {
			l.sweep(now)
		}
		w = &window{ends: now.Add(l.window)}
		l.hits[key] = w
	}
	if w.count >= l.max {
		return false, w.ends.Sub(now)
	}
	w.count++
	return true, 0
}

// Give hands back an attempt taken for key, for one that turned out not to count.
func (l *Limiter) Give(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if w := l.hits[key]; w != nil && w.count > 0 {
		w.count--
	}
}

// Len is the number of keys the limiter is tracking, expired windows not yet swept included.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.hits)
}

// Used is the attempts counted across every key's current window.
func (l *Limiter) Used() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, w := range l.hits {
		n += w.count
	}
	return n
}

func (l *Limiter) sweep(now time.Time) {
	for k, w := range l.hits {
		if !now.Before(w.ends) {
			delete(l.hits, k)
		}
	}
}
