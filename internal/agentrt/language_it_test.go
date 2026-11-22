package agentrt

// The agent's language reaches its system prompt, and stays with the conversation. Gated
// by TASKTO_AGENT_IT=1.

import (
	"strings"
	"testing"
)

func TestLanguageSection(t *testing.T) {
	s := startSidecar(t)
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// "system" makes the scripted model answer with its system prompt.
	system := func(in Input) string {
		t.Helper()
		in.Content = "system"
		sub, err := s.client.Submit(ctx, conv, in)
		if err != nil {
			t.Fatal(err)
		}
		if settled, err := s.client.Wait(ctx, sub); err != nil || settled.Status != "done" {
			t.Fatalf("wait = %+v, %v", settled, err)
		}
		return lastAssistant(t, s.client, conv)
	}

	if got := system(Input{}); !strings.Contains(got, "Reply in the language the user writes in.") {
		t.Fatalf("without a language: %q", got)
	}
	if got := system(Input{Language: "pt-BR"}); !strings.Contains(got, "The user's language is Brazilian Portuguese (português (Brasil), pt-BR).") {
		t.Fatalf("pt-BR: %q", got)
	}
	// Kept across a restart.
	s.kill()
	s.start()
	if got := system(Input{Language: "pt-BR"}); !strings.Contains(got, "Brazilian Portuguese") {
		t.Fatalf("after a restart: %q", got)
	}
	if got := system(Input{Language: ""}); !strings.Contains(got, "Reply in the language the user writes in.") || strings.Contains(got, "Portuguese") {
		t.Fatalf("cleared: %q", got)
	}

	if _, err := s.client.Submit(ctx, conv, Input{Content: "x", Language: "pt_BR"}); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("a malformed language: %v", err)
	}
}
