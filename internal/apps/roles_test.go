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

// grant is a role assignment for user id; fallback is the default's, for every member without
// roles of their own.
func grant(id int64, roles ...access.AppRole) access.AppGrant {
	return access.AppGrant{UserID: &id, Roles: roles}
}

func fallback(roles ...access.AppRole) access.AppGrant { return access.AppGrant{Roles: roles} }

func rolesPath(s access.Space, a App) string { return appPath(s, a.ID, "/roles") }

// setRoles has u replace the roles of a.
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

// view is the app as u sees it.
func (w *world) view(u *user, s access.Space, a App) App {
	w.e.t.Helper()
	rec := u.do(http.MethodGet, appPath(s, a.ID, ""), nil)
	expect(w.e.t, rec, http.StatusOK)
	return decode[App](w.e.t, rec)
}

// expectForbidden checks rec is the 403 of a role: app_forbidden, naming the capability.
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

// An app with no roles is what every app was: the space's editors do everything, over every
// record, and its viewers read every record and change nothing.
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
	// The owner of a private space is every role in it; an archived space takes the writing.
	if got := w.view(w.alice, w.alicePrivate, w.aliceApp).Access; !got.Design || !got.Submit || got.ReadRecords != access.ScopeAll {
		t.Fatalf("a private space's owner = %+v", got)
	}
	if got := w.view(w.alice, w.old, w.oldApp).Access; got.Design || got.Publish || got.Submit || got.ModifyRecords != access.ScopeNone || got.ReadRecords != access.ScopeAll {
		t.Fatalf("an editor of an archived space = %+v", got)
	}
}

// A user can submit records without being able to redesign the app, and reads only their own.
func TestAppUserSubmitsButCantRedesign(t *testing.T) {
	w := newWorld(t)
	s, a := w.shared, w.sharedApp
	w.mustSetRoles(w.alice, s, a, grant(w.owner.id, access.AppUser), grant(w.alice.id, access.AppRoles...))

	got := w.view(w.owner, s, a)
	if x := got.Access; !x.Restricted || !x.View || x.Design || x.Publish || !x.Submit || x.ManageRoles ||
		x.ReadRecords != access.ScopeOwn || x.ModifyRecords != access.ScopeOwn {
		t.Fatalf("the user's access = %+v", x)
	}
	// The row alice added is not theirs: not in the count either.
	if got.Rows != 0 || w.view(w.alice, s, a).Rows != 1 {
		t.Fatalf("rows the user counts = %d", got.Rows)
	}

	// They submit, and the answer shows their record and the summary over it alone.
	rec := w.add2(w.owner, s, a, "owner-lunch")
	expect(t, rec, http.StatusOK)
	if body := rec.Body.String(); !strings.Contains(body, "owner-lunch") || strings.Contains(body, "team-lunch") || !strings.Contains(body, `"total":"10.00"`) {
		t.Fatalf("after submitting: %s", body)
	}
	if rows := w.rowsOf(w.owner, s, a); len(rows) != 1 || rows[0] != "owner-lunch" {
		t.Fatalf("the user reads %v", rows)
	}
	// The launched screen's summary too, and the list's count.
	rec = w.owner.do(http.MethodGet, appPath(s, a.ID, "/surface"), nil)
	expect(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"total":"10.00"`) || strings.Contains(rec.Body.String(), "team-lunch") {
		t.Fatalf("surface: %s", rec.Body)
	}
	list := decode[struct{ Apps []App }](t, w.owner.do(http.MethodGet, appsPath(s, ""), nil)).Apps
	if len(list) != 1 || list[0].Rows != 1 || list[0].Access.ReadRecords != access.ScopeOwn {
		t.Fatalf("the user's list = %+v", list)
	}
	// Everyone with a wider role still sees both: the total is 13.
	if rows := w.rowsOf(w.alice, s, a); len(rows) != 2 {
		t.Fatalf("alice reads %v", rows)
	}
	rec = w.owner.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "add_expense", map[string]any{"description": "second", "amount": "5"}))
	expect(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"total":"15.00"`) {
		t.Fatalf("a user's total counts only their own records: %s", rec.Body)
	}
	rec = w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "add_expense", map[string]any{"description": "alice-two", "amount": "1"}))
	if !strings.Contains(rec.Body.String(), `"total":"19.00"`) {
		t.Fatalf("alice's total counts every record: %s", rec.Body)
	}

	// But not redesign: not the definition, the name, taking it offline, moving it, or roles.
	mine := w.e.private(w.owner, w.ws)
	expectForbidden(t, w.owner.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": 1, "name": "Mine now"}), "design")
	expectForbidden(t, w.owner.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": 1, "definition": example(t)}), "design")
	expectForbidden(t, w.owner.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": 1, "archived": true}), "publish")
	expectForbidden(t, w.owner.do(http.MethodPost, appPath(s, a.ID, "/move"), map[string]any{"version": 1, "space_id": mine.ID}), "design")
	expectForbidden(t, w.owner.do(http.MethodGet, rolesPath(s, a), nil), "manage_roles")
	expectForbidden(t, w.setRoles(w.owner, s, a, fallback(access.AppBuilder)), "manage_roles")
	if got := w.view(w.alice, s, a); got.Name != "Team expenses" || got.Version != 1 || got.ArchivedAt != nil || got.SpaceID != s.ID {
		t.Fatalf("the app changed: %+v", got)
	}

	// Changing another's record is as if it weren't there; their own they may delete.
	var theirs float64
	for _, it := range w.items(w.alice, s, a) {
		if it["description"] == "team-lunch" {
			theirs = it["id"].(float64)
		}
	}
	rec = w.owner.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "delete_expense", map[string]any{"id": theirs}))
	expect(t, rec, http.StatusUnprocessableEntity)
	if decode[struct{ Code string }](t, rec).Code != "app_row_not_found" {
		t.Fatalf("body %s", rec.Body)
	}
	if rows := w.rowsOf(w.alice, s, a); len(rows) != 4 {
		t.Fatalf("a record was deleted: %v", rows)
	}
	own := w.items(w.owner, s, a)[0]["id"]
	expect(t, w.owner.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "delete_expense", map[string]any{"id": own})), http.StatusOK)
	if rows := w.rowsOf(w.alice, s, a); len(rows) != 3 {
		t.Fatalf("rows after the user deleted one: %v", rows)
	}
}

// A record's own creator is the rule for "own": updates reach only theirs.
func TestOwnRecordsAreTheOnesTheUserCreated(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	// An app with an update action, which the example doesn't have.
	def := example(t)
	def.Actions["edit_expense"] = Action{Kind: ActionUpdate}
	rec := w.alice.do(http.MethodPost, appsPath(s, ""), map[string]any{"name": "Editable", "definition": def})
	expect(t, rec, http.StatusCreated)
	a := decode[App](t, rec)
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

	// A record whose creator's account is gone belongs to no one: only readers of all see it.
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

// Building, publishing and data access are separate: a builder who isn't also given data
// access reads no records, and can't submit.
func TestBuilderPublisherAndDataManagerAreSeparate(t *testing.T) {
	w := newWorld(t)
	s, a := w.shared, w.sharedApp
	w.mustSetRoles(w.alice, s, a,
		grant(w.alice.id, access.AppBuilder, access.AppPublisher),
		grant(w.owner.id, access.AppDataManager))

	// The builder rebuilds the app but sees no data, and adds none.
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
	// The publisher takes it offline and back, which the data manager can't, nor rename it.
	expect(t, w.alice.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": 2, "archived": true}), http.StatusOK)
	expect(t, w.alice.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": 3, "archived": false}), http.StatusOK)
	expectForbidden(t, w.owner.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": 4, "archived": true}), "publish")
	expectForbidden(t, w.owner.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": 4, "name": "No"}), "design")
	// A rename together with taking it offline needs both.
	expectForbidden(t, w.owner.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": 4, "name": "No", "archived": true}), "design")

	// The data manager reads, adds and changes everyone's.
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

	// A reader reads everything and changes nothing, though the space lets them write.
	w.mustSetRoles(w.alice, s, a, grant(w.alice.id, access.AppBuilder), grant(w.owner.id, access.AppReader))
	if rows := w.rowsOf(w.owner, s, a); len(rows) != 1 {
		t.Fatalf("the reader reads %v", rows)
	}
	expectForbidden(t, w.add2(w.owner, s, a, "x"), "submit")
	expectForbidden(t, w.owner.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "delete_expense", map[string]any{"id": 1})), "modify_records")
}

// App roles never widen what the space allows.
func TestAppRolesOnlyNarrowTheSpace(t *testing.T) {
	w := newWorld(t)
	s, a := w.shared, w.sharedApp
	// Bob views the space: every role there is gives him no more than reading.
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

	// A default of "user" meets his reading: only his own records, of which he has none.
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

	// An archived space takes every right to write, whatever the roles.
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

	// Only people who may read the space can have a role in it.
	rec := w.setRoles(w.alice, s, a, grant(w.mallory.id, access.AppUser))
	expect(t, rec, http.StatusBadRequest)
	if body(rec) != "app_roles_not_member" {
		t.Fatalf("body %s", rec.Body)
	}
	rec = w.setRoles(w.alice, s, a, grant(w.alicePrivate.ID+9999, access.AppUser))
	expect(t, rec, http.StatusBadRequest)
	// Unknown roles, no roles, a user twice, two defaults.
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
	// Nothing was set by the refused ones.
	if got := w.view(w.owner, s, a).Access; got.Restricted {
		t.Fatalf("a refused change restricted the app: %+v", got)
	}

	// Whoever sets roles keeps the right to: no locking oneself out.
	rec = w.setRoles(w.alice, s, a, grant(w.alice.id, access.AppReader))
	expect(t, rec, http.StatusBadRequest)
	if body(rec) != "app_roles_lockout" {
		t.Fatalf("body %s", rec.Body)
	}
	rec = w.setRoles(w.alice, s, a, fallback(access.AppUser))
	expect(t, rec, http.StatusBadRequest) // the default would take hers
	if body(rec) != "app_roles_lockout" {
		t.Fatalf("body %s", rec.Body)
	}

	// Set, read back, and cleared: an empty list makes the app unrestricted again.
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
	// Roles are no change of the app: its version stays.
	if v := w.view(w.alice, s, a).Version; v != 1 {
		t.Fatalf("version = %d", v)
	}
}

// Roles can't be reached from another space or workspace, and don't outlive their app.
func TestAppRolesStayInTheirApp(t *testing.T) {
	w := newWorld(t)
	w.mustSetRoles(w.alice, w.shared, w.sharedApp, grant(w.owner.id, access.AppUser), grant(w.alice.id, access.AppRoles...))
	// Alice manages the team app, but not through her private space's address.
	expect(t, w.setRoles(w.alice, w.alicePrivate, w.sharedApp), http.StatusNotFound)
	expect(t, w.alice.do(http.MethodGet, rolesPath(w.alicePrivate, w.sharedApp), nil), http.StatusNotFound)
	// Roles go with their app.
	if _, err := w.e.db.Exec(`DELETE FROM apps WHERE id = ?`, w.sharedApp.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM app_roles`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("roles left after their app: %d, %v", n, err)
	}
}

// Every write checks the caller's roles again under the write lock, as it does the space's.
func TestRolesAreCheckedAgainUnderTheWriteLock(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	s, a := w.shared, w.sharedApp
	owner := Actor{UserID: w.owner.id}
	// The owner's request was checked while the app was unrestricted; then roles narrow them.
	snapshot, err := w.e.s.access.Check(ctx, w.owner.id, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	w.mustSetRoles(w.alice, s, a, grant(w.owner.id, access.AppReader), grant(w.alice.id, access.AppRoles...))
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
	mine := w.e.private(w.owner, w.ws)
	mineSnapshot, _ := w.e.s.access.Check(ctx, w.owner.id, mine.ID)
	if _, err := w.e.s.moveApp(ctx, snapshot, owner, a.ID, 1, mineSnapshot); !errors.Is(err, ErrForbidden) {
		t.Fatalf("move with a stale snapshot = %v, want ErrForbidden", err)
	}
	if got := w.view(w.alice, s, a); got.Name != "Team expenses" || got.Rows != 1 {
		t.Fatalf("the app changed: %+v", got)
	}
}

// The agent has its user's roles and no more.
func TestAgentActsWithItsUsersRoles(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	s, a := w.shared, w.sharedApp
	w.mustSetRoles(w.alice, s, a, grant(w.owner.id, access.AppUser), grant(w.alice.id, access.AppBuilder))
	w.add(w.owner, s, a, "owner-row", "2")

	agent := func(u *user) Actor { return Actor{UserID: u.id, Agent: true} }
	// The user's agent reads their records only.
	rows, err := w.e.s.ListRows(ctx, agent(w.owner), s.ID, a.ID)
	if err != nil || len(rows) != 1 || rows[0]["description"] != "owner-row" {
		t.Fatalf("the user's agent reads %v, %v", rows, err)
	}
	// And can't change the app, though the space would let an editor.
	name := "By the agent"
	if _, err := w.e.s.UpdateApp(ctx, agent(w.owner), s.ID, a.ID, Changes{Version: 1, Name: &name}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("update by a user's agent = %v, want ErrForbidden", err)
	}
	shown, err := w.e.s.ShowApp(ctx, agent(w.owner), s.ID, a.ID)
	if err != nil || shown.Rows != 1 || shown.Access.Design {
		t.Fatalf("show_app for the user's agent = %+v, %v", shown, err)
	}
	// The builder's agent can, and reads no records.
	if _, err := w.e.s.UpdateApp(ctx, agent(w.alice), s.ID, a.ID, Changes{Version: 1, Name: &name}); err != nil {
		t.Fatal(err)
	}
	if rows, err := w.e.s.ListRows(ctx, agent(w.alice), s.ID, a.ID); err != nil || len(rows) != 0 {
		t.Fatalf("the builder's agent reads %v, %v", rows, err)
	}
	// Others' apps and spaces stay out of reach, whatever the roles.
	if _, err := w.e.s.ListRows(ctx, agent(w.mallory), s.ID, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a non-member's agent = %v, want ErrNotFound", err)
	}
	if _, err := w.e.s.ListRows(ctx, agent(w.alice), w.alicePrivate.ID, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an app through another space = %v, want ErrNotFound", err)
	}
	// A definition holds no roles: an app can't carry its own.
	def := map[string]any{"name": "Sneaky", "definition": map[string]any{}}
	raw := example(t)
	def["definition"] = map[string]any{"components": raw.Components, "collection": raw.Collection, "actions": raw.Actions, "roles": []string{"builder"}}
	expect(t, w.alice.do(http.MethodPost, appsPath(s, ""), def), http.StatusBadRequest)
}

// Restricting an app restricts it for everyone: a member it names nowhere, with no default,
// holds no role and doesn't see the app, rather than keeping every role.
func TestUnnamedMembersOfARestrictedAppSeeNothing(t *testing.T) {
	w := newWorld(t)
	s, a := w.shared, w.sharedApp
	w.mustSetRoles(w.alice, s, a, grant(w.alice.id, access.AppBuilder), grant(w.owner.id, access.AppUser))
	// Bob reads the space but is named nowhere: the app is as unseen as one of another space.
	expect(t, w.bob.do(http.MethodGet, appPath(s, a.ID, ""), nil), http.StatusNotFound)
	expect(t, w.bob.do(http.MethodGet, appPath(s, a.ID, "/surface"), nil), http.StatusNotFound)
	rec := w.bob.do(http.MethodGet, appsPath(s, ""), nil)
	expect(t, rec, http.StatusOK)
	for _, ap := range decode[struct{ Apps []App }](t, rec).Apps {
		if ap.ID == a.ID {
			t.Fatalf("bob's list shows the app: %s", rec.Body)
		}
	}
	// A default brings unnamed members back, with its roles only.
	w.mustSetRoles(w.alice, s, a, grant(w.alice.id, access.AppBuilder), fallback(access.AppReader))
	if got := w.view(w.bob, s, a).Access; !got.View || got.ReadRecords != access.ScopeAll || got.Submit {
		t.Fatalf("bob with the default = %+v", got)
	}
}

// A builder who can't read the records can't change the collection: its check against every
// row would name the rows that don't fit, and a bound tightened again and again would read
// their values out. The rest of the definition stays theirs to change.
func TestBuilderWithoutDataCantProbeRowsThroughTheCollection(t *testing.T) {
	w := newWorld(t)
	s, a := w.shared, w.sharedApp
	w.mustSetRoles(w.alice, s, a, grant(w.alice.id, access.AppBuilder))
	cur := w.view(w.alice, s, a)
	if cur.Access.ReadRecords != access.ScopeNone {
		t.Fatalf("builder reads %s", cur.Access.ReadRecords)
	}
	patch := func(def *Definition) *httptest.ResponseRecorder {
		return w.alice.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": cur.Version, "definition": def})
	}
	// "team-lunch" has amount 3: a max of 1 would not fit it.
	tight := *cur.Definition
	tight.Collection.Fields = append([]Field(nil), cur.Definition.Collection.Fields...)
	one := 1.0
	for i := range tight.Collection.Fields {
		if tight.Collection.Fields[i].Name == "amount" {
			tight.Collection.Fields[i].Max = &one
		}
	}
	rec := patch(&tight)
	expectForbidden(t, rec, "read_records")
	if strings.Contains(rec.Body.String(), "row") {
		t.Fatalf("the refusal tells of rows: %s", rec.Body)
	}
	// The same collection: allowed, and nothing to tell.
	expect(t, patch(cur.Definition), http.StatusOK)
	// With the data, the collection is theirs to change, and its check is theirs to see.
	cur = w.view(w.alice, s, a)
	w.mustSetRoles(w.alice, s, a, grant(w.alice.id, access.AppBuilder, access.AppDataManager))
	expect(t, w.alice.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": cur.Version, "definition": &tight}), http.StatusBadRequest)
}

// Roles are for members of the app's space: leaving it drops them, so rejoining, or the app
// moving to a space where they are, gives back nothing; and a moved app leaves its roles.
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
	// Back as a viewer: no role of his own, and the app is restricted, so he doesn't see it.
	w.e.addMember(w.owner, s, w.mallory, access.RoleViewer)
	expect(t, w.mallory.do(http.MethodGet, appPath(s, a.ID, ""), nil), http.StatusNotFound)

	// A moved app leaves its roles behind; the new space's access decides.
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
