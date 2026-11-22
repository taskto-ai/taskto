//go:build unix

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/conversations"
	"taskto.ai/taskto/internal/db"
)

// fakeAgentSocket serves the agent runtime's list and retire routes on the socket the server's
// sidecar would use for dataDir.
type fakeAgentSocket struct {
	mu   sync.Mutex
	list []agentrt.ConversationInfo
}

func serveFakeAgent(t *testing.T, dataDir string, list ...agentrt.ConversationInfo) *fakeAgentSocket {
	t.Helper()
	_, socketDir, _ := agentPaths(dataDir)
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", agentrt.SocketPath(socketDir))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAgentSocket{list: list}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("GET /conversations", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"conversations": f.list})
	})
	mux.HandleFunc("POST /conversations/{id}/retire", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		for i, c := range f.list {
			if c.ID == id && !c.Retired {
				f.list[i].Retired = true
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"unknown conversation"}`)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

func (f *fakeAgentSocket) retired() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int64
	for _, c := range f.list {
		if c.Retired {
			out = append(out, c.ID)
		}
	}
	return out
}

func TestConversationsCommand(t *testing.T) {
	dataDir := filepath.Join(shortDataDir(t), "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	getenv := func(string) string { return "" }
	run := func(args ...string) (string, error) {
		var stdout, stderr bytes.Buffer
		err := runConversations(ctx, append([]string{"-data-dir", dataDir}, args...), getenv, &stdout, &stderr)
		return stdout.String(), err
	}

	if _, err := run("check"); err == nil || !strings.Contains(err.Error(), "no database") {
		t.Fatalf("check without a database: err = %v", err)
	}
	sqlDB, err := db.Open(ctx, filepath.Join(dataDir, db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if _, err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`INSERT INTO users (id, email, name, password_hash) VALUES (3, 'a@example.com', 'A', 'x');
		INSERT INTO workspaces (id, name) VALUES (1, 'Home');
		INSERT INTO conversations (id, workspace_id, owner_id, agent_id, title) VALUES
			(7, 1, 3, 101, 'Private plans'), (8, 1, 3, 102, 'Kept')`); err != nil {
		t.Fatal(err)
	}

	// The server isn't running: nothing answers on the socket.
	if _, err := run("check"); err == nil || !strings.Contains(err.Error(), "run this while the server runs with -agent") {
		t.Fatalf("check without the agent runtime: err = %v", err)
	}
	old := time.Now().Add(-conversations.OrphanGrace - time.Minute)
	agent := serveFakeAgent(t, dataDir,
		agentrt.ConversationInfo{ID: 102, CreatedAt: old.UnixMilli()},
		agentrt.ConversationInfo{ID: 150, CreatedAt: old.UnixMilli()},
		agentrt.ConversationInfo{ID: 151, CreatedAt: time.Now().UnixMilli()},
	)
	for _, tc := range []struct {
		args []string
		err  string
	}{
		{nil, "want check or clean"},
		{[]string{"tidy"}, "want check or clean"},
		{[]string{"check", "clean"}, "want check or clean"},
		{[]string{"clean"}, "clean what? add -lost, -orphans or both"},
		{[]string{"-lost", "check"}, "-lost, -orphans and -all are for clean"},
		{[]string{"-all", "-orphans", "clean"}, "-all goes with -lost"},
	} {
		if _, err := run(tc.args...); err == nil || err.Error() != tc.err {
			t.Errorf("taskto conversations %v: err = %v, want %q", tc.args, err, tc.err)
		}
	}
	out, err := run("check")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"lost: 1 conversations",
		"  conversation 7 in workspace 1, owner 3, created ",
		"(agent runtime conversation 101)",
		"orphaned: 1 agent runtime conversations",
		fmt.Sprintf("  agent runtime conversation 150, created %s\n", time.UnixMilli(old.UnixMilli()).UTC().Format(time.RFC3339)),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("check printed:\n%s\nwant %q", out, want)
		}
	}
	if strings.Contains(out, "Private plans") || strings.Contains(out, "151") {
		t.Errorf("check printed:\n%s\nwant no titles and nothing created within the grace period", out)
	}
	if len(agent.retired()) != 0 {
		t.Fatalf("check retired %v", agent.retired())
	}

	out, err = run("-orphans", "clean")
	if err != nil || out != "retired 1 orphaned agent runtime conversations\n" {
		t.Fatalf("clean -orphans = %q, %v", out, err)
	}
	if fmt.Sprint(agent.retired()) != "[150]" {
		t.Fatalf("retired = %v, want [150]", agent.retired())
	}
	out, err = run("-lost", "-orphans", "clean")
	if err != nil || out != "deleted 1 lost conversations\nretired 0 orphaned agent runtime conversations\n" {
		t.Fatalf("clean -lost -orphans = %q, %v", out, err)
	}
	var left []int64
	rows, err := sqlDB.Query(`SELECT id FROM conversations ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		left = append(left, id)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(left) != "[8]" {
		t.Fatalf("conversations left = %v, want [8]", left)
	}
	if out, err := run("check"); err != nil || !strings.Contains(out, "lost: 0") || !strings.Contains(out, "orphaned: 0") {
		t.Fatalf("check after cleaning = %q, %v; want nothing found", out, err)
	}
}

func TestCleanRefusesToDeleteEveryConversation(t *testing.T) {
	dataDir := filepath.Join(shortDataDir(t), "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run := func(args ...string) (string, error) {
		var stdout, stderr bytes.Buffer
		err := runConversations(ctx, append([]string{"-data-dir", dataDir}, args...), func(string) string { return "" }, &stdout, &stderr)
		return stdout.String(), err
	}
	sqlDB, err := db.Open(ctx, filepath.Join(dataDir, db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if _, err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`INSERT INTO users (id, email, name, password_hash) VALUES (3, 'a@example.com', 'A', 'x');
		INSERT INTO workspaces (id, name) VALUES (1, 'Home');
		INSERT INTO conversations (id, workspace_id, owner_id, agent_id, title) VALUES (7, 1, 3, 101, 'A'), (8, 1, 3, 102, 'B')`); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		var n int
		if err := sqlDB.QueryRow(`SELECT count(*) FROM conversations`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// The agent runtime lists none of them: wiped, or a listing gone wrong. Nothing is
	// deleted without -all.
	serveFakeAgent(t, dataDir)
	if _, err := run("-lost", "clean"); err == nil || !strings.Contains(err.Error(), "clean -lost -all") {
		t.Fatalf("clean -lost with every conversation lost: err = %v", err)
	}
	if n := count(); n != 2 {
		t.Fatalf("%d conversations left, want 2", n)
	}
	out, err := run("-lost", "-all", "clean")
	if err != nil || !strings.Contains(out, "deleted 2 lost conversations") {
		t.Fatalf("clean -lost -all: %q, %v", out, err)
	}
	if n := count(); n != 0 {
		t.Fatalf("%d conversations left, want 0", n)
	}
}
