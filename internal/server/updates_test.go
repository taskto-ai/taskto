package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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
		updateStatus(version, empty).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/updates", nil))
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
	updateStatus("v0.9.0", "").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/updates", nil))
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
