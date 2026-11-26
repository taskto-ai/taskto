package knowledge

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
)

// gone checks that nothing of the space is on disk: not its directory, which any call that opens
// the space's root would make again.
func gone(t *testing.T, dataDir string, sp access.Space) {
	t.Helper()
	if _, err := os.Lstat(sp.Dir(dataDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the space's directory is there after its removal: %v", err)
	}
}

// A write that took the space's state before the removal and waits for its lock gets the lock
// after it. It must be refused: it would make the repository again, with content of a space that
// is gone, and a later write, which gets a new state of the space, would run under another lock.
func TestRemoveSpaceRefusesAWriteThatWasWaiting(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	sp := writable(1)
	must[Result](t)(s.Put(ctx, sp, "a.md", []byte("x"), "", alice, ""))

	reached, proceed := make(chan struct{}), make(chan struct{})
	s.beforeLock = func(access.Space) {
		s.beforeLock = nil
		close(reached)
		<-proceed
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.Put(ctx, sp, "late.md", []byte("from a purged account"), "", alice, "")
		done <- err
	}()
	<-reached
	if err := s.RemoveSpace(sp); err != nil {
		t.Fatal(err)
	}
	close(proceed)

	wantErr(t, <-done, access.ErrNotFound)
	gone(t, dataDir, sp)
}

// The same for an upload, which stages its bytes in the space's directory before it takes the
// lock: the removal comes after the repository is made and before the staged file is.
func TestRemoveSpaceRefusesAnUploadThatWasStaging(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	sp := writable(1)

	reached, proceed := make(chan struct{}), make(chan struct{})
	s.beforeLock = func(access.Space) {
		s.beforeLock = nil
		close(reached)
		<-proceed
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.PutFile(ctx, sp, "photo.png", bytes.NewReader([]byte("bytes")), "", alice, "")
		done <- err
	}()
	<-reached
	if err := s.RemoveSpace(sp); err != nil {
		t.Fatal(err)
	}
	close(proceed)

	wantErr(t, <-done, access.ErrNotFound)
	gone(t, dataDir, sp)
}

// A removed space stays removed to every call that reaches it with the Space it had, as the
// purge's callers and an in-flight request do until its rows are deleted: no write makes it again
// and no read makes its directory.
func TestRemovedSpaceIsNotMadeAgain(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	sp := writable(1)
	must[Result](t)(s.Put(ctx, sp, "a.md", []byte("x"), "", alice, ""))
	if err := s.RemoveSpace(sp); err != nil {
		t.Fatal(err)
	}

	_, err := s.Put(ctx, sp, "b.md", []byte("y"), "", alice, "")
	wantErr(t, err, access.ErrNotFound)
	_, err = s.PutFile(ctx, sp, "c.png", bytes.NewReader([]byte("z")), "", alice, "")
	wantErr(t, err, access.ErrNotFound)
	_, err = s.Delete(ctx, sp, "a.md", "", alice, "")
	wantErr(t, err, access.ErrNotFound)
	_, err = s.Move(ctx, sp, "a.md", "d.md", "", alice, "")
	wantErr(t, err, access.ErrNotFound)
	if _, err := s.Read(ctx, sp, "a.md", ""); err == nil {
		t.Fatal("a removed space reads")
	}
	if _, err := s.List(ctx, sp, "", "", 0); err == nil {
		t.Fatal("a removed space lists")
	}
	gone(t, dataDir, sp)

	// Removing it again is still a success.
	if err := s.RemoveSpace(sp); err != nil {
		t.Fatalf("removing it again: %v", err)
	}
	gone(t, dataDir, sp)
}

// A removal that stops part-way (a directory it can't remove) leaves the space refused, and
// nothing of it at its path for a write to commit into; the retry finishes it.
func TestRemoveSpaceThatFailsLeavesTheSpaceRefused(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("a directory of root's can't be made unremovable")
	}
	s, dataDir := newStore(t, Options{})
	sp := writable(1)
	must[Result](t)(s.Put(ctx, sp, "a/b.md", []byte("x"), "", alice, ""))
	// The removal renames the space's directory, then deletes it: this is where it is stuck.
	locked := sp.Dir(dataDir) + removingSuffix + "/a"
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	hold := func(mode os.FileMode) {
		t.Helper()
		if err := os.Chmod(sp.Dir(dataDir)+"/a", mode); err != nil {
			t.Fatal(err)
		}
	}
	hold(0o500) // its files can't be unlinked

	if err := s.RemoveSpace(sp); err == nil {
		t.Fatal("a removal that can't finish succeeded")
	}
	gone(t, dataDir, sp)
	_, err := s.Put(ctx, sp, "c.md", []byte("y"), "", alice, "")
	wantErr(t, err, access.ErrNotFound)
	gone(t, dataDir, sp)

	if err := os.Chmod(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveSpace(sp); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	gone(t, dataDir, sp)
	entries, err := os.ReadDir(filepath.Dir(sp.Dir(dataDir)))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("what the removal left in the spaces directory: %v", entries)
	}
}

// What the Store keeps of a removed space is dropped once its rows are: a space made later may be
// given the same ID and must work.
func TestForgetSpaceLetsALaterSpaceUseTheID(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	sp := writable(1)
	must[Result](t)(s.Put(ctx, sp, "a.md", []byte("x"), "", alice, ""))
	if err := s.RemoveSpace(sp); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HoldSpace(sp); !errors.Is(err, ErrSpaceRemoved) {
		t.Fatalf("holding a removed space: %v", err)
	}
	s.ForgetSpace(sp)
	must[Result](t)(s.Put(ctx, sp, "b.md", []byte("y"), "", alice, ""))
	if got := readText(t, s, sp, "b.md", ""); got != "y" {
		t.Fatalf("read %q", got)
	}
	// Forgetting a space that was not removed leaves it as it is.
	s.ForgetSpace(sp)
	must[Result](t)(s.Put(ctx, sp, "c.md", []byte("z"), "", alice, ""))
	if _, err := os.Stat(sp.Dir(dataDir)); err != nil {
		t.Fatal(err)
	}
}

// A space made later with a purged space's ID is another space, told by its creation time: the
// mark of the removal doesn't refuse it, whether or not ForgetSpace has run, and does still refuse
// a call that was authorized for the old one.
func TestARemovalDoesNotRefuseALaterSpaceWithTheSameID(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	old := writable(1)
	old.CreatedAt = "2026-01-01T00:00:00.000Z"
	must[Result](t)(s.Put(ctx, old, "a.md", []byte("x"), "", alice, ""))
	if err := s.RemoveSpace(old); err != nil {
		t.Fatal(err)
	}

	later := writable(1)
	later.CreatedAt = "2026-02-01T00:00:00.000Z"
	must[Result](t)(s.Put(ctx, later, "b.md", []byte("y"), "", alice, ""))
	if got := readText(t, s, later, "b.md", ""); got != "y" {
		t.Fatalf("read %q", got)
	}
	_, err := s.Put(ctx, old, "c.md", []byte("z"), "", alice, "")
	wantErr(t, err, access.ErrNotFound)
	if _, err := os.Stat(filepath.Join(old.Dir(dataDir), "c.md")); err == nil {
		t.Fatal("the stale write landed in the new space")
	}
}

// A write that took the space's state before the removal and waits for the lock gets a state
// that is new when the removal has been forgotten meanwhile (the purge deleted the rows and called
// ForgetSpace): it is refused by the space's row, and makes nothing.
func TestAWriteThatOutlivesTheForgettingIsRefusedByTheRow(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	sp := writable(1)
	sp.CreatedAt = "2026-01-01T00:00:00.000Z"
	var mu sync.Mutex
	rows := map[int64]string{1: sp.CreatedAt}
	s.SetSpaceCheck(func(c access.Space) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		created, ok := rows[c.ID]
		return ok && (c.CreatedAt == "" || c.CreatedAt == created), nil
	})
	must[Result](t)(s.Put(ctx, sp, "a.md", []byte("x"), "", alice, ""))

	reached, proceed := make(chan struct{}), make(chan struct{})
	s.beforeLock = func(access.Space) {
		s.beforeLock = nil
		close(reached)
		<-proceed
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.Put(ctx, sp, "late.md", []byte("from a purged account"), "", alice, "")
		done <- err
	}()
	<-reached
	if err := s.RemoveSpace(sp); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	delete(rows, 1) // the purge deletes the rows
	mu.Unlock()
	s.ForgetSpace(sp)
	close(proceed)

	wantErr(t, <-done, ErrSpaceRemoved)
	gone(t, dataDir, sp)

	// The ID is given to a new space: the stale call is refused by its creation time, the new one works.
	mu.Lock()
	rows[1] = "2026-03-01T00:00:00.000Z"
	mu.Unlock()
	_, err := s.Put(ctx, sp, "late.md", []byte("stale"), "", alice, "")
	wantErr(t, err, ErrSpaceRemoved)
	gone(t, dataDir, sp)
	fresh := writable(1)
	fresh.CreatedAt = rows[1]
	must[Result](t)(s.Put(ctx, fresh, "b.md", []byte("y"), "", alice, ""))
}

// Removing one space touches no other: a sibling, with its documents, uploads and holders, goes on.
func TestRemovingOneSpaceLeavesTheOthersWorking(t *testing.T) {
	s, _ := newStore(t, Options{})
	a, b := writable(1), writable(2)
	b.WorkspaceID = 2
	must[Result](t)(s.Put(ctx, a, "a.md", []byte("x"), "", alice, ""))
	must[Result](t)(s.Put(ctx, b, "b.md", []byte("y"), "", alice, ""))
	release, err := s.HoldSpace(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveSpace(a); err != nil { // does not wait for b's holder
		t.Fatal(err)
	}
	release()
	must[Result](t)(s.Put(ctx, b, "c.md", []byte("z"), "", alice, ""))
	must[Result](t)(s.PutFile(ctx, b, "d.png", bytes.NewReader([]byte("bytes")), "", alice, ""))
	if got := readText(t, s, b, "b.md", ""); got != "y" {
		t.Fatalf("read %q", got)
	}
}

// removalIsWaiting reports whether a goroutine is in RemoveSpace, blocked on a space's lock:
// observed from the goroutines' stacks, so a test needs no sleep.
func removalIsWaiting() bool {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "(*Store).RemoveSpace") && strings.Contains(g, "sync.(*RWMutex).Lock") {
			return true
		}
	}
	return false
}

// A removal that took the space's state, and waits for its lock while the state is forgotten, must
// not go on with the state nobody else will find: a writer of the space's new state would then run
// beside it, and the removal would delete what the writer is making.
func TestARemovalThatWaitedOnAForgottenStateWaitsForTheNewOne(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	old := writable(1)
	old.CreatedAt = "2026-01-01T00:00:00.000Z"
	must[Result](t)(s.Put(ctx, old, "a.md", []byte("x"), "", alice, ""))
	if err := s.RemoveSpace(old); err != nil {
		t.Fatal(err)
	}

	// The purge's retry removes it again, and has the state when it is forgotten.
	reached, proceed := make(chan struct{}), make(chan struct{})
	s.beforeLock = func(access.Space) {
		s.beforeLock = nil
		close(reached)
		<-proceed
	}
	removed := make(chan error, 1)
	go func() { removed <- s.RemoveSpace(old) }()
	<-reached
	s.ForgetSpace(old)

	// A space made later with the ID has a writer inside its write.
	later := writable(1)
	later.CreatedAt = "2026-02-01T00:00:00.000Z"
	inside, release := make(chan struct{}), make(chan struct{})
	written := make(chan error, 1)
	go func() {
		_, err := s.PutIf(ctx, later, "b.md", []byte("y"), "", alice, "", func() error {
			close(inside)
			<-release
			return nil
		})
		written <- err
	}()
	<-inside
	close(proceed)
	for !removalIsWaiting() {
		select {
		case err := <-removed:
			t.Fatalf("the removal ran beside a writer of the space (it returned %v)", err)
		default:
			runtime.Gosched()
		}
	}
	close(release)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if err := <-removed; err != nil {
		t.Fatal(err)
	}
	gone(t, dataDir, later)
}

// A removal doesn't wait for an upload's body: the bytes are staged without the space's lock, so a
// client that stalls half-way holds nothing the purge needs (the scheduler runs it in a worker
// that other jobs wait behind). The upload then ends refused, with nothing left of the space.
func TestARemovalDoesNotWaitForAStalledUpload(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	sp := writable(1)
	stalled, unblock := make(chan struct{}), make(chan struct{})
	body := &stallingReader{first: []byte("some bytes"), stalled: stalled, unblock: unblock}
	put := make(chan error, 1)
	go func() {
		_, err := s.PutFile(ctx, sp, "big.png", body, "", alice, "")
		put <- err
	}()
	<-stalled

	removed := make(chan error, 1)
	go func() { removed <- s.RemoveSpace(sp) }()
	select {
	case err := <-removed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the removal waits for an upload whose client has stalled")
	}
	close(unblock)
	wantErr(t, <-put, access.ErrNotFound)
	gone(t, dataDir, sp)
}

// stallingReader gives its first bytes, then blocks until unblock is closed.
type stallingReader struct {
	first   []byte
	stalled chan struct{}
	unblock chan struct{}
	sent    bool
}

func (r *stallingReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, r.first), nil
	}
	close(r.stalled)
	<-r.unblock
	return 0, io.EOF
}

// Staging an upload's file takes the space's lock as a reader, on its own: a writer that holds the
// space keeps it from making directories and files until it is done, and the removal, which is a
// writer, can therefore not be overtaken by it.
func TestStagingAnUploadsFileWaitsForAWriterOfTheSpace(t *testing.T) {
	s, _ := newStore(t, Options{})
	sp := writable(1)
	st := s.state(sp.ID)
	st.mu.Lock()
	done := make(chan error, 1)
	go func() {
		sg, f, err := s.openStaged(sp)
		if err == nil {
			_ = f.Close()
			sg.discard()
		}
		done <- err
	}()
	deadline := time.After(10 * time.Second)
	for !blockedIn("(*Store).openStaged", "sync.(*RWMutex).RLock") {
		select {
		case <-deadline:
			t.Fatal("staging never waited for the writer")
		case err := <-done:
			t.Fatalf("staging made its file while a writer held the space (%v)", err)
		default:
			runtime.Gosched()
		}
	}
	st.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// blockedIn reports whether a goroutine's stack has both words.
func blockedIn(a, b string) bool {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, a) && strings.Contains(g, b) {
			return true
		}
	}
	return false
}
