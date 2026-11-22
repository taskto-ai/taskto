//go:build unix

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"taskto.ai/taskto/internal/backup"
	"taskto.ai/taskto/internal/config"
	"taskto.ai/taskto/internal/serverkey"
)

// Placeholders, made up for the test: the append-only user's password and the repository's.
const (
	itRestUser     = "taskto"
	itRestPassword = "fake-append-only-password" // #nosec G101 -- a made-up placeholder.
	itRepoPassword = "fake-repository-password"
)

// The append-only backups of docs/install.md, end to end, with the real restic and
// rest-server: the server's credentials add snapshots and can't remove them, the backup
// host's direct access to the files can, the nightly schedule's run records its success
// and stops early, leaving nothing behind, when the backup host is down, and the latest
// snapshot restores into a server that starts. Opt in with TASKTO_BACKUP_IT=1; restic and rest-server come from PATH, or
// TASKTO_RESTIC and TASKTO_REST_SERVER.
func TestAppendOnlyBackupAndRestore(t *testing.T) {
	if os.Getenv("TASKTO_BACKUP_IT") == "" {
		t.Skip("set TASKTO_BACKUP_IT=1 (with restic and rest-server installed) to run the append-only backup test")
	}
	restic := itTool(t, "TASKTO_RESTIC", "restic")
	restServer := itTool(t, "TASKTO_REST_SERVER", "rest-server")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dir := t.TempDir()
	bin := filepath.Join(dir, "taskto")
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// A running server with an owner, and files such as other features keep under the data
	// directory: a CLI login of local mode's, and a large file outside the spaces' git.
	logs := captureLog(t)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the server's log:\n%s", logs)
		}
	})
	cfg := config.Default()
	cfg.Addr = "127.0.0.1:0"
	cfg.DataDir = filepath.Join(dir, "data")
	addr, stop := startRun(t, cfg)
	token := setupTokenLine.FindStringSubmatch(logs.String())
	if token == nil {
		t.Fatal("no setup token in the log")
	}
	if code := postSetup(t, addr, token[1]); code != http.StatusCreated {
		t.Fatalf("setup = %d", code)
	}
	large := make([]byte, 16<<20)
	if _, err := rand.Read(large); err != nil {
		t.Fatal(err)
	}
	extra := map[string][]byte{
		"cli/1/home/.claude/.credentials.json": []byte(`{"fake":"cli-login"}`),
		"workspaces/1/large/recording.bin":     large,
	}
	// And the cache, of what can be downloaded again: left out.
	cached := "cache/dictation/model.bin"
	extra[cached] = []byte("fake-model")
	for name, content := range extra {
		p := filepath.Join(cfg.DataDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// The backup host: rest-server, append-only, a repository per user.
	repos := filepath.Join(dir, "repos")
	hash, err := bcrypt.GenerateFromPassword([]byte(itRestPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	htpasswd := filepath.Join(dir, "htpasswd")
	if err := os.WriteFile(htpasswd, []byte(itRestUser+":"+string(hash)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	restAddr := itFreeAddr(t)
	rs := exec.CommandContext(ctx, restServer, "--path", repos, "--listen", restAddr,
		"--append-only", "--private-repos", "--htpasswd-file", htpasswd)
	var rsLog bytes.Buffer
	rs.Stdout, rs.Stderr = &rsLog, &rsLog
	if err := rs.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = rs.Process.Kill()
		_ = rs.Wait()
		if t.Failed() {
			t.Logf("rest-server:\n%s", &rsLog)
		}
	})
	itWaitListening(t, restAddr)

	// The TaskTo.AI server's side: the append-only user, as compose.yaml's backup service.
	repoURL := fmt.Sprintf("http://%s:%s@%s/%s/", itRestUser, itRestPassword, restAddr, itRestUser)
	server := itResticEnv(dir, "rest:"+repoURL, "TASKTO_DATA_DIR="+cfg.DataDir)
	itRun(t, ctx, server, restic, "init")
	backupArgs := []string{"backup", "--json", "--host=taskto", "--tag=taskto", "--stdin-filename=taskto.tar",
		"--stdin-from-command", "--", bin, "backup", "-uncompressed", "-"}
	first := itSummary(t, itRun(t, ctx, server, restic, backupArgs...))
	second := itSummary(t, itRun(t, ctx, server, restic, backupArgs...))
	if first.DataAdded < int64(len(large)) {
		t.Fatalf("the first backup added %d bytes, want at least the large file's %d", first.DataAdded, len(large))
	}
	t.Logf("restic stored %d new bytes for the first backup, %d for the second", first.DataAdded, second.DataAdded)
	// Plain archives deduplicate: the second stores little beyond what changed.
	if second.DataAdded > first.DataAdded/4 {
		t.Fatalf("the second backup added %d bytes of the first's %d: the archive doesn't deduplicate", second.DataAdded, first.DataAdded)
	}

	// restic stores nothing when `taskto backup` fails.
	failing := append([]string{}, backupArgs[:len(backupArgs)-1]...)
	failing = append(failing, "-data-dir", filepath.Join(dir, "missing"), "-")
	if out, err := itTry(ctx, server, restic, failing...); err == nil {
		t.Fatalf("a backup whose taskto backup failed succeeded:\n%s", out)
	}
	if ids := itSnapshots(t, ctx, server, restic); len(ids) != 2 {
		t.Fatalf("snapshots after a failed backup = %v, want the 2 good ones", ids)
	}

	// The nightly schedule's run, once and now (the install's first backup): it checks the
	// repository, backs up, and records the success where the server reads it.
	itRun(t, ctx, server, bin, "backup-schedule", "-now", "-restic", restic)
	st, ok, err := backup.ReadStatus(cfg.DataDir)
	if err != nil || !ok || st.LastSnapshot == "" || st.Health(time.Now()) != backup.HealthOK {
		t.Fatalf("status after backup-schedule -now = %+v, %v, %v", st, ok, err)
	}
	ids := itSnapshots(t, ctx, server, restic)
	if len(ids) != 3 || !strings.HasPrefix(ids[2], st.LastSnapshot) {
		t.Fatalf("snapshots after the scheduled one = %v, want a third, %s", ids, st.LastSnapshot)
	}

	// The backup host is down: the run stops at the check, well before restic would give
	// up, writes nothing in the data directory, and records why, without the password.
	down := itResticEnv(dir, fmt.Sprintf("rest:http://%s:%s@%s/%s/", itRestUser, itRestPassword, itFreeAddr(t), itRestUser),
		"TASKTO_DATA_DIR="+cfg.DataDir)
	started := time.Now()
	if out, err := itTry(ctx, down, bin, "backup-schedule", "-now", "-restic", restic, "-check-timeout", "5s"); err == nil {
		t.Fatalf("backup-schedule -now with the backup host down succeeded:\n%s", out)
	}
	if took := time.Since(started); took > time.Minute {
		t.Fatalf("backup-schedule -now with the backup host down took %s", took)
	}
	failed, _, err := backup.ReadStatus(cfg.DataDir)
	if err != nil || !failed.LastErrorAt.After(failed.LastSuccess) || failed.LastSnapshot != st.LastSnapshot ||
		!strings.Contains(failed.LastError, "didn't answer") || strings.Contains(failed.LastError, itRestPassword) {
		t.Fatalf("status with the backup host down = %+v, %v", failed, err)
	}
	if left, _ := filepath.Glob(filepath.Join(cfg.DataDir, ".backup-*")); len(left) != 0 {
		t.Fatalf("left in the data directory: %v", left)
	}
	if ids := itSnapshots(t, ctx, server, restic); len(ids) != 3 {
		t.Fatalf("snapshots after the failed run = %v, want 3", ids)
	}

	// The server's user can't remove anything: restic's forget is refused, and so is a
	// DELETE of a snapshot's file sent to rest-server directly.
	if out, err := itTry(ctx, server, restic, "forget", "--keep-last=1", "--prune"); err == nil {
		t.Fatalf("forget with the append-only user succeeded:\n%s", out)
	} else {
		t.Logf("forget with the append-only user: %v\n%s", err, out)
	}
	ids = itSnapshots(t, ctx, server, restic)
	if len(ids) != 3 {
		t.Fatalf("snapshots after the append-only user's forget = %v, want all 3", ids)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, "http://"+restAddr+"/"+itRestUser+"/snapshots/"+ids[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(itRestUser, itRestPassword)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Fatalf("DELETE of a snapshot with the append-only user = %d, want refused", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(repos, itRestUser, "snapshots", ids[0])); err != nil {
		t.Fatalf("the snapshot's file after the DELETE: %v", err)
	}

	// Restore the latest snapshot into a new data directory, as after losing the server.
	serverKey, err := os.ReadFile(filepath.Join(cfg.DataDir, serverkey.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(dir, "restored")
	dump := exec.CommandContext(ctx, restic, "dump", "latest", "/taskto.tar")
	dump.Env = server
	restore := exec.CommandContext(ctx, bin, "restore", "-data-dir", restored, "-")
	pipe, err := dump.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	restore.Stdin = pipe
	var dumpErr, restoreOut bytes.Buffer
	dump.Stderr = &dumpErr
	restore.Stdout, restore.Stderr = &restoreOut, &restoreOut
	if err := dump.Start(); err != nil {
		t.Fatal(err)
	}
	if err := restore.Run(); err != nil {
		t.Fatalf("taskto restore: %v\n%s", err, &restoreOut)
	}
	if err := dump.Wait(); err != nil {
		t.Fatalf("restic dump: %v\n%s", err, &dumpErr)
	}
	for name, content := range extra {
		got, err := os.ReadFile(filepath.Join(restored, filepath.FromSlash(name)))
		if name == cached {
			if err == nil {
				t.Errorf("the cache's %s was restored", name)
			}
			continue
		}
		if err != nil || !bytes.Equal(got, content) {
			t.Errorf("restored %s differs (%v)", name, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(restored, serverkey.FileName)); err != nil || !bytes.Equal(got, serverKey) {
		t.Fatalf("restored server key differs (%v)", err)
	}
	restoredCfg := cfg
	restoredCfg.DataDir = restored
	addr, stop = startRun(t, restoredCfg)
	if code := postSetup(t, addr, "x"); code != http.StatusConflict {
		t.Fatalf("setup on the restored server = %d, want 409: the owner should be there", code)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	// The backup host's side, with the repository's files: the window keeps both, and
	// forgetting a snapshot works here, the only place it can.
	host := itResticEnv(dir, filepath.Join(repos, itRestUser))
	itRun(t, ctx, host, restic, "forget", "--keep-within=90d", "--prune")
	if got := itSnapshots(t, ctx, host, restic); len(got) != 3 {
		t.Fatalf("snapshots after forget --keep-within=90d = %v, want all 3", got)
	}
	itRun(t, ctx, host, restic, "forget", "--prune", ids[0])
	if got := itSnapshots(t, ctx, host, restic); !slices.Equal(got, ids[1:]) {
		t.Fatalf("snapshots after forgetting %s on the backup host = %v, want only %v", ids[0], got, ids[1:])
	}
	itRun(t, ctx, host, restic, "check")
}

// itTool is the path of a program the test needs: from env, else from PATH. Once the test
// is opted in, a missing program fails it rather than skipping it, so CI can't pass
// without running it.
func itTool(t *testing.T, env, name string) string {
	t.Helper()
	if p := os.Getenv(env); p != "" {
		return p
	}
	p, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("%s isn't on PATH (or set %s): %v", name, env, err)
	}
	return p
}

// itFreeAddr is a loopback address with a port nothing listens on right now.
func itFreeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func itWaitListening(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing listens on %s: %v", addr, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// itResticEnv is the environment restic runs with: this process's, without any RESTIC_*
// of the caller's, and with the repository, its password and a cache in dir.
func itResticEnv(dir, repo string, extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "RESTIC_") && !strings.HasPrefix(kv, "TASKTO_") {
			env = append(env, kv)
		}
	}
	env = append(env, "RESTIC_REPOSITORY="+repo, "RESTIC_PASSWORD="+itRepoPassword,
		"RESTIC_CACHE_DIR="+filepath.Join(dir, "restic-cache"))
	return append(env, extra...)
}

func itTry(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	return append(out, stderr.Bytes()...), err
}

func itRun(t *testing.T, ctx context.Context, env []string, name string, args ...string) []byte {
	t.Helper()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s%s", filepath.Base(name), strings.Join(args, " "), err, out, &stderr)
	}
	return out
}

// resticSummary is the part of `restic backup --json`'s summary the test reads.
type resticSummary struct {
	MessageType string `json:"message_type"`
	DataAdded   int64  `json:"data_added"`
	SnapshotID  string `json:"snapshot_id"`
}

func itSummary(t *testing.T, out []byte) resticSummary {
	t.Helper()
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var s resticSummary
		if json.Unmarshal(sc.Bytes(), &s) == nil && s.MessageType == "summary" {
			if s.SnapshotID == "" {
				t.Fatalf("restic backup saved no snapshot:\n%s", out)
			}
			return s
		}
	}
	t.Fatalf("no summary in restic backup's output:\n%s", out)
	return resticSummary{}
}

// itSnapshots are the repository's snapshots' full IDs, oldest first.
func itSnapshots(t *testing.T, ctx context.Context, env []string, restic string) []string {
	t.Helper()
	var snaps []struct {
		ID   string    `json:"id"`
		Time time.Time `json:"time"`
	}
	if err := json.Unmarshal(itRun(t, ctx, env, restic, "snapshots", "--json"), &snaps); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(snaps))
	for i, s := range snaps {
		ids[i] = s.ID
	}
	return ids
}
