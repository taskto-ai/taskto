package nonnull

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const Tag = "nonnull"

func Violations(v any) []string {
	var out []string
	walk(reflect.ValueOf(v), "", map[uintptr]bool{}, &out)
	return out
}

func Require(t testing.TB, v any) {
	t.Helper()
	for _, p := range Violations(v) {
		t.Errorf("%s is nil: the API would send null where its type says a list or map", p)
	}
	if !Has(v) {
		t.Errorf("%T holds no apitypes:\"nonnull\" field: nothing was checked", v)
	}
}

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
