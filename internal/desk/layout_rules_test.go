package desk

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func reasonOf(t *testing.T, err error) string {
	t.Helper()
	var bad *layoutInvalid
	if !errors.As(err, &bad) {
		t.Fatalf("err = %v, want a layout refusal", err)
	}
	return bad.reason
}

func rawGroup(id, active string, tabs ...string) string {
	return fmt.Sprintf(`{"group":%q,"active":%q,"tabs":[%s]}`, id, active, strings.Join(tabs, ","))
}

func rawTab(id, path string) string { return fmt.Sprintf(`{"id":%q,"path":%q}`, id, path) }

func rawLayout(focus, root string) []byte {
	return []byte(fmt.Sprintf(`{"v":1,"focus":%q,"root":%s}`, focus, root))
}

func rawSplit(sizes string, children ...string) string {
	return fmt.Sprintf(`{"split":"row","sizes":%s,"children":[%s]}`, sizes, strings.Join(children, ","))
}

func nodeGroup(id, active string, tabs ...tab) *node {
	return &node{Group: id, Active: active, Tabs: tabs}
}

func nodeSplit(children ...*node) *node {
	sizes := make([]float64, len(children))
	for i := range sizes {
		sizes[i] = 1 / float64(len(children))
	}
	return &node{Split: "row", Sizes: sizes, Children: children}
}

func encoded(t *testing.T, focus string, root *node) []byte {
	t.Helper()
	raw, err := json.Marshal(layout{V: 1, Focus: focus, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCheckAcceptsEveryLimitAtItsEdge(t *testing.T) {
	home := "/w/12/home"
	oneTab := func(n int) []tab { return []tab{{ID: fmt.Sprintf("t%d", n), Path: home}} }
	sixGroups := nodeSplit(
		nodeGroup("g1", "t1", oneTab(1)...),
		nodeGroup("g2", "t2", oneTab(2)...),
		nodeGroup("g3", "t3", oneTab(3)...),
		nodeSplit(nodeGroup("g4", "t4", oneTab(4)...), nodeGroup("g5", "t5", oneTab(5)...), nodeGroup("g6", "t6", oneTab(6)...)),
	)
	sixtyTabs := nodeSplit(
		nodeGroup("g1", "t1", makeTabs(20, home)...),
		nodeGroup("g2", "t21", makeTabsFrom(21, 20, home)...),
		nodeGroup("g3", "t41", makeTabsFrom(41, 20, home)...),
	)
	longIDs := nodeGroup("g12345678901234", "abcdefghij123456", tab{ID: "abcdefghij123456", Path: home})
	longPath := nodeGroup("g1", "t1", tab{ID: "t1", Path: home + "/" + strings.Repeat("a", 501)})
	extremeSizes := &node{Split: "column", Sizes: []float64{0.1, 0.9}, Children: []*node{nodeGroup("g1", "t1", oneTab(1)...), nodeGroup("g2", "t2", oneTab(2)...)}}
	for name, c := range map[string]struct {
		focus string
		root  *node
	}{
		"six groups at three levels": {"g1", sixGroups},
		"sixty tabs":                 {"g1", sixtyTabs},
		"sixteen character ids":      {"g12345678901234", longIDs},
		"a path of 512 bytes":        {"g1", longPath},
		"sizes at 0.1 and 0.9":       {"g1", extremeSizes},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Check(12, encoded(t, c.focus, c.root)); err != nil {
				t.Fatalf("Check refused a layout at the limit: %v", err)
			}
		})
	}
	valid := rawLayout("g1", rawGroup("g1", "t1", rawTab("t1", home)))
	atCap := append(append([]byte{}, valid...), bytes.Repeat([]byte(" "), maxLayoutBytes-len(valid))...)
	if len(atCap) != maxLayoutBytes {
		t.Fatalf("padded layout is %d bytes", len(atCap))
	}
	if _, err := Check(12, atCap); err != nil {
		t.Fatalf("Check refused a layout of exactly the cap: %v", err)
	}
}

func TestCheckNamesTheRuleABadLayoutBroke(t *testing.T) {
	home := "/w/12/home"
	tab1 := rawTab("t1", home)
	valid := rawLayout("g1", rawGroup("g1", "t1", tab1))
	overCap := append(append([]byte{}, valid...), bytes.Repeat([]byte(" "), maxLayoutBytes+1-len(valid))...)
	tabsOf := func(from, count int) []string {
		out := make([]string, 0, count)
		for i := from; i < from+count; i++ {
			out = append(out, rawTab(fmt.Sprintf("t%d", i), home))
		}
		return out
	}
	twoGroups := func(a, b string) []byte {
		return rawLayout("g1", rawSplit("[0.5,0.5]", a, b))
	}
	sixtyOne := rawLayout("g1", rawSplit("[0.25,0.25,0.25,0.25]",
		rawGroup("g1", "t1", tabsOf(1, 20)...),
		rawGroup("g2", "t21", tabsOf(21, 20)...),
		rawGroup("g3", "t41", tabsOf(41, 20)...),
		rawGroup("g4", "t61", tabsOf(61, 1)...),
	))
	cases := []struct {
		name   string
		raw    []byte
		reason string
	}{
		{"valid layout padded one byte over the cap", overCap, "too_large"},
		{"61 tabs in all", sixtyOne, "too_many_tabs"},
		{"21 tabs in a group", rawLayout("g1", rawGroup("g1", "t1", tabsOf(1, 21)...)), "too_many_tabs"},
		{"tab id with a capital", rawLayout("g1", rawGroup("g1", "T1", rawTab("T1", home))), "invalid_id"},
		{"tab id with a dash", rawLayout("g1", rawGroup("g1", "t-1", rawTab("t-1", home))), "invalid_id"},
		{"tab id of 17 characters", rawLayout("g1", rawGroup("g1", "t1", rawTab("abcdefghij1234567", home))), "invalid_id"},
		{"empty tab id", rawLayout("g1", rawGroup("g1", "", rawTab("", home))), "invalid_id"},
		{"group id with a capital", rawLayout("G1", rawGroup("G1", "t1", tab1)), "invalid_id"},
		{"tab id equal to a group id", rawLayout("g1", rawGroup("g1", "g1", rawTab("g1", home))), "invalid_id"},
		{"focus names no group", rawLayout("g9", rawGroup("g1", "t1", tab1)), "focus"},
		{"focus names a tab", rawLayout("t1", rawGroup("g1", "t1", tab1)), "focus"},
		{"empty focus with a root", rawLayout("", rawGroup("g1", "t1", tab1)), "focus"},
		{"a focus with no root", []byte(`{"v":1,"focus":"g1","root":null}`), "focus"},
		{"active names no tab", rawLayout("g1", rawGroup("g1", "t9", tab1)), "active"},
		{"active names another group's tab", twoGroups(rawGroup("g1", "t2", tab1), rawGroup("g2", "t2", rawTab("t2", home))), "active"},
		{"no root key", []byte(`{"v":1,"focus":""}`), "invalid_json"},
		{"no focus key", []byte(`{"v":1,"root":null}`), "invalid_json"},
		{"no version", []byte(`{"focus":"","root":null}`), "version"},
		{"split with a group field", rawLayout("g1", `{"split":"row","group":"g9","sizes":[0.5,0.5],"children":[`+rawGroup("g1", "t1", tab1)+`,`+rawGroup("g2", "t2", rawTab("t2", home))+`]}`), "invalid_node"},
		{"unknown split", rawLayout("g1", `{"split":"diagonal","sizes":[0.5,0.5],"children":[`+rawGroup("g1", "t1", tab1)+`,`+rawGroup("g2", "t2", rawTab("t2", home))+`]}`), "invalid_node"},
		{"split of one child", rawLayout("g1", rawSplit("[1]", rawGroup("g1", "t1", tab1))), "invalid_split"},
		{"fewer sizes than children", rawLayout("g1", rawSplit("[0.5]", rawGroup("g1", "t1", tab1), rawGroup("g2", "t2", rawTab("t2", home)))), "invalid_split"},
		{"sizes below one", rawLayout("g1", rawSplit("[0.4,0.4]", rawGroup("g1", "t1", tab1), rawGroup("g2", "t2", rawTab("t2", home)))), "invalid_sizes"},
		{"size over 0.9", rawLayout("g1", rawSplit("[0.95,0.05]", rawGroup("g1", "t1", tab1), rawGroup("g2", "t2", rawTab("t2", home)))), "invalid_sizes"},
		{"group with no tabs", rawLayout("g1", `{"group":"g1","active":"t1","tabs":[]}`), "invalid_group"},
		{"path of another workspace", rawLayout("g1", rawGroup("g1", "t1", rawTab("t1", "/w/13/home"))), "invalid_path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Check(12, tc.raw)
			if err == nil {
				t.Fatal("Check accepted the layout")
			}
			if got := reasonOf(t, err); got != tc.reason {
				t.Fatalf("reason = %q, want %q", got, tc.reason)
			}
		})
	}
}

func TestCheckKeepsEveryRouteInsideTheWorkspaceAndItsFirstSegment(t *testing.T) {
	check := func(path string) error {
		raw, err := json.Marshal(layout{V: 1, Focus: "g1", Root: nodeGroup("g1", "t1", tab{ID: "t1", Path: path})})
		if err != nil {
			t.Fatal(err)
		}
		_, err = Check(12, raw)
		return err
	}
	for _, path := range []string{
		"/w/12/homeevil", "/w/12/boardsx/1", "/w/12/home/", "/w/12/home//", "/w/12", "/w/12/", "/w/12//home",
		"/w/120/home", "/w/1/home", "/w/012/home", "/w/13/home", "/w/12/Home", "/w/12/unknown", "/w/12/unknown/1",
		"w/12/home", "/x/12/home", "https://example.test/w/12/home", "//example.test/w/12/home", "/w/12/home/one/two",
	} {
		if err := check(path); err == nil {
			t.Errorf("Check accepted %q", path)
		} else if got := reasonOf(t, err); got != "invalid_path" {
			t.Errorf("%q refused for %q", path, got)
		}
	}
	for _, path := range []string{
		"/w/12/home", "/w/12/conversations", "/w/12/knowledge", "/w/12/boards", "/w/12/apps", "/w/12/notifications", "/w/12/settings",
		"/w/12/conversations/9", "/w/12/settings/members", "/w/12/knowledge/3:notes.md", "/w/12/apps/a%3Fb", "/w/12/apps/a%23b",
	} {
		if err := check(path); err != nil {
			t.Errorf("Check refused %q: %v", path, err)
		}
	}
}

func TestCheckAcceptsAStoredLayoutOfExactlyTheCapAndRefusesOneByteMore(t *testing.T) {
	tabID := func(n int) string { return fmt.Sprintf("t%015d", n) }
	build := func(extra int) []byte {
		groups := make([]*node, 0, 3)
		for g := 0; g < 3; g++ {
			tabs := make([]tab, 0, 20)
			for i := 0; i < 20; i++ {
				tabs = append(tabs, tab{ID: tabID(g*20 + i + 1), Path: "/w/12/home/" + strings.Repeat("a", 460)})
			}
			groups = append(groups, nodeGroup(fmt.Sprintf("g%d", g+1), tabID(g*20+1), tabs...))
		}
		root := nodeSplit(groups...)
		for i := 0; i < extra; i++ {
			tabs := root.Children[i%3].Tabs
			tabs[(i/3)%20].Path += "a"
		}
		return encoded(t, "g1", root)
	}
	base := len(build(0))
	pad := maxLayoutBytes - base
	if pad < 0 || pad > 60*40 {
		t.Fatalf("test layout is %d bytes, cannot be padded to the cap", base)
	}
	exact := build(pad)
	if len(exact) != maxLayoutBytes {
		t.Fatalf("layout is %d bytes, want %d", len(exact), maxLayoutBytes)
	}
	if _, err := Check(12, exact); err != nil {
		t.Fatalf("Check refused a stored layout of exactly the cap: %v", err)
	}
	_, err := Check(12, build(pad+1))
	if got := reasonOf(t, err); got != "too_large" {
		t.Fatalf("a layout one byte over the cap was refused for %q", got)
	}
}
