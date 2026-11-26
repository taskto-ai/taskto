package apps

import (
	"context"
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
