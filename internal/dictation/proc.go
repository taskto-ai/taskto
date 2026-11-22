package dictation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The local runtimes and add-ons run as child processes, one per dictation: in a process
// group of their own (killed whole on a timeout), in a folder of their own, with a clean
// environment that turns off any telemetry, and with their output capped.

const (
	maxStdout = 8 << 20
	maxStderr = 64 << 10
)

// command is a program to run.
type command struct {
	argv  []string
	dir   string // its working folder, also its HOME and TMPDIR
	stdin io.Reader
}

// env is everything a runtime sees of the environment: no keys, tokens or proxies of the
// server's. That is hygiene, not a sandbox: the program runs as the server's OS user and can
// read what the server can. Only pinned, sha256-checked runtimes and programs the operator
// installed are run.
func (c command) env() []string {
	return []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + c.dir,
		"TMPDIR=" + c.dir,
		"LANG=C.UTF-8",
		"CACTUS_TELEMETRY=0",
		"DO_NOT_TRACK=1",
	}
}

// capped keeps the first n bytes written to it and drops the rest.
type capped struct {
	buf bytes.Buffer
	n   int
	cut bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.n - c.buf.Len(); room < len(p) {
		c.cut = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}

// run runs c until it exits or ctx is done, and returns its standard output. Its error
// holds the end of the standard error, for the log: never show it to people.
func run(ctx context.Context, c command) ([]byte, error) {
	if !groupsSupported {
		return nil, errors.New("local engines need a Unix system")
	}
	cmd := exec.CommandContext(ctx, c.argv[0], c.argv[1:]...) //nolint:gosec // G204: a pinned runtime or the operator's add-on
	cmd.Dir, cmd.Env, cmd.Stdin = c.dir, c.env(), c.stdin
	stdout, stderr := &capped{n: maxStdout}, &capped{n: maxStderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	newGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(c.argv[0]), ctx.Err())
		}
		return nil, fmt.Errorf("%s: %w: %s", filepath.Base(c.argv[0]), err, lastLines(stderr.buf.String(), 3))
	}
	if stdout.cut {
		return nil, fmt.Errorf("%s: its output is over %d bytes", filepath.Base(c.argv[0]), maxStdout)
	}
	return stdout.buf.Bytes(), nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
