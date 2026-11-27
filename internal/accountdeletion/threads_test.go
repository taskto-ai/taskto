package accountdeletion

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/conversations"
	"taskto.ai/taskto/internal/knowledge"
)

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

func (k *failingKB) AnonymizeAuthorsNoting(ctx context.Context, sp access.Space, emails []string, planned func(int) error) (int, error) {
	k.mu.Lock()
	k.rewrote = append(k.rewrote, sp.ID)
	fail := k.failIDs[sp.ID]
	k.mu.Unlock()
	if fail {
		return 0, errors.New("cannot rewrite")
	}
	return k.Knowledge.AnonymizeAuthorsNoting(ctx, sp, emails, planned)
}

func TestOneStuckPrivateSpaceDoesNotStopTheRemovalOfTheOthers(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	ws2 := exec(t, w.db, `INSERT INTO workspaces (name) VALUES ('Second')`)
	exec(t, w.db, `INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, ws2, w.leaver)
	if n := w.count(`SELECT count(*) FROM spaces WHERE owner_id = ?`, w.leaver); n != 2 {
		t.Fatalf("fixture: %d private spaces", n)
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

type hangingAgent struct{ conversations.Purger }

func (hangingAgent) RetireConversation(context.Context, int64) error { return nil }

func (hangingAgent) PurgeConversation(ctx context.Context, _ int64) error {
	<-ctx.Done()
	return ctx.Err()
}

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

func TestAnAgentRuntimeThatIsNeverReadyFailsTheStepAtItsDeadlineAndLaterStepsRun(t *testing.T) {
	w := newWorld(t)
	w.svc.stepTimeout = 50 * time.Millisecond
	w.svc.SetAgent(func(ctx context.Context) (conversations.Purger, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	w.deleteAccount()
	exec(t, w.db, `INSERT INTO purge_cleanups (user_id, kind, target, created_at) VALUES (?, ?, '101', '2026-01-01T00:00:00.000Z')`,
		w.leaver, cleanAgentConversation)
	exec(t, w.db, `INSERT INTO purge_cleanups (user_id, kind, target, created_at) VALUES (?, ?, 'feed:1', '2026-01-01T00:00:01.000Z')`,
		w.leaver, cleanProviders)
	done := make(chan error, 1)
	go func() { _, err := w.svc.runCleanups(context.Background(), 0); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a step whose runtime was never ready reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiting for the runtime has no deadline")
	}
	var kinds []string
	rows, err := w.db.Query(`SELECT kind FROM purge_cleanups`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, k)
	}
	if len(kinds) != 1 || kinds[0] != cleanAgentConversation {
		t.Fatalf("steps left %v, want only the agent step (the providers step ran)", kinds)
	}
}

func TestAPurgedSpaceIDCanBeUsedAgain(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	by := knowledge.Author{Name: "Someone", Email: "someone@example.com"}
	w.deleteAccount()
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM spaces WHERE id = ?`, w.private.ID); n != 0 {
		t.Fatalf("fixture: the space's row remains")
	}
	if _, err := w.kb.Put(ctx, w.private, "again.md", []byte("a new space with the old ID"), "", by, ""); err != nil {
		t.Fatalf("the ID of a purged space can't be used again: %v", err)
	}
}

func TestAWriteThatResolvedASpaceBeforeThePurgeMakesNothingAfterIt(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.kb.SetSpaceCheck(access.SpaceRowCheck(w.db))
	by := knowledge.Author{Name: "Someone", Email: "someone@example.com"}
	stale := w.private
	w.deleteAccount()
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	if _, err := w.kb.Put(ctx, stale, "late.md", []byte("from a purged account"), "", by, ""); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("a stale write after the purge: %v", err)
	}
	if _, err := os.Stat(stale.Dir(w.dir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the purged space's directory was made again: %v", err)
	}
}

func TestForgetGoneLiftsTheRefusalOfSpacesWhoseRowIsGone(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	by := knowledge.Author{Name: "Someone", Email: "someone@example.com"}
	ws2 := exec(t, w.db, `INSERT INTO workspaces (name) VALUES ('Second')`)
	exec(t, w.db, `INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, ws2, w.leaver)
	second := access.Space{Access: access.Write, WorkspaceID: ws2}
	if err := w.db.QueryRow(`SELECT id FROM spaces WHERE owner_id = ? AND workspace_id = ?`, w.leaver, ws2).Scan(&second.ID); err != nil {
		t.Fatal(err)
	}
	for _, sp := range []access.Space{w.private, second} {
		if err := w.kb.RemoveSpace(sp); err != nil {
			t.Fatal(err)
		}
	}
	exec(t, w.db, `DELETE FROM spaces WHERE id = ?`, w.private.ID)

	w.svc.forgetGone(ctx, []access.Space{w.private, second})

	if _, err := w.kb.Put(ctx, w.private, "again.md", []byte("x"), "", by, ""); err != nil {
		t.Fatalf("a space whose row is gone is still refused: %v", err)
	}
	if _, err := w.kb.Put(ctx, second, "late.md", []byte("x"), "", by, ""); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("a space that is still recorded was forgotten: %v", err)
	}
}
