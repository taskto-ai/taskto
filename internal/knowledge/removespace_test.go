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

func gone(t *testing.T, dataDir string, sp access.Space) {
	t.Helper()
	if _, err := os.Lstat(sp.Dir(dataDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the space's directory is there after its removal: %v", err)
	}
}

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

	if err := s.RemoveSpace(sp); err != nil {
		t.Fatalf("removing it again: %v", err)
	}
	gone(t, dataDir, sp)
}

func TestRemoveSpaceThatFailsLeavesTheSpaceRefused(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("a directory of root's can't be made unremovable")
	}
	s, dataDir := newStore(t, Options{})
	sp := writable(1)
	must[Result](t)(s.Put(ctx, sp, "a/b.md", []byte("x"), "", alice, ""))
	locked := sp.Dir(dataDir) + removingSuffix + "/a"
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	hold := func(mode os.FileMode) {
		t.Helper()
		if err := os.Chmod(sp.Dir(dataDir)+"/a", mode); err != nil {
			t.Fatal(err)
		}
	}
	hold(0o500)

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
	s.ForgetSpace(sp)
	must[Result](t)(s.Put(ctx, sp, "c.md", []byte("z"), "", alice, ""))
	if _, err := os.Stat(sp.Dir(dataDir)); err != nil {
		t.Fatal(err)
	}
}

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
	delete(rows, 1)
	mu.Unlock()
	s.ForgetSpace(sp)
	close(proceed)

	wantErr(t, <-done, ErrSpaceRemoved)
	gone(t, dataDir, sp)

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
	if err := s.RemoveSpace(a); err != nil {
		t.Fatal(err)
	}
	release()
	must[Result](t)(s.Put(ctx, b, "c.md", []byte("z"), "", alice, ""))
	must[Result](t)(s.PutFile(ctx, b, "d.png", bytes.NewReader([]byte("bytes")), "", alice, ""))
	if got := readText(t, s, b, "b.md", ""); got != "y" {
		t.Fatalf("read %q", got)
	}
}

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

func TestARemovalThatWaitedOnAForgottenStateWaitsForTheNewOne(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	old := writable(1)
	old.Generation = 1
	must[Result](t)(s.Put(ctx, old, "a.md", []byte("x"), "", alice, ""))
	if err := s.RemoveSpace(old); err != nil {
		t.Fatal(err)
	}

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
	if got := readText(t, s, later, "b.md", ""); got != "y" {
		t.Fatalf("the later space's document after the old space's delayed removal: %q", got)
	}
	if _, err := os.Stat(filepath.Join(later.Dir(dataDir), "b.md")); err != nil {
		t.Fatalf("the later space's file was removed by the old space's removal: %v", err)
	}
	must[Result](t)(s.Put(ctx, later, "c.md", []byte("z"), "", alice, ""))
}

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

	s.ForgetSpace(later)
	must[Result](t)(s.Put(ctx, later, "c.md", []byte("z"), "", alice, ""))
}

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
	must[Result](t)(s.Put(ctx, later, "big.bin", bytesOf(180, 'c'), "", alice, ""))
	if _, u, err := s.Usage(later); err != nil || u != 180 {
		t.Fatalf("the unversioned bytes after its first big file = %d, %v; want 180", u, err)
	}
}

func TestARemovalForASpaceWhoseRowIsGoneLeavesTheDirectoryAlone(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	current := writable(1)
	current.Generation = 2
	must[Result](t)(s.Put(ctx, current, "a.md", []byte("x"), "", alice, ""))

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

func TestARemovalOfALaterSpaceWithTheSameIDIsNotSkipped(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	old := writable(1)
	old.Generation = 1
	must[Result](t)(s.Put(ctx, old, "a.md", []byte("x"), "", alice, ""))
	if err := s.RemoveSpace(old); err != nil {
		t.Fatal(err)
	}

	later := writable(1)
	later.Generation = 2
	must[Result](t)(New(dataDir, Options{}).Put(ctx, later, "b.md", []byte("y"), "", alice, ""))
	if err := s.RemoveSpace(later); err != nil {
		t.Fatal(err)
	}
	gone(t, dataDir, later)
	_, err := s.Put(ctx, later, "c.md", []byte("z"), "", alice, "")
	wantErr(t, err, ErrSpaceRemoved)

	if err := s.RemoveSpace(old); err != nil {
		t.Fatal(err)
	}
	_, err = s.Put(ctx, later, "d.md", []byte("w"), "", alice, "")
	wantErr(t, err, ErrSpaceRemoved)
	gone(t, dataDir, later)
}
