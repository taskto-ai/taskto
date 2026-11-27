package knowledge

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestPutIfRunsItsGuardUnderTheWriteLockAndStopsTheCommit(t *testing.T) {
	s, _ := newStore(t, Options{})
	sp := writable(1)
	stop := errors.New("revoked")

	if _, err := s.PutIf(ctx, sp, "a.md", []byte("x"), "", alice, "make a", func() error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("err = %v, want the guard's", err)
	}
	if _, err := s.Read(ctx, sp, "a.md", ""); err == nil {
		t.Fatal("a refused change was committed")
	}

	inGuard := make(chan struct{})
	var wrote sync.WaitGroup
	wrote.Add(1)
	go func() {
		defer wrote.Done()
		<-inGuard
		_, _ = s.Put(ctx, sp, "b.md", []byte("y"), "", bob, "make b")
	}()
	if _, err := s.PutIf(ctx, sp, "a.md", []byte("x"), "", alice, "make a", func() error {
		close(inGuard)
		time.Sleep(100 * time.Millisecond)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wrote.Wait()
	h := must[History](t)(s.History(ctx, sp, "", "", 10))
	if len(h.Commits) != 2 || h.Commits[0].Message != "make b" || h.Commits[1].Message != "make a" {
		t.Fatalf("history = %+v: the other writer didn't wait for the guard's change", h.Commits)
	}
}

func TestFindCommitFindsAChangeByItsMessageWhateverBecameOfTheDocument(t *testing.T) {
	s, _ := newStore(t, Options{})
	sp := writable(1)
	agent := Author{Name: "Alice", Email: "alice@example.com", Agent: "taskto app"}
	made := must[Result](t)(s.Put(ctx, sp, "a.md", []byte("x"), "", agent, "Created by an app [effect 1/2]"))
	must[Result](t)(s.Put(ctx, sp, "a.md", []byte("edited"), made.Doc.Version, alice, "edit"))
	must[Result](t)(s.Move(ctx, sp, "a.md", "moved/a.md", "", alice, "move"))

	c, ok, err := s.FindCommit(ctx, sp, "Created by an app [effect 1/2]", "taskto app")
	if err != nil || !ok || c.Message != "Created by an app [effect 1/2]" {
		t.Fatalf("found = %+v, %v, %v", c, ok, err)
	}
	if _, ok, _ := s.FindCommit(ctx, sp, "Created by an app [effect 1/3]", "taskto app"); ok {
		t.Fatal("found another entry's commit")
	}
	must[Result](t)(s.Put(ctx, sp, "c.md", []byte("z"), "", alice, "Created by an app [effect 1/9]"))
	if _, ok, _ := s.FindCommit(ctx, sp, "Created by an app [effect 1/9]", "taskto app"); ok {
		t.Fatal("found a commit a person made")
	}
	if _, ok, err := s.FindCommit(ctx, writable(2), "x", ""); ok || err != nil {
		t.Fatalf("empty space: %v, %v", ok, err)
	}
}
