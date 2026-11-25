package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/updatestatus"
)

func writeUpdateStatus(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(updatestatus.Path(dir), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The updater's status is for the server's operators only: a member, a stranger and an admin
// of a workspace they made themselves get nothing of it, and the server only reads: it makes
// no file in the updater's directory.
func TestUpdateStatusIsForAdmins(t *testing.T) {
	dir := t.TempDir()
	s := newStackWith(t, func(o *Options) {
		o.UpdateStatusDir = dir
		o.Auth.WorkspaceCreation = "anyone"
	})
	path := func(ws int64) string { return fmt.Sprintf("/api/workspaces/%d/updates", ws) }

	// The updater hasn't written anything yet.
	code, body := s.browser(s.alice.cookie, http.MethodGet, path(s.one), nil)
	if code != http.StatusOK || body["configured"] != true || body["status"] != nil || body["running_version"] == "" {
		t.Fatalf("before any run: %d %v", code, body)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	writeUpdateStatus(t, dir, `{"version":1,"checked":"`+now+`","channel":"release","recorded_version":"v0.3.0",
		"last_run":{"at":"`+now+`","result":"blocked","from":"v0.3.0","to":"v0.4.0","code":"stopped","reason":"restore /var/backups/taskto/x.tar.gz","backup":"/var/backups/taskto/x.tar.gz"},
		"last_change":null,
		"needs_person":{"kind":"blocked","code":"stopped"}}`)
	code, body = s.browser(s.alice.cookie, http.MethodGet, path(s.one), nil)
	st, _ := body["status"].(map[string]any)
	need, _ := st["needs_person"].(map[string]any)
	run, _ := st["last_run"].(map[string]any)
	if code != http.StatusOK || body["stale"] != nil || st["recorded_version"] != "v0.3.0" || need["kind"] != "blocked" || need["code"] != "stopped" ||
		run["result"] != "blocked" || run["to"] != "v0.4.0" || run["code"] != "stopped" {
		t.Fatalf("operator: %d %v", code, body)
	}
	// Only the structure is sent on: not the paths a file may carry.
	if raw := fmt.Sprint(body); strings.Contains(raw, "/var/backups") || strings.Contains(raw, "restore ") {
		t.Fatalf("a text of the file reached the page: %v", body)
	}
	if code, _ := s.browser(s.alice.cookie, http.MethodGet, path(s.two), nil); code != http.StatusOK {
		t.Fatalf("operator, in another workspace: %d", code)
	}

	// An admin of a workspace someone made later runs no server: they don't see it.
	bobs := s.createWorkspace(s.bob.cookie, "Bob's")
	if code, body := s.browser(s.bob.cookie, http.MethodGet, path(bobs), nil); code != http.StatusForbidden || body["code"] != "operator_only" ||
		body["status"] != nil || body["running_version"] != nil {
		t.Fatalf("an admin of a workspace they made: %d %v", code, body)
	}

	// bob is a member of one, not an admin; he isn't in two at all.
	if code, body := s.browser(s.bob.cookie, http.MethodGet, path(s.one), nil); code != http.StatusForbidden || body["status"] != nil {
		t.Fatalf("member: %d %v", code, body)
	}
	if code, body := s.browser(s.bob.cookie, http.MethodGet, path(s.two), nil); code != http.StatusNotFound || body["status"] != nil {
		t.Fatalf("outsider: %d %v", code, body)
	}
	if code, _ := s.browser("", http.MethodGet, path(s.one), nil); code != http.StatusUnauthorized {
		t.Fatalf("no login: %d", code)
	}
	// An admin's access token doesn't reach it: it's for the browser.
	token, _ := s.newToken(s.alice.cookie, s.one, "monitor")
	if code, body := s.api(token, http.MethodGet, path(s.one), nil); code == http.StatusOK || body["status"] != nil {
		t.Fatalf("access token: %d %v", code, body)
	}
	// Only a GET: the server can't be made to run or change anything through it.
	if code, _ := s.browser(s.alice.cookie, http.MethodPost, path(s.one), map[string]any{}); code == http.StatusOK {
		t.Fatalf("a POST: %d", code)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("the updater's directory has %v (%v), want its one file", entries, err)
	}
}

func TestUpdateStatusStaleUnreadableAndNotConfigured(t *testing.T) {
	dir := t.TempDir()
	s := newStackWith(t, func(o *Options) { o.UpdateStatusDir = dir })
	path := fmt.Sprintf("/api/workspaces/%d/updates", s.one)

	writeUpdateStatus(t, dir, `{"version":1,"checked":"2020-01-01T00:00:00Z","channel":"release"}`)
	if code, body := s.browser(s.alice.cookie, http.MethodGet, path, nil); code != http.StatusOK || body["stale"] != true {
		t.Fatalf("stale: %d %v", code, body)
	}
	// A file that isn't a status is an error that tells nothing of it.
	writeUpdateStatus(t, dir, "{ secret-looking")
	code, body := s.browser(s.alice.cookie, http.MethodGet, path, nil)
	if code != http.StatusInternalServerError || strings.Contains(fmt.Sprint(body), "secret-looking") {
		t.Fatalf("unreadable: %d %v", code, body)
	}

	// A release whose updater wrote nothing says the status is expected; a development build doesn't.
	empty := t.TempDir()
	for version, want := range map[string]bool{"v0.9.0": true, "0123456789abcdef0123456789abcdef01234567": true, "dev": false, "test": false} {
		rec := httptest.NewRecorder()
		updateStatus(version, empty, "").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/updates", nil))
		var reply map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		if got := reply["expects_status"] == true; got != want || reply["status"] != nil {
			t.Errorf("version %q with no status: expects_status = %v, want %v (%v)", version, got, want, reply)
		}
	}
	// A directory that isn't set up doesn't expect one.
	rec := httptest.NewRecorder()
	updateStatus("v0.9.0", "", "").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/updates", nil))
	if strings.Contains(rec.Body.String(), "expects_status") {
		t.Errorf("not configured but expected: %s", rec.Body)
	}
	// No directory set: not configured, still an answer.
	none := newStack(t)
	code, body = none.browser(none.alice.cookie, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/updates", none.one), nil)
	if code != http.StatusOK || body["configured"] != false || body["status"] != nil {
		t.Fatalf("not configured: %d %v", code, body)
	}
}

// requestStatus is a status of an updater that can take a request.
func requestStatus(channel, extra string) string {
	now := time.Now().UTC().Format(time.RFC3339)
	return `{"version":1,"checked":"` + now + `","channel":"` + channel + `","recorded_version":"v0.3.0","last_run":null,"last_change":null,
		"needs_person":null,"requests":{"supported":true,"last":null}` + extra + `}`
}

func (s *stack) auditCount(action string) int64 {
	return s.scalar(`SELECT count(*) FROM audit_events WHERE action = ?`, action)
}

// "Update now" is the server's operators' alone, a browser session's, and a refused request
// writes no file and records no event; an accepted one writes one small file of a fixed shape,
// atomically, and one event with the channel and nothing else.
func TestUpdateRequestIsForOperatorsAndIsAudited(t *testing.T) {
	statusDir, reqDir := t.TempDir(), t.TempDir()
	s := newStackWith(t, func(o *Options) {
		o.UpdateStatusDir, o.UpdateRequestDir = statusDir, reqDir
		o.Auth.WorkspaceCreation = "anyone"
	})
	writeUpdateStatus(t, statusDir, requestStatus("release", ""))
	path := func(ws int64) string { return fmt.Sprintf("/api/workspaces/%d/updates/request", ws) }
	noRequest := func(what string) {
		t.Helper()
		if entries, _ := os.ReadDir(reqDir); len(entries) != 0 {
			t.Fatalf("%s: the request directory has %v", what, entries)
		}
		if n := s.auditCount("server_update_requested"); n != 0 {
			t.Fatalf("%s: %d events recorded", what, n)
		}
	}

	// A member, an outsider, an admin of a workspace they made themselves, no login, an access token.
	if code, body := s.browser(s.bob.cookie, http.MethodPost, path(s.one), map[string]any{}); code != http.StatusForbidden {
		t.Fatalf("member: %d %v", code, body)
	}
	if code, _ := s.browser(s.bob.cookie, http.MethodPost, path(s.two), map[string]any{}); code != http.StatusNotFound {
		t.Fatalf("outsider: %d", code)
	}
	bobs := s.createWorkspace(s.bob.cookie, "Bob's")
	if code, body := s.browser(s.bob.cookie, http.MethodPost, path(bobs), map[string]any{}); code != http.StatusForbidden || body["code"] != "operator_only" {
		t.Fatalf("an admin of a workspace they made: %d %v", code, body)
	}
	if code, _ := s.browser("", http.MethodPost, path(s.one), map[string]any{}); code != http.StatusUnauthorized {
		t.Fatalf("no login: %d", code)
	}
	token, _ := s.newToken(s.alice.cookie, s.one, "monitor")
	if code, _ := s.api(token, http.MethodPost, path(s.one), map[string]any{}); code == http.StatusAccepted {
		t.Fatalf("an access token pressed it: %d", code)
	}
	noRequest("refused requests")

	// The operator may; the request is a file of a fixed shape and nothing else is left.
	before := time.Now().UTC().Add(-time.Second)
	code, body := s.browser(s.alice.cookie, http.MethodPost, path(s.one), map[string]any{})
	if code != http.StatusAccepted {
		t.Fatalf("operator: %d %v", code, body)
	}
	at, err := time.Parse(time.RFC3339, fmt.Sprint(body["requested_at"]))
	if err != nil || at.Before(before) {
		t.Fatalf("requested_at = %v (%v)", body["requested_at"], err)
	}
	entries, err := os.ReadDir(reqDir)
	if err != nil || len(entries) != 1 || entries[0].Name() != updatestatus.RequestFile {
		t.Fatalf("the request directory has %v (%v), want %s alone", entries, err, updatestatus.RequestFile)
	}
	raw, err := os.ReadFile(filepath.Join(reqDir, updatestatus.RequestFile))
	if err != nil || !regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ [0-9a-f]{32}\n$`).Match(raw) {
		t.Fatalf("the request is %q (%v)", raw, err)
	}
	if n := s.auditCount("server_update_requested"); n != 1 {
		t.Fatalf("%d events, want 1", n)
	}
	var details string
	if err := s.db.QueryRow(`SELECT details FROM audit_events WHERE action = 'server_update_requested' AND actor_id = ? AND workspace_id = ? AND target_type = 'workspace' AND target_id = ?`,
		s.alice.id, s.one, s.one).Scan(&details); err != nil || details != `{"channel":"release"}` {
		t.Fatalf("event: %q (%v)", details, err)
	}

	// The page sees it pending, and a second press is refused with no second event.
	code, body = s.browser(s.alice.cookie, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/updates", s.one), nil)
	req, _ := body["request"].(map[string]any)
	if code != http.StatusOK || req["available"] != false || req["reason"] != "pending" || req["pending_since"] == nil {
		t.Fatalf("status with a pending request: %d %v", code, body)
	}
	if code, body := s.browser(s.alice.cookie, http.MethodPost, path(s.one), map[string]any{}); code != http.StatusConflict || body["code"] != "update_request_pending" {
		t.Fatalf("second press: %d %v", code, body)
	}
	if n := s.auditCount("server_update_requested"); n != 1 {
		t.Fatalf("%d events after a refused second press, want 1", n)
	}
	// A request nobody took for hours doesn't block the button for good.
	old := time.Now().Add(-updatestatus.ExpireAfter - time.Minute)
	if err := os.Chtimes(filepath.Join(reqDir, updatestatus.RequestFile), old, old); err != nil {
		t.Fatal(err)
	}
	if code, body := s.browser(s.alice.cookie, http.MethodPost, path(s.one), map[string]any{}); code != http.StatusAccepted {
		t.Fatalf("after the old request expired: %d %v", code, body)
	}
}

// What the host says decides whether the button works: a run in progress, the channel off, an
// updater that can't take requests, and a server without the directory are each refused with their
// own code, with nothing written and nothing recorded.
func TestUpdateRequestRefusals(t *testing.T) {
	statusDir, reqDir := t.TempDir(), t.TempDir()
	s := newStackWith(t, func(o *Options) { o.UpdateStatusDir, o.UpdateRequestDir = statusDir, reqDir })
	path := fmt.Sprintf("/api/workspaces/%d/updates/request", s.one)
	now := time.Now().UTC().Format(time.RFC3339)
	for name, tc := range map[string]struct{ status, code string }{
		"no status yet":            {"", "update_request_unsupported"},
		"an updater before it":     {`{"version":1,"checked":"` + now + `","channel":"release"}`, "update_request_unsupported"},
		"the host can't take one":  {`{"version":1,"checked":"` + now + `","channel":"release","requests":{"supported":false,"last":null}}`, "update_request_unsupported"},
		"the channel is off":       {requestStatus("off", ""), "update_channel_off"},
		"a run is in progress":     {requestStatus("release", `,"running":{"since":"`+now+`","requested":true}`), "update_running"},
		"a status that went stale": {strings.Replace(requestStatus("release", ""), now, "2020-01-01T00:00:00Z", 1), "update_request_unsupported"},
	} {
		os.Remove(updatestatus.Path(statusDir))
		if tc.status != "" {
			writeUpdateStatus(t, statusDir, tc.status)
		}
		code, body := s.browser(s.alice.cookie, http.MethodPost, path, map[string]any{})
		if code != http.StatusConflict || body["code"] != tc.code {
			t.Errorf("%s: %d %v, want 409 %s", name, code, body, tc.code)
		}
	}
	// A run that was killed leaves its note: after a few hours it isn't believed.
	long := time.Now().Add(-updatestatus.RunningMaxAge - time.Hour).UTC().Format(time.RFC3339)
	writeUpdateStatus(t, statusDir, requestStatus("release", `,"running":{"since":"`+long+`"}`))
	if code, body := s.browser(s.alice.cookie, http.MethodPost, path, map[string]any{}); code != http.StatusAccepted {
		t.Errorf("an old running note: %d %v", code, body)
	}
	if entries, _ := os.ReadDir(reqDir); len(entries) != 1 {
		t.Errorf("request directory: %v", entries)
	}
	if n := s.auditCount("server_update_requested"); n != 1 {
		t.Errorf("%d events, want the one accepted", n)
	}

	// No directory configured, one that isn't there, and one the server can't write.
	for name, dir := range map[string]string{"not configured": "", "missing": filepath.Join(t.TempDir(), "gone")} {
		s := newStackWith(t, func(o *Options) { o.UpdateStatusDir, o.UpdateRequestDir = statusDir, dir })
		writeUpdateStatus(t, statusDir, requestStatus("release", ""))
		code, body := s.browser(s.alice.cookie, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/updates/request", s.one), map[string]any{})
		if code != http.StatusConflict || body["code"] != "update_request_unsupported" {
			t.Errorf("%s: %d %v", name, code, body)
		}
		if n := s.auditCount("server_update_requested"); n != 0 {
			t.Errorf("%s: %d events recorded", name, n)
		}
		_, body = s.browser(s.alice.cookie, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/updates", s.one), nil)
		if req, _ := body["request"].(map[string]any); req["available"] != false {
			t.Errorf("%s: the status says it is available: %v", name, body)
		}
	}
}
