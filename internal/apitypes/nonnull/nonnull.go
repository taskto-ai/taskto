// Package nonnull is the check behind the `apitypes:"nonnull"` tag: a slice or map field
// with it is told to the web app as `T[]`, never `T[] | null`, which holds only if the server
// never leaves it nil (encoding/json writes a nil slice or map as null). A package's tests
// call Require on what its code makes for the API, and apitypes's own test checks that each
// tagged field has such a test.
package nonnull

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Tag is the option of the `apitypes` struct tag this package checks.
const Tag = "nonnull"

// Violations lists the fields tagged nonnull that are nil anywhere in v (a struct, pointer,
// slice, map or interface holding them), each as a path like Card.Labels.
func Violations(v any) []string {
	var out []string
	walk(reflect.ValueOf(v), "", map[uintptr]bool{}, &out)
	return out
}

// Require fails t for each tagged field of v that is nil, and when v holds no tagged field
// at all (a test that checks nothing).
func Require(t testing.TB, v any) {
	t.Helper()
	for _, p := range Violations(v) {
		t.Errorf("%s is nil: the API would send null where its type says a list or map", p)
	}
	if !Has(v) {
		t.Errorf("%T holds no apitypes:\"nonnull\" field: nothing was checked", v)
	}
}

// Has is whether v's type holds a tagged field, however deep.
func Has(v any) bool {
	return has(reflect.TypeOf(v), map[reflect.Type]bool{})
}

func has(t reflect.Type, seen map[reflect.Type]bool) bool {
	if t == nil || seen[t] {
		return false
	}
	seen[t] = true
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		return has(t.Elem(), seen)
	case reflect.Struct:
		for f := range t.Fields() {
			if tagged(f) || has(f.Type, seen) {
				return true
			}
		}
	}
	return false
}

func tagged(f reflect.StructField) bool {
	for o := range strings.SplitSeq(f.Tag.Get("apitypes"), ",") {
		if o == Tag {
			return true
		}
	}
	return false
}

func walk(v reflect.Value, path string, seen map[uintptr]bool, out *[]string) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() || seen[v.Pointer()] {
			return
		}
		seen[v.Pointer()] = true
		walk(v.Elem(), path, seen, out)
	case reflect.Interface:
		if !v.IsNil() {
			walk(v.Elem(), path, seen, out)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			walk(v.Index(i), fmt.Sprintf("%s[%d]", path, i), seen, out)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			walk(v.MapIndex(k), fmt.Sprintf("%s[%v]", path, k), seen, out)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			f := v.Type().Field(i)
			// encoding/json writes the exported fields of an unexported embedded struct.
			if !f.IsExported() && !f.Anonymous {
				continue
			}
			p := path + "." + f.Name
			if path == "" {
				p = v.Type().Name() + "." + f.Name
			}
			fv := v.Field(i)
			if tagged(f) && (fv.Kind() == reflect.Slice || fv.Kind() == reflect.Map) && fv.IsNil() {
				*out = append(*out, p)
			}
			walk(fv, p, seen, out)
		}
	}
}
