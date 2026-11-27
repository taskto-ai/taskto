package cliaccounts

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"taskto.ai/taskto/internal/agenttools"
	"taskto.ai/taskto/internal/apierr"
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

func (s *Service) toolsFor(ctx context.Context, id string, p conversations.Principal) (*cliworker.Tools, error) {
	t := s.tools.Load()
	if t == nil || !s.ToolsReachable() {
		return nil, nil
	}
	a, err := s.Account(ctx, p.User)
	if err != nil {
		return nil, err
	}
	token, err := s.mintToolsToken(toolsClaims{User: p.User, Identity: a.Identity, Workspace: p.Workspace, Conversation: p.Conversation, Turn: id})
	if err != nil {
		return nil, err
	}
	return &cliworker.Tools{URL: strings.TrimRight(s.shared.ProxyURL, "/") + ToolsPath, Token: token, ReadOnly: t.ReadOnly}, nil
}

const toolsTokenLife = proxyTokenLife

type toolsClaims struct {
	Nonce        string `json:"n"`
	User         int64  `json:"u"`
	Identity     string `json:"i"`
	Workspace    int64  `json:"w"`
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
	token, err := sealToken(s.shared.Key, toolsTokenPrefix, toolsTokenPurpose, c)
	if err != nil {
		return "", err
	}
	s.track(toolsTokenPurpose, c.Turn, c.Nonce)
	return token, nil
}

func (s *Service) readToolsToken(token string) (toolsClaims, bool) {
	var c toolsClaims
	if s.shared == nil || !openToken(s.shared.Key, toolsTokenPrefix, toolsTokenPurpose, token, &c) ||
		time.Now().Unix() > c.Expires || c.User <= 0 || c.Conversation <= 0 {
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
	if !ok {
		return agenttools.Caller{}, errToolsToken
	}
	if same, err := s.sameAccount(r.Context(), c.User, c.Identity); err != nil {
		return agenttools.Caller{}, fmt.Errorf("checking a tools token: %w", err)
	} else if !same {
		return agenttools.Caller{}, errToolsToken
	}
	running, err := s.turnRunning(c)
	if err != nil {
		return agenttools.Caller{}, &agenttools.Refusal{Status: http.StatusServiceUnavailable, Code: apierr.CodeCLIWorkerNotReady,
			Message: "the CLI worker isn't ready yet: try again in a moment"}
	}
	if !running {
		return agenttools.Caller{}, errToolsToken
	}
	p, err := conversations.PrincipalOf(r.Context(), s.db, c.Conversation)
	if errors.Is(err, conversations.ErrInactive) || (err == nil && (p.User != c.User || p.Workspace != c.Workspace)) {
		return agenttools.Caller{}, errToolsConversation
	}
	if err != nil {
		return agenttools.Caller{}, fmt.Errorf("checking a tools token: %w", err)
	}
	return agenttools.Caller{Workspace: p.Workspace, User: p.User, Conversation: p.Conversation}, nil
}

func (s *Service) turnRunning(c toolsClaims) (bool, error) {
	if s.worker == nil {
		return false, nil
	}
	t, ok, err := s.worker.FindTurn(c.User, c.Turn)
	if err != nil {
		if outage(err) {
			return false, err
		}
		return false, nil
	}
	if !ok || t.Over() {
		return false, nil
	}
	spec := t.Spec()
	return spec.User == c.User && spec.Conversation == c.Conversation, nil
}

func (s *Service) sameAccount(ctx context.Context, user int64, identity string) (bool, error) {
	if identity == "" {
		return false, nil
	}
	a, err := s.Account(ctx, user)
	if errors.Is(err, cliworker.ErrForgotten) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return a.Identity == identity, nil
}
