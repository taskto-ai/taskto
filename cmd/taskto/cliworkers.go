package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"taskto.ai/taskto/internal/agenttools"
	"taskto.ai/taskto/internal/cliaccounts"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/config"
	"taskto.ai/taskto/internal/drain"
	"taskto.ai/taskto/internal/mcpserver"
	"taskto.ai/taskto/internal/serverkey"
)

// startCLIWorkers returns the CLI accounts service for cfg's -cli-workers mode, and a stop
// that ends every CLI turn and login. Off, the service only says so.
//
// In local mode the worker runs in this process: the CLIs on the PATH run as the server's
// own OS user, each taskto user in their own directories under <data>/cli/. That separates
// sessions and settings, not secrets, which is why the service refuses the CLIs as soon as
// a second user exists (unless the mode is local-unsafe).
func startCLIWorkers(ctx context.Context, cfg config.Config, sqlDB *sql.DB, key *serverkey.Key) (*cliaccounts.Service, func() error, error) {
	mode, err := cliaccounts.ParseMode(cfg.CLIWorkers)
	if err != nil {
		return nil, nil, err
	}
	// Before the worker is used: a purge must know that this kind of worker may hold people's files.
	if err := cliaccounts.RecordWorker(ctx, sqlDB, mode); err != nil {
		return nil, nil, fmt.Errorf("cli workers: recording the worker's kind: %w", err)
	}
	if mode == cliaccounts.Off {
		svc := cliaccounts.New(sqlDB, mode, nil)
		svc.SetShared(cliaccounts.Shared{Key: key, Egress: egressOf(cfg).Workspace})
		return svc, func() error { svc.Close(); return nil }, nil
	}
	if mode == cliaccounts.Container {
		// The worker is its own process, in its own container; the server only talks to it.
		svc := cliaccounts.New(sqlDB, mode, cliaccounts.RemoteWorker(cliworker.NewRemote(cfg.CLIWorkerSocket)))
		slog.Info("cli workers on, in the worker container", "socket", cfg.CLIWorkerSocket)
		stopProxy, err := startCLIProxy(ctx, cfg, svc, key, mode)
		if err != nil {
			svc.Close()
			return nil, nil, err
		}
		// The worker keeps its own record of the users it forgot, apart from the backup.
		svc.StartReconcile()
		return svc, func() error {
			err := stopProxy()
			svc.Close()
			return err
		}, nil
	}
	bins := map[cliworker.CLI]string{}
	for _, cli := range cliworker.CLIs {
		path, err := exec.LookPath(string(cli))
		if err != nil {
			continue
		}
		if abs, err := filepath.Abs(path); err == nil {
			bins[cli] = abs
		}
	}
	worker, err := cliworker.New(cliworker.Config{
		Layout: cliworker.Layout{Root: filepath.Join(cfg.DataDir, "cli")},
		Bins:   bins,
		Path:   os.Getenv("PATH"),
		// The directories are in the data directory, and a backup holds them with the database: a
		// directory from before homes is its number's account's.
		AdoptLegacy: true,
		// The server checked itself at start; this lets the helper run a CLI as root too.
		UnsafePrivileged: cfg.UnsafePrivilegedAgents,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("cli workers: %w", err)
	}
	found := make([]string, 0, len(bins))
	for _, cli := range cliworker.CLIs {
		if bins[cli] != "" {
			found = append(found, string(cli))
		}
	}
	slog.Info("cli workers on, in this process", "mode", mode, "clis", found)
	if mode == cliaccounts.LocalUnsafe {
		slog.Warn("cli workers in local-unsafe mode: every user's CLI runs as this server's OS user and can read other users' CLI logins and the server's data, server.key included; use this for development only")
	}
	svc := cliaccounts.New(sqlDB, mode, cliaccounts.LocalWorker(worker))
	if err := svc.Usable(ctx); err != nil && mode == cliaccounts.Local {
		slog.Warn("cli workers are refused while this server has more than one user (local mode has no OS boundary between users)")
	}
	stopProxy, err := startCLIProxy(ctx, cfg, svc, key, mode)
	if err != nil {
		svc.Close()
		worker.Close()
		return nil, nil, err
	}
	svc.StartReconcile()
	return svc, func() error {
		err := stopProxy()
		svc.Close()
		worker.Close()
		return err
	}, nil
}

// startCLIProxy starts the proxy of workspaces' shared CLI accounts on its own listener
// (docs/account-switching.md), and turns shared accounts on with its address. In local mode it
// listens on this machine, on a free port, unless told otherwise; in container mode only where
// -cli-proxy-listen says (the Compose file: a Unix socket the worker relays), or not at all.
func startCLIProxy(ctx context.Context, cfg config.Config, svc *cliaccounts.Service, key *serverkey.Key, mode cliaccounts.Mode) (func() error, error) {
	shared := cliaccounts.Shared{Key: key, Egress: egressOf(cfg).Workspace}
	listen := cfg.CLIProxyListen
	if listen == "" && mode != cliaccounts.Container {
		listen = "127.0.0.1:0"
	}
	if listen == "" {
		svc.SetShared(shared)
		slog.Info("no proxy for shared CLI accounts (-cli-proxy-listen, -cli-proxy-url): turns on them are refused")
		return func() error { return nil }, nil
	}
	// The proxy's handlers (and the per-turn tools endpoint beside it) use the database, so
	// stopping it waits for them, and the caller closes the database only after that.
	inflight := drain.New()
	srv := &http.Server{Handler: inflight.Handler(svc.ProxyHandler()), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	stop := func() error { return inflight.Shutdown(srv, proxyGrace, handlerWait) }
	if path, ok := strings.CutPrefix(listen, "unix:"); ok {
		// Config made sure of the URL: the worker's relay, as the CLIs reach it.
		shared.ProxyURL = cfg.CLIProxyURL
		svc.SetShared(shared)
		stopped := make(chan struct{})
		go serveProxySocket(ctx, srv, path, stopped)
		var once sync.Once
		var stopErr error
		return func() error {
			once.Do(func() {
				close(stopped)
				stopErr = stop()
			})
			return stopErr
		}, nil
	}
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("the shared CLI accounts' proxy: %w", err)
	}
	shared.ProxyURL = cfg.CLIProxyURL
	if shared.ProxyURL == "" {
		if mode == cliaccounts.Container {
			_ = ln.Close()
			return nil, errors.New("config: with -cli-workers container, -cli-proxy-listen needs -cli-proxy-url, the proxy's address as the worker reaches it")
		}
		addr := ln.Addr().(*net.TCPAddr)
		host := addr.IP.String()
		if addr.IP.IsUnspecified() {
			host = "127.0.0.1"
		}
		shared.ProxyURL = "http://" + net.JoinHostPort(host, strconv.Itoa(addr.Port))
	}
	svc.SetShared(shared)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("the shared CLI accounts' proxy stopped", "err", err)
		}
	}()
	slog.Info("proxy for shared CLI accounts listening", "addr", ln.Addr().String(), "url", shared.ProxyURL)
	return stop, nil
}

// serveProxySocket serves the proxy on a Unix socket at path once it can listen there. In
// container mode the worker makes the socket's directory, owned by this server's user, and
// relays the CLIs' connections to it: the worker's network has no route to this container
// (docs/cli-workers.md#the-workers-network). The worker may start after the server, so this
// waits for the directory; until then turns on shared accounts can't connect, as while the
// worker is down.
//
// Anything else that keeps it from listening (a file in the socket's place, a directory it
// can't write) is an error, logged again each minute while it lasts: the proxy is down.
func serveProxySocket(ctx context.Context, srv *http.Server, path string, stopped <-chan struct{}) {
	var last string
	var lastAt time.Time
	for {
		ln, err := listenProxySocket(ctx, path)
		if err == nil {
			slog.Info("proxy for shared CLI accounts listening", "socket", path)
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("the shared CLI accounts' proxy stopped", "err", err)
			}
			return
		}
		if err.Error() != last || time.Since(lastAt) >= time.Minute {
			if errors.Is(err, os.ErrNotExist) {
				slog.Info("the shared CLI accounts' proxy waits for the CLI worker to make its socket's directory", "socket", path)
			} else {
				slog.Error("the shared CLI accounts' proxy can't listen: turns on shared accounts fail until it can", "socket", path, "err", err)
			}
			last, lastAt = err.Error(), time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-stopped:
			return
		case <-time.After(time.Second):
		}
	}
}

// listenProxySocket listens on a Unix socket at path, replacing one left by an earlier run. Its
// mode is 666: the worker, root without CAP_DAC_OVERRIDE, connects to it as "other". What
// guards it is where it is, in a volume only the server's and the worker's containers mount;
// a CLI user in the worker who reaches it gets what the relay on 127.0.0.1 gives them anyway.
// The token is the proxy's only credential.
//
// The directory is this server's (the worker made it so); its mode is set again here, 711, so
// the worker can always pass through it, whatever a restored volume left.
func listenProxySocket(ctx context.Context, path string) (net.Listener, error) {
	if err := os.Chmod(filepath.Dir(path), 0o711); err != nil { //nolint:gosec // G302: passable, not listable
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode().Type() != os.ModeSocket {
			return nil, fmt.Errorf("%s exists and isn't a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	ln, err := new(net.ListenConfig).Listen(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil { //nolint:gosec // G302: see above
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// enableCLITools gives the CLIs of conversations' turns taskto's tools (docs/cli-tools.md): the
// MCP endpoint for turns on the proxy's listener, the browser's tools with it while the agent
// browser is on. Without the proxy's listener the CLIs can't reach it, and turns get none.
func enableCLITools(cfg config.Config, cli *cliaccounts.Service, tools *agenttools.Service) {
	if cli.Mode() == "off" {
		return
	}
	var readOnly []string
	for _, d := range agenttools.Definitions() {
		if !cfg.Browser && d.ConversationOnly {
			continue
		}
		// browser_help changes nothing but which tab is in front, and asks the user to act:
		// asking their permission first would ask them twice.
		if d.ReadOnly || d.Name == "browser_help" {
			readOnly = append(readOnly, d.Name)
		}
	}
	cli.SetTools(cliaccounts.Tools{
		Handler:  mcpserver.ForTurns(version, cli.ToolsCaller, tools, cfg.Browser),
		ReadOnly: readOnly,
	})
	if !cli.ToolsReachable() {
		slog.Warn("CLI turns get no taskto tools: the per-turn proxy has no listener (-cli-proxy-listen, -cli-proxy-url)")
	}
}

// toolsSocket is what the agent runtime's tools call back into: the knowledge-base, board and
// app tools, and, while CLI workers are on, the cli_turn tool's /cli/ routes. Both name their
// conversation with its capability, which tools checks.
func toolsSocket(tools *agenttools.Service, cli *cliaccounts.Service) http.Handler {
	if cli.Mode() == "off" {
		return tools
	}
	mux := http.NewServeMux()
	mux.Handle("/tools/", tools)
	mux.Handle("/cli/", cli.TurnRoutes(tools.Authorize))
	return mux
}
