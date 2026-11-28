package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func (s *stack) afterAdmission(fn func()) {
	var once sync.Once
	run := func() { once.Do(fn) }
	s.hooks.BeforeBegin(run)
	s.hooks.BeforeQuery("FROM skills", run)
}

func (s *stack) removeFromWorkspace(user int64) {
	s.t.Helper()
	s.exec(`DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, s.one, user)
}

func TestAWorkspaceSkillWriteOfAnAdminDemotedAfterAdmissionIsRefused(t *testing.T) {
	s := newStack(t)
	wide := s.workspaceSkills(s.one, "")
	made := s.makeSkill(s.alice.cookie, wide, "house-style")
	one := s.workspaceSkills(s.one, fmt.Sprintf("/%d", made))
	events := s.auditCount("skill_created") + s.auditCount("skill_updated") + s.auditCount("skill_removed")

	s.afterAdmission(func() { s.demote(s.alice.id) })
	s.refused(s.alice.cookie, http.MethodPost, wide, skillBody("late"), http.StatusForbidden, "admin_only")
	s.exec(`UPDATE memberships SET role = 'admin' WHERE workspace_id = ? AND user_id = ?`, s.one, s.alice.id)
	s.afterAdmission(func() { s.demote(s.alice.id) })
	s.refused(s.alice.cookie, http.MethodPatch, one, map[string]any{"title": "Taken"}, http.StatusForbidden, "admin_only")
	s.exec(`UPDATE memberships SET role = 'admin' WHERE workspace_id = ? AND user_id = ?`, s.one, s.alice.id)
	s.afterAdmission(func() { s.demote(s.alice.id) })
	s.refused(s.alice.cookie, http.MethodDelete, one, nil, http.StatusForbidden, "admin_only")

	if n := s.scalar(`SELECT count(*) FROM skills WHERE workspace_id = ? AND (name = 'late' OR title = 'Taken')`, s.one); n != 0 {
		t.Fatalf("%d skills made or changed by an admin who was demoted", n)
	}
	if n := s.scalar(`SELECT count(*) FROM skills WHERE id = ?`, made); n != 1 {
		t.Fatal("the skill was deleted by an admin who was demoted")
	}
	if got := s.auditCount("skill_created") + s.auditCount("skill_updated") + s.auditCount("skill_removed"); got != events {
		t.Fatalf("%d audit events, want %d: a refused write recorded one", got, events)
	}
}

func TestAWorkspaceSkillWriteOfAnAdminRemovedAfterAdmissionIsRefused(t *testing.T) {
	s := newStack(t)
	admin := s.invite("dana@example.com", "Dana")
	s.exec(`UPDATE memberships SET role = 'admin' WHERE workspace_id = ? AND user_id = ?`, s.one, admin.id)
	s.afterAdmission(func() { s.removeFromWorkspace(admin.id) })
	s.refused(admin.cookie, http.MethodPost, s.workspaceSkills(s.one, ""), skillBody("late"), http.StatusNotFound, "workspace_not_found")
	if n := s.scalar(`SELECT count(*) FROM skills`); n != 0 {
		t.Fatalf("%d skills made by an admin who was removed", n)
	}
	if n := s.auditCount("skill_created"); n != 0 {
		t.Fatalf("%d audit events", n)
	}
}

func TestAPersonalSkillIsNotMadeByAPersonRemovedAfterAdmission(t *testing.T) {
	s := newStack(t)
	s.afterAdmission(func() { s.removeFromWorkspace(s.carol.id) })
	s.refused(s.carol.cookie, http.MethodPost, s.mySkills(s.one, ""), skillBody("late"), http.StatusNotFound, "workspace_not_found")
	s.exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, s.one, s.carol.id)
	if n := s.scalar(`SELECT count(*) FROM skills WHERE user_id = ?`, s.carol.id); n != 0 {
		t.Fatalf("%d personal skills of a person who had left", n)
	}
	if got := s.skillNames(s.carol.cookie, s.mySkills(s.one, "")); len(got) != 0 {
		t.Fatalf("coming back brings the skill made while leaving: %v", got)
	}
}

func TestSkillReadsUseTheAccessOfTheMomentOfTheRead(t *testing.T) {
	s := newStack(t)
	wide := s.workspaceSkills(s.one, "")
	s.makeSkill(s.alice.cookie, wide, "house-style")
	draft := s.makeSkill(s.alice.cookie, wide, "draft-only")
	if code, _ := s.browser(s.alice.cookie, http.MethodPatch, s.workspaceSkills(s.one, fmt.Sprintf("/%d", draft)), map[string]any{"enabled": false}); code != http.StatusOK {
		t.Fatalf("disable = %d", code)
	}
	s.makeSkill(s.bob.cookie, s.spaceSkills(s.one, s.team, ""), "team-way")
	s.exec(`INSERT INTO space_members (space_id, user_id, role) VALUES (?, ?, 'viewer')`, s.team, s.carol.id)

	s.afterAdmission(func() { s.demote(s.alice.id) })
	if got := s.skillNames(s.alice.cookie, wide); !sameNames(got, "house-style") {
		t.Errorf("an admin demoted after admission lists %v", got)
	}
	s.exec(`UPDATE memberships SET role = 'admin' WHERE workspace_id = ? AND user_id = ?`, s.one, s.alice.id)

	s.afterAdmission(func() { s.removeFromWorkspace(s.bob.id) })
	s.refused(s.bob.cookie, http.MethodGet, wide, nil, http.StatusNotFound, "workspace_not_found")
	s.exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, s.one, s.bob.id)

	s.afterAdmission(func() {
		s.exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, s.team, s.carol.id)
	})
	s.refused(s.carol.cookie, http.MethodGet, s.spaceSkills(s.one, s.team, ""), nil, http.StatusNotFound, "space_not_found")

	s.afterAdmission(func() { s.removeFromWorkspace(s.carol.id) })
	s.refused(s.carol.cookie, http.MethodGet, s.mySkills(s.one, ""), nil, http.StatusNotFound, "workspace_not_found")
}

func TestTheAuditLogNeverShowsARestrictedSpaceSkillsTitleToAnAdminOutsideTheSpace(t *testing.T) {
	s := newStack(t)
	secret := s.spaceSkills(s.one, s.secret, "")
	body := skillBody("launch-plan")
	body["title"] = "Restricted launch plan"
	if code, reply := s.browser(s.carol.cookie, http.MethodPost, secret, body); code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, reply)
	}
	s.refused(s.alice.cookie, http.MethodGet, secret, nil, http.StatusNotFound, "space_not_found")

	rec := s.request(http.MethodGet, auditPath(s.one, "action=skill_created"), nil, sameOrigin(s.alice.cookie))
	if rec.Code != http.StatusOK {
		t.Fatalf("audit = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "Restricted launch plan") {
		t.Fatalf("an admin outside the space reads the title: %s", rec.Body)
	}
	events := decode(rec)["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("events = %v", events)
	}
	e := events[0].(map[string]any)
	if e["target_name"] != nil {
		t.Fatalf("target_name = %v", e["target_name"])
	}
	if d := e["details"].(map[string]any); d["name"] != "launch-plan" || id(d["space_id"]) != s.secret {
		t.Fatalf("details = %v", d)
	}
}

func escaped(text string) string {
	var b strings.Builder
	for _, r := range text {
		fmt.Fprintf(&b, `\u%04x`, r)
	}
	return b.String()
}

func TestASkillWithAllItsFieldsAtTheirLimitsEscapedInJSONIsAccepted(t *testing.T) {
	s := newStack(t)
	path := s.mySkills(s.one, "")
	pair := fmt.Sprintf(`\u%04x\u%04x`, 0xd83d, 0xde00)
	body := json.RawMessage(fmt.Sprintf(`{"name":"%s","title":"%s","description":"%s","body":"%s"}`,
		escaped("a"+strings.Repeat("b", 39)),
		strings.Repeat(pair, 100), strings.Repeat(pair, 300), escaped(strings.Repeat("a", 32*1024))))
	if len(body) <= 160*1024 {
		t.Fatalf("the request is %d bytes: the test needs a request near the worst case", len(body))
	}
	code, reply := s.browser(s.bob.cookie, http.MethodPost, path, body)
	if code != http.StatusCreated {
		t.Fatalf("a valid skill of %d bytes of JSON = %d %v", len(body), code, reply)
	}
	if got := reply["body"].(string); len(got) != 32*1024 || strings.Trim(got, "a") != "" {
		t.Fatalf("the body came back as %d bytes", len(got))
	}

	tooMuch := json.RawMessage(fmt.Sprintf(`{"name":"quote","title":"T","description":"D","body":"%s"}`, strings.Repeat("b", 300*1024)))
	if code, reply := s.browser(s.bob.cookie, http.MethodPost, path, tooMuch); code != http.StatusBadRequest || reply["code"] != "invalid_json" {
		t.Fatalf("a request of 300 KiB = %d %v", code, reply)
	}
}

func TestAnExportedSkillKeepsItsInstructionsByteForByte(t *testing.T) {
	s := newStack(t)
	path := s.mySkills(s.one, "")
	bodies := map[string]string{
		"three-newlines": "# Steps\n\nDo it.\n\n\n",
		"no-newline":     "Do it",
		"crlf":           "one\r\ntwo\r\n",
		"leading-blank":  "\n\nText\n",
	}
	for name, text := range bodies {
		in := skillBody(name)
		in["body"] = text
		if code, reply := s.browser(s.bob.cookie, http.MethodPost, path, in); code != http.StatusCreated || reply["body"] != text {
			t.Fatalf("create %s = %d %v", name, code, reply)
		}
	}
	rec := s.export(s.bob.cookie, s.one)
	if rec.Code != http.StatusOK {
		t.Fatalf("export = %d", rec.Code)
	}
	files := unzip(t, rec)
	for name, text := range bodies {
		front := fmt.Sprintf("---\nname: %q\ntitle: %q\ndescription: %q\nenabled: true\n---\n\n", name, "Title of "+name, "when to use "+name)
		if got := string(files["skills/"+name+".md"]); got != front+text {
			t.Errorf("%s.md = %q, want %q", name, got, front+text)
		}
	}
}
