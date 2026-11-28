package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/lockfile"
)

const agentCompactUsage = "usage: taskto agent-compact [-config file] [-data-dir dir]"

func runAgentCompact(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer) error {
	fs := flag.NewFlagSet("taskto agent-compact", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, agentCompactUsage)
		fs.PrintDefaults()
	}
	configFile := fs.String("config", "", "config file to read (env TASKTO_CONFIG)")
	dataDir := fs.String("data-dir", "", "the data directory (env TASKTO_DATA_DIR; default from the config)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return errors.New("takes no arguments")
	}
	cfg, err := serverConfig(*configFile, *dataDir, getenv, stderr)
	if err != nil {
		return err
	}
	lock, err := lockfile.DataDir(cfg.DataDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	_, _, db := agentPaths(cfg.DataDir)
	if err := agentrt.Compact(ctx, db, time.Now()); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "taskto agent-compact: compacted %s, emptied its write-ahead log, and recorded it\n", db)
	return nil
}
