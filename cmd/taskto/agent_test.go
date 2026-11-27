//go:build unix

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/cliworker/clitest"
	"taskto.ai/taskto/internal/config"
	"taskto.ai/taskto/internal/db"
	"taskto.ai/taskto/internal/privcheck"
)

func TestMain(m *testing.M) {
	clitest.Main()
	if os.Getenv("TASKTO_FAKE_SIDECAR") == "1" {
		os.Exit(fakeSidecar())
	}
	os.Exit(m.Run())
}

func fakeSidecar() int {
	socket, db := os.Getenv("TASKTO_AGENT_SOCKET"), os.Getenv("TASKTO_AGENT_DB")
	env := fmt.Sprintf("providers=%s eof=%s", os.Getenv("TASKTO_AGENT_PROVIDERS"), os.Getenv(agentrt.ExitOnEOFEnv))
	if err := os.WriteFile(db+".env", []byte(env), 0o600); err != nil {
		panic(err)
	}
	terms := make(chan os.Signal, 1)
	signal.Notify(terms, syscall.SIGTERM)
	_ = os.Remove(socket)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		panic(err)
	}
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if os.Getenv("TASKTO_FAKE_SIDECAR_HANG") == "1" && r.URL.Path != "/healthz" {
			<-r.Context().Done()
			return
		}
		_, _ = fmt.Fprintf(w, `{"status":"ok","pid":%d}`, os.Getpid())
	})}
	go func() { _ = srv.Serve(ln) }()
	eof := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		close(eof)
	}()
	select {
	case <-terms:
	case <-eof:
	}
	return 0
}

func shortDataDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "taskto-cmd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func fakeAgentConfig(t *testing.T) config.Config {
	t.Helper()
	t.Setenv("TASKTO_FAKE_SIDECAR", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dataDir := shortDataDir(t)
	entry := filepath.Join(dataDir, "index.js")
	if err := os.WriteFile(entry, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Addr = "127.0.0.1:0"
	cfg.DataDir = dataDir
	cfg.Agent = true
	cfg.AgentNode = exe
	cfg.AgentEntry = entry
	cfg.UnsafePrivilegedAgents = privilegedTestMachine(t)
	return cfg
}

func privilegedTestMachine(t *testing.T) bool {
	t.Helper()
	problems := privcheck.Process{}.Problems(context.Background())
	if len(problems) > 0 {
		t.Logf("this machine is privileged (%s): the test sets -%s", strings.Join(problems, "; "), privcheck.OverrideFlag)
	}
	return len(problems) > 0
}

func startRun(t *testing.T, cfg config.Config) (addr string, stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	addrc := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, func(a net.Addr) { addrc <- a }) }()
	stop = func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(30 * time.Second):
			t.Fatal("run didn't stop after cancel")
			return nil
		}
	}
	select {
	case a := <-addrc:
		return a.String(), stop
	case err := <-done:
		cancel()
		t.Fatalf("run returned before listening: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("run didn't start listening")
	}
	return "", nil
}

func getHealthz(t *testing.T, addr string) map[string]string {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d %v (%v), want 200", resp.StatusCode, body, err)
	}
	return body
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func processGone(pid int) bool {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return true
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	i := bytes.LastIndexByte(stat, ')')
	return i >= 0 && i+2 < len(stat) && stat[i+2] == 'Z'
}

func TestAgentPaths(t *testing.T) {
	dir, socketDir, db := agentPaths("/srv/taskto")
	if dir != "/srv/taskto/agent" || socketDir != "/srv/taskto/agent/run" || db != "/srv/taskto/agent/agent.db" {
		t.Fatalf("agentPaths = %s, %s, %s; want everything under /srv/taskto/agent", dir, socketDir, db)
	}
}

func TestRunWithTheAgentRuntime(t *testing.T) {
	cfg := fakeAgentConfig(t)
	cfg.AgentProviders = filepath.Join(cfg.DataDir, "operator.json")
	operator := `{"id":"local","kind":"openai-compatible","baseUrl":"http://127.0.0.1:11434/v1","models":[{"id":"m"}]}`
	if err := os.WriteFile(cfg.AgentProviders, []byte(`{"providers":[`+operator+`]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	addr, stop := startRun(t, cfg)
	waitFor(t, "the agent runtime to be ready", func() bool { return getHealthz(t, addr)["agent"] == "ready" })

	_, socketDir, db := agentPaths(cfg.DataDir)
	socket := filepath.Join(socketDir, "agent.sock")
	info, err := os.Stat(socketDir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("socket directory: %v, %v; want mode 700", info, err)
	}
	generated := filepath.Join(filepath.Dir(db), "providers.json")
	env, err := os.ReadFile(db + ".env")
	if want := "providers=" + generated + " eof=1"; err != nil || string(env) != want {
		t.Fatalf("the sidecar got %q (%v), want %q", env, err, want)
	}
	var file struct {
		Providers []json.RawMessage `json:"providers"`
	}
	raw, err := os.ReadFile(generated)
	if err != nil {
		t.Fatal(err)
	}
	var first bytes.Buffer
	if err := json.Unmarshal(raw, &file); err != nil || len(file.Providers) != 1 ||
		json.Compact(&first, file.Providers[0]) != nil || first.String() != operator {
		t.Fatalf("providers file = %s (%v), want the operator's provider as written", raw, err)
	}
	if info, err := os.Stat(generated); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("providers file: %v, %v; want mode 600", info, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h, err := agentrt.New(socket).Health(ctx)
	if err != nil {
		t.Fatalf("the sidecar doesn't answer on %s: %v", socket, err)
	}

	if err := stop(); err != nil {
		t.Fatalf("run after cancel: %v", err)
	}
	if !processGone(h.PID) {
		t.Fatalf("the sidecar %d outlived the server", h.PID)
	}
}

func TestRunFailsFastWithoutTheAgentRuntime(t *testing.T) {
	tests := []struct {
		name    string
		change  func(*config.Config)
		wantErr string
	}{
		{"no node", func(c *config.Config) { c.AgentNode = "/does/not/exist/node" }, "Node.js executable"},
		{"node not in PATH", func(c *config.Config) { c.AgentNode = "taskto-no-such-node" }, "Node.js executable"},
		{"no entry", func(c *config.Config) { c.AgentEntry = filepath.Join(c.DataDir, "missing.js") }, "entry point"},
		{"entry is a directory", func(c *config.Config) { c.AgentEntry = c.DataDir }, "is not a file"},
		{"no providers file", func(c *config.Config) { c.AgentProviders = filepath.Join(c.DataDir, "missing.json") }, "providers file"},
		{"providers file not JSON", func(c *config.Config) {
			c.AgentProviders = writeTemp(t, c.DataDir, `{"providers":`)
		}, "not valid JSON"},
		{"providers file takes an account's ID", func(c *config.Config) {
			c.AgentProviders = writeTemp(t, c.DataDir, `{"providers":[{"id":"ws1-x","kind":"script"}]}`)
		}, "looks like a provider account's"},
		{"socket path too long", func(c *config.Config) {
			c.DataDir = filepath.Join(c.DataDir, strings.Repeat("d", 100))
		}, "shorter data directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := fakeAgentConfig(t)
			tt.change(&cfg)
			done := make(chan error, 1)
			go func() {
				done <- run(context.Background(), cfg, func(net.Addr) { t.Error("the server started listening") })
			}()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("run = %v, want an error containing %q", err, tt.wantErr)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("run didn't fail fast")
			}
		})
	}
}

func writeTemp(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "operator.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAgentEntryLookup(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if _, err := agentEntry(""); err == nil || !strings.Contains(err.Error(), "-agent-entry") {
		t.Fatalf("no entry anywhere: err = %v, want one that says how to fix it", err)
	}
	if err := os.MkdirAll(filepath.Join("agent", "dist"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentDist, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := agentEntry(""); err != nil || got != agentDist {
		t.Fatalf("agentEntry = %q, %v; want %q", got, err, agentDist)
	}
	if got, err := agentEntry("/elsewhere/index.js"); err == nil {
		t.Fatalf("a missing configured entry = %q, want an error, not a fallback", got)
	}
}

func TestRunRefusesPrivilegedAgents(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	for name, change := range map[string]func(*config.Config){
		"agent runtime":     func(*config.Config) {},
		"local CLI workers": func(c *config.Config) { c.Agent, c.CLIWorkers = false, "local" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := fakeAgentConfig(t)
			change(&cfg)
			cfg.UnsafePrivilegedAgents = false
			err := run(context.Background(), cfg, func(net.Addr) { t.Error("the server started listening") })
			want := "`sudo -n true` runs as root without a password"
			if os.Geteuid() == 0 {
				want = "it runs as root"
			}
			if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "-"+privcheck.OverrideFlag) {
				t.Fatalf("run = %v", err)
			}
			if _, err := os.Stat(filepath.Join(cfg.DataDir, "taskto.db")); err == nil {
				t.Error("the database was opened before the check")
			}

			cfg.UnsafePrivilegedAgents = true
			_, stop := startRun(t, cfg)
			if err := stop(); err != nil {
				t.Fatalf("with the override: %v", err)
			}
		})
	}
}

func TestPendingPurgeStepsDoNotHoldUpTheServersStart(t *testing.T) {
	cfg := fakeAgentConfig(t)
	t.Setenv("TASKTO_FAKE_SIDECAR_HANG", "1")
	sqlDB, err := db.Open(context.Background(), filepath.Join(cfg.DataDir, db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(context.Background(), sqlDB); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`INSERT INTO purge_cleanups (user_id, kind, target) VALUES (7, 'agent_conversation', '101')`); err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	addr, stop := startRun(t, cfg)
	if h := getHealthz(t, addr); h["status"] != "ok" {
		t.Fatalf("healthz = %v", h)
	}
	if err := stop(); err != nil {
		t.Fatalf("run: %v", err)
	}
	sqlDB, err = db.Open(context.Background(), filepath.Join(cfg.DataDir, db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	var n int
	if err := sqlDB.QueryRow(`SELECT count(*) FROM purge_cleanups`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d steps recorded (%v), want the one that never answered", n, err)
	}
}
