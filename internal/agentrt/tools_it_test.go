package agentrt

// The sidecar's knowledge-base and board tools against a stand-in for the server's tools
// socket: what they send, and what they make of the answers. Gated by TASKTO_AGENT_IT=1.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// toolsServer stands in for the server's tools socket: it records each call's tool,
// capability and arguments, and answers with reply.
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
	// A short directory: Unix socket paths are limited to about 100 bytes.
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

	// Without a capability the tool says so, and calls nothing.
	if got := run(Input{Content: "tool:list_spaces {}"}); !strings.Contains(got, "not available in this conversation yet") {
		t.Fatalf("without a capability: %q", got)
	}
	// With one, the call carries it; a new one replaces it.
	run(Input{Content: "tool:list_spaces {}", Capability: "ttc1.7.first"})
	if got := fake.last(t); got != "list_spaces ttc1.7.first {}" {
		t.Fatalf("call = %q", got)
	}
	if got := run(Input{Content: "tool:list_spaces {}", Capability: "ttc1.7.second"}); got != `finished list_spaces: {"spaces":[{"id":1}]}` {
		t.Fatalf("answer = %q", got)
	}
	// A refusal is an error result with the server's message.
	if got := run(Input{Content: `tool:read_doc {"space_id":2,"path":"a.md"}`}); got != "finished read_doc: error: not found" {
		t.Fatalf("refusal = %q", got)
	}
	if got := fake.last(t); got != `read_doc ttc1.7.second {"space_id":2,"path":"a.md"}` {
		t.Fatalf("call = %q", got)
	}
	// Arguments outside a tool's schema never reach the server.
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

	// The capability is kept across a restart.
	s.kill()
	s.start()
	run(Input{Content: "tool:list_spaces {}"})
	if got := fake.last(t); got != "list_spaces ttc1.7.second {}" {
		t.Fatalf("after a restart: %q", got)
	}

	// A capability that isn't printable ASCII is refused.
	if _, err := s.client.Submit(ctx, conv, Input{Content: "x", Capability: "two words"}); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("a bad capability: %v", err)
	}
}

// The model is offered the pause tools only when a test turns them on, and the conversation-only
// tools (tools.json's conversation_only flag: the agent browser's) only when the server has the
// browser on. A tool it isn't offered is an error result that never reaches the server.
func TestSidecarOffersOnlyTheToolsItShould(t *testing.T) {
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
	fake := &toolsServer{reply: func(string, string) (int, string) { return http.StatusOK, `{"ok":true}` }}
	srv := &http.Server{Handler: fake, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	run := func(s *sidecar, content string) string {
		t.Helper()
		ctx := ctxFor(t)
		conv, err := s.client.CreateConversation(ctx)
		if err != nil {
			t.Fatal(err)
		}
		sub, err := s.client.Submit(ctx, conv, Input{Content: content, Capability: "ttc1.7.first"})
		if err != nil {
			t.Fatal(err)
		}
		if settled, err := s.client.Wait(ctx, sub); err != nil || settled.Status != "done" {
			t.Fatalf("wait = %+v, %v", settled, err)
		}
		return lastAssistant(t, s.client, conv)
	}
	calls := func() int {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.calls)
	}

	// Production: no spike tools (the later variable wins over the test helper's), no browser.
	s := startSidecar(t, ToolsEnv+"="+path, spikeToolsEnv+"=")
	for _, content := range []string{"tool:pause_safe:100", "tool:pause_once:100", "tool:browser_tabs {}"} {
		if got := run(s, content); !strings.Contains(got, "error") {
			t.Fatalf("%s offered without being turned on: %q", content, got)
		}
	}
	if n := calls(); n != 0 {
		t.Fatalf("%d calls reached the server for tools that weren't offered", n)
	}
	if got := run(s, "tool:list_spaces {}"); !strings.Contains(got, `{"ok":true}`) {
		t.Fatalf("list_spaces: %q", got)
	}

	// With the browser on, a conversation-only tool is offered, whatever its name.
	b := startSidecar(t, ToolsEnv+"="+path, "TASKTO_AGENT_BROWSER=true")
	if got := run(b, "tool:browser_tabs {}"); !strings.Contains(got, `{"ok":true}`) {
		t.Fatalf("browser_tabs with the browser on: %q", got)
	}
}
