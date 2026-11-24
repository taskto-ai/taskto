package apitypes

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestGeneratedIsCurrent fails when the committed web/src/api.gen.ts is not what the structs
// say: a field renamed or retyped in Go. Regenerating it makes the web app's typecheck say
// where it still reads the old field.
func TestGeneratedIsCurrent(t *testing.T) {
	want, err := Generate(Roots())
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(Output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("web/src/api.gen.ts differs from the Go structs: run go generate ./internal/apitypes, then pnpm typecheck")
	}
}

type inner struct {
	Deep string `json:"deep"`
}

type base struct {
	ID    int64 `json:"id"`
	Shade int   `json:"shade"`
}

type kind string

type sample struct {
	base
	Shade    string            `json:"shade"`
	Name     string            `json:"name"`
	Maybe    *string           `json:"maybe"`
	Omitted  *string           `json:"omitted,omitempty"`
	Count    int               `json:"count,omitempty"`
	Kind     kind              `json:"kind"`
	When     time.Time         `json:"when"`
	Bytes    []byte            `json:"bytes"`
	Raw      json.RawMessage   `json:"raw" apitypes:"json"`
	Any      any               `json:"any" apitypes:"json"`
	Tags     []string          `json:"tags"`
	Sure     []string          `json:"sure" apitypes:"nonnull"`
	Maybes   []*string         `json:"maybes" apitypes:"nonnull"`
	ByName   map[string]inner  `json:"by_name"`
	Nested   inner             `json:"nested"`
	Anon     struct{ A int }   `json:"anon"`
	Hidden   string            `json:"-"`
	Odd      string            `json:"odd-name"`
	Fallback string            // no tag: the Go name
	secret   string            //nolint:unused // proves unexported fields are left out
	Numbered int               `json:"numbered,string"`
	Fixed    [2]int            `json:"fixed"`
	Labels   map[string]string `json:"labels,omitempty"`
	Free     map[string]any    `json:"free" apitypes:"json,nonnull"`
	Key      map[uintptr]int   `json:"key"`
	TextKey  map[textKey]int   `json:"text_key"`
}

type textKey struct{ N int }

func (k textKey) MarshalText() ([]byte, error) { return []byte("k"), nil }

func TestGenerate(t *testing.T) {
	out, err := Generate([]Root{{Name: "Sample", Type: reflect.TypeFor[*sample]()}})
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{
		"export type Sample = {\n",
		"  id: number\n",                // an embedded struct's fields come up
		"  shade: string\n",             // the outer field wins over the embedded one
		"  maybe: string | null\n",      // a pointer may be null
		"  omitted?: string\n",          // a pointer that is left out when nil is never null
		"  count?: number\n",            // omitempty
		"  kind: string\n",              // a named string
		"  when: string\n",              // a time
		"  bytes: string | null\n",      // base64, null when nil
		"  raw: unknown\n",              // free-form JSON, said to be
		"  any: unknown\n",              // as is an any
		"  tags: string[] | null\n",     // a nil slice is null
		"  sure: string[]\n",            // unless promised not to be
		"  maybes: (string | null)[]\n", // the promise is of the slice, not its elements
		"  by_name: Record<string, PackageInner> | null\n",
		"  nested: PackageInner\n",
		"  anon: {\n    A: number\n  }\n", // written in place, with the Go name
		"  \"odd-name\": string\n",
		"  Fallback: string\n",
		"  numbered: string\n",
		"  fixed: number[]\n", // an array is never null
		"  labels?: Record<string, string>\n",
		"  free: Record<string, unknown>\n",
		"  key: Record<string, number> | null\n",
		"  text_key: Record<string, number> | null\n",
		"export type PackageInner = {\n  deep: string\n}\n",
	} {
		// The package's name is apitypes here.
		want = strings.ReplaceAll(want, "Package", "Apitypes")
		if !strings.Contains(got, want) {
			t.Errorf("the output lacks %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"Hidden", "secret", "  shade: number"} {
		if strings.Contains(got, not) {
			t.Errorf("the output has %q:\n%s", not, got)
		}
	}
}

// custom writes itself: its struct says nothing of it.
type custom struct{ Hidden string }

func (custom) MarshalJSON() ([]byte, error) { return []byte(`1`), nil }

// promoted gets the MarshalJSON of the time it embeds.
type promoted struct {
	time.Time
	Extra string `json:"extra"`
}

// level is a number that writes itself as text: a string, not a number.
type level int

func (level) MarshalJSON() ([]byte, error) { return []byte(`"high"`), nil }

// pointerKey's MarshalText is on its pointer, which a map key is not.
type pointerKey struct{ N int }

func (*pointerKey) MarshalText() ([]byte, error) { return []byte("k"), nil }

// code is written as text only when encoding/json can address it, which a map's values are not.
type code int

func (*code) MarshalText() ([]byte, error) { return []byte("c"), nil }

// tree holds itself: it has no type to write in place.
type tree []tree

type page[T any] struct{ Items []T }

func TestGenerateRejects(t *testing.T) {
	type a struct{ X int }
	type b struct{ Y int }
	for name, roots := range map[string][]Root{
		"not a struct": {{Name: "N", Type: reflect.TypeFor[int]()}},
		"listed twice": {{Name: "A", Type: reflect.TypeFor[a]()}, {Name: "B", Type: reflect.TypeFor[a]()}},
		"a name taken": {{Name: "A", Type: reflect.TypeFor[a]()}, {Name: "A", Type: reflect.TypeFor[b]()}},
		"a channel":    {{Name: "C", Type: reflect.TypeFor[struct{ C chan int }]()}},

		// A MarshalJSON of its own, which would leave out the field the struct has.
		"a root with a MarshalJSON":  {{Name: "C", Type: reflect.TypeFor[custom]()}},
		"a root with a promoted one": {{Name: "P", Type: reflect.TypeFor[promoted]()}},
		"a field with a MarshalJSON": {{Name: "F", Type: reflect.TypeFor[struct{ C custom }]()}},
		"a pointer to one":           {{Name: "F", Type: reflect.TypeFor[struct{ C *custom }]()}},
		"in a slice":                 {{Name: "F", Type: reflect.TypeFor[struct{ C []custom }]()}},
		"in a map":                   {{Name: "F", Type: reflect.TypeFor[struct{ C map[string]custom }]()}},
		"that the string option meets": {{Name: "F", Type: reflect.TypeFor[struct {
			L level `json:"l,string"`
		}]()}},
		"a root a time promotes to": {{Name: "T", Type: reflect.TypeFor[time.Time]()}},

		// What encoding/json decides by the value, or by where it is.
		"a marshaler on a pointer, in a map": {{Name: "F", Type: reflect.TypeFor[struct{ M map[string]code }]()}},
		"a marshaler on a pointer, by value": {{Name: "F", Type: reflect.TypeFor[struct{ C code }]()}},
		"a type that holds itself":           {{Name: "F", Type: reflect.TypeFor[struct{ T tree }]()}},
		"a name that is not TypeScript's":    {{Name: "F", Type: reflect.TypeFor[struct{ P page[int] }]()}},

		// Free-form JSON is said to be so.
		"an any":                  {{Name: "F", Type: reflect.TypeFor[struct{ V any }]()}},
		"an any in a map":         {{Name: "F", Type: reflect.TypeFor[struct{ V map[string]any }]()}},
		"a raw message":           {{Name: "F", Type: reflect.TypeFor[struct{ V json.RawMessage }]()}},
		"raw messages in a slice": {{Name: "F", Type: reflect.TypeFor[struct{ V []json.RawMessage }]()}},
		"an error":                {{Name: "F", Type: reflect.TypeFor[struct{ V error }]()}},
		"a promise on a plain type": {{Name: "F", Type: reflect.TypeFor[struct {
			V string `apitypes:"nonnull"`
		}]()}},

		// A map's key is a string, a number or an encoding.TextMarshaler itself.
		"a key that is a struct":        {{Name: "K", Type: reflect.TypeFor[struct{ M map[inner]int }]()}},
		"a key whose pointer is a text": {{Name: "K", Type: reflect.TypeFor[struct{ M map[pointerKey]int }]()}},
		"a key that is a float":         {{Name: "K", Type: reflect.TypeFor[struct{ M map[float64]int }]()}},
	} {
		if _, err := Generate(roots); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// What is allowed beside them.
	type okay struct {
		When  time.Time        `json:"when"`
		Pager *time.Time       `json:"pager"`
		Words map[textKey]int  `json:"words"`
		Ptrs  map[*textKey]int `json:"ptrs"`
		Wide  map[uintptr]int  `json:"wide"`
		Free  []any            `json:"free" apitypes:"json"`
		Ptr   *code            `json:"ptr"`
		Num   json.Number      `json:"num"`
		Quote json.Number      `json:"quote,string"`
		Deep  **int            `json:"deep,string"` //nolint:staticcheck // the option meets two pointers, which encoding/json ignores
	}
	out, err := Generate([]Root{{Name: "Okay", Type: reflect.TypeFor[okay]()}})
	if err != nil {
		t.Fatalf("allowed fields were refused: %v", err)
	}
	// A json.Number is a number, and a string with the option; the option looks through one
	// pointer, so it leaves a pointer to a pointer to a number a number.
	for _, want := range []string{"  num: number\n", "  quote: string\n", "  deep: number | null\n", "  ptr: string | null\n"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
}

type left struct {
	X int `json:"x"`
}

type right struct {
	X int `json:"x"`
}

type clash struct {
	left
	right //nolint:govet // the clash is what the test is about
}

type optionalInner struct {
	ID int `json:"id"`
}

type viaPointer struct {
	*optionalInner
	N *int `json:"n,string"`
}

func TestGenerateEmbedding(t *testing.T) {
	// Two embedded structs with the same name at the same depth: encoding/json writes neither.
	if _, err := Generate([]Root{{Name: "Clash", Type: reflect.TypeFor[clash]()}}); err == nil {
		t.Error("an ambiguous field was not rejected")
	}
	out, err := Generate([]Root{{Name: "V", Type: reflect.TypeFor[viaPointer]()}})
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	// A nil embedded pointer leaves its fields out; a pointer with the string option is still null.
	for _, want := range []string{"  id?: number\n", "  n: string | null\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("the output lacks %q:\n%s", want, got)
		}
	}
}

// wireField is how a generated field reads: whether its key may be absent and whether its
// value may be null.
type wireField struct{ optional, null bool }

var fieldLine = regexp.MustCompile(`^  ("[^"]+"|[A-Za-z_$][A-Za-z0-9_$]*)(\?)?: (.*)$`)

// shapeOf reads the top-level fields of the type generated for v from the output.
func shapeOf(t *testing.T, v any) map[string]wireField {
	t.Helper()
	out, err := Generate([]Root{{Name: "Wire", Type: reflect.TypeOf(v)}})
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]wireField{}
	in := false
	for line := range strings.SplitSeq(string(out), "\n") {
		switch {
		case line == "export type Wire = {":
			in = true
		case in && line == "}":
			return fields
		case in:
			if m := fieldLine.FindStringSubmatch(line); m != nil {
				typ := m[3]
				fields[strings.Trim(m[1], `"`)] = wireField{optional: m[2] == "?", null: strings.HasSuffix(typ, "| null") || typ == "unknown"}
			}
		}
	}
	t.Fatalf("no Wire in:\n%s", out)
	return nil
}

// sameAsEncoding fails when what encoding/json writes for a value is not what the generated
// shape says: a key written that the shape lacks, or null where it says never, or a key left
// out of a field it says is always there.
func sameAsEncoding(t *testing.T, name string, shape map[string]wireField, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for key, val := range got {
		f, ok := shape[key]
		switch {
		case !ok:
			t.Errorf("%s: %s writes %q, which the shape lacks", name, raw, key)
		case string(val) == "null" && !f.null:
			t.Errorf("%s: %s writes %q as null, which the shape says it never is", name, raw, key)
		}
	}
	for key, f := range shape {
		if _, ok := got[key]; !ok && !f.optional {
			t.Errorf("%s: %s leaves out %q, which the shape says is always there", name, raw, key)
		}
	}
}

type zeroSlice []int

// IsZero says even a nil one is not zero, so omitzero writes it, as null.
func (zeroSlice) IsZero() bool { return false }

type unexportedEmbedded struct {
	Quiet int `json:"quiet"`
}

type promotedHidden struct {
	Loud int `json:"loud"`
}

type wireSample struct {
	*optionalWire
	promotedHidden
	unexportedEmbedded `json:"tagged"` // an unexported struct with a name is written under it
	Plain              *int            `json:"plain"`
	Struct             inner           `json:"struct,omitempty"` // omitempty never leaves a struct out
	Zero               inner           `json:"zero,omitzero"`    // omitzero does
	List               []string        `json:"list,omitempty"`
	Items              []string        `json:"items"`
	Both               *int            `json:"both,omitempty"`
	Cut                zeroSlice       `json:"cut,omitzero"`
	Arr                [2]int          `json:"arr,omitempty"` // an array with elements is always written
	Raw                []byte          `json:"raw"`
	Dash               string          `json:"-,"`             //nolint:staticcheck // the tag is what is tested
	Skipped            string          `json:"it's,omitempty"` //nolint:staticcheck // an invalid name, which encoding/json ignores
	Text               map[string]int  `json:"text,omitempty"`
	Number             *int            `json:"number,omitempty,string"`
	PtrList            *[]string       `json:"ptr_list,omitempty"` // a pointer to a nil slice is written, as null
	PtrText            *string         `json:"ptr_text,omitempty"`
	PtrPtr             **[]string      `json:"ptr_ptr,omitempty"`
	hidden             string          //nolint:unused // not written
}

type optionalWire struct {
	Value *int   `json:"value"`
	Name  string `json:"name"`
}

func TestShapeIsWhatEncodingWrites(t *testing.T) {
	shape := shapeOf(t, wireSample{})
	one := 1
	for name, v := range map[string]wireSample{
		"zero values":      {},
		"an embedded one":  {optionalWire: &optionalWire{}},
		"with a value":     {optionalWire: &optionalWire{Value: &one, Name: "n"}, Plain: &one, Both: &one, Number: &one},
		"empty, not nil":   {List: []string{}, Items: []string{}, Text: map[string]int{}, Cut: zeroSlice{}},
		"filled in":        {List: []string{"a"}, Items: []string{"b"}, Raw: []byte("x"), Text: map[string]int{"a": 1}, Cut: zeroSlice{1}},
		"zero struct":      {Zero: inner{}},
		"a nil list held":  {PtrList: new([]string), PtrText: new(string), PtrPtr: new(*[]string)},
		"a list held":      {PtrList: &[]string{"a"}},
		"a nonzero struct": {Zero: inner{Deep: "d"}, Struct: inner{Deep: "d"}},
	} {
		sameAsEncoding(t, name, shape, v)
	}
	// The cases that made a field look wrong, said outright.
	for key, want := range map[string]wireField{
		"value":    {optional: true, null: true},  // an embedded pointer's nil *int: absent, or null when present
		"name":     {optional: true, null: false}, // absent with the embedded pointer nil
		"loud":     {},                            // an unexported embedded struct's fields come up
		"tagged":   {},                            // and one with a name is that name
		"plain":    {null: true},
		"struct":   {},               // omitempty doesn't leave a struct out
		"zero":     {optional: true}, // omitzero does
		"list":     {optional: true}, // left out when empty: never null
		"items":    {null: true},     // written as null when nil
		"both":     {optional: true},
		"cut":      {optional: true, null: true}, // a type that says its nil is not zero
		"arr":      {},
		"raw":      {null: true},
		"-":        {},
		"Skipped":  {optional: true}, // an invalid name in a tag is ignored: the Go name
		"number":   {optional: true},
		"ptr_list": {optional: true, null: true}, // a pointer, left out when nil, to a slice that may be nil
		"ptr_text": {optional: true},
		"ptr_ptr":  {optional: true, null: true},
	} {
		if got := shape[key]; got != want {
			t.Errorf("%s: %+v, want %+v", key, got, want)
		}
	}
	for _, not := range []string{"quiet", "hidden", "it's"} {
		if _, has := shape[not]; has {
			t.Errorf("%s is in the shape", not)
		}
	}
}
