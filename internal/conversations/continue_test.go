package conversations

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/providers"
)

func (e *env) resumed(c Conversation) int {
	e.t.Helper()
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	return e.rt.resumes[c.agentIDForTest(e.t, e)]
}

func (e *env) continueBody(provider, model string, after int64) map[string]any {
	return map[string]any{"model": map[string]any{"provider": provider, "model_id": model}, "after_entry": after}
}

func (e *env) continues() []continueCall {
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	return append([]continueCall(nil), e.rt.resumed...)
}

func TestOwnerContinuesAConversation(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	got := decode[SubmissionReply](t, owner.expect(http.StatusAccepted, http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), e.continueBody("script", "script-1", 7)))
	if got.SubmissionID == 0 || e.resumed(c) != 1 {
		t.Fatalf("continue = %+v, resumed %d times; want a submission and one call", got, e.resumed(c))
	}
	e.rt.mu.Lock()
	inputs := len(e.rt.inputs)
	e.rt.mu.Unlock()
	if inputs != 0 {
		t.Fatalf("a continue sent %d inputs, want none", inputs)
	}
}

func TestAnotherUserCannotContinueAConversation(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	member := e.invite(owner, ws, "member@example.com")
	outsider := e.invite(owner, ws, "outsider@example.com")
	other := e.workspace(owner, outsider)
	c := owner.create(ws, nil)
	url := fmt.Sprintf("/%d/continue", c.ID)
	member.expect(http.StatusNotFound, http.MethodPost, path(ws, url), e.continueBody("script", "script-1", 7))
	owner.expect(http.StatusNotFound, http.MethodPost, path(other, url), e.continueBody("script", "script-1", 7))
	outsider.expect(http.StatusNotFound, http.MethodPost, path(other, url), e.continueBody("script", "script-1", 7))
	owner.expect(http.StatusNotFound, http.MethodPost, path(ws, "/999999/continue"), e.continueBody("script", "script-1", 7))
	owner.expect(http.StatusNotFound, http.MethodPost, path(ws, "/x/continue"), e.continueBody("script", "script-1", 7))
	if got := e.resumed(c); got != 0 {
		t.Fatalf("the agent runtime was asked to continue %d times, want none", got)
	}
}

func TestContinueIsRefusedForAnArchivedConversation(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	owner.expect(http.StatusOK, http.MethodPatch, path(ws, fmt.Sprintf("/%d", c.ID)), map[string]any{"archived": true})
	e.svc.agent = func(context.Context) (Runtime, error) {
		t.Error("the agent runtime was reached for an archived conversation")
		return e.rt, nil
	}
	rec := owner.expect(http.StatusConflict, http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), e.continueBody("script", "script-1", 7))
	if code := decode[map[string]string](t, rec)["code"]; code != string(apierr.CodeConversationArchived) {
		t.Fatalf("code = %q", code)
	}
	if got := e.resumed(c); got != 0 {
		t.Fatalf("an archived conversation was continued %d times", got)
	}
}

func TestContinueOfABusyConversationIsRefused(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	e.rt.mu.Lock()
	e.rt.err = &agentrt.StatusError{Code: 409, Message: "the conversation is busy", Reason: "busy"}
	e.rt.mu.Unlock()
	rec := owner.expect(http.StatusConflict, http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), e.continueBody("script", "script-1", 7))
	if code := decode[map[string]string](t, rec)["code"]; code != string(apierr.CodeConversationBusy) {
		t.Fatalf("code = %q, want %q", code, apierr.CodeConversationBusy)
	}
}

func TestContinueRechecksTheArchiveBeforeTheCall(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	e.svc.agent = func(context.Context) (Runtime, error) {
		if _, err := e.db.Exec(`UPDATE conversations SET archived_at = 'now' WHERE id = ?`, c.ID); err != nil {
			t.Error(err)
		}
		return e.rt, nil
	}
	owner.expect(http.StatusConflict, http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), e.continueBody("script", "script-1", 7))
	if got := e.resumed(c); got != 0 {
		t.Fatalf("the agent runtime was asked to continue %d times after the archive, want none", got)
	}
}

func TestContinueOfALostConversationIsGone(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	e.rt.mu.Lock()
	e.rt.err = &agentrt.StatusError{Code: 404, Message: "unknown conversation"}
	e.rt.mu.Unlock()
	owner.expect(http.StatusGone, http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), e.continueBody("script", "script-1", 7))
}

func TestContinueRunsOnTheModelItWasGiven(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	owner.expect(http.StatusAccepted, http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), e.continueBody("backup", "b-1", 9))
	got := e.continues()
	want := continueCall{model: agentrt.ModelRef{Provider: "backup", ModelID: "b-1"}, after: 9}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("the runtime was asked %+v, want [%+v]", got, want)
	}
	e.rt.mu.Lock()
	sets := e.rt.sets
	e.rt.mu.Unlock()
	if sets != 0 {
		t.Fatalf("the continue wrote the conversation's model on its own: %d model writes", sets)
	}
}

func TestContinueNeedsAModelAndTheEntryItAnswers(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	url := path(ws, fmt.Sprintf("/%d/continue", c.ID))
	owner.expect(http.StatusBadRequest, http.MethodPost, url, map[string]any{})
	owner.expect(http.StatusBadRequest, http.MethodPost, url, e.continueBody("script", "script-1", 0))
	owner.expect(http.StatusBadRequest, http.MethodPost, url, map[string]any{"after_entry": 3})
	if got := e.continues(); len(got) != 0 {
		t.Fatalf("the runtime was asked to continue after refused requests: %+v", got)
	}
}

func TestContinueOfAStaleFailureIsRefused(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	e.rt.mu.Lock()
	e.rt.err = &agentrt.StatusError{Code: 409, Message: "the conversation moved on", Reason: "moved"}
	e.rt.mu.Unlock()
	rec := owner.expect(http.StatusConflict, http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), e.continueBody("script", "script-1", 7))
	if code := decode[map[string]string](t, rec)["code"]; code != string(apierr.CodeConversationMoved) {
		t.Fatalf("code = %q, want %q", code, apierr.CodeConversationMoved)
	}
}

func TestContinueOnAModelTheOwnerCannotRunIsRefusedBeforeTheRuntime(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	url := path(ws, fmt.Sprintf("/%d/continue", c.ID))
	e.runnable = e.runnable[:1]
	rec := owner.expect(http.StatusConflict, http.MethodPost, url, e.continueBody("backup", "b-1", 7))
	if code := decode[map[string]string](t, rec)["code"]; code != string(apierr.CodeModelUnavailable) {
		t.Fatalf("code = %q, want %q", code, apierr.CodeModelUnavailable)
	}
	owner.expect(http.StatusConflict, http.MethodPost, url, e.continueBody("script", "no-such-model", 7))
	owner.expect(http.StatusBadRequest, http.MethodPost, url, e.continueBody("u999-mine", "m", 7))
	if got := e.continues(); len(got) != 0 {
		t.Fatalf("the runtime was asked to continue on a model that can't be run: %+v", got)
	}
}

func TestContinueModelsAreThoseTheOwnerCanRunWhenListedAndWhenContinuing(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	listing := path(ws, fmt.Sprintf("/%d/continue-models", c.ID))
	names := func() []string {
		var out []string
		for _, p := range decode[ContinueModels](t, owner.expect(http.StatusOK, http.MethodGet, listing, nil)).Providers {
			out = append(out, p.Provider)
		}
		return out
	}
	if got := names(); len(got) != 2 {
		t.Fatalf("listed %v, want both providers", got)
	}
	e.runnable = []providers.Choice{e.runnable[0]}
	if got := names(); len(got) != 1 || got[0] != "script" {
		t.Fatalf("listed %v after the other became unusable, want [script]", got)
	}
	owner.expect(http.StatusConflict, http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), e.continueBody("backup", "b-1", 7))
}

func TestAnotherUserCannotListAConversationsContinueModels(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	member := e.invite(owner, ws, "member@example.com")
	c := owner.create(ws, nil)
	member.expect(http.StatusNotFound, http.MethodGet, path(ws, fmt.Sprintf("/%d/continue-models", c.ID)), nil)
	owner.expect(http.StatusNotFound, http.MethodGet, path(ws, "/999999/continue-models"), nil)
}

func TestContinueRechecksTheArchiveAfterLookingUpTheModels(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	e.svc.SetContinueModels(func(context.Context, Principal) ([]providers.Choice, error) {
		if _, err := e.db.Exec(`UPDATE conversations SET archived_at = 'now' WHERE id = ?`, c.ID); err != nil {
			t.Error(err)
		}
		return e.runnable, nil
	})
	owner.expect(http.StatusConflict, http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), e.continueBody("script", "script-1", 7))
	if got := e.continues(); len(got) != 0 {
		t.Fatalf("the runtime was asked to continue an archived conversation: %+v", got)
	}
}

func TestAContinuedRunIsCountedInUsage(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	clearUsageMark(t, e, c.ID)
	owner.expect(http.StatusAccepted, http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), e.continueBody("script", "script-1", 7))
	var marked bool
	if err := e.db.QueryRow(`SELECT usage_mark IS NOT NULL FROM conversations WHERE id = ?`, c.ID).Scan(&marked); err != nil {
		t.Fatal(err)
	}
	if !marked {
		t.Fatal("a continue left the conversation without a usage mark, so its run's tokens are never counted")
	}
}

func TestContinueRechecksTheArchiveAfterTheUsageLookup(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	clearUsageMark(t, e, c.ID)
	e.rt.onEntries = func() {
		if _, err := e.db.Exec(`UPDATE conversations SET archived_at = 'now' WHERE id = ?`, c.ID); err != nil {
			t.Error(err)
		}
	}
	owner.expect(http.StatusConflict, http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), e.continueBody("script", "script-1", 7))
	if got := e.continues(); len(got) != 0 {
		t.Fatalf("the runtime was asked to continue an archive that came during the usage lookup: %+v", got)
	}
}

func TestAnArchiveWaitsForAContinueThatPassedItsCheck(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	archived := make(chan struct{})
	e.rt.onContinue = func() {
		go func() {
			owner.expect(http.StatusOK, http.MethodPatch, path(ws, fmt.Sprintf("/%d", c.ID)), map[string]any{"archived": true})
			close(archived)
		}()
		select {
		case <-archived:
			t.Error("an archive finished while a continue that had passed its archive check was still being admitted")
		case <-time.After(300 * time.Millisecond):
		}
	}
	owner.expect(http.StatusAccepted, http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), e.continueBody("script", "script-1", 7))
	waitFor(t, "the archive to finish after the continue", func() bool {
		select {
		case <-archived:
			return true
		default:
			return false
		}
	})
}

func TestContinueAsksForTheModelsOfTheConversationItContinues(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	var asked []Principal
	var mu sync.Mutex
	e.svc.SetContinueModels(func(_ context.Context, p Principal) ([]providers.Choice, error) {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, p)
		return e.runnable, nil
	})
	owner.expect(http.StatusOK, http.MethodGet, path(ws, fmt.Sprintf("/%d/continue-models", c.ID)), nil)
	owner.expect(http.StatusAccepted, http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), e.continueBody("script", "script-1", 7))
	want := Principal{Conversation: c.ID, AgentConversation: c.agentIDForTest(t, e), Workspace: ws, User: owner.id}
	if len(asked) != 2 || asked[0] != want || asked[1] != want {
		t.Fatalf("the models were asked for %+v, want the conversation %+v each time", asked, want)
	}
}

func TestAContinuedRunIsCountedInTheDailyUsageWhenItSettles(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	agent := c.agentIDForTest(t, e)
	clearUsageMark(t, e, c.ID)
	e.rt.rawEntries = map[int64][]json.RawMessage{agent: {json.RawMessage(`{"id":1,"kind":"pi.user","model":[{"role":"user","content":"hi"}]}`)}}
	e.rt.settle = make(chan struct{})
	owner.expect(http.StatusAccepted, http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), e.continueBody("script", "script-1", 1))
	e.rt.mu.Lock()
	e.rt.rawEntries[agent] = append(e.rt.rawEntries[agent], usageEntries(2, 2)...)
	e.rt.mu.Unlock()
	close(e.rt.settle)
	waitFor(t, "the continued reply's tokens to be counted", func() bool {
		input, output, _ := agentUsage(t, e, owner.id)
		return input == 3 && output == 2
	})
}

func TestTheReplyOfAContinueNamesTheModelTheRuntimeAdmittedItOn(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	got := decode[SubmissionReply](t, owner.expect(http.StatusAccepted, http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), e.continueBody("backup", "b-1", 7)))
	if got.Model != (modelRef{Provider: "backup", ModelID: "b-1"}) {
		t.Fatalf("the reply names %+v, want the model the run was admitted on", got.Model)
	}
}

func (c *client) sendCancelled(ctx context.Context, method, path string, body any) <-chan *httptest.ResponseRecorder {
	c.e.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		c.e.t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(raw)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.AddCookie(c.cookie)
	done := make(chan *httptest.ResponseRecorder, 1)
	rec := httptest.NewRecorder()
	go func() {
		c.e.h.ServeHTTP(rec, r)
		done <- rec
	}()
	return done
}

func TestACancelledRequestStopsWaitingForTheAdmissionLock(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	unlock, ok := e.svc.lock(context.Background(), c.ID)
	if !ok {
		t.Fatal("the lock of a free conversation was refused")
	}
	requests := map[string]struct {
		method, path string
		body         any
	}{
		"an archive": {http.MethodPatch, path(ws, fmt.Sprintf("/%d", c.ID)), map[string]any{"archived": true}},
		"a continue": {http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), e.continueBody("script", "script-1", 7)},
	}
	for name, q := range requests {
		ctx, cancel := context.WithCancel(context.Background())
		done := owner.sendCancelled(ctx, q.method, q.path, q.body)
		select {
		case <-done:
			t.Fatalf("%s finished while the lock was held", name)
		case <-time.After(200 * time.Millisecond):
		}
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s kept waiting for the lock after its request was cancelled", name)
		}
	}
	unlock()
	if got := e.continues(); len(got) != 0 {
		t.Fatalf("a cancelled continue reached the runtime: %+v", got)
	}
	owner.expect(http.StatusOK, http.MethodPatch, path(ws, fmt.Sprintf("/%d", c.ID)), map[string]any{"archived": true})
	e.svc.mu.Lock()
	left := len(e.svc.guards)
	e.svc.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d conversation locks are left after every waiter went", left)
	}
}

func (e *env) holdsOnModel(id int64, users int) bool {
	e.svc.mu.Lock()
	defer e.svc.mu.Unlock()
	h := e.svc.modelHolds[id]
	return h != nil && h.users == users
}

func TestAContinueCannotPassAMentionBetweenItsSwitchAndItsSend(t *testing.T) {
	e := newEnv(t, false)
	e.svc.modelWait = 50 * time.Millisecond
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	agent := c.agentIDForTest(t, e)
	mentioned := agentrt.ModelRef{Provider: "cli-codex", ModelID: "default"}

	submitting, release := make(chan struct{}), make(chan struct{})
	e.rt.mu.Lock()
	e.rt.onSubmit = func() {
		close(submitting)
		<-release
	}
	e.rt.mu.Unlock()

	sent := make(chan int, 1)
	go func() {
		sent <- owner.do(http.MethodPost, path(ws, fmt.Sprintf("/%d/messages", c.ID)), mention(mentioned.Provider, mentioned.ModelID, "hello")).Code
	}()
	<-submitting

	rec := owner.do(http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), e.continueBody("backup", "b-1", 7))
	if rec.Code != http.StatusConflict || apiErrorCode(t, rec.Body.Bytes()) != string(apierr.CodeConversationBusy) {
		close(release)
		t.Fatalf("a continue during a mention's switch and send answered %d %s, want 409 conversation_busy", rec.Code, rec.Body.String())
	}
	if got := e.continues(); len(got) != 0 {
		close(release)
		t.Fatalf("the runtime was asked to continue between a mention's switch and send: %+v", got)
	}
	close(release)
	if code := <-sent; code != http.StatusAccepted {
		t.Fatalf("the mention answered %d, want 202", code)
	}
	e.rt.mu.Lock()
	got := e.rt.models[agent]
	e.rt.mu.Unlock()
	if got != mentioned {
		t.Fatalf("the model is %+v after the mention, want the one it named, %+v", got, mentioned)
	}
	owner.expect(http.StatusAccepted, http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), e.continueBody("backup", "b-1", 7))
}

func TestAMentionWaitsForAContinueBeingAdmitted(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)

	admitting, release := make(chan struct{}), make(chan struct{})
	e.rt.onContinue = func() {
		close(admitting)
		<-release
	}
	continued := make(chan int, 1)
	go func() {
		continued <- owner.do(http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), e.continueBody("backup", "b-1", 7)).Code
	}()
	<-admitting

	sent := make(chan int, 1)
	go func() {
		sent <- owner.do(http.MethodPost, path(ws, fmt.Sprintf("/%d/messages", c.ID)), mention("cli-codex", "default", "hello")).Code
	}()
	for waiting := false; !waiting; {
		select {
		case code := <-sent:
			close(release)
			t.Fatalf("a mention answered %d while a continue was being admitted", code)
		default:
			waiting = e.holdsOnModel(c.ID, 2)
			runtime.Gosched()
		}
	}
	e.rt.mu.Lock()
	switched := e.rt.sets
	e.rt.mu.Unlock()
	if switched != 0 {
		close(release)
		t.Fatalf("a mention switched the model %d times while a continue was being admitted", switched)
	}
	close(release)
	if code := <-continued; code != http.StatusAccepted {
		t.Fatalf("the continue answered %d, want 202", code)
	}
	if code := <-sent; code != http.StatusAccepted {
		t.Fatalf("the mention answered %d, want 202", code)
	}
}

func TestAStalledContinueEndsAtItsDeadlineAndFreesTheHold(t *testing.T) {
	e := newEnv(t, false)
	e.svc.holdLimit = 50 * time.Millisecond
	e.svc.settleLimit = 50 * time.Millisecond
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	e.rt.stallContinue = true

	done := make(chan int, 1)
	go func() {
		done <- owner.do(http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), e.continueBody("backup", "b-1", 7)).Code
	}()
	select {
	case code := <-done:
		if code != http.StatusServiceUnavailable {
			t.Fatalf("the stalled continue answered %d, want 503", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the stalled continue never ended")
	}
	for end := time.Now().Add(2 * time.Second); e.holdsOnModel(c.ID, 1) && time.Now().Before(end); {
		runtime.Gosched()
	}
	e.svc.mu.Lock()
	holds := len(e.svc.modelHolds)
	e.svc.mu.Unlock()
	if holds != 0 {
		t.Fatalf("%d model holds are left after the stalled continue ended", holds)
	}
	owner.expect(http.StatusOK, http.MethodPut, path(ws, fmt.Sprintf("/%d/model", c.ID)), map[string]any{"provider": "backup", "model_id": "b-1"})
}

func TestAContinueThatTimedOutKeepsTheHoldUntilTheRuntimeAnswersAgain(t *testing.T) {
	e := newEnv(t, false)
	e.svc.holdLimit = 50 * time.Millisecond
	e.svc.modelWait = 5 * time.Second
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	agent := c.agentIDForTest(t, e)
	mentioned := agentrt.ModelRef{Provider: "cli-codex", ModelID: "default"}
	e.rt.mu.Lock()
	e.rt.lateContinue = true
	e.rt.confirm = make(chan struct{})
	e.rt.mu.Unlock()

	rec := owner.do(http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), e.continueBody("backup", "b-1", 7))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("the stalled continue answered %d, want 503", rec.Code)
	}

	submitting, release := make(chan struct{}), make(chan struct{})
	e.rt.mu.Lock()
	e.rt.onSubmit = func() {
		close(submitting)
		<-release
	}
	e.rt.mu.Unlock()
	sent := make(chan int, 1)
	go func() {
		sent <- owner.do(http.MethodPost, path(ws, fmt.Sprintf("/%d/messages", c.ID)), mention(mentioned.Provider, mentioned.ModelID, "hello")).Code
	}()
	for waiting := false; !waiting; {
		select {
		case <-submitting:
			waiting = true
		default:
			waiting = e.holdsOnModel(c.ID, 2)
			runtime.Gosched()
		}
	}
	e.rt.landLate()
	close(e.rt.confirm)
	close(release)
	if code := <-sent; code != http.StatusAccepted {
		t.Fatalf("the mention answered %d, want 202", code)
	}
	e.rt.mu.Lock()
	got := e.rt.models[agent]
	e.rt.mu.Unlock()
	if got != mentioned {
		t.Fatalf("the model is %+v after the mention, want %+v: a continue that timed out landed after the mention's switch", got, mentioned)
	}
}
