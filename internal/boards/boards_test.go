package boards

import (
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/auth"
)

func TestEveryMemberHasAPersonalBoard(t *testing.T) {
	w := newWorld(t)
	for _, u := range []*user{w.owner, w.alice, w.bob} {
		s := w.e.private(u, w.ws)
		got := w.boards(u, s)
		if len(got) != 1 || got[0].Kind != Personal || got[0].Name != "Personal" || got[0].SpaceID != s.ID ||
			got[0].CreatedBy == nil || got[0].CreatedBy.ID != u.id {
			t.Errorf("user %d's private space has boards %+v, want one personal board", u.id, got)
		}
	}
	// A newcomer gets one when they join.
	carol := w.e.join(w.owner, w.ws, "Carol", auth.RoleMember)
	if got := w.boards(carol, w.e.private(carol, w.ws)); len(got) != 1 || got[0].Kind != Personal {
		t.Errorf("a new member's boards: %+v", got)
	}

	// A private space has only that board, which can be renamed but not archived.
	p := w.alicePersonal
	rec := w.alice.do(http.MethodPost, spacePath(w.alicePrivate, "/boards"), map[string]string{"name": "Second"})
	expect(t, rec, http.StatusBadRequest)
	expect(t, w.alice.do(http.MethodPatch, boardPath(w.alicePrivate, p.ID, ""), map[string]any{"version": p.Version, "archived": true}),
		http.StatusBadRequest)
	rec = w.alice.do(http.MethodPatch, boardPath(w.alicePrivate, p.ID, ""), map[string]any{"version": p.Version, "name": " Mine "})
	expect(t, rec, http.StatusOK)
	if got := decode[Board](t, rec); got.Name != "Mine" || got.Version != p.Version+1 {
		t.Errorf("renamed personal board: %+v", got)
	}

	// The schema holds the rules too.
	extra := w.createCard(w.alice, w.shared, w.team, map[string]any{"title": "x"})
	refused := []struct {
		name  string
		query string
		args  []any
	}{
		{"a second personal board", `INSERT INTO boards (space_id, kind, name) VALUES (?, 'personal', 'x')`, []any{w.alicePrivate.ID}},
		{"a team board in a private space", `INSERT INTO boards (space_id, kind, name) VALUES (?, 'team', 'x')`, []any{w.alicePrivate.ID}},
		{"a personal board in a shared space", `INSERT INTO boards (space_id, kind, name) VALUES (?, 'personal', 'x')`, []any{w.shared.ID}},
		{"moving a board to another space", `UPDATE boards SET space_id = ? WHERE id = ?`, []any{w.shared.ID, w.team2.ID}},
		{"archiving a personal board", `UPDATE boards SET archived_at = 'x' WHERE id = ?`, []any{p.ID}},
		{"moving a card to another board", `UPDATE cards SET board_id = ? WHERE id = ?`, []any{w.team2.ID, w.teamCard.ID}},
		{"a blocker on another board", `INSERT INTO card_blockers (card_id, blocker_id) VALUES (?, ?)`, []any{w.teamCard.ID, w.team2Card.ID}},
		{"assigned to a person and the agent", `UPDATE cards SET assignee_id = ?, assignee_agent = 1 WHERE id = ?`, []any{w.alice.id, w.teamCard.ID}},
		{"two cards at the same place", `UPDATE cards SET rank = (SELECT rank FROM cards WHERE id = ?) WHERE id = ?`, []any{w.teamCard.ID, extra.ID}},
	}
	for _, tc := range refused {
		if _, err := w.e.db.Exec(tc.query, tc.args...); err == nil {
			t.Errorf("%s: allowed", tc.name)
		}
	}
}

func TestTeamBoards(t *testing.T) {
	w := newWorld(t)
	b := w.createBoard(w.alice, w.shared, "  Roadmap ")
	if b.Name != "Roadmap" || b.Kind != Team || b.Version != 1 || b.SpaceID != w.shared.ID || b.CreatedBy.ID != w.alice.id {
		t.Errorf("created %+v", b)
	}
	expect(t, w.alice.do(http.MethodPost, spacePath(w.shared, "/boards"), map[string]string{"name": " "}), http.StatusBadRequest)
	expect(t, w.alice.do(http.MethodPost, spacePath(w.shared, "/boards"), map[string]string{"name": strings.Repeat("x", 101)}), http.StatusBadRequest)
	if got := titlesOf(w.boards(w.bob, w.shared)); !slices.Equal(got, []string{"Launch", "Ops", "Roadmap"}) {
		t.Errorf("boards = %v", got)
	}
	rec := w.bob.do(http.MethodGet, boardPath(w.shared, b.ID, ""), nil)
	expect(t, rec, http.StatusOK)

	// Rename: needs the version, and refuses an old one.
	path := boardPath(w.shared, b.ID, "")
	expect(t, w.alice.do(http.MethodPatch, path, map[string]any{"name": "x"}), http.StatusBadRequest)
	expect(t, w.alice.do(http.MethodPatch, path, map[string]any{"version": 1}), http.StatusBadRequest)
	rec = w.alice.do(http.MethodPatch, path, map[string]any{"version": 1, "name": "Plan"})
	expect(t, rec, http.StatusOK)
	b = decode[Board](t, rec)
	rec = w.owner.do(http.MethodPatch, path, map[string]any{"version": 1, "name": "Other"})
	expect(t, rec, http.StatusConflict)
	if reply := decode[struct {
		Code    string
		Current Board
	}](t, rec); reply.Code != "board_changed" || reply.Current.Name != "Plan" || reply.Current.Version != 2 {
		t.Errorf("409 carries %+v, want board_changed and the current board", reply)
	}

	// Archive: it leaves the list, stays readable, and refuses changes to its cards.
	c := w.createCard(w.alice, w.shared, b, map[string]any{"title": "Card"})
	rec = w.alice.do(http.MethodPatch, path, map[string]any{"version": b.Version, "archived": true})
	expect(t, rec, http.StatusOK)
	b = decode[Board](t, rec)
	if b.ArchivedAt == nil || b.UpdatedBy.ID != w.alice.id {
		t.Errorf("archived board: %+v", b)
	}
	if got := titlesOf(w.boards(w.alice, w.shared)); !slices.Equal(got, []string{"Launch", "Ops"}) {
		t.Errorf("boards after archiving = %v", got)
	}
	rec = w.alice.do(http.MethodGet, spacePath(w.shared, "/boards?archived=true"), nil)
	expect(t, rec, http.StatusOK)
	if got := titlesOf(decode[struct{ Boards []Board }](t, rec).Boards); !slices.Equal(got, []string{"Launch", "Ops", "Plan"}) {
		t.Errorf("boards with archived = %v", got)
	}
	expect(t, w.alice.do(http.MethodGet, spacePath(w.shared, "/boards?archived=yes"), nil), http.StatusBadRequest)
	w.card(w.bob, w.shared, c)
	rec = w.alice.do(http.MethodPost, boardPath(w.shared, b.ID, "/cards"), map[string]string{"title": "x"})
	expect(t, rec, http.StatusForbidden)
	if code := decode[struct{ Code string }](t, rec).Code; code != "board_archived" {
		t.Errorf("a card on an archived board: code %q", code)
	}
	expect(t, w.alice.do(http.MethodPatch, cardPath(w.shared, b.ID, c.ID, ""), map[string]any{"version": 1, "title": "x"}), http.StatusForbidden)
	expect(t, w.alice.do(http.MethodPost, cardPath(w.shared, b.ID, c.ID, "/comments"), map[string]any{"body": "x"}), http.StatusForbidden)
	// The board itself is read-only too: no rename while archived, but renaming while
	// unarchiving is fine.
	expect(t, w.alice.do(http.MethodPatch, path, map[string]any{"version": b.Version, "name": "Renamed"}), http.StatusForbidden)
	rec = w.alice.do(http.MethodPatch, path, map[string]any{"version": b.Version, "archived": false, "name": "Plan again"})
	expect(t, rec, http.StatusOK)
	if got := decode[Board](t, rec); got.ArchivedAt != nil || got.Name != "Plan again" {
		t.Errorf("unarchived and renamed: %+v", got)
	}
	// Unarchived, it takes changes again.
	expect(t, w.alice.do(http.MethodPost, boardPath(w.shared, b.ID, "/cards"), map[string]string{"title": "x"}), http.StatusCreated)
}

func titlesOf(boards []Board) []string {
	out := []string{}
	for _, b := range boards {
		out = append(out, b.Name)
	}
	return out
}

func TestCardLifecycle(t *testing.T) {
	w := newWorld(t)
	conv := w.conversation(w.alice, w.ws)
	reason := "waiting for the vendor"
	rec := w.alice.do(http.MethodPost, boardPath(w.shared, w.team.ID, "/cards"), map[string]any{
		"title":          "  Write the launch post ",
		"body":           "## Plan\n\n- draft\n- review",
		"column":         "todo",
		"assignee":       map[string]any{"kind": "user", "id": w.bob.id},
		"blocked_reason": reason,
		"blocked_by":     []int64{w.teamCard.ID, w.teamCard.ID},
		"docs":           []string{"launch/post.md", "launch/post.md"},
		"conversations":  []string{conv, conv},
	})
	expect(t, rec, http.StatusCreated)
	c := decode[Card](t, rec)
	if c.Title != "Write the launch post" || c.Column != Todo || c.Version != 1 || c.BoardID != w.team.ID ||
		c.Assignee == nil || c.Assignee.Kind != AssignUser || c.Assignee.ID != w.bob.id || c.Assignee.Name != "Bob" ||
		!c.Blocked || *c.BlockedReason != reason || !slices.Equal(c.BlockedBy, []int64{w.teamCard.ID}) ||
		!slices.Equal(c.Docs, []string{"launch/post.md"}) || !slices.Equal(c.Conversations, []string{conv}) ||
		c.CreatedBy == nil || c.CreatedBy.ID != w.alice.id || c.CreatedBy.Agent || c.CreatedAt == "" || c.UpdatedAt == "" {
		t.Fatalf("created %+v", c)
	}

	// Edit: only what is sent changes; null clears.
	path := cardPath(w.shared, w.team.ID, c.ID, "")
	rec = w.owner.do(http.MethodPatch, path, map[string]any{
		"version": 1, "body": "Done drafting.", "assignee": map[string]string{"kind": "agent"},
		"blocked_reason": nil, "blocked_by": []int64{}, "docs": []string{},
	})
	expect(t, rec, http.StatusOK)
	c = decode[Card](t, rec)
	if c.Title != "Write the launch post" || c.Body != "Done drafting." || c.Assignee.Kind != AssignAgent ||
		c.Blocked || c.BlockedReason != nil || len(c.BlockedBy) != 0 || len(c.Docs) != 0 ||
		!slices.Equal(c.Conversations, []string{conv}) || c.Version != 2 ||
		c.UpdatedBy.ID != w.owner.id || c.CreatedBy.ID != w.alice.id {
		t.Errorf("edited %+v", c)
	}
	expect(t, w.owner.do(http.MethodPatch, path, map[string]any{"version": 2}), http.StatusBadRequest)
	expect(t, w.owner.do(http.MethodPatch, path, map[string]any{"title": "x"}), http.StatusBadRequest)
	rec = w.owner.do(http.MethodPatch, path, map[string]any{"version": 2, "assignee": nil})
	expect(t, rec, http.StatusOK)
	c = decode[Card](t, rec)
	if c.Assignee != nil {
		t.Errorf("unassigned card has assignee %+v", c.Assignee)
	}

	// Comments: the thread, oldest first; they don't change the card's version.
	for _, who := range []*user{w.alice, w.owner} {
		rec = who.do(http.MethodPost, cardPath(w.shared, w.team.ID, c.ID, "/comments"), map[string]string{"body": "from " + fmt.Sprint(who.id)})
		expect(t, rec, http.StatusCreated)
		if cm := decode[Comment](t, rec); cm.Author.ID != who.id || cm.CardID != c.ID || cm.CreatedAt == "" {
			t.Errorf("comment %+v", cm)
		}
	}
	expect(t, w.alice.do(http.MethodPost, cardPath(w.shared, w.team.ID, c.ID, "/comments"), map[string]string{"body": "  "}), http.StatusBadRequest)
	th := w.card(w.bob, w.shared, c)
	if th.Version != c.Version || len(th.Comments) != 2 || th.Comments[0].Author.Name != "Alice" || th.Comments[1].Author.Name != "Owner" {
		t.Errorf("thread %+v", th)
	}

	// Archive and restore: the card leaves the board and comes back to its column.
	rec = w.alice.do(http.MethodPost, cardPath(w.shared, w.team.ID, c.ID, "/archive"), map[string]any{"version": c.Version})
	expect(t, rec, http.StatusOK)
	c = decode[Card](t, rec)
	if c.Column != Archived || c.ArchivedFrom == nil || *c.ArchivedFrom != Todo {
		t.Errorf("archived %+v", c)
	}
	if got := titles(w.cards(w.alice, w.shared, w.team, "")); slices.Contains(got, c.Title) {
		t.Errorf("the archived card is still listed: %v", got)
	}
	if got := titles(w.cards(w.alice, w.shared, w.team, "?column=archived")); !slices.Equal(got, []string{c.Title}) {
		t.Errorf("archived cards = %v", got)
	}
	expect(t, w.alice.do(http.MethodPost, cardPath(w.shared, w.team.ID, c.ID, "/archive"), map[string]any{"version": c.Version}), http.StatusBadRequest)
	rec = w.alice.do(http.MethodPost, cardPath(w.shared, w.team.ID, c.ID, "/restore"), map[string]any{"version": c.Version})
	expect(t, rec, http.StatusOK)
	c = decode[Card](t, rec)
	if c.Column != Todo || c.ArchivedFrom != nil {
		t.Errorf("restored %+v", c)
	}
	expect(t, w.alice.do(http.MethodPost, cardPath(w.shared, w.team.ID, c.ID, "/restore"), map[string]any{"version": c.Version}), http.StatusBadRequest)
	// Restore to another column.
	c = decode[Card](t, w.alice.do(http.MethodPost, cardPath(w.shared, w.team.ID, c.ID, "/archive"), map[string]any{"version": c.Version}))
	rec = w.alice.do(http.MethodPost, cardPath(w.shared, w.team.ID, c.ID, "/restore"), map[string]any{"version": c.Version, "column": "done"})
	expect(t, rec, http.StatusOK)
	if got := decode[Card](t, rec); got.Column != Done {
		t.Errorf("restored to done: %+v", got)
	}

	// Validation.
	bad := []map[string]any{
		{},
		{"title": " "},
		{"title": "two\nlines"},
		{"title": strings.Repeat("x", 201)},
		{"title": "x", "body": strings.Repeat("x", 33<<10)},
		{"title": "x", "column": "archived"},
		{"title": "x", "column": "later"},
		{"title": "x", "position": -1},
		{"title": "x", "blocked_reason": " "},
		{"title": "x", "docs": []string{"../secrets"}},
		{"title": "x", "docs": []string{"/etc/passwd"}},
		{"title": "x", "docs": []string{".git/config"}},
		{"title": "x", "conversations": []string{"has space"}},
		{"title": "x", "conversations": []string{""}},
		{"title": "x", "assignee": map[string]any{"kind": "robot"}},
		{"title": "x", "unknown": true},
	}
	for _, body := range bad {
		expect(t, w.alice.do(http.MethodPost, boardPath(w.shared, w.team.ID, "/cards"), body), http.StatusBadRequest)
	}
}

func TestMovesAndOrder(t *testing.T) {
	w := newWorld(t)
	b := w.createBoard(w.alice, w.shared, "Order")
	add := func(title string, extra map[string]any) Card {
		body := map[string]any{"title": title}
		for k, v := range extra {
			body[k] = v
		}
		return w.createCard(w.alice, w.shared, b, body)
	}
	a := add("a", nil)
	add("b", nil)
	add("c", nil)
	add("first", map[string]any{"position": 0})
	add("second", map[string]any{"position": 1})
	add("last", map[string]any{"position": 99})
	add("t1", map[string]any{"column": "todo"})
	column := func(col Column) []string {
		return titles(w.cards(w.bob, w.shared, b, "?column="+string(col)))
	}
	if got := column(Backlog); !slices.Equal(got, []string{"first", "second", "a", "b", "c", "last"}) {
		t.Fatalf("backlog = %v", got)
	}

	move := func(c Card, col Column, position any) Card {
		t.Helper()
		body := map[string]any{"version": c.Version, "column": col}
		if position != nil {
			body["position"] = position
		}
		rec := w.alice.do(http.MethodPost, cardPath(w.shared, b.ID, c.ID, "/move"), body)
		expect(t, rec, http.StatusOK)
		return decode[Card](t, rec)
	}
	// Within a column; the position doesn't count the card itself.
	a = move(a, Backlog, 0)
	if got := column(Backlog); !slices.Equal(got, []string{"a", "first", "second", "b", "c", "last"}) {
		t.Errorf("after moving a to the top: %v", got)
	}
	a = move(a, Backlog, 3)
	if got := column(Backlog); !slices.Equal(got, []string{"first", "second", "b", "a", "c", "last"}) {
		t.Errorf("after moving a to 3: %v", got)
	}
	// To another column, at a place and at the end.
	a = move(a, Todo, 0)
	if got := column(Todo); !slices.Equal(got, []string{"a", "t1"}) {
		t.Errorf("todo = %v", got)
	}
	if a.Version != 4 || a.Column != Todo {
		t.Errorf("moved card %+v", a)
	}
	a = move(a, Doing, nil)
	// A move writes one row: the others keep their ranks.
	var n int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM cards WHERE board_id = ? AND version > 1`, b.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d cards changed by moving one", n)
	}

	// The whole board, in board order.
	if got := titles(w.cards(w.bob, w.shared, b, "")); !slices.Equal(got, []string{"first", "second", "b", "c", "last", "t1", "a"}) {
		t.Errorf("board = %v", got)
	}

	// Many moves to the same spot keep a stable, total order.
	cards := w.cards(w.alice, w.shared, b, "?column=backlog")
	for i := range 30 {
		c := cards[i%len(cards)]
		cur := w.card(w.alice, w.shared, c)
		move(cur.Card, Backlog, 1)
	}
	got := w.cards(w.alice, w.shared, b, "?column=backlog")
	for i := 1; i < len(got); i++ {
		if got[i-1].Rank >= got[i].Rank {
			t.Fatalf("ranks out of order: %q then %q", got[i-1].Rank, got[i].Rank)
		}
	}

	for _, body := range []map[string]any{
		{"version": a.Version, "column": "archived"},
		{"version": a.Version, "column": "nowhere"},
		{"version": a.Version, "column": "todo", "position": -1},
		{"column": "todo"},
	} {
		expect(t, w.alice.do(http.MethodPost, cardPath(w.shared, b.ID, a.ID, "/move"), body), http.StatusBadRequest)
	}
}

func TestBlocked(t *testing.T) {
	w := newWorld(t)
	other := w.createCard(w.alice, w.shared, w.team, map[string]any{"title": "Other"})
	path := cardPath(w.shared, w.team.ID, w.teamCard.ID, "")
	c := w.teamCard

	// Blocked by cards only, with no reason, is blocked.
	rec := w.alice.do(http.MethodPatch, path, map[string]any{"version": c.Version, "blocked_by": []int64{other.ID}})
	expect(t, rec, http.StatusOK)
	c = decode[Card](t, rec)
	if !c.Blocked || c.BlockedReason != nil || !slices.Equal(c.BlockedBy, []int64{other.ID}) {
		t.Errorf("blocked by a card: %+v", c)
	}
	// A reason alone too.
	rec = w.alice.do(http.MethodPatch, path, map[string]any{"version": c.Version, "blocked_by": []int64{}, "blocked_reason": "legal review"})
	expect(t, rec, http.StatusOK)
	c = decode[Card](t, rec)
	if !c.Blocked || *c.BlockedReason != "legal review" || len(c.BlockedBy) != 0 {
		t.Errorf("blocked with a reason: %+v", c)
	}
	// Changing the reason keeps the blockers, and the other way round.
	rec = w.alice.do(http.MethodPatch, path, map[string]any{"version": c.Version, "blocked_by": []int64{other.ID}})
	expect(t, rec, http.StatusOK)
	c = decode[Card](t, rec)
	if *c.BlockedReason != "legal review" || len(c.BlockedBy) != 1 {
		t.Errorf("blockers set, reason kept: %+v", c)
	}
	rec = w.alice.do(http.MethodPatch, path, map[string]any{"version": c.Version, "blocked_reason": nil, "blocked_by": []int64{}})
	expect(t, rec, http.StatusOK)
	if c = decode[Card](t, rec); c.Blocked {
		t.Errorf("unblocked: %+v", c)
	}

	// Blockers are other cards of the same board; a refused change changes nothing.
	for _, ids := range [][]int64{{c.ID}, {w.team2Card.ID}, {w.oldCard.ID}, {w.aliceCard.ID}, {9999}, {other.ID, w.team2Card.ID}} {
		expect(t, w.alice.do(http.MethodPatch, path, map[string]any{"version": c.Version, "blocked_by": ids}), http.StatusBadRequest)
	}
	if got := w.card(w.alice, w.shared, c); got.Version != c.Version || len(got.BlockedBy) != 0 {
		t.Errorf("after refused blockers: %+v", got)
	}
}

func TestFilters(t *testing.T) {
	w := newWorld(t)
	b := w.createBoard(w.alice, w.shared, "Filters")
	add := func(body map[string]any) Card { return w.createCard(w.alice, w.shared, b, body) }
	first := add(map[string]any{"title": "Fix the LOGIN page", "assignee": map[string]any{"kind": "user", "id": w.alice.id}})
	add(map[string]any{"title": "Agent work", "body": "the login flow", "column": "doing", "assignee": map[string]any{"kind": "agent"}})
	add(map[string]any{"title": "Bob's card", "column": "doing", "assignee": map[string]any{"kind": "user", "id": w.bob.id}, "blocked_reason": "waiting"})
	add(map[string]any{"title": "100% done_ish", "column": "done", "blocked_by": []int64{first.ID}})
	add(map[string]any{"title": "Nobody's", "column": "review"})

	cases := []struct {
		query string
		who   *user
		want  []string
	}{
		{"", w.alice, []string{"Fix the LOGIN page", "Agent work", "Bob's card", "Nobody's", "100% done_ish"}},
		{"?column=doing", w.alice, []string{"Agent work", "Bob's card"}},
		{"?assignee=me", w.alice, []string{"Fix the LOGIN page"}},
		{"?assignee=me", w.bob, []string{"Bob's card"}},
		{fmt.Sprintf("?assignee=%d", w.bob.id), w.alice, []string{"Bob's card"}},
		{"?assignee=agent", w.alice, []string{"Agent work"}},
		{"?assignee=none", w.alice, []string{"Nobody's", "100% done_ish"}},
		{"?blocked=true", w.alice, []string{"Bob's card", "100% done_ish"}},
		{"?blocked=false", w.alice, []string{"Fix the LOGIN page", "Agent work", "Nobody's"}},
		{"?q=login", w.alice, []string{"Fix the LOGIN page", "Agent work"}},
		{"?q=100%25", w.alice, []string{"100% done_ish"}},
		{"?q=0%25+d", w.alice, []string{"100% done_ish"}},
		{"?q=e_i", w.alice, []string{"100% done_ish"}},
		{"?q=%25", w.alice, []string{"100% done_ish"}},
		{"?q=nothing+like+this", w.alice, []string{}},
		{"?column=doing&assignee=agent&q=flow&blocked=false", w.alice, []string{"Agent work"}},
		{"?column=archived", w.alice, []string{}},
	}
	for _, tc := range cases {
		if got := titles(w.cards(tc.who, w.shared, b, tc.query)); !slices.Equal(got, tc.want) {
			t.Errorf("%s as %d = %v, want %v", tc.query, tc.who.id, got, tc.want)
		}
	}
	for _, q := range []string{"?column=later", "?assignee=someone", "?blocked=yes", "?q=" + strings.Repeat("x", 201)} {
		expect(t, w.alice.do(http.MethodGet, boardPath(w.shared, b.ID, "/cards"+q), nil), http.StatusBadRequest)
	}
}

func TestAssignees(t *testing.T) {
	w := newWorld(t)
	assign := func(u *user, s access.Space, b Board, c Card, assignee any) int {
		t.Helper()
		cur := w.card(u, s, c)
		return u.do(http.MethodPatch, cardPath(s, b.ID, c.ID, ""), map[string]any{"version": cur.Version, "assignee": assignee}).Code
	}
	user := func(u *user) map[string]any { return map[string]any{"kind": "user", "id": u.id} }
	cases := []struct {
		name  string
		space access.Space
		board Board
		card  Card
		to    any
		want  int
	}{
		{"an editor of the space", w.shared, w.team, w.teamCard, user(w.alice), 200},
		{"a viewer of the space", w.shared, w.team, w.teamCard, user(w.bob), 200},
		{"the agent", w.shared, w.team, w.teamCard, map[string]string{"kind": "agent"}, 200},
		{"a workspace member who can't see the space", w.shared, w.team, w.teamCard, user(w.mallory), 400},
		{"an admin who can't see the space", w.shared, w.team, w.teamCard, user(w.admin2), 400},
		{"a removed member", w.shared, w.team, w.teamCard, user(w.gone), 400},
		{"another workspace's member", w.shared, w.team, w.teamCard, user(w.stranger), 400},
		{"a user who doesn't exist", w.shared, w.team, w.teamCard, map[string]any{"kind": "user", "id": 9999}, 400},
		{"no user ID", w.shared, w.team, w.teamCard, map[string]any{"kind": "user"}, 400},
		{"yourself, on your personal board", w.alicePrivate, w.alicePersonal, w.aliceCard, user(w.alice), 200},
		{"someone else, on your personal board", w.alicePrivate, w.alicePersonal, w.aliceCard, user(w.bob), 400},
		{"an admin, on your personal board", w.alicePrivate, w.alicePersonal, w.aliceCard, user(w.owner), 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := assign(w.alice, tc.space, tc.board, tc.card, tc.to); got != tc.want {
				t.Errorf("status = %d, want %d", got, tc.want)
			}
		})
	}
	// On create too.
	expect(t, w.alice.do(http.MethodPost, boardPath(w.shared, w.team.ID, "/cards"),
		map[string]any{"title": "x", "assignee": user(w.mallory)}), http.StatusBadRequest)
}

// TestConcurrentEdits checks that of several updates made against the same version,
// one wins and the others get 409 with the current card.
func TestConcurrentEdits(t *testing.T) {
	w := newWorld(t)
	path := cardPath(w.shared, w.team.ID, w.teamCard.ID, "")

	// One after the other: the second still names version 1.
	expect(t, w.alice.do(http.MethodPatch, path, map[string]any{"version": 1, "title": "Alice's title"}), http.StatusOK)
	rec := w.owner.do(http.MethodPatch, path, map[string]any{"version": 1, "title": "Owner's title"})
	expect(t, rec, http.StatusConflict)
	reply := decode[struct {
		Error   string
		Code    string
		Current Card
	}](t, rec)
	if reply.Current.Title != "Alice's title" || reply.Current.Version != 2 || reply.Error == "" || reply.Code != "card_changed" {
		t.Errorf("409 reply %+v", reply)
	}
	expect(t, w.owner.do(http.MethodPost, cardPath(w.shared, w.team.ID, w.teamCard.ID, "/move"), map[string]any{"version": 1, "column": "todo"}),
		http.StatusConflict)
	expect(t, w.owner.do(http.MethodPost, cardPath(w.shared, w.team.ID, w.teamCard.ID, "/archive"), map[string]any{"version": 1}), http.StatusConflict)

	// At the same time.
	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			r := w.alice.do(http.MethodPost, cardPath(w.shared, w.team.ID, w.teamCard.ID, "/move"),
				map[string]any{"version": 2, "column": "doing", "position": i})
			codes[i] = r.Code
		})
	}
	wg.Wait()
	ok, conflict := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		}
	}
	if ok != 1 || conflict != n-1 {
		t.Errorf("concurrent moves: %v, want one 200 and the rest 409", codes)
	}
	if got := w.card(w.alice, w.shared, w.teamCard); got.Version != 3 || got.Column != Doing {
		t.Errorf("after the concurrent moves: %+v", got)
	}
}

// TestErrorCodes checks that the errors people can cause carry the code and params the web
// app says them with, and that the rest are bad_request.
func TestErrorCodes(t *testing.T) {
	w := newWorld(t)
	cards := boardPath(w.shared, w.team.ID, "/cards")
	path := cardPath(w.shared, w.team.ID, w.teamCard.ID, "")
	for name, tt := range map[string]struct {
		method, path string
		body         map[string]any
		code         string
		params       map[string]any
	}{
		"board name": {http.MethodPost, spacePath(w.shared, "/boards"), map[string]any{"name": strings.Repeat("n", 101)},
			"board_name_length", map[string]any{"max": float64(100)}},
		"title": {http.MethodPost, cards, map[string]any{"title": "two\nlines"}, "card_title_invalid", map[string]any{"max": float64(200)}},
		"body": {http.MethodPatch, path, map[string]any{"version": 1, "body": strings.Repeat("b", 33<<10)},
			"card_body_too_long", map[string]any{"max": float64(32)}},
		"blocked reason": {http.MethodPatch, path, map[string]any{"version": 1, "blocked_reason": " "},
			"blocked_reason_length", map[string]any{"max": float64(1000)}},
		"doc path": {http.MethodPatch, path, map[string]any{"version": 1, "docs": []string{"../up.md"}},
			"doc_path_invalid", map[string]any{"path": "../up.md"}},
		"links": {http.MethodPatch, path, map[string]any{"version": 1, "docs": manyDocs(51)}, "too_many_links", map[string]any{"max": float64(50)}},
		"comment": {http.MethodPost, cardPath(w.shared, w.team.ID, w.teamCard.ID, "/comments"), map[string]any{"body": "  "},
			"comment_length", map[string]any{"max": float64(32)}},
		"blocker": {http.MethodPatch, path, map[string]any{"version": 1, "blocked_by": []int64{999_999}},
			"card_blocker_not_found", map[string]any{"id": float64(999_999)}},
		"column":  {http.MethodPost, cards, map[string]any{"title": "x", "column": "later"}, "bad_request", nil},
		"version": {http.MethodPatch, path, map[string]any{"title": "x"}, "bad_request", nil},
	} {
		t.Run(name, func(t *testing.T) {
			rec := w.alice.do(tt.method, tt.path, tt.body)
			expect(t, rec, http.StatusBadRequest)
			reply := decode[struct {
				Code   string
				Params map[string]any
			}](t, rec)
			if reply.Code != tt.code || !reflect.DeepEqual(reply.Params, tt.params) {
				t.Fatalf("reply = %+v, want %s %v", reply, tt.code, tt.params)
			}
		})
	}
}

func manyDocs(n int) []string {
	docs := make([]string, n)
	for i := range docs {
		docs[i] = fmt.Sprintf("notes/%d.md", i)
	}
	return docs
}
