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

type LocalHost struct {
	Root  string
	Home  func(a cliowner.Account) (string, error)
	Found func(a cliowner.Account) (string, bool, error)
	Bins  map[Program]string
	Path  string
}

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

func (h *LocalHost) Paths(a cliowner.Account) (Paths, error) {
	if !a.Valid() {
		return Paths{}, errors.New("invalid account")
	}
	if h.Home == nil {
		return HomePaths(filepath.Join(h.Root, a.Identity)), nil
	}
	home, err := h.Home(a)
	if err != nil {
		return Paths{}, err
	}
	return HomePaths(home), nil
}

func (h *LocalHost) Bin(prog Program) string { return h.Bins[prog] }

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
	cmd := exec.CommandContext(context.Background(), argv[0], argv[1:]...)
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

func (h *LocalHost) DialVNC(ctx context.Context, a cliowner.Account) (io.ReadWriteCloser, error) {
	p, err := h.Paths(a)
	if err != nil {
		return nil, err
	}
	return DialRetry(ctx, p.VNCSocket())
}

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

func (h *LocalHost) Clean(a cliowner.Account) error {
	p, err := h.Paths(a)
	if err != nil {
		return err
	}
	return Clean(p)
}

func (h *LocalHost) Purge(a cliowner.Account) error {
	if a.Identity == "" {
		return nil
	}
	if h.Found != nil {
		home, ok, err := h.Found(a)
		if err != nil || !ok {
			return err
		}
		return Purge(HomePaths(home))
	}
	p, err := h.Paths(a)
	if err != nil {
		return err
	}
	return Purge(p)
}
