package ratelimit

import (
	"sync"
	"time"
)

type Limiter struct {
	max    int
	window time.Duration
	now    func() time.Time

	mu        sync.Mutex
	hits      map[string]*window
	nextSweep time.Time
	sweeps    int
}

type window struct {
	count int
	ends  time.Time
}

const SweepAt = 10_000

func New(maxAttempts int, per time.Duration, now func() time.Time) *Limiter {
	return &Limiter{max: maxAttempts, window: per, now: now, hits: map[string]*window{}}
}

func (l *Limiter) Take(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w := l.hits[key]
	if w == nil || !now.Before(w.ends) {
		if len(l.hits) >= SweepAt && !now.Before(l.nextSweep) {
			l.sweep(now)
			l.nextSweep = now.Add(l.window / 4)
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

func (l *Limiter) Give(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if w := l.hits[key]; w != nil && w.count > 0 {
		w.count--
	}
}

func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.hits)
}

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
	l.sweeps++
	for k, w := range l.hits {
		if !now.Before(w.ends) {
			delete(l.hits, k)
		}
	}
}
