//go:build unix

package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/config"
	"taskto.ai/taskto/internal/lockfile"
)

// A second server on the same data directory stops before it opens anything in it, and
// says which process holds it; the first keeps serving. Released on exit, the lock lets the
// next server start.
func TestRunRefusesADataDirectoryInUse(t *testing.T) {
	cfg := config.Default()
	cfg.Addr = "127.0.0.1:0"
	cfg.DataDir = t.TempDir()
	addr, stop := startRun(t, cfg)

	err := runRefused(cfg)
	want := fmt.Sprintf("in use by another taskto process (pid %d)", os.Getpid())
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("second run = %v, want %q", err, want)
	}
	if getHealthz(t, addr)["database"] != "ok" {
		t.Fatal("the first server stopped serving")
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	_, stop = startRun(t, cfg)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

// With the agent runtime on too, the refusal comes first: before the database is opened
// and migrated, and before the agent runtime's providers file is written.
func TestRunLocksBeforeAnythingElse(t *testing.T) {
	cfg := fakeAgentConfig(t)
	entry := cfg.AgentEntry // the fake's entry point, which lives in the data directory
	lock, err := lockfile.DataDir(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := runRefused(cfg); err == nil || !strings.Contains(err.Error(), "in use by another taskto process") {
		t.Fatalf("run on a locked data directory = %v, want refused", err)
	}
	entries, err := os.ReadDir(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != lockfile.DataDirName && e.Name() != strings.TrimPrefix(entry, cfg.DataDir+string(os.PathSeparator)) {
			t.Fatalf("a refused server created %s in the data directory", e.Name())
		}
	}
}

// runRefused runs a server that should be refused at once. If it starts instead, it is
// stopped after a few seconds and returns nil.
func runRefused(cfg config.Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return run(ctx, cfg, nil)
}
