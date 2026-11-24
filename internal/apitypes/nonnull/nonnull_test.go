package nonnull

import (
	"slices"
	"testing"
)

type leaf struct {
	Tags  []string       `json:"tags" apitypes:"nonnull"`
	Free  map[string]any `json:"free" apitypes:"json,nonnull"`
	Loose []string       `json:"loose"` // not promised: nil is fine
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
	// A nil pointer or interface holds nothing to check; a cycle ends.
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
