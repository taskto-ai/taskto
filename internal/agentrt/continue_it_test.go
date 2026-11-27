package agentrt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func twoScriptModels(t *testing.T) []string {
	t.Helper()
	config := map[string]any{"providers": []any{
		map[string]any{"id": "script", "kind": "script"},
		map[string]any{"id": "backup", "kind": "script", "name": "Backup"},
	}}
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(path, []byte(mustJSON(config)), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"TASKTO_AGENT_PROVIDERS=" + path}
}

func conflictReason(err error) string {
	var se *StatusError
	if errors.As(err, &se) && se.Code == 409 {
		return se.Reason
	}
	return ""
}

func TestContinueAfterAFailedReplyAnswersOnTheNewModel(t *testing.T) {
	s := startSidecar(t, twoScriptModels(t)...)
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := s.client.Submit(ctx, conv, Input{Content: "fail:script"})
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := s.client.Wait(ctx, failed); err != nil || settled.Status != "unanswered" || settled.Reason != "model_error" {
		t.Fatalf("the failing request settled %+v, %v; want unanswered with model_error", settled, err)
	}
	if err := s.client.SetModel(ctx, conv, ModelRef{Provider: "backup", ModelID: "script-1"}); err != nil {
		t.Fatal(err)
	}
	sub, err := s.client.Continue(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := s.client.Wait(ctx, sub); err != nil || settled.Status != "done" {
		t.Fatalf("the continue settled %+v, %v; want done", settled, err)
	}
	if got := lastAssistant(t, s.client, conv); got != "echo: fail:script" {
		t.Fatalf("the answer after the continue = %q, want the request answered again on the new model", got)
	}
}

func TestContinueOfABusyConversationIsRefusedAndStartsNothing(t *testing.T) {
	s := startSidecar(t)
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.client.Watch(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	running, err := s.client.Submit(ctx, conv, Input{Content: "tool:pause_safe:30000"})
	if err != nil {
		t.Fatal(err)
	}
	waitForOps(t, w, "waited 200ms")
	if _, err := s.client.Continue(ctx, conv); conflictReason(err) != "busy" {
		t.Fatalf("continue while a run is going: err = %v, want a 409 with reason busy", err)
	}
	if err := s.client.Abort(ctx, conv); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if settled, err := s.client.Wait(waitCtx, running); err != nil || settled.Status != "unanswered" {
		t.Fatalf("the running submission = %+v, %v; want unanswered", settled, err)
	}
}

func TestContinueOfARetiredOrUnknownConversationIsNotFound(t *testing.T) {
	s := startSidecar(t)
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.client.RetireConversation(ctx, conv); err != nil {
		t.Fatal(err)
	}
	if _, err := s.client.Continue(ctx, conv); !notFound(err) {
		t.Fatalf("continue of a retired conversation: err = %v, want a 404", err)
	}
	if _, err := s.client.Continue(ctx, conv+1000); !notFound(err) {
		t.Fatalf("continue of an unknown conversation: err = %v, want a 404", err)
	}
}

func TestContinueOvertakenByARetireStartsNoRun(t *testing.T) {
	s := startSidecar(t, "TASKTO_AGENT_TEST_CONTINUE_PAUSE_MS=600")
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resumed := make(chan error, 1)
	go func() {
		_, err := s.client.Continue(ctx, conv)
		resumed <- err
	}()
	waitForOutput(t, s, "test: the continue is waiting before its commit")
	if err := s.client.RetireConversation(ctx, conv); err != nil {
		t.Fatal(err)
	}
	if err := <-resumed; !notFound(err) {
		t.Fatalf("a continue the retire overtook: err = %v, want a 404", err)
	}
}
