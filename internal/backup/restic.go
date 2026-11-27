package backup

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type Restic struct {
	Path    string
	Taskto  string
	DataDir string
	Env     []string
	Log     io.Writer
}

func BackupArgs(taskto, dataDir string) []string {
	return []string{"backup", "--json", "--host=taskto", "--tag=taskto", "--stdin-filename=taskto.tar",
		"--stdin-from-command", "--", taskto, "backup", "-uncompressed", "-data-dir", dataDir, "-"}
}

func (r Restic) Check(ctx context.Context) error {
	return r.run(ctx, nil, "cat", "config")
}

func (r Restic) Backup(ctx context.Context) (string, error) {
	var id string
	err := r.run(ctx, func(line []byte) {
		var m struct {
			MessageType string `json:"message_type"`
			SnapshotID  string `json:"snapshot_id"`
		}
		if json.Unmarshal(line, &m) == nil && m.MessageType == "summary" {
			id = m.SnapshotID
		}
	}, BackupArgs(r.Taskto, r.DataDir)...)
	if err == nil && id == "" {
		err = errors.New("restic backup: no snapshot in its summary")
	}
	return id, err
}

const waitDelay = 30 * time.Second

func (r Restic) run(ctx context.Context, line func([]byte), args ...string) error {
	cmd := exec.CommandContext(ctx, r.Path, args...)
	cmd.Env = r.Env
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = waitDelay
	tail := &tailWriter{max: 4096}
	cmd.Stderr = tail
	if r.Log != nil {
		cmd.Stderr = io.MultiWriter(tail, r.Log)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("restic: %w", err)
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if line != nil {
			line(sc.Bytes())
		}
	}
	_, _ = io.Copy(io.Discard, stdout)
	if err := cmd.Wait(); err != nil {
		if msg := lastMessage(tail.String()); msg != "" {
			return fmt.Errorf("restic %s: %s (%w)", args[0], msg, err)
		}
		return fmt.Errorf("restic %s: %w", args[0], err)
	}
	return nil
}

func lastMessage(stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		var m struct {
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(l), &m) == nil {
			if m.Message != "" {
				return m.Message
			}
			if m.Error.Message != "" {
				return m.Error.Message
			}
		}
		return l
	}
	return ""
}

type tailWriter struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if over := len(t.b) - t.max; over > 0 {
		t.b = bytes.Clone(t.b[over:])
	}
	return len(p), nil
}

func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
