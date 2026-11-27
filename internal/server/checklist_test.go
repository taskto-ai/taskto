package server

import (
	"fmt"
	"net/http"
	"testing"

	"taskto.ai/taskto/internal/mail"
)

func checklistPath(ws int64, tail string) string {
	return fmt.Sprintf("/api/workspaces/%d/checklist%s", ws, tail)
}

func steps(t *testing.T, body map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	items, _ := body["items"].([]any)
	for _, it := range items {
		m := it.(map[string]any)
		out[m["id"].(string)], _ = m["state"].(string)
	}
	if len(out) != 6 {
		t.Fatalf("the checklist's steps: %v", body)
	}
	return out
}

func TestChecklistIsWorkedOutFromWhatExists(t *testing.T) {
	s := newStackWith(t, func(o *Options) { o.Mail = mail.NewService(&recorder{}, testInfo) })
	code, body := s.browser(s.alice.cookie, http.MethodGet, checklistPath(s.one, ""), nil)
	if code != http.StatusOK || body["dismissed"] != false || body["complete"] != false {
		t.Fatalf("one: %d %v", code, body)
	}
	got := steps(t, body)
	want := map[string]string{"invite": "done", "provider": "todo", "space": "done", "app": "todo", "email": "done", "backups": "todo"}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("one: %s = %s, want %s (%v)", k, got[k], v, got)
		}
	}
	code, body = s.browser(s.alice.cookie, http.MethodGet, checklistPath(s.two, ""), nil)
	if code != http.StatusOK {
		t.Fatalf("two: %d %v", code, body)
	}
	got = steps(t, body)
	for k, v := range map[string]string{"invite": "todo", "provider": "todo", "space": "todo", "app": "todo", "email": "done", "backups": "todo"} {
		if got[k] != v {
			t.Fatalf("two: %s = %s, want %s (%v)", k, got[k], v, got)
		}
	}

	s.exec(`INSERT INTO provider_accounts (workspace_id, name, display_name, kind, base_url, models)
		VALUES (?, 'p', 'P', 'openai-compatible', 'https://models.example.com/v1', '[]')`, s.two)
	_, body = s.browser(s.alice.cookie, http.MethodGet, checklistPath(s.two, ""), nil)
	if got = steps(t, body); got["provider"] != "done" {
		t.Fatalf("two after a provider: %v", got)
	}
	_, body = s.browser(s.alice.cookie, http.MethodGet, checklistPath(s.one, ""), nil)
	if got = steps(t, body); got["provider"] != "todo" {
		t.Fatalf("a provider of workspace two marked workspace one: %v", got)
	}
}

func TestChecklistWithoutAnEmailServerOrBackups(t *testing.T) {
	s := newStack(t)
	_, body := s.browser(s.alice.cookie, http.MethodGet, checklistPath(s.one, ""), nil)
	if got := steps(t, body); got["email"] != "todo" || got["backups"] != "todo" {
		t.Fatalf("%v", got)
	}
}

func TestChecklistDismissAndSkipAreAdminOnly(t *testing.T) {
	s := newStack(t)
	for _, c := range []struct{ method, tail string }{
		{http.MethodGet, ""},
		{http.MethodPut, "/dismissed"},
		{http.MethodDelete, "/dismissed"},
		{http.MethodPut, "/skipped/provider"},
		{http.MethodDelete, "/skipped/provider"},
	} {
		code, body := s.browser(s.bob.cookie, c.method, checklistPath(s.one, c.tail), nil)
		if code != http.StatusForbidden || body["code"] != "admin_only" || body["items"] != nil {
			t.Fatalf("member %s %s: %d %v", c.method, c.tail, code, body)
		}
		code, body = s.browser(s.bob.cookie, c.method, checklistPath(s.two, c.tail), nil)
		if code != http.StatusNotFound || body["code"] != "workspace_not_found" || body["items"] != nil {
			t.Fatalf("outsider %s %s: %d %v", c.method, c.tail, code, body)
		}
		if code, _ := s.browser("", c.method, checklistPath(s.one, c.tail), nil); code != http.StatusUnauthorized {
			t.Fatalf("signed out %s %s: %d", c.method, c.tail, code)
		}
	}
	if n := s.scalar(`SELECT (SELECT COUNT(*) FROM setup_checklist_dismissed) + (SELECT COUNT(*) FROM setup_checklist_skipped)`); n != 0 {
		t.Fatalf("%d marks were made by people who may not", n)
	}
}

func TestChecklistChoicesBelongToTheirWorkspace(t *testing.T) {
	s := newStack(t)
	if code, body := s.browser(s.alice.cookie, http.MethodPut, checklistPath(s.one, "/dismissed"), nil); code != http.StatusNoContent {
		t.Fatalf("dismiss: %d %v", code, body)
	}
	if code, body := s.browser(s.alice.cookie, http.MethodPut, checklistPath(s.one, "/skipped/provider"), nil); code != http.StatusNoContent {
		t.Fatalf("skip: %d %v", code, body)
	}
	_, body := s.browser(s.alice.cookie, http.MethodGet, checklistPath(s.one, ""), nil)
	if body["dismissed"] != true || steps(t, body)["provider"] != "skipped" {
		t.Fatalf("one: %v", body)
	}
	_, body = s.browser(s.alice.cookie, http.MethodGet, checklistPath(s.two, ""), nil)
	if body["dismissed"] != false || steps(t, body)["provider"] != "todo" {
		t.Fatalf("two: %v", body)
	}
	if code, _ := s.browser(s.alice.cookie, http.MethodDelete, checklistPath(s.one, "/dismissed"), nil); code != http.StatusNoContent {
		t.Fatalf("restore: %d", code)
	}
	if code, _ := s.browser(s.alice.cookie, http.MethodDelete, checklistPath(s.one, "/skipped/provider"), nil); code != http.StatusNoContent {
		t.Fatalf("unskip: %d", code)
	}
	_, body = s.browser(s.alice.cookie, http.MethodGet, checklistPath(s.one, ""), nil)
	if body["dismissed"] != false || steps(t, body)["provider"] != "todo" {
		t.Fatalf("one again: %v", body)
	}
	code, body := s.browser(s.alice.cookie, http.MethodPut, checklistPath(s.one, "/skipped/nothing"), nil)
	if code != http.StatusNotFound || body["code"] != "not_found" {
		t.Fatalf("unknown step: %d %v", code, body)
	}
}

func TestChecklistIgnoresAMembersPrivateApps(t *testing.T) {
	s := newStack(t)
	s.app(s.bob.cookie, s.bob.private, "Notes")
	_, body := s.browser(s.alice.cookie, http.MethodGet, checklistPath(s.one, ""), nil)
	if got := steps(t, body); got["app"] != "todo" {
		t.Fatalf("%v", got)
	}
	s.app(s.bob.cookie, s.team, "Shared notes")
	_, body = s.browser(s.alice.cookie, http.MethodGet, checklistPath(s.one, ""), nil)
	if got := steps(t, body); got["app"] != "done" {
		t.Fatalf("%v", got)
	}
}

func TestChecklistIsRefusedToAccessTokens(t *testing.T) {
	s := newStack(t)
	token, _ := s.newToken(s.alice.cookie, s.one, "ci")
	for _, c := range []struct{ method, tail string }{
		{http.MethodGet, ""}, {http.MethodPut, "/dismissed"}, {http.MethodPut, "/skipped/invite"},
	} {
		code, body := s.api(token, c.method, checklistPath(s.one, c.tail), nil)
		if code != http.StatusForbidden || body["code"] != "token_refused" || body["items"] != nil {
			t.Fatalf("%s %s: %d %v", c.method, c.tail, code, body)
		}
	}
	if n := s.scalar(`SELECT (SELECT COUNT(*) FROM setup_checklist_dismissed) + (SELECT COUNT(*) FROM setup_checklist_skipped)`); n != 0 {
		t.Fatalf("a token made %d marks", n)
	}
}

func TestChecklistListsServerStepsOnlyForOperators(t *testing.T) {
	s := newStackWith(t, func(o *Options) {
		o.Auth.WorkspaceCreation = "anyone"
		o.Mail = mail.NewService(&recorder{}, testInfo)
	})
	bobs := s.createWorkspace(s.bob.cookie, "Bob's")
	code, body := s.browser(s.bob.cookie, http.MethodGet, checklistPath(bobs, ""), nil)
	if code != http.StatusOK {
		t.Fatalf("bob's checklist = %d %v", code, body)
	}
	items, _ := body["items"].([]any)
	var ids []string
	for _, it := range items {
		ids = append(ids, it.(map[string]any)["id"].(string))
	}
	if fmt.Sprint(ids) != "[invite provider space app]" {
		t.Fatalf("bob's steps = %v, want only the workspace's own", ids)
	}
	if body["complete"] != false {
		t.Fatalf("complete = %v", body["complete"])
	}
	_, body = s.browser(s.alice.cookie, http.MethodGet, checklistPath(s.one, ""), nil)
	if got := steps(t, body); got["email"] != "done" || got["backups"] != "todo" {
		t.Fatalf("the operator's steps = %v", got)
	}
}
