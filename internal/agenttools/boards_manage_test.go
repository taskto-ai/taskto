package agenttools

import (
	"context"
	"net/http"
	"testing"

	"taskto.ai/taskto/internal/boards"
)

// The agent creates, renames and archives boards, and archives and restores cards, with
// the user's own access and as their changes marked as the agent's.
func TestBoardManagementTools(t *testing.T) {
	w := newWorld(t)

	// A team board in a shared space the user edits.
	b := w.ok(w.aliceConv, "create_board", map[string]any{"space_id": w.shared, "name": "  Roadmap "})
	if b["name"] != "Roadmap" || b["kind"] != "team" || b["archived_at"] != nil {
		t.Fatalf("created board = %v", b)
	}
	// Boards record the user, not the agent mark (cards do).
	if by := b["created_by"].(map[string]any); id(by["id"]) != w.alice {
		t.Fatalf("created_by = %v", by)
	}
	space := map[string]any{"space_id": w.shared, "board_id": id(b["id"])}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for _, m := range []map[string]any{space, extra} {
			for k, v := range m {
				out[k] = v
			}
		}
		return out
	}

	// A private space has one board; names are checked as over HTTP.
	w.status(http.StatusBadRequest, w.aliceConv, "create_board", map[string]any{"space_id": w.alicePrivate, "name": "Second"})
	w.status(http.StatusBadRequest, w.aliceConv, "create_board", map[string]any{"space_id": w.shared, "name": "   "})

	// Rename, with versions.
	b = w.ok(w.aliceConv, "update_board", with(map[string]any{"version": b["version"], "name": "Plan"}))
	if b["name"] != "Plan" || id(b["version"]) != 2 {
		t.Fatalf("renamed board = %v", b)
	}
	w.status(http.StatusConflict, w.aliceConv, "update_board", with(map[string]any{"version": 1, "name": "Stale"}))
	w.status(http.StatusBadRequest, w.aliceConv, "update_board", with(map[string]any{"version": b["version"]}))

	// A personal board can't be archived.
	w.status(http.StatusBadRequest, w.aliceConv, "update_board", map[string]any{"space_id": w.alicePrivate, "board_id": w.alicePersonal, "version": 1, "archived": true})

	// Cards: archive leaves the columns, restore puts it back where it was.
	card := w.ok(w.aliceConv, "create_card", with(map[string]any{"title": "Ship", "column": "review"}))
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
	w.status(http.StatusBadRequest, w.aliceConv, "restore_card", withCard(map[string]any{"version": card["version"]})) // not archived
	card = w.ok(w.aliceConv, "archive_card", withCard(map[string]any{"version": card["version"]}))
	if card["column"] != "archived" || card["archived_from"] != "review" {
		t.Fatalf("archived card = %v", card)
	}
	if u := card["updated_by"].(map[string]any); id(u["id"]) != w.alice || u["agent"] != true {
		t.Fatalf("updated_by = %v", u)
	}
	w.status(http.StatusConflict, w.aliceConv, "archive_card", withCard(map[string]any{"version": 1}))
	w.status(http.StatusBadRequest, w.aliceConv, "archive_card", withCard(map[string]any{"version": card["version"]})) // already
	if got := w.ok(w.aliceConv, "list_cards", with(map[string]any{}))["cards"].([]any); len(got) != 0 {
		t.Fatalf("cards = %v", got)
	}
	if got := w.ok(w.aliceConv, "list_cards", with(map[string]any{"column": "archived"}))["cards"].([]any); len(got) != 1 {
		t.Fatalf("archived cards = %v", got)
	}
	w.status(http.StatusBadRequest, w.aliceConv, "restore_card", withCard(map[string]any{"version": card["version"], "column": "archived"}))
	card = w.ok(w.aliceConv, "restore_card", withCard(map[string]any{"version": card["version"]}))
	if card["column"] != "review" {
		t.Fatalf("restored card = %v", card)
	}
	card = w.ok(w.aliceConv, "archive_card", withCard(map[string]any{"version": card["version"]}))
	card = w.ok(w.aliceConv, "restore_card", withCard(map[string]any{"version": card["version"], "column": "todo"}))
	if card["column"] != "todo" {
		t.Fatalf("restored card = %v", card)
	}

	// An archived board leaves list_boards unless asked for, and is read-only.
	b = w.ok(w.aliceConv, "update_board", with(map[string]any{"version": b["version"], "archived": true}))
	if b["archived_at"] == nil {
		t.Fatalf("archived board = %v", b)
	}
	names := func(args map[string]any) []string {
		var out []string
		for _, v := range w.ok(w.aliceConv, "list_boards", args)["boards"].([]any) {
			out = append(out, v.(map[string]any)["name"].(string))
		}
		return out
	}
	if got := names(map[string]any{"space_id": w.shared}); len(got) != 0 {
		t.Fatalf("boards = %v", got)
	}
	if got := names(map[string]any{"space_id": w.shared, "archived": true}); len(got) != 1 || got[0] != "Plan" {
		t.Fatalf("boards with archived = %v", got)
	}
	w.status(http.StatusForbidden, w.aliceConv, "update_board", with(map[string]any{"version": b["version"], "name": "Nope"}))
	w.status(http.StatusForbidden, w.aliceConv, "archive_card", withCard(map[string]any{"version": card["version"]}))
	b = w.ok(w.aliceConv, "update_board", with(map[string]any{"version": b["version"], "archived": false}))
	if b["archived_at"] != nil {
		t.Fatalf("unarchived board = %v", b)
	}
	if got := names(map[string]any{"space_id": w.shared}); len(got) != 1 {
		t.Fatalf("boards = %v", got)
	}

	// A viewer reads the boards, archived ones too, and changes nothing.
	if got := w.ok(w.bobConv, "list_boards", map[string]any{"space_id": w.shared, "archived": true})["boards"].([]any); len(got) != 1 {
		t.Fatalf("bob's boards = %v", got)
	}
	for tool, args := range map[string]map[string]any{
		"create_board": {"space_id": w.shared, "name": "Bob's"},
		"update_board": with(map[string]any{"version": b["version"], "name": "Bob's"}),
		"archive_card": withCard(map[string]any{"version": card["version"]}),
		"restore_card": withCard(map[string]any{"version": card["version"]}),
	} {
		w.status(http.StatusForbidden, w.bobConv, tool, args)
	}
	got, err := w.tools.o.Boards.ShowCard(context.Background(), boards.Actor{UserID: w.alice}, w.shared, id(b["id"]), id(card["id"]))
	if err != nil || got.Column != boards.Todo || got.Version != id(card["version"]) {
		t.Fatalf("card after bob's attempts = %+v, %v", got, err)
	}
}

// Someone else's spaces and boards are "not found" to the new tools, as to the others, and
// nothing changes.
func TestBoardManagementToolsCannotReachOtherUsersBoards(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	bobCard, err := w.tools.o.Boards.CreateCard(ctx, boards.Actor{UserID: w.bob}, w.bobPrivate, w.bobPersonal, boards.NewCard{Title: "Bob's card"})
	if err != nil {
		t.Fatal(err)
	}

	missing := w.alicePrivate2 + 1000
	for _, space := range []int64{w.bobPrivate, w.alicePrivate2, missing} {
		for tool, args := range map[string]map[string]any{
			"list_boards":  {"archived": true},
			"create_board": {"name": "x"},
			"update_board": {"board_id": w.bobPersonal, "version": 1, "name": "x"},
			"archive_card": {"board_id": w.bobPersonal, "card_id": bobCard.ID, "version": 1},
			"restore_card": {"board_id": w.bobPersonal, "card_id": bobCard.ID, "version": 1},
		} {
			args["space_id"] = space
			if out := w.status(http.StatusNotFound, w.aliceConv, tool, args); out["error"] != "not found" {
				t.Fatalf("%s in space %d: %v", tool, space, out)
			}
		}
	}
	// Bob's board through alice's own space is no way round.
	w.status(http.StatusNotFound, w.aliceConv, "update_board", map[string]any{"space_id": w.alicePrivate, "board_id": w.bobPersonal, "version": 1, "name": "x"})
	w.status(http.StatusNotFound, w.aliceConv, "archive_card", map[string]any{"space_id": w.alicePrivate, "board_id": w.bobPersonal, "card_id": bobCard.ID, "version": 1})

	// A conversation of another workspace can't reach this one's shared space.
	w.status(http.StatusNotFound, w.aliceConv2, "create_board", map[string]any{"space_id": w.shared, "name": "x"})

	card, err := w.tools.o.Boards.ShowCard(ctx, boards.Actor{UserID: w.bob}, w.bobPrivate, w.bobPersonal, bobCard.ID)
	if err != nil || card.Version != 1 || card.Column == boards.Archived {
		t.Fatalf("bob's card = %+v, %v", card, err)
	}
	b, err := w.tools.o.Boards.ListBoards(ctx, boards.Actor{UserID: w.bob}, w.bobPrivate)
	if err != nil || len(b) != 1 || b[0].Name == "x" || b[0].Version != 1 {
		t.Fatalf("bob's boards = %+v, %v", b, err)
	}
}
