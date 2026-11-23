package apps

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
)

func stamp(d time.Duration) string { return time.Now().Add(d).UTC().Format(time.RFC3339) }

func where(extra map[string]any) map[string]any {
	v := map[string]any{"lat": 38.7223, "lon": -9.1393, "accuracy_m": 12.5, "captured_at": stamp(-time.Minute), "source": "device"}
	for k, x := range extra {
		if x == nil {
			delete(v, k)
		} else {
			v[k] = x
		}
	}
	return v
}

func TestLocationValuesAreChecked(t *testing.T) {
	f := Field{Name: "place", Type: TypeLocation}
	good := map[string]any{
		"device":              where(nil),
		"manual, no accuracy": where(map[string]any{"source": "manual", "accuracy_m": nil}),
		"edges":               where(map[string]any{"lat": -90.0, "lon": 180.0, "accuracy_m": 0.0}),
		"millis":              where(map[string]any{"captured_at": time.Now().UTC().Add(-time.Hour).Format("2006-01-02T15:04:05.000Z")}),
		"offset zone":         where(map[string]any{"captured_at": time.Now().Add(-time.Hour).In(time.FixedZone("x", 3600)).Format(time.RFC3339)}),
		"a little ahead":      where(map[string]any{"captured_at": stamp(2 * time.Minute)}),
		"json numbers":        where(map[string]any{"lat": json.Number("38.5"), "lon": json.Number("-9")}),
	}
	for name, v := range good {
		got, present, err := f.parse(v)
		if err != nil || !present {
			t.Errorf("%s: %v %v", name, got, err)
			continue
		}
		m := got.(map[string]any)
		if !strings.HasSuffix(m["captured_at"].(string), "Z") || len(m["captured_at"].(string)) != len("2006-01-02T15:04:05.000Z") {
			t.Errorf("%s: captured_at kept as %v", name, m["captured_at"])
		}
	}
	bad := map[string]any{
		"lat over":       where(map[string]any{"lat": 90.1}),
		"lat under":      where(map[string]any{"lat": -91.0}),
		"lon over":       where(map[string]any{"lon": 180.5}),
		"lon missing":    where(map[string]any{"lon": nil}),
		"lat as text":    where(map[string]any{"lat": "38.7"}),
		"nan":            where(map[string]any{"lat": json.Number("NaN")}),
		"negative acc":   where(map[string]any{"accuracy_m": -1.0}),
		"huge acc":       where(map[string]any{"accuracy_m": 2e6}),
		"acc as text":    where(map[string]any{"accuracy_m": "10"}),
		"device no acc":  where(map[string]any{"accuracy_m": nil}),
		"no source":      where(map[string]any{"source": nil}),
		"other source":   where(map[string]any{"source": "gps"}),
		"no time":        where(map[string]any{"captured_at": nil}),
		"time as number": where(map[string]any{"captured_at": 1700000000.0}),
		"not a time":     where(map[string]any{"captured_at": "yesterday"}),
		"date only":      where(map[string]any{"captured_at": "2026-11-20"}),
		"before 2000":    where(map[string]any{"captured_at": "1999-12-31T23:59:59Z"}),
		"unknown key":    where(map[string]any{"altitude": 5.0}),
		"text":           "38.7,-9.1",
		"list":           []any{38.7, -9.1},
		"number":         12.0,
		"in the future":  where(map[string]any{"captured_at": stamp(time.Hour)}),
		"just too far":   where(map[string]any{"captured_at": stamp(MaxClockSkew + time.Minute)}),
	}
	for name, v := range bad {
		if got, _, err := f.parse(v); err == nil {
			t.Errorf("%s: accepted as %v", name, got)
		} else if !strings.Contains(err.Error(), "place") {
			t.Errorf("%s: error %q doesn't name the field", name, err)
		}
	}
	// No value at all is no value.
	for _, empty := range []any{nil, "", "  ", map[string]any{}} {
		if _, present, err := f.parse(empty); err != nil || present {
			t.Errorf("%#v: present %v, %v", empty, present, err)
		}
	}
	// Read as a row holds it, a time ahead of the clock is no reason to hide what was stored.
	if _, present, err := f.stored(where(map[string]any{"captured_at": stamp(24 * time.Hour)})); err != nil || !present {
		t.Errorf("stored in the future: %v %v", present, err)
	}
	if err := f.fits(map[string]any{"place": where(map[string]any{"captured_at": stamp(24 * time.Hour)})}); err != nil {
		t.Errorf("a stored row checked against the clock: %v", err)
	}
}

func TestBarcodeValuesAreChecked(t *testing.T) {
	any13 := Field{Name: "code", Type: TypeBarcode, MaxLength: 40}
	for _, s := range []string{"HELLO-123", "https://example.com/a?b=1", " padded "} {
		if got, present, err := any13.parse(s); err != nil || !present || got != strings.TrimSpace(s) {
			t.Errorf("%q: %v %v %v", s, got, present, err)
		}
	}
	for _, s := range []string{"has\nnewline", "tab\there", "nul\x00", strings.Repeat("x", 41)} {
		if _, _, err := any13.parse(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	for _, v := range []any{12.0, true, []any{"a"}, map[string]any{}} {
		if _, _, err := any13.parse(v); err == nil {
			t.Errorf("%#v accepted", v)
		}
	}
	if _, present, err := any13.parse(""); err != nil || present {
		t.Errorf("blank: %v %v", present, err)
	}
	ean := Field{Name: "code", Type: TypeBarcode, Format: "ean_13", MaxLength: 20}
	for _, s := range []string{"4006381333931", "9780306406157"} {
		if _, _, err := ean.parse(s); err != nil {
			t.Errorf("ean_13 %s: %v", s, err)
		}
	}
	for _, s := range []string{"4006381333932", "400638133393", "40063813339311", "40063813339x1", "HELLO"} {
		if _, _, err := ean.parse(s); err == nil {
			t.Errorf("ean_13 %s accepted", s)
		}
	}
	upc := Field{Name: "code", Type: TypeBarcode, Format: "upc_a", MaxLength: 20}
	if _, _, err := upc.parse("036000291452"); err != nil {
		t.Errorf("upc_a: %v", err)
	}
	if _, _, err := upc.parse("036000291453"); err == nil {
		t.Error("upc_a with a wrong check digit accepted")
	}
	short := Field{Name: "code", Type: TypeBarcode, Format: "ean_8", MaxLength: 20}
	if _, _, err := short.parse("96385074"); err != nil {
		t.Errorf("ean_8: %v", err)
	}
	// A QR code's text is anything printable.
	qr := Field{Name: "code", Type: TypeBarcode, Format: "qr_code", MaxLength: 100}
	if _, _, err := qr.parse("BEGIN:VCARD"); err != nil {
		t.Errorf("qr_code: %v", err)
	}
	// A value a row holds from before the field named a format is still read, as text.
	if _, present, _ := ean.stored("anything goes here"); !present {
		t.Error("a stored code under a stricter format was dropped")
	}
}

func TestSignatureValuesAreChecked(t *testing.T) {
	f := Field{Name: "signed", Type: TypeSignature}
	id := strings.Repeat("ab", 16)
	for _, v := range []any{id, []any{id}} {
		if got, present, err := f.parse(v); err != nil || !present || got != id {
			t.Errorf("%#v: %v %v %v", v, got, present, err)
		}
	}
	for _, v := range []any{"short", strings.Repeat("AB", 16), []any{id, id}, []any{1.0}, 12.0, map[string]any{"id": id}, true} {
		if _, _, err := f.parse(v); err == nil {
			t.Errorf("%#v accepted", v)
		}
	}
	if _, present, err := f.parse([]any{}); err != nil || present {
		t.Errorf("empty list: %v %v", present, err)
	}
}

func captureDefinition(t *testing.T, fields string) Definition {
	t.Helper()
	var d Definition
	text := `{"components": [{"id": "root", "component": "Column", "children": ["go"]},
	  {"id": "go_label", "component": "Text", "text": "Go"},
	  {"id": "go", "component": "Button", "child": "go_label", "action": {"event": {"name": "add_note", "context": {}}}}],
	  "collection": {"fields": ` + fields + `}, "actions": {"add_note": {"kind": "insert"}}}`
	if err := json.Unmarshal([]byte(text), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestCaptureFieldDefinitions(t *testing.T) {
	ok := captureDefinition(t, `[{"name": "p", "type": "location"}, {"name": "s", "type": "signature"}, {"name": "c", "type": "barcode", "format": "ean_13"}]`)
	checked, err := ok.Check()
	if err != nil {
		t.Fatal(err)
	}
	for _, need := range []string{LocationCapability, SignatureCapability, BarcodeCapability} {
		if !contains(checked.Requires, need) {
			t.Errorf("requires %v lacks %s", checked.Requires, need)
		}
	}
	if checked.Collection.Fields[0].Capture != CaptureButton || checked.Collection.Fields[1].MaxBytes != DefaultSignatureBytes || checked.Collection.Fields[2].MaxLength != DefaultMaxLength {
		t.Errorf("defaults not filled in: %+v", checked.Collection.Fields)
	}
	for _, c := range Capabilities() {
		if c == "field.location" {
			goto listed
		}
	}
	t.Error("Capabilities lacks field.location")
listed:
	bad := map[string]string{
		`[{"name": "p", "type": "location", "capture": "always"}]`:                                       "capture",
		`[{"name": "p", "type": "string", "capture": "submit"}]`:                                         "only a location field has capture",
		`[{"name": "c", "type": "barcode", "format": "ean_14"}]`:                                         "format is one of",
		`[{"name": "c", "type": "string", "format": "qr_code"}]`:                                         "only a barcode field has format",
		`[{"name": "c", "type": "barcode", "max_length": 20000}]`:                                        "max_length",
		`[{"name": "n", "type": "number", "editable": true}]`:                                            "editable",
		`[{"name": "p", "type": "location", "max_length": 5}]`:                                           "max_length",
		`[{"name": "p", "type": "location", "max_bytes": 5}]`:                                            "max_bytes",
		`[{"name": "s", "type": "signature", "max_files": 2}]`:                                           "max_files",
		`[{"name": "s", "type": "signature", "max_bytes": 99999999}]`:                                    "max_bytes",
		`[{"name": "p", "type": "location", "required_when": {"field": "p", "op": "eq", "value": "x"}}]`: "",
	}
	for fields, want := range bad {
		if _, err := captureDefinition(t, fields).Check(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", fields, err, want)
		}
	}
	// Version 1 knows none of them.
	for _, f := range []string{`{"name": "p", "type": "location"}`, `{"name": "s", "type": "signature"}`, `{"name": "c", "type": "barcode"}`} {
		d := captureDefinition(t, "["+f+"]")
		d.DefinitionVersion = 1
		if _, err := d.Check(); err == nil || !strings.Contains(err.Error(), "version 2") {
			t.Errorf("%s in version 1: %v", f, err)
		}
	}
	// A rule can compare a barcode, and can't read a location or a signature.
	c := Collection{Fields: []Field{{Name: "c", Type: TypeBarcode}, {Name: "p", Type: TypeLocation}, {Name: "s", Type: TypeSignature}}}
	if err := (Rule{Field: "c", Op: OpEq, Value: "abc"}).check(c); err != nil {
		t.Errorf("rule on a barcode: %v", err)
	}
	for _, name := range []string{"p", "s"} {
		if err := (Rule{Field: name, Op: OpEq, Value: "abc"}).check(c); err == nil {
			t.Errorf("a rule read %s", name)
		}
	}
	// The input components are for their own kind of field only.
	for component, wrong := range map[string]string{"LocationInput": "s", "SignatureInput": "c", "BarcodeInput": "p"} {
		d := captureDefinition(t, `[{"name": "p", "type": "location"}, {"name": "s", "type": "signature"}, {"name": "c", "type": "barcode"}]`)
		d.Components[0] = json.RawMessage(`{"id": "root", "component": "Column", "children": ["in"]}`)
		d.Components = append(d.Components, json.RawMessage(`{"id": "in", "component": "`+component+`", "field": "`+wrong+`", "value": {"path": "/draft/`+wrong+`"}}`))
		if _, err := d.Check(); err == nil || !strings.Contains(err.Error(), "field") {
			t.Errorf("%s for %s: %v", component, wrong, err)
		}
	}
}

func TestSubmitCapturedFieldsAreNamedInTheContext(t *testing.T) {
	screen := func(key string) Definition {
		d := captureDefinition(t, `[{"name": "p", "type": "location", "capture": "submit"}]`)
		d.Components = []json.RawMessage{
			json.RawMessage(`{"id": "root", "component": "Column", "children": ["in", "go"]}`),
			json.RawMessage(`{"id": "in", "component": "LocationInput", "field": "p", "value": {"path": "/draft/p"}}`),
			json.RawMessage(`{"id": "go_label", "component": "Text", "text": "Go"}`),
			json.RawMessage(`{"id": "go", "component": "Button", "child": "go_label", "action": {"event": {"name": "add_note", "context": {"` + key + `": {"path": "/draft/p"}}}}}`),
		}
		return d
	}
	if _, err := screen("p").Check(); err != nil {
		t.Fatal(err)
	}
	if _, err := screen("where").Check(); err == nil || !strings.Contains(err.Error(), `named "p"`) {
		t.Fatalf("renamed entry: %v", err)
	}
}

func TestEquipmentCheckoutExampleIsDrawnAndSaved(t *testing.T) {
	all, err := Examples()
	if err != nil {
		t.Fatal(err)
	}
	for _, ex := range all {
		if ex.Name != "Equipment checkout" {
			continue
		}
		for _, need := range []string{LocationCapability, SignatureCapability, BarcodeCapability,
			ComponentCapability("LocationInput"), ComponentCapability("SignatureInput"), ComponentCapability("BarcodeInput")} {
			if !contains(ex.Definition.Requires, need) {
				t.Errorf("requires %v lacks %s", ex.Definition.Requires, need)
			}
		}
		surface, _ := json.Marshal(Surface("app-1", ex.Definition, nil))
		for _, want := range []string{`"_fields":{"equipment":{"editable":false,"format":"","required":true},"place":{"capture":"submit"`, `"_files":{"signed":{"max_bytes":1048576,"max_files":1}}`} {
			if !strings.Contains(string(surface), want) {
				t.Errorf("surface lacks %s: %s", want, surface)
			}
		}
		return
	}
	t.Fatal("no equipment example")
}

// An app with one of each capture field, and an action that adds a record and one that changes it
// (which is what the records explorer sends too).
const captureApp = `{
  "components": [
    {"id": "root", "component": "Column", "children": ["code", "place", "sign", "other", "add"]},
    {"id": "code", "component": "BarcodeInput", "field": "code", "value": {"path": "/draft/code"}},
    {"id": "place", "component": "LocationInput", "field": "place", "value": {"path": "/draft/place"}},
    {"id": "sign", "component": "SignatureInput", "field": "signed", "value": {"path": "/draft/signed"}},
    {"id": "other", "component": "LocationInput", "field": "free", "value": {"path": "/draft/free"}},
    {"id": "add_label", "component": "Text", "text": "Add"},
    {"id": "add", "component": "Button", "child": "add_label", "action": {"event": {"name": "add_note", "context": {
      "title": {"path": "/draft/title"}, "code": {"path": "/draft/code"}, "place": {"path": "/draft/place"},
      "signed": {"path": "/draft/signed"}, "free": {"path": "/draft/free"}}}}}
  ],
  "collection": {"fields": [
    {"name": "title", "type": "string"},
    {"name": "code", "type": "barcode", "format": "ean_13"},
    {"name": "place", "type": "location"},
    {"name": "signed", "type": "signature"},
    {"name": "free", "type": "location", "editable": true}
  ]},
  "actions": {"add_note": {"kind": "insert"}, "edit_note": {"kind": "update"}}
}`

func (w *world) createCapture(u *user, s access.Space) App {
	w.e.t.Helper()
	var def map[string]any
	if err := jsonUnmarshal(captureApp, &def); err != nil {
		w.e.t.Fatal(err)
	}
	rec := u.do(http.MethodPost, appsPath(s, ""), map[string]any{"name": "Capture", "definition": def})
	expect(w.e.t, rec, http.StatusCreated)
	return w.publish(u, s, decode[App](w.e.t, rec))
}

func (w *world) edit(u *user, s access.Space, a App, id int64, fields map[string]any) *httptest.ResponseRecorder {
	ctx := map[string]any{"id": id}
	for k, v := range fields {
		ctx[k] = v
	}
	return u.do(http.MethodPost, appPath(s, a.ID, "/actions"), ClientAction{
		Name: "edit_note", SurfaceID: SurfaceID(a.ID), SourceComponentID: "explorer", Context: ctx})
}

func (w *world) rowData(a App, id int64) map[string]any {
	w.e.t.Helper()
	var raw string
	if err := w.e.db.QueryRow(`SELECT data FROM app_rows WHERE id = ? AND app_id = ?`, id, a.ID).Scan(&raw); err != nil {
		w.e.t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		w.e.t.Fatal(err)
	}
	return data
}

func (w *world) latest(a App) int64 {
	var id int64
	if err := w.e.db.QueryRow(`SELECT max(id) FROM app_rows WHERE app_id = ?`, a.ID).Scan(&id); err != nil {
		w.e.t.Fatal(err)
	}
	return id
}

func TestCapturedValuesAreStoredAndShown(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createCapture(w.alice, s)
	sig := w.upload(w.alice, s, a, "signed", testPNG(t, 300, 100), 5000)
	loc := where(nil)
	rec := w.insert(w.alice, s, a, map[string]any{"code": "4006381333931", "place": loc, "signed": sig})
	expect(t, rec, http.StatusOK)
	id := w.latest(a)
	data := w.rowData(a, id)
	if got, _ := data["place"].(map[string]any); got["lat"] != 38.7223 || got["source"] != "device" || got["accuracy_m"] != 12.5 {
		t.Fatalf("stored location = %v", data["place"])
	}
	if data["signed"] != sig || data["code"] != "4006381333931" {
		t.Fatalf("stored = %v", data)
	}
	if w.attachedTo(sig) == nil {
		t.Fatal("the signature isn't attached")
	}
	// The screen is told what it needs of the fields, and the record's own readers read the signature.
	surface := w.alice.do(http.MethodGet, appPath(s, a.ID, "/surface"), nil).Body.String()
	for _, want := range []string{`"_fields":{"code":{"editable":false,"format":"ean_13","required":false},"free":{"capture":"button","editable":true,"required":false},"place":{"capture":"button","editable":false,"required":false},"signed":{"editable":false,"required":false}}`,
		`"_files":{"signed":{"max_bytes":1048576,"max_files":1}}`} {
		if !strings.Contains(surface, want) {
			t.Fatalf("surface lacks %s: %s", want, surface)
		}
	}
	expect(t, w.fetch(w.alice, s, a, sig, false), http.StatusOK)
	expect(t, w.fetch(w.alice, s, a, sig, true), http.StatusOK)
	// The records explorer's query reads them.
	list := w.alice.do(http.MethodGet, appPath(s, a.ID, "/records/query?limit=5"), nil)
	expect(t, list, http.StatusOK)
	if !strings.Contains(list.Body.String(), `"source":"device"`) || !strings.Contains(list.Body.String(), sig) {
		t.Fatalf("query = %s", list.Body)
	}
	// The agent sees the signature as the picture it is, never its bytes.
	items, err := w.e.s.ListRows(t.Context(), Actor{UserID: w.alice.id, Agent: true}, s.ID, a.ID)
	if err != nil || len(items) != 1 {
		t.Fatal(items, err)
	}
	if d, ok := items[0]["signed"].(Described); !ok || d.ID != sig || d.ContentType != "image/png" {
		t.Fatalf("agent sees %#v", items[0]["signed"])
	}
	// What can't be compared can't be filtered or sorted by.
	for _, q := range []string{"filter=place:eq:x", "filter=signed:eq:x", "sort=place", "sort=signed"} {
		rec := w.alice.do(http.MethodGet, appPath(s, a.ID, "/records/query?"+url.PathEscape(q)), nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, %s", q, rec.Code, rec.Body)
		}
	}
	// A code is filtered by like a text.
	expect(t, w.alice.do(http.MethodGet, appPath(s, a.ID, "/records/query?filter=code:eq:4006381333931"), nil), http.StatusOK)
}

func TestInsertRefusesBadCapturedValues(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createCapture(w.alice, s)
	for name, fields := range map[string]map[string]any{
		"lat out of range":  {"place": where(map[string]any{"lat": 91.0})},
		"future":            {"place": where(map[string]any{"captured_at": stamp(2 * time.Hour)})},
		"text as location":  {"place": "here"},
		"bad check digit":   {"code": "4006381333930"},
		"not digits":        {"code": "ABCDEFGHIJKLM"},
		"signature not id":  {"signed": "me"},
		"unknown signature": {"signed": strings.Repeat("0f", 16)},
		"unknown key":       {"place": where(map[string]any{"speed": 3.0})},
	} {
		rec := w.insert(w.alice, s, a, fields)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, %s", name, rec.Code, rec.Body)
		}
	}
	if n := w.rowCount(a); n != 0 {
		t.Fatalf("%d rows were stored", n)
	}
	// The error says which field and in what words: the code for the app to translate.
	rec := w.insert(w.alice, s, a, map[string]any{"place": where(map[string]any{"lat": 91.0})})
	if got := decode[struct {
		Code   string
		Params map[string]any
	}](t, rec); got.Code != "app_field_not_location" || got.Params["field"] != "place" {
		t.Fatalf("error = %s", rec.Body)
	}
	rec = w.insert(w.alice, s, a, map[string]any{"place": where(map[string]any{"captured_at": stamp(2 * time.Hour)})})
	expectCode(t, rec, http.StatusUnprocessableEntity, "app_field_location_future")
	expectCode(t, w.insert(w.alice, s, a, map[string]any{"code": "4006381333930"}), http.StatusUnprocessableEntity, "app_field_not_barcode")
}

func TestACapturedLocationCannotBeChanged(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createCapture(w.alice, s)
	first := where(nil)
	expect(t, w.insert(w.alice, s, a, map[string]any{"place": first, "free": first}), http.StatusOK)
	id := w.latest(a)
	before := w.rowData(a, id)

	// Changing it, from the screen's update action or the explorer's (the same call), is refused and
	// changes nothing: not a new revision, not the stored value.
	moved := where(map[string]any{"lat": 41.15, "lon": -8.61})
	for name, change := range map[string]map[string]any{
		"another position":  {"place": moved},
		"another accuracy":  {"place": where(map[string]any{"accuracy_m": 3.0})},
		"another time":      {"place": where(map[string]any{"captured_at": stamp(-time.Hour)})},
		"made manual":       {"place": where(map[string]any{"source": "manual"})},
		"cleared":           {"place": nil},
		"cleared with {}":   {"place": map[string]any{}},
		"cleared with text": {"place": ""},
		"with another edit": {"place": moved, "title": "renamed"},
	} {
		rec := w.edit(w.alice, s, a, id, change)
		expectCode(t, rec, http.StatusUnprocessableEntity, "app_field_locked")
		if got := w.rowData(a, id); !jsonEqual(got, before) {
			t.Fatalf("%s changed the record: %v", name, got)
		}
	}
	// The history has the insert only.
	var revisions int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM app_row_history WHERE row_id = ?`, id).Scan(&revisions); err != nil || revisions != 1 {
		t.Fatalf("history = %d %v", revisions, err)
	}
	// Other fields change freely, and saying the location again as it was changes nothing.
	expect(t, w.edit(w.alice, s, a, id, map[string]any{"title": "renamed"}), http.StatusOK)
	expect(t, w.edit(w.alice, s, a, id, map[string]any{"title": "renamed twice", "place": first}), http.StatusOK)
	if got := w.rowData(a, id); got["title"] != "renamed twice" || !jsonEqual(got["place"], before["place"]) {
		t.Fatalf("stored = %v", got)
	}
	// A location the field lets be edited is edited.
	expect(t, w.edit(w.alice, s, a, id, map[string]any{"free": moved}), http.StatusOK)
	if got := w.rowData(a, id)["free"].(map[string]any); got["lat"] != 41.15 {
		t.Fatalf("editable location = %v", got)
	}
	// A record that has none takes one once: that is the capture. The second is the change.
	expect(t, w.insert(w.alice, s, a, map[string]any{"title": "no place"}), http.StatusOK)
	late := w.latest(a)
	expect(t, w.edit(w.alice, s, a, late, map[string]any{"place": first}), http.StatusOK)
	expectCode(t, w.edit(w.alice, s, a, late, map[string]any{"place": moved}), http.StatusUnprocessableEntity, "app_field_locked")
	// Deleting and restoring a record leave the capture alone.
	if got := w.rowData(a, late)["place"]; !jsonEqual(got, before["place"]) {
		t.Fatalf("late capture = %v", got)
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestASignatureIsAPNGAndCannotBeChanged(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createCapture(w.alice, s)

	// The server finds the type in the bytes: a JPEG is no signature, and is not kept.
	jpg := testJPEG(t, 100, 40)
	id := w.start(w.alice, s, a, "signed", len(jpg))
	w.send(w.alice, s, a, id, jpg, 4000)
	expectCode(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/uploads/"+id+"/finalize"), map[string]any{}), http.StatusBadRequest, "app_signature_type")
	expectCode(t, w.alice.do(http.MethodGet, appPath(s, a.ID, "/uploads/"+id), nil), http.StatusNotFound, "not_found")
	// An attachment photo's field takes a JPEG still: the type is the signature's alone. A photo
	// uploaded for another field can't be a signature either.
	other := w.upload(w.alice, s, a, "signed", testPNG(t, 120, 50), 4000)
	// Its size is bounded by the field's limit.
	expectCode(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/uploads"), map[string]any{"field": "signed", "size": DefaultSignatureBytes + 1}),
		http.StatusBadRequest, "app_upload_size")
	// A field that is no attachment or signature has no upload.
	expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/uploads"), map[string]any{"field": "place", "size": 10}), http.StatusBadRequest)

	expect(t, w.insert(w.alice, s, a, map[string]any{"signed": []any{other}}), http.StatusOK)
	rid := w.latest(a)
	if w.rowData(a, rid)["signed"] != other {
		t.Fatalf("stored = %v", w.rowData(a, rid))
	}
	// Once signed, the signature stays: another picture, none, or the same id in a list are tried.
	second := w.upload(w.alice, s, a, "signed", testPNG(t, 130, 60), 4000)
	expectCode(t, w.edit(w.alice, s, a, rid, map[string]any{"signed": second}), http.StatusUnprocessableEntity, "app_field_locked")
	expectCode(t, w.edit(w.alice, s, a, rid, map[string]any{"signed": ""}), http.StatusUnprocessableEntity, "app_field_locked")
	if w.attachedTo(second) != nil {
		t.Fatal("a refused change attached the new signature")
	}
	expect(t, w.edit(w.alice, s, a, rid, map[string]any{"signed": []any{other}, "title": "ok"}), http.StatusOK)
	if got := w.rowData(a, rid); got["signed"] != other || got["title"] != "ok" {
		t.Fatalf("stored = %v", got)
	}
	// Nobody else uses the picture someone attached.
	expect(t, w.insert(w.alice, s, a, map[string]any{"signed": other}), http.StatusUnprocessableEntity)
}

func TestSignatureFieldTakesAPNGOnlyAtAttach(t *testing.T) {
	// Whatever finalizing did, a record can't hold an attachment of another type as a signature:
	// the type is checked again when the record claims it.
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createCapture(w.alice, s)
	id := w.upload(w.alice, s, a, "signed", testPNG(t, 80, 30), 4000)
	if _, err := w.e.db.Exec(`UPDATE app_attachments SET content_type = 'image/jpeg' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	expectCode(t, w.insert(w.alice, s, a, map[string]any{"signed": id}), http.StatusUnprocessableEntity, "app_attachment_bad")
	if w.attachedTo(id) != nil || w.rowCount(a) != 0 {
		t.Fatal("a picture that isn't a PNG was attached as a signature")
	}
}

func TestStoredCapturesFromBeforeAreStillShownAndEditable(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createCapture(w.alice, s)
	expect(t, w.insert(w.alice, s, a, map[string]any{"title": "old"}), http.StatusOK)
	id := w.latest(a)
	// A row written before the server's clock was set right: a capture whose time is ahead of now.
	future := where(map[string]any{"captured_at": stamp(48 * time.Hour)})
	raw, _ := json.Marshal(map[string]any{"title": "old", "place": future, "code": "4006381333931"})
	if _, err := w.e.db.Exec(`UPDATE app_rows SET data = ? WHERE id = ?`, string(raw), id); err != nil {
		t.Fatal(err)
	}
	list := w.alice.do(http.MethodGet, appPath(s, a.ID, "/records/query"), nil)
	expect(t, list, http.StatusOK)
	if !strings.Contains(list.Body.String(), `"place":{`) || !strings.Contains(list.Body.String(), "4006381333931") {
		t.Fatalf("a stored capture was hidden: %s", list.Body)
	}
	// An edit of another field goes through, and the stored values stay as they were.
	expect(t, w.edit(w.alice, s, a, id, map[string]any{"title": "new"}), http.StatusOK)
	got := w.rowData(a, id)
	if got["title"] != "new" || got["code"] != "4006381333931" || !jsonEqual(got["place"], future) {
		t.Fatalf("stored = %v", got)
	}
	// What is sent anew is held to today's rules.
	expectCode(t, w.edit(w.alice, s, a, id, map[string]any{"code": "also not an ean"}), http.StatusUnprocessableEntity, "app_field_not_barcode")
}

func TestCapturedValuesObeyTheRolesOfTheApp(t *testing.T) {
	// A viewer of a shared space can't capture anything: the writes are the space's editors'.
	w := newWorld(t)
	a := w.createCapture(w.alice, w.shared)
	rec := w.insert(w.bob, w.shared, a, map[string]any{"place": where(nil)})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer: %d %s", rec.Code, rec.Body)
	}
	// Someone with no access to the space can't upload a signature, or read one.
	expect(t, w.mallory.do(http.MethodPost, appPath(w.shared, a.ID, "/uploads"), map[string]any{"field": "signed", "size": 10}), http.StatusNotFound)
	sig := w.upload(w.alice, w.shared, a, "signed", testPNG(t, 50, 20), 4000)
	expect(t, w.insert(w.alice, w.shared, a, map[string]any{"signed": sig}), http.StatusOK)
	expect(t, w.fetch(w.mallory, w.shared, a, sig, false), http.StatusNotFound)
	expect(t, w.fetch(w.bob, w.shared, a, sig, false), http.StatusOK)
	// Nor can a member of the space change the editor's captured location, even a data manager: it is
	// the server that refuses, not the screen.
	expect(t, w.insert(w.alice, w.shared, a, map[string]any{"place": where(nil)}), http.StatusOK)
	id := w.latest(a)
	rec = w.edit(w.alice, w.shared, a, id, map[string]any{"place": where(map[string]any{"lat": 1.0})})
	expectCode(t, rec, http.StatusUnprocessableEntity, "app_field_locked")
}

// An app with one field, an attachment field or a signature field, drawn by a button that sends it.
func retypeApp(kind string) map[string]any {
	var def map[string]any
	text := `{
	  "components": [
	    {"id": "root", "component": "Column", "children": ["add"]},
	    {"id": "add_label", "component": "Text", "text": "Add"},
	    {"id": "add", "component": "Button", "child": "add_label", "action": {"event": {"name": "add_note", "context": {
	      "title": {"path": "/draft/title"}, "sig": {"path": "/draft/sig"}}}}}
	  ],
	  "collection": {"fields": [{"name": "title", "type": "string"}, {"name": "sig", "type": "` + kind + `"}]},
	  "actions": {"add_note": {"kind": "insert"}, "edit_note": {"kind": "update"}}
	}`
	if err := jsonUnmarshal(text, &def); err != nil {
		panic(err)
	}
	return def
}

func (w *world) retype(u *user, s access.Space, a App, kind string, ack ...SchemaChange) *httptest.ResponseRecorder {
	w.e.t.Helper()
	var def Definition
	raw, _ := json.Marshal(retypeApp(kind))
	if err := json.Unmarshal(raw, &def); err != nil {
		w.e.t.Fatal(err)
	}
	cur := w.view(u, s, a)
	cur = w.mustPatchDraft(u, s, cur, def)
	return w.tryPublish(u, s, cur, ack...)
}

func TestAPhotoNeverBecomesALockedSignature(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	rec := w.alice.do(http.MethodPost, appsPath(s, ""), map[string]any{"name": "Retyped", "definition": retypeApp("attachment")})
	expect(t, rec, http.StatusCreated)
	a := w.publish(w.alice, s, decode[App](t, rec))

	// A photo in a record, and a PNG photo uploaded and waiting to be saved.
	photo := w.upload(w.alice, s, a, "sig", testJPEG(t, 90, 40), 4000)
	pending := w.upload(w.alice, s, a, "sig", testPNG(t, 90, 40), 4000)
	expect(t, w.insert(w.alice, s, a, map[string]any{"sig": []any{photo}}), http.StatusOK)
	rid := w.latest(a)

	// Making the field a signature is a change of type, which the publisher acknowledges.
	expectCode(t, w.retype(w.alice, s, a, "signature"), http.StatusConflict, "app_migration_required")
	a = decode[App](t, w.view2(w.alice, s, a))
	expect(t, w.retype(w.alice, s, a, "signature", SchemaChange{ChangeFieldTypeChanged, "sig"}), http.StatusOK)

	// The stored list of photos is no signature: it is not shown, and not locked.
	list := w.alice.do(http.MethodGet, appPath(s, a.ID, "/records/query"), nil)
	expect(t, list, http.StatusOK)
	if strings.Contains(list.Body.String(), photo) {
		t.Fatalf("a photo is shown as a signature: %s", list.Body)
	}
	if _, locked := (Field{Name: "sig", Type: TypeSignature}).locked(w.rowData(a, rid)); locked {
		t.Fatal("a photo is a locked signature")
	}
	// A photo that was waiting for the field it was is not a signature either, though it is a PNG.
	expectCode(t, w.edit(w.alice, s, a, rid, map[string]any{"sig": pending}), http.StatusUnprocessableEntity, "app_attachment_bad")
	if w.attachedTo(pending) != nil {
		t.Fatal("a photo was attached as a signature")
	}
	// A picture drawn for the signature is the capture, and then it is locked.
	signature := w.upload(w.alice, s, a, "sig", testPNG(t, 120, 50), 4000)
	expect(t, w.edit(w.alice, s, a, rid, map[string]any{"sig": signature}), http.StatusOK)
	if got := w.rowData(a, rid)["sig"]; got != signature {
		t.Fatalf("stored = %v", got)
	}
	other := w.upload(w.alice, s, a, "sig", testPNG(t, 130, 50), 4000)
	expectCode(t, w.edit(w.alice, s, a, rid, map[string]any{"sig": other}), http.StatusUnprocessableEntity, "app_field_locked")
}

func (w *world) view2(u *user, s access.Space, a App) *httptest.ResponseRecorder {
	return u.do(http.MethodGet, appPath(s, a.ID, ""), nil)
}

func TestARetypedStoredListIsNoSignature(t *testing.T) {
	id := strings.Repeat("ab", 16)
	f := Field{Name: "sig", Type: TypeSignature}
	// A screen's picture input binds a list of one; a row never holds one.
	if got, present, err := f.parse([]any{id}); err != nil || !present || got != id {
		t.Fatalf("a list from a screen: %v %v %v", got, present, err)
	}
	if _, present, err := f.stored([]any{id}); err == nil && present {
		t.Fatal("a list of photos is read as a stored signature")
	}
	if ofType(f, []any{id}) {
		t.Fatal("a list of photos is of a signature's type")
	}
	if _, locked := f.locked(map[string]any{"sig": []any{id}}); locked {
		t.Fatal("a list of photos is a locked signature")
	}
	if _, locked := f.locked(map[string]any{"sig": id}); !locked {
		t.Fatal("a signature isn't locked")
	}
}

func TestTypeChangesIntoCapturedFieldsAreAcknowledged(t *testing.T) {
	// Whether or not a row holds a value: what the field takes from then on is locked.
	for _, from := range []string{TypeAttachment, TypeString} {
		for _, into := range []string{TypeSignature, TypeLocation} {
			live := Collection{Fields: []Field{{Name: "x", Type: from}}}
			next := Collection{Fields: []Field{{Name: "x", Type: into}}}
			want := []SchemaChange{{ChangeFieldTypeChanged, "x"}}
			for _, blind := range []bool{true, false} {
				if got := breakingChanges(&live, next, nil, blind); !slices.Equal(got, want) {
					t.Errorf("%s to %s, blind %v: %v", from, into, blind, got)
				}
			}
		}
	}
	// Another change of type with no value stored is still free.
	live := Collection{Fields: []Field{{Name: "x", Type: TypeString}}}
	next := Collection{Fields: []Field{{Name: "x", Type: TypeNumber}}}
	if got := breakingChanges(&live, next, nil, false); len(got) != 0 {
		t.Errorf("string to number with no rows: %v", got)
	}
}

func TestAPendingUploadIsHeldToTheFieldsLimitNow(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createCapture(w.alice, s)
	// A signature uploaded under the default limit, then the field's limit is tightened.
	sig := w.upload(w.alice, s, a, "signed", testPNG(t, 300, 100), 5000)
	cur := w.view(w.alice, s, a)
	cur = w.mustPatchDraft(w.alice, s, cur, fieldsOf(*cur.Draft, "signed", func(f *Field) { f.MaxBytes = 50 }))
	expect(t, w.tryPublish(w.alice, s, cur), http.StatusOK)
	expectCode(t, w.insert(w.alice, s, a, map[string]any{"signed": sig}), http.StatusUnprocessableEntity, "app_attachment_bad")
	if w.attachedTo(sig) != nil || w.rowCount(a) != 0 {
		t.Fatal("a signature over the limit was attached")
	}

	// Photos the same.
	p := w.createPhotos(w.alice, s, "Photos")
	photo := w.upload(w.alice, s, p, "photos", testJPEG(t, 300, 200), 5000)
	cur = w.view(w.alice, s, p)
	cur = w.mustPatchDraft(w.alice, s, cur, fieldsOf(*cur.Draft, "photos", func(f *Field) { f.MaxBytes = 100 }))
	expect(t, w.tryPublish(w.alice, s, cur), http.StatusOK)
	expectCode(t, w.insert(w.alice, s, p, map[string]any{"photos": []any{photo}}), http.StatusUnprocessableEntity, "app_attachment_bad")
	if w.attachedTo(photo) != nil {
		t.Fatal("a photo over the limit was attached")
	}
}

func TestClearingARequiredCapturedFieldIsALockedChange(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createCapture(w.alice, s)
	cur := w.view(w.alice, s, a)
	cur = w.mustPatchDraft(w.alice, s, cur, fieldsOf(*cur.Draft, "place", func(f *Field) { f.Required = true }))
	expect(t, w.tryPublish(w.alice, s, cur), http.StatusOK)
	expect(t, w.insert(w.alice, s, a, map[string]any{"place": where(nil)}), http.StatusOK)
	id := w.latest(a)
	// Clearing it is refused as the change it is, not for the field's being required.
	for _, clear := range []any{nil, "", map[string]any{}} {
		expectCode(t, w.edit(w.alice, s, a, id, map[string]any{"place": clear}), http.StatusUnprocessableEntity, "app_field_locked")
	}
	// A required field that was never captured is still required.
	expectCode(t, w.insert(w.alice, s, a, map[string]any{}), http.StatusUnprocessableEntity, "app_field_required")
}

func TestStrictCapturedValuesRefuseWhatParsersLetThrough(t *testing.T) {
	code := Field{Name: "code", Type: TypeBarcode, MaxLength: 40}
	for _, s := range []string{"4006381333931\n", "\t4006381333931", "\r\nabc", "abc\x00 "} {
		if got, _, err := code.parse(s); err == nil {
			t.Errorf("%q accepted as %v", s, got)
		}
	}
	// A line break alone is no code, and spaces around one are still padding.
	if _, present, err := code.parse("\n"); err != nil || present {
		t.Errorf("a line break: %v %v", present, err)
	}
	if got, _, err := code.parse("  abc "); err != nil || got != "abc" {
		t.Errorf("padded: %v %v", got, err)
	}
	place := Field{Name: "place", Type: TypeLocation}
	for _, zone := range []string{"+24:60", "+24:00", "+23:60", "-24:00", "+99:00", "+05:75"} {
		at := time.Now().Add(-48*time.Hour).UTC().Format("2006-01-02T15:04:05") + zone
		if _, _, err := place.parse(where(map[string]any{"captured_at": at})); err == nil {
			t.Errorf("captured_at %s accepted", at)
		}
	}
	for _, zone := range []string{"Z", "+23:59", "-12:00", "+00:00"} {
		at := time.Now().Add(-48*time.Hour).UTC().Format("2006-01-02T15:04:05") + zone
		if _, _, err := place.parse(where(map[string]any{"captured_at": at})); err != nil {
			t.Errorf("captured_at %s: %v", at, err)
		}
	}
}
