package boards

import (
	"errors"
	"slices"
	"testing"
)

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
	want = append([]int64{w.aliceCard.ID}, want...)

	var got []int64
	moved := false
	err := w.e.s.ExportCards(ctx, w.alice.id, space.ID, board.ID, func(th Thread) error {
		got = append(got, th.ID)
		if !moved {
			moved = true
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
	err := w.e.s.ExportBoards(ctx, w.mallory.id, w.alicePrivate.ID, func(Board) error {
		t.Error("a board of another person's private space was handed over")
		return nil
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("another person's private space = %v, want ErrNotFound", err)
	}
	if err := w.e.s.ExportCards(ctx, w.mallory.id, w.alicePrivate.ID, w.alicePersonal.ID, func(Thread) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another person's cards = %v, want ErrNotFound", err)
	}
	var list []Board
	err = w.e.s.ExportBoards(ctx, w.alice.id, w.alicePrivate.ID, func(b Board) error { list = append(list, b); return nil })
	if err != nil || len(list) != 1 || list[0].ID != w.alicePersonal.ID {
		t.Fatalf("own boards = %v, %v", list, err)
	}
}

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

func TestExportCardsReadACardOnlyAsOfACheckOfTheMembership(t *testing.T) {
	w := newWorld(t)
	revoke := func() {
		if _, err := w.e.db.Exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, w.shared.ID, w.alice.id); err != nil {
			t.Fatal(err)
		}
	}
	w.e.hooks.BeforeQuery("SELECT c.id FROM cards c", func() { w.e.hooks.BeforeBegin(revoke) })
	seen := 0
	err := w.e.s.ExportCards(t.Context(), w.alice.id, w.shared.ID, w.team.ID, func(Thread) error { seen++; return nil })
	if !errors.Is(err, ErrNotFound) || seen != 0 {
		t.Fatalf("err = %v after %d cards, want ErrNotFound and no card", err, seen)
	}

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
