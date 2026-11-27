package notifications_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/boards"
	"taskto.ai/taskto/internal/mail"
	"taskto.ai/taskto/internal/notifications"
)

var cardEmailKinds = []string{
	notifications.KindCardComment,
	notifications.KindCardMention,
	notifications.KindCardDue,
	notifications.KindCardOverdue,
}

var emailSubject = map[string]string{
	notifications.KindCardComment: "A card of yours has a new comment",
	notifications.KindCardMention: "You were mentioned in a comment",
	notifications.KindCardDue:     "A card of yours is due",
	notifications.KindCardOverdue: "A card of yours is overdue",
}

func (w *world) notice(kind, title string) {
	w.t.Helper()
	ctx := context.Background()
	newCard := func(assignee *boards.Assignee, due *string) int64 {
		c, err := w.cards.CreateCard(ctx, boards.Actor{UserID: w.owner.id}, w.space.ID, w.board,
			boards.NewCard{Title: title, Assignee: assignee, Due: due})
		if err != nil {
			w.t.Fatal(err)
		}
		return c.ID
	}
	const due = "2026-12-01"
	switch kind {
	case notifications.KindCardComment:
		w.comment(w.owner, newCard(&boards.Assignee{Kind: boards.AssignUser, ID: w.bob.id}, nil), "Plain words about the paint")
	case notifications.KindCardMention:
		w.comment(w.owner, newCard(nil, nil), "Plain words about the paint "+mention("Bob", w.bob))
	case notifications.KindCardDue, notifications.KindCardOverdue:
		d := due
		card := newCard(&boards.Assignee{Kind: boards.AssignUser, ID: w.bob.id}, &d)
		tx, err := w.db.Begin()
		if err != nil {
			w.t.Fatal(err)
		}
		if err := notifications.Add(ctx, tx, notifications.Notice{UserID: w.bob.id, WorkspaceID: w.ws, Kind: kind,
			Ref: notifications.Ref{Space: w.space.ID, Board: w.board, Card: card, Due: due}}); err != nil {
			w.t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			w.t.Fatal(err)
		}
	default:
		w.t.Fatalf("no producer for %s", kind)
	}
}

func (w *world) ofKind(u *user, kind string) (id int64) {
	w.t.Helper()
	for _, it := range w.inbox(u, "").Notifications {
		if it.Kind == kind {
			return it.ID
		}
	}
	w.t.Fatalf("no %s notification in the inbox: %+v", kind, w.inbox(u, "").Notifications)
	return 0
}

func (w *world) removeBobFromTheSpace() {
	w.t.Helper()
	expect(w.t, w.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", w.ws, w.space.ID, w.bob.id), nil), http.StatusNoContent)
}

func TestEveryKindThatIsMailedHasItsOwnChoiceOffByDefault(t *testing.T) {
	w := newMailWorld(t)
	got := decode[notifications.EmailChoices](t, w.bob.do(http.MethodGet, w.emailPath(), nil))
	want := []string{
		notifications.KindCardAssigned, notifications.KindCardComment, notifications.KindCardMention,
		notifications.KindCardDue, notifications.KindCardOverdue, notifications.KindAppEffectFailed,
	}
	kinds := notifications.EmailKinds()
	slices.Sort(kinds)
	slices.Sort(want)
	if !slices.Equal(kinds, want) {
		t.Fatalf("EmailKinds() = %v, want %v", kinds, want)
	}
	for _, k := range want {
		if mode, ok := got.Kinds[k]; !ok || mode != notifications.ModeOff {
			t.Fatalf("kind %s is %q (listed: %v) in %+v", k, mode, ok, got.Kinds)
		}
	}
	if len(got.Kinds) != len(want) {
		t.Fatalf("choices for %d kinds: %+v", len(got.Kinds), got.Kinds)
	}
	for _, k := range cardEmailKinds {
		w.notice(k, "Nobody asked")
	}
	w.svc.DrainEmailOnce(context.Background())
	w.later(48 * time.Hour)
	w.sched.RunDue(context.Background())
	if n := len(w.box.messages()); n != 0 || w.count("email_deliveries", "1 = 1") != 0 {
		t.Fatalf("%d mails with email off", n)
	}
}

func TestEachKindIsMailedAsItHappensWhenChosenAndNoOtherIs(t *testing.T) {
	for _, kind := range cardEmailKinds {
		t.Run(kind, func(t *testing.T) {
			w := newMailWorld(t)
			ctx := context.Background()
			got := w.choose(w.bob, map[string]any{"kinds": map[string]string{kind: "instant"}})
			if got.Kinds[kind] != "instant" {
				t.Fatalf("choices = %+v", got.Kinds)
			}
			for _, other := range cardEmailKinds {
				if other != kind && got.Kinds[other] != "off" {
					t.Fatalf("choosing %s turned %s to %q", kind, other, got.Kinds[other])
				}
			}
			for _, k := range cardEmailKinds {
				w.notice(k, "Quarterly layoffs plan")
			}
			w.svc.DrainEmailOnce(ctx)
			msgs := w.box.messages()
			if len(msgs) != 1 || len(msgs[0].To) != 1 || msgs[0].To[0] != "bob@example.com" {
				t.Fatalf("mails = %+v", msgs)
			}
			if msgs[0].Subject != emailSubject[kind] {
				t.Fatalf("subject %q, want %q", msgs[0].Subject, emailSubject[kind])
			}
			all := msgs[0].Subject + msgs[0].Text + msgs[0].HTML
			link := fmt.Sprintf("%s/notification/%d", publicURL, w.ofKind(w.bob, kind))
			if !strings.Contains(msgs[0].Text, link) || !strings.Contains(msgs[0].HTML, link) {
				t.Fatalf("no link %s in %q", link, msgs[0].Text)
			}
			for _, leak := range []string{"Quarterly", "layoffs", "paint", "Plain words", "2026", "Owner", "Bob", "Team", "Home",
				fmt.Sprintf("%d:%d:", w.space.ID, w.board)} {
				if strings.Contains(all, leak) {
					t.Fatalf("the mail says %q: %s", leak, all)
				}
			}
			w.svc.DrainEmailOnce(ctx)
			if len(w.box.messages()) != 1 {
				t.Fatalf("%d mails after the worker came again", len(w.box.messages()))
			}
		})
	}
}

func TestNoKindIsMailedToSomeoneWhoCannotReadWhatItIsAbout(t *testing.T) {
	for _, kind := range cardEmailKinds {
		t.Run(kind+" instant", func(t *testing.T) {
			w := newMailWorld(t)
			w.choose(w.bob, map[string]any{"kinds": map[string]string{kind: "instant"}})
			w.notice(kind, "Secret")
			w.removeBobFromTheSpace()
			w.svc.DrainEmailOnce(context.Background())
			if n := len(w.box.messages()); n != 0 {
				t.Fatalf("%d mails of a %s bob can't read now: %+v", n, kind, w.box.messages())
			}
		})
		t.Run(kind+" digest", func(t *testing.T) {
			w := newMailWorld(t)
			w.choose(w.bob, map[string]any{"kinds": map[string]string{kind: "digest"}})
			w.notice(kind, "Secret")
			w.removeBobFromTheSpace()
			w.later(26 * time.Hour)
			w.sched.RunDue(context.Background())
			if n := len(w.box.messages()); n != 0 {
				t.Fatalf("a summary counting a %s bob can't read now: %+v", kind, w.box.messages())
			}
		})
	}
}

func TestADueNoticeIsNotMailedOnceTheCardsDateHasMoved(t *testing.T) {
	w := newMailWorld(t)
	w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardDue: "instant", notifications.KindCardOverdue: "digest"}})
	w.notice(notifications.KindCardDue, "Moved")
	w.notice(notifications.KindCardOverdue, "Moved")
	if _, err := w.db.Exec(`UPDATE cards SET due = NULL WHERE title = 'Moved'`); err != nil {
		t.Fatal(err)
	}
	w.svc.DrainEmailOnce(context.Background())
	w.later(26 * time.Hour)
	w.sched.RunDue(context.Background())
	if n := len(w.box.messages()); n != 0 {
		t.Fatalf("%d mails of a date that was cleared: %+v", n, w.box.messages())
	}
}

func TestTheSummaryCountsEveryKindChosenForItAndNoOther(t *testing.T) {
	w := newMailWorld(t)
	ctx := context.Background()
	chosen := map[string]string{
		notifications.KindCardComment: "digest", notifications.KindCardMention: "digest", notifications.KindCardDue: "digest",
	}
	w.choose(w.bob, map[string]any{"kinds": chosen})
	for _, k := range cardEmailKinds {
		w.notice(k, "Secret plan")
	}
	w.svc.DrainEmailOnce(ctx)
	if n := len(w.box.messages()); n != 0 {
		t.Fatalf("%d instant mails for kinds chosen for the summary", n)
	}
	w.later(26 * time.Hour)
	w.sched.RunDue(ctx)
	msgs := w.box.messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "3 of your notifications") {
		t.Fatalf("the summary = %+v", msgs)
	}
	if all := msgs[0].Subject + msgs[0].Text + msgs[0].HTML; strings.Contains(all, "Secret") || strings.Contains(all, "plan") {
		t.Fatalf("the summary says what the notifications are about: %s", all)
	}
	w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardOverdue: "digest"}})
	w.notice(notifications.KindCardOverdue, "Secret plan")
	w.stampNow()
	w.later(25 * time.Hour)
	w.sched.RunDue(ctx)
	msgs = w.box.messages()
	if len(msgs) != 2 || !strings.Contains(msgs[1].Text, "1 of your notifications") {
		t.Fatalf("the next summary = %+v", msgs)
	}
}

func TestEveryKindThatIsMailedHasAMessageInEveryLanguage(t *testing.T) {
	box := &mailbox{}
	svc := mail.NewService(box, mail.Info{Host: "smtp.example.com"})
	const link = "https://taskto.example.com/notification/7"
	for _, kind := range notifications.EmailKinds() {
		subjects := map[string]string{}
		for _, locale := range []string{"en", "pt-BR"} {
			if err := svc.SendTemplate(context.Background(), "bob@example.com", locale, "notification_"+kind, map[string]string{"action_url": link}); err != nil {
				t.Fatalf("%s in %s: %v", kind, locale, err)
			}
			m := box.last
			if m.Subject == "" || !strings.Contains(m.Text, link) || !strings.Contains(m.HTML, link) || strings.ContainsAny(m.Subject+m.Text, "{}") {
				t.Fatalf("%s in %s is incomplete: %+v", kind, locale, m)
			}
			subjects[locale] = m.Subject
		}
		if subjects["en"] == subjects["pt-BR"] {
			t.Fatalf("%s says %q in both languages", kind, subjects["en"])
		}
	}
}

func TestADueNoticeIsNotToldOnceTheCardIsNoLongerOpenAndBobs(t *testing.T) {
	for name, change := range map[string]string{
		"done":               `UPDATE cards SET col = 'done' WHERE title = 'Finished'`,
		"archived":           `UPDATE cards SET col = 'archived', archived_from = 'todo' WHERE title = 'Finished'`,
		"reassigned":         `UPDATE cards SET assignee_id = (SELECT id FROM users WHERE email = 'owner@example.com') WHERE title = 'Finished'`,
		"given to the agent": `UPDATE cards SET assignee_id = NULL, assignee_agent = 1 WHERE title = 'Finished'`,
		"unassigned":         `UPDATE cards SET assignee_id = NULL WHERE title = 'Finished'`,
		"board archived":     `UPDATE boards SET archived_at = '2026-01-01T00:00:00Z' WHERE id = (SELECT board_id FROM cards WHERE title = 'Finished')`,
		"space archived":     `UPDATE spaces SET archived_at = '2026-01-01T00:00:00Z' WHERE id = (SELECT space_id FROM boards WHERE id = (SELECT board_id FROM cards WHERE title = 'Finished'))`,
	} {
		for _, kind := range []string{notifications.KindCardDue, notifications.KindCardOverdue} {
			t.Run(name+" "+kind+" instant", func(t *testing.T) {
				w := newMailWorld(t)
				w.choose(w.bob, map[string]any{"kinds": map[string]string{kind: "instant"}})
				w.notice(kind, "Finished")
				if _, err := w.db.Exec(change); err != nil {
					t.Fatal(err)
				}
				w.svc.DrainEmailOnce(context.Background())
				if n := len(w.box.messages()); n != 0 {
					t.Fatalf("%d mails of a card that is %s: %+v", n, name, w.box.messages())
				}
				for _, it := range w.inbox(w.bob, "").Notifications {
					if it.Kind == kind {
						t.Fatalf("the inbox still tells of a card that is %s: %+v", name, it)
					}
				}
			})
			t.Run(name+" "+kind+" digest", func(t *testing.T) {
				w := newMailWorld(t)
				w.choose(w.bob, map[string]any{"kinds": map[string]string{kind: "digest"}})
				w.notice(kind, "Finished")
				if _, err := w.db.Exec(change); err != nil {
					t.Fatal(err)
				}
				w.later(26 * time.Hour)
				w.sched.RunDue(context.Background())
				if n := len(w.box.messages()); n != 0 {
					t.Fatalf("a summary counting a card that is %s: %+v", name, w.box.messages())
				}
			})
		}
	}
	w := newMailWorld(t)
	w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardDue: "instant"}})
	w.notice(notifications.KindCardDue, "Open")
	w.svc.DrainEmailOnce(context.Background())
	if n := len(w.box.messages()); n != 1 {
		t.Fatalf("%d mails of an open card", n)
	}
}

func TestAnInstantMailIsDecidedAndAddressedInOneFinalRead(t *testing.T) {
	for name, change := range map[string]func(w *world, tx *sql.Tx){
		"access to the space ends": func(w *world, tx *sql.Tx) {
			if _, err := tx.Exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, w.space.ID, w.bob.id); err != nil {
				w.t.Fatal(err)
			}
		},
		"the card is finished": func(w *world, tx *sql.Tx) {
			if _, err := tx.Exec(`UPDATE cards SET col = 'done'`); err != nil {
				w.t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newMailWorld(t)
			w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardDue: "instant"}})
			w.notice(notifications.KindCardDue, "Racing")
			final := false
			w.svc.AfterReserve(func() { final = true })
			w.svc.BetweenReads(func(tx *sql.Tx) {
				if final {
					change(w, tx)
				}
			})
			w.svc.DrainEmailOnce(context.Background())
			if n := len(w.box.messages()); n != 0 {
				t.Fatalf("%d mails after %s in the last read: %+v", n, name, w.box.messages())
			}
		})
	}
}
