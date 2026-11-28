package conversations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/apierr"
)

func TestAgentMentionParsing(t *testing.T) {
	cases := []struct {
		content string
		want    modelRef
		ok      bool
	}{
		{"@[Codex](agent:cli-codex/default) review this", modelRef{"cli-codex", "default"}, true},
		{"@[Codex](agent:cli-codex/default)", modelRef{"cli-codex", "default"}, true},
		{"@[Codex](agent:cli-codex/default)\nreview this", modelRef{"cli-codex", "default"}, true},
		{"@[Claude Code](agent:u7-home/gpt-4o:mini.v2) hi", modelRef{"u7-home", "gpt-4o:mini.v2"}, true},
		{"review @[Codex](agent:cli-codex/default) this", modelRef{}, false},
		{" @[Codex](agent:cli-codex/default) hi", modelRef{}, false},
		{"@[Codex](agent:cli-codex/default)hi", modelRef{}, false},
		{"@[Codex](agent:cli-codex) hi", modelRef{}, false},
		{"@[](agent:cli-codex/default) hi", modelRef{}, false},
		{"@[Co\ndex](agent:cli-codex/default) hi", modelRef{}, false},
		{"@[" + strings.Repeat("a", 101) + "](agent:cli-codex/default) hi", modelRef{}, false},
		{"@[Codex](agent:cli codex/default) hi", modelRef{}, false},
		{"@[Codex](user:3) hi", modelRef{}, false},
		{"> @[Codex](agent:cli-codex/default) hi", modelRef{}, false},
	}
	for _, c := range cases {
		got, ok := agentMention(c.content)
		if ok != c.ok || got != c.want {
			t.Errorf("agentMention(%q) = %+v, %v; want %+v, %v", c.content, got, ok, c.want, c.ok)
		}
	}
}

func mention(provider, model, rest string) map[string]any {
	return map[string]any{"content": fmt.Sprintf("@[Agent](agent:%s/%s) %s", provider, model, rest)}
}

func TestLeadingAgentMentionSwitchesThenSubmits(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	send := path(ws, fmt.Sprintf("/%d/messages", c.ID))
	thisWS := fmt.Sprintf("ws%d-gateway", ws)

	body := mention("cli-codex", "default", "review this")
	reply := decode[SendReply](t, owner.expect(http.StatusAccepted, http.MethodPost, send, body))
	if reply.SubmissionID == 0 || reply.Model == nil || *reply.Model != (modelRef{"cli-codex", "default"}) {
		t.Fatalf("reply = %+v, want a submission and the model cli-codex/default", reply)
	}
	owner.expect(http.StatusAccepted, http.MethodPost, send, mention(thisWS, "m", "and now this"))
	plain := decode[SendReply](t, owner.expect(http.StatusAccepted, http.MethodPost, send, map[string]any{"content": "no mention"}))
	if plain.Model != nil {
		t.Fatalf("a message with no mention answered a model: %+v", plain.Model)
	}

	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	if want := []string{"set-model cli-codex/default", "submit", "set-model " + thisWS + "/m", "submit", "submit"}; fmt.Sprint(e.rt.calls) != fmt.Sprint(want) {
		t.Fatalf("the agent runtime saw %q, want %q", e.rt.calls, want)
	}
	agent := c.agentIDForTest(t, e)
	if got := e.rt.models[agent]; got != (agentrt.ModelRef{Provider: thisWS, ModelID: "m"}) {
		t.Fatalf("model = %+v, want the last one mentioned", got)
	}
	if got := e.rt.inputs[agent][0].Content; got != body["content"] {
		t.Fatalf("the agent got %q, want the token kept in the message", got)
	}
}

func TestAgentMentionLaterInTheTextChangesNothing(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	send := path(ws, fmt.Sprintf("/%d/messages", c.ID))

	for _, content := range []string{
		"ask @[Codex](agent:cli-codex/default) to review this",
		"> @[Codex](agent:cli-codex/default) said so",
		"@[Codex](agent:cli-codex/default)no space",
		"@[Codex](agent:cli-codex) no model",
	} {
		reply := decode[SendReply](t, owner.expect(http.StatusAccepted, http.MethodPost, send, map[string]any{"content": content}))
		if reply.Model != nil {
			t.Fatalf("%q answered the model %+v", content, reply.Model)
		}
	}

	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	if want := []string{"submit", "submit", "submit", "submit"}; fmt.Sprint(e.rt.calls) != fmt.Sprint(want) {
		t.Fatalf("the agent runtime saw %q, want only submissions", e.rt.calls)
	}
	if got := e.rt.models[c.agentIDForTest(t, e)]; got != defaultModel {
		t.Fatalf("model = %+v, want it unchanged", got)
	}
}

func TestAgentMentionOfAModelTheCallerMayNotUseSendsNothing(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	member := e.invite(owner, ws, "member@example.com")
	other := e.workspace(owner, member)
	c := owner.create(ws, nil)
	send := path(ws, fmt.Sprintf("/%d/messages", c.ID))

	for _, provider := range []string{
		fmt.Sprintf("u%d-home", member.id),
		fmt.Sprintf("ws%d-gateway", other),
		"u0-x",
		"ws0-x",
	} {
		rec := owner.expect(http.StatusBadRequest, http.MethodPost, send, mention(provider, "m", "hello"))
		if got := decode[map[string]any](t, rec)["code"]; got != string(apierr.CodeModelUnknown) {
			t.Fatalf("%s: code = %v, want %s", provider, got, apierr.CodeModelUnknown)
		}
	}

	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	if len(e.rt.calls) != 0 {
		t.Fatalf("the agent runtime saw %q, want nothing", e.rt.calls)
	}
	if got := e.rt.models[c.agentIDForTest(t, e)]; got != defaultModel {
		t.Fatalf("model = %+v, want it unchanged", got)
	}
}

func TestAgentMentionNeedsAWritableConversationOfTheCaller(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	member := e.invite(owner, ws, "member@example.com")
	mine := owner.create(ws, nil)
	archived := owner.create(ws, nil)
	owner.expect(http.StatusOK, http.MethodPatch, path(ws, fmt.Sprintf("/%d", archived.ID)), map[string]any{"archived": true})

	owner.expect(http.StatusConflict, http.MethodPost, path(ws, fmt.Sprintf("/%d/messages", archived.ID)), mention("cli-codex", "default", "hi"))
	member.expect(http.StatusNotFound, http.MethodPost, path(ws, fmt.Sprintf("/%d/messages", mine.ID)), mention("cli-codex", "default", "hi"))
	owner.expect(http.StatusBadRequest, http.MethodPost, path(ws, fmt.Sprintf("/%d/messages", mine.ID)), map[string]any{"content": "   "})

	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	if len(e.rt.calls) != 0 {
		t.Fatalf("the agent runtime saw %q, want nothing", e.rt.calls)
	}
	for _, c := range []Conversation{mine, archived} {
		if got := e.rt.models[c.agentIDForTest(t, e)]; got != defaultModel {
			t.Fatalf("model of %d = %+v, want it unchanged", c.ID, got)
		}
	}
}

func TestAgentMentionRechecksTheArchiveBeforeSwitching(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	e.svc.agent = func(ctx context.Context) (Runtime, error) {
		if _, err := e.db.Exec(`UPDATE conversations SET archived_at = 'now' WHERE id = ?`, c.ID); err != nil {
			t.Error(err)
		}
		return e.rt, nil
	}
	owner.expect(http.StatusConflict, http.MethodPost, path(ws, fmt.Sprintf("/%d/messages", c.ID)), mention("cli-codex", "default", "hi"))

	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	if len(e.rt.calls) != 0 {
		t.Fatalf("the agent runtime saw %q, want nothing", e.rt.calls)
	}
}

func TestAgentMentionFailureOfTheSwitchSendsNothing(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	e.rt.mu.Lock()
	e.rt.err = &agentrt.StatusError{Code: 400, Message: "unknown model", Reason: "unknown_model"}
	e.rt.mu.Unlock()

	rec := owner.expect(http.StatusBadRequest, http.MethodPost, path(ws, fmt.Sprintf("/%d/messages", c.ID)), mention("cli-codex", "default", "hi"))
	if got := decode[map[string]any](t, rec)["code"]; got != string(apierr.CodeModelUnknown) {
		t.Fatalf("code = %v, want %s", got, apierr.CodeModelUnknown)
	}
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	if len(e.rt.inputs[c.agentIDForTest(t, e)]) != 0 {
		t.Fatalf("a message was sent after the switch failed")
	}
}

func TestAMentionWhoseSendFailsLeavesTheConversationOnItsAgent(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	send := path(ws, fmt.Sprintf("/%d/messages", c.ID))
	agent := c.agentIDForTest(t, e)

	e.rt.mu.Lock()
	e.rt.submitErr = errors.New("submit failed")
	e.rt.mu.Unlock()
	owner.expect(http.StatusServiceUnavailable, http.MethodPost, send, mention("cli-codex", "default", "hello"))

	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	if got := e.rt.models[agent]; got != defaultModel {
		t.Fatalf("a mention whose send failed left the model %+v, want %+v", got, defaultModel)
	}
	if len(e.rt.inputs[agent]) != 0 {
		t.Fatalf("the failed send recorded %d inputs", len(e.rt.inputs[agent]))
	}
}

func TestAModelChosenWhileAMentionsSendFailsIsKept(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	send := path(ws, fmt.Sprintf("/%d/messages", c.ID))
	chosen := agentrt.ModelRef{Provider: fmt.Sprintf("ws%d-gateway", ws), ModelID: "m"}
	agent := c.agentIDForTest(t, e)

	submitting, release := make(chan struct{}), make(chan struct{})
	e.rt.mu.Lock()
	e.rt.submitErr = errors.New("submit failed")
	e.rt.onSubmit = func() {
		close(submitting)
		<-release
	}
	e.rt.mu.Unlock()

	sent := make(chan int, 1)
	go func() { sent <- owner.do(http.MethodPost, send, mention("cli-codex", "default", "hello")).Code }()
	<-submitting
	set := make(chan int, 1)
	go func() {
		set <- owner.do(http.MethodPut, path(ws, fmt.Sprintf("/%d/model", c.ID)), map[string]any{"provider": chosen.Provider, "model_id": chosen.ModelID}).Code
	}()
	for waiting := false; !waiting; {
		select {
		case code := <-set:
			set <- code
			waiting = true
		default:
			e.svc.mu.Lock()
			waiting = e.svc.modelHolds[c.ID] != nil && e.svc.modelHolds[c.ID].users == 2
			e.svc.mu.Unlock()
			runtime.Gosched()
		}
	}
	close(release)
	if code := <-sent; code != http.StatusServiceUnavailable {
		t.Fatalf("the failed send answered %d, want 503", code)
	}
	if code := <-set; code != http.StatusOK {
		t.Fatalf("the owner's choice answered %d, want 200", code)
	}

	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	if got := e.rt.models[agent]; got != chosen {
		t.Fatalf("the model is %+v after the failed send, want the owner's choice %+v", got, chosen)
	}
}

func apiErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return e.Code
}

func TestTheOwnersModelChangeGivesUpOnAHeldModel(t *testing.T) {
	e := newEnv(t, false)
	e.svc.modelWait = 50 * time.Millisecond
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	send := path(ws, fmt.Sprintf("/%d/messages", c.ID))
	chosen := agentrt.ModelRef{Provider: fmt.Sprintf("ws%d-gateway", ws), ModelID: "m"}
	agent := c.agentIDForTest(t, e)

	submitting, release := make(chan struct{}), make(chan struct{})
	e.rt.mu.Lock()
	e.rt.onSubmit = func() {
		close(submitting)
		<-release
	}
	e.rt.mu.Unlock()

	sent := make(chan int, 1)
	go func() { sent <- owner.do(http.MethodPost, send, mention("cli-codex", "default", "hello")).Code }()
	<-submitting
	e.rt.mu.Lock()
	during := e.rt.models[agent]
	e.rt.mu.Unlock()

	set := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		set <- owner.do(http.MethodPut, path(ws, fmt.Sprintf("/%d/model", c.ID)), map[string]any{"provider": chosen.Provider, "model_id": chosen.ModelID})
	}()
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-set:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("the model change was still waiting for a held model long after its bound")
	}
	if rec.Code != http.StatusConflict || apiErrorCode(t, rec.Body.Bytes()) != string(apierr.CodeConversationBusy) {
		t.Fatalf("the model change answered %d %s, want 409 conversation_busy", rec.Code, rec.Body.String())
	}
	e.rt.mu.Lock()
	after := e.rt.models[agent]
	e.rt.mu.Unlock()
	if after != during {
		t.Fatalf("the refused model change moved the model from %+v to %+v", during, after)
	}
	close(release)
	if code := <-sent; code != http.StatusAccepted {
		t.Fatalf("the mention's send answered %d, want 202", code)
	}
}

func TestAStalledSendEndsAtItsDeadlineAndFreesTheHold(t *testing.T) {
	e := newEnv(t, false)
	e.svc.admitLimit, e.svc.admitMargin = 50*time.Millisecond, time.Millisecond
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	send := path(ws, fmt.Sprintf("/%d/messages", c.ID))
	chosen := agentrt.ModelRef{Provider: fmt.Sprintf("ws%d-gateway", ws), ModelID: "m"}
	agent := c.agentIDForTest(t, e)

	e.rt.mu.Lock()
	e.rt.stallSend = true
	before := e.rt.models[agent]
	e.rt.mu.Unlock()

	sent := make(chan int, 1)
	go func() { sent <- owner.do(http.MethodPost, send, mention("cli-codex", "default", "hello")).Code }()
	select {
	case code := <-sent:
		if code != http.StatusServiceUnavailable {
			t.Fatalf("the stalled send answered %d, want 503", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the stalled send never ended")
	}
	e.rt.mu.Lock()
	got := e.rt.models[agent]
	e.rt.mu.Unlock()
	if got != before {
		t.Fatalf("the model is %+v after the stalled send, want it put back to %+v", got, before)
	}
	e.svc.mu.Lock()
	holds := len(e.svc.modelHolds)
	e.svc.mu.Unlock()
	if holds != 0 {
		t.Fatalf("%d model holds are left after the stalled send ended", holds)
	}
	owner.expect(http.StatusOK, http.MethodPut, path(ws, fmt.Sprintf("/%d/model", c.ID)), map[string]any{"provider": chosen.Provider, "model_id": chosen.ModelID})
}

func TestAStalledPutBackEndsAtItsOwnDeadlineAndFreesTheHold(t *testing.T) {
	e := newEnv(t, false)
	e.svc.admitLimit, e.svc.admitMargin = 50*time.Millisecond, time.Millisecond
	e.svc.putBackLimit = 50 * time.Millisecond
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	send := path(ws, fmt.Sprintf("/%d/messages", c.ID))

	e.rt.mu.Lock()
	e.rt.stallSend = true
	e.rt.stallPutBack = true
	e.rt.mu.Unlock()

	sent := make(chan int, 1)
	go func() { sent <- owner.do(http.MethodPost, send, mention("cli-codex", "default", "hello")).Code }()
	select {
	case code := <-sent:
		if code != http.StatusServiceUnavailable {
			t.Fatalf("the send whose put-back stalled answered %d, want 503", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the put-back never ended")
	}
	e.svc.mu.Lock()
	holds := len(e.svc.modelHolds)
	e.svc.mu.Unlock()
	if holds != 0 {
		t.Fatalf("%d model holds are left after the put-back stalled", holds)
	}
}
