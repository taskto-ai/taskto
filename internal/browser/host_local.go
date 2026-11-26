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
	"strconv"
	"time"
)

// LocalHost runs the browsers' processes as this process's own OS user: local mode, where
// there is no OS boundary between users (the CLI workers' one-user rule applies). Each user's
// browser directory is Root/u<id>/browser.
type LocalHost struct {
	Root string
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

// Paths is user's browser directory.
func (h *LocalHost) Paths(user int64) Paths {
	return Paths{Root: filepath.Join(h.Root, "u"+strconv.FormatInt(user, 10), "browser")}
}

// Bin implements Host.
func (h *LocalHost) Bin(prog Program) string { return h.Bins[prog] }

// Start implements Host.
func (h *LocalHost) Start(user int64, spec ProcSpec, extra []*os.File) (*exec.Cmd, error) {
	if user <= 0 {
		return nil, errors.New("invalid user")
	}
	bin := h.Bins[spec.Program]
	if bin == "" {
		return nil, ErrUnavailable
	}
	p := h.Paths(user)
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
func (h *LocalHost) DialVNC(ctx context.Context, user int64) (io.ReadWriteCloser, error) {
	return DialRetry(ctx, h.Paths(user).VNCSocket())
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
func (h *LocalHost) Clean(user int64) error { return Clean(h.Paths(user)) }

// Purge implements Host.
func (h *LocalHost) Purge(user int64) error {
	if user <= 0 {
		return errors.New("invalid user")
	}
	return Purge(h.Paths(user))
}
