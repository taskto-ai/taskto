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

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/config"
	"taskto.ai/taskto/internal/providers"
)

var agentDist = filepath.Join("agent", "dist", "index.js")

func agentPaths(dataDir string) (dir, socketDir, db string) {
	dir = filepath.Join(dataDir, "agent")
	return dir, filepath.Join(dir, "run"), filepath.Join(dir, "agent.db")
}

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
		DropEnv:   dropEnv,
		Env:       []string{"TASKTO_AGENT_PROVIDERS=" + feed.Path(), "TASKTO_AGENT_BROWSER=" + strconv.FormatBool(cfg.Browser)},
		Tools:     tools,
	})
	if errors.Is(err, agentrt.ErrSocketPathTooLong) {
		return agentRuntime{}, fmt.Errorf("agent runtime: %w; the socket lives in the data directory, so choose a shorter data directory", err)
	}
	if err != nil {
		return agentRuntime{}, err
	}
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
		ctx, cancel := context.WithTimeout(context.Background(), sidecarStopWait)
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
