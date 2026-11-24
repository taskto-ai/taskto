package search

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
)

// cardHitJSON is a card found, as the API says it.
type cardHitJSON struct {
	SpaceID   int64  `json:"space_id"`
	SpaceName string `json:"space_name"`
	BoardID   int64  `json:"board_id"`
	BoardName string `json:"board_name"`
	CardID    int64  `json:"card_id"`
	Title     string `json:"title"`
	Column    string `json:"column"`
	Snippet   []Segment
}

type cardsReply struct {
	Results    []cardHitJSON `json:"results"`
	NextOffset *int          `json:"next_offset"`
}

func boardsURL(sp access.Space) string {
	return fmt.Sprintf("/api/workspaces/%d/spaces/%d/boards", sp.WorkspaceID, sp.ID)
}

// firstBoard is the first board of sp that u can list: a private space's personal board.
func (w *world) firstBoard(u *user, sp access.Space) int64 {
	w.e.t.Helper()
	rec := u.do(http.MethodGet, boardsURL(sp), nil)
	expect(w.e.t, rec, http.StatusOK)
	list := decode[struct{ Boards []struct{ ID int64 } }](w.e.t, rec).Boards
	if len(list) == 0 {
		w.e.t.Fatalf("no board in space %d", sp.ID)
	}
	return list[0].ID
}

// teamBoard creates a team board in sp, as u.
func (w *world) teamBoard(u *user, sp access.Space, name string) int64 {
	w.e.t.Helper()
	rec := u.do(http.MethodPost, boardsURL(sp), map[string]string{"name": name})
	expect(w.e.t, rec, http.StatusCreated)
	return decode[struct{ ID int64 }](w.e.t, rec).ID
}

type cardJSON struct {
	ID      int64
	Version int64
}

func cardURL(sp access.Space, board int64, tail string) string {
	return fmt.Sprintf("%s/%d/cards%s", boardsURL(sp), board, tail)
}

// addCard adds a card to a board, as u.
func (w *world) addCard(u *user, sp access.Space, board int64, title, body string) cardJSON {
	w.e.t.Helper()
	rec := u.do(http.MethodPost, cardURL(sp, board, ""), map[string]string{"title": title, "body": body})
	expect(w.e.t, rec, http.StatusCreated)
	return decode[cardJSON](w.e.t, rec)
}

func (w *world) patchCard(u *user, sp access.Space, board int64, c cardJSON, change map[string]any) cardJSON {
	w.e.t.Helper()
	change["version"] = c.Version
	rec := u.do(http.MethodPatch, cardURL(sp, board, fmt.Sprintf("/%d", c.ID)), change)
	expect(w.e.t, rec, http.StatusOK)
	return decode[cardJSON](w.e.t, rec)
}

func (w *world) commentOn(u *user, sp access.Space, board int64, c cardJSON, body string) {
	w.e.t.Helper()
	expect(w.e.t, u.do(http.MethodPost, cardURL(sp, board, fmt.Sprintf("/%d/comments", c.ID)), map[string]string{"body": body}), http.StatusCreated)
}

// searchCards searches the workspace, or one space, for cards as u.
func (w *world) searchCards(u *user, sp *access.Space, q string) *cardsReply {
	w.e.t.Helper()
	target := fmt.Sprintf("/api/workspaces/%d/search/cards?q=%s", w.ws, url.QueryEscape(q))
	if sp != nil {
		target = fmt.Sprintf("/api/workspaces/%d/spaces/%d/search/cards?q=%s", sp.WorkspaceID, sp.ID, url.QueryEscape(q))
	}
	rec := u.do(http.MethodGet, target, nil)
	expect(w.e.t, rec, http.StatusOK)
	r := decode[cardsReply](w.e.t, rec)
	return &r
}

// titles lists the titles of the cards of a reply.
func (r *cardsReply) titles() []string {
	out := []string{}
	for _, c := range r.Results {
		out = append(out, c.Title)
	}
	return out
}

// wantTitles checks which cards a reply has, in any order: use wantRanked for an order.
func wantTitles(t *testing.T, who string, r *cardsReply, want ...string) {
	t.Helper()
	got := r.titles()
	slices.Sort(got)
	slices.Sort(want)
	if len(want) == 0 {
		want = []string{}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s found %q, want %q", who, got, want)
	}
}

func wantRanked(t *testing.T, who string, r *cardsReply, want ...string) {
	t.Helper()
	got := r.titles()
	if len(want) == 0 {
		want = []string{}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s found %q, want %q", who, got, want)
	}
}

// Cards are found only in the boards of the spaces the caller can read: their private
// space, and the shared spaces they are a member of; never another member's private space
// or a shared space they aren't in, admins included.
func TestCardSearchIsOnlyOfBoardsTheCallerCanRead(t *testing.T) {
	w := newWorld(t)
	team := w.teamBoard(w.owner, w.team, "Roadmap")
	secret := w.teamBoard(w.owner, w.secret, "Secret board")
	w.addCard(w.alice, w.team, team, "Quokka rollout", "Ship the quokka release")
	w.addCard(w.owner, w.secret, secret, "Quokka acquisition", "Hush")
	w.addCard(w.alice, w.private[w.alice], w.firstBoard(w.alice, w.private[w.alice]), "Quokka diary", "Alice's own")
	w.addCard(w.bob, w.private[w.bob], w.firstBoard(w.bob, w.private[w.bob]), "Quokka notes", "Bob's own")
	w.addCard(w.mallory, w.private[w.mallory], w.firstBoard(w.mallory, w.private[w.mallory]), "Quokka plot", "Mallory's own")
	w.addCard(w.owner, w.private[w.owner], w.firstBoard(w.owner, w.private[w.owner]), "Quokka ledger", "Owner's own")

	// Each sees the team's board, and their own board, and nothing else; the owner is the
	// workspace's admin and a member of the secret space, but not of the others' private ones.
	wantTitles(t, "alice", w.searchCards(w.alice, nil, "quokka"), "Quokka diary", "Quokka rollout")
	wantTitles(t, "bob (a viewer)", w.searchCards(w.bob, nil, "quokka"), "Quokka notes", "Quokka rollout")
	wantTitles(t, "mallory (in no shared space)", w.searchCards(w.mallory, nil, "quokka"), "Quokka plot")
	wantTitles(t, "owner", w.searchCards(w.owner, nil, "quokka"), "Quokka acquisition", "Quokka ledger", "Quokka rollout")

	// One space: its own cards, and 404 for a space the caller can't read, as for documents.
	wantTitles(t, "alice in team", w.searchCards(w.alice, &w.team, "quokka"), "Quokka rollout")
	alicePrivate, malloryPrivate := w.private[w.alice], w.private[w.mallory]
	wantTitles(t, "alice in her own space", w.searchCards(w.alice, &alicePrivate, "quokka"), "Quokka diary")
	for _, c := range []struct {
		who *user
		sp  access.Space
	}{
		{w.mallory, w.team}, {w.alice, w.secret}, {w.bob, w.private[w.alice]},
		{w.owner, w.private[w.alice]}, {w.alice, w.private[w.bob]},
	} {
		rec := c.who.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces/%d/search/cards?q=quokka", c.sp.WorkspaceID, c.sp.ID), nil)
		expect(t, rec, http.StatusNotFound)
	}
	// Without a session, nothing.
	anon := &user{e: w.e}
	expect(t, anon.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/search/cards?q=quokka", w.ws), nil), http.StatusUnauthorized)

	// A space's cards are never found through another space the caller can read.
	wantTitles(t, "mallory in her own space", w.searchCards(w.mallory, &malloryPrivate, "quokka"), "Quokka plot")
	wantTitles(t, "mallory searching for what only the team has", w.searchCards(w.mallory, nil, "rollout"))
}

// A person who left the workspace has a private space nobody reads, and the cards of an
// archived space or board are still found by those who read them.
func TestCardSearchOfClosedAndArchivedSpaces(t *testing.T) {
	w := newWorld(t)
	oldBoard := w.teamBoard(w.owner, w.old, "Old board")
	w.addCard(w.owner, w.old, oldBoard, "Narwhal census", "of the old space")
	strangerBoard := w.firstBoard(w.stranger, w.private[w.stranger])
	w.addCard(w.stranger, w.private[w.stranger], strangerBoard, "Narwhal secrets", "left behind")
	team := w.teamBoard(w.owner, w.team, "Doomed board")
	w.addCard(w.owner, w.team, team, "Narwhal archive", "in a board that is archived next")
	expect(t, w.owner.do(http.MethodPatch, boardsURL(w.team)+fmt.Sprintf("/%d", team), map[string]any{"archived": true, "version": 1}), http.StatusOK)
	w.finish()

	// Alice reads the archived space (a viewer) and the archived board.
	wantTitles(t, "alice", w.searchCards(w.alice, nil, "narwhal"), "Narwhal archive", "Narwhal census")
	// Nobody reads the private space of the person who left, the owner included; the
	// stranger has no access to anything.
	wantTitles(t, "owner", w.searchCards(w.owner, nil, "narwhal"), "Narwhal archive", "Narwhal census")
	rec := w.stranger.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/search/cards?q=narwhal", w.ws), nil)
	if rec.Code == http.StatusOK {
		t.Fatalf("a person who left searched the workspace: %s", rec.Body)
	}
}

// What is found follows the card: its title, body, thread and column, in the same
// transaction the board's change was made in or at the next search.
func TestCardSearchFollowsTheCard(t *testing.T) {
	w := newWorld(t)
	board := w.teamBoard(w.alice, w.team, "Work")
	c := w.addCard(w.alice, w.team, board, "Plan the orchard", "Apples in spring")
	found := func(q string, want ...string) {
		t.Helper()
		wantTitles(t, "bob searching "+q, w.searchCards(w.bob, nil, q), want...)
	}
	found("orchard", "Plan the orchard")
	found("spring", "Plan the orchard")
	found("pears")

	// A change to the title and body is searched at once; the words that went are gone.
	c = w.patchCard(w.alice, w.team, board, c, map[string]any{"title": "Plan the vineyard", "body": "Pears in autumn"})
	found("orchard")
	found("spring")
	found("vineyard", "Plan the vineyard")
	found("pears", "Plan the vineyard")

	// A comment changes no version of the card, and is searched too, by every reader.
	w.commentOn(w.alice, w.team, board, c, "Grapes need a trellis")
	found("trellis", "Plan the vineyard")
	w.commentOn(w.alice, w.team, board, c, "Second thoughts about the pergola")
	found("pergola", "Plan the vineyard")
	found("trellis", "Plan the vineyard")

	// Moving and archiving say where the card is; an archived card stays found.
	moved := decode[struct {
		ID      int64
		Version int64
		Column  string
	}](t, w.alice.do(http.MethodPost, cardURL(w.team, board, fmt.Sprintf("/%d/move", c.ID)), map[string]any{"version": c.Version, "column": "doing"}))
	if r := w.searchCards(w.bob, nil, "vineyard"); len(r.Results) != 1 || r.Results[0].Column != "doing" {
		t.Fatalf("after the move: %+v", r.Results)
	}
	expect(t, w.alice.do(http.MethodPost, cardURL(w.team, board, fmt.Sprintf("/%d/archive", c.ID)), map[string]any{"version": moved.Version}), http.StatusOK)
	if r := w.searchCards(w.bob, nil, "vineyard"); len(r.Results) != 1 || r.Results[0].Column != "archived" {
		t.Fatalf("after the archive: %+v", r.Results)
	}

	// A card that is gone, with its board or its space, is no longer found, and its index
	// goes with it.
	if _, err := w.e.idx.db.Exec(`DELETE FROM boards WHERE id = ?`, board); err != nil {
		t.Fatal(err)
	}
	found("vineyard")
	var n int
	if err := w.e.idx.db.QueryRow(`SELECT (SELECT count(*) FROM search_cards) + (SELECT count(*) FROM search_card_fts)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the index kept %d rows of a deleted card: %v", n, err)
	}
}

// A card is found by its text, shown with the words that matched marked, never as markup;
// documents and cards are searched apart.
func TestCardSearchResultsAndDocuments(t *testing.T) {
	w := newWorld(t)
	board := w.teamBoard(w.alice, w.team, "Work")
	w.addCard(w.alice, w.team, board, "Budget", "We <b>budget</b> for the pangolin sanctuary in March")
	w.addCard(w.alice, w.team, board, "Pangolin", "Plain")
	w.put(w.alice, w.team, "pangolin.md", "# Pangolins\nA document")

	r := w.searchCards(w.bob, nil, "pangolin")
	// A title counts more than the body.
	wantRanked(t, "bob", r, "Pangolin", "Budget")
	hit := r.Results[1]
	if hit.SpaceName != "Team" || hit.BoardName != "Work" || hit.BoardID != board || hit.SpaceID != w.team.ID || hit.Column != "backlog" {
		t.Fatalf("hit = %+v", hit)
	}
	var plain strings.Builder
	marked := false
	for _, s := range hit.Snippet {
		plain.WriteString(s.Text)
		marked = marked || (s.Match && strings.EqualFold(s.Text, "pangolin"))
	}
	if !marked || !strings.Contains(plain.String(), "<b>budget</b>") {
		t.Fatalf("snippet = %+v", hit.Snippet)
	}

	// The documents' search finds the document, not the cards.
	rec := w.search(w.bob, nil, "pangolin")
	expect(t, rec, http.StatusOK)
	docs := decode[Results](t, rec)
	if len(docs.Results) != 1 || docs.Results[0].Path != "pangolin.md" {
		t.Fatalf("documents found: %+v", docs.Results)
	}

	// A query is checked as a document search's is.
	for _, q := range []string{"", "   ", "!!!", strings.Repeat("a", MaxQueryLen+1)} {
		rec := w.bob.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/search/cards?q=%s", w.ws, url.QueryEscape(q)), nil)
		expect(t, rec, http.StatusBadRequest)
	}
	expectCode(t, w.bob.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/search/cards?q=", w.ws), nil),
		http.StatusBadRequest, string(apierr.CodeSearchQueryRequired), nil)
}

// Cards come a page at a time, as documents do.
func TestCardSearchPages(t *testing.T) {
	w := newWorld(t)
	board := w.teamBoard(w.alice, w.team, "Work")
	for i := range 5 {
		w.addCard(w.alice, w.team, board, fmt.Sprintf("Lemur %d", i), "")
	}
	var seen []string
	next := 0
	for range 5 {
		rec := w.bob.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/search/cards?q=lemur&limit=2&offset=%d", w.ws, next), nil)
		expect(t, rec, http.StatusOK)
		page := decode[cardsReply](t, rec)
		seen = append(seen, page.titles()...)
		if page.NextOffset == nil {
			break
		}
		next = *page.NextOffset
	}
	if len(seen) != 5 {
		t.Fatalf("pages gave %q", seen)
	}
	distinct := map[string]bool{}
	for _, s := range seen {
		distinct[s] = true
	}
	if len(distinct) != 5 {
		t.Fatalf("pages repeated cards: %q", seen)
	}
}

// Text in scripts written without spaces is found anywhere in a card, as in a document.
func TestCardSearchUnspacedScripts(t *testing.T) {
	w := newWorld(t)
	board := w.teamBoard(w.alice, w.team, "Work")
	w.addCard(w.alice, w.team, board, "東京都庁の見学", "")
	wantTitles(t, "bob", w.searchCards(w.bob, nil, "京都"), "東京都庁の見学")
	wantTitles(t, "bob", w.searchCards(w.bob, nil, "大阪"))
}

// The index of cards made at startup covers the cards there already were.
func TestCardsAreIndexedAtStartup(t *testing.T) {
	w := newWorld(t)
	board := w.teamBoard(w.alice, w.team, "Work")
	w.addCard(w.alice, w.team, board, "Capybara census", "")
	if err := w.e.idx.syncCards(ctx, ""); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := w.e.idx.db.QueryRow(`SELECT count(*) FROM search_cards`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("indexed %d cards at startup: %v", n, err)
	}
	wantTitles(t, "bob", w.searchCards(w.bob, nil, "capybara"), "Capybara census")
}
