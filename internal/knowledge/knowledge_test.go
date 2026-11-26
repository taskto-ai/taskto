package knowledge

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"taskto.ai/taskto/internal/access"
)

var (
	ctx   = context.Background()
	alice = Author{Name: "Alice", Email: "alice@example.com"}
	bob   = Author{Name: "Bob", Email: "bob@example.com"}
)

// writable is a space its caller may write.
func writable(id int64) access.Space {
	return access.Space{ID: id, WorkspaceID: 1, Kind: access.Shared, Access: access.Write}
}

func newStore(t *testing.T, opts Options) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	return New(dir, opts), dir
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("err = %v, want %v", err, target)
	}
}

func readText(t *testing.T, s *Store, sp access.Space, name, at string) string {
	t.Helper()
	return string(must[Doc](t)(s.Read(ctx, sp, name, at)).Content)
}

// checkClean checks that the space's directory is a clean checkout of main, as git
// itself sees it, and that its files are exactly want.
func checkClean(t *testing.T, dataDir string, sp access.Space, want map[string]string) {
	t.Helper()
	dir := sp.Dir(dataDir)
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	status, err := wt.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !status.IsClean() {
		t.Errorf("working tree isn't clean:\n%s", status)
	}
	got := map[string]string{}
	files := os.DirFS(dir)
	err = fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && d.Name() == gitDir:
			return fs.SkipDir
		case d.IsDir():
			return nil
		}
		b, err := fs.ReadFile(files, p)
		got[p] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("files = %v, want %v", got, want)
	}
}

func TestCRUD(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	sp := writable(1)

	// Nothing is created until the first write.
	if page := must[Page](t)(s.List(ctx, sp, "", "", 0)); len(page.Docs) != 0 {
		t.Fatalf("new space lists %v", page.Docs)
	}
	_, err := s.Read(ctx, sp, "a.md", "")
	wantErr(t, err, ErrNotFound)
	if _, err := os.Lstat(filepath.Join(sp.Dir(dataDir), gitDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("reading created the repository: %v", err)
	}

	created := must[Result](t)(s.Put(ctx, sp, "notes/plan.md", []byte("# Plan\n"), "", alice, ""))
	if created.Doc == nil || created.Doc.Path != "notes/plan.md" || created.Doc.Size != 7 || len(created.Commit) != 40 {
		t.Fatalf("create = %+v", created)
	}
	must[Result](t)(s.Put(ctx, sp, "logo.png", []byte{0x89, 'P', 'N', 'G', 0}, "", alice, "Add the logo"))
	doc := must[Doc](t)(s.Read(ctx, sp, "notes/plan.md", ""))
	if string(doc.Content) != "# Plan\n" || doc.Version != created.Doc.Version {
		t.Fatalf("read = %+v", doc)
	}

	updated := must[Result](t)(s.Put(ctx, sp, "notes/plan.md", []byte("# Plan\n\nv2\n"), doc.Version, bob, ""))
	if updated.Doc.Version == doc.Version {
		t.Fatal("the version didn't change")
	}
	// Writing the same content again changes nothing and commits nothing.
	same := must[Result](t)(s.Put(ctx, sp, "notes/plan.md", []byte("# Plan\n\nv2\n"), updated.Doc.Version, bob, ""))
	if same.Commit != updated.Commit {
		t.Fatalf("an unchanged document made commit %s", same.Commit)
	}

	page := must[Page](t)(s.List(ctx, sp, "", "", 0))
	if len(page.Docs) != 2 || page.Docs[0].Path != "logo.png" || page.Docs[1].Path != "notes/plan.md" || page.Next != "" {
		t.Fatalf("list = %+v", page)
	}
	if page.Docs[1].Version != updated.Doc.Version || page.Docs[1].Size != 11 {
		t.Fatalf("listed %+v, want version %s and size 11", page.Docs[1], updated.Doc.Version)
	}
	checkClean(t, dataDir, sp, map[string]string{"logo.png": "\x89PNG\x00", "notes/plan.md": "# Plan\n\nv2\n"})

	moved := must[Result](t)(s.Move(ctx, sp, "notes/plan.md", "archive/2026/plan.md", updated.Doc.Version, alice, ""))
	if moved.Doc == nil || moved.Doc.Path != "archive/2026/plan.md" || moved.Doc.Version != updated.Doc.Version {
		t.Fatalf("move = %+v", moved)
	}
	_, err = s.Read(ctx, sp, "notes/plan.md", "")
	wantErr(t, err, ErrNotFound)
	checkClean(t, dataDir, sp, map[string]string{"logo.png": "\x89PNG\x00", "archive/2026/plan.md": "# Plan\n\nv2\n"})

	must[Result](t)(s.Delete(ctx, sp, "archive/2026/plan.md", "", alice, ""))
	must[Result](t)(s.Delete(ctx, sp, "logo.png", "", alice, ""))
	if page := must[Page](t)(s.List(ctx, sp, "", "", 0)); len(page.Docs) != 0 {
		t.Fatalf("after deleting everything, list = %v", page.Docs)
	}
	_, err = s.Delete(ctx, sp, "logo.png", "", alice, "")
	wantErr(t, err, ErrNotFound)
	checkClean(t, dataDir, sp, map[string]string{})
}

func TestConflicts(t *testing.T) {
	s, _ := newStore(t, Options{})
	sp := writable(1)
	v1 := must[Result](t)(s.Put(ctx, sp, "a.md", []byte("one"), "", alice, "")).Doc.Version

	// Two people edit version 1; the second to save gets a conflict, not an overwrite.
	must[Result](t)(s.Put(ctx, sp, "a.md", []byte("alice's"), v1, alice, ""))
	_, err := s.Put(ctx, sp, "a.md", []byte("bob's"), v1, bob, "")
	wantErr(t, err, ErrConflict)
	if got := readText(t, s, sp, "a.md", ""); got != "alice's" {
		t.Fatalf("content = %q, want alice's edit kept", got)
	}

	tests := []struct {
		name string
		do   func() error
	}{
		{"create over an existing document", func() error {
			_, err := s.Put(ctx, sp, "a.md", []byte("x"), "", bob, "")
			return err
		}},
		{"update a deleted document", func() error {
			_, err := s.Put(ctx, sp, "gone.md", []byte("x"), v1, bob, "")
			return err
		}},
		{"move from an old version", func() error {
			_, err := s.Move(ctx, sp, "a.md", "b.md", v1, bob, "")
			return err
		}},
		{"delete an old version", func() error {
			_, err := s.Delete(ctx, sp, "a.md", v1, bob, "")
			return err
		}},
		{"a name differing only by case", func() error {
			_, err := s.Put(ctx, sp, "A.md", []byte("x"), "", bob, "")
			return err
		}},
		{"a file inside a document", func() error {
			_, err := s.Put(ctx, sp, "a.md/b.md", []byte("x"), "", bob, "")
			return err
		}},
		{"move onto a document", func() error {
			must[Result](t)(s.Put(ctx, sp, "c.md", []byte("c"), "", bob, ""))
			_, err := s.Move(ctx, sp, "c.md", "a.md", "", bob, "")
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { wantErr(t, tt.do(), ErrConflict) })
	}

	must[Result](t)(s.Put(ctx, sp, "dir/x.md", []byte("x"), "", bob, ""))
	_, err = s.Put(ctx, sp, "dir", []byte("x"), "", bob, "")
	wantErr(t, err, ErrConflict)
}

func TestConcurrentWrites(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	sp := writable(1)
	base := must[Result](t)(s.Put(ctx, sp, "shared.md", []byte("v1"), "", alice, "")).Doc.Version

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var wins, conflicts int
	for i := range n {
		wg.Go(func() {
			// Everyone edits the same version of shared.md: one wins.
			_, err := s.Put(ctx, sp, "shared.md", fmt.Appendf(nil, "edit %d", i), base, alice, "")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrConflict):
				conflicts++
			default:
				t.Error(err)
			}
		})
		wg.Go(func() {
			// Separate documents don't conflict.
			if _, err := s.Put(ctx, sp, fmt.Sprintf("own/%d.md", i), []byte("mine"), "", bob, ""); err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() {
			if _, err := s.List(ctx, sp, "", "", 0); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if wins != 1 || conflicts != n-1 {
		t.Fatalf("%d saves won and %d conflicted, want 1 and %d", wins, conflicts, n-1)
	}
	h := must[History](t)(s.History(ctx, sp, "", "", 0))
	if len(h.Commits) != 1+1+n {
		t.Fatalf("%d commits, want %d", len(h.Commits), 2+n)
	}
	files := map[string]string{"shared.md": readText(t, s, sp, "shared.md", "")}
	for i := range n {
		files[fmt.Sprintf("own/%d.md", i)] = "mine"
	}
	checkClean(t, dataDir, sp, files)
}

func TestHistoryAndRestore(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	sp := writable(1)
	clock := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	now = func() time.Time { clock = clock.Add(time.Minute); return clock }
	t.Cleanup(func() { now = time.Now })

	r1 := must[Result](t)(s.Put(ctx, sp, "a.md", []byte("one"), "", alice, "First draft"))
	must[Result](t)(s.Put(ctx, sp, "other.md", []byte("unrelated"), "", bob, ""))
	r2 := must[Result](t)(s.Put(ctx, sp, "a.md", []byte("two"), r1.Doc.Version, Author{Name: "Bob", Email: "bob@example.com", Agent: "taskto agent"}, ""))
	r3 := must[Result](t)(s.Delete(ctx, sp, "a.md", r2.Doc.Version, alice, ""))

	h := must[History](t)(s.History(ctx, sp, "a.md", "", 0))
	got := make([]string, len(h.Commits))
	for i, c := range h.Commits {
		got[i] = fmt.Sprintf("%s %s %s %q %s", c.Change, c.Author.Name, c.Agent, c.Message, c.Version)
	}
	want := []string{
		fmt.Sprintf("delete Alice  %q ", "Delete a.md"),
		fmt.Sprintf("update Bob taskto agent %q %s", "Update a.md", r2.Doc.Version),
		fmt.Sprintf("create Alice  %q %s", "First draft", r1.Doc.Version),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("history:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if h.Commits[0].Commit != r3.Commit || h.Commits[2].Commit != r1.Commit {
		t.Fatalf("history commits = %+v", h.Commits)
	}
	if h.Commits[2].Time != "2026-01-02T03:05:05.000Z" || h.Commits[2].Author.Email != "alice@example.com" {
		t.Fatalf("first commit = %+v", h.Commits[2])
	}

	// Pages: one at a time, each continuing before the last.
	var paged []string
	for before := ""; ; {
		page := must[History](t)(s.History(ctx, sp, "a.md", before, 1))
		for _, c := range page.Commits {
			paged = append(paged, c.Commit)
		}
		if page.Next == "" {
			break
		}
		before = page.Next
	}
	if len(paged) != 3 || paged[0] != r3.Commit || paged[2] != r1.Commit {
		t.Fatalf("paged history = %v", paged)
	}
	if all := must[History](t)(s.History(ctx, sp, "", "", 0)); len(all.Commits) != 4 {
		t.Fatalf("space history has %d commits, want 4", len(all.Commits))
	}

	// Past versions.
	if got := readText(t, s, sp, "a.md", r1.Commit); got != "one" {
		t.Fatalf("a.md at r1 = %q", got)
	}
	_, err := s.Read(ctx, sp, "a.md", r3.Commit)
	wantErr(t, err, ErrNotFound)
	_, err = s.Read(ctx, sp, "a.md", strings.Repeat("0", 40))
	wantErr(t, err, ErrNotFound)
	_, err = s.Read(ctx, sp, "a.md", "HEAD~1")
	wantErr(t, err, ErrInvalid)

	// Restoring brings a deleted document back, as a new commit.
	restored := must[Result](t)(s.Restore(ctx, sp, "a.md", r1.Commit, "", bob, ""))
	if restored.Doc.Version != r1.Doc.Version || readText(t, s, sp, "a.md", "") != "one" {
		t.Fatalf("restore = %+v", restored)
	}
	h = must[History](t)(s.History(ctx, sp, "a.md", "", 0))
	if h.Commits[0].Commit != restored.Commit || h.Commits[0].Change != "create" || h.Commits[0].Message != "Restore a.md from "+r1.Commit[:7] {
		t.Fatalf("after restore, newest = %+v", h.Commits[0])
	}
	// Restoring over a newer version needs that version.
	r4 := must[Result](t)(s.Put(ctx, sp, "a.md", []byte("four"), restored.Doc.Version, alice, ""))
	_, err = s.Restore(ctx, sp, "a.md", r2.Commit, restored.Doc.Version, bob, "")
	wantErr(t, err, ErrConflict)
	must[Result](t)(s.Restore(ctx, sp, "a.md", r2.Commit, r4.Doc.Version, bob, "Back to two"))
	_, err = s.Restore(ctx, sp, "other.md", r1.Commit, "", bob, "")
	wantErr(t, err, ErrNotFound)

	checkClean(t, dataDir, sp, map[string]string{"a.md": "two", "other.md": "unrelated"})
}

func TestAuthorsInGit(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	sp := writable(1)
	tricky := Author{Name: "Eve <eve@evil> \n", Email: "eve@example.com>", Agent: "Agent\nSmith"}
	must[Result](t)(s.Put(ctx, sp, "a.md", []byte("x"), "", tricky, ""))
	repo := must[*git.Repository](t)(git.PlainOpen(sp.Dir(dataDir)))
	head := must[*plumbing.Reference](t)(repo.Head())
	c := must[*object.Commit](t)(repo.CommitObject(head.Hash()))
	if c.Author.Name != "Eve eve@evil" || c.Author.Email != "eve@example.com" || c.Committer.Name != "AgentSmith" {
		t.Fatalf("author %q <%s>, committer %q", c.Author.Name, c.Author.Email, c.Committer.Name)
	}
}

func TestLimits(t *testing.T) {
	s, _ := newStore(t, Options{MaxFileSize: 100, MaxRepoSize: 2000})
	sp := writable(1)

	_, err := s.Put(ctx, sp, "big.bin", make([]byte, 101), "", alice, "")
	wantErr(t, err, ErrTooLarge)
	must[Result](t)(s.Put(ctx, sp, "max.bin", make([]byte, 100), "", alice, ""))

	// Random bytes don't compress: the repository fills up.
	var full error
	for i := 0; i < 100 && full == nil; i++ {
		_, full = s.Put(ctx, sp, fmt.Sprintf("r%d.bin", i), []byte(randomSuffix()+randomSuffix()+randomSuffix()+randomSuffix()), "", alice, "")
	}
	wantErr(t, full, ErrRepoFull)
	// Moves and deletes still work, so people can tidy up.
	must[Result](t)(s.Move(ctx, sp, "max.bin", "old/max.bin", "", alice, ""))

	deep := strings.Repeat("d/", MaxDepth-1)
	tests := map[string]error{
		deep + "ok.md":                   nil,
		deep + "d/deep.md":               ErrInvalid,
		strings.Repeat("n", 256) + ".md": ErrInvalid,
		strings.Repeat(strings.Repeat("p", 70)+"/", 15) + "long.md": ErrInvalid,
	}
	small, _ := newStore(t, Options{})
	for name, want := range tests {
		_, err := small.Put(ctx, sp, name, []byte("x"), "", alice, "")
		if !errors.Is(err, want) {
			t.Errorf("%.40s…: err = %v, want %v", name, err, want)
		}
	}

	for _, msg := range []string{"two\nlines", strings.Repeat("m", MaxMessageLen+1)} {
		_, err := small.Put(ctx, sp, "m.md", []byte("x"), "", alice, msg)
		wantErr(t, err, ErrInvalid)
	}
	_, err = small.Put(ctx, sp, "bad.md", []byte{0xff, 0xfe}, "", alice, "")
	wantErr(t, err, ErrInvalid)
}

func TestListPages(t *testing.T) {
	s, _ := newStore(t, Options{})
	sp := writable(1)
	names := []string{"a-b.md", "a.md", "a/b/c.md", "a/z.md", "a0.md", "b.png", "z/y/x.md"}
	for _, n := range slices.Backward(names) {
		must[Result](t)(s.Put(ctx, sp, n, []byte(n), "", alice, ""))
	}
	for _, limit := range []int{1, 2, 3, 7} {
		var got []string
		for after := ""; ; {
			page := must[Page](t)(s.List(ctx, sp, "", after, limit))
			if len(page.Docs) > limit {
				t.Fatalf("limit %d: page of %d", limit, len(page.Docs))
			}
			for _, d := range page.Docs {
				got = append(got, d.Path)
			}
			if page.Next == "" {
				break
			}
			after = page.Next
		}
		if !slices.Equal(got, names) {
			t.Fatalf("limit %d: listed %v, want %v", limit, got, names)
		}
	}
	page := must[Page](t)(s.List(ctx, sp, "a", "", 0))
	if len(page.Docs) != 2 || page.Docs[0].Path != "a/b/c.md" || page.Docs[1].Path != "a/z.md" {
		t.Fatalf("folder a lists %+v", page.Docs)
	}
	_, err := s.List(ctx, sp, "a.md", "", 0)
	wantErr(t, err, ErrNotFound)
	_, err = s.List(ctx, sp, "", "", MaxListLimit+1)
	wantErr(t, err, ErrInvalid)
}

func TestMoveFolder(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	sp := writable(1)
	for _, n := range []string{"team/a.md", "team/sub/b.md", "keep.md"} {
		must[Result](t)(s.Put(ctx, sp, n, []byte(n), "", alice, ""))
	}
	res := must[Result](t)(s.Move(ctx, sp, "team", "archive/team", "", alice, ""))
	if res.Doc != nil {
		t.Fatalf("a folder move returned doc %+v", res.Doc)
	}
	_, err := s.Move(ctx, sp, "archive", "archive/inner", "", alice, "")
	wantErr(t, err, ErrInvalid)
	_, err = s.Delete(ctx, sp, "archive", "", alice, "")
	wantErr(t, err, ErrInvalid)
	checkClean(t, dataDir, sp, map[string]string{
		"archive/team/a.md": "team/a.md", "archive/team/sub/b.md": "team/sub/b.md", "keep.md": "keep.md",
	})
}

func TestAccessChecked(t *testing.T) {
	s, _ := newStore(t, Options{})
	sp := writable(1)
	must[Result](t)(s.Put(ctx, sp, "a.md", []byte("x"), "", alice, ""))

	readOnly := sp
	readOnly.Access = access.Read
	_, err := s.Put(ctx, readOnly, "b.md", []byte("x"), "", alice, "")
	wantErr(t, err, ErrReadOnly)
	must[Doc](t)(s.Read(ctx, readOnly, "a.md", ""))

	none := sp
	none.Access = access.None
	_, err = s.Read(ctx, none, "a.md", "")
	wantErr(t, err, access.ErrNotFound)
	_, err = s.Delete(ctx, none, "a.md", "", alice, "")
	wantErr(t, err, access.ErrNotFound)
}

// TestLinksOutOfTheSpace plants links in a space's directory, as something outside the
// API might, and checks nothing is read or written through them.
func TestLinksOutOfTheSpace(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	outside := t.TempDir()
	secret := filepath.Join(outside, "config")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A .git that is a link: the repository can't be opened.
	sp := writable(1)
	if err := os.MkdirAll(sp.Dir(dataDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(sp.Dir(dataDir), gitDir)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, sp, "a.md", []byte("x"), "", alice, ""); err == nil {
		t.Fatal("wrote through a .git link")
	}

	// A folder that is a link out: the file isn't written through it.
	sp2 := writable(2)
	must[Result](t)(s.Put(ctx, sp2, "first.md", []byte("x"), "", alice, ""))
	if err := os.Symlink(outside, filepath.Join(sp2.Dir(dataDir), "notes")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, sp2, "notes/config", []byte("pwned"), "", alice, ""); err == nil {
		t.Fatal("wrote through a link out of the space")
	}

	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(secret); string(b) != "secret" || len(entries) != 1 {
		t.Fatalf("the directory outside changed: %v, config %q", entries, b)
	}
}

// A folder move can't put what's inside it past the depth or length limits, even when
// the destination folder itself is fine.
func TestMoveFolderKeepsTheLimits(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	sp := writable(1)
	deep := strings.Repeat("d/", MaxDepth-2) + "doc.md" // MaxDepth-1 levels
	must[Result](t)(s.Put(ctx, sp, deep, []byte("x"), "", alice, ""))
	head := must[History](t)(s.History(ctx, sp, "", "", 0)).Commits[0].Commit
	_, err := s.Move(ctx, sp, "d", "x/y/d", "", alice, "")
	wantErr(t, err, ErrInvalid)
	if got := must[History](t)(s.History(ctx, sp, "", "", 0)).Commits[0].Commit; got != head {
		t.Fatalf("a refused move committed %s", got)
	}
	checkClean(t, dataDir, sp, map[string]string{deep: "x"})
}

// The real git CLI sees the space as a clean checkout too, file modes included.
func TestGitCLISeesACleanCheckout(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git isn't installed")
	}
	s, dataDir := newStore(t, Options{})
	sp := writable(1)
	r := must[Result](t)(s.Put(ctx, sp, "notes/a.md", []byte("one"), "", alice, ""))
	must[Result](t)(s.Put(ctx, sp, "notes/a.md", []byte("two"), r.Doc.Version, alice, ""))
	must[Result](t)(s.Move(ctx, sp, "notes", "kept/notes", "", alice, ""))
	v := must[Result](t)(s.Put(ctx, sp, "talks/intro.mp4", []byte("unversioned"), "", alice, ""))
	must[Result](t)(s.Put(ctx, sp, "talks/intro.mp4", []byte("unversioned, replaced"), v.Doc.Version, alice, ""))
	cmd := exec.Command(gitBin, "-c", "safe.directory=*", "status", "--porcelain")
	cmd.Dir = sp.Dir(dataDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, out)
	}
	if len(out) != 0 {
		t.Fatalf("git status isn't clean:\n%s", out)
	}
}

// A change whose files can't all be written changes nothing: main stays, the files are
// put back, and the same change works once the problem is gone.
func TestFailedChangeChangesNothing(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	sp := writable(1)
	for _, n := range []string{"team/a.md", "team/b.md"} {
		must[Result](t)(s.Put(ctx, sp, n, []byte(n), "", alice, ""))
	}
	head := must[History](t)(s.History(ctx, sp, "", "", 0)).Commits[0].Commit

	// Something outside git sits where the move's second file must go.
	obstacle := filepath.Join(sp.Dir(dataDir), "done", "b.md", "in-the-way")
	if err := os.MkdirAll(obstacle, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(ctx, sp, "team", "done", "", alice, ""); err == nil {
		t.Fatal("the move succeeded over a directory in the way")
	}
	if got := must[History](t)(s.History(ctx, sp, "", "", 0)).Commits[0].Commit; got != head {
		t.Fatalf("main moved to %s after a failed change", got)
	}
	for _, n := range []string{"team/a.md", "team/b.md"} {
		if b, err := os.ReadFile(filepath.Join(sp.Dir(dataDir), n)); err != nil || string(b) != n {
			t.Errorf("%s after the failed move: %q, %v", n, b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(sp.Dir(dataDir), "done", "a.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("done/a.md was left behind: %v", err)
	}

	if err := os.RemoveAll(filepath.Join(sp.Dir(dataDir), "done")); err != nil {
		t.Fatal(err)
	}
	must[Result](t)(s.Move(ctx, sp, "team", "done", "", alice, ""))
	checkClean(t, dataDir, sp, map[string]string{"done/a.md": "team/a.md", "done/b.md": "team/b.md"})
}

// Renames add commits and trees but no content; they still count, so renaming back and
// forth can't grow a full repository without end.
func TestRenamesCanNotOutgrowTheLimit(t *testing.T) {
	s, dataDir := newStore(t, Options{MaxRepoSize: 16 << 10})
	sp := writable(1)
	must[Result](t)(s.Put(ctx, sp, "a.md", []byte("x"), "", alice, ""))
	names := [2]string{"a.md", "b.md"}
	var err error
	i := 0
	for ; i < 1000 && err == nil; i++ {
		_, err = s.Move(ctx, sp, names[i%2], names[(i+1)%2], "", alice, "")
	}
	wantErr(t, err, ErrRepoFull)
	var size int64
	_ = filepath.WalkDir(filepath.Join(sp.Dir(dataDir), gitDir), func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				size += fi.Size()
			}
		}
		return nil
	})
	if size > 16<<10 {
		t.Fatalf(".git is %d bytes after %d renames, over the 16 KiB limit", size, i)
	}
}

// A change cut off before main moved (the process died) is undone by the next write: the
// files it had written go back to what main holds.
func TestUnfinishedChangeIsRepaired(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	sp := writable(1)
	must[Result](t)(s.Put(ctx, sp, "a.md", []byte("committed"), "", alice, ""))

	// What a crash mid-change leaves: a file written, the pending list, main unmoved.
	dir := sp.Dir(dataDir)
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("half-written"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.md"), []byte("never committed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, pendingFile), []byte("a.md\nnew.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	must[Result](t)(s.Put(ctx, sp, "b.md", []byte("next"), "", alice, ""))
	checkClean(t, dataDir, sp, map[string]string{"a.md": "committed", "b.md": "next"})
	if _, err := os.Stat(filepath.Join(dir, pendingFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the pending list is still there: %v", err)
	}
}

func TestChangeWithoutAuthorIsInvalid(t *testing.T) {
	s, _ := newStore(t, Options{})
	_, err := s.Put(ctx, writable(1), "a.md", []byte("x"), "", Author{}, "")
	wantErr(t, err, ErrInvalid)
}
