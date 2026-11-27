package apps

import (
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

func planner(t *testing.T) Definition {
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
	t.Fatal("no Task planner example")
	return Definition{}
}

var plannerSeed = []Row{
	{ID: 1, Revision: 1, Data: map[string]any{"title": "Pay rent", "category": "home", "due_time": "09:30"}},
	{ID: 2, Revision: 1, Data: map[string]any{"title": "Fix the shelf", "category": "other", "category_note": "errand", "done": true}},
}

func TestPlannerGoldens(t *testing.T) {
	def := planner(t)
	golden(t, "planner.surface.json", Surface(surfaceID, def, plannerSeed))
	golden(t, "planner.open-new.json", Opened(surfaceID, "new", map[string]any{}))
	golden(t, "planner.open-detail.json", Opened(surfaceID, "detail", item(def, plannerSeed[1])))
	golden(t, "planner.open-tasks.json", Opened(surfaceID, "tasks", map[string]any{}))
}

func TestExamplesValidate(t *testing.T) {
	all, err := Examples()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 2 {
		t.Fatalf("examples = %d", len(all))
	}
	def := planner(t)
	if def.DefinitionVersion != DefinitionVersion || len(def.Screens) != 3 || def.Entry != "tasks" || def.Components != nil {
		t.Fatalf("planner = version %d, %d screens, entry %q", def.DefinitionVersion, len(def.Screens), def.Entry)
	}
	for _, need := range []string{"component.Screens", "component.Show", "component.ChoicePicker", "component.DateTimeInput"} {
		if !slices.Contains(def.Requires, need) {
			t.Errorf("requires %v lacks %s", def.Requires, need)
		}
	}
	for _, ex := range all {
		stored, _ := json.Marshal(ex.Definition)
		var again Definition
		if err := json.Unmarshal(stored, &again); err != nil {
			t.Fatal(err)
		}
		if _, err := again.Check(); err != nil {
			t.Errorf("%s no longer checks once stored: %v", ex.Name, err)
		}
	}
	ref, err := Reference()
	if err != nil || ref.DefinitionVersion != DefinitionVersion {
		t.Fatalf("reference = %v, %v", ref.DefinitionVersion, err)
	}
}

func TestVersionOneDefinitionsAreReadUnchanged(t *testing.T) {
	def := example(t)
	v1 := def
	v1.DefinitionVersion = 1
	checked, err := v1.Check()
	if err != nil || checked.DefinitionVersion != 1 {
		t.Fatalf("a version 1 definition: %+v, %v", checked.DefinitionVersion, err)
	}
	a, _ := json.Marshal(Surface(surfaceID, def, seed))
	b, _ := json.Marshal(Surface(surfaceID, checked, seed))
	if string(a) != string(b) {
		t.Fatal("a version 1 definition draws a different surface")
	}
	var old Definition
	raw, _ := json.Marshal(map[string]any{"components": def.Components, "collection": def.Collection, "actions": def.Actions, "summaries": def.Summaries})
	if err := json.Unmarshal(raw, &old); err != nil {
		t.Fatal(err)
	}
	if decoded, err := decodeDefinition(string(raw), 1, nil); err != nil || decoded.DefinitionVersion != 1 {
		t.Fatalf("a definition stored without a version = %+v, %v", decoded.DefinitionVersion, err)
	}
	if again, err := old.Check(); err != nil || again.DefinitionVersion != DefinitionVersion {
		t.Fatalf("no version = %d, %v", again.DefinitionVersion, err)
	}
	for name, change := range map[string]func(*Definition){
		"a choice field": func(d *Definition) {
			d.Collection.Fields = append(d.Collection.Fields, Field{Name: "k", Type: TypeChoice, Options: []string{"a"}})
		},
		"a time field": func(d *Definition) {
			d.Collection.Fields = append(d.Collection.Fields, Field{Name: "k", Type: TypeTime})
		},
		"required_when": func(d *Definition) {
			d.Collection.Fields[0].RequiredWhen = &Rule{Field: "amount", Op: OpGt, Value: 1.0}
		},
		"a navigate": func(d *Definition) { d.Actions["go"] = Action{Kind: ActionNavigate, Screen: "x"} },
		"a summary where": func(d *Definition) {
			d.Summaries["big"] = Summary{Kind: "count", Where: &Rule{Field: "amount", Op: OpGt, Value: 1.0}}
		},
		"the Show component": func(d *Definition) {
			d.Components = append(slices.Clone(d.Components), json.RawMessage(`{"id":"s","component":"Show","when":{"field":"amount","op":"gt","value":1},"child":"title"}`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := example(t)
			d.DefinitionVersion = 1
			d.Collection.Fields = slices.Clone(d.Collection.Fields)
			d.Actions = map[string]Action{"add_expense": {Kind: ActionInsert}, "delete_expense": {Kind: ActionDelete}}
			d.Summaries = map[string]Summary{"total": {Kind: "sum", Field: "amount"}}
			change(&d)
			if _, err := d.Check(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestCheckRefusesBadScreensActionsAndFields(t *testing.T) {
	bad := map[string]struct {
		change func(*Definition)
		want   string
	}{
		"components and screens": {func(d *Definition) {
			d.Components = []json.RawMessage{json.RawMessage(`{"id":"root","component":"Text","text":"x"}`)}
		}, "not both"},
		"an entry without screens": {func(d *Definition) {
			s := example(t)
			s.Entry = "x"
			*d = s
		}, "has no screens"},
		"no entry":                     {func(d *Definition) { d.Entry = "" }, "entry: must name one"},
		"an entry that isn't one":      {func(d *Definition) { d.Entry = "nowhere" }, "entry: must name one"},
		"an entry that shows a record": {func(d *Definition) { d.Entry = "detail" }, "shows a record"},
		"a duplicate screen":           {func(d *Definition) { d.Screens[1].Name = "tasks" }, "declared twice"},
		"a screen's name":              {func(d *Definition) { d.Screens[1].Name = "New Screen" }, "a name is"},
		"too many screens": {func(d *Definition) {
			for i := range MaxScreens {
				d.Screens = append(d.Screens, Screen{Name: fmt.Sprintf("s%d", i), Root: "r", Components: []json.RawMessage{json.RawMessage(`{"id":"r","component":"Text","text":"x"}`)}})
			}
		}, "screens: at most 12"},
		"too many components in all": {func(d *Definition) {
			var more []json.RawMessage
			for i := range MaxComponents - 10 {
				more = append(more, json.RawMessage(fmt.Sprintf(`{"id":"x%d","component":"Divider"}`, i)))
			}
			d.Screens[0].Components = append(slices.Clone(d.Screens[0].Components), more...)
		}, "components in all"},
		"an id used by two screens": {func(d *Definition) {
			d.Screens[1].Components = append(slices.Clone(d.Screens[1].Components), json.RawMessage(`{"id":"tasks-title","component":"Divider"}`))
		}, `id "tasks-title" is used twice`},
		"the host's id": {func(d *Definition) {
			d.Screens[1].Components = append(slices.Clone(d.Screens[1].Components), json.RawMessage(`{"id":"root","component":"Divider"}`))
		}, "the server's"},
		"a root that isn't there": {func(d *Definition) { d.Screens[1].Root = "nowhere" }, `no component has the id "nowhere"`},
		"a root called root":      {func(d *Definition) { d.Screens[1].Root = "root" }, "host"},
		"a reference to another screen": {func(d *Definition) {
			d.Screens[1].Components = append(slices.Clone(d.Screens[1].Components), json.RawMessage(`{"id":"peek","component":"Card","child":"tasks-title"}`))
		}, `refers to "tasks-title"`},
		"the host authored": {func(d *Definition) {
			d.Screens[1].Components = append(slices.Clone(d.Screens[1].Components), json.RawMessage(`{"id":"mine","component":"Screens","current":"x","screens":[]}`))
		}, `"Screens" is not a component`},
		"an unknown component": {func(d *Definition) {
			d.Screens[1].Components[1] = json.RawMessage(`{"id":"new-title","component":"Script","src":"x"}`)
		}, `"Script" is not a component`},
		"navigate to nowhere":       {func(d *Definition) { d.Actions["new_task"] = Action{Kind: ActionNavigate, Screen: "nowhere"} }, "navigate names a screen"},
		"navigate without a screen": {func(d *Definition) { d.Actions["new_task"] = Action{Kind: ActionNavigate} }, "navigate names a screen"},
		"navigate with then": {func(d *Definition) {
			d.Actions["new_task"] = Action{Kind: ActionNavigate, Screen: "new", Then: "tasks"}
		}, "only an insert, update or delete has then"},
		"a screen on an insert":   {func(d *Definition) { d.Actions["add_task"] = Action{Kind: ActionInsert, Screen: "new"} }, "only navigate has a screen"},
		"then to nowhere":         {func(d *Definition) { d.Actions["add_task"] = Action{Kind: ActionInsert, Then: "nowhere"} }, "then names a screen"},
		"then to a record screen": {func(d *Definition) { d.Actions["add_task"] = Action{Kind: ActionInsert, Then: "detail"} }, "shows a record"},
		"an event for a record screen without its id": {func(d *Definition) {
			d.Screens[0].Components[8] = json.RawMessage(`{"id":"task-open","component":"Button","child":"task-open-label","action":{"event":{"name":"open_task"}}}`)
		}, "needs the record's id"},
		"an event for another screen with an id": {func(d *Definition) {
			d.Screens[0].Components[10] = json.RawMessage(`{"id":"tasks-new","component":"Button","child":"tasks-new-label","action":{"event":{"name":"new_task","context":{"id":1}}}}`)
		}, "takes no id"},
		"an event naming no action": {func(d *Definition) {
			d.Screens[0].Components[10] = json.RawMessage(`{"id":"tasks-new","component":"Button","child":"tasks-new-label","action":{"event":{"name":"wipe"}}}`)
		}, `event "wipe" is not one of the app's actions`},
		"Show with a rule over an unknown field": {func(d *Definition) {
			d.Screens[1].Components[4] = json.RawMessage(`{"id":"new-note-when","component":"Show","when":{"field":"nope","op":"eq","value":"x"},"child":"new-note"}`)
		}, "doesn't declare"},
		"Show with a literal outside the options": {func(d *Definition) {
			d.Screens[1].Components[4] = json.RawMessage(`{"id":"new-note-when","component":"Show","when":{"field":"category","op":"eq","value":"nowhere"},"child":"new-note"}`)
		}, "must be one of the options"},
		"Show with a source of its own": {func(d *Definition) {
			d.Screens[1].Components[4] = json.RawMessage(`{"id":"new-note-when","component":"Show","source":"eval","when":{"field":"category","op":"eq","value":"other"},"child":"new-note"}`)
		}, "source: must be one of draft, record"},
		"Show without its rule": {func(d *Definition) {
			d.Screens[1].Components[4] = json.RawMessage(`{"id":"new-note-when","component":"Show","child":"new-note"}`)
		}, `Show needs "when"`},
		"Show with an expression": {func(d *Definition) {
			d.Screens[1].Components[4] = json.RawMessage(`{"id":"new-note-when","component":"Show","when":"category == 'other'","child":"new-note"}`)
		}, "a rule is an object"},
		"a choice without options":        {func(d *Definition) { d.Collection.Fields[1].Options = nil }, "1 to 50 options"},
		"a choice with a repeated option": {func(d *Definition) { d.Collection.Fields[1].Options = []string{"a", "a"} }, "different texts"},
		"a choice with a blank option":    {func(d *Definition) { d.Collection.Fields[1].Options = []string{"a", " "} }, "different texts"},
		"too many options": {func(d *Definition) {
			d.Collection.Fields[1].Options = nil
			for i := range MaxOptions + 1 {
				d.Collection.Fields[1].Options = append(d.Collection.Fields[1].Options, fmt.Sprintf("o%d", i))
			}
		}, "1 to 50 options"},
		"options on a string":  {func(d *Definition) { d.Collection.Fields[0].Options = []string{"a"} }, "only a choice field has options"},
		"max_length on a time": {func(d *Definition) { d.Collection.Fields[4].MaxLength = 5 }, "only a string field"},
		"a summary's bad where": {func(d *Definition) {
			d.Summaries["open"] = Summary{Kind: "count", Where: &Rule{Field: "done", Op: OpGt, Value: true}}
		}, "eq or ne only"},
		"summary over the screen": {func(d *Definition) { d.Summaries["screen"] = Summary{Kind: "count"} }, "screens' own"},
	}
	for name, tt := range bad {
		t.Run(name, func(t *testing.T) {
			d := planner(t)
			d.Screens = slices.Clone(d.Screens)
			for i := range d.Screens {
				d.Screens[i].Components = slices.Clone(d.Screens[i].Components)
			}
			d.Collection.Fields = slices.Clone(d.Collection.Fields)
			d.Actions = maps2(d.Actions)
			d.Summaries = map[string]Summary{"open": d.Summaries["open"]}
			tt.change(&d)
			if _, err := d.Check(); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want ErrInvalid containing %q", err, tt.want)
			}
		})
	}
}

func maps2(m map[string]Action) map[string]Action {
	out := make(map[string]Action, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func TestParseTimeAndChoice(t *testing.T) {
	def := planner(t)
	plan := func(ctx map[string]any) (Change, error) {
		return Plan(surfaceID, def, ClientAction{Name: "add_task", SurfaceID: surfaceID, Context: ctx})
	}
	ch, err := plan(map[string]any{"title": "x", "category": []any{"work"}, "due_time": " 07:05 "})
	if err != nil || ch.Data["category"] != "work" || ch.Data["due_time"] != "07:05" || ch.Then != "tasks" {
		t.Fatalf("a good row = %+v, %v", ch, err)
	}
	for name, ctx := range map[string]map[string]any{
		"a choice outside its options":  {"title": "x", "category": "play"},
		"two choices":                   {"title": "x", "category": []any{"work", "home"}},
		"a choice that isn't text":      {"title": "x", "category": 3.0},
		"a time without minutes":        {"title": "x", "category": "work", "due_time": "7"},
		"a time past midnight":          {"title": "x", "category": "work", "due_time": "24:00"},
		"a time with seconds":           {"title": "x", "category": "work", "due_time": "07:05:00"},
		"a time that is a date":         {"title": "x", "category": "work", "due_time": "2026-11-18"},
		"no choice at all":              {"title": "x"},
		"a note missing where required": {"title": "x", "category": "other"},
	} {
		if _, err := plan(ctx); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	shrunk := fieldsOf(def, "category", func(f *Field) { f.Options = []string{"work"} })
	if it := item(shrunk, Row{ID: 1, Data: map[string]any{"category": "home"}}); it["category"] != "home" {
		t.Fatalf("item = %v", it)
	}
}

func TestPlanNavigate(t *testing.T) {
	def := planner(t)
	nav := func(name string, ctx map[string]any) (Change, error) {
		return Plan(surfaceID, def, ClientAction{Name: name, SurfaceID: surfaceID, Context: ctx})
	}
	if ch, err := nav("new_task", nil); err != nil || ch.Kind != ActionNavigate || ch.Screen != "new" || ch.ID != 0 {
		t.Fatalf("navigate = %+v, %v", ch, err)
	}
	if ch, err := nav("open_task", map[string]any{"id": 2.0}); err != nil || ch.Screen != "detail" || ch.ID != 2 {
		t.Fatalf("open = %+v, %v", ch, err)
	}
	for name, a := range map[string]struct {
		action string
		ctx    map[string]any
	}{
		"a record screen without an id": {"open_task", nil},
		"a bad id":                      {"open_task", map[string]any{"id": 0.0}},
		"another key":                   {"open_task", map[string]any{"id": 1.0, "title": "x"}},
		"an id for a screen with none":  {"new_task", map[string]any{"id": 1.0}},
		"a key for a screen with none":  {"new_task", map[string]any{"title": "x"}},
		"an action that isn't declared": {"go_anywhere", nil},
	} {
		if _, err := nav(a.action, a.ctx); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if ch, err := Plan(surfaceID, def, ClientAction{Name: "open_task", SurfaceID: surfaceID, Context: map[string]any{"id": 2.0}, ExpectedRevision: rev(1)}); err != nil || ch.Screen != "detail" || ch.ID != 2 || ch.Expected != nil {
		t.Errorf("a revision on a navigate: %+v, %v", ch, err)
	}
}

func TestAnswersKeepTheScreenAndTheForm(t *testing.T) {
	def := planner(t)
	msgs := Refreshed(surfaceID, def, plannerSeed, false)
	raw, _ := json.Marshal(msgs)
	for _, own := range []string{`"path":"/"`, `"path":"/screen"`, `"path":"/record"`, `"path":"/draft"`} {
		if strings.Contains(string(raw), own) {
			t.Fatalf("a refresh of an app with screens sends %s: %s", own, raw)
		}
	}
	if !strings.Contains(string(raw), `"path":"/items"`) || !strings.Contains(string(raw), `"path":"/open"`) {
		t.Fatalf("a refresh lacks the rows or summaries: %s", raw)
	}
	if !strings.Contains(string(raw), `"value":"1"`) {
		t.Fatalf("open = %s", raw)
	}
	if raw, _ := json.Marshal(Refreshed(surfaceID, def, plannerSeed, true)); !strings.Contains(string(raw), `"path":"/draft"`) {
		t.Fatal("an insert doesn't clear the form")
	}
	if got := Refreshed(surfaceID, example(t), seed, false); len(got) != 1 || got[0]["updateDataModel"].(map[string]any)["path"] != "/" {
		t.Fatalf("a single-screen answer = %v", got)
	}
}

func (w *world) createFrom(u *user, s access.Space, def Definition) App {
	w.e.t.Helper()
	rec := u.do(http.MethodPost, appsPath(s, ""), map[string]any{"name": "Planner", "definition": def})
	expect(w.e.t, rec, http.StatusCreated)
	return w.publish(u, s, decode[App](w.e.t, rec))
}

func (w *world) post(u *user, s access.Space, a App, release *int64, name string, ctx map[string]any) *httptest.ResponseRecorder {
	w.e.t.Helper()
	ca := ClientAction{Name: name, SurfaceID: SurfaceID(a.ID), SourceComponentID: "x", Context: ctx, Release: release}
	return u.do(http.MethodPost, appPath(s, a.ID, "/actions"), ca)
}

func TestNavigationAndConditionalRequirementsOverHTTP(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.createFrom(w.alice, s, planner(t))

	body := w.surfaceBody(w.alice, s, a, "")
	for _, want := range []string{`"component":"Screens"`, `"value":{"draft":{},"items":[],"open":"0","record":{},"screen":"tasks"}`} {
		if !strings.Contains(body, want) {
			t.Fatalf("surface lacks %s: %s", want, body)
		}
	}

	for name, tt := range map[string]struct {
		ctx  map[string]any
		code string
	}{
		"a note when other":    {map[string]any{"title": "x", "category": "other"}, "app_field_required_when"},
		"a blank note":         {map[string]any{"title": "x", "category": "other", "category_note": "  "}, "app_field_required_when"},
		"a choice not offered": {map[string]any{"title": "x", "category": "play"}, "app_field_not_option"},
		"a bad time":           {map[string]any{"title": "x", "category": "work", "due_time": "25:00"}, "app_field_not_time"},
	} {
		rec := w.post(w.alice, s, a, nil, "add_task", tt.ctx)
		expect(t, rec, http.StatusUnprocessableEntity)
		if errorCode(t, rec) != tt.code {
			t.Fatalf("%s: %s", name, rec.Body)
		}
	}
	if rows := storedRows(t, w.e.db, a.ID); len(rows) != 0 {
		t.Fatalf("refused actions stored %v", rows)
	}

	rec := w.post(w.alice, s, a, nil, "add_task", map[string]any{"title": "Fix", "category": []any{"other"}, "category_note": "errand", "due_time": "08:15"})
	expect(t, rec, http.StatusOK)
	for _, want := range []string{`"path":"/items"`, `"path":"/draft"`, `"path":"/screen"`, `"value":"tasks"`, `"due_time":"08:15"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("add answer lacks %s: %s", want, rec.Body)
		}
	}
	if strings.Contains(rec.Body.String(), `"path":"/"`) {
		t.Fatalf("the answer replaced the whole data model: %s", rec.Body)
	}
	expect(t, w.post(w.alice, s, a, nil, "add_task", map[string]any{"title": "Run", "category": "work"}), http.StatusOK)
	items := w.items(w.alice, s, a)
	id := int64(items[0]["id"].(float64))
	home := int64(items[1]["id"].(float64))

	expect(t, w.post(w.alice, s, a, nil, "complete_task", map[string]any{"id": id, "done": true}), http.StatusOK)
	nonote := w.post(w.alice, s, a, nil, "complete_task", map[string]any{"id": home, "category": "other"})
	expect(t, nonote, http.StatusUnprocessableEntity)
	if errorCode(t, nonote) != "app_field_required_when" {
		t.Fatalf("an update making the note required: %s", nonote.Body)
	}

	rec = w.post(w.bob, s, a, nil, "new_task", nil)
	expect(t, rec, http.StatusOK)
	if got := rec.Body.String(); !strings.Contains(got, `"value":"new"`) || strings.Contains(got, "items") || strings.Contains(got, "draft") {
		t.Fatalf("navigate answer = %s", got)
	}
	rec = w.post(w.bob, s, a, nil, "open_task", map[string]any{"id": id})
	expect(t, rec, http.StatusOK)
	if got := rec.Body.String(); !strings.Contains(got, `"value":"detail"`) || !strings.Contains(got, "errand") {
		t.Fatalf("open answer = %s", got)
	}
	expect(t, w.post(w.bob, s, a, nil, "add_task", map[string]any{"title": "x", "category": "work"}), http.StatusForbidden)
	rec = w.post(w.alice, s, a, nil, "open_task", map[string]any{"id": 9999})
	expect(t, rec, http.StatusUnprocessableEntity)
	if errorCode(t, rec) != "app_row_not_found" {
		t.Fatalf("a missing record: %s", rec.Body)
	}
	pa := w.createFrom(w.alice, w.alicePrivate, planner(t))
	expect(t, w.post(w.bob, w.alicePrivate, pa, nil, "new_task", nil), http.StatusNotFound)
	expect(t, w.post(w.mallory, w.shared, a, nil, "new_task", nil), http.StatusNotFound)
	draft := decode[App](t, w.alice.do(http.MethodPost, appsPath(s, ""), map[string]any{"name": "Draft", "definition": planner(t)}))
	expect(t, w.post(w.alice, s, draft, nil, "new_task", nil), http.StatusConflict)
}

func TestADetailScreenOpensOnlyWhatTheCallerReads(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.createFrom(w.alice, s, planner(t))
	expect(t, w.post(w.alice, s, a, nil, "add_task", map[string]any{"title": "Alice's", "category": "work"}), http.StatusOK)
	id := int64(w.items(w.alice, s, a)[0]["id"].(float64))
	w.mustSetRoles(w.alice, s, a, grant(w.alice.id, access.AppRoles...), grant(w.bob.id, access.AppUser))
	rec := w.post(w.bob, s, a, nil, "open_task", map[string]any{"id": id})
	expect(t, rec, http.StatusUnprocessableEntity)
	if strings.Contains(rec.Body.String(), "Alice's") {
		t.Fatalf("a record the caller doesn't read leaked: %s", rec.Body)
	}
	expect(t, w.post(w.alice, s, a, nil, "open_task", map[string]any{"id": id}), http.StatusOK)
}

func TestAnOlderReleaseCannotSkipAConditionalRequirement(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	loose := fieldsOf(planner(t), "category_note", func(f *Field) { f.RequiredWhen = nil })
	a := w.createFrom(w.alice, s, loose)
	expect(t, w.post(w.alice, s, a, nil, "add_task", map[string]any{"title": "Fix", "category": "other"}), http.StatusOK)

	cur := w.view(w.alice, s, a)
	cur = w.mustPatchDraft(w.alice, s, cur, planner(t))
	rec := w.tryPublish(w.alice, s, cur)
	expect(t, rec, http.StatusConflict)
	if errorCode(t, rec) != "app_migration_required" {
		t.Fatalf("an unacknowledged conditional requirement = %s", rec.Body)
	}
	expect(t, w.tryPublish(w.alice, s, cur, SchemaChange{ChangeFieldRequiredWhen, "category_note"}), http.StatusOK)

	rec = w.post(w.alice, s, a, release(1), "add_task", map[string]any{"title": "Again", "category": "other"})
	expect(t, rec, http.StatusUnprocessableEntity)
	if errorCode(t, rec) != "app_field_required_when" {
		t.Fatalf("an older release's insert = %s", rec.Body)
	}
	expect(t, w.post(w.alice, s, a, release(1), "add_task", map[string]any{"title": "Again", "category": "other", "category_note": "n"}), http.StatusOK)
	id := int64(w.items(w.alice, s, a)[0]["id"].(float64))
	expect(t, w.post(w.alice, s, a, release(1), "complete_task", map[string]any{"id": id, "done": true}), http.StatusUnprocessableEntity)
	expect(t, w.post(w.alice, s, a, nil, "complete_task", map[string]any{"id": id, "done": true, "category_note": "later"}), http.StatusOK)
	rec = w.tryRollback(w.alice, s, w.view(w.alice, s, a), 1)
	expect(t, rec, http.StatusOK)
}

func TestNewKindsOfSchemaChangeAreJudgedBlindForWhoReadsNoRows(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	loose := fieldsOf(planner(t), "category_note", func(f *Field) { f.RequiredWhen = nil })
	shrunk := fieldsOf(planner(t), "category", func(f *Field) { f.Options = []string{"work", "other"} })
	for _, withRow := range []bool{false, true} {
		a := w.createFrom(w.alice, s, loose)
		if withRow {
			expect(t, w.post(w.alice, s, a, nil, "add_task", map[string]any{"title": "Fix", "category": "other"}), http.StatusOK)
		}
		w.mustSetRoles(w.alice, s, a, grant(w.owner.id, access.AppBuilder, access.AppPublisher), grant(w.alice.id, access.AppRoles...))
		cur := w.view(w.owner, s, a)
		if cur.Access.ReadRecords != access.ScopeNone {
			t.Fatalf("the owner reads records: %+v", cur.Access)
		}
		cur = w.mustPatchDraft(w.owner, s, cur, planner(t))
		rec := w.tryPublish(w.owner, s, cur)
		expect(t, rec, http.StatusConflict)
		if errorCode(t, rec) != "app_migration_required" || !strings.Contains(rec.Body.String(), ChangeFieldRequiredWhen) {
			t.Fatalf("with a row = %v: %s", withRow, rec.Body)
		}
		cur = decode[App](t, w.tryPublish(w.owner, s, cur, SchemaChange{ChangeFieldRequiredWhen, "category_note"}))
		cur = w.mustPatchDraft(w.owner, s, cur, shrunk)
		rec = w.tryPublish(w.owner, s, cur)
		expect(t, rec, http.StatusConflict)
		if !strings.Contains(rec.Body.String(), ChangeFieldNarrowed) {
			t.Fatalf("with a row = %v: %s", withRow, rec.Body)
		}
		expect(t, w.tryPublish(w.owner, s, cur, SchemaChange{ChangeFieldNarrowed, "category"}), http.StatusOK)
	}
}

func TestNewKindsOfSchemaChangeAreJudgedByRowsForWhoReadsThemAll(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	loose := fieldsOf(planner(t), "category_note", func(f *Field) { f.RequiredWhen = nil })
	a := w.createFrom(w.alice, s, loose)
	expect(t, w.post(w.alice, s, a, nil, "add_task", map[string]any{"title": "Run", "category": "work"}), http.StatusOK)
	cur := w.view(w.alice, s, a)
	cur = w.mustPatchDraft(w.alice, s, cur, fieldsOf(planner(t), "category", func(f *Field) { f.Options = []string{"work", "home", "other", "more"} }))
	expect(t, w.tryPublish(w.alice, s, cur), http.StatusOK)
	cur = w.mustPatchDraft(w.alice, s, w.view(w.alice, s, a), fieldsOf(planner(t), "category", func(f *Field) { f.Options = []string{"work", "other"} }))
	expect(t, w.tryPublish(w.alice, s, cur), http.StatusOK)
	expect(t, w.post(w.alice, s, a, nil, "add_task", map[string]any{"title": "Mop", "category": "work"}), http.StatusOK)
	cur = w.mustPatchDraft(w.alice, s, w.view(w.alice, s, a), fieldsOf(planner(t), "category", func(f *Field) {
		f.Options = []string{"other"}
	}))
	expect(t, w.tryPublish(w.alice, s, cur), http.StatusConflict)
}
