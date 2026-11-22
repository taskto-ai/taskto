//go:build unix

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"taskto.ai/taskto/internal/browser"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/privcheck"
)

// runCLIWorker is `taskto cli-worker`: the CLI worker of container mode (docs/cli-workers.md),
// in a container of its own, as root. Each TaskTo user's CLIs run as a Unix user of their own;
// the server reaches the worker on a Unix socket only its user can open.
func runCLIWorker(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("cli-worker", flag.ContinueOnError)
	fs.SetOutput(stderr)
	socket := fs.String("socket", "/run/taskto-cli/worker.sock", "the socket the server connects to")
	owner := fs.String("socket-owner", "65532:65532", "uid:gid of the server, the only one who may open the socket")
	homes := fs.String("homes", "/homes", "where each user's directory is (a volume of its own)")
	uidBase := fs.Int("uid-base", 100000, "the first uid: TaskTo user N runs as this plus N")
	path := fs.String("path", os.Getenv("PATH"), "PATH for the CLIs and the commands they run")
	unsafe := fs.Bool(privcheck.OverrideFlag, false, "development only: start even if the image has setuid or setgid programs")
	browserIdle := fs.Duration("browser-idle", 5*time.Minute, "how long an agent browser runs unused before it stops")
	browserMax := fs.Int("browser-max", 4, "how many agent browsers may run at once")
	proxySocket := fs.String("proxy-socket", "", "the server's socket for the shared CLI accounts' proxy, relayed to -proxy-listen for the CLIs (empty: no relay)")
	proxyListen := fs.String("proxy-listen", "127.0.0.1:8081", "where the CLIs reach the proxy: the server's -cli-proxy-url")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := checkWorkerImage("/", *homes, *unsafe); err != nil {
		return err
	}
	uid, gid, err := parseOwner(*owner)
	if err != nil {
		return err
	}
	bins := map[cliworker.CLI]string{}
	for _, cli := range cliworker.CLIs {
		if p, err := exec.LookPath(string(cli)); err == nil {
			if abs, err := filepath.Abs(p); err == nil {
				bins[cli] = abs
			}
		}
	}
	w, err := cliworker.New(cliworker.Config{
		Layout:    cliworker.Layout{Root: *homes},
		Bins:      bins,
		Path:      *path,
		Isolation: &cliworker.Isolation{UIDBase: *uidBase},
	})
	if err != nil {
		return err
	}
	defer w.Close()

	ln, err := listenOwnerOnly(*socket, uid, gid)
	if err != nil {
		return err
	}
	if *proxySocket != "" {
		// The worker's network reaches the internet only, not the server
		// (docs/cli-workers.md#the-workers-network), so the proxy comes over the socket volume.
		if err := prepareProxyDir(filepath.Dir(*proxySocket), uid, gid); err != nil {
			return fmt.Errorf("the proxy's socket directory: %w", err)
		}
		relay, err := new(net.ListenConfig).Listen(context.Background(), "tcp", *proxyListen)
		if err != nil {
			return fmt.Errorf("the proxy's relay: %w", err)
		}
		defer relay.Close()
		go relayProxy(relay, *proxySocket)
		slog.Info("cli worker relays the shared accounts' proxy", "listen", relay.Addr().String(), "socket", *proxySocket)
	}
	// The agent browsers (docs/agent-browser.md), when the image has them: each user's as their
	// own Unix user, through the helper. Chromium's sandbox can't start in this unprivileged
	// container, so it is off; the OS boundary is the users'.
	browsers := browser.NewManager(browser.Config{
		Host:      w.BrowserHost(browser.FindBins("")),
		Idle:      *browserIdle,
		Max:       *browserMax,
		NoSandbox: true,
	})
	defer browsers.Close()
	mux := http.NewServeMux()
	mux.Handle("/browser/", browser.NewHandler(browsers))
	mux.Handle("/", w.Handler())
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	found := make([]string, 0, len(bins))
	for _, cli := range cliworker.CLIs {
		if bins[cli] != "" {
			found = append(found, string(cli))
		}
	}
	slog.Info("cli worker listening", "socket", *socket, "homes", *homes, "clis", found, "uid_base", *uidBase,
		"browser", browsers.Available(browser.Personal), "testing_browser", browsers.Available(browser.Testing))
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	return nil
}

// checkWorkerImage refuses an image (the file system at root) with a setuid or setgid
// program: a CLI user must have nothing to escalate with (docs/agent-safety.md). The users'
// homes are theirs, not the image's, and other file systems (/proc, volumes) aren't looked at.
func checkWorkerImage(root, homes string, override bool) error {
	found, unreadable, more, err := privcheck.SetuidFiles(root, 10, homes, filepath.Join(root, "proc"), filepath.Join(root, "sys"), filepath.Join(root, "dev"))
	if err != nil {
		return fmt.Errorf("checking the image for setuid programs: %w", err)
	}
	var problems []string
	if len(found) > 0 {
		list := strings.Join(found, ", ")
		if more {
			list += " and more"
		}
		problems = append(problems, "the image has setuid or setgid programs ("+list+"); remove the bits, as the Dockerfile does with chmod a-s")
	}
	// The worker keeps only four capabilities, so a directory of another system user's (apt's
	// cache, say) can be closed to it. The image's own build cleared every setuid bit as full
	// root, so this only says what it couldn't look at again.
	if len(unreadable) > 0 {
		slog.Warn("cli worker: couldn't check these directories for setuid programs", "dirs", unreadable)
	}
	return privcheck.Enforce("the CLI worker", problems, override)
}

func parseOwner(s string) (int, int, error) {
	u, g, ok := strings.Cut(s, ":")
	uid, err1 := strconv.Atoi(u)
	gid, err2 := strconv.Atoi(g)
	if !ok || err1 != nil || err2 != nil || uid < 0 || gid < 0 {
		return 0, 0, fmt.Errorf("-socket-owner must be uid:gid")
	}
	return uid, gid, nil
}

// listenOwnerOnly listens on a Unix socket at path that only uid can open: mode 600, owned by
// uid:gid, in a directory the worker makes passable (711) but not listable.
func listenOwnerOnly(path string, uid, gid int) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o711); err != nil { //nolint:gosec // G301: passable, not listable; the socket is 600
		return nil, err
	}
	if err := os.Chmod(dir, 0o711); err != nil { //nolint:gosec // G302: as above
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode().Type() != os.ModeSocket {
			return nil, fmt.Errorf("%s exists and isn't a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	old := syscall.Umask(0o177)
	ln, err := new(net.ListenConfig).Listen(context.Background(), "unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	// Mode first, then owner: once the socket is the server's, root without CAP_FOWNER (the
	// worker keeps only SETUID, SETGID, CHOWN and KILL) can no longer change its mode.
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	if err := os.Chown(path, uid, gid); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// prepareProxyDir makes dir, where the server listens for the proxy (-cli-proxy-listen
// unix:…), the server's: uid:gid, mode 711, so the worker can pass through it to the socket
// but not list it. The volume's root is the worker's (711), so the server can't make it.
func prepareProxyDir(dir string, uid, gid int) error {
	if err := os.Mkdir(dir, 0o711); err != nil && !errors.Is(err, os.ErrExist) { //nolint:gosec // G301: passable, not listable
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	switch {
	case !info.IsDir() || !ok:
		return fmt.Errorf("%s isn't a directory", dir)
	case int(st.Uid) == uid:
		return nil
	case st.Uid != 0:
		return fmt.Errorf("%s belongs to uid %d, not root or the server's %d", dir, st.Uid, uid)
	}
	// Mode first, then owner: once it is the server's, root without CAP_FOWNER can't chmod it.
	if err := os.Chmod(dir, 0o711); err != nil { //nolint:gosec // G302: as above
		return err
	}
	return os.Chown(dir, uid, gid)
}

// relayProxy joins each connection to ln to the server's proxy socket, until ln is closed.
// It reads nothing: the CLI's requests and the proxy's answers pass as they are.
func relayProxy(ln net.Listener, socket string) {
	for {
		c, err := ln.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			slog.Warn("cli worker: the proxy's relay", "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go relayConn(c, socket)
	}
}

func relayConn(c net.Conn, socket string) {
	defer c.Close()
	d := net.Dialer{Timeout: 5 * time.Second}
	s, err := d.Dial("unix", socket)
	if err != nil {
		slog.Warn("cli worker: the server's proxy isn't there", "socket", socket, "err", err)
		return
	}
	defer s.Close()
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(s, c)
	go pipe(c, s)
	<-done
	<-done
}
