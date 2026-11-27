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

// taskto's own tools in CLI turns (docs/cli-tools.md): every turn the server starts gets the
// server's MCP endpoint for turns, served on the proxy's listener at ToolsPath, with a token for
// that turn alone. The token says which turn of which conversation it is for; whom a call acts
// for, and with what access, is looked up again on every request (ToolsCaller).

// ToolsPath is where the proxy's listener serves the MCP endpoint for turns.
const ToolsPath = "/cli-mcp"

// Tools turns taskto's tools on for turns (SetTools).
type Tools struct {
	// Handler serves the endpoint: MCP, with each request's caller from ToolsCaller.
	Handler http.Handler
	// ReadOnly names the tools that change nothing: the permission hook lets them through
	// without asking (docs/cli-tools.md#permissions).
	ReadOnly []string
}

// SetTools turns taskto's tools on for the turns started from now, at the proxy's address. It
// may be called after the proxy's listener started: until then its endpoint answers that the
// tools are off.
func (s *Service) SetTools(t Tools) { s.tools.Store(&t) }

// ToolsReachable reports whether the CLIs can reach taskto's tools: there is a proxy address.
func (s *Service) ToolsReachable() bool { return s.shared != nil && s.shared.ProxyURL != "" }

// toolsFor returns the tools of a turn the server starts: nil while they are off or there is no
// proxy address for the CLIs to reach them at.
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

// toolsTokenLife bounds a tools token, as the proxy's (proxyTokenLife).
const toolsTokenLife = proxyTokenLife

// toolsClaims is what a tools token says: for which turn, of which conversation and user,
// until when.
type toolsClaims struct {
	// Nonce tells the tokens of one turn apart: a replay of a turn gets a new token, and the
	// one before stops working.
	Nonce string `json:"n"`
	User  int64  `json:"u"`
	// Identity is the user's account's, as the proxy's (proxyClaims.Identity).
	Identity string `json:"i"`
	// Workspace is the conversation's, as the proxy's (proxyClaims.Workspace).
	Workspace    int64  `json:"w"`
	Conversation int64  `json:"c"`
	Turn         string `json:"t"`
	Expires      int64  `json:"e"`
}

// toolsTokenPurpose separates the tools tokens' MACs from the proxy's and any other use of the
// server key: a proxy token is never a tools token.
const toolsTokenPurpose = "cli-tools-token" //nolint:gosec // G101: a MAC's purpose, not a credential

const toolsTokenPrefix = "ttm1."

// mintToolsToken returns the tools token of a turn.
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

// readToolsToken checks a tools token's signature, expiry and nonce.
func (s *Service) readToolsToken(token string) (toolsClaims, bool) {
	var c toolsClaims
	if s.shared == nil || !openToken(s.shared.Key, toolsTokenPrefix, toolsTokenPurpose, token, &c) ||
		time.Now().Unix() > c.Expires || c.User <= 0 || c.Conversation <= 0 {
		return c, false
	}
	return c, !s.retired(c.Nonce)
}

// The refusals of ToolsCaller, for the CLI's model to read.
var (
	errToolsToken = &agenttools.Refusal{Status: http.StatusUnauthorized,
		Message: "this token isn't valid: taskto's tools work only during the turn they were given to"}
	errToolsConversation = &agenttools.Refusal{Status: http.StatusForbidden,
		Message: "this conversation can't use taskto's tools: it is archived, or its owner left the workspace or was disabled"}
)

// ToolsCaller returns whom a request to the endpoint for turns acts for: the owner of the
// conversation its token names, in that conversation's workspace, as the database says now.
// The token must be signed and current, its turn must not have ended or been replaced, the
// worker must still be running that turn, and the conversation must be active and still the
// token's user's. A refusal is an *agenttools.Refusal; any other error is the database's.
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
		// The worker can't be asked: not a bad token, and no tool runs while it is unknown.
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

// turnRunning reports whether the worker is running c's turn, for c's user and conversation.
// The worker outlives a restart of the server, and a restart of the worker stops every CLI, so
// this holds where the server's own list of ended turns, in memory, doesn't.
// An error is the worker's: it couldn't be asked.
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

// sameAccount reports whether user's account is the one with identity: a token minted before a
// restore of the server's data names a number the restore may have given to another account.
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
