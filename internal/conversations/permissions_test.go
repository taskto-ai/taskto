package conversations

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"taskto.ai/taskto/internal/cliworker"
)

type fakeQuestions struct {
	mu         sync.Mutex
	user, conv int64
	q          cliworker.PermissionRequest
	answered   map[string]bool
}

func (f *fakeQuestions) Questions(_ context.Context, user, conv int64) ([]cliworker.PermissionRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if user != f.user || conv != f.conv || f.answered != nil {
		return nil, nil
	}
	return []cliworker.PermissionRequest{f.q}, nil
}

func (f *fakeQuestions) Answer(_ context.Context, user, conv int64, id string, allow bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if user != f.user || conv != f.conv || id != f.q.ID || f.answered != nil {
		return cliworker.ErrNoPermission
	}
	f.answered = map[string]bool{id: allow}
	return nil
}

func TestCLIPermissions(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	conv := owner.create(ws, nil)
	one := path(ws, fmt.Sprintf("/%d", conv.ID))

	if conv.CLIPermissions != "ask" {
		t.Fatalf("a new conversation has cli_permissions %q", conv.CLIPermissions)
	}
	got := decode[Conversation](t, owner.expect(http.StatusOK, http.MethodPatch, one, map[string]any{"cli_permissions": "allow_all"}))
	if got.CLIPermissions != "allow_all" {
		t.Fatalf("after allow_all: %+v", got)
	}
	got = decode[Conversation](t, owner.expect(http.StatusOK, http.MethodGet, one, nil))
	if got.CLIPermissions != "allow_all" {
		t.Fatalf("read back: %+v", got)
	}
	owner.expect(http.StatusBadRequest, http.MethodPatch, one, map[string]any{"cli_permissions": "everything"})
	got = decode[Conversation](t, owner.expect(http.StatusOK, http.MethodPatch, one, map[string]any{"cli_permissions": "ask"}))
	if got.CLIPermissions != "ask" {
		t.Fatalf("back to ask: %+v", got)
	}

	if r := decode[map[string][]cliworker.PermissionRequest](t, owner.expect(http.StatusOK, http.MethodGet, one+"/cli-permissions", nil)); len(r["requests"]) != 0 {
		t.Fatalf("questions without workers: %+v", r)
	}
	owner.expect(http.StatusNotFound, http.MethodPost, one+"/cli-permissions/q1", map[string]any{"allow": true})

	q := &fakeQuestions{user: owner.id, conv: conv.ID, q: cliworker.PermissionRequest{ID: "q1", CLI: cliworker.Claude, Tool: "Bash", Input: "touch x"}}
	e.svc.SetCLIQuestions(q)
	r := decode[map[string][]cliworker.PermissionRequest](t, owner.expect(http.StatusOK, http.MethodGet, one+"/cli-permissions", nil))
	if len(r["requests"]) != 1 || r["requests"][0].ID != "q1" || r["requests"][0].Tool != "Bash" {
		t.Fatalf("questions = %+v", r)
	}
	owner.expect(http.StatusBadRequest, http.MethodPost, one+"/cli-permissions/q1", map[string]any{})
	owner.expect(http.StatusNotFound, http.MethodPost, one+"/cli-permissions/q2", map[string]any{"allow": true})
	owner.expect(http.StatusNoContent, http.MethodPost, one+"/cli-permissions/q1", map[string]any{"allow": false})
	if q.answered["q1"] {
		t.Fatalf("answered %+v, want a denial", q.answered)
	}
	owner.expect(http.StatusNotFound, http.MethodPost, one+"/cli-permissions/q1", map[string]any{"allow": true})

	member := e.invite(owner, ws, "member@example.com")
	member.expect(http.StatusNotFound, http.MethodGet, one+"/cli-permissions", nil)
	member.expect(http.StatusNotFound, http.MethodPost, one+"/cli-permissions/q1", map[string]any{"allow": true})
	member.expect(http.StatusNotFound, http.MethodPatch, one, map[string]any{"cli_permissions": "allow_all"})
}
