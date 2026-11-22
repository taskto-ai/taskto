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

// Restic is the Runner that sends backups to a restic repository, as compose.yaml's
// backup service does: restic runs `taskto backup -uncompressed -` itself and saves a
// snapshot only if it exits cleanly. The repository and its password come from restic's
// own environment (RESTIC_REPOSITORY_FILE, RESTIC_PASSWORD_FILE). It only ever adds:
// nothing here asks restic to remove a snapshot.
type Restic struct {
	// Path is the restic program.
	Path string
	// Taskto is this binary, which restic runs for the archive.
	Taskto string
	// DataDir is the data directory to back up.
	DataDir string
	// Env is restic's environment; nil is this process's.
	Env []string
	// Log receives restic's messages as well, if not nil.
	Log io.Writer
}

// BackupArgs are restic's arguments for one backup of dataDir with the binary taskto.
// The host name is fixed, so snapshots don't depend on the container's.
func BackupArgs(taskto, dataDir string) []string {
	return []string{"backup", "--json", "--host=taskto", "--tag=taskto", "--stdin-filename=taskto.tar",
		"--stdin-from-command", "--", taskto, "backup", "-uncompressed", "-data-dir", dataDir, "-"}
}

// Check reads the repository's config file: it answers only if the repository is there,
// reachable, and opens with the password.
func (r Restic) Check(ctx context.Context) error {
	return r.run(ctx, nil, "cat", "config")
}

// Backup runs one backup and returns the snapshot's ID, from restic's JSON summary.
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

// waitDelay is how long restic has to stop (and stop `taskto backup`) once asked, before
// it is killed.
const waitDelay = 30 * time.Second

// run runs restic with args, passing each line of its output to line, if not nil. Its
// error has the last thing restic said.
func (r Restic) run(ctx context.Context, line func([]byte), args ...string) error {
	cmd := exec.CommandContext(ctx, r.Path, args...) // #nosec G204 -- the operator's restic, with arguments made here.
	cmd.Env = r.Env
	// restic stops cleanly on an interrupt, stopping the command it runs too.
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
	_, _ = io.Copy(io.Discard, stdout) // a line too long for the scanner: drain the rest
	if err := cmd.Wait(); err != nil {
		if msg := lastMessage(tail.String()); msg != "" {
			return fmt.Errorf("restic %s: %s (%w)", args[0], msg, err)
		}
		return fmt.Errorf("restic %s: %w", args[0], err)
	}
	return nil
}

// lastMessage is the last non-empty line restic wrote to its standard error, with the
// message taken out of a JSON one (restic --json writes errors as JSON).
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

// tailWriter keeps the last max bytes written to it.
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
