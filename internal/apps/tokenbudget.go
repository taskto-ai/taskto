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

const (
	TokenActionsPerMinute = 60
	TokenActionBurst      = 60
)

type tokenBudget struct {
	mu      sync.Mutex
	buckets map[budgetKey]*bucket
}

var budgets sync.Map

func sharedBudget(db *sql.DB) *tokenBudget {
	b, _ := budgets.LoadOrStore(db, &tokenBudget{})
	return b.(*tokenBudget)
}

func (s *Service) WithClock(clock func() time.Time) *Service {
	s.clock = clock
	return s
}

type budgetKey struct{ user, workspace int64 }

type bucket struct {
	tokens float64
	at     time.Time
}

const pruneAbove = 1024

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

type rateLimitedError struct {
	api  *apierr.Error
	wait time.Duration
}

func (e *rateLimitedError) Error() string { return e.api.Error() }
func (e *rateLimitedError) Unwrap() error { return e.api }

func IsRateLimited(err error) bool {
	var limited *rateLimitedError
	return errors.As(err, &limited)
}

func RetryAfter(err error) (int, bool) {
	var limited *rateLimitedError
	if !errors.As(err, &limited) {
		return 0, false
	}
	return limited.retryAfter(), true
}

func (e *rateLimitedError) retryAfter() int { return max(1, int(math.Ceil(e.wait.Seconds()))) }

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
