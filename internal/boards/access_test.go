package boards

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
)

type route struct {
	name   string
	method string
	path   func(s access.Space, board, card int64) string
	body   func(c Card) any
	write  bool
}

var routes = []route{
	{"list boards", "GET", func(s access.Space, _, _ int64) string { return spacePath(s, "/boards") }, nil, false},
	{"create board", "POST", func(s access.Space, _, _ int64) string { return spacePath(s, "/boards") },
		func(Card) any { return map[string]string{"name": "x"} }, true},
	{"get board", "GET", func(s access.Space, b, _ int64) string { return boardPath(s, b, "") }, nil, false},
	{"rename board", "PATCH", func(s access.Space, b, _ int64) string { return boardPath(s, b, "") },
		func(Card) any { return map[string]any{"version": 1, "name": "x"} }, true},
	{"list cards", "GET", func(s access.Space, b, _ int64) string { return boardPath(s, b, "/cards") }, nil, false},
	{"create card", "POST", func(s access.Space, b, _ int64) string { return boardPath(s, b, "/cards") },
		func(Card) any { return map[string]string{"title": "x"} }, true},
	{"get card", "GET", func(s access.Space, b, c int64) string { return cardPath(s, b, c, "") }, nil, false},
	{"edit card", "PATCH", func(s access.Space, b, c int64) string { return cardPath(s, b, c, "") },
		func(c Card) any { return map[string]any{"version": c.Version, "title": "x"} }, true},
	{"move card", "POST", func(s access.Space, b, c int64) string { return cardPath(s, b, c, "/move") },
		func(c Card) any { return map[string]any{"version": c.Version, "column": "doing"} }, true},
	{"archive card", "POST", func(s access.Space, b, c int64) string { return cardPath(s, b, c, "/archive") },
		func(c Card) any { return map[string]any{"version": c.Version} }, true},
	{"restore card", "POST", func(s access.Space, b, c int64) string { return cardPath(s, b, c, "/restore") },
		func(c Card) any { return map[string]any{"version": c.Version} }, true},
	{"comment", "POST", func(s access.Space, b, c int64) string { return cardPath(s, b, c, "/comments") },
		func(Card) any { return map[string]string{"body": "x"} }, true},
}

func TestEveryRouteRefusesOutsiders(t *testing.T) {
	w := newWorld(t)
	cases := []struct {
		name        string
		who         *user
		space       access.Space
		board       int64
		card        Card
		read, write int
	}{
		{"viewer", w.bob, w.shared, w.team.ID, w.teamCard, 200, 403},
		{"editor of an archived space", w.alice, w.archived, w.old.ID, w.oldCard, 200, 403},
		{"workspace member not in the shared space", w.mallory, w.shared, w.team.ID, w.teamCard, 404, 404},
		{"admin not in the shared space", w.admin2, w.shared, w.team.ID, w.teamCard, 404, 404},
		{"removed member", w.gone, w.shared, w.team.ID, w.teamCard, 404, 404},
		{"another member's personal board", w.bob, w.alicePrivate, w.alicePersonal.ID, w.aliceCard, 404, 404},
		{"an admin, another member's personal board", w.owner, w.alicePrivate, w.alicePersonal.ID, w.aliceCard, 404, 404},
		{"another workspace's admin", w.stranger, w.shared, w.team.ID, w.teamCard, 404, 404},
		{"through another workspace", w.stranger, in(w.other, w.shared), w.team.ID, w.teamCard, 404, 404},
		{"another workspace's space through this one", w.owner, in(w.ws, w.otherShared), w.team.ID, w.teamCard, 404, 404},
	}
	for _, tc := range cases {
		for _, rt := range routes {
			t.Run(tc.name+"/"+rt.name, func(t *testing.T) {
				want := tc.read
				if rt.write {
					want = tc.write
				}
				var body any
				if rt.body != nil {
					body = rt.body(tc.card)
				}
				expect(t, tc.who.do(rt.method, rt.path(tc.space, tc.board, tc.card.ID), body), want)
			})
		}
	}

	misplaced := []struct {
		name     string
		space    access.Space
		board    int64
		card     Card
		cardOnly bool
	}{
		{"a team board through the editor's private space", w.alicePrivate, w.team.ID, w.teamCard, false},
		{"a personal board through a shared space", w.shared, w.alicePersonal.ID, w.aliceCard, false},
		{"a board of another shared space", w.shared, w.old.ID, w.oldCard, false},
		{"a board that doesn't exist", w.shared, 9999, w.teamCard, false},
		{"a card through another board of its space", w.shared, w.team2.ID, w.teamCard, true},
		{"a card that doesn't exist", w.shared, w.team.ID, Card{ID: 9999, Version: 1}, true},
	}
	for _, tc := range misplaced {
		for _, rt := range routes {
			path := rt.path(tc.space, tc.board, tc.card.ID)
			if !strings.Contains(path, "/boards/") || (tc.cardOnly && !strings.Contains(path, "/cards/")) {
				continue
			}
			t.Run(tc.name+"/"+rt.name, func(t *testing.T) {
				var body any
				if rt.body != nil {
					body = rt.body(tc.card)
				}
				expect(t, w.alice.do(rt.method, path, body), http.StatusNotFound)
			})
		}
	}
	for _, rest := range []string{"/boards/x", "/boards/x/cards", fmt.Sprintf("/boards/%d/cards/y", w.team.ID)} {
		expect(t, w.alice.do(http.MethodGet, spacePath(w.shared, rest), nil), http.StatusNotFound)
	}

	anon := &user{e: w.e}
	for _, rt := range routes {
		var body any
		if rt.body != nil {
			body = rt.body(w.teamCard)
		}
		expect(t, anon.do(rt.method, rt.path(w.shared, w.team.ID, w.teamCard.ID), body), http.StatusUnauthorized)
	}

	for _, c := range []struct {
		u    *user
		s    access.Space
		card Card
	}{{w.alice, w.shared, w.teamCard}, {w.alice, w.archived, w.oldCard}, {w.alice, w.alicePrivate, w.aliceCard}, {w.alice, w.shared, w.team2Card}} {
		got := w.card(c.u, c.s, c.card)
		if got.Version != 1 || got.Title != c.card.Title || got.Column != Backlog || len(got.Comments) != 0 {
			t.Errorf("card %d after the refused calls: %+v", c.card.ID, got)
		}
	}
	if got := w.boards(w.alice, w.shared); len(got) != 2 || got[0].Version != 1 || got[1].Version != 1 {
		t.Errorf("shared space's boards after the refused calls: %+v", got)
	}
	var n int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM boards`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if want := 3 + 8; n != want {
		t.Errorf("%d boards after the refused calls, want %d", n, want)
	}
}

func TestAgentToolsActForTheirUser(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	s := w.e.s
	agentFor := func(u *user) Actor { return Actor{UserID: u.id, Agent: true} }

	c, err := s.CreateCard(ctx, agentFor(w.alice), w.shared.ID, w.team.ID, NewCard{Title: "From the agent", Column: Todo})
	if err != nil {
		t.Fatal(err)
	}
	if c.CreatedBy == nil || c.CreatedBy.ID != w.alice.id || !c.CreatedBy.Agent || c.Column != Todo {
		t.Errorf("created %+v, want alice's agent as its author", c)
	}
	reason := "waiting on the team card"
	if c, err = s.SetBlocked(ctx, agentFor(w.alice), w.shared.ID, w.team.ID, c.ID, c.Version, &reason, []int64{w.teamCard.ID}); err != nil {
		t.Fatal(err)
	}
	if !c.Blocked || *c.BlockedReason != reason || len(c.BlockedBy) != 1 {
		t.Errorf("after SetBlocked: %+v", c)
	}
	if c, err = s.SetBlocked(ctx, agentFor(w.alice), w.shared.ID, w.team.ID, c.ID, c.Version, nil, nil); err != nil || c.Blocked {
		t.Errorf("after unblocking: %+v, %v", c, err)
	}
	if c, err = s.MoveCard(ctx, agentFor(w.alice), w.shared.ID, w.team.ID, c.ID, Move{Version: c.Version, Column: Doing}); err != nil || c.Column != Doing {
		t.Errorf("MoveCard: %+v, %v", c, err)
	}
	title := "Renamed by the agent"
	if c, err = s.UpdateCard(ctx, agentFor(w.alice), w.shared.ID, w.team.ID, c.ID, CardChanges{Version: c.Version, Title: &title}); err != nil || c.Title != title {
		t.Errorf("UpdateCard: %+v, %v", c, err)
	}
	if _, err := s.MoveCard(ctx, agentFor(w.alice), w.shared.ID, w.team.ID, c.ID, Move{Version: 1, Column: Done}); !errors.Is(err, ErrStale) {
		t.Errorf("MoveCard with an old version: %v, want ErrStale", err)
	}
	cm, err := s.Comment(ctx, agentFor(w.alice), w.shared.ID, w.team.ID, c.ID, "Started on it.")
	if err != nil || cm.Author == nil || !cm.Author.Agent || cm.Author.ID != w.alice.id {
		t.Errorf("Comment: %+v, %v", cm, err)
	}
	th, err := s.ShowCard(ctx, agentFor(w.bob), w.shared.ID, w.team.ID, c.ID)
	if err != nil || len(th.Comments) != 1 || th.UpdatedBy == nil || !th.UpdatedBy.Agent {
		t.Errorf("ShowCard for a viewer: %+v, %v", th, err)
	}
	if boards, err := s.ListBoards(ctx, agentFor(w.alice), w.alicePrivate.ID); err != nil || len(boards) != 1 || boards[0].Kind != Personal {
		t.Errorf("ListBoards of alice's private space: %+v, %v", boards, err)
	}
	if cards, err := s.ListCards(ctx, agentFor(w.bob), w.shared.ID, w.team.ID, Filter{Assignee: Unassigned}); err != nil || len(cards) != 2 {
		t.Errorf("ListCards for a viewer: %d cards, %v", len(cards), err)
	}

	refused := []struct {
		name string
		call func() error
		want error
	}{
		{"a viewer's agent creates", func() error {
			_, err := s.CreateCard(ctx, agentFor(w.bob), w.shared.ID, w.team.ID, NewCard{Title: "x"})
			return err
		}, ErrReadOnly},
		{"a viewer's agent comments", func() error {
			_, err := s.Comment(ctx, agentFor(w.bob), w.shared.ID, w.team.ID, c.ID, "x")
			return err
		}, ErrReadOnly},
		{"an editor's agent, archived space", func() error {
			_, err := s.MoveCard(ctx, agentFor(w.alice), w.archived.ID, w.old.ID, w.oldCard.ID, Move{Version: 1, Column: Done})
			return err
		}, ErrReadOnly},
		{"an outsider's agent lists", func() error {
			_, err := s.ListCards(ctx, agentFor(w.mallory), w.shared.ID, w.team.ID, Filter{})
			return err
		}, ErrNotFound},
		{"an admin's agent, another's personal board", func() error {
			_, err := s.ShowCard(ctx, agentFor(w.owner), w.alicePrivate.ID, w.alicePersonal.ID, w.aliceCard.ID)
			return err
		}, ErrNotFound},
		{"a removed member's agent", func() error {
			_, err := s.ListBoards(ctx, agentFor(w.gone), w.shared.ID)
			return err
		}, ErrNotFound},
		{"another workspace's agent", func() error {
			_, err := s.UpdateCard(ctx, agentFor(w.stranger), w.shared.ID, w.team.ID, w.teamCard.ID, CardChanges{Version: 1, Title: &title})
			return err
		}, ErrNotFound},
		{"a board of another space, through one the user may write", func() error {
			_, err := s.CreateCard(ctx, agentFor(w.alice), w.alicePrivate.ID, w.team.ID, NewCard{Title: "x"})
			return err
		}, ErrNotFound},
		{"a card through another board", func() error {
			_, err := s.ShowCard(ctx, agentFor(w.alice), w.shared.ID, w.team2.ID, w.teamCard.ID)
			return err
		}, ErrNotFound},
	}
	for _, tc := range refused {
		if err := tc.call(); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
}
