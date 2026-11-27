package knowledge

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"testing"
	"time"
)

func TestListAtAndStreamReadOneCommit(t *testing.T) {
	s, _ := newStore(t, Options{LargeFileSize: 150})
	sp := writable(1)
	big := bytesOf(200, 'b')
	must[Result](t)(s.Put(ctx, sp, "a.md", []byte("one"), "", alice, ""))
	must[Result](t)(s.Put(ctx, sp, "dir/b.md", []byte("bee"), "", alice, ""))
	must[Result](t)(s.Put(ctx, sp, "big.bin", big, "", alice, ""))
	pinned := must[string](t)(s.Head(ctx, sp))

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
	if _, _, err := s.Stream(ctx, sp, "big.bin", pinned); err == nil {
		if got := read("big.bin"); string(got) != string(big) {
			t.Errorf("big.bin differs")
		}
	} else {
		wantErr(t, err, ErrNotKept)
	}

	_, rc, err := s.Stream(ctx, sp, "a.md", "")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.Put(ctx, sp, "later.md", []byte("x"), "", alice, "")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a write waits for a reader that hasn't closed its stream")
	}
	_ = rc.Close()

	if _, _, err := s.Stream(ctx, sp, "a.md", "0123456789012345678901234567890123456789"); err == nil {
		t.Error("a commit the repository doesn't have was read")
	}
	if _, err := s.ListAt(ctx, writable(99), "0123456789012345678901234567890123456789", "", "", 0); err == nil {
		t.Error("a listing at a commit of a space with no repository succeeded")
	}
	if page := must[Page](t)(s.ListAt(ctx, writable(99), "", "", "", 0)); len(page.Docs) != 0 {
		t.Errorf("a space with no repository lists %v", page.Docs)
	}
}

func TestStreamSpoolsALargerVersionedFile(t *testing.T) {
	old := streamMemory
	streamMemory = 8
	t.Cleanup(func() { streamMemory = old })
	s, _ := newStore(t, Options{})
	sp := writable(1)
	body := bytesOf(5000, 'q')
	must[Result](t)(s.Put(ctx, sp, "long.md", body, "", alice, ""))
	_, rc, err := s.Stream(ctx, sp, "long.md", "")
	if err != nil {
		t.Fatal(err)
	}
	f, ok := rc.(*spooled)
	if !ok {
		t.Fatalf("a %d byte file wasn't spooled: %T", len(body), rc)
	}
	name := f.Name()
	got, err := io.ReadAll(rc)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("read %d bytes (%v), want %d", len(got), err, len(body))
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(name); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the temporary file stays after Close: %v", err)
	}
}
