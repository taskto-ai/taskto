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
	// An "extension" that is most of the name isn't kept whole.
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
	file := n.claim("documents", "a_", "x.md") // a folder a_
	clash := n.claim("documents", "a_")        // then a file of the same name
	if file != "documents/a_/x.md" || clash == "documents/a_" {
		t.Errorf("file = %q, clash = %q", file, clash)
	}
	n = newNames()
	n.claim("documents", "a_")
	if got := n.claim("documents", "a_", "x.md"); strings.HasPrefix(got, "documents/a_/") {
		t.Errorf("a folder over a file: %q", got)
	}
	// The folder keeps its new name for the files that follow.
	if a, b := n.claim("documents", "a_", "y.md"), n.claim("documents", "a_", "z.md"); strings.Split(a, "/")[1] != strings.Split(b, "/")[1] {
		t.Errorf("one folder, two names: %q %q", a, b)
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
