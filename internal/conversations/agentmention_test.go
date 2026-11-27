package conversations

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

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
