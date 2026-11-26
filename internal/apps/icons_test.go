package apps

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/apierr"
)

// A plus sign on the 24 by 24 view box, and a check mark.
const (
	plusPath  = "M19 13h-6v6h-2v-6H5v-2h6V5h2v6h6v2z"
	checkPath = "M9 16.17 4.83 12l-1.42 1.41L9 19 21 7l-1.41-1.41z"
)

// iconScreen is a one-screen app around components, with an add action for its buttons.
func iconScreen(components ...string) Definition {
	raw := []json.RawMessage{json.RawMessage(`{"id":"root","component":"Column","children":["icon"]}`)}
	for _, c := range components {
		raw = append(raw, json.RawMessage(c))
	}
	return Definition{Components: raw, Collection: Collection{Fields: []Field{{Name: "title", Type: TypeString}}},
		Actions: map[string]Action{"add": {Kind: ActionInsert}}}
}

func pathIcon(path string) string {
	b, _ := json.Marshal(map[string]any{"id": "icon", "component": "Icon", "name": map[string]any{"svgPath": path}})
	return string(b)
}

func codeOf(err error) (apierr.Code, apierr.Params) {
	var coded *invalidError
	if errors.As(err, &coded) {
		return coded.code, coded.params
	}
	return "", nil
}

func TestIconPathAcceptsPathData(t *testing.T) {
	for _, path := range []string{plusPath, checkPath, "m0 0h24v24H0z", "M1.5e1-2E-1,3 4Z", "M" + strings.Repeat("0", MaxIconPathLen-1)} {
		got, err := iconScreen(pathIcon(path)).Check()
		if err != nil {
			t.Fatalf("%.40q: %v", path, err)
		}
		if !slices.Contains(got.Requires, IconPathCapability) {
			t.Errorf("%.40q: requires = %v, want %s", path, got.Requires, IconPathCapability)
		}
	}
	if !slices.Contains(Capabilities(), IconPathCapability) {
		t.Errorf("the catalog doesn't list %s", IconPathCapability)
	}
}

// Everything that isn't path data is refused, with the code the web app translates and the
// component it is in.
func TestIconPathRefusesAnythingElse(t *testing.T) {
	bad := map[string]string{
		"empty":                  "",
		"a script element":       `M0 0<script>alert(1)</script>`,
		"closing the attribute":  `M0 0"><img src=x onerror=alert(1)>`,
		"a javascript: URL":      "javascript:alert(1)",
		"a data: URL":            "data:image/svg+xml;base64,PHN2Zz4=",
		"an http: URL":           "http://elsewhere.example/icon.svg",
		"after a moveto, a URL":  "M0 0 url(http://elsewhere.example/x)",
		"a double quote":         `M0 0"`,
		"a single quote":         "M0 0'",
		"an angle bracket":       "M0 0>",
		"a round bracket":        "M0 0(",
		"a square bracket":       "M0 0[",
		"a semicolon":            "M0 0;",
		"an unknown command":     "M0 0 X1 1",
		"a lowercase other":      "M0 0 b1 1",
		"not starting a moveto":  "L0 0",
		"a leading space":        " M0 0",
		"a newline":              "M0 0\nL1 1",
		"a tab":                  "M0 0\tL1 1",
		"a non-ASCII character":  "M0 0 L1 1 ",
		"over the length limit":  "M" + strings.Repeat("0", MaxIconPathLen),
		"a hash, as in a colour": "M0 0 #fff",
	}
	for name, path := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := iconScreen(pathIcon(path)).Check()
			code, params := codeOf(err)
			if !errors.Is(err, ErrInvalid) || code != apierr.CodeAppIconPath {
				t.Fatalf("err = %v (code %q), want %s", err, code, apierr.CodeAppIconPath)
			}
			if params["component"] != "icon" || params["max"] != MaxIconPathLen {
				t.Errorf("params = %v", params)
			}
		})
	}
	// Not a string, or with another key beside it.
	for name, icon := range map[string]string{
		"a number":        `{"id":"icon","component":"Icon","name":{"svgPath":5}}`,
		"an object":       `{"id":"icon","component":"Icon","name":{"svgPath":{"path":"/x"}}}`,
		"another key":     `{"id":"icon","component":"Icon","name":{"svgPath":"M0 0","href":"http://elsewhere.example"}}`,
		"a style beside":  `{"id":"icon","component":"Icon","name":{"svgPath":"M0 0"},"style":"color:red"}`,
		"an unknown name": `{"id":"icon","component":"Icon","name":"receipt"}`,
	} {
		if _, err := iconScreen(icon).Check(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

// An icon given by name stays valid, so apps saved with one still save and publish, and needs no
// new capability. One bound to data stays valid too, but needs the capability: the bound value
// could be a path, which only a client that checks paths may draw.
func TestIconByNameStaysValid(t *testing.T) {
	bound, err := iconScreen(`{"id":"icon","component":"Icon","name":{"path":"/icon"}}`).Check()
	if err != nil || !slices.Contains(bound.Requires, IconPathCapability) {
		t.Fatalf("bound: requires = %v, %v; want %s", bound.Requires, err, IconPathCapability)
	}
	for _, icon := range []string{
		`{"id":"icon","component":"Icon","name":"star"}`,
		`{"id":"icon","component":"Icon","name":"add"}`,
	} {
		got, err := iconScreen(icon).Check()
		if err != nil {
			t.Fatalf("%s: %v", icon, err)
		}
		if slices.Contains(got.Requires, IconPathCapability) {
			t.Errorf("%s: requires %s", icon, IconPathCapability)
		}
	}
	// In a button with nothing else, too: a name draws nothing, so the label rule is for paths.
	if _, err := iconScreen(`{"id":"icon","component":"Button","child":"star","action":{"event":{"name":"add"}}}`,
		`{"id":"star","component":"Icon","name":"star"}`).Check(); err != nil {
		t.Fatal(err)
	}
}

// A path icon that is all of a button, or a modal's trigger, needs a name of its own; beside a
// label it doesn't.
func TestIconAloneNeedsAName(t *testing.T) {
	button := `{"id":"icon","component":"Button","child":"plus","action":{"event":{"name":"add"}}}`
	labelled := `{"id":"plus","component":"Icon","name":{"svgPath":"` + plusPath + `"},"accessibility":{"label":"Add"}}`
	unlabelled := `{"id":"plus","component":"Icon","name":{"svgPath":"` + plusPath + `"}}`
	blank := `{"id":"plus","component":"Icon","name":{"svgPath":"` + plusPath + `"},"accessibility":{"label":"  "}}`
	bound := `{"id":"plus","component":"Icon","name":{"svgPath":"` + plusPath + `"},"accessibility":{"label":{"path":"/label"}}}`
	modal := `{"id":"icon","component":"Modal","trigger":"plus","content":"body"}`
	body := `{"id":"body","component":"Text","text":"Hello"}`
	labelledButton := `{"id":"icon","component":"Button","child":"plus","action":{"event":{"name":"add"}},"accessibility":{"label":"Add"}}`

	for name, comps := range map[string][]string{
		"a button's icon with no label":         {button, unlabelled},
		"a button's icon with a blank label":    {button, blank},
		"a modal's trigger with no label":       {modal, unlabelled, body},
		"a labelled button, an unlabelled icon": {labelledButton, unlabelled},
		"icons only, through a row": {`{"id":"icon","component":"Button","child":"row","action":{"event":{"name":"add"}}}`,
			`{"id":"row","component":"Row","children":["plus","named"]}`, unlabelled, `{"id":"named","component":"Icon","name":"star"}`},
		"icons only, through a card and a column": {`{"id":"icon","component":"Modal","trigger":"card","content":"body"}`, body,
			`{"id":"card","component":"Card","child":"col"}`, `{"id":"col","component":"Column","children":["plus"]}`, unlabelled},
	} {
		_, err := iconScreen(comps...).Check()
		code, params := codeOf(err)
		if code != apierr.CodeAppIconLabel || params["component"] != "plus" {
			t.Errorf("%s: err = %v (code %q, params %v), want %s for plus", name, err, code, params, apierr.CodeAppIconLabel)
		}
	}
	for name, comps := range map[string][]string{
		"a button's labelled icon": {button, labelled},
		"a bound label":            {button, bound},
		"a modal's labelled icon":  {modal, labelled, body},
		"beside a label": {`{"id":"icon","component":"Button","child":"row","action":{"event":{"name":"add"}}}`,
			`{"id":"row","component":"Row","children":["plus","text"]}`, unlabelled, `{"id":"text","component":"Text","text":"Add"}`},
		"standing in a column": {`{"id":"icon","component":"Column","children":["plus"]}`, unlabelled},
		"beside text, deeper in": {`{"id":"icon","component":"Button","child":"card","action":{"event":{"name":"add"}}}`,
			`{"id":"card","component":"Card","child":"row"}`, `{"id":"row","component":"Row","children":["plus","col"]}`, unlabelled,
			`{"id":"col","component":"Column","children":["text"]}`, `{"id":"text","component":"Text","text":"Add"}`},
		"a labelled icon through a row": {`{"id":"icon","component":"Button","child":"row","action":{"event":{"name":"add"}}}`,
			`{"id":"row","component":"Row","children":["plus"]}`, labelled},
	} {
		if _, err := iconScreen(comps...).Check(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The definition's own size limit is what caps how many paths an app holds.
func TestIconPathsCountTowardTheDefinitionSize(t *testing.T) {
	long := "M" + strings.Repeat("1 ", (MaxIconPathLen-1)/2)
	children := []string{}
	comps := []string{}
	for i := range MaxDefinitionSize/MaxIconPathLen + 1 {
		id := "i" + strings.Repeat("x", i%5) + string(rune('a'+i%26)) + string(rune('a'+i/26))
		children = append(children, `"`+id+`"`)
		b, _ := json.Marshal(map[string]any{"id": id, "component": "Icon", "name": map[string]any{"svgPath": long}})
		comps = append(comps, string(b))
	}
	comps = append(comps, `{"id":"icon","component":"Column","children":[`+strings.Join(children, ",")+`]}`)
	_, err := iconScreen(comps...).Check()
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "KiB") {
		t.Fatalf("err = %v, want the definition's size limit", err)
	}
}

// The three bundled starter templates draw icons, as paths.
func TestTemplatesHaveIcons(t *testing.T) {
	all, err := Templates()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("%d templates", len(all))
	}
	for _, tpl := range all {
		if !slices.Contains(tpl.Definition.Requires, IconPathCapability) {
			t.Errorf("%s: requires = %v, want %s", tpl.ID, tpl.Definition.Requires, IconPathCapability)
		}
	}
}

// References that fan out (each of four rows naming the next one 200 times) are looked at once
// each: the check stays fast and still finds the unlabelled icon at the bottom.
func TestIconLabelsDoNotMultiplyFanOut(t *testing.T) {
	fan := func(next string) string {
		refs := make([]string, maxListLen)
		for i := range refs {
			refs[i] = `"` + next + `"`
		}
		return "[" + strings.Join(refs, ",") + "]"
	}
	comps := []string{`{"id":"icon","component":"Button","child":"r1","action":{"event":{"name":"add"}}}`,
		`{"id":"r1","component":"Row","children":` + fan("r2") + `}`,
		`{"id":"r2","component":"Row","children":` + fan("r3") + `}`,
		`{"id":"r3","component":"Row","children":` + fan("r4") + `}`,
		`{"id":"r4","component":"Row","children":["plus"]}`,
		`{"id":"plus","component":"Icon","name":{"svgPath":"` + plusPath + `"}}`}
	start := time.Now()
	_, err := iconScreen(comps...).Check()
	if code, params := codeOf(err); code != apierr.CodeAppIconLabel || params["component"] != "plus" {
		t.Fatalf("err = %v, want %s for plus", err, apierr.CodeAppIconLabel)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the check took %v", took)
	}
}
