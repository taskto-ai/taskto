package knowledge

import (
	"slices"
	"testing"

	"taskto.ai/taskto/internal/access"
)

func TestChanges(t *testing.T) {
	s, _ := newStore(t, Options{})
	sp := writable(1)
	var heard []int64
	s.OnChange(func(sp access.Space) { heard = append(heard, sp.ID) })

	d := must[Diff](t)(s.Changes(ctx, sp, ""))
	if d.Head != "" || !d.Full || len(d.Changes) != 0 {
		t.Fatalf("no repository: %+v", d)
	}
	if h := must[string](t)(s.Head(ctx, sp)); h != "" {
		t.Fatalf("head = %q", h)
	}

	a := must[Result](t)(s.Put(ctx, sp, "a.md", []byte("a"), "", alice, ""))
	b := must[Result](t)(s.Put(ctx, sp, "dir/b.md", []byte("b"), "", alice, ""))
	c := must[Result](t)(s.Put(ctx, sp, "dir/sub/c.png", []byte("c"), "", alice, ""))
	first := c.Commit
	if h := must[string](t)(s.Head(ctx, sp)); h != first {
		t.Fatalf("head = %s, want %s", h, first)
	}
	d = must[Diff](t)(s.Changes(ctx, sp, ""))
	want := []Change{{"a.md", a.Doc.Version}, {"dir/b.md", b.Doc.Version}, {"dir/sub/c.png", c.Doc.Version}}
	if d.Head != first || !d.Full || !slices.Equal(d.Changes, want) {
		t.Fatalf("everything: %+v", d)
	}

	a2 := must[Result](t)(s.Put(ctx, sp, "a.md", []byte("a2"), a.Doc.Version, alice, ""))
	must[Result](t)(s.Move(ctx, sp, "dir", "moved", "", alice, ""))
	must[Result](t)(s.Delete(ctx, sp, "moved/b.md", "", alice, ""))
	last := must[Result](t)(s.Put(ctx, sp, "new.md", []byte("n"), "", alice, ""))
	d = must[Diff](t)(s.Changes(ctx, sp, first))
	want = []Change{
		{"a.md", a2.Doc.Version}, {"dir/b.md", ""}, {"dir/sub/c.png", ""},
		{"moved/sub/c.png", c.Doc.Version}, {"new.md", last.Doc.Version},
	}
	if d.Head != last.Commit || d.Full || !slices.Equal(d.Changes, want) {
		t.Fatalf("since the first commit: %+v", d)
	}
	if d := must[Diff](t)(s.Changes(ctx, sp, last.Commit)); d.Full || len(d.Changes) != 0 {
		t.Fatalf("since the head: %+v", d)
	}
	for _, since := range []string{"ffffffffffffffffffffffffffffffffffffffff", "not a commit"} {
		if d := must[Diff](t)(s.Changes(ctx, sp, since)); !d.Full || len(d.Changes) != 3 {
			t.Fatalf("since %q: %+v", since, d)
		}
	}

	_, err := s.Put(ctx, sp, "a.md", []byte("x"), a.Doc.Version, alice, "")
	wantErr(t, err, ErrConflict)
	if len(heard) != 8 {
		t.Fatalf("heard %d changes, want 8", len(heard))
	}
	_, err = s.Changes(ctx, access.Space{ID: 1, WorkspaceID: 1, Access: access.None}, "")
	wantErr(t, err, access.ErrNotFound)
}
