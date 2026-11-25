package boards

import (
	"errors"
	"slices"
	"testing"
)

// The export reads a board's cards a page of IDs at a time, over every column in one statement:
// whatever moves, is archived or is added meanwhile, each card that was there from the start is
// read once, whole, with its comments.
func TestExportCardsReadsEachCardOnceWhileTheyMove(t *testing.T) {
	old := exportPage
	exportPage = 2
	t.Cleanup(func() { exportPage = old })

	w := newWorld(t)
	ctx := t.Context()
	space, board := w.alicePrivate, w.alicePersonal
	var want []int64
	for i := range 7 {
		c := w.createCard(w.alice, space, board, map[string]any{"title": string(rune('a' + i))})
		want = append(want, c.ID)
	}
	// The personal board's own first card is there already.
	want = append([]int64{w.aliceCard.ID}, want...)

	var got []int64
	moved := false
	err := w.e.s.ExportCards(ctx, w.alice.id, space.ID, board.ID, func(th Thread) error {
		got = append(got, th.ID)
		if !moved {
			moved = true
			// Between pages, every card goes to another column and one is archived.
			if _, err := w.e.db.Exec(`UPDATE cards SET col = 'done' WHERE board_id = ?`, board.ID); err != nil {
				return err
			}
			if _, err := w.e.db.Exec(`UPDATE cards SET col = 'archived', archived_from = 'done' WHERE id = ?`, want[3]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("cards read = %v, want each of %v once", got, want)
	}
}

func TestExportBoardsAndCardsAreTheCallersToRead(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	if err := w.e.s.ExportBoards(ctx, w.mallory.id, w.alicePrivate.ID, func(Board) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another person's private space = %v, want ErrNotFound", err)
	}
	if err := w.e.s.ExportCards(ctx, w.mallory.id, w.alicePrivate.ID, w.alicePersonal.ID, func(Thread) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another person's cards = %v, want ErrNotFound", err)
	}
	var list []Board
	err := w.e.s.ExportBoards(ctx, w.alice.id, w.alicePrivate.ID, func(b Board) error { list = append(list, b); return nil })
	if err != nil || len(list) != 1 || list[0].ID != w.alicePersonal.ID {
		t.Fatalf("own boards = %v, %v", list, err)
	}
}

// The boards of a space are read a page at a time, by ID, archived ones too, each once.
func TestExportBoardsPagesByID(t *testing.T) {
	old := exportPage
	exportPage = 2
	t.Cleanup(func() { exportPage = old })
	w := newWorld(t)
	want := []int64{w.team.ID, w.team2.ID}
	for _, name := range []string{"c", "b", "a", "d"} {
		want = append(want, w.createBoard(w.alice, w.shared, name).ID)
	}
	var got []int64
	err := w.e.s.ExportBoards(t.Context(), w.alice.id, w.shared.ID, func(b Board) error { got = append(got, b.ID); return nil })
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("boards = %v, %v, want %v by ID", got, err, want)
	}
}

// A person who loses a shared space while its cards are read gets no card after that: the
// membership is checked again with every page of IDs and every card, not once for the board.
func TestExportCardsStopAtTheNextReadAfterTheCallerLosesTheSpace(t *testing.T) {
	old := exportPage
	exportPage = 2
	t.Cleanup(func() { exportPage = old })
	w := newWorld(t)
	for i := range 5 {
		w.createCard(w.alice, w.shared, w.team, map[string]any{"title": string(rune('a' + i))})
	}
	seen := 0
	err := w.e.s.ExportCards(t.Context(), w.alice.id, w.shared.ID, w.team.ID, func(Thread) error {
		seen++
		if _, err := w.e.db.Exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, w.shared.ID, w.alice.id); err != nil {
			t.Fatal(err)
		}
		return nil
	})
	if !errors.Is(err, ErrNotFound) || seen != 1 {
		t.Fatalf("err = %v after %d cards, want ErrNotFound after the one read while they were a member", err, seen)
	}
}

// Between a page of IDs and the card it names, and between the check and the card's own read, the
// space can go: the card is read in a snapshot that begins with the check, so a person who lost
// the space before it gets nothing of the card, and one who loses it during it gets the card as
// it was while they held it.
func TestExportCardsReadACardOnlyAsOfACheckOfTheMembership(t *testing.T) {
	w := newWorld(t)
	revoke := func() {
		if _, err := w.e.db.Exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, w.shared.ID, w.alice.id); err != nil {
			t.Fatal(err)
		}
	}
	// Lost after the page of IDs, before the card's own snapshot begins.
	w.e.hooks.BeforeQuery("SELECT c.id FROM cards c", func() { w.e.hooks.BeforeBegin(revoke) })
	seen := 0
	err := w.e.s.ExportCards(t.Context(), w.alice.id, w.shared.ID, w.team.ID, func(Thread) error { seen++; return nil })
	if !errors.Is(err, ErrNotFound) || seen != 0 {
		t.Fatalf("err = %v after %d cards, want ErrNotFound and no card", err, seen)
	}

	// Lost while the card is being read: what the snapshot read is of the state that allowed it.
	w2 := newWorld(t)
	w2.e.hooks.BeforeQuery("FROM card_comments cc", func() {
		if _, err := w2.e.db.Exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, w2.shared.ID, w2.alice.id); err != nil {
			t.Fatal(err)
		}
	})
	var got []int64
	err = w2.e.s.ExportCards(t.Context(), w2.alice.id, w2.shared.ID, w2.team.ID, func(th Thread) error { got = append(got, th.ID); return nil })
	if err != nil || len(got) != 1 {
		t.Fatalf("cards read = %v, %v, want the one read as a member", got, err)
	}
}
