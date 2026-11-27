package apps

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
)

func homePath(ws int64, rest string) string {
	return fmt.Sprintf("/api/workspaces/%d/home%s", ws, rest)
}

func defaultsPath(s access.Space, rest string) string {
	return fmt.Sprintf("/api/workspaces/%d/spaces/%d/home/defaults%s", s.WorkspaceID, s.ID, rest)
}

func offers() *Contributions {
	return &Contributions{
		Widgets: []Widget{
			{ID: "spend", Title: "Spending", Kind: WidgetKindSummaries, Size: WidgetMedium, Summaries: []WidgetSummary{{Summary: "total", Label: "Total spent"}}},
			{ID: "open", Title: "Open expenses", Kind: WidgetKindShortcut},
		},
		Pin: &Pin{Label: "Spend"},
	}
}

func (w *world) contribute(u *user, s access.Space, a App, c *Contributions) App {
	w.e.t.Helper()
	got := decode[App](w.e.t, u.do(http.MethodGet, appPath(s, a.ID, ""), nil))
	def := *got.Draft
	def.Contributions = c
	rec := u.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": got.Version, "definition": def})
	expect(w.e.t, rec, http.StatusOK)
	return w.publish(u, s, decode[App](w.e.t, rec))
}

func (w *world) home(u *user) HomeView {
	w.e.t.Helper()
	rec := u.do(http.MethodGet, homePath(w.ws, ""), nil)
	expect(w.e.t, rec, http.StatusOK)
	return decode[HomeView](w.e.t, rec)
}

func (w *world) place(u *user, s access.Space, a App, kind, widget string, status int) HomeItem {
	w.e.t.Helper()
	rec := u.do(http.MethodPost, homePath(w.ws, "/items"), NewHomeItem{Kind: kind, SpaceID: s.ID, AppID: a.ID, Widget: widget})
	expect(w.e.t, rec, status)
	if status != http.StatusCreated {
		return HomeItem{}
	}
	return decode[HomeItem](w.e.t, rec)
}

func (w *world) widgetData(u *user, s access.Space, a App, widget string) (map[string]any, int) {
	w.e.t.Helper()
	rec := u.do(http.MethodGet, appPath(s, a.ID, "/surface?widget="+widget), nil)
	if rec.Code != http.StatusOK {
		return nil, rec.Code
	}
	msgs := decode[[]struct {
		UpdateDataModel *struct {
			Value map[string]any `json:"value"`
		} `json:"updateDataModel"`
	}](w.e.t, rec)
	for _, m := range msgs {
		if m.UpdateDataModel != nil {
			return m.UpdateDataModel.Value, rec.Code
		}
	}
	w.e.t.Fatal("no data model in the widget's surface")
	return nil, 0
}

func TestContributionsAreCheckedStrictly(t *testing.T) {
	def := example(t)
	ok := func(c *Contributions) (Definition, error) {
		d := def
		d.Contributions = c
		return d.Check()
	}
	checked, err := ok(offers())
	if err != nil {
		t.Fatal(err)
	}
	if checked.Contributions.Version != ContributionsVersion || checked.Contributions.Widgets[1].Size != WidgetSmall {
		t.Fatalf("checked = %+v", checked.Contributions)
	}
	for _, need := range []string{SlotHomeWidgets, SlotNavPin, "component.Stat", "component.Column"} {
		found := false
		for _, r := range checked.Requires {
			found = found || r == need
		}
		if !found {
			t.Errorf("requires %v lacks %s", checked.Requires, need)
		}
	}
	long := strings.Repeat("x", 61)
	bad := map[string]*Contributions{
		"empty":             {},
		"a future version":  {Version: 2, Pin: &Pin{Label: "x"}},
		"too many widgets":  {Widgets: manyWidgets(MaxWidgets + 1)},
		"a bad id":          {Widgets: []Widget{{ID: "Bad Id", Title: "t", Kind: WidgetKindShortcut}}},
		"a twin id":         {Widgets: []Widget{{ID: "a", Title: "t", Kind: WidgetKindShortcut}, {ID: "a", Title: "t", Kind: WidgetKindShortcut}}},
		"a long title":      {Widgets: []Widget{{ID: "a", Title: long, Kind: WidgetKindShortcut}}},
		"a two-line title":  {Widgets: []Widget{{ID: "a", Title: "two\nlines", Kind: WidgetKindShortcut}}},
		"a padded title":    {Widgets: []Widget{{ID: "a", Title: " pad", Kind: WidgetKindShortcut}}},
		"an empty title":    {Widgets: []Widget{{ID: "a", Title: "", Kind: WidgetKindShortcut}}},
		"an unknown kind":   {Widgets: []Widget{{ID: "a", Title: "t", Kind: "iframe"}}},
		"an unknown size":   {Widgets: []Widget{{ID: "a", Title: "t", Kind: WidgetKindShortcut, Size: "huge"}}},
		"no summaries":      {Widgets: []Widget{{ID: "a", Title: "t", Kind: WidgetKindSummaries}}},
		"an unknown ref":    {Widgets: []Widget{{ID: "a", Title: "t", Kind: WidgetKindSummaries, Summaries: []WidgetSummary{{Summary: "nope", Label: "x"}}}}},
		"a repeated ref":    {Widgets: []Widget{{ID: "a", Title: "t", Kind: WidgetKindSummaries, Summaries: []WidgetSummary{{Summary: "total", Label: "x"}, {Summary: "total", Label: "y"}}}}},
		"a long label":      {Widgets: []Widget{{ID: "a", Title: "t", Kind: WidgetKindSummaries, Summaries: []WidgetSummary{{Summary: "total", Label: long}}}}},
		"a screen without":  {Widgets: []Widget{{ID: "a", Title: "t", Kind: WidgetKindScreen, Screen: "main"}}},
		"a shortcut + ref":  {Widgets: []Widget{{ID: "a", Title: "t", Kind: WidgetKindShortcut, Summaries: []WidgetSummary{{Summary: "total", Label: "x"}}}}},
		"summaries+screen":  {Widgets: []Widget{{ID: "a", Title: "t", Kind: WidgetKindSummaries, Screen: "x", Summaries: []WidgetSummary{{Summary: "total", Label: "x"}}}}},
		"a pin's long text": {Pin: &Pin{Label: strings.Repeat("p", 31)}},
		"a pin's screen":    {Pin: &Pin{Label: "p", Screen: "main"}},
	}
	for name, c := range bad {
		if _, err := ok(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	old := def
	old.DefinitionVersion = 1
	old.Contributions = offers()
	if _, err := old.Check(); err == nil || !strings.Contains(err.Error(), "contributions") {
		t.Errorf("version 1 with contributions: %v", err)
	}
	for _, text := range []string{`{"widgetz": []}`, `{"pin": {"label": "x", "url": "https://example.com"}}`, `{"widgets": [{"id": "a", "title": "t", "kind": "shortcut", "html": "<b>"}]}`} {
		var c Contributions
		if err := json.Unmarshal([]byte(text), &c); err == nil {
			t.Errorf("%s: read", text)
		}
	}
	planner := plannerDefinition(t)
	planner.Contributions = &Contributions{
		Widgets: []Widget{{ID: "tasks", Title: "Tasks", Kind: WidgetKindScreen, Screen: "tasks", Size: WidgetLarge}},
		Pin:     &Pin{Label: "Tasks", Screen: "new"},
	}
	if _, err := planner.Check(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*Contributions{
		{Widgets: []Widget{{ID: "t", Title: "t", Kind: WidgetKindScreen, Screen: "detail"}}},
		{Widgets: []Widget{{ID: "t", Title: "t", Kind: WidgetKindScreen, Screen: "missing"}}},
		{Pin: &Pin{Label: "p", Screen: "detail"}},
	} {
		p := plannerDefinition(t)
		p.Contributions = c
		if _, err := p.Check(); err == nil {
			t.Errorf("%+v: accepted", c)
		}
	}
}

func manyWidgets(n int) []Widget {
	out := make([]Widget, n)
	for i := range out {
		out[i] = Widget{ID: fmt.Sprintf("w%d", i), Title: "t", Kind: WidgetKindShortcut}
	}
	return out
}

func plannerDefinition(t *testing.T) Definition {
	t.Helper()
	all, err := Examples()
	if err != nil {
		t.Fatal(err)
	}
	for _, ex := range all {
		if ex.Name == "Task planner" {
			return ex.Definition
		}
	}
	t.Fatal("no screens example")
	return Definition{}
}

func TestPinAnAppAndPlaceAWidget(t *testing.T) {
	w := newWorld(t)
	app := w.contribute(w.alice, w.alicePrivate, w.aliceApp, offers())

	pin := w.place(w.alice, w.alicePrivate, app, KindPin, "", http.StatusCreated)
	if pin.State != StateReady || pin.Label != "Spend" || pin.Source != SourcePersonal || pin.Release == nil {
		t.Fatalf("pin = %+v", pin)
	}
	if again := w.place(w.alice, w.alicePrivate, app, KindPin, "", http.StatusCreated); again.ID != pin.ID {
		t.Fatalf("pinning twice made %d and %d", pin.ID, again.ID)
	}
	widget := w.place(w.alice, w.alicePrivate, app, KindWidget, "spend", http.StatusCreated)
	if widget.State != StateReady || widget.Widget == nil || widget.Widget.Title != "Spending" || widget.Widget.Size != WidgetMedium {
		t.Fatalf("widget = %+v", widget)
	}
	w.place(w.alice, w.alicePrivate, app, KindWidget, "nope", http.StatusNotFound)
	w.place(w.alice, w.alicePrivate, app, KindWidget, "", http.StatusBadRequest)
	w.place(w.alice, w.alicePrivate, app, KindPin, "spend", http.StatusBadRequest)
	w.place(w.alice, w.alicePrivate, app, "banner", "", http.StatusBadRequest)

	home := w.home(w.alice)
	if len(home.Widgets) != 1 || len(home.Pins) != 1 || home.Widgets[0].ID != widget.ID || home.Pins[0].AppName != app.Name {
		t.Fatalf("home = %+v", home)
	}
	if other := w.home(w.bob); len(other.Widgets)+len(other.Pins)+len(other.Hidden) != 0 {
		t.Fatalf("bob's home = %+v", other)
	}

	data, code := w.widgetData(w.alice, w.alicePrivate, app, "spend")
	if code != http.StatusOK || data["total"] != "1.00" {
		t.Fatalf("widget data = %v (%d)", data, code)
	}
	if _, has := data["items"]; has {
		t.Fatal("a summaries widget carries rows")
	}
	if _, code := w.widgetData(w.alice, w.alicePrivate, app, "nope"); code != http.StatusNotFound {
		t.Fatalf("an unknown widget = %d", code)
	}
	rec := w.alice.do(http.MethodGet, appPath(w.alicePrivate, app.ID, "/surface?widget=open"), nil)
	expect(t, rec, http.StatusOK)
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("a shortcut's surface = %s", rec.Body.String())
	}

	second := w.place(w.alice, w.alicePrivate, app, KindWidget, "open", http.StatusCreated)
	rec = w.alice.do(http.MethodPut, homePath(w.ws, "/order"), map[string]any{"kind": KindWidget, "ids": []int64{second.ID, widget.ID}})
	expect(t, rec, http.StatusOK)
	if got := decode[HomeView](t, rec).Widgets; len(got) != 2 || got[0].ID != second.ID || got[1].ID != widget.ID {
		t.Fatalf("order = %+v", got)
	}
	for name, ids := range map[string][]int64{"twice": {widget.ID, widget.ID}, "a pin": {pin.ID}, "unknown": {9999}} {
		expect(t, w.alice.do(http.MethodPut, homePath(w.ws, "/order"), map[string]any{"kind": KindWidget, "ids": ids}), http.StatusBadRequest)
		_ = name
	}
	expect(t, w.alice.do(http.MethodPut, homePath(w.ws, "/order"), map[string]any{"kind": "x", "ids": []int64{}}), http.StatusBadRequest)
	if got := w.home(w.alice).Widgets; got[0].ID != second.ID {
		t.Fatalf("a refused order changed it: %+v", got)
	}

	w.place(w.bob, w.alicePrivate, app, KindPin, "", http.StatusNotFound)
	expect(t, w.bob.do(http.MethodDelete, homePath(w.ws, fmt.Sprintf("/items/%d", widget.ID)), nil), http.StatusNotFound)
	expect(t, w.alice.do(http.MethodDelete, homePath(w.ws, fmt.Sprintf("/items/%d", widget.ID)), nil), http.StatusNoContent)
	expect(t, w.alice.do(http.MethodDelete, homePath(w.ws, fmt.Sprintf("/items/%d", widget.ID)), nil), http.StatusNotFound)
	if got := w.home(w.alice).Widgets; len(got) != 1 || got[0].ID != second.ID {
		t.Fatalf("after removing = %+v", got)
	}
}

func TestHomeIsBounded(t *testing.T) {
	w := newWorld(t)
	for i := 0; i < MaxHomePins; i++ {
		a := w.create(w.alice, w.alicePrivate, fmt.Sprintf("App %d", i))
		w.place(w.alice, w.alicePrivate, a, KindPin, "", http.StatusCreated)
	}
	a := w.create(w.alice, w.alicePrivate, "One more")
	rec := w.alice.do(http.MethodPost, homePath(w.ws, "/items"), NewHomeItem{Kind: KindPin, SpaceID: w.alicePrivate.ID, AppID: a.ID})
	expect(t, rec, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), `"home_limit"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestWidgetsFollowTheAppsAccessRules(t *testing.T) {
	w := newWorld(t)
	app := w.contribute(w.alice, w.shared, w.sharedApp, offers())
	w.place(w.bob, w.shared, app, KindWidget, "spend", http.StatusCreated)
	if data, code := w.widgetData(w.bob, w.shared, app, "spend"); code != http.StatusOK || data["total"] != "3.00" {
		t.Fatalf("bob's widget = %v (%d)", data, code)
	}
	w.place(w.mallory, w.shared, app, KindWidget, "spend", http.StatusNotFound)
	if _, code := w.widgetData(w.mallory, w.shared, app, "spend"); code != http.StatusNotFound {
		t.Fatalf("mallory's widget = %d", code)
	}
	setRoles := func(grants ...access.AppGrant) {
		t.Helper()
		expect(t, w.alice.do(http.MethodPut, appPath(w.shared, app.ID, "/roles"), map[string]any{"grants": grants}), http.StatusOK)
	}
	alice := access.AppGrant{UserID: &w.alice.id, Roles: []access.AppRole{access.AppBuilder, access.AppPublisher, access.AppDataManager}}
	setRoles(alice, access.AppGrant{UserID: &w.owner.id, Roles: []access.AppRole{access.AppUser}},
		access.AppGrant{UserID: &w.bob.id, Roles: []access.AppRole{access.AppReader}})
	if data, code := w.widgetData(w.owner, w.shared, app, "spend"); code != http.StatusOK || data["total"] != "0.00" {
		t.Fatalf("an own-scope widget = %v (%d)", data, code)
	}
	if data, _ := w.widgetData(w.alice, w.shared, app, "spend"); data["total"] != "3.00" {
		t.Fatalf("alice's widget = %v", data)
	}
	if data, _ := w.widgetData(w.bob, w.shared, app, "spend"); data["total"] != "3.00" {
		t.Fatalf("bob's widget = %v", data)
	}
	rec := w.owner.do(http.MethodPost, appPath(w.shared, app.ID, "/actions?widget=spend"),
		action(app, "add_expense", map[string]any{"description": "owner-coffee", "amount": "2"}))
	expect(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), "team-lunch") || !strings.Contains(rec.Body.String(), `"2.00"`) {
		t.Fatalf("the owner's answer = %s", rec.Body.String())
	}
	expect(t, w.bob.do(http.MethodPost, appPath(w.shared, app.ID, "/actions?widget=spend"),
		action(app, "add_expense", map[string]any{"description": "x", "amount": "1"})), http.StatusForbidden)
	setRoles(alice, access.AppGrant{UserID: &w.owner.id, Roles: []access.AppRole{access.AppUser}})
	if got := w.home(w.bob); len(got.Widgets) != 0 {
		t.Fatalf("bob still has %+v", got.Widgets)
	}
	if _, code := w.widgetData(w.bob, w.shared, app, "spend"); code != http.StatusNotFound {
		t.Fatalf("bob's widget after losing the app = %d", code)
	}
	setRoles(alice, access.AppGrant{UserID: &w.bob.id, Roles: []access.AppRole{access.AppReader}})
	if got := w.home(w.bob); len(got.Widgets) != 1 {
		t.Fatalf("bob's widget came back as %+v", got.Widgets)
	}
	expect(t, w.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", w.ws, w.shared.ID, w.bob.id), nil), http.StatusNoContent)
	if got := w.home(w.bob); len(got.Widgets) != 0 || len(got.Pins) != 0 {
		t.Fatalf("a former member's home = %+v", got)
	}
	if _, code := w.widgetData(w.bob, w.shared, app, "spend"); code != http.StatusNotFound {
		t.Fatalf("a former member's widget = %d", code)
	}
}

func TestAnUnavailableAppIsToldAsSuchAndNeverShowsData(t *testing.T) {
	w := newWorld(t)
	app := w.contribute(w.alice, w.alicePrivate, w.aliceApp, offers())
	widget := w.place(w.alice, w.alicePrivate, app, KindWidget, "spend", http.StatusCreated)
	pin := w.place(w.alice, w.alicePrivate, app, KindPin, "", http.StatusCreated)

	expect(t, w.alice.do(http.MethodPatch, appPath(w.alicePrivate, app.ID, ""), map[string]any{"version": app.Version, "archived": true}), http.StatusOK)
	home := w.home(w.alice)
	if home.Widgets[0].State != StateUnavailable || home.Widgets[0].Reason != ReasonOffline || home.Widgets[0].Widget != nil || home.Widgets[0].Release != nil ||
		home.Pins[0].State != StateUnavailable || home.Pins[0].Label != "" {
		t.Fatalf("an offline app's items = %+v", home)
	}
	if _, code := w.widgetData(w.alice, w.alicePrivate, app, "spend"); code != http.StatusConflict {
		t.Fatalf("an offline widget = %d", code)
	}
	w.place(w.alice, w.alicePrivate, app, KindWidget, "open", http.StatusConflict)
	cur := decode[App](t, w.alice.do(http.MethodGet, appPath(w.alicePrivate, app.ID, ""), nil))
	expect(t, w.alice.do(http.MethodPatch, appPath(w.alicePrivate, app.ID, ""), map[string]any{"version": cur.Version, "archived": false}), http.StatusOK)
	if home := w.home(w.alice); home.Widgets[0].State != StateReady || home.Pins[0].State != StateReady {
		t.Fatalf("restored = %+v", home)
	}

	cur = decode[App](t, w.alice.do(http.MethodGet, appPath(w.alicePrivate, app.ID, ""), nil))
	def := *cur.Draft
	def.Contributions = &Contributions{Widgets: []Widget{offers().Widgets[1]}}
	rec := w.alice.do(http.MethodPatch, appPath(w.alicePrivate, app.ID, ""), map[string]any{"version": cur.Version, "definition": def})
	expect(t, rec, http.StatusOK)
	w.publish(w.alice, w.alicePrivate, decode[App](t, rec))
	home = w.home(w.alice)
	if home.Widgets[0].State != StateUnavailable || home.Widgets[0].Reason != ReasonRemoved || home.Pins[0].Label != app.Name {
		t.Fatalf("after the widget was removed = %+v", home)
	}
	if _, code := w.widgetData(w.alice, w.alicePrivate, app, "spend"); code != http.StatusNotFound {
		t.Fatalf("a removed widget = %d", code)
	}

	cur = decode[App](t, w.alice.do(http.MethodGet, appPath(w.alicePrivate, app.ID, ""), nil))
	expect(t, w.alice.do(http.MethodPost, appPath(w.alicePrivate, app.ID, "/move"), map[string]any{"version": cur.Version, "space_id": w.shared.ID}), http.StatusOK)
	home = w.home(w.alice)
	if len(home.Pins) != 1 || home.Pins[0].SpaceID != w.shared.ID || home.Pins[0].ID != pin.ID || len(home.Widgets) != 1 || home.Widgets[0].ID != widget.ID {
		t.Fatalf("after the move = %+v", home)
	}
	if got := w.home(w.mallory); len(got.Widgets)+len(got.Pins) != 0 {
		t.Fatalf("mallory's home = %+v", got)
	}
}

func TestSharedDefaults(t *testing.T) {
	w := newWorld(t)
	app := w.contribute(w.alice, w.shared, w.sharedApp, offers())

	body := NewHomeItem{Kind: KindWidget, AppID: app.ID, Widget: "spend"}
	expect(t, w.bob.do(http.MethodPost, defaultsPath(w.shared, ""), body), http.StatusForbidden)
	expect(t, w.mallory.do(http.MethodPost, defaultsPath(w.shared, ""), body), http.StatusNotFound)
	expect(t, w.mallory.do(http.MethodGet, defaultsPath(w.shared, ""), nil), http.StatusNotFound)
	rec := w.alice.do(http.MethodPost, defaultsPath(w.shared, ""), body)
	expect(t, rec, http.StatusCreated)
	def := decode[HomeItem](t, rec)
	if def.DefaultID == 0 || def.Source != SourceDefault || !def.CanManage {
		t.Fatalf("default = %+v", def)
	}
	expect(t, w.alice.do(http.MethodPost, defaultsPath(w.shared, ""), NewHomeItem{Kind: KindPin, AppID: app.ID}), http.StatusCreated)
	expect(t, w.alice.do(http.MethodPost, defaultsPath(w.shared, ""), NewHomeItem{Kind: KindWidget, AppID: app.ID, Widget: "nope"}), http.StatusNotFound)
	expect(t, w.alice.do(http.MethodPost, defaultsPath(w.alicePrivate, ""), NewHomeItem{Kind: KindPin, AppID: w.aliceApp.ID}), http.StatusConflict)
	expect(t, w.alice.do(http.MethodPost, defaultsPath(w.shared, ""), NewHomeItem{Kind: KindPin, AppID: w.aliceApp.ID}), http.StatusNotFound)

	home := w.home(w.bob)
	if len(home.Widgets) != 1 || home.Widgets[0].Source != SourceDefault || home.Widgets[0].DefaultID != def.DefaultID || len(home.Pins) != 1 {
		t.Fatalf("bob's home = %+v", home)
	}
	if got := w.home(w.mallory); len(got.Widgets)+len(got.Pins) != 0 {
		t.Fatalf("mallory sees the space's defaults: %+v", got)
	}
	hide := homePath(w.ws, fmt.Sprintf("/hidden/%d", def.DefaultID))
	expect(t, w.bob.do(http.MethodPut, hide, nil), http.StatusNoContent)
	expect(t, w.bob.do(http.MethodPut, hide, nil), http.StatusNoContent)
	home = w.home(w.bob)
	if len(home.Widgets) != 0 || len(home.Hidden) != 1 || home.Hidden[0].DefaultID != def.DefaultID {
		t.Fatalf("bob's home with it hidden = %+v", home)
	}
	if got := w.home(w.alice); len(got.Widgets) != 1 {
		t.Fatalf("hiding is bob's own: alice's home = %+v", got)
	}
	expect(t, w.mallory.do(http.MethodPut, hide, nil), http.StatusNotFound)
	for _, d := range decode[struct{ Defaults []HomeItem }](t, w.bob.do(http.MethodGet, defaultsPath(w.shared, ""), nil)).Defaults {
		if d.CanManage {
			t.Fatalf("bob may manage %+v", d)
		}
	}
	expect(t, w.bob.do(http.MethodDelete, hide, nil), http.StatusNoContent)
	if home = w.home(w.bob); len(home.Widgets) != 1 || len(home.Hidden) != 0 {
		t.Fatalf("shown again = %+v", home)
	}
	mine := w.place(w.bob, w.shared, app, KindWidget, "spend", http.StatusCreated)
	if home = w.home(w.bob); len(home.Widgets) != 1 || home.Widgets[0].ID != mine.ID || home.Widgets[0].Source != SourcePersonal {
		t.Fatalf("a placed default = %+v", home.Widgets)
	}

	removal := defaultsPath(w.shared, fmt.Sprintf("/%d", def.DefaultID))
	expect(t, w.bob.do(http.MethodDelete, removal, nil), http.StatusForbidden)
	expect(t, w.mallory.do(http.MethodDelete, removal, nil), http.StatusNotFound)
	expect(t, w.alice.do(http.MethodDelete, defaultsPath(w.alicePrivate, fmt.Sprintf("/%d", def.DefaultID)), nil), http.StatusNotFound)
	expect(t, w.alice.do(http.MethodPut, appPath(w.shared, app.ID, "/roles"), map[string]any{"grants": []access.AppGrant{
		{UserID: &w.alice.id, Roles: []access.AppRole{access.AppBuilder}},
		{UserID: &w.owner.id, Roles: []access.AppRole{access.AppBuilder, access.AppPublisher}},
		{UserID: &w.bob.id, Roles: []access.AppRole{access.AppReader}},
	}}), http.StatusOK)
	expect(t, w.alice.do(http.MethodDelete, removal, nil), http.StatusForbidden)
	expect(t, w.alice.do(http.MethodPost, defaultsPath(w.shared, ""), NewHomeItem{Kind: KindWidget, AppID: app.ID, Widget: "open"}), http.StatusForbidden)
	if got := w.alice.do(http.MethodGet, defaultsPath(w.shared, ""), nil); !strings.Contains(got.Body.String(), `"default_id":`+fmt.Sprint(def.DefaultID)) {
		t.Fatalf("alice's list = %s", got.Body.String())
	}
	expect(t, w.owner.do(http.MethodDelete, removal, nil), http.StatusNoContent)
	if home = w.home(w.bob); len(home.Widgets) != 1 || home.Widgets[0].Source != SourcePersonal || len(home.Pins) != 1 {
		t.Fatalf("after removing the default = %+v", home)
	}

	expect(t, w.owner.do(http.MethodPut, appPath(w.shared, app.ID, "/roles"), map[string]any{"grants": []access.AppGrant{}}), http.StatusOK)
	cur := decode[App](t, w.alice.do(http.MethodGet, appPath(w.shared, app.ID, ""), nil))
	expect(t, w.alice.do(http.MethodPost, appPath(w.shared, app.ID, "/move"), map[string]any{"version": cur.Version, "space_id": w.alicePrivate.ID}), http.StatusOK)
	var n int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM home_defaults WHERE app_id = ?`, app.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("defaults left after the move = %d, %v", n, err)
	}
	if got := w.home(w.bob); len(got.Pins) != 0 {
		t.Fatalf("bob still has the pin: %+v", got.Pins)
	}
}

func TestAvailableAppsAndWhoMaySetDefaults(t *testing.T) {
	w := newWorld(t)
	app := w.contribute(w.alice, w.shared, w.sharedApp, offers())
	list := func(u *user) []AvailableApp {
		rec := u.do(http.MethodGet, homePath(w.ws, "/available"), nil)
		expect(t, rec, http.StatusOK)
		return decode[struct{ Apps []AvailableApp }](t, rec).Apps
	}
	find := func(all []AvailableApp, id int64) *AvailableApp {
		for i := range all {
			if all[i].AppID == id {
				return &all[i]
			}
		}
		return nil
	}
	got := find(list(w.alice), app.ID)
	if got == nil || len(got.Widgets) != 2 || got.Pin == nil || !got.CanSetDefaults || got.SpaceKind != "shared" {
		t.Fatalf("alice's %+v", got)
	}
	if got := find(list(w.bob), app.ID); got == nil || got.CanSetDefaults {
		t.Fatalf("bob's %+v", got)
	}
	if got := find(list(w.mallory), app.ID); got != nil {
		t.Fatalf("mallory is offered %+v", got)
	}
	if got := find(list(w.alice), w.bobApp.ID); got != nil {
		t.Fatalf("alice is offered bob's private app")
	}
	if got := find(list(w.alice), w.oldApp.ID); got == nil || got.CanSetDefaults {
		t.Fatalf("an archived space's app = %+v (defaults can't be set there)", got)
	}
}

func TestHomeRoutesNeedASession(t *testing.T) {
	w := newWorld(t)
	nobody := &user{e: w.e}
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, homePath(w.ws, "")},
		{http.MethodGet, homePath(w.ws, "/available")},
		{http.MethodPost, homePath(w.ws, "/items")},
		{http.MethodDelete, homePath(w.ws, "/items/1")},
		{http.MethodPut, homePath(w.ws, "/order")},
		{http.MethodPut, homePath(w.ws, "/hidden/1")},
		{http.MethodDelete, homePath(w.ws, "/hidden/1")},
		{http.MethodGet, defaultsPath(w.shared, "")},
		{http.MethodPost, defaultsPath(w.shared, "")},
		{http.MethodDelete, defaultsPath(w.shared, "/1")},
	} {
		if rec := nobody.do(r.method, r.path, map[string]any{}); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d without a session", r.method, r.path, rec.Code)
		}
	}
	app := w.contribute(w.alice, w.shared, w.sharedApp, offers())
	expect(t, w.stranger.do(http.MethodGet, homePath(w.ws, ""), nil), http.StatusNotFound)
	rec := w.stranger.do(http.MethodPost, homePath(w.other, "/items"), NewHomeItem{Kind: KindPin, SpaceID: w.shared.ID, AppID: app.ID})
	expect(t, rec, http.StatusNotFound)
	rec = w.alice.do(http.MethodPost, homePath(w.other, "/items"), NewHomeItem{Kind: KindPin, SpaceID: w.shared.ID, AppID: app.ID})
	expect(t, rec, http.StatusNotFound)
}

func TestWidgetScreensAndPinsOpenWhereTheyShould(t *testing.T) {
	w := newWorld(t)
	planner := plannerDefinition(t)
	planner.Contributions = &Contributions{
		Widgets: []Widget{{ID: "tasks", Title: "Tasks", Kind: WidgetKindScreen, Screen: "tasks", Size: WidgetLarge}},
		Pin:     &Pin{Label: "New task", Screen: "new"},
	}
	rec := w.alice.do(http.MethodPost, appsPath(w.alicePrivate, ""), map[string]any{"name": "Tasks", "definition": planner})
	expect(t, rec, http.StatusCreated)
	app := w.publish(w.alice, w.alicePrivate, decode[App](t, rec))
	for i := 0; i < MaxWidgetItems+5; i++ {
		expect(t, w.alice.do(http.MethodPost, appPath(w.alicePrivate, app.ID, "/actions"),
			ClientAction{Name: "add_task", SurfaceID: SurfaceID(app.ID), Context: map[string]any{"title": fmt.Sprintf("Task %d", i), "category": "work"}}), http.StatusOK)
	}
	data, code := w.widgetData(w.alice, w.alicePrivate, app, "tasks")
	if code != http.StatusOK || data["screen"] != "tasks" {
		t.Fatalf("widget data = %v (%d)", data, code)
	}
	if items, _ := data["items"].([]any); len(items) != MaxWidgetItems || data["open"] != fmt.Sprint(MaxWidgetItems+5) {
		t.Fatalf("a screen widget holds %d rows, open = %v", len(items), data["open"])
	}
	rec = w.alice.do(http.MethodGet, appPath(w.alicePrivate, app.ID, "/surface?screen=new"), nil)
	expect(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"screen":"new"`) {
		t.Fatalf("the pin's screen: %s", rec.Body.String())
	}
	expect(t, w.alice.do(http.MethodGet, appPath(w.alicePrivate, app.ID, "/surface?screen=detail"), nil), http.StatusBadRequest)
	expect(t, w.alice.do(http.MethodGet, appPath(w.alicePrivate, app.ID, "/surface?screen=zzz"), nil), http.StatusBadRequest)
	expect(t, w.alice.do(http.MethodGet, appPath(w.alicePrivate, app.ID, "/surface?widget=tasks&screen=new"), nil), http.StatusBadRequest)
	rec = w.alice.do(http.MethodPost, appPath(w.alicePrivate, app.ID, "/actions?widget=tasks"),
		ClientAction{Name: "add_task", SurfaceID: SurfaceID(app.ID), Context: map[string]any{"title": "One more", "category": "work"}})
	expect(t, rec, http.StatusOK)
	var msgs []struct {
		UpdateDataModel *struct {
			Path  string `json:"path"`
			Value any    `json:"value"`
		} `json:"updateDataModel"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &msgs); err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.UpdateDataModel != nil && m.UpdateDataModel.Path == "/items" {
			if items, _ := m.UpdateDataModel.Value.([]any); len(items) != MaxWidgetItems {
				t.Fatalf("a widget's action answered %d rows", len(items))
			}
		}
	}
	before := storedRows(t, w.e.db, app.ID)
	expect(t, w.alice.do(http.MethodPost, appPath(w.alicePrivate, app.ID, "/actions?widget=NOT%20AN%20ID"),
		ClientAction{Name: "add_task", SurfaceID: SurfaceID(app.ID), Context: map[string]any{"title": "x", "category": "work"}}), http.StatusBadRequest)
	if after := storedRows(t, w.e.db, app.ID); len(after) != len(before) {
		t.Fatalf("a refused widget action stored a row: %d rows, were %d", len(after), len(before))
	}
}
