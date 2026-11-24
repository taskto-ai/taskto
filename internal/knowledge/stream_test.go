package knowledge

import (
	"io"
	"testing"
)

// ListAt and Stream read one commit whatever the space does meanwhile: a file replaced, renamed or
// deleted after it is still listed, and read, as it was.
func TestListAtAndStreamReadOneCommit(t *testing.T) {
	s, _ := newStore(t, Options{LargeFileSize: 150})
	sp := writable(1)
	big := bytesOf(200, 'b')
	must[Result](t)(s.Put(ctx, sp, "a.md", []byte("one"), "", alice, ""))
	must[Result](t)(s.Put(ctx, sp, "dir/b.md", []byte("bee"), "", alice, ""))
	must[Result](t)(s.Put(ctx, sp, "big.bin", big, "", alice, ""))
	pinned := must[string](t)(s.Head(ctx, sp))

	// The space changes after the commit is taken: a replaced, a renamed, a deleted file.
	a := must[Doc](t)(s.Read(ctx, sp, "a.md", ""))
	must[Result](t)(s.Put(ctx, sp, "a.md", []byte("two"), a.Version, alice, ""))
	must[Result](t)(s.Move(ctx, sp, "dir", "folder", "", alice, ""))
	must[Result](t)(s.Delete(ctx, sp, "big.bin", "", alice, ""))

	var paths []string
	after := ""
	for {
		page := must[Page](t)(s.ListAt(ctx, sp, pinned, "", after, 1))
		for _, d := range page.Docs {
			paths = append(paths, d.Path)
		}
		if page.Next == "" {
			break
		}
		after = page.Next
	}
	if want := []string{"a.md", "big.bin", "dir/b.md"}; len(paths) != len(want) || paths[0] != want[0] || paths[1] != want[1] || paths[2] != want[2] {
		t.Fatalf("listed at the pinned commit: %v, want %v", paths, want)
	}

	read := func(name string) []byte {
		t.Helper()
		_, rc, err := s.Stream(ctx, sp, name, pinned)
		if err != nil {
			t.Fatalf("Stream %s: %v", name, err)
		}
		b, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		if err := rc.Close(); err != nil {
			t.Fatal(err)
		}
		return b
	}
	if got := string(read("a.md")); got != "one" {
		t.Errorf("a.md at the pinned commit = %q, want its first version", got)
	}
	if got := string(read("dir/b.md")); got != "bee" {
		t.Errorf("dir/b.md = %q", got)
	}
	// A file deleted since: its unversioned bytes are gone with it, which Stream says.
	if _, _, err := s.Stream(ctx, sp, "big.bin", pinned); err == nil {
		if got := read("big.bin"); string(got) != string(big) {
			t.Errorf("big.bin differs")
		}
	} else {
		wantErr(t, err, ErrNotKept)
	}

	// Closing a stream lets writers in: the space isn't held after it.
	_, rc, err := s.Stream(ctx, sp, "a.md", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	must[Result](t)(s.Put(ctx, sp, "later.md", []byte("x"), "", alice, ""))

	if _, _, err := s.Stream(ctx, sp, "a.md", "0123456789012345678901234567890123456789"); err == nil {
		t.Error("a commit the repository doesn't have was read")
	}
}
