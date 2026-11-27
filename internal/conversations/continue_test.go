package conversations

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/apierr"
)

func (e *env) resumed(c Conversation) int {
	e.t.Helper()
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	return e.rt.resumes[c.agentIDForTest(e.t, e)]
}

func TestOwnerContinuesAConversation(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	got := decode[SubmissionReply](t, owner.expect(http.StatusAccepted, http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), map[string]any{}))
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
	member.expect(http.StatusNotFound, http.MethodPost, path(ws, url), map[string]any{})
	owner.expect(http.StatusNotFound, http.MethodPost, path(other, url), map[string]any{})
	outsider.expect(http.StatusNotFound, http.MethodPost, path(other, url), map[string]any{})
	owner.expect(http.StatusNotFound, http.MethodPost, path(ws, "/999999/continue"), map[string]any{})
	owner.expect(http.StatusNotFound, http.MethodPost, path(ws, "/x/continue"), map[string]any{})
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
	rec := owner.expect(http.StatusConflict, http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), map[string]any{})
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
	rec := owner.expect(http.StatusConflict, http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), map[string]any{})
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
	owner.expect(http.StatusConflict, http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), map[string]any{})
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
	owner.expect(http.StatusGone, http.MethodPost, path(ws, fmt.Sprintf("/%d/continue", c.ID)), map[string]any{})
}
