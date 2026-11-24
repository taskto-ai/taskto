package export

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"sync"
	"time"
)

// Starting an export (POST) and downloading it (GET) are two requests: only the first changes
// anything, so the browser's own cross-site protections (the CSRF guard on POST) cover it, and
// the download is a plain GET that reads. What joins them is a ticket: a random, single-use
// token that the first request hands to the page, good for a short while, for one person, one
// workspace and one session. It holds the slot the gate gave out (one export at a time, and the
// hour's count), so the slot is free again when the download ends, or when the ticket expires
// unused.

const (
	defaultTicketTTL   = 60 * time.Second
	defaultIdleTimeout = 2 * time.Minute
)

type ticket struct {
	user, workspace, session int64
	leave                    func()
	timer                    *time.Timer
}

type tickets struct {
	ttl time.Duration

	mu sync.Mutex
	m  map[string]*ticket // by the SHA-256 of the token, so a dump of memory holds no usable one
}

func newTickets(ttl time.Duration) *tickets {
	if ttl <= 0 {
		ttl = defaultTicketTTL
	}
	return &tickets{ttl: ttl, m: map[string]*ticket{}}
}

func keyOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// issue makes a ticket of the slot that leave gives back, and returns its token.
func (t *tickets) issue(user, workspace, session int64, leave func()) (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	key := keyOf(token)
	tk := &ticket{user: user, workspace: workspace, session: session, leave: leave}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.m[key] = tk
	tk.timer = time.AfterFunc(t.ttl, func() { t.expire(key, tk) })
	return token, nil
}

func (t *tickets) expire(key string, tk *ticket) {
	t.mu.Lock()
	if t.m[key] != tk {
		t.mu.Unlock()
		return
	}
	delete(t.m, key)
	t.mu.Unlock()
	tk.leave()
}

// redeem takes the ticket of token, if it is this person's, for this workspace and this session,
// and unused: it returns the function that ends the export. A ticket asked for by anyone else is
// left for its owner.
func (t *tickets) redeem(token string, user, workspace, session int64) (leave func(), ok bool) {
	key := keyOf(token)
	t.mu.Lock()
	defer t.mu.Unlock()
	tk := t.m[key]
	if tk == nil || tk.user != user || tk.workspace != workspace || tk.session != session {
		return nil, false
	}
	delete(t.m, key)
	tk.timer.Stop()
	return tk.leave, true
}
