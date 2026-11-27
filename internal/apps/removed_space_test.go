package apps

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
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

	root, done, err := w.e.s.filesRootAs(context.Background(), s.ID, 0)
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

func (w *world) generation(s int64) int64 {
	w.e.t.Helper()
	var c int64
	if err := w.e.db.QueryRow(`SELECT generation FROM spaces WHERE id = ?`, s).Scan(&c); err != nil {
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

	if err := w.e.s.relocateOne(ctx, a.ID, id, from.ID, w.generation(from.ID), to.ID, w.generation(to.ID)); err != nil {
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
	err := w.e.s.relocateOne(ctx, a.ID, id, from.ID, w.generation(from.ID), to.ID, w.generation(to.ID)+1000)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("a relocation to a space that was replaced: %v", err)
	}
	if got := w.files(to, a); len(got) != 0 {
		t.Fatalf("a relocation to a space that was replaced copied into %v", got)
	}

	if err := w.e.s.relocateOne(ctx, a.ID, id, from.ID, w.generation(from.ID), to.ID, w.generation(to.ID)); err != nil {
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
	_, _, err := w.e.s.filesRootAs(ctx, s.ID, 0)
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
		if err := w.e.s.relocateOne(ctx, a.ID, id, dir[0].ID, w.generation(dir[0].ID), dir[1].ID, w.generation(dir[1].ID)); err != nil {
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
	if err := w.e.s.relocateOne(ctx, a.ID, id, w.alicePrivate.ID, w.generation(w.alicePrivate.ID), w.alicePrivate.ID, w.generation(w.alicePrivate.ID)); err != nil {
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
	if err := w.e.s.unlinkFiles(ctx, s.ID, w.generation(s.ID), a.ID, id); err != nil {
		t.Fatalf("removing the files of an attachment in a removed space: %v", err)
	}
	if err := w.e.s.sweepSpace(ctx, s, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("sweeping a removed space: %v", err)
	}
	if _, err := os.Lstat(s.Dir(w.e.dataDir)); err == nil {
		t.Fatal("the removed space's directory was made again")
	}
}

func TestARelocationThatLosesTheAppWhileWaitingForTheDatabaseMovesNothing(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.e.s.WithSpaceHolder(knowledge.New(w.e.dataDir, knowledge.Options{}))
	from, to := w.alicePrivate, w.shared
	a := w.createPhotos(w.alice, from, "Photos")
	picture := testPNG(t, 64, 48)
	id := w.upload(w.alice, from, a, "photos", picture, 4096)
	before := w.files(from, a)
	if len(before) == 0 {
		t.Fatal("fixture: the upload has no files")
	}
	if _, err := w.e.db.Exec(`UPDATE apps SET space_id = ? WHERE id = ?`, to.ID, a.ID); err != nil {
		t.Fatal(err)
	}

	holding, release := make(chan struct{}), make(chan struct{})
	moved := make(chan error, 1)
	go func() {
		moved <- w.e.s.write(ctx, func(tx *sql.Tx) error {
			close(holding)
			<-release
			_, err := tx.ExecContext(ctx, `UPDATE apps SET space_id = ? WHERE id = ?`, from.ID, a.ID)
			return err
		})
	}()
	<-holding
	relocated := make(chan error, 1)
	go func() {
		relocated <- w.e.s.relocateOne(ctx, a.ID, id, from.ID, w.generation(from.ID), to.ID, w.generation(to.ID))
	}()
	spin(t, "the relocation to wait for the database", func() bool { return goroutineWith("relocateOne", "(*Service).write") })
	if got := w.files(to, a); !slices.Equal(got, before) {
		t.Fatalf("the relocation waits for the database with %v in the other space, want its copy of %v", got, before)
	}
	close(release)
	if err := <-moved; err != nil {
		t.Fatal(err)
	}
	if err := <-relocated; err != nil {
		t.Fatal(err)
	}

	var space int64
	if err := w.e.db.QueryRow(`SELECT space_id FROM app_attachments WHERE id = ?`, id).Scan(&space); err != nil || space != from.ID {
		t.Fatalf("the attachment is recorded in space %d (%v), the app is in %d", space, err, from.ID)
	}
	if got := w.files(to, a); len(got) != 0 {
		t.Fatalf("the copy of a move that was not recorded is left in the other space: %v", got)
	}
	if got := w.files(from, a); !slices.Equal(got, before) {
		t.Fatalf("the files changed: %v, were %v", got, before)
	}
}

func TestARelocationMovesTheFilesWhicheverSpaceHasTheLowerID(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.e.s.WithSpaceHolder(knowledge.New(w.e.dataDir, knowledge.Options{}))
	a := w.createPhotos(w.alice, w.alicePrivate, "Photos")
	picture := testPNG(t, 64, 48)
	id := w.upload(w.alice, w.alicePrivate, a, "photos", picture, 4096)
	want := w.files(w.alicePrivate, a)
	if len(want) == 0 {
		t.Fatal("fixture: the upload has no files")
	}

	at := w.alicePrivate
	for _, to := range []access.Space{w.shared, w.alicePrivate} {
		from := at
		if _, err := w.e.db.Exec(`UPDATE apps SET space_id = ? WHERE id = ?`, to.ID, a.ID); err != nil {
			t.Fatal(err)
		}
		if err := w.e.s.relocateOne(ctx, a.ID, id, from.ID, w.generation(from.ID), to.ID, w.generation(to.ID)); err != nil {
			t.Fatal(err)
		}
		if got := w.files(to, a); !slices.Equal(got, want) {
			t.Fatalf("the move from space %d to %d put %v in the new space, want %v", from.ID, to.ID, got, want)
		}
		if got := w.files(from, a); len(got) != 0 {
			t.Fatalf("the move from space %d to %d left %v in the old space", from.ID, to.ID, got)
		}
		var space int64
		if err := w.e.db.QueryRow(`SELECT space_id FROM app_attachments WHERE id = ?`, id).Scan(&space); err != nil || space != to.ID {
			t.Fatalf("the attachment is recorded in space %d (%v), want %d", space, err, to.ID)
		}
		at = to
	}
}

func TestHoldingAnAttachmentsSpaceKeepsADatabaseError(t *testing.T) {
	w := newWorld(t)
	w.e.s.WithSpaceHolder(knowledge.New(w.e.dataDir, knowledge.Options{}))
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	id := w.start(w.alice, s, a, "photos", 10)

	space, root, done, err := w.e.s.holdAttachmentSpace(context.Background(), a.ID, id)
	if err != nil || root == nil || space != s.ID {
		t.Fatalf("holding the space of an upload that is there: space %d, root %v, %v", space, root, err)
	}
	done()
	if _, root, done, err := w.e.s.holdAttachmentSpace(context.Background(), a.ID, strings.Repeat("0", 32)); !errors.Is(err, ErrNotFound) || root != nil || done != nil {
		t.Fatalf("an upload that isn't there: %v", err)
	}

	unreadable, cancel := context.WithCancel(context.Background())
	cancel()
	_, root, done, err = w.e.s.holdAttachmentSpace(unreadable, a.ID, id)
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("a read that failed was answered as %v, want the database's error", err)
	}
	if !errors.Is(err, context.Canceled) || root != nil || done != nil {
		t.Fatalf("the error was not kept: %v", err)
	}
}

func TestAChunkWhoseSpaceHoldFailsOnTheDatabaseIsNotNotFound(t *testing.T) {
	w := newWorld(t)
	kb := knowledge.New(w.e.dataDir, knowledge.Options{})
	var failing atomic.Bool
	kb.SetSpaceCheck(func(sp access.Space) (bool, error) {
		if failing.Load() {
			return false, errors.New("disk I/O error")
		}
		return access.SpaceRowCheck(w.e.db)(sp)
	})
	w.e.s.WithSpaceHolder(kb)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	picture := testPNG(t, 64, 48)
	id := w.start(w.alice, s, a, "photos", len(picture))

	failing.Store(true)
	rec := w.chunk(w.alice, s, a, id, 0, picture)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("a chunk whose space hold failed on the database: status %d (%s), want 500", rec.Code, rec.Body.String())
	}
	failing.Store(false)
	expect(t, w.chunk(w.alice, s, a, id, 0, picture), http.StatusOK)
}

func TestFileCleanupOfAReplacedSpaceLeavesTheReplacementsFiles(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.e.s.WithSpaceHolder(knowledge.New(w.e.dataDir, knowledge.Options{}))
	space := w.shared
	old := access.Space{ID: space.ID, WorkspaceID: space.WorkspaceID, Generation: w.generation(space.ID)}

	if _, err := w.e.db.Exec(`DELETE FROM spaces WHERE id = ?`, space.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.db.Exec(`INSERT INTO spaces (id, workspace_id, kind, name) VALUES (?, ?, 'shared', 'Replacement')`, space.ID, space.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	fresh := w.generation(space.ID)
	if fresh == old.Generation {
		t.Fatalf("fixture: the replacement has the generation %d too", fresh)
	}
	const id = "0123456789abcdef0123456789abcdef"
	root, done, err := w.e.s.filesRootAs(ctx, space.ID, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(root, fileName(7, id, ""), []byte("the replacement's picture")); err != nil {
		t.Fatal(err)
	}
	done()
	name := filepath.Join(space.Dir(w.e.dataDir), fileName(7, id, ""))
	still := func(what string) {
		t.Helper()
		if _, err := os.Stat(name); err != nil {
			t.Fatalf("%s took the replacement's file: %v", what, err)
		}
	}

	if err := w.e.s.unlinkFiles(ctx, space.ID, old.Generation, 7, id); err != nil {
		t.Fatalf("removing the files of an attachment of the space that was: %v", err)
	}
	still("removing the files of an attachment of the space that was")
	if err := w.e.s.sweepSpace(ctx, old, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("sweeping the space that was: %v", err)
	}
	still("sweeping the space that was")
	if _, _, err := w.e.s.filesRootAs(ctx, space.ID, old.Generation); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the files of the space that was: %v", err)
	}

	if err := w.e.s.unlinkFiles(ctx, space.ID, fresh, 7, id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(name); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the replacement's own cleanup left the file: %v", err)
	}
}

func TestAnAttachmentIsLoadedWithItsSpacesGeneration(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	id := w.start(w.alice, s, a, "photos", 10)
	u, err := loadAttachment(context.Background(), w.e.db, a.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	if u.spaceID != s.ID || u.spaceGen == 0 || u.spaceGen != w.generation(s.ID) {
		t.Fatalf("the attachment's space %d, generation %d; the space's generation is %d", u.spaceID, u.spaceGen, w.generation(s.ID))
	}
}

func TestTheSweepListsSpacesWithTheirGenerations(t *testing.T) {
	w := newWorld(t)
	spaces, err := w.e.s.spacesToSweep(context.Background())
	if err != nil || len(spaces) == 0 {
		t.Fatalf("the spaces to sweep: %v, %v", spaces, err)
	}
	for _, sp := range spaces {
		if sp.Generation == 0 || sp.Generation != w.generation(sp.ID) {
			t.Fatalf("space %d is listed with generation %d, it has %d", sp.ID, sp.Generation, w.generation(sp.ID))
		}
	}
}
