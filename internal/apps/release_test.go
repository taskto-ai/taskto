package apps

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
)

func (w *world) patchDraft(u *user, s access.Space, a App, def Definition) *httptest.ResponseRecorder {
	w.e.t.Helper()
	return u.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": a.Version, "definition": def})
}

func (w *world) mustPatchDraft(u *user, s access.Space, a App, def Definition) App {
	w.e.t.Helper()
	rec := w.patchDraft(u, s, a, def)
	expect(w.e.t, rec, http.StatusOK)
	return decode[App](w.e.t, rec)
}

func (w *world) tryPublish(u *user, s access.Space, a App, ack ...SchemaChange) *httptest.ResponseRecorder {
	w.e.t.Helper()
	if ack == nil {
		ack = []SchemaChange{}
	}
	return u.do(http.MethodPost, appPath(s, a.ID, "/publish"), map[string]any{"version": a.Version, "acknowledge": ack})
}

func (w *world) tryRollback(u *user, s access.Space, a App, release int64) *httptest.ResponseRecorder {
	w.e.t.Helper()
	return u.do(http.MethodPost, appPath(s, a.ID, "/rollback"), map[string]any{"version": a.Version, "release": release})
}

func (w *world) surfaceBody(u *user, s access.Space, a App, query string) string {
	w.e.t.Helper()
	rec := u.do(http.MethodGet, appPath(s, a.ID, "/surface"+query), nil)
	expect(w.e.t, rec, http.StatusOK)
	return rec.Body.String()
}

func with(def Definition, f Field) Definition {
	def.Collection.Fields = append(slices.Clone(def.Collection.Fields), f)
	return def
}

func without(def Definition, name string) Definition {
	def.Collection.Fields = slices.DeleteFunc(slices.Clone(def.Collection.Fields), func(f Field) bool { return f.Name == name })
	return def
}

func fieldsOf(def Definition, name string, change func(*Field)) Definition {
	def.Collection.Fields = slices.Clone(def.Collection.Fields)
	for i := range def.Collection.Fields {
		if def.Collection.Fields[i].Name == name {
			change(&def.Collection.Fields[i])
		}
	}
	return def
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	return decode[struct{ Code string }](t, rec).Code
}

func TestDraftEditsLeaveTheLiveAppUnchanged(t *testing.T) {
	w := newWorld(t)
	s, a := w.alicePrivate, w.aliceApp
	before := w.surfaceBody(w.alice, s, a, "")
	live := w.view(w.alice, s, a)
	if live.Live == nil || live.Live.Number != 1 || !live.Live.Live || live.UnpublishedChanges || live.Draft == nil {
		t.Fatalf("a published app = %+v", live)
	}

	def := *live.Draft
	def.Components = append(slices.Clone(def.Components), []byte(`{"id":"note","component":"Text","text":"Draft only"}`))
	def.Components[0] = []byte(`{"id":"root","component":"Column","children":["title","total","items","form","note"]}`)
	edited := w.mustPatchDraft(w.alice, s, live, def)
	if !edited.UnpublishedChanges || edited.Live.Number != 1 || edited.Version != live.Version+1 {
		t.Fatalf("after editing = %+v", edited)
	}
	if got := w.surfaceBody(w.alice, s, a, ""); got != before {
		t.Fatalf("a draft edit changed the launched app:\n%s\nwas\n%s", got, before)
	}
	if !sameDefinition(*edited.Live.Definition, *live.Live.Definition) || sameDefinition(*edited.Draft, *edited.Live.Definition) {
		t.Fatal("the live definition changed, or the draft didn't")
	}
	if got := w.surfaceBody(w.alice, s, a, "?release=draft"); !strings.Contains(got, "Draft only") || strings.Contains(before, "Draft only") {
		t.Fatalf("the draft's surface = %s", got)
	}
	list := decode[struct{ Apps []App }](t, w.alice.do(http.MethodGet, appsPath(s, ""), nil)).Apps
	if len(list) != 1 || !list[0].UnpublishedChanges || list[0].Live == nil || list[0].Live.Number != 1 || list[0].Live.Definition != nil || list[0].Draft != nil {
		t.Fatalf("listed = %+v", list)
	}
	rec := w.alice.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": edited.Version, "name": "Renamed"})
	expect(t, rec, http.StatusOK)
	if got := decode[App](t, rec); got.Live.Number != 1 || !got.UnpublishedChanges {
		t.Fatalf("after renaming = %+v", got)
	}
	back := w.mustPatchDraft(w.alice, s, w.view(w.alice, s, a), *live.Live.Definition)
	if back.UnpublishedChanges {
		t.Fatal("a draft equal to the live release is reported as a change")
	}
	expect(t, w.tryPublish(w.alice, s, back), http.StatusConflict)

	edited = w.mustPatchDraft(w.alice, s, back, def)
	rec = w.tryPublish(w.alice, s, edited)
	expect(t, rec, http.StatusOK)
	published := decode[App](t, rec)
	if published.Live.Number != 2 || published.UnpublishedChanges || !sameDefinition(*published.Live.Definition, def) {
		t.Fatalf("published = %+v", published)
	}
	if got := w.surfaceBody(w.alice, s, a, ""); !strings.Contains(got, "Draft only") {
		t.Fatal("the new release isn't what is launched")
	}
	if got := w.surfaceBody(w.alice, s, a, "?release=1"); got != before {
		t.Fatalf("release 1 is drawn differently than when it was live:\n%s", got)
	}
	expect(t, w.alice.do(http.MethodGet, appPath(s, a.ID, "/surface?release=nope"), nil), http.StatusBadRequest)
	expect(t, w.alice.do(http.MethodGet, appPath(s, a.ID, "/surface?release=9"), nil), http.StatusNotFound)
	one := decode[Release](t, w.alice.do(http.MethodGet, appPath(s, a.ID, "/releases/1"), nil))
	if one.Live || !sameDefinition(*one.Definition, *live.Live.Definition) {
		t.Fatalf("release 1 = %+v", one)
	}
	list2 := decode[struct{ Releases []Release }](t, w.alice.do(http.MethodGet, appPath(s, a.ID, "/releases"), nil)).Releases
	if len(list2) != 2 || list2[0].Number != 2 || !list2[0].Live || list2[1].Number != 1 || list2[1].Live || list2[0].Definition != nil {
		t.Fatalf("history = %+v", list2)
	}
}

func TestANewAppIsADraftUntilPublished(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.draft(w.alice, s, "Fresh")
	if a.Live != nil || !a.UnpublishedChanges || a.Draft == nil {
		t.Fatalf("a new app = %+v", a)
	}
	rec := w.alice.do(http.MethodGet, appPath(s, a.ID, "/surface"), nil)
	expect(t, rec, http.StatusConflict)
	if errorCode(t, rec) != "app_not_published" {
		t.Fatalf("launch = %s", rec.Body)
	}
	expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "add_expense", map[string]any{"description": "x", "amount": "1"})), http.StatusConflict)
	w.surfaceBody(w.alice, s, a, "?release=draft")
	expect(t, w.tryRollback(w.alice, s, a, 1), http.StatusNotFound)
	a = w.publish(w.alice, s, a)
	w.add(w.alice, s, a, "first", "1")
}

func TestOnlyPublishersPublishAndRollBack(t *testing.T) {
	w := newWorld(t)
	s, a := w.shared, w.sharedApp
	w.mustSetRoles(w.alice, s, a, grant(w.owner.id, access.AppBuilder), grant(w.alice.id, access.AppRoles...), grant(w.bob.id, access.AppPublisher, access.AppUser))

	ownersView := w.view(w.owner, s, a)
	draft := w.mustPatchDraft(w.owner, s, ownersView, without(*ownersView.Draft, "description"))
	expectForbidden(t, w.tryPublish(w.owner, s, draft, SchemaChange{ChangeFieldRemoved, "description"}), "publish")
	expectForbidden(t, w.tryRollback(w.owner, s, draft, 1), "publish")
	rec := w.tryPublish(w.bob, s, draft, SchemaChange{ChangeFieldRemoved, "description"})
	expect(t, rec, http.StatusForbidden)
	if errorCode(t, rec) != "space_read_only" {
		t.Fatalf("a viewer publishing = %s", rec.Body)
	}
	expect(t, w.tryRollback(w.bob, s, draft, 1), http.StatusForbidden)
	expect(t, w.tryPublish(w.mallory, s, draft), http.StatusNotFound)
	expect(t, w.tryRollback(w.mallory, s, draft, 1), http.StatusNotFound)
	expect(t, w.tryPublish(w.stranger, in(w.other, s), draft), http.StatusNotFound)

	if got := w.view(w.alice, s, a); got.Live.Number != 1 {
		t.Fatalf("a refused publish published: live = %d", got.Live.Number)
	}
	draft = w.view(w.alice, s, a)
	expect(t, w.tryPublish(w.alice, s, draft, SchemaChange{ChangeFieldRemoved, "description"}), http.StatusOK)
	if got := w.view(w.alice, s, a); got.Live.Number != 2 {
		t.Fatalf("live = %d", got.Live.Number)
	}
	expect(t, w.tryRollback(w.alice, s, w.view(w.alice, s, a), 1), http.StatusOK)
}

func TestAppUsersCantEditDefinitions(t *testing.T) {
	w := newWorld(t)
	s, a := w.shared, w.sharedApp
	w.mustSetRoles(w.alice, s, a, grant(w.owner.id, access.AppUser), grant(w.alice.id, access.AppRoles...))
	alice := w.view(w.alice, s, a)
	draft := w.mustPatchDraft(w.alice, s, alice, with(*alice.Draft, Field{Name: "category", Type: TypeString}))

	user := w.view(w.owner, s, a)
	if user.Draft != nil || user.UnpublishedChanges || user.Live == nil || user.Live.Definition == nil || sameDefinition(*user.Live.Definition, *draft.Draft) {
		t.Fatalf("a user sees the draft or doesn't see the live definition: %+v", user)
	}
	if strings.Contains(w.surfaceBody(w.owner, s, a, ""), "category") {
		t.Fatal("a user's launch draws the draft")
	}
	expectForbidden(t, w.patchDraft(w.owner, s, user, *draft.Draft), "design")
	expectForbidden(t, w.owner.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": user.Version, "name": "Mine"}), "design")
	expectForbidden(t, w.owner.do(http.MethodGet, appPath(s, a.ID, "/surface?release=draft"), nil), "design")
	expectForbidden(t, w.owner.do(http.MethodGet, appPath(s, a.ID, "/surface?release=2"), nil), "design")
	expectForbidden(t, w.owner.do(http.MethodGet, appPath(s, a.ID, "/releases"), nil), "design")
	expectForbidden(t, w.owner.do(http.MethodGet, appPath(s, a.ID, "/releases/1"), nil), "design")
	w.surfaceBody(w.owner, s, a, "?release=1")
	w.add(w.owner, s, a, "mine", "5")
	if got := w.view(w.alice, s, a); !got.UnpublishedChanges {
		t.Fatal("the draft was lost")
	}
}

func TestSchemaChangesNeedAnExplicitMigration(t *testing.T) {
	type tc struct {
		name   string
		edit   func(Definition) Definition
		change SchemaChange
	}
	for _, tt := range []tc{
		{"a field with values is removed", func(d Definition) Definition { return without(d, "description") }, SchemaChange{ChangeFieldRemoved, "description"}},
		{"a field's type changes", func(d Definition) Definition {
			return fieldsOf(d, "description", func(f *Field) { f.Type, f.MaxLength = TypeNumber, 0 })
		}, SchemaChange{ChangeFieldTypeChanged, "description"}},
		{"a new field is required and rows lack it", func(d Definition) Definition {
			return with(d, Field{Name: "category", Type: TypeString, Required: true})
		}, SchemaChange{ChangeFieldRequired, "category"}},
		{"an optional field becomes required and rows lack it", func(d Definition) Definition {
			return fieldsOf(d, "note", func(f *Field) { f.Required = true })
		}, SchemaChange{ChangeFieldRequired, "note"}},
		{"a limit refuses a stored value", func(d Definition) Definition {
			return fieldsOf(d, "description", func(f *Field) { f.MaxLength = 3 })
		}, SchemaChange{ChangeFieldNarrowed, "description"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			s, a := w.alicePrivate, w.aliceApp
			if tt.change.Field == "note" {
				cur := w.view(w.alice, s, a)
				cur = w.mustPatchDraft(w.alice, s, cur, with(*cur.Draft, Field{Name: "note", Type: TypeString}))
				a = decode[App](t, w.tryPublish(w.alice, s, cur))
			}
			rowsBefore := storedRows(t, w.e.db, a.ID)
			cur := w.view(w.alice, s, a)
			cur = w.mustPatchDraft(w.alice, s, cur, tt.edit(*cur.Draft))

			rec := w.tryPublish(w.alice, s, cur)
			expect(t, rec, http.StatusConflict)
			reply := decode[struct {
				Code   string
				Params struct{ Changes []SchemaChange }
			}](t, rec)
			if reply.Code != "app_migration_required" || !slices.Equal(reply.Params.Changes, []SchemaChange{tt.change}) {
				t.Fatalf("refusal = %s", rec.Body)
			}
			if got := w.view(w.alice, s, a); got.Live.Number != cur.Live.Number || !got.UnpublishedChanges {
				t.Fatalf("a refused publish changed the app: %+v", got)
			}
			other := SchemaChange{ChangeFieldRemoved, "amount"}
			expect(t, w.tryPublish(w.alice, s, cur, other), http.StatusConflict)
			rec = w.tryPublish(w.alice, s, cur, tt.change, other)
			expect(t, rec, http.StatusConflict)
			if errorCode(t, rec) != "app_migration_unneeded" {
				t.Fatalf("an acknowledgement of a change that isn't made = %s", rec.Body)
			}
			for _, bad := range [][]SchemaChange{
				{{"field_exploded", tt.change.Field}}, {{tt.change.Change, "Not A Name"}}, {tt.change, tt.change},
			} {
				rec = w.tryPublish(w.alice, s, cur, bad...)
				expect(t, rec, http.StatusBadRequest)
				if errorCode(t, rec) != "app_migration_invalid" {
					t.Fatalf("%v = %s", bad, rec.Body)
				}
			}
			expect(t, w.tryPublish(w.alice, s, cur, tt.change), http.StatusOK)
			if got := storedRows(t, w.e.db, a.ID); !slices.Equal(got, rowsBefore) {
				t.Fatalf("rows = %v, were %v", got, rowsBefore)
			}
		})
	}
}

func TestChangesNoRowIsAffectedByNeedNoMigration(t *testing.T) {
	w := newWorld(t)
	s, a := w.alicePrivate, w.aliceApp
	cur := w.view(w.alice, s, a)
	def := with(*cur.Draft, Field{Name: "category", Type: TypeString})
	def = with(def, Field{Name: "paid", Type: TypeBoolean})
	def = fieldsOf(def, "description", func(f *Field) { f.MaxLength = 500 })
	cur = w.mustPatchDraft(w.alice, s, cur, def)
	cur = decode[App](t, w.tryPublish(w.alice, s, cur))
	if cur.Live.Number != 2 {
		t.Fatalf("live = %d", cur.Live.Number)
	}
	cur = w.mustPatchDraft(w.alice, s, cur, fieldsOf(without(*cur.Draft, "category"), "paid", func(f *Field) { f.Type = TypeString }))
	expect(t, w.tryPublish(w.alice, s, cur), http.StatusOK)
	cur = w.view(w.alice, s, a)
	cur = w.mustPatchDraft(w.alice, s, cur, fieldsOf(*cur.Draft, "paid", func(f *Field) { f.Required = true }))
	rec := w.tryPublish(w.alice, s, cur)
	expect(t, rec, http.StatusConflict)
	if errorCode(t, rec) != "app_migration_required" {
		t.Fatalf("a required field the row lacks = %s", rec.Body)
	}
}

func TestRollbackNeverChangesData(t *testing.T) {
	w := newWorld(t)
	s, a := w.alicePrivate, w.aliceApp
	cur := w.view(w.alice, s, a)
	cur = w.mustPatchDraft(w.alice, s, cur, with(*cur.Draft, Field{Name: "category", Type: TypeString}))
	cur = decode[App](t, w.tryPublish(w.alice, s, cur))
	expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"),
		action(a, "add_expense", map[string]any{"description": "with category", "amount": "7", "category": "food"})), http.StatusOK)
	w.add(w.alice, s, a, "plain", "8")
	rowsBefore := storedRows(t, w.e.db, a.ID)
	if len(rowsBefore) != 3 {
		t.Fatalf("rows = %v", rowsBefore)
	}

	rec := w.tryRollback(w.alice, s, w.view(w.alice, s, a), 1)
	expect(t, rec, http.StatusOK)
	rolled := decode[App](t, rec)
	if rolled.Live.Number != 1 || !rolled.Live.Live || rolled.Version <= cur.Version {
		t.Fatalf("rolled back = %+v", rolled)
	}
	if got := storedRows(t, w.e.db, a.ID); !slices.Equal(got, rowsBefore) {
		t.Fatalf("rows after the rollback = %v, were %v", got, rowsBefore)
	}
	if got := w.rowsOf(w.alice, s, a); len(got) != 3 {
		t.Fatalf("rows shown = %v", got)
	}
	for _, it := range w.items(w.alice, s, a) {
		if _, shown := it["category"]; shown {
			t.Fatalf("release 1 shows a field it doesn't declare: %v", it)
		}
	}
	if !rolled.UnpublishedChanges || sameDefinition(*rolled.Draft, *rolled.Live.Definition) {
		t.Fatalf("the draft after the rollback = %+v", rolled)
	}
	again := decode[App](t, w.tryPublish(w.alice, s, rolled))
	if again.Live.Number != 3 {
		t.Fatalf("published after a rollback = release %d", again.Live.Number)
	}
	if got := storedRows(t, w.e.db, a.ID); !slices.Equal(got, rowsBefore) {
		t.Fatalf("rows after publishing again = %v", got)
	}

	expect(t, w.tryRollback(w.alice, s, w.view(w.alice, s, a), 2), http.StatusOK)
	hist := decode[struct{ Releases []Release }](t, w.alice.do(http.MethodGet, appPath(s, a.ID, "/releases"), nil)).Releases
	if len(hist) != 3 || !hist[1].Live || hist[0].Live || hist[2].Live {
		t.Fatalf("history = %+v", hist)
	}
	rec = w.tryRollback(w.alice, s, w.view(w.alice, s, a), 2)
	expect(t, rec, http.StatusConflict)
	if errorCode(t, rec) != "app_release_already_live" {
		t.Fatalf("rolling back to the live release = %s", rec.Body)
	}
	rec = w.tryRollback(w.alice, s, w.view(w.alice, s, a), 9)
	expect(t, rec, http.StatusNotFound)
	if errorCode(t, rec) != "app_release_not_found" {
		t.Fatalf("rolling back to a release that isn't = %s", rec.Body)
	}
	expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/rollback"), map[string]any{"version": w.view(w.alice, s, a).Version}), http.StatusBadRequest)
	stale := w.view(w.alice, s, a)
	stale.Version--
	rec = w.tryRollback(w.alice, s, stale, 1)
	expect(t, rec, http.StatusConflict)
	if errorCode(t, rec) != "app_changed" {
		t.Fatalf("stale = %s", rec.Body)
	}
}

func TestRollbackRefusesAReleaseTheRowsDontFit(t *testing.T) {
	w := newWorld(t)
	s, a := w.alicePrivate, w.aliceApp
	cur := w.view(w.alice, s, a)
	next := with(without(*cur.Draft, "amount"), Field{Name: "category", Type: TypeString, Required: true})
	next.Summaries = nil
	cur = w.mustPatchDraft(w.alice, s, cur, next)
	cur = decode[App](t, w.tryPublish(w.alice, s, cur, SchemaChange{ChangeFieldRemoved, "amount"}, SchemaChange{ChangeFieldRequired, "category"}))
	if _, err := w.e.db.Exec(`UPDATE app_rows SET data = json('{"description":"x","category":"c"}') WHERE app_id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	rec := w.tryRollback(w.alice, s, cur, 1)
	expect(t, rec, http.StatusConflict)
	reply := decode[struct {
		Code   string
		Params struct{ Fields []string }
	}](t, rec)
	if reply.Code != "app_release_incompatible" || !slices.Equal(reply.Params.Fields, []string{"amount"}) {
		t.Fatalf("refusal = %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), `"x"`) || strings.Contains(rec.Body.String(), "row") {
		t.Fatalf("the refusal tells of rows: %s", rec.Body)
	}
	if got := w.view(w.alice, s, a); got.Live.Number != 2 {
		t.Fatalf("a refused rollback moved the app to release %d", got.Live.Number)
	}
}

func storedRows(t *testing.T, q *sql.DB, appID int64) []string {
	t.Helper()
	rows, err := q.Query(`SELECT id || ':' || data FROM app_rows WHERE app_id = ? ORDER BY id`, appID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func plainApp(t *testing.T) Definition {
	t.Helper()
	raw := func(items ...string) []json.RawMessage {
		var out []json.RawMessage
		for _, i := range items {
			out = append(out, json.RawMessage(i))
		}
		return out
	}
	def, err := Definition{
		Components: raw(
			`{"id":"root","component":"Column","children":["list","description","add"]}`,
			`{"id":"list","component":"List","children":{"componentId":"line","path":"/items"}}`,
			`{"id":"line","component":"Text","text":{"path":"description"}}`,
			`{"id":"description","component":"TextField","label":"Description","value":{"path":"/draft/description"}}`,
			`{"id":"add-label","component":"Text","text":"Add"}`,
			`{"id":"add","component":"Button","child":"add-label","variant":"primary","action":{"event":{"name":"add_expense","context":{"description":{"path":"/draft/description"}}}}}`,
		),
		Collection: Collection{Fields: []Field{
			{Name: "description", Type: TypeString, Required: true, MaxLength: 200},
			{Name: "amount", Type: TypeString},
			{Name: "category", Type: TypeString, Required: true},
		}},
		Actions: map[string]Action{"add_expense": {Kind: ActionInsert}},
	}.Check()
	if err != nil {
		t.Fatal(err)
	}
	return def
}

func TestActionsNameTheirRelease(t *testing.T) {
	w := newWorld(t)
	s, a := w.alicePrivate, w.aliceApp
	act := func(release *int64, name string, ctx map[string]any) *httptest.ResponseRecorder {
		w.e.t.Helper()
		ca := action(a, name, ctx)
		ca.Release = release
		return w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), ca)
	}
	rel := func(n int64) *int64 { return &n }
	add := map[string]any{"description": "from an old screen", "amount": "2"}

	cur := w.view(w.alice, s, a)
	cur = w.mustPatchDraft(w.alice, s, cur, with(*cur.Draft, Field{Name: "category", Type: TypeString}))
	cur = decode[App](t, w.tryPublish(w.alice, s, cur))
	expect(t, act(rel(1), "add_expense", add), http.StatusOK)
	expect(t, act(nil, "add_expense", add), http.StatusOK)
	expect(t, act(rel(2), "add_expense", add), http.StatusOK)

	rec := act(rel(1), "add_expense", add)
	expect(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), "category") {
		t.Fatalf("release 1 is drawn with release 2's field: %s", rec.Body)
	}
	rec = act(rel(7), "add_expense", add)
	expect(t, rec, http.StatusNotFound)
	if errorCode(t, rec) != "app_release_not_found" {
		t.Fatalf("an unknown release = %s", rec.Body)
	}

	cur = w.view(w.alice, s, a)
	cur = w.mustPatchDraft(w.alice, s, cur, fieldsOf(*cur.Draft, "category", func(f *Field) { f.Required = true }))
	rec = w.tryPublish(w.alice, s, cur)
	expect(t, rec, http.StatusConflict)
	cur = decode[App](t, w.tryPublish(w.alice, s, cur, SchemaChange{ChangeFieldRequired, "category"}))
	before := storedRows(t, w.e.db, a.ID)
	for _, from := range []*int64{rel(1), rel(2)} {
		rec = act(from, "add_expense", add)
		expect(t, rec, http.StatusConflict)
		reply := decode[struct {
			Code   string
			Params map[string]any
		}](t, rec)
		if reply.Code != "app_release_outdated" || reply.Params["live"] != float64(3) || reply.Params["release"] != float64(*from) {
			t.Fatalf("an insert of release %d = %s", *from, rec.Body)
		}
	}
	expect(t, act(rel(3), "add_expense", map[string]any{"description": "d", "amount": "1", "category": "c"}), http.StatusOK)
	rec = act(nil, "add_expense", add)
	expect(t, rec, http.StatusUnprocessableEntity)
	if errorCode(t, rec) != "app_field_required" {
		t.Fatalf("live insert without the category = %s", rec.Body)
	}
	if got := storedRows(t, w.e.db, a.ID); len(got) != len(before)+1 {
		t.Fatalf("rows = %v after one accepted insert, were %v", got, before)
	}
	row := w.items(w.alice, s, a)[0]["id"]
	expect(t, act(rel(1), "delete_expense", map[string]any{"id": row}), http.StatusOK)

	cur = w.view(w.alice, s, a)
	cur = w.mustPatchDraft(w.alice, s, cur, plainApp(t))
	cur = decode[App](t, w.tryPublish(w.alice, s, cur, SchemaChange{ChangeFieldTypeChanged, "amount"}))
	row = w.items(w.alice, s, a)[0]["id"]
	for _, tt := range []struct {
		name string
		ctx  map[string]any
	}{
		{"add_expense", map[string]any{"description": "d", "amount": "1", "category": "c"}},
		{"delete_expense", map[string]any{"id": row}},
	} {
		rec = act(rel(3), tt.name, tt.ctx)
		expect(t, rec, http.StatusConflict)
		if errorCode(t, rec) != "app_release_outdated" {
			t.Fatalf("%s from release 3 = %s", tt.name, rec.Body)
		}
	}
	for _, it := range w.items(w.alice, s, a) {
		if v, shown := it["amount"]; shown {
			t.Fatalf("a number is shown as the text field's value: %v", v)
		}
	}
}

func TestConcurrentDraftEditsConflict(t *testing.T) {
	w := newWorld(t)
	s, a := w.shared, w.sharedApp
	cur := w.view(w.alice, s, a)
	first := w.mustPatchDraft(w.alice, s, cur, with(*cur.Draft, Field{Name: "category", Type: TypeString}))
	rec := w.patchDraft(w.owner, s, cur, with(*cur.Draft, Field{Name: "paid", Type: TypeBoolean}))
	expect(t, rec, http.StatusConflict)
	reply := decode[struct {
		Code    string
		Current App
	}](t, rec)
	if reply.Code != "app_changed" || reply.Current.Version != first.Version || reply.Current.Draft == nil ||
		!sameDefinition(*reply.Current.Draft, *first.Draft) {
		t.Fatalf("conflict = %s", rec.Body)
	}
	rec = w.tryPublish(w.owner, s, cur)
	expect(t, rec, http.StatusConflict)
	if errorCode(t, rec) != "app_changed" {
		t.Fatalf("publishing a stale draft = %s", rec.Body)
	}
	if got := w.view(w.owner, s, a); got.Live.Number != 1 || !sameDefinition(*got.Draft, *first.Draft) {
		t.Fatalf("the app = %+v", got)
	}
	cur = w.view(w.alice, s, a)
	codes := make(chan int, 2)
	for i, u := range []*user{w.alice, w.owner} {
		go func() {
			codes <- w.patchDraft(u, s, cur, with(*cur.Draft, Field{Name: fmt.Sprintf("extra%d", i), Type: TypeString})).Code
		}()
	}
	got := []int{<-codes, <-codes}
	slices.Sort(got)
	if got[0] != http.StatusOK || got[1] != http.StatusConflict {
		t.Fatalf("concurrent edits answered %v", got)
	}
}

func TestReleasesStayInTheirSpace(t *testing.T) {
	w := newWorld(t)
	a, s := w.sharedApp, w.shared
	cur := w.view(w.alice, s, a)
	cur = w.mustPatchDraft(w.alice, s, cur, with(*cur.Draft, Field{Name: "category", Type: TypeString}))

	extra := w.e.createShared(w.owner, w.ws, "Extra")
	w.e.addMember(w.owner, extra, w.alice, access.RoleEditor)
	for _, wrong := range []access.Space{w.alicePrivate, extra} {
		for _, r := range []struct{ method, path string }{
			{http.MethodPost, appPath(wrong, a.ID, "/publish")},
			{http.MethodPost, appPath(wrong, a.ID, "/rollback")},
			{http.MethodGet, appPath(wrong, a.ID, "/releases")},
			{http.MethodGet, appPath(wrong, a.ID, "/releases/1")},
			{http.MethodGet, appPath(wrong, a.ID, "/surface?release=draft")},
		} {
			var body any
			if strings.HasSuffix(r.path, "/publish") {
				body = map[string]any{"version": cur.Version, "acknowledge": []SchemaChange{}}
			} else if r.method == http.MethodPost {
				body = map[string]any{"version": cur.Version, "release": 1}
			}
			expect(t, w.alice.do(r.method, r.path, body), http.StatusNotFound)
		}
	}
	for _, r := range []struct{ method, path string }{
		{http.MethodPost, appPath(in(w.other, s), a.ID, "/publish")},
		{http.MethodGet, appPath(in(w.other, s), a.ID, "/releases")},
		{http.MethodGet, appPath(w.otherShared, a.ID, "/releases")},
		{http.MethodGet, appPath(in(w.other, s), a.ID, "/surface?release=draft")},
	} {
		var body any
		if r.method == http.MethodPost {
			body = map[string]any{"version": cur.Version, "acknowledge": []SchemaChange{}}
		}
		expect(t, w.stranger.do(r.method, r.path, body), http.StatusNotFound)
	}
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, appPath(s, a.ID, "/releases")},
		{http.MethodGet, appPath(s, a.ID, "/surface?release=draft")},
		{http.MethodPost, appPath(s, a.ID, "/publish")},
	} {
		var body any
		if r.method == http.MethodPost {
			body = map[string]any{"version": cur.Version, "acknowledge": []SchemaChange{}}
		}
		expect(t, w.mallory.do(r.method, r.path, body), http.StatusNotFound)
	}
	expect(t, w.alice.do(http.MethodGet, appPath(w.alicePrivate, w.aliceApp.ID, "/releases/2"), nil), http.StatusNotFound)
	expect(t, w.tryPublish(w.alice, w.old, w.view(w.alice, w.old, w.oldApp)), http.StatusForbidden)
}

func TestTheServiceChecksThePublishCapabilityToo(t *testing.T) {
	w := newWorld(t)
	s, a := w.shared, w.sharedApp
	cur := w.view(w.alice, s, a)
	cur = w.mustPatchDraft(w.alice, s, cur, with(*cur.Draft, Field{Name: "category", Type: TypeString}))
	w.mustSetRoles(w.alice, s, a, grant(w.owner.id, access.AppBuilder), grant(w.alice.id, access.AppRoles...))
	snapshot, err := w.e.s.access.Check(context.Background(), w.owner.id, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.e.s.publishApp(context.Background(), snapshot, Actor{UserID: w.owner.id}, a.ID, Publish{Version: cur.Version})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("a builder without the publish capability published: %v", err)
	}
	if got := w.view(w.alice, s, a); got.Live.Number != 1 {
		t.Fatalf("live = %d", got.Live.Number)
	}
}

func TestDefinitionsSayWhatTheyNeed(t *testing.T) {
	def := example(t)
	if def.DefinitionVersion != DefinitionVersion {
		t.Fatalf("version = %d", def.DefinitionVersion)
	}
	want := []string{"component.Button", "component.Column", "component.List", "component.Row", "component.Stat", "component.Text", "component.TextField"}
	if !slices.Equal(def.Requires, want) {
		t.Fatalf("requires = %v, want %v", def.Requires, want)
	}
	liar := def
	liar.Requires = []string{}
	checked, err := liar.Check()
	if err != nil || !slices.Equal(checked.Requires, want) {
		t.Fatalf("checked = %v, %v", checked.Requires, err)
	}
	called := def
	called.Components = slices.Clone(def.Components)
	called.Components = append(called.Components, []byte(`{"id":"when","component":"Text","text":{"call":"formatDate","args":{"value":"2024-01-02","format":"yyyy-MM-dd"}}}`))
	called.Components[0] = []byte(`{"id":"root","component":"Column","children":["title","total","items","form","when"]}`)
	if checked, err = called.Check(); err != nil || !slices.Contains(checked.Requires, "function.formatDate") {
		t.Fatalf("a function call: %v, %v", checked.Requires, err)
	}
	all := Capabilities()
	for _, c := range checked.Requires {
		if !slices.Contains(all, c) {
			t.Fatalf("%s isn't a capability", c)
		}
	}
	if !slices.IsSorted(all) || !slices.Contains(all, "component.Stat") || !slices.Contains(all, "function.pluralize") {
		t.Fatalf("capabilities = %v", all)
	}
	for _, v := range []int{DefinitionVersion + 1, -1} {
		newer := def
		newer.DefinitionVersion = v
		_, err := newer.Check()
		var bad *invalidError
		if !errors.Is(err, ErrInvalid) || !errors.As(err, &bad) || bad.code != "app_definition_version" {
			t.Fatalf("version %d: %v", v, err)
		}
	}
}

func TestReleasesAreImmutable(t *testing.T) {
	w := newWorld(t)
	a := w.aliceApp
	if _, err := w.e.db.Exec(`UPDATE app_releases SET definition = '{}' WHERE app_id = ?`, a.ID); err == nil {
		t.Fatal("a release was changed")
	}
	if _, err := w.e.db.Exec(`DELETE FROM app_releases WHERE app_id = ?`, a.ID); err == nil {
		t.Fatal("a release was deleted while its app is there")
	}
	if _, err := w.e.db.Exec(`UPDATE apps SET live_release = 9 WHERE id = ?`, a.ID); err == nil {
		t.Fatal("an app serves a release it doesn't have")
	}
	if _, err := w.e.db.Exec(`INSERT INTO app_releases (app_id, number, definition, definition_version, requires) VALUES (?, 1, '{}', 1, '[]')`, a.ID); err == nil {
		t.Fatal("a release number was reused")
	}
	if _, err := w.e.db.Exec(`DELETE FROM spaces WHERE id = ?`, w.shared.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM app_releases WHERE app_id = ?`, w.sharedApp.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("releases left: %d (%v)", n, err)
	}
}
