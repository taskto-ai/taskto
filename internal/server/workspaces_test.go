package server

import (
	"fmt"
	"net/http"
	"testing"

	"taskto.ai/taskto/internal/apps"
)

// A workspace made through the API is as separate from the others as the first one is: the
// people, spaces, boards, apps, documents and tokens of one are 404 from the other, over REST
// and MCP, whoever the caller is in either.
func TestANewWorkspaceIsIsolatedFromTheFirst(t *testing.T) {
	s := newStack(t) // alice made "Two" (s.two) through POST /api/workspaces
	three := s.createWorkspace(s.alice.cookie, "Three")
	threePrivate := s.scalar(`SELECT id FROM spaces WHERE workspace_id = ? AND owner_id = ?`, three, s.alice.id)

	// Alice's private space in each new workspace has its personal board, as in the first.
	if n := s.scalar(`SELECT count(*) FROM boards WHERE space_id = ?`, threePrivate); n != 1 {
		t.Fatalf("boards in the new private space = %d, want 1", n)
	}
	if code, body := s.browser(s.alice.cookie, http.MethodPut, s.docs(three, threePrivate)+"/three.md", map[string]any{"content": "# Three\n\nneedle\n"}); code != http.StatusCreated {
		t.Fatalf("alice writes = %d %v", code, body)
	}
	ex, err := apps.Examples()
	if err != nil {
		t.Fatal(err)
	}
	code, app := s.browser(s.alice.cookie, http.MethodPost, s.apps(three, threePrivate), map[string]any{"name": "Expenses", "definition": ex[0].Definition})
	if code != http.StatusCreated {
		t.Fatalf("create app = %d %v", code, app)
	}
	appID := id(app["id"])

	w3 := fmt.Sprintf("/api/workspaces/%d", three)
	w1 := fmt.Sprintf("/api/workspaces/%d", s.one)

	// Bob and carol are members of the first workspace only: the new one doesn't exist for them.
	for name, p := range map[string]person{"bob": s.bob, "carol": s.carol} {
		for _, path := range []string{
			w3 + "/members", w3 + "/spaces", w3 + "/tokens", w3 + "/invites",
			fmt.Sprintf("%s/spaces/%d", w3, threePrivate),
			s.docs(three, threePrivate), s.docs(three, threePrivate) + "/three.md",
			fmt.Sprintf("%s/spaces/%d/boards", w3, threePrivate),
			s.apps(three, threePrivate), fmt.Sprintf("%s/%d", s.apps(three, threePrivate), appID),
		} {
			if code, _ := s.browser(p.cookie, http.MethodGet, path, nil); code != http.StatusNotFound {
				t.Errorf("%s: GET %s = %d, want 404", name, path, code)
			}
		}
		if code, _ := s.browser(p.cookie, http.MethodPost, w3+"/spaces", map[string]any{"name": "Mine"}); code != http.StatusNotFound {
			t.Errorf("%s makes a space in the new workspace = %d, want 404", name, code)
		}
		if code, _ := s.browser(p.cookie, http.MethodPatch, w3, map[string]any{"locale": "pt-BR"}); code != http.StatusNotFound {
			t.Errorf("%s changes the new workspace = %d, want 404", name, code)
		}
	}
	// Alice isn't made a member of anyone's by being its creator's neighbour: the new workspace
	// has her alone.
	if n := s.scalar(`SELECT count(*) FROM memberships WHERE workspace_id = ?`, three); n != 1 {
		t.Errorf("members of the new workspace = %d, want 1", n)
	}
	code, members := s.browser(s.alice.cookie, http.MethodGet, w3+"/members", nil)
	if list, _ := members["members"].([]any); code != http.StatusOK || len(list) != 1 {
		t.Errorf("members of the new workspace = %d %v", code, members)
	}

	// Ids don't cross: a space, document or app of the new workspace through the first one's
	// address, and the first one's through the new one's, even for someone in both.
	for _, path := range []string{
		fmt.Sprintf("%s/spaces/%d", w1, threePrivate),
		s.docs(s.one, threePrivate) + "/three.md",
		fmt.Sprintf("%s/%d", s.apps(s.one, threePrivate), appID),
		fmt.Sprintf("%s/spaces/%d", w3, s.alice.private),
		s.docs(three, s.alice.private),
		fmt.Sprintf("%s/spaces/%d", w3, s.team),
	} {
		if code, _ := s.browser(s.alice.cookie, http.MethodGet, path, nil); code != http.StatusNotFound {
			t.Errorf("alice: GET %s = %d, want 404", path, code)
		}
	}
	// The first workspace's spaces list has none of the new one's.
	_, list := s.browser(s.alice.cookie, http.MethodGet, w1+"/spaces", nil)
	for _, sp := range list["spaces"].([]any) {
		if got := id(sp.(map[string]any)["id"]); got == threePrivate || got == s.alicePrivate2 {
			t.Errorf("the first workspace lists space %d", got)
		}
	}

	// Tokens are bound to the workspace they were made in, and see only it, over REST and MCP.
	one, _ := s.newToken(s.alice.cookie, s.one, "agent in one")
	tok3, _ := s.newToken(s.alice.cookie, three, "agent in three")
	if code, _ := s.api(one, http.MethodGet, s.docs(three, threePrivate)+"/three.md", nil); code != http.StatusNotFound {
		t.Errorf("a token of one reads the new workspace = %d, want 404", code)
	}
	if code, refused, text := s.tool(one, "read_doc", map[string]any{"space_id": threePrivate, "path": "three.md"}); code != http.StatusOK || !refused || text != "error: not found" {
		t.Errorf("a token of one reads the new workspace over MCP = %d %v %q", code, refused, text)
	}
	if code, _ := s.api(tok3, http.MethodGet, s.docs(s.one, s.alice.private), nil); code != http.StatusNotFound {
		t.Errorf("a token of the new workspace reads one = %d, want 404", code)
	}
	if code, _ := s.api(tok3, http.MethodGet, w1+"/members", nil); code != http.StatusNotFound {
		t.Errorf("a token of the new workspace reads one's members = %d, want 404", code)
	}
	if code, refused, text := s.tool(tok3, "read_doc", map[string]any{"space_id": s.alice.private, "path": "anything.md"}); code != http.StatusOK || !refused || text != "error: not found" {
		t.Errorf("a token of the new workspace reads one over MCP = %d %v %q", code, refused, text)
	}
	if got := s.toolOK(tok3, "read_doc", map[string]any{"space_id": threePrivate, "path": "three.md"}); got["content"] == nil {
		t.Errorf("a token of the new workspace can't read its own: %v", got)
	}
	for _, sp := range s.toolOK(tok3, "list_spaces", nil)["spaces"].([]any) {
		if got := id(sp.(map[string]any)["id"]); got != threePrivate {
			t.Errorf("a token of the new workspace lists space %d", got)
		}
	}
	// Tokens made in the new workspace are its admin's own: bob has none there.
	if code, _ := s.browser(s.bob.cookie, http.MethodPost, w3+"/tokens", map[string]any{"name": "x"}); code != http.StatusNotFound {
		t.Errorf("bob makes a token in the new workspace = %d, want 404", code)
	}

	// Removing alice from nothing: bob (a member of one) making a workspace is refused by the
	// default setting, and leaves nothing behind.
	before := s.scalar(`SELECT count(*) FROM workspaces`)
	if code, body := s.browser(s.bob.cookie, http.MethodPost, "/api/workspaces", map[string]any{"name": "Bob's"}); code != http.StatusForbidden || body["code"] != "workspace_creation_refused" {
		t.Errorf("bob creates a workspace = %d %v, want 403 workspace_creation_refused", code, body)
	}
	if after := s.scalar(`SELECT count(*) FROM workspaces`); after != before {
		t.Errorf("workspaces = %d, was %d", after, before)
	}
}

// With "anyone", a member who administers nothing makes a workspace of their own, and the
// admin of the workspace they came from is a stranger there.
func TestAnyoneMayCreateAWorkspaceWhenTheOperatorSaysSo(t *testing.T) {
	s := newStackWith(t, func(o *Options) { o.Auth.WorkspaceCreation = "anyone" })
	bobs := s.createWorkspace(s.bob.cookie, "Bob's")
	w := fmt.Sprintf("/api/workspaces/%d", bobs)
	for _, path := range []string{w + "/members", w + "/spaces", w + "/invites", w + "/tokens"} {
		if code, _ := s.browser(s.alice.cookie, http.MethodGet, path, nil); code != http.StatusNotFound {
			t.Errorf("alice (admin of one): GET %s = %d, want 404", path, code)
		}
	}
	if code, body := s.browser(s.bob.cookie, http.MethodGet, w+"/invites", nil); code != http.StatusOK {
		t.Errorf("bob lists his workspace's invites = %d %v", code, body)
	}
	// Bob is still only a member of the workspace he came from.
	if code, _ := s.browser(s.bob.cookie, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/invites", s.one), nil); code != http.StatusForbidden {
		t.Errorf("bob lists one's invites = %d, want 403", code)
	}
}
