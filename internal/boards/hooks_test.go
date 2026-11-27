package boards

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

var errNoGo = errors.New("no go")

func agentFor(u *user) Actor { return Actor{UserID: u.id, Agent: true} }

func TestCreateCardHooksRunInTheCardsTransaction(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	s, who := w.e.s, agentFor(w.alice)
	count := func() (n int) {
		if err := w.e.db.QueryRow(`SELECT count(*) FROM cards WHERE board_id = ?`, w.team.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()

	_, err := s.CreateCard(ctx, who, w.shared.ID, w.team.ID, NewCard{Title: "x", Check: func(*sql.Tx) error { return errNoGo }})
	if !errors.Is(err, errNoGo) || count() != before {
		t.Fatalf("err = %v, %d cards", err, count()-before)
	}
	_, err = s.CreateCard(ctx, who, w.shared.ID, w.team.ID, NewCard{Title: "x", Created: func(*sql.Tx, int64) error { return errNoGo }})
	if !errors.Is(err, errNoGo) || count() != before {
		t.Fatalf("err = %v, %d cards", err, count()-before)
	}
	var seen []int64
	nc := NewCard{Title: "x", Origin: "test:1", Created: func(_ *sql.Tx, id int64) error { seen = append(seen, id); return nil }}
	first, err := s.CreateCard(ctx, who, w.shared.ID, w.team.ID, nc)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.CreateCard(ctx, who, w.shared.ID, w.team.ID, nc)
	if err != nil || again.ID != first.ID || len(seen) != 2 || seen[0] != first.ID || seen[1] != first.ID || count() != before+1 {
		t.Fatalf("again = %+v, %v, seen %v, %d cards", again, err, seen, count()-before)
	}
}

func TestMoveCardIfMovesOnlyAfterItsGuardAndKeepsItsWrites(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	s, who := w.e.s, agentFor(w.alice)
	note := func(tx *sql.Tx, text string) {
		if _, err := tx.Exec(`UPDATE cards SET body = ? WHERE id = ?`, text, w.teamCard.ID); err != nil {
			t.Fatal(err)
		}
	}

	c, moved, err := s.MoveCardIf(ctx, who, w.shared.ID, w.team.ID, w.teamCard.ID, Done, func(tx *sql.Tx, _ Card) (bool, error) {
		note(tx, "noted")
		return false, nil
	})
	if err != nil || moved || c.Column == Done || c.Body != "noted" {
		t.Fatalf("card = %+v, moved %v, %v", c, moved, err)
	}
	_, _, err = s.MoveCardIf(ctx, who, w.shared.ID, w.team.ID, w.teamCard.ID, Done, func(tx *sql.Tx, _ Card) (bool, error) {
		note(tx, "lost")
		return false, errNoGo
	})
	if !errors.Is(err, errNoGo) {
		t.Fatalf("err = %v", err)
	}
	c, moved, err = s.MoveCardIf(ctx, who, w.shared.ID, w.team.ID, w.teamCard.ID, Done, func(tx *sql.Tx, c Card) (bool, error) {
		if c.Body != "noted" {
			t.Errorf("the guard saw %q", c.Body)
		}
		return true, nil
	})
	if err != nil || !moved || c.Column != Done || c.Body != "noted" {
		t.Fatalf("card = %+v, moved %v, %v", c, moved, err)
	}
	version := c.Version
	c, moved, err = s.MoveCardIf(ctx, who, w.shared.ID, w.team.ID, w.teamCard.ID, Done, func(*sql.Tx, Card) (bool, error) { return true, nil })
	if err != nil || moved || c.Version != version {
		t.Fatalf("card = %+v, moved %v, %v", c, moved, err)
	}
	if _, _, err := s.MoveCardIf(ctx, agentFor(w.bob), w.shared.ID, w.team.ID, w.teamCard.ID, Todo, nil); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("viewer: %v", err)
	}
	if _, _, err := s.MoveCardIf(ctx, who, w.shared.ID, w.team.ID, w.teamCard.ID, Archived, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("archived column: %v", err)
	}
}
