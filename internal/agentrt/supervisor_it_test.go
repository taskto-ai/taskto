//go:build unix

package agentrt

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestSupervisorRestartsTheRealSidecar(t *testing.T) {
	if os.Getenv("TASKTO_AGENT_IT") != "1" {
		t.Skip("set TASKTO_AGENT_IT=1 to run the agent sidecar integration tests")
	}
	entry, err := filepath.Abs(testEntry)
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
	ctx := ctxFor(t)
	c, err := s.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.Socket())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %o, want 600", perm)
	}

	conv, err := c.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first, err := c.Submit(ctx, conv, Input{
		Content: "hello",
		Entry:   &InputEntry{Kind: "taskto.input", Data: json.RawMessage(`{"v":1,"author":7}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := c.Wait(ctx, first); err != nil || settled.Status != "done" {
		t.Fatalf("wait = %+v, %v; want done", settled, err)
	}
	for _, bad := range []*InputEntry{
		{Kind: "pi.user", Data: json.RawMessage(`{"v":1}`)},
		{Kind: "other.input", Data: json.RawMessage(`{"v":1}`)},
		{Kind: "taskto.input", Data: json.RawMessage(`"text"`)},
		{Kind: "taskto.input", Data: json.RawMessage(`{"text":"` + strings.Repeat("x", 5000) + `"}`)},
	} {
		if _, err := c.Submit(ctx, conv, Input{Content: "refused", Entry: bad}); err == nil {
			t.Fatalf("submit with entry %.40s was accepted; want a refusal", string(bad.Data))
		}
	}
	entries, err := c.Entries(ctx, conv, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Entries []struct {
			Kind string `json:"kind"`
			Data struct {
				V      int   `json:"v"`
				Author int64 `json:"author"`
			} `json:"data"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(entries, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) == 0 || page.Entries[0].Kind != "taskto.input" || page.Entries[0].Data.V != 1 || page.Entries[0].Data.Author != 7 {
		t.Fatalf("entries = %s; want taskto.input with author metadata", entries)
	}
	messages, err := c.Messages(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	sawInput := false
	for _, m := range messages {
		if m.Role == "user" && m.Text == "hello" {
			sawInput = true
		}
	}
	if !sawInput {
		t.Fatalf("messages = %+v; want the model to see the input hello", messages)
	}
	w, err := c.Watch(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	second, err := c.Submit(ctx, conv, Input{Content: "tool:pause_safe:2500"})
	if err != nil {
		t.Fatal(err)
	}
	waitForOps(t, w, "waited 400ms")

	pid := s.PID()
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a new sidecar", func() bool { p := s.PID(); return p != 0 && p != pid })
	if _, err := s.Ready(ctx); err != nil {
		t.Fatalf("ready after the kill: %v", err)
	}
	if !processGone(pid) {
		t.Fatalf("the killed sidecar %d is still there", pid)
	}

	if settled, err := c.Wait(ctx, second); err != nil || settled.Status != "done" {
		t.Fatalf("after restart: wait = %+v, %v; want done", settled, err)
	}
	msgs, err := c.Messages(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	var answers []string
	for _, m := range msgs {
		if m.Role == "assistant" {
			answers = append(answers, m.Text)
		}
	}
	joined := strings.Join(answers, "\n")
	if !strings.Contains(joined, "echo: hello") || !strings.Contains(joined, "finished pause_safe: paused 2500ms") {
		t.Fatalf("answers after restart = %q; want the first answer and the finished tool call", answers)
	}

	newPID := s.PID()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !processGone(newPID) {
		t.Fatal("the sidecar survived Stop")
	}
}
