package conversations

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/apierr"
)

const sidecarEntry = "../../agent/dist/test-entry.js"

type output struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

func (o *output) has(fragment string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return strings.Contains(o.buf.String(), fragment)
}

type realAgent struct {
	client *agentrt.Client
	out    *output
	gate   string
}

func (a *realAgent) release(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(a.gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func startRealAgent(t *testing.T) *realAgent {
	t.Helper()
	if os.Getenv("TASKTO_AGENT_IT") != "1" {
		t.Skip("set TASKTO_AGENT_IT=1 to run the agent sidecar integration tests")
	}
	entry, err := filepath.Abs(sidecarEntry)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "taskto-conv-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	config := filepath.Join(dir, "providers.json")
	if err := os.WriteFile(config, []byte(`{"providers":[{"id":"script","kind":"script"},{"id":"backup","kind":"script"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &realAgent{out: &output{}, gate: filepath.Join(dir, "gate")}
	socket := filepath.Join(dir, "agent.sock")
	cmd := exec.Command("node", entry)
	cmd.Env = append(os.Environ(),
		"TASKTO_AGENT_SOCKET="+socket,
		"TASKTO_AGENT_DB="+filepath.Join(dir, "agent.db"),
		"TASKTO_AGENT_PROVIDERS="+config,
		"TASKTO_AGENT_TEST_CONTINUE_GATE="+a.gate,
	)
	cmd.Stderr = a.out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	a.client = agentrt.New(socket)
	waitFor(t, "the sidecar to answer", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return a.client.Healthy(ctx) == nil
	})
	return a
}

func lastEntryOf(t *testing.T, c *agentrt.Client, id int64) int64 {
	t.Helper()
	raw, err := c.Entries(t.Context(), id, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Entries []struct {
			ID int64 `json:"id"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &page); err != nil || len(page.Entries) == 0 {
		t.Fatalf("entries %s: %v", raw, err)
	}
	return page.Entries[len(page.Entries)-1].ID
}

func (e *env) countRows(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(query, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

type failedConversation struct {
	e     *env
	agent *realAgent
	owner *client
	ws    int64
	c     Conversation
	id    int64
	after int64
	retry map[string]any
}

func (f *failedConversation) transcript() string {
	f.e.t.Helper()
	raw, err := f.agent.client.Entries(f.e.t.Context(), f.id, 0, 100)
	if err != nil {
		f.e.t.Fatal(err)
	}
	return string(raw)
}

func (f *failedConversation) lastEntry() int64 {
	f.e.t.Helper()
	return lastEntryOf(f.e.t, f.agent.client, f.id)
}

func newFailedConversation(t *testing.T) *failedConversation {
	t.Helper()
	a := startRealAgent(t)
	e := newEnv(t, false)
	e.svc.agent = func(context.Context) (Runtime, error) { return Client(a.client), nil }
	e.svc.admitLimit, e.svc.admitMargin = 600*time.Millisecond, 100*time.Millisecond
	e.runnable[1].Models[0].ID = "script-1"
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	id := c.agentIDForTest(t, e)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	state, err := a.client.Model(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := a.client.Submit(ctx, id, agentrt.Input{Content: "fail:script", ExpectRevision: &state.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := a.client.Wait(ctx, failed); err != nil || settled.Status != "unanswered" {
		t.Fatalf("the failing request settled %+v: %v", settled, err)
	}
	f := &failedConversation{e: e, agent: a, owner: owner, ws: ws, c: c, id: id}
	f.after = f.lastEntry()
	f.retry = e.continueBody("backup", "script-1", f.after)
	return f
}

func (f *failedConversation) base() string { return path(f.ws, fmt.Sprintf("/%d", f.c.ID)) }

func (f *failedConversation) continueTimesOut() {
	f.e.t.Helper()
	rec := f.owner.expect(http.StatusServiceUnavailable, http.MethodPost, f.base()+"/continue", f.retry)
	if code := apiErrorCode(f.e.t, rec.Body.Bytes()); code != string(apierr.CodeAgentNotReady) {
		f.e.t.Fatalf("the continue answered 503 %q, want %q", code, apierr.CodeAgentNotReady)
	}
	if !f.agent.out.has("test: the continue is waiting before its commit") {
		f.e.t.Fatal("the continue never reached the sidecar")
	}
}

func (f *failedConversation) lateContinueMustHaveAdmittedNothing(before string) {
	f.e.t.Helper()
	f.agent.release(f.e.t)
	waitFor(f.e.t, "the sidecar to end the late continue", func() bool { return f.agent.out.has("test: the continue ended") })
	if got := f.transcript(); got != before {
		f.e.t.Fatalf("a continue that reached the sidecar after its 503 changed the conversation:\n%s\n%s", before, got)
	}
	state, err := f.agent.client.Model(f.e.t.Context(), f.id)
	if err != nil || state.Provider != "script" {
		f.e.t.Fatalf("the model is %+v, %v after the late continue; want script", state, err)
	}
	if n := f.e.countRows(`SELECT COUNT(*) FROM agent_usage_daily WHERE workspace_id = ? AND source = 'agent' AND engine = 'backup'`, f.ws); n != 0 {
		f.e.t.Fatalf("%d usage rows for a reply that was never admitted", n)
	}
}

func TestRealAgentAContinueThatReachesTheSidecarAfterItsAnswerOf503AdmitsNothingAfterAnArchive(t *testing.T) {
	f := newFailedConversation(t)
	before := f.transcript()
	f.continueTimesOut()

	archived := decode[Conversation](t, f.owner.expect(http.StatusOK, http.MethodPatch, f.base(), map[string]any{"archived": true}))
	if archived.ArchivedAt == nil {
		t.Fatal("the archive did not take effect")
	}
	fresh := f.owner.expect(http.StatusConflict, http.MethodPost, f.base()+"/continue", f.retry)
	if code := apiErrorCode(t, fresh.Body.Bytes()); code != string(apierr.CodeConversationArchived) {
		t.Fatalf("a fresh continue of the archived conversation answered %q", code)
	}
	if _, err := f.e.db.Exec(`DELETE FROM search_conversation_queue WHERE conversation_id = ?`, f.c.ID); err != nil {
		t.Fatal(err)
	}
	f.lateContinueMustHaveAdmittedNothing(before)
	if n := f.e.countRows(`SELECT COUNT(*) FROM search_conversation_queue WHERE conversation_id = ?`, f.c.ID); n != 0 {
		t.Fatalf("the conversation was queued for indexing %d times for a reply that was never admitted", n)
	}
}

func TestRealAgentALateContinueLeavesNeitherAReplyNorMissingSearchAndUsageRecords(t *testing.T) {
	f := newFailedConversation(t)
	before := f.transcript()
	f.continueTimesOut()
	if _, err := f.e.db.Exec(`DELETE FROM search_conversation_queue WHERE conversation_id = ?`, f.c.ID); err != nil {
		t.Fatal(err)
	}
	f.lateContinueMustHaveAdmittedNothing(before)
	if n := f.e.countRows(`SELECT COUNT(*) FROM search_conversation_queue WHERE conversation_id = ?`, f.c.ID); n != 0 {
		t.Fatalf("the conversation was queued for indexing %d times for a reply that was never admitted", n)
	}
}

func TestRealAgentAContinueThatIsAnsweredInTimeIsAdmittedWithItsSearchAndUsageRecords(t *testing.T) {
	f := newFailedConversation(t)
	f.agent.release(t)
	if _, err := f.e.db.Exec(`DELETE FROM search_conversation_queue WHERE conversation_id = ?`, f.c.ID); err != nil {
		t.Fatal(err)
	}
	rec := f.owner.expect(http.StatusAccepted, http.MethodPost, f.base()+"/continue", f.retry)
	got := decode[SubmissionReply](t, rec)
	if got.Model.Provider != "backup" {
		t.Fatalf("the continue ran on %+v, want backup", got.Model)
	}
	waitFor(t, "the continued reply's usage", func() bool {
		return f.e.countRows(`SELECT COUNT(*) FROM agent_usage_daily WHERE workspace_id = ? AND source = 'agent' AND engine = 'backup'`, f.ws) == 1
	})
	waitFor(t, "the continued reply to be queued for indexing", func() bool {
		return f.e.countRows(`SELECT COUNT(*) FROM search_conversation_queue WHERE conversation_id = ?`, f.c.ID) == 1
	})
	if f.lastEntry() == f.after {
		t.Fatal("the continue added no reply")
	}
}
