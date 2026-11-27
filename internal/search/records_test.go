package search

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apps"
)

type recordHitJSON struct {
	SpaceID   int64     `json:"space_id"`
	SpaceName string    `json:"space_name"`
	AppID     int64     `json:"app_id"`
	AppName   string    `json:"app_name"`
	RecordID  int64     `json:"record_id"`
	Snippet   []Segment `json:"snippet"`
}

type recordsReply struct {
	Results    []recordHitJSON `json:"results"`
	NextOffset *int            `json:"next_offset"`
	Incomplete bool            `json:"incomplete"`
}

func (r *recordsReply) texts() []string {
	out := []string{}
	for _, h := range r.Results {
		var b strings.Builder
		for _, s := range h.Snippet {
			b.WriteString(s.Text)
		}
		out = append(out, b.String())
	}
	return out
}

func appsURL(sp access.Space, tail string) string {
	return fmt.Sprintf("/api/workspaces/%d/spaces/%d/apps%s", sp.WorkspaceID, sp.ID, tail)
}

func expenseDefinition(t testing.TB) apps.Definition {
	t.Helper()
	all, err := apps.Examples()
	if err != nil {
		t.Fatal(err)
	}
	for _, ex := range all {
		if ex.Name == "Expense tracker" {
			return ex.Definition
		}
	}
	t.Fatal("no Expense tracker example")
	return apps.Definition{}
}

type appJSON struct {
	ID      int64 `json:"id"`
	Version int64 `json:"version"`
}

func (w *world) newApp(u *user, sp access.Space, name string, publish bool) appJSON {
	w.e.t.Helper()
	rec := u.do(http.MethodPost, appsURL(sp, ""), map[string]any{"name": name, "definition": expenseDefinition(w.e.t)})
	expect(w.e.t, rec, http.StatusCreated)
	a := decode[appJSON](w.e.t, rec)
	if publish {
		rec = u.do(http.MethodPost, appsURL(sp, fmt.Sprintf("/%d/publish", a.ID)), map[string]any{"version": a.Version, "acknowledge": []apps.SchemaChange{}})
		expect(w.e.t, rec, http.StatusOK)
		a = decode[appJSON](w.e.t, rec)
	}
	return a
}

func (w *world) addRecord(u *user, sp access.Space, a appJSON, description string) {
	w.e.t.Helper()
	rec := u.do(http.MethodPost, appsURL(sp, fmt.Sprintf("/%d/actions", a.ID)), apps.ClientAction{
		Name: "add_expense", SurfaceID: apps.SurfaceID(a.ID), SourceComponentID: "add",
		Context: map[string]any{"description": description, "amount": "10"},
	})
	expect(w.e.t, rec, http.StatusOK)
}

func (w *world) rowID(a appJSON, description string) int64 {
	w.e.t.Helper()
	var id int64
	if err := w.e.db.QueryRow(`SELECT id FROM app_rows WHERE app_id = ? AND json_extract(data, '$.description') = ?`, a.ID, description).Scan(&id); err != nil {
		w.e.t.Fatal(err)
	}
	return id
}

func (w *world) setAppRoles(u *user, sp access.Space, a appJSON, grants ...access.AppGrant) {
	w.e.t.Helper()
	expect(w.e.t, u.do(http.MethodPut, appsURL(sp, fmt.Sprintf("/%d/roles", a.ID)), map[string]any{"grants": grants}), http.StatusOK)
}

func roleOf(u *user, roles ...access.AppRole) access.AppGrant {
	id := u.id
	return access.AppGrant{UserID: &id, Roles: roles}
}

func (w *world) searchRecords(u *user, sp *access.Space, q string) *recordsReply {
	w.e.t.Helper()
	rec := u.do(http.MethodGet, w.recordsTarget(sp, q), nil)
	expect(w.e.t, rec, http.StatusOK)
	r := decode[recordsReply](w.e.t, rec)
	return &r
}

func (w *world) recordsTarget(sp *access.Space, q string) string {
	if sp != nil {
		return fmt.Sprintf("/api/workspaces/%d/spaces/%d/search/records?q=%s", sp.WorkspaceID, sp.ID, url.QueryEscape(q))
	}
	return fmt.Sprintf("/api/workspaces/%d/search/records?q=%s", w.ws, url.QueryEscape(q))
}

func wantRecords(t *testing.T, who string, r *recordsReply, want ...string) {
	t.Helper()
	got := r.texts()
	slices.Sort(got)
	slices.Sort(want)
	if len(want) == 0 {
		want = []string{}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s found %q, want %q", who, got, want)
	}
}

func TestRecordSearchIsOnlyOfAppsTheCallerCanRead(t *testing.T) {
	w := newWorld(t)
	team := w.newApp(w.alice, w.team, "Team expenses", true)
	w.addRecord(w.alice, w.team, team, "zanzibar team lunch")
	mine := w.newApp(w.alice, w.private[w.alice], "Alice's expenses", true)
	w.addRecord(w.alice, w.private[w.alice], mine, "zanzibar alice secret")
	theirs := w.newApp(w.bob, w.private[w.bob], "Bob's expenses", true)
	w.addRecord(w.bob, w.private[w.bob], theirs, "zanzibar bob secret")
	hidden := w.newApp(w.owner, w.secret, "Secret expenses", true)
	w.addRecord(w.owner, w.secret, hidden, "zanzibar vault")

	wantRecords(t, "alice", w.searchRecords(w.alice, nil, "zanzibar"), "zanzibar team lunch", "zanzibar alice secret")
	wantRecords(t, "bob", w.searchRecords(w.bob, nil, "zanzibar"), "zanzibar team lunch", "zanzibar bob secret")
	wantRecords(t, "owner", w.searchRecords(w.owner, nil, "zanzibar"), "zanzibar vault", "zanzibar team lunch")
	wantRecords(t, "mallory", w.searchRecords(w.mallory, nil, "zanzibar"))

	hit := w.searchRecords(w.alice, &w.team, "zanzibar").Results
	if len(hit) != 1 || hit[0].SpaceID != w.team.ID || hit[0].SpaceName != "Team" || hit[0].AppID != team.ID ||
		hit[0].AppName != "Team expenses" || hit[0].RecordID != w.rowID(team, "zanzibar team lunch") {
		t.Fatalf("alice's hit = %+v", hit)
	}

	for _, other := range []access.Space{w.private[w.bob], w.secret} {
		expect(t, w.alice.do(http.MethodGet, w.recordsTarget(&other, "zanzibar"), nil), http.StatusNotFound)
	}
	expect(t, w.mallory.do(http.MethodGet, w.recordsTarget(&w.team, "zanzibar"), nil), http.StatusNotFound)

	expect(t, (&user{e: w.e}).do(http.MethodGet, w.recordsTarget(nil, "zanzibar"), nil), http.StatusUnauthorized)
}

func TestRecordSearchFindsFieldValuesOnly(t *testing.T) {
	w := newWorld(t)
	a := w.newApp(w.alice, w.team, "Expenses", true)
	w.addRecord(w.alice, w.team, a, "lunch with the board")
	id := w.rowID(a, "lunch with the board")
	if _, err := w.e.db.Exec(`UPDATE app_rows SET data = json_set(data, '$.retired_note', 'quokka retired', '$.amount', 'quokka text')
		WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	wantRecords(t, "alice", w.searchRecords(w.alice, nil, "lunch"), "lunch with the board")
	for _, q := range []string{"quokka", "description", "amount", "retired_note", "Expenses"} {
		wantRecords(t, "alice for "+q, w.searchRecords(w.alice, nil, q))
	}
}

func TestRecordSearchFollowsTheAppsRoles(t *testing.T) {
	w := newWorld(t)
	w.addMember(w.team, w.mallory, access.RoleEditor)
	a := w.newApp(w.alice, w.team, "Team expenses", true)
	w.setAppRoles(w.alice, w.team, a, roleOf(w.alice, access.AppRoles...), roleOf(w.mallory, access.AppUser), roleOf(w.bob, access.AppReader))
	w.addRecord(w.alice, w.team, a, "zanzibar by alice")
	w.addRecord(w.mallory, w.team, a, "zanzibar by mallory")

	both := []string{"zanzibar by alice", "zanzibar by mallory"}
	wantRecords(t, "alice", w.searchRecords(w.alice, nil, "zanzibar"), both...)
	wantRecords(t, "bob, a reader", w.searchRecords(w.bob, nil, "zanzibar"), both...)
	wantRecords(t, "mallory, a user", w.searchRecords(w.mallory, nil, "zanzibar"), "zanzibar by mallory")
	wantRecords(t, "mallory in the space", w.searchRecords(w.mallory, &w.team, "zanzibar"), "zanzibar by mallory")
	rec := w.mallory.do(http.MethodGet, appsURL(w.team, fmt.Sprintf("/%d/records", a.ID)), nil)
	expect(t, rec, http.StatusOK)
	if body := rec.Body.String(); strings.Contains(body, "by alice") || !strings.Contains(body, "by mallory") {
		t.Fatalf("mallory's list of records: %s", body)
	}

	w.setAppRoles(w.alice, w.team, a, roleOf(w.alice, access.AppRoles...))
	wantRecords(t, "bob without a role", w.searchRecords(w.bob, nil, "zanzibar"))
	wantRecords(t, "mallory without a role", w.searchRecords(w.mallory, nil, "zanzibar"))
	wantRecords(t, "alice", w.searchRecords(w.alice, nil, "zanzibar"), both...)
}

func TestRecordSearchSaysNothingOfRecordsTheCallerCantRead(t *testing.T) {
	w := newWorld(t)
	w.addMember(w.team, w.mallory, access.RoleEditor)
	a := w.newApp(w.alice, w.team, "Team expenses", true)
	w.setAppRoles(w.alice, w.team, a, roleOf(w.alice, access.AppRoles...), roleOf(w.mallory, access.AppUser))
	w.addRecord(w.mallory, w.team, a, "zanzibar by mallory")
	w.searchRecords(w.alice, nil, "zanzibar")

	n := 0
	w.e.idx.afterRecordSync = func() {
		n++
		if _, err := w.e.db.Exec(`INSERT INTO app_rows (app_id, data, created_by) VALUES (?, ?, ?)`,
			a.ID, fmt.Sprintf(`{"description":"zanzibar later %d","amount":1}`, n), w.alice.id); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { w.e.idx.afterRecordSync = nil }()
	if r := w.searchRecords(w.mallory, nil, "zanzibar"); r.Incomplete {
		t.Fatalf("mallory's search was incomplete by records she can't read: %+v", r)
	}
	r := w.searchRecords(w.alice, nil, "zanzibar")
	if !r.Incomplete || r.NextOffset != nil {
		t.Fatalf("alice's search = %+v, want incomplete", r)
	}
	w.e.idx.afterRecordSync = nil
	if r := w.searchRecords(w.alice, nil, "zanzibar"); r.Incomplete || len(r.Results) != 1+n {
		t.Fatalf("alice's later search = %+v, want %d records", r, 1+n)
	}
}

func TestRecordSearchLeavesOutDraftsAndDeletedRecords(t *testing.T) {
	w := newWorld(t)
	draft := w.newApp(w.alice, w.team, "Draft expenses", false)
	if _, err := w.e.db.Exec(`INSERT INTO app_rows (app_id, data, created_by) VALUES (?, ?, ?)`,
		draft.ID, `{"description":"zanzibar in a draft","amount":1}`, w.alice.id); err != nil {
		t.Fatal(err)
	}
	wantRecords(t, "alice", w.searchRecords(w.alice, nil, "zanzibar"))

	rec := w.alice.do(http.MethodPost, appsURL(w.team, fmt.Sprintf("/%d/publish", draft.ID)), map[string]any{"version": draft.Version, "acknowledge": []apps.SchemaChange{}})
	expect(t, rec, http.StatusOK)
	wantRecords(t, "alice after publishing", w.searchRecords(w.alice, nil, "zanzibar"), "zanzibar in a draft")

	id := w.rowID(draft, "zanzibar in a draft")
	if _, err := w.e.db.Exec(`UPDATE app_rows SET deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), revision = revision + 1 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	wantRecords(t, "alice after deleting", w.searchRecords(w.alice, nil, "zanzibar"))
	if _, err := w.e.db.Exec(`UPDATE app_rows SET deleted_at = NULL, revision = revision + 1 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	wantRecords(t, "alice after restoring", w.searchRecords(w.alice, nil, "zanzibar"), "zanzibar in a draft")
	if _, err := w.e.db.Exec(`DELETE FROM app_rows WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	wantRecords(t, "alice after removing", w.searchRecords(w.alice, nil, "zanzibar"))
	var n int
	if err := w.e.db.QueryRow(`SELECT (SELECT count(*) FROM search_records) + (SELECT count(*) FROM search_record_fts)
		+ (SELECT count(*) FROM search_record_queue)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("index rows left = %d, %v", n, err)
	}
}

func TestRecordSearchLeavesOutArchivedApps(t *testing.T) {
	w := newWorld(t)
	a := w.newApp(w.alice, w.team, "Expenses", true)
	w.addRecord(w.alice, w.team, a, "zanzibar lunch")
	wantRecords(t, "alice", w.searchRecords(w.alice, nil, "zanzibar"), "zanzibar lunch")
	if _, err := w.e.db.Exec(`UPDATE apps SET archived_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	wantRecords(t, "alice, archived", w.searchRecords(w.alice, nil, "zanzibar"))
	if _, err := w.e.db.Exec(`UPDATE apps SET archived_at = NULL WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	wantRecords(t, "alice, restored", w.searchRecords(w.alice, nil, "zanzibar"), "zanzibar lunch")
}

func TestRecordSearchFollowsTheLiveRelease(t *testing.T) {
	w := newWorld(t)
	a := w.newApp(w.alice, w.team, "Expenses", true)
	w.addRecord(w.alice, w.team, a, "zanzibar lunch")
	wantRecords(t, "release 1", w.searchRecords(w.alice, nil, "zanzibar"), "zanzibar lunch")

	if _, err := w.e.db.Exec(`INSERT INTO app_releases (app_id, number, definition, definition_version, requires)
		SELECT app_id, 2, json_set(definition, '$.collection.fields', json('[{"name":"amount","type":"number"}]')), definition_version, requires
		FROM app_releases WHERE app_id = ? AND number = 1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.db.Exec(`UPDATE apps SET live_release = 2 WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	wantRecords(t, "release 2", w.searchRecords(w.alice, nil, "zanzibar"))
	if _, err := w.e.db.Exec(`UPDATE apps SET live_release = 1 WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	wantRecords(t, "release 1 again", w.searchRecords(w.alice, nil, "zanzibar"), "zanzibar lunch")
}

func TestRecordSearchFollowsAnAppToItsNewSpace(t *testing.T) {
	w := newWorld(t)
	a := w.newApp(w.alice, w.team, "Team expenses", true)
	w.addRecord(w.alice, w.team, a, "zanzibar lunch")
	wantRecords(t, "bob in the team", w.searchRecords(w.bob, nil, "zanzibar"), "zanzibar lunch")

	move := func(to access.Space) {
		t.Helper()
		var version int64
		if err := w.e.db.QueryRow(`SELECT version FROM apps WHERE id = ?`, a.ID).Scan(&version); err != nil {
			t.Fatal(err)
		}
		expect(t, w.alice.do(http.MethodPost, appsURL(w.team, fmt.Sprintf("/%d/move", a.ID)), map[string]any{"version": version, "space_id": to.ID}), http.StatusOK)
	}
	moved := false
	w.e.idx.afterRecordSync = func() {
		if !moved {
			moved = true
			move(w.private[w.alice])
		}
	}
	defer func() { w.e.idx.afterRecordSync = nil }()
	r := w.searchRecords(w.bob, nil, "zanzibar")
	if len(r.Results) != 0 {
		t.Fatalf("bob found %+v after the app left for alice's private space", r.Results)
	}
	w.e.idx.afterRecordSync = nil
	wantRecords(t, "bob after", w.searchRecords(w.bob, nil, "zanzibar"))
	hit := w.searchRecords(w.alice, nil, "zanzibar").Results
	if len(hit) != 1 || hit[0].SpaceID != w.private[w.alice].ID || hit[0].SpaceName != w.private[w.alice].Name {
		t.Fatalf("alice's hit = %+v, want the app in her private space", hit)
	}
	wantRecords(t, "alice in the team", w.searchRecords(w.alice, &w.team, "zanzibar"))
}

func TestRecordSearchChecksAccessAgainAfterIndexing(t *testing.T) {
	w := newWorld(t)
	a := w.newApp(w.alice, w.team, "Team expenses", true)
	w.addRecord(w.alice, w.team, a, "zanzibar lunch")
	w.searchRecords(w.bob, nil, "zanzibar")
	var spaces []access.Space
	spaces, err := w.e.spaces.Readable(ctx, w.bob.id, w.ws)
	if err != nil {
		t.Fatal(err)
	}
	w.e.idx.afterRecordSync = func() {
		if _, err := w.e.db.Exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, w.team.ID, w.bob.id); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { w.e.idx.afterRecordSync = nil }()
	res, err := w.e.idx.SearchRecords(ctx, w.e.spaces, w.bob.id, w.ws, spaces, Query{Text: "zanzibar"})
	if err != nil || len(res.Results) != 0 {
		t.Fatalf("after bob left the team: %+v, %v", res, err)
	}
}

func TestRecordSearchIsOfOneWorkspace(t *testing.T) {
	w := newWorld(t)
	a := w.newApp(w.alice, w.team, "Team expenses", true)
	w.addRecord(w.alice, w.team, a, "zanzibar here")

	var other int64
	if err := w.e.db.QueryRow(`INSERT INTO workspaces (name) VALUES ('Elsewhere') RETURNING id`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'admin')`, other, w.alice.id); err != nil {
		t.Fatal(err)
	}
	elsewhere := w.alice.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/spaces", other), map[string]string{"name": "There"})
	expect(t, elsewhere, http.StatusCreated)
	there := decode[access.Space](t, elsewhere)
	b := w.newApp(w.alice, there, "Their expenses", true)
	w.addRecord(w.alice, there, b, "zanzibar there")

	wantRecords(t, "alice here", w.searchRecords(w.alice, nil, "zanzibar"), "zanzibar here")
	rec := w.alice.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/search/records?q=zanzibar", other), nil)
	expect(t, rec, http.StatusOK)
	if r := decode[recordsReply](t, rec); fmt.Sprint(r.texts()) != "[zanzibar there]" {
		t.Fatalf("alice there = %+v", r)
	}
	spaces, err := w.e.spaces.Readable(ctx, w.alice.id, other)
	if err != nil || len(spaces) == 0 {
		t.Fatalf("alice's spaces there: %v, %v", spaces, err)
	}
	res, err := w.e.idx.SearchRecords(ctx, w.e.spaces, w.alice.id, w.ws, spaces, Query{Text: "zanzibar"})
	if err != nil || len(res.Results) != 0 {
		t.Fatalf("a space of another workspace was searched: %+v, %v", res, err)
	}
	expect(t, w.mallory.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/search/records?q=zanzibar", other), nil), http.StatusNotFound)
}

func TestRecordSearchPagesNewestChangeFirst(t *testing.T) {
	w := newWorld(t)
	a := w.newApp(w.alice, w.team, "Team expenses", true)
	for i := 1; i <= 3; i++ {
		w.addRecord(w.alice, w.team, a, fmt.Sprintf("zanzibar number %d", i))
	}
	id := w.rowID(a, "zanzibar number 1")
	if _, err := w.e.db.Exec(`UPDATE app_rows SET data = json_set(data, '$.description', 'zanzibar number 1 again'),
		updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '+1 minute') WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	target := fmt.Sprintf("/api/workspaces/%d/search/records?q=zanzibar&limit=2", w.ws)
	first := decode[recordsReply](t, w.alice.do(http.MethodGet, target, nil))
	if fmt.Sprint(first.texts()) != "[zanzibar number 1 again zanzibar number 3]" || first.NextOffset == nil || *first.NextOffset != 2 {
		t.Fatalf("first page = %+v", first)
	}
	second := decode[recordsReply](t, w.alice.do(http.MethodGet, fmt.Sprintf("%s&offset=%d", target, *first.NextOffset), nil))
	if fmt.Sprint(second.texts()) != "[zanzibar number 2]" || second.NextOffset != nil {
		t.Fatalf("second page = %+v", second)
	}
	expect(t, w.alice.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/search/records", w.ws), nil), http.StatusBadRequest)
}

func TestArchivedAppsRecordsAreNotIndexed(t *testing.T) {
	w := newWorld(t)
	a := w.newApp(w.alice, w.team, "Expenses", true)
	w.addRecord(w.alice, w.team, a, "zanzibar lunch")
	wantRecords(t, "alice", w.searchRecords(w.alice, nil, "zanzibar"), "zanzibar lunch")
	indexed := func() int {
		var n int
		if err := w.e.db.QueryRow(`SELECT count(*) FROM search_record_fts`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if indexed() != 1 {
		t.Fatalf("indexed = %d", indexed())
	}
	if _, err := w.e.db.Exec(`UPDATE apps SET archived_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.e.idx.syncRecords(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if indexed() != 0 {
		t.Fatalf("an archived app's records are indexed: %d", indexed())
	}
	if _, err := w.e.db.Exec(`UPDATE apps SET archived_at = NULL WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	wantRecords(t, "alice, restored", w.searchRecords(w.alice, nil, "zanzibar"), "zanzibar lunch")
	if indexed() != 1 {
		t.Fatalf("indexed after restoring = %d", indexed())
	}
}

func TestRecordSearchHasNoOffsetAfterLosingTheSpace(t *testing.T) {
	w := newWorld(t)
	a := w.newApp(w.alice, w.team, "Team expenses", true)
	w.addRecord(w.alice, w.team, a, "zanzibar one")
	w.addRecord(w.alice, w.team, a, "zanzibar two")
	spaces, err := w.e.spaces.Readable(ctx, w.bob.id, w.ws)
	if err != nil {
		t.Fatal(err)
	}
	w.e.idx.afterRecordSync = func() {
		if _, err := w.e.db.Exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, w.team.ID, w.bob.id); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { w.e.idx.afterRecordSync = nil }()
	res, err := w.e.idx.SearchRecords(ctx, w.e.spaces, w.bob.id, w.ws, spaces, Query{Text: "zanzibar", Limit: 1})
	if err != nil || len(res.Results) != 0 || res.NextOffset != nil {
		t.Fatalf("after losing the space: %+v, %v", res, err)
	}
}

func TestRecordSearchLaterPageIsIncompleteWhenAccessChanges(t *testing.T) {
	w := newWorld(t)
	w.addMember(w.team, w.mallory, access.RoleEditor)
	a := w.newApp(w.alice, w.team, "Team expenses", true)
	w.setAppRoles(w.alice, w.team, a, roleOf(w.alice, access.AppRoles...), roleOf(w.mallory, access.AppReader))
	for i := range 3 {
		w.addRecord(w.alice, w.team, a, fmt.Sprintf("zanzibar %d", i))
	}
	for i := range 2 {
		if _, err := w.e.db.Exec(`INSERT INTO app_rows (app_id, data, created_by) VALUES (?, ?, ?)`,
			a.ID, fmt.Sprintf(`{"description":"zanzibar mine %d","amount":1}`, i), w.mallory.id); err != nil {
			t.Fatal(err)
		}
	}
	target := w.recordsTarget(nil, "zanzibar") + "&limit=2"
	first := decode[recordsReply](t, w.mallory.do(http.MethodGet, target, nil))
	if len(first.Results) != 2 || first.NextOffset == nil || first.Incomplete {
		t.Fatalf("first page = %+v", first)
	}
	w.e.idx.afterRecordSync = func() {
		if _, err := w.e.db.Exec(`UPDATE app_roles SET role = 'user' WHERE app_id = ? AND user_id = ?`, a.ID, w.mallory.id); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { w.e.idx.afterRecordSync = nil }()
	second := decode[recordsReply](t, w.mallory.do(http.MethodGet, fmt.Sprintf("%s&offset=%d", target, *first.NextOffset), nil))
	if !second.Incomplete || len(second.Results) != 0 || second.NextOffset != nil {
		t.Fatalf("second page after her access changed = %+v", second)
	}
}

func TestRecordSyncStopsAtItsLimit(t *testing.T) {
	w := newWorld(t)
	a := w.newApp(w.alice, w.team, "Team expenses", true)
	for i := range 5 {
		w.addRecord(w.alice, w.team, a, fmt.Sprintf("zanzibar %d", i))
	}
	queued := func() int {
		var n int
		if err := w.e.db.QueryRow(`SELECT count(*) FROM search_record_queue`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if queued() != 5 {
		t.Fatalf("queued = %d", queued())
	}
	if n, err := w.e.idx.syncRecordsWithin(ctx, fmt.Sprintf("[%d]", a.ID), 2); err != nil || n != 2 {
		t.Fatalf("indexed %d, %v", n, err)
	}
	if queued() != 3 {
		t.Fatalf("queued after a sync of 2 = %d, want 3", queued())
	}
	if err := w.e.idx.syncRecords(ctx, ""); err != nil || queued() != 0 {
		t.Fatalf("after the background sync: %d, %v", queued(), err)
	}
}

func TestRecordSearchSharesItsAllowanceAcrossAttempts(t *testing.T) {
	w := newWorld(t)
	a := w.newApp(w.alice, w.team, "Team expenses", true)
	for i := range 6 {
		w.addRecord(w.alice, w.team, a, fmt.Sprintf("zanzibar %d", i))
	}
	w.e.idx.recordSyncLimit = 4
	r := w.searchRecords(w.alice, nil, "zanzibar")
	var queued int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM search_record_queue`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 2 || !r.Incomplete || len(r.Results) != 4 {
		t.Fatalf("queued %d, reply %+v: want 2 left over, 4 found, incomplete", queued, r)
	}
	if r := w.searchRecords(w.alice, nil, "zanzibar"); r.Incomplete || len(r.Results) != 6 {
		t.Fatalf("next search = %+v", r)
	}
}

func TestRecordSearchLooksAgainWhenTheCallersAccessChanges(t *testing.T) {
	w := newWorld(t)
	w.addMember(w.team, w.mallory, access.RoleEditor)
	a := w.newApp(w.alice, w.team, "Team expenses", true)
	w.setAppRoles(w.alice, w.team, a, roleOf(w.alice, access.AppRoles...), roleOf(w.mallory, access.AppReader))
	if _, err := w.e.db.Exec(`INSERT INTO app_rows (app_id, data, created_by) VALUES (?, ?, ?)`,
		a.ID, `{"description":"zanzibar by mallory","amount":1}`, w.mallory.id); err != nil {
		t.Fatal(err)
	}
	w.addRecord(w.alice, w.team, a, "zanzibar by alice")
	wantRecords(t, "mallory, a reader", w.searchRecords(w.mallory, nil, "zanzibar"), "zanzibar by alice", "zanzibar by mallory")

	n := 0
	w.e.idx.afterRecordSync = func() {
		n++
		if n == 1 {
			if _, err := w.e.db.Exec(`UPDATE app_roles SET role = 'user' WHERE app_id = ? AND user_id = ?`, a.ID, w.mallory.id); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := w.e.db.Exec(`INSERT INTO app_rows (app_id, data, created_by) VALUES (?, ?, ?)`,
			a.ID, fmt.Sprintf(`{"description":"zanzibar later %d","amount":1}`, n), w.alice.id); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { w.e.idx.afterRecordSync = nil }()
	r := w.searchRecords(w.mallory, nil, "zanzibar")
	if r.Incomplete {
		t.Fatalf("her search says records she no longer reads are changing: %+v", r)
	}
	wantRecords(t, "mallory, now a user", r, "zanzibar by mallory")
}

func TestRecordSearchLeavesOutAnAppArchivedWhileIndexing(t *testing.T) {
	w := newWorld(t)
	a := w.newApp(w.alice, w.team, "Team expenses", true)
	w.addRecord(w.alice, w.team, a, "zanzibar lunch")
	b := w.newApp(w.alice, w.team, "Team travel", true)
	w.addRecord(w.alice, w.team, b, "zanzibar flights")
	wantRecords(t, "before", w.searchRecords(w.alice, nil, "zanzibar"), "zanzibar lunch", "zanzibar flights")

	n := 0
	w.e.idx.afterRecordSync = func() {
		n++
		if _, err := w.e.db.Exec(`UPDATE apps SET archived_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`, a.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := w.e.db.Exec(`INSERT INTO app_rows (app_id, data, created_by) VALUES (?, ?, ?)`,
			b.ID, fmt.Sprintf(`{"description":"zanzibar later %d","amount":1}`, n), w.alice.id); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { w.e.idx.afterRecordSync = nil }()
	r := w.searchRecords(w.alice, nil, "zanzibar")
	for _, hit := range r.Results {
		if hit.AppID == a.ID {
			t.Fatalf("a record of an archived app was returned: %+v", r)
		}
	}
	if !r.Incomplete {
		t.Fatalf("a search that was behind on its last attempt = %+v, want incomplete", r)
	}
}
