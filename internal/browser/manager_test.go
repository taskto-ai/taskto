//go:build unix

package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	switch os.Getenv("TASKTO_FAKE_PROGRAM") {
	case "chromium":
		fakeChromium(os.Getenv("TASKTO_FAKE_LOG"))
		return
	case "xvfb":
		_, _ = os.NewFile(3, "displayfd").WriteString("7\n")
		runUntilStopped()
	case "x11vnc":
		runUntilStopped()
	}
	os.Exit(m.Run())
}

func runUntilStopped() {
	for {
		time.Sleep(time.Hour)
	}
}

func fakeChromium(logPath string) {
	logf := func(what string) {
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%s %d\n", what, os.Getpid())
			f.Close()
		}
	}
	logf("start")
	in, out := os.NewFile(3, "devtools-in"), os.NewFile(4, "devtools-out")
	br := bufio.NewReader(in)
	shutdown := func() {
		time.Sleep(700 * time.Millisecond)
		logf("exit")
		os.Exit(0)
	}
	for {
		msg, err := readMessage(br)
		if err != nil {
			shutdown()
		}
		var m struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(msg, &m)
		b, _ := json.Marshal(map[string]any{"id": m.ID, "result": map[string]any{}})
		_, _ = out.Write(append(b, 0))
		if m.Method == "Browser.close" {
			shutdown()
		}
	}
}

type fakeHost struct {
	log string
	vnc func(io.ReadWriteCloser)
}

func (h *fakeHost) Bin(Program) string { return "fake" }

func (h *fakeHost) Start(_ int64, spec ProcSpec, extra []*os.File) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), "TASKTO_FAKE_PROGRAM="+string(spec.Program), "TASKTO_FAKE_LOG="+h.log)
	cmd.ExtraFiles = extra
	NewGroup(cmd)
	return cmd, cmd.Start()
}

func (h *fakeHost) DialVNC(context.Context, int64) (io.ReadWriteCloser, error) {
	if h.vnc == nil {
		return nil, ErrUnavailable
	}
	ours, theirs := net.Pipe()
	go h.vnc(theirs)
	return ours, nil
}

func (h *fakeHost) Clean(int64) error { return nil }

func (h *fakeHost) Purge(int64) error { return nil }

func TestRestartWaitsForTheOldBrowser(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	m := NewManager(Config{Host: &fakeHost{log: log}, Idle: 50 * time.Millisecond, Tick: 10 * time.Millisecond})
	t.Cleanup(m.Close)
	ctx := context.Background()
	if err := m.Start(ctx, 1, Testing); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, _ := m.Status(ctx, 1)
		if !st[Testing].Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the idle browser wasn't stopped")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := m.Start(ctx, 1, Testing); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		events = append(events, strings.Fields(line)[0])
	}
	if strings.Join(events, " ") != "start exit start" {
		t.Fatalf("the browsers' starts and exits: %v", events)
	}
}
