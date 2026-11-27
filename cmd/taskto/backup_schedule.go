package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"time"

	"taskto.ai/taskto/internal/backup"
)

const backupScheduleUsage = "usage: taskto backup-schedule [-config file] [-data-dir dir] [-at HH:MM] [-retries n] [-retry-every duration] [-timeout duration] [-check-timeout duration] [-restic path] [-now]"

const (
	defaultBackupAt           = "03:15"
	defaultBackupRetries      = 6
	defaultBackupRetryEvery   = time.Hour
	defaultBackupTimeout      = 4 * time.Hour
	defaultBackupCheckTimeout = 2 * time.Minute
)

func runBackupSchedule(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer) error {
	at, retries, every, timeout := defaultBackupAt, defaultBackupRetries, defaultBackupRetryEvery, defaultBackupTimeout
	if v := getenv("TASKTO_BACKUP_AT"); v != "" {
		at = v
	}
	if v := getenv("TASKTO_BACKUP_RETRIES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("TASKTO_BACKUP_RETRIES=%q isn't a number", v)
		}
		retries = n
	}
	for name, d := range map[string]*time.Duration{"TASKTO_BACKUP_RETRY_EVERY": &every, "TASKTO_BACKUP_TIMEOUT": &timeout} {
		if v := getenv(name); v != "" {
			parsed, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("%s=%q isn't a duration such as 1h or 30m", name, v)
			}
			*d = parsed
		}
	}

	fs := flag.NewFlagSet("taskto backup-schedule", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, backupScheduleUsage)
		fs.PrintDefaults()
	}
	configFile := fs.String("config", "", "config file to read (env TASKTO_CONFIG)")
	dataDir := fs.String("data-dir", "", "the data directory (env TASKTO_DATA_DIR; default from the config)")
	fs.StringVar(&at, "at", at, "the nightly backup's time, HH:MM in UTC (env TASKTO_BACKUP_AT)")
	fs.IntVar(&retries, "retries", retries, "how many times a failed night is tried again (env TASKTO_BACKUP_RETRIES)")
	fs.DurationVar(&every, "retry-every", every, "the time between a failed backup and its retry (env TASKTO_BACKUP_RETRY_EVERY)")
	fs.DurationVar(&timeout, "timeout", timeout, "how long one backup may take before it is stopped (env TASKTO_BACKUP_TIMEOUT)")
	checkTimeout := fs.Duration("check-timeout", defaultBackupCheckTimeout, "how long the repository has to answer before a backup, which is then not started")
	restic := fs.String("restic", "restic", "the restic program")
	now := fs.Bool("now", false, "back up once, now, and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return errors.New("no arguments besides the flags")
	}
	atDur, err := backup.ParseAt(at)
	if err != nil {
		return err
	}
	cfg, err := serverConfig(*configFile, *dataDir, getenv, stderr)
	if err != nil {
		return err
	}
	if info, err := os.Stat(cfg.DataDir); err != nil || !info.IsDir() {
		return fmt.Errorf("data directory %s isn't there: start the server on it first", cfg.DataDir)
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(stderr, nil))
	s := &backup.Scheduler{
		DataDir: cfg.DataDir,
		Schedule: backup.Schedule{
			At: atDur, Retries: retries, RetryEvery: every,
			Timeout: timeout, CheckTimeout: *checkTimeout,
		},
		Runner: backup.Restic{Path: *restic, Taskto: self, DataDir: cfg.DataDir, Log: stderr},
		Log:    log,
	}
	if err := s.Schedule.Validate(); err != nil {
		return err
	}
	if !*now {
		log.Info("backup schedule: started", "at", at+" UTC", "retries", retries, "retry_every", every, "data_dir", cfg.DataDir)
		return s.Run(ctx)
	}
	st, err := s.Attempt(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "taskto backup-schedule: saved snapshot %s; the next backup is at %s\n",
		st.LastSnapshot, st.NextAttempt.Format("2006-01-02 15:04 UTC"))
	return nil
}
