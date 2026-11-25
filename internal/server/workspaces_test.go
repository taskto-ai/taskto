package server

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/backup"
	"taskto.ai/taskto/internal/mail"
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
	// The document the reverse check reads exists, and the token of its own workspace reads it, so
	// "not found" below can only be the isolation and not a missing file.
	if code, body := s.api(one, http.MethodPut, s.docs(s.one, s.alice.private)+"/anything.md", map[string]any{"content": "# Anything\n"}); code != http.StatusCreated {
		t.Fatalf("a token of one writes a document in one = %d %v", code, body)
	}
	if got := s.toolOK(one, "read_doc", map[string]any{"space_id": s.alice.private, "path": "anything.md"}); got["content"] == nil {
		t.Fatalf("a token of one can't read its own document: %v", got)
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

// What belongs to the whole server (its backups, its outgoing email) is for operators, the
// admins of the workspace setup made: not for someone who is admin of a workspace they made
// themselves, where -workspace-creation=anyone lets any member make one.
func TestServerWideViewsAreForOperatorsNotForAdminsOfCreatedWorkspaces(t *testing.T) {
	rec := &recorder{}
	s := newStackWith(t, func(o *Options) {
		o.Auth.WorkspaceCreation = "anyone"
		o.Mail = mail.NewService(rec, testInfo)
		o.MailTestCooldown = -1
	})
	writeBackupStatus(t, s.dir, backup.Status{At: "03:15", LastError: "restic: dial tcp backup.internal.example:8000: refused",
		LastErrorAt: time.Now().UTC()})
	bobs := s.createWorkspace(s.bob.cookie, "Bob's")
	for name, tt := range map[string]struct {
		cookie string
		ws     int64
		want   int
	}{
		"the operator, in the first workspace":       {s.alice.cookie, s.one, http.StatusOK},
		"the operator, in a workspace she made":      {s.alice.cookie, s.two, http.StatusOK},
		"an admin of a workspace they made":          {s.bob.cookie, bobs, http.StatusForbidden},
		"the same person, a member of the first one": {s.bob.cookie, s.one, http.StatusForbidden},
	} {
		for _, path := range []string{
			fmt.Sprintf("/api/workspaces/%d/backup", tt.ws),
			fmt.Sprintf("/api/workspaces/%d/updates", tt.ws),
			mailPath(tt.ws, ""),
		} {
			code, body := s.browser(tt.cookie, http.MethodGet, path, nil)
			if code != tt.want {
				t.Errorf("%s: GET %s = %d %v, want %d", name, path, code, body, tt.want)
			}
			if tt.ws == bobs && body["code"] != "operator_only" {
				t.Errorf("%s: GET %s code = %v, want operator_only", name, path, body["code"])
			}
			if tt.want == http.StatusForbidden && (body["last_error"] != nil || body["host"] != nil) {
				t.Errorf("%s: GET %s leaked %v", name, path, body)
			}
		}
	}
	// A test message goes through the operator's relay: not for them either.
	if code, body := s.browser(s.bob.cookie, http.MethodPost, mailPath(bobs, "/test"), map[string]any{}); code != http.StatusForbidden || body["code"] != "operator_only" {
		t.Errorf("bob's test email = %d %v", code, body)
	}
	if got := rec.messages(); len(got) != 0 {
		t.Fatalf("a non-operator sent %d messages through the relay", len(got))
	}
	// /api/me says who may see them, which the web app uses.
	for who, want := range map[string]bool{"alice": true, "bob": false, "carol": false} {
		cookie := map[string]string{"alice": s.alice.cookie, "bob": s.bob.cookie, "carol": s.carol.cookie}[who]
		if _, body := s.browser(cookie, http.MethodGet, "/api/me", nil); body["is_operator"] != want {
			t.Errorf("%s: is_operator = %v, want %v", who, body["is_operator"], want)
		}
	}
}

// A form opened for one account and submitted after the browser's session became another's
// (the web app names the account it is for) makes no workspace: 409, and the other account
// ends up with nothing.
func TestCreatingAWorkspaceNamingAnotherAccountMakesNone(t *testing.T) {
	s := newStackWith(t, func(o *Options) { o.Auth.WorkspaceCreation = "anyone" })
	before := s.scalar(`SELECT count(*) FROM workspaces`)
	rec := s.request(http.MethodPost, "/api/workspaces", map[string]any{"name": "Not for bob"}, sameOrigin(s.bob.cookie), func(r *http.Request) {
		r.Header.Set(access.ExpectedUserHeader, fmt.Sprint(s.alice.id))
	})
	if rec.Code != http.StatusConflict || decode(rec)["code"] != "session_account_changed" {
		t.Fatalf("creating as bob for alice = %d %s, want 409 session_account_changed", rec.Code, rec.Body)
	}
	if got := s.scalar(`SELECT count(*) FROM workspaces`); got != before {
		t.Fatalf("a refused creation made %d workspaces", got-before)
	}
	rec = s.request(http.MethodPost, "/api/workspaces", map[string]any{"name": "For bob"}, sameOrigin(s.bob.cookie), func(r *http.Request) {
		r.Header.Set(access.ExpectedUserHeader, fmt.Sprint(s.bob.id))
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("creating as bob for bob = %d %s", rec.Code, rec.Body)
	}
}
