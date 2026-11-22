package agenttools

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
)

// browserRecorder stands in for the agent browser, recording whom each call was for.
type browserRecorder struct {
	mu    sync.Mutex
	calls []struct{ user, conversation int64 }
}

func (b *browserRecorder) Call(_ context.Context, user, conversation int64, _ string, _ json.RawMessage) (any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, struct{ user, conversation int64 }{user, conversation})
	return map[string]string{"ok": "yes"}, nil
}

// The browser tools act for the conversation's owner, in that conversation: whom they are for
// comes from the capability, never from the arguments.
func TestBrowserToolsActForTheConversationsOwner(t *testing.T) {
	w := newWorld(t)
	if code, out := w.call(w.aliceConv, "browser_open", map[string]any{"url": "https://example.com"}); code != http.StatusConflict || out["error"] != "the agent browser is off on this server" {
		t.Fatalf("with the browser off: %d %v", code, out)
	}
	rec := &browserRecorder{}
	w.tools.o.Browser = rec
	w.ok(w.aliceConv, "browser_open", map[string]any{"url": "https://example.com"})
	w.ok(w.bobConv, "browser_tabs", map[string]any{})
	w.ok(w.aliceConv2, "browser_snapshot", map[string]any{"tab": "T1"})
	want := []struct{ user, conversation int64 }{{w.alice, w.aliceConv}, {w.bob, w.bobConv}, {w.alice, w.aliceConv2}}
	if len(rec.calls) != len(want) {
		t.Fatalf("calls = %v", rec.calls)
	}
	for i, c := range want {
		if rec.calls[i] != c {
			t.Errorf("call %d was for %v, want %v", i, rec.calls[i], c)
		}
	}
	// Over MCP (no conversation) the browser tools are refused, though they aren't listed.
	if _, err := w.tools.Call(context.Background(), Caller{Workspace: w.one, User: w.alice, Token: "t"}, "browser_open", json.RawMessage(`{"url":"https://example.com"}`)); err == nil {
		t.Fatal("a browser tool ran without a conversation")
	}
	if len(rec.calls) != len(want) {
		t.Fatal("the browser was reached over MCP")
	}
}
