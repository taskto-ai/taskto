package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"taskto.ai/taskto/internal/db"
	"taskto.ai/taskto/internal/lockfile"
)

// fixture is a data directory like a running server's: the server's database, an agent
// database in WAL mode with writes not yet checkpointed, a key file, a space's files, a
// socket and a symbolic link.
type fixture struct {
	dir     string
	server  *sql.DB
	agent   *sql.DB
	files   map[string]string // other files by relative path, and their content
	execBit string            // a file the owner may execute
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	f := &fixture{dir: t.TempDir(), files: map[string]string{
		"server.key":                                      "fake-server-key-material",
		"workspaces/1/spaces/1/kb.git/HEAD":               "ref: refs/heads/main\n",
		"workspaces/1/spaces/1/kb.git/objects/ab/cdef":    "\x00\x01binary\xff",
		"workspaces/1/spaces/1/kb.git/hooks/post-receive": "#!/bin/sh\n",
		"workspaces/1/spaces/2/notes/empty.md":            "",
	}, execBit: "workspaces/1/spaces/1/kb.git/hooks/post-receive"}
	for name, content := range f.files {
		p := filepath.Join(f.dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		mode := fs.FileMode(0o600)
		if name == f.execBit {
			mode = 0o700
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(f.dir, "workspaces", "1", "spaces", "3"), 0o700); err != nil {
		t.Fatal(err)
	}

	var err error
	f.server, err = db.Open(ctx, filepath.Join(f.dir, db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.server.Close() })
	if _, err := db.Migrate(ctx, f.server); err != nil {
		t.Fatal(err)
	}
	if _, err := f.server.Exec(`INSERT INTO users (email, name, password_hash) VALUES ('owner@example.com', 'Owner', 'fake-hash')`); err != nil {
		t.Fatal(err)
	}

	// The agent database stays open, in WAL mode with checkpoints off, so its latest rows
	// are only in agent.db-wal: a copy of agent.db alone would miss them.
	if err := os.MkdirAll(filepath.Join(f.dir, "agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.agent, err = sql.Open("sqlite", "file:"+filepath.Join(f.dir, "agent", "agent.db")+
		"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	f.agent.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = f.agent.Close() })
	for _, q := range []string{
		`CREATE TABLE events (id INTEGER PRIMARY KEY, body TEXT NOT NULL)`,
		`INSERT INTO events (body) VALUES ('first'), ('second')`,
	} {
		if _, err := f.agent.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if info, err := os.Stat(filepath.Join(f.dir, "agent", "agent.db-wal")); err != nil || info.Size() == 0 {
		t.Fatalf("agent.db-wal = %v, %v: the fixture wants uncheckpointed writes", info, err)
	}

	// A socket, as the agent runtime has. Unix socket paths are short, so it may not fit
	// under the test's temporary directory; then that part is left out.
	sock := filepath.Join(f.dir, "agent", "run", "agent.sock")
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	if len(sock) < 100 {
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(f.dir, "workspaces", "1", "spaces", "2", "link")); err != nil {
		t.Fatal(err)
	}
	// What the server writes from the database at start, decrypted keys included.
	for _, name := range []string{"agent/providers.json", "agent/.tmp-123", "agent/keys/ws1-local.key"} {
		p := filepath.Join(f.dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("fake-derived"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func backup(t *testing.T, dir string) (*bytes.Buffer, Result) {
	t.Helper()
	return backupWith(t, dir, Options{})
}

func backupWith(t *testing.T, dir string, opts Options) (*bytes.Buffer, Result) {
	t.Helper()
	var buf bytes.Buffer
	res, err := Create(context.Background(), dir, "v-test", &buf, opts)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return &buf, res
}

// entries lists the names in an archive, gzipped or plain.
func entries(t *testing.T, archive []byte) []string {
	t.Helper()
	var r io.Reader = bytes.NewReader(archive)
	if bytes.HasPrefix(archive, gzipMagic) {
		gz, err := gzip.NewReader(r)
		if err != nil {
			t.Fatal(err)
		}
		r = gz
	}
	tr := tar.NewReader(r)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
	}
}

func count(t *testing.T, sqlDB *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := sqlDB.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	// What a failed backup left behind isn't data.
	stale := filepath.Join(f.dir, tmpPrefix+"stale")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "snapshot.db"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive, res := backup(t, f.dir)

	if want := []string{"agent/agent.db", db.FileName}; !slices.Equal(res.Databases, want) {
		t.Fatalf("databases = %v, want %v", res.Databases, want)
	}
	if res.Files != len(f.files) || !slices.Equal(res.Skipped, []string{"workspaces/1/spaces/2/link"}) {
		t.Fatalf("files = %d, skipped = %v; want %d files and the link skipped", res.Files, res.Skipped, len(f.files))
	}
	if leftovers, _ := filepath.Glob(filepath.Join(f.dir, tmpPrefix+"*")); len(leftovers) != 1 {
		t.Fatalf("the backup left %v in the data directory", leftovers)
	}
	names := entries(t, archive.Bytes())
	if names[0] != manifestName || names[len(names)-1] != endName {
		t.Fatalf("entries = %q, want the manifest first and the end last", names)
	}
	for _, n := range names {
		if strings.HasPrefix(n, tmpPrefix) || strings.HasPrefix(n, "agent/keys") || strings.HasPrefix(n, "agent/.tmp-") ||
			n == "agent/providers.json" || strings.HasSuffix(n, "-wal") || strings.HasSuffix(n, "-shm") || strings.HasSuffix(n, ".sock") || strings.HasSuffix(n, "link") {
			t.Errorf("the archive has %s", n)
		}
	}

	// A later write isn't in the backup.
	if _, err := f.agent.Exec(`INSERT INTO events (body) VALUES ('after the backup')`); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "restored")
	m, err := Restore(ctx, archive, dst)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if m.Format != format || m.Version != "v-test" || m.Created.IsZero() {
		t.Fatalf("manifest = %+v", m)
	}
	for _, name := range []string{manifestName, endName} {
		if _, err := os.Stat(filepath.Join(dst, name)); !os.IsNotExist(err) {
			t.Fatalf("%s was restored into the data directory (%v)", name, err)
		}
	}

	for name, content := range f.files {
		p := filepath.Join(dst, filepath.FromSlash(name))
		got, err := os.ReadFile(p)
		if err != nil || string(got) != content {
			t.Errorf("%s = %q (%v), want %q", name, got, err, content)
		}
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		want := fs.FileMode(0o600)
		if name == f.execBit {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode = %v, want %v", name, info.Mode().Perm(), want)
		}
	}
	if info, err := os.Stat(filepath.Join(dst, "workspaces", "1", "spaces", "3")); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Errorf("the empty space directory = %v, %v; want an owner-only directory", info, err)
	}
	if info, err := os.Stat(dst); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("data directory = %v, %v; want owner-only", info, err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "workspaces", "1", "spaces", "2", "link")); !os.IsNotExist(err) {
		t.Errorf("the symbolic link was restored (%v)", err)
	}

	// The restored server database opens, is up to date and has the user.
	server, err := db.Open(ctx, filepath.Join(dst, db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if applied, err := db.Migrate(ctx, server); err != nil || len(applied) != 0 {
		t.Fatalf("migrations on the restored database = %v, %v; want none to apply", applied, err)
	}
	if n := count(t, server, `SELECT count(*) FROM users WHERE email = 'owner@example.com'`); n != 1 {
		t.Fatalf("restored users = %d, want 1", n)
	}

	// The agent database has the rows that were only in its WAL file, and not the later one.
	agent, err := sql.Open("sqlite", filepath.Join(dst, "agent", "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	if n := count(t, agent, `SELECT count(*) FROM events`); n != 2 {
		t.Fatalf("restored agent events = %d, want 2", n)
	}

	// A backup of the restored directory restores again: the round trip is stable.
	again, _ := backup(t, dst)
	if _, err := Restore(ctx, again, filepath.Join(t.TempDir(), "again")); err != nil {
		t.Fatalf("restore of a backup of a restore: %v", err)
	}
}

// A backup taken while the server writes is consistent: every database passes the
// integrity check and holds a whole number of transactions.
func TestBackupWhileWriting(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.server.Exec(`CREATE TABLE pairs (n INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	var writeErr error
	wg.Go(func() {
		for i := 0; !stop.Load(); i++ {
			// Each transaction adds two rows: a torn copy could have an odd number.
			tx, err := f.server.BeginTx(ctx, nil)
			if err == nil {
				_, err = tx.Exec(`INSERT INTO pairs (n) VALUES (?), (?)`, i, i)
			}
			if err == nil {
				err = tx.Commit()
			}
			if err != nil {
				writeErr = err
				return
			}
		}
	})
	var archives []*bytes.Buffer
	for range 3 {
		buf, _ := backup(t, f.dir)
		archives = append(archives, buf)
	}
	stop.Store(true)
	wg.Wait()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	for i, a := range archives {
		dst := filepath.Join(t.TempDir(), fmt.Sprint(i))
		if _, err := Restore(ctx, a, dst); err != nil {
			t.Fatalf("restore %d: %v", i, err)
		}
		server, err := sql.Open("sqlite", filepath.Join(dst, db.FileName))
		if err != nil {
			t.Fatal(err)
		}
		if n := count(t, server, `SELECT count(*) FROM pairs`); n%2 != 0 {
			t.Errorf("backup %d has %d rows: a transaction was torn", i, n)
		}
		_ = server.Close()
	}
}

func TestRestoreRefusesANonEmptyDirectory(t *testing.T) {
	f := newFixture(t)
	archive, _ := backup(t, f.dir)
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, "keep"), []byte("existing data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), archive, dst); err == nil || !strings.Contains(err.Error(), "isn't empty") {
		t.Fatalf("Restore into a non-empty directory = %v, want refused", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "keep")); string(got) != "existing data" {
		t.Fatal("the existing file changed")
	}
	// An empty directory, such as a fresh volume's mount point, is fine.
	if _, err := Restore(context.Background(), bytes.NewReader(archive.Bytes()), t.TempDir()); err != nil {
		t.Fatalf("Restore into an empty directory: %v", err)
	}
}

// A backup runs beside the server that holds the data directory's lock, and leaves the
// lock file out. A restore takes the lock: it is refused while another process holds it,
// and a lock file left behind on its own doesn't make the directory non-empty.
func TestBackupAndRestoreWithTheLock(t *testing.T) {
	f := newFixture(t)
	server, err := lockfile.DataDir(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	archive, _ := backup(t, f.dir)
	if names := entries(t, archive.Bytes()); slices.Contains(names, lockfile.DataDirName) {
		t.Fatalf("the backup has the lock file: %v", names)
	}

	dst := t.TempDir()
	other, err := lockfile.DataDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), bytes.NewReader(archive.Bytes()), dst); err == nil || !strings.Contains(err.Error(), "in use by another taskto process") {
		t.Fatalf("Restore into a locked directory = %v, want refused", err)
	}
	other.Close()
	if _, err := Restore(context.Background(), bytes.NewReader(archive.Bytes()), dst); err != nil {
		t.Fatalf("Restore into a directory with only a lock file left in it: %v", err)
	}
	// Restore released it.
	again, err := lockfile.DataDir(dst)
	if err != nil {
		t.Fatalf("lock after the restore: %v", err)
	}
	again.Close()
}

type entry struct {
	name     string
	typeflag byte
	body     string
	linkname string
}

// archiveOf makes a gzipped archive of es, after manifest if it isn't nil, and with the
// end entry a format 2 manifest needs.
func archiveOf(t *testing.T, manifest *Manifest, es ...entry) []byte {
	t.Helper()
	if manifest != nil && manifest.Format >= 2 {
		es = append(es, endOf(len(es)))
	}
	return rawArchive(t, true, manifest, es...)
}

// endOf is an end entry that counts n entries.
func endOf(n int) entry {
	raw, _ := json.Marshal(end{Entries: n})
	return entry{name: endName, typeflag: tar.TypeReg, body: string(raw)}
}

// rawArchive makes an archive of exactly es, after manifest if it isn't nil, gzipped or
// plain.
func rawArchive(t *testing.T, gzipped bool, manifest *Manifest, es ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w io.WriteCloser = nopCloser{&buf}
	if gzipped {
		w = gzip.NewWriter(&buf)
	}
	tw := tar.NewWriter(w)
	if manifest != nil {
		raw, _ := json.Marshal(manifest)
		es = append([]entry{{name: manifestName, typeflag: tar.TypeReg, body: string(raw)}}, es...)
	}
	for _, e := range es {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typeflag, Mode: 0o600, Size: int64(len(e.body)), Linkname: e.linkname}
		if e.typeflag != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestRestoreRefusesBadArchives(t *testing.T) {
	ok := &Manifest{Format: format, Version: "v-test"}
	file := func(name string) entry { return entry{name: name, typeflag: tar.TypeReg, body: "x"} }
	for name, tc := range map[string]struct {
		archive []byte
		wantErr string
	}{
		"not gzip":          {[]byte("plain text"), "not a TaskTo.AI backup"},
		"no manifest":       {archiveOf(t, nil, file("taskto.db")), "manifest is missing"},
		"newer format":      {archiveOf(t, &Manifest{Format: format + 1}), "isn't supported"},
		"parent path":       {archiveOf(t, ok, file("../escape")), "unexpected path"},
		"nested parent":     {archiveOf(t, ok, file("spaces/../../escape")), "unexpected path"},
		"absolute path":     {archiveOf(t, ok, file("/tmp/escape")), "unexpected path"},
		"unclean path":      {archiveOf(t, ok, file("spaces//x")), "unexpected path"},
		"a second manifest": {archiveOf(t, ok, file(manifestName)), "unexpected path"},
		"no end":            {rawArchive(t, true, ok, file("a")), "its end is missing"},
		"no end, plain":     {rawArchive(t, false, ok, file("a")), "its end is missing"},
		"end counts more":   {rawArchive(t, false, ok, file("a"), endOf(2)), "counts 2 entries, but 1 came"},
		"end counts fewer":  {rawArchive(t, false, ok, file("a"), file("b"), endOf(1)), "counts 1 entries, but 2 came"},
		"after the end":     {rawArchive(t, false, ok, file("a"), endOf(1), file("b")), "after its end"},
		"gzip, counts more": {rawArchive(t, true, ok, file("a"), endOf(2)), "counts 2 entries, but 1 came"},
		"gzip, after end":   {rawArchive(t, true, ok, file("a"), endOf(1), file("b")), "after its end"},
		"format 1 plain":    {rawArchive(t, false, &Manifest{Format: 1}, file("a")), "format 1 backup is gzipped"},
		"format 1 with end": {rawArchive(t, true, &Manifest{Format: 1}, file("a"), endOf(1)), "unexpected path"},
		"symlink":           {archiveOf(t, ok, entry{name: "link", typeflag: tar.TypeSymlink, linkname: "/etc"}), "neither a file nor a directory"},
		"hard link":         {archiveOf(t, ok, entry{name: "link", typeflag: tar.TypeLink, linkname: "x"}), "neither a file nor a directory"},
		"repeated file":     {archiveOf(t, ok, file("a"), file("a")), "exists"},
		"file then dir":     {archiveOf(t, ok, file("a"), file("a/b")), "exists"},
		"corrupt database": {archiveOf(t, ok, entry{name: "taskto.db", typeflag: tar.TypeReg,
			body: string(sqliteHeader) + strings.Repeat("\xff", 4096)}), "taskto.db"},
	} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			_, err := Restore(context.Background(), bytes.NewReader(tc.archive), filepath.Join(parent, "data"))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Restore = %v, want an error containing %q", err, tc.wantErr)

			}
			if _, err := os.Lstat(filepath.Join(parent, "escape")); !os.IsNotExist(err) {
				t.Fatal("a file was written outside the data directory")
			}
		})
	}
}

// A truncated archive fails: a backup cut short in transfer must not restore quietly.
func TestRestoreRefusesATruncatedArchive(t *testing.T) {
	f := newFixture(t)
	archive, _ := backup(t, f.dir)
	cut := archive.Bytes()[:archive.Len()-20]
	if _, err := Restore(context.Background(), bytes.NewReader(cut), filepath.Join(t.TempDir(), "data")); err == nil {
		t.Fatal("a truncated archive restored without an error")
	}
}

func TestCreateNeedsADirectory(t *testing.T) {
	if _, err := Create(context.Background(), filepath.Join(t.TempDir(), "missing"), "v", io.Discard, Options{}); err == nil {
		t.Fatal("Create of a missing data directory succeeded")
	}
}

// Stray files named like the archive's manifest or end are left out (and reported), so
// the backup still restores.
func TestAFileNamedLikeTheManifestIsSkipped(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{manifestName, endName} {
		if err := os.WriteFile(filepath.Join(f.dir, name), []byte("not ours"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	archive, res := backup(t, f.dir)
	for _, name := range []string{manifestName, endName} {
		if !slices.Contains(res.Skipped, name) {
			t.Errorf("skipped = %v, want %s reported", res.Skipped, name)
		}
	}
	if _, err := Restore(context.Background(), archive, t.TempDir()); err != nil {
		t.Fatalf("the backup doesn't restore: %v", err)
	}
}

// A plain backup, as restic stores it, restores like a gzipped one.
func TestPlainBackupRoundTrip(t *testing.T) {
	f := newFixture(t)
	archive, res := backupWith(t, f.dir, Options{Uncompressed: true})
	if bytes.HasPrefix(archive.Bytes(), gzipMagic) {
		t.Fatal("an uncompressed backup is gzipped")
	}
	if names := entries(t, archive.Bytes()); names[0] != manifestName || names[len(names)-1] != endName {
		t.Fatalf("entries = %q, want the manifest first and the end last", names)
	}
	dst := filepath.Join(t.TempDir(), "restored")
	m, err := Restore(context.Background(), archive, dst)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if m.Format != format {
		t.Fatalf("format = %d, want %d", m.Format, format)
	}
	for name, content := range f.files {
		if got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(name))); err != nil || string(got) != content {
			t.Errorf("%s = %q (%v), want %q", name, got, err, content)
		}
	}
	if len(res.Databases) != 2 {
		t.Fatalf("databases = %v, want 2", res.Databases)
	}
}

// A plain archive has no checksum: one cut short anywhere, even right after an entry, is
// refused by its missing end.
func TestRestoreRefusesATruncatedPlainArchive(t *testing.T) {
	f := newFixture(t)
	archive, _ := backupWith(t, f.dir, Options{Uncompressed: true})
	raw := archive.Bytes()
	// Every cut at a 512-byte block boundary (where an entry may end), and a few others.
	cuts := []int{len(raw) - 1, len(raw) / 3}
	for n := 512; n < len(raw)-1024; n += 512 {
		cuts = append(cuts, n)
	}
	for _, n := range cuts {
		if _, err := Restore(context.Background(), bytes.NewReader(raw[:n]), filepath.Join(t.TempDir(), "data")); err == nil {
			t.Fatalf("an archive cut at %d of %d bytes restored without an error", n, len(raw))
		}
	}
}

// Backups made before the end entry existed (format 1, gzipped) still restore.
func TestRestoreReadsFormat1(t *testing.T) {
	archive := rawArchive(t, true, &Manifest{Format: 1, Version: "v-old"},
		entry{name: "spaces/", typeflag: tar.TypeDir},
		entry{name: "spaces/a.md", typeflag: tar.TypeReg, body: "kept"})
	dst := filepath.Join(t.TempDir(), "data")
	if _, err := Restore(context.Background(), bytes.NewReader(archive), dst); err != nil {
		t.Fatalf("Restore of a format 1 backup: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "spaces", "a.md")); err != nil || string(got) != "kept" {
		t.Fatalf("spaces/a.md = %q, %v", got, err)
	}
}

// The cache holds only what can be downloaded again (dictation models): a backup leaves it
// out, and a cache file elsewhere is kept.
func TestTheCacheIsLeftOut(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"cache/dictation/model.onnx", "workspaces/1/spaces/1/cache/kept.md"} {
		p := filepath.Join(f.dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	archive, res := backup(t, f.dir)
	names := entries(t, archive.Bytes())
	for _, n := range names {
		if n == "cache/" || strings.HasPrefix(n, "cache/") {
			t.Errorf("the backup has %s", n)
		}
	}
	if !slices.Contains(names, "workspaces/1/spaces/1/cache/kept.md") {
		t.Errorf("a space's own cache/ directory was left out: %v", names)
	}
	if slices.Contains(res.Skipped, cacheDir) {
		t.Errorf("skipped = %v: the cache is left out on purpose, not reported", res.Skipped)
	}
}

// A restore that fails partway removes what it wrote, so the directory is empty for the
// next attempt; the lock file may stay.
func TestAFailedRestoreLeavesTheDirectoryEmpty(t *testing.T) {
	f := newFixture(t)
	archive, _ := backupWith(t, f.dir, Options{Uncompressed: true})
	raw := archive.Bytes()
	dst := filepath.Join(t.TempDir(), "data")
	if _, err := Restore(context.Background(), bytes.NewReader(raw[:len(raw)*2/3]), dst); err == nil {
		t.Fatal("a cut archive restored")
	}
	left, err := os.ReadDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range left {
		if e.Name() != lockfile.DataDirName {
			t.Errorf("the failed restore left %s", e.Name())
		}
	}
	if _, err := Restore(context.Background(), bytes.NewReader(raw), dst); err != nil {
		t.Fatalf("restore after a failed one: %v", err)
	}
}

// A format 1 archive has no end entry: one cut short is refused by gzip's checksum.
func TestRestoreRefusesATruncatedFormat1Archive(t *testing.T) {
	archive := rawArchive(t, true, &Manifest{Format: 1}, entry{name: "a.md", typeflag: tar.TypeReg, body: "kept"})
	for _, cut := range []int{4, 8, 20} {
		if _, err := Restore(context.Background(), bytes.NewReader(archive[:len(archive)-cut]), filepath.Join(t.TempDir(), "data")); err == nil {
			t.Fatalf("a format 1 archive cut by %d bytes restored", cut)
		}
	}
}
