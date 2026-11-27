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

func near(got, disk int64) bool { return got <= disk && disk-got < 4<<10 }

func TestUsageFollowsEveryChange(t *testing.T) {
	s, dataDir := newStore(t, Options{LargeFileSize: 150})
	sp := writable(1)

	v, u, err := s.Usage(sp)
	if err != nil || v != 0 || u != 0 {
		t.Fatalf("a space nobody wrote = %d, %d, %v", v, u, err)
	}
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
	r = must[Result](t)(s.Put(ctx, sp, "big.bin", bytesOf(180, 'c'), r.Doc.Version, alice, ""))
	if _, u, _ = s.Usage(sp); u != 180 {
		t.Fatalf("after replacing it = %d, want 180", u)
	}
	must[Result](t)(s.Delete(ctx, sp, "big.bin", r.Doc.Version, alice, ""))
	if _, u, _ = s.Usage(sp); u != 0 {
		t.Fatalf("after deleting it = %d, want 0", u)
	}

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
	if len(asked) != 1 || asked[0] != 5+changeOverhead {
		t.Fatalf("a document asked %v, want [%d]", asked, 5+changeOverhead)
	}
	asked = nil
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

	must[Result](t)(s.Move(ctx, sp, "a.md", "moved.md", doc.Doc.Version, alice, ""))
	must[Result](t)(s.Delete(ctx, sp, "big.bin", must[Doc](t)(s.Read(ctx, sp, "big.bin", "")).Version, alice, ""))
}

func TestAFirstMeasurementThatARacedWriteOvertookAnswersThePublishedFigure(t *testing.T) {
	dir := t.TempDir()
	sp := writable(1)
	must[Result](t)(New(dir, Options{}).Put(ctx, sp, "a.md", []byte("hello"), "", alice, ""))

	s := New(dir, Options{})
	var published = usage{versioned: 123456, unversioned: 7}
	s.afterMeasure = func(got access.Space) {
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
	must[Result](t)(s.Put(ctx, sp, "big.bin", bytesOf(200, 'b'), "", alice, ""))
	if held != released || held < 2 {
		t.Fatalf("held %d, released %d after a large file", held, released)
	}
}

func TestReplacingAnUnversionedFileNearTheCapIsCheckedByItsNetSize(t *testing.T) {
	s, _ := newStore(t, Options{LargeFileSize: 150})
	sp := writable(1)
	var used atomic.Int64
	const capacity = 10_000
	s.SetQuota(func(_ access.Space, adding int64) (func(), error) {
		if adding > 0 && used.Load()+adding > capacity {
			return nil, &quota.Error{Scope: quota.ScopeWorkspace, Limit: capacity, Used: used.Load()}
		}
		return func() {}, nil
	})
	first := must[Result](t)(s.Put(ctx, sp, "big.bin", bytesOf(6000, 'a'), "", alice, ""))
	used.Store(capacity - 1100)

	if _, err := s.Put(ctx, sp, "big.bin", bytesOf(6600, 'b'), first.Doc.Version, alice, ""); !errors.Is(err, quota.ErrExceeded) {
		t.Fatalf("a replacement that grows past the room: %v, want the quota's", err)
	}
	shrunk := must[Result](t)(s.Put(ctx, sp, "big.bin", bytesOf(5000, 'c'), first.Doc.Version, alice, ""))
	if shrunk.Doc.Size != 5000 {
		t.Fatalf("the shrunk file = %+v", shrunk.Doc)
	}
	must[Result](t)(s.Put(ctx, sp, "big.bin", bytesOf(5000, 'd'), shrunk.Doc.Version, alice, ""))
}
