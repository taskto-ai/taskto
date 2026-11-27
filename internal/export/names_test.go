package export

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCleanElemMakesNamesSafeAnywhere(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plan.md", "plan.md"},
		{"..", "_"},
		{".", "_"},
		{"", "_"},
		{"   ", "_"},
		{"a/b", "a_b"},
		{`a\b`, "a_b"},
		{"a:b*c?d\"e<f>g|h", "a_b_c_d_e_f_g_h"},
		{"trailing. . ", "trailing"},
		{"nul", "_nul"},
		{"CON.txt", "_CON.txt"},
		{"com1", "_com1"},
		{"console.txt", "console.txt"},
		{"tab\there\nnl\x00nul", "tabherenlnul"},
		{"bidi\u202etxt.exe", "biditxt.exe"},
		{"résumé ☕.md", "résumé ☕.md"},
		{"bad\xffutf8", "badutf8"},
		{".bashrc", ".bashrc"},
		{"COM¹.md", "_COM¹.md"},
		{"lpt³", "_lpt³"},
		{strings.Repeat(".", 120) + "abcdefghijklmnop", "_"},
		{"CON" + strings.Repeat(" ", 117) + "x", "_CON"},
	} {
		if got := cleanElem(tc.in, maxElemBytes); got != tc.want {
			t.Errorf("cleanElem(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCleanElemShortensAtACharacterAndKeepsTheExtension(t *testing.T) {
	long := strings.Repeat("é", 200) + ".markdown"
	got := cleanElem(long, maxElemBytes)
	if len(got) > maxElemBytes || !strings.HasSuffix(got, ".markdown") || !utf8.ValidString(got) {
		t.Errorf("cleanElem(long) = %q (%d bytes)", got, len(got))
	}
	noExt := cleanElem(strings.Repeat("字", 100), 50)
	if len(noExt) > 50 || !utf8.ValidString(noExt) {
		t.Errorf("cleanElem = %q (%d bytes)", noExt, len(noExt))
	}
	if got := cleanElem("a."+strings.Repeat("x", 300), maxElemBytes); len(got) > maxElemBytes {
		t.Errorf("a long extension: %d bytes", len(got))
	}
}

func TestNamesAreUniqueEvenIgnoringCase(t *testing.T) {
	n := newNames()
	got := []string{
		n.claim("documents", "a.md"),
		n.claim("documents", "A.md"),
		n.claim("documents", "a.md"),
		n.claim("documents", "b.md"),
	}
	want := []string{"documents/a.md", "documents/A (2).md", "documents/a (3).md", "documents/b.md"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("claim %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestNamesNeverMakeAFileOfAFolder(t *testing.T) {
	n := newNames()
	file := n.claim("documents", "a_", "x.md")
	clash := n.claim("documents", "a_")
	if file != "documents/a_/x.md" || clash == "documents/a_" {
		t.Errorf("file = %q, clash = %q", file, clash)
	}
	n = newNames()
	n.claim("documents", "a_")
	got := n.claim("documents", "a_", "x.md")
	if strings.HasPrefix(got, "documents/a_/") {
		t.Errorf("a folder over a file: %q", got)
	}
	first := strings.Split(got, "/")[1]
	if a, b := n.claim("documents", "a_", "y.md"), n.claim("documents", "a_", "z.md"); strings.Split(a, "/")[1] != first || strings.Split(b, "/")[1] != first {
		t.Errorf("one folder, three names: %q %q %q", got, a, b)
	}
}

func TestSuffixKeepsWithinTheLimit(t *testing.T) {
	name := cleanElem(strings.Repeat("a", 300)+".md", maxElemBytes)
	n := newNames()
	n.claim("d", name)
	if got := n.claim("d", name); len(got)-len("d/") > maxElemBytes || !strings.HasSuffix(got, " (2).md") {
		t.Errorf("claim = %q", got)
	}
}

func TestCleanElemNeverCutsANameToNothing(t *testing.T) {
	for _, in := range []string{
		strings.Repeat(".", 130) + strings.Repeat("x", 30),
		"." + strings.Repeat("y", 300),
		strings.Repeat(".", 200) + "a.b",
		strings.Repeat(" .", 100) + "name",
	} {
		got := cleanElem(in, maxElemBytes)
		if got == "" || strings.Trim(got, ". ") == "" || len(got) > maxElemBytes || strings.ContainsAny(got, "/\\") {
			t.Errorf("cleanElem(%.20q… %d bytes) = %q", in, len(in), got)
		}
		n := newNames()
		n.claim("documents", in)
		again := n.claim("documents", in)
		if parts := strings.Split(again, "/"); len(parts) != 2 || parts[1] == "" || len(parts[1]) > maxElemBytes {
			t.Errorf("claim of a repeated %.20q… = %q", in, again)
		}
	}
}

func TestNamesAreUniqueByNormalizationAndCase(t *testing.T) {
	n := newNames()
	nfc, nfd := "café.md", "café.md"
	got := []string{
		n.claim("documents", nfc),
		n.claim("documents", nfd),
		n.claim("documents", "CAFÉ.md"),
		n.claim("documents", "Straße.md"),
		n.claim("documents", "STRASSE.md"),
	}
	seen := map[string]bool{}
	for _, name := range got {
		if key := foldKey(name); seen[key] {
			t.Errorf("%q repeats an earlier name once folded: %v", name, got)
		} else {
			seen[key] = true
		}
	}
}

func TestNamesKeepDistinctFoldersApart(t *testing.T) {
	n := newNames()
	a := n.claim("documents", "a:b", "x.md")
	b := n.claim("documents", "a_b", "x.md")
	c := n.claim("documents", "A_B", "x.md")
	again := n.claim("documents", "a:b", "y.md")
	nfd := n.claim("documents", "é", "x.md")
	nfc := n.claim("documents", "é", "x.md")
	folder := func(s string) string { return strings.Split(s, "/")[1] }
	if folder(a) == folder(b) || folder(a) == folder(c) || folder(b) == folder(c) || folder(nfd) == folder(nfc) {
		t.Errorf("distinct folders were merged: %q %q %q, %q %q", a, b, c, nfd, nfc)
	}
	if folder(a) != folder(again) {
		t.Errorf("one folder, two names: %q and %q", a, again)
	}
	seen := map[string]bool{}
	for _, name := range []string{a, b, c, again, nfd, nfc} {
		if k := foldKey(name); seen[k] {
			t.Errorf("entry %q repeats another once folded", name)
		}
		seen[foldKey(name)] = true
	}
}
