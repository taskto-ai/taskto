package dictation

import (
	"slices"
	"testing"
)

func TestParseGlossary(t *testing.T) {
	md := `---
title: Glossary
- not a term: front matter
---
# Glossary

Words we use. A paragraph isn't a term.

- **taskto**: the product
- Acme — the company
* ` + "`Parakeet`" + ` - the speech model
+ [Desert Ant](https://desertant.com): a vendor
1. Zanzibar
2) São Paulo: a city
- [ ] Wi-Fi
- : nothing before the colon
- ` + "```" + `
- a term that is far too long to be a glossary term, longer than sixty characters in all

` + "```" + `
- inside a fence
` + "```" + `
`
	got := ParseGlossary([]byte(md))
	want := []string{"taskto", "Acme", "Parakeet", "Desert Ant", "Zanzibar", "São Paulo", "Wi-Fi"}
	if !slices.Equal(got, want) {
		t.Fatalf("terms = %q, want %q", got, want)
	}
}

func TestCorrect(t *testing.T) {
	terms := []string{"taskto", "Zanzibar", "Parakeet", "Go", "São Paulo", "Wurst", "Kubernetes cluster", "Node.js"}
	for _, tc := range []struct{ in, want string }{
		{"I use task to every day.", "I use taskto every day."},
		{"Taskto is great", "taskto is great"},
		{"we went to sao paulo, then home", "we went to São Paulo, then home"},
		{"a trip to Zansibar", "a trip to Zanzibar"},
		{"the parakeat model", "the Parakeet model"},
		{"the parrakeat model", "the parrakeat model"},
		{"let's go now", "let's go now"},
		{"the wost case", "the wost case"},
		{"some wurst please", "some Wurst please"},
		{"deploy to the kubernetis cluster today", "deploy to the Kubernetes cluster today"},
		{"task, to do", "task, to do"},
		{"written in node.js", "written in Node.js"},
		{"", ""},
	} {
		if got := Correct(tc.in, terms); got != tc.want {
			t.Errorf("Correct(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := Correct("task to", nil); got != "task to" {
		t.Fatalf("without terms: %q", got)
	}
}

func TestDistance(t *testing.T) {
	for _, tc := range []struct {
		a, b  string
		limit int
		want  int
	}{
		{"kitten", "sitting", 5, 3},
		{"kitten", "sitting", 1, 2},
		{"abc", "abc", 0, 0},
		{"abc", "abcdef", 2, 3},
	} {
		if got := distance([]rune(tc.a), []rune(tc.b), tc.limit); got != tc.want {
			t.Errorf("distance(%q, %q, %d) = %d, want %d", tc.a, tc.b, tc.limit, got, tc.want)
		}
	}
}
