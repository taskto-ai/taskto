package conversations

import (
	"context"
	"fmt"
	"net/http"
	"testing"

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
	e.svc.SetContinueModels(func(context.Context, int64, int64) ([]providers.Choice, error) {
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
