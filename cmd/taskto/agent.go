//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/config"
	"taskto.ai/taskto/internal/providers"
)

// agentDist is the sidecar's built entry point, relative to the repository or install.
var agentDist = filepath.Join("agent", "dist", "index.js")

// agentPaths returns where the agent runtime keeps its state: the agent/ directory in the
// data directory, holding its database and, in the owner-only run/, its socket. Neither is
// configured separately, so one database always has one socket: a sidecar a crashed server
// left behind is always on the socket the next sidecar probes before it opens the database.
func agentPaths(dataDir string) (dir, socketDir, db string) {
	dir = filepath.Join(dataDir, "agent")
	return dir, filepath.Join(dir, "run"), filepath.Join(dir, "agent.db")
}

// startAgent starts the agent runtime's supervisor without waiting for the sidecar to
// answer, and returns it with the feed that keeps its providers in step with the provider
// accounts; its stop stops both. It fails at once if the Node.js
// executable, the entry point or the providers file is missing, or the providers file is
// unusable, rather than leaving the supervisor to restart a sidecar that can never start.
//
// The sidecar reads the providers file the feed writes under <data>/agent/: the operator's
// providers (-agent-providers) first, then the accounts in the database. Its tools call
// back into the server through tools, served on a socket next to the sidecar's own. The
// variables in dropEnv are kept out of its environment.
func startAgent(ctx context.Context, cfg config.Config, store *providers.Store, tools http.Handler, cli []providers.CLIProvider, dropEnv []string) (agentRuntime, error) {
	node, err := exec.LookPath(cfg.AgentNode)
	if err != nil {
		return agentRuntime{}, fmt.Errorf("agent runtime: Node.js executable %q not found (set -agent-node, or turn the agent runtime off): %w", cfg.AgentNode, err)
	}
	entry, err := agentEntry(cfg.AgentEntry)
	if err != nil {
		return agentRuntime{}, err
	}
	if cfg.AgentProviders != "" {
		if err := regularFile(cfg.AgentProviders); err != nil {
			return agentRuntime{}, fmt.Errorf("agent runtime: providers file: %w", err)
		}
	}

	dir, socketDir, db := agentPaths(cfg.DataDir)
	var sup *agentrt.Supervisor
	feed, err := providers.NewFeed(store, providers.FeedConfig{
		Dir:          dir,
		OperatorFile: cfg.AgentProviders,
		Egress:       egressOf(cfg),
		CLI:          cli,
		Reload: func(ctx context.Context) error {
			_, err := sup.Client().ReloadProviders(ctx)
			if se := (*agentrt.StatusError)(nil); errors.As(err, &se) {
				return fmt.Errorf("%w: %w", providers.ErrRefused, err)
			}
			return err
		},
	})
	if err != nil {
		return agentRuntime{}, fmt.Errorf("agent runtime: %w", err)
	}
	sup, err = agentrt.NewSupervisor(agentrt.SupervisorConfig{
		Node:      node,
		Entry:     entry,
		SocketDir: socketDir,
		DB:        db,
		// Secrets the server read from its environment: the sidecar never sees them.
		DropEnv: dropEnv,
		// Always set, so the sidecar never picks up a different file from elsewhere. The
		// browser tools are offered only while the agent browser is on.
		Env: []string{"TASKTO_AGENT_PROVIDERS=" + feed.Path(), "TASKTO_AGENT_BROWSER=" + strconv.FormatBool(cfg.Browser)},
		// The channel the agent's tools call back into the server through.
		Tools: tools,
	})
	if errors.Is(err, agentrt.ErrSocketPathTooLong) {
		return agentRuntime{}, fmt.Errorf("agent runtime: %w; the socket lives in the data directory, so choose a shorter data directory", err)
	}
	if err != nil {
		return agentRuntime{}, err
	}
	// Before the sidecar starts: it reads the file once at startup, then on every reload.
	if err := feed.Write(ctx); err != nil {
		return agentRuntime{}, fmt.Errorf("agent runtime: providers: %w", err)
	}
	if err := sup.Launch(); err != nil {
		return agentRuntime{}, err
	}
	feedCtx, stopFeed := context.WithCancel(context.Background())
	feedDone := make(chan struct{})
	go func() {
		defer close(feedDone)
		feed.Run(feedCtx)
	}()
	slog.Info("agent runtime starting", "node", node, "entry", entry, "database", db, "socket", sup.Socket(), "providers", feed.Path())

	stop := func() error {
		stopFeed()
		<-feedDone
		// The sidecar gets its StopTimeout (15 s) after SIGTERM; then it is killed.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		err := sup.Stop(ctx)
		if err != nil {
			slog.Warn("agent runtime stopped late", "err", err)
		}
		if errors.Is(err, agentrt.ErrToolsBusy) {
			return err
		}
		return nil
	}
	return agentRuntime{state: sup.State, ready: sup.Ready, feed: feed, stop: stop}, nil
}

// agentEntry returns the sidecar's entry point: the configured one, or else agent/dist/index.js
// next to the binary, one directory above it (bin/taskto in a checkout), or in the working
// directory (go run in a checkout).
func agentEntry(configured string) (string, error) {
	if configured != "" {
		if err := regularFile(configured); err != nil {
			return "", fmt.Errorf("agent runtime: entry point: %w", err)
		}
		return configured, nil
	}
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		dir := filepath.Dir(exe)
		candidates = append(candidates, filepath.Join(dir, agentDist), filepath.Join(dir, "..", agentDist))
	}
	candidates = append(candidates, agentDist)
	for _, c := range candidates {
		if regularFile(c) == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("agent runtime: no entry point at %s: build it (pnpm --filter @taskto/agent build) or set -agent-entry",
		strings.Join(candidates, ", "))
}

// regularFile checks that path is an existing regular file (or a symlink to one).
func regularFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a file", path)
	}
	return nil
}
