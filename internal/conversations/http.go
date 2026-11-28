package conversations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/providers"
)

func (s *Service) Routes(mux *http.ServeMux, a *auth.Service) {
	s.language = a.Language
	s.watchSession = a.WatchSession
	member := func(h http.HandlerFunc) http.Handler { return a.RequireMember(h) }
	const base = "/api/workspaces/{workspace}/conversations"

	mux.Handle("GET "+base, member(s.handleList))
	mux.Handle("POST "+base, member(s.handleCreate))
	mux.Handle("GET "+base+"/{conversation}", member(s.handleGet))
	mux.Handle("PATCH "+base+"/{conversation}", member(s.handleUpdate))
	mux.Handle("POST "+base+"/{conversation}/messages", member(s.handleSend))
	mux.Handle("POST "+base+"/{conversation}/notes", member(s.handleNote))
	mux.Handle("GET "+base+"/{conversation}/messages", member(s.handleMessages))
	mux.Handle("GET "+base+"/{conversation}/model", member(s.handleModel))
	mux.Handle("PUT "+base+"/{conversation}/model", member(s.handleSetModel))
	mux.Handle("POST "+base+"/{conversation}/stop", member(s.handleStop))
	mux.Handle("GET "+base+"/{conversation}/entries", member(s.handleEntries))
	mux.Handle("GET "+base+"/{conversation}/events", member(s.handleEvents))
	mux.Handle("GET "+base+"/{conversation}/cli-permissions", member(s.handleQuestions))
	mux.Handle("POST "+base+"/{conversation}/cli-permissions/{id}", member(s.handleAnswer))
}

const (
	maxBody         = 64 << 10
	maxTitleLen     = 200
	maxRequestIDLen = 128
	maxRefLen       = 256
	pageEntries     = 50
	maxPageEntries  = 100
	abortTimeout    = 30 * time.Second
)

func caller(r *http.Request) (user, workspace int64) {
	u, _ := auth.UserFrom(r.Context())
	m, _ := auth.MembershipFrom(r.Context())
	return u.ID, m.WorkspaceID
}

func (s *Service) conversationInPath(w http.ResponseWriter, r *http.Request) (Conversation, bool) {
	id, err := strconv.ParseInt(r.PathValue("conversation"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, apierr.CodeConversationNotFound, errNotFound.Error())
		return Conversation{}, false
	}
	user, workspace := caller(r)
	c, err := s.find(r.Context(), workspace, user, id)
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusNotFound, apierr.CodeConversationNotFound, errNotFound.Error())
		return Conversation{}, false
	}
	if err != nil {
		serverError(w, r, err)
		return Conversation{}, false
	}
	return c, true
}

func (s *Service) writable(w http.ResponseWriter, r *http.Request) (Conversation, bool) {
	c, ok := s.conversationInPath(w, r)
	if ok && c.ArchivedAt != nil {
		writeError(w, http.StatusConflict, apierr.CodeConversationArchived, "the conversation is archived: unarchive it first")
		return Conversation{}, false
	}
	return c, ok
}

func (s *Service) stillWritable(w http.ResponseWriter, r *http.Request) bool {
	_, ok := s.writable(w, r)
	return ok
}

func (s *Service) runtime(w http.ResponseWriter, r *http.Request) (Runtime, bool) {
	if s.agent == nil {
		writeError(w, http.StatusServiceUnavailable, apierr.CodeAgentOff, "the agent runtime is off: start the server with -agent")
		return nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.readyWait)
	defer cancel()
	rt, err := s.agent(ctx)
	if err != nil {
		if r.Context().Err() == nil {
			unavailable(w)
		}
		return nil, false
	}
	return rt, true
}

func unavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "5")
	writeError(w, http.StatusServiceUnavailable, apierr.CodeAgentNotReady, "the agent runtime isn't ready: try again shortly")
}

func conversationError(w http.ResponseWriter, r *http.Request, c Conversation, err error) {
	var se *agentrt.StatusError
	if errors.As(err, &se) && se.Code == http.StatusNotFound && r.Context().Err() == nil {
		slog.ErrorContext(r.Context(), "conversations: the agent runtime doesn't have a recorded conversation; see taskto conversations check",
			"conversation", c.ID, "agent_conversation", c.agentID, "workspace", c.WorkspaceID)
		writeError(w, http.StatusGone, apierr.CodeConversationLost, "the agent runtime no longer has this conversation's content")
		return
	}
	agentError(w, r, err)
}

func agentError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	var se *agentrt.StatusError
	switch {
	case errors.As(err, &se) && (se.Code == http.StatusBadRequest || se.Code == http.StatusConflict):
		code, known := agentRefusals[se.Reason]
		if !known {
			code = apierr.CodeBadRequest
		}
		writeError(w, se.Code, code, se.Message)
	case errors.As(err, &se):
		serverError(w, r, err)
	default:
		slog.WarnContext(r.Context(), "conversations: agent runtime unreachable", "path", r.URL.Path, "err", err)
		unavailable(w)
	}
}

var agentRefusals = map[string]apierr.Code{
	"unknown_model": apierr.CodeModelUnknown,
	"no_model":      apierr.CodeConversationNoModel,
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	archived := false
	switch r.URL.Query().Get("archived") {
	case "", "false":
	case "true":
		archived = true
	default:
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, `archived must be "true" or "false"`)
		return
	}
	user, workspace := caller(r)
	list, err := s.list(r.Context(), workspace, user, archived)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]Conversation{"conversations": list})
}

type modelRef struct {
	Provider string `json:"provider"`
	ModelID  string `json:"model_id"`
}

func usable(w http.ResponseWriter, r *http.Request, m modelRef) (agentrt.ModelRef, bool) {
	if m.Provider == "" || m.ModelID == "" || len(m.Provider) > maxRefLen || len(m.ModelID) > maxRefLen {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "model must be {provider, model_id}")
		return agentrt.ModelRef{}, false
	}
	user, workspace := caller(r)
	if !providers.Usable(m.Provider, user, workspace) {
		writeError(w, http.StatusBadRequest, apierr.CodeModelUnknown, "unknown model")
		return agentrt.ModelRef{}, false
	}
	return agentrt.ModelRef{Provider: m.Provider, ModelID: m.ModelID}, true
}

func (s *Service) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title string    `json:"title"`
		Model *modelRef `json:"model"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	title, ok := validTitle(w, req.Title)
	if !ok {
		return
	}
	var model agentrt.ModelRef
	if req.Model != nil {
		if model, ok = usable(w, r, *req.Model); !ok {
			return
		}
	}
	rt, ok := s.runtime(w, r)
	if !ok {
		return
	}
	agentID, err := rt.CreateConversationOn(r.Context(), model)
	if err != nil {
		agentError(w, r, err)
		return
	}
	user, workspace := caller(r)
	c, err := s.insert(context.WithoutCancel(r.Context()), workspace, user, agentID, title)
	if err != nil {
		slog.ErrorContext(r.Context(), "conversations: recording a new conversation failed; the agent runtime's conversation is left unowned",
			"agent_conversation", agentID, "workspace", workspace, "by", user, "err", err)
		serverError(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "conversation created", "conversation", c.ID, "workspace", workspace, "by", user)
	writeJSON(w, http.StatusCreated, c)
}

func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) {
	if c, ok := s.conversationInPath(w, r); ok {
		writeJSON(w, http.StatusOK, c)
	}
}

func (s *Service) handleUpdate(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conversationInPath(w, r)
	if !ok {
		return
	}
	var req struct {
		Title          *string `json:"title"`
		Archived       *bool   `json:"archived"`
		CLIPermissions *string `json:"cli_permissions"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Title == nil && req.Archived == nil && req.CLIPermissions == nil {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "nothing to change: send title, archived or cli_permissions")
		return
	}
	if p := req.CLIPermissions; p != nil && *p != "ask" && *p != "allow_all" {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, `cli_permissions must be "ask" or "allow_all"`)
		return
	}
	if req.Title != nil {
		title, ok := validTitle(w, *req.Title)
		if !ok {
			return
		}
		req.Title = &title
	}
	user, workspace := caller(r)
	c, err := s.update(r.Context(), workspace, user, c.ID, req.Title, req.Archived, req.CLIPermissions)
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusNotFound, apierr.CodeConversationNotFound, errNotFound.Error())
		return
	}
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

var whenBusy = map[string]string{"": "", "follow_up": "followUp", "steer": "steer"}

func (s *Service) handleSend(w http.ResponseWriter, r *http.Request) {
	c, ok := s.writable(w, r)
	if !ok {
		return
	}
	var req struct {
		Content       string `json:"content"`
		WhenBusy      string `json:"when_busy"`
		RequestID     string `json:"request_id"`
		BrowserLocale string `json:"browser_locale"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !auth.ValidLocale(req.BrowserLocale) {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, `browser_locale must be a BCP 47 tag such as "pt-BR", or ""`)
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "content must not be empty")
		return
	}
	busy, known := whenBusy[req.WhenBusy]
	if !known {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, `when_busy must be "follow_up" or "steer"`)
		return
	}
	if len(req.RequestID) > maxRequestIDLen || !printableASCII(req.RequestID) {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "request_id must be up to 128 printable ASCII characters")
		return
	}
	var switchTo *agentrt.ModelRef
	var reply SendReply
	if mention, ok := agentMention(req.Content); ok {
		model, ok := usable(w, r, mention)
		if !ok {
			return
		}
		switchTo = &model
		reply.Model = &mention
	}
	rt, ok := s.runtime(w, r)
	if !ok {
		return
	}
	if !s.stillWritable(w, r) {
		return
	}
	user, _ := caller(r)
	var before agentrt.ModelRef
	ctx := r.Context()
	if switchTo != nil {
		done, err := s.holdModel(ctx, c.ID)
		if err != nil {
			return
		}
		defer done()
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.holdLimit)
		defer cancel()
		was, err := rt.Model(ctx, c.agentID)
		if err != nil {
			conversationError(w, r, c, err)
			return
		}
		before = was
		if err := rt.SetModel(ctx, c.agentID, *switchTo); err != nil {
			conversationError(w, r, c, err)
			return
		}
		slog.InfoContext(r.Context(), "conversation model changed by a mention", "conversation", c.ID, "provider", switchTo.Provider, "by", user)
	}
	data, err := json.Marshal(struct {
		V      int   `json:"v"`
		Author int64 `json:"author"`
	}{V: 1, Author: user})
	if err != nil {
		writeError(w, http.StatusInternalServerError, apierr.CodeInternal, "could not prepare conversation message")
		return
	}
	in := agentrt.Input{
		Content: req.Content, WhenBusy: busy, RequestID: req.RequestID,
		Entry: &agentrt.InputEntry{Kind: "taskto.input", Data: data},
	}
	if s.language != nil {
		in.Language = s.language(r, req.BrowserLocale)
	}
	if s.capability != nil {
		in.Capability = s.capability(c.ID, c.agentID)
	}
	model, _ := rt.Model(ctx, c.agentID)
	if err := s.ensureUsageMark(ctx, rt, c.ID, c.agentID); err != nil {
		conversationError(w, r, c, err)
		return
	}
	sub, err := rt.Submit(ctx, c.agentID, in)
	if err != nil {
		if switchTo != nil {
			backCtx, stop := context.WithTimeout(context.WithoutCancel(r.Context()), putBackLimit)
			defer stop()
			if back := rt.SetModel(backCtx, c.agentID, before); back != nil {
				slog.WarnContext(r.Context(), "conversations: putting back the model after a mention's send failed", "conversation", c.ID, "err", back)
			}
		}
		conversationError(w, r, c, err)
		return
	}
	if err := s.touch(context.WithoutCancel(r.Context()), c.ID, c.agentID); err != nil {
		slog.WarnContext(r.Context(), "conversations: marking a conversation updated", "conversation", c.ID, "err", err)
	}
	s.markWhenSettled(rt, Principal{Conversation: c.ID, AgentConversation: c.agentID, Workspace: c.WorkspaceID, User: user}, model.Provider, sub)
	reply.SubmissionID = sub
	writeJSON(w, http.StatusAccepted, reply)
}

func (s *Service) handleMessages(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conversationInPath(w, r)
	if !ok {
		return
	}
	rt, ok := s.runtime(w, r)
	if !ok {
		return
	}
	msgs, err := rt.Messages(r.Context(), c.agentID)
	if err != nil {
		conversationError(w, r, c, err)
		return
	}
	if msgs == nil {
		msgs = []agentrt.Message{}
	}
	writeJSON(w, http.StatusOK, map[string][]agentrt.Message{"messages": msgs})
}

func (s *Service) handleStop(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conversationInPath(w, r)
	if !ok {
		return
	}
	rt, ok := s.runtime(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), abortTimeout)
	defer cancel()
	if err := rt.Abort(ctx, c.agentID); err != nil {
		conversationError(w, r, c, err)
		return
	}
	user, _ := caller(r)
	slog.InfoContext(r.Context(), "conversation stopped", "conversation", c.ID, "by", user)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleEntries(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conversationInPath(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	var before int64
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "before must be a positive entry ID")
			return
		}
		before = n
	}
	limit := pageEntries
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxPageEntries {
			writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, fmt.Sprintf("limit must be 1 to %d", maxPageEntries))
			return
		}
		limit = n
	}
	rt, ok := s.runtime(w, r)
	if !ok {
		return
	}
	page, err := rt.Entries(r.Context(), c.agentID, before, limit)
	if err != nil {
		conversationError(w, r, c, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Service) handleModel(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conversationInPath(w, r)
	if !ok {
		return
	}
	rt, ok := s.runtime(w, r)
	if !ok {
		return
	}
	m, err := rt.Model(r.Context(), c.agentID)
	if err != nil {
		conversationError(w, r, c, err)
		return
	}
	writeJSON(w, http.StatusOK, modelRef{Provider: m.Provider, ModelID: m.ModelID})
}

func (s *Service) handleSetModel(w http.ResponseWriter, r *http.Request) {
	c, ok := s.writable(w, r)
	if !ok {
		return
	}
	var req modelRef
	if !readJSON(w, r, &req) {
		return
	}
	model, ok := usable(w, r, req)
	if !ok {
		return
	}
	rt, ok := s.runtime(w, r)
	if !ok {
		return
	}
	waitCtx, stopWaiting := context.WithTimeout(r.Context(), s.modelWait)
	done, err := s.holdModel(waitCtx, c.ID)
	stopWaiting()
	if err != nil {
		if r.Context().Err() == nil {
			writeError(w, http.StatusConflict, apierr.CodeConversationBusy, "the conversation's model is being changed by a message: try again in a moment")
		}
		return
	}
	defer done()
	if !s.stillWritable(w, r) {
		return
	}
	if err := rt.SetModel(r.Context(), c.agentID, model); err != nil {
		conversationError(w, r, c, err)
		return
	}
	user, _ := caller(r)
	slog.InfoContext(r.Context(), "conversation model changed", "conversation", c.ID, "provider", model.Provider, "by", user)
	writeJSON(w, http.StatusOK, req)
}

func validTitle(w http.ResponseWriter, title string) (string, bool) {
	title = strings.TrimSpace(title)
	if utf8.RuneCountInString(title) > maxTitleLen || strings.IndexFunc(title, func(r rune) bool { return !unicode.IsPrint(r) && r != ' ' }) >= 0 {
		writeError(w, http.StatusBadRequest, apierr.CodeConversationTitleInvalid,
			fmt.Sprintf("title must be up to %d printable characters", maxTitleLen), apierr.Params{"max": maxTitleLen})
		return "", false
	}
	return title, true
}

func printableASCII(s string) bool {
	for _, b := range []byte(s) {
		if b < 0x20 || b > 0x7e {
			return false
		}
	}
	return true
}

func serverError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	slog.ErrorContext(r.Context(), "conversations: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	apierr.Internal(w)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, apierr.CodeInvalidJSON, "invalid JSON body")
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, apierr.CodeInvalidJSON, "invalid JSON body: one object expected")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.Error("conversations: encode response", "err", err)
		status, body = http.StatusInternalServerError, []byte(`{"error":"could not encode the response","code":"internal"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func writeError(w http.ResponseWriter, status int, code apierr.Code, msg string, params ...apierr.Params) {
	apierr.Write(w, status, code, msg, params...)
}
