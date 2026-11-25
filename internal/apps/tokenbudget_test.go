package apps

import (
	"context"
	"errors"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
)

func TestTokenBudgetRefillsAtItsRate(t *testing.T) {
	b := &tokenBudget{}
	key := budgetKey{user: 1, workspace: 1}
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range TokenActionBurst {
		if _, ok := b.take(key, at); !ok {
			t.Fatalf("action %d of the burst refused", i+1)
		}
	}
	wait, ok := b.take(key, at)
	if ok || wait <= 0 || wait > time.Minute/TokenActionsPerMinute+time.Second {
		t.Fatalf("over the burst: ok=%v wait=%v, want a refusal that says to wait about a second", ok, wait)
	}
	// Another user, and the same user in another workspace, have budgets of their own.
	for _, other := range []budgetKey{{user: 2, workspace: 1}, {user: 1, workspace: 2}} {
		if _, ok := b.take(other, at); !ok {
			t.Errorf("%+v shares a budget with another", other)
		}
	}
	// A refusal spends nothing, and a second later one action is back.
	if _, ok := b.take(key, at.Add(time.Second)); !ok {
		t.Error("no action after a second")
	}
	if _, ok := b.take(key, at.Add(time.Second)); ok {
		t.Error("two actions after a second")
	}
	// A long rest refills the bucket to the burst and no further.
	later := at.Add(time.Hour)
	for i := range TokenActionBurst {
		if _, ok := b.take(key, later); !ok {
			t.Fatalf("action %d after a rest refused", i+1)
		}
	}
	if _, ok := b.take(key, later); ok {
		t.Error("a rest made more than the burst")
	}
}

func TestTokenBudgetForgetsBucketsThatRefilled(t *testing.T) {
	b := &tokenBudget{}
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range pruneAbove + 10 {
		b.take(budgetKey{user: int64(i), workspace: 1}, at)
	}
	b.take(budgetKey{user: -1, workspace: 1}, at.Add(time.Hour))
	if n := len(b.buckets); n > 2 {
		t.Errorf("%d buckets kept after they all refilled", n)
	}
}

// Only an actor with a token is limited: a person's session never is.
func TestOnlyTokensSpendTheBudget(t *testing.T) {
	s := New(nil, nil)
	s.budget = &tokenBudget{}
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	s.clock = func() time.Time { return at }
	token := Actor{UserID: 1, Agent: true, Via: "laptop"}
	for range TokenActionBurst {
		if err := s.spendTokenAction(token, 1); err != nil {
			t.Fatal(err)
		}
	}
	err := s.spendTokenAction(token, 1)
	var limited *rateLimitedError
	if !errors.As(err, &limited) || !IsRateLimited(err) || limited.retryAfter() < 1 {
		t.Fatalf("over the budget: %v", err)
	}
	// The agent of a conversation (no token) and a session are not limited.
	for _, a := range []Actor{{UserID: 1}, {UserID: 1, Agent: true}} {
		for range 3 * TokenActionBurst {
			if err := s.spendTokenAction(a, 1); err != nil {
				t.Fatalf("%+v limited: %v", a, err)
			}
		}
	}
	// act refuses before it looks at anything else.
	if _, err := s.act(context.Background(), access.Space{ID: 1, WorkspaceID: 1}, token, 1, ClientAction{}); !IsRateLimited(err) {
		t.Errorf("act over the budget = %v", err)
	}
}
