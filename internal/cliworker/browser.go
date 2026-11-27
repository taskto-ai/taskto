package cliworker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"taskto.ai/taskto/internal/browser"
	"taskto.ai/taskto/internal/cliowner"
)

// The agent browser (docs/agent-browser.md) runs in the worker like a CLI turn: each process
// starts through the helper, which prepares the user's browser directory and execs the
// program as the user (their Unix user in container mode). The worker never opens a path in a
// home: even the live view's connection to the user's VNC socket is made by the helper, as
// the user, and bridged over its stdin and stdout.

// BrowserPaths is the browser directory of the account with home number home: <its home>/browser.
func (l Layout) BrowserPaths(home int64) browser.Paths {
	return browser.Paths{Root: filepath.Join(l.Home(home).Root, "browser")}
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

func (h *browserHost) Start(a cliowner.Account, spec browser.ProcSpec, extra []*os.File) (*exec.Cmd, error) {
	bin := h.bins[spec.Program]
	if bin == "" {
		return nil, browser.ErrUnavailable
	}
	acct, err := h.w.resolve(a, true)
	if err != nil {
		return nil, err
	}
	if err := h.w.browserGate(acct); err != nil {
		return nil, err
	}
	cmd, header, err := h.w.helperCommand(acct, helperRequest{Op: "browser", Bin: bin, Browser: &spec})
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
	go logLines(r, a.User, spec.Program)
	return cmd, nil
}

// browserGate refuses a browser's process while acct is retired or its home is being removed:
// the removal stops its browsers after it closed the gate, so a process that passed here before is
// one of those it stops (its session was registered first), and none starts after.
func (w *Worker) browserGate(acct account) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.admissible(acct, "", "")
}

// BrowserAllowed is the agent browsers' per-account rule in the worker (browser.Config.Allow):
// every operation on a's browsers, reading an open one's state and tabs too, is refused while the
// account is retired (a deleted account, or one of a history a restore went back from) and while
// its home is being removed. The refusal is browser.ErrAccountClosing, which the server's API
// knows.
func (w *Worker) BrowserAllowed(a cliowner.Account) error {
	if !a.Valid() {
		return browser.ErrAccountClosing
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.retired(a.Identity) || w.gated(a.Identity, "", "") {
		return browser.ErrAccountClosing
	}
	return nil
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
//
// The connection is a helper request of the user's from its first step to its close (admit): a
// removal of their directory that begins meanwhile cancels it, and waits until it has ended, so
// no helper of the dial's starts, or still runs, once their files go.
func (h *browserHost) DialVNC(ctx context.Context, a cliowner.Account) (io.ReadWriteCloser, error) {
	acct, err := h.w.resolve(a, false)
	if err != nil {
		return nil, err
	}
	held, release, err := h.w.admit(context.Background(), acct, "", "")
	if err != nil {
		return nil, err
	}
	if hook := h.w.dialHookForTest; hook != nil {
		hook()
	}
	c, err := h.dialVNC(ctx, held, acct)
	if err != nil {
		release()
		return nil, err
	}
	if hook := h.w.handOffHookForTest; hook != nil {
		hook()
	}
	// A removal's cancel closes the connection, which ends the request. The watch is published
	// under the connection's lock, so a cancel that fires at once waits for it.
	c.mu.Lock()
	c.release = release
	c.stopWatch = context.AfterFunc(held, func() { _ = c.Close() })
	c.mu.Unlock()
	return c, nil
}

func (h *browserHost) dialVNC(ctx, held context.Context, acct account) (*helperConn, error) {
	cmd, header, err := h.w.helperCommand(acct, helperRequest{Op: "browser-vnc"})
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
	// Nothing starts for a request a removal has cancelled.
	if err := held.Err(); err != nil {
		childOut.Close()
		stdout.Close()
		return nil, ErrRemoving
	}
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
				slog.Warn("browser: the live view's helper", "user", acct.user, "said", cut(s, 500))
			}
			return nil, err
		}
	case <-held.Done():
		// A removal of the user's directory began: the dial ends now.
		c.Close()
		return nil, ErrRemoving
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
	// release ends the helper request the connection is (DialVNC), and stopWatch its watch for a
	// removal's cancel; both are set under mu, and nil before the connection is handed out.
	mu        sync.Mutex
	release   func()
	stopWatch func() bool
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
		c.mu.Lock()
		stop, release := c.stopWatch, c.release
		c.mu.Unlock()
		if stop != nil {
			stop()
		}
		if release != nil {
			release()
		}
	})
	return nil
}

// Clean removes the account's testing profile, as its Unix user. It is a helper request of the
// account's (admit): a removal of its home that begins meanwhile cancels it and waits for it.
func (h *browserHost) Clean(a cliowner.Account) error {
	acct, err := h.w.resolve(a, false)
	if errors.Is(err, ErrUnknownAccount) {
		return nil
	}
	if err != nil {
		return err
	}
	held, release, err := h.w.admit(context.Background(), acct, "", "")
	if err != nil {
		return err
	}
	defer release()
	return h.quickOpCtx(held, acct, "browser-clean", "removing the testing profile")
}

// Purge removes a deleted account's browser directory, as its Unix user, under the account's gate
// (not beside a removal of its home): a directory with read-only parts too (browser.Purge). Only
// that account's home is touched. A purge from before identities were kept names none: the CLI
// worker's step removes the number's directory from before homes, the browser's with it.
func (h *browserHost) Purge(a cliowner.Account) error {
	if a.Identity == "" {
		return nil
	}
	r, ok, err := h.w.find(a.Identity)
	if err != nil || !ok {
		return err
	}
	end, err := h.w.beginGate(context.Background(), scope{id: a.Identity, all: true})
	if err != nil {
		return err
	}
	defer end()
	if _, err := os.Lstat(h.w.cfg.Layout.Home(r.Home).Root); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return h.quickOp(account{id: a.Identity, user: a.User, home: r.Home}, "browser-purge", "removing the browser's files")
}

// quickOp runs one of the helper's short browser ops as the user, stopping it after 30 s.
func (h *browserHost) quickOp(acct account, op, what string) error {
	return h.quickOpCtx(context.Background(), acct, op, what)
}

// quickOpCtx is quickOp, stopped at once when ctx ends (a removal cancelled the request).
func (h *browserHost) quickOpCtx(ctx context.Context, acct account, op, what string) error {
	cmd, header, err := h.w.helperCommand(acct, helperRequest{Op: op})
	if err != nil {
		return err
	}
	cmd.Stdin = strings.NewReader(string(header))
	stderr := &tailBuffer{max: 2 << 10}
	cmd.Stderr = stderr
	done := make(chan error, 1)
	if err := ctx.Err(); err != nil {
		return ErrRemoving
	}
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
	case <-ctx.Done():
		_ = signalGroup(cmd.Process.Pid, sigKill)
		<-done //nolint:errcheck // killed: its own error says nothing more
		return ErrRemoving
	case <-time.After(30 * time.Second):
		_ = signalGroup(cmd.Process.Pid, sigKill)
		return fmt.Errorf("%s took too long, and was stopped: %w", what, <-done)
	}
}

// runBrowserOp is the helper's side, as the user.
func runBrowserOp(req helperRequest, layout Layout, stdout io.Writer) error {
	p := layout.BrowserPaths(req.Home)
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
	case "browser-purge":
		if err := browser.Purge(p); err != nil {
			return err
		}
		_, err := io.WriteString(stdout, "{}\n")
		return err
	}
	return fmt.Errorf("unknown op %q", req.Op)
}
