package search

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"taskto.ai/taskto/internal/access"
)

type paletteReply struct {
	Places        PlaceResults         `json:"places"`
	Docs          []Result             `json:"docs"`
	Cards         []CardResult         `json:"cards"`
	Records       []RecordResult       `json:"records"`
	Conversations []ConversationResult `json:"conversations"`
	Incomplete    bool                 `json:"incomplete"`
}

func (w *world) searchPalette(u *user, query string) (int, paletteReply) {
	w.e.t.Helper()
	rec := u.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/search/palette?q=%s", w.ws, url.QueryEscape(query)), nil)
	if rec.Code != http.StatusOK {
		return rec.Code, paletteReply{}
	}
	return rec.Code, decode[paletteReply](w.e.t, rec)
}

func TestPaletteReturnsEverySearchKind(t *testing.T) {
	w := newWorld(t)
	w.put(w.alice, w.team, "orchard.md", "orchard plan")
	board := w.teamBoard(w.alice, w.team, "Orchard board")
	w.addCard(w.alice, w.team, board, "Orchard card", "")
	app := w.newApp(w.alice, w.team, "Orchard expenses", true)
	w.addRecord(w.alice, w.team, app, "orchard lunch")
	src := newFakeTranscripts()
	w.e.idx.SetConversations(src)
	w.conversation(w.alice, w.ws, "Orchard trip", "planning", src)

	status, got := w.searchPalette(w.alice, "orchard")
	if status != http.StatusOK {
		t.Fatalf("palette status = %d", status)
	}
	if len(got.Places.Spaces) != 0 || len(got.Places.Apps) != 1 || got.Places.Apps[0].Name != "Orchard expenses" ||
		len(got.Places.Boards) != 1 || len(got.Docs) != 1 || len(got.Cards) != 1 || len(got.Records) != 1 || len(got.Conversations) != 1 {
		t.Fatalf("palette results = %+v", got)
	}
}

func TestPalettePlaceNamesIgnoreCaseAndAccents(t *testing.T) {
	w := newWorld(t)
	visible := w.newApp(w.alice, w.team, "Café reports", false)
	archived := w.newApp(w.alice, w.team, "Café archived", false)
	hidden := w.newApp(w.owner, w.team, "Café hidden", false)
	if _, err := w.e.db.Exec(`UPDATE apps SET archived_at = CURRENT_TIMESTAMP WHERE id = ?`, archived.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.db.Exec(`DELETE FROM app_roles WHERE app_id = ?`, hidden.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.db.Exec(`INSERT INTO app_roles (app_id, user_id, role) VALUES (?, ?, 'builder')`, hidden.ID, w.owner.id); err != nil {
		t.Fatal(err)
	}
	board := w.teamBoard(w.alice, w.team, "Café planning")
	archivedBoard := w.teamBoard(w.alice, w.team, "Café archived planning")
	if _, err := w.e.db.Exec(`UPDATE boards SET archived_at = CURRENT_TIMESTAMP WHERE id = ?`, archivedBoard); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.db.Exec(`UPDATE spaces SET name = 'Café team' WHERE id = ?`, w.team.ID); err != nil {
		t.Fatal(err)
	}
	archivedSpace := w.createShared("Café retired space")
	w.addMember(archivedSpace, w.alice, access.RoleViewer)
	w.newApp(w.owner, archivedSpace, "Café retired app", false)
	w.teamBoard(w.owner, archivedSpace, "Café retired board")
	if _, err := w.e.db.Exec(`UPDATE spaces SET archived_at = CURRENT_TIMESTAMP WHERE id = ?`, archivedSpace.ID); err != nil {
		t.Fatal(err)
	}

	status, got := w.searchPalette(w.alice, "CAFE")
	if status != http.StatusOK || len(got.Places.Spaces) != 1 || got.Places.Spaces[0].ID != w.team.ID ||
		len(got.Places.Apps) != 1 || got.Places.Apps[0].AppID != visible.ID ||
		len(got.Places.Boards) != 1 || got.Places.Boards[0].BoardID != board {
		t.Fatalf("palette status %d, places %+v", status, got.Places)
	}
	expect(t, w.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", w.ws, w.team.ID, w.alice.id), nil), http.StatusNoContent)
	status, got = w.searchPalette(w.alice, "CAFE")
	if status != http.StatusOK || len(got.Places.Spaces) != 0 || len(got.Places.Apps) != 0 || len(got.Places.Boards) != 0 {
		t.Fatalf("revoked space remained in palette: status %d places %+v", status, got.Places)
	}
}

func TestPlaceRankingUsesUnicodeCharacterCount(t *testing.T) {
	names := []string{"abcd", "a猫"}
	ranked := sortPlaces(names, func(s string) string { return s }, "a", 5)
	if len(ranked) != 2 || ranked[0] != "a猫" {
		t.Fatalf("ranked names = %v", ranked)
	}
}

func TestPaletteQueryValidation(t *testing.T) {
	w := newWorld(t)
	base := fmt.Sprintf("/api/workspaces/%d/search/palette", w.ws)
	expectCode(t, w.alice.do(http.MethodGet, base, nil), http.StatusBadRequest, "search_query_required", nil)
	for _, limit := range []string{"11", "-1", "many"} {
		expectCode(t, w.alice.do(http.MethodGet, base+"?q=orchard&limit="+limit, nil), http.StatusBadRequest, "bad_request", nil)
	}
	expect(t, w.alice.do(http.MethodGet, base+"?q=orchard&limit=10", nil), http.StatusOK)
}

func TestPaletteRefusesAccessTokens(t *testing.T) {
	w := newWorld(t)
	created := decode[struct {
		Secret string `json:"token"`
	}](t, w.alice.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/tokens", w.ws), map[string]string{"name": "palette"}))
	target := fmt.Sprintf("/api/workspaces/%d/search/palette?q=orchard", w.ws)
	for secret, status := range map[string]int{created.Secret: http.StatusForbidden, "invalid": http.StatusUnauthorized} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.Header.Set("Authorization", "Bearer "+secret)
		response := httptest.NewRecorder()
		w.e.h.ServeHTTP(response, request)
		expect(t, response, status)
	}
	cards := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/search/cards?q=orchard", w.ws), nil)
	cards.Header.Set("Authorization", "Bearer "+created.Secret)
	response := httptest.NewRecorder()
	w.e.h.ServeHTTP(response, cards)
	expect(t, response, http.StatusOK)
}

func TestPaletteLimitsEachKind(t *testing.T) {
	w := newWorld(t)
	for i := 0; i < 4; i++ {
		w.newApp(w.alice, w.team, fmt.Sprintf("Orchard app %d", i), false)
	}
	for _, c := range []struct {
		query string
		want  int
	}{{"orchard", 5}, {"orchard&limit=2", 2}} {
		rec := w.alice.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/search/palette?q=%s", w.ws, c.query), nil)
		expect(t, rec, http.StatusOK)
		if got := decode[paletteReply](t, rec); len(got.Places.Apps) != min(c.want, 4) {
			t.Fatalf("query %q: %d apps, want %d", c.query, len(got.Places.Apps), min(c.want, 4))
		}
	}
}

func TestPaletteDoesNotCallEmbeddingsEndpoint(t *testing.T) {
	w := newWorld(t)
	w.setModel()
	w.put(w.alice, w.team, "orchard.md", "orchard")
	if _, err := w.e.idx.Sync(ctx, w.team); err != nil {
		t.Fatal(err)
	}
	w.e.embed.calls.Store(0)
	status, _ := w.searchPalette(w.alice, "orchard")
	if status != http.StatusOK || w.e.embed.calls.Load() != 0 {
		t.Fatalf("palette status %d, embedding calls %d", status, w.e.embed.calls.Load())
	}
}

func TestPaletteDoesNotReturnAnotherUsersPrivateResults(t *testing.T) {
	w := newWorld(t)
	private := w.private[w.alice]
	w.put(w.alice, private, "orchard.md", "orchard private")
	board := w.firstBoard(w.alice, private)
	w.addCard(w.alice, private, board, "Orchard private card", "")
	app := w.newApp(w.alice, private, "Orchard private app", true)
	w.addRecord(w.alice, private, app, "orchard private record")
	src := newFakeTranscripts()
	w.e.idx.SetConversations(src)
	w.conversation(w.alice, w.ws, "Orchard private conversation", "orchard private", src)

	status, got := w.searchPalette(w.bob, "orchard")
	if status != http.StatusOK || len(got.Places.Spaces) != 0 || len(got.Places.Apps) != 0 || len(got.Places.Boards) != 0 ||
		len(got.Docs) != 0 || len(got.Cards) != 0 || len(got.Records) != 0 || len(got.Conversations) != 0 {
		t.Fatalf("palette status %d, leaked results %+v", status, got)
	}
}
