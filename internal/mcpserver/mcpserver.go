package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"taskto.ai/taskto/internal/agenttools"
	"taskto.ai/taskto/internal/auth"
)

const Path = "/mcp"

const maxBody = agenttools.MaxArgs + 64<<10

const instructions = `taskto.ai: the knowledge base (Markdown documents in spaces) and the boards (cards in columns) of one workspace, as the user whose access token this is, with exactly their access. Start with list_spaces, for the tools that take a space_id. Documents are read with read_doc and changed with write_doc, move_doc, delete_doc and restore_doc (doc_history lists a document's commits), passing the version read; cards are changed with the version last read.`

type callerKey struct{}

func New(version string, accounts *auth.Service, tools *agenttools.Service) http.Handler {
	return serve(version, instructions, tools, false, true, authenticate(accounts))
}

const turnInstructions = `taskto.ai: the knowledge base (Markdown documents in spaces), the boards (cards in columns) and the personal apps of the workspace this conversation is in, as the user whose conversation it is, with exactly their access; and, when the server has it on, the user's own browser. Start with list_spaces: every other knowledge-base, board and app tool takes a space_id from it. Documents are read with read_doc and changed with write_doc, move_doc, delete_doc and restore_doc (doc_history lists a document's commits), passing the version read; cards are changed with the version last read. After browser_help, end your turn: the user answers in the conversation.`

type Authorize func(*http.Request) (agenttools.Caller, error)

func ForTurns(version string, authorize Authorize, tools *agenttools.Service, browser bool) http.Handler {
	return serve(version, turnInstructions, tools, browser, false, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := authorize(r)
			var refused *agenttools.Refusal
			switch {
			case errors.As(err, &refused):
				if refused.Status == http.StatusUnauthorized {
					w.Header().Set("WWW-Authenticate", `Bearer realm="taskto", error="invalid_token"`)
				}
				writeError(w, refused.Status, refused.Message)
				return
			case err != nil:
				if r.Context().Err() == nil {
					slog.ErrorContext(r.Context(), "mcp: checking a turn's token", "err", err)
					writeError(w, http.StatusInternalServerError, "internal error")
				}
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, c)))
		})
	})
}

func serve(version, instructions string, tools *agenttools.Service, withBrowser, tokens bool, auth func(http.Handler) http.Handler) http.Handler {
	s := mcp.NewServer(&mcp.Implementation{Name: "taskto", Title: "taskto.ai", Version: version},
		&mcp.ServerOptions{Instructions: instructions})
	closed := false
	for _, d := range agenttools.Definitions() {
		if (d.ConversationOnly && !withBrowser) || (d.NoToken && tokens) {
			continue
		}
		s.AddTool(&mcp.Tool{
			Name:        d.Name,
			Description: d.Description,
			InputSchema: d.InputSchema,
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: d.ReadOnly, OpenWorldHint: &closed},
		}, handler(tools))
	}
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{
		Stateless:                  true,
		JSONResponse:               true,
		DisableLocalhostProtection: true,
		MaxRequestBodyBytes:        maxBody,
	})
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusForbidden, "cross-origin request refused")
	}))
	return cop.Handler(auth(h))
}

func authenticate(accounts *auth.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return authenticated(accounts, next)
	}
}

func authenticated(accounts *auth.Service, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := auth.BearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="taskto"`)
			writeError(w, http.StatusUnauthorized, "an access token is required: Authorization: Bearer ttp_… (see docs/mcp.md)")
			return
		}
		u, t, err := accounts.TokenUser(r.Context(), raw)
		if errors.Is(err, auth.ErrTokenInvalid) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="taskto", error="invalid_token"`)
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}
		if err != nil {
			if r.Context().Err() == nil {
				slog.ErrorContext(r.Context(), "mcp: checking an access token", "err", err)
				writeError(w, http.StatusInternalServerError, "internal error")
			}
			return
		}
		c := agenttools.Caller{Workspace: t.WorkspaceID, User: u.ID, Token: t.Name}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, c)))
	})
}

func handler(tools *agenttools.Service) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		c, ok := ctx.Value(callerKey{}).(agenttools.Caller)
		if !ok {
			return nil, errors.New("no caller")
		}
		out, err := tools.Call(ctx, c, req.Params.Name, req.Params.Arguments)
		var refused *agenttools.Refusal
		if errors.As(err, &refused) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "error: " + refused.Message}}}, nil
		}
		if err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(out)}}}, nil
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
