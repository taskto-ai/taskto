package cliaccounts

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"taskto.ai/taskto/internal/agenttools"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/conversations"
)

const ToolsPath = "/cli-mcp"

type Tools struct {
	Handler  http.Handler
	ReadOnly []string
}

func (s *Service) SetTools(t Tools) { s.tools.Store(&t) }

func (s *Service) ToolsReachable() bool { return s.shared != nil && s.shared.ProxyURL != "" }

func (s *Service) toolsFor(id string, p conversations.Principal) (*cliworker.Tools, error) {
	t := s.tools.Load()
	if t == nil || !s.ToolsReachable() {
		return nil, nil
	}
	token, err := s.mintToolsToken(toolsClaims{User: p.User, Conversation: p.Conversation, Turn: id})
	if err != nil {
		return nil, err
	}
	return &cliworker.Tools{URL: strings.TrimRight(s.shared.ProxyURL, "/") + ToolsPath, Token: token, ReadOnly: t.ReadOnly}, nil
}

const toolsTokenLife = proxyTokenLife

type toolsClaims struct {
	Nonce        string `json:"n"`
	User         int64  `json:"u"`
	Conversation int64  `json:"c"`
	Turn         string `json:"t"`
	Expires      int64  `json:"e"`
}

const toolsTokenPurpose = "cli-tools-token"

const toolsTokenPrefix = "ttm1."

func (s *Service) mintToolsToken(c toolsClaims) (string, error) {
	c.Expires = time.Now().Add(toolsTokenLife).Unix()
	var nonce [12]byte
	_, _ = rand.Read(nonce[:])
	c.Nonce = b64.EncodeToString(nonce[:])
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	s.track(toolsTokenPurpose, c.Turn, c.Nonce)
	return toolsTokenPrefix + b64.EncodeToString(payload) + "." + b64.EncodeToString(s.shared.Key.MAC(toolsTokenPurpose, payload)), nil
}

func (s *Service) readToolsToken(token string) (toolsClaims, bool) {
	var c toolsClaims
	rest, ok := strings.CutPrefix(token, toolsTokenPrefix)
	if !ok || s.shared == nil {
		return c, false
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 2 {
		return c, false
	}
	payload, err1 := b64.DecodeString(parts[0])
	mac, err2 := b64.DecodeString(parts[1])
	if err1 != nil || err2 != nil || !hmac.Equal(mac, s.shared.Key.MAC(toolsTokenPurpose, payload)) {
		return c, false
	}
	if err := json.Unmarshal(payload, &c); err != nil || time.Now().Unix() > c.Expires || c.User <= 0 || c.Conversation <= 0 {
		return c, false
	}
	return c, !s.retired(c.Nonce)
}

var (
	errToolsToken = &agenttools.Refusal{Status: http.StatusUnauthorized,
		Message: "this token isn't valid: taskto's tools work only during the turn they were given to"}
	errToolsConversation = &agenttools.Refusal{Status: http.StatusForbidden,
		Message: "this conversation can't use taskto's tools: it is archived, or its owner left the workspace or was disabled"}
)

func (s *Service) ToolsCaller(r *http.Request) (agenttools.Caller, error) {
	scheme, raw, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return agenttools.Caller{}, errToolsToken
	}
	c, ok := s.readToolsToken(strings.TrimSpace(raw))
	if !ok || !s.turnRunning(c) {
		return agenttools.Caller{}, errToolsToken
	}
	p, err := conversations.PrincipalOf(r.Context(), s.db, c.Conversation)
	if errors.Is(err, conversations.ErrInactive) || (err == nil && p.User != c.User) {
		return agenttools.Caller{}, errToolsConversation
	}
	if err != nil {
		return agenttools.Caller{}, fmt.Errorf("checking a tools token: %w", err)
	}
	return agenttools.Caller{Workspace: p.Workspace, User: p.User, Conversation: p.Conversation}, nil
}

func (s *Service) turnRunning(c toolsClaims) bool {
	if s.worker == nil {
		return false
	}
	t, ok := s.worker.FindTurn(c.Turn)
	if !ok || t.Over() {
		return false
	}
	spec := t.Spec()
	return spec.User == c.User && spec.Conversation == c.Conversation
}
