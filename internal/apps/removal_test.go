package apps

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
)

// A chunk, a deletion's cleanup or a sweep that reaches a space after its directory was removed
// (the purge of an account removes the files first and the rows after) must not make the
// directory again: the photo files it would leave have no rows and nothing removes them. And
// removing one person's space never stops another's upload.
func TestUploadsDoNotMakeARemovedSpacesDirectoryAgain(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	alice, bob := w.alicePrivate, w.bobPrivate
	aliceApp := w.createPhotos(w.alice, alice, "Photos")
	bobApp := w.createPhotos(w.bob, bob, "Photos")
	picture := testPNG(t, 64, 48)

	aliceUpload := w.start(w.alice, alice, aliceApp, "photos", len(picture))
	expect(t, w.chunk(w.alice, alice, aliceApp, aliceUpload, 0, picture[:100]), http.StatusOK)
	bobUpload := w.start(w.bob, bob, bobApp, "photos", len(picture))
	expect(t, w.chunk(w.bob, bob, bobApp, bobUpload, 0, picture[:100]), http.StatusOK)

	// What the knowledge base's RemoveSpace does to alice's directory.
	if err := access.RemoveSpaceDir(w.e.dataDir, alice); err != nil {
		t.Fatal(err)
	}

	rec := w.chunk(w.alice, alice, aliceApp, aliceUpload, 100, picture[100:])
	if rec.Code == http.StatusOK {
		t.Fatal("a chunk was stored in a removed space")
	}
	if err := w.e.s.unlinkFiles(ctx, alice.ID, aliceApp.ID, aliceUpload); err != nil {
		t.Fatalf("cleaning up a removed space's file: %v", err)
	}
	if err := w.e.s.sweepSpace(ctx, alice, time.Now().Add(24*time.Hour)); err != nil {
		t.Fatalf("sweeping a removed space: %v", err)
	}
	if _, err := os.Stat(alice.Dir(w.e.dataDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the removed space's directory was made again: %v", err)
	}

	// Bob's upload, in his own space, goes on and finishes.
	expect(t, w.chunk(w.bob, bob, bobApp, bobUpload, 100, picture[100:]), http.StatusOK)
	expect(t, w.bob.do(http.MethodPost, appPath(bob, bobApp.ID, "/uploads/"+bobUpload+"/finalize"), map[string]any{}), http.StatusOK)
}

// Cleaning up an upload that isn't a picture, while the space's removal is waiting for the
// finalize that found it, goes through the directory the finalize already holds: entering the space
// again would queue behind the removal, which waits for the finalize, and neither would finish.
func TestCleanupInsideAHeldSpaceDoesNotWaitForAPendingRemoval(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	id := w.start(w.alice, s, a, "photos", 10)
	expect(t, w.chunk(w.alice, s, a, id, 0, []byte("not a pict")), http.StatusOK)

	root, done, err := w.e.s.filesRoot(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	removed := make(chan error, 1)
	go func() { removed <- access.RemoveSpaceDir(w.e.dataDir, s) }() // queues behind the held space
	time.Sleep(100 * time.Millisecond)

	cleaned := make(chan error, 1)
	go func() { cleaned <- w.e.s.deleteAttachment(ctx, id, a.ID, root) }()
	select {
	case err := <-cleaned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cleanup waits for the removal that waits for it")
	}
	done()
	if err := <-removed; err != nil {
		t.Fatal(err)
	}
}

// A space whose row is gone is refused even without the removal mark: a request that read the row
// before the purge deleted it, and entered after the mark was lifted, doesn't make the directory.
func TestAGoneSpacesRowIsRecheckedOnceInside(t *testing.T) {
	w := newWorld(t)
	gone := access.Space{ID: 987654, WorkspaceID: w.alicePrivate.WorkspaceID}
	if _, _, err := w.e.s.openSpaceFiles(context.Background(), gone); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a space with no row: %v, want sql.ErrNoRows", err)
	}
	if _, err := os.Stat(gone.Dir(w.e.dataDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("its directory was made: %v", err)
	}
	root, done, err := w.e.s.openSpaceFiles(context.Background(), w.alicePrivate)
	if err != nil {
		t.Fatalf("a space with a row: %v", err)
	}
	_ = root
	done()
}
