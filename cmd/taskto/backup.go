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

const (
	backupUsage  = "usage: taskto backup [-config file] [-data-dir dir] [-uncompressed] FILE|-"
	restoreUsage = "usage: taskto restore [-config file] [-data-dir dir] FILE|-"
)

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
	outDir, name := filepath.Dir(absOut), filepath.Base(absOut)
	dir, err := os.OpenRoot(outDir)
	if err != nil {
		return backup.Result{}, err
	}
	defer dir.Close()
	if _, err := dir.Lstat(name); err == nil {
		return backup.Result{}, fmt.Errorf("%s already exists", out)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return backup.Result{}, err
	}

	f, err := os.CreateTemp(outDir, ".taskto-backup-*")
	if err != nil {
		return backup.Result{}, err
	}
	tmp := filepath.Base(f.Name())
	defer func() { _ = dir.Remove(tmp) }()
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
	err = dir.Link(tmp, name)
	switch {
	case err == nil:
		return res, nil
	case errors.Is(err, fs.ErrExist):
		return res, fmt.Errorf("%s already exists", out)
	}
	if err := copyExclusive(dir, tmp, name); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return res, fmt.Errorf("%s already exists", out)
		}
		return res, err
	}
	return res, nil
}

func copyExclusive(dir *os.Root, src, dst string) (err error) {
	in, err := dir.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := dir.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = dir.Remove(dst)
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func runRestore(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stderr io.Writer) error {
	cfg, in, err := commandConfig("restore", restoreUsage, args, getenv, stderr, nil)
	if err != nil {
		return err
	}
	r := stdin
	if in != "-" {
		f, err := os.Open(in)
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

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(info, null) {
		return false
	}
	return true
}
