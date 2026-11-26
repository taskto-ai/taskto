package access

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func spaceWithFile(t *testing.T, dataDir string, s Space) {
	t.Helper()
	root, err := OpenRoot(dataDir, s)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.WriteFile(filepath.Join(s.Dir(dataDir), "a.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveSpaceDirRefusesLaterEntriesUntilForgotten(t *testing.T) {
	dataDir := t.TempDir()
	s := Space{ID: 7, WorkspaceID: 1}
	spaceWithFile(t, dataDir, s)

	if err := RemoveSpaceDir(dataDir, s); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Dir(dataDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the directory remains: %v", err)
	}
	if _, err := EnterSpace(dataDir, s); !errors.Is(err, ErrNotFound) {
		t.Fatalf("entering a removed space: %v, want ErrNotFound", err)
	}
	if err := RemoveSpaceDir(dataDir, s); err != nil {
		t.Fatalf("removing it again: %v", err)
	}

	// Its rows are gone and its ID is given to a new space: that one works.
	ForgetRemoved(dataDir, s)
	leave, err := EnterSpace(dataDir, s)
	if err != nil {
		t.Fatalf("entering a space made after the removal was forgotten: %v", err)
	}
	leave()
}

// The removal waits for whoever is using the directory, then deletes it: a writer that is half-way
// through is never cut off from under its own files.
func TestRemoveSpaceDirWaitsForWhoIsInside(t *testing.T) {
	dataDir := t.TempDir()
	s := Space{ID: 7, WorkspaceID: 1}
	spaceWithFile(t, dataDir, s)
	leave, err := EnterSpace(dataDir, s)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- RemoveSpaceDir(dataDir, s) }()
	select {
	case err := <-done:
		t.Fatalf("the removal went ahead of a writer inside: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := os.Stat(filepath.Join(s.Dir(dataDir), "a.md")); err != nil {
		t.Fatalf("the writer's files went while it was inside: %v", err)
	}
	leave()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Dir(dataDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the directory remains: %v", err)
	}
}

// Removing one space never affects another's writers: not a sibling, not a space of another
// workspace with the same ID, and not a different data directory's.
func TestRemovingOneSpaceLeavesOthersAlone(t *testing.T) {
	dataDir := t.TempDir()
	gone := Space{ID: 7, WorkspaceID: 1}
	sibling := Space{ID: 8, WorkspaceID: 1}
	sameID := Space{ID: 7, WorkspaceID: 2}
	for _, s := range []Space{gone, sibling, sameID} {
		spaceWithFile(t, dataDir, s)
	}
	leave, err := EnterSpace(dataDir, sibling)
	if err != nil {
		t.Fatal(err)
	}
	defer leave()

	// The sibling's writer is inside, and the removal of the other space doesn't wait for it.
	if err := RemoveSpaceDir(dataDir, gone); err != nil {
		t.Fatal(err)
	}
	for _, s := range []Space{sibling, sameID} {
		l, err := EnterSpace(dataDir, s)
		if err != nil {
			t.Fatalf("space %d of workspace %d refused after another's removal: %v", s.ID, s.WorkspaceID, err)
		}
		l()
		if _, err := os.Stat(filepath.Join(s.Dir(dataDir), "a.md")); err != nil {
			t.Fatalf("space %d of workspace %d lost its files: %v", s.ID, s.WorkspaceID, err)
		}
	}
	other := t.TempDir()
	if l, err := EnterSpace(other, gone); err != nil {
		t.Fatalf("the same space under another data directory refused: %v", err)
	} else {
		l()
	}
}
