package knowledge

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"taskto.ai/taskto/internal/access"
)

// history writes three versions of a document, by alice, bob and alice again (whose address is
// written in capitals the second time), and a file by bob.
func anonymizeFixture(t *testing.T) (*Store, string) {
	t.Helper()
	s, dataDir := newStore(t, Options{})
	sp := writable(1)
	r := must[Result](t)(s.Put(ctx, sp, "plan.md", []byte("v1"), "", alice, "first"))
	r = must[Result](t)(s.Put(ctx, sp, "plan.md", []byte("v2"), r.Doc.Version, bob, "second"))
	shouting := Author{Name: "ALICE A.", Email: "Alice@Example.com"}
	must[Result](t)(s.Put(ctx, sp, "plan.md", []byte("v3"), r.Doc.Version, shouting, "third"))
	must[Result](t)(s.Put(ctx, sp, "other.md", []byte("x"), "", bob, "fourth"))
	return s, dataDir
}

func TestAnonymizeAuthorsReplacesOnlyTheNamedAddress(t *testing.T) {
	s, dataDir := anonymizeFixture(t)
	sp := writable(1)
	before := gitHistory(t, dataDir, sp)

	n, err := s.AnonymizeAuthors(ctx, sp, []string{"ALICE@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("rewrote %d commits, want 2", n)
	}
	after := gitHistory(t, dataDir, sp)
	if len(after) != len(before) {
		t.Fatalf("%d commits, was %d", len(after), len(before))
	}
	deleted := fmt.Sprintf("%s <%s>", DeletedAuthorName, DeletedAuthorEmail)
	for i := range before {
		b, a := before[i], after[i]
		if a.message != b.message {
			t.Errorf("commit %d's message changed: %q", i, a.message)
		}
		for path, blob := range b.files {
			if a.files[path] != blob {
				t.Errorf("commit %d's file %s changed (a document's version must not)", i, path)
			}
		}
		named := strings.Contains(strings.ToLower(b.author), "alice")
		switch {
		case named && !strings.HasPrefix(a.author, deleted):
			t.Errorf("commit %d is still by %q", i, a.author)
		case named && !strings.HasPrefix(a.committer, deleted):
			t.Errorf("commit %d is still committed by %q", i, a.committer)
		case !named && (a.author != b.author || a.committer != b.committer):
			t.Errorf("commit %d, by someone else, changed: %q", i, a.author)
		}
		// The time of the commit stays.
		if named && !strings.HasSuffix(a.author, b.author[strings.LastIndex(b.author, "> ")+1:]) {
			t.Errorf("commit %d's date changed: %q -> %q", i, b.author, a.author)
		}
	}
	// The documents read as they did, and their history lists the placeholder.
	if doc := must[Doc](t)(s.Read(ctx, sp, "plan.md", "")); string(doc.Content) != "v3" {
		t.Errorf("plan.md = %q", doc.Content)
	}
	h := must[History](t)(s.History(ctx, sp, "plan.md", "", 10))
	for _, c := range h.Commits {
		if strings.Contains(strings.ToLower(c.Author.Email), "alice") {
			t.Errorf("history still lists %q", c.Author.Email)
		}
	}
	gitFsck(t, dataDir, sp)
	// Nothing in git holds the old address: the old commits were deleted, not just unlinked.
	var leaks []string
	_ = filepath.WalkDir(filepath.Join(sp.Dir(dataDir), ".git", "objects"), func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			leaks = append(leaks, path)
		}
		return nil
	})
	if len(leaks) > len(after)*3+10 {
		t.Errorf("%d loose objects remain for %d commits: the old ones weren't pruned", len(leaks), len(after))
	}

	// Idempotent: nothing left to do.
	if n, err := s.AnonymizeAuthors(ctx, sp, []string{"alice@example.com"}); err != nil || n != 0 {
		t.Errorf("a second run rewrote %d commits: %v", n, err)
	}
	// A space with no repository, and no addresses, are nothing to do.
	if n, err := s.AnonymizeAuthors(ctx, writable(99), []string{"alice@example.com"}); err != nil || n != 0 {
		t.Errorf("a space without a repository: %d, %v", n, err)
	}
	if n, err := s.AnonymizeAuthors(ctx, sp, nil); err != nil || n != 0 {
		t.Errorf("no addresses: %d, %v", n, err)
	}
}

func TestAnonymizeAuthorsRecoversFromACrash(t *testing.T) {
	for _, step := range []string{"objects", "ref"} {
		t.Run(step, func(t *testing.T) {
			s, dataDir := anonymizeFixture(t)
			sp := writable(1)
			rewriteFault = func(at string) error {
				if at == step {
					return errors.New("crash")
				}
				return nil
			}
			t.Cleanup(func() { rewriteFault = nil })
			if _, err := s.AnonymizeAuthors(ctx, sp, []string{"alice@example.com"}); err == nil {
				t.Fatal("the crash didn't stop it")
			}
			rewriteFault = nil
			if _, err := os.Stat(filepath.Join(sp.Dir(dataDir), filepath.FromSlash(rewriteFile))); err != nil {
				t.Fatalf("no journal after the crash: %v", err)
			}

			// The next process finishes it.
			s = New(dataDir, Options{})
			if err := s.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			for _, c := range gitHistory(t, dataDir, sp) {
				if strings.Contains(strings.ToLower(c.author), "alice") || strings.Contains(strings.ToLower(c.committer), "alice") {
					t.Errorf("a commit is still by %q", c.author)
				}
			}
			if _, err := os.Stat(filepath.Join(sp.Dir(dataDir), filepath.FromSlash(rewriteFile))); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the journal is still there: %v", err)
			}
			gitFsck(t, dataDir, sp)
		})
	}
}

// A purge that stopped after main moved and is tried again (the scheduler's retry, not a restart)
// must finish the rewrite, not find main clean and call it done: the journal still names the
// address and the old commits, which hold it, are still in the object store.
func TestAnonymizeAuthorsAgainAfterAStopAfterTheRefMovedFinishesTheRewrite(t *testing.T) {
	s, dataDir := anonymizeFixture(t)
	sp := writable(1)
	rewriteFault = func(at string) error {
		if at == "ref" {
			return errors.New("stopped")
		}
		return nil
	}
	t.Cleanup(func() { rewriteFault = nil })
	if _, err := s.AnonymizeAuthors(ctx, sp, []string{"alice@example.com"}); err == nil {
		t.Fatal("the stop didn't stop it")
	}
	rewriteFault = nil
	journal := filepath.Join(sp.Dir(dataDir), filepath.FromSlash(rewriteFile))
	if _, err := os.Stat(journal); err != nil {
		t.Fatalf("no journal after the stop: %v", err)
	}
	if !storeHolds(t, dataDir, sp, "alice@example.com") {
		t.Fatal("fixture: the old commits aren't in the store after the stop")
	}

	// The same call again, on the same Store, with no restart in between.
	if _, err := s.AnonymizeAuthors(ctx, sp, []string{"alice@example.com"}); err != nil {
		t.Fatalf("the second attempt: %v", err)
	}
	if _, err := os.Stat(journal); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the journal, which names the address, is still there: %v", err)
	}
	if storeHolds(t, dataDir, sp, "alice@example.com") {
		t.Error("a commit holding the address is still in the object store")
	}
	gitFsck(t, dataDir, sp)
}

// storeHolds reports whether any commit in the repository's object store, reachable or not, has
// an author or committer with the address.
func storeHolds(t *testing.T, dataDir string, sp access.Space, email string) bool {
	t.Helper()
	repo, err := git.PlainOpen(sp.Dir(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	it, err := repo.CommitObjects()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	if err := it.ForEach(func(c *object.Commit) error {
		if strings.EqualFold(c.Author.Email, email) || strings.EqualFold(c.Committer.Email, email) {
			found = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return found
}

func TestRemoveSpaceDeletesItsFilesAndIsRepeatable(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	sp := writable(1)
	must[Result](t)(s.Put(ctx, sp, "a.md", []byte("x"), "", alice, ""))
	if err := s.RemoveSpace(sp); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sp.Dir(dataDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the directory remains: %v", err)
	}
	if err := s.RemoveSpace(sp); err != nil {
		t.Fatalf("removing it again: %v", err)
	}
	if _, err := s.Read(ctx, sp, "a.md", ""); err == nil {
		t.Fatal("a document of a removed space reads")
	}
}

// The count to rewrite is told before the rewrite is journaled: a caller that can't keep it stops
// the rewrite, and a space with nothing of theirs tells nothing.
func TestAnonymizeAuthorsNotingTellsTheCountFirst(t *testing.T) {
	s, dataDir := anonymizeFixture(t)
	sp := writable(1)
	before := gitHistory(t, dataDir, sp)
	refused := errors.New("can't keep the count")
	if _, err := s.AnonymizeAuthorsNoting(ctx, sp, []string{"alice@example.com"}, func(int) error { return refused }); !errors.Is(err, refused) {
		t.Fatalf("err = %v, want the caller's", err)
	}
	if after := gitHistory(t, dataDir, sp); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatal("the history changed though the count couldn't be kept")
	}
	var told []int
	n, err := s.AnonymizeAuthorsNoting(ctx, sp, []string{"alice@example.com"}, func(n int) error { told = append(told, n); return nil })
	if err != nil || n != 2 || fmt.Sprint(told) != "[2]" {
		t.Fatalf("rewrote %d (%v), told %v; want 2, told [2]", n, err, told)
	}
	told = nil
	if n, err := s.AnonymizeAuthorsNoting(ctx, sp, []string{"alice@example.com"}, func(n int) error { told = append(told, n); return nil }); err != nil || n != 0 || told != nil {
		t.Fatalf("again: %d, %v, told %v; want 0 and nothing told", n, err, told)
	}
}
