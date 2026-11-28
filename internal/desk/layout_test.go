package desk

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestCheckAcceptsAndNormalizesLayouts(t *testing.T) {
	raw := []byte(` { "root" : { "tabs" : [{"path":"/w/12/conversations/4","id":"t1"}], "active":"t1", "group":"g1" }, "focus":"g1", "v":1 } `)
	got, err := Check(12, raw)
	if err != nil {
		t.Fatal(err)
	}
	var value layout
	if err := json.Unmarshal(got, &value); err != nil || value.V != 1 || value.Focus != "g1" || value.Root == nil || value.Root.Active != "t1" {
		t.Fatalf("normalized layout %s, err %v", got, err)
	}
	if string(got) == string(raw) {
		t.Fatal("layout was not normalized")
	}
	if empty, err := Check(12, []byte(`{"v":1,"focus":"","root":null}`)); err != nil || string(empty) != `{"v":1,"focus":"","root":null}` {
		t.Fatalf("empty layout %s, err %v", empty, err)
	}
}

func TestCheckRejectsInvalidLayouts(t *testing.T) {
	validTab := func(id, path string) tab { return tab{ID: id, Path: path} }
	group := func(id, active string, tabs ...tab) *node { return &node{Group: id, Active: active, Tabs: tabs} }
	split := func(children ...*node) *node {
		sizes := make([]float64, len(children))
		for i := range sizes {
			sizes[i] = 1 / float64(len(children))
		}
		return &node{Split: "row", Sizes: sizes, Children: children}
	}
	base := layout{V: 1, Focus: "g1", Root: group("g1", "t1", validTab("t1", "/w/12/home"))}
	tooDeep := split(
		group("g1", "t1", validTab("t1", "/w/12/home")),
		split(
			group("g2", "t2", validTab("t2", "/w/12/home")),
			split(group("g3", "t3", validTab("t3", "/w/12/home")), group("g4", "t4", validTab("t4", "/w/12/home"))),
		),
	)
	tooManyGroups := split(
		group("g1", "t1", validTab("t1", "/w/12/home")),
		group("g2", "t2", validTab("t2", "/w/12/home")),
		group("g3", "t3", validTab("t3", "/w/12/home")),
		split(
			group("g4", "t4", validTab("t4", "/w/12/home")),
			group("g5", "t5", validTab("t5", "/w/12/home")),
			group("g6", "t6", validTab("t6", "/w/12/home")),
			group("g7", "t7", validTab("t7", "/w/12/home")),
		),
	)
	tooManyTabs := group("g1", "t1", makeTabs(21, "/w/12/home")...)
	tooManyTotal := split(group("g1", "t1", makeTabs(16, "/w/12/home")...), group("g2", "t17", makeTabsFrom(17, 16, "/w/12/home")...), group("g3", "t33", makeTabsFrom(33, 16, "/w/12/home")...), group("g4", "t49", makeTabsFrom(49, 16, "/w/12/home")...))
	tooManyTotal.Sizes = []float64{0.25, 0.25, 0.25, 0.25}
	tooManyChildren := split(
		group("g1", "t1", validTab("t1", "/w/12/home")),
		group("g2", "t2", validTab("t2", "/w/12/home")),
		group("g3", "t3", validTab("t3", "/w/12/home")),
		group("g4", "t4", validTab("t4", "/w/12/home")),
		group("g5", "t5", validTab("t5", "/w/12/home")),
	)
	badSizes := split(group("g1", "t1", validTab("t1", "/w/12/home")), group("g2", "t2", validTab("t2", "/w/12/home")))
	badSizes.Sizes = []float64{0.4, 0.4}
	badSizeRange := split(group("g1", "t1", validTab("t1", "/w/12/home")), group("g2", "t2", validTab("t2", "/w/12/home")))
	badSizeRange.Sizes = []float64{0.95, 0.05}
	duplicate := split(group("g1", "t1", validTab("t1", "/w/12/home")), group("g2", "t1", validTab("t1", "/w/12/home")))
	duplicate.Sizes = []float64{0.5, 0.5}
	badID := layout{V: 1, Focus: "bad-id", Root: group("bad-id", "t1", validTab("t1", "/w/12/home"))}
	badPath := layout{V: 1, Focus: "g1", Root: group("g1", "t1", validTab("t1", "/w/13/home"))}
	longPath := layout{V: 1, Focus: "g1", Root: group("g1", "t1", validTab("t1", "/w/12/home/"+strings.Repeat("a", 502)))}
	longID := layout{V: 1, Focus: "g12345678901234567", Root: group("g12345678901234567", "t1", validTab("t1", "/w/12/home"))}
	unknown := []byte(`{"v":1,"focus":"","root":null,"extra":true}`)
	deepJSON, _ := json.Marshal(layout{V: 1, Focus: "g1", Root: tooDeep})
	groupJSON, _ := json.Marshal(layout{V: 1, Focus: "g1", Root: tooManyGroups})
	tabJSON, _ := json.Marshal(layout{V: 1, Focus: "g1", Root: tooManyTabs})
	totalJSON, _ := json.Marshal(layout{V: 1, Focus: "g1", Root: tooManyTotal})
	sizeJSON, _ := json.Marshal(layout{V: 1, Focus: "g1", Root: badSizes})
	sizeRangeJSON, _ := json.Marshal(layout{V: 1, Focus: "g1", Root: badSizeRange})
	childrenJSON, _ := json.Marshal(layout{V: 1, Focus: "g1", Root: tooManyChildren})
	duplicateJSON, _ := json.Marshal(layout{V: 1, Focus: "g1", Root: duplicate})
	badIDJSON, _ := json.Marshal(badID)
	badPathJSON, _ := json.Marshal(badPath)
	longPathJSON, _ := json.Marshal(longPath)
	longIDJSON, _ := json.Marshal(longID)
	baseJSON, _ := json.Marshal(base)
	tooLarge := []byte(strings.Repeat(" ", maxLayoutBytes+1))
	cases := []struct {
		name string
		raw  []byte
	}{
		{name: "too large", raw: tooLarge},
		{name: "unknown field", raw: unknown},
		{name: "too deep", raw: deepJSON},
		{name: "too many groups", raw: groupJSON},
		{name: "too many tabs per group", raw: tabJSON},
		{name: "too many tabs overall", raw: totalJSON},
		{name: "too many split children", raw: childrenJSON},
		{name: "bad sizes", raw: sizeJSON},
		{name: "size range", raw: sizeRangeJSON},
		{name: "duplicate ids", raw: duplicateJSON},
		{name: "bad id", raw: badIDJSON},
		{name: "another workspace", raw: badPathJSON},
		{name: "path too long", raw: longPathJSON},
		{name: "id too long", raw: longIDJSON},
		{name: "invalid version", raw: []byte(`{"v":2,"focus":"","root":null}`)},
		{name: "unclosed json", raw: []byte(`{"v":1`)},
		{name: "trailing json", raw: append(baseJSON, []byte(` {}`)...)},
		{name: "invalid route", raw: []byte(`{"v":1,"focus":"g1","root":{"group":"g1","active":"t1","tabs":[{"id":"t1","path":"/w/12/home/one/two"}]}}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Check(12, tc.raw); err == nil {
				t.Fatal("Check accepted invalid layout")
			}
		})
	}
}

func TestCheckRefusesPathsThatLeaveTheirRoute(t *testing.T) {
	for _, path := range []string{
		`/w/12/apps/..\..\13\home`,
		`/w/12/apps/a\b`,
		"/w/12/apps/..",
		"/w/12/apps/.",
		"/w/12/apps/%2e%2e",
		"/w/12/apps/%2E",
		"/w/12/apps/a%2Fb",
		"/w/12/apps/a%5Cb",
		"/w/12/apps/a?b",
		"/w/12/apps/a#b",
		"/w/12/apps/a\nb",
		"/w/12/apps/%zz",
	} {
		raw, err := json.Marshal(layout{V: 1, Focus: "g1", Root: &node{Group: "g1", Active: "t1", Tabs: []tab{{ID: "t1", Path: path}}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Check(12, raw); err == nil {
			t.Fatalf("Check accepted %q", path)
		}
	}
	for _, path := range []string{"/w/12/knowledge/3:notes.md", "/w/12/knowledge/3:my%20notes.md", "/w/12/boards/4"} {
		raw, err := json.Marshal(layout{V: 1, Focus: "g1", Root: &node{Group: "g1", Active: "t1", Tabs: []tab{{ID: "t1", Path: path}}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Check(12, raw); err != nil {
			t.Fatalf("Check refused %q: %v", path, err)
		}
	}
}

func TestCheckAcceptsTheKnowledgePathsTheWebAppBuilds(t *testing.T) {
	check := func(path string) error {
		raw, err := json.Marshal(layout{V: 1, Focus: "g1", Root: &node{Group: "g1", Active: "t1", Tabs: []tab{{ID: "t1", Path: path}}}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = Check(12, raw)
		return err
	}
	for _, path := range []string{
		"/w/12/knowledge/3%3Anotes.md",
		"/w/12/knowledge/3%3Afolder%2Fnotes.md",
		"/w/12/knowledge/3%3Aa%2Fb%2Fc%2Fmy%20notes.md",
		"/w/12/knowledge/3%3Afolder%2Fa%3Fb%23c.md",
		"/w/12/knowledge/3%3A",
		"/w/12/boards/1%3A2%3A3",
	} {
		if err := check(path); err != nil {
			t.Fatalf("Check refused %q: %v", path, err)
		}
	}
	for _, path := range []string{
		"/w/12/knowledge/3%3A..%2Fx",
		"/w/12/knowledge/3%3Afolder%2F..%2Fx",
		"/w/12/knowledge/3%3Afolder%2F..",
		"/w/12/knowledge/3%3Afolder%2F.%2Fx",
		"/w/12/knowledge/%2F..",
		"/w/12/knowledge/%2Fx",
		"/w/12/knowledge/3%3A%2Fx",
		"/w/12/knowledge/3%3Afolder%2F%2Fx",
		"/w/12/knowledge/3%3Afolder%2F",
		"/w/12/knowledge/3%3Afolder%5Cx",
		"/w/12/knowledge/3%3Afolder%2Fx%0Ay",
		"/w/12/knowledge/3%3Afolder/x",
		"/w/12/boards/1%3A2%2F3",
		"/w/12/apps/a%2Fb",
	} {
		if err := check(path); err == nil {
			t.Fatalf("Check accepted %q", path)
		}
	}
}

func TestCheckLimitsTheStoredSizeToo(t *testing.T) {
	path := "/w/12/home/" + strings.Repeat("&", 490)
	raw, err := json.Marshal(layout{V: 1, Focus: "g1", Root: &node{Group: "g1", Active: "t1", Tabs: makeTabs(12, path)}})
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.ReplaceAll(raw, []byte("\\"+"u0026"), []byte("&"))
	if len(raw) > maxLayoutBytes {
		t.Fatalf("input is %d bytes, over the limit already", len(raw))
	}
	_, err = Check(12, raw)
	var bad *layoutInvalid
	if !errors.As(err, &bad) || bad.reason != "too_large" {
		t.Fatalf("Check err = %v", err)
	}
}

func makeTabs(count int, path string) []tab { return makeTabsFrom(1, count, path) }

func makeTabsFrom(start, count int, path string) []tab {
	tabs := make([]tab, 0, count)
	for i := start; i < start+count; i++ {
		tabs = append(tabs, tab{ID: fmt.Sprintf("t%d", i), Path: path})
	}
	return tabs
}
