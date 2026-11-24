package apitypes

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
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
	Raw      json.RawMessage   `json:"raw"`
	Any      any               `json:"any"`
	Tags     []string          `json:"tags"`
	Maybes   []*string         `json:"maybes"`
	ByName   map[string]inner  `json:"by_name"`
	Nested   inner             `json:"nested"`
	Anon     struct{ A int }   `json:"anon"`
	Hidden   string            `json:"-"`
	Odd      string            `json:"odd-name"`
	Fallback string            // no tag: the Go name
	secret   string            //nolint:unused // proves unexported fields are left out
	Custom   custom            `json:"custom"`
	Numbered int               `json:"numbered,string"`
	Fixed    [2]int            `json:"fixed"`
	Labels   map[string]string `json:"labels,omitempty"`
}

type custom struct{}

func (custom) MarshalJSON() ([]byte, error) { return []byte(`1`), nil }

func TestGenerate(t *testing.T) {
	out, err := Generate([]Root{{Name: "Sample", Type: reflect.TypeFor[*sample]()}})
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{
		"export type Sample = {\n",
		"  id: number\n",           // an embedded struct's fields come up
		"  shade: string\n",        // the outer field wins over the embedded one
		"  maybe: string | null\n", // a pointer may be null
		"  omitted?: string\n",     // a pointer that is left out when nil
		"  count?: number\n",       // omitempty
		"  kind: string\n",         // a named string
		"  when: string\n",         // a time
		"  bytes: string\n",        // base64
		"  raw: unknown\n",
		"  any: unknown\n",
		"  tags: string[]\n",
		"  maybes: (string | null)[]\n",
		"  by_name: Record<string, PackageInner>\n",
		"  nested: PackageInner\n",
		"  anon: {\n    A: number\n  }\n", // written in place, with the Go name
		"  \"odd-name\": string\n",
		"  Fallback: string\n",
		"  custom: unknown\n",
		"  numbered: string\n",
		"  fixed: number[]\n",
		"  labels?: Record<string, string>\n",
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

func TestGenerateRejects(t *testing.T) {
	type a struct{ X int }
	type b struct{ Y int }
	for name, roots := range map[string][]Root{
		"not a struct": {{Name: "N", Type: reflect.TypeFor[int]()}},
		"listed twice": {{Name: "A", Type: reflect.TypeFor[a]()}, {Name: "B", Type: reflect.TypeFor[a]()}},
		"a name taken": {{Name: "A", Type: reflect.TypeFor[a]()}, {Name: "A", Type: reflect.TypeFor[b]()}},
		"a channel":    {{Name: "C", Type: reflect.TypeFor[struct{ C chan int }]()}},
	} {
		if _, err := Generate(roots); err == nil {
			t.Errorf("%s: no error", name)
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
