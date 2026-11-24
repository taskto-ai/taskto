package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"taskto.ai/taskto/internal/boards"
	"taskto.ai/taskto/internal/schedule"
)

// A card dated and assigned on the web keeps its reminders, as one the agent dates does: the
// server's HTTP routes and the command's tools schedule on the one scheduler.

func (s *stack) reminderJobs(user, card int64) (remind, overdue bool) {
	s.t.Helper()
	has := func(kind schedule.Kind) bool {
		_, err := s.sched.Find(context.Background(), s.db, user, kind, fmt.Sprintf("card:%d", card))
		if err != nil && !errors.Is(err, schedule.ErrNotFound) {
			s.t.Fatal(err)
		}
		return err == nil
	}
	return has(boards.KindDueReminder), has(boards.KindOverdue)
}

func TestACardDatedOnTheWebGetsItsReminders(t *testing.T) {
	s := newStack(t)
	code, b := s.browser(s.bob.cookie, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/spaces/%d/boards", s.one, s.team), map[string]any{"name": "Work"})
	if code != http.StatusCreated {
		t.Fatalf("create board = %d %v", code, b)
	}
	board := id(b["id"])
	cards := fmt.Sprintf("/api/workspaces/%d/spaces/%d/boards/%d/cards", s.one, s.team, board)
	due := time.Now().UTC().AddDate(0, 0, 10).Format("2006-01-02")

	code, card := s.browser(s.bob.cookie, http.MethodPost, cards, map[string]any{
		"title": "Pay the invoice", "due": due, "assignee": map[string]any{"kind": "user", "id": s.bob.id},
	})
	if code != http.StatusCreated {
		t.Fatalf("create card = %d %v", code, card)
	}
	cardID := id(card["id"])
	if r, o := s.reminderJobs(s.bob.id, cardID); !r || !o {
		t.Fatalf("jobs of a dated, assigned card made on the web: reminder %v, overdue %v", r, o)
	}
	if r, o := s.reminderJobs(s.alice.id, cardID); r || o {
		t.Fatal("someone who is not the assignee has jobs")
	}

	// Archiving the space takes them away, and bringing it back gives them again.
	space := fmt.Sprintf("/api/workspaces/%d/spaces/%d", s.one, s.team)
	if code, body := s.browser(s.alice.cookie, http.MethodPatch, space, map[string]any{"archived": true}); code != http.StatusOK {
		t.Fatalf("archive space = %d %v", code, body)
	}
	if r, o := s.reminderJobs(s.bob.id, cardID); r || o {
		t.Fatalf("jobs of a card in an archived space: reminder %v, overdue %v", r, o)
	}
	if code, body := s.browser(s.alice.cookie, http.MethodPatch, space, map[string]any{"archived": false}); code != http.StatusOK {
		t.Fatalf("restore space = %d %v", code, body)
	}
	if r, o := s.reminderJobs(s.bob.id, cardID); !r || !o {
		t.Fatalf("jobs after the space came back: reminder %v, overdue %v", r, o)
	}

	// Taking bob out of the workspace takes his jobs with him.
	if code, body := s.browser(s.alice.cookie, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", s.one, s.bob.id), nil); code != http.StatusNoContent {
		t.Fatalf("remove member = %d %v", code, body)
	}
	if r, o := s.reminderJobs(s.bob.id, cardID); r || o {
		t.Fatalf("jobs of a person who left: reminder %v, overdue %v", r, o)
	}
}
