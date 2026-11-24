// Package agenttools is the knowledge-base, board and app tools of agents: the server's side of
// the conversation agent's tools, which the sidecar calls back into the server for, and the
// tools of the MCP server (internal/mcpserver), which people's own agents call with an access
// token. Either way it acts for one user in one workspace, with exactly their access.
//
// The tools are defined once, in tools.json: names, descriptions and the JSON Schemas of
// their arguments (Definitions). The MCP server lists them from there, and the sidecar from
// its copy, agent/src/taskto-tools.json, which go generate writes and a test keeps the same.
//
// The sidecar's channel is internal: the agentrt supervisor serves it on an owner-only Unix socket
// next to the sidecar's own (SupervisorConfig.Tools), never on the network, and it has no
// sessions or cookies. A call names its conversation with that conversation's capability, a
// MAC under the server key over the conversation's ID and the agent runtime's, which the
// server issues and every message sent to the conversation carries to the sidecar
// (conversations.SetCapabilities).
// A capability only says which conversation the call is for; whom it acts for, and what
// they may do, is looked up again on every call:
//
//   - the conversation must exist, not be archived, and its owner must still be a member of
//     its workspace (conversations.PrincipalOf);
//   - every space is checked with access.Checker for the owner, and must be in the
//     conversation's workspace, so a conversation never reaches the owner's spaces of
//     another workspace (whose model providers are someone else's choice). A space the
//     owner can't read, or of another workspace, is "not found", as over HTTP;
//   - writes are the owner's, marked as the agent's: knowledge commits have the owner as
//     author and the agent as committer (knowledge.Author.Agent), board and app changes
//     are boards.Actor{Agent: true} and apps.Actor{Agent: true}.
//
// A call over MCP is checked the same way (Call), for the token's user in the token's
// workspace, and its writes are marked with the token's name instead of the agent's.
//
// Every reply is bounded (MaxResult): documents are read in pieces, long texts in lists
// are cut, lists are paged or shortened to fit.
//
// The tools are listed in docs/agent-tools.md.
package agenttools

import (
	"context"
	"crypto/hmac"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/boards"
	"taskto.ai/taskto/internal/conversations"
	"taskto.ai/taskto/internal/knowledge"
	"taskto.ai/taskto/internal/search"
	"taskto.ai/taskto/internal/serverkey"
)

// AgentName is the agent's name in the knowledge base's history: the committer of the
// changes it makes for a user.
const AgentName = "TaskTo.AI agent"

const (
	// MaxResult bounds a tool's reply, in bytes of JSON.
	MaxResult = 64 << 10
	// maxBody bounds a call's arguments: a document written is at most this.
	maxBody = 1 << 20
)

// Options are what the tools work with. Search may be nil: search_docs then says search
// is off.
type Options struct {
	DB        *sql.DB
	Key       *serverkey.Key
	Access    *access.Checker
	Boards    *boards.Service
	Apps      *apps.Service
	Knowledge *knowledge.Store
	Search    *search.Index
	// Browser runs the agent browser's tools (internal/agentbrowser); nil means the browser
	// is off, and its tools say so.
	Browser BrowserTools
}

// BrowserTools runs the agent browser's tools for user, in conversation. A refusal it
// returns as a *Refusal goes to the model as it is.
type BrowserTools interface {
	Call(ctx context.Context, user, conversation int64, name string, args json.RawMessage) (any, error)
}

// Service serves the tools; it is an http.Handler for the supervisor's tools socket.
type Service struct {
	o     Options
	tools map[string]tool
}

// tool runs one call: args is the request body, the tool's own JSON object.
type tool func(ctx context.Context, c *call, args json.RawMessage) (any, error)

//go:generate cp tools.json ../../agent/src/taskto-tools.json

//go:embed tools.json
var definitionsJSON []byte

// Definition is a tool as an agent sees it.
type Definition struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// ReadOnly tools change nothing: the sidecar may run them again after a crash, and MCP
	// clients may call them without asking.
	ReadOnly bool `json:"read_only"`
	// InputSchema is the JSON Schema of the arguments: an object.
	InputSchema json.RawMessage `json:"input_schema"`
	// ConversationOnly tools are the conversation agent's alone, not listed over MCP: the
	// agent browser's, whose hand-over to the user happens in a conversation.
	ConversationOnly bool `json:"conversation_only,omitempty"`
}

// Definitions returns the tools, in the order of tools.json.
func Definitions() []Definition {
	var defs []Definition
	if err := json.Unmarshal(definitionsJSON, &defs); err != nil {
		panic("agenttools: tools.json: " + err.Error())
	}
	return defs
}

// New returns the tools over o.
func New(o Options) *Service {
	s := &Service{o: o}
	s.tools = map[string]tool{
		"list_spaces":    s.listSpaces,
		"list_members":   s.listMembers,
		"search_docs":    s.searchDocs,
		"list_docs":      s.listDocs,
		"read_doc":       s.readDoc,
		"write_doc":      s.writeDoc,
		"list_boards":    s.listBoards,
		"list_cards":     s.listCards,
		"show_card":      s.showCard,
		"create_card":    s.createCard,
		"update_card":    s.updateCard,
		"move_card":      s.moveCard,
		"comment_card":   s.commentCard,
		"set_blocked":    s.setBlocked,
		"app_catalog":    s.appCatalog,
		"list_apps":      s.listApps,
		"show_app":       s.showApp,
		"list_app_rows":  s.listAppRows,
		"query_app_rows": s.queryAppRows,
		"create_app":     s.createApp,
		"update_app":     s.updateApp,

		"list_app_templates":   s.listAppTemplates,
		"install_app_template": s.installAppTemplate,
		"duplicate_app":        s.duplicateApp,
		"list_app_effects":     s.listAppEffects,
		"list_app_releases":    s.listAppReleases,
		"show_app_roles":       s.showAppRoles,
	}
	for _, name := range browserTools {
		s.tools[name] = s.browserTool(name)
	}
	for _, d := range Definitions() {
		if s.tools[d.Name] == nil {
			panic("agenttools: tools.json defines " + d.Name + ", which has no implementation")
		}
	}
	return s
}

// capabilityPurpose separates the capabilities' MACs from any other use of the server key.
const capabilityPurpose = "taskto agent tools: conversation"

// capabilityPrefix starts every capability, and says its version.
const capabilityPrefix = "ttc1."

// Capability returns the capability of conversation, whose agent runtime ID is
// agentConversation: "ttc1.<conversation ID>.<MAC>". The MAC covers both IDs, so it would
// never fit another conversation that came to have the same ID. It is the same every
// time, so it can be handed out with every message; it changes only with the server key.
func (s *Service) Capability(conversation, agentConversation int64) string {
	id := strconv.FormatInt(conversation, 10)
	return capabilityPrefix + id + "." + base64.RawURLEncoding.EncodeToString(s.mac(conversation, agentConversation))
}

func (s *Service) mac(conversation, agentConversation int64) []byte {
	return s.o.Key.MAC(capabilityPurpose, fmt.Appendf(nil, "%d/%d", conversation, agentConversation))
}

// parseCapability returns the conversation a capability names, and its MAC, still to be
// checked against the conversation's agent runtime ID.
func parseCapability(capability string) (int64, []byte, bool) {
	rest, ok := strings.CutPrefix(capability, capabilityPrefix)
	if !ok {
		return 0, nil, false
	}
	id, sum, ok := strings.Cut(rest, ".")
	if !ok {
		return 0, nil, false
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != id {
		return 0, nil, false
	}
	mac, err := base64.RawURLEncoding.DecodeString(sum)
	return n, mac, err == nil
}

// Caller is whom a call is for: a user, in one workspace.
type Caller struct {
	Workspace int64
	User      int64
	// Token is the name of the access token the call came with, over MCP; empty for the
	// conversation agent's calls.
	Token string
	// Conversation is the conversation the agent's call is for; 0 over MCP.
	Conversation int64
}

// call is one tool call.
type call struct {
	Caller
}

// errNotFound is the answer for a space, document, board or card that doesn't exist or
// that the user can't read: the model can't tell the two apart either.
var errNotFound = errors.New("not found")

// errInvalid is for arguments a tool can't use; its message says why.
type errInvalid string

func (e errInvalid) Error() string { return string(e) }

func invalidf(format string, args ...any) error { return errInvalid(fmt.Sprintf(format, args...)) }

// space returns spaceID as the call's user sees it, if they may read it and it is in the
// call's workspace, and errNotFound otherwise. It asks package access every time.
func (s *Service) space(ctx context.Context, c *call, spaceID int64) (access.Space, error) {
	sp, err := s.o.Access.Check(ctx, c.User, spaceID)
	if errors.Is(err, access.ErrNotFound) || (err == nil && sp.WorkspaceID != c.Workspace) {
		return access.Space{}, errNotFound
	}
	return sp, err
}

// ServeHTTP serves POST /tools/{name}: the body is the tool's arguments, a JSON object, and
// the Authorization header is "Bearer <capability>". A result is 200 with its JSON; a
// refusal is 4xx {"error": "…"}, with a message for the model.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, ok := strings.CutPrefix(r.URL.Path, "/tools/")
	t := s.tools[name]
	if !ok || t == nil {
		writeError(w, http.StatusNotFound, "unknown tool")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "tools are called with POST")
		return
	}
	p, err := s.Authorize(r)
	var refusal *Refusal
	if errors.As(err, &refusal) {
		writeError(w, refusal.Status, refusal.Message)
		return
	}
	if err != nil {
		s.fail(w, r, name, err)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the arguments are over %d KiB", maxBody>>10))
		return
	}
	out, err := s.Call(r.Context(), Caller{Workspace: p.Workspace, User: p.User, Conversation: p.Conversation}, name, body)
	if err != nil {
		s.fail(w, r, name, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// #nosec G705 -- a JSON reply, on the sidecar's own socket: no browser reads it.
	_, _ = w.Write(out)
}

// Authorize returns whom a request on the tools socket is for: the principal of the
// conversation its capability ("Authorization: Bearer <capability>") names, as of now. A
// missing or wrong capability, or a conversation that can't use tools any more, is a
// *Refusal with the status to answer; any other error is the database's.
func (s *Service) Authorize(r *http.Request) (conversations.Principal, error) {
	const noCapability = "no valid capability for this conversation: send it a new message"
	token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	id, mac, ok := parseCapability(token)
	if !bearer || !ok {
		return conversations.Principal{}, &Refusal{Status: http.StatusUnauthorized, Message: noCapability}
	}
	p, err := conversations.PrincipalOf(r.Context(), s.o.DB, id)
	if errors.Is(err, conversations.ErrInactive) {
		return conversations.Principal{}, &Refusal{Status: http.StatusForbidden, Message: err.Error()}
	}
	if err != nil {
		return conversations.Principal{}, err
	}
	if !hmac.Equal(mac, s.mac(p.Conversation, p.AgentConversation)) {
		return conversations.Principal{}, &Refusal{Status: http.StatusUnauthorized, Message: noCapability}
	}
	return p, nil
}

// MaxArgs bounds a call's arguments, in bytes of JSON: a document written is at most this.
const MaxArgs = maxBody

// Call runs tool name with args, a JSON object, for c, and returns its result: at most
// MaxResult bytes of JSON. The caller must have checked that c.User is a member of
// c.Workspace, now; every space the call names is checked here. A refusal is a *Refusal;
// any other error means ctx ended.
func (s *Service) Call(ctx context.Context, c Caller, name string, args json.RawMessage) (json.RawMessage, error) {
	t := s.tools[name]
	if t == nil {
		return nil, &Refusal{Status: http.StatusNotFound, Message: "unknown tool"}
	}
	res, err := t(ctx, &call{Caller: c}, args)
	if err != nil {
		return nil, s.refusal(ctx, name, err)
	}
	out, err := json.Marshal(res)
	if err != nil {
		return nil, s.refusal(ctx, name, err)
	}
	if len(out) > MaxResult {
		// The tools shape their results to fit; this is the backstop.
		return nil, &Refusal{Status: http.StatusRequestEntityTooLarge, Message: fmt.Sprintf("the result is over %d KiB: ask for less", MaxResult>>10)}
	}
	return out, nil
}

// Refusal is a call the tools turned down: Message says why, for the model, and Status is
// the HTTP status the tools socket answers with.
type Refusal struct {
	Status  int
	Message string
}

func (r *Refusal) Error() string { return r.Message }

// refusal returns the Refusal for err, logging errors that aren't the caller's; it returns
// ctx's error if ctx ended.
func (s *Service) refusal(ctx context.Context, name string, err error) error {
	var invalid errInvalid
	var refused *Refusal
	switch {
	case errors.As(err, &refused):
		return refused
	case errors.As(err, &invalid), errors.Is(err, boards.ErrInvalid), errors.Is(err, knowledge.ErrInvalid), errors.Is(err, search.ErrInvalid),
		errors.Is(err, apps.ErrInvalid):
		return &Refusal{http.StatusBadRequest, err.Error()}
	case errors.Is(err, errNotFound), errors.Is(err, boards.ErrNotFound), errors.Is(err, access.ErrNotFound), errors.Is(err, apps.ErrNotFound):
		return &Refusal{http.StatusNotFound, errNotFound.Error()}
	case errors.Is(err, knowledge.ErrNotFound):
		return &Refusal{http.StatusNotFound, err.Error()}
	case errors.Is(err, boards.ErrReadOnly), errors.Is(err, boards.ErrBoardArchived), errors.Is(err, knowledge.ErrReadOnly), errors.Is(err, apps.ErrReadOnly),
		errors.Is(err, apps.ErrForbidden):
		return &Refusal{http.StatusForbidden, err.Error()}
	case errors.Is(err, boards.ErrStale), errors.Is(err, knowledge.ErrConflict), errors.Is(err, apps.ErrStale), errors.Is(err, apps.ErrFull):
		return &Refusal{http.StatusConflict, err.Error()}
	case errors.Is(err, knowledge.ErrTooLarge), errors.Is(err, knowledge.ErrRepoFull):
		return &Refusal{http.StatusRequestEntityTooLarge, err.Error()}
	default:
		if ctx.Err() != nil {
			return ctx.Err() // the caller went away
		}
		slog.ErrorContext(ctx, "agent tools: call failed", "tool", name, "err", err)
		return &Refusal{http.StatusInternalServerError, "internal error"}
	}
}

// fail answers with the status and message for err.
func (s *Service) fail(w http.ResponseWriter, r *http.Request, name string, err error) {
	var refused *Refusal
	if !errors.As(err, &refused) {
		err = s.refusal(r.Context(), name, err)
		if !errors.As(err, &refused) {
			return // the sidecar went away
		}
	}
	writeError(w, refused.Status, refused.Message)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// decode reads a tool's arguments strictly: unknown fields and trailing data are errors,
// so a misspelt argument doesn't silently do something else. An empty body is {}.
func decode(raw json.RawMessage, v any) error {
	if len(strings.TrimSpace(string(raw))) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalidf("invalid arguments: %v", err)
	}
	if dec.More() {
		return errInvalid("invalid arguments: one JSON object expected")
	}
	return nil
}

// fitting returns the largest n of total for which build(n) is at most MaxResult bytes of
// JSON, and build(n); n is 0 if even that doesn't fit.
func fitting(total int, build func(n int) any) (int, any, error) {
	size := func(n int) (int, error) {
		b, err := json.Marshal(build(n))
		return len(b), err
	}
	lo, hi := 0, total // build(lo) fits, if anything does
	if n, err := size(hi); err != nil || n <= MaxResult {
		return hi, build(hi), err
	}
	for lo < hi-1 {
		mid := (lo + hi) / 2
		n, err := size(mid)
		if err != nil {
			return 0, nil, err
		}
		if n <= MaxResult {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo, build(lo), nil
}

// cut shortens s to at most n bytes, at a character boundary, and reports whether it did.
func cut(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}
