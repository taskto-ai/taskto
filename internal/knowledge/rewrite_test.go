package knowledge

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"taskto.ai/taskto/internal/access"
)

// tick makes commit times one minute apart, each in a time zone of its own, so a rewrite
// that lost a date or a zone shows.
func tick(t *testing.T) {
	clock := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	n := 0
	now = func() time.Time {
		n++
		clock = clock.Add(time.Minute)
		return clock.In(time.FixedZone("", (n%5-2)*3600))
	}
	t.Cleanup(func() { now = time.Now })
}

// snapshot is a commit of main as git has it: everything but its ID and parents, with its
// files' blobs by path.
type snapshot struct {
	id                plumbing.Hash
	author, committer string
	message           string
	files             map[string]plumbing.Hash
}

func signature(s object.Signature) string {
	return fmt.Sprintf("%s <%s> %d %s", s.Name, s.Email, s.When.Unix(), s.When.Format("-0700"))
}

// history returns main's commits, newest first, as git has them.
func gitHistory(t *testing.T, dataDir string, sp access.Space) []snapshot {
	t.Helper()
	repo, err := git.PlainOpen(sp.Dir(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := repo.Reference(branch, false)
	if err != nil {
		t.Fatal(err)
	}
	var out []snapshot
	for h := ref.Hash(); !h.IsZero(); {
		c, err := repo.CommitObject(h)
		if err != nil {
			t.Fatal(err)
		}
		snap := snapshot{id: c.Hash, author: signature(c.Author), committer: signature(c.Committer), message: c.Message, files: map[string]plumbing.Hash{}}
		tree, err := c.Tree()
		if err != nil {
			t.Fatal(err)
		}
		err = tree.Files().ForEach(func(f *object.File) error {
			snap.files[f.Name] = f.Hash
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, snap)
		h = plumbing.ZeroHash
		if len(c.ParentHashes) > 0 {
			h = c.ParentHashes[0]
		}
	}
	return out
}

func blobHash(content []byte) plumbing.Hash {
	return plumbing.ComputeHash(plumbing.BlobObject, content)
}

func pointerBlob(content []byte) plumbing.Hash { return blobHash([]byte(pointerText(content))) }

// hasObject reports whether the space's git has the object h, loose or packed.
func hasGitObject(t *testing.T, dataDir string, sp access.Space, h plumbing.Hash) bool {
	t.Helper()
	repo, err := git.PlainOpen(sp.Dir(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	return repo.Storer.HasEncodedObject(h) == nil
}

// checkRewritten checks that after is before with every blob of name in old (but the
// current one) replaced by its pointer, and nothing else: the same commits, in the same
// order, by the same people at the same times, with the same messages and files. It
// returns the commits whose ID changed.
func checkRewritten(t *testing.T, before, after []snapshot, name string, old map[plumbing.Hash][]byte) int {
	t.Helper()
	if len(after) != len(before) {
		t.Fatalf("%d commits after the rewrite, want %d", len(after), len(before))
	}
	changed := 0
	for i := range before {
		b, a := before[i], after[i]
		if a.author != b.author || a.committer != b.committer || a.message != b.message {
			t.Errorf("commit %d: %s / %s / %q, want %s / %s / %q", i, a.author, a.committer, a.message, b.author, b.committer, b.message)
		}
		want := map[string]plumbing.Hash{}
		for p, h := range b.files {
			want[p] = h
			if content, ok := old[h]; ok && p == name {
				want[p] = pointerBlob(content)
			}
		}
		if fmt.Sprint(a.files) != fmt.Sprint(want) {
			t.Errorf("commit %d's files = %v, want %v", i, a.files, want)
		}
		if a.id != b.id {
			changed++
		}
	}
	return changed
}

// A versioned file whose new content is over the limit leaves git history: every older
// version becomes a pointer whose bytes are gone, and nothing else in any commit changes.
func TestGrownFileLeavesHistory(t *testing.T) {
	tick(t)
	s, dataDir := newStore(t, Options{LargeFileSize: 100})
	sp := writable(1)
	first := must[Result](t)(s.Put(ctx, sp, "notes.md", []byte("# Notes\n"), "", bob, "Start"))
	v1, v2 := []byte("version one, private"), []byte("version two, private")
	r1 := must[Result](t)(s.Put(ctx, sp, "data/file.bin", v1, "", alice, ""))
	notes := must[Result](t)(s.Put(ctx, sp, "notes.md", []byte("# Notes\nmore\n"), first.Doc.Version, Author{Name: "Bob", Email: "bob@example.com", Agent: "Helper"}, ""))
	r2 := must[Result](t)(s.Put(ctx, sp, "data/file.bin", v2, r1.Doc.Version, alice, "Second"))
	must[Result](t)(s.Put(ctx, sp, "data/other.bin", v1, "", bob, "")) // the same bytes, at another path
	before := gitHistory(t, dataDir, sp)

	big := bytesOf(500, 'g')
	r3 := must[Result](t)(s.Put(ctx, sp, "data/file.bin", big, r2.Doc.Version, alice, "Grow"))
	if r3.Doc.Versioned {
		t.Fatalf("the grown version is versioned: %+v", r3.Doc)
	}
	after := gitHistory(t, dataDir, sp)
	if r3.Commit != after[0].id.String() {
		t.Fatalf("the change returned %s, main is at %s", r3.Commit, after[0].id)
	}
	// The new commit is the last; the ones before it are the old ones, rewritten.
	changed := checkRewritten(t, before, after[1:], "data/file.bin", map[plumbing.Hash][]byte{blobHash(v1): v1, blobHash(v2): v2})
	if changed != 4 {
		t.Errorf("%d commits changed ID, want the 4 from the file's first version on", changed)
	}
	if after[len(after)-1].id != before[len(before)-1].id {
		t.Error("the commit before the file's first version changed ID")
	}

	// Its old versions are gone: their bytes can't be read or restored, and git has only
	// the copy at the other path.
	for _, at := range []string{after[2].id.String(), after[4].id.String()} {
		_, err := openBytes(t, s, sp, "data/file.bin", at)
		wantErr(t, err, ErrNotKept)
		_, err = s.Restore(ctx, sp, "data/file.bin", at, "", alice, "")
		wantErr(t, err, ErrNotKept)
		if d := must[Doc](t)(s.Read(ctx, sp, "data/file.bin", at)); d.Versioned || d.Size != int64(len(v1)) {
			t.Errorf("old version's entry = %+v", d.Entry)
		}
	}
	if hasGitObject(t, dataDir, sp, blobHash(v2)) {
		t.Error("git still has the old version's bytes")
	}
	if !hasGitObject(t, dataDir, sp, blobHash(v1)) {
		t.Error("the same bytes at another path were deleted")
	}
	for _, old := range before[:4] {
		if hasGitObject(t, dataDir, sp, old.id) {
			t.Errorf("git still has the old commit %s", old.id)
		}
		_, err := s.Read(ctx, sp, "notes.md", old.id.String())
		wantErr(t, err, ErrNotFound)
	}

	// Its history still lists every change, by whom, when.
	h := must[History](t)(s.History(ctx, sp, "data/file.bin", "", 0))
	if len(h.Commits) != 3 || h.Commits[1].Message != "Second" || h.Commits[2].Author.Name != "Alice" {
		t.Fatalf("history = %+v", h.Commits)
	}
	// Other documents keep their versions, so their editors' base_version still holds.
	must[Result](t)(s.Put(ctx, sp, "notes.md", []byte("# Notes\nedited\n"), notes.Doc.Version, bob, ""))
	checkClean(t, dataDir, sp, map[string]string{
		"data/file.bin": pointerText(big), "data/other.bin": string(v1), "notes.md": "# Notes\nedited\n",
	})
	if _, err := os.Stat(filepath.Join(sp.Dir(dataDir), filepath.FromSlash(rewriteFile))); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the journal is still there: %v", err)
	}
	gitFsck(t, dataDir, sp)
}

// gitFsck checks the repository with git itself, if it is installed.
func gitFsck(t *testing.T, dataDir string, sp access.Space) {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return
	}
	cmd := exec.Command(gitBin, "-c", "safe.directory=*", "fsck", "--strict", "--no-dangling")
	cmd.Dir = sp.Dir(dataDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git fsck: %v\n%s", err, out)
	}
}

// An editor holding a version from before the rewrite gets a conflict, never a silent
// overwrite: the file's version is new, and commits from before are gone.
func TestStaleBaseVersionAfterRewrite(t *testing.T) {
	s, _ := newStore(t, Options{LargeFileSize: 100})
	sp := writable(1)
	r1 := must[Result](t)(s.Put(ctx, sp, "file.bin", []byte("small"), "", alice, ""))
	stale := r1.Doc.Version
	must[Result](t)(s.Put(ctx, sp, "file.bin", bytesOf(200, 'b'), stale, bob, ""))

	_, err := s.Put(ctx, sp, "file.bin", []byte("an editor's save"), stale, alice, "")
	wantErr(t, err, ErrConflict)
	_, err = s.Delete(ctx, sp, "file.bin", stale, alice, "")
	wantErr(t, err, ErrConflict)
	_, err = s.Move(ctx, sp, "file.bin", "moved.bin", stale, alice, "")
	wantErr(t, err, ErrConflict)
	// A history link or restore from before the rewrite names a commit that is gone.
	_, err = s.Restore(ctx, sp, "file.bin", r1.Commit, "", alice, "")
	wantErr(t, err, ErrNotFound)
	_, err = s.Read(ctx, sp, "file.bin", r1.Commit)
	wantErr(t, err, ErrNotFound)
	if got := must[[]byte](t)(openBytes(t, s, sp, "file.bin", "")); !bytes.Equal(got, bytesOf(200, 'b')) {
		t.Fatal("the current version was overwritten")
	}
}

// A versioned file renamed to a kind that is always unversioned moves its bytes out of
// git, and its older versions, under its old name, leave history.
func TestRenameToUnversionedKindLeavesHistory(t *testing.T) {
	tick(t)
	s, dataDir := newStore(t, Options{})
	sp := writable(1)
	v1, v2 := []byte("layers, first"), []byte("layers, second")
	r := must[Result](t)(s.Put(ctx, sp, "art/cover.dat", v1, "", alice, ""))
	r = must[Result](t)(s.Put(ctx, sp, "art/cover.dat", v2, r.Doc.Version, alice, ""))
	before := gitHistory(t, dataDir, sp)

	moved := must[Result](t)(s.Move(ctx, sp, "art/cover.dat", "art/cover.PSD", r.Doc.Version, alice, ""))
	if moved.Doc.Versioned || moved.Doc.Size != int64(len(v2)) {
		t.Fatalf("moved = %+v", moved.Doc)
	}
	after := gitHistory(t, dataDir, sp)
	if moved.Commit != after[0].id.String() {
		t.Fatalf("the move returned %s, main is at %s", moved.Commit, after[0].id)
	}
	checkRewritten(t, before, after[1:], "art/cover.dat", map[plumbing.Hash][]byte{blobHash(v1): v1, blobHash(v2): v2})
	if got := must[[]byte](t)(openBytes(t, s, sp, "art/cover.PSD", "")); !bytes.Equal(got, v2) {
		t.Fatalf("the moved file reads %q", got)
	}
	_, err := openBytes(t, s, sp, "art/cover.dat", after[2].id.String())
	wantErr(t, err, ErrNotKept)
	if hasGitObject(t, dataDir, sp, blobHash(v1)) || hasGitObject(t, dataDir, sp, blobHash(v2)) {
		t.Error("git still has the old versions' bytes")
	}
	checkClean(t, dataDir, sp, map[string]string{"art/cover.PSD": pointerText(v2)})
	gitFsck(t, dataDir, sp)

	// A Markdown document renamed so leaves too; its old versions read as such.
	md := must[Result](t)(s.Put(ctx, sp, "draft.md", []byte("# Draft\n"), "", alice, ""))
	md = must[Result](t)(s.Put(ctx, sp, "draft.md", []byte("# Draft\nsecond\n"), md.Doc.Version, alice, ""))
	must[Result](t)(s.Move(ctx, sp, "draft.md", "draft.zip", "", alice, ""))
	h := must[History](t)(s.History(ctx, sp, "draft.md", "", 0))
	old := must[Doc](t)(s.Read(ctx, sp, "draft.md", h.Commits[1].Commit))
	if old.Versioned || old.Content != nil {
		t.Fatalf("an old version of draft.md = %+v, %q", old.Entry, old.Content)
	}
}

// A rewrite stopped at any step, as by a crash, is finished at the next start, or at the
// next write; one planned for a change that never landed is dropped.
func TestRewriteRecoversFromACrash(t *testing.T) {
	for _, step := range []string{"objects", "ref"} {
		t.Run(step, func(t *testing.T) {
			s, dataDir := newStore(t, Options{LargeFileSize: 100})
			sp := writable(1)
			v1 := []byte("small, private")
			r := must[Result](t)(s.Put(ctx, sp, "f.bin", v1, "", alice, ""))
			before := gitHistory(t, dataDir, sp)

			rewriteFault = func(at string) error {
				if at == step {
					return errors.New("crash")
				}
				return nil
			}
			t.Cleanup(func() { rewriteFault = nil })
			big := bytesOf(200, 'x')
			must[Result](t)(s.Put(ctx, sp, "f.bin", big, r.Doc.Version, alice, ""))
			rewriteFault = nil
			if !hasGitObject(t, dataDir, sp, blobHash(v1)) {
				t.Fatal("the crash came after the old version was deleted")
			}

			// The next process.
			s = New(dataDir, Options{LargeFileSize: 100})
			if err := s.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			after := gitHistory(t, dataDir, sp)
			checkRewritten(t, before, after[1:], "f.bin", map[plumbing.Hash][]byte{blobHash(v1): v1})
			if hasGitObject(t, dataDir, sp, blobHash(v1)) || hasGitObject(t, dataDir, sp, before[0].id) {
				t.Error("the old version is still in git")
			}
			if _, err := os.Stat(filepath.Join(sp.Dir(dataDir), filepath.FromSlash(rewriteFile))); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the journal is still there: %v", err)
			}
			checkClean(t, dataDir, sp, map[string]string{"f.bin": pointerText(big)})
			gitFsck(t, dataDir, sp)
		})
	}

	t.Run("next write", func(t *testing.T) {
		s, dataDir := newStore(t, Options{LargeFileSize: 100})
		sp := writable(1)
		r := must[Result](t)(s.Put(ctx, sp, "f.bin", []byte("small"), "", alice, ""))
		rewriteFault = func(string) error { return errors.New("crash") }
		t.Cleanup(func() { rewriteFault = nil })
		must[Result](t)(s.Put(ctx, sp, "f.bin", bytesOf(200, 'x'), r.Doc.Version, alice, ""))
		rewriteFault = nil
		must[Result](t)(s.Put(ctx, sp, "other.md", []byte("next"), "", alice, ""))
		if hasGitObject(t, dataDir, sp, blobHash([]byte("small"))) {
			t.Error("the next write didn't finish the rewrite")
		}
	})

	t.Run("change never landed", func(t *testing.T) {
		s, dataDir := newStore(t, Options{LargeFileSize: 100})
		sp := writable(1)
		must[Result](t)(s.Put(ctx, sp, "f.bin", []byte("small"), "", alice, ""))
		before := gitHistory(t, dataDir, sp)
		journal := rewritePlan{after: before[0].id.String(), paths: []string{"f.bin"}}.encode()
		if err := os.WriteFile(filepath.Join(sp.Dir(dataDir), filepath.FromSlash(rewriteFile)), journal, 0o600); err != nil {
			t.Fatal(err)
		}
		s = New(dataDir, Options{LargeFileSize: 100})
		if err := s.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		if after := gitHistory(t, dataDir, sp); after[0].id != before[0].id || !hasGitObject(t, dataDir, sp, blobHash([]byte("small"))) {
			t.Fatal("a rewrite was done for a change that never landed")
		}
		if _, err := os.Stat(filepath.Join(sp.Dir(dataDir), filepath.FromSlash(rewriteFile))); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the journal is still there: %v", err)
		}
	})
}

// Writers and readers of the space wait for a rewrite and see the history before it or
// after it, never half of it; no change is lost.
func TestRewriteWithConcurrentWrites(t *testing.T) {
	s, dataDir := newStore(t, Options{LargeFileSize: 100})
	sp := writable(1)
	var base string
	for i := range 20 {
		r := must[Result](t)(s.Put(ctx, sp, "grows.bin", fmt.Appendf(nil, "small %d", i), base, alice, ""))
		base = r.Doc.Version
	}

	const n = 8
	var wg sync.WaitGroup
	wg.Go(func() {
		if _, err := s.Put(ctx, sp, "grows.bin", bytesOf(500, 'g'), base, alice, ""); err != nil {
			t.Error(err)
		}
	})
	for i := range n {
		wg.Go(func() {
			if _, err := s.Put(ctx, sp, fmt.Sprintf("own/%d.md", i), []byte("mine"), "", bob, ""); err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() {
			h, err := s.History(ctx, sp, "grows.bin", "", 0)
			if err != nil {
				t.Error(err)
				return
			}
			// Every commit it lists can be read: none is from a history half rewritten.
			for _, c := range h.Commits {
				if _, err := s.Read(ctx, sp, "grows.bin", c.Commit); err != nil && !errors.Is(err, ErrNotFound) {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()

	h := must[History](t)(s.History(ctx, sp, "", "", 0))
	if len(h.Commits) != 20+1+n {
		t.Fatalf("%d commits, want %d", len(h.Commits), 21+n)
	}
	for i := range 20 {
		if hasGitObject(t, dataDir, sp, blobHash(fmt.Appendf(nil, "small %d", i))) {
			t.Fatalf("version %d is still in git", i)
		}
	}
	files := map[string]string{"grows.bin": pointerText(bytesOf(500, 'g'))}
	for i := range n {
		files[fmt.Sprintf("own/%d.md", i)] = "mine"
	}
	checkClean(t, dataDir, sp, files)
	gitFsck(t, dataDir, sp)
}

// A history that git gc packed (outside the server) loses the old versions too: the packs
// are rewritten with only what main reaches.
func TestRewritePrunesPacks(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git isn't installed")
	}
	s, dataDir := newStore(t, Options{LargeFileSize: 100})
	sp := writable(1)
	r := must[Result](t)(s.Put(ctx, sp, "f.bin", []byte("packed, private"), "", alice, ""))
	must[Result](t)(s.Put(ctx, sp, "a.md", []byte("# A\n"), "", alice, ""))
	cmd := exec.Command(gitBin, "-c", "safe.directory=*", "gc", "--quiet")
	cmd.Dir = sp.Dir(dataDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git gc: %v\n%s", err, out)
	}

	must[Result](t)(s.Put(ctx, sp, "f.bin", bytesOf(200, 'x'), r.Doc.Version, alice, ""))
	if hasGitObject(t, dataDir, sp, blobHash([]byte("packed, private"))) {
		t.Fatal("the old version is still in a pack")
	}
	if got := readText(t, s, sp, "a.md", ""); got != "# A\n" {
		t.Fatalf("a.md = %q", got)
	}
	gitFsck(t, dataDir, sp)
}

// The rewrite is idempotent: done again, it changes nothing.
func TestRewriteIsIdempotent(t *testing.T) {
	s, dataDir := newStore(t, Options{LargeFileSize: 100})
	sp := writable(1)
	r := must[Result](t)(s.Put(ctx, sp, "f.bin", []byte("small"), "", alice, ""))
	must[Result](t)(s.Put(ctx, sp, "f.bin", bytesOf(200, 'x'), r.Doc.Version, alice, ""))
	before := gitHistory(t, dataDir, sp)
	err := s.write(sp, 0, false, func(r *repo) error {
		head, err := r.head()
		if err != nil {
			return err
		}
		stats, err := r.rewriteHistory(head, []string{"f.bin"}, nil)
		if stats.rewritten != 0 || stats.head != head.Hash {
			t.Errorf("a second rewrite changed %d commits", stats.rewritten)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if after := gitHistory(t, dataDir, sp); after[0].id != before[0].id {
		t.Fatal("main moved")
	}
}

// A rewrite's cost grows with the history: about a commit and a tree per commit.
func BenchmarkRewrite(b *testing.B) {
	for _, commits := range []int{100, 1000} {
		b.Run(fmt.Sprint(commits), func(b *testing.B) {
			for b.Loop() {
				b.StopTimer()
				s := New(b.TempDir(), Options{LargeFileSize: 100})
				sp := writable(1)
				var base string
				for i := range commits {
					name := fmt.Sprintf("notes/%d.md", i%50)
					if i%10 == 0 {
						name = "grows.bin"
					}
					cur := ""
					if name == "grows.bin" {
						cur = base
					} else if i >= 50 {
						d, err := s.Read(ctx, sp, name, "")
						if err != nil {
							b.Fatal(err)
						}
						cur = d.Version
					}
					r, err := s.Put(ctx, sp, name, fmt.Appendf(nil, "content %d", i), cur, alice, "")
					if err != nil {
						b.Fatal(err)
					}
					if name == "grows.bin" {
						base = r.Doc.Version
					}
				}
				b.StartTimer()
				if _, err := s.Put(ctx, sp, "grows.bin", bytesOf(200, 'g'), base, alice, ""); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// An admin takes a document's old versions out of history, say for personal data saved by
// mistake: its current version stays, every other is gone for good, and the history says
// who did it.
func TestRemoveOldVersions(t *testing.T) {
	tick(t)
	s, dataDir := newStore(t, Options{})
	sp := writable(1)
	clean := []byte("# Contacts\n\nAsk the front desk.\n")
	leaked := []byte("# Contacts\n\nJane: +1 555 0100, jane@example.com\n")
	r := must[Result](t)(s.Put(ctx, sp, "contacts.md", clean, "", alice, ""))
	r = must[Result](t)(s.Put(ctx, sp, "contacts.md", leaked, r.Doc.Version, alice, ""))
	must[Result](t)(s.Put(ctx, sp, "other.md", []byte("# Other\n"), "", bob, ""))
	r = must[Result](t)(s.Put(ctx, sp, "contacts.md", clean, r.Doc.Version, alice, "Take the number out"))
	before := gitHistory(t, dataDir, sp)

	// The current version must be the one the admin saw.
	_, err := s.RemoveOldVersions(ctx, sp, "contacts.md", blobHash(leaked).String(), bob)
	wantErr(t, err, ErrConflict)

	res := must[Removed](t)(s.RemoveOldVersions(ctx, sp, "contacts.md", r.Doc.Version, bob))
	if res.Removed != 1 || res.Doc == nil || res.Doc.Version != r.Doc.Version || !res.Doc.Versioned {
		t.Fatalf("removed = %+v", res)
	}
	after := gitHistory(t, dataDir, sp)
	if res.Commit != after[0].id.String() || after[0].message != "Remove the old versions of contacts.md from history\n" ||
		!strings.HasPrefix(after[0].author, "Bob <bob@example.com> ") {
		t.Fatalf("the record = %+v, want by Bob, at %s", after[0], res.Commit)
	}
	if fmt.Sprint(after[0].files) != fmt.Sprint(before[0].files) {
		t.Fatal("the record changed a file")
	}
	// The same version as now, earlier on, isn't an old version: it stays readable.
	checkRewritten(t, before, after[1:], "contacts.md", map[plumbing.Hash][]byte{blobHash(leaked): leaked})
	if hasGitObject(t, dataDir, sp, blobHash(leaked)) {
		t.Fatal("git still has the leaked version")
	}
	h := must[History](t)(s.History(ctx, sp, "contacts.md", "", 0))
	if len(h.Commits) != 3 || *h.Commits[1].Versioned || !*h.Commits[0].Versioned || !*h.Commits[2].Versioned {
		t.Fatalf("history = %+v", h.Commits)
	}
	old := must[Doc](t)(s.Read(ctx, sp, "contacts.md", h.Commits[1].Commit))
	if old.Versioned || old.Content != nil || old.Size != int64(len(leaked)) {
		t.Fatalf("the removed version reads %+v, %q", old.Entry, old.Content)
	}
	_, err = s.Restore(ctx, sp, "contacts.md", h.Commits[1].Commit, "", alice, "")
	wantErr(t, err, ErrNotKept)
	if got := readText(t, s, sp, "contacts.md", h.Commits[2].Commit); got != string(clean) {
		t.Fatalf("the first version reads %q", got)
	}
	checkClean(t, dataDir, sp, map[string]string{"contacts.md": string(clean), "other.md": "# Other\n"})
	gitFsck(t, dataDir, sp)

	// Again: nothing left to take out, and nothing happens.
	again := must[Removed](t)(s.RemoveOldVersions(ctx, sp, "contacts.md", "", bob))
	if again.Removed != 0 || again.Commit != res.Commit {
		t.Fatalf("again = %+v", again)
	}

	// A deleted file loses every version.
	must[Result](t)(s.Delete(ctx, sp, "other.md", "", bob, ""))
	gone := must[Removed](t)(s.RemoveOldVersions(ctx, sp, "other.md", "", bob))
	if gone.Removed != 1 || gone.Doc != nil {
		t.Fatalf("deleted file = %+v", gone)
	}
	if hasGitObject(t, dataDir, sp, blobHash([]byte("# Other\n"))) {
		t.Fatal("git still has the deleted file")
	}

	_, err = s.RemoveOldVersions(ctx, sp, "never.md", "", bob)
	wantErr(t, err, ErrNotFound)
	must[Result](t)(s.Put(ctx, sp, "dir/a.md", []byte("a"), "", bob, ""))
	_, err = s.RemoveOldVersions(ctx, sp, "dir", "", bob)
	wantErr(t, err, ErrInvalid)
	_, err = s.RemoveOldVersions(ctx, access.Space{ID: 1, WorkspaceID: 1, Access: access.Read}, "contacts.md", "", bob)
	wantErr(t, err, ErrReadOnly)
}

// A space whose repository is full can still have old versions taken out: it frees space.
func TestRemoveOldVersionsInAFullSpace(t *testing.T) {
	s, _ := newStore(t, Options{MaxRepoSize: 24 << 10})
	sp := writable(1)
	// Bytes that don't compress, so each version takes its size.
	noise := rand.NewChaCha8([32]byte{})
	var base string
	var err error
	for err == nil {
		content := make([]byte, 2<<10)
		_, _ = noise.Read(content)
		var r Result
		if r, err = s.Put(ctx, sp, "log.bin", content, base, alice, ""); err == nil {
			base = r.Doc.Version
		}
	}
	wantErr(t, err, ErrRepoFull)
	res := must[Removed](t)(s.RemoveOldVersions(ctx, sp, "log.bin", "", alice))
	if res.Removed == 0 {
		t.Fatal("nothing removed")
	}
	must[Result](t)(s.Put(ctx, sp, "next.txt", []byte("room again"), "", alice, ""))
}

// A ref other than main, made with git outside the server against the rules, never ends
// up pointing at deleted objects: what it reaches is kept (and logged), the rest goes.
func TestRewriteKeepsWhatOtherRefsReach(t *testing.T) {
	s, dataDir := newStore(t, Options{LargeFileSize: 100})
	sp := writable(1)
	r1 := must[Result](t)(s.Put(ctx, sp, "f.bin", []byte("tagged"), "", alice, ""))
	r2 := must[Result](t)(s.Put(ctx, sp, "f.bin", []byte("untagged"), r1.Doc.Version, alice, ""))
	repo, err := git.PlainOpen(sp.Dir(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Storer.SetReference(plumbing.NewHashReference("refs/tags/old", plumbing.NewHash(r1.Commit))); err != nil {
		t.Fatal(err)
	}
	must[Result](t)(s.Put(ctx, sp, "f.bin", bytesOf(200, 'x'), r2.Doc.Version, alice, ""))
	if !hasGitObject(t, dataDir, sp, plumbing.NewHash(r1.Commit)) || !hasGitObject(t, dataDir, sp, blobHash([]byte("tagged"))) {
		t.Fatal("what the tag reaches was deleted")
	}
	if hasGitObject(t, dataDir, sp, blobHash([]byte("untagged"))) {
		t.Fatal("a version no ref reaches is still in git")
	}
	gitFsck(t, dataDir, sp)
}
