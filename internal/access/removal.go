package access

import (
	"fmt"
	"os"
	"sync"
)

// A space's directory is removed while the server runs (a purged account's private space), and
// every writer into it (the knowledge base's commits and uploads, the apps' photos) opens it
// through OpenRoot, which creates a missing directory. Without a guard, a writer that resolved the
// space before its removal and runs after it re-creates the directory with a new repository or
// photo files in it: files with no rows, which nothing ever removes.
//
// The guard is one gate per space directory, per process (the data directory has one server,
// see internal/lockfile):
//
//   - a writer or reader of the space's files holds it shared, through EnterSpace, for as long as
//     it uses the directory;
//   - RemoveSpaceDir holds it exclusively, so it waits for those in flight, deletes the
//     directory and marks the space removed: whoever enters afterwards is refused with ErrNotFound
//     and touches nothing;
//   - ForgetRemoved lifts the mark once the space's rows are gone, so a space made later that
//     is given the same ID (SQLite reuses the largest) works.
//
// Lock order: the knowledge base's own space lock first, this gate after it. Code holding the gate
// must not call into the knowledge base.

type gate struct {
	mu   sync.RWMutex
	gone bool
}

var (
	gatesMu sync.Mutex
	gates   = map[string]*gate{}
)

func gateOf(dir string) *gate {
	gatesMu.Lock()
	defer gatesMu.Unlock()
	g, ok := gates[dir]
	if !ok {
		g = &gate{}
		gates[dir] = g
	}
	return g
}

// EnterSpace holds s's directory against removal until leave is called, and returns ErrNotFound
// if it was removed (RemoveSpaceDir) and not forgotten. Call leave once, after the last use of the
// directory, an *os.Root on it included.
func EnterSpace(dataDir string, s Space) (leave func(), err error) {
	g := gateOf(s.Dir(dataDir))
	g.mu.RLock()
	if g.gone {
		g.mu.RUnlock()
		return nil, ErrNotFound
	}
	return g.mu.RUnlock, nil
}

// RemoveSpaceDir deletes s's directory, with everything in it, once the writers that entered
// it have left, and refuses new ones until ForgetRemoved. Removing what is gone already succeeds.
func RemoveSpaceDir(dataDir string, s Space) error {
	dir := s.Dir(dataDir)
	g := gateOf(dir)
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("removing space %d's directory: %w", s.ID, err)
	}
	g.gone = true
	return nil
}

// ForgetRemoved lifts the mark RemoveSpaceDir left, for a space whose rows are deleted: nothing
// can resolve it any more, and its ID may belong to a space made later.
func ForgetRemoved(dataDir string, s Space) {
	dir := s.Dir(dataDir)
	gatesMu.Lock()
	defer gatesMu.Unlock()
	delete(gates, dir) // a later EnterSpace makes a fresh gate; one already held keeps its own
}
