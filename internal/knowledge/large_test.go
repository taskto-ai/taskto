package knowledge

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
)

func oidOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func pointerText(b []byte) string {
	return string(pointer{oid: oidOf(b), size: int64(len(b))}.encode())
}

// objectsOnDisk lists the oids under the space's .git/lfs/objects.
func objectsOnDisk(t *testing.T, dataDir string, sp access.Space) []string {
	t.Helper()
	var oids []string
	dir := filepath.Join(sp.Dir(dataDir), filepath.FromSlash(objectsDir))
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			oids = append(oids, d.Name())
		}
		return err
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	slices.Sort(oids)
	return oids
}

func openBytes(t *testing.T, s *Store, sp access.Space, name, at string) ([]byte, error) {
	t.Helper()
	_, f, err := s.Open(ctx, sp, name, at)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func bytesOf(n int, b byte) []byte { return bytes.Repeat([]byte{b}, n) }

func TestUnversionedFiles(t *testing.T) {
	s, dataDir := newStore(t, Options{LargeFileSize: 150})
	sp := writable(1)

	big := bytesOf(200, 'b')
	r := must[Result](t)(s.Put(ctx, sp, "big.bin", big, "", alice, ""))
	if r.Doc.Versioned || r.Doc.Size != 200 {
		t.Fatalf("big.bin = %+v, want unversioned, 200 bytes", r.Doc)
	}
	clip := []byte("tiny video")
	must[Result](t)(s.Put(ctx, sp, "media/clip.MP4", clip, "", alice, "")) // audio and video, whatever their size and case
	if code := must[Result](t)(s.Put(ctx, sp, "src/app.ts", []byte("export {}"), "", alice, "")); !code.Doc.Versioned {
		t.Fatal("a TypeScript file was taken for video")
	}
	must[Result](t)(s.Delete(ctx, sp, "src/app.ts", "", alice, ""))
	small := must[Result](t)(s.Put(ctx, sp, "small.bin", bytesOf(150, 's'), "", alice, ""))
	if !small.Doc.Versioned {
		t.Fatal("a file at the limit isn't versioned")
	}
	_, err := s.Put(ctx, sp, "long.md", bytesOf(151, 'm'), "", alice, "")
	wantErr(t, err, ErrTooLarge) // Markdown is always versioned, so at most the limit

	// Git holds pointers, the bytes are beside it, and the checkout is clean.
	if got, want := objectsOnDisk(t, dataDir, sp), sortedOIDs(big, clip); !slices.Equal(got, want) {
		t.Fatalf("objects = %v, want %v", got, want)
	}
	checkClean(t, dataDir, sp, map[string]string{
		"big.bin": pointerText(big), "media/clip.MP4": pointerText(clip), "small.bin": string(bytesOf(150, 's')),
	})

	page := must[Page](t)(s.List(ctx, sp, "", "", 0))
	want := []Entry{
		{Path: "big.bin", Version: r.Doc.Version, Size: 200},
		{Path: "media/clip.MP4", Size: int64(len(clip))},
		{Path: "small.bin", Version: small.Doc.Version, Size: 150, Versioned: true},
	}
	for i := range page.Docs {
		if i < len(want) && want[i].Version == "" {
			want[i].Version = page.Docs[i].Version
		}
	}
	if !slices.Equal(page.Docs, want) {
		t.Fatalf("list = %+v, want %+v", page.Docs, want)
	}
	doc := must[Doc](t)(s.Read(ctx, sp, "big.bin", ""))
	if doc.Versioned || doc.Content != nil || doc.Size != 200 {
		t.Fatalf("read = %+v, want no content", doc.Entry)
	}
	if got := must[[]byte](t)(openBytes(t, s, sp, "big.bin", "")); !bytes.Equal(got, big) {
		t.Fatalf("open = %q", got)
	}

	// Replacing it frees the old bytes at once: that version can't be read or restored.
	first := r.Commit
	bigger := bytesOf(300, 'B')
	r = must[Result](t)(s.Put(ctx, sp, "big.bin", bigger, r.Doc.Version, alice, ""))
	if got, want := objectsOnDisk(t, dataDir, sp), sortedOIDs(bigger, clip); !slices.Equal(got, want) {
		t.Fatalf("objects after the replace = %v, want %v", got, want)
	}
	_, err = openBytes(t, s, sp, "big.bin", first)
	wantErr(t, err, ErrNotKept)
	_, err = s.Restore(ctx, sp, "big.bin", first, "", alice, "")
	wantErr(t, err, ErrNotKept)
	if h := must[History](t)(s.History(ctx, sp, "big.bin", "", 0)); len(h.Commits) != 2 {
		t.Fatalf("history = %+v, want the two changes", h.Commits)
	}
	// The old version as listed is still there, without its bytes.
	if old := must[Doc](t)(s.Read(ctx, sp, "big.bin", first)); old.Size != 200 || old.Versioned {
		t.Fatalf("old version = %+v", old.Entry)
	}

	// Moves keep the bytes; nothing unversioned becomes Markdown, nor the other way round.
	r = must[Result](t)(s.Move(ctx, sp, "big.bin", "media/big.bin", "", alice, ""))
	if r.Doc.Versioned || r.Doc.Size != 300 {
		t.Fatalf("moved = %+v", r.Doc)
	}
	if got := must[[]byte](t)(openBytes(t, s, sp, "media/big.bin", "")); !bytes.Equal(got, bigger) {
		t.Fatal("the move lost the bytes")
	}
	_, err = s.Move(ctx, sp, "media/big.bin", "big.md", "", alice, "")
	wantErr(t, err, ErrInvalid)
	_, err = s.Put(ctx, sp, "pointer.md", []byte(pointerText(clip)), "", alice, "")
	wantErr(t, err, ErrInvalid) // Markdown that would read as a version taken out of history
	must[Result](t)(s.Move(ctx, sp, "media", "videos", "", alice, ""))
	if got := must[[]byte](t)(openBytes(t, s, sp, "videos/clip.MP4", "")); !bytes.Equal(got, clip) {
		t.Fatal("the folder's move lost the bytes")
	}

	// Deleting frees them too.
	must[Result](t)(s.Delete(ctx, sp, "videos/big.bin", "", alice, ""))
	if got, want := objectsOnDisk(t, dataDir, sp), sortedOIDs(clip); !slices.Equal(got, want) {
		t.Fatalf("objects after the delete = %v, want %v", got, want)
	}
}

// Heavy binaries are unversioned at any size, by their extension in any case; Markdown
// never is, and kinds that are often small stay versioned under the limit.
func TestAlwaysUnversionedTypes(t *testing.T) {
	for _, name := range []string{
		"a.zip", "b.ZIP", "c.7z", "d.rar", "e.tar", "f.tar.gz", "g.TGZ", "h.xz", "i.bz2", "j.zst",
		"k.iso", "l.dmg", "m.img", "n.psd", "o.PsB", "p.tif", "q.TIFF", "r.raw", "s.cr2", "t.NEF",
		"u.arw", "v.dng", "w.ai", "x.sketch", "y.fig", "z.xcf", "dir.zip/scene.blend",
		"clip.MP4", "song.flac",
	} {
		if !AlwaysUnversioned(name) {
			t.Errorf("%s isn't always unversioned", name)
		}
	}
	for _, name := range []string{
		"notes.md", "archive.zip.md", "README.MD", "photo.png", "photo.jpg", "doc.pdf", "sheet.xlsx",
		"font.woff2", "font.ttf", "tool.exe", "lib.so", "app.ts", "zip", ".zip", "dir/.GZ", "zip.txt", "archive.zip/notes.txt",
	} {
		if AlwaysUnversioned(name) {
			t.Errorf("%s is always unversioned", name)
		}
	}

	s, dataDir := newStore(t, Options{LargeFileSize: 1 << 20})
	sp := writable(1)
	for _, name := range []string{"tiny.PSD", "tiny.Zip", "tiny.tiff"} {
		r := must[Result](t)(s.Put(ctx, sp, name, []byte("a few bytes"), "", alice, ""))
		if r.Doc.Versioned {
			t.Errorf("%s = %+v, want unversioned", name, r.Doc)
		}
	}
	r := must[Result](t)(s.PutFile(ctx, sp, "streamed.7Z", strings.NewReader("seven"), "", alice, ""))
	if r.Doc.Versioned {
		t.Errorf("streamed.7Z = %+v, want unversioned", r.Doc)
	}
	r = must[Result](t)(s.Put(ctx, sp, "archive.zip.md", []byte("# Not an archive\n"), "", alice, ""))
	if !r.Doc.Versioned {
		t.Error("a Markdown document was kept out of git")
	}
	if got := objectsOnDisk(t, dataDir, sp); len(got) != 2 { // "a few bytes" once, and "seven"
		t.Errorf("objects = %v, want 2", got)
	}
}

func isExtension(s string) bool {
	if len(s) < 2 || s[0] != '.' {
		return false
	}
	for _, r := range s[1:] {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// Audio and video, and the heavy binaries, are separate lists: an extension in both
// would have two answers to what it is.
func TestUnversionedListsAreDisjoint(t *testing.T) {
	for ext := range heavyTypes {
		if mediaTypes[ext] != "" {
			t.Errorf("%s is both media and a heavy binary", ext)
		}
		if !isExtension(ext) {
			t.Errorf("%q isn't a lower-case extension", ext)
		}
	}
	for ext := range mediaTypes {
		if !isExtension(ext) {
			t.Errorf("%q isn't a lower-case extension", ext)
		}
	}
}

// docs/api.md lists every extension that is always unversioned, and none that isn't.
func TestUnversionedTypesAreDocumented(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "api.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	start := strings.Index(doc, "<!-- unversioned-types -->")
	end := strings.Index(doc, "<!-- /unversioned-types -->")
	if start < 0 || end < start {
		t.Fatal("docs/api.md has no unversioned-types block")
	}
	// Every code span in the block that is an extension; one listed twice is a mistake.
	var listed []string
	for i, field := range strings.Split(doc[start:end], "`") {
		if i%2 == 1 && isExtension(field) {
			listed = append(listed, field)
		}
	}
	slices.Sort(listed)
	if want := UnversionedExtensions(); !slices.Equal(listed, want) {
		t.Errorf("docs/api.md lists %v\nthe code has %v", listed, want)
	}
}

func sortedOIDs(contents ...[]byte) []string {
	var oids []string
	for _, c := range contents {
		oids = append(oids, oidOf(c))
	}
	slices.Sort(oids)
	return oids
}

// Bytes that read as a pointer are kept as an unversioned file's, so they read back as
// they were, and gc never takes the bytes they would point to for its own.
func TestPointerShapedContent(t *testing.T) {
	s, _ := newStore(t, Options{})
	sp := writable(1)
	fake := []byte(pointerText([]byte("someone else's bytes")))
	r := must[Result](t)(s.Put(ctx, sp, "fake.txt", fake, "", alice, ""))
	if r.Doc.Versioned || r.Doc.Size != int64(len(fake)) {
		t.Fatalf("fake.txt = %+v", r.Doc)
	}
	if got := must[[]byte](t)(openBytes(t, s, sp, "fake.txt", "")); !bytes.Equal(got, fake) {
		t.Fatalf("read back %q", got)
	}
	// As Markdown it is refused: it would read as an older version taken out of history.
	// Anything else, even one byte more, is text like any other.
	_, err := s.Put(ctx, sp, "fake.md", fake, "", alice, "")
	wantErr(t, err, ErrInvalid)
	md := must[Result](t)(s.Put(ctx, sp, "fake.md", append(fake, '\n'), "", alice, ""))
	if !md.Doc.Versioned || readText(t, s, sp, "fake.md", "") != string(fake)+"\n" {
		t.Fatalf("fake.md = %+v", md.Doc)
	}
	for _, bad := range []string{
		strings.Replace(string(fake), "size ", "size 0", 1),
		strings.Replace(string(fake), "sha256:", "sha256:A", 1),
		string(fake) + "\n",
	} {
		if _, ok := parsePointer([]byte(bad)); ok {
			t.Errorf("%q parsed as a pointer", bad)
		}
	}
}

func TestPutFileStreams(t *testing.T) {
	s, dataDir := newStore(t, Options{LargeFileSize: 100, MaxFileSize: 1000})
	sp := writable(1)
	r := must[Result](t)(s.PutFile(ctx, sp, "a.bin", bytes.NewReader(bytesOf(500, 'a')), "", alice, ""))
	if r.Doc.Versioned || r.Doc.Size != 500 {
		t.Fatalf("a.bin = %+v", r.Doc)
	}
	r = must[Result](t)(s.PutFile(ctx, sp, "b.bin", bytes.NewReader(bytesOf(50, 'b')), "", alice, ""))
	if !r.Doc.Versioned {
		t.Fatalf("b.bin = %+v, want versioned", r.Doc)
	}
	r = must[Result](t)(s.PutFile(ctx, sp, "c.md", strings.NewReader("# C\n"), "", alice, ""))
	if !r.Doc.Versioned || readText(t, s, sp, "c.md", "") != "# C\n" {
		t.Fatalf("c.md = %+v", r.Doc)
	}
	_, err := s.PutFile(ctx, sp, "huge.bin", bytes.NewReader(bytesOf(1001, 'h')), "", alice, "")
	wantErr(t, err, ErrTooLarge)
	_, err = s.PutFile(ctx, sp, "long.md", bytes.NewReader(bytesOf(101, 'l')), "", alice, "")
	wantErr(t, err, ErrTooLarge)
	_, err = s.PutFile(ctx, sp, "a.bin", bytes.NewReader(bytesOf(600, 'x')), "", alice, "")
	wantErr(t, err, ErrConflict)
	// Nothing staged is left behind.
	entries, err := os.ReadDir(filepath.Join(sp.Dir(dataDir), filepath.FromSlash(tmpDir)))
	if err != nil || len(entries) != 0 {
		t.Fatalf("staged files left: %v, %v", entries, err)
	}
	if got, want := objectsOnDisk(t, dataDir, sp), sortedOIDs(bytesOf(500, 'a')); !slices.Equal(got, want) {
		t.Fatalf("objects = %v, want %v", got, want)
	}
}

func TestUnversionedLimit(t *testing.T) {
	s, _ := newStore(t, Options{LargeFileSize: 10, MaxUnversionedSize: 500})
	sp := writable(1)
	r := must[Result](t)(s.Put(ctx, sp, "a.bin", bytesOf(300, 'a'), "", alice, ""))
	_, err := s.Put(ctx, sp, "b.bin", bytesOf(300, 'b'), "", alice, "")
	wantErr(t, err, ErrRepoFull)
	// Replacing a file counts only the new bytes: the old ones go with the change.
	r = must[Result](t)(s.Put(ctx, sp, "a.bin", bytesOf(400, 'A'), r.Doc.Version, alice, ""))
	must[Result](t)(s.Delete(ctx, sp, "a.bin", r.Doc.Version, alice, ""))
	must[Result](t)(s.Put(ctx, sp, "b.bin", bytesOf(300, 'b'), "", alice, ""))
}

// What a process that stopped mid-change or mid-upload leaves, bytes nothing points to and
// old staged uploads, goes at the next change that collects.
func TestLeftoversAreCollected(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	sp := writable(1)
	must[Result](t)(s.Put(ctx, sp, "keep.mp3", []byte("song"), "", alice, ""))
	dir := sp.Dir(dataDir)
	orphan := []byte("adopted, never committed")
	orphanPath := filepath.Join(dir, filepath.FromSlash(objectPath(oidOf(orphan))))
	staleUpload := filepath.Join(dir, filepath.FromSlash(tmpDir), "upload-stale")
	freshUpload := filepath.Join(dir, filepath.FromSlash(tmpDir), "upload-fresh")
	for _, p := range []string{orphanPath, staleUpload, freshUpload} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, orphan, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * tmpMaxAge)
	if err := os.Chtimes(staleUpload, old, old); err != nil {
		t.Fatal(err)
	}

	must[Result](t)(s.Put(ctx, sp, "other.bin", []byte("x"), "", alice, ""))
	must[Result](t)(s.Delete(ctx, sp, "other.bin", "", alice, ""))
	if got, want := objectsOnDisk(t, dataDir, sp), sortedOIDs([]byte("song")); !slices.Equal(got, want) {
		t.Fatalf("objects = %v, want %v", got, want)
	}
	if _, err := os.Stat(staleUpload); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the stale upload is still there: %v", err)
	}
	if _, err := os.Stat(freshUpload); err != nil {
		t.Errorf("an upload that may still be running was removed: %v", err)
	}
}

// .git's own limit doesn't count the unversioned files' bytes, which have theirs.
func TestRepoLimitLeavesOutUnversionedBytes(t *testing.T) {
	s, _ := newStore(t, Options{LargeFileSize: 10, MaxRepoSize: 16 << 10})
	sp := writable(1)
	for i := range 3 {
		must[Result](t)(s.Put(ctx, sp, "v"+string(rune('a'+i))+".wav", bytesOf(64<<10, byte('a'+i)), "", alice, ""))
	}
}
