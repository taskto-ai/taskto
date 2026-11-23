package apps

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// testdata/rules.json holds the cases both this package and the web app (web/src/apps/rules.test.ts)
// evaluate rules by: they must agree, since the browser hides what the server then requires.
func TestRulesAreEvaluatedAsTheSharedCasesSay(t *testing.T) {
	raw, err := os.ReadFile("testdata/rules.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Collection Collection
		Cases      []struct {
			Name  string
			Rule  Rule
			Row   map[string]any
			Holds bool
		}
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	def, err := Definition{
		Components: []json.RawMessage{json.RawMessage(`{"id":"root","component":"Text","text":"x"}`)},
		Collection: file.Collection,
		Actions:    map[string]Action{"add": {Kind: ActionInsert}},
	}.Check()
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range file.Cases {
		t.Run(tt.Name, func(t *testing.T) {
			if err := tt.Rule.check(def.Collection); err != nil {
				t.Fatalf("the rule doesn't check: %v", err)
			}
			if got := def.Collection.holds(tt.Rule, tt.Row); got != tt.Holds {
				t.Fatalf("holds = %v, want %v", got, tt.Holds)
			}
		})
	}
}

func TestRulesAreCheckedStrictly(t *testing.T) {
	c := Collection{Fields: []Field{
		{Name: "kind", Type: TypeChoice, Options: []string{"a", "b"}},
		{Name: "n", Type: TypeNumber},
		{Name: "t", Type: TypeString},
		{Name: "ok", Type: TypeBoolean},
		{Name: "at", Type: TypeTime},
		{Name: "day", Type: TypeDate},
	}}
	deep := `{"field":"n","op":"eq","value":1}`
	for range maxRuleDepth {
		deep = `{"all":[` + deep + `]}`
	}
	many := strings.TrimSuffix(strings.Repeat(`{"field":"n","op":"eq","value":1},`, maxRuleList+1), ",")
	var wide []string
	for range maxRuleNodes / maxRuleList {
		wide = append(wide, `{"all":[`+strings.TrimSuffix(strings.Repeat(`{"field":"n","op":"eq","value":1},`, maxRuleList), ",")+`]}`)
	}
	bad := map[string]struct{ rule, want string }{
		"an unknown field":              {`{"field":"nope","op":"eq","value":1}`, "doesn't declare"},
		"an unknown operator":           {`{"field":"n","op":"matches","value":1}`, "op must be one of"},
		"no operator":                   {`{"field":"n","value":1}`, "op must be one of"},
		"no value":                      {`{"field":"n","op":"eq"}`, "needs its value"},
		"a text for a number":           {`{"field":"n","op":"eq","value":"1"}`, "must be a number"},
		"a number for a text":           {`{"field":"t","op":"eq","value":1}`, "must be text"},
		"a text for a boolean":          {`{"field":"ok","op":"eq","value":"true"}`, "true or false"},
		"an option the choice lacks":    {`{"field":"kind","op":"eq","value":"c"}`, "must be one of the options"},
		"ordering a choice":             {`{"field":"kind","op":"lt","value":"a"}`, "eq or ne only"},
		"ordering text":                 {`{"field":"t","op":"gt","value":"a"}`, "eq or ne only"},
		"ordering a boolean":            {`{"field":"ok","op":"gt","value":true}`, "eq or ne only"},
		"a time that isn't one":         {`{"field":"at","op":"eq","value":"9:30"}`, "HH:MM"},
		"a date that isn't one":         {`{"field":"day","op":"eq","value":"1 May"}`, "YYYY-MM-DD"},
		"an unknown key":                {`{"field":"n","op":"eq","value":1,"eval":"1+1"}`, "unknown field"},
		"a comparison and all":          {`{"field":"n","op":"eq","value":1,"all":[]}`, "one of them"},
		"all and any":                   {`{"all":[` + deep + `],"any":[` + deep + `]}`, "one of them"},
		"an empty rule":                 {`{}`, "one of them"},
		"an empty all":                  {`{"all":[]}`, "holds 1 to"},
		"too many rules in one all":     {`{"all":[` + many + `]}`, "holds 1 to"},
		"nested past the depth":         {`{"all":[` + deep + `]}`, "at most 3 deep"},
		"more parts than the bound":     {`{"all":[` + strings.Join(wide, ",") + `]}`, "at most 16 parts"},
		"a value that isn't a literal":  {`{"field":"n","op":"eq","value":{"path":"/draft/n"}}`, "must be a number"},
		"a rule that isn't an object":   {`"n eq 1"`, ""},
		"a long text":                   {`{"field":"t","op":"eq","value":"` + strings.Repeat("x", DefaultMaxLength+1) + `"}`, "at most"},
		"an expression in the operator": {`{"field":"n","op":"eq || true","value":1}`, "op must be one of"},
	}
	for name, tt := range bad {
		t.Run(name, func(t *testing.T) {
			var r Rule
			err := json.Unmarshal([]byte(tt.rule), &r)
			if err == nil {
				err = r.check(c)
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
	good := []string{
		`{"field":"kind","op":"eq","value":"a"}`,
		`{"field":"n","op":"gte","value":-2.5}`,
		`{"field":"at","op":"lt","value":"09:30"}`,
		`{"field":"day","op":"gt","value":"2026-01-31"}`,
		`{"field":"ok","op":"ne","value":false}`,
		`{"all":[{"any":[{"field":"kind","op":"eq","value":"a"},{"field":"kind","op":"eq","value":"b"}]},{"field":"ok","op":"eq","value":true}]}`,
		deep,
	}
	for _, rule := range good {
		var r Rule
		if err := json.Unmarshal([]byte(rule), &r); err != nil {
			t.Fatal(err)
		}
		if err := r.check(c); err != nil {
			t.Errorf("%s: %v", rule, err)
		}
	}
}

func TestConditionalRequirementsAreEnforcedOnEveryRow(t *testing.T) {
	def := planner(t)
	c := def.Collection
	other := map[string]any{"title": "x", "category": "other"}
	if err := c.Fits(other); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "category_note") {
		t.Fatalf("a row where the rule holds and the field is missing: %v", err)
	}
	if err := c.Fits(map[string]any{"title": "x", "category": "other", "category_note": "errand"}); err != nil {
		t.Fatalf("with the field: %v", err)
	}
	if err := c.Fits(map[string]any{"title": "x", "category": "home"}); err != nil {
		t.Fatalf("where the rule doesn't hold: %v", err)
	}
	// An update that makes the rule hold, or clears the field it asks for, is refused too.
	if _, err := c.Merge(map[string]any{"title": "x", "category": "home"}, map[string]any{"category": "other"}); err == nil {
		t.Fatal("an update making the field required without it was taken")
	}
	if _, err := c.Merge(map[string]any{"title": "x", "category": "other", "category_note": "n"}, map[string]any{"category_note": nil}); err == nil {
		t.Fatal("an update clearing a conditionally required field was taken")
	}
	// A conditionally required field can't also be always required, or read itself.
	for name, change := range map[string]func(*Field){
		"always required too": func(f *Field) { f.Required = true },
		"reading itself":      func(f *Field) { f.RequiredWhen = &Rule{Field: f.Name, Op: OpEq, Value: "x"} },
		"a rule that is bad":  func(f *Field) { f.RequiredWhen = &Rule{Field: "category", Op: OpEq, Value: "nowhere"} },
	} {
		t.Run(name, func(t *testing.T) {
			next := fieldsOf(def, "category_note", change)
			if _, err := next.Check(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}
