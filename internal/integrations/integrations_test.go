package integrations

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/audit/audittest"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/db"
	"taskto.ai/taskto/internal/serverkey"
)

var setupToken = auth.NewSetupToken()

const password = "fake-password-for-tests"

var (
	stripeTestKey  = "sk_" + "test_" + strings.Repeat("Fake", 6)
	stripeOtherKey = "sk_" + "test_" + strings.Repeat("Rota", 6)
	stripeLiveKey  = "sk_" + "live_" + strings.Repeat("Fake", 6)
	stripeHook     = "whsec_" + strings.Repeat("Fake", 6)
	stripeHookNew  = "whsec_" + strings.Repeat("Rota", 6)
	squareToken    = "square-token-" + strings.Repeat("fake", 6)
)

type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

type env struct {
	t      *testing.T
	db     *sql.DB
	dir    string
	key    *serverkey.Key
	store  *Store
	svc    *Service
	h      http.Handler
	logs   *logs
	bodies bytes.Buffer
	mu     sync.Mutex
}

func newEnv(t *testing.T, publicURL string) *env {
	t.Helper()
	return newEnvWith(t, Options{PublicURL: publicURL})
}

func newEnvWith(t *testing.T, opts Options) *env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	sqlDB, err := db.Open(ctx, filepath.Join(dir, db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	key, err := serverkey.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, db: sqlDB, dir: dir, key: key, store: NewStore(sqlDB, key), logs: &logs{}}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	mux := http.NewServeMux()
	a := auth.New(sqlDB, auth.Options{HashParams: auth.Params{Memory: 64, Time: 1, Threads: 1}, SetupToken: setupToken})
	a.Routes(mux)
	spaces := access.New(sqlDB, a)
	spaces.Routes(mux)
	e.svc = New(e.store, opts)
	e.svc.Routes(mux, a, spaces)
	e.svc.HookRoutes(mux)
	e.h = a.ClientAddr(auth.CSRF(mux))
	return e
}

type client struct {
	e      *env
	cookie *http.Cookie
	id     int64
	bearer string
}

func (c *client) do(method, path string, body any) *httptest.ResponseRecorder {
	c.e.t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
		if method != http.MethodGet {
			r.Header.Set("Content-Type", "application/json")
		}
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			c.e.t.Fatal(err)
		}
		r = httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if c.bearer != "" {
		r.Header.Set("Authorization", "Bearer "+c.bearer)
	} else if c.cookie != nil {
		r.AddCookie(c.cookie)
	}
	rec := httptest.NewRecorder()
	c.e.h.ServeHTTP(rec, r)
	for _, ck := range rec.Result().Cookies() {
		if ck.MaxAge >= 0 {
			c.cookie = ck
		}
	}
	c.e.mu.Lock()
	c.e.bodies.Write(rec.Body.Bytes())
	c.e.mu.Unlock()
	return rec
}

func (c *client) raw(method, path, body string) *httptest.ResponseRecorder {
	c.e.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if c.cookie != nil {
		r.AddCookie(c.cookie)
	}
	rec := httptest.NewRecorder()
	c.e.h.ServeHTTP(rec, r)
	return rec
}

func mustDraft(t *testing.T, body map[string]any) draft {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var req createRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	d, err := newDraft(req)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func expect(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, status, rec.Body.String())
	}
}

func expectCodeNonFatal(t *testing.T, rec *httptest.ResponseRecorder, status int, code, label string) {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); rec.Code != status || err != nil || body.Code != code {
		t.Errorf("%s = %d %q, want %d %q", label, rec.Code, rec.Body.String(), status, code)
	}
}

func expectCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	expect(t, rec, status)
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Code != code {
		t.Fatalf("code = %q, want %q; body %s", body.Code, code, rec.Body.String())
	}
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

type me struct {
	User       struct{ ID int64 } `json:"user"`
	Workspaces []struct {
		ID int64 `json:"id"`
	} `json:"workspaces"`
}

func (e *env) setup() (*client, int64) {
	e.t.Helper()
	c := &client{e: e}
	rec := c.do(http.MethodPost, "/api/setup", map[string]string{
		"email": "owner@example.com", "name": "Owner", "password": password, "workspace_name": "Home", "setup_token": setupToken,
	})
	expect(e.t, rec, http.StatusCreated)
	m := decode[me](e.t, rec)
	c.id = m.User.ID
	return c, m.Workspaces[0].ID
}

func (e *env) invite(admin *client, ws int64, email, role string) *client {
	e.t.Helper()
	rec := admin.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", ws), map[string]string{"email": email, "role": role})
	expect(e.t, rec, http.StatusCreated)
	token := decode[struct {
		Token string `json:"token"`
	}](e.t, rec).Token
	c := &client{e: e}
	rec = c.do(http.MethodPost, "/api/invites/accept", map[string]string{"token": token, "name": email, "password": password})
	expect(e.t, rec, http.StatusCreated)
	c.id = decode[me](e.t, rec).User.ID
	return c
}

func (e *env) otherWorkspace(owner *client) (*client, int64) {
	e.t.Helper()
	rec := owner.do(http.MethodPost, "/api/workspaces", map[string]string{"name": "Second"})
	expect(e.t, rec, http.StatusCreated)
	return owner, decode[struct {
		Workspace auth.Membership `json:"workspace"`
	}](e.t, rec).Workspace.WorkspaceID
}

func wsPath(ws int64, rest string) string { return fmt.Sprintf("/api/workspaces/%d/%s", ws, rest) }

func hookFor(name string) string {
	if name == "main" || name == "shop" {
		return stripeHook
	}
	sum := sha256.Sum256([]byte(name))
	return "whsec_" + hex.EncodeToString(sum[:16])
}

func stripeBody(name string) map[string]any {
	return map[string]any{
		"provider": "stripe", "name": name, "display_name": "Stripe " + name,
		"secrets":          map[string]string{"api_key": stripeTestKey, "webhook_secret": hookFor(name)},
		"max_amount_minor": 500000, "currencies": []string{"eur", "USD"},
	}
}

func withHook(body map[string]any, secret string) map[string]any {
	secrets := map[string]string{}
	for k, v := range body["secrets"].(map[string]string) {
		secrets[k] = v
	}
	secrets["webhook_secret"] = secret
	body["secrets"] = secrets
	return body
}

func squareBody(name string) map[string]any {
	return map[string]any{
		"provider": "square", "name": name,
		"settings":         map[string]string{"environment": "sandbox", "location_id": "L123"},
		"secrets":          map[string]string{"api_key": squareToken},
		"max_amount_minor": 100000, "currencies": []string{"USD"},
	}
}

func (e *env) create(c *client, ws int64, body map[string]any) Integration {
	e.t.Helper()
	rec := c.do(http.MethodPost, wsPath(ws, "integrations"), body)
	expect(e.t, rec, http.StatusCreated)
	return decode[Integration](e.t, rec)
}

func (e *env) events(ws int64, action string) []audit.Row {
	e.t.Helper()
	rows, err := audit.List(context.Background(), e.db, audit.Filter{Workspace: ws, Action: action, Limit: 100})
	if err != nil {
		e.t.Fatal(err)
	}
	return rows
}

func TestCreateListGetAndLimits(t *testing.T) {
	e := newEnv(t, "https://tasks.example.com/")
	owner, ws := e.setup()
	in := e.create(owner, ws, stripeBody("main"))
	if in.Provider != "stripe" || in.Kind != "payments" || in.Mode != "test" || in.Name != "main" || in.DisplayName != "Stripe main" ||
		in.MaxAmountMinor != 500000 || strings.Join(in.Currencies, ",") != "EUR,USD" || in.Disabled || len(in.Spaces) != 0 {
		t.Fatalf("created = %+v", in)
	}
	if in.Secrets["api_key"] != "set" || in.Secrets["webhook_secret"] != "set" {
		t.Fatalf("secrets = %v", in.Secrets)
	}
	if in.WebhookURL == nil || !strings.HasPrefix(*in.WebhookURL, "https://tasks.example.com/hooks/") || len(*in.WebhookURL) != len("https://tasks.example.com/hooks/")+32 {
		t.Fatalf("webhook_url = %v", in.WebhookURL)
	}

	sq := e.create(owner, ws, squareBody("shop"))
	if sq.Mode != "test" || sq.Settings["location_id"] != "L123" || sq.Secrets["webhook_secret"] != "not set" {
		t.Fatalf("square = %+v", sq)
	}
	live := squareBody("shop-live")
	live["settings"] = map[string]string{"environment": "production", "location_id": "L9"}
	if got := e.create(owner, ws, live); got.Mode != "live" {
		t.Fatalf("production square mode = %q", got.Mode)
	}

	list := decode[struct {
		Integrations []Integration `json:"integrations"`
	}](t, owner.do(http.MethodGet, wsPath(ws, "integrations"), nil)).Integrations
	if len(list) != 3 || list[0].ID != in.ID || list[1].ID != sq.ID {
		t.Fatalf("list = %+v", list)
	}
	got := decode[Integration](t, owner.do(http.MethodGet, wsPath(ws, fmt.Sprintf("integrations/%d", in.ID)), nil))
	if got.ID != in.ID || got.Name != "main" {
		t.Fatalf("get = %+v", got)
	}
	e2 := newEnv(t, "")
	owner2, ws2 := e2.setup()
	if got := e2.create(owner2, ws2, stripeBody("main")); got.WebhookURL != nil {
		t.Fatalf("webhook_url without a public URL = %v", got.WebhookURL)
	}
}

func TestRegistryIsClosedAndListed(t *testing.T) {
	e := newEnv(t, "")
	owner, ws := e.setup()
	rec := owner.do(http.MethodGet, wsPath(ws, "integration-providers"), nil)
	expect(t, rec, http.StatusOK)
	reg := decode[Registry](t, rec)
	if len(reg.Providers) != 2 || reg.Providers[0].ID != "stripe" || reg.Providers[1].ID != "square" ||
		reg.WebhooksAvailable || reg.MaxAmountMinor != MaxAmountMinor || len(reg.Currencies) < 10 {
		t.Fatalf("registry = %+v", reg)
	}
	expectCode(t, owner.do(http.MethodPost, wsPath(ws, "integrations"), map[string]any{
		"provider": "paypal", "name": "x", "secrets": map[string]string{"api_key": stripeTestKey}, "max_amount_minor": 1, "currencies": []string{"USD"},
	}), http.StatusBadRequest, "integration_provider_unknown")
	in := e.create(owner, ws, stripeBody("main"))
	one := wsPath(ws, fmt.Sprintf("integrations/%d", in.ID))
	expectCode(t, owner.do(http.MethodPatch, one, map[string]any{"provider": "square"}), http.StatusBadRequest, "invalid_json")
	expectCode(t, owner.do(http.MethodPatch, one, map[string]any{"name": "other"}), http.StatusBadRequest, "invalid_json")
	for _, p := range Providers() {
		if err := audit.Record(context.Background(), e.db, audit.Event{Workspace: ws, Action: audit.IntegrationRemoved,
			TargetType: audit.TargetIntegration, Details: audit.Details{"provider": p.ID(), "kind": p.Kinds()[0]}}); err != nil {
			t.Errorf("the audit log can't record provider %s: %v", p.ID(), err)
		}
	}
}

func TestValidation(t *testing.T) {
	e := newEnv(t, "")
	owner, ws := e.setup()
	post := func(mutate func(map[string]any)) *httptest.ResponseRecorder {
		body := stripeBody("main")
		mutate(body)
		return owner.do(http.MethodPost, wsPath(ws, "integrations"), body)
	}
	for name, tc := range map[string]struct {
		mutate func(map[string]any)
		status int
		code   string
	}{
		"bad name":     {func(b map[string]any) { b["name"] = "Bad Name" }, 400, "integration_name_invalid"},
		"empty name":   {func(b map[string]any) { b["name"] = "" }, 400, "integration_name_invalid"},
		"long name":    {func(b map[string]any) { b["name"] = strings.Repeat("a", 41) }, 400, "integration_name_invalid"},
		"display name": {func(b map[string]any) { b["display_name"] = strings.Repeat("a", 101) }, 400, "integration_display_name_invalid"},
		"control name": {func(b map[string]any) { b["display_name"] = "a\x07b" }, 400, "integration_display_name_invalid"},
		"no key":       {func(b map[string]any) { b["secrets"] = map[string]string{} }, 400, "integration_secret_required"},
		"publishable key": {func(b map[string]any) {
			b["secrets"] = map[string]string{"api_key": "pk_" + "test_" + strings.Repeat("Fake", 6)}
		}, 400, "integration_secret_invalid"},
		"bad hook secret": {func(b map[string]any) {
			b["secrets"] = map[string]string{"api_key": stripeTestKey, "webhook_secret": "nope"}
		}, 400, "integration_secret_invalid"},
		"extra secret":    {func(b map[string]any) { b["secrets"] = map[string]string{"api_key": stripeTestKey, "password": "x"} }, 400, "bad_request"},
		"extra setting":   {func(b map[string]any) { b["settings"] = map[string]string{"base_url": "https://evil.example"} }, 400, "integration_setting_invalid"},
		"no ceiling":      {func(b map[string]any) { delete(b, "max_amount_minor") }, 400, "integration_limit_invalid"},
		"negative":        {func(b map[string]any) { b["max_amount_minor"] = -5 }, 400, "integration_limit_invalid"},
		"too big":         {func(b map[string]any) { b["max_amount_minor"] = MaxAmountMinor + 1 }, 400, "integration_limit_invalid"},
		"no currencies":   {func(b map[string]any) { b["currencies"] = []string{} }, 400, "integration_currency_invalid"},
		"unknown money":   {func(b map[string]any) { b["currencies"] = []string{"EUR", "XXX"} }, 400, "integration_currency_invalid"},
		"duplicate money": {func(b map[string]any) { b["currencies"] = []string{"EUR", "eur"} }, 400, "integration_currency_invalid"},
		"strict body":     {func(b map[string]any) { b["admin"] = true }, 400, "invalid_json"},
	} {
		t.Run(name, func(t *testing.T) { expectCode(t, post(tc.mutate), tc.status, tc.code) })
	}
	for name, settings := range map[string]map[string]string{
		"none":          nil,
		"no location":   {"environment": "sandbox"},
		"bad choice":    {"environment": "moon", "location_id": "L1"},
		"a url":         {"environment": "sandbox", "location_id": "https://evil.example/x"},
		"a host":        {"environment": "sandbox", "location_id": "evil.example:8080"},
		"unknown field": {"environment": "sandbox", "location_id": "L1", "base_url": "x"},
	} {
		t.Run("square "+name, func(t *testing.T) {
			body := squareBody("shop")
			body["settings"] = settings
			expectCode(t, owner.do(http.MethodPost, wsPath(ws, "integrations"), body), http.StatusBadRequest, "integration_setting_invalid")
		})
	}
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM integrations`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("integrations after refused creates = %d, %v", n, err)
	}
	if got := e.events(ws, audit.IntegrationCreated); len(got) != 0 {
		t.Fatalf("events after refused creates = %+v", got)
	}
	e.create(owner, ws, stripeBody("main"))
	expectCode(t, owner.do(http.MethodPost, wsPath(ws, "integrations"), stripeBody("main")), http.StatusConflict, "integration_name_taken")
	for i := 1; i < maxIntegrations; i++ {
		e.create(owner, ws, stripeBody(fmt.Sprintf("n%d", i)))
	}
	expectCode(t, owner.do(http.MethodPost, wsPath(ws, "integrations"), stripeBody("one-more")), http.StatusConflict, "too_many_integrations")
}

func TestUpdateReplacesAndRotates(t *testing.T) {
	e := newEnv(t, "")
	owner, ws := e.setup()
	in := e.create(owner, ws, stripeBody("main"))
	one := wsPath(ws, fmt.Sprintf("integrations/%d", in.ID))

	rec := owner.do(http.MethodPatch, one, map[string]any{"display_name": "Payments", "max_amount_minor": 20000, "currencies": []string{"BRL"}})
	expect(t, rec, http.StatusOK)
	got := decode[Integration](t, rec)
	if got.DisplayName != "Payments" || got.MaxAmountMinor != 20000 || strings.Join(got.Currencies, ",") != "BRL" || got.Mode != "test" {
		t.Fatalf("updated = %+v", got)
	}
	rec = owner.do(http.MethodPatch, one, map[string]any{"secrets": map[string]string{"api_key": stripeLiveKey}})
	expect(t, rec, http.StatusOK)
	if got := decode[Integration](t, rec); got.Mode != "live" || got.Secrets["api_key"] != "set" {
		t.Fatalf("after a live key = %+v", got)
	}
	expectCode(t, owner.do(http.MethodPatch, one, map[string]any{"display_name": "Half", "secrets": map[string]string{"api_key": "nope"}}),
		http.StatusBadRequest, "integration_secret_invalid")
	expectCode(t, owner.do(http.MethodPatch, one, map[string]any{"currencies": []string{}}), http.StatusBadRequest, "integration_currency_invalid")
	expectCode(t, owner.do(http.MethodPatch, one, map[string]any{"max_amount_minor": 0}), http.StatusBadRequest, "integration_limit_invalid")
	if got := decode[Integration](t, owner.do(http.MethodGet, one, nil)); got.DisplayName != "Payments" {
		t.Fatalf("a refused edit changed the integration: %+v", got)
	}

	expect(t, owner.do(http.MethodPatch, one, map[string]any{"secrets": map[string]string{"webhook_secret": stripeHookNew}}), http.StatusOK)
	creds, err := e.store.credentials(context.Background(), ws, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if creds.WebhookSecret.Reveal() != stripeHookNew || creds.WebhookSecretPrevious.Reveal() != stripeHook || creds.APIKey.Reveal() != stripeLiveKey {
		t.Fatal("the credentials aren't the ones set")
	}
	e.store.now = func() time.Time { return time.Now().Add(previousOverlap + time.Minute) }
	creds, err = e.store.credentials(context.Background(), ws, in.ID)
	if err != nil || creds.WebhookSecretPrevious != "" || creds.WebhookSecret.Reveal() != stripeHookNew {
		t.Fatalf("after the overlap = %v, %v", creds.WebhookSecretPrevious, err)
	}
	e.store.now = time.Now

	expect(t, owner.do(http.MethodPatch, one, map[string]any{"disabled": true}), http.StatusOK)
	if got := decode[Integration](t, owner.do(http.MethodGet, one, nil)); !got.Disabled || got.DisabledAt == nil {
		t.Fatalf("disabled = %+v", got)
	}
	expect(t, owner.do(http.MethodPatch, one, map[string]any{"disabled": true}), http.StatusOK)
	expect(t, owner.do(http.MethodPatch, one, map[string]any{"disabled": false}), http.StatusOK)
	if got := decode[Integration](t, owner.do(http.MethodGet, one, nil)); got.Disabled {
		t.Fatalf("enabled = %+v", got)
	}

	sq := e.create(owner, ws, squareBody("shop"))
	rec = owner.do(http.MethodPatch, wsPath(ws, fmt.Sprintf("integrations/%d", sq.ID)),
		map[string]any{"settings": map[string]string{"environment": "production", "location_id": "L2"}})
	expect(t, rec, http.StatusOK)
	if got := decode[Integration](t, rec); got.Mode != "live" || got.Settings["location_id"] != "L2" {
		t.Fatalf("square updated = %+v", got)
	}
	expectCode(t, owner.do(http.MethodPatch, wsPath(ws, fmt.Sprintf("integrations/%d", sq.ID)), map[string]any{"settings": map[string]string{}}),
		http.StatusBadRequest, "integration_setting_invalid")
}

func TestSecretsNeverLeave(t *testing.T) {
	e := newEnv(t, "https://tasks.example.com")
	owner, ws := e.setup()
	member := e.invite(owner, ws, "member@example.com", "member")
	in := e.create(owner, ws, stripeBody("main"))
	one := wsPath(ws, fmt.Sprintf("integrations/%d", in.ID))
	owner.do(http.MethodGet, wsPath(ws, "integrations"), nil)
	owner.do(http.MethodGet, one, nil)
	owner.do(http.MethodPost, one+"/check", nil)
	const badHook = "not-a-hook-secret-zzzz"
	member.do(http.MethodPatch, one, map[string]any{"secrets": map[string]string{"api_key": stripeOtherKey}})
	member.do(http.MethodPost, wsPath(ws, "integrations"), stripeBody("sneaky"))
	owner.do(http.MethodPost, wsPath(ws, "integrations"), map[string]any{"provider": "stripe", "name": "Bad Name", "secrets": map[string]string{"api_key": stripeOtherKey}})
	owner.do(http.MethodPatch, one, map[string]any{"secrets": map[string]string{"api_key": stripeOtherKey, "webhook_secret": badHook}})
	expect(t, owner.do(http.MethodPatch, one, map[string]any{"secrets": map[string]string{"api_key": stripeOtherKey, "webhook_secret": stripeHookNew}}), http.StatusOK)
	rec := owner.do(http.MethodPatch, one, map[string]any{"secrets": []string{stripeLiveKey}})
	expect(t, rec, http.StatusBadRequest)

	var stored []byte
	rows, err := e.db.Query(`SELECT sealed FROM integration_secrets`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		if err := rows.Scan(&stored); err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{stripeTestKey, stripeOtherKey, stripeHook, stripeHookNew} {
			if bytes.Contains(stored, []byte(s)) {
				t.Errorf("the database holds a secret in clear")
			}
		}
	}
	_ = rows.Close()
	e.mu.Lock()
	bodies := e.bodies.String()
	e.mu.Unlock()
	secrets := map[string]string{"test key": stripeTestKey, "rotated key": stripeOtherKey, "live key": stripeLiveKey, "webhook secret": stripeHook, "new webhook secret": stripeHookNew, "rejected webhook secret": badHook}
	for name, text := range map[string]string{"a response": bodies, "the log": e.logs.String()} {
		for what, s := range secrets {
			if strings.Contains(text, s) {
				t.Errorf("%s contains the %s", name, what)
			}
		}
	}
	audittest.NoSecrets(t, e.db, secrets)

	creds, err := e.store.credentials(context.Background(), ws, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(creds)
	var out bytes.Buffer
	l := slog.New(slog.NewTextHandler(&out, nil))
	l.Info("credentials", "creds", creds, "key", creds.APIKey)
	for how, text := range map[string]string{
		"%v": fmt.Sprintf("%v", creds), "%+v": fmt.Sprintf("%+v", creds), "%#v": fmt.Sprintf("%#v", creds),
		"%s": fmt.Sprintf("%s", creds.APIKey), "json": string(raw), "slog": out.String(),
	} {
		for what, s := range secrets {
			if strings.Contains(text, s) {
				t.Errorf("formatting credentials with %s shows the %s", how, what)
			}
		}
	}
	if creds.APIKey.Reveal() != stripeOtherKey {
		t.Fatal("Reveal doesn't give the key")
	}
}

func TestSealedSecretsAreBoundToTheirRow(t *testing.T) {
	e := newEnv(t, "")
	owner, ws := e.setup()
	a := e.create(owner, ws, stripeBody("a"))
	b := e.create(owner, ws, stripeBody("b"))
	if _, err := e.db.Exec(`UPDATE integration_secrets SET sealed = (SELECT sealed FROM integration_secrets WHERE integration_id = ? AND name = 'api_key')
		WHERE integration_id = ? AND name = 'api_key'`, a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.credentials(context.Background(), ws, b.ID); err == nil {
		t.Fatal("a sealed key copied to another integration opened")
	}
	rec := owner.do(http.MethodPost, wsPath(ws, fmt.Sprintf("integrations/%d/check", b.ID)), nil)
	expect(t, rec, http.StatusOK)
	if got := decode[CheckResult](t, rec); got.OK || got.Code != "integration_secret_unreadable" || got.Params["secret"] != "api_key" {
		t.Fatalf("check = %+v", got)
	}
	other, err := serverkey.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.store.key = other
	if got := decode[CheckResult](t, owner.do(http.MethodPost, wsPath(ws, fmt.Sprintf("integrations/%d/check", a.ID)), nil)); got.OK || got.Code != "integration_secret_unreadable" {
		t.Fatalf("check with another server key = %+v", got)
	}
}

func TestCheck(t *testing.T) {
	e := newEnv(t, "")
	owner, ws := e.setup()
	in := e.create(owner, ws, stripeBody("main"))
	rec := owner.do(http.MethodPost, wsPath(ws, fmt.Sprintf("integrations/%d/check", in.ID)), nil)
	expect(t, rec, http.StatusOK)
	if got := decode[CheckResult](t, rec); !got.OK || got.Remote {
		t.Fatalf("check = %+v", got)
	}
	if _, err := e.db.Exec(`DELETE FROM integration_secrets WHERE integration_id = ? AND name = 'api_key'`, in.ID); err != nil {
		t.Fatal(err)
	}
	if got := decode[CheckResult](t, owner.do(http.MethodPost, wsPath(ws, fmt.Sprintf("integrations/%d/check", in.ID)), nil)); got.OK || got.Code != "integration_secret_required" {
		t.Fatalf("check without a key = %+v", got)
	}
}

func TestRemove(t *testing.T) {
	e := newEnv(t, "")
	owner, ws := e.setup()
	in := e.create(owner, ws, stripeBody("main"))
	one := wsPath(ws, fmt.Sprintf("integrations/%d", in.ID))
	shared := e.sharedSpace(owner, ws, "Shop")
	expect(t, owner.do(http.MethodPut, one+"/spaces", map[string]any{"spaces": []int64{shared}}), http.StatusOK)
	count := func() (n int) {
		if err := e.db.QueryRow(`SELECT (SELECT count(*) FROM integration_secrets) + (SELECT count(*) FROM integration_spaces)`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	if before < 3 {
		t.Fatalf("secrets and an approval = %d rows", before)
	}
	if _, err := e.db.Exec(`CREATE TABLE guard (integration_id INTEGER REFERENCES integrations (id) ON DELETE RESTRICT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`INSERT INTO guard VALUES (?)`, in.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, owner.do(http.MethodDelete, one, nil), http.StatusConflict, "integration_in_use")
	expect(t, owner.do(http.MethodGet, one, nil), http.StatusOK)
	if got := count(); got != before {
		t.Fatalf("a refused removal left %d secret and approval rows, want %d", got, before)
	}
	if got := e.events(ws, audit.IntegrationRemoved); len(got) != 0 {
		t.Fatalf("a refused removal recorded %+v", got)
	}
	if _, err := e.db.Exec(`DELETE FROM guard`); err != nil {
		t.Fatal(err)
	}
	expect(t, owner.do(http.MethodDelete, one, nil), http.StatusNoContent)
	expectCode(t, owner.do(http.MethodGet, one, nil), http.StatusNotFound, "integration_not_found")
	expectCode(t, owner.do(http.MethodDelete, one, nil), http.StatusNotFound, "integration_not_found")
	if n := count(); n != 0 {
		t.Fatalf("secrets and spaces left after a removal = %d", n)
	}
	expectCode(t, owner.do(http.MethodGet, wsPath(ws, "integrations/abc"), nil), http.StatusNotFound, "integration_not_found")
}

func (e *env) sharedSpace(c *client, ws int64, name string) int64 {
	e.t.Helper()
	rec := c.do(http.MethodPost, wsPath(ws, "spaces"), map[string]string{"name": name})
	expect(e.t, rec, http.StatusCreated)
	return decode[struct {
		ID int64 `json:"id"`
	}](e.t, rec).ID
}

func (e *env) privateSpace(c *client, ws int64) int64 {
	e.t.Helper()
	rec := c.do(http.MethodGet, wsPath(ws, "spaces"), nil)
	expect(e.t, rec, http.StatusOK)
	for _, s := range decode[struct {
		Spaces []access.Space `json:"spaces"`
	}](e.t, rec).Spaces {
		if s.Kind == access.Private {
			return s.ID
		}
	}
	e.t.Fatal("no private space")
	return 0
}

func TestSpacesAndUsable(t *testing.T) {
	e := newEnv(t, "")
	owner, ws := e.setup()
	member := e.invite(owner, ws, "member@example.com", "member")
	in := e.create(owner, ws, stripeBody("main"))
	shared := e.sharedSpace(owner, ws, "Shop")
	mine := e.privateSpace(owner, ws)
	theirs := e.privateSpace(member, ws)
	spaces := wsPath(ws, fmt.Sprintf("integrations/%d/spaces", in.ID))
	ctx := context.Background()

	if ok, err := e.store.usable(ctx, ws, in.ID, shared); err != nil || ok {
		t.Fatalf("usable before allowing = %v, %v", ok, err)
	}
	rec := owner.do(http.MethodPut, spaces, map[string]any{"spaces": []int64{shared, mine, shared}})
	expect(t, rec, http.StatusOK)
	if got := decode[Integration](t, rec); len(got.Spaces) != 2 {
		t.Fatalf("spaces = %v", got.Spaces)
	}
	if ok, err := e.store.usable(ctx, ws, in.ID, shared); err != nil || !ok {
		t.Fatalf("usable after allowing = %v, %v", ok, err)
	}
	expectCode(t, owner.do(http.MethodPut, spaces, map[string]any{"spaces": []int64{shared, theirs}}), http.StatusNotFound, "space_not_found")
	expectCode(t, owner.do(http.MethodPut, spaces, map[string]any{"spaces": []int64{999999}}), http.StatusNotFound, "space_not_found")
	expectCode(t, owner.do(http.MethodPut, spaces, map[string]any{}), http.StatusBadRequest, "bad_request")
	if got := decode[Integration](t, owner.do(http.MethodGet, wsPath(ws, fmt.Sprintf("integrations/%d", in.ID)), nil)); len(got.Spaces) != 2 {
		t.Fatalf("a refused change moved the spaces: %v", got.Spaces)
	}
	expect(t, owner.do(http.MethodPatch, wsPath(ws, fmt.Sprintf("integrations/%d", in.ID)), map[string]any{"disabled": true}), http.StatusOK)
	if ok, err := e.store.usable(ctx, ws, in.ID, shared); err != nil || ok {
		t.Fatalf("a disabled integration is usable: %v, %v", ok, err)
	}
	expect(t, owner.do(http.MethodPatch, wsPath(ws, fmt.Sprintf("integrations/%d", in.ID)), map[string]any{"disabled": false}), http.StatusOK)
	expect(t, owner.do(http.MethodPut, spaces, map[string]any{"spaces": []int64{mine}}), http.StatusOK)
	if ok, err := e.store.usable(ctx, ws, in.ID, shared); err != nil || ok {
		t.Fatalf("an integration is usable in a space it was taken from: %v, %v", ok, err)
	}
	if ok, err := e.store.usable(ctx, ws+1000, in.ID, mine); err != nil || ok {
		t.Fatalf("an integration is usable by another workspace's id: %v, %v", ok, err)
	}
	otherWs, err := e.db.Exec(`INSERT INTO workspaces (name) VALUES ('Other')`)
	if err != nil {
		t.Fatal(err)
	}
	ow, _ := otherWs.LastInsertId()
	sp, err := e.db.Exec(`INSERT INTO spaces (workspace_id, kind, name) VALUES (?, 'shared', 'x')`, ow)
	if err != nil {
		t.Fatal(err)
	}
	spID, _ := sp.LastInsertId()
	if _, err := e.db.Exec(`INSERT INTO integration_spaces (integration_id, space_id) VALUES (?, ?)`, in.ID, spID); err == nil {
		t.Fatal("the schema allowed a space of another workspace")
	}
}

func TestSpaceReadersSeeNoSecrets(t *testing.T) {
	e := newEnv(t, "https://tasks.example.com")
	owner, ws := e.setup()
	member := e.invite(owner, ws, "member@example.com", "member")
	outsider := e.invite(owner, ws, "outsider@example.com", "member")
	in := e.create(owner, ws, stripeBody("main"))
	hidden := e.create(owner, ws, stripeBody("hidden"))
	shared := e.sharedSpace(owner, ws, "Shop")
	expect(t, owner.do(http.MethodPost, wsPath(ws, fmt.Sprintf("spaces/%d/members", shared)), map[string]any{"user_id": member.id, "role": "viewer"}), http.StatusNoContent)
	expect(t, owner.do(http.MethodPut, wsPath(ws, fmt.Sprintf("integrations/%d/spaces", in.ID)), map[string]any{"spaces": []int64{shared}}), http.StatusOK)
	expect(t, owner.do(http.MethodPatch, wsPath(ws, fmt.Sprintf("integrations/%d", hidden.ID)), map[string]any{"display_name": "Hidden"}), http.StatusOK)

	path := wsPath(ws, fmt.Sprintf("spaces/%d/integrations", shared))
	rec := member.do(http.MethodGet, path, nil)
	expect(t, rec, http.StatusOK)
	got := decode[struct {
		Integrations []SpaceIntegration `json:"integrations"`
	}](t, rec).Integrations
	if len(got) != 1 || got[0].ID != in.ID || got[0].Kind != "payments" || got[0].Status != "active" || got[0].Name != "main" {
		t.Fatalf("a member's list = %+v", got)
	}
	for _, leak := range []string{"secret", "webhook", "hooks", "public", "api_key", "mode", "currencies", "provider"} {
		if strings.Contains(strings.ToLower(rec.Body.String()), leak) {
			t.Errorf("the space's list shows %q: %s", leak, rec.Body.String())
		}
	}
	expect(t, owner.do(http.MethodPatch, wsPath(ws, fmt.Sprintf("integrations/%d", in.ID)), map[string]any{"disabled": true}), http.StatusOK)
	if got := decode[struct {
		Integrations []SpaceIntegration `json:"integrations"`
	}](t, member.do(http.MethodGet, path, nil)).Integrations; got[0].Status != "disabled" {
		t.Fatalf("status = %q", got[0].Status)
	}
	expectCode(t, outsider.do(http.MethodGet, path, nil), http.StatusNotFound, "space_not_found")
	expectCode(t, (&client{e: e}).do(http.MethodGet, path, nil), http.StatusUnauthorized, "not_logged_in")
	rec = member.do(http.MethodPost, wsPath(ws, "tokens"), map[string]any{"name": "t"})
	expect(t, rec, http.StatusCreated)
	token := &client{e: e, bearer: decode[auth.TokenCreated](t, rec).Secret}
	if rec := token.do(http.MethodGet, path, nil); rec.Code != http.StatusForbidden {
		t.Errorf("an access token on the space route = %d, want 403; body %s", rec.Code, rec.Body.String())
	}
	_, ws2 := e.otherWorkspace(owner)
	expectCode(t, owner.do(http.MethodGet, wsPath(ws2, fmt.Sprintf("spaces/%d/integrations", shared)), nil), http.StatusNotFound, "space_not_found")
}

func TestIsolation(t *testing.T) {
	e := newEnv(t, "")
	owner, ws := e.setup()
	member := e.invite(owner, ws, "member@example.com", "member")
	in := e.create(owner, ws, stripeBody("main"))
	_, ws2 := e.otherWorkspace(owner)
	admin2 := e.invite(owner, ws2, "admin2@example.com", "admin")
	in2 := e.create(owner, ws2, withHook(stripeBody("main"), stripeHookNew))

	one := wsPath(ws, fmt.Sprintf("integrations/%d", in.ID))
	routes := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, wsPath(ws, "integrations"), nil},
		{http.MethodGet, wsPath(ws, "integration-providers"), nil},
		{http.MethodPost, wsPath(ws, "integrations"), stripeBody("another")},
		{http.MethodGet, one, nil},
		{http.MethodPatch, one, map[string]any{"display_name": "Mine now"}},
		{http.MethodDelete, one, nil},
		{http.MethodPut, one + "/spaces", map[string]any{"spaces": []int64{}}},
		{http.MethodPost, one + "/check", nil},
	}
	rec := owner.do(http.MethodPost, wsPath(ws, "tokens"), map[string]any{"name": "t"})
	expect(t, rec, http.StatusCreated)
	token := &client{e: e, bearer: decode[auth.TokenCreated](t, rec).Secret}

	for _, r := range routes {
		label := r.method + " " + r.path
		expectCode(t, member.do(r.method, r.path, r.body), http.StatusForbidden, "admin_only")
		if rec := token.do(r.method, r.path, r.body); rec.Code != http.StatusForbidden {
			t.Errorf("%s with an access token = %d, want 403", label, rec.Code)
		}
		if rec := (&client{e: e}).do(r.method, r.path, r.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session = %d, want 401", label, rec.Code)
		}
		if rec := admin2.do(r.method, r.path, r.body); rec.Code != http.StatusNotFound {
			t.Errorf("%s as another workspace's admin = %d, want 404", label, rec.Code)
		}
	}
	other := wsPath(ws2, fmt.Sprintf("integrations/%d", in.ID))
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, other}, {http.MethodPatch, other}, {http.MethodDelete, other}, {http.MethodPut, other + "/spaces"}, {http.MethodPost, other + "/check"},
	} {
		var body any
		switch r.method {
		case http.MethodPatch:
			body = map[string]any{"display_name": "stolen"}
		case http.MethodPut:
			body = map[string]any{"spaces": []int64{}}
		}
		expectCode(t, admin2.do(r.method, r.path, body), http.StatusNotFound, "integration_not_found")
	}
	if got := decode[Integration](t, owner.do(http.MethodGet, one, nil)); got.DisplayName != "Stripe main" {
		t.Fatalf("another workspace changed the integration: %+v", got)
	}
	list := decode[struct {
		Integrations []Integration `json:"integrations"`
	}](t, admin2.do(http.MethodGet, wsPath(ws2, "integrations"), nil)).Integrations
	if len(list) != 1 || list[0].ID != in2.ID {
		t.Fatalf("the other workspace's list = %+v", list)
	}
	sharedHere := e.sharedSpace(owner, ws, "Shop")
	expectCode(t, admin2.do(http.MethodPut, wsPath(ws2, fmt.Sprintf("integrations/%d/spaces", in2.ID)), map[string]any{"spaces": []int64{sharedHere}}),
		http.StatusNotFound, "space_not_found")
	if got := e.events(ws2, audit.IntegrationSpacesChanged); len(got) != 0 {
		t.Fatalf("a refused grant recorded an event in the requested workspace: %+v", got)
	}
	for _, action := range []string{audit.IntegrationUpdated, audit.IntegrationSpacesChanged, audit.IntegrationRemoved, audit.IntegrationDisabled} {
		if got := e.events(ws, action); len(got) != 0 {
			t.Fatalf("refused requests recorded %s: %+v", action, got)
		}
	}
	if got := e.events(ws, audit.IntegrationCreated); len(got) != 1 {
		t.Fatalf("refused creates recorded events: %+v", got)
	}
}

func TestAdminsNeedNotBeOperators(t *testing.T) {
	e := newEnv(t, "")
	owner, _ := e.setup()
	_, ws2 := e.otherWorkspace(owner)
	admin2 := e.invite(owner, ws2, "admin2@example.com", "admin")
	in := e.create(admin2, ws2, stripeBody("main"))
	expect(t, admin2.do(http.MethodPatch, wsPath(ws2, fmt.Sprintf("integrations/%d", in.ID)), map[string]any{"display_name": "Mine"}), http.StatusOK)
	expect(t, admin2.do(http.MethodDelete, wsPath(ws2, fmt.Sprintf("integrations/%d", in.ID)), nil), http.StatusNoContent)
}

func TestAudit(t *testing.T) {
	e := newEnv(t, "")
	owner, ws := e.setup()
	in := e.create(owner, ws, stripeBody("main"))
	one := wsPath(ws, fmt.Sprintf("integrations/%d", in.ID))

	created := e.events(ws, audit.IntegrationCreated)
	if len(created) != 1 {
		t.Fatalf("created = %+v", created)
	}
	c := created[0]
	if *c.ActorID != owner.id || *c.TargetID != in.ID || c.TargetType != audit.TargetIntegration || c.TargetCreatedAt == nil ||
		c.Details["provider"] != "stripe" || c.Details["kind"] != "payments" || c.Details["mode"] != "test" ||
		c.Details["api_key_set"] != true || c.Details["webhook_secret_set"] != true || len(c.Details) != 5 {
		t.Fatalf("created = %+v", c)
	}

	expect(t, owner.do(http.MethodPatch, one, map[string]any{"display_name": "Payments", "max_amount_minor": 1000, "secrets": map[string]string{"api_key": stripeLiveKey}}), http.StatusOK)
	updated := e.events(ws, audit.IntegrationUpdated)
	if len(updated) != 1 {
		t.Fatalf("updated = %+v", updated)
	}
	u := updated[0]
	if fmt.Sprint(u.Details["fields"]) != "[api_key display_name max_amount]" || u.Details["api_key_changed"] != true ||
		u.Details["webhook_secret_changed"] != false || u.Details["mode"] != "live" || u.Details["provider"] != "stripe" {
		t.Fatalf("updated = %+v", u)
	}
	expect(t, owner.do(http.MethodPatch, one, map[string]any{"display_name": "Payments", "currencies": []string{"USD", "EUR"}}), http.StatusOK)
	reordered := e.events(ws, audit.IntegrationUpdated)
	if len(reordered) != 2 || fmt.Sprint(reordered[0].Details["fields"]) != "[currencies]" {
		t.Fatalf("updated after a reorder = %+v", reordered)
	}
	n := len(e.events(ws, audit.IntegrationUpdated))
	for range 2 {
		expect(t, owner.do(http.MethodPatch, one, map[string]any{"display_name": "Payments"}), http.StatusOK)
		if got := len(e.events(ws, audit.IntegrationUpdated)); got != n {
			t.Fatalf("an edit that changed nothing recorded an event: %d, was %d", got, n)
		}
	}

	expect(t, owner.do(http.MethodPatch, one, map[string]any{"disabled": true}), http.StatusOK)
	expect(t, owner.do(http.MethodPatch, one, map[string]any{"disabled": false}), http.StatusOK)
	if d, en := e.events(ws, audit.IntegrationDisabled), e.events(ws, audit.IntegrationEnabled); len(d) != 1 || len(en) != 1 || *d[0].TargetID != in.ID {
		t.Fatalf("disabled = %+v, enabled = %+v", d, en)
	}

	shared := e.sharedSpace(owner, ws, "Shop")
	expect(t, owner.do(http.MethodPut, one+"/spaces", map[string]any{"spaces": []int64{shared}}), http.StatusOK)
	expect(t, owner.do(http.MethodPut, one+"/spaces", map[string]any{"spaces": []int64{shared}}), http.StatusOK)
	expect(t, owner.do(http.MethodPut, one+"/spaces", map[string]any{"spaces": []int64{}}), http.StatusOK)
	sc := e.events(ws, audit.IntegrationSpacesChanged)
	if len(sc) != 2 || sc[1].Details["added"] != float64(1) || sc[1].Details["removed"] != float64(0) || sc[0].Details["removed"] != float64(1) {
		t.Fatalf("spaces changed = %+v", sc)
	}

	expect(t, owner.do(http.MethodDelete, one, nil), http.StatusNoContent)
	removed := e.events(ws, audit.IntegrationRemoved)
	if len(removed) != 1 || *removed[0].TargetID != in.ID || removed[0].Details["provider"] != "stripe" || removed[0].TargetCreatedAt == nil ||
		*removed[0].TargetCreatedAt != *c.TargetCreatedAt {
		t.Fatalf("removed = %+v", removed)
	}
	audittest.NoSecrets(t, e.db, map[string]string{"key": stripeTestKey, "live key": stripeLiveKey, "webhook": stripeHook, "name": "Payments"})
}

func TestAuditShowsTheIntegrationByName(t *testing.T) {
	e := newEnv(t, "")
	owner, ws := e.setup()
	in := e.create(owner, ws, stripeBody("main"))
	page, err := audit.ListWorkspace(context.Background(), e.db, ws, audit.Query{Action: audit.IntegrationCreated})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].TargetName == nil || *page.Events[0].TargetName != "Stripe main" || *page.Events[0].TargetID != in.ID {
		t.Fatalf("events = %+v", page.Events)
	}
	expect(t, owner.do(http.MethodDelete, wsPath(ws, fmt.Sprintf("integrations/%d", in.ID)), nil), http.StatusNoContent)
	page, err = audit.ListWorkspace(context.Background(), e.db, ws, audit.Query{Action: audit.IntegrationCreated})
	if err != nil || page.Events[0].TargetName != nil {
		t.Fatalf("after removal = %+v, %v", page.Events, err)
	}
}

func TestImmutableColumnsCannotBeUpdated(t *testing.T) {
	e := newEnv(t, "")
	owner, ws := e.setup()
	in := e.create(owner, ws, stripeBody("main"))
	_, ws2 := e.otherWorkspace(owner)
	other := e.create(owner, ws, stripeBody("second"))
	for _, set := range []string{
		"name = 'x'", "provider = 'square'", "public_id = '" + strings.Repeat("a", 32) + "'",
		fmt.Sprintf("workspace_id = %d", ws2), fmt.Sprintf("id = %d", in.ID+1000), "created_at = '2000-01-01T00:00:00.000Z'",
	} {
		if _, err := e.db.Exec(`UPDATE integrations SET `+set+` WHERE id = ?`, in.ID); err == nil || !strings.Contains(err.Error(), "never change") {
			t.Errorf("UPDATE integrations SET %s: %v, want the trigger's refusal", set, err)
		}
	}
	if _, err := e.db.Exec(`DELETE FROM integration_secrets WHERE integration_id = ? AND name = 'api_key'`, other.ID); err != nil {
		t.Fatal(err)
	}
	for _, set := range []string{"name = 'webhook_secret_previous'", fmt.Sprintf("integration_id = %d", other.ID)} {
		if _, err := e.db.Exec(`UPDATE integration_secrets SET `+set+` WHERE integration_id = ? AND name = 'api_key'`, in.ID); err == nil || !strings.Contains(err.Error(), "never") {
			t.Errorf("UPDATE integration_secrets SET %s: %v, want the trigger's refusal", set, err)
		}
	}
	if _, err := e.db.Exec(`UPDATE integrations SET display_name = 'Renamed', updated_at = updated_at WHERE id = ?`, in.ID); err != nil {
		t.Errorf("a display name update was refused: %v", err)
	}
}

func TestSealedSecretsBindWorkspaceRowAndName(t *testing.T) {
	e := newEnv(t, "")
	owner, ws := e.setup()
	in := e.create(owner, ws, stripeBody("main"))
	var publicID string
	var sealed []byte
	if err := e.db.QueryRow(`SELECT public_id FROM integrations WHERE id = ?`, in.ID).Scan(&publicID); err != nil {
		t.Fatal(err)
	}
	if err := e.db.QueryRow(`SELECT sealed FROM integration_secrets WHERE integration_id = ? AND name = 'api_key'`, in.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	own := sealRef{workspace: ws, id: in.ID, publicID: publicID}
	if plain, err := e.key.Open(sealed, own.ad(SecretAPIKey)); err != nil || string(plain) != stripeTestKey {
		t.Fatalf("the sealed key doesn't open under its own data: %v", err)
	}
	for name, ref := range map[string]sealRef{
		"another row":       {workspace: ws, id: in.ID + 1, publicID: publicID},
		"another workspace": {workspace: ws + 1, id: in.ID, publicID: publicID},
		"another public id": {workspace: ws, id: in.ID, publicID: strings.Repeat("0", 32)},
	} {
		if _, err := e.key.Open(sealed, ref.ad(SecretAPIKey)); err == nil {
			t.Errorf("a sealed key opened under %s", name)
		}
	}
	for _, name := range []string{SecretWebhookSecret, SecretWebhookSecretPrevious, ""} {
		if _, err := e.key.Open(sealed, own.ad(name)); err == nil {
			t.Errorf("a sealed key opened under the name %q", name)
		}
	}

	if _, err := e.db.Exec(`UPDATE integration_secrets SET sealed = ? WHERE integration_id = ? AND name = 'webhook_secret'`, sealed, in.ID); err != nil {
		t.Fatal(err)
	}
	var bad unreadableError
	if _, err := e.store.credentials(context.Background(), ws, in.ID); !errors.As(err, &bad) || bad.secret != SecretWebhookSecret {
		t.Fatalf("a key copied over the webhook secret opened: %v", err)
	}

	_, ws2 := e.otherWorkspace(owner)
	other := e.create(owner, ws2, withHook(stripeBody("main"), hookFor("elsewhere")))
	if _, err := e.db.Exec(`UPDATE integration_secrets SET sealed = ? WHERE integration_id = ? AND name = 'api_key'`, sealed, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.credentials(context.Background(), ws2, other.ID); !errors.As(err, &bad) || bad.secret != SecretAPIKey {
		t.Fatalf("a key copied to another workspace's integration opened: %v", err)
	}
}

func TestIntegrationIDsAreNeverReused(t *testing.T) {
	e := newEnv(t, "")
	owner, ws := e.setup()
	first := e.create(owner, ws, stripeBody("main"))
	expect(t, owner.do(http.MethodDelete, wsPath(ws, fmt.Sprintf("integrations/%d", first.ID)), nil), http.StatusNoContent)
	second := e.create(owner, ws, stripeBody("main"))
	if second.ID <= first.ID {
		t.Fatalf("the id of a removed integration was reused: %d after %d", second.ID, first.ID)
	}
}

func TestSpacesAreScopedToTheCaller(t *testing.T) {
	e := newEnv(t, "")
	owner, ws := e.setup()
	other := e.invite(owner, ws, "admin2@example.com", "admin")
	in := e.create(owner, ws, stripeBody("main"))
	shared := e.sharedSpace(owner, ws, "Shop")
	mine := e.privateSpace(owner, ws)
	theirs := e.privateSpace(other, ws)
	one := wsPath(ws, fmt.Sprintf("integrations/%d", in.ID))
	ctx := context.Background()
	sorted := func(ids []int64) []int64 {
		out := slices.Clone(ids)
		slices.Sort(out)
		return out
	}

	got := decode[Integration](t, other.do(http.MethodPut, one+"/spaces", map[string]any{"spaces": []int64{shared, theirs}}))
	if !slices.Equal(sorted(got.Spaces), sorted([]int64{shared, theirs})) || got.OtherSpaces != 0 {
		t.Fatalf("the other admin's view = %+v", got)
	}

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"get": owner.do(http.MethodGet, one, nil), "put": owner.do(http.MethodPut, one+"/spaces", map[string]any{"spaces": []int64{shared}}),
	} {
		expect(t, rec, http.StatusOK)
		if got := decode[Integration](t, rec); !slices.Equal(got.Spaces, []int64{shared}) || got.OtherSpaces != 1 {
			t.Fatalf("%s: the first admin's view = %+v", name, got)
		}
	}
	list := decode[struct {
		Integrations []Integration `json:"integrations"`
	}](t, owner.do(http.MethodGet, wsPath(ws, "integrations"), nil)).Integrations
	if len(list) != 1 || !slices.Equal(list[0].Spaces, []int64{shared}) || list[0].OtherSpaces != 1 {
		t.Fatalf("the first admin's list = %+v", list)
	}
	for _, rec := range []*httptest.ResponseRecorder{owner.do(http.MethodGet, one, nil), owner.do(http.MethodGet, wsPath(ws, "integrations"), nil)} {
		var raw any
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		if mentions(raw, float64(theirs)) {
			t.Fatalf("the other admin's private space id %d is in %s", theirs, rec.Body.String())
		}
	}

	expectCode(t, owner.do(http.MethodPut, one+"/spaces", map[string]any{"spaces": []int64{shared, theirs}}), http.StatusNotFound, "space_not_found")

	got = decode[Integration](t, owner.do(http.MethodPut, one+"/spaces", map[string]any{"spaces": []int64{mine}}))
	if !slices.Equal(got.Spaces, []int64{mine}) || got.OtherSpaces != 1 {
		t.Fatalf("after replacing the first admin's spaces = %+v", got)
	}
	if ok, err := e.store.usable(ctx, ws, in.ID, theirs); err != nil || !ok {
		t.Fatalf("the other admin's private space was removed by someone else: %v, %v", ok, err)
	}
	got = decode[Integration](t, owner.do(http.MethodPut, one+"/spaces", map[string]any{"spaces": []int64{}}))
	if len(got.Spaces) != 0 || got.OtherSpaces != 1 {
		t.Fatalf("after emptying the first admin's spaces = %+v", got)
	}
	if ok, err := e.store.usable(ctx, ws, in.ID, theirs); err != nil || !ok {
		t.Fatalf("emptying the list removed another admin's private space: %v, %v", ok, err)
	}
	if ok, err := e.store.usable(ctx, ws, in.ID, shared); err != nil || ok {
		t.Fatalf("a shared space the admin took out is still allowed: %v, %v", ok, err)
	}
	got = decode[Integration](t, other.do(http.MethodGet, one, nil))
	if !slices.Equal(got.Spaces, []int64{theirs}) || got.OtherSpaces != 0 {
		t.Fatalf("the other admin's view after = %+v", got)
	}
	expect(t, other.do(http.MethodPut, one+"/spaces", map[string]any{"spaces": []int64{}}), http.StatusOK)
	if ok, err := e.store.usable(ctx, ws, in.ID, theirs); err != nil || ok {
		t.Fatalf("the other admin couldn't remove their own space: %v, %v", ok, err)
	}
	var added, removed float64
	for _, ev := range e.events(ws, audit.IntegrationSpacesChanged) {
		added += ev.Details["added"].(float64)
		removed += ev.Details["removed"].(float64)
	}
	if added != 3 || removed != 3 {
		t.Fatalf("audit added %v, removed %v; want 3 and 3", added, removed)
	}
}

func mentions(v any, n float64) bool {
	switch v := v.(type) {
	case float64:
		return v == n
	case []any:
		for _, x := range v {
			if mentions(x, n) {
				return true
			}
		}
	case map[string]any:
		for _, x := range v {
			if mentions(x, n) {
				return true
			}
		}
	}
	return false
}

func TestWebhookAddressFollowsThePublicURL(t *testing.T) {
	e := newEnv(t, "https://old.example.com")
	owner, ws := e.setup()
	in := e.create(owner, ws, stripeBody("main"))
	if in.WebhookURL == nil || !strings.HasPrefix(*in.WebhookURL, "https://old.example.com"+HookPrefix) {
		t.Fatalf("webhook url = %v", in.WebhookURL)
	}
	var stored int
	if err := e.db.QueryRow(`SELECT count(*) FROM pragma_table_info('integrations') WHERE name = 'webhook_url'`).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("the address is stored: %d, %v", stored, err)
	}
	mux := http.NewServeMux()
	a := auth.New(e.db, auth.Options{HashParams: auth.Params{Memory: 64, Time: 1, Threads: 1}, SetupToken: setupToken})
	a.Routes(mux)
	spaces := access.New(e.db, a)
	New(e.store, Options{PublicURL: "https://new.example.com"}).Routes(mux, a, spaces)
	e.h = auth.CSRF(mux)
	moved := decode[Integration](t, owner.do(http.MethodGet, wsPath(ws, fmt.Sprintf("integrations/%d", in.ID)), nil))
	if moved.WebhookURL == nil || *moved.WebhookURL != "https://new.example.com"+HookPrefix+strings.TrimPrefix(*in.WebhookURL, "https://old.example.com"+HookPrefix) {
		t.Fatalf("webhook url after the change = %v", moved.WebhookURL)
	}
	if in.WebhookURLChanged || !moved.WebhookURLChanged {
		t.Fatalf("webhook_url_changed before %v, after %v", in.WebhookURLChanged, moved.WebhookURLChanged)
	}
}

func TestRegistryServesCurrencyExponents(t *testing.T) {
	e := newEnv(t, "")
	owner, ws := e.setup()
	reg := decode[Registry](t, owner.do(http.MethodGet, wsPath(ws, "integration-providers"), nil))
	if len(reg.Exponents) != len(reg.Currencies) {
		t.Fatalf("exponents = %v for currencies %v", reg.Exponents, reg.Currencies)
	}
	for _, c := range reg.Currencies {
		want, _ := Exponent(c)
		if got, ok := reg.Exponents[c]; !ok || got != want {
			t.Errorf("%s: exponent %d, %v; want %d", c, got, ok, want)
		}
	}
	if reg.Exponents["JPY"] != 0 || reg.Exponents["EUR"] != 2 || reg.Exponents["COP"] != 2 {
		t.Errorf("exponents = %v", reg.Exponents)
	}
}

func TestAllowedSpaceCannotMoveToAnotherWorkspace(t *testing.T) {
	e := newEnv(t, "")
	owner, ws := e.setup()
	in := e.create(owner, ws, stripeBody("main"))
	shared := e.sharedSpace(owner, ws, "Shop")
	expect(t, owner.do(http.MethodPut, wsPath(ws, fmt.Sprintf("integrations/%d/spaces", in.ID)), map[string]any{"spaces": []int64{shared}}), http.StatusOK)
	_, ws2 := e.otherWorkspace(owner)
	foreign := e.sharedSpace(owner, ws2, "Elsewhere")
	if _, err := e.db.Exec(`UPDATE integration_spaces SET space_id = ? WHERE integration_id = ?`, foreign, in.ID); err == nil {
		t.Fatal("an allowed space was moved to another workspace's space")
	}
	in2 := e.create(owner, ws2, withHook(stripeBody("main"), stripeHookNew))
	if _, err := e.db.Exec(`UPDATE integration_spaces SET integration_id = ? WHERE integration_id = ?`, in2.ID, in.ID); err == nil {
		t.Fatal("an allowed space was moved to another workspace's integration")
	}
	if ok, _ := e.store.usable(context.Background(), ws, in.ID, shared); !ok {
		t.Fatal("a refused update changed the allowed space")
	}
}

func TestNonPositiveIDsAreNotFound(t *testing.T) {
	e := newEnv(t, "")
	owner, ws := e.setup()
	in := e.create(owner, ws, stripeBody("main"))
	for _, id := range []string{"0", "-1"} {
		one := wsPath(ws, "integrations/"+id)
		expectCode(t, owner.do(http.MethodGet, one, nil), http.StatusNotFound, "integration_not_found")
		expectCode(t, owner.do(http.MethodPatch, one, map[string]any{"display_name": "x", "secrets": map[string]string{"api_key": stripeOtherKey}}), http.StatusNotFound, "integration_not_found")
		expectCode(t, owner.do(http.MethodDelete, one, nil), http.StatusNotFound, "integration_not_found")
		expectCode(t, owner.do(http.MethodPut, one+"/spaces", map[string]any{"spaces": []int64{}}), http.StatusNotFound, "integration_not_found")
		expectCode(t, owner.do(http.MethodPost, one+"/check", nil), http.StatusNotFound, "integration_not_found")
	}
	if _, err := e.store.credentials(context.Background(), ws, 0); !errors.Is(err, errNotFound) {
		t.Fatalf("credentials of id 0: %v", err)
	}
	got := decode[Integration](t, owner.do(http.MethodGet, wsPath(ws, fmt.Sprintf("integrations/%d", in.ID)), nil))
	if got.DisplayName != "Stripe main" {
		t.Fatalf("a refused request changed the integration: %+v", got)
	}
}

func TestWebhookSecretRetryKeepsThePrevious(t *testing.T) {
	e := newEnv(t, "")
	owner, ws := e.setup()
	in := e.create(owner, ws, stripeBody("main"))
	one := wsPath(ws, fmt.Sprintf("integrations/%d", in.ID))
	rotate := map[string]any{"secrets": map[string]string{"webhook_secret": stripeHookNew}}
	expect(t, owner.do(http.MethodPatch, one, rotate), http.StatusOK)
	expect(t, owner.do(http.MethodPatch, one, rotate), http.StatusOK)
	creds, err := e.store.credentials(context.Background(), ws, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if creds.WebhookSecret.Reveal() != stripeHookNew || creds.WebhookSecretPrevious.Reveal() != stripeHook {
		t.Fatalf("after a retried rotation the current is %q and the previous %q", creds.WebhookSecret, creds.WebhookSecretPrevious)
	}
	expect(t, owner.do(http.MethodPatch, one, map[string]any{"secrets": map[string]string{"webhook_secret": stripeHook}}), http.StatusOK)
	creds, _ = e.store.credentials(context.Background(), ws, in.ID)
	if creds.WebhookSecret.Reveal() != stripeHook || creds.WebhookSecretPrevious.Reveal() != stripeHookNew {
		t.Fatal("a new secret didn't rotate")
	}
}

func TestBodyMustBeAnObject(t *testing.T) {
	e := newEnv(t, "")
	owner, ws := e.setup()
	in := e.create(owner, ws, stripeBody("main"))
	one := wsPath(ws, fmt.Sprintf("integrations/%d", in.ID))
	for _, body := range []string{"null", "[]", `"x"`, "1", "true"} {
		for _, r := range []struct{ method, path string }{
			{http.MethodPatch, one}, {http.MethodPost, wsPath(ws, "integrations")}, {http.MethodPut, one + "/spaces"},
		} {
			expectCodeNonFatal(t, owner.raw(r.method, r.path, body), http.StatusBadRequest, "invalid_json", r.method+" "+r.path+" with "+body)
		}
	}
	expect(t, owner.do(http.MethodPatch, one, map[string]any{}), http.StatusOK)
}

func TestWritesOfAnAdminDisabledMeanwhile(t *testing.T) {
	e := newEnv(t, "")
	owner, ws := e.setup()
	admin := e.invite(owner, ws, "admin2@example.com", "admin")
	in := e.create(owner, ws, stripeBody("main"))
	if _, err := e.db.Exec(`UPDATE users SET disabled_at = '2026-01-01T00:00:00.000Z' WHERE id = ?`, admin.id); err != nil {
		t.Fatal(err)
	}
	ctx := audit.WithActor(context.Background(), admin.id)
	if err := e.store.remove(ctx, ws, in.ID); !errors.Is(err, auth.ErrAccountDisabled) {
		t.Fatalf("remove by a disabled admin = %v", err)
	}
	if _, err := e.store.create(ctx, ws, admin.id, mustDraft(t, stripeBody("second"))); !errors.Is(err, auth.ErrAccountDisabled) {
		t.Fatalf("create by a disabled admin = %v", err)
	}
	if _, err := e.store.getSnapshot(context.Background(), ws, in.ID); err != nil {
		t.Fatalf("the integration is gone: %v", err)
	}
	if got := e.events(ws, audit.IntegrationRemoved); len(got) != 0 {
		t.Fatalf("recorded %+v", got)
	}
	rec := httptest.NewRecorder()
	fail(rec, httptest.NewRequest(http.MethodDelete, "/", nil), auth.ErrAccountDisabled)
	expectCode(t, rec, http.StatusForbidden, "account_disabled")
}

func TestCheckOfARemovedIntegrationIsNotFound(t *testing.T) {
	e := newEnv(t, "")
	owner, ws := e.setup()
	in := e.create(owner, ws, stripeBody("main"))
	one := wsPath(ws, fmt.Sprintf("integrations/%d", in.ID))
	expect(t, owner.do(http.MethodDelete, one, nil), http.StatusNoContent)
	expectCode(t, owner.do(http.MethodPost, one+"/check", nil), http.StatusNotFound, "integration_not_found")
}

func TestAnAdminWhoKeepsAnApprovedSpaceMakesItTheirs(t *testing.T) {
	e := newEnv(t, "")
	owner, ws := e.setup()
	member := e.invite(owner, ws, "member@example.com", "member")
	in := e.create(owner, ws, stripeBody("main"))
	shared := e.sharedSpace(owner, ws, "Shop")
	theirs := e.privateSpace(member, ws)
	for _, sp := range []int64{shared, theirs} {
		if _, err := e.db.Exec(`INSERT INTO integration_spaces (integration_id, space_id, from_approval) VALUES (?, ?, 1)`, in.ID, sp); err != nil {
			t.Fatal(err)
		}
	}
	owned := func() []int64 {
		rows, err := e.db.Query(`SELECT space_id FROM integration_spaces WHERE integration_id = ? AND from_approval = 1 ORDER BY space_id`, in.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			out = append(out, id)
		}
		return out
	}
	spaces := wsPath(ws, fmt.Sprintf("integrations/%d/spaces", in.ID))

	rec := owner.do(http.MethodPut, spaces, map[string]any{"spaces": []int64{shared}})
	expect(t, rec, http.StatusOK)
	if got := decode[Integration](t, rec); !slices.Equal(got.ApprovalSpaces, []int64{shared}) || got.OtherSpaces != 1 {
		t.Fatalf("the integration as the admin sees it: %+v", got)
	}
	if got := owned(); len(got) != 2 {
		t.Fatalf("a save adopted a grant: still an approval's = %v", got)
	}
	if got := e.events(ws, audit.IntegrationSpacesKept); len(got) != 0 {
		t.Fatalf("adoption events after a plain save: %+v", got)
	}

	expectCode(t, owner.do(http.MethodPut, spaces, map[string]any{"spaces": []int64{}, "keep": []int64{shared}}), http.StatusBadRequest, "bad_request")
	expectCode(t, owner.do(http.MethodPut, spaces, map[string]any{"spaces": []int64{shared}, "keep": []int64{theirs}}), http.StatusBadRequest, "bad_request")
	expect(t, owner.do(http.MethodPut, spaces, map[string]any{"spaces": []int64{shared}, "keep": []int64{shared}}), http.StatusOK)
	if got := owned(); len(got) != 1 || got[0] != theirs {
		t.Fatalf("spaces still an approval's = %v, want only %d", got, theirs)
	}
	kept := e.events(ws, audit.IntegrationSpacesKept)
	if len(kept) != 1 || kept[0].Details["kept"] != float64(1) || kept[0].Details["provider"] != "stripe" || *kept[0].TargetID != in.ID {
		t.Fatalf("adoption events = %+v", kept)
	}
	if got := e.events(ws, audit.IntegrationSpacesChanged); len(got) != 0 {
		t.Fatalf("an adoption recorded as a change of the list: %+v", got)
	}
	expect(t, owner.do(http.MethodPut, spaces, map[string]any{"spaces": []int64{shared}, "keep": []int64{shared}}), http.StatusOK)
	if got := e.events(ws, audit.IntegrationSpacesKept); len(got) != 1 {
		t.Fatalf("adoption events after naming a kept space again: %+v", got)
	}
}
