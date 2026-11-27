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
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/boards"
	"taskto.ai/taskto/internal/conversations"
	"taskto.ai/taskto/internal/integrations"
	"taskto.ai/taskto/internal/knowledge"
	"taskto.ai/taskto/internal/quota"
	"taskto.ai/taskto/internal/search"
	"taskto.ai/taskto/internal/serverkey"
)

const AgentName = "taskto.ai agent"

const (
	MaxResult = 64 << 10
	maxBody   = 1 << 20
)

type Options struct {
	DB           *sql.DB
	Key          *serverkey.Key
	Access       *access.Checker
	Boards       *boards.Service
	Apps         *apps.Service
	Knowledge    *knowledge.Store
	Search       *search.Index
	Integrations *integrations.Store
	Browser      BrowserTools
}

type BrowserTools interface {
	Call(ctx context.Context, user, conversation int64, name string, args json.RawMessage) (any, error)
}

type Service struct {
	o     Options
	tools map[string]tool
}

type tool func(ctx context.Context, c *call, args json.RawMessage) (any, error)

//go:generate cp tools.json ../../agent/src/taskto-tools.json

//go:embed tools.json
var definitionsJSON []byte

type Definition struct {
	Name             string          `json:"name"`
	Description      string          `json:"description"`
	ReadOnly         bool            `json:"read_only"`
	InputSchema      json.RawMessage `json:"input_schema"`
	ConversationOnly bool            `json:"conversation_only,omitempty"`
	NoToken          bool            `json:"no_token,omitempty"`
}

func Definitions() []Definition {
	var defs []Definition
	if err := json.Unmarshal(definitionsJSON, &defs); err != nil {
		panic("agenttools: tools.json: " + err.Error())
	}
	return defs
}

func New(o Options) *Service {
	s := &Service{o: o}
	s.tools = map[string]tool{
		"list_spaces":          s.listSpaces,
		"list_members":         s.listMembers,
		"search_docs":          s.searchDocs,
		"search_cards":         s.searchCards,
		"search_records":       s.searchRecords,
		"search_conversations": s.searchConversations,
		"list_docs":            s.listDocs,
		"read_doc":             s.readDoc,
		"write_doc":            s.writeDoc,
		"move_doc":             s.moveDoc,
		"delete_doc":           s.deleteDoc,
		"doc_history":          s.docHistory,
		"restore_doc":          s.restoreDoc,
		"list_boards":          s.listBoards,
		"list_cards":           s.listCards,
		"show_card":            s.showCard,
		"create_card":          s.createCard,
		"update_card":          s.updateCard,
		"move_card":            s.moveCard,
		"comment_card":         s.commentCard,
		"set_blocked":          s.setBlocked,
		"create_board":         s.createBoard,
		"update_board":         s.updateBoard,
		"archive_card":         s.archiveCard,
		"restore_card":         s.restoreCard,
		"app_catalog":          s.appCatalog,
		"list_apps":            s.listApps,
		"list_integrations":    s.listIntegrations,
		"show_app":             s.showApp,
		"list_app_rows":        s.listAppRows,
		"query_app_rows":       s.queryAppRows,
		"create_app":           s.createApp,
		"update_app":           s.updateApp,
		"list_app_actions":     s.listAppActions,
		"run_app_action":       s.runAppAction,

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

const capabilityPurpose = "taskto agent tools: conversation"

const capabilityPrefix = "ttc1."

func (s *Service) Capability(conversation, agentConversation int64) string {
	id := strconv.FormatInt(conversation, 10)
	return capabilityPrefix + id + "." + base64.RawURLEncoding.EncodeToString(s.mac(conversation, agentConversation))
}

func (s *Service) mac(conversation, agentConversation int64) []byte {
	return s.o.Key.MAC(capabilityPurpose, fmt.Appendf(nil, "%d/%d", conversation, agentConversation))
}

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

type Caller struct {
	Workspace    int64
	User         int64
	Token        string
	Conversation int64
}

type call struct {
	Caller
}

var errNotFound = errors.New("not found")

type errInvalid string

func (e errInvalid) Error() string { return string(e) }

func invalidf(format string, args ...any) error { return errInvalid(fmt.Sprintf(format, args...)) }

func (s *Service) space(ctx context.Context, c *call, spaceID int64) (access.Space, error) {
	sp, err := s.o.Access.Check(ctx, c.User, spaceID)
	if errors.Is(err, access.ErrNotFound) || (err == nil && sp.WorkspaceID != c.Workspace) {
		return access.Space{}, errNotFound
	}
	return sp, err
}

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
		writeRefusal(w, refusal)
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
	_, _ = w.Write(out)
}

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

const MaxArgs = maxBody

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
		return nil, &Refusal{Status: http.StatusRequestEntityTooLarge, Message: fmt.Sprintf("the result is over %d KiB: ask for less", MaxResult>>10)}
	}
	return out, nil
}

type Refusal struct {
	Status  int
	Message string
	Code    apierr.Code
	Params  apierr.Params
}

func (r *Refusal) Error() string { return r.Message }

func (s *Service) refusal(ctx context.Context, name string, err error) error {
	var invalid errInvalid
	var refused *Refusal
	var coded *apierr.Error
	switch {
	case errors.As(err, &refused):
		return refused
	case errors.As(err, &invalid), errors.Is(err, boards.ErrInvalid), errors.Is(err, knowledge.ErrInvalid), errors.Is(err, search.ErrInvalid),
		errors.Is(err, apps.ErrInvalid):
		return &Refusal{Status: http.StatusBadRequest, Message: err.Error()}
	case errors.Is(err, errNotFound), errors.Is(err, boards.ErrNotFound), errors.Is(err, access.ErrNotFound), errors.Is(err, apps.ErrNotFound):
		return &Refusal{Status: http.StatusNotFound, Message: errNotFound.Error()}
	case errors.Is(err, knowledge.ErrNotFound):
		return &Refusal{Status: http.StatusNotFound, Message: err.Error()}
	case errors.Is(err, boards.ErrReadOnly), errors.Is(err, boards.ErrBoardArchived), errors.Is(err, knowledge.ErrReadOnly), errors.Is(err, apps.ErrReadOnly),
		errors.Is(err, apps.ErrForbidden):
		return &Refusal{Status: http.StatusForbidden, Message: err.Error()}
	case errors.Is(err, boards.ErrStale), errors.Is(err, knowledge.ErrConflict), errors.Is(err, apps.ErrStale), errors.Is(err, apps.ErrFull),
		errors.Is(err, apps.ErrOffline), errors.Is(err, apps.ErrNotPublished), errors.Is(err, apps.ErrOperationReused):
		return &Refusal{Status: http.StatusConflict, Message: err.Error()}
	case errors.Is(err, apps.ErrReleaseOutdated):
		return &Refusal{Status: http.StatusConflict, Message: "the app's live release is not expected_release: nothing was done; call list_app_actions again and decide again"}
	case errors.Is(err, apps.ErrConflict):
		return &Refusal{Status: http.StatusConflict, Message: err.Error() + ": read the record again (query_app_rows) and decide again"}
	case apps.IsRateLimited(err):
		seconds, _ := apps.RetryAfter(err)
		unit := "seconds"
		if seconds == 1 {
			unit = "second"
		}
		return &Refusal{Status: http.StatusTooManyRequests, Message: fmt.Sprintf("too many actions run with access tokens just now: try again in %d %s", seconds, unit)}
	case errors.As(err, &coded):
		return &Refusal{Status: coded.Status, Message: coded.Msg, Code: coded.Code, Params: coded.Params}
	case errors.Is(err, knowledge.ErrTooLarge), errors.Is(err, knowledge.ErrRepoFull):
		return &Refusal{Status: http.StatusRequestEntityTooLarge, Message: err.Error()}
	case errors.Is(err, quota.ErrExceeded):
		return &Refusal{Status: http.StatusInsufficientStorage, Message: err.Error()}
	case errors.Is(err, quota.ErrUnavailable):
		return &Refusal{Status: http.StatusServiceUnavailable, Message: "the storage in use can't be measured right now; try again in a moment"}
	case errors.Is(err, knowledge.ErrNotKept):
		return &Refusal{Status: http.StatusGone, Message: err.Error()}
	default:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.ErrorContext(ctx, "agent tools: call failed", "tool", name, "err", err)
		return &Refusal{Status: http.StatusInternalServerError, Message: "internal error"}
	}
}

func (s *Service) fail(w http.ResponseWriter, r *http.Request, name string, err error) {
	var refused *Refusal
	if !errors.As(err, &refused) {
		err = s.refusal(r.Context(), name, err)
		if !errors.As(err, &refused) {
			return
		}
	}
	writeRefusal(w, refused)
}

func writeRefusal(w http.ResponseWriter, r *Refusal) {
	if r.Code == "" {
		writeError(w, r.Status, r.Message)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(r.Status)
	body := map[string]any{"error": r.Message, "code": r.Code}
	if len(r.Params) > 0 {
		body["params"] = r.Params
	}
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

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

func fitting(total int, build func(n int) any) (int, any, error) {
	size := func(n int) (int, error) {
		b, err := json.Marshal(build(n))
		return len(b), err
	}
	lo, hi := 0, total
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

func cut(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}
