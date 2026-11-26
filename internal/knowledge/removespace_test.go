package knowledge

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

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
