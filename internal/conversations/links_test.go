package conversations

import (
	"fmt"
	"slices"
	"testing"
)

func TestLinkedConversationsInAMessage(t *testing.T) {
	s := New(nil, nil)
	s.SetPublicURL("https://taskto.example")
	cases := []struct {
		name    string
		content string
		want    []int64
	}{
		{"relative", "continue from /w/1/conversations/42 please", []int64{42}},
		{"relative at the start", "/w/1/conversations/42", []int64{42}},
		{"absolute on the public URL", "see https://taskto.example/w/1/conversations/42", []int64{42}},
		{"absolute in different case", "see HTTPS://TASKTO.EXAMPLE/w/1/conversations/42", []int64{42}},
		{"in a Markdown link", "[that](https://taskto.example/w/1/conversations/42) one", []int64{42}},
		{"with a query and an anchor", "https://taskto.example/w/1/conversations/42?x=1#top", []int64{42}},
		{"another host", "see https://evil.example/w/1/conversations/42", nil},
		{"the host with a login in front of another", "https://taskto.example@evil.example/w/1/conversations/42", nil},
		{"the pattern in another site's path", "https://evil.example/a/w/1/conversations/42", nil},
		{"the pattern inside a longer path", "see docs/w/1/conversations/42", nil},
		{"the public host as a prefix of another", "https://taskto.example.evil.example/w/1/conversations/42", nil},
		{"another port", "https://taskto.example:8443/w/1/conversations/42", nil},
		{"another scheme", "http://taskto.example/w/1/conversations/42", nil},
		{"another workspace", "/w/2/conversations/42", nil},
		{"the conversation itself", "/w/1/conversations/7", nil},
		{"duplicates collapse", "/w/1/conversations/42 and /w/1/conversations/042 and /w/1/conversations/42", []int64{42}},
		{"at most three, in order", "/w/1/conversations/5 /w/1/conversations/4 /w/1/conversations/3 /w/1/conversations/2", []int64{5, 4, 3}},
		{"an id with letters after it", "/w/1/conversations/42abc", nil},
		{"an id too large for a number", "/w/1/conversations/99999999999999999999", nil},
		{"another section", "/w/1/documents/42", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := s.linkedIn(c.content, 1, 7); !slices.Equal(got, c.want) {
				t.Fatalf("linkedIn(%q) = %v, want %v", c.content, got, c.want)
			}
		})
	}
}

func TestWithNoPublicURLOnlyRelativeLinksCount(t *testing.T) {
	s := New(nil, nil)
	for _, origin := range []string{"", "not a url", "ftp://taskto.example", "https://user@taskto.example"} {
		s.SetPublicURL(origin)
		if got := s.linkedIn("https://taskto.example/w/1/conversations/42 and /w/1/conversations/43", 1, 7); !slices.Equal(got, []int64{43}) {
			t.Fatalf("with public URL %q the links were %v, want only the relative one", origin, got)
		}
	}
}

func TestATailKeepsWholeLines(t *testing.T) {
	text := "first line\nsecond line\nthird line\n"
	cases := []struct {
		limit int
		want  string
	}{
		{100, text},
		{len("third line\n"), "third line\n"},
		{len("third line\n") + 3, "third line\n"},
		{len("second line\nthird line\n"), "second line\nthird line\n"},
	}
	for _, c := range cases {
		if got := tail(text, c.limit); got != c.want {
			t.Errorf("tail(limit %d) = %q, want %q", c.limit, got, c.want)
		}
	}
	long := "é" + fmt.Sprintf("%0100d", 0)
	if got := tail(long, 101); got != fmt.Sprintf("%0100d", 0) {
		t.Fatalf("a cut through a character kept %q", got)
	}
}
