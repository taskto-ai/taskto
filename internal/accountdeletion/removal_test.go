package accountdeletion

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/knowledge"
)

// lateWriterKB is a writer that had resolved the private space before it was removed and reaches
// it just after: it tries the write once the removal is done, as the purge goes on.
type lateWriterKB struct {
	Knowledge
	put func(sp access.Space) error
	got []error
}

func (k *lateWriterKB) RemoveSpace(sp access.Space) error {
	if err := k.Knowledge.RemoveSpace(sp); err != nil {
		return err
	}
	k.got = append(k.got, k.put(sp))
	return nil
}

// A write that reaches a private space between the removal of its files and the deletion of its
// rows used to make the directory and a repository again, files no row owned and nothing removed.
// It is refused, the space's ID can be used again once its rows are gone, and the purge of one
// account never affects another's writes.
func TestAWriteBetweenTheRemovalAndTheRowsDoesNotMakeThePrivateSpaceAgain(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	author := knowledge.Author{Name: "Late", Email: "late@example.com"}
	kb := &lateWriterKB{Knowledge: w.kb, put: func(sp access.Space) error {
		_, err := w.kb.Put(ctx, sp, "late.md", []byte("late"), "", author, "")
		return err
	}}
	w.svc.o.Knowledge = kb
	w.deleteAccount()

	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	if len(kb.got) != 1 || !errors.Is(kb.got[0], access.ErrNotFound) {
		t.Fatalf("the late write's result = %v, want the space not found", kb.got)
	}
	if _, err := os.Stat(w.private.Dir(w.dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the purged space's directory was made again: %v", err)
	}

	// The other people's spaces take writes as before: carol's private space and the shared one.
	for _, sp := range []access.Space{w.otherPrv, w.team} {
		if _, err := w.kb.Put(ctx, sp, "after.md", []byte("after"), "", author, ""); err != nil {
			t.Fatalf("a write to space %d after another account's purge: %v", sp.ID, err)
		}
	}

	// SQLite gives the largest ID again once its row is gone: a space made with the purged one's ID
	// is a new space, and works.
	exec(t, w.db, `INSERT INTO spaces (id, workspace_id, kind, name) VALUES (?, ?, 'shared', 'Reused')`, w.private.ID, w.ws)
	if _, err := w.kb.Put(ctx, w.private, "fresh.md", []byte("fresh"), "", author, ""); err != nil {
		t.Fatalf("a write to a space made with a purged space's ID: %v", err)
	}
}

// The writer that reaches the space only after the purge has deleted the rows and lifted the
// removal mark (a request that had resolved the space before) is refused too, with the server's
// check of the space's row set as the server sets it.
func TestAWriterThatArrivesAfterThePurgeIsRefused(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.kb.SetSpaceCheck(func(sp access.Space) (bool, error) {
		var one int
		err := w.db.QueryRow(`SELECT 1 FROM spaces WHERE id = ? AND workspace_id = ?`, sp.ID, sp.WorkspaceID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	})
	w.deleteAccount()
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	author := knowledge.Author{Name: "Late", Email: "late@example.com"}
	_, err := w.kb.Put(ctx, w.private, "late.md", []byte("late"), "", author, "")
	if !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("a write after the purge: %v, want the space not found", err)
	}
	if _, err := os.Stat(w.private.Dir(w.dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the purged space's directory was made again: %v", err)
	}
	if _, err := w.kb.Put(ctx, w.otherPrv, "after.md", []byte("after"), "", author, ""); err != nil {
		t.Fatalf("another account's space after the purge: %v", err)
	}
}
