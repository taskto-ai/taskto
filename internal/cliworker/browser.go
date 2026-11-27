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

func (l Layout) BrowserPaths(home int64) browser.Paths {
	return browser.HomePaths(l.Home(home).Root)
}

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

func (w *Worker) browserGate(acct account) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.admissible(acct, "", "")
}

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
	stdout, childOut, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = childOut
	stderr := &tailBuffer{max: 2 << 10}
	cmd.Stderr = stderr
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
		c.Close()
		return nil, ErrRemoving
	case <-ctx.Done():
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

type helperConn struct {
	cmd       *exec.Cmd
	w         io.WriteCloser
	out       *os.File
	r         *bufio.Reader
	once      sync.Once
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

func (h *browserHost) Purge(a cliowner.Account) error {
	if a.Identity == "" {
		return nil
	}
	r, ok, err := h.w.find(a.Identity)
	if err != nil || !ok || r.Home == 0 {
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

func (h *browserHost) quickOp(acct account, op, what string) error {
	return h.quickOpCtx(context.Background(), acct, op, what)
}

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
		return fmt.Errorf("%w: %w", ErrRemoving, <-done)
	case <-time.After(30 * time.Second):
		_ = signalGroup(cmd.Process.Pid, sigKill)
		return fmt.Errorf("%s took too long, and was stopped: %w", what, <-done)
	}
}

func runBrowserOp(req helperRequest, layout Layout, stdout io.Writer) error {
	p := layout.BrowserPaths(req.Home)
	switch req.Op {
	case "browser":
		if req.Browser == nil || !filepath.IsAbs(req.Bin) {
			return errors.New("invalid request")
		}
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
