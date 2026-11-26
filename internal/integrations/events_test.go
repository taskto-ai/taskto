package integrations

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/apierr"
)

// applier is an Applier a test controls: it records what it was given and answers with fn.
type applier struct {
	mu   sync.Mutex
	seen []StoredEvent
	fn   func(StoredEvent) error
	got  chan StoredEvent
}

func (a *applier) ApplyEvent(_ context.Context, ev StoredEvent) error {
	a.mu.Lock()
	a.seen = append(a.seen, ev)
	fn := a.fn
	a.mu.Unlock()
	if a.got != nil {
		select {
		case a.got <- ev:
		default:
		}
	}
	if fn == nil {
		return nil
	}
	return fn(ev)
}

func (a *applier) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.seen)
}

// put stores n events for the rig's integration, as the route does, and returns their ids.
func (r *rig) put(n int) []int64 {
	r.t.Helper()
	var ids []int64
	for i := range n {
		ev := Event{ID: fmt.Sprintf("evt_%d_%s", i, token()[:8]), Kind: EventPaymentSucceeded, RefToken: token(), ObjectID: "cs_test_" + token(),
			AmountMinor: int64(1000 + i), Currency: "EUR", OccurredAt: r.clock.Now()}
		if got, err := r.store.record(context.Background(), r.in.ID, ev, SourceWebhook, []byte(ev.ID)); err != nil || got != storedNew {
			r.t.Fatalf("record: %v, %v", got, err)
		}
		r.clock.Advance(time.Millisecond) // received_at differs: the worker takes them in order
	}
	for _, row := range r.rows(r.in.ID) {
		ids = append(ids, row.ID)
	}
	return ids
}

func (r *rig) state(id int64) (state string, attempts int, code string, next string) {
	r.t.Helper()
	var c, n *string
	if err := r.db.QueryRow(`SELECT state, attempts, error_code, next_attempt_at FROM integration_events WHERE id = ?`, id).Scan(&state, &attempts, &c, &n); err != nil {
		r.t.Fatal(err)
	}
	if c != nil {
		code = *c
	}
	if n != nil {
		next = *n
	}
	return state, attempts, code, next
}

func TestTheWorkerAppliesEventsInOrder(t *testing.T) {
	r := newRig(t)
	ids := r.put(3)
	a := &applier{}
	r.svc.SetApplier(a)
	r.svc.drainEvents(context.Background())

	if a.count() != 3 {
		t.Fatalf("applied %d events, want 3", a.count())
	}
	for i, ev := range a.seen {
		if ev.ID != ids[i] || ev.IntegrationID != r.in.ID || ev.WorkspaceID != r.ws || ev.Source != SourceWebhook || ev.Kind != EventPaymentSucceeded ||
			ev.Currency != "EUR" || ev.AmountMinor != int64(1000+i) || ev.Attempts != 1 || ev.RefToken == "" || !strings.HasPrefix(ev.Event.ID, "evt_") || ev.Event.ID != ev.ProviderEvent {
			t.Errorf("event %d as the worker got it: %+v", i, ev)
		}
	}
	for _, id := range ids {
		if state, attempts, code, _ := r.state(id); state != EventApplied || attempts != 1 || code != "" {
			t.Errorf("event %d: %s after %d tries, code %q", id, state, attempts, code)
		}
	}
	var processed int
	if err := r.db.QueryRow(`SELECT count(*) FROM integration_events WHERE processed_at IS NOT NULL AND lease_until IS NULL`).Scan(&processed); err != nil || processed != 3 {
		t.Errorf("%d events noted as processed: %v", processed, err)
	}
	// Done is done: nothing runs again.
	r.svc.drainEvents(context.Background())
	if a.count() != 3 {
		t.Errorf("an applied event ran again: %d", a.count())
	}
}

func TestWithoutAnApplierEventsWait(t *testing.T) {
	r := newRig(t)
	ids := r.put(2)
	r.svc.drainEvents(context.Background())
	for _, id := range ids {
		if state, attempts, _, _ := r.state(id); state != EventPending || attempts != 0 {
			t.Errorf("event %d: %s after %d tries, want it untouched", id, state, attempts)
		}
	}
}

func TestTheOutcomesOfAnApplier(t *testing.T) {
	r := newRig(t)
	ids := r.put(4)
	a := &applier{fn: func(ev StoredEvent) error {
		switch ev.AmountMinor {
		case 1000:
			return Ignore(apierr.CodeNotFound, apierr.Params{"why": "late"})
		case 1001:
			return &EventError{Code: apierr.CodeBadRequest, Params: apierr.Params{"max": 3}}
		case 1002:
			return fmt.Errorf("the database said %s", stripeTestKey) // not one of the reasons: a failure that may pass
		}
		return nil
	}}
	r.svc.SetApplier(a)
	r.svc.drainEvents(context.Background())

	if state, attempts, code, _ := r.state(ids[0]); state != EventIgnored || attempts != 1 || code != string(apierr.CodeNotFound) {
		t.Errorf("ignored: %s, %d, %q", state, attempts, code)
	}
	var params string
	if err := r.db.QueryRow(`SELECT error_params FROM integration_events WHERE id = ?`, ids[0]).Scan(&params); err != nil || !strings.Contains(params, "late") {
		t.Errorf("the ignore's params: %q, %v", params, err)
	}
	// A refusal that doesn't pass fails at once, keeping its code.
	if state, attempts, code, _ := r.state(ids[1]); state != EventFailed || attempts != 1 || code != string(apierr.CodeBadRequest) {
		t.Errorf("refused: %s, %d, %q", state, attempts, code)
	}
	// An error of another kind is retried, and what it said is kept nowhere.
	if state, attempts, code, next := r.state(ids[2]); state != EventPending || attempts != 1 || code != string(apierr.CodeIntegrationEventGaveUp) || next <= r.svc.store.stamp(r.clock.Now()) {
		t.Errorf("transient: %s, %d, %q, next %q", state, attempts, code, next)
	}
	if strings.Contains(r.logs.String(), stripeTestKey) {
		t.Error("an applier's error text reached the log")
	}
	if state, _, _, _ := r.state(ids[3]); state != EventApplied {
		t.Errorf("the fourth: %s", state)
	}
}

func TestRetriesAreBoundedAndBackOff(t *testing.T) {
	r := newRig(t)
	id := r.put(1)[0]
	a := &applier{fn: func(StoredEvent) error { return &EventError{Code: apierr.CodeBadRequest, Transient: true} }}
	r.svc.SetApplier(a)
	var waits []time.Duration
	for range maxEventAttempts {
		before := r.clock.Now()
		r.svc.drainEvents(context.Background())
		state, _, _, next := r.state(id)
		if state == EventFailed {
			break
		}
		at, err := time.Parse(timeFormat, next)
		if err != nil {
			t.Fatal(err)
		}
		waits = append(waits, at.Sub(before))
		// Not yet due: another look does nothing.
		r.svc.drainEvents(context.Background())
		r.clock.Advance(at.Sub(before) + time.Millisecond)
	}
	state, attempts, code, _ := r.state(id)
	if state != EventFailed || attempts != maxEventAttempts || code != string(apierr.CodeBadRequest) {
		t.Fatalf("after the bound: %s, %d tries, %q", state, attempts, code)
	}
	if a.count() != maxEventAttempts {
		t.Errorf("the applier ran %d times, want %d", a.count(), maxEventAttempts)
	}
	for i := 1; i < len(waits); i++ {
		if waits[i] <= waits[i-1] && waits[i] != eventBackoffMax {
			t.Errorf("the waits don't grow: %v", waits)
		}
	}
	if waits[len(waits)-1] > eventBackoffMax {
		t.Errorf("a wait over the cap: %v", waits)
	}
	// A failed event stays failed.
	r.clock.Advance(24 * time.Hour)
	r.svc.drainEvents(context.Background())
	if a.count() != maxEventAttempts {
		t.Error("a failed event ran again")
	}
}

func TestALeaseThatRunsOutIsTakenAgain(t *testing.T) {
	r := newRig(t)
	id := r.put(1)[0]
	ctx := context.Background()

	ev, lease1, found, err := r.svc.claimEvent(ctx)
	if err != nil || !found || ev.ID != id || ev.Attempts != 1 {
		t.Fatalf("claim: %+v, %v, %v", ev, found, err)
	}
	// While the lease holds, nobody else gets it.
	if _, _, found, err := r.svc.claimEvent(ctx); err != nil || found {
		t.Fatalf("a leased event was claimed again: %v, %v", found, err)
	}
	// The worker died. Its lease runs out and another takes the event, which counts the try.
	r.clock.Advance(eventLease + time.Second)
	ev, lease2, found, err := r.svc.claimEvent(ctx)
	if err != nil || !found || ev.ID != id || ev.Attempts != 2 || lease2 == lease1 {
		t.Fatalf("reclaim: %+v, lease %d then %d, %v, %v", ev, lease1, lease2, found, err)
	}
	// The first worker comes back after all: it can't note a result for the claim it lost.
	if err := r.svc.settleEvent(ctx, ev, lease1, nil); err != nil {
		t.Fatal(err)
	}
	if state, _, _, _ := r.state(id); state != EventRunning {
		t.Fatalf("a stale worker settled the event: %s", state)
	}
	if err := r.svc.settleEvent(ctx, ev, lease2, nil); err != nil {
		t.Fatal(err)
	}
	if state, _, _, _ := r.state(id); state != EventApplied {
		t.Fatalf("the owner couldn't settle: %s", state)
	}

	// A lease that runs out on the last permitted try isn't followed by another: the bound holds.
	id2 := r.put(1)[1]
	if _, err := r.db.Exec(`UPDATE integration_events SET state = 'running', attempts = ?, lease_until = ? WHERE id = ?`,
		maxEventAttempts, r.svc.store.stamp(r.clock.Now().Add(-time.Second)), id2); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := r.svc.claimEvent(ctx); err != nil || found {
		t.Fatalf("an event past its bound was claimed: %v, %v", found, err)
	}
	if state, _, code, _ := r.state(id2); state != EventFailed || code != string(apierr.CodeIntegrationEventGaveUp) {
		t.Fatalf("past the bound: %s, %q", state, code)
	}
}

func TestOldEventsArePurged(t *testing.T) {
	r := newRig(t)
	ids := r.put(6)
	age := func(id int64, state string, by time.Duration) {
		t.Helper()
		if _, err := r.db.Exec(`UPDATE integration_events SET state = ?, received_at = ? WHERE id = ?`, state, r.svc.store.stamp(r.clock.Now().Add(-by)), id); err != nil {
			t.Fatal(err)
		}
	}
	age(ids[0], EventApplied, keepEventsDone+time.Hour) // gone
	age(ids[1], EventApplied, time.Hour)                // kept: recent
	age(ids[2], EventIgnored, keepEventsDone+time.Hour) // gone
	age(ids[3], EventFailed, keepEventsDone+time.Hour)  // kept: waits for an admin
	age(ids[4], EventFailed, keepEventsAll+time.Hour)   // gone: nobody came
	age(ids[5], EventPending, keepEventsAll+time.Hour)  // kept: the worker's
	if err := r.svc.purgeEvents(context.Background()); err != nil {
		t.Fatal(err)
	}
	var left []int64
	for _, row := range r.rows(r.in.ID) {
		left = append(left, row.ID)
	}
	if fmt.Sprint(left) != fmt.Sprint([]int64{ids[1], ids[3], ids[5]}) {
		t.Fatalf("left %v, want %v", left, []int64{ids[1], ids[3], ids[5]})
	}
}

// The worker the server starts: a delivery to the route wakes it and the event is applied, and
// stopping it waits for it.
func TestAWebhookWakesTheWorker(t *testing.T) {
	r := newRig(t)
	a := &applier{got: make(chan StoredEvent, 4)}
	r.svc.SetApplier(a)
	stop := r.svc.StartEvents(context.Background())
	stopped := false
	defer func() {
		if !stopped {
			stop()
		}
	}()

	s, ref := r.session(2500, "eur")
	r.deliver(r.send, r.must(r.fake.Pay(s.ID)))
	select {
	case ev := <-a.got:
		if ev.RefToken != ref || ev.Kind != EventPaymentSucceeded || ev.WorkspaceID != r.ws {
			t.Fatalf("the worker got %+v", ev)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the worker didn't apply the event")
	}
	// The result is noted by the worker after the applier returns.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if state, _, _, _ := r.state(r.rows(r.in.ID)[0].ID); state == EventApplied {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the event wasn't noted as applied")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Stopping waits for the worker, and nothing runs after it.
	stop()
	stopped = true
	seen := a.count()
	r.put(1)
	time.Sleep(50 * time.Millisecond)
	if a.count() != seen {
		t.Error("a stopped worker applied an event")
	}
}

// What the table holds is bound to its integration: one row per provider event id for each, and
// removed with the integration.
func TestEventsBelongToTheirIntegration(t *testing.T) {
	r := newRig(t)
	other, _, _ := r.integration(stripeBody("second"), hookFor("second"))
	ev := Event{ID: "evt_same", Kind: EventPaymentExpired, RefToken: token(), ObjectID: "cs_test_x", AmountMinor: 100, Currency: "USD", OccurredAt: r.clock.Now()}
	ctx := context.Background()
	for _, in := range []int64{r.in.ID, other.ID} {
		if got, err := r.store.record(ctx, in, ev, SourceWebhook, []byte("a")); err != nil || got != storedNew {
			t.Fatalf("first for %d: %v, %v", in, got, err)
		}
		if got, err := r.store.record(ctx, in, ev, SourceWebhook, []byte("a")); err != nil || got != storedAgain {
			t.Fatalf("again for %d: %v, %v", in, got, err)
		}
	}
	if len(r.rows(r.in.ID)) != 1 || len(r.rows(other.ID)) != 1 {
		t.Fatalf("rows: %d and %d, want one each", len(r.rows(r.in.ID)), len(r.rows(other.ID)))
	}
	// A replay of a stored event never changes it, whatever the second delivery says.
	if got, err := r.store.record(ctx, r.in.ID, Event{ID: "evt_same", Kind: EventPaymentSucceeded, Currency: "EUR", OccurredAt: r.clock.Now()}, SourceWebhook, []byte("b")); err != nil || got != storedAgain {
		t.Fatalf("a changed replay: %v, %v", got, err)
	}
	if row := r.rows(r.in.ID)[0]; row.Kind != EventPaymentExpired || row.Currency != "USD" {
		t.Fatalf("a replay rewrote the event: %+v", row)
	}
	// The table refuses what the code never writes.
	for name, q := range map[string]string{
		"an unknown state":  `UPDATE integration_events SET state = 'done'`,
		"an unknown source": `UPDATE integration_events SET source = 'email'`,
		"a short token":     `UPDATE integration_events SET ref_token = 'abc'`,
		"a negative amount": `UPDATE integration_events SET amount_minor = -1`,
		"a short hash":      `UPDATE integration_events SET body_sha256 = 'abc'`,
		"a lower currency":  `UPDATE integration_events SET currency = 'e'`,
	} {
		if _, err := r.db.Exec(q); err == nil {
			t.Errorf("the table accepted %s", name)
		}
	}
	// Deleting the integration deletes its events and no other's.
	expect(t, r.owner.do(http.MethodDelete, wsPath(r.ws, fmt.Sprintf("integrations/%d", other.ID)), nil), http.StatusNoContent)
	if len(r.rows(other.ID)) != 0 || len(r.rows(r.in.ID)) != 1 {
		t.Fatalf("after the delete: %d and %d", len(r.rows(other.ID)), len(r.rows(r.in.ID)))
	}
}

func TestEventErrorsAreCodes(t *testing.T) {
	var ee *EventError
	if err := Ignore(apierr.CodeNotFound); !errors.As(err, &ee) || !ee.ignored || ee.Error() != string(apierr.CodeNotFound) {
		t.Fatalf("Ignore = %v", err)
	}
}
