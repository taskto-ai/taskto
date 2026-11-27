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
	Shade    string           `json:"shade"`
	Name     string           `json:"name"`
	Maybe    *string          `json:"maybe"`
	Omitted  *string          `json:"omitted,omitempty"`
	Count    int              `json:"count,omitempty"`
	Kind     kind             `json:"kind"`
	When     time.Time        `json:"when"`
	Bytes    []byte           `json:"bytes"`
	Raw      json.RawMessage  `json:"raw" apitypes:"json"`
	Any      any              `json:"any" apitypes:"json"`
	Tags     []string         `json:"tags"`
	Sure     []string         `json:"sure" apitypes:"nonnull"`
	Maybes   []*string        `json:"maybes" apitypes:"nonnull"`
	ByName   map[string]inner `json:"by_name"`
	Nested   inner            `json:"nested"`
	Anon     struct{ A int }  `json:"anon"`
	Hidden   string           `json:"-"`
	Odd      string           `json:"odd-name"`
	Fallback string
	secret   string
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
		"  id: number\n",
		"  shade: string\n",
		"  maybe: string | null\n",
		"  omitted?: string\n",
		"  count?: number\n",
		"  kind: string\n",
		"  when: string\n",
		"  bytes: string | null\n",
		"  raw: unknown\n",
		"  any: unknown\n",
		"  tags: string[] | null\n",
		"  sure: string[]\n",
		"  maybes: (string | null)[]\n",
		"  by_name: Record<string, PackageInner> | null\n",
		"  nested: PackageInner\n",
		"  anon: {\n    A: number\n  }\n",
		"  \"odd-name\": string\n",
		"  Fallback: string\n",
		"  numbered: string\n",
		"  fixed: number[]\n",
		"  labels?: Record<string, string>\n",
		"  free: Record<string, unknown>\n",
		"  key: Record<string, number> | null\n",
		"  text_key: Record<string, number> | null\n",
		"export type PackageInner = {\n  deep: string\n}\n",
	} {
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

type custom struct{ Hidden string }

func (custom) MarshalJSON() ([]byte, error) { return []byte(`1`), nil }

type promoted struct {
	time.Time
	Extra string `json:"extra"`
}

type level int

func (level) MarshalJSON() ([]byte, error) { return []byte(`"high"`), nil }

type pointerKey struct{ N int }

func (*pointerKey) MarshalText() ([]byte, error) { return []byte("k"), nil }

type code int

func (*code) MarshalText() ([]byte, error) { return []byte("c"), nil }

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

		"a marshaler on a pointer, in a map": {{Name: "F", Type: reflect.TypeFor[struct{ M map[string]code }]()}},
		"a marshaler on a pointer, by value": {{Name: "F", Type: reflect.TypeFor[struct{ C code }]()}},
		"a type that holds itself":           {{Name: "F", Type: reflect.TypeFor[struct{ T tree }]()}},
		"a name that is not TypeScript's":    {{Name: "F", Type: reflect.TypeFor[struct{ P page[int] }]()}},

		"an any":                  {{Name: "F", Type: reflect.TypeFor[struct{ V any }]()}},
		"an any in a map":         {{Name: "F", Type: reflect.TypeFor[struct{ V map[string]any }]()}},
		"a raw message":           {{Name: "F", Type: reflect.TypeFor[struct{ V json.RawMessage }]()}},
		"raw messages in a slice": {{Name: "F", Type: reflect.TypeFor[struct{ V []json.RawMessage }]()}},
		"an error":                {{Name: "F", Type: reflect.TypeFor[struct{ V error }]()}},
		"a promise on a plain type": {{Name: "F", Type: reflect.TypeFor[struct {
			V string `apitypes:"nonnull"`
		}]()}},

		"a key that is a struct":        {{Name: "K", Type: reflect.TypeFor[struct{ M map[inner]int }]()}},
		"a key whose pointer is a text": {{Name: "K", Type: reflect.TypeFor[struct{ M map[pointerKey]int }]()}},
		"a key that is a float":         {{Name: "K", Type: reflect.TypeFor[struct{ M map[float64]int }]()}},
	} {
		if _, err := Generate(roots); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
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
		Deep  **int            `json:"deep,string"`
	}
	out, err := Generate([]Root{{Name: "Okay", Type: reflect.TypeFor[okay]()}})
	if err != nil {
		t.Fatalf("allowed fields were refused: %v", err)
	}
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
	right
}

type optionalInner struct {
	ID int `json:"id"`
}

type viaPointer struct {
	*optionalInner
	N *int `json:"n,string"`
}

func TestGenerateEmbedding(t *testing.T) {
	if _, err := Generate([]Root{{Name: "Clash", Type: reflect.TypeFor[clash]()}}); err == nil {
		t.Error("an ambiguous field was not rejected")
	}
	out, err := Generate([]Root{{Name: "V", Type: reflect.TypeFor[viaPointer]()}})
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{"  id?: number\n", "  n: string | null\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("the output lacks %q:\n%s", want, got)
		}
	}
}

type wireField struct{ optional, null bool }

var fieldLine = regexp.MustCompile(`^  ("[^"]+"|[A-Za-z_$][A-Za-z0-9_$]*)(\?)?: (.*)$`)

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
	unexportedEmbedded `json:"tagged"`
	Plain              *int           `json:"plain"`
	Struct             inner          `json:"struct,omitempty"`
	Zero               inner          `json:"zero,omitzero"`
	List               []string       `json:"list,omitempty"`
	Items              []string       `json:"items"`
	Both               *int           `json:"both,omitempty"`
	Cut                zeroSlice      `json:"cut,omitzero"`
	Arr                [2]int         `json:"arr,omitempty"`
	Raw                []byte         `json:"raw"`
	Dash               string         `json:"-,"`
	Skipped            string         `json:"it's,omitempty"`
	Text               map[string]int `json:"text,omitempty"`
	Number             *int           `json:"number,omitempty,string"`
	PtrList            *[]string      `json:"ptr_list,omitempty"`
	PtrText            *string        `json:"ptr_text,omitempty"`
	PtrPtr             **[]string     `json:"ptr_ptr,omitempty"`
	hidden             string
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
	for key, want := range map[string]wireField{
		"value":    {optional: true, null: true},
		"name":     {optional: true, null: false},
		"loud":     {},
		"tagged":   {},
		"plain":    {null: true},
		"struct":   {},
		"zero":     {optional: true},
		"list":     {optional: true},
		"items":    {null: true},
		"both":     {optional: true},
		"cut":      {optional: true, null: true},
		"arr":      {},
		"raw":      {null: true},
		"-":        {},
		"Skipped":  {optional: true},
		"number":   {optional: true},
		"ptr_list": {optional: true, null: true},
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
