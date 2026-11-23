package apps

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
)

func templatesPath(ws int64) string { return fmt.Sprintf("/api/workspaces/%d/app-templates", ws) }

func installPath(s access.Space, id string) string {
	return fmt.Sprintf("/api/workspaces/%d/spaces/%d/app-templates/%s/install", s.WorkspaceID, s.ID, id)
}

// install installs template id into s as u.
func (w *world) install(u *user, s access.Space, id string, body any) *httptest.ResponseRecorder {
	w.e.t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	return u.do(http.MethodPost, installPath(s, id), body)
}

func (w *world) mustInstall(u *user, s access.Space, id string, body any) App {
	w.e.t.Helper()
	rec := w.install(u, s, id, body)
	expect(w.e.t, rec, http.StatusCreated)
	return decode[App](w.e.t, rec)
}

// crew is a shared space where alice and bob both write, so their own-record roles can be told apart.
func (w *world) crew() access.Space {
	s := w.e.createShared(w.owner, w.ws, "Crew")
	w.e.addMember(w.owner, s, w.alice, access.RoleEditor)
	w.e.addMember(w.owner, s, w.bob, access.RoleEditor)
	return s
}

// surfaceData is the data model of the app's surface as u reads it.
func (w *world) surfaceData(u *user, s access.Space, a App) map[string]any {
	w.e.t.Helper()
	rec := u.do(http.MethodGet, appPath(s, a.ID, "/surface"), nil)
	expect(w.e.t, rec, http.StatusOK)
	msgs := decode[[]struct {
		UpdateDataModel *struct {
			Path  string         `json:"path"`
			Value map[string]any `json:"value"`
		} `json:"updateDataModel"`
	}](w.e.t, rec)
	for _, m := range msgs {
		if m.UpdateDataModel != nil && m.UpdateDataModel.Path == "/" {
			return m.UpdateDataModel.Value
		}
	}
	w.e.t.Fatal("no data model in the surface")
	return nil
}

func TestBundledTemplatesAreValid(t *testing.T) {
	all, err := Templates()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, tpl := range all {
		ids = append(ids, tpl.ID)
		raw, err := json.Marshal(tpl.Definition)
		if err != nil || len(raw) > MaxDefinitionSize {
			t.Errorf("%s: definition is %d bytes (%v)", tpl.ID, len(raw), err)
		}
		// Checked once more as an app's definition is when it is saved.
		if _, err := tpl.Definition.Check(); err != nil {
			t.Errorf("%s: %v", tpl.ID, err)
		}
		if tpl.Definition.Contributions == nil || len(tpl.Definition.Contributions.Widgets) == 0 {
			t.Errorf("%s offers no Home widget", tpl.ID)
		}
	}
	if strings.Join(ids, ",") != "equipment-checkout,expense-tracker,inspection" {
		t.Fatalf("templates = %v", ids)
	}
	infos, _ := TemplateInfos()
	byID := map[string]TemplateInfo{}
	for _, i := range infos {
		byID[i.ID] = i
	}
	if !byID["inspection"].NeedsBoard || byID["expense-tracker"].NeedsBoard || byID["equipment-checkout"].NeedsBoard {
		t.Fatalf("needs_board = %+v", byID)
	}
	if got := strings.Join(byID["inspection"].Captures, ","); got != "location,signature" {
		t.Fatalf("inspection captures = %s", got)
	}
	if got := strings.Join(byID["expense-tracker"].Captures, ","); got != "attachment" {
		t.Fatalf("expense captures = %s", got)
	}
	if got := strings.Join(byID["equipment-checkout"].Captures, ","); got != "barcode" {
		t.Fatalf("equipment captures = %s", got)
	}
}

func TestATemplateThatDoesNotCheckIsNotLoaded(t *testing.T) {
	good := `{"template_version": 1, "id": "x", "version": 1, "name": "X", "description": "d", "recommended_space": "private",
		"definition": {"components": [{"id": "root", "component": "Text", "text": "x"}], "collection": {"fields": []}, "actions": {"a": {"kind": "insert"}}}}`
	var tpl Template
	if err := json.Unmarshal([]byte(good), &tpl); err != nil || tpl.check() != nil {
		t.Fatalf("the good one: %v", err)
	}
	for name, mutate := range map[string]func(*Template){
		"a future format":      func(t *Template) { t.TemplateVersion = 2 },
		"a bad id":             func(t *Template) { t.ID = "Bad Id" },
		"no description":       func(t *Template) { t.Description = " " },
		"no space":             func(t *Template) { t.RecommendedSpace = "" },
		"an installer can't":   func(t *Template) { t.Roles = &TemplateRoles{Installer: []access.AppRole{access.AppUser}} },
		"an unknown component": func(t *Template) { t.Definition.Components[0] = json.RawMessage(`{"id":"root","component":"Iframe"}`) },
	} {
		var c Template
		_ = json.Unmarshal([]byte(good), &c)
		mutate(&c)
		if err := c.check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTemplatesAreListedToMembersOnly(t *testing.T) {
	w := newWorld(t)
	rec := w.alice.do(http.MethodGet, templatesPath(w.ws), nil)
	expect(t, rec, http.StatusOK)
	if got := decode[struct{ Templates []TemplateInfo }](t, rec).Templates; len(got) != 3 {
		t.Fatalf("templates = %+v", got)
	}
	// Someone who left the workspace, and a request with no session.
	expect(t, w.stranger.do(http.MethodGet, templatesPath(w.ws), nil), http.StatusNotFound)
	anon := &user{e: w.e}
	if rec := anon.do(http.MethodGet, templatesPath(w.ws), nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", rec.Code)
	}
}

func TestInstallingMakesAnIndependentDraftWithNoData(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.mustInstall(w.alice, s, "expense-tracker", nil)
	if a.Name != "Expense tracker" || a.Live != nil || a.Rows != 0 || !a.UnpublishedChanges || a.NeedsBoard {
		t.Fatalf("installed = %+v", a)
	}
	if a.Draft == nil && a.Access.Design {
		// The create reply carries no definition; the app read does.
		a = w.view(w.alice, s, a)
	}
	// A private space has one member: nothing to restrict, and no role rows.
	var roles int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM app_roles WHERE app_id = ?`, a.ID).Scan(&roles); err != nil || roles != 0 {
		t.Fatalf("roles in a private space: %d %v", roles, err)
	}
	// Not launchable until published, through the ordinary route.
	expect(t, w.alice.do(http.MethodGet, appPath(s, a.ID, "/surface"), nil), http.StatusConflict)
	published := w.publish(w.alice, s, a)
	if published.Live == nil {
		t.Fatal("not live after publishing")
	}
	// Installing twice makes two apps that share nothing.
	b := w.mustInstall(w.alice, s, "expense-tracker", map[string]any{"name": "Trip costs"})
	if b.ID == a.ID || b.Name != "Trip costs" {
		t.Fatalf("second = %+v", b)
	}
	expect(t, w.alice.do(http.MethodPatch, appPath(s, b.ID, ""), map[string]any{"version": b.Version, "name": "Renamed"}), http.StatusOK)
	if got := w.view(w.alice, s, a); got.Name != "Expense tracker" || got.Live == nil {
		t.Fatalf("the first changed with the second: %+v", got)
	}
	// Nothing else came with it: no record, upload, history or operation of any app.
	for table, want := range map[string]int{"app_rows": w.tableCount("app_rows"), "app_attachments": 0, "app_effects": 0, "app_operations": 0} {
		if got := w.tableCount(table); got != want {
			t.Errorf("%s has %d rows after installing", table, got)
		}
	}
}

func (w *world) tableCount(table string) int {
	var n int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		w.e.t.Fatal(err)
	}
	return n
}

func TestInstallRefusals(t *testing.T) {
	w := newWorld(t)
	// A space they may only read, one they can't see, one of another workspace, and an unknown template.
	expect(t, w.bob.do(http.MethodPost, installPath(w.shared, "expense-tracker"), map[string]any{}), http.StatusForbidden)
	expect(t, w.mallory.do(http.MethodPost, installPath(w.shared, "expense-tracker"), map[string]any{}), http.StatusNotFound)
	expect(t, w.alice.do(http.MethodPost, installPath(w.alicePrivate, "nope"), map[string]any{}), http.StatusNotFound)
	expect(t, w.stranger.do(http.MethodPost, installPath(w.otherShared, "expense-tracker"), map[string]any{}), http.StatusCreated)
	expect(t, w.alice.do(http.MethodPost, installPath(w.otherShared, "expense-tracker"), map[string]any{}), http.StatusNotFound)
	// Someone else's private space is as unseen as any.
	expect(t, w.bob.do(http.MethodPost, installPath(w.alicePrivate, "expense-tracker"), map[string]any{}), http.StatusNotFound)
	// Nothing was made in them.
	for _, s := range []access.Space{w.shared, w.alicePrivate} {
		for _, a := range decode[struct{ Apps []App }](t, w.alice.do(http.MethodGet, appsPath(s, ""), nil)).Apps {
			if strings.Contains(a.Name, "Expense tracker") {
				t.Errorf("an app was installed in %s: %+v", s.Name, a)
			}
		}
	}
	// A template that takes no board takes none; a body with other keys is refused.
	expectCode(t, w.alice.do(http.MethodPost, installPath(w.alicePrivate, "expense-tracker"), map[string]any{"board": map[string]any{"space_id": 1, "board_id": 1}}),
		http.StatusBadRequest, "app_template_board")
	expect(t, w.alice.do(http.MethodPost, installPath(w.alicePrivate, "expense-tracker"), map[string]any{"definition": map[string]any{}}), http.StatusBadRequest)
}

func TestAnExpensePhotoIsPrivateToItsOwner(t *testing.T) {
	w := newWorld(t)
	s := w.crew()
	a := w.mustInstall(w.alice, s, "expense-tracker", nil)
	// The installer holds their own roles; every other member the default, which reads own records only.
	grants := decode[struct{ Grants []access.AppGrant }](t, w.alice.do(http.MethodGet, appPath(s, a.ID, "/roles"), nil)).Grants
	if len(grants) != 2 || *grants[0].UserID != w.alice.id || grants[1].UserID != nil ||
		fmt.Sprint(grants[0].Roles) != "[builder publisher user]" || fmt.Sprint(grants[1].Roles) != "[user]" {
		t.Fatalf("grants = %+v", grants)
	}
	a = w.publish(w.alice, s, w.view(w.alice, s, a))

	spend := func(u *user, description string, amount float64) (photo string) {
		photo = w.upload(u, s, a, "receipt", testPNG(t, 120, 80), 4000)
		rec := u.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "add_expense", map[string]any{
			"description": description, "amount": amount, "spent_on": "2026-11-02", "category": "food", "receipt": []any{photo}}))
		expect(t, rec, http.StatusOK)
		return photo
	}
	alicePhoto := spend(w.alice, "Team lunch", 40)
	bobPhoto := spend(w.bob, "Taxi", 15.5)

	expect(t, w.fetch(w.alice, s, a, alicePhoto, false), http.StatusOK)
	expect(t, w.fetch(w.alice, s, a, alicePhoto, true), http.StatusOK)
	expect(t, w.fetch(w.bob, s, a, bobPhoto, false), http.StatusOK)
	// The other member can't read it, nor its thumbnail, though they write the same space.
	for _, thumb := range []bool{false, true} {
		if rec := w.fetch(w.bob, s, a, alicePhoto, thumb); rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "PNG") {
			t.Errorf("bob read alice's receipt (thumbnail %v): %d", thumb, rec.Code)
		}
		if rec := w.fetch(w.alice, s, a, bobPhoto, thumb); rec.Code == http.StatusOK {
			t.Errorf("alice read bob's receipt (thumbnail %v): %d", thumb, rec.Code)
		}
	}
	// Nor can they see the records: each reads their own.
	if items := w.items(w.bob, s, a); len(items) != 1 || items[0]["description"] != "Taxi" {
		t.Fatalf("bob's items = %v", items)
	}
	if items := w.items(w.alice, s, a); len(items) != 1 || items[0]["description"] != "Team lunch" {
		t.Fatalf("alice's items = %v", items)
	}
	// Someone outside the space has nothing either.
	expect(t, w.fetch(w.mallory, s, a, alicePhoto, false), http.StatusNotFound)
	// The sums are over the records each reads.
	if got := w.surfaceData(w.bob, s, a)["total"]; got != "15.50" {
		t.Fatalf("bob's total = %v", got)
	}
}

func TestTheMonthlySumCountsTheCurrentMonthOnly(t *testing.T) {
	w := newWorld(t)
	was := currentMonth
	currentMonth = func() string { return "2026-11" }
	t.Cleanup(func() { currentMonth = was })
	s := w.alicePrivate
	a := w.publish(w.alice, s, w.mustInstall(w.alice, s, "expense-tracker", nil))
	for _, e := range []struct {
		date   string
		amount float64
	}{{"2026-11-02", 10}, {"2026-10-31", 5}, {"2026-11-30", 2.5}, {"2025-11-15", 100}} {
		expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "add_expense", map[string]any{
			"description": "x", "amount": e.amount, "spent_on": e.date, "category": "other"})), http.StatusOK)
	}
	data := w.surfaceData(w.alice, s, a)
	if data["month_total"] != "12.50" || data["total"] != "117.50" {
		t.Fatalf("summaries = %v", data)
	}
	// The Home widget shows the same.
	widget, code := w.widgetData(w.alice, s, a, "spending")
	if code != http.StatusOK || widget["month_total"] != "12.50" {
		t.Fatalf("widget = %v (%d)", widget, code)
	}
	// month_of names a date field.
	d := example(t)
	d.Summaries = map[string]Summary{"bad": {Kind: "sum", Field: "amount", MonthOf: "description"}}
	if _, err := d.Check(); err == nil {
		t.Fatal("month_of over a text field was accepted")
	}
}

func TestEquipmentOwnRecordVisibility(t *testing.T) {
	w := newWorld(t)
	s := w.crew()
	a := w.mustInstall(w.alice, s, "equipment-checkout", nil)
	a = w.publish(w.alice, s, w.view(w.alice, s, a))
	checkOut := func(u *user, tag string) *httptest.ResponseRecorder {
		return u.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "check_out", map[string]any{
			"equipment": tag, "borrower": "Someone", "checked_out_on": "2026-11-20", "condition": "good", "status": "out"}))
	}
	expect(t, checkOut(w.alice, "TAG-0001"), http.StatusOK)
	expect(t, checkOut(w.bob, "TAG-0002"), http.StatusOK)
	expect(t, checkOut(w.bob, "TAG-0003"), http.StatusOK)

	// The plain user reads their own checkouts and counts them; the data manager reads all.
	if items := w.items(w.bob, s, a); len(items) != 2 {
		t.Fatalf("bob reads %d records, want his 2", len(items))
	}
	if items := w.items(w.alice, s, a); len(items) != 3 {
		t.Fatalf("the data manager reads %d records, want 3", len(items))
	}
	if got := w.surfaceData(w.bob, s, a)["out"]; got != "2" {
		t.Fatalf("bob's count = %v", got)
	}
	if got := w.surfaceData(w.alice, s, a)["out"]; got != "3" {
		t.Fatalf("alice's count = %v", got)
	}
	if got := w.view(w.bob, s, a); got.Rows != 2 || got.Access.ReadRecords != access.ScopeOwn || got.Access.Design || got.Access.Publish {
		t.Fatalf("bob's view = %+v", got.Access)
	}
	if got := w.view(w.alice, s, a); got.Rows != 3 || got.Access.ReadRecords != access.ScopeAll {
		t.Fatalf("alice's view = %+v", got.Access)
	}
	// The explorer's query answers to the same scope.
	q := decode[QueryResult](t, w.bob.do(http.MethodGet, appPath(s, a.ID, "/records/query"), nil))
	if len(q.Records) != 2 || q.Total != 2 {
		t.Fatalf("bob.s query = %+v", q)
	}
	// A plain user can't change the app or its roles.
	bob := w.view(w.bob, s, a)
	expect(t, w.bob.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": bob.Version, "name": "Mine"}), http.StatusForbidden)
	expect(t, w.bob.do(http.MethodGet, appPath(s, a.ID, "/roles"), nil), http.StatusForbidden)

	// Returning: the date is needed with the status, only their own item can be returned by a user,
	// and the data manager may return any.
	aliceItem := w.items(w.alice, s, a)[0]["id"]
	bobItem := w.items(w.bob, s, a)[0]["id"]
	ret := func(u *user, id any, ctx map[string]any) *httptest.ResponseRecorder {
		c := map[string]any{"id": id, "status": "returned"}
		for k, v := range ctx {
			c[k] = v
		}
		return u.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "return_item", c))
	}
	if rec := ret(w.bob, bobItem, nil); rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusBadRequest {
		t.Fatalf("returning with no date: %d %s", rec.Code, rec.Body)
	}
	if rec := ret(w.bob, aliceItem, map[string]any{"returned_on": "2026-11-21"}); rec.Code == http.StatusOK {
		t.Fatalf("bob returned alice's item: %s", rec.Body)
	}
	expect(t, ret(w.bob, bobItem, map[string]any{"returned_on": "2026-11-21"}), http.StatusOK)
	if got := w.surfaceData(w.bob, s, a)["out"]; got != "1" {
		t.Fatalf("bob's count after returning = %v", got)
	}
	// The data manager returns the other one of his, and her own.
	bobOther := w.items(w.alice, s, a)[2]["id"]
	expect(t, ret(w.alice, bobOther, map[string]any{"returned_on": "2026-11-22"}), http.StatusOK)
	if got := w.surfaceData(w.bob, s, a)["out"]; got != "0" {
		t.Fatalf("bob's count after the data manager returned his item = %v", got)
	}
	if got := w.surfaceData(w.alice, s, a)["out"]; got != "1" {
		t.Fatalf("alice's count = %v", got)
	}
	expect(t, ret(w.alice, aliceItem, map[string]any{"returned_on": "2026-11-22"}), http.StatusOK)
}

func TestInspectionNeedsABoardTheInstallerMayWrite(t *testing.T) {
	f := newFX(t)
	s := f.shared
	// No board, a board that isn't there, one in a space the installer can't write, and one of
	// another workspace: none installs anything.
	before := len(decode[struct{ Apps []App }](t, f.alice.do(http.MethodGet, appsPath(s, ""), nil)).Apps)
	expectCode(t, f.install(f.alice, s, "inspection", nil), http.StatusBadRequest, "app_template_board")
	expectCode(t, f.install(f.alice, s, "inspection", map[string]any{"board": map[string]any{"space_id": f.maint.ID, "board_id": f.board + 99}}),
		http.StatusBadRequest, "app_effect_target")
	expectCode(t, f.install(f.alice, s, "inspection", map[string]any{"board": map[string]any{"space_id": f.alicePrivate.ID + 99, "board_id": 1}}),
		http.StatusBadRequest, "app_effect_target")
	expectCode(t, f.install(f.alice, s, "inspection", map[string]any{"board": map[string]any{"space_id": f.otherShared.ID, "board_id": 1}}),
		http.StatusBadRequest, "app_effect_target")
	// A board of a space the installer only reads is refused, though the space they install into is theirs.
	ops := f.e.createShared(f.owner, f.ws, "Ops")
	f.e.addMember(f.owner, ops, f.alice, access.RoleViewer)
	var opsBoard int64
	if err := f.e.db.QueryRow(`INSERT INTO boards (space_id, kind, name, created_by, updated_by) VALUES (?, 'team', 'Ops', ?, ?) RETURNING id`,
		ops.ID, f.owner.id, f.owner.id).Scan(&opsBoard); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.install(f.alice, s, "inspection", map[string]any{"board": map[string]any{"space_id": ops.ID, "board_id": opsBoard}}),
		http.StatusBadRequest, "app_effect_target")
	if after := len(decode[struct{ Apps []App }](t, f.alice.do(http.MethodGet, appsPath(s, ""), nil)).Apps); after != before {
		t.Fatalf("a refused install left %d apps behind", after-before)
	}
	// Half a board is no board.
	expect(t, f.install(f.alice, s, "inspection", map[string]any{"board": map[string]any{"space_id": f.maint.ID}}), http.StatusBadRequest)
}

// inspect presses the template's submit button as u.
func (f *fx) inspect(u *user, a App, operation, site, result string, extra map[string]any) *httptest.ResponseRecorder {
	f.e.t.Helper()
	ctx := map[string]any{"site": site, "result": result}
	for k, v := range extra {
		ctx[k] = v
	}
	act := action(a, "submit_inspection", ctx)
	act.Operation = operation
	return u.do(http.MethodPost, appPath(f.shared, a.ID, "/actions"), act)
}

func TestAFailedInspectionMakesOneCardAndMovesTheWidget(t *testing.T) {
	f := newFX(t)
	s := f.shared
	app := f.mustInstall(f.alice, s, "inspection", map[string]any{"board": map[string]any{"space_id": f.maint.ID, "board_id": f.board}})
	if !app.Access.Publish {
		t.Fatalf("access = %+v", app.Access)
	}
	// The installer is told which board; it is a draft, so nothing is made by anything yet.
	if app.NeedsBoard {
		t.Fatal("a board was chosen, and the app still says it needs one")
	}
	app = f.publish(f.alice, s, f.view(f.alice, s, app))
	cardsBefore := len(f.cards())
	data := func() map[string]any {
		d, code := f.widgetData(f.alice, s, app, "failures")
		if code != http.StatusOK {
			t.Fatalf("widget = %d", code)
		}
		return d
	}
	if d := data(); d["open_failures"] != "0" || d["total"] != "0" {
		t.Fatalf("widget before = %v", d)
	}
	capture := func(name string) map[string]any {
		return map[string]any{"place": where(nil), "signed": f.upload(f.alice, s, app, "signed", testPNG(t, 300, 100), 6000), "notes": name}
	}

	// A pass makes no card; a fail without notes is refused, and nothing is stored or queued.
	expect(t, f.inspect(f.alice, app, "op-pass-0001", "North gate", "pass", capture("")), http.StatusOK)
	bad := capture("")
	delete(bad, "notes")
	if rec := f.inspect(f.alice, app, "op-fail-0000", "East gate", "fail", bad); rec.Code == http.StatusOK {
		t.Fatalf("a failure without notes was accepted: %s", rec.Body)
	}
	f.work()
	if got := len(f.cards()); got != cardsBefore {
		t.Fatalf("cards = %d after a pass and a refusal", got)
	}
	if d := data(); d["open_failures"] != "0" || d["total"] != "1" {
		t.Fatalf("widget after a pass = %v", d)
	}

	// A failure makes exactly one card, however often the request is sent again.
	failure := capture("Latch is broken")
	for i := 0; i < 3; i++ {
		expect(t, f.inspect(f.alice, app, "op-fail-0001", "East gate", "fail", failure), http.StatusOK)
	}
	f.work()
	f.work()
	cards := f.cards()
	if len(cards) != cardsBefore+1 {
		t.Fatalf("cards = %+v, want one more than before", cards)
	}
	if last := cards[len(cards)-1]; !strings.HasPrefix(last.Title, "Failed inspection: East gate (#") || last.Column != "todo" {
		t.Fatalf("card = %+v", last)
	}
	if d := data(); d["open_failures"] != "1" || d["total"] != "2" {
		t.Fatalf("widget after a failure = %v", d)
	}
	// Another failure is another card, and the widget counts both.
	expect(t, f.inspect(f.alice, app, "op-fail-0002", "West gate", "fail", capture("Cracked")), http.StatusOK)
	f.work()
	if got := len(f.cards()); got != cardsBefore+2 {
		t.Fatalf("cards = %d", got)
	}
	if d := data(); d["open_failures"] != "2" {
		t.Fatalf("widget = %v", d)
	}
	// Fixing one lowers the count and makes no card.
	id := f.items(f.alice, s, app)[2]["id"]
	expect(t, f.alice.do(http.MethodPost, appPath(s, app.ID, "/actions"), action(app, "close_inspection", map[string]any{"id": id, "status": "closed"})), http.StatusOK)
	f.work()
	if d := data(); d["open_failures"] != "1" {
		t.Fatalf("widget after fixing = %v", d)
	}
	if got := len(f.cards()); got != cardsBefore+2 {
		t.Fatalf("cards = %d after fixing", got)
	}
	// What the installer's own app made is the only effect there is: the template's.
	if n := f.count(`SELECT count(*) FROM app_effects WHERE app_id = ?`, app.ID); n != 2 {
		t.Fatalf("effects of the app = %d", n)
	}
}

func TestACopyWithoutABoardCannotBePublishedUntilOneIsChosen(t *testing.T) {
	f := newFX(t)
	f.e.addMember(f.owner, f.shared, f.mallory, access.RoleEditor)
	// Mallory writes Team and nothing of Maintenance. A board of hers is somewhere she may write.
	var mine int64
	if err := f.e.db.QueryRow(`INSERT INTO boards (space_id, kind, name, created_by, updated_by) VALUES (?, 'team', 'Mine', ?, ?) RETURNING id`,
		f.shared.ID, f.owner.id, f.owner.id).Scan(&mine); err != nil {
		t.Fatal(err)
	}
	f.mustSubmit(f.alice, "op-fail-0001", "East gate", "fail")
	f.work()
	var recordsBefore, filesBefore, historyBefore, effectsBefore int
	_ = f.e.db.QueryRow(`SELECT count(*) FROM app_rows`).Scan(&recordsBefore)
	_ = f.e.db.QueryRow(`SELECT count(*) FROM app_row_history`).Scan(&historyBefore)
	_ = f.e.db.QueryRow(`SELECT count(*) FROM app_attachments`).Scan(&filesBefore)
	_ = f.e.db.QueryRow(`SELECT count(*) FROM app_effects`).Scan(&effectsBefore)

	rec := f.mallory.do(http.MethodPost, appPath(f.shared, f.app.ID, "/duplicate"), map[string]any{"name": "My inspections"})
	expect(t, rec, http.StatusCreated)
	if strings.Contains(rec.Body.String(), "East gate") || strings.Contains(rec.Body.String(), "leaking") {
		t.Fatalf("the copy carries a record: %s", rec.Body)
	}
	copyApp := decode[App](t, rec)
	if copyApp.ID == f.app.ID || copyApp.Name != "My inspections" || copyApp.Live != nil || copyApp.Rows != 0 || !copyApp.NeedsBoard {
		t.Fatalf("copy = %+v", copyApp)
	}
	// Only the definition was copied.
	for table, want := range map[string]int{"app_rows": recordsBefore, "app_row_history": historyBefore, "app_attachments": filesBefore, "app_effects": effectsBefore} {
		if got := f.tableCount(table); got != want {
			t.Errorf("%s: %d rows after copying, was %d", table, got, want)
		}
	}
	if n := f.count(`SELECT count(*) FROM app_roles WHERE app_id = ` + fmt.Sprint(copyApp.ID)); n != 0 {
		t.Fatalf("the copy holds %d role rows", n)
	}
	got := f.view(f.mallory, f.shared, copyApp)
	raw, _ := json.Marshal(got.Draft)
	if strings.Contains(string(raw), fmt.Sprintf(`"space_id":%d`, f.maint.ID)) || strings.Contains(string(raw), fmt.Sprintf(`"board_id":%d`, f.board)) {
		t.Fatalf("the copy still names the original's targets: %s", raw)
	}
	// The effects' aims stay, so choosing a board is all that is left.
	if !strings.Contains(string(raw), "Fix {{site}}") || !strings.Contains(string(raw), `"create_card"`) {
		t.Fatalf("the copy lost its effects: %s", raw)
	}

	// Publishing waits for a board; the copier can't choose the original's, but can choose their own.
	expectCode(t, f.mallory.do(http.MethodPost, appPath(f.shared, copyApp.ID, "/publish"), map[string]any{"version": got.Version, "acknowledge": []SchemaChange{}}),
		http.StatusBadRequest, "app_effect_target_unset")
	choose := func(space, board int64) *httptest.ResponseRecorder {
		return f.mallory.do(http.MethodPost, appPath(f.shared, copyApp.ID, "/effect-board"),
			map[string]any{"version": got.Version, "space_id": space, "board_id": board})
	}
	expectCode(t, choose(f.maint.ID, f.board), http.StatusBadRequest, "app_effect_target")
	chosen := choose(f.shared.ID, mine)
	expect(t, chosen, http.StatusOK)
	next := decode[App](t, chosen)
	if next.NeedsBoard || next.Version <= got.Version {
		t.Fatalf("after choosing = %+v", next)
	}
	// A second choice finds nothing waiting.
	expect(t, f.mallory.do(http.MethodPost, appPath(f.shared, copyApp.ID, "/effect-board"),
		map[string]any{"version": next.Version, "space_id": f.shared.ID, "board_id": mine}), http.StatusBadRequest)
	expect(t, f.mallory.do(http.MethodPost, appPath(f.shared, copyApp.ID, "/publish"), map[string]any{"version": next.Version, "acknowledge": []SchemaChange{}}), http.StatusOK)

	// The source is untouched.
	if src := f.view(f.alice, f.shared, f.app); src.Version != f.app.Version+0 && src.Rows != 1 {
		t.Fatalf("source = %+v", src)
	}
	// Only someone who may design the app chooses its board.
	bobCopy := f.view(f.alice, f.shared, copyApp)
	expect(t, f.bob.do(http.MethodPost, appPath(f.shared, copyApp.ID, "/effect-board"), map[string]any{"version": bobCopy.Version, "space_id": f.shared.ID, "board_id": mine}), http.StatusForbidden)
}

func TestDuplicatingLeaksNothingTheCopierCannotRead(t *testing.T) {
	f := newFX(t)
	s := f.shared
	f.e.addMember(f.owner, s, f.mallory, access.RoleEditor)
	// A record with a photo of the receipt kind: the source is a restricted app.
	f.mustSubmit(f.alice, "op-fail-0001", "East gate", "fail")
	f.mustSetRolesFor()
	// Mallory holds no role in the restricted source: it isn't there for her, copying included.
	expect(t, f.mallory.do(http.MethodPost, appPath(s, f.app.ID, "/duplicate"), map[string]any{}), http.StatusNotFound)
	// An outsider and another workspace's member: the same.
	outsider := f.stranger
	expect(t, outsider.do(http.MethodPost, appPath(s, f.app.ID, "/duplicate"), map[string]any{}), http.StatusNotFound)
	expect(t, f.mallory.do(http.MethodPost, appPath(in(f.other, s), f.app.ID, "/duplicate"), map[string]any{}), http.StatusNotFound)

	// Bob, a plain user of the source, reads only his own records and sees the live definition: a
	// copy of that, in a space he may write, has none of the records he can't read.
	crew := f.e.createShared(f.owner, f.ws, "Elsewhere")
	f.e.addMember(f.owner, crew, f.bob, access.RoleEditor)
	rec := f.bob.do(http.MethodPost, appPath(s, f.app.ID, "/duplicate"), map[string]any{"space_id": crew.ID})
	expect(t, rec, http.StatusCreated)
	if strings.Contains(rec.Body.String(), "East gate") {
		t.Fatalf("a record is in the copy: %s", rec.Body)
	}
	copyApp := decode[App](t, rec)
	if copyApp.SpaceID != crew.ID || copyApp.Rows != 0 || copyApp.Live != nil {
		t.Fatalf("copy = %+v", copyApp)
	}
	if n := f.count(fmt.Sprintf(`SELECT count(*) FROM app_rows WHERE app_id = %d`, copyApp.ID)); n != 0 {
		t.Fatalf("copy has %d rows", n)
	}
	// His roles in the source don't come along: the copy is unrestricted, so in a space he writes
	// he has what the space gives him, and the source's other members have nothing in it.
	if copyApp.Access.Restricted {
		t.Fatalf("the copy kept the source's restrictions: %+v", copyApp.Access)
	}
	// A plain user can't ask for the draft, only the live release.
	expect(t, f.bob.do(http.MethodPost, appPath(s, f.app.ID, "/duplicate"), map[string]any{"source": "draft", "space_id": crew.ID}), http.StatusForbidden)
	expect(t, f.bob.do(http.MethodPost, appPath(s, f.app.ID, "/duplicate"), map[string]any{"source": "later", "space_id": crew.ID}), http.StatusBadRequest)
	// A space he can't write, one he can't see, and one that is read-only for him.
	expect(t, f.bob.do(http.MethodPost, appPath(s, f.app.ID, "/duplicate"), map[string]any{"space_id": f.alicePrivate.ID}), http.StatusNotFound)
	expect(t, f.bob.do(http.MethodPost, appPath(s, f.app.ID, "/duplicate"), map[string]any{"space_id": f.maint.ID}), http.StatusForbidden)
	// An app with no release has nothing for them to copy.
	draft := f.draft(f.alice, s, "Draft only")
	expect(t, f.bob.do(http.MethodPost, appPath(s, draft.ID, "/duplicate"), map[string]any{"space_id": crew.ID}), http.StatusConflict)
	// A viewer of the source's own space may not copy into it.
	expect(t, f.bob.do(http.MethodPost, appPath(s, f.app.ID, "/duplicate"), map[string]any{}), http.StatusForbidden)
	// No copy touched the source.
	if n := f.count(fmt.Sprintf(`SELECT count(*) FROM app_rows WHERE app_id = %d`, f.app.ID)); n != 1 {
		t.Fatalf("the source has %d rows", n)
	}
}

// mustSetRolesFor makes the inspections app restricted: alice reads all and may do everything, bob
// reads his own records, and no one else holds a role.
func (f *fx) mustSetRolesFor() {
	f.w().mustSetRoles(f.alice, f.shared, f.app,
		grant(f.alice.id, access.AppBuilder, access.AppPublisher, access.AppDataManager),
		grant(f.bob.id, access.AppUser))
}

func TestDuplicatingAnAppWithAPhotoCopiesNoPhoto(t *testing.T) {
	w := newWorld(t)
	s := w.crew()
	a := w.publish(w.alice, s, w.mustInstall(w.alice, s, "expense-tracker", nil))
	photo := w.upload(w.alice, s, a, "receipt", testPNG(t, 100, 100), 4000)
	expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "add_expense", map[string]any{
		"description": "Secret gift", "amount": 99, "spent_on": "2026-11-02", "category": "other", "receipt": []any{photo}})), http.StatusOK)
	files := w.files(s, a)

	// Bob has his own role in the app (the default: own records), and copies it into his private space.
	rec := w.bob.do(http.MethodPost, appPath(s, a.ID, "/duplicate"), map[string]any{"space_id": w.bobPrivate.ID})
	expect(t, rec, http.StatusCreated)
	copyApp := decode[App](t, rec)
	if strings.Contains(rec.Body.String(), "Secret gift") || strings.Contains(rec.Body.String(), photo) {
		t.Fatalf("the copy carries the record: %s", rec.Body)
	}
	if copyApp.Rows != 0 || len(w.files(w.bobPrivate, copyApp)) != 0 {
		t.Fatalf("copy = %+v, files %v", copyApp, w.files(w.bobPrivate, copyApp))
	}
	copyApp = w.publish(w.bob, w.bobPrivate, w.view(w.bob, w.bobPrivate, copyApp))
	// The original photo can't be fetched through the copy, nor by its id from anywhere bob reads.
	if rec := w.fetch(w.bob, w.bobPrivate, copyApp, photo, false); rec.Code == http.StatusOK {
		t.Fatalf("the copy served the original's photo: %d", rec.Code)
	}
	if rec := w.fetch(w.bob, s, a, photo, false); rec.Code == http.StatusOK {
		t.Fatalf("bob read alice's photo: %d", rec.Code)
	}
	if got := w.files(s, a); fmt.Sprint(got) != fmt.Sprint(files) {
		t.Fatalf("the source's files changed: %v -> %v", files, got)
	}
	// And the copy is independent: records in it don't reach the source.
	expect(t, w.bob.do(http.MethodPost, appPath(w.bobPrivate, copyApp.ID, "/actions"), action(copyApp, "add_expense", map[string]any{
		"description": "Mine", "amount": 1, "spent_on": "2026-11-03", "category": "food"})), http.StatusOK)
	if items := w.items(w.alice, s, a); len(items) != 1 {
		t.Fatalf("the source has %d records", len(items))
	}
}

// The equipment checkout's installer is its only manager. When they leave the space their roles
// go with them, and the space's administrator can still give someone the data manager role.
func TestASharedInstallIsNeverLeftWithoutSomeoneToManageItsRoles(t *testing.T) {
	w := newWorld(t)
	s := w.crew()
	a := w.publish(w.alice, s, w.mustInstall(w.alice, s, "equipment-checkout", nil))
	if _, err := w.e.db.Exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, s.ID, w.alice.id); err != nil {
		t.Fatal(err)
	}
	if got := w.view(w.bob, s, a); got.Access.ManageRoles || got.Access.ReadRecords != access.ScopeOwn {
		t.Fatalf("bob's access = %+v", got.Access)
	}
	// The workspace admin (the space's creator) reads the roles and hands bob the data manager's.
	expect(t, w.owner.do(http.MethodGet, appPath(s, a.ID, "/roles"), nil), http.StatusOK)
	expect(t, w.owner.do(http.MethodPut, appPath(s, a.ID, "/roles"), map[string]any{"grants": []map[string]any{
		{"user_id": w.bob.id, "roles": []string{"data_manager"}},
		{"user_id": nil, "roles": []string{"user"}},
	}}), http.StatusOK)
	if got := w.view(w.bob, s, a); got.Access.ReadRecords != access.ScopeAll {
		t.Fatalf("bob's access after the grant = %+v", got.Access)
	}
	// The admin's right is to the roles alone: not to design the app or publish it.
	if got := w.view(w.owner, s, a); !got.Access.ManageRoles || got.Access.Design || got.Access.Publish {
		t.Fatalf("the admin's access = %+v", got.Access)
	}
}

// A member who isn't an administrator of the workspace gets nothing from the rule.
func TestOnlyAnAdministratorKeepsTheRightToManageRoles(t *testing.T) {
	w := newWorld(t)
	s := w.crew()
	a := w.publish(w.alice, s, w.mustInstall(w.alice, s, "equipment-checkout", nil))
	expect(t, w.bob.do(http.MethodPut, appPath(s, a.ID, "/roles"), map[string]any{"grants": []map[string]any{
		{"user_id": w.bob.id, "roles": []string{"data_manager"}}}}), http.StatusForbidden)
}

// A value stored before a field was retyped isn't a date, whatever it starts with: it is in no month,
// online or in the rows offline.
func TestAMonthlySumIgnoresValuesThatAreNotDates(t *testing.T) {
	w := newWorld(t)
	was := currentMonth
	currentMonth = func() string { return "2026-11" }
	t.Cleanup(func() { currentMonth = was })
	s := w.alicePrivate
	a := w.publish(w.alice, s, w.mustInstall(w.alice, s, "expense-tracker", nil))
	expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "add_expense", map[string]any{
		"description": "x", "amount": 10, "spent_on": "2026-11-02", "category": "other"})), http.StatusOK)
	for _, bad := range []string{`2026-11-31`, `2026-11-xx`, `2026-11-`} {
		if _, err := w.e.db.Exec(`INSERT INTO app_rows (app_id, data, created_by, updated_by) VALUES (?, ?, ?, ?)`,
			a.ID, `{"description":"old","amount":1000,"spent_on":"`+bad+`","category":"other"}`, w.alice.id, w.alice.id); err != nil {
			t.Fatal(err)
		}
	}
	if got := w.surfaceData(w.alice, s, a)["month_total"]; got != "10.00" {
		t.Fatalf("month_total = %v, want only the row with a real date", got)
	}
}

// A release with a monthly summary needs a client that totals a month: an older one says it must be
// updated instead of showing every month's total as this month's.
func TestAMonthlySummaryRequiresTheCapability(t *testing.T) {
	all, err := Templates()
	if err != nil {
		t.Fatal(err)
	}
	var d Definition
	for _, tpl := range all {
		if tpl.ID == "expense-tracker" {
			d = tpl.Definition
		}
	}
	got, err := d.Check()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.Requires, SummaryMonthCapability) || !slices.Contains(Capabilities(), SummaryMonthCapability) {
		t.Fatalf("requires = %v", got.Requires)
	}
	d, err = cloneDefinition(d)
	if err != nil {
		t.Fatal(err)
	}
	for name, sum := range d.Summaries {
		sum.MonthOf = ""
		d.Summaries[name] = sum
	}
	if got, err = d.Check(); err != nil || slices.Contains(got.Requires, SummaryMonthCapability) {
		t.Fatalf("summaries over every row need no new capability: %v %v", got.Requires, err)
	}
}

// The inspection's signature is required of a real submission, and a preview holds none: its pass and
// fail buttons still work, and a failure shows the one card it would make (and no more).
func TestTheInspectionTemplateWorksInThePreview(t *testing.T) {
	f := newFX(t)
	s := f.shared
	app := f.mustInstall(f.alice, s, "inspection", map[string]any{"board": map[string]any{"space_id": f.maint.ID, "board_id": f.board}})
	start := f.mustPreview(f.alice, s, app, PreviewRequest{})
	step := func(result, notes string) PreviewReply {
		// The place is taken when the form is submitted; the signature is the one thing a preview can't hold.
		ctx := map[string]any{"site": "East gate", "result": result, "place": where(nil)}
		if notes != "" {
			ctx["notes"] = notes
		}
		return f.mustPreview(f.alice, s, app, PreviewRequest{Rows: start.Rows, Action: &ClientAction{
			Name: "submit_inspection", SurfaceID: SurfaceID(app.ID), SourceComponentID: "submit", Context: ctx}})
	}
	if pass := step("pass", ""); len(pass.Effects) != 0 || len(pass.Rows) != len(start.Rows)+1 {
		t.Fatalf("a pass in the preview = %d effects, %d rows", len(pass.Effects), len(pass.Rows))
	}
	fail := step("fail", "Latch is broken")
	if len(fail.Effects) != 1 || fail.Effects[0].Kind != EffectCreateCard || fail.Effects[0].Error != nil ||
		fail.Effects[0].Status != StateSimulated || !strings.HasPrefix(fail.Effects[0].Detail.Title, "Failed inspection: East gate (#") {
		t.Fatalf("a failure in the preview = %+v", fail.Effects)
	}
	// The same through the endpoint that only simulates the effects.
	rec := f.alice.do(http.MethodPost, appPath(s, app.ID, "/actions/preview"), map[string]any{
		"name": "submit_inspection", "surfaceId": SurfaceID(app.ID), "sourceComponentId": "submit",
		"context": map[string]any{"site": "East gate", "result": "fail", "notes": "Latch is broken", "place": where(nil)}})
	expect(t, rec, http.StatusOK)
	if p := decode[Preview](t, rec); len(p.Effects) != 1 || p.Effects[0].Error != nil {
		t.Fatalf("simulated = %+v", p)
	}
	// Nothing real was made.
	f.work()
	if n := f.count(`SELECT count(*) FROM app_rows WHERE app_id = ?`, app.ID) + f.count(`SELECT count(*) FROM app_effects WHERE app_id = ?`, app.ID); n != 0 {
		t.Fatalf("a preview made %d rows", n)
	}
}

// Returning an item in the equipment checkout's preview acts on the preview's sample rows: the app's own
// rows are never read, so a return isn't reported as an error for want of a record the app doesn't hold.
func TestAReturnInThePreviewNeverReadsTheAppsRows(t *testing.T) {
	w := newWorld(t)
	s := w.crew()
	app := w.mustInstall(w.alice, s, "equipment-checkout", nil)
	start := w.mustPreview(w.alice, s, app, PreviewRequest{})
	if len(start.Rows) == 0 {
		t.Fatal("the preview has no sample rows")
	}
	ret := map[string]any{"name": "return_item", "surfaceId": SurfaceID(app.ID), "sourceComponentId": "return",
		"context": map[string]any{"id": start.Rows[0].ID, "status": "returned", "returned_on": "2026-11-21"}}
	rec := w.alice.do(http.MethodPost, appPath(s, app.ID, "/actions/preview"), ret)
	expect(t, rec, http.StatusOK)
	if p := decode[Preview](t, rec); len(p.Effects) != 0 {
		t.Fatalf("effects = %+v", p.Effects)
	}
	// The preview made no row of the app's own, and found the one it acted on among its sample rows.
	var n int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM app_rows WHERE app_id = ?`, app.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the app holds %d rows (%v)", n, err)
	}
}

// Someone who belongs to two workspaces can write a space of each, but an app only ever aims at the
// workspace it is in: saving one that doesn't is refused, and a copy never keeps such a target.
func TestAnAppNeverAimsAtAnotherWorkspace(t *testing.T) {
	f := newFX(t)
	// Alice is also an editor of a space of the other workspace, with a board there.
	if _, err := f.e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, f.other, f.alice.id); err != nil {
		t.Fatal(err)
	}
	f.e.addMember(f.stranger, f.otherShared, f.alice, access.RoleEditor)
	var abroad int64
	if err := f.e.db.QueryRow(`INSERT INTO boards (space_id, kind, name, created_by, updated_by) VALUES (?, 'team', 'Theirs', ?, ?) RETURNING id`,
		f.otherShared.ID, f.stranger.id, f.stranger.id).Scan(&abroad); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.e.s.access.CanWrite(f.ctx(), f.alice.id, f.otherShared.ID); err != nil || !ok {
		t.Fatalf("alice can't write the other workspace's space: %v %v", ok, err)
	}
	aimed, err := cloneDefinition(f.inspections())
	if err != nil {
		t.Fatal(err)
	}
	for name, act := range aimed.Actions {
		for i := range act.Effects {
			if act.Effects[i].Kind == EffectCreateCard {
				act.Effects[i].SpaceID, act.Effects[i].BoardID = f.otherShared.ID, abroad
			}
		}
		aimed.Actions[name] = act
	}

	// Saving and updating refuse it, though the author may write both.
	expectCode(t, f.alice.do(http.MethodPost, appsPath(f.shared, ""), map[string]any{"name": "Abroad", "definition": aimed}),
		http.StatusBadRequest, "app_effect_target")
	home := f.mustInstall(f.alice, f.shared, "inspection", map[string]any{"board": map[string]any{"space_id": f.maint.ID, "board_id": f.board}})
	expectCode(t, f.alice.do(http.MethodPatch, appPath(f.shared, home.ID, ""), map[string]any{"version": home.Version, "definition": aimed}),
		http.StatusBadRequest, "app_effect_target")
	expectCode(t, f.install(f.alice, f.shared, "inspection", map[string]any{"board": map[string]any{"space_id": f.otherShared.ID, "board_id": abroad}}),
		http.StatusBadRequest, "app_effect_target")

	// A copy clears it, whatever the copier may write.
	if err := f.e.s.clearForeignTargets(f.ctx(), f.shared.WorkspaceID, Actor{UserID: f.alice.id}, &aimed); err != nil {
		t.Fatal(err)
	}
	if left := aimed.unsetTargets(); len(left) == 0 {
		t.Fatal("the copy kept a target in another workspace")
	}
	// Its own workspace's boards stay, for a caller who may write them.
	kept, err := cloneDefinition(f.inspections())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.e.s.clearForeignTargets(f.ctx(), f.shared.WorkspaceID, Actor{UserID: f.alice.id}, &kept); err != nil {
		t.Fatal(err)
	}
	if len(kept.unsetTargets()) != 0 {
		t.Fatal("a target the copier may write was cleared")
	}
	for _, act := range aimed.Actions {
		for _, e := range act.Effects {
			if e.SpaceID == f.otherShared.ID {
				t.Fatalf("an effect still names the other workspace's space: %+v", e)
			}
		}
	}
}
