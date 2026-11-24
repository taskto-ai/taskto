package access

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"taskto.ai/taskto/internal/auth"
)

func memberIDs(members []Member) []int64 {
	ids := []int64{}
	for _, m := range members {
		ids = append(ids, m.ID)
	}
	return ids
}

func TestWorkspaceMembersPages(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	ids := func(members []auth.Member) []int64 {
		out := []int64{}
		for _, m := range members {
			out = append(out, m.ID)
		}
		return out
	}

	all, err := w.e.c.WorkspaceMembers(ctx, w.alice.id, w.ws, auth.RosterPage{ByID: true})
	if err != nil {
		t.Fatal(err)
	}
	// The owner, admin2, alice, bob and mallory; gone and stranger have left.
	want := []int64{w.owner.id, w.admin2.id, w.alice.id, w.bob.id, w.mallory.id}
	if got := ids(all); !slices.Equal(got, want) {
		t.Fatalf("members = %v, want %v", got, want)
	}
	page, err := w.e.c.WorkspaceMembers(ctx, w.alice.id, w.ws, auth.RosterPage{AfterID: w.admin2.id, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(page); !slices.Equal(got, want[2:4]) {
		t.Fatalf("a page after admin2, of 2 = %v, want %v", got, want[2:4])
	}

	// Not a member of the workspace: the same answer as for a workspace that doesn't exist.
	for _, who := range []int64{w.gone.id, w.stranger.id} {
		if _, err := w.e.c.WorkspaceMembers(ctx, who, w.ws, auth.RosterPage{}); !errors.Is(err, ErrNotFound) {
			t.Errorf("user %d: err = %v, want ErrNotFound", who, err)
		}
	}
	if _, err := w.e.c.WorkspaceMembers(ctx, w.alice.id, w.other, auth.RosterPage{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("another workspace: err = %v, want ErrNotFound", err)
	}
}

func TestReadableSpaceMembersIsOfTheCallsWorkspace(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	_, members, err := w.e.c.ReadableSpaceMembers(ctx, w.alice.id, w.ws, w.shared.ID, auth.RosterPage{ByID: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := memberIDs(members), []int64{w.owner.id, w.alice.id, w.bob.id}; !slices.Equal(got, want) {
		t.Fatalf("members of shared = %v, want %v", got, want)
	}
	_, members, err = w.e.c.ReadableSpaceMembers(ctx, w.alice.id, w.ws, w.shared.ID, auth.RosterPage{AfterID: w.owner.id, Limit: 1})
	if err != nil || !slices.Equal(memberIDs(members), []int64{w.alice.id}) {
		t.Fatalf("a page of shared after the owner, of 1 = %v, %v", memberIDs(members), err)
	}
	// A private space has its owner alone, past which there is nothing.
	_, members, err = w.e.c.ReadableSpaceMembers(ctx, w.alice.id, w.ws, w.alicePrivate.ID, auth.RosterPage{})
	if err != nil || !slices.Equal(memberIDs(members), []int64{w.alice.id}) {
		t.Fatalf("members of alice's private space = %v, %v", memberIDs(members), err)
	}
	_, members, err = w.e.c.ReadableSpaceMembers(ctx, w.alice.id, w.ws, w.alicePrivate.ID, auth.RosterPage{AfterID: w.alice.id})
	if err != nil || len(members) != 0 {
		t.Fatalf("members of alice's private space after her = %v, %v", memberIDs(members), err)
	}

	// A space the user reads, asked about as of another workspace they are also in, is not found.
	both := w.e.join(w.owner, w.ws, "Both", auth.RoleMember)
	if _, err := w.e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, w.other, both.id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.e.c.ReadableSpaceMembers(ctx, both.id, w.other, w.ownerPrivate.ID, auth.RosterPage{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a private space of another user: err = %v, want ErrNotFound", err)
	}
	if _, _, err := w.e.c.ReadableSpaceMembers(ctx, both.id, w.other, both.id+1000, auth.RosterPage{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a space that doesn't exist: err = %v, want ErrNotFound", err)
	}
	mine := w.e.private(both, w.ws)
	if _, _, err := w.e.c.ReadableSpaceMembers(ctx, both.id, w.other, mine.ID, auth.RosterPage{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a readable space of workspace %d asked for as of %d: err = %v, want ErrNotFound", w.ws, w.other, err)
	}
	if _, _, err := w.e.c.ReadableSpaceMembers(ctx, both.id, w.ws, mine.ID, auth.RosterPage{}); err != nil {
		t.Errorf("the same, as of its own workspace: %v", err)
	}
	// Not shared with the user, nor readable: the same.
	if _, _, err := w.e.c.ReadableSpaceMembers(ctx, w.mallory.id, w.ws, w.shared.ID, auth.RosterPage{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a shared space the user isn't in: err = %v, want ErrNotFound", err)
	}
}

// What a call checks and what it lists are of one snapshot: while bob and mallory take each
// other's place in the shared space, in one transaction each time, a list that bob gets has
// bob in it. Checked apart, bob could pass the check and then be listed as gone, with
// mallory in his place.
func TestSpaceMembersAreOfTheSnapshotTheyAreCheckedIn(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	swap := func(out, in int64) {
		t.Helper()
		tx, err := w.e.db.BeginTx(ctx, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, `DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, w.shared.ID, out); err != nil {
			t.Error(err)
			return
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO space_members (space_id, user_id, role) VALUES (?, ?, 'viewer')`, w.shared.ID, in); err != nil {
			t.Error(err)
			return
		}
		if err := tx.Commit(); err != nil {
			t.Error(err)
		}
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			swap(w.bob.id, w.mallory.id)
			swap(w.mallory.id, w.bob.id)
		}
	}()
	defer func() { close(stop); wg.Wait() }()

	listed := 0
	for i := 0; i < 3000 && !t.Failed(); i++ {
		_, members, err := w.e.c.ReadableSpaceMembers(ctx, w.bob.id, w.ws, w.shared.ID, auth.RosterPage{ByID: true})
		if errors.Is(err, ErrNotFound) {
			continue // bob wasn't in it then
		}
		if err != nil {
			t.Fatal(err)
		}
		listed++
		seen := false
		for _, m := range members {
			seen = seen || m.ID == w.bob.id
		}
		if !seen {
			t.Fatalf("bob passed the check and was not listed: %v", memberIDs(members))
		}
	}
	if listed == 0 {
		t.Fatal("bob was never listed")
	}
}
