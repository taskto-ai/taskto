package agenttools

import (
	"context"
	"encoding/json"
	"net/http"
)

// browserTools are the agent browser's tools (docs/agent-browser.md), run by Options.Browser.
var browserTools = []string{
	"browser_tabs", "browser_open", "browser_navigate", "browser_snapshot", "browser_click",
	"browser_type", "browser_press", "browser_close", "browser_help",
}

// browserTool is a tool of the agent browser: always for the call's user, the conversation's
// owner, and only in a conversation.
func (s *Service) browserTool(name string) tool {
	return func(ctx context.Context, c *call, args json.RawMessage) (any, error) {
		if s.o.Browser == nil {
			return nil, &Refusal{Status: http.StatusConflict, Message: "the agent browser is off on this server"}
		}
		if c.Conversation == 0 {
			return nil, &Refusal{Status: http.StatusForbidden, Message: "the browser tools work only in a taskto.ai conversation"}
		}
		return s.o.Browser.Call(ctx, c.User, c.Conversation, name, args)
	}
}
