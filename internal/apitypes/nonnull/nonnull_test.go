package nonnull

import (
	"slices"
	"testing"
)

type leaf struct {
	Tags  []string       `json:"tags" apitypes:"nonnull"`
	Free  map[string]any `json:"free" apitypes:"json,nonnull"`
	Loose []string       `json:"loose"`
}

type holder struct {
	Items  []leaf           `json:"items"`
	ByName map[string]*leaf `json:"by_name"`
	One    *leaf            `json:"one"`
	Any    any              `json:"any"`
}

func TestViolationsFindsNilPromisedFieldsAnywhere(t *testing.T) {
	ok := leaf{Tags: []string{}, Free: map[string]any{}}
	if got := Violations(ok); len(got) != 0 {
		t.Errorf("a kept promise: %v", got)
	}
	if got := Violations(leaf{}); !slices.Equal(got, []string{"leaf.Tags", "leaf.Free"}) {
		t.Errorf("a bare leaf: %v", got)
	}
	h := holder{
		Items:  []leaf{ok, {}},
		ByName: map[string]*leaf{"a": {Free: map[string]any{}}},
		One:    &ok,
		Any:    leaf{Tags: []string{}},
	}
	got := Violations(h)
	want := []string{"holder.Items[1].Tags", "holder.Items[1].Free", "holder.ByName[a].Tags", "holder.Any.Free"}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("violations = %v, want %v", got, want)
	}
	if got := Violations(holder{}); len(got) != 0 {
		t.Errorf("an empty holder: %v", got)
	}
	type loop struct {
		Next *loop
		Tags []string `apitypes:"nonnull"`
	}
	l := &loop{Tags: []string{}}
	l.Next = l
	if got := Violations(l); len(got) != 0 {
		t.Errorf("a loop: %v", got)
	}
}

func TestHas(t *testing.T) {
	if !Has(holder{}) || !Has(&[]leaf{}) || !Has(map[string]leaf{}) {
		t.Error("a type holding a promise is not seen to")
	}
	if Has(struct{ A []string }{}) || Has(3) || Has(nil) {
		t.Error("a type holding none is")
	}
}

type promoted struct {
	Tags []string `apitypes:"nonnull"`
}

type embedder struct {
	promoted
	Other []string
}

type embedderPtr struct {
	*promoted
}

func TestViolationsLooksInsideUnexportedEmbeddedStructs(t *testing.T) {
	if got := Violations(embedder{}); !slices.Equal(got, []string{"embedder.promoted.Tags"}) {
		t.Errorf("an embedded struct: %v", got)
	}
	if got := Violations(embedderPtr{promoted: &promoted{}}); !slices.Equal(got, []string{"embedderPtr.promoted.Tags"}) {
		t.Errorf("an embedded pointer: %v", got)
	}
	if got := Violations(embedder{promoted: promoted{Tags: []string{}}}); len(got) != 0 {
		t.Errorf("a kept promise: %v", got)
	}
}
