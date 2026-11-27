package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/db"
)

const setupToken = "fake-setup-token-for-the-mcp-test-0000000"

func do(t *testing.T, h http.Handler, cookie *http.Cookie, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://example.com")
	r.Host = "example.com"
	r.RemoteAddr = "192.0.2.7:1234"
	if cookie != nil {
		r.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestAuditAnMCPTokenUseIsOneEventPerLastUsedUpdate(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := db.Migrate(ctx, d); err != nil {
		t.Fatal(err)
	}
	accounts := auth.New(d, auth.Options{SetupToken: setupToken, HashParams: auth.Params{Memory: 64, Time: 1, Threads: 1}})
	mux := http.NewServeMux()
	accounts.Routes(mux)
	h := accounts.ClientAddr(auth.CSRF(mux))

	rec := do(t, h, nil, http.MethodPost, "/api/setup", map[string]string{
		"email": "owner@example.com", "name": "Owner", "password": "fake-owner-password-1", "workspace_name": "Home", "setup_token": setupToken})
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup = %d %s", rec.Code, rec.Body)
	}
	cookie := rec.Result().Cookies()[0]
	var me struct {
		Workspaces []struct {
			WorkspaceID int64 `json:"id"`
		} `json:"workspaces"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	ws := me.Workspaces[0].WorkspaceID
	rec = do(t, h, cookie, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/tokens", ws), map[string]string{"name": "agent"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("token = %d %s", rec.Code, rec.Body)
	}
	var created struct {
		ID    int64  `json:"id"`
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	reached := 0
	handler := authenticated(accounts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached++ }))
	for range 5 {
		r := httptest.NewRequest(http.MethodPost, Path, nil)
		r.Header.Set("Authorization", "Bearer "+created.Token)
		handler.ServeHTTP(httptest.NewRecorder(), r)
	}
	if reached != 5 {
		t.Fatalf("%d of 5 MCP requests got through", reached)
	}
	rows, err := audit.List(ctx, d, audit.Filter{Workspace: ws, Action: audit.AccessTokenUsed})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("access_token_used = %d events for 5 requests in a minute, want 1: %+v", len(rows), rows)
	}
	if r := rows[0]; r.TargetID == nil || *r.TargetID != created.ID || r.ActorID == nil || r.TargetType != audit.TargetAccessToken {
		t.Fatalf("event = %+v, want the token %d used by its owner", r, created.ID)
	}
}
