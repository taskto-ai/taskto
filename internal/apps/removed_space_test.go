package apps

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"runtime"
	"strings"
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

// removalIsWaiting reports whether a goroutine is in knowledge's RemoveSpace, blocked on the space's
// exclusive lock: observed from the goroutines' stacks, so the test needs no sleep and no hook in
// production code.
func removalIsWaiting() bool {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "knowledge.(*Store).RemoveSpace") && strings.Contains(g, "sync.(*RWMutex).Lock") {
			return true
		}
	}
	return false
}

// A finalize that finds its picture isn't one removes the files through the directory it holds. If
// it entered the space again while a removal waits for it to leave, the second entry would queue
// behind the removal, which waits for the first: neither could go on.
func TestCleanupInsideAHeldSpaceDoesNotWaitForAPendingRemoval(t *testing.T) {
	w := newWorld(t)
	kb := knowledge.New(w.e.dataDir, knowledge.Options{})
	w.e.s.WithSpaceHolder(kb)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	id := w.start(w.alice, s, a, "photos", 10)

	root, done, err := w.e.s.filesRoot(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	removed := make(chan error, 1)
	go func() { removed <- kb.RemoveSpace(s) }()
	deadline := time.After(10 * time.Second)
	for !removalIsWaiting() {
		select {
		case <-deadline:
			t.Fatal("the removal never waited for the held space")
		default:
			runtime.Gosched()
		}
	}

	cleaned := make(chan error, 1)
	go func() { cleaned <- w.e.s.deleteAttachment(context.Background(), id, a.ID, root) }()
	select {
	case err := <-cleaned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cleanup waits for the removal, which waits for the cleanup")
	}
	done()
	if err := <-removed; err != nil {
		t.Fatal(err)
	}
}
