//go:build unix

package cliaccounts

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
	"taskto.ai/taskto/internal/conversations"
)

func (e *turnEnv) turnWith(ctx context.Context, conversation int64, b turnBody) []cliworker.Event {
	e.t.Helper()
	raw, _ := json.Marshal(b)
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/cli/turn", bytes.NewReader(raw))
	r.Header.Set("Authorization", fmt.Sprintf("Bearer conv-%d", conversation))
	rec := httptest.NewRecorder()
	e.tools.ServeHTTP(rec, r)
	var events []cliworker.Event
	sc := bufio.NewScanner(rec.Body)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var ev cliworker.Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			e.t.Errorf("event %q: %v", sc.Text(), err)
			return events
		}
		events = append(events, ev)
	}
	return events
}

func lastDone(events []cliworker.Event) cliworker.Event {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == cliworker.EventDone {
			return events[i]
		}
	}
	return cliworker.Event{}
}

var limitedBody = turnBody{Turn: "t1", CLI: "claude", Model: "default", Prompt: "do it"}

type heldWrites struct {
	models  *modelCalls
	entered chan string
	release chan struct{}
	once    sync.Once
}

func (h *heldWrites) free() { h.once.Do(func() { close(h.release) }) }

func holdWrites(e *turnEnv, models *modelCalls, only string) *heldWrites {
	h := &heldWrites{models: models, entered: make(chan string, 8), release: make(chan struct{})}
	e.t.Cleanup(h.free)
	e.svc.SetModelSetter(func(ctx context.Context, agent int64, provider, model string) error {
		if only == "" || only == provider {
			h.entered <- provider
			<-h.release
		}
		return models.set(ctx, agent, provider, model)
	})
	return h
}

func TestAReattachDuringTheModelWriteTakesTheDecisionOfTheFirstRequest(t *testing.T) {
	e, conv, models := limitedClaudeEnv(t, true)
	e.provide(mine(e, "api"), "big")
	e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(mine(e, "api"), "big")}})
	held := holdWrites(e, models, "")
	results := make(chan []cliworker.Event, 2)
	go func() { results <- e.turn(conv, limitedBody) }()
	<-held.entered
	e.owner.setSwitching(map[string]any{"enabled": false, "queue": []map[string]any{}})
	go func() { results <- e.turn(conv, limitedBody) }()
	select {
	case got := <-results:
		t.Fatalf("a reattach ended while the first request's model write was still going: %+v", lastDone(got))
	case <-time.After(300 * time.Millisecond):
	}
	held.free()
	first, second := lastDone(<-results), lastDone(<-results)
	if !first.Continue || !second.Continue || first.Seq != second.Seq || first.Note != second.Note {
		t.Fatalf("first %+v, reattach %+v", first, second)
	}
	if len(models.refs) != 1 {
		t.Fatalf("model changes = %v", models.refs)
	}
}

func TestEveryModelWriteChecksTheOwnerAndTheCatalogAgain(t *testing.T) {
	losses := map[string]func(e *turnEnv, second string){
		"the owner left the workspace": func(e *turnEnv, _ string) {
			if _, err := e.db.Exec(`DELETE FROM memberships WHERE user_id = ?`, e.owner.id); err != nil {
				e.t.Fatal(err)
			}
		},
		"switching was turned off": func(e *turnEnv, _ string) {
			if _, err := e.db.Exec(`UPDATE cli_switching SET enabled = 0 WHERE user_id = ?`, e.owner.id); err != nil {
				e.t.Fatal(err)
			}
		},
		"the next provider was deleted": func(e *turnEnv, second string) {
			if _, err := e.db.Exec(`DELETE FROM provider_accounts WHERE name = ?`, second); err != nil {
				e.t.Fatal(err)
			}
		},
	}
	for name, lose := range losses {
		t.Run(name, func(t *testing.T) {
			e, conv, models := limitedClaudeEnv(t, true)
			first, second := mine(e, "first"), mine(e, "second")
			e.provide(first, "a")
			e.provide(second, "b")
			e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(first, "a"), modelEntry(second, "b")}})
			e.svc.SetModelSetter(func(ctx context.Context, agent int64, provider, model string) error {
				if provider == first {
					lose(e, "second")
					return errors.New("refused")
				}
				return models.set(ctx, agent, provider, model)
			})
			d := done(e.turn(conv, limitedBody))
			if d.Continue || len(models.refs) != 0 {
				t.Fatalf("done = %+v, model changes %v", d, models.refs)
			}
		})
	}
}

func TestAHopOfOneUserDoesNotWaitForAnotherUsersModelWrite(t *testing.T) {
	e, ownerConv, models := limitedClaudeEnv(t, true)
	e.provide(mine(e, "api"), "big")
	e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(mine(e, "api"), "big")}})
	theirs := fmt.Sprintf("u%d-api", e.other.id)
	e.provide(theirs, "big")
	e.other.connect(cliworker.Claude)
	clitest.WriteScenario(t, e.dirs(e.other.id).Config(cliworker.Claude), clitest.Scenario{Fail: "You've hit your usage limit · resets 3pm"})
	e.other.setSwitching(map[string]any{"enabled": true, "queue": []map[string]any{{"account": "claude"}, modelEntry(theirs, "big")}})
	held := holdWrites(e, models, mine(e, "api"))
	blocked := make(chan struct{})
	go func() {
		defer close(blocked)
		e.turn(ownerConv, limitedBody)
	}()
	<-held.entered
	finished := make(chan cliworker.Event, 1)
	go func() { finished <- done(e.turn(e.conversation(e.other), limitedBody)) }()
	select {
	case d := <-finished:
		if !d.Continue {
			t.Fatalf("the other user's turn = %+v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("another user's hop waited for a model write that was not its own")
	}
	held.free()
	<-blocked
}

func TestAWaitingReattachLeavesWhenItsCallerDoes(t *testing.T) {
	e, conv, models := limitedClaudeEnv(t, true)
	e.provide(mine(e, "api"), "big")
	e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(mine(e, "api"), "big")}})
	held := holdWrites(e, models, "")
	first := make(chan struct{})
	go func() {
		defer close(first)
		e.turn(conv, limitedBody)
	}()
	<-held.entered
	ctx, cancel := context.WithCancel(context.Background())
	left := make(chan struct{})
	go func() {
		defer close(left)
		e.turnWith(ctx, conv, limitedBody)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-left:
	case <-time.After(5 * time.Second):
		t.Fatal("a reattach whose caller was gone kept waiting for another request's model write")
	}
	held.free()
	<-first
	if len(models.refs) != 1 {
		t.Fatalf("model changes = %v", models.refs)
	}
}

func TestAHopThatEndedBeforeAnyWriteIsNoDecisionForTheNextRequest(t *testing.T) {
	e, conv, models := limitedClaudeEnv(t, true)
	e.provide(mine(e, "api"), "big")
	e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(mine(e, "api"), "big")}})
	var writes atomic.Int32
	e.svc.SetModelSetter(func(ctx context.Context, agent int64, provider, model string) error {
		writes.Add(1)
		return models.set(ctx, agent, provider, model)
	})
	p, err := conversations.PrincipalOf(context.Background(), e.db, conv)
	if err != nil {
		t.Fatal(err)
	}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if hop := e.svc.hopOnce(gone, p, "turn-x", "stopped", "note"); hop != nil || writes.Load() != 0 {
		t.Fatalf("a hop whose caller was gone wrote %d models and returned %+v", writes.Load(), hop)
	}
	if hop := e.svc.hopOnce(context.Background(), p, "turn-x", "stopped", "note"); hop == nil || len(models.refs) != 1 {
		t.Fatalf("the next request got %+v, model changes %v", hop, models.refs)
	}
}

func TestAReconnectAnnouncesNoModelWhileAnEarlierWriteMayStillLand(t *testing.T) {
	e, conv, models := limitedClaudeEnv(t, true)
	first, second := mine(e, "first"), mine(e, "second")
	e.provide(first, "a")
	e.provide(second, "b")
	e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(first, "a"), modelEntry(second, "b")}})
	reached, land := make(chan struct{}), make(chan struct{})
	landed := make(chan struct{})
	var calls atomic.Int32
	e.svc.SetModelSetter(func(ctx context.Context, agent int64, provider, model string) error {
		if calls.Add(1) > 1 {
			return models.set(ctx, agent, provider, model)
		}
		close(reached)
		<-ctx.Done()
		go func() {
			defer close(landed)
			<-land
			_ = models.set(context.WithoutCancel(ctx), agent, provider, model)
		}()
		return ctx.Err()
	})
	e.svc.SetModelReader(func(context.Context, int64) (string, string, error) { return "cli-claude", "default", nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	left := make(chan struct{})
	go func() {
		defer close(left)
		e.turnWith(ctx, conv, limitedBody)
	}()
	<-reached
	cancel()
	<-left
	e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(second, "b")}})
	events := e.turn(conv, limitedBody)
	close(land)
	<-landed
	if d := lastDone(events); d.Continue || d.Note != "" {
		t.Fatalf("the reconnect announced %+v while the first write could still land", d)
	}
	for _, ev := range events {
		if ev.Type == cliworker.EventNotice {
			t.Fatalf("the reconnect announced a model: %+v", ev)
		}
	}
	if len(models.refs) != 1 || models.refs[0] != first+"/a" {
		t.Fatalf("model changes = %v, want only the first write that landed late", models.refs)
	}
}

func TestAModelWriteEndsWithItsCallerAndIsBounded(t *testing.T) {
	t.Run("the caller leaves", func(t *testing.T) {
		e, conv, _ := limitedClaudeEnv(t, true)
		e.provide(mine(e, "api"), "big")
		e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(mine(e, "api"), "big")}})
		reached, ended := make(chan struct{}), make(chan struct{})
		e.svc.SetModelSetter(func(ctx context.Context, _ int64, _, _ string) error {
			close(reached)
			<-ctx.Done()
			close(ended)
			return ctx.Err()
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go e.turnWith(ctx, conv, limitedBody)
		<-reached
		cancel()
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
			t.Fatal("the model write outlived the caller that asked for it")
		}
	})
	t.Run("the runtime never answers", func(t *testing.T) {
		e, conv, models := limitedClaudeEnv(t, true)
		e.provide(mine(e, "api"), "big")
		e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(mine(e, "api"), "big")}})
		e.svc.modelWait = 100 * time.Millisecond
		e.svc.SetModelSetter(func(ctx context.Context, _ int64, _, _ string) error {
			<-ctx.Done()
			return ctx.Err()
		})
		finished := make(chan cliworker.Event, 1)
		go func() { finished <- done(e.turn(conv, limitedBody)) }()
		select {
		case d := <-finished:
			if d.Continue || len(models.refs) != 0 {
				t.Fatalf("done = %+v", d)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a model write that never answers held the turn")
		}
	})
}

func TestNoHopWhileTheRuntimeKeepsTheModelOfAContinuedReply(t *testing.T) {
	e, conv, models := limitedClaudeEnv(t, true)
	first, second := mine(e, "first"), mine(e, "second")
	e.provide(first, "a")
	e.provide(second, "b")
	e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(first, "a"), modelEntry(second, "b")}})
	var tried []string
	e.svc.SetModelSetter(func(_ context.Context, _ int64, provider, _ string) error {
		tried = append(tried, provider)
		return fmt.Errorf("conflict: %w", ErrModelBusy)
	})
	d := done(e.turn(conv, limitedBody))
	if d.Continue || d.Note != "" || len(tried) != 1 || len(models.refs) != 0 {
		t.Fatalf("done = %+v, tried %v, model changes %v", d, tried, models.refs)
	}
}

func TestAnUncertainModelWriteIsReadBackBeforeAnyOtherModelIsTried(t *testing.T) {
	type outcome struct {
		cont  bool
		tried []string
		reads int
	}
	run := func(t *testing.T, setErr error, committed bool, readErr error) outcome {
		e, conv, _ := limitedClaudeEnv(t, true)
		first, second := mine(e, "first"), mine(e, "second")
		e.provide(first, "a")
		e.provide(second, "b")
		e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(first, "a"), modelEntry(second, "b")}})
		var mu sync.Mutex
		var out outcome
		stored := [2]string{"unchanged", "unchanged"}
		e.svc.SetModelSetter(func(_ context.Context, _ int64, provider, model string) error {
			mu.Lock()
			defer mu.Unlock()
			out.tried = append(out.tried, provider)
			if committed {
				stored = [2]string{provider, model}
			}
			return setErr
		})
		e.svc.SetModelReader(func(_ context.Context, _ int64) (string, string, error) {
			mu.Lock()
			defer mu.Unlock()
			out.reads++
			return stored[0], stored[1], readErr
		})
		out.cont = done(e.turn(conv, limitedBody)).Continue
		return out
	}
	t.Run("a write that landed before its answer was lost is the hop", func(t *testing.T) {
		for name, err := range map[string]error{"a deadline": context.DeadlineExceeded, "a lost connection": fmt.Errorf("closed: %w", ErrModelUncertain)} {
			got := run(t, err, true, nil)
			if !got.cont || len(got.tried) != 1 || got.reads != 1 {
				t.Fatalf("%s after the commit: %+v, want the first model kept as the hop with no second write", name, got)
			}
		}
	})
	t.Run("a write that is not confirmed ends the hop with no other write", func(t *testing.T) {
		for name, got := range map[string]outcome{
			"not landed":     run(t, context.DeadlineExceeded, false, nil),
			"read failed":    run(t, context.DeadlineExceeded, true, errors.New("unreachable")),
			"lost then read": run(t, fmt.Errorf("closed: %w", ErrModelUncertain), false, nil),
		} {
			if got.cont || len(got.tried) != 1 || got.reads != 1 {
				t.Fatalf("%s: %+v, want no continuation and no second write", name, got)
			}
		}
	})
	t.Run("an answer that says no is still a skip", func(t *testing.T) {
		got := run(t, errors.New("unknown model"), false, nil)
		if got.cont || len(got.tried) != 2 || got.reads != 0 {
			t.Fatalf("a refused write: %+v, want the second model tried and no read", got)
		}
	})
}

func TestAStopThatLandsDuringTheModelWriteSendsNoContinuation(t *testing.T) {
	e, conv, models := limitedClaudeEnv(t, true)
	e.provide(mine(e, "api"), "big")
	e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(mine(e, "api"), "big")}})
	id := fmt.Sprintf("c%d-t1", conv)
	e.svc.SetModelSetter(func(ctx context.Context, agent int64, provider, model string) error {
		e.svc.markStopped(id)
		return models.set(ctx, agent, provider, model)
	})
	events := e.turn(conv, limitedBody)
	if d := lastDone(events); d.Continue || d.Note != "" {
		t.Fatalf("done = %+v, want the failure the person stopped, with no continuation", d)
	}
	for _, ev := range events {
		if ev.Type == cliworker.EventNotice {
			t.Fatalf("a stopped turn announced a hop: %+v", ev)
		}
	}
}

func TestAPinnedTurnIsNotMovedToAQueuedModelByALimit(t *testing.T) {
	e, conv, models := limitedClaudeEnv(t, true)
	e.provide(mine(e, "api"), "big")
	e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(mine(e, "api"), "big")}})
	pinned := limitedBody
	pinned.Pinned = true
	events := e.turn(conv, pinned)
	if d := lastDone(events); d.Continue || d.Note != "" || d.Status != cliworker.StatusFailed {
		t.Fatalf("done = %+v, want the limit's failure with no continuation", d)
	}
	for _, ev := range events {
		if ev.Type == cliworker.EventNotice {
			t.Fatalf("a pinned turn announced a hop: %+v", ev)
		}
	}
	if len(models.refs) != 0 {
		t.Fatalf("a pinned turn wrote the model %v", models.refs)
	}
	unpinned := limitedBody
	unpinned.Turn = "t2"
	if !lastDone(e.turn(conv, unpinned)).Continue {
		t.Fatal("the same limit without the pin no longer hops: the test shows nothing")
	}
}
