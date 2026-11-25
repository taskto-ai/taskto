package apps

import (
	"database/sql"
	"errors"
	"math"
	"net/http"
	"sync"
	"time"

	"taskto.ai/taskto/internal/apierr"
)

// The write budget of access tokens. A token runs actions as its user without a person at a
// browser, so a script in a loop could insert and delete rows as fast as the server answers:
// the app's quota counts the live rows only, and every row it deletes stays (with its history)
// until a retention rule removes it (none purges them yet, so insert and delete cycles keep
// growing storage within the budget). The budget bounds how fast that can grow. It is per user
// and workspace, whichever of the user's tokens (or MCP connections) acts, so making more
// tokens buys nothing, and it counts every action a token runs, not only those that write,
// since the server can't tell before it has run one. A session is never limited by it: it is a
// person using the app.
const (
	// TokenActionsPerMinute is the rate a user's tokens may run actions at in a workspace, and
	// TokenActionBurst how many they may run at once before it applies.
	TokenActionsPerMinute = 60
	TokenActionBurst      = 60
)

// tokenBudget is a token bucket for each user in each workspace, in memory: a restart gives a
// full bucket, which is no more than the burst.
type tokenBudget struct {
	mu      sync.Mutex
	buckets map[budgetKey]*bucket
}

// budgets holds one budget for each database, so that the services over it (the server's routes
// and the agent tools' MCP endpoint are two) spend from the same one: more routes to the same
// actions don't make more of them.
var budgets sync.Map // *sql.DB -> *tokenBudget

func sharedBudget(db *sql.DB) *tokenBudget {
	b, _ := budgets.LoadOrStore(db, &tokenBudget{})
	return b.(*tokenBudget)
}

// WithClock sets the time the budget and the outbox's worker go by, for tests; it returns the
// service.
func (s *Service) WithClock(clock func() time.Time) *Service {
	s.clock = clock
	return s
}

type budgetKey struct{ user, workspace int64 }

type bucket struct {
	tokens float64
	at     time.Time
}

// pruneAbove is the number of buckets that makes take drop the ones that have refilled, which
// are the same as a bucket that was never made.
const pruneAbove = 1024

// take spends one action from key's bucket at time at. If the bucket is empty it spends
// nothing and says how long until an action is available.
func (b *tokenBudget) take(key budgetKey, at time.Time) (wait time.Duration, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buckets == nil {
		b.buckets = map[budgetKey]*bucket{}
	}
	if len(b.buckets) > pruneAbove {
		for k, v := range b.buckets {
			if v.tokens+refilled(at.Sub(v.at)) >= TokenActionBurst {
				delete(b.buckets, k)
			}
		}
	}
	bk, found := b.buckets[key]
	if !found {
		bk = &bucket{tokens: TokenActionBurst, at: at}
		b.buckets[key] = bk
	}
	if d := at.Sub(bk.at); d > 0 {
		bk.tokens = math.Min(TokenActionBurst, bk.tokens+refilled(d))
		bk.at = at
	}
	if bk.tokens >= 1 {
		bk.tokens--
		return 0, true
	}
	missing := 1 - bk.tokens
	return time.Duration(math.Ceil(missing / TokenActionsPerMinute * float64(time.Minute))), false
}

func refilled(d time.Duration) float64 {
	return d.Minutes() * TokenActionsPerMinute
}

// rateLimitedError is the refusal of an action over the budget: a 429 that says, in
// Retry-After, when to try again. It is an *apierr.Error to everything that reads one (the REST
// routes and the MCP tools), and fail adds the header.
type rateLimitedError struct {
	api  *apierr.Error
	wait time.Duration
}

func (e *rateLimitedError) Error() string { return e.api.Error() }
func (e *rateLimitedError) Unwrap() error { return e.api }

// IsRateLimited reports whether err is the refusal of an action over an access token's budget.
func IsRateLimited(err error) bool {
	var limited *rateLimitedError
	return errors.As(err, &limited)
}

// RetryAfter is how many seconds to wait before an action may run again, for an error that is the
// refusal of an action over an access token's budget (IsRateLimited), and whether err is one.
func RetryAfter(err error) (int, bool) {
	var limited *rateLimitedError
	if !errors.As(err, &limited) {
		return 0, false
	}
	return limited.retryAfter(), true
}

// retryAfter is wait in whole seconds, at least one.
func (e *rateLimitedError) retryAfter() int { return max(1, int(math.Ceil(e.wait.Seconds()))) }

// spendTokenAction takes an action from the budget of an actor who acts with an access token
// (Via is its name); anyone else is not limited.
func (s *Service) spendTokenAction(actor Actor, workspaceID int64) error {
	if actor.Via == "" {
		return nil
	}
	wait, ok := s.budget.take(budgetKey{actor.UserID, workspaceID}, s.clock())
	if ok {
		return nil
	}
	return &rateLimitedError{
		api: apierr.New(http.StatusTooManyRequests, apierr.CodeAppTokenRateLimited,
			"too many actions run with access tokens just now: try again in a moment",
			apierr.Params{"max": TokenActionsPerMinute, "seconds": int(math.Ceil(wait.Seconds()))}),
		wait: wait,
	}
}
