package knowledge

import (
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// DeleteKept refuses an unversioned file, whose bytes a delete removes for good, and still
// deletes a document, whose history keeps it. Delete, the HTTP route's, is unchanged.
func TestDeleteKeptRefusesUnversionedFiles(t *testing.T) {
	s, _ := newStore(t, Options{})
	sp := writable(1)
	must[Result](t)(s.Put(ctx, sp, "song.mp3", []byte("ID3 audio"), "", alice, ""))
	must[Result](t)(s.Put(ctx, sp, "backup.zip", []byte("PK archive"), "", alice, ""))
	doc := must[Result](t)(s.Put(ctx, sp, "plan.md", []byte("plan"), "", alice, ""))

	for _, name := range []string{"song.mp3", "backup.zip"} {
		_, err := s.DeleteKept(ctx, sp, name, "", alice, "")
		wantErr(t, err, ErrInvalid)
		if got, err := openBytes(t, s, sp, name, ""); err != nil || len(got) == 0 {
			t.Fatalf("%s after the refused delete: %q, %v", name, got, err)
		}
	}
	must[Result](t)(s.DeleteKept(ctx, sp, "plan.md", doc.Doc.Version, alice, ""))
	if got := readText(t, s, sp, "plan.md", doc.Commit); got != "plan" {
		t.Fatalf("the deleted document's history = %q", got)
	}
	// The HTTP route's delete is as it was.
	must[Result](t)(s.Delete(ctx, sp, "song.mp3", "", alice, ""))
}

// MoveKept refuses a rename that would take a file's older versions out of history; a rename
// that keeps its storage goes through, and Move, the HTTP route's, is unchanged.
func TestMoveKeptRefusesAStorageChange(t *testing.T) {
	s, _ := newStore(t, Options{})
	sp := writable(1)
	first := must[Result](t)(s.Put(ctx, sp, "draft.md", []byte("one"), "", alice, ""))
	second := must[Result](t)(s.Put(ctx, sp, "draft.md", []byte("two"), first.Doc.Version, alice, ""))
	must[Result](t)(s.Put(ctx, sp, "song.mp3", []byte("ID3"), "", alice, ""))

	_, err := s.MoveKept(ctx, sp, "draft.md", "draft.zip", "", alice, "")
	wantErr(t, err, ErrInvalid)
	// Nothing moved, and the history at the old path is whole.
	if got := readText(t, s, sp, "draft.md", first.Commit); got != "one" {
		t.Fatalf("draft.md at its first commit = %q", got)
	}
	if h := must[History](t)(s.History(ctx, sp, "draft.md", "", 0)); len(h.Commits) != 2 || h.Commits[0].Commit != second.Commit {
		t.Fatalf("history = %+v", h)
	}
	// Moving whole folders, between versioned names and between unversioned ones, keeps storage.
	must[Result](t)(s.Put(ctx, sp, "notes.txt", []byte("n"), "", alice, ""))
	must[Result](t)(s.MoveKept(ctx, sp, "notes.txt", "notes.csv", "", alice, ""))
	must[Result](t)(s.MoveKept(ctx, sp, "draft.md", "old/draft.md", "", alice, ""))
	must[Result](t)(s.MoveKept(ctx, sp, "song.mp3", "track.mp3", "", alice, ""))
	must[Result](t)(s.MoveKept(ctx, sp, "old", "older", "", alice, ""))

	// The route's move still makes the file unversioned, as it always did.
	must[Result](t)(s.Move(ctx, sp, "notes.csv", "notes.zip", "", alice, ""))
}

// A name that is the same as an existing one by Unicode form or case never replaces it: on
// macOS and Windows they are one file, and the second would overwrite the first's bytes.
func TestUnicodeEquivalentNamesConflict(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	sp := writable(1)
	const nfc, nfd = "café", "café"
	must[Result](t)(s.Put(ctx, sp, nfc+".md", []byte("the cafe"), "", alice, ""))
	must[Result](t)(s.Put(ctx, sp, "draft.md", []byte("draft"), "", alice, ""))
	must[Result](t)(s.Put(ctx, sp, "Menu/"+nfc+".md", []byte("menu"), "", alice, ""))

	_, err := s.Move(ctx, sp, "draft.md", nfd+".md", "", alice, "")
	wantErr(t, err, ErrConflict)
	_, err = s.Put(ctx, sp, nfd+".md", []byte("replacement"), "", alice, "")
	wantErr(t, err, ErrConflict)
	_, err = s.Put(ctx, sp, "Menu/"+nfd+".MD", []byte("replacement"), "", alice, "")
	wantErr(t, err, ErrConflict)
	// A folder of the same name in another form, too.
	_, err = s.Put(ctx, sp, "Menü/x.md", []byte("x"), "", alice, "")
	if err != nil {
		t.Fatalf("a different name was refused: %v", err)
	}
	must[Result](t)(s.Put(ctx, sp, "Caf\u00e9/inside.md", []byte("in"), "", alice, ""))
	_, err = s.Put(ctx, sp, "CAFÉ/other.md", []byte("x"), "", alice, "")
	wantErr(t, err, ErrConflict)
	_, err = s.MoveKept(ctx, sp, "draft.md", "CAFÉ.md", "", alice, "")
	wantErr(t, err, ErrConflict)

	if got := readText(t, s, sp, nfc+".md", ""); got != "the cafe" {
		t.Fatalf("the existing document = %q", got)
	}
	if b, err := os.ReadFile(filepath.Join(sp.Dir(dataDir), nfc+".md")); err != nil || string(b) != "the cafe" {
		t.Fatalf("the checkout's file = %q, %v", b, err)
	}
	// Renaming a file to the same name in another form is fine: it is the only one.
	must[Result](t)(s.Move(ctx, sp, "Menu/"+nfc+".md", "Menu/"+nfd+".md", "", alice, ""))
}

// A commit that isn't in main's history (one a failed change stored and left behind) is read
// by no one: not as a version, not as a listing, a history's start or a restore's source.
func TestCommitsOutsideMainAreNotFound(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	sp := writable(1)
	first := must[Result](t)(s.Put(ctx, sp, "a.md", []byte("one"), "", alice, ""))

	// A commit on top of the head that no branch points to, with a different file in it.
	r, err := openRepo(dataDir, sp, s.state(sp.ID), false)
	if err != nil {
		t.Fatal(err)
	}
	head := must[*object.Commit](t)(r.head())
	blob := must[plumbing.Hash](t)(r.storeBlob([]byte("never committed")))
	tree := must[plumbing.Hash](t)(r.setPath(head.TreeHash, []string{"ghost.md"}, "ghost.md", blob))
	dangling, _, err := r.newCommit(head, tree, alice, "dangling")
	r.close()
	if err != nil {
		t.Fatal(err)
	}
	at := dangling.String()

	_, err = s.Read(ctx, sp, "ghost.md", at)
	wantErr(t, err, ErrNotFound)
	_, err = s.ReadText(ctx, sp, "ghost.md", at, 1<<20)
	wantErr(t, err, ErrNotFound)
	_, _, err = s.Open(ctx, sp, "ghost.md", at)
	wantErr(t, err, ErrNotFound)
	_, err = s.ListAt(ctx, sp, at, "", "", 0)
	wantErr(t, err, ErrNotFound)
	_, err = s.History(ctx, sp, "a.md", at, 0)
	wantErr(t, err, ErrNotFound)
	_, err = s.Restore(ctx, sp, "ghost.md", at, "", alice, "")
	wantErr(t, err, ErrNotFound)
	if _, err := s.Read(ctx, sp, "ghost.md", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ghost.md is in the space: %v", err)
	}
	// A commit of main is read as before.
	if got := readText(t, s, sp, "a.md", first.Commit); got != "one" {
		t.Fatalf("a.md at its commit = %q", got)
	}
}

// ReadText returns only text, whatever the name: a binary under a Markdown name, Latin-1 and
// NUL are refused rather than returned with replacement characters.
func TestReadTextIsText(t *testing.T) {
	s, _ := newStore(t, Options{})
	sp := writable(1)
	must[Result](t)(s.Put(ctx, sp, "note.md", []byte("café"), "", alice, ""))
	must[Result](t)(s.Put(ctx, sp, "blob.bin", []byte("\x89PNG\r\n\x1a\n\x00\xff"), "", alice, ""))
	must[Result](t)(s.Move(ctx, sp, "blob.bin", "blob.md", "", alice, "")) // allowed: both are versioned
	must[Result](t)(s.Put(ctx, sp, "latin1.txt", []byte("caf\xe9"), "", alice, ""))
	must[Result](t)(s.Put(ctx, sp, "nul.txt", []byte("a\x00b"), "", alice, ""))
	must[Result](t)(s.Put(ctx, sp, "song.mp3", []byte("ID3"), "", alice, ""))

	if doc := must[Doc](t)(s.ReadText(ctx, sp, "note.md", "", 1<<20)); string(doc.Content) != "café" || doc.Size != 5 {
		t.Fatalf("note.md = %+v", doc)
	}
	for _, name := range []string{"blob.md", "latin1.txt", "nul.txt", "song.mp3"} {
		_, err := s.ReadText(ctx, sp, name, "", 1<<20)
		wantErr(t, err, ErrInvalid)
	}
	_, err := s.ReadText(ctx, sp, "nothing.txt", "", 1<<20)
	wantErr(t, err, ErrNotFound)
}

// The size limit and the file's kind are decided before the content is loaded: a file whose
// bytes can't be read at all is still refused for being too large.
func TestReadTextChecksSizeBeforeLoading(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	sp := writable(1)
	// Bigger than the limit, and its stored object cut short after the header: the size is in
	// the header, the content can't be read.
	content := make([]byte, 2<<20)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range content {
		content[i] = "abcdefghijklmnopqrstuvwxyz"[rng.IntN(26)]
	}
	res := must[Result](t)(s.Put(ctx, sp, "big.txt", content, "", alice, ""))
	hash := res.Doc.Version
	object := filepath.Join(sp.Dir(dataDir), ".git", "objects", hash[:2], hash[2:])
	if err := os.Chmod(object, 0o600); err != nil { // git writes its objects read-only
		t.Fatal(err)
	}
	if err := os.Truncate(object, 4096); err != nil {
		t.Fatal(err)
	}
	_, err := s.ReadText(ctx, sp, "big.txt", "", 1<<20)
	wantErr(t, err, ErrInvalid)
	// Read, which loads it first, can't tell: this is what the check avoids.
	if _, err := s.Read(ctx, sp, "big.txt", ""); err == nil || errors.Is(err, ErrInvalid) {
		t.Fatalf("Read of the damaged file: %v", err)
	}
}
