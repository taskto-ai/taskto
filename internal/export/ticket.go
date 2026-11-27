package export

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"sync"
	"time"
)

const (
	defaultTicketTTL   = 60 * time.Second
	defaultIdleTimeout = 2 * time.Minute
)

type ticket struct {
	user, workspace, session int64
	leave                    func()
	ex                       *run
	timer                    *time.Timer
}

type tickets struct {
	ttl time.Duration

	mu sync.Mutex
	m  map[string]*ticket
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

func (t *tickets) issue(user, workspace, session int64, leave func(), ex *run) (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	key := keyOf(token)
	tk := &ticket{user: user, workspace: workspace, session: session, leave: leave, ex: ex}
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

func (t *tickets) redeem(token string, user, workspace, session int64) (ex *run, leave func(), ok bool) {
	key := keyOf(token)
	t.mu.Lock()
	defer t.mu.Unlock()
	tk := t.m[key]
	if tk == nil || tk.user != user || tk.workspace != workspace || tk.session != session {
		return nil, nil, false
	}
	delete(t.m, key)
	tk.timer.Stop()
	return tk.ex, tk.leave, true
}
