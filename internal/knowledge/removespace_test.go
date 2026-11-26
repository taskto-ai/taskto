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

// A space made later with a purged space's ID is another space, told by its generation: the
// mark of the removal doesn't refuse it, whether or not ForgetSpace has run, and does still refuse
// a call that was authorized for the old one.
func TestARemovalDoesNotRefuseALaterSpaceWithTheSameID(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	old := writable(1)
	old.Generation = 1
	must[Result](t)(s.Put(ctx, old, "a.md", []byte("x"), "", alice, ""))
	if err := s.RemoveSpace(old); err != nil {
		t.Fatal(err)
	}

	later := writable(1)
	later.Generation = 2
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
	sp.Generation = 1
	var mu sync.Mutex
	rows := map[int64]int64{1: sp.Generation}
	s.SetSpaceCheck(func(c access.Space) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		created, ok := rows[c.ID]
		return ok && (c.Generation == 0 || c.Generation == created), nil
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

	// The ID is given to a new space: the stale call is refused by its generation, the new one works.
	mu.Lock()
	rows[1] = 3
	mu.Unlock()
	_, err := s.Put(ctx, sp, "late.md", []byte("stale"), "", alice, "")
	wantErr(t, err, ErrSpaceRemoved)
	gone(t, dataDir, sp)
	fresh := writable(1)
	fresh.Generation = rows[1]
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
	old.Generation = 1
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
	later.Generation = 2
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
	// The removal was for the space that was, whose directory is gone: the files there are the
	// later space's, and stay, and the space goes on working (the removal marked nothing).
	if got := readText(t, s, later, "b.md", ""); got != "y" {
		t.Fatalf("the later space's document after the old space's delayed removal: %q", got)
	}
	if _, err := os.Stat(filepath.Join(later.Dir(dataDir), "b.md")); err != nil {
		t.Fatalf("the later space's file was removed by the old space's removal: %v", err)
	}
	must[Result](t)(s.Put(ctx, later, "c.md", []byte("z"), "", alice, ""))
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

// Forgetting a removed space drops the mark of that space only: when a later space with the same
// ID was removed before the earlier one's purge got to forget it, the mark is the later one's, and
// the earlier purge's forgetting must not lift it.
func TestForgetSpaceLeavesTheMarkOfALaterSpaceWithTheSameID(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	earlier := writable(1)
	earlier.Generation = 1
	later := writable(1)
	later.Generation = 2
	must[Result](t)(s.Put(ctx, later, "a.md", []byte("x"), "", alice, ""))
	if err := s.RemoveSpace(later); err != nil {
		t.Fatal(err)
	}

	s.ForgetSpace(earlier)

	_, err := s.Put(ctx, later, "b.md", []byte("y"), "", alice, "")
	wantErr(t, err, ErrSpaceRemoved)
	gone(t, dataDir, later)

	// Its own mark is forgotten.
	s.ForgetSpace(later)
	must[Result](t)(s.Put(ctx, later, "c.md", []byte("z"), "", alice, ""))
}

// What the Store measured of a removed space goes with its directory: a space made with the ID
// before ForgetSpace has run (its mark is kept until the purge has deleted the rows) starts from
// nothing, not from the counters and the usage of the one that was.
func TestARemovedSpacesUsageIsNotInheritedByALaterSpaceWithTheSameID(t *testing.T) {
	s, _ := newStore(t, Options{LargeFileSize: 150})
	old := writable(1)
	old.Generation = 1
	must[Result](t)(s.Put(ctx, old, "big.bin", bytesOf(200, 'b'), "", alice, ""))
	if _, u, err := s.Usage(old); err != nil || u != 200 {
		t.Fatalf("fixture: the unversioned bytes = %d, %v", u, err)
	}
	if err := s.RemoveSpace(old); err != nil {
		t.Fatal(err)
	}

	later := writable(1)
	later.Generation = 2
	if v, u, err := s.Usage(later); err != nil || v != 0 || u != 0 {
		t.Fatalf("the usage of a space made with the removed one's ID, before it wrote: %d, %d, %v", v, u, err)
	}
	must[Result](t)(s.Put(ctx, later, "a.md", []byte("hello"), "", alice, ""))
	if v, u, err := s.Usage(later); err != nil || u != 0 || v <= 0 {
		t.Fatalf("the usage after its first document: %d, %d, %v; the old space's 200 unversioned bytes were inherited", v, u, err)
	}
	// And the counter it writes by: a second unversioned file counts from nothing.
	must[Result](t)(s.Put(ctx, later, "big.bin", bytesOf(180, 'c'), "", alice, ""))
	if _, u, err := s.Usage(later); err != nil || u != 180 {
		t.Fatalf("the unversioned bytes after its first big file = %d, %v; want 180", u, err)
	}
}

// With the Store's check of rows, a removal for a space whose row is not the one it was given (it
// was purged, and another has its ID) does nothing: the directory is the other's.
func TestARemovalForASpaceWhoseRowIsGoneLeavesTheDirectoryAlone(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	current := writable(1)
	current.Generation = 2
	must[Result](t)(s.Put(ctx, current, "a.md", []byte("x"), "", alice, ""))

	// The server has restarted: this Store has not been used for the space, so only the row can say
	// whose the directory is. The space with the ID has generation 2.
	s = New(dataDir, Options{})
	s.SetSpaceCheck(func(c access.Space) (bool, error) { return c.Generation == 0 || c.Generation == 2, nil })
	stale := writable(1)
	stale.Generation = 1
	if err := s.RemoveSpace(stale); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(current.Dir(dataDir), "a.md")); err != nil {
		t.Fatalf("a removal for the space that was took the current one's file: %v", err)
	}
	must[Result](t)(s.Put(ctx, current, "b.md", []byte("y"), "", alice, ""))
}

// A space made with a removed space's ID, before the removal is forgotten, is removed like any
// other when its turn comes: what the state remembers of the earlier space doesn't make its
// removal a no-op, which would leave its files and its mark unset.
func TestARemovalOfALaterSpaceWithTheSameIDIsNotSkipped(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	old := writable(1)
	old.Generation = 1
	must[Result](t)(s.Put(ctx, old, "a.md", []byte("x"), "", alice, ""))
	if err := s.RemoveSpace(old); err != nil { // not forgotten yet
		t.Fatal(err)
	}

	// The later space has files, made through another Store (this one has not served it): its
	// removal is the first thing this Store does for it.
	later := writable(1)
	later.Generation = 2
	must[Result](t)(New(dataDir, Options{}).Put(ctx, later, "b.md", []byte("y"), "", alice, ""))
	if err := s.RemoveSpace(later); err != nil {
		t.Fatal(err)
	}
	gone(t, dataDir, later)
	_, err := s.Put(ctx, later, "c.md", []byte("z"), "", alice, "")
	wantErr(t, err, ErrSpaceRemoved)
}
