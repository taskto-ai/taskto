package backup

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"taskto.ai/taskto/internal/db"
	"taskto.ai/taskto/internal/lockfile"
)

const (
	manifestName = "taskto-backup.json"
	endName      = "taskto-backup-end.json"
)

const tmpPrefix = ".backup-"

var derived = []string{"agent/providers.json", "agent/keys"}

const cacheDir = "cache"

const format = 2

var gzipMagic = []byte{0x1f, 0x8b}

var sqliteHeader = []byte("SQLite format 3\x00")

type Manifest struct {
	Format  int       `json:"format"`
	Version string    `json:"version"`
	Created time.Time `json:"created"`
}

type end struct {
	Entries int `json:"entries"`
}

type Options struct {
	Uncompressed bool
}

type Result struct {
	Manifest
	Databases []string
	Files     int
	Skipped   []string
}

func Create(ctx context.Context, dataDir, version string, w io.Writer, opts Options) (Result, error) {
	res := Result{Manifest: Manifest{Format: format, Version: version, Created: time.Now().UTC().Truncate(time.Second)}}
	info, err := os.Stat(dataDir)
	if err != nil {
		return res, fmt.Errorf("data directory: %w", err)
	}
	if !info.IsDir() {
		return res, fmt.Errorf("data directory: %s is not a directory", dataDir)
	}
	_ = CleanStale(dataDir)
	tmp, err := os.MkdirTemp(dataDir, tmpPrefix)
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(tmp)
	lock, err := lockfile.Lock(filepath.Join(tmp, lockName))
	if err != nil {
		return res, err
	}
	defer lock.Close()

	var gz io.WriteCloser = nopCloser{w}
	if !opts.Uncompressed {
		gz = gzip.NewWriter(w)
	}
	tw := tar.NewWriter(gz)
	entries := 0
	manifest, err := json.MarshalIndent(res.Manifest, "", "  ")
	if err != nil {
		return res, err
	}
	if err := tw.WriteHeader(&tar.Header{Name: manifestName, Mode: 0o600, Size: int64(len(manifest)), ModTime: res.Created, Typeflag: tar.TypeReg}); err != nil {
		return res, err
	}
	if _, err := tw.Write(manifest); err != nil {
		return res, err
	}

	err = filepath.WalkDir(dataDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(dataDir, p)
		if err != nil || rel == "." {
			return err
		}
		name := filepath.ToSlash(rel)
		if d.IsDir() && path.Dir(name) == "." && strings.HasPrefix(name, tmpPrefix) {
			return filepath.SkipDir
		}
		if name == StatusDir && d.IsDir() {
			return filepath.SkipDir
		}
		if name == manifestName || name == endName {
			res.Skipped = append(res.Skipped, name)
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if name == lockfile.DataDirName || name == cacheDir || slices.Contains(derived, name) || (path.Dir(name) == "agent" && strings.HasPrefix(path.Base(name), ".tmp-")) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		switch t := d.Type(); {
		case t.IsDir():
			entries++
			return tw.WriteHeader(&tar.Header{Name: name + "/", Mode: 0o700, Typeflag: tar.TypeDir, ModTime: modTime(d)})
		case t&fs.ModeSocket != 0:
			return nil
		case !t.IsRegular():
			res.Skipped = append(res.Skipped, name)
			return nil
		}
		if journal(p) {
			return nil
		}
		isDB, err := isSQLite(p)
		if err != nil {
			return err
		}
		if isDB {
			snap := filepath.Join(tmp, "snapshot.db")
			if err := snapshot(ctx, p, snap); err != nil {
				return fmt.Errorf("back up the database %s: %w", name, err)
			}
			err := addFile(tw, name, snap, d)
			if rmErr := os.Remove(snap); err == nil {
				err = rmErr
			}
			if err != nil {
				return err
			}
			entries++
			res.Databases = append(res.Databases, name)
			return nil
		}
		if err := addFile(tw, name, p, d); err != nil {
			return err
		}
		entries++
		res.Files++
		return nil
	})
	if err != nil {
		return res, err
	}
	trailer, err := json.Marshal(end{Entries: entries})
	if err != nil {
		return res, err
	}
	if err := tw.WriteHeader(&tar.Header{Name: endName, Mode: 0o600, Size: int64(len(trailer)), ModTime: res.Created, Typeflag: tar.TypeReg}); err != nil {
		return res, err
	}
	if _, err := tw.Write(trailer); err != nil {
		return res, err
	}
	if err := tw.Close(); err != nil {
		return res, err
	}
	return res, gz.Close()
}

const staleAge = time.Hour

func CleanStale(dataDir string) error {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), tmpPrefix) {
			continue
		}
		dir := filepath.Join(dataDir, e.Name())
		lockPath := filepath.Join(dir, lockName)
		_, err := os.Lstat(lockPath)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if info, err := e.Info(); err != nil || time.Since(info.ModTime()) < staleAge {
				continue
			}
		case err != nil:
			errs = append(errs, err)
			continue
		default:
			lock, err := lockfile.Lock(lockPath)
			if errors.Is(err, lockfile.ErrLocked) {
				continue
			}
			if err != nil {
				errs = append(errs, err)
				continue
			}
			_ = lock.Close()
		}
		errs = append(errs, os.RemoveAll(dir))
	}
	return errors.Join(errs...)
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

func modTime(d fs.DirEntry) time.Time {
	info, err := d.Info()
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

func fileMode(perm fs.FileMode) fs.FileMode { return 0o600 | perm&0o100 }

func journal(p string) bool {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if base, ok := strings.CutSuffix(p, suffix); ok {
			isDB, err := isSQLite(base)
			return err == nil && isDB
		}
	}
	return false
}

func isSQLite(p string) (bool, error) {
	f, err := os.Open(filepath.Clean(p))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	head := make([]byte, len(sqliteHeader))
	if _, err := io.ReadFull(f, head); err != nil {
		return false, nil
	}
	return bytes.Equal(head, sqliteHeader), nil
}

func snapshot(ctx context.Context, src, dst string) error {
	sqlDB, err := open(src)
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	_, err = sqlDB.ExecContext(ctx, `VACUUM INTO ?`, dst)
	return err
}

func open(p string) (*sql.DB, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: "_pragma=busy_timeout(10000)"}
	return sql.Open("sqlite", u.String())
}

func addFile(tw *tar.Writer, name, src string, d fs.DirEntry) error {
	f, err := os.Open(filepath.Clean(src))
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	var perm fs.FileMode
	if orig, err := d.Info(); err == nil {
		perm = orig.Mode().Perm()
	}
	hdr := &tar.Header{Name: name, Mode: int64(fileMode(perm)), Size: info.Size(), ModTime: modTime(d), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := io.CopyN(tw, f, info.Size()); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%s changed while it was backed up: try again", name)
		}
		return err
	}
	return nil
}

func Restore(ctx context.Context, r io.Reader, dataDir string) (Manifest, error) {
	var m Manifest
	if err := emptyDir(dataDir); err != nil {
		return m, err
	}
	lock, err := lockfile.DataDir(dataDir)
	if err != nil {
		return m, err
	}
	defer lock.Close()
	if err := emptyDir(dataDir); err != nil {
		return m, err
	}
	m, err = restore(ctx, r, dataDir)
	if err != nil {
		if clearErr := clearDir(dataDir); clearErr != nil {
			err = errors.Join(err, fmt.Errorf("remove what the failed restore wrote: %w", clearErr))
		}
	}
	return m, err
}

func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if e.Name() != lockfile.DataDirName {
			errs = append(errs, os.RemoveAll(filepath.Join(dir, e.Name())))
		}
	}
	return errors.Join(errs...)
}

func restore(ctx context.Context, r io.Reader, dataDir string) (Manifest, error) {
	var m Manifest
	root, err := os.OpenRoot(dataDir)
	if err != nil {
		return m, err
	}
	defer root.Close()

	br := bufio.NewReader(r)
	var gz *gzip.Reader
	src := io.Reader(br)
	if magic, _ := br.Peek(len(gzipMagic)); bytes.Equal(magic, gzipMagic) {
		if gz, err = gzip.NewReader(br); err != nil {
			return m, fmt.Errorf("not a taskto.ai backup: %w", err)
		}
		src = gz
	}
	tr := tar.NewReader(src)
	hdr, err := tr.Next()
	if err != nil || hdr.Name != manifestName {
		return m, errors.New("not a taskto.ai backup: the manifest is missing")
	}
	if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&m); err != nil {
		return m, fmt.Errorf("not a taskto.ai backup: %w", err)
	}
	switch {
	case m.Format != 1 && m.Format != format:
		return m, fmt.Errorf("backup format %d isn't supported by this version (it reads formats 1 to %d)", m.Format, format)
	case m.Format == 1 && gz == nil:
		return m, errors.New("not a taskto.ai backup: a format 1 backup is gzipped")
	}

	var databases []string
	read, ended := 0, false
	for {
		if err := ctx.Err(); err != nil {
			return m, err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return m, fmt.Errorf("read the backup: %w", err)
		}
		if ended {
			return m, fmt.Errorf("the backup has %q after its end", hdr.Name)
		}
		if hdr.Name == endName && m.Format >= 2 {
			var e end
			if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&e); err != nil {
				return m, fmt.Errorf("read the backup's end: %w", err)
			}
			if e.Entries != read {
				return m, fmt.Errorf("the backup is damaged: its end counts %d entries, but %d came before it", e.Entries, read)
			}
			ended = true
			continue
		}
		read++
		name := strings.TrimSuffix(hdr.Name, "/")
		if !filepath.IsLocal(name) || path.Clean(name) != name || name == manifestName || name == endName || name == lockfile.DataDirName {
			return m, fmt.Errorf("the backup has an unexpected path %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := mkdirAll(root, name); err != nil {
				return m, err
			}
		case tar.TypeReg:
			if err := mkdirAll(root, path.Dir(name)); err != nil {
				return m, err
			}
			isDB, err := restoreFile(root, name, fileMode(fs.FileMode(hdr.Mode&0o777)), tr)
			if err != nil {
				return m, err
			}
			if isDB {
				databases = append(databases, name)
			}
		default:
			return m, fmt.Errorf("the backup has %q, which is neither a file nor a directory", hdr.Name)
		}
	}
	if m.Format >= 2 && !ended {
		return m, errors.New("the backup is incomplete: its end is missing (was it cut short?)")
	}
	if gz != nil {
		if _, err := io.Copy(io.Discard, gz); err != nil {
			return m, fmt.Errorf("read the backup: %w", err)
		}
	}

	for _, name := range databases {
		if err := check(ctx, filepath.Join(dataDir, filepath.FromSlash(name))); err != nil {
			return m, fmt.Errorf("restored database %s: %w", name, err)
		}
	}
	if slices.Contains(databases, db.FileName) {
		if err := endLogins(ctx, filepath.Join(dataDir, db.FileName)); err != nil {
			return m, fmt.Errorf("end the restored logins: %w", err)
		}
		if err := forgetWorkerHistory(ctx, filepath.Join(dataDir, db.FileName)); err != nil {
			return m, fmt.Errorf("mark the CLI workers' history unknown: %w", err)
		}
	}
	return m, nil
}

func forgetWorkerHistory(ctx context.Context, p string) error {
	sqlDB, err := open(p)
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	var n int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'cli_worker_history'`).Scan(&n); err != nil || n == 0 {
		return err
	}
	_, err = sqlDB.ExecContext(ctx, `INSERT INTO cli_worker_history (worker, since)
		VALUES ('unknown', max(strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), coalesce((SELECT max(created_at) FROM users), '')))
		ON CONFLICT (worker) DO UPDATE SET since = max(since, excluded.since)`)
	return err
}

type loginTable struct{ name, clear string }

var loginTables = []loginTable{
	{"push_deliveries", `DELETE FROM push_deliveries`},
	{"push_subscriptions", `DELETE FROM push_subscriptions`},
	{"sessions", `DELETE FROM sessions`},
	{"access_tokens", `DELETE FROM access_tokens`},
	{"password_resets", `DELETE FROM password_resets`},
	{"login_challenges", `DELETE FROM login_challenges`},
}

func endLogins(ctx context.Context, p string) error {
	sqlDB, err := open(p)
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, table := range loginTables {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table.name).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, table.clear); err != nil {
			return fmt.Errorf("%s: %w", table.name, err)
		}
	}
	return tx.Commit()
}

func emptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return os.MkdirAll(dir, 0o700)
	}
	if err != nil {
		return fmt.Errorf("data directory: %w", err)
	}
	entries = slices.DeleteFunc(entries, func(e fs.DirEntry) bool { return e.Name() == lockfile.DataDirName })
	if len(entries) > 0 {
		return fmt.Errorf("data directory %s isn't empty: restore into a new or empty directory", dir)
	}
	return nil
}

func mkdirAll(root *os.Root, name string) error {
	if name == "." {
		return nil
	}
	if err := mkdirAll(root, path.Dir(name)); err != nil {
		return err
	}
	err := root.Mkdir(name, 0o700)
	if errors.Is(err, fs.ErrExist) {
		info, statErr := root.Lstat(name)
		if statErr == nil && info.IsDir() {
			return nil
		}
	}
	return err
}

func restoreFile(root *os.Root, name string, mode fs.FileMode, r io.Reader) (isDB bool, err error) {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return false, err
	}
	head := &prefix{want: len(sqliteHeader)}
	if _, err := io.Copy(io.MultiWriter(f, head), r); err != nil {
		_ = f.Close()
		return false, fmt.Errorf("restore %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	return bytes.Equal(head.b, sqliteHeader), nil
}

type prefix struct {
	want int
	b    []byte
}

func (p *prefix) Write(b []byte) (int, error) {
	if n := p.want - len(p.b); n > 0 {
		p.b = append(p.b, b[:min(n, len(b))]...)
	}
	return len(b), nil
}

func check(ctx context.Context, p string) error {
	sqlDB, err := open(p)
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	var result string
	if err := sqlDB.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("integrity check: %s", result)
	}
	return nil
}
