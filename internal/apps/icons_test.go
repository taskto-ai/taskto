package apps

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/apierr"
)

const (
	plusPath  = "M19 13h-6v6h-2v-6H5v-2h6V5h2v6h6v2z"
	checkPath = "M3 13l3-3 4 4 8-8 3 3-11 11z"
)

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
	for name, tc := range map[string]struct{ icon, says string }{
		"a number":    {`{"id":"icon","component":"Icon","name":{"svgPath":5}}`, "must be text, not a number"},
		"null":        {`{"id":"icon","component":"Icon","name":{"svgPath":null}}`, "must be text, not null"},
		"a list":      {`{"id":"icon","component":"Icon","name":{"svgPath":["M0 0"]}}`, "must be text, not a list"},
		"an object":   {`{"id":"icon","component":"Icon","name":{"svgPath":{"path":"/x"}}}`, "must be text, not an object"},
		"another key": {`{"id":"icon","component":"Icon","name":{"svgPath":"M0 0","href":"http://elsewhere.example"}}`, "no other key"},
	} {
		_, err := iconScreen(tc.icon).Check()
		if code, _ := codeOf(err); code != apierr.CodeAppIconPath || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: err = %v (code %q), want %s saying %q", name, err, code, apierr.CodeAppIconPath, tc.says)
		}
	}
	if _, err := iconScreen(pathIcon("M0 0 L1 1\u00a0")).Check(); err == nil || !strings.Contains(err.Error(), `'\u00a0'`) {
		t.Errorf("non-ASCII: err = %v", err)
	}
	for name, icon := range map[string]string{
		"a style beside":  `{"id":"icon","component":"Icon","name":{"svgPath":"M0 0"},"style":"color:red"}`,
		"an unknown name": `{"id":"icon","component":"Icon","name":"receipt"}`,
	} {
		if _, err := iconScreen(icon).Check(); !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), ".go") {
			t.Errorf("%s: err = %v, want ErrInvalid naming no file", name, err)
		}
	}
}

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
	if _, err := iconScreen(`{"id":"icon","component":"Button","child":"star","action":{"event":{"name":"add"}}}`,
		`{"id":"star","component":"Icon","name":"star"}`).Check(); err != nil {
		t.Fatal(err)
	}
}

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
			`{"id":"row","component":"Row","children":["plus","bound"]}`, unlabelled, `{"id":"bound","component":"Icon","name":{"path":"/icon"}}`},
		"beside a blank text": {`{"id":"icon","component":"Button","child":"row","action":{"event":{"name":"add"}}}`,
			`{"id":"row","component":"Row","children":["plus","text"]}`, unlabelled, `{"id":"text","component":"Text","text":""}`},
		"beside text of spaces": {`{"id":"icon","component":"Button","child":"row","action":{"event":{"name":"add"}}}`,
			`{"id":"row","component":"Row","children":["plus","text"]}`, unlabelled, `{"id":"text","component":"Text","text":"   "}`},
		"beside a divider": {`{"id":"icon","component":"Button","child":"row","action":{"event":{"name":"add"}}}`,
			`{"id":"row","component":"Row","children":["plus","line"]}`, unlabelled, `{"id":"line","component":"Divider"}`},
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
		"a name, all the button shows": {`{"id":"icon","component":"Button","child":"named","action":{"event":{"name":"add"}}}`,
			`{"id":"named","component":"Icon","name":"delete"}`},
		"a name beside an unlabelled path": {`{"id":"icon","component":"Button","child":"row","action":{"event":{"name":"add"}}}`,
			`{"id":"row","component":"Row","children":["plus","named"]}`, unlabelled, `{"id":"named","component":"Icon","name":"add"}`},
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
	_, err := iconScreen(comps...).Check()
	if code, params := codeOf(err); code != apierr.CodeAppIconLabel || params["component"] != "plus" {
		t.Fatalf("err = %v, want %s for plus", err, apierr.CodeAppIconLabel)
	}
	byID := map[string]map[string]any{}
	for _, c := range comps {
		obj, err := decodeObject(json.RawMessage(c))
		if err != nil {
			t.Fatal(err)
		}
		byID[obj["id"].(string)] = obj
	}
	w := iconWalk{byID: byID, pathIcons: map[string]bool{"plus": true}, memo: map[contentAt]contentName{}}
	if got := w.names("r1", 0); got.named || got.icon != "plus" {
		t.Fatalf("names = %+v", got)
	}
	if limit := 4 * maxListLen; w.visits > limit {
		t.Errorf("the walk took %d steps, want at most %d", w.visits, limit)
	}
}

func TestACopyOfAnOldReleaseIsNotHeldToTheLabelRule(t *testing.T) {
	w := newWorld(t)
	s := w.crew()
	a := w.publish(w.alice, s, w.mustInstall(w.alice, s, "expense-tracker", nil))
	var def map[string]any
	var raw string
	if err := w.e.db.QueryRow(`SELECT definition FROM app_releases WHERE app_id = ? AND number = 1`, a.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(raw), &def); err != nil {
		t.Fatal(err)
	}
	for _, c := range def["screens"].([]any)[0].(map[string]any)["components"].([]any) {
		if c := c.(map[string]any); c["id"] == "list-new" {
			c["child"] = "list-new-icon"
		}
	}
	old, _ := json.Marshal(def)
	if _, err := w.e.db.Exec(`INSERT INTO app_releases (app_id, number, definition, definition_version, requires)
		SELECT app_id, 2, ?, definition_version, requires FROM app_releases WHERE app_id = ? AND number = 1`, string(old), a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.db.Exec(`UPDATE apps SET live_release = 2 WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := definitionOf(t, old).Check(); codeOfErr(err) != apierr.CodeAppIconLabel {
		t.Fatalf("saved again, the release would be refused with %s: %v", apierr.CodeAppIconLabel, err)
	}

	rec := w.bob.do(http.MethodPost, appPath(s, a.ID, "/duplicate"), map[string]any{"space_id": w.bobPrivate.ID})
	expect(t, rec, http.StatusCreated)
	copyApp := w.view(w.bob, w.bobPrivate, decode[App](t, rec))
	rec = w.bob.do(http.MethodPost, appPath(w.bobPrivate, copyApp.ID, "/publish"), map[string]any{"version": copyApp.Version, "acknowledge": []SchemaChange{}})
	expect(t, rec, http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), string(apierr.CodeAppIconLabel)) {
		t.Fatalf("publishing the copy: %s", rec.Body)
	}
}

func definitionOf(t *testing.T, raw []byte) Definition {
	t.Helper()
	var d Definition
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func codeOfErr(err error) apierr.Code {
	code, _ := codeOf(err)
	return code
}
