package conversations

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/agentrt"
)

func TestStartIsIdempotentAndKeepsOriginPrivate(t *testing.T) {
	e := newEnv(t, false)
	owner, workspace := e.setup()
	e.svc.capability = func(conversation, agentConversation int64) string {
		return fmt.Sprintf("cap:%d:%d", conversation, agentConversation)
	}
	e.svc.userLanguage = func(context.Context, int64) string { return "pt-BR" }
	st := Start{
		Workspace: workspace,
		User:      owner.id,
		Origin:    Origin{Kind: "app", Key: "app-effect:12", SpaceID: 3, AppID: 5},
		Title:     "Generated report",
		Prompt:    "Summarize this record.",
	}
	first, err := e.svc.Start(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.svc.Start(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if first.Conversation.ID != second.Conversation.ID || first.Submission != second.Submission {
		t.Fatalf("retry returned %+v, first was %+v", second, first)
	}
	if first.Conversation.Origin == nil || first.Conversation.Origin.Kind != "app" ||
		first.Conversation.Origin.SpaceID != 3 || first.Conversation.Origin.AppID != 5 {
		t.Fatalf("origin = %+v", first.Conversation.Origin)
	}
	if e.rt.created[0] != (agentrt.ModelRef{}) || len(e.rt.created) != 1 || len(e.rt.inputs[101]) != 1 {
		t.Fatalf("created=%v inputs=%v", e.rt.created, e.rt.inputs)
	}
	input := e.rt.inputs[first.Conversation.agentID][0]
	if input.RequestID != "origin:app-effect:12" || input.Capability != "" || input.Language != "pt-BR" {
		t.Fatalf("input = %+v", input)
	}
	st.Tools = true
	third, err := e.svc.Start(context.Background(), st)
	if err != nil || third.Conversation.ID != first.Conversation.ID {
		t.Fatalf("tools retry = %+v, %v", third, err)
	}
	other := st
	other.Origin.Key = "app-effect:13"
	if _, err := e.svc.Start(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if len(e.rt.created) != 2 {
		t.Fatalf("created %d runtime conversations, want 2", len(e.rt.created))
	}
	listed := owner.expect(http.StatusOK, http.MethodGet, path(workspace, ""), nil)
	if got := listed.Body.String(); strings.Contains(got, "app-effect:12") {
		t.Fatalf("the private origin key was returned: %s", got)
	}
}

func TestStartSubmitsCapabilityOnlyWhenRequested(t *testing.T) {
	e := newEnv(t, false)
	owner, workspace := e.setup()
	e.svc.capability = func(conversation, agentConversation int64) string { return "taskto-capability" }
	base := Start{Workspace: workspace, User: owner.id, Prompt: "Do work", Origin: Origin{Kind: "routine", Key: "routine-run:1", RoutineID: 2}}
	started, err := e.svc.Start(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.rt.inputs[101][0].Capability; got != "" {
		t.Fatalf("capability without tools = %q", got)
	}
	base.Origin.Key = "routine-run:2"
	base.Tools = true
	started, err = e.svc.Start(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.rt.inputs[102][0].Capability; got != "taskto-capability" {
		t.Fatalf("capability with tools = %q", got)
	}
	if err := e.svc.Stop(context.Background(), started.Conversation.ID); err != nil {
		t.Fatal(err)
	}
	if e.rt.aborts[102] != 1 {
		t.Fatalf("aborts = %v", e.rt.aborts)
	}
}

func TestStartRequiresActiveMembershipAndUsableModel(t *testing.T) {
	e := newEnv(t, false)
	owner, workspace := e.setup()
	other := e.workspace()
	base := Start{Workspace: other, User: owner.id, Prompt: "Do work", Origin: Origin{Kind: "app", Key: "app-effect:18"}}
	if _, err := e.svc.Start(context.Background(), base); !errors.Is(err, ErrNotMember) {
		t.Fatalf("non-member error = %v", err)
	}
	member := e.invite(owner, workspace, "member@example.com")
	base.Workspace = workspace
	base.Model.Provider = fmt.Sprintf("u%d-personal", member.id)
	base.Model.ModelID = "private-model"
	if _, err := e.svc.Start(context.Background(), base); !errors.Is(err, ErrModelUnknown) {
		t.Fatalf("another user's model error = %v", err)
	}
	base.Model = agentrt.ModelRef{}
	if _, err := e.db.Exec("UPDATE users SET disabled_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?", owner.id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Start(context.Background(), base); !errors.Is(err, ErrNotMember) {
		t.Fatalf("disabled user error = %v", err)
	}
}

func TestStartFailsWhenAgentRuntimeIsOff(t *testing.T) {
	e := newEnv(t, true)
	owner, workspace := e.setup()
	_, err := e.svc.Start(context.Background(), Start{
		Workspace: workspace, User: owner.id, Prompt: "Do work", Origin: Origin{Kind: "app", Key: "app-effect:19"},
	})
	if !errors.Is(err, ErrRuntimeDown) {
		t.Fatalf("runtime-off error = %v", err)
	}
}

func TestOutcomeWaitsAndReturnsLastAssistantReply(t *testing.T) {
	e := newEnv(t, false)
	owner, workspace := e.setup()
	started, err := e.svc.Start(context.Background(), Start{
		Workspace: workspace, User: owner.id, Prompt: "Do work", Origin: Origin{Kind: "routine", Key: "routine-run:3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.rt.mu.Lock()
	e.rt.settle = make(chan struct{})
	e.rt.replies[started.Conversation.agentID] = []agentrt.Message{{Role: "assistant", Text: "first"}, {Role: "assistant", Text: "last"}}
	e.rt.mu.Unlock()
	out, err := e.svc.Outcome(context.Background(), started.Conversation.ID, started.Submission, 5*time.Millisecond)
	if err != nil || out.Settled {
		t.Fatalf("pending outcome = %+v, %v", out, err)
	}
	e.rt.mu.Lock()
	close(e.rt.settle)
	e.rt.settle = nil
	e.rt.mu.Unlock()
	out, err = e.svc.Outcome(context.Background(), started.Conversation.ID, started.Submission, time.Second)
	if err != nil || !out.Settled || out.Status != "done" || out.Reply != "last" {
		t.Fatalf("settled outcome = %+v, %v", out, err)
	}
}

func TestStartedConversationRemainsPrivateToItsOwner(t *testing.T) {
	e := newEnv(t, false)
	owner, workspace := e.setup()
	member := e.invite(owner, workspace, "member@example.com")
	started, err := e.svc.Start(context.Background(), Start{
		Workspace: workspace, User: owner.id, Prompt: "Private prompt", Origin: Origin{Kind: "app", Key: "app-effect:21"},
	})
	if err != nil {
		t.Fatal(err)
	}
	list := decode[struct{ Conversations []Conversation }](t, member.expect(http.StatusOK, http.MethodGet, path(workspace, ""), nil))
	for _, conversation := range list.Conversations {
		if conversation.ID == started.Conversation.ID {
			t.Fatal("another member can list a server-started conversation")
		}
	}
	member.expect(http.StatusNotFound, http.MethodGet, path(workspace, fmt.Sprintf("/%d", started.Conversation.ID)), nil)
}
