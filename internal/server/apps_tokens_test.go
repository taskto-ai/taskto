package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apps"
)

// addExpense is the example app's action that adds a row, as the screen's button sends it.
func addExpense(appID int64, description string) map[string]any {
	return map[string]any{
		"name": "add_expense", "surfaceId": apps.SurfaceID(appID), "sourceComponentId": "add",
		"context": map[string]any{"description": description, "amount": "4"},
	}
}

// A token reads an app and its rows, and runs its actions, over REST, as its user in its
// workspace: the same answers as the session's, the changes marked with the token's name.
func TestTokensUseTheAppsRoutes(t *testing.T) {
	s := newStack(t)
	appID := s.app(s.alice.cookie, s.alice.private, "coffee")
	token, _ := s.newToken(s.alice.cookie, s.one, "laptop")
	base := fmt.Sprintf("%s/%d", s.apps(s.one, s.alice.private), appID)

	for _, path := range []string{
		s.apps(s.one, s.alice.private), base, base + "/surface", base + "/releases", base + "/releases/1",
		base + "/records", base + "/records/deleted", base + "/records/query", base + "/effects",
	} {
		session := s.request(http.MethodGet, path, nil, sameOrigin(s.alice.cookie))
		withToken := s.request(http.MethodGet, path, nil, bearer(token))
		if withToken.Code != http.StatusOK || withToken.Code != session.Code {
			t.Errorf("GET %s with a token = %d %s, with a session %d", path, withToken.Code, withToken.Body, session.Code)
		}
		if withToken.Code == http.StatusOK && withToken.Body.String() != session.Body.String() {
			t.Errorf("GET %s answers a token %s, a session %s", path, withToken.Body, session.Body)
		}
	}

	// An action as the token's user: the row is hers, marked with the token's name.
	rec := s.request(http.MethodPost, base+"/actions", addExpense(appID, "tea"), bearer(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("action with a token = %d %s", rec.Code, rec.Body)
	}
	code, recs := s.api(token, http.MethodGet, base+"/records", nil)
	if code != http.StatusOK || len(recs["records"].([]any)) != 2 {
		t.Fatalf("records = %d %v, want coffee and tea", code, recs)
	}
	var tea map[string]any
	for _, r := range recs["records"].([]any) {
		if r.(map[string]any)["data"].(map[string]any)["description"] == "tea" {
			tea = r.(map[string]any)
		}
	}
	if tea == nil {
		t.Fatalf("no tea in %v", recs)
	}
	if by := tea["created_by"].(map[string]any); id(by["id"]) != s.alice.id || by["agent"] != true || by["via"] != "laptop" {
		t.Errorf("a row added with a token is by %v, want alice's, the agent's, via laptop", by)
	}
	// The row the browser added stays unmarked.
	for _, r := range recs["records"].([]any) {
		rec := r.(map[string]any)
		if rec["data"].(map[string]any)["description"] != "coffee" {
			continue
		}
		if by := rec["created_by"].(map[string]any); by["agent"] != false || by["via"] != nil {
			t.Errorf("a browser's row is by %v", by)
		}
	}
	// A record's history and its effects are reachable too.
	if code, _ := s.api(token, http.MethodGet, fmt.Sprintf("%s/records/%d/history", base, id(tea["id"])), nil); code != http.StatusOK {
		t.Errorf("history with a token = %d", code)
	}
	if code, _ := s.api(token, http.MethodGet, fmt.Sprintf("%s/records/%d/effects", base, id(tea["id"])), nil); code != http.StatusOK {
		t.Errorf("record effects with a token = %d", code)
	}
	// And the same row is there over MCP.
	rows := s.toolOK(token, "list_app_rows", map[string]any{"space_id": s.alice.private, "app_id": appID})["rows"].([]any)
	if len(rows) != 2 {
		t.Errorf("list_app_rows = %v, want the same two rows", rows)
	}
}

// What a token can't do over the apps' REST routes: everything that builds, publishes, shares
// or serves the web app itself, 403 with or without a session cookie alongside. Alice is the
// app's owner and a workspace admin, so only the token can be why.
func TestTokensCannotBuildPublishOrShareApps(t *testing.T) {
	s := newStack(t)
	appID := s.app(s.alice.cookie, s.alice.private, "coffee")
	token, _ := s.newToken(s.alice.cookie, s.one, "laptop")
	base := fmt.Sprintf("%s/%d", s.apps(s.one, s.alice.private), appID)
	ws := fmt.Sprintf("/api/workspaces/%d/spaces/%d", s.one, s.alice.private)
	ex, err := apps.Examples()
	if err != nil {
		t.Fatal(err)
	}
	refused := []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, s.apps(s.one, s.alice.private), map[string]any{"name": "x", "definition": ex[0].Definition}},
		{http.MethodPatch, base, map[string]any{"version": 2, "name": "x"}},
		{http.MethodPost, base + "/move", map[string]any{"to_space": s.team}},
		{http.MethodPost, base + "/duplicate", map[string]any{}},
		{http.MethodPost, base + "/effect-board", map[string]any{}},
		{http.MethodPost, base + "/preview", map[string]any{}},
		{http.MethodPost, base + "/actions/preview", map[string]any{}},
		{http.MethodPost, base + "/publish", map[string]any{"version": 2, "acknowledge": []any{}}},
		{http.MethodPost, base + "/rollback", map[string]any{}},
		{http.MethodPost, base + "/records/1/restore", map[string]any{}},
		{http.MethodPost, base + "/effects/1/retry", map[string]any{}},
		{http.MethodGet, base + "/offline", nil},
		{http.MethodGet, base + "/roles", nil},
		{http.MethodPut, base + "/roles", map[string]any{}},
		{http.MethodPost, ws + "/app-templates/expenses/install", map[string]any{}},
		{http.MethodPost, base + "/uploads", map[string]any{}},
		{http.MethodGet, base + "/uploads/x", nil},
		{http.MethodDelete, base + "/uploads/x", nil},
		{http.MethodGet, base + "/attachments/x", nil},
		{http.MethodGet, base + "/attachments/x/thumbnail", nil},
	}
	for _, withSession := range []bool{false, true} {
		for _, c := range refused {
			with := []func(*http.Request){bearer(token)}
			if withSession {
				with = append(with, sameOrigin(s.alice.cookie))
			}
			rec := s.request(c.method, c.path, c.body, with...)
			// Some of these routes only exist when the server keeps files: then it is the
			// router's 404 or 405, which is no answer to a token either.
			if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
				if strings.Contains(c.path, "/uploads") || strings.Contains(c.path, "/attachments") {
					continue
				}
			}
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "access tokens can't be used here") {
				t.Errorf("%s %s with a token (and a session: %v) = %d %s, want 403", c.method, c.path, withSession, rec.Code, rec.Body)
			}
		}
	}
	// Nothing changed: the app is as it was published.
	code, a := s.browser(s.alice.cookie, http.MethodGet, base, nil)
	if code != http.StatusOK || a["name"] != "Expenses" || id(a["version"]) != 2 {
		t.Errorf("the app = %d %v, want Expenses as published", code, a)
	}
}

// A token reaches only the apps its user can, in its own workspace: not another user's
// private space, not a shared space its user isn't in, not another workspace, and a viewer's
// token can read but not act. All the same 404 or 403 a session gets.
func TestAppsTokensKeepTheirUsersAccessAndWorkspace(t *testing.T) {
	s := newStack(t)
	carolApp := s.app(s.carol.cookie, s.carol.private, "needle")
	secretApp := s.app(s.carol.cookie, s.secret, "needle")
	bob, _ := s.newToken(s.bob.cookie, s.one, "bob's agent")
	alice, _ := s.newToken(s.alice.cookie, s.one, "alice's agent")

	// Bob's token against carol's private space and the shared space bob isn't in: 404, as on
	// every other route that takes a token, for the list, the app, its rows and its actions.
	for sp, app := range map[int64]int64{s.carol.private: carolApp, s.secret: secretApp} {
		base := fmt.Sprintf("%s/%d", s.apps(s.one, sp), app)
		for _, path := range []string{s.apps(s.one, sp), base, base + "/surface", base + "/records", base + "/records/query", base + "/releases", base + "/effects"} {
			if code, _ := s.api(bob, http.MethodGet, path, nil); code != http.StatusNotFound {
				t.Errorf("bob's token: GET %s = %d, want 404", path, code)
			}
		}
		if code, _ := s.api(bob, http.MethodPost, base+"/actions", addExpense(app, "mine")); code != http.StatusNotFound {
			t.Errorf("bob's token acts in space %d = %d, want 404", sp, code)
		}
		// Nor carol's app through bob's own space, whose path it isn't in.
		through := fmt.Sprintf("%s/%d", s.apps(s.one, s.bob.private), app)
		for _, path := range []string{through, through + "/records", through + "/surface"} {
			if code, _ := s.api(bob, http.MethodGet, path, nil); code != http.StatusNotFound {
				t.Errorf("bob's token: GET %s = %d, want 404", path, code)
			}
		}
		if code, _ := s.api(bob, http.MethodPost, through+"/actions", addExpense(app, "mine")); code != http.StatusNotFound {
			t.Errorf("bob's token acts on carol's app through his space = %d, want 404", code)
		}
	}
	if n := s.scalar(`SELECT count(*) FROM app_rows WHERE app_id IN (?, ?)`, carolApp, secretApp); n != 2 {
		t.Errorf("%d rows in carol's apps, want her two", n)
	}

	// Another workspace of the same user: alice's token is for one.
	for _, path := range []string{s.apps(s.two, s.alicePrivate2), s.apps(s.two, s.alicePrivate2) + "/1/surface"} {
		if code, _ := s.api(alice, http.MethodGet, path, nil); code != http.StatusNotFound {
			t.Errorf("alice's token: GET %s = %d, want 404", path, code)
		}
	}
	if code, _ := s.api(alice, http.MethodPost, s.apps(s.two, s.alicePrivate2)+"/1/actions", addExpense(1, "x")); code != http.StatusNotFound {
		t.Errorf("alice's token acts in two = %d, want 404", code)
	}

	// A viewer's token reads and can't act. Carol shares an app in a space bob only reads.
	viewerSpace := s.sharedSpace("Reading", s.carol)
	ws := fmt.Sprintf("/api/workspaces/%d/spaces/%d", s.one, viewerSpace)
	if code, body := s.browser(s.alice.cookie, http.MethodPost, ws+"/members", map[string]any{"user_id": s.bob.id, "role": "viewer"}); code != http.StatusNoContent {
		t.Fatalf("add bob as a viewer = %d %v", code, body)
	}
	shared := s.app(s.carol.cookie, viewerSpace, "shared")
	base := fmt.Sprintf("%s/%d", s.apps(s.one, viewerSpace), shared)
	if code, body := s.api(bob, http.MethodGet, base+"/records", nil); code != http.StatusOK || len(body["records"].([]any)) != 1 {
		t.Errorf("a viewer's token reads = %d %v, want the row", code, body)
	}
	rec := s.request(http.MethodPost, base+"/actions", addExpense(shared, "no"), bearer(bob))
	if rec.Code != http.StatusForbidden {
		t.Errorf("a viewer's token adds a row = %d %s, want 403", rec.Code, rec.Body)
	}
	if n := s.scalar(`SELECT count(*) FROM app_rows WHERE app_id = ?`, shared); n != 1 {
		t.Errorf("%d rows, want the one", n)
	}
}

// A request that names an account, as the offline outbox's does, is the session's: with a
// token it is refused, so a token can't be used to send someone else's queued work.
func TestAppsTokensRefuseTheExpectedUserHeader(t *testing.T) {
	s := newStack(t)
	appID := s.app(s.alice.cookie, s.alice.private, "coffee")
	token, _ := s.newToken(s.alice.cookie, s.one, "laptop")
	path := fmt.Sprintf("%s/%d/records", s.apps(s.one, s.alice.private), appID)
	rec := s.request(http.MethodGet, path, nil, bearer(token), func(r *http.Request) {
		r.Header.Set(access.ExpectedUserHeader, fmt.Sprint(s.alice.id))
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("a token naming its account = %d %s, want 403", rec.Code, rec.Body)
	}
}

// A user's tokens have a budget of actions in a workspace, whichever route they run them on:
// over it the answer is a 429 with Retry-After and a code the web app translates. The budget is
// the user's own: a session, another user's token and the same user's other workspace are
// untouched by it.
func TestAppsTokensHaveAnActionBudget(t *testing.T) {
	s := newStack(t)
	appID := s.app(s.alice.cookie, s.alice.private, "coffee")
	bobApp := s.app(s.bob.cookie, s.bob.private, "tea")
	first, _ := s.newToken(s.alice.cookie, s.one, "laptop")
	second, _ := s.newToken(s.alice.cookie, s.one, "phone")
	bob, _ := s.newToken(s.bob.cookie, s.one, "bob's agent")
	base := fmt.Sprintf("%s/%d", s.apps(s.one, s.alice.private), appID)

	frozen := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	s.frozen = &frozen

	// The burst is spent by two tokens of the same user between them.
	for i := range apps.TokenActionBurst {
		token := first
		if i%2 == 1 {
			token = second
		}
		if rec := s.request(http.MethodPost, base+"/actions", addExpense(appID, fmt.Sprintf("row %d", i)), bearer(token)); rec.Code != http.StatusOK {
			t.Fatalf("action %d = %d %s", i+1, rec.Code, rec.Body)
		}
	}
	for _, token := range []string{first, second} {
		rec := s.request(http.MethodPost, base+"/actions", addExpense(appID, "one too many"), bearer(token))
		retry, err := strconv.Atoi(rec.Header().Get("Retry-After"))
		if rec.Code != http.StatusTooManyRequests || err != nil || retry < 1 || !strings.Contains(rec.Body.String(), `"code":"app_token_rate_limited"`) {
			t.Fatalf("over the budget = %d %s, Retry-After %q", rec.Code, rec.Body, rec.Header().Get("Retry-After"))
		}
	}
	// Over MCP the same budget refuses, as a tool error.
	release := s.toolOK(first, "list_app_actions", map[string]any{"space_id": s.alice.private, "app_id": appID})["release"]
	if code, refused, text := s.tool(first, "run_app_action", map[string]any{"space_id": s.alice.private, "app_id": appID, "expected_release": release, "action": "add_expense", "context": map[string]any{"description": "x", "amount": 4}}); code != http.StatusOK || !refused || !strings.Contains(text, "too many actions") {
		t.Errorf("run_app_action over the budget = %d %v %s, want a refusal", code, refused, text)
	}
	if n := s.scalar(`SELECT count(*) FROM app_rows WHERE app_id = ?`, appID); n != apps.TokenActionBurst+1 {
		t.Errorf("%d rows, want the coffee and the %d the budget allowed", n, apps.TokenActionBurst)
	}

	// Reading is not an action: it is not limited.
	if code, _ := s.api(first, http.MethodGet, base+"/records", nil); code != http.StatusOK {
		t.Errorf("a token reads over its action budget = %d", code)
	}
	// A second later one action is back; no more.
	frozen = frozen.Add(time.Second)
	if rec := s.request(http.MethodPost, base+"/actions", addExpense(appID, "later"), bearer(first)); rec.Code != http.StatusOK {
		t.Errorf("an action a second later = %d %s", rec.Code, rec.Body)
	}
	if rec := s.request(http.MethodPost, base+"/actions", addExpense(appID, "too soon"), bearer(second)); rec.Code != http.StatusTooManyRequests {
		t.Errorf("a second action in that second = %d %s, want 429", rec.Code, rec.Body)
	}
	// The user's session, who is a person, still acts.
	if code, body := s.browser(s.alice.cookie, http.MethodPost, base+"/actions", addExpense(appID, "by hand")); code != http.StatusOK {
		t.Errorf("a session acts after its user's tokens ran out = %d %v", code, body)
	}
	// Another user's token has a budget of its own.
	if rec := s.request(http.MethodPost, fmt.Sprintf("%s/%d/actions", s.apps(s.one, s.bob.private), bobApp), addExpense(bobApp, "bob's"), bearer(bob)); rec.Code != http.StatusOK {
		t.Errorf("bob's token after alice's ran out = %d %s", rec.Code, rec.Body)
	}
}

// An app restricted to roles holds a token to them too: a reader's token reads the rows in a
// space that lets her write, and can't insert.
func TestAppsTokensKeepTheAppsRoles(t *testing.T) {
	s := newStack(t)
	bob, _ := s.newToken(s.bob.cookie, s.one, "bob's agent")

	// Alice shares a space with bob as an editor, then restricts an app in it to a reader role.
	space := s.sharedSpace("Cafe", s.bob)
	shared := s.app(s.alice.cookie, space, "shared")
	sharedBase := fmt.Sprintf("%s/%d", s.apps(s.one, space), shared)
	grants := map[string]any{"grants": []map[string]any{{"user_id": s.bob.id, "roles": []string{"reader"}}}}
	if code, body := s.browser(s.alice.cookie, http.MethodPut, sharedBase+"/roles", grants); code != http.StatusOK {
		t.Fatalf("restrict the app = %d %v", code, body)
	}
	if code, body := s.api(bob, http.MethodGet, sharedBase+"/records", nil); code != http.StatusOK || len(body["records"].([]any)) != 1 {
		t.Errorf("a reader's token reads = %d %v, want the row", code, body)
	}
	rec := s.request(http.MethodPost, sharedBase+"/actions", addExpense(shared, "no"), bearer(bob))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"code":"app_forbidden"`) {
		t.Errorf("a reader's token inserts = %d %s, want 403 app_forbidden", rec.Code, rec.Body)
	}
}
