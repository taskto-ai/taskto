package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"taskto.ai/taskto/internal/backup"
	"taskto.ai/taskto/internal/config"
)

// backupUsage and restoreUsage are the subcommands' synopses.
const (
	backupUsage  = "usage: taskto backup [-config file] [-data-dir dir] [-uncompressed] FILE|-"
	restoreUsage = "usage: taskto restore [-config file] [-data-dir dir] FILE|-"
)

// commandConfig parses a subcommand's flags, -config, -data-dir and any that extra adds,
// and returns the configuration they and the environment give, and the one argument, FILE
// or "-".
func commandConfig(name, usage string, args []string, getenv func(string) string, stderr io.Writer, extra func(*flag.FlagSet)) (config.Config, string, error) {
	fs := flag.NewFlagSet("taskto "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, usage)
		fs.PrintDefaults()
	}
	configFile := fs.String("config", "", "config file to read (env TASKTO_CONFIG)")
	dataDir := fs.String("data-dir", "", "the data directory (env TASKTO_DATA_DIR; default from the config)")
	if extra != nil {
		extra(fs)
	}
	if err := fs.Parse(args); err != nil {
		return config.Config{}, "", err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return config.Config{}, "", errors.New("want one FILE, or - for standard input or output")
	}
	cfg, err := serverConfig(*configFile, *dataDir, getenv, stderr)
	return cfg, fs.Arg(0), err
}

// serverConfig returns the configuration a subcommand's -config and -data-dir (either may be
// empty) and the environment give, by the server's own rules, so the subcommand finds the
// data directory the server uses.
func serverConfig(configFile, dataDir string, getenv func(string) string, stderr io.Writer) (config.Config, error) {
	var cfgArgs []string
	if configFile != "" {
		cfgArgs = append(cfgArgs, "-config", configFile)
	}
	if dataDir != "" {
		cfgArgs = append(cfgArgs, "-data-dir", dataDir)
	}
	return config.Load(cfgArgs, getenv, stderr)
}

// runBackup is `taskto backup`: it writes a snapshot of the data directory to FILE, or to
// standard output for "-". It works while the server runs.
func runBackup(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer, stderr io.Writer) error {
	var opts backup.Options
	cfg, out, err := commandConfig("backup", backupUsage, args, getenv, stderr, func(fs *flag.FlagSet) {
		fs.BoolVar(&opts.Uncompressed, "uncompressed", false,
			"write a plain tar, not a gzipped one: for restic, which compresses, encrypts and deduplicates it")
	})
	if err != nil {
		return err
	}

	var res backup.Result
	if out == "-" {
		if isTerminal(stdout) {
			return errors.New("standard output is a terminal: redirect it to a file (with docker compose exec, add -T)")
		}
		res, err = backup.Create(ctx, cfg.DataDir, version, stdout, opts)
		if err != nil {
			return err
		}
	} else {
		if res, err = backupToFile(ctx, cfg.DataDir, out, opts); err != nil {
			return err
		}
	}
	for _, p := range res.Skipped {
		fmt.Fprintf(stderr, "taskto backup: skipped %s: not a regular file or directory\n", p)
	}
	fmt.Fprintf(stderr, "taskto backup: %s: %d databases (%s) and %d other files\n",
		cfg.DataDir, len(res.Databases), strings.Join(res.Databases, ", "), res.Files)
	return nil
}

// backupToFile writes the backup to a temporary file next to out and renames it into
// place once it is complete, so a failed backup never leaves a file that looks whole.
func backupToFile(ctx context.Context, dataDir, out string, opts backup.Options) (backup.Result, error) {
	absData, err := filepath.Abs(dataDir)
	if err != nil {
		return backup.Result{}, err
	}
	absOut, err := filepath.Abs(out)
	if err != nil {
		return backup.Result{}, err
	}
	if rel, err := filepath.Rel(absData, absOut); err == nil && filepath.IsLocal(rel) {
		return backup.Result{}, fmt.Errorf("%s is inside the data directory: write the backup somewhere else", out)
	}
	if _, err := os.Lstat(absOut); err == nil { // #nosec G703 -- the operator names the backup file.
		return backup.Result{}, fmt.Errorf("%s already exists", out)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return backup.Result{}, err
	}

	// CreateTemp makes the file owner-only: a backup holds password hashes and keys.
	f, err := os.CreateTemp(filepath.Dir(absOut), ".taskto-backup-*")
	if err != nil {
		return backup.Result{}, err
	}
	defer os.Remove(f.Name()) // the temporary name goes once the backup is linked in place
	res, err := backup.Create(ctx, dataDir, version, f, opts)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return res, err
	}
	// A link, not a rename: it fails if the file exists by now (another backup to the same
	// path, say), where a rename would silently replace it.
	err = os.Link(f.Name(), absOut) // #nosec G703 -- the operator names the backup file.
	switch {
	case err == nil:
		return res, nil
	case errors.Is(err, fs.ErrExist):
		return res, fmt.Errorf("%s already exists", out)
	}
	// Some filesystems (network shares, FAT) have no hard links: create the file
	// exclusively instead, which can't replace one either, and copy the backup into it.
	if err := copyExclusive(f.Name(), absOut); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return res, fmt.Errorf("%s already exists", out)
		}
		return res, err
	}
	return res, nil
}

// copyExclusive copies the file src to dst, which must not exist yet. A failed copy
// removes what it wrote.
func copyExclusive(src, dst string) (err error) {
	in, err := os.Open(src) // #nosec G304 G703 -- the backup's own temporary file.
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 G703 -- the operator names the backup file.
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(dst) // #nosec G703 -- dst is the operator's backup path.
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// runRestore is `taskto restore`: it restores the backup in FILE, or standard input for
// "-", gzipped or plain, into the data directory, which must be empty or not exist. Stop
// the server first.
func runRestore(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stderr io.Writer) error {
	cfg, in, err := commandConfig("restore", restoreUsage, args, getenv, stderr, nil)
	if err != nil {
		return err
	}
	r := stdin
	if in != "-" {
		f, err := os.Open(in) // #nosec G304 G703 -- the operator names the backup to restore.
		if err != nil {
			return err
		}
		defer f.Close()
		r = f
	}
	m, err := backup.Restore(ctx, r, cfg.DataDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "taskto restore: restored the backup of %s (version %s) into %s\n",
		m.Created.Format("2006-01-02 15:04:05 UTC"), m.Version, cfg.DataDir)
	if m.Version != version {
		fmt.Fprintf(stderr, "taskto restore: this binary is version %s; on start it brings an older database up to date, and refuses one from a newer version\n", version)
	}
	return nil
}

// isTerminal reports whether w is a character device, such as a terminal, where an
// archive would be garbage.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	// /dev/null is a character device too, but writing a backup to it is a fine test.
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(info, null) {
		return false
	}
	return true
}
