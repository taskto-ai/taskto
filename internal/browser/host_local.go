package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"taskto.ai/taskto/internal/cliowner"
)

// LocalHost runs the browsers' processes as this process's own OS user: local mode, where
// there is no OS boundary between users (the CLI workers' one-user rule applies). Each account's
// browser directory is <its CLI home>/browser, Home saying where its home is (the local CLI
// worker's, cliworker.Worker.HomeDir); without Home, Root/<identity>/browser (tests).
type LocalHost struct {
	Root string
	Home func(a cliowner.Account) (string, error)
	Bins map[Program]string
	// Path is $PATH for the browser's processes.
	Path string
}

// FindBins looks for the programs on the PATH: chromium (or chromium-browser), Xvfb and
// x11vnc. chromium, if set, is the browser's executable instead.
func FindBins(chromium string) map[Program]string {
	bins := map[Program]string{}
	look := func(prog Program, names ...string) {
		for _, name := range names {
			if p, err := exec.LookPath(name); err == nil {
				if abs, err := filepath.Abs(p); err == nil {
					bins[prog] = abs
					return
				}
			}
		}
	}
	if chromium != "" {
		look(Chromium, chromium)
	} else {
		look(Chromium, "chromium", "chromium-browser")
	}
	look(Xvfb, "Xvfb")
	look(X11vnc, "x11vnc")
	return bins
}

// Paths is a's browser directory.
func (h *LocalHost) Paths(a cliowner.Account) (Paths, error) {
	if !a.Valid() {
		return Paths{}, errors.New("invalid account")
	}
	if h.Home == nil {
		return Paths{Root: filepath.Join(h.Root, a.Identity, "browser")}, nil
	}
	home, err := h.Home(a)
	if err != nil {
		return Paths{}, err
	}
	return Paths{Root: filepath.Join(home, "browser")}, nil
}

// Bin implements Host.
func (h *LocalHost) Bin(prog Program) string { return h.Bins[prog] }

// Start implements Host.
func (h *LocalHost) Start(a cliowner.Account, spec ProcSpec, extra []*os.File) (*exec.Cmd, error) {
	bin := h.Bins[spec.Program]
	if bin == "" {
		return nil, ErrUnavailable
	}
	p, err := h.Paths(a)
	if err != nil {
		return nil, err
	}
	if err := Prepare(p, spec); err != nil {
		return nil, err
	}
	path := h.Path
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	argv, env, err := Command(p, spec, bin, path)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec,noctx // G204: the configured browser programs; stopped as a group by the Manager
	cmd.Env = env
	cmd.Dir = p.Home()
	cmd.ExtraFiles = extra
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	NewGroup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// DialVNC implements Host. x11vnc makes its socket a moment after it starts, so a refused
// or missing socket is tried again for a few seconds.
func (h *LocalHost) DialVNC(ctx context.Context, a cliowner.Account) (io.ReadWriteCloser, error) {
	p, err := h.Paths(a)
	if err != nil {
		return nil, err
	}
	return DialRetry(ctx, p.VNCSocket())
}

// DialRetry connects to the Unix socket path, trying again for up to 10 s while it isn't
// there yet.
func DialRetry(ctx context.Context, path string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var d net.Dialer
	for {
		conn, err := d.DialContext(ctx, "unix", path)
		if err == nil {
			return conn, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("the display isn't served: %w", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Clean implements Host.
func (h *LocalHost) Clean(a cliowner.Account) error {
	p, err := h.Paths(a)
	if err != nil {
		return err
	}
	return Clean(p)
}

// Purge implements Host.
// A purge from before identities were kept names none: the CLI worker's step removes the number's
// directory from before homes, the browser's with it.
func (h *LocalHost) Purge(a cliowner.Account) error {
	if a.Identity == "" {
		return nil
	}
	p, err := h.Paths(a)
	if err != nil {
		return err
	}
	return Purge(p)
}
