package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/backup"
)

func writeBackupStatus(t *testing.T, dir string, st backup.Status) {
	t.Helper()
	p := backup.StatusPath(dir)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// /healthz/backup is for outside monitoring, with no login: 200 while the backups are on
// time, 503 once a night is missed or when none run, and never the error, which can name
// the backup host.
func TestHealthzBackup(t *testing.T) {
	dir := t.TempDir()
	h := newHandler(t, openDB(t), Options{BackupDataDir: dir})
	check := func(name string, wantCode int, wantStatus string) {
		t.Helper()
		rec := get(h, "/healthz/backup")
		body := rec.Body.String()
		if rec.Code != wantCode || !strings.Contains(body, `"status":"`+wantStatus+`"`) {
			t.Fatalf("%s: /healthz/backup = %d %s, want %d and %s", name, rec.Code, body, wantCode, wantStatus)
		}
		if strings.Contains(body, "backup.example.com") || strings.Contains(body, "error") {
			t.Fatalf("%s: /healthz/backup tells the error: %s", name, body)
		}
	}
	check("no schedule", http.StatusServiceUnavailable, "none")

	now := time.Now().UTC()
	failed := backup.Status{LastSuccess: now.Add(-20 * time.Hour), LastErrorAt: now.Add(-time.Hour), DueBy: now.Add(time.Hour),
		LastError: "the repository didn't answer: rest:https://***@backup.example.com:8000/taskto/"}
	writeBackupStatus(t, dir, backup.Status{LastSuccess: now.Add(-time.Hour), DueBy: now.Add(20 * time.Hour)})
	check("on time", http.StatusOK, "ok")
	writeBackupStatus(t, dir, failed)
	check("failing", http.StatusOK, "failing")
	failed.DueBy = now.Add(-time.Minute)
	writeBackupStatus(t, dir, failed)
	check("late", http.StatusServiceUnavailable, "late")
	if err := os.WriteFile(backup.StatusPath(dir), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	check("unreadable", http.StatusServiceUnavailable, "unknown")

	if rec := get(newHandler(t, openDB(t), Options{}), "/healthz/backup"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("without a data directory: %d", rec.Code)
	}
}

// The whole status, error included, is for the server's operators only.
func TestBackupStatusIsForAdmins(t *testing.T) {
	s := newStack(t)
	path := func(ws int64) string { return fmt.Sprintf("/api/workspaces/%d/backup", ws) }
	if code, body := s.browser(s.alice.cookie, http.MethodGet, path(s.one), nil); code != http.StatusOK || body["health"] != "none" {
		t.Fatalf("before any backup: %d %v", code, body)
	}

	now := time.Now().UTC()
	writeBackupStatus(t, s.dir, backup.Status{At: "03:15", Retries: 6, RetryEverySeconds: 3600,
		LastSuccess: now.Add(-20 * time.Hour), LastSnapshot: "0123abcd", LastErrorAt: now.Add(-time.Hour),
		LastError: "the repository didn't answer", DueBy: now.Add(time.Hour), NextAttempt: now.Add(time.Hour)})
	code, body := s.browser(s.alice.cookie, http.MethodGet, path(s.one), nil)
	if code != http.StatusOK || body["health"] != "failing" || body["last_error"] != "the repository didn't answer" ||
		body["last_snapshot"] != "0123abcd" || body["at"] != "03:15" || body["next_attempt"] == nil {
		t.Fatalf("admin: %d %v", code, body)
	}
	if code, _ := s.browser(s.alice.cookie, http.MethodGet, path(s.two), nil); code != http.StatusOK {
		t.Fatalf("admin of another workspace: %d", code)
	}

	// bob is a member of one, not an admin; he isn't in two at all.
	if code, body := s.browser(s.bob.cookie, http.MethodGet, path(s.one), nil); code != http.StatusForbidden || body["last_error"] != nil {
		t.Fatalf("member: %d %v", code, body)
	}
	if code, body := s.browser(s.bob.cookie, http.MethodGet, path(s.two), nil); code != http.StatusNotFound || body["last_error"] != nil {
		t.Fatalf("outsider: %d %v", code, body)
	}
	if code, _ := s.browser("", http.MethodGet, path(s.one), nil); code != http.StatusUnauthorized {
		t.Fatalf("no login: %d", code)
	}
	// An admin's access token doesn't reach it: it's for the browser.
	token, _ := s.newToken(s.alice.cookie, s.one, "monitor")
	if code, body := s.api(token, http.MethodGet, path(s.one), nil); code == http.StatusOK || body["last_error"] != nil {
		t.Fatalf("access token: %d %v", code, body)
	}
}
