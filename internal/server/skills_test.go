package server

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func (s *stack) mySkills(ws int64, tail string) string {
	return fmt.Sprintf("/api/me/workspaces/%d/skills%s", ws, tail)
}

func (s *stack) spaceSkills(ws, space int64, tail string) string {
	return fmt.Sprintf("/api/workspaces/%d/spaces/%d/skills%s", ws, space, tail)
}

func (s *stack) workspaceSkills(ws int64, tail string) string {
	return fmt.Sprintf("/api/workspaces/%d/skills%s", ws, tail)
}

func skillBody(name string) map[string]any {
	return map[string]any{"name": name, "title": "Title of " + name, "description": "when to use " + name, "body": "# " + name + "\n\nDo it well."}
}

func (s *stack) makeSkill(cookie, path, name string) int64 {
	s.t.Helper()
	code, body := s.browser(cookie, http.MethodPost, path, skillBody(name))
	if code != http.StatusCreated {
		s.t.Fatalf("create skill %s at %s = %d %v", name, path, code, body)
	}
	return id(body["id"])
}

func (s *stack) skillNames(cookie, path string) []string {
	s.t.Helper()
	code, body := s.browser(cookie, http.MethodGet, path, nil)
	if code != http.StatusOK {
		s.t.Fatalf("list %s = %d %v", path, code, body)
	}
	out := []string{}
	for _, v := range body["skills"].([]any) {
		out = append(out, v.(map[string]any)["name"].(string))
	}
	return out
}

func (s *stack) refused(cookie, method, path string, body any, status int, code string) {
	s.t.Helper()
	got, reply := s.browser(cookie, method, path, body)
	if got != status || reply["code"] != code {
		s.t.Errorf("%s %s = %d %v, want %d %s", method, path, got, reply, status, code)
	}
}

func sameNames(got []string, want ...string) bool { return slices.Equal(got, want) }

func TestPersonalSkillsAreTheirOwnersAloneInTheirWorkspace(t *testing.T) {
	s := newStack(t)
	mine := s.makeSkill(s.bob.cookie, s.mySkills(s.one, ""), "closing")
	one := s.mySkills(s.one, fmt.Sprintf("/%d", mine))

	if got := s.skillNames(s.bob.cookie, s.mySkills(s.one, "")); !sameNames(got, "closing") {
		t.Fatalf("bob's skills = %v", got)
	}
	for name, cookie := range map[string]string{"carol": s.carol.cookie, "alice, an admin": s.alice.cookie} {
		if got := s.skillNames(cookie, s.mySkills(s.one, "")); len(got) != 0 {
			t.Errorf("%s lists %v", name, got)
		}
		s.refused(cookie, http.MethodPatch, one, map[string]any{"title": "Taken"}, http.StatusNotFound, "skill_not_found")
		s.refused(cookie, http.MethodDelete, one, nil, http.StatusNotFound, "skill_not_found")
		s.refused(cookie, http.MethodPatch, one, map[string]any{"enabled": false}, http.StatusNotFound, "skill_not_found")
	}
	s.refused(s.alice.cookie, http.MethodPatch, s.mySkills(s.two, fmt.Sprintf("/%d", mine)), map[string]any{"title": "Taken"}, http.StatusNotFound, "skill_not_found")
	s.refused(s.alice.cookie, http.MethodDelete, s.mySkills(s.two, fmt.Sprintf("/%d", mine)), nil, http.StatusNotFound, "skill_not_found")
	s.refused(s.bob.cookie, http.MethodGet, s.mySkills(s.two, ""), nil, http.StatusNotFound, "workspace_not_found")
	s.refused(s.bob.cookie, http.MethodPost, s.mySkills(s.two, ""), skillBody("elsewhere"), http.StatusNotFound, "workspace_not_found")
	s.refused("", http.MethodGet, s.mySkills(s.one, ""), nil, http.StatusUnauthorized, "not_logged_in")

	if got := s.scalar(`SELECT count(*) FROM skills WHERE id = ? AND title = 'Title of closing' AND enabled = 1`, mine); got != 1 {
		t.Fatal("another person's request changed the skill")
	}
	if got := s.skillNames(s.alice.cookie, s.mySkills(s.two, "")); len(got) != 0 {
		t.Fatalf("alice's skills of workspace two: %v", got)
	}

	code, body := s.browser(s.bob.cookie, http.MethodPatch, one, map[string]any{"title": "Closing the month", "enabled": false})
	if code != http.StatusOK || body["title"] != "Closing the month" || body["enabled"] != false || body["scope"] != "user" || body["space_id"] != nil {
		t.Fatalf("patch = %d %v", code, body)
	}
	if code, _ := s.browser(s.bob.cookie, http.MethodDelete, one, nil); code != http.StatusNoContent {
		t.Fatalf("delete = %d", code)
	}
	if n := s.auditCount("skill_created") + s.auditCount("skill_updated") + s.auditCount("skill_removed"); n != 0 {
		t.Fatalf("%d audit events for a personal skill", n)
	}
}

func TestSpaceSkillsAreReadByReadersAndChangedByWritersOnly(t *testing.T) {
	s := newStack(t)
	team := s.spaceSkills(s.one, s.team, "")
	made := s.makeSkill(s.bob.cookie, team, "team-way")
	one := s.spaceSkills(s.one, s.team, fmt.Sprintf("/%d", made))

	s.refused(s.carol.cookie, http.MethodGet, team, nil, http.StatusNotFound, "space_not_found")
	s.exec(`INSERT INTO space_members (space_id, user_id, role) VALUES (?, ?, 'viewer')`, s.team, s.carol.id)
	if got := s.skillNames(s.carol.cookie, team); !sameNames(got, "team-way") {
		t.Fatalf("a reader's list = %v", got)
	}
	s.refused(s.carol.cookie, http.MethodPost, team, skillBody("by-reader"), http.StatusForbidden, "space_read_only")
	s.refused(s.carol.cookie, http.MethodPatch, one, map[string]any{"title": "Taken"}, http.StatusForbidden, "space_read_only")
	s.refused(s.carol.cookie, http.MethodDelete, one, nil, http.StatusForbidden, "space_read_only")

	code, body := s.browser(s.bob.cookie, http.MethodPatch, one, map[string]any{"enabled": false})
	if code != http.StatusOK || body["enabled"] != false || id(body["space_id"]) != s.team {
		t.Fatalf("disable = %d %v", code, body)
	}
	if got := s.skillNames(s.carol.cookie, team); len(got) != 0 {
		t.Fatalf("a reader sees a disabled skill: %v", got)
	}
	if got := s.skillNames(s.bob.cookie, team); !sameNames(got, "team-way") {
		t.Fatalf("a writer's list = %v", got)
	}

	secret := s.spaceSkills(s.one, s.secret, "")
	hidden := s.makeSkill(s.carol.cookie, secret, "secret-way")
	hiddenPath := s.spaceSkills(s.one, s.secret, fmt.Sprintf("/%d", hidden))
	for _, cookie := range []string{s.alice.cookie, s.bob.cookie} {
		s.refused(cookie, http.MethodGet, secret, nil, http.StatusNotFound, "space_not_found")
		s.refused(cookie, http.MethodPost, secret, skillBody("sneaky"), http.StatusNotFound, "space_not_found")
		s.refused(cookie, http.MethodPatch, hiddenPath, map[string]any{"title": "Taken"}, http.StatusNotFound, "space_not_found")
		s.refused(cookie, http.MethodDelete, hiddenPath, nil, http.StatusNotFound, "space_not_found")
	}
	s.refused(s.carol.cookie, http.MethodPatch, s.spaceSkills(s.one, s.secret, fmt.Sprintf("/%d", made)), map[string]any{"title": "Taken"}, http.StatusNotFound, "skill_not_found")
	s.refused(s.bob.cookie, http.MethodDelete, s.spaceSkills(s.one, s.team, fmt.Sprintf("/%d", hidden)), nil, http.StatusNotFound, "skill_not_found")
	s.refused(s.alice.cookie, http.MethodGet, s.spaceSkills(s.two, s.team, ""), nil, http.StatusNotFound, "space_not_found")
	s.refused(s.alice.cookie, http.MethodPatch, s.spaceSkills(s.two, s.alicePrivate2, fmt.Sprintf("/%d", made)), map[string]any{"title": "Taken"}, http.StatusNotFound, "space_not_found")
	s.refused(s.bob.cookie, http.MethodGet, s.spaceSkills(s.two, s.team, ""), nil, http.StatusNotFound, "workspace_not_found")
	s.refused(s.bob.cookie, http.MethodGet, s.spaceSkills(s.one, s.bob.private, ""), nil, http.StatusNotFound, "space_not_found")
	s.refused(s.bob.cookie, http.MethodPost, s.spaceSkills(s.one, s.bob.private, ""), skillBody("private"), http.StatusNotFound, "space_not_found")

	if got := s.scalar(`SELECT count(*) FROM skills WHERE scope = 'space'`); got != 2 {
		t.Fatalf("%d space skills, want the two that were made", got)
	}
	if got := s.scalar(`SELECT count(*) FROM skills WHERE title = 'Taken' OR name IN ('sneaky', 'by-reader', 'private')`); got != 0 {
		t.Fatal("a refused request changed or made a skill")
	}

	if code, _ := s.browser(s.bob.cookie, http.MethodPatch, one, map[string]any{"enabled": true}); code != http.StatusOK {
		t.Fatalf("enable = %d", code)
	}
	s.exec(`UPDATE spaces SET archived_at = '2026-01-01T00:00:00.000Z' WHERE id = ?`, s.team)
	s.refused(s.bob.cookie, http.MethodPost, team, skillBody("late"), http.StatusForbidden, "space_archived")
	s.refused(s.bob.cookie, http.MethodPatch, one, map[string]any{"enabled": false}, http.StatusForbidden, "space_archived")
	if got := s.skillNames(s.bob.cookie, team); !sameNames(got, "team-way") {
		t.Fatalf("an archived space's skills are still readable: %v", got)
	}
}

func TestWorkspaceSkillsAreUsedByMembersAndChangedByAdminsOnly(t *testing.T) {
	s := newStack(t)
	wide := s.workspaceSkills(s.one, "")
	made := s.makeSkill(s.alice.cookie, wide, "house-style")
	draft := s.makeSkill(s.alice.cookie, wide, "draft-only")
	one := s.workspaceSkills(s.one, fmt.Sprintf("/%d", made))
	if code, _ := s.browser(s.alice.cookie, http.MethodPatch, s.workspaceSkills(s.one, fmt.Sprintf("/%d", draft)), map[string]any{"enabled": false}); code != http.StatusOK {
		t.Fatalf("disable = %d", code)
	}

	if got := s.skillNames(s.bob.cookie, wide); !sameNames(got, "house-style") {
		t.Fatalf("a member's list = %v", got)
	}
	if got := s.skillNames(s.alice.cookie, wide); !sameNames(got, "draft-only", "house-style") {
		t.Fatalf("an admin's list = %v", got)
	}
	s.refused(s.bob.cookie, http.MethodPost, wide, skillBody("by-member"), http.StatusForbidden, "admin_only")
	s.refused(s.bob.cookie, http.MethodPatch, one, map[string]any{"title": "Taken"}, http.StatusForbidden, "admin_only")
	s.refused(s.bob.cookie, http.MethodDelete, one, nil, http.StatusForbidden, "admin_only")

	if got := s.skillNames(s.alice.cookie, s.workspaceSkills(s.two, "")); len(got) != 0 {
		t.Fatalf("workspace two lists workspace one's skills: %v", got)
	}
	s.refused(s.alice.cookie, http.MethodPatch, s.workspaceSkills(s.two, fmt.Sprintf("/%d", made)), map[string]any{"title": "Taken"}, http.StatusNotFound, "skill_not_found")
	s.refused(s.alice.cookie, http.MethodDelete, s.workspaceSkills(s.two, fmt.Sprintf("/%d", made)), nil, http.StatusNotFound, "skill_not_found")
	s.refused(s.bob.cookie, http.MethodGet, s.workspaceSkills(s.two, ""), nil, http.StatusNotFound, "workspace_not_found")
	s.refused(s.alice.cookie, http.MethodPatch, s.mySkills(s.one, fmt.Sprintf("/%d", made)), map[string]any{"title": "Taken"}, http.StatusNotFound, "skill_not_found")
	s.refused(s.alice.cookie, http.MethodDelete, s.spaceSkills(s.one, s.team, fmt.Sprintf("/%d", made)), nil, http.StatusNotFound, "skill_not_found")

	if got := s.scalar(`SELECT count(*) FROM skills WHERE title = 'Taken' OR name = 'by-member'`); got != 0 {
		t.Fatal("a refused request changed or made a skill")
	}
	if code, _ := s.browser(s.alice.cookie, http.MethodDelete, one, nil); code != http.StatusNoContent {
		t.Fatalf("an admin's delete = %d", code)
	}
}

func TestSkillRoutesRefuseAccessTokens(t *testing.T) {
	s := newStack(t)
	token, _ := s.newToken(s.alice.cookie, s.one, "agent")
	for name, path := range map[string]string{
		"personal":  s.mySkills(s.one, ""),
		"space":     s.spaceSkills(s.one, s.team, ""),
		"workspace": s.workspaceSkills(s.one, ""),
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			var body any
			if method == http.MethodPost {
				body = skillBody("by-token")
			}
			if code, reply := s.api(token, method, path, body); code != http.StatusForbidden || reply["code"] != "token_refused" {
				t.Errorf("%s %s with a token = %d %v", method, name, code, reply)
			}
		}
	}
	if n := s.scalar(`SELECT count(*) FROM skills`); n != 0 {
		t.Fatalf("%d skills made with a token", n)
	}
}

func TestSkillRequestsAreChecked(t *testing.T) {
	s := newStack(t)
	path := s.mySkills(s.one, "")
	bad := map[string]struct {
		body map[string]any
		code string
	}{
		"a name with a capital":  {map[string]any{"name": "Quote", "title": "T", "description": "D", "body": "B"}, "skill_name_invalid"},
		"no title":               {map[string]any{"name": "quote", "title": "", "description": "D", "body": "B"}, "skill_title_length"},
		"a long description":     {map[string]any{"name": "quote", "title": "T", "description": strings.Repeat("d", 301), "body": "B"}, "skill_description_length"},
		"an empty body":          {map[string]any{"name": "quote", "title": "T", "description": "D", "body": ""}, "skill_body_length"},
		"a body over 32 KiB":     {map[string]any{"name": "quote", "title": "T", "description": "D", "body": strings.Repeat("b", 32*1024+1)}, "skill_body_length"},
		"a field it doesn't has": {map[string]any{"name": "quote", "title": "T", "description": "D", "body": "B", "origin": "gallery:x@1"}, "invalid_json"},
		"a scope of its own":     {map[string]any{"name": "quote", "title": "T", "description": "D", "body": "B", "scope": "workspace"}, "invalid_json"},
	}
	for name, c := range bad {
		code, reply := s.browser(s.bob.cookie, http.MethodPost, path, c.body)
		if code != http.StatusBadRequest || reply["code"] != c.code {
			t.Errorf("%s: %d %v, want 400 %s", name, code, reply, c.code)
		}
	}
	huge := skillBody("huge")
	huge["body"] = strings.Repeat("b", 200*1024)
	if code, reply := s.browser(s.bob.cookie, http.MethodPost, path, huge); code != http.StatusBadRequest || reply["code"] != "invalid_json" {
		t.Errorf("a request body of 200 KiB: %d %v", code, reply)
	}
	made := s.makeSkill(s.bob.cookie, path, "quote")
	s.refused(s.bob.cookie, http.MethodPost, path, skillBody("quote"), http.StatusConflict, "skill_name_taken")
	s.refused(s.bob.cookie, http.MethodPatch, path+fmt.Sprintf("/%d", made), map[string]any{}, http.StatusBadRequest, "bad_request")
	s.refused(s.bob.cookie, http.MethodPatch, path+"/abc", map[string]any{"title": "T"}, http.StatusNotFound, "skill_not_found")
	s.refused(s.bob.cookie, http.MethodPatch, path+"/0", map[string]any{"title": "T"}, http.StatusNotFound, "skill_not_found")
	s.refused(s.bob.cookie, http.MethodPatch, path+fmt.Sprintf("/%d", made), map[string]any{"scope": "workspace"}, http.StatusBadRequest, "invalid_json")
	if n := s.scalar(`SELECT count(*) FROM skills`); n != 1 {
		t.Fatalf("%d skills, want the one that was made", n)
	}

	rec := s.request(http.MethodPost, path, skillBody("form"), sameOrigin(s.bob.cookie), func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") })
	if rec.Code != http.StatusUnsupportedMediaType && rec.Code != http.StatusBadRequest {
		t.Fatalf("a post that isn't JSON = %d", rec.Code)
	}
}

func TestWorkspaceAndSpaceSkillChangesReachTheAuditLogAndNothingElseDoes(t *testing.T) {
	s := newStack(t)
	wide := s.makeSkill(s.alice.cookie, s.workspaceSkills(s.one, ""), "house-style")
	inTeam := s.makeSkill(s.bob.cookie, s.spaceSkills(s.one, s.team, ""), "team-way")
	s.makeSkill(s.bob.cookie, s.mySkills(s.one, ""), "bobs-own")
	s.exec(`INSERT INTO space_members (space_id, user_id, role) VALUES (?, ?, 'viewer')`, s.team, s.carol.id)
	s.refused(s.bob.cookie, http.MethodPost, s.workspaceSkills(s.one, ""), skillBody("by-member"), http.StatusForbidden, "admin_only")
	s.refused(s.carol.cookie, http.MethodPost, s.spaceSkills(s.one, s.team, ""), skillBody("by-reader"), http.StatusForbidden, "space_read_only")
	if code, _ := s.browser(s.bob.cookie, http.MethodPatch, s.spaceSkills(s.one, s.team, fmt.Sprintf("/%d", inTeam)), map[string]any{"body": "Changed instructions"}); code != http.StatusOK {
		t.Fatalf("patch = %d", code)
	}
	if code, _ := s.browser(s.alice.cookie, http.MethodDelete, s.workspaceSkills(s.one, fmt.Sprintf("/%d", wide)), nil); code != http.StatusNoContent {
		t.Fatalf("delete = %d", code)
	}

	if got := [3]int64{s.auditCount("skill_created"), s.auditCount("skill_updated"), s.auditCount("skill_removed")}; got != [3]int64{2, 1, 1} {
		t.Fatalf("created, updated, removed = %v", got)
	}
	code, page := s.browser(s.alice.cookie, http.MethodGet, auditPath(s.one, "action=skill_updated"), nil)
	if code != http.StatusOK {
		t.Fatalf("audit = %d %v", code, page)
	}
	events := page["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("events = %v", events)
	}
	e := events[0].(map[string]any)
	details := e["details"].(map[string]any)
	if e["target_type"] != "skill" || id(e["target_id"]) != inTeam || e["target_name"] != "Title of team-way" || details["scope"] != "space" ||
		details["name"] != "team-way" || id(details["space_id"]) != s.team {
		t.Fatalf("the event = %v", e)
	}
	if strings.Contains(fmt.Sprint(page), "Changed instructions") || strings.Contains(fmt.Sprint(page), "Do it well") {
		t.Fatal("a skill's text is in the audit log")
	}
	code, removed := s.browser(s.alice.cookie, http.MethodGet, auditPath(s.one, "action=skill_removed"), nil)
	if code != http.StatusOK || len(removed["events"].([]any)) != 1 || removed["events"].([]any)[0].(map[string]any)["target_created_at"] == nil {
		t.Fatalf("the removal = %d %v", code, removed)
	}
	code, other := s.browser(s.alice.cookie, http.MethodGet, auditPath(s.two, "action=skill_removed"), nil)
	if code != http.StatusOK || len(other["events"].([]any)) != 0 {
		t.Fatalf("workspace two's audit log = %d %v", code, other)
	}
}

func TestAPersonsExportHoldsTheirOwnSkillsOnly(t *testing.T) {
	s := newStack(t)
	s.makeSkill(s.bob.cookie, s.mySkills(s.one, ""), "closing")
	s.makeSkill(s.carol.cookie, s.mySkills(s.one, ""), "carols-secret")
	s.makeSkill(s.bob.cookie, s.spaceSkills(s.one, s.team, ""), "team-way")
	s.makeSkill(s.alice.cookie, s.workspaceSkills(s.one, ""), "house-style")
	other := s.makeSkill(s.bob.cookie, s.mySkills(s.one, ""), "draft")
	if code, _ := s.browser(s.bob.cookie, http.MethodPatch, s.mySkills(s.one, fmt.Sprintf("/%d", other)), map[string]any{"enabled": false, "title": "A \"draft\"\nof two lines"}); code == http.StatusOK {
		t.Fatal("a title of two lines")
	}
	if code, _ := s.browser(s.bob.cookie, http.MethodPatch, s.mySkills(s.one, fmt.Sprintf("/%d", other)), map[string]any{"enabled": false, "title": `A "draft": yes`}); code != http.StatusOK {
		t.Fatalf("patch = %d", code)
	}

	rec := s.export(s.bob.cookie, s.one)
	if rec.Code != http.StatusOK {
		t.Fatalf("export = %d %s", rec.Code, rec.Body)
	}
	files := unzip(t, rec)
	var skillFiles []string
	for name := range files {
		if strings.HasPrefix(name, "skills/") {
			skillFiles = append(skillFiles, name)
		}
	}
	if len(skillFiles) != 2 {
		t.Fatalf("skill files = %v", skillFiles)
	}
	closing := string(files["skills/closing.md"])
	if !strings.HasPrefix(closing, "---\nname: \"closing\"\ntitle: \"Title of closing\"\ndescription: \"when to use closing\"\nenabled: true\n---\n\n# closing\n\nDo it well.\n") {
		t.Fatalf("closing.md = %q", closing)
	}
	if draft := string(files["skills/draft.md"]); !strings.Contains(draft, `title: "A \"draft\": yes"`) || !strings.Contains(draft, "enabled: false") {
		t.Fatalf("draft.md = %q", draft)
	}
	for name, b := range files {
		if strings.Contains(string(b), "carols-secret") || strings.Contains(string(b), "team-way") || strings.Contains(string(b), "house-style") {
			t.Errorf("%s holds a skill that isn't bob's own", name)
		}
	}
	if !strings.Contains(string(files["manifest.json"]), `"skills": 2`) {
		t.Errorf("manifest = %s", files["manifest.json"])
	}
}
