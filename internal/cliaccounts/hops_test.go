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

func TestAHopThatEndedWithItsCallerIsNoDecisionForTheNextRequest(t *testing.T) {
	e, conv, models := limitedClaudeEnv(t, true)
	e.provide(mine(e, "api"), "big")
	e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(mine(e, "api"), "big")}})
	reached := make(chan struct{})
	var calls atomic.Int32
	e.svc.SetModelSetter(func(ctx context.Context, agent int64, provider, model string) error {
		if calls.Add(1) == 1 {
			close(reached)
			<-ctx.Done()
			return ctx.Err()
		}
		return models.set(ctx, agent, provider, model)
	})
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
	d := done(e.turn(conv, limitedBody))
	if !d.Continue || len(models.refs) != 1 {
		t.Fatalf("the next request got %+v, model changes %v", d, models.refs)
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
