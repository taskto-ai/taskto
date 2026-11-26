//go:build unix

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"taskto.ai/taskto/internal/config"
	"taskto.ai/taskto/internal/db"
	"taskto.ai/taskto/internal/serverkey"
)

// logBuffer collects the server's log lines, safe for concurrent use.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog sends slog's output to a buffer for the rest of the test.
func captureLog(t *testing.T) *logBuffer {
	t.Helper()
	logs := &logBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs
}

var setupTokenLine = regexp.MustCompile(`setup_token=([A-Za-z0-9_-]{43})`)

func postSetup(t *testing.T, addr, token string) int {
	t.Helper()
	body := `{"email":"owner@example.com","name":"Owner","password":"fake-owner-password","workspace_name":"Home"`
	if token != "" {
		body += `,"setup_token":"` + token + `"`
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+addr+"/api/setup", strings.NewReader(body+"}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// A fresh server prints a setup token, and setup needs it; a set-up server prints none.
func TestFirstRunSetupToken(t *testing.T) {
	logs := captureLog(t)
	cfg := config.Default()
	cfg.Addr = "127.0.0.1:0"
	cfg.DataDir = t.TempDir()

	addr, stop := startRun(t, cfg)
	match := setupTokenLine.FindStringSubmatch(logs.String())
	if match == nil {
		t.Fatalf("a fresh server logged no setup token:\n%s", logs)
	}
	if code := postSetup(t, addr, ""); code != http.StatusForbidden {
		t.Fatalf("setup without the token = %d, want 403", code)
	}
	if code := postSetup(t, addr, match[1]); code != http.StatusCreated {
		t.Fatalf("setup with the token = %d, want 201", code)
	}
	if code := postSetup(t, addr, match[1]); code != http.StatusConflict {
		t.Fatalf("setup again with the token = %d, want 409", code)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	before := len(logs.String())
	_, stop = startRun(t, cfg)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if after := logs.String()[before:]; strings.Contains(after, "setup_token") {
		t.Fatalf("a set-up server logged a setup token:\n%s", after)
	}
}

// taskto backup and taskto restore, as an operator runs them: back up a server's data
// directory while it runs, restore it elsewhere, and start a server on the copy.
func TestBackupAndRestoreCommands(t *testing.T) {
	ctx := context.Background()
	serverLogs := captureLog(t)
	cfg := config.Default()
	cfg.Addr = "127.0.0.1:0"
	cfg.DataDir = t.TempDir()
	addr, stop := startRun(t, cfg)
	// The server key the server made on its first start: without it, the provider keys in
	// the database can't be read.
	serverKey, err := os.ReadFile(filepath.Join(cfg.DataDir, serverkey.FileName))
	if err != nil {
		t.Fatal(err)
	}
	token := setupTokenLine.FindStringSubmatch(serverLogs.String())
	if token == nil {
		t.Fatal("no setup token in the log")
	}
	if code := postSetup(t, addr, token[1]); code != http.StatusCreated {
		t.Fatalf("setup = %d", code)
	}

	dir := t.TempDir()
	archive := filepath.Join(dir, "taskto.tar.gz")
	env := func(k string) string { return map[string]string{"TASKTO_DATA_DIR": cfg.DataDir}[k] }
	var stderr bytes.Buffer
	if err := runBackup(ctx, []string{archive}, env, nil, &stderr); err != nil {
		t.Fatalf("taskto backup: %v\n%s", err, &stderr)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(archive)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the backup file = %v, %v; want owner-only", info, err)
	}
	if !strings.Contains(stderr.String(), "taskto.db") {
		t.Fatalf("taskto backup said %q, want the databases named", &stderr)
	}

	// Refusals: the file exists; the file is inside the data directory.
	if err := runBackup(ctx, []string{archive}, env, nil, &stderr); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("backup over an existing file = %v, want refused", err)
	}
	if err := runBackup(ctx, []string{filepath.Join(cfg.DataDir, "b.tar.gz")}, env, nil, &stderr); err == nil || !strings.Contains(err.Error(), "inside the data directory") {
		t.Fatalf("backup into the data directory = %v, want refused", err)
	}
	if err := runBackup(ctx, []string{"-data-dir", cfg.DataDir}, env, nil, &stderr); err == nil {
		t.Fatal("backup without a FILE succeeded")
	}

	// Standard output, and restore from standard input, with -data-dir over the environment.
	var piped bytes.Buffer
	if err := runBackup(ctx, []string{"-"}, env, &piped, &stderr); err != nil {
		t.Fatalf("taskto backup -: %v", err)
	}
	restored := filepath.Join(dir, "restored")
	if err := runRestore(ctx, []string{"-data-dir", restored, "-"}, env, &piped, &stderr); err != nil {
		t.Fatalf("taskto restore -: %v\n%s", err, &stderr)
	}
	// And from the file, into a fresh directory; the occupied one is refused.
	if err := runRestore(ctx, []string{"-data-dir", restored, archive}, env, nil, &stderr); err == nil {
		t.Fatal("restore into a data directory with data succeeded")
	}
	fromFile := filepath.Join(dir, "from-file")
	if err := runRestore(ctx, []string{"-data-dir", fromFile, archive}, env, nil, &stderr); err != nil {
		t.Fatalf("taskto restore FILE: %v", err)
	}

	for _, d := range []string{restored, fromFile} {
		if key, err := os.ReadFile(filepath.Join(d, serverkey.FileName)); err != nil || !bytes.Equal(key, serverKey) {
			t.Fatalf("restored server key differs (%v)", err)
		}
		sqlDB, err := sql.Open("sqlite", filepath.Join(d, db.FileName))
		if err != nil {
			t.Fatal(err)
		}
		var n int
		err = sqlDB.QueryRow(`SELECT count(*) FROM users`).Scan(&n)
		_ = sqlDB.Close()
		if err != nil || n != 1 {
			t.Fatalf("restored users = %d, %v; want the owner", n, err)
		}
		// A server starts on the restored data, already set up: no setup token.
		logs := captureLog(t)
		restoredCfg := cfg
		restoredCfg.DataDir = d
		addr, stop := startRun(t, restoredCfg)
		if code := postSetup(t, addr, "x"); code != http.StatusConflict {
			t.Fatalf("setup on the restored server = %d, want 409", code)
		}
		if err := stop(); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(logs.String(), "setup_token") {
			t.Fatal("the restored server logged a setup token")
		}
	}
}

// apiCall makes one request as a browser would, with the session cookie it holds ("" for
// none), and returns the status and the session cookie the answer set ("" if none).
func apiCall(t *testing.T, addr, method, path, cookie string, body any) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, "http://"+addr+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		// #nosec G124 -- a request cookie, sent by a test to a plain-http server.
		req.AddCookie(&http.Cookie{Name: "taskto_session", Value: cookie})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	set := ""
	for _, c := range resp.Cookies() {
		if c.Name == "taskto_session" && c.MaxAge >= 0 {
			set = c.Value
		}
	}
	return resp.StatusCode, set
}

// A restore never brings a login back: a browser that kept its cookie after its session was
// ended (a password change signed it out, and a refused session's 401 leaves the cookie)
// is not signed in again by restoring a backup made before.
func TestRestoreDoesNotReviveEndedSessions(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Addr = "127.0.0.1:0"
	cfg.DataDir = t.TempDir()
	cfg.InsecureCookies = true // the test talks plain http
	logs := captureLog(t)
	addr, stop := startRun(t, cfg)
	token := setupTokenLine.FindStringSubmatch(logs.String())
	if token == nil {
		t.Fatal("no setup token in the log")
	}
	if code := postSetup(t, addr, token[1]); code != http.StatusCreated {
		t.Fatalf("setup = %d", code)
	}
	const oldPassword, newPassword = "fake-owner-password", "fake-owner-password-changed"
	login := func(addr string) string {
		code, c := apiCall(t, addr, http.MethodPost, "/api/login",
			"", map[string]string{"email": "owner@example.com", "password": oldPassword})
		if code != http.StatusOK || c == "" {
			t.Fatalf("login = %d, cookie %q", code, c)
		}
		return c
	}
	first, second := login(addr), login(addr)

	// The backup has both sessions active.
	dir := t.TempDir()
	archive := filepath.Join(dir, "taskto.tar.gz")
	env := func(k string) string { return map[string]string{"TASKTO_DATA_DIR": cfg.DataDir}[k] }
	var stderr bytes.Buffer
	if err := runBackup(ctx, []string{archive}, env, nil, &stderr); err != nil {
		t.Fatalf("taskto backup: %v\n%s", err, &stderr)
	}

	// The password changes in the first browser: the second one's session ends.
	if code, _ := apiCall(t, addr, http.MethodPost, "/api/me/password", first,
		map[string]string{"current_password": oldPassword, "new_password": newPassword}); code != http.StatusNoContent {
		t.Fatalf("change password = %d", code)
	}
	if code, _ := apiCall(t, addr, http.MethodGet, "/api/me", second, nil); code != http.StatusUnauthorized {
		t.Fatalf("the signed-out session got %d, want 401", code)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	// Restore the older backup, as an operator does, and start a server on it.
	restored := filepath.Join(dir, "restored")
	if err := runRestore(ctx, []string{"-data-dir", restored, archive}, env, nil, &stderr); err != nil {
		t.Fatalf("taskto restore: %v\n%s", err, &stderr)
	}
	cfg.DataDir = restored
	addr, stop = startRun(t, cfg)
	defer func() { _ = stop() }()
	for name, c := range map[string]string{"the ended session": second, "the session that changed the password, as the backup had it": first} {
		if code, _ := apiCall(t, addr, http.MethodGet, "/api/me", c, nil); code != http.StatusUnauthorized {
			t.Errorf("%s: /api/me after the restore = %d, want 401", name, code)
		}
	}
	// The data is the backup's, and signing in again works with its password.
	if c := login(addr); c == "" {
		t.Fatal("no session after signing in again")
	} else if code, _ := apiCall(t, addr, http.MethodGet, "/api/me", c, nil); code != http.StatusOK {
		t.Fatalf("/api/me with the new login = %d, want 200", code)
	}
}

// /dev/null is a character device, but a backup written to it (a smoke test, a cron dry
// run) isn't one written to a terminal.
func TestDevNullIsNotATerminal(t *testing.T) {
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Skip(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Fatal("/dev/null was taken for a terminal")
	}
}

// Where hard links aren't available, the backup is copied into a file created exclusively:
// it lands complete, and an existing file is still never replaced.
func TestCopyExclusive(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("backup bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	if err := copyExclusive(src, dst); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "backup bytes" {
		t.Fatalf("copied %q", got)
	}
	if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %o, want 600", fi.Mode().Perm())
	}
	if err := copyExclusive(src, dst); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second copy: err = %v, want it to refuse the existing file", err)
	}
}
