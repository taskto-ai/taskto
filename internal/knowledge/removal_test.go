package knowledge

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"testing"

	"taskto.ai/taskto/internal/access"
)

func noDir(t *testing.T, dataDir string, sp access.Space) {
	t.Helper()
	if _, err := os.Stat(sp.Dir(dataDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("space %d's directory exists after its removal: %v", sp.ID, err)
	}
}

// A write that arrives once the space is removed, while its rows are still there (the purge
// deletes the files first), is refused. It used to make the directory and a new repository again,
// with nothing that would ever remove them.
func TestAWriteAfterTheRemovalDoesNotMakeTheSpaceAgain(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	sp := writable(1)
	must[Result](t)(s.Put(ctx, sp, "a.md", []byte("x"), "", alice, ""))
	if err := s.RemoveSpace(sp); err != nil {
		t.Fatal(err)
	}

	_, err := s.Put(ctx, sp, "b.md", []byte("y"), "", bob, "")
	wantErr(t, err, access.ErrNotFound)
	_, err = s.Delete(ctx, sp, "a.md", "", bob, "")
	wantErr(t, err, access.ErrNotFound)
	_, err = s.Read(ctx, sp, "a.md", "")
	wantErr(t, err, access.ErrNotFound)
	_, err = s.List(ctx, sp, "", "", 0)
	wantErr(t, err, access.ErrNotFound)
	_, err = s.PutFile(ctx, sp, "big.bin", bytes.NewReader(make([]byte, 64)), "", bob, "")
	wantErr(t, err, access.ErrNotFound)
	noDir(t, dataDir, sp)
}

// The writer that had resolved the space and was only waiting for its lock when the removal
// took it: the case the race is about. It runs after the removal and must find the space gone.
func TestAWriterWaitingForTheLockWhenTheSpaceIsRemovedIsRefused(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	sp := writable(1)
	must[Result](t)(s.Put(ctx, sp, "a.md", []byte("x"), "", alice, ""))

	// Hold the space's lock as a writer in flight would; the writers below queue behind it.
	st := s.state(sp.ID)
	st.mu.Lock()
	const writers = 8
	errs := make(chan error, writers)
	var started sync.WaitGroup
	for i := range writers {
		started.Add(1)
		go func() {
			started.Done()
			_, err := s.Put(ctx, sp, fmt.Sprintf("w%d.md", i), []byte("late"), "", bob, "")
			errs <- err
		}()
	}
	started.Wait()
	// What RemoveSpace does under the lock, which the test holds in its place.
	if err := access.RemoveSpaceDir(s.dataDir, sp); err != nil {
		t.Fatal(err)
	}
	st.mu.Unlock()

	for range writers {
		wantErr(t, <-errs, access.ErrNotFound)
	}
	noDir(t, dataDir, sp)
}

// Writers and removals racing, over and over: whatever the order, nothing is left of the space
// once its removal returned and the writers did, and every writer either committed before it or
// was refused.
func TestWritersRacingTheRemovalLeaveNothingBehind(t *testing.T) {
	for round := range 20 {
		s, dataDir := newStore(t, Options{})
		sp := writable(1)
		must[Result](t)(s.Put(ctx, sp, "a.md", []byte("x"), "", alice, ""))

		var wg sync.WaitGroup
		errs := make(chan error, 16)
		for i := range 6 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := s.Put(ctx, sp, fmt.Sprintf("w%d.md", i), []byte("late"), "", bob, "")
				if err != nil && !errors.Is(err, access.ErrNotFound) {
					errs <- err
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.RemoveSpace(sp); err != nil {
				errs <- err
			}
		}()
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("round %d: %v", round, err)
		}
		noDir(t, dataDir, sp)
	}
}

// Removing one space doesn't touch another's writes, in the same workspace or another that has
// a space with the same ID, and a space made after the removal is forgotten, with the removed
// one's ID, works.
func TestRemovingASpaceNeverAffectsAnothersWrites(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	gone := writable(1)
	sibling := writable(2)
	otherWorkspace := access.Space{ID: 1, WorkspaceID: 2, Kind: access.Shared, Access: access.Write}
	for _, sp := range []access.Space{gone, sibling, otherWorkspace} {
		must[Result](t)(s.Put(ctx, sp, "a.md", []byte("x"), "", alice, ""))
	}

	// A writer of the sibling is inside its space while the other is removed.
	st := s.state(sibling.ID)
	st.mu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := s.Put(ctx, sibling, "b.md", []byte("y"), "", bob, "")
		done <- err
	}()
	if err := s.RemoveSpace(gone); err != nil {
		t.Fatal(err)
	}
	st.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatalf("the sibling's write failed after another space's removal: %v", err)
	}
	must[Result](t)(s.Put(ctx, otherWorkspace, "b.md", []byte("y"), "", bob, ""))
	if got := readText(t, s, sibling, "b.md", ""); got != "y" {
		t.Fatalf("sibling's document = %q", got)
	}
	noDir(t, dataDir, gone)

	// Its rows are deleted, and a space later made with the same ID is writable.
	access.ForgetRemoved(dataDir, gone)
	must[Result](t)(s.Put(ctx, gone, "new.md", []byte("fresh"), "", alice, ""))
	if got := readText(t, s, gone, "new.md", ""); got != "fresh" {
		t.Fatalf("a space made with a removed one's ID: %q", got)
	}
}
