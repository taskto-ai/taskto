package apps

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
)

const seriesName = "by_category"

func (w *world) seriesPlanner(t *testing.T) App {
	t.Helper()
	def := plannerDefinition(t)
	def.Series = map[string]Series{seriesName: {Kind: "count", GroupBy: "category"}}
	def.Contributions = &Contributions{
		Widgets: []Widget{{ID: "tasks", Title: "Tasks", Kind: WidgetKindScreen, Screen: "tasks", Size: WidgetLarge}},
	}
	app := w.createFrom(w.alice, w.shared, def)
	w.mustSetRoles(w.alice, w.shared, app,
		grant(w.alice.id, access.AppBuilder, access.AppPublisher, access.AppDataManager, access.AppUser),
		grant(w.bob.id, access.AppUser),
	)
	expect(t, w.owner.do(http.MethodPatch, fmtMember(w, w.bob.id), map[string]any{"role": access.RoleEditor}), http.StatusNoContent)
	w.setOffline(w.owner, w.shared, true)
	return app
}

func fmtMember(w *world, userID int64) string {
	return fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", w.ws, w.shared.ID, userID)
}

func (w *world) addPlannerTask(t *testing.T, u *user, app App, title, category string, widget bool) []byte {
	t.Helper()
	path := appPath(w.shared, app.ID, "/actions")
	if widget {
		path += "?widget=tasks"
	}
	rec := u.do(http.MethodPost, path, ClientAction{
		Name: "add_task", SurfaceID: SurfaceID(app.ID),
		Context: map[string]any{"title": title, "category": category},
	})
	expect(t, rec, http.StatusOK)
	return rec.Body.Bytes()
}

func countedBySeries(t *testing.T, messages []byte, name string) int {
	t.Helper()
	var msgs []struct {
		UpdateDataModel *struct {
			Path  string          `json:"path"`
			Value json.RawMessage `json:"value"`
		} `json:"updateDataModel"`
	}
	if err := json.Unmarshal(messages, &msgs); err != nil {
		t.Fatal(err)
	}
	var found json.RawMessage
	for _, m := range msgs {
		if m.UpdateDataModel == nil {
			continue
		}
		switch m.UpdateDataModel.Path {
		case "/":
			var root map[string]json.RawMessage
			if err := json.Unmarshal(m.UpdateDataModel.Value, &root); err != nil {
				t.Fatal(err)
			}
			if v, ok := root[name]; ok {
				found = v
			}
		case "/" + name:
			found = m.UpdateDataModel.Value
		}
	}
	if found == nil {
		t.Fatalf("no series %q in %s", name, messages)
	}
	var data SeriesData
	if err := json.Unmarshal(found, &data); err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, line := range data.Series {
		for _, v := range line.Values {
			if v != nil {
				total += int(*v)
			}
		}
	}
	return total
}

func TestSeriesCountOnlyRecordsTheViewerMayReadOnEverySurface(t *testing.T) {
	w := newWorld(t)
	app := w.seriesPlanner(t)
	w.addPlannerTask(t, w.alice, app, "alice one", "work", false)
	w.addPlannerTask(t, w.alice, app, "alice two", "home", false)
	w.addPlannerTask(t, w.bob, app, "bob one", "work", false)

	surface := func(u *user, query string) []byte {
		rec := u.do(http.MethodGet, appPath(w.shared, app.ID, "/surface"+query), nil)
		expect(t, rec, http.StatusOK)
		return rec.Body.Bytes()
	}
	download := func(u *user) []byte {
		rec := w.download(u, w.shared, app)
		expect(t, rec, http.StatusOK)
		got := decode[OfflineApp](t, rec)
		if _, ok := got.Definition.Series[seriesName]; !ok {
			t.Fatalf("the download's definition lacks the series: %+v", got.Definition.Series)
		}
		raw, err := json.Marshal(got.Messages)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	for _, tc := range []struct {
		name       string
		bob, alice func() []byte
	}{
		{"the app's surface",
			func() []byte { return surface(w.bob, "") },
			func() []byte { return surface(w.alice, "") }},
		{"a Home screen widget",
			func() []byte { return surface(w.bob, "?widget=tasks") },
			func() []byte { return surface(w.alice, "?widget=tasks") }},
		{"the offline download",
			func() []byte { return download(w.bob) },
			func() []byte { return download(w.alice) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := countedBySeries(t, tc.bob(), seriesName); got != 1 {
				t.Fatalf("a user who reads only their own records is shown %d, want 1", got)
			}
			if got := countedBySeries(t, tc.alice(), seriesName); got != 3 {
				t.Fatalf("a data manager is shown %d, want all 3", got)
			}
		})
	}

	t.Run("the answer to an action", func(t *testing.T) {
		if got := countedBySeries(t, w.addPlannerTask(t, w.bob, app, "bob two", "home", false), seriesName); got != 2 {
			t.Fatalf("after bob's insert his answer counts %d, want his 2", got)
		}
		if got := countedBySeries(t, w.addPlannerTask(t, w.alice, app, "alice three", "home", false), seriesName); got != 5 {
			t.Fatalf("after alice's insert her answer counts %d, want all 5", got)
		}
	})

	t.Run("the answer to an action of a Home widget", func(t *testing.T) {
		if got := countedBySeries(t, w.addPlannerTask(t, w.bob, app, "bob three", "work", true), seriesName); got != 3 {
			t.Fatalf("after bob's insert through a widget his answer counts %d, want his 3", got)
		}
	})
}

func TestAPreviewCountsOnlyTheRowsItWasGiven(t *testing.T) {
	w := newWorld(t)
	app := w.seriesPlanner(t)
	w.addPlannerTask(t, w.alice, app, "stored one", "work", false)
	w.addPlannerTask(t, w.bob, app, "stored two", "work", false)
	reply := w.mustPreview(w.alice, w.shared, app, PreviewRequest{Rows: []PreviewRow{
		{ID: 1, Revision: 1, Data: map[string]any{"title": "sample", "category": "home"}},
	}})
	raw, err := json.Marshal(reply.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if got := countedBySeries(t, raw, seriesName); got != 1 {
		t.Fatalf("a preview counts %d rows, want only the one it was given", got)
	}
	if strings.Contains(string(raw), "stored") {
		t.Fatalf("a preview shows a stored row: %s", raw)
	}
}

func TestEverySurfaceCountsBucketsFromTheSeriesClock(t *testing.T) {
	pinned := time.Date(2031, 5, 17, 9, 0, 0, 0, time.UTC)
	was := seriesNow
	seriesNow = func() time.Time { return pinned }
	t.Cleanup(func() { seriesNow = was })
	def := Definition{
		Collection: Collection{Fields: []Field{{Name: "day", Type: TypeDate}}},
		Series:     map[string]Series{"by_month": {Kind: "count", GroupBy: "day", Bucket: "month", Last: 2}},
	}
	rows := []Row{{ID: 1, Revision: 1, Data: map[string]any{"day": "2031-05-01"}}}
	keys := func(raw []byte) string {
		var data SeriesData
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatal(err)
		}
		return data.X[0].Key + "," + data.X[1].Key
	}
	model := dataModel("app", def, rows, "", 0)["updateDataModel"].(map[string]any)["value"].(map[string]any)["by_month"]
	raw, _ := json.Marshal(model)
	if got := keys(raw); got != "2031-04,2031-05" {
		t.Fatalf("the data model's buckets = %s", got)
	}
	screens := def
	screens.Screens = []Screen{{Name: "main", Root: "root"}}
	screens.Entry = "main"
	var refreshed json.RawMessage
	for _, m := range Refreshed("app", screens, rows, false) {
		update := m["updateDataModel"].(map[string]any)
		if update["path"] == "/by_month" {
			refreshed, _ = json.Marshal(update["value"])
		}
	}
	if refreshed == nil {
		t.Fatal("an answer doesn't refresh the series")
	}
	if got := keys(refreshed); got != "2031-04,2031-05" {
		t.Fatalf("the answer's buckets = %s", got)
	}
}

func TestAnOlderReleaseShowsOnlySeriesWhoseFieldsStillMeanTheSame(t *testing.T) {
	fields := func(amount, status, name, ok string) Collection {
		return Collection{Fields: []Field{
			{Name: "amount", Type: amount},
			{Name: "status", Type: status, Options: []string{"a"}},
			{Name: "name", Type: name},
			{Name: "ok", Type: ok},
		}}
	}
	where := &Rule{Field: "name", Op: "eq", Value: "x"}
	from := Definition{
		Collection: fields(TypeNumber, TypeChoice, TypeString, TypeBoolean),
		Series: map[string]Series{
			"count": {Kind: "count", GroupBy: "status"},
			"sum":   {Kind: "sum", Field: "amount", GroupBy: "status"},
			"split": {Kind: "count", GroupBy: "status", SplitBy: "ok"},
			"where": {Kind: "count", GroupBy: "status", Where: where},
		},
	}
	for _, tc := range []struct {
		name string
		live Collection
		want []string
	}{
		{"nothing changed", fields(TypeNumber, TypeChoice, TypeString, TypeBoolean), []string{"count", "sum", "split", "where"}},
		{"the summed field changed type", fields(TypeString, TypeChoice, TypeString, TypeBoolean), []string{"count", "split", "where"}},
		{"the grouped field changed type", fields(TypeNumber, TypeString, TypeString, TypeBoolean), []string{}},
		{"the split field changed type", fields(TypeNumber, TypeChoice, TypeString, TypeString), []string{"count", "sum", "where"}},
		{"the filtered field changed type", fields(TypeNumber, TypeChoice, TypeChoice, TypeBoolean), []string{"count", "sum", "split"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shown := shownBy(Definition{Collection: tc.live}, from)
			got := sortedKeys(shown.Series)
			want := append([]string{}, tc.want...)
			sort.Strings(want)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("shown series = %v, want %v", got, want)
			}
		})
	}
}

func TestAMalformedSeriesIsRefusedWhenSavedAndTheDraftIsUnchanged(t *testing.T) {
	w := newWorld(t)
	app := w.seriesPlanner(t)
	before := w.view(w.alice, w.shared, app)
	bad := *before.Draft
	bad.Series = map[string]Series{seriesName: {Kind: "count", GroupBy: "no_such_field"}}
	rec := w.patchDraft(w.alice, w.shared, app, bad)
	expect(t, rec, http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), "group_by") {
		t.Fatalf("the refusal doesn't say what is wrong: %s", rec.Body)
	}
	after := w.view(w.alice, w.shared, app)
	if after.Version != before.Version || len(after.Draft.Series) != 1 || after.Draft.Series[seriesName].GroupBy != "category" {
		t.Fatalf("a refused save changed the draft: %+v", after.Draft.Series)
	}
}
