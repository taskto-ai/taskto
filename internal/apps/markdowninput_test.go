package apps

import (
	"net/http"
	"strings"
	"testing"
)

func markdownApp(field string) string {
	return `{
  "components": [
    {"id": "root", "component": "Column", "children": ["body", "add"]},
    {"id": "body", "component": "MarkdownInput", "field": "` + field + `", "label": "Notes", "value": {"path": "/draft/` + field + `"}},
    {"id": "add_label", "component": "Text", "text": "Add"},
    {"id": "add", "component": "Button", "child": "add_label", "action": {"event": {"name": "add_note", "context": {
      "title": {"path": "/draft/title"}, "notes": {"path": "/draft/notes"}}}}}
  ],
  "collection": {"fields": [
    {"name": "title", "type": "string"},
    {"name": "notes", "type": "string", "max_length": 200},
    {"name": "amount", "type": "number"}
  ]},
  "actions": {"add_note": {"kind": "insert"}}
}`
}

func TestAMarkdownInputTakesOnlyAStringField(t *testing.T) {
	for field, wantOK := range map[string]bool{"notes": true, "title": true, "amount": false, "missing": false} {
		var d Definition
		if err := jsonUnmarshal(markdownApp(field), &d); err != nil {
			t.Fatal(err)
		}
		checked, err := d.Check()
		if wantOK {
			if err != nil {
				t.Errorf("%s: %v", field, err)
				continue
			}
			if !contains(checked.Requires, ComponentCapability("MarkdownInput")) {
				t.Errorf("requires %v lacks the MarkdownInput capability", checked.Requires)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "is not a string field") {
			t.Errorf("%s: %v", field, err)
		}
	}
}

func TestAMarkdownInputKeepsItsFieldsLimitAndNoOtherString(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	var def map[string]any
	if err := jsonUnmarshal(markdownApp("notes"), &def); err != nil {
		t.Fatal(err)
	}
	rec := w.alice.do(http.MethodPost, appsPath(s, ""), map[string]any{"name": "Notes", "definition": def})
	expect(t, rec, http.StatusCreated)
	a := w.publish(w.alice, s, decode[App](t, rec))

	surface := w.alice.do(http.MethodGet, appPath(s, a.ID, "/surface"), nil).Body.String()
	if !strings.Contains(surface, `"_fields":{"notes":{"editable":false,"max_length":200,"required":false}}`) {
		t.Fatalf("surface lacks the limit of the field: %s", surface)
	}

	over := w.insert(w.alice, s, a, map[string]any{"notes": strings.Repeat("x", 201)})
	expect(t, over, http.StatusUnprocessableEntity)
	if got := decode[struct{ Code string }](t, over).Code; got != "app_field_too_long" {
		t.Fatalf("code = %q", got)
	}
	expect(t, w.insert(w.alice, s, a, map[string]any{"notes": strings.Repeat("x", 200)}), http.StatusOK)
}

func TestAStringFieldWithNoMarkdownInputHasNoLimitInTheModel(t *testing.T) {
	var d Definition
	if err := jsonUnmarshal(markdownApp("notes"), &d); err != nil {
		t.Fatal(err)
	}
	checked, err := d.Check()
	if err != nil {
		t.Fatal(err)
	}
	checked.Components = checked.Components[:1]
	checked.Components[0] = []byte(`{"id": "root", "component": "Column", "children": []}`)
	if got := checked.fieldsModel(); got != nil {
		t.Fatalf("fields model = %v", got)
	}
}
