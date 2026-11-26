// Package backup writes a consistent snapshot of the data directory to an archive, and
// restores one.
//
// The archive is a tar, gzipped unless the caller asks for it plain (Options): a backup
// program that deduplicates, such as restic, stores a plain tar's unchanged files once,
// where every gzipped archive is new from the first change on. Its first entry is a
// manifest (manifestName); then come the data directory's directories and files, by path
// relative to it; the last entry (endName) counts them, so a restore notices an archive
// cut short even without gzip's checksum. Every SQLite
// database in it (the server's, the agent runtime's, any later one) is found by its file
// header and copied with VACUUM INTO, which reads one transaction's view of the database
// while the server keeps running: a live WAL database copied byte by byte can be torn.
// Their -wal, -shm and -journal files are left out, since the copy already holds what
// they had committed. Other files (the server key, the spaces' git repositories) are
// copied as they are, except the files the server derives from the database at start
// (derived), the cache of what can be downloaded again (cacheDir), and the lock file of
// the server that runs on it. Sockets are skipped, and so are symbolic links, which a
// restore would have to follow.
//
// A backup runs alongside the server, so it doesn't take the data directory's lock. A
// restore takes it, so no server starts on the data directory while it is written.
//
// Each database is consistent on its own; two databases are copied one after the other,
// so they can be a moment apart, as after a crash.
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

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"taskto.ai/taskto/internal/db"
	"taskto.ai/taskto/internal/lockfile"
)

// manifestName is the archive's first entry, and endName its last. Neither is a data
// directory file, and a restore doesn't write them.
const (
	manifestName = "taskto-backup.json"
	endName      = "taskto-backup-end.json"
)

// tmpPrefix starts the name of the directory, in the data directory, where Create makes
// its snapshots. It is never backed up.
const tmpPrefix = ".backup-"

// derived are paths the server writes afresh from the database at every start, so a backup
// leaves them out: the agent runtime's providers file and its key files, which hold the
// provider accounts' API keys in clear (the database keeps them encrypted with server.key).
// The providers feed's temporary files, named .tmp-*, are left out with them.
var derived = []string{"agent/providers.json", "agent/keys"}

// cacheDir is the data directory's cache: only what can be downloaded again, and is
// checked when it is (dictation models, docs/dictation.md). A backup leaves it out, and a
// restored server fills it again as it needs to.
const cacheDir = "cache"

// format is the archive layout's version. Format 1 was always gzipped and had no end
// entry; format 2 has the end entry, and may be plain. A restore reads both, and refuses
// others.
const format = 2

// gzipMagic starts every gzip stream.
var gzipMagic = []byte{0x1f, 0x8b}

// sqliteHeader starts every SQLite database file.
var sqliteHeader = []byte("SQLite format 3\x00")

// Manifest describes a backup.
type Manifest struct {
	Format int `json:"format"`
	// Version is the version of the binary that made the backup.
	Version string `json:"version"`
	// Created is when the backup started, in UTC.
	Created time.Time `json:"created"`
}

// end is the archive's last entry: the number of entries between the manifest and it.
type end struct {
	Entries int `json:"entries"`
}

// Options are how Create writes the archive.
type Options struct {
	// Uncompressed writes a plain tar instead of a gzipped one, for a backup program that
	// compresses and deduplicates what it stores (restic).
	Uncompressed bool
}

// Result is what Create backed up.
type Result struct {
	Manifest
	// Databases are the SQLite databases, by path relative to the data directory.
	Databases []string
	// Files is the number of other files.
	Files int
	// Skipped are the paths left out because they are symbolic links or other special
	// files, but not sockets.
	Skipped []string
}

// Create writes a snapshot of dataDir to w, gzipped unless opts say otherwise. version is
// the running binary's version, recorded in the manifest.
func Create(ctx context.Context, dataDir, version string, w io.Writer, opts Options) (Result, error) {
	res := Result{Manifest: Manifest{Format: format, Version: version, Created: time.Now().UTC().Truncate(time.Second)}}
	info, err := os.Stat(dataDir)
	if err != nil {
		return res, fmt.Errorf("data directory: %w", err)
	}
	if !info.IsDir() {
		return res, fmt.Errorf("data directory: %s is not a directory", dataDir)
	}
	// The snapshots are made in the data directory, which has room for its databases,
	// rather than in a temporary directory that can be small or in memory.
	// What a backup that was killed left behind goes first: it can be as large as the
	// databases.
	_ = CleanStale(dataDir)
	tmp, err := os.MkdirTemp(dataDir, tmpPrefix)
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(tmp)
	// Held while the backup runs, so CleanStale leaves this directory alone.
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
	entries := 0 // written after the manifest, for the end entry
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
			return filepath.SkipDir // this backup's snapshots, or a failed one's
		}
		if name == StatusDir && d.IsDir() {
			return filepath.SkipDir // the backup schedule's status: about this server, not data
		}
		if name == manifestName || name == endName {
			// Not the server's file, and its name is one of the archive's own: archiving it
			// would make a backup that restore refuses. Reported, like other skipped files.
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
			return nil // the agent runtime's socket: the sidecar makes a new one
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
	// The end entry, written only once everything else is: an archive without it was cut
	// short.
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

// staleAge is how old a backup's directory without a lock file must be before CleanStale
// takes it for a leftover: a backup that has only just made it hasn't locked it yet.
const staleAge = time.Hour

// CleanStale removes what backups that were killed left in dataDir: their directories of
// snapshots, which no running backup holds the lock of.
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
			// No lock file: just made, or left by a version without one.
			if info, err := e.Info(); err != nil || time.Since(info.ModTime()) < staleAge {
				continue
			}
		case err != nil:
			// Can't tell whether a backup holds it: leave it.
			errs = append(errs, err)
			continue
		default:
			lock, err := lockfile.Lock(lockPath)
			if errors.Is(err, lockfile.ErrLocked) {
				continue // a backup is running
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

// nopCloser is a writer whose Close does nothing: the plain archive's stand-in for gzip.
type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

func modTime(d fs.DirEntry) time.Time {
	info, err := d.Info()
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// fileMode is the mode a file is archived and restored with: owner-only, keeping whether
// the owner may execute it (a git hook, say).
func fileMode(perm fs.FileMode) fs.FileMode { return 0o600 | perm&0o100 }

// journal reports whether p is a SQLite -wal, -shm or -journal file next to its database.
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
	f, err := os.Open(p) // #nosec G304 -- a file in the data directory being backed up.
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	head := make([]byte, len(sqliteHeader))
	if _, err := io.ReadFull(f, head); err != nil {
		return false, nil // shorter than a header: not a database
	}
	return bytes.Equal(head, sqliteHeader), nil
}

// snapshot copies the database at src to dst with VACUUM INTO: a consistent copy, made
// as one read transaction, so writers carry on meanwhile.
func snapshot(ctx context.Context, src, dst string) error {
	sqlDB, err := open(src)
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	_, err = sqlDB.ExecContext(ctx, `VACUUM INTO ?`, dst)
	return err
}

// open opens the database at p with only a busy timeout: its other settings (journal
// mode and so on) are its owner's, and a backup or restore mustn't change them.
func open(p string) (*sql.DB, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: "_pragma=busy_timeout(10000)"}
	return sql.Open("sqlite", u.String())
}

// addFile writes the file at src to tw as name, with d's time and mode. A file that
// shrinks while it is read is an error; one that grows is copied up to the size it had
// when opened.
func addFile(tw *tar.Writer, name, src string, d fs.DirEntry) error {
	f, err := os.Open(src) // #nosec G304 -- a file in the data directory, or our snapshot of one.
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

// Restore writes the backup read from r, gzipped or plain, into dataDir, which must be
// empty or not exist, and checks every database it restored. A format 2 archive must end
// with its end entry, which counts the entries before it. Restore holds the data
// directory's lock while it runs, so it fails if a server runs on dataDir, and no server
// starts there until it is done. A restore that fails removes what it wrote.
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
	// Again, now that no server can start: one may have started since the first check.
	if err := emptyDir(dataDir); err != nil {
		return m, err
	}
	// The directory was empty and is ours until the lock goes: whatever a failed restore
	// wrote is removed, so the next attempt finds it empty again.
	m, err = restore(ctx, r, dataDir)
	if err != nil {
		if clearErr := clearDir(dataDir); clearErr != nil {
			err = errors.Join(err, fmt.Errorf("remove what the failed restore wrote: %w", clearErr))
		}
	}
	return m, err
}

// clearDir removes everything in dir but the data directory's lock file.
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

// restore writes the backup read from r into dataDir, which Restore has checked is empty
// and holds the lock of.
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
		// Format 1 was always gzipped, and has no end entry: plain, a cut couldn't be seen.
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
		// Only plain relative paths inside the data directory; os.Root also refuses any
		// path that would leave it.
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
	// The gzip trailer's checksum is only checked once the stream is read to its end.
	if gz != nil {
		if _, err := io.Copy(io.Discard, gz); err != nil { // #nosec G110 -- discarded, never stored.
			return m, fmt.Errorf("read the backup: %w", err)
		}
	}

	for _, name := range databases {
		if err := check(ctx, filepath.Join(dataDir, filepath.FromSlash(name))); err != nil {
			return m, fmt.Errorf("restored database %s: %w", name, err)
		}
	}
	// The server's own database: no login the backup held may come back (see endLogins).
	if slices.Contains(databases, db.FileName) {
		if err := endLogins(ctx, filepath.Join(dataDir, db.FileName)); err != nil {
			return m, fmt.Errorf("end the restored logins: %w", err)
		}
		if err := forgetWorkerHistory(ctx, filepath.Join(dataDir, db.FileName)); err != nil {
			return m, fmt.Errorf("mark the CLI workers' history unknown: %w", err)
		}
	}
	// The container CLI worker's directories aren't in the backup: until the server has told it who
	// exists, a directory from before it recorded owners can't be given to anyone.
	if err := writeMark(filepath.Join(dataDir, RestoredMark)); err != nil {
		return m, fmt.Errorf("mark the data directory restored: %w", err)
	}
	return m, nil
}

// RestoredMark is the file a restore leaves in the data directory, for the server: the CLI
// worker's directories (in container mode a volume of their own, which no backup covers) may be
// from another history than the restored database's. The server removes it once it has told the
// worker who exists (docs/cli-workers.md).
const RestoredMark = "cli-worker-restored"

// writeMark makes the file at path, durably.
func writeMark(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // G304: a fixed name in the restored data directory
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// forgetWorkerHistory marks the CLI workers' history of the restored server database at p as
// unknown up to now (cli_worker_history, migration 0059). The backup's record says which workers
// had run when it was made, not which ran since, while its accounts were still in use: a purge of
// one of those accounts must wait for either worker to remove their files. A database from a
// version without the record gets it when the server migrates it.
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
	// Not before any restored account was made, whatever this host's clock says.
	_, err = sqlDB.ExecContext(ctx, `INSERT INTO cli_worker_history (worker, since)
		VALUES ('unknown', max(strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), coalesce((SELECT max(created_at) FROM users), '')))
		ON CONFLICT (worker) DO UPDATE SET since = max(since, excluded.since)`)
	return err
}

// loginTables are the tables of the server's database that hold a bearer's way in, emptied
// by a restore, in the order that leaves no row pointing at a deleted one: a push
// subscription and its deliveries depend on a session.
//
// A server ends a login by deleting its row (signing out, a password change, revoking a
// token, using a reset link or a challenge), so a backup made before that holds the row
// still, and restoring it would bring the login back: a browser that kept its cookie after
// its session was refused would be signed in again with no password. A restore can't tell
// which rows were ended after the backup, so it ends them all. Tables a database from an
// older version doesn't have yet are skipped.
//
// What a restore leaves, because it holds no revocation: invites (they expire, and use
// needs the address's own account) and two-factor recovery codes and TOTP steps (the state
// of the account as of the backup, like the password itself, which a restore also brings
// back: a person who changed it since changes it again).
var loginTables = []string{
	"push_deliveries",
	"push_subscriptions",
	"sessions",
	"access_tokens",
	"password_resets",
	"login_challenges",
}

// endLogins deletes every row of loginTables in the restored server database at p, in one
// transaction.
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
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		// The names are constants of this file, never input.
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil { // #nosec G202
			return fmt.Errorf("%s: %w", table, err)
		}
	}
	return tx.Commit()
}

// emptyDir makes dir if it doesn't exist, and fails if it isn't an empty directory, so
// a restore never mixes with or overwrites existing data. The data directory's lock file
// doesn't count: a restore that failed before writing anything leaves it.
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

// restoreFile writes one file of the backup with mode, and reports whether it is a
// SQLite database. A path that already exists is an error: the archive repeats it.
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

// prefix keeps the first want bytes written to it.
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

// check runs SQLite's integrity check on a restored database.
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
