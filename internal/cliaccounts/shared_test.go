//go:build unix

package cliaccounts

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

const sharedKey = "fake-shared-key-for-the-tests"

// provider is a fake model provider: it records what reaches it and answers like one.
type provider struct {
	*httptest.Server
	mu   sync.Mutex
	seen []*http.Request
	// status, if set, is the answer; 429 comes with a Retry-After.
	status int
}

func newProvider(t *testing.T) *provider {
	t.Helper()
	p := &provider{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		p.mu.Lock()
		p.seen = append(p.seen, r.Clone(r.Context()))
		status := p.status
		p.mu.Unlock()
		if r.Header.Get("X-Api-Key") != sharedKey && r.Header.Get("Authorization") != "Bearer "+sharedKey {
			http.Error(w, `{"error":"invalid key"}`, http.StatusUnauthorized)
			return
		}
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "120")
		}
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Set-Cookie", "tracker=1")
		_, _ = io.WriteString(w, "data: {\"ok\":true}\n\n")
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *provider) requests() []*http.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.seen)
}

// addShared adds a shared account for cli to ws as admin, at the provider.
func (c *client) addShared(ws int64, cli cliworker.CLI, baseURL string) SharedAccount {
	c.e.t.Helper()
	rec := c.do(http.MethodPost, sharedPath(ws), map[string]string{"cli": string(cli), "label": "Company key", "base_url": baseURL, "api_key": sharedKey})
	expect(c.e.t, rec, http.StatusCreated)
	return decode[SharedAccount](c.e.t, rec)
}

func sharedPath(ws int64) string { return "/api/workspaces/" + itoa(ws) + "/cli-accounts" }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// secondWorkspace makes c the admin of a workspace of its own.
func (e *env) secondWorkspace(c *client) int64 {
	e.t.Helper()
	var ws int64
	if err := e.db.QueryRow(`INSERT INTO workspaces (name) VALUES ('Other') RETURNING id`).Scan(&ws); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'admin')`, ws, c.id); err != nil {
		e.t.Fatal(err)
	}
	return ws
}

func TestSharedAccountsAPI(t *testing.T) {
	e := newEnv(t, LocalUnsafe, nil)
	admin, ws := e.setup()
	member := e.invite(admin, ws, "member@example.com")
	outsider := e.invite(admin, ws, "outsider@example.com")
	if _, err := e.db.Exec(`DELETE FROM memberships WHERE user_id = ?`, outsider.id); err != nil {
		t.Fatal(err)
	}
	other := e.secondWorkspace(outsider)
	p := newProvider(t)

	a := admin.addShared(ws, cliworker.Claude, p.URL+"/v1/")
	if a.APIKey != "set" || a.BaseURL != p.URL || a.CLI != cliworker.Claude || a.Label != "Company key" {
		t.Fatalf("created = %+v", a)
	}
	// The default address is the provider's own.
	rec := admin.do(http.MethodPost, sharedPath(ws), map[string]string{"cli": "codex", "label": "OpenAI", "api_key": sharedKey})
	expect(t, rec, http.StatusCreated)
	if got := decode[SharedAccount](t, rec).BaseURL; got != "https://api.openai.com" {
		t.Errorf("default base URL = %s", got)
	}

	// Members read them, without the key, and their mode.
	rec = member.do(http.MethodGet, sharedPath(ws), nil)
	expect(t, rec, http.StatusOK)
	l := decode[sharedList](t, rec)
	if len(l.Accounts) != 2 || l.Mode != OwnElseShared || l.DefaultMode != OwnElseShared || !l.Enabled {
		t.Fatalf("list = %+v", l)
	}
	if strings.Contains(rec.Body.String(), sharedKey) {
		t.Fatal("the key is in the list")
	}

	// Only admins change them.
	path := sharedPath(ws) + "/" + itoa(a.ID)
	expect(t, member.do(http.MethodPost, sharedPath(ws), map[string]string{"cli": "grok", "label": "x", "api_key": sharedKey}), http.StatusForbidden)
	expect(t, member.do(http.MethodPatch, path, map[string]string{"label": "mine"}), http.StatusForbidden)
	expect(t, member.do(http.MethodDelete, path, nil), http.StatusForbidden)
	expect(t, member.do(http.MethodPost, path+"/test", map[string]any{}), http.StatusForbidden)
	expect(t, member.do(http.MethodPatch, sharedPath(ws)+"/modes", map[string]any{"default_mode": "shared"}), http.StatusForbidden)

	// Another workspace's admin reaches none of them, through either workspace's path.
	expect(t, outsider.do(http.MethodGet, sharedPath(ws), nil), http.StatusNotFound)
	otherPath := sharedPath(other) + "/" + itoa(a.ID)
	expect(t, outsider.do(http.MethodPatch, otherPath, map[string]string{"label": "stolen"}), http.StatusNotFound)
	expect(t, outsider.do(http.MethodDelete, otherPath, nil), http.StatusNotFound)
	expect(t, outsider.do(http.MethodPost, otherPath+"/test", map[string]any{}), http.StatusNotFound)
	if l := decode[sharedList](t, outsider.do(http.MethodGet, sharedPath(other), nil)); len(l.Accounts) != 0 {
		t.Fatalf("another workspace lists %+v", l.Accounts)
	}

	// Refusals.
	for _, body := range []map[string]string{
		{"cli": "claude", "label": "x", "api_key": "short"},
		{"cli": "claude", "label": "", "api_key": sharedKey},
		{"cli": "nope", "label": "x", "api_key": sharedKey},
		{"cli": "claude", "label": "x", "api_key": sharedKey, "base_url": "http://example.com"},
		{"cli": "claude", "label": "x", "api_key": sharedKey, "base_url": "https://user:pw@example.com"}, //nolint:gosec // G101: made up, to be refused
		{"cli": "claude", "label": "x", "api_key": sharedKey, "model": "bad model"},
		{"cli": "claude", "label": "x"},
	} {
		expect(t, admin.do(http.MethodPost, sharedPath(ws), body), http.StatusBadRequest)
	}
	expect(t, admin.do(http.MethodPatch, path, map[string]string{"cli": "codex"}), http.StatusBadRequest)

	// The test reaches the provider with the key, and says what it answered, never the key.
	rec = admin.do(http.MethodPost, path+"/test", map[string]any{})
	expect(t, rec, http.StatusOK)
	if got := decode[map[string]any](t, rec); got["ok"] != true {
		t.Fatalf("test = %v", got)
	}
	if r := p.requests(); len(r) != 1 || r[0].URL.Path != "/v1/models" || r[0].Header.Get("X-Api-Key") != sharedKey {
		t.Fatalf("the provider saw %+v", r)
	}
	rec = admin.do(http.MethodPatch, path, map[string]string{"api_key": "fake-other-key-that-fails", "label": "Renamed"})
	expect(t, rec, http.StatusOK)
	if got := decode[SharedAccount](t, rec); got.Label != "Renamed" {
		t.Errorf("patched = %+v", got)
	}
	rec = admin.do(http.MethodPost, path+"/test", map[string]any{})
	if got := decode[map[string]any](t, rec); got["ok"] != false || !strings.Contains(got["message"].(string), "refused the key") {
		t.Fatalf("test with a bad key = %v", got)
	}
	if strings.Contains(rec.Body.String(), "fake-other") {
		t.Fatal("the key is in the test's answer")
	}

	// The key is sealed in the database.
	var sealed []byte
	if err := e.db.QueryRow(`SELECT api_key FROM cli_shared_accounts WHERE id = ?`, a.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "fake-other") {
		t.Fatal("the key is stored in clear")
	}

	expect(t, admin.do(http.MethodDelete, path, nil), http.StatusNoContent)
	expect(t, admin.do(http.MethodPatch, path, map[string]string{"label": "gone"}), http.StatusNotFound)
}

func TestModes(t *testing.T) {
	e := newEnv(t, LocalUnsafe, nil)
	admin, ws := e.setup()
	member := e.invite(admin, ws, "member@example.com")
	outsider := e.invite(admin, ws, "outsider@example.com")
	if _, err := e.db.Exec(`DELETE FROM memberships WHERE user_id = ?`, outsider.id); err != nil {
		t.Fatal(err)
	}
	modes := sharedPath(ws) + "/modes"
	rec := admin.do(http.MethodPatch, modes, map[string]any{"default_mode": "own", "members": map[string]any{itoa(member.id): "shared"}})
	expect(t, rec, http.StatusOK)
	if got := decode[sharedList](t, member.do(http.MethodGet, sharedPath(ws), nil)); got.Mode != SharedOnly || got.DefaultMode != OwnOnly {
		t.Fatalf("member's list = %+v", got)
	}
	if got := decode[sharedList](t, admin.do(http.MethodGet, sharedPath(ws), nil)); got.Mode != OwnOnly {
		t.Fatalf("admin's mode = %s", got.Mode)
	}
	// Back to the default.
	expect(t, admin.do(http.MethodPatch, modes, map[string]any{"members": map[string]any{itoa(member.id): nil}}), http.StatusOK)
	if got := decode[sharedList](t, member.do(http.MethodGet, sharedPath(ws), nil)); got.Mode != OwnOnly {
		t.Fatalf("member's mode = %s", got.Mode)
	}
	// Someone who isn't a member, and modes that aren't.
	expect(t, admin.do(http.MethodPatch, modes, map[string]any{"members": map[string]any{itoa(outsider.id): "own"}}), http.StatusNotFound)
	expect(t, admin.do(http.MethodPatch, modes, map[string]any{"members": map[string]any{itoa(member.id): "everything"}}), http.StatusBadRequest)
	expect(t, admin.do(http.MethodPatch, modes, map[string]any{"default_mode": "any"}), http.StatusBadRequest)
	rec = admin.do(http.MethodGet, modes, nil)
	expect(t, rec, http.StatusOK)
	got := decode[struct {
		DefaultMode AccountMode  `json:"default_mode"`
		Members     []memberMode `json:"members"`
	}](t, rec)
	if got.DefaultMode != OwnOnly || len(got.Members) != 2 {
		t.Fatalf("modes = %+v", got)
	}
}

// A member's turn runs on the workspace's shared account by mode; the CLI gets the proxy's
// address and a token for the turn, never the key; the proxy forwards the turn's requests
// with the key, and stops taking the token when the turn ends.
func TestTurnOnASharedAccount(t *testing.T) {
	e := newTurnEnv(t)
	p := newProvider(t)
	a := e.owner.addShared(e.ws, cliworker.Claude, p.URL)
	conv := e.conversation(e.other)

	// The member has no Claude Code login of their own: the shared account runs the turn.
	if d := done(e.turn(conv, turnBody{Turn: "t1", CLI: "claude", Model: "default", Prompt: "hello"})); d.Status != cliworker.StatusOK {
		t.Fatalf("turn = %+v", d)
	}
	config := e.dirs(e.other.id).ConfigOf(cliworker.Claude, sharedSlot(a.ID))
	calls := turnCalls(t, config)
	if len(calls) != 1 {
		t.Fatalf("turns in the shared account's directory = %d", len(calls))
	}
	var token string
	for _, kv := range calls[0].Env {
		if v, ok := strings.CutPrefix(kv, "ANTHROPIC_AUTH_TOKEN="); ok {
			token = v
		}
		if strings.Contains(kv, sharedKey) {
			t.Fatalf("the key reached the CLI: %s", kv)
		}
	}
	if !slices.Contains(calls[0].Env, "ANTHROPIC_BASE_URL="+e.proxy.URL+"/cli-proxy") || token == "" {
		t.Fatalf("the CLI wasn't pointed at the proxy: %v", calls[0].Env)
	}

	// The turn is over: its token no longer works.
	if res := proxyCall(t, e, token, "/cli-proxy/v1/messages"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an ended turn's token: %d", res.StatusCode)
	}

	// A token for a running turn works, and the provider sees the key, not the token.
	claims := proxyClaims{Account: a.ID, User: e.other.id, Identity: e.acct(e.other.id).Identity, Workspace: e.ws, Conversation: conv, Turn: "c1-live"}
	live, err := e.svc.mintToken(claims)
	if err != nil {
		t.Fatal(err)
	}
	res := proxyCall(t, e, live, "/cli-proxy/v1/messages?beta=true")
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Body, `"ok":true`) || res.Header.Get("Set-Cookie") != "" {
		t.Fatalf("proxied = %+v", res)
	}
	seen := p.requests()
	last := seen[len(seen)-1]
	if last.URL.Path != "/v1/messages" || last.URL.RawQuery != "beta=true" || last.Header.Get("X-Api-Key") != sharedKey ||
		last.Header.Get("Authorization") != "" || last.Header.Get("Anthropic-Version") != "2023-06-01" || last.Header.Get("Cookie") != "" {
		t.Fatalf("the provider saw %s %v", last.URL, last.Header)
	}

	// Refusals: a forged or foreign token, a path outside /v1, a conversation that isn't the
	// user's, the user out of the workspace, a mode that rules shared accounts out.
	if res := proxyCall(t, e, live+"x", "/cli-proxy/v1/messages"); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("forged token: %d", res.StatusCode)
	}
	// A token of a turn from before a restore of the server's data: signed with the same key,
	// for the same number, but another account's, which had the number in the lost history.
	elsewhere := claims
	elsewhere.Workspace, elsewhere.Turn = e.ws+1, "c1-elsewhere"
	if token, err := e.svc.mintToken(elsewhere); err != nil {
		t.Fatal(err)
	} else if res := proxyCall(t, e, token, "/cli-proxy/v1/messages"); res.StatusCode != http.StatusForbidden {
		t.Errorf("a token of another workspace's conversation: %d", res.StatusCode)
	}
	before := claims
	before.Identity, before.Turn = strings.Repeat("ab", 16), "c1-before"
	if earlier, err := e.svc.mintToken(before); err != nil {
		t.Fatal(err)
	} else if res := proxyCall(t, e, earlier, "/cli-proxy/v1/messages"); res.StatusCode != http.StatusForbidden {
		t.Errorf("a token of the number's earlier account: %d", res.StatusCode)
	}
	if res := proxyCall(t, e, live, "/cli-proxy/admin"); res.StatusCode != http.StatusNotFound {
		t.Errorf("outside /v1: %d", res.StatusCode)
	}
	foreign, _ := e.svc.mintToken(proxyClaims{Account: a.ID, User: e.owner.id, Identity: e.acct(e.owner.id).Identity, Workspace: e.ws, Conversation: conv, Turn: "c1-foreign"})
	if res := proxyCall(t, e, foreign, "/cli-proxy/v1/messages"); res.StatusCode != http.StatusForbidden {
		t.Errorf("another user's conversation: %d", res.StatusCode)
	}
	expect(t, e.owner.do(http.MethodPatch, sharedPath(e.ws)+"/modes", map[string]any{"members": map[string]any{itoa(e.other.id): "own"}}), http.StatusOK)
	if res := proxyCall(t, e, live, "/cli-proxy/v1/messages"); res.StatusCode != http.StatusForbidden {
		t.Errorf("mode own: %d", res.StatusCode)
	}
	expect(t, e.owner.do(http.MethodPatch, sharedPath(e.ws)+"/modes", map[string]any{"members": map[string]any{itoa(e.other.id): nil}}), http.StatusOK)
	if _, err := e.db.Exec(`DELETE FROM memberships WHERE user_id = ?`, e.other.id); err != nil {
		t.Fatal(err)
	}
	if res := proxyCall(t, e, live, "/cli-proxy/v1/messages"); res.StatusCode != http.StatusForbidden {
		t.Errorf("after leaving the workspace: %d", res.StatusCode)
	}
}

// A 429 from the provider is the shared account's limit, with when it resets.
func TestProxyRecordsALimit(t *testing.T) {
	e := newTurnEnv(t)
	p := newProvider(t)
	p.status = http.StatusTooManyRequests
	a := e.owner.addShared(e.ws, cliworker.Codex, p.URL)
	conv := e.conversation(e.owner)
	token, err := e.svc.mintToken(proxyClaims{Account: a.ID, User: e.owner.id, Identity: e.acct(e.owner.id).Identity, Workspace: e.ws, Conversation: conv, Turn: "c1-t"})
	if err != nil {
		t.Fatal(err)
	}
	if res := proxyCall(t, e, token, "/cli-proxy/v1/responses"); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if r := p.requests(); r[len(r)-1].Header.Get("Authorization") != "Bearer "+sharedKey || r[len(r)-1].Header.Get("X-Api-Key") != "" {
		t.Fatalf("OpenAI's credential: %v", r[len(r)-1].Header)
	}
	l := decode[sharedList](t, e.owner.do(http.MethodGet, sharedPath(e.ws), nil))
	got := l.Accounts[0]
	if len(got.Limits) != 1 || got.Limits[0].Window != "api" || got.Limits[0].Status != "rejected" || got.Limits[0].ResetsAt == nil ||
		time.Until(*got.Limits[0].ResetsAt) < time.Minute {
		t.Fatalf("limits = %+v", got.Limits)
	}
}

// The mode decides between the member's own account and the workspace's.
func TestModeChoosesTheAccount(t *testing.T) {
	e := newTurnEnv(t)
	p := newProvider(t)
	a := e.owner.addShared(e.ws, cliworker.Grok, p.URL)
	e.other.connect(cliworker.Grok)
	conv := e.conversation(e.other)
	own := e.dirs(e.other.id).Config(cliworker.Grok)
	shared := e.dirs(e.other.id).ConfigOf(cliworker.Grok, sharedSlot(a.ID))
	setMode := func(m any) {
		t.Helper()
		expect(t, e.owner.do(http.MethodPatch, sharedPath(e.ws)+"/modes", map[string]any{"members": map[string]any{itoa(e.other.id): m}}), http.StatusOK)
	}
	turns := func(dir string) int { return len(clitest.Calls(t, dir)) }

	// own_else_shared, own connected: own.
	e.turn(conv, turnBody{Turn: "t1", CLI: "grok", Model: "default", Prompt: "one"})
	if turns(own) == 0 || turns(shared) != 0 {
		t.Fatalf("own %d, shared %d", turns(own), turns(shared))
	}
	// shared: the workspace's, even with their own connected.
	setMode("shared")
	e.turn(conv, turnBody{Turn: "t2", CLI: "grok", Model: "default", Prompt: "two"})
	if turns(shared) != 1 {
		t.Fatalf("shared mode: shared %d", turns(shared))
	}
	// own, with their own needing a new sign-in: asks for it, never the shared one.
	setMode("own")
	if err := e.svc.setReconnect(t.Context(), e.accountID(e.other.id, cliworker.Grok)); err != nil {
		t.Fatal(err)
	}
	if d := done(e.turn(conv, turnBody{Turn: "t3", CLI: "grok", Model: "default", Prompt: "three"})); d.Status != cliworker.StatusLoginRequired {
		t.Fatalf("own mode, reconnect: %+v", d)
	}
	// own_else_shared, their own needing a new sign-in: the shared one.
	setMode(nil)
	if d := done(e.turn(conv, turnBody{Turn: "t4", CLI: "grok", Model: "default", Prompt: "four"})); d.Status != cliworker.StatusOK || turns(shared) != 2 {
		t.Fatalf("own_else_shared, reconnect: %+v, shared %d", d, turns(shared))
	}
	// shared, none for the CLI: refused, saying so.
	setMode("shared")
	if d := done(e.turn(conv, turnBody{Turn: "t5", CLI: "claude", Model: "default", Prompt: "five"})); d.Status != cliworker.StatusFailed || !strings.Contains(d.Text, "shared accounts only") {
		t.Fatalf("shared mode, none: %+v", d)
	}
}

// A shared account's turn whose key the provider refuses fails, rather than waiting for the
// member to sign in: it isn't their login.
func TestSharedKeyRefused(t *testing.T) {
	e := newTurnEnv(t)
	p := newProvider(t)
	a := e.owner.addShared(e.ws, cliworker.Claude, p.URL)
	conv := e.conversation(e.other)
	config := e.dirs(e.other.id).ConfigOf(cliworker.Claude, sharedSlot(a.ID))
	e.dirsSlot(e.other.id, cliworker.Claude, sharedSlot(a.ID))
	clitest.WriteScenario(t, config, clitest.Scenario{Expired: true})
	events := e.turn(conv, turnBody{Turn: "t1", CLI: "claude", Model: "default", Prompt: "hello"})
	d := done(events)
	if d.Status != cliworker.StatusFailed || !strings.Contains(d.Text, "an admin should check its key") {
		t.Fatalf("done = %+v", d)
	}
	for _, ev := range events {
		if ev.Type == cliworker.EventLoginRequired {
			t.Fatalf("a refused shared key asked the member to sign in: %+v", events)
		}
	}
	code, _, body := e.post(conv, "/cli/wait", map[string]string{"cli": "claude"})
	if code != http.StatusOK || !strings.Contains(body, `"connected"`) {
		t.Fatalf("wait = %d %s", code, body)
	}
}

// proxied is the proxy's answer to a call.
type proxied struct {
	StatusCode int
	Header     http.Header
	Body       string
}

func proxyCall(t *testing.T, e *turnEnv, token, path string) proxied {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.proxy.URL+path, strings.NewReader(`{"model":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Cookie", "session=leak")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return proxied{StatusCode: res.StatusCode, Header: res.Header, Body: string(body)}
}

// A turn run again (a replay) gets a new token, and its earlier one stops working.
func TestReplayRetiresTheEarlierToken(t *testing.T) {
	e := newTurnEnv(t)
	p := newProvider(t)
	a := e.owner.addShared(e.ws, cliworker.Claude, p.URL)
	conv := e.conversation(e.owner)
	claims := proxyClaims{Account: a.ID, User: e.owner.id, Identity: e.acct(e.owner.id).Identity, Workspace: e.ws, Conversation: conv, Turn: "c1-replayed"}
	first, err := e.svc.mintToken(claims)
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.svc.mintToken(claims)
	if err != nil {
		t.Fatal(err)
	}
	if res := proxyCall(t, e, first, "/cli-proxy/v1/messages"); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("the earlier token: %d", res.StatusCode)
	}
	if res := proxyCall(t, e, second, "/cli-proxy/v1/messages"); res.StatusCode != http.StatusOK {
		t.Errorf("the new token: %d", res.StatusCode)
	}
	e.svc.endTurn("c1-replayed")
	if res := proxyCall(t, e, second, "/cli-proxy/v1/messages"); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("after the turn ended: %d", res.StatusCode)
	}
}

// An admin's request that passed the middleware before the admin was disabled reaches the handler
// after: it changes nothing and is answered 403, in every write of the shared accounts.
func TestDisabledAdminChangesNoSharedAccountSettings(t *testing.T) {
	e := newEnv(t, LocalUnsafe, nil)
	admin, ws := e.setup()
	member := e.invite(admin, ws, "member@example.com")
	p := newProvider(t)
	existing := admin.addShared(ws, cliworker.Claude, p.URL)

	a := auth.New(e.db, auth.Options{HashParams: auth.Params{Memory: 64, Time: 1, Threads: 1}, SetupToken: setupToken})
	// The admin is disabled after RequireAdmin let the request through, before the handler runs.
	race := func(h http.HandlerFunc) http.Handler {
		return a.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := e.db.Exec(`UPDATE users SET disabled_at = '2030-01-01T00:00:00.000Z' WHERE id = ?`, admin.id); err != nil {
				t.Error(err)
			}
			h(w, r)
		}))
	}
	mux := http.NewServeMux()
	const base = "/api/workspaces/{workspace}/cli-accounts"
	mux.Handle("POST "+base, race(e.svc.handleSharedCreate))
	mux.Handle("PATCH "+base+"/modes", race(e.svc.handleSetModes))
	mux.Handle("PATCH "+base+"/{account}", race(e.svc.withShared(e.svc.handleSharedUpdate)))
	mux.Handle("DELETE "+base+"/{account}", race(e.svc.withShared(e.svc.handleSharedDelete)))

	call := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		if _, err := e.db.Exec(`UPDATE users SET disabled_at = NULL WHERE id = ?`, admin.id); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(admin.cookie)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		return rec
	}
	one := sharedPath(ws) + "/" + itoa(existing.ID)
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"create": call(http.MethodPost, sharedPath(ws), map[string]string{"cli": "claude", "label": "Another", "base_url": p.URL, "api_key": sharedKey}),
		"modes":  call(http.MethodPatch, sharedPath(ws)+"/modes", map[string]any{"default_mode": "shared", "members": map[string]any{itoa(member.id): "shared"}}),
		"update": call(http.MethodPatch, one, map[string]string{"label": "Renamed"}),
		"delete": call(http.MethodDelete, one, nil),
	} {
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), string(apierr.CodeAccountDisabled)) {
			t.Errorf("%s by a disabled admin: %d %s, want 403 %s", name, rec.Code, rec.Body.String(), apierr.CodeAccountDisabled)
		}
	}
	if _, err := e.db.Exec(`UPDATE users SET disabled_at = NULL WHERE id = ?`, admin.id); err != nil {
		t.Fatal(err)
	}
	list := decode[sharedList](t, admin.do(http.MethodGet, sharedPath(ws), nil))
	if len(list.Accounts) != 1 || list.Accounts[0].Label != existing.Label || list.DefaultMode != OwnElseShared {
		t.Fatalf("a disabled admin changed something: %+v", list)
	}
}

// identityIn reports whether the identity can be read in v: as it is, or in any part of it a
// base64 decoding opens (a token's dot-separated parts).
func identityIn(v, identity string) bool {
	if strings.Contains(v, identity) {
		return true
	}
	for _, part := range strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '=' || r == ' ' }) {
		for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.StdEncoding, base64.RawStdEncoding} {
			if b, err := enc.DecodeString(part); err == nil && strings.Contains(string(b), identity) {
				return true
			}
		}
	}
	return false
}

// The tokens a turn's CLI gets, the proxy's and the tools', name the account in a way only this
// server can read: the CLI holds them, and the account's identity never reaches it, even decoded.
func TestATurnsTokensDoNotShowTheIdentity(t *testing.T) {
	e := newTurnEnv(t)
	e.svc.SetTools(Tools{Handler: http.NotFoundHandler()})
	p := newProvider(t)
	a := e.owner.addShared(e.ws, cliworker.Claude, p.URL)
	conv := e.conversation(e.other)
	if d := done(e.turn(conv, turnBody{Turn: "t1", CLI: "claude", Model: "default", Prompt: "hello"})); d.Status != cliworker.StatusOK {
		t.Fatalf("turn = %+v", d)
	}
	calls := turnCalls(t, e.dirs(e.other.id).ConfigOf(cliworker.Claude, sharedSlot(a.ID)))
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	identity := e.acct(e.other.id).Identity
	var tokens int
	for _, kv := range calls[0].Env {
		if strings.HasPrefix(kv, "ANTHROPIC_AUTH_TOKEN=") || strings.HasPrefix(kv, "TASKTO_TOOLS_TOKEN=") {
			tokens++
		}
		if identityIn(kv, identity) {
			t.Errorf("the CLI's environment shows the account's identity: %s", kv)
		}
	}
	if tokens != 2 {
		t.Fatalf("fixture: the CLI got %d of the two tokens: %v", tokens, calls[0].Env)
	}
}
