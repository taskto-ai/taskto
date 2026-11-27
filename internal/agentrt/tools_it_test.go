package agentrt

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type toolsServer struct {
	mu    sync.Mutex
	calls []string
	reply func(tool, capability string) (int, string)
}

func (f *toolsServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	tool := strings.TrimPrefix(r.URL.Path, "/tools/")
	capability := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	f.calls = append(f.calls, fmt.Sprintf("%s %s %s", tool, capability, body))
	f.mu.Unlock()
	code, out := f.reply(tool, capability)
	w.WriteHeader(code)
	_, _ = io.WriteString(w, out)
}

func (f *toolsServer) last(t *testing.T) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("no call reached the tools socket")
	}
	return f.calls[len(f.calls)-1]
}

func TestServerTools(t *testing.T) {
	if os.Getenv("TASKTO_AGENT_IT") != "1" {
		t.Skip("set TASKTO_AGENT_IT=1 to run the agent sidecar integration tests")
	}
	dir, err := os.MkdirTemp("", "taskto-tools-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "tools.sock")
	ln, err := new(net.ListenConfig).Listen(context.Background(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	fake := &toolsServer{reply: func(tool, capability string) (int, string) {
		if capability != "ttc1.7.second" {
			return http.StatusUnauthorized, `{"error":"no valid capability"}`
		}
		if tool == "read_doc" {
			return http.StatusNotFound, `{"error":"not found"}`
		}
		return http.StatusOK, `{"spaces":[{"id":1}]}`
	}}
	srv := &http.Server{Handler: fake, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	s := startSidecar(t, ToolsEnv+"="+path)
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run := func(in Input) string {
		t.Helper()
		sub, err := s.client.Submit(ctx, conv, in)
		if err != nil {
			t.Fatal(err)
		}
		if settled, err := s.client.Wait(ctx, sub); err != nil || settled.Status != "done" {
			t.Fatalf("wait = %+v, %v", settled, err)
		}
		return lastAssistant(t, s.client, conv)
	}

	if got := run(Input{Content: "tool:list_spaces {}"}); !strings.Contains(got, "not available in this conversation yet") {
		t.Fatalf("without a capability: %q", got)
	}
	run(Input{Content: "tool:list_spaces {}", Capability: "ttc1.7.first"})
	if got := fake.last(t); got != "list_spaces ttc1.7.first {}" {
		t.Fatalf("call = %q", got)
	}
	if got := run(Input{Content: "tool:list_spaces {}", Capability: "ttc1.7.second"}); got != `finished list_spaces: {"spaces":[{"id":1}]}` {
		t.Fatalf("answer = %q", got)
	}
	if got := run(Input{Content: `tool:read_doc {"space_id":2,"path":"a.md"}`}); got != "finished read_doc: error: not found" {
		t.Fatalf("refusal = %q", got)
	}
	if got := fake.last(t); got != `read_doc ttc1.7.second {"space_id":2,"path":"a.md"}` {
		t.Fatalf("call = %q", got)
	}
	fake.mu.Lock()
	n := len(fake.calls)
	fake.mu.Unlock()
	run(Input{Content: `tool:read_doc {"space_id":2,"path":"a.md","user_id":1}`})
	fake.mu.Lock()
	after := len(fake.calls)
	fake.mu.Unlock()
	if after != n {
		t.Fatalf("a call with an unknown argument reached the server: %q", fake.last(t))
	}

	s.kill()
	s.start()
	run(Input{Content: "tool:list_spaces {}"})
	if got := fake.last(t); got != "list_spaces ttc1.7.second {}" {
		t.Fatalf("after a restart: %q", got)
	}

	if _, err := s.client.Submit(ctx, conv, Input{Content: "x", Capability: "two words"}); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("a bad capability: %v", err)
	}
}

func fixtureTools(t *testing.T) (path string, fake *toolsServer) {
	t.Helper()
	dir, err := os.MkdirTemp("", "taskto-tools-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path = filepath.Join(dir, "tools.sock")
	ln, err := new(net.ListenConfig).Listen(context.Background(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	fake = &toolsServer{reply: func(string, string) (int, string) { return http.StatusOK, `{"ok":true}` }}
	srv := &http.Server{Handler: fake, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return path, fake
}

func (f *toolsServer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func runTurn(t *testing.T, c *Client, content string) string {
	t.Helper()
	ctx := ctxFor(t)
	conv, err := c.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := c.Submit(ctx, conv, Input{Content: content, Capability: "ttc1.7.first"})
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := c.Wait(ctx, sub); err != nil || settled.Status != "done" {
		t.Fatalf("wait = %+v, %v", settled, err)
	}
	return lastAssistant(t, c, conv)
}

func TestSidecarOffersConversationOnlyToolsByTheirFlag(t *testing.T) {
	path, fake := fixtureTools(t)

	off := startSidecar(t, ToolsEnv+"="+path, "TASKTO_AGENT_BROWSER=false")
	for _, tool := range []string{"fixture_flagged", "browser_tabs"} {
		if got := runTurn(t, off.client, "tool:"+tool+" {}"); !strings.Contains(got, "error") {
			t.Fatalf("%s offered with the browser off: %q", tool, got)
		}
	}
	if n := fake.callCount(); n != 0 {
		t.Fatalf("%d calls reached the server for tools that weren't offered", n)
	}
	for _, tool := range []string{"browser_unflagged", "list_spaces"} {
		if got := runTurn(t, off.client, "tool:"+tool+" {}"); !strings.Contains(got, `{"ok":true}`) {
			t.Fatalf("%s with the browser off: %q", tool, got)
		}
	}

	on := startSidecar(t, ToolsEnv+"="+path, "TASKTO_AGENT_BROWSER=true")
	for _, tool := range []string{"fixture_flagged", "browser_tabs", "browser_unflagged", "list_spaces"} {
		if got := runTurn(t, on.client, "tool:"+tool+" {}"); !strings.Contains(got, `{"ok":true}`) {
			t.Fatalf("%s with the browser on: %q", tool, got)
		}
	}
}

func TestProductionSidecarRefusesThePauseTools(t *testing.T) {
	path, fake := fixtureTools(t)
	s := startSidecarAt(t, productionEntry, ToolsEnv+"="+path, spikeToolsEnv+"=1")
	for _, content := range []string{"tool:pause_safe:100", "tool:pause_once:100", "tool:fixture_flagged {}"} {
		if got := runTurn(t, s.client, content); !strings.Contains(got, "error") {
			t.Fatalf("%s offered by the production sidecar: %q", content, got)
		}
	}
	if n := fake.callCount(); n != 0 {
		t.Fatalf("%d calls reached the server for tools that weren't offered", n)
	}
	if got := runTurn(t, s.client, "tool:list_spaces {}"); !strings.Contains(got, `{"ok":true}`) {
		t.Fatalf("list_spaces: %q", got)
	}
}

func TestSupervisedSidecarRefusesThePauseToolsWhateverTheServersEnvironment(t *testing.T) {
	if os.Getenv("TASKTO_AGENT_IT") != "1" {
		t.Skip("set TASKTO_AGENT_IT=1 to run the agent sidecar integration tests")
	}
	t.Setenv(spikeToolsEnv, "1")
	entry, err := filepath.Abs(productionEntry)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "taskto-agent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := NewSupervisor(SupervisorConfig{
		Entry:     entry,
		SocketDir: filepath.Join(dir, "run"),
		DB:        filepath.Join(dir, "agent.db"),
		Logger:    slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(ctxFor(t)) })
	c, err := s.Start(ctxFor(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"tool:pause_safe:100", "tool:pause_once:100"} {
		if got := runTurn(t, c, content); !strings.Contains(got, "error") {
			t.Fatalf("%s offered by the supervised sidecar: %q", content, got)
		}
	}
}
