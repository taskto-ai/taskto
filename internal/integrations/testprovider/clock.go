package testprovider

import (
	"crypto/rand"
	"sync"
	"time"
)

// Clock tells the time. The fake and the sender take one by constructor option, so a test moves
// time (a link that expires, a signature that is too old) without sleeping.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// DefaultStart is where NewManualClock starts when given the zero time.
var DefaultStart = time.Date(2030, time.March, 1, 12, 0, 0, 0, time.UTC)

// ManualClock is a Clock a test moves by hand. It is safe for concurrent use.
type ManualClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewManualClock returns a clock at start (DefaultStart if start is the zero time).
func NewManualClock(start time.Time) *ManualClock {
	if start.IsZero() {
		start = DefaultStart
	}
	return &ManualClock{now: start}
}

// Now returns the clock's time.
func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward (or back, for a negative d).
func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set moves the clock to t.
func (c *ManualClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

const alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// randomID returns n random letters and digits, the shape of the tail of Stripe's ids.
func randomID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("testprovider: no randomness: " + err.Error())
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
