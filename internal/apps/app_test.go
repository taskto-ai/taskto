package apps

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The golden files are the contract with the web renderer: web/src/apps/apps.test.tsx draws
// the same surface, sends expenses.action.json from a real button click, applies the
// response, and checks its catalog against catalog.json. Regenerate with:
// go test ./internal/apps -update
var update = flag.Bool("update", false, "rewrite golden files")

// surfaceID is the example's surface in the golden files.
var surfaceID = SurfaceID(1)

func example(t *testing.T) Definition {
	t.Helper()
	all, err := Examples()
	if err != nil {
		t.Fatal(err)
	}
	for _, ex := range all {
		if ex.Name == "Expense tracker" {
			return ex.Definition
		}
	}
	t.Fatal("no Expense tracker example")
	return Definition{}
}

func golden(t *testing.T, name string, got any) {
	t.Helper()
	data, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(data, want) {
		t.Fatalf("%s differs from the golden file:\n%s", name, data)
	}
}

var seed = []Row{{ID: 1, Data: map[string]any{"description": "Coffee", "amount": 4.5}}}

func TestSurface(t *testing.T) {
	golden(t, "expenses.surface.json", Surface(surfaceID, example(t), seed))
}

// The catalog as the web app must register it: every component with its props, and the
// functions.
func TestCatalogContract(t *testing.T) {
	comps := map[string][]string{}
	for name, spec := range components {
		comps[name] = sortedKeys(spec)
	}
	golden(t, "catalog.json", map[string]any{"id": CatalogID, "components": comps, "functions": sortedKeys(functions)})
}

func TestStoredDefinitionDrawsTheSameSurface(t *testing.T) {
	def := example(t)
	stored, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	var reloaded Definition
	if err := json.Unmarshal(stored, &reloaded); err != nil {
		t.Fatal(err)
	}
	if again, err := reloaded.Check(); err != nil {
		t.Fatalf("a stored definition no longer checks: %v", err)
	} else {
		reloaded = again
	}
	a, _ := json.Marshal(Surface(surfaceID, def, seed))
	b, _ := json.Marshal(Surface(surfaceID, reloaded, seed))
	if !bytes.Equal(a, b) {
		t.Fatal("a definition stored and loaded again draws a different surface")
	}
}

func TestPlanGoldenAction(t *testing.T) {
	var action ClientAction
	raw, err := os.ReadFile("testdata/expenses.action.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &action); err != nil {
		t.Fatal(err)
	}
	def := example(t)
	ch, err := Plan(surfaceID, def, action)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Kind != ActionInsert || ch.Data["description"] != "Lunch" || ch.Data["amount"] != 12.25 {
		t.Fatalf("change = %+v", ch)
	}
	golden(t, "expenses.after-add.json", []Message{DataModel(surfaceID, def, append(seed, Row{ID: 2, Data: ch.Data}))})
}

// screen returns the example with change applied to its components, decoded as objects.
func screen(t *testing.T, change func(map[string]map[string]any) []map[string]any) Definition {
	t.Helper()
	def := example(t)
	byID := map[string]map[string]any{}
	var order []string
	for _, raw := range def.Components {
		var c map[string]any
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		byID[c["id"].(string)] = c
		order = append(order, c["id"].(string))
	}
	extra := change(byID)
	def.Components = nil
	for _, id := range order {
		if c, kept := byID[id]; kept {
			raw, _ := json.Marshal(c)
			def.Components = append(def.Components, raw)
		}
	}
	for _, c := range extra {
		raw, _ := json.Marshal(c)
		def.Components = append(def.Components, raw)
	}
	return def
}

func TestCheckRefusesScreensOutsideTheCatalog(t *testing.T) {
	if _, err := example(t).Check(); err != nil {
		t.Fatalf("the example: %v", err)
	}
	type comps = map[string]map[string]any
	bad := map[string]struct {
		change func(comps) []map[string]any
		want   string
	}{
		"unknown component": {func(c comps) []map[string]any {
			c["title"]["component"] = "Script"
			return nil
		}, `"Script" is not a component`},
		"a basic component left out": {func(c comps) []map[string]any {
			c["title"] = map[string]any{"id": "title", "component": "Image", "url": "https://elsewhere.example/p.png"}
			return nil
		}, `"Image" is not a component`},
		"unknown prop": {func(c comps) []map[string]any {
			c["title"]["onclick"] = "alert(1)"
			return nil
		}, `Text has no prop "onclick"`},
		"prop of the wrong type": {func(c comps) []map[string]any {
			c["title"]["text"] = 42
			return nil
		}, "text: must be text"},
		"enum value outside its list": {func(c comps) []map[string]any {
			c["title"]["variant"] = "blink"
			return nil
		}, "variant: must be one of"},
		"missing required prop": {func(c comps) []map[string]any {
			delete(c["add"], "action")
			return nil
		}, `Button needs "action"`},
		"binding with more keys": {func(c comps) []map[string]any {
			c["total"]["value"] = map[string]any{"path": "/total", "script": "x"}
			return nil
		}, "data binding"},
		"no root": {func(c comps) []map[string]any {
			c["root"]["id"] = "main"
			return nil
		}, `no component has the id "root"`},
		"duplicate id": {func(c comps) []map[string]any {
			return []map[string]any{{"id": "title", "component": "Divider"}}
		}, `id "title" is used twice`},
		"dangling reference": {func(c comps) []map[string]any {
			c["root"]["children"] = []any{"title", "nowhere"}
			return nil
		}, `refers to "nowhere"`},
		"a cycle": {func(c comps) []map[string]any {
			c["form"]["children"] = []any{"description", "root"}
			return nil
		}, "contains itself"},
		"event the app doesn't declare": {func(c comps) []map[string]any {
			c["add"]["action"] = map[string]any{"event": map[string]any{"name": "wipe_everything"}}
			return nil
		}, `event "wipe_everything" is not one of the app's actions`},
		"openUrl": {func(c comps) []map[string]any {
			c["add"]["action"] = map[string]any{"functionCall": map[string]any{"call": "openUrl", "args": map[string]any{"url": "https://elsewhere.example"}}}
			return nil
		}, `"openUrl" is not a function`},
		"function of the wrong type": {func(c comps) []map[string]any {
			c["title"]["text"] = map[string]any{"call": "email", "args": map[string]any{"value": "x"}}
			return nil
		}, "email returns a boolean, not a string"},
		"unknown function argument": {func(c comps) []map[string]any {
			c["title"]["text"] = map[string]any{"call": "formatString", "args": map[string]any{"value": "x", "eval": "y"}}
			return nil
		}, `unknown key "eval"`},
		"length without bounds": {func(c comps) []map[string]any {
			c["description"]["checks"] = []any{map[string]any{"condition": map[string]any{"call": "length", "args": map[string]any{"value": "x"}}, "message": "m"}}
			return nil
		}, "length needs min or max"},
		"too many components": {func(c comps) []map[string]any {
			var more []map[string]any
			for i := range MaxComponents {
				more = append(more, map[string]any{"id": fmt.Sprintf("d%d", i), "component": "Divider"})
			}
			return more
		}, "at most 300"},
	}
	for name, tt := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := screen(t, tt.change).Check()
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want ErrInvalid containing %q", err, tt.want)
			}
		})
	}
}

func TestCheckAcceptsTheWholeCatalog(t *testing.T) {
	def := screen(t, func(c map[string]map[string]any) []map[string]any {
		c["root"]["children"] = []any{"title", "total", "items", "form", "tabs", "modal", "card", "icon", "check", "pick", "slide", "when", "rule"}
		dyn := func(fn string, args map[string]any) map[string]any { return map[string]any{"call": fn, "args": args} }
		return []map[string]any{
			{"id": "tabs", "component": "Tabs", "tabs": []any{map[string]any{"title": "One", "child": "tab-one"}}},
			{"id": "tab-one", "component": "Text", "text": dyn("pluralize", map[string]any{"value": map[string]any{"path": "/n"}, "one": "a row", "other": "rows"})},
			{"id": "modal", "component": "Modal", "trigger": "modal-open", "content": "modal-body"},
			{"id": "modal-open", "component": "Text", "text": "Open"},
			{"id": "modal-body", "component": "Text", "text": dyn("formatCurrency", map[string]any{"value": 3, "currency": "EUR"}), "variant": "caption"},
			{"id": "card", "component": "Card", "child": "card-body", "weight": 1},
			{"id": "card-body", "component": "Divider", "axis": "vertical"},
			{"id": "icon", "component": "Icon", "name": "star", "accessibility": map[string]any{"label": "Star"}},
			{"id": "check", "component": "CheckBox", "label": "Done", "value": map[string]any{"path": "/draft/done"}},
			{"id": "pick", "component": "ChoicePicker", "options": []any{map[string]any{"label": "A", "value": "a"}}, "value": []any{"a"}, "displayStyle": "chips"},
			{"id": "slide", "component": "Slider", "max": 10, "value": map[string]any{"path": "/draft/n"}},
			{"id": "when", "component": "DateTimeInput", "value": map[string]any{"path": "/draft/when"}, "enableDate": true, "min": "2026-01-01"},
			{"id": "rule", "component": "TextField", "label": "Code", "checks": []any{map[string]any{
				"condition": dyn("and", map[string]any{"values": []any{
					dyn("required", map[string]any{"value": map[string]any{"path": "/draft/code"}}),
					dyn("email", map[string]any{"value": map[string]any{"path": "/draft/code"}}),
				}}),
				"message": "Required, and an email address",
			}}},
		}
	})
	if _, err := def.Check(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckRefusesBadDefinitions(t *testing.T) {
	n := func(f float64) *float64 { return &f }
	bad := map[string]struct {
		change func(*Definition)
		want   string
	}{
		"field type":       {func(d *Definition) { d.Collection.Fields[0].Type = "blob" }, "type must be"},
		"duplicate field":  {func(d *Definition) { d.Collection.Fields[1].Name = d.Collection.Fields[0].Name }, "declared twice"},
		"reserved field":   {func(d *Definition) { d.Collection.Fields[0].Name = "id" }, "not items, draft or id"},
		"field name":       {func(d *Definition) { d.Collection.Fields[0].Name = "../x" }, "a name is"},
		"max_length":       {func(d *Definition) { d.Collection.Fields[0].MaxLength = MaxMaxLength + 1 }, "max_length is 1 to"},
		"min over max":     {func(d *Definition) { d.Collection.Fields[1].Min, d.Collection.Fields[1].Max = n(5), n(1) }, "min is more than max"},
		"limit of a type":  {func(d *Definition) { d.Collection.Fields[0].Min = n(1) }, "only a number field"},
		"action kind":      {func(d *Definition) { d.Actions["wipe"] = Action{Kind: "delete_all"} }, "kind must be insert, update or delete"},
		"no action":        {func(d *Definition) { d.Actions = nil }, "at least one"},
		"reserved summary": {func(d *Definition) { d.Summaries["items"] = Summary{Kind: "count"} }, "not items"},
		"sum of text":      {func(d *Definition) { d.Summaries["total"] = Summary{Kind: "sum", Field: "description"} }, "not a number field"},
		"summary kind":     {func(d *Definition) { d.Summaries["avg"] = Summary{Kind: "avg", Field: "amount"} }, "kind must be count or sum"},
		// No regular expressions: a pattern runs in every viewer's browser (see catalog.go).
		"validationRegexp": {func(d *Definition) {
			raw, _ := json.Marshal(map[string]any{"id": "code", "component": "TextField", "label": "Code", "validationRegexp": "(a+)+$"})
			d.Components = append(d.Components, raw)
		}, `TextField has no prop "validationRegexp"`},
		"regex function": {func(d *Definition) {
			raw, _ := json.Marshal(map[string]any{"id": "code", "component": "TextField", "label": "Code", "checks": []any{map[string]any{
				"condition": map[string]any{"call": "regex", "args": map[string]any{"value": map[string]any{"path": "/draft/code"}, "pattern": "(x|xx)*y"}},
				"message":   "bad",
			}}})
			d.Components = append(d.Components, raw)
		}, `"regex" is not a function of the catalog`},
		"too large": {func(d *Definition) {
			long := strings.Repeat("x", maxTextLen)
			for i := range 15 {
				raw, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("big%d", i), "component": "Text", "text": long})
				d.Components = append(d.Components, raw)
			}
		}, "over 48 KiB"},
	}
	for name, tt := range bad {
		t.Run(name, func(t *testing.T) {
			def := example(t)
			def.Collection.Fields = append([]Field(nil), def.Collection.Fields...)
			def.Actions = map[string]Action{"add_expense": {Kind: ActionInsert}, "delete_expense": {Kind: ActionDelete}}
			def.Summaries = map[string]Summary{"total": {Kind: "sum", Field: "amount"}}
			tt.change(&def)
			if _, err := def.Check(); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want ErrInvalid containing %q", err, tt.want)
			}
		})
	}
}

// todo is an app whose actions are of every kind, over every type of field.
func todo(t *testing.T) Definition {
	t.Helper()
	max := 10.0
	def, err := Definition{
		Components: []json.RawMessage{json.RawMessage(`{"id":"root","component":"Text","text":"Todo"}`)},
		Collection: Collection{Fields: []Field{
			{Name: "title", Type: TypeString, Required: true, MaxLength: 10},
			{Name: "done", Type: TypeBoolean},
			{Name: "due", Type: TypeDate},
			{Name: "points", Type: TypeNumber, Max: &max},
		}},
		Actions: map[string]Action{"add": {Kind: ActionInsert}, "edit": {Kind: ActionUpdate}, "remove": {Kind: ActionDelete}},
	}.Check()
	if err != nil {
		t.Fatal(err)
	}
	return def
}

func TestPlan(t *testing.T) {
	def := todo(t)
	act := func(name string, ctx map[string]any) (Change, error) {
		return Plan(surfaceID, def, ClientAction{Name: name, SurfaceID: surfaceID, Context: ctx})
	}
	ch, err := act("add", map[string]any{"title": " Milk ", "done": "false", "due": "2026-11-18", "points": "3"})
	if err != nil || ch.Data["title"] != "Milk" || ch.Data["done"] != false || ch.Data["due"] != "2026-11-18" || ch.Data["points"] != 3.0 {
		t.Fatalf("insert = %+v, %v", ch, err)
	}
	ch, err = act("edit", map[string]any{"id": 7.0, "done": true, "due": ""})
	if err != nil || ch.ID != 7 || ch.Data["done"] != true || ch.Data["due"] != nil {
		t.Fatalf("update = %+v, %v", ch, err)
	}
	row, err := def.Collection.Merge(map[string]any{"title": "Milk", "due": "2026-11-18", "old": "kept"}, ch.Data)
	if err != nil || row["done"] != true || row["due"] != nil || row["old"] != "kept" {
		t.Fatalf("merged = %v, %v", row, err)
	}
	if _, err := def.Collection.Merge(map[string]any{"title": "Milk"}, map[string]any{"title": nil}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("clearing a required field: %v", err)
	}
	if ch, err = act("remove", map[string]any{"id": "7"}); err != nil || ch.ID != 7 {
		t.Fatalf("delete = %+v, %v", ch, err)
	}
}

func TestPlanRefusesBadActions(t *testing.T) {
	def := todo(t)
	bad := map[string]ClientAction{
		"unknown action":        {Name: "drop", Context: map[string]any{}},
		"another surface":       {Name: "add", SurfaceID: "app-2", Context: map[string]any{"title": "x"}},
		"missing required":      {Name: "add", Context: map[string]any{"done": true}},
		"blank required":        {Name: "add", Context: map[string]any{"title": "  "}},
		"too long":              {Name: "add", Context: map[string]any{"title": "eleven char"}},
		"number not a number":   {Name: "add", Context: map[string]any{"title": "x", "points": "lots"}},
		"NaN":                   {Name: "add", Context: map[string]any{"title": "x", "points": "NaN"}},
		"infinity":              {Name: "add", Context: map[string]any{"title": "x", "points": "Inf"}},
		"over max":              {Name: "add", Context: map[string]any{"title": "x", "points": 11.0}},
		"bad boolean":           {Name: "add", Context: map[string]any{"title": "x", "done": "maybe"}},
		"bad date":              {Name: "add", Context: map[string]any{"title": "x", "due": "18/11/2026"}},
		"text for a string":     {Name: "add", Context: map[string]any{"title": 5.0}},
		"undeclared field":      {Name: "add", Context: map[string]any{"title": "x", "owner": "me"}},
		"id on insert":          {Name: "add", Context: map[string]any{"title": "x", "id": 1.0}},
		"update without id":     {Name: "edit", Context: map[string]any{"done": true}},
		"update with a bad id":  {Name: "edit", Context: map[string]any{"id": 1.5}},
		"delete with fields":    {Name: "remove", Context: map[string]any{"id": 1.0, "title": "x"}},
		"delete with a zero id": {Name: "remove", Context: map[string]any{"id": 0.0}},
	}
	for name, a := range bad {
		t.Run(name, func(t *testing.T) {
			if a.SurfaceID == "" {
				a.SurfaceID = surfaceID
			}
			if _, err := Plan(surfaceID, def, a); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestDataModelShowsDeclaredFieldsOnly(t *testing.T) {
	def := todo(t)
	msg := DataModel(surfaceID, def, []Row{{ID: 3, Data: map[string]any{"title": "Milk", "old": "hidden"}}})
	raw, _ := json.Marshal(msg)
	if !strings.Contains(string(raw), `"items":[{"id":3,"title":"Milk"}]`) || strings.Contains(string(raw), "hidden") {
		t.Fatalf("data model = %s", raw)
	}
}
