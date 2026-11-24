//go:build unix

package main

// The conversation API with the real sidecar and its scripted model, through the server
// binary: create, send, steer, stream the events and rebuild the view from them in Node
// with Chord's applyImmutable (as the browser will), and nobody else reaching any of it.
// Gated by TASKTO_AGENT_IT=1 like the other integration tests.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/db"
)

// streamEvent is one line of the sidecar's watch, as an SSE event carries it.
type streamEvent struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
	Ops   json.RawMessage `json:"ops"`
	Kind  string          `json:"kind"`
}

// eventsClient reads GET …/events in the background. Each event's data arrives on events;
// the channel closes when the stream ends.
type eventsClient struct {
	events <-chan streamEvent
	cancel context.CancelFunc
}

func (a *api) openEvents(path string) *eventsClient {
	a.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	a.t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+path, nil)
	if err != nil {
		a.t.Fatal(err)
	}
	res, err := a.http.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	a.t.Cleanup(func() { _ = res.Body.Close() })
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" {
		a.t.Fatalf("GET %s = %d %s", path, res.StatusCode, res.Header.Get("Content-Type"))
	}
	events := make(chan streamEvent, 1024)
	go func() {
		defer close(events)
		r := bufio.NewReaderSize(res.Body, 1<<20)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			data, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "data: ")
			if !ok {
				continue // retry, heartbeats and the blank line between events
			}
			var ev streamEvent
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				a.t.Errorf("event data %q: %v", data, err)
				return
			}
			events <- ev
		}
	}()
	return &eventsClient{events: events, cancel: cancel}
}

// next returns the next event, failing the test if none comes within a while.
func (e *eventsClient) next(t *testing.T) streamEvent {
	t.Helper()
	select {
	case ev, ok := <-e.events:
		if !ok {
			t.Fatal("the event stream ended")
		}
		return ev
	case <-time.After(30 * time.Second):
		t.Fatal("no event within 30 s")
	}
	return streamEvent{}
}

// rebuild applies ops batches to value with Chord's applyImmutable in Node, from the agent
// package, whose dependencies have it, and returns the view.
func rebuild(t *testing.T, value json.RawMessage, batches []json.RawMessage) any {
	t.Helper()
	input, err := json.Marshal(map[string]any{"value": value, "batches": batches})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("node", "--input-type=module", "-e", `
import { applyImmutable } from '@earendil-works/chord/delta'
let raw = ''
for await (const chunk of process.stdin) raw += chunk
const { value, batches } = JSON.parse(raw)
let view = value
for (const ops of batches) view = applyImmutable(view, ops)
process.stdout.write(JSON.stringify(view))
`)
	cmd.Dir = filepath.Join("..", "..", "agent")
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, stderr.String())
	}
	var view any
	if err := json.Unmarshal(out, &view); err != nil {
		t.Fatal(err)
	}
	return view
}

func TestConversationsThroughTheServer(t *testing.T) {
	if os.Getenv("TASKTO_AGENT_IT") != "1" {
		t.Skip("set TASKTO_AGENT_IT=1 to run the agent sidecar integration tests")
	}
	// test-entry.js: the sidecar with the pause tools the scripted model calls.
	entry, err := filepath.Abs("../../agent/dist/test-entry.js")
	if err != nil {
		t.Fatal(err)
	}
	dir := shortDataDir(t)
	bin := filepath.Join(dir, "taskto")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	dataDir := filepath.Join(dir, "data")
	server, addr := startServer(t, bin, "-addr", "127.0.0.1:0", "-data-dir", dataDir, "-agent", "-agent-entry", entry, "-insecure-cookies")
	_, socketDir, _ := agentPaths(dataDir)
	sidecar := agentrt.New(filepath.Join(socketDir, "agent.sock"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	newAPI := func() *api {
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		return &api{t: t, base: "http://" + addr, http: &http.Client{Jar: jar}}
	}
	type me struct {
		User struct {
			ID int64 `json:"id"`
		} `json:"user"`
		Workspaces []struct {
			ID int64 `json:"id"`
		} `json:"workspaces"`
	}
	owner := newAPI()
	var ownerMe me
	// The setup token, from the server's log, as an operator reads it.
	token := setupTokenLine.FindStringSubmatch(server.output.String())
	if token == nil {
		t.Fatal("the server logged no setup token")
	}
	if err := json.Unmarshal(owner.expect(http.StatusCreated, http.MethodPost, "/api/setup", map[string]string{
		"email": "owner@example.com", "name": "Owner", "password": "fake-password-for-tests", "workspace_name": "Home",
		"setup_token": token[1],
	}), &ownerMe); err != nil {
		t.Fatal(err)
	}
	ws := ownerMe.Workspaces[0].ID
	var invite struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(owner.expect(http.StatusCreated, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", ws),
		map[string]string{"email": "member@example.com"}), &invite); err != nil {
		t.Fatal(err)
	}
	member := newAPI()
	member.expect(http.StatusCreated, http.MethodPost, "/api/invites/accept", map[string]string{
		"token": invite.Token, "name": "Member", "password": "fake-password-for-tests",
	})

	base := fmt.Sprintf("/api/workspaces/%d/conversations", ws)
	var conv struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(owner.expect(http.StatusCreated, http.MethodPost, base, map[string]any{"title": "Integration"}), &conv); err != nil {
		t.Fatal(err)
	}
	one := fmt.Sprintf("%s/%d", base, conv.ID)

	// Stream while a tool runs, and steer it.
	stream := owner.openEvents(one + "/events")
	first := stream.next(t)
	if first.Type != "value" {
		t.Fatalf("first event = %+v, want the view", first)
	}
	var batches []json.RawMessage
	waitForOps := func(want string) {
		t.Helper()
		for {
			ev := stream.next(t)
			if ev.Type != "ops" {
				t.Fatalf("event = %+v, want ops", ev)
			}
			batches = append(batches, ev.Ops)
			if strings.Contains(string(ev.Ops), want) {
				return
			}
		}
	}
	send := func(body map[string]any) int64 {
		t.Helper()
		var out struct {
			SubmissionID int64 `json:"submission_id"`
		}
		if err := json.Unmarshal(owner.expect(http.StatusAccepted, http.MethodPost, one+"/messages", body), &out); err != nil {
			t.Fatal(err)
		}
		return out.SubmissionID
	}
	toolRun := send(map[string]any{"content": "tool:pause_safe:1200"})
	waitForOps("waited 200ms")
	steer := send(map[string]any{"content": "use pnpm", "when_busy": "steer", "request_id": "steer-1"})
	if again := send(map[string]any{"content": "use pnpm", "when_busy": "steer", "request_id": "steer-1"}); again != steer {
		t.Fatalf("a retried send = submission %d, want %d", again, steer)
	}
	for _, id := range []int64{toolRun, steer} {
		if settled, err := sidecar.Wait(ctx, id); err != nil || settled.Status != "done" {
			t.Fatalf("submission %d = %+v, %v; want done", id, settled, err)
		}
	}
	var msgs struct {
		Messages []agentrt.Message `json:"messages"`
	}
	if err := json.Unmarshal(owner.expect(http.StatusOK, http.MethodGet, one+"/messages", nil), &msgs); err != nil {
		t.Fatal(err)
	}
	if n := len(msgs.Messages); n == 0 || msgs.Messages[n-1].Text != "echo: use pnpm" {
		t.Fatalf("messages = %+v; want the steer answered in the same run", msgs.Messages)
	}

	// The view rebuilt from the stream is the one a new stream starts from. The answer streams
	// in pieces, so read until the stream has been quiet for a while.
	for quiet := false; !quiet; {
		select {
		case ev := <-stream.events:
			batches = append(batches, ev.Ops)
		case <-time.After(time.Second):
			quiet = true
		}
	}
	stream.cancel()
	fresh := owner.openEvents(one + "/events")
	snapshot := fresh.next(t)
	var want any
	if err := json.Unmarshal(snapshot.Value, &want); err != nil {
		t.Fatal(err)
	}
	if before := rebuild(t, first.Value, []json.RawMessage{}); len(batches) == 0 || reflect.DeepEqual(before, want) {
		t.Fatalf("%d ops batches; the view didn't change, so the comparison below would prove nothing", len(batches))
	}
	if got := rebuild(t, first.Value, batches); !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.Marshal(got)
		t.Fatalf("rebuilt view differs from a fresh one:\nrebuilt %s\nfresh   %s", gotJSON, snapshot.Value)
	}

	// The model: the default, then the owner's own provider, which nobody else may use.
	getModel := func(a *api, path string) map[string]string {
		var m map[string]string
		if err := json.Unmarshal(a.expect(http.StatusOK, http.MethodGet, path, nil), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := getModel(owner, one+"/model"); m["provider"] != "script" || m["model_id"] != "script-1" {
		t.Fatalf("model = %v", m)
	}
	var account struct {
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal(owner.expect(http.StatusCreated, http.MethodPost, "/api/me/providers", map[string]any{
		"name": "home", "base_url": "http://127.0.0.1:9/v1", "models": []map[string]any{{"id": "home-model"}},
	}), &account); err != nil {
		t.Fatal(err)
	}
	personal := map[string]any{"provider": account.Provider, "model_id": "home-model"}
	owner.expect(http.StatusOK, http.MethodPut, one+"/model", personal)
	if m := getModel(owner, one+"/model"); m["provider"] != account.Provider {
		t.Fatalf("model after the switch = %v", m)
	}
	owner.expect(http.StatusOK, http.MethodPut, one+"/model", map[string]any{"provider": "script", "model_id": "script-1"})
	owner.expect(http.StatusBadRequest, http.MethodPut, one+"/model", map[string]any{"provider": "script", "model_id": "nope"})
	member.expect(http.StatusBadRequest, http.MethodPost, base, map[string]any{"model": personal})
	var mine struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(member.expect(http.StatusCreated, http.MethodPost, base, map[string]any{}), &mine); err != nil {
		t.Fatal(err)
	}
	member.expect(http.StatusBadRequest, http.MethodPut, fmt.Sprintf("%s/%d/model", base, mine.ID), personal)

	// Another member of the workspace reaches nothing of the owner's conversation.
	for _, r := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, one, nil},
		{http.MethodPatch, one, map[string]any{"title": "mine"}},
		{http.MethodPost, one + "/messages", map[string]any{"content": "hi"}},
		{http.MethodGet, one + "/messages", nil},
		{http.MethodGet, one + "/model", nil},
		{http.MethodPut, one + "/model", map[string]any{"provider": "script", "model_id": "script-1"}},
		{http.MethodGet, one + "/events", nil},
	} {
		member.expect(http.StatusNotFound, r.method, r.path, r.body)
	}
	var listed struct {
		Conversations []struct {
			ID int64 `json:"id"`
		} `json:"conversations"`
	}
	if err := json.Unmarshal(member.expect(http.StatusOK, http.MethodGet, base, nil), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Conversations) != 1 || listed.Conversations[0].ID != mine.ID {
		t.Fatalf("the member lists %+v, want only their own", listed.Conversations)
	}

	// The sidecar dies: the open stream ends, and a new one starts from the current view once
	// the supervisor has restarted it.
	h, err := sidecar.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(h.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for ev := range fresh.events {
		if ev.Type == "end" {
			if ev.Kind != "interrupted" {
				t.Fatalf("end = %+v, want interrupted", ev)
			}
			break
		}
	}
	if _, ok := <-fresh.events; ok {
		t.Fatal("the stream went on after its end")
	}
	waitFor(t, "a new sidecar", func() bool {
		h2, err := sidecar.Health(ctx)
		return err == nil && h2.PID != h.PID
	})
	again := owner.openEvents(one + "/events")
	if ev := again.next(t); ev.Type != "value" || !strings.Contains(string(ev.Value), "echo: use pnpm") {
		t.Fatalf("after the restart: %+v, want the current view", ev)
	}
}

// A conversation the agent runtime lost, through the real server and sidecar: its routes
// answer 410 and the server logs the mismatch; `taskto conversations` finds it and, with
// clean -lost, deletes it, while a conversation the sidecar created just now without a row
// (one the server could be recording) isn't taken for an orphan.
func TestLostConversationThroughTheServer(t *testing.T) {
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
	server, addr := startServer(t, bin, "-addr", "127.0.0.1:0", "-data-dir", dataDir, "-agent", "-agent-entry", entry, "-insecure-cookies")
	_, socketDir, _ := agentPaths(dataDir)
	sidecar := agentrt.New(agentrt.SocketPath(socketDir))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	owner := &api{t: t, base: "http://" + addr, http: &http.Client{Jar: jar}}
	token := setupTokenLine.FindStringSubmatch(server.output.String())
	if token == nil {
		t.Fatal("the server logged no setup token")
	}
	var me struct {
		Workspaces []struct {
			ID int64 `json:"id"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(owner.expect(http.StatusCreated, http.MethodPost, "/api/setup", map[string]string{
		"email": "owner@example.com", "name": "Owner", "password": "fake-password-for-tests", "workspace_name": "Home",
		"setup_token": token[1],
	}), &me); err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("/api/workspaces/%d/conversations", me.Workspaces[0].ID)
	var kept, lost struct {
		ID int64 `json:"id"`
	}
	for _, c := range []any{&kept, &lost} {
		if err := json.Unmarshal(owner.expect(http.StatusCreated, http.MethodPost, base, map[string]any{}), c); err != nil {
			t.Fatal(err)
		}
	}
	// One with no row: as if recording it had failed a moment ago.
	if _, err := sidecar.CreateConversation(ctx); err != nil {
		t.Fatal(err)
	}

	// The agent runtime loses one: as if its data had been restored from an older backup
	// than the server's. Retiring it makes the sidecar answer 404 for it, the same as that.
	sqlDB, err := db.Open(ctx, filepath.Join(dataDir, db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	var agentID int64
	if err := sqlDB.QueryRow(`SELECT agent_id FROM conversations WHERE id = ?`, lost.ID).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	if err := sidecar.RetireConversation(ctx, agentID); err != nil {
		t.Fatal(err)
	}
	one := fmt.Sprintf("%s/%d", base, lost.ID)
	if got := string(owner.expect(http.StatusGone, http.MethodGet, one+"/messages", nil)); !strings.Contains(got, "no longer has this conversation") {
		t.Fatalf("messages of the lost conversation = %s", got)
	}
	owner.expect(http.StatusGone, http.MethodPost, one+"/messages", map[string]any{"content": "hello?"})
	owner.expect(http.StatusOK, http.MethodGet, fmt.Sprintf("%s/%d/messages", base, kept.ID), nil)
	if want := fmt.Sprintf("agent_conversation=%d", agentID); !strings.Contains(server.output.String(), want) {
		t.Fatalf("the server didn't log the mismatch (%s)", want)
	}

	run := func(args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if err := runConversations(ctx, append([]string{"-data-dir", dataDir}, args...), func(string) string { return "" }, &stdout, &stderr); err != nil {
			t.Fatalf("taskto conversations %v: %v\n%s", args, err, stderr.String())
		}
		return stdout.String()
	}
	out := run("check")
	if !strings.Contains(out, "lost: 1 conversations") || !strings.Contains(out, fmt.Sprintf("conversation %d in workspace", lost.ID)) ||
		!strings.Contains(out, "orphaned: 0") {
		t.Fatalf("check printed:\n%s\nwant conversation %d lost and no orphans yet", out, lost.ID)
	}
	if out := run("-lost", "-orphans", "clean"); out != "deleted 1 lost conversations\nretired 0 orphaned agent runtime conversations\n" {
		t.Fatalf("clean printed %q", out)
	}
	owner.expect(http.StatusNotFound, http.MethodGet, one, nil)
	owner.expect(http.StatusOK, http.MethodGet, fmt.Sprintf("%s/%d/messages", base, kept.ID), nil)
}
