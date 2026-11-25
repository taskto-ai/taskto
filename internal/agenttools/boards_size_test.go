package agenttools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/boards"
)

// checkFits fails unless out, as the tools socket sends it, is within MaxResult.
func checkFits(t *testing.T, what string, out map[string]any) {
	t.Helper()
	b, err := json.Marshal(out)
	if err != nil || len(b) > MaxResult {
		t.Fatalf("%s: %d bytes, %v", what, len(b), err)
	}
}

// A card or comment the tools just changed is answered even when its body, escaped as
// JSON, is more than a result may be: the body is cut and the reply says so.
func TestCardRepliesFitTheResultLimit(t *testing.T) {
	w := newWorld(t)
	space := map[string]any{"space_id": w.alicePrivate, "board_id": w.alicePersonal}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for _, m := range []map[string]any{space, extra} {
			for k, v := range m {
				out[k] = v
			}
		}
		return out
	}
	// "<" is six bytes escaped, a newline two: both make 32 KiB more than 64 KiB.
	for name, filler := range map[string]string{"angle": "<", "newline": "\n", "quote": `"`} {
		t.Run(name, func(t *testing.T) {
			big := strings.Repeat(filler, 32<<10)
			bigText := "x" + big[1:] // a body that isn't only white space
			if filler != "\n" {
				bigText = big
			}

			card := w.ok(w.aliceConv, "create_card", with(map[string]any{"title": "Big " + name, "body": bigText}))
			checkFits(t, "create_card", card)
			if card["body_truncated"] != true || !strings.HasSuffix(card["body"].(string), truncatedNotes) || id(card["id"]) == 0 {
				t.Fatalf("created card = %v", card["body_truncated"])
			}
			stored, err := w.tools.o.Boards.ShowCard(context.Background(), boards.Actor{UserID: w.alice}, w.alicePrivate, w.alicePersonal, id(card["id"]))
			if err != nil || len(stored.Body) != len(bigText) {
				t.Fatalf("the card was stored with %d bytes, %v", len(stored.Body), err)
			}
			cref := with(map[string]any{"card_id": id(card["id"])})
			withCard := func(extra map[string]any) map[string]any {
				out := map[string]any{}
				for _, m := range []map[string]any{cref, extra} {
					for k, v := range m {
						out[k] = v
					}
				}
				return out
			}

			card = w.ok(w.aliceConv, "update_card", withCard(map[string]any{"version": card["version"], "title": "Big again"}))
			checkFits(t, "update_card", card)
			card = w.ok(w.aliceConv, "move_card", withCard(map[string]any{"version": card["version"], "column": "doing"}))
			checkFits(t, "move_card", card)
			card = w.ok(w.aliceConv, "set_blocked", withCard(map[string]any{"version": card["version"], "reason": "waiting"}))
			checkFits(t, "set_blocked", card)
			card = w.ok(w.aliceConv, "archive_card", withCard(map[string]any{"version": card["version"]}))
			checkFits(t, "archive_card", card)
			card = w.ok(w.aliceConv, "restore_card", withCard(map[string]any{"version": card["version"]}))
			checkFits(t, "restore_card", card)
			for _, out := range []map[string]any{card} {
				if out["body_truncated"] != true || out["column"] != "doing" {
					t.Fatalf("restored card = %v %v", out["body_truncated"], out["column"])
				}
			}

			comment := w.ok(w.aliceConv, "comment_card", withCard(map[string]any{"body": bigText}))
			checkFits(t, "comment_card", comment)
			if comment["body_truncated"] != true || id(comment["id"]) == 0 {
				t.Fatalf("comment = %v", comment["body_truncated"])
			}

			shown := w.ok(w.aliceConv, "show_card", cref)
			checkFits(t, "show_card", shown)
			if shown["body_truncated"] != true || !strings.HasSuffix(shown["body"].(string), truncatedNotes) {
				t.Fatalf("shown card's body_truncated = %v", shown["body_truncated"])
			}
		})
	}

	// A small card is answered whole, with no mark.
	card := w.ok(w.aliceConv, "create_card", with(map[string]any{"title": "Small", "body": "<ok>"}))
	if _, marked := card["body_truncated"]; marked || card["body"] != "<ok>" {
		t.Fatalf("small card = %v", card)
	}
}

// With many boards, the archived ones, which come last, are reached by pages.
func TestListBoardsPages(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	actor := boards.Actor{UserID: w.alice}
	name := func(i int) string { return fmt.Sprintf("%03d %s", i, strings.Repeat("n", 90)) }
	const active, archived = 700, 3
	var archivedIDs []int64
	for i := range active + archived {
		b, err := w.tools.o.Boards.CreateBoard(ctx, actor, w.shared, name(i))
		if err != nil {
			t.Fatal(err)
		}
		if i >= active {
			yes := true
			if _, err := w.tools.o.Boards.UpdateBoard(ctx, actor, w.shared, b.ID, boards.BoardChanges{Version: b.Version, Archived: &yes}); err != nil {
				t.Fatal(err)
			}
			archivedIDs = append(archivedIDs, b.ID)
		}
	}

	walk := func(args map[string]any) (ids []int64, pages int) {
		for {
			out := w.ok(w.aliceConv, "list_boards", args)
			checkFits(t, "list_boards", out)
			pages++
			for _, v := range out["boards"].([]any) {
				ids = append(ids, id(v.(map[string]any)["id"]))
			}
			next, more := out["next"]
			if more != (out["truncated"] == true) {
				t.Fatalf("truncated = %v, next = %v", out["truncated"], next)
			}
			if !more {
				return ids, pages
			}
			if pages > 20 {
				t.Fatal("no end to the pages")
			}
			args = map[string]any{"space_id": w.shared, "archived": args["archived"], "after": next}
		}
	}

	ids, pages := walk(map[string]any{"space_id": w.shared, "archived": true})
	if len(ids) != active+archived || pages < 2 {
		t.Fatalf("%d boards in %d pages", len(ids), pages)
	}
	seen := map[int64]bool{}
	for _, b := range ids {
		if seen[b] {
			t.Fatalf("board %d twice", b)
		}
		seen[b] = true
	}
	for _, b := range archivedIDs {
		if !seen[b] {
			t.Fatalf("archived board %d is unreachable", b)
		}
	}
	if ids, _ := walk(map[string]any{"space_id": w.shared}); len(ids) != active {
		t.Fatalf("%d active boards", len(ids))
	}

	// An after that is not in the list (the archived ones are not, without archived) or not
	// an id is refused, and for a space the user can't read it is "not found" first.
	w.status(400, w.aliceConv, "list_boards", map[string]any{"space_id": w.shared, "after": archivedIDs[0]})
	w.status(400, w.aliceConv, "list_boards", map[string]any{"space_id": w.shared, "after": -1})
	w.status(404, w.aliceConv, "list_boards", map[string]any{"space_id": w.bobPrivate, "after": archivedIDs[0]})
}

// A card with the most docs validation allows, each with the longest escaped path, is
// answered too: the last docs are left out, and the reply says so.
func TestCardRepliesFitWithManyLongDocs(t *testing.T) {
	w := newWorld(t)
	var docs []string
	for i := range 50 {
		docs = append(docs, fmt.Sprintf("%02d/%s.md", i, strings.Repeat("<", 240)))
	}
	card := w.ok(w.aliceConv, "create_card", map[string]any{
		"space_id": w.alicePrivate, "board_id": w.alicePersonal, "title": "Linked", "docs": docs, "body": strings.Repeat("<", 4<<10),
	})
	checkFits(t, "create_card", card)
	if card["docs_truncated"] != true || len(card["docs"].([]any)) == 0 || len(card["docs"].([]any)) >= len(docs) {
		t.Fatalf("created card's docs = %d, docs_truncated = %v", len(card["docs"].([]any)), card["docs_truncated"])
	}
	if first := card["docs"].([]any)[0]; first != docs[0] {
		t.Fatalf("first doc = %v", first)
	}
	cref := map[string]any{"space_id": w.alicePrivate, "board_id": w.alicePersonal, "card_id": id(card["id"])}
	card = w.ok(w.aliceConv, "archive_card", map[string]any{"space_id": w.alicePrivate, "board_id": w.alicePersonal, "card_id": id(card["id"]), "version": card["version"]})
	checkFits(t, "archive_card", card)
	if card["column"] != "archived" || card["docs_truncated"] != true {
		t.Fatalf("archived card = %v", card["docs_truncated"])
	}
	shown := w.ok(w.aliceConv, "show_card", cref)
	checkFits(t, "show_card", shown)
	if shown["docs_truncated"] != true {
		t.Fatalf("shown card's docs_truncated = %v", shown["docs_truncated"])
	}
	stored, err := w.tools.o.Boards.ShowCard(context.Background(), boards.Actor{UserID: w.alice}, w.alicePrivate, w.alicePersonal, id(card["id"]))
	if err != nil || len(stored.Docs) != len(docs) {
		t.Fatalf("stored docs = %d, %v", len(stored.Docs), err)
	}
}

// The docs kept leave room for the body's cut marker: with the docs that fit and no body
// just under the limit, the reply is still within it.
func TestFitCardLeavesRoomForTheCutMarker(t *testing.T) {
	var docs []string
	for i := range 40 {
		docs = append(docs, fmt.Sprintf("d/%02d-%s.md", i, strings.Repeat("a", 1000)))
	}
	card := boards.Card{Title: "t", Body: strings.Repeat("<", 20<<10), Docs: docs}
	build := func(c boards.Card, m cutMarks) any { return cardReply{c, m} }
	size := func(c boards.Card) int {
		out, err := json.Marshal(build(c, cutMarks{true, true}))
		if err != nil {
			t.Fatal(err)
		}
		return len(out)
	}
	// The title pads the card with no body to 3 bytes under the limit: it fits, but no
	// body, even one cut to its marker, would.
	bare := card
	bare.Body = ""
	card.Title = strings.Repeat("x", MaxResult-3-size(bare)+1)
	bare.Title = card.Title
	if got := size(bare); got != MaxResult-3 {
		t.Fatalf("bare card is %d bytes", got)
	}

	got, marks, err := fitCard(card, build)
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(build(got, marks))
	if err != nil || len(out) > MaxResult {
		t.Fatalf("reply is %d bytes, %v", len(out), err)
	}
	if !marks.DocsTruncated || !marks.BodyTruncated || len(got.Docs) >= len(docs) {
		t.Fatalf("marks = %+v, %d docs", marks, len(got.Docs))
	}
}

// show_card counts the 16 KiB cut's mark in the size it fits, whatever the card's size.
func TestShowCardFitsWithItsCutMark(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	actor := boards.Actor{UserID: w.alice}
	var docs []string
	for i := range 50 {
		docs = append(docs, fmt.Sprintf("d/%02d-%s.md", i, strings.Repeat("a", 962)))
	}
	card, err := w.tools.o.Boards.CreateCard(ctx, actor, w.alicePrivate, w.alicePersonal, boards.NewCard{
		Title: "x", Body: strings.Repeat("a", 20<<10), Docs: docs,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Titles of every length from 1 to 200 characters step the reply a byte at a time
	// across the limit, past the point where the marks fit exactly.
	for n := 1; n <= 200; n++ {
		title := strings.Repeat("x", n)
		card, err = w.tools.o.Boards.UpdateCard(ctx, actor, w.alicePrivate, w.alicePersonal, card.ID, boards.CardChanges{Version: card.Version, Title: &title})
		if err != nil {
			t.Fatal(err)
		}
		shown := w.ok(w.aliceConv, "show_card", map[string]any{"space_id": w.alicePrivate, "board_id": w.alicePersonal, "card_id": card.ID})
		checkFits(t, fmt.Sprintf("show_card with a title of %d", n), shown)
		if shown["body_truncated"] != true {
			t.Fatalf("title of %d: body_truncated = %v", n, shown["body_truncated"])
		}
	}
}
