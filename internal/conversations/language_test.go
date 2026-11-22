package conversations

import (
	"fmt"
	"net/http"
	"testing"
)

// The agent's language: the user's pick, else the browser's, else the workspace's default,
// else none.
func TestSendCarriesTheLanguage(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	msgs := path(ws, fmt.Sprintf("/%d/messages", c.ID))
	send := func(body map[string]any) string {
		t.Helper()
		body["content"] = "hi"
		owner.expect(http.StatusAccepted, http.MethodPost, msgs, body)
		e.rt.mu.Lock()
		defer e.rt.mu.Unlock()
		inputs := e.rt.inputs[c.agentIDForTest(t, e)]
		return inputs[len(inputs)-1].Language
	}

	if got := send(map[string]any{}); got != "" {
		t.Errorf("nothing known: %q, want none", got)
	}
	if got := send(map[string]any{"browser_locale": "es-419"}); got != "es-419" {
		t.Errorf("the browser's: %q", got)
	}
	owner.expect(http.StatusOK, http.MethodPatch, fmt.Sprintf("/api/workspaces/%d", ws), map[string]any{"locale": "de"})
	if got := send(map[string]any{}); got != "de" {
		t.Errorf("the workspace's: %q", got)
	}
	if got := send(map[string]any{"browser_locale": "es-419"}); got != "es-419" {
		t.Errorf("the browser's over the workspace's: %q", got)
	}
	owner.expect(http.StatusOK, http.MethodPatch, "/api/me", map[string]any{"locale": "pt-BR"})
	if got := send(map[string]any{"browser_locale": "es-419"}); got != "pt-BR" {
		t.Errorf("the user's pick: %q", got)
	}

	rec := owner.do(http.MethodPost, msgs, map[string]any{"content": "hi", "browser_locale": "pt_BR"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a malformed browser_locale: %d, want 400", rec.Code)
	}
}
