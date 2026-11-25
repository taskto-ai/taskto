package boards

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"
)

// A card whose assignee's account was deleted has no assignee id (the schema sets it null) and
// says it was a deleted account's, until someone assigns it.
func TestACardWhoseAssigneeWasDeletedSaysSoUntilItIsAssigned(t *testing.T) {
	w := newWorld(t)
	b := w.createBoard(w.alice, w.shared, "Work")
	c := w.createCard(w.alice, w.shared, b, map[string]any{"title": "Ship it", "assignee": map[string]any{"kind": "user", "id": w.bob.id}})
	// What the purge does to the card, short of deleting the account.
	if _, err := w.e.db.Exec(`UPDATE cards SET assignee_deleted = 1, assignee_id = NULL WHERE id = ?`, c.ID); err != nil {
		t.Fatal(err)
	}
	got := w.card(w.alice, w.shared, c)
	if got.Assignee == nil || got.Assignee.Kind != AssignDeleted || got.Assignee.ID != 0 || got.Assignee.Name != "" {
		t.Fatalf("assignee = %+v, want a deleted one", got.Assignee)
	}
	// It is listed with the cards nobody is assigned to: it needs someone.
	if got := titles(w.cards(w.alice, w.shared, b, "?assignee=none")); !slices.Equal(got, []string{"Ship it"}) {
		t.Errorf("unassigned = %v", got)
	}
	// A deleted assignee can't be sent: it isn't someone to assign.
	rec := w.alice.do(http.MethodPatch, cardPath(w.shared, b.ID, c.ID, ""), map[string]any{"version": c.Version, "assignee": map[string]any{"kind": "deleted"}})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("assigning to a deleted user: %d, %s", rec.Code, rec.Body)
	}
	// Assigning anyone, or nobody, replaces it.
	for _, assignee := range []any{nil, map[string]any{"kind": "agent"}, map[string]any{"kind": "user", "id": w.alice.id}} {
		if _, err := w.e.db.Exec(`UPDATE cards SET assignee_deleted = 1, assignee_id = NULL, assignee_agent = 0 WHERE id = ?`, c.ID); err != nil {
			t.Fatal(err)
		}
		cur := w.card(w.alice, w.shared, c)
		rec := w.alice.do(http.MethodPatch, cardPath(w.shared, b.ID, c.ID, ""), map[string]any{"version": cur.Version, "assignee": assignee})
		expect(t, rec, http.StatusOK)
		var marked int
		if err := w.e.db.QueryRow(`SELECT count(*) FROM cards WHERE id = ? AND assignee_deleted = 1`, c.ID).Scan(&marked); err != nil {
			t.Fatal(err)
		}
		if marked != 0 {
			t.Errorf("assigning %v left the mark", assignee)
		}
	}
}

func TestAnonymizeMentionsRewritesOnlyTheLabelsOfThatPerson(t *testing.T) {
	w := newWorld(t)
	b := w.createBoard(w.alice, w.shared, "Work")
	c := w.createCard(w.alice, w.shared, b, map[string]any{"title": "Ship it"})
	bodies := []string{
		fmt.Sprintf("thanks @[Bob Smith](user:%d) and @[Alice](user:%d)", w.bob.id, w.alice.id),
		fmt.Sprintf("again @[Bob](user:%d), @[Bob](user:%d)", w.bob.id, w.bob.id),
		fmt.Sprintf("in code: `@[Bob](user:%d)` is still the person's label", w.bob.id),
		fmt.Sprintf("another person's id that starts the same: @[Bo](user:%d0)", w.bob.id),
		"nobody",
	}
	ids := make([]int64, len(bodies))
	for i, body := range bodies {
		res, err := w.e.db.Exec(`INSERT INTO card_comments (card_id, author_id, body) VALUES (?, ?, ?)`, c.ID, w.alice.id, body)
		if err != nil {
			t.Fatal(err)
		}
		ids[i], _ = res.LastInsertId()
	}
	var n int64
	tx, err := w.e.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if n, err = AnonymizeMentions(context.Background(), tx, w.bob.id, "Deleted user"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		fmt.Sprintf("thanks @[Deleted user](user:%d) and @[Alice](user:%d)", w.bob.id, w.alice.id),
		fmt.Sprintf("again @[Deleted user](user:%d), @[Deleted user](user:%d)", w.bob.id, w.bob.id),
		// Code isn't a mention, but it holds a name the writer typed: it is the person's all the same.
		fmt.Sprintf("in code: `@[Deleted user](user:%d)` is still the person's label", w.bob.id),
		bodies[3],
		"nobody",
	}
	for i, id := range ids {
		var got string
		if err := w.e.db.QueryRow(`SELECT body FROM card_comments WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want[i] {
			t.Errorf("comment %d = %q, want %q", i, got, want[i])
		}
	}
	if n != 3 {
		t.Errorf("changed %d comments, want 3", n)
	}
}
