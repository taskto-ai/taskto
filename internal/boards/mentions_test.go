package boards

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/auth"
)

func TestMentionsReadsOnlyTheTokenWithAnID(t *testing.T) {
	for _, c := range []struct {
		body string
		want []int64
	}{
		{"nothing here", nil},
		{"hi @[Ana](user:12)", []int64{12}},
		{"@[Ana](user:12) and @[Bo, Jr.](user:7) and again @[Ana](user:12)", []int64{12, 7}},
		{"@[Renamed](user:12)", []int64{12}},
		{"@Ana @ana@example.com [Ana](user:12) @[Ana](12) @[Ana](user:) @[Ana](user:x1)", nil},
		{"@[Ana](user:0) @[Ana](user:99999999999999999999)", nil},
		{"@[multi\nline](user:3) @[](user:4)", nil},
		// A label a Markdown link's label couldn't hold isn't a link.
		{"@[a[b](user:3) @[a\\](user:4)", nil},
	} {
		if got := Mentions(c.body); !slices.Equal(got, c.want) {
			t.Errorf("Mentions(%q) = %v, want %v", c.body, got, c.want)
		}
	}
	// A comment tells a bounded number of people.
	var b strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&b, "@[x](user:%d) ", i)
	}
	if got := Mentions(b.String()); len(got) != maxMentions {
		t.Errorf("%d mentions", len(got))
	}
}

func TestMentionsInCodeOrEscapedTellNobody(t *testing.T) {
	const tok, tok2 = "@[Ana](user:12)", "@[Bo](user:7)"
	for _, c := range []struct {
		name string
		body string
		want []int64
	}{
		{"fenced with backticks", "```\n" + tok + "\n```", nil},
		{"fenced with a language", "text\n```go\n" + tok + "\n```\nthen " + tok + " 2", []int64{12}},
		{"fenced with tildes", "~~~\n" + tok + "\n~~~", nil},
		{"a longer fence holds a shorter one", "````\n```\n" + tok + "\n```\n````", nil},
		{"a fence the comment never closes", "```\n" + tok + "\nmore", nil},
		{"an indented fence", "  ```\n" + tok + "\n  ```\n@[Bo](user:7)", []int64{7}},
		{"a fence with windows line ends", "```\r\n" + tok + "\r\n```\r\n@[Bo](user:7)", []int64{7}},
		{"after a closed fence", "```\ncode\n```\n" + tok, []int64{12}},
		{"inline code", "see `" + tok + "` here", nil},
		{"inline code with a double fence", "see ``a ` " + tok + "`` here", nil},
		{"inline code over two lines", "see `a\n" + tok + "` here", nil},
		{"code before the mention", "`x` " + tok, []int64{12}},
		{"a backtick that never closes", "it`s " + tok, []int64{12}},
		{"code that ends before a blank line", "`open\n\n" + tok + "`", []int64{12}},
		{"an escaped at sign", "\\" + tok, nil},
		{"an escaped bracket", "@\\[Ana](user:12)", nil},
		{"an escaped backslash isn't an escape", "\\\\" + tok, []int64{12}},
		{"an escaped backtick opens no code", "\\`" + tok + "\\`", []int64{12}},
		{"a backslash before a letter is text", "\\a " + tok, []int64{12}},
		{"a quoted fence", "> ~~~\n> " + tok + "\n> ~~~", nil},
		{"a quoted fence ends with the quote", "> ```\n> code\n\n" + tok, []int64{12}},
		{"an unprefixed line stays in a quoted fence", "> ~~~\n> code\n" + tok, nil},
		{"a fence ends where its list item does", "- ~~~\n  code\n- " + tok2, []int64{7}},
		{"a list marker in a fence is code", "~~~\n- ~~~\n" + tok2 + "\n~~~", nil},
		{"a heading", "# Hello " + tok, []int64{12}},
		{"a heading in code", "```\n# " + tok + "\n```", nil},
		{"a table cell", "| a | b |\n|---|---|\n| " + tok + " | `" + tok2 + "` |", []int64{12}},
		{"a fence in a list item", "- ```\n  " + tok + "\n  ```\n- " + tok2, []int64{7}},
		{"a fence in a numbered item", "1. ~~~\n   " + tok + "\n   ~~~", nil},
		{"a quote that is not code", "> " + tok, []int64{12}},
		{"a link's destination", "[doc](https://example.test/" + tok + ")", nil},
		{"a link's destination with a title", "[doc](https://example.test/x?y=" + tok + " \"t\") " + tok2, []int64{7}},
		{"an image's destination", "![pic](https://example.test/" + tok + ")", nil},
		{"a mention in a link's label", "[see " + tok + "](https://example.test/a)", []int64{12}},
		{"a mention in a link's label and one in its destination", "[" + tok + "](https://x.test/" + tok2 + ")", []int64{12}},
		{"an autolink", "<https://example.test/" + tok + ">", nil},
		{"a bare address", "see https://example.test/" + tok + " now", nil},
		{"a token after a bare address", "https://example.test/x " + tok, []int64{12}},
		{"an image's label isn't read", "![See " + tok + "](https://example.test/i.png) " + tok2, []int64{7}},
		{"a table's extra cell isn't shown", "| a | b |\n|---|---|\n| " + tok + " | x | " + tok2 + " |", []int64{12}},
		{"a very long fence closes it", strings.Repeat("`", 1500) + "\n" + tok + "\n" + strings.Repeat("`", 1500) + "\n" + tok2, []int64{7}},
		{"one in code and one outside", "`" + tok + "` @[Bo](user:7)", []int64{7}},
	} {
		if got := Mentions(c.body); !slices.Equal(got, c.want) {
			t.Errorf("%s: Mentions(%q) = %v, want %v", c.name, c.body, got, c.want)
		}
	}
}

// commentTo posts a comment as u on a card of the shared space, over the API.
func (w *world) commentTo(u *user, c Card, body string) Comment {
	w.e.t.Helper()
	rec := u.do(http.MethodPost, cardPath(w.shared, c.BoardID, c.ID, "/comments"), map[string]string{"body": body})
	expect(w.e.t, rec, http.StatusCreated)
	return decode[Comment](w.e.t, rec)
}

func (w *world) notified(u *user) (n int) {
	w.e.t.Helper()
	if err := w.e.db.QueryRow(`SELECT count(*) FROM notifications WHERE user_id = ?`, u.id).Scan(&n); err != nil {
		w.e.t.Fatal(err)
	}
	return n
}

func TestWhoACommentTellsIsDecidedWhenItIsWritten(t *testing.T) {
	w := newWorld(t)
	// The card is Bob's; Alice comments and mentions Carol, and as she does the card goes to Owner
	// and Carol leaves the space: whoever is chosen before the comment's write is wrong by then.
	ctx := context.Background()
	carol := w.e.join(w.owner, w.ws, "Carol", auth.RoleMember)
	w.e.addMember(w.owner, w.shared, carol, access.RoleEditor)
	if _, err := w.e.db.Exec(`UPDATE cards SET assignee_id = ? WHERE id = ?`, w.bob.id, w.teamCard.ID); err != nil {
		t.Fatal(err)
	}
	w.e.hooks.BeforeBegin(func() {
		if _, err := w.e.db.ExecContext(ctx, `UPDATE cards SET assignee_id = ? WHERE id = ?`, w.owner.id, w.teamCard.ID); err != nil {
			t.Error(err)
		}
		if _, err := w.e.db.ExecContext(ctx, `DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, w.shared.ID, carol.id); err != nil {
			t.Error(err)
		}
	})
	w.commentTo(w.alice, w.teamCard, "Over to you, "+fmt.Sprintf("@[Carol](user:%d)", carol.id))
	if got := []int{w.notified(w.bob), w.notified(w.owner), w.notified(carol)}; !slices.Equal(got, []int{0, 1, 0}) {
		t.Fatalf("notifications for bob (was assignee), owner (is assignee), carol (left) = %v, want [0 1 0]", got)
	}
}

func TestACommentNamesWhomItMentionsFromTheirIDForWhoeverMayReadTheSpace(t *testing.T) {
	w := newWorld(t)
	// The label says Alice; the ID is Bob's. The label is only what the writer typed.
	body := fmt.Sprintf("@[Alice](user:%d) and @[Mallory](user:%d) and @[Nobody](user:99999) and @[Owner](user:%d)",
		w.bob.id, w.mallory.id, w.owner.id)
	cm := w.commentTo(w.alice, w.teamCard, body)
	want := []MentionedPerson{{ID: w.bob.id, Name: "Bob"}, {ID: w.owner.id, Name: "Owner"}}
	if !reflect.DeepEqual(cm.Mentions, want) {
		t.Fatalf("the answer's mentions = %+v, want %+v: the label is nobody's name, and who can't read the space isn't named", cm.Mentions, want)
	}
	for _, u := range []*user{w.alice, w.bob} {
		got := w.card(u, w.shared, w.teamCard).Comments
		if len(got) != 1 || !reflect.DeepEqual(got[0].Mentions, want) {
			t.Fatalf("user %d reads %+v", u.id, got)
		}
	}
	// A comment that mentions nobody has an empty list, never null.
	rec := w.alice.do(http.MethodPost, cardPath(w.shared, w.teamCard.BoardID, w.teamCard.ID, "/comments"), map[string]string{"body": "plain"})
	expect(t, rec, http.StatusCreated)
	if !strings.Contains(rec.Body.String(), `"mentions":[]`) {
		t.Fatalf("body = %s", rec.Body)
	}
	// Code isn't a mention: no name for it either.
	cm = w.commentTo(w.alice, w.teamCard, "`"+fmt.Sprintf("@[Bob](user:%d)", w.bob.id)+"`")
	if len(cm.Mentions) != 0 {
		t.Fatalf("mentions = %+v", cm.Mentions)
	}
	// Someone who can't read the space reads nothing of it, names included.
	expect(t, w.mallory.do(http.MethodGet, cardPath(w.shared, w.teamCard.BoardID, w.teamCard.ID, ""), nil), http.StatusNotFound)
}
