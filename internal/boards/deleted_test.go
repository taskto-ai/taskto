package boards

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/auth"
)

func TestACardWhoseAssigneeWasDeletedSaysSoUntilItIsAssigned(t *testing.T) {
	w := newWorld(t)
	b := w.createBoard(w.alice, w.shared, "Work")
	c := w.createCard(w.alice, w.shared, b, map[string]any{"title": "Ship it", "assignee": map[string]any{"kind": "user", "id": w.bob.id}})
	if _, err := w.e.db.Exec(`UPDATE cards SET assignee_deleted = 1, assignee_id = NULL WHERE id = ?`, c.ID); err != nil {
		t.Fatal(err)
	}
	got := w.card(w.alice, w.shared, c)
	if got.Assignee == nil || got.Assignee.Kind != AssignDeleted || got.Assignee.ID != 0 || got.Assignee.Name != "" {
		t.Fatalf("assignee = %+v, want a deleted one", got.Assignee)
	}
	if got := titles(w.cards(w.alice, w.shared, b, "?assignee=none")); !slices.Equal(got, []string{"Ship it"}) {
		t.Errorf("unassigned = %v", got)
	}
	rec := w.alice.do(http.MethodPatch, cardPath(w.shared, b.ID, c.ID, ""), map[string]any{"version": c.Version, "assignee": map[string]any{"kind": "deleted"}})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("assigning to a deleted user: %d, %s", rec.Code, rec.Body)
	}
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

func TestAnonymizeMentionsRewritesOnlyTheMentionsOfThatPerson(t *testing.T) {
	w := newWorld(t)
	b := w.createBoard(w.alice, w.shared, "Work")
	c := w.createCard(w.alice, w.shared, b, map[string]any{"title": "Ship it"})
	bodies := []string{
		fmt.Sprintf("thanks @[Bob Smith](user:%d) and @[Alice](user:%d)", w.bob.id, w.alice.id),
		fmt.Sprintf("again @[Bob](user:%d), @[Bob](user:%d)", w.bob.id, w.bob.id),
		fmt.Sprintf("in code: `@[Bob](user:%d)` is still the person's label", w.bob.id),
		fmt.Sprintf("another person's id that starts the same: @[Bo](user:%d0)", w.bob.id),
		fmt.Sprintf("padded @[Bob Smith](user:%03d) and @[Bob](user:0%d)", w.bob.id, w.bob.id),
		fmt.Sprintf("| @[Bob\\|Smith](user:%d) | ok |", w.bob.id),
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
		fmt.Sprintf("thanks @[Deleted user](user:0) and @[Alice](user:%d)", w.alice.id),
		"again @[Deleted user](user:0), @[Deleted user](user:0)",
		"in code: `@[Deleted user](user:0)` is still the person's label",
		bodies[3],
		"padded @[Deleted user](user:0) and @[Deleted user](user:0)",
		"| @[Deleted user](user:0) | ok |",
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
	if n != 5 {
		t.Errorf("changed %d comments, want 5", n)
	}
	for _, i := range []int{0, 1, 4} {
		for _, id := range Mentions(want[i]) {
			if id == w.bob.id {
				t.Errorf("comment %d still mentions Bob", i)
			}
			if id == 0 {
				t.Errorf("comment %d mentions the placeholder as a person", i)
			}
		}
	}
}

func TestADeletedAccountsMentionsNeverResolveToALaterAccount(t *testing.T) {
	w := newWorld(t)
	late := w.e.join(w.owner, w.ws, "Late", auth.RoleMember)
	w.e.addMember(w.owner, w.shared, late, access.RoleEditor)
	var newest int64
	if err := w.e.db.QueryRow(`SELECT max(id) FROM users`).Scan(&newest); err != nil || newest != late.id {
		t.Fatalf("fixture: the newest account is %d, %v; want %d", newest, err, late.id)
	}
	rewritten := w.commentTo(w.alice, w.teamCard, fmt.Sprintf("hello @[Late](user:%d) and @[Bob](user:%d)", late.id, w.bob.id))
	untouched := fmt.Sprintf("a stale @[Late](user:%d)", late.id)

	tx, err := w.e.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AnonymizeMentions(context.Background(), tx, late.id, "Deleted user"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DELETE FROM spaces WHERE owner_id = ?`, `DELETE FROM conversations WHERE owner_id = ?`, `DELETE FROM users WHERE id = ?`} {
		if _, err := tx.Exec(q, late.id); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.db.Exec(`INSERT INTO card_comments (card_id, author_id, body) VALUES (?, ?, ?)`, w.teamCard.ID, w.alice.id, untouched); err != nil {
		t.Fatal(err)
	}

	next := w.e.join(w.owner, w.ws, "Newcomer", auth.RoleMember)
	w.e.addMember(w.owner, w.shared, next, access.RoleEditor)
	if next.id <= late.id {
		t.Fatalf("the next account got id %d, which is not above the deleted one's %d", next.id, late.id)
	}
	for _, c := range w.card(w.alice, w.shared, w.teamCard).Comments {
		for _, m := range c.Mentions {
			if m.ID == next.id || m.Name == "Newcomer" || m.Name == "Late" {
				t.Errorf("comment %q mentions %+v after the account was deleted", c.Body, m)
			}
		}
		if strings.Contains(c.Body, "Late") && c.Body != untouched {
			t.Errorf("a comment still carries the deleted person's name: %q", c.Body)
		}
	}
	if got := w.card(w.alice, w.shared, w.teamCard).Comments[0].Body; got != fmt.Sprintf("hello @[Deleted user](user:0) and @[Bob](user:%d)", w.bob.id) {
		t.Errorf("the rewritten comment reads %q", got)
	}
	_ = rewritten
}
