package accountdeletion

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/conversations"
)

// failingKB fails the removal and the rewriting of the spaces it is told to, and notes every space it
// was asked about.
type failingKB struct {
	Knowledge
	mu      sync.Mutex
	failIDs map[int64]bool
	removed []int64
	rewrote []int64
}

func (k *failingKB) RemoveSpace(sp access.Space) error {
	k.mu.Lock()
	k.removed = append(k.removed, sp.ID)
	fail := k.failIDs[sp.ID]
	k.mu.Unlock()
	if fail {
		return errors.New("cannot remove")
	}
	return k.Knowledge.RemoveSpace(sp)
}

func (k *failingKB) AnonymizeAuthors(ctx context.Context, sp access.Space, emails []string) (int, error) {
	k.mu.Lock()
	k.rewrote = append(k.rewrote, sp.ID)
	fail := k.failIDs[sp.ID]
	k.mu.Unlock()
	if fail {
		return 0, errors.New("cannot rewrite")
	}
	return k.Knowledge.AnonymizeAuthors(ctx, sp, emails)
}

// A private space whose directory can't be removed doesn't keep the leaver's other private spaces
// from being removed, and the account stays until all are gone.
func TestOneStuckPrivateSpaceDoesNotStopTheRemovalOfTheOthers(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	ws2 := exec(t, w.db, `INSERT INTO workspaces (name) VALUES ('Second')`)
	exec(t, w.db, `INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, ws2, w.leaver)
	if n := w.count(`SELECT count(*) FROM spaces WHERE owner_id = ?`, w.leaver); n != 2 {
		t.Fatalf("fixture: %d private spaces", n) // a membership makes its private space
	}
	kb := &failingKB{Knowledge: w.kb, failIDs: map[int64]bool{w.private.ID: true}}
	w.svc.o.Knowledge = kb
	w.deleteAccount()
	if err := w.svc.Purge(ctx, w.leaver); err == nil {
		t.Fatal("the purge reported success with a directory left")
	}
	if len(kb.removed) != 2 {
		t.Fatalf("removal tried for %v, want both private spaces", kb.removed)
	}
	if n := w.count(`SELECT count(*) FROM users WHERE id = ?`, w.leaver); n != 1 {
		t.Fatal("the account went with a directory left")
	}
	kb.failIDs = nil
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
}

// A shared repository that can't be rewritten doesn't keep the others' histories from being
// anonymized, and the purge stays pending.
func TestOneStuckSharedRepositoryDoesNotStopTheRewritingOfTheOthers(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	other := exec(t, w.db, `INSERT INTO spaces (workspace_id, kind, name) VALUES (?, 'shared', 'Other')`, w.ws)
	kb := &failingKB{Knowledge: w.kb, failIDs: map[int64]bool{w.team.ID: true}}
	w.svc.o.Knowledge = kb
	w.deleteAccount()
	if err := w.svc.Purge(ctx, w.leaver); err == nil {
		t.Fatal("the purge reported success with a history left")
	}
	found := false
	for _, id := range kb.rewrote {
		found = found || id == other
	}
	if !found {
		t.Fatalf("rewriting tried for %v, not for the second shared space %d", kb.rewrote, other)
	}
	if n := w.count(`SELECT count(*) FROM users WHERE id = ?`, w.leaver); n != 1 {
		t.Fatal("the account went with a history left")
	}
}

// movesApps fails FinishMoves for one space and notes the spaces it was asked about.
type movesApps struct {
	Apps
	mu    sync.Mutex
	fail  int64
	moved []int64
}

func (a *movesApps) FinishMoves(ctx context.Context, space int64) error {
	a.mu.Lock()
	a.moved = append(a.moved, space)
	a.mu.Unlock()
	if space == a.fail {
		return errors.New("unreadable attachment")
	}
	return a.Apps.FinishMoves(ctx, space)
}

// An app move that can't finish in one private space doesn't skip the others, and nothing is removed
// until all are finished.
func TestOneStuckAppMoveDoesNotSkipTheOtherPrivateSpaces(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	ws2 := exec(t, w.db, `INSERT INTO workspaces (name) VALUES ('Second')`)
	exec(t, w.db, `INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, ws2, w.leaver)
	var second int64
	if err := w.db.QueryRow(`SELECT id FROM spaces WHERE owner_id = ? AND workspace_id = ?`, w.leaver, ws2).Scan(&second); err != nil {
		t.Fatal(err)
	}
	a := &movesApps{Apps: w.apps, fail: w.private.ID}
	w.svc.SetApps(a)
	kb := &failingKB{Knowledge: w.kb}
	w.svc.o.Knowledge = kb
	w.deleteAccount()
	if err := w.svc.Purge(ctx, w.leaver); err == nil {
		t.Fatal("the purge reported success with a move unfinished")
	}
	if len(a.moved) != 2 || a.moved[1] != second {
		t.Fatalf("moves tried for %v, want both private spaces", a.moved)
	}
	if len(kb.removed) != 0 {
		t.Fatalf("removed %v with a move unfinished", kb.removed)
	}
}

// hangingAgent never answers until its context ends.
type hangingAgent struct{ conversations.Purger }

func (hangingAgent) RetireConversation(context.Context, int64) error { return nil }

func (hangingAgent) PurgeConversation(ctx context.Context, _ int64) error {
	<-ctx.Done()
	return ctx.Err()
}

// A runtime that accepts a call and never answers fails the step at its deadline: it doesn't hold
// the maintenance loop, and the step stays recorded.
func TestASilentAgentRuntimeFailsTheStepAtItsDeadline(t *testing.T) {
	w := newWorld(t)
	w.svc.stepTimeout = 50 * time.Millisecond
	w.svc.SetAgent(func(context.Context) (conversations.Purger, error) { return hangingAgent{}, nil })
	w.deleteAccount()
	exec(t, w.db, `INSERT INTO purge_cleanups (user_id, kind, target, created_at) VALUES (?, ?, '101', '2026-01-01T00:00:00.000Z')`,
		w.leaver, cleanAgentConversation)
	done := make(chan error, 1)
	go func() { _, err := w.svc.runCleanups(context.Background(), 0); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a step that never answered reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the step has no deadline")
	}
	if n, _ := w.svc.Pending(context.Background(), 0); n != 1 {
		t.Fatalf("%d steps recorded, want the one that failed", n)
	}
}
