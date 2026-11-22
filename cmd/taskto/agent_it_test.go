//go:build unix

package main

// The server binary with the real sidecar (agent/dist). Like internal/agentrt's integration
// tests it needs Node and a built agent package, so it only runs with TASKTO_AGENT_IT=1
// (make agent-it).

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/privcheck"
)

type serverProcess struct {
	cmd    *exec.Cmd
	output *lockedBuffer
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var listeningAddr = regexp.MustCompile(`taskto listening"? addr=(\S+)`)

// startServer starts the server binary and waits until its agent runtime is ready.
func startServer(t *testing.T, bin string, args ...string) (*serverProcess, string) {
	t.Helper()
	if privilegedTestMachine(t) {
		args = append(args, "-"+privcheck.OverrideFlag)
	}
	s := &serverProcess{cmd: exec.Command(bin, args...), output: &lockedBuffer{}}
	s.cmd.Stdout = s.output
	s.cmd.Stderr = s.output
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s.cmd.ProcessState == nil {
			_ = s.cmd.Process.Kill()
			_ = s.cmd.Wait()
		}
		if t.Failed() {
			t.Logf("server %d output:\n%s", s.cmd.Process.Pid, s.output.String())
		}
	})
	var addr string
	waitFor(t, "the server to listen", func() bool {
		m := listeningAddr.FindStringSubmatch(s.output.String())
		if m != nil {
			addr = m[1]
		}
		return m != nil
	})
	waitFor(t, "the agent runtime to be ready", func() bool { return getHealthz(t, addr)["agent"] == "ready" })
	return s, addr
}

// A server killed with SIGKILL runs no shutdown code, yet its sidecar exits too (its stdin
// closes), and the next server starts on the same data directory without a hitch.
func TestKilledServerTakesTheSidecarWithIt(t *testing.T) {
	if os.Getenv("TASKTO_AGENT_IT") != "1" {
		t.Skip("set TASKTO_AGENT_IT=1 to run the agent sidecar integration tests")
	}
	entry, err := filepath.Abs("../../agent/dist/index.js")
	if err != nil {
		t.Fatal(err)
	}
	dir := shortDataDir(t)
	bin := filepath.Join(dir, "taskto")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	dataDir := filepath.Join(dir, "data")
	args := []string{"-addr", "127.0.0.1:0", "-data-dir", dataDir, "-agent", "-agent-entry", entry}
	_, socketDir, _ := agentPaths(dataDir)
	client := agentrt.New(filepath.Join(socketDir, "agent.sock"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	first, _ := startServer(t, bin, args...)
	h, err := client.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sidecar := h.PID
	t.Cleanup(func() { _ = syscall.Kill(sidecar, syscall.SIGKILL) })

	if err := first.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = first.cmd.Wait()
	killed := time.Now()
	deadline := killed.Add(10 * time.Second)
	for !processGone(sidecar) {
		if time.Now().After(deadline) {
			t.Fatalf("the sidecar %d still runs 10 s after its server was killed", sidecar)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("the sidecar exited %s after its server was killed", time.Since(killed).Round(time.Millisecond))

	second, _ := startServer(t, bin, args...)
	if out := second.output.String(); strings.Contains(out, "in use") || strings.Contains(out, "another agent runtime") {
		t.Fatalf("the fresh server met a leftover:\n%s", out)
	}
	h, err = client.Health(ctx)
	if err != nil || h.PID == sidecar {
		t.Fatalf("health after the restart = %+v, %v; want a new sidecar", h, err)
	}
	conv, err := client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := client.Submit(ctx, conv, agentrt.Input{Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := client.Wait(ctx, sub); err != nil || settled.Status != "done" {
		t.Fatalf("wait = %+v, %v; want done", settled, err)
	}

	// A clean stop stops the sidecar too.
	if err := second.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := second.cmd.Wait(); err != nil {
		t.Fatalf("server after SIGTERM: %v", err)
	}
	if !processGone(h.PID) {
		t.Fatalf("the sidecar %d outlived a clean stop", h.PID)
	}
}
