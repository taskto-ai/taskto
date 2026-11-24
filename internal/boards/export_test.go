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
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("cards read = %v, want each of %v once", got, want)
	}
}

func TestExportBoardsAndCardsAreTheCallersToRead(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	if _, err := w.e.s.ExportBoards(ctx, w.mallory.id, w.alicePrivate.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another person's private space = %v, want ErrNotFound", err)
	}
	if err := w.e.s.ExportCards(ctx, w.mallory.id, w.alicePrivate.ID, w.alicePersonal.ID, func(Thread) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another person's cards = %v, want ErrNotFound", err)
	}
	list, err := w.e.s.ExportBoards(ctx, w.alice.id, w.alicePrivate.ID)
	if err != nil || len(list) != 1 || list[0].ID != w.alicePersonal.ID {
		t.Fatalf("own boards = %v, %v", list, err)
	}
}
