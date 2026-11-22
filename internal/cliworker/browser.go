package cliworker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"taskto.ai/taskto/internal/browser"
)

// The agent browser (docs/agent-browser.md) runs in the worker like a CLI turn: each process
// starts through the helper, which prepares the user's browser directory and execs the
// program as the user (their Unix user in container mode). The worker never opens a path in a
// home: even the live view's connection to the user's VNC socket is made by the helper, as
// the user, and bridged over its stdin and stdout.

// BrowserPaths is user's browser directory: <their directory>/browser.
func (l Layout) BrowserPaths(user int64) browser.Paths {
	return browser.Paths{Root: filepath.Join(l.User(user).Root, "browser")}
}

// BrowserHost returns the browser.Host that runs the browsers' programs (bins) through the
// helper, as each user's own Unix user when the worker isolates users.
func (w *Worker) BrowserHost(bins map[browser.Program]string) browser.Host {
	return &browserHost{w: w, bins: bins}
}

type browserHost struct {
	w    *Worker
	bins map[browser.Program]string
}

func (h *browserHost) Bin(prog browser.Program) string { return h.bins[prog] }

func (h *browserHost) Start(user int64, spec browser.ProcSpec, extra []*os.File) (*exec.Cmd, error) {
	bin := h.bins[spec.Program]
	if bin == "" {
		return nil, browser.ErrUnavailable
	}
	cmd, header, err := h.w.helperCommand(user, helperRequest{Op: "browser", Bin: bin, Browser: &spec})
	if err != nil {
		return nil, err
	}
	cmd.Stdin = strings.NewReader(string(header))
	cmd.ExtraFiles = extra
	// stderr is a pipe of ours, not a Writer: cmd.Wait then doesn't wait for every process
	// that inherited it. What the programs say goes to the debug log; the helper's own
	// refusals to the warnings.
	r, wr, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = wr
	err = cmd.Start()
	wr.Close()
	if err != nil {
		r.Close()
		return nil, err
	}
	go logLines(r, user, spec.Program)
	return cmd, nil
}

// logLines logs what a browser's process writes on stderr, a line at a time.
func logLines(r io.ReadCloser, user int64, prog browser.Program) {
	defer r.Close()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4096), 64<<10)
	for sc.Scan() {
		line := cut(sc.Text(), 500)
		if strings.HasPrefix(line, "cli-helper:") {
			slog.Warn("browser: the helper refused", "user", user, "program", prog, "said", line)
		} else {
			slog.Debug("browser: "+string(prog), "user", user, "said", line)
		}
	}
}

// DialVNC starts the helper as the user to connect to their VNC socket, and returns its
// stdin and stdout as the stream. The helper says "ok" once it is connected.
func (h *browserHost) DialVNC(ctx context.Context, user int64) (io.ReadWriteCloser, error) {
	cmd, header, err := h.w.helperCommand(user, helperRequest{Op: "browser-vnc"})
	if err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// A pipe of our own, not StdoutPipe: Wait closes that one, and the live view may still be
	// reading it when the stream is closed. This one is closed by Close, after Wait.
	stdout, childOut, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = childOut
	stderr := &tailBuffer{max: 2 << 10}
	cmd.Stderr = stderr
	err = cmd.Start()
	childOut.Close()
	if err != nil {
		stdout.Close()
		return nil, err
	}
	c := &helperConn{cmd: cmd, w: stdin, out: stdout, r: bufio.NewReader(stdout)}
	if _, err := stdin.Write(header); err != nil {
		c.Close()
		return nil, err
	}
	status := make(chan error, 1)
	go func() {
		line, err := c.r.ReadString('\n')
		switch {
		case err != nil:
			status <- errors.New("the display isn't served")
		case line != "ok\n":
			status <- errors.New(strings.TrimSpace(line))
		default:
			status <- nil
		}
	}()
	select {
	case err := <-status:
		if err != nil {
			c.Close()
			if s := strings.TrimSpace(stderr.String()); s != "" {
				slog.Warn("browser: the live view's helper", "user", user, "said", cut(s, 500))
			}
			return nil, err
		}
	case <-ctx.Done():
		// The helper may have answered just as ctx ended: its answer wins.
		select {
		case err := <-status:
			if err != nil {
				c.Close()
				return nil, err
			}
		default:
			c.Close()
			return nil, ctx.Err()
		}
	}
	return c, nil
}

// helperConn is a running helper's stdout and stdin as one stream. Closing it ends the helper.
type helperConn struct {
	cmd  *exec.Cmd
	w    io.WriteCloser
	out  *os.File
	r    *bufio.Reader
	once sync.Once
}

func (c *helperConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *helperConn) Write(p []byte) (int, error) { return c.w.Write(p) }

func (c *helperConn) Close() error {
	c.once.Do(func() {
		_ = c.w.Close()
		_ = signalGroup(c.cmd.Process.Pid, sigKill)
		_ = c.cmd.Wait()
		// A read still waiting ends now, with an error: the stream is over either way.
		_ = c.out.Close()
	})
	return nil
}

func (h *browserHost) Clean(user int64) error {
	cmd, header, err := h.w.helperCommand(user, helperRequest{Op: "browser-clean"})
	if err != nil {
		return err
	}
	cmd.Stdin = strings.NewReader(string(header))
	stderr := &tailBuffer{max: 2 << 10}
	cmd.Stderr = stderr
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return nil
	case <-time.After(30 * time.Second):
		_ = signalGroup(cmd.Process.Pid, sigKill)
		return fmt.Errorf("removing the testing profile took too long, and was stopped: %w", <-done)
	}
}

// runBrowserOp is the helper's side, as the user.
func runBrowserOp(req helperRequest, layout Layout, stdout io.Writer) error {
	p := layout.BrowserPaths(req.User)
	switch req.Op {
	case "browser":
		if req.Browser == nil || !filepath.IsAbs(req.Bin) {
			return errors.New("invalid request")
		}
		// Owner-only for whatever the programs make (x11vnc's socket among them).
		setUmask(0o077)
		if err := browser.Prepare(p, *req.Browser); err != nil {
			return err
		}
		argv, env, err := browser.Command(p, *req.Browser, req.Bin, req.Path)
		if err != nil {
			return err
		}
		if err := closeStdin(); err != nil {
			return err
		}
		if err := os.Chdir(p.Home()); err != nil {
			return err
		}
		if err := unprivileged(req); err != nil {
			return err
		}
		// fds 3 and up (the DevTools pipe, Xvfb's -displayfd) were the worker's ExtraFiles:
		// they are not close-on-exec, so the program gets them.
		return execve(argv[0], argv, env)
	case "browser-vnc":
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		conn, err := browser.DialRetry(ctx, p.VNCSocket())
		cancel()
		if err != nil {
			_, _ = io.WriteString(stdout, "the display isn't served\n")
			return err
		}
		defer conn.Close()
		if _, err := io.WriteString(stdout, "ok\n"); err != nil {
			return err
		}
		browser.Bridge(conn, struct {
			io.Reader
			io.Writer
		}{os.Stdin, stdout})
		return nil
	case "browser-clean":
		if err := browser.Clean(p); err != nil {
			return err
		}
		_, err := io.WriteString(stdout, "{}\n")
		return err
	}
	return fmt.Errorf("unknown op %q", req.Op)
}
