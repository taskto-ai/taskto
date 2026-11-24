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

// A person who is added back to a space, or to the workspace and then the space, has their
// reminders back: the server gives the hooks that do it to the spaces and the accounts.
func TestAPersonAddedBackHasTheirRemindersBack(t *testing.T) {
	s := newStack(t)
	code, b := s.browser(s.bob.cookie, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/spaces/%d/boards", s.one, s.team), map[string]any{"name": "Work"})
	if code != http.StatusCreated {
		t.Fatalf("create board = %d %v", code, b)
	}
	cards := fmt.Sprintf("/api/workspaces/%d/spaces/%d/boards/%d/cards", s.one, s.team, id(b["id"]))
	due := time.Now().UTC().AddDate(0, 0, 10).Format("2006-01-02")
	code, card := s.browser(s.bob.cookie, http.MethodPost, cards, map[string]any{
		"title": "Pay the invoice", "due": due, "assignee": map[string]any{"kind": "user", "id": s.bob.id},
	})
	if code != http.StatusCreated {
		t.Fatalf("create card = %d %v", code, card)
	}
	cardID := id(card["id"])
	want := func(step string, remind, overdue bool) {
		t.Helper()
		if r, o := s.reminderJobs(s.bob.id, cardID); r != remind || o != overdue {
			t.Fatalf("%s: reminder %v, overdue %v; want %v and %v", step, r, o, remind, overdue)
		}
	}
	want("made", true, true)

	members := fmt.Sprintf("/api/workspaces/%d/spaces/%d/members", s.one, s.team)
	add := func() {
		t.Helper()
		if code, body := s.browser(s.alice.cookie, http.MethodPost, members, map[string]any{"user_id": s.bob.id, "role": "editor"}); code != http.StatusNoContent {
			t.Fatalf("add to the space = %d %v", code, body)
		}
	}
	if code, body := s.browser(s.alice.cookie, http.MethodDelete, fmt.Sprintf("%s/%d", members, s.bob.id), nil); code != http.StatusNoContent {
		t.Fatalf("remove from the space = %d %v", code, body)
	}
	want("left the space", false, false)
	add()
	want("added to the space", true, true)

	// Out of the workspace, and back in through a new invite: the space is theirs again once they
	// are added to it, and not before.
	if code, body := s.browser(s.alice.cookie, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", s.one, s.bob.id), nil); code != http.StatusNoContent {
		t.Fatalf("remove from the workspace = %d %v", code, body)
	}
	want("left the workspace", false, false)
	code, inv := s.browser(s.alice.cookie, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", s.one), map[string]any{"email": "bob@example.com"})
	if code != http.StatusCreated {
		t.Fatalf("invite = %d %v", code, inv)
	}
	if code, body := s.browser(s.bob.cookie, http.MethodPost, "/api/invites/accept", map[string]any{"token": inv["token"]}); code != http.StatusOK {
		t.Fatalf("accept = %d %v", code, body)
	}
	want("back in the workspace", false, false)
	add()
	want("added to the space again", true, true)
}
