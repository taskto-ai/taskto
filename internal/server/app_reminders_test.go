package server

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/notifications"
	"taskto.ai/taskto/internal/serverkey"
)

func (s *stack) appReminderJobs(user int64) int64 {
	s.t.Helper()
	return s.scalar(`SELECT count(*) FROM scheduled_jobs WHERE kind = ? AND user_id = ?`, string(apps.KindRecordReminder), user)
}

func (s *stack) appReminderRunAt(user int64) string {
	s.t.Helper()
	var at string
	if err := s.db.QueryRow(`SELECT CAST(run_at AS TEXT) FROM scheduled_jobs WHERE kind = ? AND user_id = ?`, string(apps.KindRecordReminder), user).Scan(&at); err != nil {
		s.t.Fatal(err)
	}
	return at
}

func TestTheServerWiresAppRemindersToTheSchedulerTheHooksAndTheInbox(t *testing.T) {
	var inbox *notifications.Service
	s := newStackDB(t, func(o *Options, sqlDB *sql.DB) {
		if err := apps.New(sqlDB, access.New(sqlDB, nil)).EnableReminders(o.Scheduler); err != nil {
			t.Fatal(err)
		}
		key, err := serverkey.Load(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		inbox, err = notifications.New(context.Background(), sqlDB, access.New(sqlDB, nil), notifications.Options{Key: key})
		if err != nil {
			t.Fatal(err)
		}
		o.Notifications = inbox
	})
	teamPath := fmt.Sprintf("/api/workspaces/%d/spaces/%d", s.one, s.team)
	definition := map[string]any{
		"components": []any{map[string]any{"id": "root", "component": "Text", "text": "Dues"}},
		"collection": map[string]any{"fields": []any{
			map[string]any{"name": "title", "type": "string"},
			map[string]any{"name": "due", "type": "date"},
		}},
		"actions":   map[string]any{"add": map[string]any{"kind": "insert"}},
		"reminders": []any{map[string]any{"name": "due", "date": "due", "title": "Pay {{title}}"}},
	}
	launch := func(cookie, spacePath string) (int64, string) {
		t.Helper()
		code, created := s.browser(cookie, http.MethodPost, spacePath+"/apps", map[string]any{"name": "Dues", "definition": definition})
		if code != http.StatusCreated {
			t.Fatalf("create app = %d %v", code, created)
		}
		path := fmt.Sprintf("%s/apps/%d", spacePath, id(created["id"]))
		if code, body := s.browser(cookie, http.MethodPost, path+"/publish", map[string]any{"version": created["version"], "acknowledge": []any{}}); code != http.StatusOK {
			t.Fatalf("publish = %d %v", code, body)
		}
		return id(created["id"]), path
	}
	app, appPath := launch(s.alice.cookie, teamPath)
	due := time.Now().UTC().AddDate(0, 0, 3).Format(time.DateOnly)
	addTo := func(cookie string, app int64, appPath string) {
		t.Helper()
		body := apps.ClientAction{Name: "add", SurfaceID: apps.SurfaceID(app), SourceComponentID: "add", Context: map[string]any{"title": "rent", "due": due}}
		if code, got := s.browser(cookie, http.MethodPost, appPath+"/actions", body); code != http.StatusOK {
			t.Fatalf("add = %d %v", code, got)
		}
	}

	add := func(cookie string) { addTo(cookie, app, appPath) }
	add(s.alice.cookie)
	if n := s.appReminderJobs(s.alice.id); n != 1 {
		t.Fatalf("%d reminder jobs after a record was saved, want 1", n)
	}

	before := s.appReminderRunAt(s.alice.id)
	if code, body := s.browser(s.alice.cookie, http.MethodPatch, "/api/me", map[string]any{"time_zone": "Asia/Tokyo"}); code != http.StatusOK {
		t.Fatalf("time zone = %d %v", code, body)
	}
	if after := s.appReminderRunAt(s.alice.id); after == before {
		t.Fatalf("the job stays at %s after its creator's time zone changed", before)
	}

	if code, body := s.browser(s.alice.cookie, http.MethodPatch, teamPath, map[string]any{"archived": true}); code != http.StatusOK {
		t.Fatalf("archive = %d %v", code, body)
	}
	if n := s.appReminderJobs(s.alice.id); n != 0 {
		t.Fatalf("%d reminder jobs in an archived space", n)
	}
	if code, body := s.browser(s.alice.cookie, http.MethodPatch, teamPath, map[string]any{"archived": false}); code != http.StatusOK {
		t.Fatalf("restore = %d %v", code, body)
	}
	if n := s.appReminderJobs(s.alice.id); n != 1 {
		t.Fatalf("%d reminder jobs once the space is back, want 1", n)
	}

	add(s.bob.cookie)
	if n := s.appReminderJobs(s.bob.id); n != 1 {
		t.Fatalf("%d jobs for bob, want 1", n)
	}
	members := teamPath + "/members"
	if code, body := s.browser(s.alice.cookie, http.MethodDelete, fmt.Sprintf("%s/%d", members, s.bob.id), nil); code != http.StatusNoContent {
		t.Fatalf("remove = %d %v", code, body)
	}
	if n := s.appReminderJobs(s.bob.id); n != 0 {
		t.Fatalf("%d jobs for a member who left", n)
	}
	if code, body := s.browser(s.alice.cookie, http.MethodPost, members, map[string]any{"user_id": s.bob.id, "role": "editor"}); code != http.StatusNoContent {
		t.Fatalf("add back = %d %v", code, body)
	}
	if n := s.appReminderJobs(s.bob.id); n != 1 {
		t.Fatalf("%d jobs for a member added back, want 1", n)
	}

	bobApp, bobPath := launch(s.bob.cookie, fmt.Sprintf("/api/workspaces/%d/spaces/%d", s.one, s.bob.private))
	addTo(s.bob.cookie, bobApp, bobPath)
	inPrivate := func() int64 {
		return s.scalar(`SELECT count(*) FROM scheduled_jobs WHERE kind = ? AND user_id = ? AND space_id = ?`,
			string(apps.KindRecordReminder), s.bob.id, s.bob.private)
	}
	if n := inPrivate(); n != 1 {
		t.Fatalf("%d jobs in bob's private space, want 1", n)
	}
	if code, body := s.browser(s.alice.cookie, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", s.one, s.bob.id), nil); code != http.StatusNoContent {
		t.Fatalf("remove from the workspace = %d %v", code, body)
	}
	if n := inPrivate(); n != 0 {
		t.Fatalf("%d jobs in the private space of a member who left the workspace", n)
	}
	code, inv := s.browser(s.alice.cookie, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", s.one), map[string]any{"email": "bob@example.com"})
	if code != http.StatusCreated {
		t.Fatalf("invite = %d %v", code, inv)
	}
	if code, body := s.browser(s.bob.cookie, http.MethodPost, "/api/invites/accept", map[string]any{"token": inv["token"]}); code != http.StatusOK {
		t.Fatalf("accept = %d %v", code, body)
	}
	if n := inPrivate(); n != 1 {
		t.Fatalf("%d jobs in the private space of a member back in the workspace, want 1", n)
	}

	var record int64
	if err := s.db.QueryRow(`SELECT id FROM app_rows WHERE app_id = ? AND created_by = ?`, app, s.alice.id).Scan(&record); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	err = notifications.Add(context.Background(), tx, notifications.Notice{
		UserID: s.alice.id, WorkspaceID: s.one, Kind: notifications.KindAppReminder,
		Ref: notifications.Ref{Space: s.team, App: app, Record: record, Reminder: "due"}, DedupeKey: "wired",
	})
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rec := s.request(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/notifications", s.one), nil, sameOrigin(s.alice.cookie))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Pay rent") {
		t.Fatalf("inbox = %d %s, want the reminder's text filled from the record", rec.Code, rec.Body)
	}
}
