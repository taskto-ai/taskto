package apps

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"testing"
	"time"

	"taskto.ai/taskto/internal/knowledge"
)

// The files of an app's attachments are in the space's directory, which the knowledge base removes
// with a purged account's private space. An upload's chunk, a finished picture, a read and the
// sweep each open that directory, which makes it if it isn't there: after the removal they must
// refuse instead, and leave nothing of the space on disk.
func TestNothingMakesARemovedSpaceAgain(t *testing.T) {
	w := newWorld(t)
	kb := knowledge.New(w.e.dataDir, knowledge.Options{})
	w.e.s.WithSpaceHolder(kb)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	picture := testPNG(t, 64, 48)
	done := w.upload(w.alice, s, a, "photos", picture, 4096)
	pending := w.start(w.alice, s, a, "photos", len(picture))
	w.send(w.alice, s, a, pending, picture[:10], 10)
	received := w.start(w.alice, s, a, "photos", len(picture))
	w.send(w.alice, s, a, received, picture, 4096)

	if err := kb.RemoveSpace(s); err != nil {
		t.Fatal(err)
	}
	gone := func(what string) {
		t.Helper()
		if _, err := os.Lstat(s.Dir(w.e.dataDir)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s made the removed space's directory again: %v", what, err)
		}
	}
	gone("the removal")

	expect(t, w.chunk(w.alice, s, a, pending, 10, picture[10:]), http.StatusNotFound)
	gone("a chunk")
	expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/uploads/"+received+"/finalize"), map[string]any{}), http.StatusNotFound)
	gone("a finalize")
	expect(t, w.fetch(w.alice, s, a, done, false), http.StatusNotFound)
	gone("a read")
	expect(t, w.alice.do(http.MethodDelete, appPath(s, a.ID, "/uploads/"+pending), nil), http.StatusNoContent)
	gone("an abandoned upload")
	if err := w.e.s.Sweep(context.Background(), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("the sweep: %v", err)
	}
	gone("the sweep")
}
