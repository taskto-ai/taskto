package knowledge

import (
	"errors"
	"os"
	"path/filepath"
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
