package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func deskLayoutFor(workspace int64, route string) map[string]any {
	return map[string]any{"v": 1, "focus": "g1", "root": map[string]any{"group": "g1", "active": "t1", "tabs": []any{map[string]any{"id": "t1", "path": fmt.Sprintf("/w/%d/%s", workspace, route)}}}}
}

func deskRoute(layout map[string]any) string {
	return layout["root"].(map[string]any)["tabs"].([]any)[0].(map[string]any)["path"].(string)
}

func (s *stack) deskBody(cookie, method, path string, body []byte) (int, map[string]any) {
	s.t.Helper()
	rec := s.request(method, path, nil, sameOrigin(cookie), func(r *http.Request) {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Type", "application/json")
	})
	return rec.Code, decode(rec)
}

func (s *stack) saveDesk(cookie string, workspace int64, desk string, version int, layout map[string]any) (int, map[string]any) {
	s.t.Helper()
	return s.browser(cookie, http.MethodPut, deskPath(workspace, desk, "/layout"), map[string]any{"version": version, "layout": layout})
}

func TestDeskStaleVersionsAnswerDeskChangedAndFreshOnesSave(t *testing.T) {
	s := newStack(t)
	home, boards, apps := deskLayoutFor(s.one, "home"), deskLayoutFor(s.one, "boards"), deskLayoutFor(s.one, "apps")
	code, saved := s.saveDesk(s.alice.cookie, s.one, "main", 0, home)
	if code != http.StatusOK {
		t.Fatalf("first save = %d %v", code, saved)
	}
	id := fmt.Sprint(int64(saved["id"].(float64)))
	refused := func(label, desk string, version int, wantVersion float64, wantRoute string) {
		t.Helper()
		code, body := s.saveDesk(s.alice.cookie, s.one, desk, version, apps)
		current, _ := body["current"].(map[string]any)
		if code != http.StatusConflict || body["code"] != "desk_changed" || current["version"] != wantVersion || current["id"] != saved["id"] {
			t.Fatalf("%s = %d %v", label, code, body)
		}
		if got := deskRoute(current["layout"].(map[string]any)); got != wantRoute {
			t.Fatalf("%s: current layout holds %q, want %q (what is stored, not what was sent)", label, got, wantRoute)
		}
		code, stored := s.browser(s.alice.cookie, http.MethodGet, deskPath(s.one, "main", ""), nil)
		if code != http.StatusOK || stored["version"] != wantVersion || deskRoute(stored["layout"].(map[string]any)) != wantRoute {
			t.Fatalf("%s changed the stored desk: %d %v", label, code, stored)
		}
	}
	prefix := fmt.Sprintf("/w/%d/", s.one)
	refused("main, version 0 after the first save", "main", 0, 1, prefix+"home")
	refused("by id, version 0 after the first save", id, 0, 1, prefix+"home")
	refused("main, version from the future", "main", 7, 1, prefix+"home")
	refused("by id, negative version", id, -1, 1, prefix+"home")

	if code, body := s.saveDesk(s.alice.cookie, s.one, id, 1, boards); code != http.StatusOK || body["version"] != float64(2) || body["id"] != saved["id"] {
		t.Fatalf("fresh save by id = %d %v", code, body)
	}
	if code, body := s.saveDesk(s.alice.cookie, s.one, "main", 2, home); code != http.StatusOK || body["version"] != float64(3) {
		t.Fatalf("fresh save by main after a save by id = %d %v", code, body)
	}
	if code, body := s.saveDesk(s.alice.cookie, s.one, "main", 3, boards); code != http.StatusOK || body["version"] != float64(4) {
		t.Fatalf("second fresh save by main = %d %v", code, body)
	}
	refused("main, the version just replaced", "main", 3, 4, prefix+"boards")
	refused("by id, the version just replaced", id, 2, 4, prefix+"boards")
}

func TestDeskRequestsAreCappedAndStrict(t *testing.T) {
	s := newStack(t)
	path := deskPath(s.one, "main", "/layout")
	valid := fmt.Sprintf(`{"v":1,"focus":"g1","root":{"group":"g1","active":"t1","tabs":[{"id":"t1","path":"/w/%d/home"}]}}`, s.one)
	body := func(version int, layout string, extra string) []byte {
		return []byte(fmt.Sprintf(`{"version":%d,"layout":%s%s}`, version, layout, extra))
	}
	count := func() int64 { return s.scalar(`SELECT count(*) FROM desks`) }
	sized := func(n int) string { return "{" + strings.Repeat(" ", n-len(valid)) + valid[1:] }

	for name, c := range map[string]struct {
		raw    []byte
		status int
		code   string
	}{
		"a body of a megabyte":            {body(0, valid, `,"pad":"`+strings.Repeat("x", 1<<20)+`"`), http.StatusRequestEntityTooLarge, "request_too_large"},
		"a layout one byte over 32 KiB":   {body(0, sized(32*1024+1), ""), http.StatusBadRequest, "desk_invalid"},
		"a layout padded past the cap":    {body(0, valid+strings.Repeat(" ", 40*1024), ""), http.StatusRequestEntityTooLarge, "request_too_large"},
		"an unknown field in the request": {body(0, valid, `,"extra":true`), http.StatusBadRequest, "invalid_json"},
		"data after the request":          {append(body(0, valid, ""), []byte(` {}`)...), http.StatusBadRequest, "invalid_json"},
		"no layout":                       {[]byte(`{"version":0}`), http.StatusBadRequest, "desk_invalid"},
		"a layout that is not an object":  {body(0, `"home"`, ""), http.StatusBadRequest, "desk_invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			code, got := s.deskBody(s.alice.cookie, http.MethodPut, path, c.raw)
			if code != c.status || got["code"] != c.code {
				t.Fatalf("status %d, body %v", code, got)
			}
			if n := count(); n != 0 {
				t.Fatalf("a refused request left %d rows", n)
			}
		})
	}

	var tabs []string
	for i := 1; i <= 12; i++ {
		tabs = append(tabs, fmt.Sprintf(`{"id":"t%d","path":"/w/%d/home/%s"}`, i, s.one, strings.Repeat("&", 490)))
	}
	grows := fmt.Sprintf(`{"v":1,"focus":"g1","root":{"group":"g1","active":"t1","tabs":[%s]}}`, strings.Join(tabs, ","))
	if len(grows) >= 32*1024 {
		t.Fatalf("the layout is %d bytes before it is stored", len(grows))
	}
	code, got := s.deskBody(s.alice.cookie, http.MethodPut, path, body(0, grows, ""))
	if code != http.StatusBadRequest || got["code"] != "desk_invalid" || got["params"].(map[string]any)["reason"] != "too_large" {
		t.Fatalf("a layout that is under 32 KiB as sent and over it as stored = %d %v", code, got)
	}
	if n := count(); n != 0 {
		t.Fatalf("a layout too large as stored left %d rows", n)
	}

	exact := body(0, sized(32*1024), "")
	if code, got := s.deskBody(s.alice.cookie, http.MethodPut, path, exact); code != http.StatusOK || got["version"] != float64(1) {
		t.Fatalf("a layout of exactly 32 KiB = %d %v", code, got)
	}
	if code, got := s.deskBody(s.alice.cookie, http.MethodPut, path, body(1, sized(32*1024), "")); code != http.StatusOK || got["version"] != float64(2) {
		t.Fatalf("a second save of exactly 32 KiB = %d %v", code, got)
	}

	padded := body(2, valid, "")
	padded = append(padded, bytes.Repeat([]byte(" "), 30*1024)...)
	if code, got := s.deskBody(s.alice.cookie, http.MethodPut, path, padded); code != http.StatusOK || got["version"] != float64(3) {
		t.Fatalf("a request under the cap = %d %v", code, got)
	}
	_, stored := s.browser(s.alice.cookie, http.MethodGet, deskPath(s.one, "main", ""), nil)
	if len(fmt.Sprint(stored["layout"])) > 1000 {
		t.Fatalf("the stored layout keeps its padding: %v", stored["layout"])
	}
}

func TestDeskIDsThatAreNotDeskIDsAreNotFound(t *testing.T) {
	s := newStack(t)
	layout := deskLayoutFor(s.one, "home")
	for _, desk := range []string{"0", "-1", "00", "abc", "1x", "main2", "Main", "9223372036854775808"} {
		if code, body := s.browser(s.alice.cookie, http.MethodGet, deskPath(s.one, desk, ""), nil); code != http.StatusNotFound || body["code"] != "not_found" {
			t.Fatalf("GET %q = %d %v", desk, code, body)
		}
		if code, body := s.saveDesk(s.alice.cookie, s.one, desk, 0, layout); code != http.StatusNotFound || body["code"] != "not_found" {
			t.Fatalf("PUT %q = %d %v", desk, code, body)
		}
	}
	if n := s.scalar(`SELECT count(*) FROM desks`); n != 0 {
		t.Fatalf("refused ids left %d rows", n)
	}
}

func TestDeskAccessTokensAreRefusedOnEveryRoute(t *testing.T) {
	s := newStack(t)
	code, saved := s.saveDesk(s.alice.cookie, s.one, "main", 0, deskLayoutFor(s.one, "home"))
	if code != http.StatusOK {
		t.Fatalf("save = %d %v", code, saved)
	}
	id := fmt.Sprint(int64(saved["id"].(float64)))
	token, _ := s.newToken(s.alice.cookie, s.one, "desk routes")
	layout := map[string]any{"version": 1, "layout": deskLayoutFor(s.one, "boards")}
	for _, r := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, fmt.Sprintf("/api/workspaces/%d/desks", s.one), nil},
		{http.MethodGet, deskPath(s.one, "main", ""), nil},
		{http.MethodGet, deskPath(s.one, id, ""), nil},
		{http.MethodPut, deskPath(s.one, "main", "/layout"), layout},
		{http.MethodPut, deskPath(s.one, id, "/layout"), layout},
	} {
		if code, body := s.api(token, r.method, r.path, r.body); code != http.StatusForbidden || body["code"] != "token_refused" {
			t.Fatalf("%s %s with an access token = %d %v", r.method, r.path, code, body)
		}
	}
	if code, body := s.browser(s.alice.cookie, http.MethodGet, deskPath(s.one, "main", ""), nil); code != http.StatusOK || body["version"] != float64(1) || deskRoute(body["layout"].(map[string]any)) != fmt.Sprintf("/w/%d/home", s.one) {
		t.Fatalf("an access token changed the desk: %d %v", code, body)
	}
}

func TestDeskMembersKeepSeparateMainDesksPerWorkspace(t *testing.T) {
	s := newStack(t)
	listOne := fmt.Sprintf("/api/workspaces/%d/desks", s.one)
	listTwo := fmt.Sprintf("/api/workspaces/%d/desks", s.two)
	code, aliceOne := s.saveDesk(s.alice.cookie, s.one, "main", 0, deskLayoutFor(s.one, "home"))
	if code != http.StatusOK {
		t.Fatalf("alice save = %d %v", code, aliceOne)
	}
	if code, body := s.browser(s.bob.cookie, http.MethodGet, deskPath(s.one, "main", ""), nil); code != http.StatusOK || body["id"] != float64(0) || body["version"] != float64(0) || body["layout"].(map[string]any)["root"] != nil {
		t.Fatalf("bob sees alice's main desk: %d %v", code, body)
	}
	if code, body := s.browser(s.bob.cookie, http.MethodGet, listOne, nil); code != http.StatusOK || len(body["desks"].([]any)) != 1 || body["desks"].([]any)[0].(map[string]any)["id"] != float64(0) {
		t.Fatalf("bob's list shows alice's desk: %d %v", code, body)
	}
	if code, body := s.browser(s.alice.cookie, http.MethodGet, listTwo, nil); code != http.StatusOK || body["desks"].([]any)[0].(map[string]any)["id"] != float64(0) {
		t.Fatalf("alice's list of another workspace shows her desk here: %d %v", code, body)
	}
	code, bobOne := s.saveDesk(s.bob.cookie, s.one, "main", 0, deskLayoutFor(s.one, "boards"))
	if code != http.StatusOK || bobOne["id"] == aliceOne["id"] || bobOne["version"] != float64(1) {
		t.Fatalf("bob save = %d %v", code, bobOne)
	}
	code, aliceTwo := s.saveDesk(s.alice.cookie, s.two, "main", 0, deskLayoutFor(s.two, "apps"))
	if code != http.StatusOK || aliceTwo["id"] == aliceOne["id"] || aliceTwo["id"] == bobOne["id"] {
		t.Fatalf("alice save in the second workspace = %d %v", code, aliceTwo)
	}

	want := map[string]string{
		"alice/one": fmt.Sprintf("/w/%d/home", s.one),
		"bob/one":   fmt.Sprintf("/w/%d/boards", s.one),
		"alice/two": fmt.Sprintf("/w/%d/apps", s.two),
	}
	read := func(label, cookie string, workspace int64, version float64) {
		t.Helper()
		code, body := s.browser(cookie, http.MethodGet, deskPath(workspace, "main", ""), nil)
		if code != http.StatusOK || body["version"] != version || deskRoute(body["layout"].(map[string]any)) != want[label] {
			t.Fatalf("%s main desk = %d %v", label, code, body)
		}
	}
	read("alice/one", s.alice.cookie, s.one, 1)
	read("bob/one", s.bob.cookie, s.one, 1)
	read("alice/two", s.alice.cookie, s.two, 1)

	want["bob/one"] = fmt.Sprintf("/w/%d/settings", s.one)
	if code, body := s.saveDesk(s.bob.cookie, s.one, "main", 1, deskLayoutFor(s.one, "settings")); code != http.StatusOK || body["id"] != bobOne["id"] || body["version"] != float64(2) {
		t.Fatalf("bob's second save = %d %v", code, body)
	}
	read("alice/one", s.alice.cookie, s.one, 1)
	read("alice/two", s.alice.cookie, s.two, 1)
	want["alice/one"] = fmt.Sprintf("/w/%d/notifications", s.one)
	if code, body := s.saveDesk(s.alice.cookie, s.one, "main", 1, deskLayoutFor(s.one, "notifications")); code != http.StatusOK || body["id"] != aliceOne["id"] || body["version"] != float64(2) {
		t.Fatalf("alice's second save = %d %v", code, body)
	}
	read("bob/one", s.bob.cookie, s.one, 2)
	read("alice/two", s.alice.cookie, s.two, 1)

	if code, body := s.saveDesk(s.alice.cookie, s.two, "main", 0, deskLayoutFor(s.one, "home")); code != http.StatusBadRequest || body["code"] != "desk_invalid" || body["params"].(map[string]any)["reason"] != "invalid_path" {
		t.Fatalf("alice saved a path of another workspace on this workspace's desk: %d %v", code, body)
	}
}

func TestDeskLeavingOneWorkspaceKeepsEveryOtherDesk(t *testing.T) {
	s := newStack(t)
	for _, c := range []struct {
		cookie    string
		workspace int64
	}{{s.alice.cookie, s.one}, {s.alice.cookie, s.two}, {s.bob.cookie, s.one}} {
		if code, body := s.saveDesk(c.cookie, c.workspace, "main", 0, deskLayoutFor(c.workspace, "home")); code != http.StatusOK {
			t.Fatalf("save = %d %v", code, body)
		}
	}
	s.exec(`DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, s.two, s.alice.id)
	for _, c := range []struct {
		user, workspace int64
		want            int64
	}{{s.alice.id, s.two, 0}, {s.alice.id, s.one, 1}, {s.bob.id, s.one, 1}} {
		if got := s.scalar(`SELECT count(*) FROM desks WHERE user_id = ? AND workspace_id = ?`, c.user, c.workspace); got != c.want {
			t.Fatalf("desks of user %d in workspace %d = %d, want %d", c.user, c.workspace, got, c.want)
		}
	}
	if code, body := s.browser(s.alice.cookie, http.MethodGet, deskPath(s.two, "main", ""), nil); code == http.StatusOK {
		t.Fatalf("a person who left still reads the workspace's desks: %v", body)
	}
}
