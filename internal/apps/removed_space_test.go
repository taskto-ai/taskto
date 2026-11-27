package apps

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/knowledge"
)

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

func (w *world) created(s int64) string {
	w.e.t.Helper()
	var c string
	if err := w.e.db.QueryRow(`SELECT created_at FROM spaces WHERE id = ?`, s).Scan(&c); err != nil {
		w.e.t.Fatal(err)
	}
	return c
}

func TestAStaleRelocationMovesNothing(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	kb := knowledge.New(w.e.dataDir, knowledge.Options{})
	w.e.s.WithSpaceHolder(kb)
	from, to := w.alicePrivate, w.shared
	a := w.createPhotos(w.alice, from, "Photos")
	picture := testPNG(t, 64, 48)
	id := w.upload(w.alice, from, a, "photos", picture, 4096)
	before := w.files(from, a)

	if err := w.e.s.relocateOne(ctx, a.ID, id, from.ID, w.created(from.ID), to.ID, w.created(to.ID)); err != nil {
		t.Fatal(err)
	}
	if got := w.files(to, a); len(got) != 0 {
		t.Fatalf("a stale relocation copied into %v", got)
	}
	if got := w.files(from, a); !slices.Equal(got, before) {
		t.Fatalf("a stale relocation changed the files: %v, was %v", got, before)
	}
	var space int64
	if err := w.e.db.QueryRow(`SELECT space_id FROM app_attachments WHERE id = ?`, id).Scan(&space); err != nil || space != from.ID {
		t.Fatalf("the attachment's space is %d (%v)", space, err)
	}

	if _, err := w.e.db.Exec(`UPDATE apps SET space_id = ? WHERE id = ?`, to.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	err := w.e.s.relocateOne(ctx, a.ID, id, from.ID, w.created(from.ID), to.ID, "1999-01-01T00:00:00.000Z")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("a relocation to a space that was replaced: %v", err)
	}
	if got := w.files(to, a); len(got) != 0 {
		t.Fatalf("a relocation to a space that was replaced copied into %v", got)
	}

	if err := w.e.s.relocateOne(ctx, a.ID, id, from.ID, w.created(from.ID), to.ID, w.created(to.ID)); err != nil {
		t.Fatal(err)
	}
	if got := w.files(to, a); len(got) != len(before) {
		t.Fatalf("the move put %v in the new space, want %v", got, before)
	}
	if got := w.files(from, a); len(got) != 0 {
		t.Fatalf("the move left %v in the old space", got)
	}
}

func TestAChunkWaitsForTheSpaceWithoutHoldingTheDatabase(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	kb := knowledge.New(w.e.dataDir, knowledge.Options{})
	w.e.s.WithSpaceHolder(kb)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	picture := testPNG(t, 64, 48)
	id := w.start(w.alice, s, a, "photos", len(picture))

	release, err := kb.HoldSpace(s)
	if err != nil {
		t.Fatal(err)
	}
	removed := make(chan error, 1)
	go func() { removed <- kb.RemoveSpace(s) }()
	spin(t, "the removal to wait for the held space", removalIsWaiting)
	chunk := make(chan int, 1)
	go func() { chunk <- w.chunk(w.alice, s, a, id, 0, picture).Code }()
	spin(t, "the chunk to wait for the space", func() bool { return goroutineWith("putChunk", "sync.(*RWMutex).RLock") })

	written := make(chan error, 1)
	go func() { written <- w.e.s.write(ctx, func(*sql.Tx) error { return nil }) }()
	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("a write to the database while a chunk waits for the space: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a chunk waiting for the space holds the database's write lock")
	}

	release()
	if err := <-removed; err != nil {
		t.Fatal(err)
	}
	if code := <-chunk; code != http.StatusNotFound {
		t.Fatalf("the chunk after the removal: status %d, want 404", code)
	}
}

func TestAFilesRootOfAGoneSpaceIsNotFound(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.e.s.WithSpaceHolder(knowledge.New(w.e.dataDir, knowledge.Options{}))
	s := w.alicePrivate
	if _, err := w.e.db.Exec(`DELETE FROM spaces WHERE id = ?`, s.ID); err != nil {
		t.Fatal(err)
	}
	_, _, err := w.e.s.filesRoot(ctx, s.ID)
	if !errors.Is(err, ErrNotFound) || !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("filesRoot of a space with no row: %v", err)
	}
	if _, err := os.Lstat(s.Dir(w.e.dataDir)); err == nil {
		t.Fatal("its directory was made")
	}
}

func goroutineWith(a, b string) bool {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, a) && strings.Contains(g, b) {
			return true
		}
	}
	return false
}

func spin(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		default:
			runtime.Gosched()
		}
	}
}

func TestAFinalizeADocumentWriteAndAChunkDoNotWaitOnEachOther(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	kb := knowledge.New(w.e.dataDir, knowledge.Options{})
	w.e.s.WithSpaceHolder(kb)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	picture := testPNG(t, 64, 48)
	id := w.start(w.alice, s, a, "photos", len(picture))

	finalizing, err := kb.HoldSpace(s)
	if err != nil {
		t.Fatal(err)
	}
	wrote := make(chan error, 1)
	go func() {
		_, err := kb.Put(ctx, s, "note.md", []byte("an agent's note"), "", knowledge.Author{Name: "Agent", Email: "agent@example.com"}, "")
		wrote <- err
	}()
	spin(t, "the document write to queue for the space", func() bool { return goroutineWith("(*Store).Put", "sync.(*RWMutex).Lock") })
	chunk := make(chan int, 1)
	go func() { chunk <- w.chunk(w.alice, s, a, id, 0, picture).Code }()
	spin(t, "the chunk to queue for the space", func() bool { return goroutineWith("putChunk", "sync.(*RWMutex).RLock") })

	tx := make(chan error, 1)
	go func() { tx <- w.e.s.write(ctx, func(*sql.Tx) error { return nil }) }()
	select {
	case err := <-tx:
		if err != nil {
			t.Fatalf("the finalize's transaction: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a finalize waits for the database lock that a chunk holds while it waits for the space")
	}
	finalizing()
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	if code := <-chunk; code != http.StatusOK {
		t.Fatalf("the chunk: status %d", code)
	}
}

type holderFunc func(sp access.Space) (func(), error)

func (f holderFunc) HoldSpace(sp access.Space) (func(), error) { return f(sp) }

func TestFinalizingANonPictureDoesNotEnterTheSpaceAgain(t *testing.T) {
	w := newWorld(t)
	kb := knowledge.New(w.e.dataDir, knowledge.Options{})
	var armed atomic.Bool
	removed := make(chan error, 1)
	w.e.s.WithSpaceHolder(holderFunc(func(sp access.Space) (func(), error) {
		release, err := kb.HoldSpace(sp)
		if err == nil && armed.CompareAndSwap(true, false) {
			go func() { removed <- kb.RemoveSpace(sp) }()
			spin(t, "the removal to wait for the finalize's hold", removalIsWaiting)
		}
		return release, err
	}))
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	notPicture := []byte("this is text, not a picture of anything")
	id := w.start(w.alice, s, a, "photos", len(notPicture))
	w.send(w.alice, s, a, id, notPicture, 4096)

	armed.Store(true)
	answered := make(chan int, 1)
	go func() {
		answered <- w.alice.do(http.MethodPost, appPath(s, a.ID, "/uploads/"+id+"/finalize"), map[string]any{}).Code
	}()
	select {
	case code := <-answered:
		if code != http.StatusUnprocessableEntity && code != http.StatusBadRequest {
			t.Fatalf("finalizing a file that isn't a picture: status %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the finalize waits for a removal that waits for the finalize")
	}
	if err := <-removed; err != nil {
		t.Fatal(err)
	}
}

type recordingHolder struct {
	*knowledge.Store
	mu    sync.Mutex
	order []int64
}

func (r *recordingHolder) HoldSpace(sp access.Space) (func(), error) {
	r.mu.Lock()
	r.order = append(r.order, sp.ID)
	r.mu.Unlock()
	return r.Store.HoldSpace(sp)
}

func TestARelocationHoldsItsSpacesInOrderAndNeverMovesToItself(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	held := &recordingHolder{Store: knowledge.New(w.e.dataDir, knowledge.Options{})}
	w.e.s.WithSpaceHolder(held)
	low, high := w.alicePrivate, w.shared
	if low.ID > high.ID {
		low, high = high, low
	}
	a := w.createPhotos(w.alice, w.alicePrivate, "Photos")
	picture := testPNG(t, 64, 48)
	id := w.upload(w.alice, w.alicePrivate, a, "photos", picture, 4096)

	for _, dir := range [][2]access.Space{{low, high}, {high, low}} {
		held.mu.Lock()
		held.order = nil
		held.mu.Unlock()
		if err := w.e.s.relocateOne(ctx, a.ID, id, dir[0].ID, w.created(dir[0].ID), dir[1].ID, w.created(dir[1].ID)); err != nil {
			t.Fatal(err)
		}
		held.mu.Lock()
		order := slices.Clone(held.order)
		held.mu.Unlock()
		if !slices.Equal(order, []int64{low.ID, high.ID}) {
			t.Fatalf("a move from space %d to %d held %v, want %v", dir[0].ID, dir[1].ID, order, []int64{low.ID, high.ID})
		}
	}

	files := w.files(w.alicePrivate, a)
	if err := w.e.s.relocateOne(ctx, a.ID, id, w.alicePrivate.ID, w.created(w.alicePrivate.ID), w.alicePrivate.ID, w.created(w.alicePrivate.ID)); err != nil {
		t.Fatal(err)
	}
	if got := w.files(w.alicePrivate, a); !slices.Equal(got, files) || len(got) == 0 {
		t.Fatalf("a move onto itself changed the files: %v, was %v", got, files)
	}
}

func TestCleanupsOfARemovedSpaceHaveNothingToDo(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	kb := knowledge.New(w.e.dataDir, knowledge.Options{})
	w.e.s.WithSpaceHolder(kb)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	picture := testPNG(t, 64, 48)
	id := w.upload(w.alice, s, a, "photos", picture, 4096)
	if err := kb.RemoveSpace(s); err != nil {
		t.Fatal(err)
	}
	if err := w.e.s.unlinkFiles(ctx, s.ID, a.ID, id); err != nil {
		t.Fatalf("removing the files of an attachment in a removed space: %v", err)
	}
	if err := w.e.s.sweepSpace(ctx, s, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("sweeping a removed space: %v", err)
	}
	if _, err := os.Lstat(s.Dir(w.e.dataDir)); err == nil {
		t.Fatal("the removed space's directory was made again")
	}
}
