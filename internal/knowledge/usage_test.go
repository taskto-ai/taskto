package knowledge

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/quota"
)

// gitBytes is what .git holds, the unversioned files apart, counted from the disk.
func gitBytes(t *testing.T, dataDir string, sp access.Space) int64 {
	t.Helper()
	root, err := os.OpenRoot(sp.Dir(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	n, err := gitSize(root.FS())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// near reports whether the counter's figure is the disk's, to within git's own bookkeeping
// (refs, the index) which the incremental count of the objects a commit stores leaves out.
func near(got, disk int64) bool { return got <= disk && disk-got < 4<<10 }

func TestUsageFollowsEveryChange(t *testing.T) {
	s, dataDir := newStore(t, Options{LargeFileSize: 150})
	sp := writable(1)

	v, u, err := s.Usage(sp)
	if err != nil || v != 0 || u != 0 {
		t.Fatalf("a space nobody wrote = %d, %d, %v", v, u, err)
	}
	// The zero was not kept: a first write is seen at once.
	must[Result](t)(s.Put(ctx, sp, "a.md", []byte("hello"), "", alice, ""))
	v, u, err = s.Usage(sp)
	if err != nil || u != 0 || v <= 0 || !near(v, gitBytes(t, dataDir, sp)) {
		t.Fatalf("after a document = %d, %d, %v; .git has %d", v, u, err, gitBytes(t, dataDir, sp))
	}

	big := bytesOf(200, 'b')
	r := must[Result](t)(s.Put(ctx, sp, "big.bin", big, "", alice, ""))
	v, u, _ = s.Usage(sp)
	if u != 200 || !near(v, gitBytes(t, dataDir, sp)) {
		t.Fatalf("after an unversioned file = %d, %d; want 200 and .git's %d", v, u, gitBytes(t, dataDir, sp))
	}
	// Replacing it frees its bytes, deleting too.
	r = must[Result](t)(s.Put(ctx, sp, "big.bin", bytesOf(180, 'c'), r.Doc.Version, alice, ""))
	if _, u, _ = s.Usage(sp); u != 180 {
		t.Fatalf("after replacing it = %d, want 180", u)
	}
	must[Result](t)(s.Delete(ctx, sp, "big.bin", r.Doc.Version, alice, ""))
	if _, u, _ = s.Usage(sp); u != 0 {
		t.Fatalf("after deleting it = %d, want 0", u)
	}

	// A server that starts again measures the same.
	again := New(dataDir, Options{LargeFileSize: 150})
	av, au, err := again.Usage(sp)
	if v, _, _ = s.Usage(sp); err != nil || !near(v, av) || au != 0 {
		t.Fatalf("measured from the disk = %d, %d, %v; the counters say %d, 0", av, au, err, v)
	}
}

func TestUsageOfASpaceWithoutDocumentsIsZeroAndCreatesNothing(t *testing.T) {
	s, dataDir := newStore(t, Options{})
	sp := writable(7)
	if v, u, err := s.Usage(sp); err != nil || v != 0 || u != 0 {
		t.Fatalf("Usage = %d, %d, %v", v, u, err)
	}
	if _, err := os.Stat(filepath.Dir(sp.Dir(dataDir))); err == nil {
		t.Fatal("measuring a space made its directory")
	}
}

func TestQuotaHookRefusesContentThatDoesNotFit(t *testing.T) {
	s, _ := newStore(t, Options{LargeFileSize: 150})
	sp := writable(1)
	var asked []int64
	var refuse atomic.Bool
	s.SetQuota(func(got access.Space, adding int64) (func(), error) {
		if got.ID != sp.ID {
			t.Errorf("asked about space %d", got.ID)
		}
		asked = append(asked, adding)
		if refuse.Load() {
			return nil, &quota.Error{Scope: quota.ScopeUser, Limit: 1 << 20, Used: 1 << 20}
		}
		return func() {}, nil
	})

	doc := must[Result](t)(s.Put(ctx, sp, "a.md", []byte("hello"), "", alice, ""))
	// A document asks for its bytes and the commit's overhead.
	if len(asked) != 1 || asked[0] != 5+changeOverhead {
		t.Fatalf("a document asked %v, want [%d]", asked, 5+changeOverhead)
	}
	asked = nil
	// A large file asks once, for its own bytes and the commit's overhead, when it knows what the
	// file it replaces frees.
	big := bytesOf(200, 'b')
	must[Result](t)(s.Put(ctx, sp, "big.bin", big, "", alice, ""))
	if len(asked) != 1 || asked[0] != 200+changeOverhead {
		t.Fatalf("an unversioned file asked %v, want [%d]", asked, 200+changeOverhead)
	}

	refuse.Store(true)
	for name, put := range map[string]func() error{
		"document": func() error { _, err := s.Put(ctx, sp, "b.md", []byte("x"), "", alice, ""); return err },
		"update":   func() error { _, err := s.Put(ctx, sp, "a.md", []byte("hi"), doc.Doc.Version, alice, ""); return err },
		"large":    func() error { _, err := s.Put(ctx, sp, "c.bin", bytesOf(300, 'c'), "", alice, ""); return err },
	} {
		err := put()
		var over *quota.Error
		if !errors.As(err, &over) || !errors.Is(err, quota.ErrExceeded) {
			t.Fatalf("%s: err = %v, want the quota's", name, err)
		}
	}
	if _, err := s.Read(ctx, sp, "b.md", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused document was written: %v", err)
	}
	if got := objectsOnDisk(t, s.dataDir, sp); len(got) != 1 {
		t.Fatalf("a refused file left bytes: %v", got)
	}

	// A change that adds nothing is never refused: a full space can make room.
	must[Result](t)(s.Move(ctx, sp, "a.md", "moved.md", doc.Doc.Version, alice, ""))
	must[Result](t)(s.Delete(ctx, sp, "big.bin", must[Doc](t)(s.Read(ctx, sp, "big.bin", "")).Version, alice, ""))
}

// A first measurement that a write publishes under is dropped: what it answers, and what is kept,
// is the write's figure, never the walk's of files that were changing.
func TestAFirstMeasurementThatARacedWriteOvertookAnswersThePublishedFigure(t *testing.T) {
	dir := t.TempDir()
	sp := writable(1)
	must[Result](t)(New(dir, Options{}).Put(ctx, sp, "a.md", []byte("hello"), "", alice, ""))

	s := New(dir, Options{}) // a server that started since: nothing is published for the space
	var published = usage{versioned: 123456, unversioned: 7}
	s.afterMeasure = func(got access.Space) {
		// A write finishes while the walk is still in hand.
		s.state(got.ID).publish(&published)
	}
	v, u, err := s.Usage(sp)
	if err != nil || v != published.versioned || u != published.unversioned {
		t.Fatalf("Usage = %d, %d, %v; want the published %+v", v, u, err, published)
	}
	s.afterMeasure = nil
	if v, u, err := s.Usage(sp); err != nil || v != published.versioned || u != published.unversioned {
		t.Fatalf("Usage after = %d, %d, %v; the walk overwrote the published figure", v, u, err)
	}
}

// A measurement is stored under the generation seen when the cache was found empty, in the same
// look: a write that published in between can't be overwritten by it.
func TestFillNeverReplacesWhatWasPublished(t *testing.T) {
	st := &spaceState{}
	_, gen, ok := st.published()
	if ok {
		t.Fatal("a new space has a published usage")
	}
	st.publish(&usage{versioned: 10})
	if got := st.fill(gen, usage{versioned: 99}); got.versioned != 10 {
		t.Fatalf("fill answered %+v, want the published one", got)
	}
	if u, _, ok := st.published(); !ok || u.versioned != 10 {
		t.Fatalf("published = %+v, %v", u, ok)
	}
	// Cleared by a write that couldn't measure: nothing is published, so the walk's figure answers, unstored.
	st.publish(nil)
	_, gen, _ = st.published()
	st.publish(nil)
	if got := st.fill(gen, usage{versioned: 5}); got.versioned != 5 {
		t.Fatalf("fill answered %+v, want the measurement", got)
	}
	if _, _, ok := st.published(); ok {
		t.Fatal("a stale measurement was stored")
	}
}

// What a change reserves is held until its usage is published, and given back then, whether the
// change worked or not.
func TestQuotaReservationsAreReleasedAfterTheUsageIsPublished(t *testing.T) {
	s, _ := newStore(t, Options{LargeFileSize: 150})
	sp := writable(1)
	var held, released int
	var atRelease []usage
	s.SetQuota(func(access.Space, int64) (func(), error) {
		held++
		return func() {
			released++
			u, _, ok := s.state(sp.ID).published()
			if !ok {
				t.Error("released before the usage was published")
			}
			atRelease = append(atRelease, u)
		}, nil
	})
	must[Result](t)(s.Put(ctx, sp, "a.md", []byte("hello"), "", alice, ""))
	if held != 1 || released != 1 {
		t.Fatalf("held %d, released %d after a document", held, released)
	}
	if atRelease[0].versioned <= 0 {
		t.Fatalf("usage at release = %+v, want the commit's bytes counted", atRelease[0])
	}
	// A change that reserves and then fails (a base that is well formed but not the version) gives
	// its bytes back all the same: the reservation is made before the base is compared.
	before := held
	if _, err := s.Put(ctx, sp, "a.md", []byte("hi"), strings.Repeat("0", 40), alice, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("a stale put: %v, want a conflict", err)
	}
	if held != before+1 {
		t.Fatalf("the stale put reserved %d times, want once", held-before)
	}
	if held != released {
		t.Fatalf("held %d, released %d: a failed change kept its reservation", held, released)
	}
	// An unversioned file reserves once its net size is known, and releases too.
	must[Result](t)(s.Put(ctx, sp, "big.bin", bytesOf(200, 'b'), "", alice, ""))
	if held != released || held < 2 {
		t.Fatalf("held %d, released %d after a large file", held, released)
	}
}

// Replacing or shrinking an unversioned file at the cap is judged by what it changes: the file
// it replaces is subtracted before the check, not after a refusal.
func TestReplacingAnUnversionedFileNearTheCapIsCheckedByItsNetSize(t *testing.T) {
	s, _ := newStore(t, Options{LargeFileSize: 150})
	sp := writable(1)
	var used atomic.Int64 // what the quota thinks the workspace stores
	const capacity = 10_000
	s.SetQuota(func(_ access.Space, adding int64) (func(), error) {
		if adding > 0 && used.Load()+adding > capacity {
			return nil, &quota.Error{Scope: quota.ScopeWorkspace, Limit: capacity, Used: used.Load()}
		}
		return func() {}, nil
	})
	first := must[Result](t)(s.Put(ctx, sp, "big.bin", bytesOf(6000, 'a'), "", alice, ""))
	used.Store(capacity - 1100) // room for the commit's overhead and little more

	// Growing by 600 doesn't fit; it is the net growth and the commit that are refused.
	if _, err := s.Put(ctx, sp, "big.bin", bytesOf(6600, 'b'), first.Doc.Version, alice, ""); !errors.Is(err, quota.ErrExceeded) {
		t.Fatalf("a replacement that grows past the room: %v, want the quota's", err)
	}
	// Shrinking, or replacing with a file as large, fits: the bytes it frees pay for the new ones.
	shrunk := must[Result](t)(s.Put(ctx, sp, "big.bin", bytesOf(5000, 'c'), first.Doc.Version, alice, ""))
	if shrunk.Doc.Size != 5000 {
		t.Fatalf("the shrunk file = %+v", shrunk.Doc)
	}
	must[Result](t)(s.Put(ctx, sp, "big.bin", bytesOf(5000, 'd'), shrunk.Doc.Version, alice, ""))
}
