// Package mcpserver is the server's MCP endpoint: people's own agents (Claude Code, Codex,
// any MCP client) use the knowledge base and the boards through it, with the tools the
// conversation agent has (internal/agenttools).
//
// It speaks the Model Context Protocol over Streamable HTTP at /mcp, statelessly: every
// request is a POST of one JSON-RPC message, answered with JSON, and carries the access
// token it is made with ("Authorization: Bearer ttp_…", see internal/auth). Nothing is kept
// between requests, so every request is checked from scratch:
//
//   - the token must exist, not have expired, and its user must still be a member of its
//     workspace (auth.Service.TokenUser); otherwise 401;
//   - each tool call acts for that user in that workspace only, and checks every space it
//     names with package access, as the conversation agent's do;
//   - changes are the user's, marked with the token's name: the committer of a document's
//     commit, and the "via" of a board change.
//
// Requests a browser marks as cross-origin are refused, and a body must be JSON: no web
// page can use a token it doesn't already have, and the endpoint never reads a cookie.
// docs/mcp.md says how to set up clients. The tests are this package's, for what the endpoint
// itself does (its tool lists and hints, its refusals), and internal/server's tokens_test.go,
// through the whole server.
//
// ForTurns is the same endpoint for the CLIs of conversations' turns (docs/cli-tools.md), on
// the per-turn proxy's listener: its caller is the conversation's owner, from the turn's token,
// and it has the browser tools too while the agent browser is on.
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

// Path is where the endpoint is served.
const Path = "/mcp"

// maxBody bounds a request: a tool's arguments, and the JSON-RPC message around them.
const maxBody = agenttools.MaxArgs + 64<<10

// instructions are what a client tells its model about the server.
const instructions = `TaskTo.AI: the knowledge base (Markdown documents in spaces) and the boards (cards in columns) of one workspace, as the user whose access token this is, with exactly their access. Start with list_spaces, for the tools that take a space_id. Documents are read with read_doc and changed with write_doc, move_doc, delete_doc and restore_doc (doc_history lists a document's commits), passing the version read; cards are changed with the version last read.`

type callerKey struct{}

// New returns the endpoint's handler: tokens are checked with accounts, and tools run the
// calls. version is the server's, as it introduces itself.
func New(version string, accounts *auth.Service, tools *agenttools.Service) http.Handler {
	// The agent browser's tools are left out: their hand-over to the user is in a conversation.
	return serve(version, instructions, tools, false, true, authenticate(accounts))
}

// turnInstructions are what a turn's CLI tells its model about the server.
const turnInstructions = `TaskTo.AI: the knowledge base (Markdown documents in spaces), the boards (cards in columns) and the personal apps of the workspace this conversation is in, as the user whose conversation it is, with exactly their access; and, when the server has it on, the user's own browser. Start with list_spaces: every other knowledge-base, board and app tool takes a space_id from it. Documents are read with read_doc and changed with write_doc, move_doc, delete_doc and restore_doc (doc_history lists a document's commits), passing the version read; cards are changed with the version last read. After browser_help, end your turn: the user answers in the conversation.`

// Authorize returns whom a request is for, or refuses it with an *agenttools.Refusal.
type Authorize func(*http.Request) (agenttools.Caller, error)

// ForTurns returns the endpoint for the CLIs of conversations' turns: authorize finds each
// request's caller from its turn's token (cliaccounts.Service.ToolsCaller), and tools run the
// calls; browser says whether to offer the agent browser's tools.
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

// serve returns the MCP handler of the tools, behind cross-origin protection and auth, which
// puts each request's caller on its context. withBrowser adds the conversation-only tools; tokens
// is for the endpoint of access tokens, which leaves out the tools only a conversation has.
func serve(version, instructions string, tools *agenttools.Service, withBrowser, tokens bool, auth func(http.Handler) http.Handler) http.Handler {
	s := mcp.NewServer(&mcp.Implementation{Name: "taskto", Title: "TaskTo.AI", Version: version},
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
		Stateless:    true,
		JSONResponse: true,
		// The SDK refuses requests to a loopback address with another Host, against DNS
		// rebinding. That is every request behind a reverse proxy on the same machine, and
		// a rebinding page has no token to send: the token is the protection here.
		DisableLocalhostProtection: true,
		MaxRequestBodyBytes:        maxBody,
	})
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusForbidden, "cross-origin request refused")
	}))
	return cop.Handler(auth(h))
}

// authenticate lets through requests with a valid access token, with whom they act for on
// the context, and answers 401 to the rest. It never logs or echoes the token.
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

// handler runs a tool call for the request's caller. A refusal is a tool error, with the
// message the conversation agent would get, so the model can correct itself.
func handler(tools *agenttools.Service) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		c, ok := ctx.Value(callerKey{}).(agenttools.Caller)
		if !ok {
			// authenticate puts the caller on every request: this is a wiring error.
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
