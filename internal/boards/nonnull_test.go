package boards

import (
	"net/http"
	"testing"

	"taskto.ai/taskto/internal/apitypes/nonnull"
)

// A card's lists are told to the web app as lists, never null (apitypes:"nonnull"): so a card
// with nothing in them, made or changed any way, still sends [] for each.
func TestCardListsAreNeverNil(t *testing.T) {
	w := newWorld(t)
	c := w.createCard(w.alice, w.shared, w.team, map[string]any{"title": "Bare"})
	nonnull.Require(t, c)

	nonnull.Require(t, w.card(w.alice, w.shared, c))
	for _, got := range w.cards(w.alice, w.shared, w.team, "") {
		nonnull.Require(t, got)
	}

	// Edits that empty the lists, and the other changes a card goes through.
	path := cardPath(w.shared, c.BoardID, c.ID, "")
	rec := w.alice.do(http.MethodPatch, path, map[string]any{
		"version": c.Version, "blocked_by": []int64{}, "docs": []string{}, "conversations": []string{}, "labels": []string{},
	})
	expect(t, rec, http.StatusOK)
	c = decode[Card](t, rec)
	nonnull.Require(t, c)

	rec = w.alice.do(http.MethodPost, path+"/move", map[string]any{"version": c.Version, "column": "doing"})
	expect(t, rec, http.StatusOK)
	c = decode[Card](t, rec)
	nonnull.Require(t, c)
	rec = w.alice.do(http.MethodPost, path+"/archive", map[string]any{"version": c.Version})
	expect(t, rec, http.StatusOK)
	c = decode[Card](t, rec)
	nonnull.Require(t, c)
	rec = w.alice.do(http.MethodPost, path+"/restore", map[string]any{"version": c.Version, "column": "todo"})
	expect(t, rec, http.StatusOK)
	nonnull.Require(t, decode[Card](t, rec))

	// A card with a comment, and the thread the tools give.
	rec = w.alice.do(http.MethodPost, path+"/comments", map[string]string{"body": "hello"})
	expect(t, rec, http.StatusCreated)
	nonnull.Require(t, decode[Comment](t, rec))
	thread := w.card(w.alice, w.shared, c)
	if len(thread.Comments) != 1 {
		t.Fatalf("comments: %+v", thread.Comments)
	}
	nonnull.Require(t, thread)
}
