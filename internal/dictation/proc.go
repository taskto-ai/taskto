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

const (
	maxStdout = 8 << 20
	maxStderr = 64 << 10
)

type command struct {
	argv  []string
	dir   string
	stdin io.Reader
}

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

func run(ctx context.Context, c command) ([]byte, error) {
	if !groupsSupported {
		return nil, errors.New("local engines need a Unix system")
	}
	cmd := exec.CommandContext(ctx, c.argv[0], c.argv[1:]...)
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
