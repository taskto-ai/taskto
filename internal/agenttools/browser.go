package agenttools

import (
	"context"
	"encoding/json"
	"net/http"
)

var browserTools = []string{
	"browser_tabs", "browser_open", "browser_navigate", "browser_snapshot", "browser_click",
	"browser_type", "browser_press", "browser_close", "browser_help",
}

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
