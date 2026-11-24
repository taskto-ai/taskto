package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func auditPath(ws int64, query string) string {
	p := fmt.Sprintf("/api/workspaces/%d/audit", ws)
	if query != "" {
		p += "?" + query
	}
	return p
}

// eventIDs lists the ids of a reply's events, newest first.
func eventIDs(t *testing.T, body map[string]any) []int64 {
	t.Helper()
	list, ok := body["events"].([]any)
	if !ok {
		t.Fatalf("no events in %v", body)
	}
	var ids []int64
	for _, e := range list {
		ids = append(ids, id(e.(map[string]any)["id"]))
	}
	return ids
}

func TestAuditLogIsTheAdminsOwnWorkspacesOnly(t *testing.T) {
	s := newStack(t)
	// Alice administers both workspaces. Each has an event of its own, and one belongs to no
	// workspace (a login), which is nobody's to read as a workspace log.
	s.exec(`INSERT INTO audit_events (workspace_id, actor_id, action) VALUES (?, ?, 'workspace_updated')`, s.two, s.alice.id)
	secondOnly := s.scalar(`SELECT max(id) FROM audit_events`)
	s.exec(`INSERT INTO audit_events (workspace_id, actor_id, action, target_type, target_id) VALUES (NULL, ?, 'login_succeeded', 'user', ?)`, s.bob.id, s.bob.id)
	login := s.scalar(`SELECT max(id) FROM audit_events`)

	code, body := s.browser(s.alice.cookie, http.MethodGet, auditPath(s.one, "limit=200"), nil)
	if code != http.StatusOK {
		t.Fatalf("workspace one: %d %v", code, body)
	}
	ids := eventIDs(t, body)
	if len(ids) == 0 {
		t.Fatal("the setup and the invites left no events in workspace one")
	}
	for _, e := range body["events"].([]any) {
		if e.(map[string]any)["workspace_id"] != float64(s.one) {
			t.Fatalf("an event of another workspace in the log of one: %v", e)
		}
	}
	for _, other := range []int64{secondOnly, login} {
		for _, got := range ids {
			if got == other {
				t.Fatalf("event %d, not of workspace one, is in its log", other)
			}
		}
	}

	code, body = s.browser(s.alice.cookie, http.MethodGet, auditPath(s.two, ""), nil)
	if got := eventIDs(t, body); code != http.StatusOK || len(got) != 1 || got[0] != secondOnly {
		t.Fatalf("workspace two: %d %v", code, body)
	}

	// No parameter widens it: the workspace is the one the caller administers, whatever else the
	// request names.
	for _, q := range []string{"workspace=" + fmt.Sprint(s.two), "workspace_id=" + fmt.Sprint(s.two), "workspace_id=0", "workspace=0", "actor=" + fmt.Sprint(s.bob.id) + "&workspace_id=0"} {
		code, body := s.browser(s.alice.cookie, http.MethodGet, auditPath(s.one, q+"&limit=200"), nil)
		if code != http.StatusOK && code != http.StatusBadRequest {
			t.Fatalf("%s: %d %v", q, code, body)
		}
		if code == http.StatusOK {
			for _, got := range eventIDs(t, body) {
				if got == secondOnly || got == login {
					t.Fatalf("%s reached event %d of another workspace", q, got)
				}
			}
		}
	}
}

func TestAuditLogRefusesMembersOutsidersAndTokens(t *testing.T) {
	s := newStack(t)
	// bob is a member of one, not an admin; he isn't in two at all.
	if code, body := s.browser(s.bob.cookie, http.MethodGet, auditPath(s.one, ""), nil); code != http.StatusForbidden || body["code"] != "admin_only" || body["events"] != nil {
		t.Fatalf("member: %d %v", code, body)
	}
	if code, body := s.browser(s.bob.cookie, http.MethodGet, auditPath(s.two, ""), nil); code != http.StatusNotFound || body["events"] != nil {
		t.Fatalf("outsider: %d %v", code, body)
	}
	if code, body := s.browser("", http.MethodGet, auditPath(s.one, ""), nil); code != http.StatusUnauthorized || body["events"] != nil {
		t.Fatalf("no session: %d %v", code, body)
	}
	// An admin's access token doesn't read the log: it is a browser's view.
	token, _ := s.newToken(s.alice.cookie, s.one, "ci")
	if code, body := s.api(token, http.MethodGet, auditPath(s.one, ""), nil); code != http.StatusForbidden || body["events"] != nil {
		t.Fatalf("token: %d %v", code, body)
	}
	// An admin of one who is a plain member of the other, after a demotion.
	s.exec(`UPDATE memberships SET role = 'member' WHERE workspace_id = ? AND user_id = ?`, s.two, s.alice.id)
	if code, body := s.browser(s.alice.cookie, http.MethodGet, auditPath(s.two, ""), nil); code != http.StatusForbidden || body["events"] != nil {
		t.Fatalf("demoted: %d %v", code, body)
	}
}

func TestAuditLogFiltersAndPagesAndNamesWhoDidIt(t *testing.T) {
	s := newStack(t)
	// The setup already made events: alice invited bob and carol, and made two shared spaces.
	code, all := s.browser(s.alice.cookie, http.MethodGet, auditPath(s.one, "limit=200"), nil)
	if code != http.StatusOK {
		t.Fatalf("%d %v", code, all)
	}
	total := len(eventIDs(t, all))

	code, body := s.browser(s.alice.cookie, http.MethodGet, auditPath(s.one, "action=space_created"), nil)
	if code != http.StatusOK || len(eventIDs(t, body)) != 2 {
		t.Fatalf("action filter: %d %v", code, body)
	}
	first := body["events"].([]any)[0].(map[string]any)
	if first["action"] != "space_created" || first["actor_id"] != float64(s.alice.id) || first["target_type"] != "space" {
		t.Fatalf("event = %v", first)
	}
	// The names are read now: the actor, and the space the event is about.
	if actor, _ := first["actor"].(map[string]any); actor["name"] != "Alice" || actor["email"] != "alice@example.com" {
		t.Fatalf("actor = %v", first["actor"])
	}
	if first["target_name"] != "Team" {
		t.Fatalf("target_name = %v", first["target_name"])
	}
	s.exec(`UPDATE spaces SET name = 'Renamed' WHERE id = ?`, s.team)
	_, body = s.browser(s.alice.cookie, http.MethodGet, auditPath(s.one, "action=space_created&limit=1"), nil)
	if got := body["events"].([]any)[0].(map[string]any)["target_name"]; got != "Renamed" {
		t.Fatalf("a name is read when the log is, not when the event was recorded: %v", got)
	}

	if code, body := s.browser(s.alice.cookie, http.MethodGet, auditPath(s.one, "actor="+fmt.Sprint(s.bob.id)), nil); code != http.StatusOK || len(eventIDs(t, body)) != 1 || body["events"].([]any)[0].(map[string]any)["action"] != "invite_accepted" {
		t.Fatalf("actor filter: %d %v", code, body)
	}
	if code, body := s.browser(s.alice.cookie, http.MethodGet, auditPath(s.one, "target_type=invite"), nil); code != http.StatusOK || len(eventIDs(t, body)) == 0 {
		t.Fatalf("target filter: %d %v", code, body)
	}
	future := url.QueryEscape("2999-01-01T00:00:00Z")
	if code, body := s.browser(s.alice.cookie, http.MethodGet, auditPath(s.one, "from="+future), nil); code != http.StatusOK || len(eventIDs(t, body)) != 0 {
		t.Fatalf("from: %d %v", code, body)
	}
	if code, body := s.browser(s.alice.cookie, http.MethodGet, auditPath(s.one, "to="+future), nil); code != http.StatusOK || len(eventIDs(t, body)) != total {
		t.Fatalf("to: %d %v", code, body)
	}

	// Pages follow each other without gaps or repeats, and the last has no cursor.
	var paged []int64
	cursor := ""
	for range total + 1 {
		q := "limit=2"
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		code, body := s.browser(s.alice.cookie, http.MethodGet, auditPath(s.one, q), nil)
		if code != http.StatusOK {
			t.Fatalf("page: %d %v", code, body)
		}
		paged = append(paged, eventIDs(t, body)...)
		cursor, _ = body["next"].(string)
		if cursor == "" {
			break
		}
	}
	if fmt.Sprint(paged) != fmt.Sprint(eventIDs(t, all)) {
		t.Fatalf("pages %v, all %v", paged, eventIDs(t, all))
	}
}

func TestAuditLogRefusesWhatItDoesNotKnow(t *testing.T) {
	s := newStack(t)
	for _, q := range []string{
		"action=nonsense", "action=%27+OR+1%3D1", "target_type=nonsense", "actor=x", "actor=-1", "actor=0", "limit=0", "limit=201", "limit=x",
		"from=yesterday", "to=2026-01-01", "cursor=nope", "cursor=" + url.QueryEscape("MjAyNi0wMS0wMVQwMDowMDowMC4wMDBafHg"),
	} {
		code, body := s.browser(s.alice.cookie, http.MethodGet, auditPath(s.one, q), nil)
		if code != http.StatusBadRequest || body["code"] != "bad_request" || body["events"] != nil {
			t.Errorf("%s: %d %v", q, code, body)
		}
	}
}

func TestAuditLogDoesNotNameWhoLeft(t *testing.T) {
	s := newStack(t)
	// carol is removed: her events stay, her name is no longer shown, her id is.
	if code, body := s.browser(s.alice.cookie, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", s.one, s.carol.id), nil); code != http.StatusNoContent {
		t.Fatalf("remove = %d %v", code, body)
	}
	_, body := s.browser(s.alice.cookie, http.MethodGet, auditPath(s.one, "action=member_removed"), nil)
	list := body["events"].([]any)
	if len(list) != 1 {
		t.Fatalf("events = %v", body)
	}
	e := list[0].(map[string]any)
	if e["target_id"] != float64(s.carol.id) || e["target_user"] != nil || e["actor"] == nil {
		t.Fatalf("event = %v", e)
	}
	if strings.Contains(fmt.Sprint(body), "carol@example.com") || strings.Contains(fmt.Sprint(body), "Carol") {
		t.Fatalf("a former member's name or email is in the log's reply: %v", body)
	}
}
