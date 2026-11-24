//go:build unix

package agentrt

// The supervisor with the real sidecar (agent/dist). Like sidecar_test.go, it only runs
// with TASKTO_AGENT_IT=1 (make agent-it).

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestSupervisorRestartsTheRealSidecar(t *testing.T) {
	t.Setenv(spikeToolsEnv, "1") // the pause tools the scripted model calls
	if os.Getenv("TASKTO_AGENT_IT") != "1" {
		t.Skip("set TASKTO_AGENT_IT=1 to run the agent sidecar integration tests")
	}
	entry, err := filepath.Abs("../../agent/dist/index.js")
	if err != nil {
		t.Fatal(err)
	}
	// A short directory: Unix socket paths are limited to about 100 bytes.
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

	// One answered submission, and one killed in the middle of a replay-safe tool call.
	conv, err := c.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first, err := c.Submit(ctx, conv, Input{Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := c.Wait(ctx, first); err != nil || settled.Status != "done" {
		t.Fatalf("wait = %+v, %v; want done", settled, err)
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

	// The same client reaches the new process, which has the conversation and finishes the
	// interrupted call.
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
