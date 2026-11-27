package server

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
)

func deskPath(workspace int64, desk string, tail string) string {
	return fmt.Sprintf("/api/workspaces/%d/desks/%s%s", workspace, desk, tail)
}

func TestDeskLayoutsArePrivateAndWorkspaceScoped(t *testing.T) {
	s := newStack(t)
	listPath := fmt.Sprintf("/api/workspaces/%d/desks", s.one)
	code, list := s.browser(s.alice.cookie, http.MethodGet, listPath, nil)
	if code != http.StatusOK || s.scalar(`SELECT count(*) FROM desks WHERE user_id = ? AND workspace_id = ?`, s.alice.id, s.one) != 0 {
		t.Fatalf("initial list = %d %v", code, list)
	}
	items := list["desks"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != float64(0) {
		t.Fatalf("initial desk list = %v", list)
	}
	code, empty := s.browser(s.alice.cookie, http.MethodGet, deskPath(s.one, "main", ""), nil)
	if code != http.StatusOK || empty["version"] != float64(0) || empty["layout"].(map[string]any)["root"] != nil {
		t.Fatalf("initial main desk = %d %v", code, empty)
	}
	layout := map[string]any{"v": 1, "focus": "g1", "root": map[string]any{"group": "g1", "active": "t1", "tabs": []any{map[string]any{"id": "t1", "path": fmt.Sprintf("/w/%d/home", s.one)}}}}
	code, saved := s.browser(s.alice.cookie, http.MethodPut, deskPath(s.one, "main", "/layout"), map[string]any{"version": 0, "layout": layout})
	if code != http.StatusOK || saved["id"].(float64) == 0 || saved["version"] != float64(1) {
		t.Fatalf("save main desk = %d %v", code, saved)
	}
	id := int64(saved["id"].(float64))
	code, after := s.browser(s.alice.cookie, http.MethodGet, listPath, nil)
	listed := after["desks"].([]any)
	if code != http.StatusOK || len(listed) != 1 || listed[0].(map[string]any)["id"] != float64(id) {
		t.Fatalf("list after save = %d %v", code, after)
	}
	path := deskPath(s.one, fmt.Sprint(id), "")
	if code, body := s.browser(s.bob.cookie, http.MethodGet, path, nil); code != http.StatusNotFound || body["code"] != "not_found" {
		t.Fatalf("another member read desk = %d %v", code, body)
	}
	if code, body := s.browser(s.bob.cookie, http.MethodPut, deskPath(s.one, fmt.Sprint(id), "/layout"), map[string]any{"version": 1, "layout": layout}); code != http.StatusNotFound {
		t.Fatalf("another member wrote desk = %d %v", code, body)
	}
	if code, body := s.browser(s.alice.cookie, http.MethodGet, deskPath(s.two, fmt.Sprint(id), ""), nil); code != http.StatusNotFound {
		t.Fatalf("another workspace read desk = %d %v", code, body)
	}
	if code, body := s.browser(s.alice.cookie, http.MethodPut, deskPath(s.two, fmt.Sprint(id), "/layout"), map[string]any{"version": 1, "layout": layout}); code != http.StatusNotFound {
		t.Fatalf("another workspace wrote desk = %d %v", code, body)
	}
	code, stale := s.browser(s.alice.cookie, http.MethodPut, deskPath(s.one, "main", "/layout"), map[string]any{"version": 0, "layout": layout})
	if code != http.StatusConflict || stale["code"] != "desk_changed" || stale["current"].(map[string]any)["version"] != float64(1) {
		t.Fatalf("stale update = %d %v", code, stale)
	}
	if code, next := s.browser(s.alice.cookie, http.MethodPut, deskPath(s.one, "main", "/layout"), map[string]any{"version": 1, "layout": layout}); code != http.StatusOK || next["version"] != float64(2) || next["id"] != float64(id) {
		t.Fatalf("second save through main = %d %v", code, next)
	}
	token, _ := s.newToken(s.alice.cookie, s.one, "desk test")
	if code, body := s.api(token, http.MethodGet, listPath, nil); code != http.StatusForbidden || body["code"] != "token_refused" {
		t.Fatalf("access token read desks = %d %v", code, body)
	}
	if code, current := s.browser(s.alice.cookie, http.MethodGet, deskPath(s.one, "main", ""), nil); code != http.StatusOK || current["layout"].(map[string]any)["root"] == nil {
		t.Fatalf("saved layout did not survive a read: %d %v", code, current)
	}
	login := s.request(http.MethodPost, "/api/login", map[string]any{"email": "alice@example.com", "password": fakePassword}, sameOrigin(""))
	if login.Code != http.StatusOK {
		t.Fatalf("second login = %d %s", login.Code, login.Body)
	}
	if code, current := s.browser(sessionCookie(login), http.MethodGet, deskPath(s.one, "main", ""), nil); code != http.StatusOK || current["layout"].(map[string]any)["root"] == nil {
		t.Fatalf("saved layout did not follow another session: %d %v", code, current)
	}
	if code, _ := s.browser(s.bob.cookie, http.MethodPut, deskPath(s.one, "main", "/layout"), map[string]any{"version": 0, "layout": layout}); code != http.StatusOK {
		t.Fatalf("save second member main desk = %d", code)
	}
	s.exec(`DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, s.one, s.bob.id)
	if n := s.scalar(`SELECT count(*) FROM desks WHERE user_id = ? AND workspace_id = ?`, s.bob.id, s.one); n != 0 {
		t.Fatalf("desks remained after membership removal: %d", n)
	}
}

func TestConcurrentFirstDeskSavesUseOneVersion(t *testing.T) {
	s := newStack(t)
	path := deskPath(s.one, "main", "/layout")
	body := map[string]any{"version": 0, "layout": map[string]any{"v": 1, "focus": "g1", "root": map[string]any{"group": "g1", "active": "t1", "tabs": []any{map[string]any{"id": "t1", "path": fmt.Sprintf("/w/%d/home", s.one)}}}}}
	results := make(chan int, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _ := s.browser(s.alice.cookie, http.MethodPut, path, body)
			results <- code
		}()
	}
	wg.Wait()
	close(results)
	statuses := map[int]int{}
	for code := range results {
		statuses[code]++
	}
	if statuses[http.StatusOK] != 1 || statuses[http.StatusConflict] != 1 {
		t.Fatalf("concurrent saves returned %v", statuses)
	}
	if count := s.scalar(`SELECT count(*) FROM desks WHERE user_id = ? AND workspace_id = ? AND name = '' AND version = 1`, s.alice.id, s.one); count != 1 {
		t.Fatalf("main desk count after concurrent saves = %d", count)
	}
}
