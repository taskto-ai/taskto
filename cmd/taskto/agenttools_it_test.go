//go:build unix

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/apps"
)

func TestAgentToolsThroughTheServer(t *testing.T) {
	if os.Getenv("TASKTO_AGENT_IT") != "1" {
		t.Skip("set TASKTO_AGENT_IT=1 to run the agent sidecar integration tests")
	}
	entry, err := filepath.Abs("../../agent/dist/index.js")
	if err != nil {
		t.Fatal(err)
	}
	dir := shortDataDir(t)
	bin := filepath.Join(dir, "taskto")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	dataDir := filepath.Join(dir, "data")
	t.Setenv("TASKTO_AGENT_SCRIPT_TOKENS_PER_SECOND", "0")
	server, addr := startServer(t, bin, "-addr", "127.0.0.1:0", "-data-dir", dataDir, "-agent", "-agent-entry", entry, "-insecure-cookies")
	_, socketDir, _ := agentPaths(dataDir)
	sidecar := agentrt.New(filepath.Join(socketDir, "agent.sock"))
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	info, err := os.Lstat(filepath.Join(socketDir, "tools.sock"))
	if err != nil || info.Mode().Type() != os.ModeSocket || info.Mode().Perm() != 0o600 {
		t.Fatalf("tools socket: %v, %v", info, err)
	}

	newAPI := func() *api {
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		return &api{t: t, base: "http://" + addr, http: &http.Client{Jar: jar}}
	}
	as := func(t *testing.T, a *api) *api {
		bound := *a
		bound.t = t
		return &bound
	}
	decodeInto := func(t *testing.T, raw []byte, v any) {
		t.Helper()
		if err := json.Unmarshal(raw, v); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
	}
	var me struct {
		User struct {
			ID int64 `json:"id"`
		} `json:"user"`
		Workspaces []struct {
			ID int64 `json:"id"`
		} `json:"workspaces"`
	}
	owner := newAPI()
	token := setupTokenLine.FindStringSubmatch(server.output.String())
	if token == nil {
		t.Fatal("the server logged no setup token")
	}
	decodeInto(t, owner.expect(http.StatusCreated, http.MethodPost, "/api/setup", map[string]string{
		"email": "owner@example.com", "name": "Owner", "password": "fake-password-for-tests", "workspace_name": "Home",
		"setup_token": token[1],
	}), &me)
	ws := me.Workspaces[0].ID
	var invite struct {
		Token string `json:"token"`
	}
	decodeInto(t, owner.expect(http.StatusCreated, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", ws),
		map[string]string{"email": "member@example.com"}), &invite)
	member := newAPI()
	decodeInto(t, member.expect(http.StatusCreated, http.MethodPost, "/api/invites/accept", map[string]string{
		"token": invite.Token, "name": "Member", "password": "fake-password-for-tests",
	}), &me)
	memberID := me.User.ID

	type space struct {
		ID   int64  `json:"id"`
		Kind string `json:"kind"`
	}
	privateOf := func(a *api) int64 {
		var list struct{ Spaces []space }
		decodeInto(t, a.expect(http.StatusOK, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces", ws), nil), &list)
		for _, s := range list.Spaces {
			if s.Kind == "private" {
				return s.ID
			}
		}
		t.Fatal("no private space")
		return 0
	}
	ownerPrivate, memberPrivate := privateOf(owner), privateOf(member)
	var team space
	decodeInto(t, owner.expect(http.StatusCreated, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/spaces", ws), map[string]string{"name": "Team"}), &team)
	owner.expect(http.StatusNoContent, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members", ws, team.ID),
		map[string]any{"user_id": memberID, "role": "editor"})
	spacePath := func(s int64, rest string) string { return fmt.Sprintf("/api/workspaces/%d/spaces/%d%s", ws, s, rest) }
	var board struct {
		ID int64 `json:"id"`
	}
	decodeInto(t, owner.expect(http.StatusCreated, http.MethodPost, spacePath(team.ID, "/boards"), map[string]string{"name": "Launch"}), &board)
	member.expect(http.StatusCreated, http.MethodPut, spacePath(memberPrivate, "/docs/secret.md"), map[string]string{"content": "# Member's secret\n\nzanzibar"})

	base := fmt.Sprintf("/api/workspaces/%d/conversations", ws)
	type conversation struct {
		ID   int64 `json:"id"`
		path string
	}
	open := func(t *testing.T, a *api) conversation {
		t.Helper()
		var c conversation
		decodeInto(t, a.expect(http.StatusCreated, http.MethodPost, base, map[string]any{}), &c)
		c.path = fmt.Sprintf("%s/%d", base, c.ID)
		return c
	}
	turn := func(t *testing.T, a *api, c conversation, tool string, args map[string]any) string {
		t.Helper()
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		var sent struct {
			SubmissionID int64 `json:"submission_id"`
		}
		decodeInto(t, a.expect(http.StatusAccepted, http.MethodPost, c.path+"/messages", map[string]any{"content": "tool:" + tool + " " + string(raw)}), &sent)
		if settled, err := sidecar.Wait(ctx, sent.SubmissionID); err != nil || settled.Status != "done" {
			t.Fatalf("%s: %+v, %v", tool, settled, err)
		}
		var msgs struct{ Messages []agentrt.Message }
		decodeInto(t, a.expect(http.StatusOK, http.MethodGet, c.path+"/messages", nil), &msgs)
		last := msgs.Messages[len(msgs.Messages)-1].Text
		got, ok := strings.CutPrefix(last, "finished "+tool+": ")
		if !ok {
			t.Fatalf("%s: the model said %q", tool, last)
		}
		return got
	}
	use := func(t *testing.T, tool string, args map[string]any) string {
		t.Helper()
		o := as(t, owner)
		return turn(t, o, open(t, o), tool, args)
	}

	t.Run("an agent sees the owner's spaces and none of the member's", func(t *testing.T) {
		spaces := use(t, "list_spaces", map[string]any{})
		if !strings.Contains(spaces, fmt.Sprintf(`"id":%d,`, ownerPrivate)) || !strings.Contains(spaces, fmt.Sprintf(`"id":%d,`, team.ID)) ||
			strings.Contains(spaces, fmt.Sprintf(`"id":%d,`, memberPrivate)) {
			t.Fatalf("list_spaces = %s", spaces)
		}
		if got := use(t, "read_doc", map[string]any{"space_id": memberPrivate, "path": "secret.md"}); got != "error: not found" {
			t.Fatalf("reading the member's document: %s", got)
		}
		if got := use(t, "search_docs", map[string]any{"query": "zanzibar"}); !strings.Contains(got, `"results":[]`) {
			t.Fatalf("searching for the member's document: %s", got)
		}
	})

	t.Run("a document the agent writes is the owner's, committed by the agent", func(t *testing.T) {
		if got := use(t, "write_doc", map[string]any{"space_id": team.ID, "path": "notes/plan.md", "content": "# Plan\n\nlaunch"}); !strings.Contains(got, `"commit"`) {
			t.Fatalf("write_doc = %s", got)
		}
		var history struct {
			Commits []struct {
				Author struct{ Name string } `json:"author"`
				Agent  string                `json:"agent"`
			} `json:"commits"`
		}
		decodeInto(t, as(t, member).expect(http.StatusOK, http.MethodGet, spacePath(team.ID, "/history/notes/plan.md"), nil), &history)
		if len(history.Commits) != 1 || history.Commits[0].Author.Name != "Owner" || history.Commits[0].Agent == "" {
			t.Fatalf("history = %+v", history)
		}
		if got := use(t, "read_doc", map[string]any{"space_id": team.ID, "path": "notes/plan.md"}); !strings.Contains(got, `launch`) {
			t.Fatalf("read_doc = %s", got)
		}
	})

	var cardConv conversation
	t.Run("a card the agent creates links the conversation by its API ID", func(t *testing.T) {
		o, m := as(t, owner), as(t, member)
		cardConv = open(t, o)
		convID := strconv.FormatInt(cardConv.ID, 10)
		created := turn(t, o, cardConv, "create_card", map[string]any{"space_id": team.ID, "board_id": board.ID, "title": "Launch", "conversations": []string{convID}})
		var card struct {
			ID            int64    `json:"id"`
			Conversations []string `json:"conversations"`
			CreatedBy     struct {
				ID    int64 `json:"id"`
				Agent bool  `json:"agent"`
			} `json:"created_by"`
		}
		decodeInto(t, []byte(created), &card)
		if len(card.Conversations) != 1 || card.Conversations[0] != convID || !card.CreatedBy.Agent {
			t.Fatalf("create_card = %s", created)
		}
		m.expect(http.StatusOK, http.MethodGet, spacePath(team.ID, fmt.Sprintf("/boards/%d/cards/%d", board.ID, card.ID)), nil)
		m.expect(http.StatusNotFound, http.MethodGet, fmt.Sprintf("%s/%s", base, card.Conversations[0]), nil)
		o.expect(http.StatusOK, http.MethodGet, fmt.Sprintf("%s/%s", base, card.Conversations[0]), nil)
		theirs := open(t, m)
		if got := use(t, "create_card", map[string]any{"space_id": team.ID, "board_id": board.ID, "title": "x", "conversations": []string{strconv.FormatInt(theirs.ID, 10)}}); !strings.HasPrefix(got, "error: conversations:") {
			t.Fatalf("linking the member's conversation: %s", got)
		}
	})

	t.Run("an app the agent builds in the team space is launched by the member once published", func(t *testing.T) {
		o, m := as(t, owner), as(t, member)
		example, err := apps.Examples()
		if err != nil {
			t.Fatal(err)
		}
		if got := use(t, "app_catalog", map[string]any{}); !strings.Contains(got, `"Stat"`) {
			t.Fatalf("app_catalog = %s", got)
		}
		built := use(t, "create_app", map[string]any{"space_id": team.ID, "name": "Team expenses", "definition": example[0].Definition})
		var app struct {
			ID        int64 `json:"id"`
			Version   int64 `json:"version"`
			CreatedBy struct {
				Agent bool `json:"agent"`
			} `json:"created_by"`
		}
		decodeInto(t, []byte(built), &app)
		if app.ID == 0 || !app.CreatedBy.Agent {
			t.Fatalf("create_app = %s", built)
		}
		surface := spacePath(team.ID, fmt.Sprintf("/apps/%d/surface", app.ID))
		m.expect(http.StatusConflict, http.MethodGet, surface, nil)
		o.expect(http.StatusOK, http.MethodPost, spacePath(team.ID, fmt.Sprintf("/apps/%d/publish", app.ID)),
			map[string]any{"version": app.Version, "acknowledge": []any{}})
		m.expect(http.StatusOK, http.MethodGet, surface, nil)
		if got := use(t, "create_app", map[string]any{"space_id": team.ID, "name": "Bad", "definition": map[string]any{
			"components": []any{map[string]any{"id": "root", "component": "Script"}}, "collection": map[string]any{"fields": []any{}},
			"actions": map[string]any{"x": map[string]any{"kind": "insert"}},
		}}); !strings.Contains(got, `"Script" is not a component`) {
			t.Fatalf("an app outside the catalog: %s", got)
		}
	})

	t.Run("the agent reaches no app of the member's private space", func(t *testing.T) {
		example, err := apps.Examples()
		if err != nil {
			t.Fatal(err)
		}
		var theirApp struct {
			ID int64 `json:"id"`
		}
		decodeInto(t, as(t, member).expect(http.StatusCreated, http.MethodPost, spacePath(memberPrivate, "/apps"),
			map[string]any{"name": "Mine", "definition": example[0].Definition}), &theirApp)
		if got := use(t, "show_app", map[string]any{"space_id": memberPrivate, "app_id": theirApp.ID}); got != "error: not found" {
			t.Fatalf("the member's app: %s", got)
		}
	})

	t.Run("the capability never shows in what the browser gets", func(t *testing.T) {
		if cardConv.ID == 0 {
			t.Skip("no conversation had a tool turn")
		}
		o := as(t, owner)
		stream := o.openEvents(cardConv.path + "/events")
		if ev := stream.next(t); ev.Type != "value" || strings.Contains(string(ev.Value), "ttc1.") {
			t.Fatalf("the conversation's view: %s", ev.Value)
		}
		stream.cancel()
		if _, msgs := o.do(http.MethodGet, cardConv.path+"/messages", nil); strings.Contains(string(msgs), "ttc1.") {
			t.Fatal("the messages show the capability")
		}
	})

	t.Run("a member taken off the team space loses its access from the next call on", func(t *testing.T) {
		m := as(t, member)
		as(t, owner).expect(http.StatusNoContent, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", ws, team.ID, memberID), nil)
		got := turn(t, m, open(t, m), "read_doc", map[string]any{"space_id": team.ID, "path": "notes/plan.md"})
		if got != "error: not found" {
			t.Fatalf("the member's agent after leaving the space: %q", got)
		}
	})
}
