//go:build unix

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/conversations"
	"taskto.ai/taskto/internal/db"
)

const conversationsUsage = `usage: taskto conversations [-config file] [-data-dir dir] check
       taskto conversations [-config file] [-data-dir dir] [-lost [-all]] [-orphans] clean`

func runConversations(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("taskto conversations", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, conversationsUsage)
		fs.PrintDefaults()
	}
	configFile := fs.String("config", "", "config file to read (env TASKTO_CONFIG)")
	dataDir := fs.String("data-dir", "", "the data directory (env TASKTO_DATA_DIR; default from the config)")
	lost := fs.Bool("lost", false, "clean: delete the conversations the agent runtime no longer has")
	orphans := fs.Bool("orphans", false, "clean: retire the agent runtime's conversations the server never recorded")
	all := fs.Bool("all", false, "clean -lost: also when the agent runtime has none of the server's conversations")
	if err := fs.Parse(args); err != nil {
		return err
	}
	action := fs.Arg(0)
	switch {
	case fs.NArg() != 1 || (action != "check" && action != "clean"):
		fs.Usage()
		return errors.New("want check or clean")
	case action == "clean" && !*lost && !*orphans:
		return errors.New("clean what? add -lost, -orphans or both")
	case action == "check" && (*lost || *orphans || *all):
		return errors.New("-lost, -orphans and -all are for clean")
	case *all && !*lost:
		return errors.New("-all goes with -lost")
	}
	cfg, err := serverConfig(*configFile, *dataDir, getenv, stderr)
	if err != nil {
		return err
	}

	dbPath := filepath.Join(cfg.DataDir, db.FileName)
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("no database in %s: %w", cfg.DataDir, err)
	}
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	_, socketDir, _ := agentPaths(cfg.DataDir)
	socket := agentrt.SocketPath(socketDir)
	rt := agentrt.New(socket)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	healthCtx, healthCancel := context.WithTimeout(ctx, 5*time.Second)
	err = rt.Healthy(healthCtx)
	healthCancel()
	if err != nil {
		return fmt.Errorf("the agent runtime doesn't answer on %s: run this while the server runs with -agent: %w", socket, err)
	}

	drift, err := conversations.CheckDrift(ctx, sqlDB, rt, time.Now())
	if err != nil {
		return err
	}
	if action == "check" {
		printDrift(stdout, drift)
		return nil
	}
	if *lost && drift.AllLost() && !*all {
		return fmt.Errorf("the agent runtime has none of the server's %d conversations: if its data really was "+
			"wiped, run clean -lost -all to delete them all; otherwise check the agent runtime first", drift.Recorded)
	}
	if *lost {
		n, err := conversations.RemoveLost(ctx, sqlDB, drift.Lost)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "deleted %d lost conversations\n", n)
	}
	if *orphans {
		n, err := conversations.RetireOrphans(ctx, sqlDB, rt, drift.Orphans)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "retired %d orphaned agent runtime conversations\n", n)
	}
	return nil
}

func printDrift(w io.Writer, d conversations.Drift) {
	fmt.Fprintf(w, "lost: %d conversations the agent runtime no longer has (clean -lost deletes them)\n", len(d.Lost))
	for _, l := range d.Lost {
		fmt.Fprintf(w, "  conversation %d in workspace %d, owner %d, created %s (agent runtime conversation %d)\n",
			l.ID, l.WorkspaceID, l.OwnerID, l.CreatedAt, l.AgentID)
	}
	fmt.Fprintf(w, "orphaned: %d agent runtime conversations the server never recorded (clean -orphans retires them)\n", len(d.Orphans))
	for _, o := range d.Orphans {
		created := "before creation times were kept"
		if !o.CreatedAt.IsZero() {
			created = o.CreatedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(w, "  agent runtime conversation %d, created %s\n", o.AgentID, created)
	}
}
