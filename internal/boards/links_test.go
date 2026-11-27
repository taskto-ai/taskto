package boards

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestConversationLinks(t *testing.T) {
	w := newWorld(t)
	alices := w.conversation(w.alice, w.ws)
	owners := w.conversation(w.owner, w.ws)
	if _, err := w.e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, w.other, w.alice.id); err != nil {
		t.Fatal(err)
	}
	alicesElsewhere := w.conversation(w.alice, w.other)
	missing := "999999"

	create := func(u *user, convs ...string) *httptest.ResponseRecorder {
		return u.do(http.MethodPost, boardPath(w.shared, w.team.ID, "/cards"), map[string]any{"title": "x", "conversations": convs})
	}
	if r := create(w.alice, alices); r.Code != http.StatusCreated {
		t.Fatalf("alice linking her own conversation: %d %s", r.Code, r.Body.String())
	}
	var refusals []string
	for _, conv := range []string{owners, alicesElsewhere, missing} {
		r := create(w.alice, alices, conv)
		if r.Code != http.StatusBadRequest {
			t.Fatalf("alice linking %s: %d %s", conv, r.Code, r.Body.String())
		}
		refusals = append(refusals, strings.ReplaceAll(r.Body.String(), conv, "N"))
	}
	if refusals[0] != refusals[1] || refusals[1] != refusals[2] {
		t.Fatalf("refusals differ: %q", refusals)
	}
	for _, bad := range []string{"0" + alices, "+" + alices, "-1", "0", "1e3", " " + alices, "conv-1", ""} {
		if r := create(w.alice, bad); r.Code != http.StatusBadRequest {
			t.Fatalf("linking %q: %d %s", bad, r.Code, r.Body.String())
		}
	}

	c := w.createCard(w.alice, w.shared, w.team, map[string]any{"title": "Linked", "conversations": []string{alices}})
	rec := w.owner.do(http.MethodPatch, cardPath(w.shared, w.team.ID, c.ID, ""), map[string]any{
		"version": c.Version, "title": "Linked twice", "conversations": []string{alices, owners},
	})
	expect(t, rec, http.StatusOK)
	c = decode[Card](t, rec)
	if !slices.Equal(c.Conversations, []string{alices, owners}) {
		t.Fatalf("links = %v", c.Conversations)
	}
	if r := create(w.owner, alices); r.Code != http.StatusBadRequest {
		t.Fatalf("the owner linking alice's conversation: %d %s", r.Code, r.Body.String())
	}
	rec = w.owner.do(http.MethodPatch, cardPath(w.shared, w.team.ID, c.ID, ""), map[string]any{
		"version": c.Version, "conversations": []string{owners},
	})
	expect(t, rec, http.StatusOK)
	c = decode[Card](t, rec)
	rec = w.owner.do(http.MethodPatch, cardPath(w.shared, w.team.ID, c.ID, ""), map[string]any{
		"version": c.Version, "conversations": []string{owners, alices},
	})
	expect(t, rec, http.StatusBadRequest)

	ctx := context.Background()
	if _, err := w.e.s.CreateCard(ctx, Actor{UserID: w.alice.id, Agent: true}, w.shared.ID, w.team.ID,
		NewCard{Title: "By the agent", Conversations: []string{alices}}); err != nil {
		t.Fatalf("the agent for alice: %v", err)
	}
	if _, err := w.e.s.CreateCard(ctx, Actor{UserID: w.owner.id, Agent: true}, w.shared.ID, w.team.ID,
		NewCard{Title: "By the agent", Conversations: []string{alices}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("the agent for the owner, linking alice's conversation: %v", err)
	}
}
