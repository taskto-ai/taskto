package apps

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
)

func grant(id int64, roles ...access.AppRole) access.AppGrant {
	return access.AppGrant{UserID: &id, Roles: roles}
}

func fallback(roles ...access.AppRole) access.AppGrant { return access.AppGrant{Roles: roles} }

func rolesPath(s access.Space, a App) string { return appPath(s, a.ID, "/roles") }

func (w *world) setRoles(u *user, s access.Space, a App, grants ...access.AppGrant) *httptest.ResponseRecorder {
	w.e.t.Helper()
	if grants == nil {
		grants = []access.AppGrant{}
	}
	return u.do(http.MethodPut, rolesPath(s, a), map[string]any{"grants": grants})
}

func (w *world) mustSetRoles(u *user, s access.Space, a App, grants ...access.AppGrant) {
	w.e.t.Helper()
	expect(w.e.t, w.setRoles(u, s, a, grants...), http.StatusOK)
}

func (w *world) view(u *user, s access.Space, a App) App {
	w.e.t.Helper()
	rec := u.do(http.MethodGet, appPath(s, a.ID, ""), nil)
	expect(w.e.t, rec, http.StatusOK)
	return decode[App](w.e.t, rec)
}

func expectForbidden(t *testing.T, rec *httptest.ResponseRecorder, capability string) {
	t.Helper()
	expect(t, rec, http.StatusForbidden)
	reply := decode[struct {
		Code   string
		Params map[string]any
	}](t, rec)
	if reply.Code != "app_forbidden" || reply.Params["capability"] != capability {
		t.Fatalf("403 reply = %s, want app_forbidden for %s", rec.Body, capability)
	}
}

func (w *world) rowsOf(u *user, s access.Space, a App) []string {
	w.e.t.Helper()
	var out []string
	for _, it := range w.items(u, s, a) {
		out = append(out, it["description"].(string))
	}
	return out
}

func (w *world) add2(u *user, s access.Space, a App, description string) *httptest.ResponseRecorder {
	return u.do(http.MethodPost, appPath(s, a.ID, "/actions"),
		action(a, "add_expense", map[string]any{"description": description, "amount": "10"}))
}

func TestAppWithoutRolesIsAsTheSpaceAllows(t *testing.T) {
	w := newWorld(t)
	s, a := w.shared, w.sharedApp
	editor := w.view(w.alice, s, a)
	if got := editor.Access; got.Restricted || !got.View || !got.Design || !got.Publish || !got.Submit || !got.ManageRoles ||
		got.ReadRecords != access.ScopeAll || got.ModifyRecords != access.ScopeAll || len(got.Roles) != len(access.AppRoles) {
		t.Fatalf("an editor's access = %+v", got)
	}
	viewer := w.view(w.bob, s, a)
	if got := viewer.Access; got.Restricted || !got.View || got.Design || got.Publish || got.Submit || got.ManageRoles ||
		got.ReadRecords != access.ScopeAll || got.ModifyRecords != access.ScopeNone || len(got.Roles) != 1 || got.Roles[0] != access.AppReader {
		t.Fatalf("a viewer's access = %+v", got)
	}
	if editor.Rows != 1 || viewer.Rows != 1 {
		t.Fatalf("rows %d and %d, want 1 and 1", editor.Rows, viewer.Rows)
	}
	if got := w.view(w.alice, w.alicePrivate, w.aliceApp).Access; !got.Design || !got.Submit || got.ReadRecords != access.ScopeAll {
		t.Fatalf("a private space's owner = %+v", got)
	}
	if got := w.view(w.alice, w.old, w.oldApp).Access; got.Design || got.Publish || got.Submit || got.ModifyRecords != access.ScopeNone || got.ReadRecords != access.ScopeAll {
		t.Fatalf("an editor of an archived space = %+v", got)
	}
}

func TestAppUserSubmitsButCantRedesign(t *testing.T) {
	w := newWorld(t)
	w.e.addMember(w.owner, w.shared, w.mallory, access.RoleEditor)
	s, a := w.shared, w.sharedApp
	w.mustSetRoles(w.alice, s, a, grant(w.mallory.id, access.AppUser), grant(w.alice.id, access.AppRoles...))

	got := w.view(w.mallory, s, a)
	if x := got.Access; !x.Restricted || !x.View || x.Design || x.Publish || !x.Submit || x.ManageRoles ||
		x.ReadRecords != access.ScopeOwn || x.ModifyRecords != access.ScopeOwn {
		t.Fatalf("the user's access = %+v", x)
	}
	if got.Rows != 0 || w.view(w.alice, s, a).Rows != 1 {
		t.Fatalf("rows the user counts = %d", got.Rows)
	}

	rec := w.add2(w.mallory, s, a, "owner-lunch")
	expect(t, rec, http.StatusOK)
	if body := rec.Body.String(); !strings.Contains(body, "owner-lunch") || strings.Contains(body, "team-lunch") || !strings.Contains(body, `"total":"10.00"`) {
		t.Fatalf("after submitting: %s", body)
	}
	if rows := w.rowsOf(w.mallory, s, a); len(rows) != 1 || rows[0] != "owner-lunch" {
		t.Fatalf("the user reads %v", rows)
	}
	rec = w.mallory.do(http.MethodGet, appPath(s, a.ID, "/surface"), nil)
	expect(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"total":"10.00"`) || strings.Contains(rec.Body.String(), "team-lunch") {
		t.Fatalf("surface: %s", rec.Body)
	}
	list := decode[struct{ Apps []App }](t, w.mallory.do(http.MethodGet, appsPath(s, ""), nil)).Apps
	if len(list) != 1 || list[0].Rows != 1 || list[0].Access.ReadRecords != access.ScopeOwn {
		t.Fatalf("the user's list = %+v", list)
	}
	if rows := w.rowsOf(w.alice, s, a); len(rows) != 2 {
		t.Fatalf("alice reads %v", rows)
	}
	rec = w.mallory.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "add_expense", map[string]any{"description": "second", "amount": "5"}))
	expect(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"total":"15.00"`) {
		t.Fatalf("a user's total counts only their own records: %s", rec.Body)
	}
	rec = w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "add_expense", map[string]any{"description": "alice-two", "amount": "1"}))
	if !strings.Contains(rec.Body.String(), `"total":"19.00"`) {
		t.Fatalf("alice's total counts every record: %s", rec.Body)
	}

	mine := w.e.private(w.mallory, w.ws)
	expectForbidden(t, w.mallory.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": 1, "name": "Mine now"}), "design")
	expectForbidden(t, w.mallory.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": 1, "definition": example(t)}), "design")
	expectForbidden(t, w.mallory.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": 1, "archived": true}), "publish")
	expectForbidden(t, w.mallory.do(http.MethodPost, appPath(s, a.ID, "/move"), map[string]any{"version": 1, "space_id": mine.ID}), "design")
	expectForbidden(t, w.mallory.do(http.MethodGet, rolesPath(s, a), nil), "manage_roles")
	expectForbidden(t, w.setRoles(w.mallory, s, a, fallback(access.AppBuilder)), "manage_roles")
	if got := w.view(w.alice, s, a); got.Name != "Team expenses" || got.Version != 1 || got.ArchivedAt != nil || got.SpaceID != s.ID {
		t.Fatalf("the app changed: %+v", got)
	}

	var theirs float64
	for _, it := range w.items(w.alice, s, a) {
		if it["description"] == "team-lunch" {
			theirs = it["id"].(float64)
		}
	}
	rec = w.mallory.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "delete_expense", map[string]any{"id": theirs}))
	expect(t, rec, http.StatusUnprocessableEntity)
	if decode[struct{ Code string }](t, rec).Code != "app_row_not_found" {
		t.Fatalf("body %s", rec.Body)
	}
	if rows := w.rowsOf(w.alice, s, a); len(rows) != 4 {
		t.Fatalf("a record was deleted: %v", rows)
	}
	own := w.items(w.mallory, s, a)[0]["id"]
	expect(t, w.mallory.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "delete_expense", map[string]any{"id": own})), http.StatusOK)
	if rows := w.rowsOf(w.alice, s, a); len(rows) != 3 {
		t.Fatalf("rows after the user deleted one: %v", rows)
	}
}

func TestOwnRecordsAreTheOnesTheUserCreated(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	def := example(t)
	def.Actions["edit_expense"] = Action{Kind: ActionUpdate}
	rec := w.alice.do(http.MethodPost, appsPath(s, ""), map[string]any{"name": "Editable", "definition": def})
	expect(t, rec, http.StatusCreated)
	a := w.publish(w.alice, s, decode[App](t, rec))
	w.add(w.alice, s, a, "alice-row", "1")
	w.add(w.owner, s, a, "owner-row", "2")
	w.mustSetRoles(w.alice, s, a, grant(w.owner.id, access.AppUser), grant(w.alice.id, access.AppRoles...))

	edit := func(id any) *httptest.ResponseRecorder {
		return w.owner.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "edit_expense", map[string]any{"id": id, "amount": "9"}))
	}
	var aliceRow, ownerRow any
	for _, it := range w.items(w.alice, s, a) {
		if it["description"] == "alice-row" {
			aliceRow = it["id"]
		} else {
			ownerRow = it["id"]
		}
	}
	expect(t, edit(aliceRow), http.StatusUnprocessableEntity)
	expect(t, edit(ownerRow), http.StatusOK)
	for _, it := range w.items(w.alice, s, a) {
		want := 1.0
		if it["description"] == "owner-row" {
			want = 9
		}
		if it["amount"] != want {
			t.Fatalf("%v: amount %v, want %v", it["description"], it["amount"], want)
		}
	}

	if _, err := w.e.db.Exec(`UPDATE app_rows SET created_by = NULL WHERE app_id = ? AND data LIKE '%owner-row%'`, a.ID); err != nil {
		t.Fatal(err)
	}
	if rows := w.rowsOf(w.owner, s, a); len(rows) != 0 {
		t.Fatalf("the user reads %v of records without a creator", rows)
	}
	expect(t, edit(ownerRow), http.StatusUnprocessableEntity)
	if rows := w.rowsOf(w.alice, s, a); len(rows) != 2 {
		t.Fatalf("a reader of all reads %v", rows)
	}
}

func TestBuilderPublisherAndDataManagerAreSeparate(t *testing.T) {
	w := newWorld(t)
	s, a := w.shared, w.sharedApp
	w.mustSetRoles(w.alice, s, a,
		grant(w.alice.id, access.AppBuilder, access.AppPublisher),
		grant(w.owner.id, access.AppDataManager))

	b := w.view(w.alice, s, a)
	if b.Rows != 0 || b.Access.ReadRecords != access.ScopeNone || b.Access.Submit {
		t.Fatalf("builder's view = %+v", b)
	}
	if items := w.items(w.alice, s, a); len(items) != 0 {
		t.Fatalf("builder reads %v", items)
	}
	if rec := w.alice.do(http.MethodGet, appPath(s, a.ID, "/surface"), nil); !strings.Contains(rec.Body.String(), `"total":"0.00"`) {
		t.Fatalf("builder's summaries leak: %s", rec.Body)
	}
	expectForbidden(t, w.add2(w.alice, s, a, "x"), "submit")
	expect(t, w.alice.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": 1, "name": "Renamed"}), http.StatusOK)
	expect(t, w.alice.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": 2, "archived": true}), http.StatusOK)
	expect(t, w.alice.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": 3, "archived": false}), http.StatusOK)
	expectForbidden(t, w.owner.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": 4, "archived": true}), "publish")
	expectForbidden(t, w.owner.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": 4, "name": "No"}), "design")
	expectForbidden(t, w.owner.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": 4, "name": "No", "archived": true}), "design")

	w.add(w.owner, s, a, "managed", "2")
	if rows := w.rowsOf(w.owner, s, a); len(rows) != 2 {
		t.Fatalf("the data manager reads %v", rows)
	}
	var theirs any
	for _, it := range w.items(w.owner, s, a) {
		if it["description"] == "team-lunch" {
			theirs = it["id"]
		}
	}
	expect(t, w.owner.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "delete_expense", map[string]any{"id": theirs})), http.StatusOK)

	w.mustSetRoles(w.alice, s, a, grant(w.alice.id, access.AppBuilder), grant(w.owner.id, access.AppReader))
	if rows := w.rowsOf(w.owner, s, a); len(rows) != 1 {
		t.Fatalf("the reader reads %v", rows)
	}
	expectForbidden(t, w.add2(w.owner, s, a, "x"), "submit")
	expectForbidden(t, w.owner.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "delete_expense", map[string]any{"id": 1})), "modify_records")
}

func TestAppRolesOnlyNarrowTheSpace(t *testing.T) {
	w := newWorld(t)
	s, a := w.shared, w.sharedApp
	w.mustSetRoles(w.alice, s, a, grant(w.bob.id, access.AppBuilder, access.AppPublisher, access.AppDataManager, access.AppUser), grant(w.alice.id, access.AppRoles...))
	got := w.view(w.bob, s, a)
	if x := got.Access; !x.View || x.Design || x.Publish || x.Submit || x.ManageRoles || x.ModifyRecords != access.ScopeNone || x.ReadRecords != access.ScopeAll {
		t.Fatalf("a viewer's access with every role = %+v", x)
	}
	rec := w.add2(w.bob, s, a, "x")
	expect(t, rec, http.StatusForbidden)
	if decode[struct{ Code string }](t, rec).Code != "space_read_only" {
		t.Fatalf("body %s", rec.Body)
	}
	expect(t, w.bob.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": 1, "name": "x"}), http.StatusForbidden)
	expect(t, w.setRoles(w.bob, s, a, grant(w.bob.id, access.AppBuilder)), http.StatusForbidden)
	expect(t, w.bob.do(http.MethodGet, rolesPath(s, a), nil), http.StatusForbidden)

	w.mustSetRoles(w.alice, s, a, grant(w.alice.id, access.AppBuilder, access.AppDataManager), fallback(access.AppUser))
	if rows := w.rowsOf(w.owner, s, a); len(rows) != 0 {
		t.Fatalf("a member under the default reads %v", rows)
	}
	if bob := w.view(w.bob, s, a); bob.Access.ReadRecords != access.ScopeOwn || bob.Rows != 0 {
		t.Fatalf("bob = %+v", bob)
	}
	if rows := w.rowsOf(w.alice, s, a); len(rows) != 1 {
		t.Fatalf("alice's own assignment reads %v", rows)
	}

	w.mustSetRoles(w.alice, s, a, fallback(access.AppUser, access.AppBuilder, access.AppPublisher))
	if _, err := w.e.db.Exec(`UPDATE spaces SET archived_at = '2026-01-01T00:00:00.000Z' WHERE id = ?`, s.ID); err != nil {
		t.Fatal(err)
	}
	if got := w.view(w.alice, s, a).Access; got.Design || got.Publish || got.Submit || got.ManageRoles {
		t.Fatalf("access in an archived space = %+v", got)
	}
	expect(t, w.add2(w.alice, s, a, "x"), http.StatusForbidden)
}

func TestSettingRoles(t *testing.T) {
	w := newWorld(t)
	s, a := w.shared, w.sharedApp
	body := func(rec *httptest.ResponseRecorder) (code string) {
		return decode[struct{ Code string }](t, rec).Code
	}

	rec := w.setRoles(w.alice, s, a, grant(w.mallory.id, access.AppUser))
	expect(t, rec, http.StatusBadRequest)
	if body(rec) != "app_roles_not_member" {
		t.Fatalf("body %s", rec.Body)
	}
	rec = w.setRoles(w.alice, s, a, grant(w.alicePrivate.ID+9999, access.AppUser))
	expect(t, rec, http.StatusBadRequest)
	for name, grants := range map[string][]access.AppGrant{
		"unknown":   {grant(w.alice.id, "admin")},
		"none":      {grant(w.alice.id)},
		"twice":     {grant(w.alice.id, access.AppBuilder), grant(w.alice.id, access.AppReader)},
		"defaults":  {fallback(access.AppUser), fallback(access.AppReader)},
		"duplicate": {grant(w.alice.id, access.AppBuilder, access.AppBuilder)},
	} {
		rec := w.setRoles(w.alice, s, a, grants...)
		expect(t, rec, http.StatusBadRequest)
		if body(rec) != "app_roles_invalid" {
			t.Fatalf("%s: body %s", name, rec.Body)
		}
	}
	if got := w.view(w.owner, s, a).Access; got.Restricted {
		t.Fatalf("a refused change restricted the app: %+v", got)
	}

	rec = w.setRoles(w.alice, s, a, grant(w.alice.id, access.AppReader))
	expect(t, rec, http.StatusBadRequest)
	if body(rec) != "app_roles_lockout" {
		t.Fatalf("body %s", rec.Body)
	}
	rec = w.setRoles(w.alice, s, a, fallback(access.AppUser))
	expect(t, rec, http.StatusBadRequest)
	if body(rec) != "app_roles_lockout" {
		t.Fatalf("body %s", rec.Body)
	}

	w.mustSetRoles(w.alice, s, a, grant(w.owner.id, access.AppUser, access.AppReader), fallback(access.AppReader), grant(w.alice.id, access.AppBuilder))
	rec = w.alice.do(http.MethodGet, rolesPath(s, a), nil)
	expect(t, rec, http.StatusOK)
	reply := decode[struct {
		Grants []access.AppGrant
		Roles  []access.AppRole
	}](t, rec)
	if len(reply.Grants) != 3 || len(reply.Roles) != 5 || reply.Grants[2].UserID != nil || reply.Grants[2].Roles[0] != access.AppReader {
		t.Fatalf("roles = %s", rec.Body)
	}
	if got := w.view(w.owner, s, a).Access; got.ReadRecords != access.ScopeAll || got.ModifyRecords != access.ScopeOwn || !got.Submit || got.Design {
		t.Fatalf("user and reader together = %+v", got)
	}
	w.mustSetRoles(w.alice, s, a)
	if got := w.view(w.owner, s, a).Access; got.Restricted || !got.Design {
		t.Fatalf("after clearing: %+v", got)
	}
	if v := w.view(w.alice, s, a).Version; v != 1 {
		t.Fatalf("version = %d", v)
	}
}

func TestAppRolesStayInTheirApp(t *testing.T) {
	w := newWorld(t)
	w.mustSetRoles(w.alice, w.shared, w.sharedApp, grant(w.owner.id, access.AppUser), grant(w.alice.id, access.AppRoles...))
	expect(t, w.setRoles(w.alice, w.alicePrivate, w.sharedApp), http.StatusNotFound)
	expect(t, w.alice.do(http.MethodGet, rolesPath(w.alicePrivate, w.sharedApp), nil), http.StatusNotFound)
	if _, err := w.e.db.Exec(`DELETE FROM apps WHERE id = ?`, w.sharedApp.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM app_roles`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("roles left after their app: %d, %v", n, err)
	}
}

func TestRolesAreCheckedAgainUnderTheWriteLock(t *testing.T) {
	w := newWorld(t)
	w.e.addMember(w.owner, w.shared, w.mallory, access.RoleEditor)
	ctx := context.Background()
	s, a := w.shared, w.sharedApp
	owner := Actor{UserID: w.mallory.id}
	snapshot, err := w.e.s.access.Check(ctx, w.mallory.id, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	w.mustSetRoles(w.alice, s, a, grant(w.mallory.id, access.AppReader), grant(w.alice.id, access.AppRoles...))
	name := "late"
	if _, err := w.e.s.updateApp(ctx, snapshot, owner, a.ID, Changes{Version: 1, Name: &name}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("update with a stale snapshot = %v, want ErrForbidden", err)
	}
	if _, err := w.e.s.act(ctx, snapshot, owner, a.ID, action(a, "add_expense", map[string]any{"description": "late", "amount": "1"})); !errors.Is(err, ErrForbidden) {
		t.Fatalf("act with a stale snapshot = %v, want ErrForbidden", err)
	}
	if _, err := w.e.s.setAppRoles(ctx, snapshot, owner, a.ID, nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("setting roles with a stale snapshot = %v, want ErrForbidden", err)
	}
	mine := w.e.private(w.mallory, w.ws)
	mineSnapshot, _ := w.e.s.access.Check(ctx, w.mallory.id, mine.ID)
	if _, err := w.e.s.moveApp(ctx, snapshot, owner, a.ID, 1, mineSnapshot); !errors.Is(err, ErrForbidden) {
		t.Fatalf("move with a stale snapshot = %v, want ErrForbidden", err)
	}
	if got := w.view(w.alice, s, a); got.Name != "Team expenses" || got.Rows != 1 {
		t.Fatalf("the app changed: %+v", got)
	}
}

func TestAgentActsWithItsUsersRoles(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	s, a := w.shared, w.sharedApp
	w.mustSetRoles(w.alice, s, a, grant(w.owner.id, access.AppUser), grant(w.alice.id, access.AppBuilder))
	w.add(w.owner, s, a, "owner-row", "2")

	agent := func(u *user) Actor { return Actor{UserID: u.id, Agent: true} }
	rows, err := w.e.s.ListRows(ctx, agent(w.owner), s.ID, a.ID)
	if err != nil || len(rows) != 1 || rows[0]["description"] != "owner-row" {
		t.Fatalf("the user's agent reads %v, %v", rows, err)
	}
	name := "By the agent"
	if _, err := w.e.s.UpdateApp(ctx, agent(w.owner), s.ID, a.ID, Changes{Version: 1, Name: &name}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("update by a user's agent = %v, want ErrForbidden", err)
	}
	shown, err := w.e.s.ShowApp(ctx, agent(w.owner), s.ID, a.ID)
	if err != nil || shown.Rows != 1 || shown.Access.Design {
		t.Fatalf("show_app for the user's agent = %+v, %v", shown, err)
	}
	if _, err := w.e.s.UpdateApp(ctx, agent(w.alice), s.ID, a.ID, Changes{Version: 1, Name: &name}); err != nil {
		t.Fatal(err)
	}
	if rows, err := w.e.s.ListRows(ctx, agent(w.alice), s.ID, a.ID); err != nil || len(rows) != 0 {
		t.Fatalf("the builder's agent reads %v, %v", rows, err)
	}
	if _, err := w.e.s.ListRows(ctx, agent(w.mallory), s.ID, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a non-member's agent = %v, want ErrNotFound", err)
	}
	if _, err := w.e.s.ListRows(ctx, agent(w.alice), w.alicePrivate.ID, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an app through another space = %v, want ErrNotFound", err)
	}
	def := map[string]any{"name": "Sneaky", "definition": map[string]any{}}
	raw := example(t)
	def["definition"] = map[string]any{"components": raw.Components, "collection": raw.Collection, "actions": raw.Actions, "roles": []string{"builder"}}
	expect(t, w.alice.do(http.MethodPost, appsPath(s, ""), def), http.StatusBadRequest)
}

func TestUnnamedMembersOfARestrictedAppSeeNothing(t *testing.T) {
	w := newWorld(t)
	s, a := w.shared, w.sharedApp
	w.mustSetRoles(w.alice, s, a, grant(w.alice.id, access.AppBuilder), grant(w.owner.id, access.AppUser))
	expect(t, w.bob.do(http.MethodGet, appPath(s, a.ID, ""), nil), http.StatusNotFound)
	expect(t, w.bob.do(http.MethodGet, appPath(s, a.ID, "/surface"), nil), http.StatusNotFound)
	rec := w.bob.do(http.MethodGet, appsPath(s, ""), nil)
	expect(t, rec, http.StatusOK)
	for _, ap := range decode[struct{ Apps []App }](t, rec).Apps {
		if ap.ID == a.ID {
			t.Fatalf("bob's list shows the app: %s", rec.Body)
		}
	}
	w.mustSetRoles(w.alice, s, a, grant(w.alice.id, access.AppBuilder), fallback(access.AppReader))
	if got := w.view(w.bob, s, a).Access; !got.View || got.ReadRecords != access.ScopeAll || got.Submit {
		t.Fatalf("bob with the default = %+v", got)
	}
}

func withMax(def Definition, max float64) *Definition {
	def.Collection.Fields = append([]Field(nil), def.Collection.Fields...)
	for i := range def.Collection.Fields {
		if def.Collection.Fields[i].Name == "amount" {
			def.Collection.Fields[i].Max = &max
		}
	}
	return &def
}

func TestPublisherWithoutDataIsToldOfNoRows(t *testing.T) {
	w := newWorld(t)
	s, a := w.shared, w.sharedApp
	w.mustSetRoles(w.alice, s, a, grant(w.alice.id, access.AppBuilder, access.AppPublisher))
	if got := w.view(w.alice, s, a).Access.ReadRecords; got != access.ScopeNone {
		t.Fatalf("builder reads %s", got)
	}
	refusal := func(u *user, max float64) *httptest.ResponseRecorder {
		cur := w.view(u, s, a)
		rec := u.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": cur.Version, "definition": withMax(*cur.Draft, max)})
		expect(t, rec, http.StatusOK)
		cur = decode[App](t, rec)
		return u.do(http.MethodPost, appPath(s, a.ID, "/publish"), map[string]any{"version": cur.Version, "acknowledge": []SchemaChange{}})
	}
	tight, loose := refusal(w.alice, 1), refusal(w.alice, 1000)
	for _, rec := range []*httptest.ResponseRecorder{tight, loose} {
		expect(t, rec, http.StatusConflict)
		reply := decode[struct {
			Code   string
			Params struct{ Changes []SchemaChange }
		}](t, rec)
		if reply.Code != "app_migration_required" || len(reply.Params.Changes) != 1 || reply.Params.Changes[0] != (SchemaChange{ChangeFieldNarrowed, "amount"}) {
			t.Fatalf("refusal = %s", rec.Body)
		}
		for _, leak := range []string{"team-lunch", "rows"} {
			if strings.Contains(rec.Body.String(), leak) {
				t.Fatalf("the refusal tells of the rows (%q): %s", leak, rec.Body)
			}
		}
	}
	if tight.Body.String() != loose.Body.String() {
		t.Fatalf("the rows decide the answer:\n%s\n%s", tight.Body, loose.Body)
	}

	cur := w.view(w.alice, s, a)
	expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/publish"),
		map[string]any{"version": cur.Version, "acknowledge": []SchemaChange{{ChangeFieldNarrowed, "amount"}}}), http.StatusOK)
	cur = w.view(w.alice, s, a)
	rec := w.alice.do(http.MethodPost, appPath(s, a.ID, "/rollback"), map[string]any{"version": cur.Version, "release": 1})
	expectForbidden(t, rec, "read_records")

	w.mustSetRoles(w.alice, s, a, grant(w.alice.id, access.AppBuilder, access.AppPublisher, access.AppDataManager))
	expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/rollback"), map[string]any{"version": w.view(w.alice, s, a).Version, "release": 1}), http.StatusOK)
	expect(t, refusal(w.alice, 1000), http.StatusOK)
}

func TestRolesDontOutliveTheSpaceTheyWereSetIn(t *testing.T) {
	w := newWorld(t)
	s, a := w.shared, w.sharedApp
	w.e.addMember(w.owner, s, w.mallory, access.RoleEditor)
	w.mustSetRoles(w.alice, s, a, grant(w.alice.id, access.AppRoles...), grant(w.mallory.id, access.AppDataManager))
	roleRows := func() (n int) {
		t.Helper()
		if err := w.e.db.QueryRow(`SELECT count(*) FROM app_roles WHERE app_id = ? AND user_id = ?`, a.ID, w.mallory.id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	expect(t, w.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", s.WorkspaceID, s.ID, w.mallory.id), nil), http.StatusNoContent)
	if n := roleRows(); n != 0 {
		t.Fatalf("Mallory kept %d roles after leaving the space", n)
	}
	w.e.addMember(w.owner, s, w.mallory, access.RoleViewer)
	expect(t, w.mallory.do(http.MethodGet, appPath(s, a.ID, ""), nil), http.StatusNotFound)

	dest := w.e.createShared(w.owner, w.ws, "Elsewhere team")
	w.e.addMember(w.owner, dest, w.alice, access.RoleEditor)
	w.e.addMember(w.owner, dest, w.mallory, access.RoleViewer)
	cur := w.view(w.alice, s, a)
	rec := w.alice.do(http.MethodPost, appPath(s, a.ID, "/move"), map[string]any{"version": cur.Version, "space_id": dest.ID})
	expect(t, rec, http.StatusOK)
	var n int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM app_roles WHERE app_id = ?`, a.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a moved app kept %d roles (%v)", n, err)
	}
	if got := w.view(w.mallory, dest, a).Access; got.Restricted || got.ModifyRecords != access.ScopeNone {
		t.Fatalf("Mallory in the new space = %+v; want a viewer's access", got)
	}
}
