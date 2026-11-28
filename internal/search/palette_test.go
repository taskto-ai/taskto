package search

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
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
	for _, limit := range []string{"0", "11", "-1", "many"} {
		rec := w.alice.do(http.MethodGet, base+"?q=orchard&limit="+limit, nil)
		expectCode(t, rec, http.StatusBadRequest, "bad_request", nil)
		if !strings.Contains(rec.Body.String(), "limit must be a number from 1 to 10") {
			t.Fatalf("limit %s: %s", limit, rec.Body)
		}
	}
	expect(t, w.alice.do(http.MethodGet, base+"?q=orchard&limit=1", nil), http.StatusOK)
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

func (w *world) readableSpaces(u *user) []access.Space {
	w.e.t.Helper()
	spaces, err := w.e.spaces.Readable(ctx, u.id, w.ws)
	if err != nil {
		w.e.t.Fatal(err)
	}
	for i := range spaces {
		spaces[i] = w.generationOf(spaces[i])
	}
	return spaces
}

func (w *world) placesIn(u *user, spaces []access.Space, text string) PlaceResults {
	w.e.t.Helper()
	got, err := w.e.idx.SearchPlaces(ctx, w.e.spaces, u.id, w.ws, spaces, text, 10)
	if err != nil {
		w.e.t.Fatal(err)
	}
	return got
}

func noPlaces(p PlaceResults) bool {
	return len(p.Spaces) == 0 && len(p.Apps) == 0 && len(p.Boards) == 0
}

func TestPalettePlacesOfASpaceLostBetweenReadingAndAnsweringAreNotReleased(t *testing.T) {
	w := newWorld(t)
	w.newApp(w.alice, w.team, "Café reports", false)
	w.teamBoard(w.alice, w.team, "Café planning")
	w.exec(`UPDATE spaces SET name = 'Café team' WHERE id = ?`, w.team.ID)
	spaces := w.readableSpaces(w.alice)
	if got := w.placesIn(w.alice, spaces, "cafe"); len(got.Spaces) != 1 || len(got.Apps) != 1 || len(got.Boards) != 1 {
		t.Fatalf("fixture: places before the removal = %+v", got)
	}

	w.removeMember(w.team, w.alice)
	if got := w.placesIn(w.alice, spaces, "cafe"); !noPlaces(got) {
		t.Fatalf("places of a space alice no longer reads = %+v", got)
	}
}

func TestPalettePlacesOfAnArchivedSpaceAreNotReleasedEvenFromAnEarlierList(t *testing.T) {
	w := newWorld(t)
	w.newApp(w.alice, w.team, "Café reports", false)
	w.exec(`UPDATE spaces SET name = 'Café team' WHERE id = ?`, w.team.ID)
	spaces := w.readableSpaces(w.alice)
	w.exec(`UPDATE spaces SET archived_at = CURRENT_TIMESTAMP WHERE id = ?`, w.team.ID)
	if got := w.placesIn(w.alice, spaces, "cafe"); !noPlaces(got) {
		t.Fatalf("places of a space archived since the list = %+v", got)
	}
}

func TestPaletteDoesNotNameAReplacedSpaceAfterItsIDIsGivenAgain(t *testing.T) {
	w := newWorld(t)
	gone := w.createShared("Café gone")
	w.addMember(gone, w.alice, access.RoleViewer)
	spaces := w.readableSpaces(w.alice)
	if got := w.placesIn(w.alice, spaces, "cafe gone"); len(got.Spaces) != 1 {
		t.Fatalf("fixture: places before the replacement = %+v", got)
	}

	fresh := w.replace(gone, false)
	w.exec(`INSERT INTO space_members (space_id, user_id, role) VALUES (?, ?, 'viewer')`, fresh.ID, w.alice.id)
	if got := w.placesIn(w.alice, spaces, "cafe gone"); !noPlaces(got) {
		t.Fatalf("a removed space's name came back under the ID it had: %+v", got)
	}
	if got := w.placesIn(w.alice, w.readableSpaces(w.alice), "replacement"); len(got.Spaces) != 1 || got.Spaces[0].ID != fresh.ID {
		t.Fatalf("the replacement is not found: %+v", got)
	}
}

func TestPaletteDoesNotSearchPrivateSpaceNamesButSearchesTheirApps(t *testing.T) {
	w := newWorld(t)
	private := w.private[w.alice]
	if private.Name == "" {
		t.Fatal("fixture: the private space has no name")
	}
	w.newApp(w.alice, private, "Mine to count", false)
	_, got := w.searchPalette(w.alice, private.Name)
	if len(got.Places.Spaces) != 0 {
		t.Fatalf("a private space was found by its name: %+v", got.Places.Spaces)
	}
	if _, got := w.searchPalette(w.alice, "mine to"); len(got.Places.Apps) != 1 {
		t.Fatalf("alice's own private app: %+v", got.Places)
	}
}

func TestPlaceNamesAreRankedByPrefixThenLengthThenName(t *testing.T) {
	names := []string{"Big Café", "Café reports", "Cafés", "Café", "Cafe b", "Cafe a"}
	got := sortPlaces(slices.Clone(names), func(s string) string { return s }, "cafe", 10)
	want := []string{"Café", "Cafés", "Cafe a", "Cafe b", "Café reports", "Big Café"}
	if !slices.Equal(got, want) {
		t.Fatalf("ranked %v, want %v", got, want)
	}
	if got := sortPlaces(slices.Clone(names), func(s string) string { return s }, "cafe", 2); !slices.Equal(got, want[:2]) {
		t.Fatalf("limited to two: %v", got)
	}
}

func TestPlaceNamesMatchWithoutCaseOrAccents(t *testing.T) {
	for _, c := range []struct{ name, query string }{
		{"Crème brûlée", "creme BRULEE"},
		{"Cafe", "café"},
		{"CAFÉ", "cafe"},
		{"Ångström lab", "angstrom"},
	} {
		if !placeMatches(c.name, folded(c.query)) {
			t.Errorf("%q was not found by %q", c.name, c.query)
		}
	}
	if placeMatches("Cafe", folded("cafes")) {
		t.Error("a longer query found a shorter name")
	}
}

func TestPaletteGivesFiveOfEachKindByDefaultAndWhatLimitAsksFor(t *testing.T) {
	w := newWorld(t)
	for i := 0; i < 12; i++ {
		w.newApp(w.alice, w.team, fmt.Sprintf("Orchard app %d", i), false)
		w.teamBoard(w.alice, w.team, fmt.Sprintf("Orchard board %d", i))
	}
	for _, c := range []struct {
		query string
		want  int
	}{{"orchard", 5}, {"orchard&limit=2", 2}, {"orchard&limit=10", 10}} {
		rec := w.alice.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/search/palette?q=%s", w.ws, c.query), nil)
		expect(t, rec, http.StatusOK)
		if got := decode[paletteReply](t, rec); len(got.Places.Apps) != c.want || len(got.Places.Boards) != c.want {
			t.Fatalf("query %q: %d apps and %d boards, want %d of each", c.query, len(got.Places.Apps), len(got.Places.Boards), c.want)
		}
	}
}

func TestPaletteIsOnlyForAMemberOfTheWorkspaceWithASession(t *testing.T) {
	w := newWorld(t)
	target := fmt.Sprintf("/api/workspaces/%d/search/palette?q=orchard", w.ws)
	anonymous := &user{e: w.e}
	expect(t, anonymous.do(http.MethodGet, target, nil), http.StatusUnauthorized)
	expect(t, w.stranger.do(http.MethodGet, target, nil), http.StatusOK)
	w.finish()
	if rec := w.stranger.do(http.MethodGet, target, nil); rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Fatalf("a person who left the workspace: %d %s", rec.Code, rec.Body)
	}
}

func TestPaletteSaysWhenIndexingIsBehindAndAnswersEmptyKindsAsLists(t *testing.T) {
	w := newWorld(t)
	rec := w.alice.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/search/palette?q=nothingmatches", w.ws), nil)
	expect(t, rec, http.StatusOK)
	for _, empty := range []string{`"spaces":[]`, `"apps":[]`, `"boards":[]`, `"docs":[]`, `"cards":[]`, `"records":[]`, `"conversations":[]`, `"incomplete":false`} {
		if !strings.Contains(rec.Body.String(), empty) {
			t.Fatalf("the reply lacks %s: %s", empty, rec.Body)
		}
	}

	board := w.teamBoard(w.alice, w.team, "Work")
	c := w.addCard(w.alice, w.team, board, "Tapir", "")
	if _, err := w.e.idx.db.Exec(`CREATE TRIGGER sync_fails BEFORE DELETE ON search_card_queue BEGIN SELECT RAISE(ABORT, 'no'); END`); err != nil {
		t.Fatal(err)
	}
	w.patchCard(w.alice, w.team, board, c, map[string]any{"body": "tapir, changed"})
	if _, got := w.searchPalette(w.bob, "tapir"); !got.Incomplete {
		t.Fatalf("a palette whose card index could not catch up did not say so: %+v", got)
	}
}

func TestAFailedQueryEmbeddingLogsOnlyThatItFailed(t *testing.T) {
	w := newWorld(t)
	var logged bytes.Buffer
	w.e.idx.log = slog.New(slog.NewTextHandler(&logged, nil))
	w.setModel()
	w.put(w.alice, w.team, "orchard.md", "orchard")
	if _, err := w.e.idx.Sync(ctx, w.team); err != nil {
		t.Fatal(err)
	}
	w.e.embed.fail.Store(true)
	expect(t, w.search(w.alice, nil, "orchard"), http.StatusOK)
	out := logged.String()
	if !strings.Contains(out, "embedding a query failed") {
		t.Fatalf("nothing was logged for the failure: %q", out)
	}
	for _, leak := range []string{fakeKey, "answered", "503", w.e.embed.srv.URL} {
		if strings.Contains(out, leak) {
			t.Errorf("the log holds %q: %s", leak, out)
		}
	}
}

func TestPaletteRefusesAQueryOverTheLengthTheDocsName(t *testing.T) {
	w := newWorld(t)
	base := fmt.Sprintf("/api/workspaces/%d/search/palette?q=", w.ws)
	expect(t, w.alice.do(http.MethodGet, base+strings.Repeat("a", MaxQueryLen), nil), http.StatusOK)
	expectCode(t, w.alice.do(http.MethodGet, base+strings.Repeat("a", MaxQueryLen+1), nil), http.StatusBadRequest, "search_query_too_long", map[string]any{"max": float64(MaxQueryLen)})
	expectCode(t, w.alice.do(http.MethodGet, base+"%20%20", nil), http.StatusBadRequest, "search_query_required", nil)
}

func (w *world) restrictApp(a appJSON, users ...*user) {
	w.e.t.Helper()
	w.exec(`INSERT INTO app_roles (app_id, user_id, role) VALUES (?, ?, 'builder')`, a.ID, w.owner.id)
	for _, u := range users {
		w.exec(`INSERT INTO app_roles (app_id, user_id, role) VALUES (?, ?, 'user')`, a.ID, u.id)
	}
}

func placeNames[T any](items []T, name func(T) string) []string {
	out := []string{}
	for _, item := range items {
		out = append(out, name(item))
	}
	return out
}

func TestPaletteFillsItsPageFromAppsTheCallerStillReadsAfterALoss(t *testing.T) {
	w := newWorld(t)
	first := w.newApp(w.alice, w.team, "Café a", false)
	w.newApp(w.alice, w.team, "Café b", false)
	w.newApp(w.alice, w.team, "Café c", false)
	w.restrictApp(first, w.alice, w.bob)
	spaces := w.readableSpaces(w.alice)
	appNames := func(p PlaceResults) []string { return placeNames(p.Apps, func(a PlaceApp) string { return a.Name }) }

	w.e.idx.afterPlacesRead = func() {
		w.e.idx.afterPlacesRead = nil
		w.exec(`DELETE FROM app_roles WHERE app_id = ? AND user_id = ?`, first.ID, w.alice.id)
	}
	got, err := w.e.idx.SearchPlaces(ctx, w.e.spaces, w.alice.id, w.ws, spaces, "cafe", 2)
	if err != nil {
		t.Fatal(err)
	}
	if names := appNames(got); !slices.Equal(names, []string{"Café b", "Café c"}) {
		t.Fatalf("alice lost the best match while the page was made; apps = %v", names)
	}

	gotBob, err := w.e.idx.SearchPlaces(ctx, w.e.spaces, w.bob.id, w.ws, w.readableSpaces(w.bob), "cafe", 2)
	if err != nil {
		t.Fatal(err)
	}
	if names := appNames(gotBob); !slices.Equal(names, []string{"Café a", "Café b"}) {
		t.Fatalf("bob keeps his role in the app alice lost; apps = %v", names)
	}
}

func TestPaletteFillsItsPageFromSpacesAndBoardsTheCallerStillReadsAfterALoss(t *testing.T) {
	w := newWorld(t)
	near := w.createShared("Café a")
	far := w.createShared("Café bb")
	for _, sp := range []access.Space{near, far} {
		w.addMember(sp, w.alice, access.RoleViewer)
		w.addMember(sp, w.bob, access.RoleViewer)
	}
	w.teamBoard(w.owner, near, "Café a")
	w.teamBoard(w.owner, far, "Café bb")
	spaces := w.readableSpaces(w.alice)

	w.e.idx.afterPlacesRead = func() {
		w.e.idx.afterPlacesRead = nil
		w.removeMember(near, w.alice)
	}
	got, err := w.e.idx.SearchPlaces(ctx, w.e.spaces, w.alice.id, w.ws, spaces, "cafe", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Spaces) != 1 || got.Spaces[0].ID != far.ID || len(got.Boards) != 1 || got.Boards[0].SpaceID != far.ID {
		t.Fatalf("alice lost the nearer space while the page was made; places = %+v", got)
	}

	gotBob, err := w.e.idx.SearchPlaces(ctx, w.e.spaces, w.bob.id, w.ws, w.readableSpaces(w.bob), "cafe", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotBob.Spaces) != 1 || gotBob.Spaces[0].ID != near.ID || len(gotBob.Boards) != 1 || gotBob.Boards[0].SpaceID != near.ID {
		t.Fatalf("bob is still in the nearer space; places = %+v", gotBob)
	}
}

func TestPalettePlacesAreCheckedAgainInOneSnapshot(t *testing.T) {
	w := newWorld(t)
	app := w.newApp(w.alice, w.team, "Café a", false)
	w.restrictApp(app, w.alice, w.bob)
	spaces := w.readableSpaces(w.alice)

	w.e.idx.betweenPlaceChecks = func() {
		w.e.idx.betweenPlaceChecks = nil
		w.exec(`DELETE FROM app_roles WHERE app_id = ? AND user_id = ?`, app.ID, w.alice.id)
	}
	got, err := w.e.idx.SearchPlaces(ctx, w.e.spaces, w.alice.id, w.ws, spaces, "cafe", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Apps) != 1 {
		t.Fatalf("the space and its apps were not read as of the same moment: %+v", got.Apps)
	}

	if after := w.placesIn(w.alice, spaces, "cafe"); len(after.Apps) != 0 {
		t.Fatalf("the next page still names an app alice no longer reads: %+v", after.Apps)
	}
	if bob := w.placesIn(w.bob, w.readableSpaces(w.bob), "cafe"); len(bob.Apps) != 1 {
		t.Fatalf("bob's role in the app was touched: %+v", bob.Apps)
	}
}

func TestSearchPlacesRefusesALimitOutsideOneToTen(t *testing.T) {
	w := newWorld(t)
	spaces := w.readableSpaces(w.alice)
	for _, limit := range []int{0, -1, 11, 1000} {
		if _, err := w.e.idx.SearchPlaces(ctx, w.e.spaces, w.alice.id, w.ws, spaces, "cafe", limit); !errors.Is(err, ErrInvalid) ||
			!strings.Contains(err.Error(), "limit must be a number from 1 to 10") {
			t.Errorf("limit %d: err = %v", limit, err)
		}
	}
	for _, limit := range []int{1, 10} {
		if _, err := w.e.idx.SearchPlaces(ctx, w.e.spaces, w.alice.id, w.ws, spaces, "cafe", limit); err != nil {
			t.Errorf("limit %d: %v", limit, err)
		}
	}
}
