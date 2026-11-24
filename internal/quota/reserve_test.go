package quota

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/db"
)

// fakeFiles measures spaces from a map; hook runs inside each measurement, for a test to hold
// or fail it at a chosen point.
type fakeFiles struct {
	mu   sync.Mutex
	size map[int64]int64
	hook func(spaceID int64) error
}

// Usage reads the size first and runs the hook after, so a hook that holds the measurement
// leaves it with the figure it read before.
func (f *fakeFiles) Usage(sp access.Space) (int64, int64, error) {
	f.mu.Lock()
	n := f.size[sp.ID]
	f.mu.Unlock()
	if f.hook != nil {
		if err := f.hook(sp.ID); err != nil {
			return 0, 0, err
		}
	}
	return n, 0, nil
}

func (f *fakeFiles) set(id, n int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.size[id] = n
}

// twoSpaces makes a workspace with two shared spaces, 1 and 2, and returns them.
func twoSpaces(t *testing.T) (*sql.DB, access.Space, access.Space) {
	t.Helper()
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO workspaces (id, name) VALUES (1, 'W')`,
		`INSERT INTO spaces (id, workspace_id, kind, name) VALUES (1, 1, 'shared', 'A'), (2, 1, 'shared', 'B')`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	return sqlDB, access.Space{ID: 1, WorkspaceID: 1}, access.Space{ID: 2, WorkspaceID: 1}
}

// rendezvous makes the first n callers wait for each other (or for the timeout), so a test
// can have several checks all measure before any of them decides.
type rendezvous struct {
	mu      sync.Mutex
	arrived int
	n       int
	all     chan struct{}
}

func newRendezvous(n int) *rendezvous { return &rendezvous{n: n, all: make(chan struct{})} }

func (r *rendezvous) wait() {
	r.mu.Lock()
	r.arrived++
	first := r.arrived <= r.n
	if r.arrived == r.n {
		close(r.all)
	}
	r.mu.Unlock()
	if first {
		select {
		case <-r.all:
		case <-time.After(5 * time.Second):
		}
	}
}

// Two writers in different spaces, each measuring before the other has stored anything, can't
// both take the last of the workspace's bytes.
func TestConcurrentWritesInDifferentSpacesCannotBothFit(t *testing.T) {
	sqlDB, a, b := twoSpaces(t)
	files := &fakeFiles{size: map[int64]int64{}}
	meet := newRendezvous(2)
	files.hook = func(id int64) error {
		if id == 1 {
			meet.wait() // both checks have measured the workspace before either decides
		}
		return nil
	}
	s := New(sqlDB, Limits{Workspace: 100}, files)

	var wg sync.WaitGroup
	results := make([]error, 2)
	held := make([]*Reservation, 2)
	for i, sp := range []access.Space{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			held[i], results[i] = s.ReserveDB(context.Background(), sp, 60)
		}()
	}
	wg.Wait()
	refused := 0
	for _, err := range results {
		if errors.Is(err, ErrExceeded) {
			refused++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if refused != 1 {
		t.Fatalf("%d of two 60-byte writes refused in a 100-byte workspace, want exactly 1 (%v)", refused, results)
	}

	// Once the winner's bytes are stored and its reservation released, they still count: not twice, not zero.
	for _, r := range held {
		if r != nil {
			files.set(1, 60)
			r.Release()
		}
	}
	if err := s.CheckDB(context.Background(), b, 41); !errors.Is(err, ErrExceeded) {
		t.Fatalf("41 more bytes: %v, want refused", err)
	}
	if err := s.CheckDB(context.Background(), b, 40); err != nil {
		t.Fatalf("40 more bytes: %v, want allowed", err)
	}
}

// A write that stores its bytes and releases between another check's measuring and deciding
// must not leave that check with the old figures.
func TestCheckMeasuresAgainWhenAWriteFinishedMeanwhile(t *testing.T) {
	sqlDB, a, b := twoSpaces(t)
	files := &fakeFiles{size: map[int64]int64{}}
	s := New(sqlDB, Limits{Workspace: 100}, files)

	first, err := s.ReserveDB(context.Background(), a, 60)
	if err != nil {
		t.Fatal(err)
	}
	measured, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	files.hook = func(int64) error {
		once.Do(func() {
			close(measured)
			<-proceed // the first measurement of the check below holds here, with a's size still 0
		})
		return nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.ReserveDB(context.Background(), b, 60)
		done <- err
	}()
	<-measured
	// The writer stores its bytes, publishes them and gives its reservation back.
	files.set(1, 60)
	first.Release()
	close(proceed)
	if err := <-done; !errors.Is(err, ErrExceeded) {
		t.Fatalf("a check that measured before the write finished got %v, want refused", err)
	}
}

// A space that can't be measured is not an empty one: the write is refused, retryably.
func TestAnUnmeasurableSpaceRefusesTheWrite(t *testing.T) {
	sqlDB, a, b := twoSpaces(t)
	files := &fakeFiles{size: map[int64]int64{}}
	failing := errors.New("disk trouble")
	files.hook = func(id int64) error {
		if id == b.ID {
			return failing
		}
		return nil
	}
	s := New(sqlDB, Limits{Workspace: 100}, files)
	_, err := s.ReserveDB(context.Background(), a, 10)
	if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrExceeded) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if api, ok := AsAPI(err); !ok || api.Status != 503 || api.Code != "quota_unavailable" {
		t.Fatalf("AsAPI = %+v, %v", api, ok)
	}
	// Nothing was reserved by the refused write.
	files.hook = nil
	if err := s.CheckDB(context.Background(), a, 100); err != nil {
		t.Fatalf("a refused write left a reservation: %v", err)
	}
}

func TestReleaseIsIdempotentAndNilSafe(t *testing.T) {
	sqlDB, a, _ := twoSpaces(t)
	s := New(sqlDB, Limits{Workspace: 100}, &fakeFiles{size: map[int64]int64{}})
	r, err := s.ReserveDB(context.Background(), a, 70)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CheckDB(context.Background(), a, 40); !errors.Is(err, ErrExceeded) {
		t.Fatalf("while held: %v", err)
	}
	r.Release()
	r.Release()
	var none *Reservation
	none.Release()
	if err := s.CheckDB(context.Background(), a, 100); err != nil {
		t.Fatalf("after release: %v", err)
	}
}
