package cliaccounts

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/conversations"
	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/serverkey"
)

const proxyTokenLife = 2 * time.Hour

type proxyClaims struct {
	Nonce        string `json:"n"`
	Account      int64  `json:"a"`
	User         int64  `json:"u"`
	Identity     string `json:"i"`
	Workspace    int64  `json:"w"`
	Conversation int64  `json:"c"`
	Turn         string `json:"t"`
	Expires      int64  `json:"e"`
}

const proxyTokenPurpose = "cli-proxy-token"

var b64 = base64.RawURLEncoding

func (s *Service) mintToken(c proxyClaims) (string, error) {
	c.Expires = time.Now().Add(proxyTokenLife).Unix()
	var nonce [12]byte
	_, _ = rand.Read(nonce[:])
	c.Nonce = b64.EncodeToString(nonce[:])
	token, err := sealToken(s.shared.Key, proxyTokenPrefix, proxyTokenPurpose, c)
	if err != nil {
		return "", err
	}
	s.track(proxyTokenPurpose, c.Turn, c.Nonce)
	return token, nil
}

const proxyTokenPrefix = "tt1."

func sealToken(key *serverkey.Key, prefix, purpose string, v any) (string, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sealed, err := key.Seal(payload, []byte(purpose))
	if err != nil {
		return "", err
	}
	return prefix + b64.EncodeToString(sealed) + "." + b64.EncodeToString(key.MAC(purpose, sealed)), nil
}

func openToken(key *serverkey.Key, prefix, purpose, token string, v any) bool {
	rest, ok := strings.CutPrefix(token, prefix)
	if !ok || key == nil {
		return false
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 2 {
		return false
	}
	sealed, err1 := b64.DecodeString(parts[0])
	mac, err2 := b64.DecodeString(parts[1])
	if err1 != nil || err2 != nil || !hmac.Equal(mac, key.MAC(purpose, sealed)) {
		return false
	}
	payload, err := key.Open(sealed, []byte(purpose))
	return err == nil && json.Unmarshal(payload, v) == nil
}

func (s *Service) track(kind, turn, nonce string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := kind + " " + turn
	if old, ok := s.tokens[key]; ok {
		s.ended[old] = time.Now()
	}
	s.tokens[key] = nonce
}

func (s *Service) retired(nonce string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ended := s.ended[nonce]
	return ended
}

var errBadToken = errors.New("this token isn't valid: it belongs to a turn that ended, or to none")

func (s *Service) readToken(token string) (proxyClaims, error) {
	var c proxyClaims
	if !openToken(s.shared.Key, proxyTokenPrefix, proxyTokenPurpose, token, &c) || time.Now().Unix() > c.Expires {
		return c, errBadToken
	}
	if s.retired(c.Nonce) {
		return c, errBadToken
	}
	return c, nil
}

func (s *Service) endTurn(turn string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for n, at := range s.ended {
		if now.Sub(at) > proxyTokenLife {
			delete(s.ended, n)
		}
	}
	for _, kind := range []string{proxyTokenPurpose, toolsTokenPurpose} {
		if nonce, ok := s.tokens[kind+" "+turn]; ok {
			s.ended[nonce] = now
			delete(s.tokens, kind+" "+turn)
		}
	}
}

const maxProxyBody = 32 << 20

func (s *Service) ProxyHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/cli-proxy/v1/", s.handleProxy)
	mux.HandleFunc(ToolsPath, func(w http.ResponseWriter, r *http.Request) {
		t := s.tools.Load()
		if t == nil {
			proxyError(w, http.StatusNotFound, "taskto's tools are off on this server")
			return
		}
		t.Handler.ServeHTTP(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		proxyError(w, http.StatusNotFound, "not found")
	})
	return mux
}

func proxyError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": "taskto_proxy_error", "message": msg},
	})
}

func (s *Service) handleProxy(w http.ResponseWriter, r *http.Request) {
	if s.shared == nil {
		proxyError(w, http.StatusServiceUnavailable, "shared accounts are off on this server")
		return
	}
	token := r.Header.Get("X-Api-Key")
	if token == "" {
		token, _ = strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	c, err := s.readToken(strings.TrimSpace(token))
	if err != nil {
		proxyError(w, http.StatusUnauthorized, err.Error())
		return
	}
	a, refusal, err := s.proxyAccount(r.Context(), c)
	if err != nil {
		slog.ErrorContext(r.Context(), "cli proxy: checking a token", "err", err)
		proxyError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if refusal != "" {
		proxyError(w, http.StatusForbidden, refusal)
		return
	}
	key, err := s.apiKey(a)
	if err != nil {
		slog.ErrorContext(r.Context(), "cli proxy: opening a shared account's key", "account", a.ID, "err", err)
		proxyError(w, http.StatusBadGateway, "the workspace's shared account can't be used: "+err.Error())
		return
	}
	target, err := url.Parse(a.BaseURL)
	if err != nil {
		proxyError(w, http.StatusBadGateway, "the workspace's shared account has an invalid address")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxProxyBody)
	rest := strings.TrimPrefix(r.URL.Path, "/cli-proxy")
	started := time.Now()
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = target.Scheme, target.Host
			pr.Out.URL.Path, pr.Out.URL.RawPath = target.Path+rest, ""
			pr.Out.URL.RawQuery = r.URL.RawQuery
			pr.Out.Host = target.Host
			for _, h := range []string{"Cookie", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
				pr.Out.Header.Del(h)
			}
			authorize(pr.Out.Header, a.CLI, key)
		},
		Transport:     s.upstream,
		FlushInterval: -1,
		ModifyResponse: func(res *http.Response) error {
			res.Header.Del("Set-Cookie")
			if res.StatusCode == http.StatusTooManyRequests {
				s.recordProxyLimit(a, res)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			msg := "the provider couldn't be reached"
			if refused := egress.Refusal(err); refused != nil {
				msg = "the server's egress policy doesn't allow this address: " + refused.Error()
			}
			slog.WarnContext(req.Context(), "cli proxy: forwarding failed", "account", a.ID, "user", c.User, "err", err)
			proxyError(w, http.StatusBadGateway, msg)
		},
	}
	rec := &statusRecorder{ResponseWriter: w}
	rp.ServeHTTP(rec, r)
	slog.InfoContext(r.Context(), "cli proxy", "account", a.ID, "user", c.User, "conversation", c.Conversation,
		"method", r.Method, "path", rest, "status", rec.status, "took", time.Since(started).Round(time.Millisecond))
}

func (s *Service) proxyAccount(ctx context.Context, c proxyClaims) (sharedRow, string, error) {
	if ok, err := s.sameAccount(ctx, c.User, c.Identity); err != nil || !ok {
		return sharedRow{}, "this token's account is gone", err
	}
	p, err := conversations.PrincipalOf(ctx, s.db, c.Conversation)
	if errors.Is(err, conversations.ErrInactive) || (err == nil && (p.User != c.User || p.Workspace != c.Workspace)) {
		return sharedRow{}, "this conversation can't use the workspace's accounts: it is archived, or its owner left the workspace or was disabled", nil
	}
	if err != nil {
		return sharedRow{}, "", err
	}
	a, ok, err := s.sharedAccount(ctx, p.Workspace, c.Account)
	if err != nil || !ok {
		return sharedRow{}, "the workspace's shared account was removed", err
	}
	mode, err := s.modeOf(ctx, p.Workspace, p.User)
	if err != nil {
		return sharedRow{}, "", err
	}
	if !mode.usesShared() {
		return sharedRow{}, "an admin set your turns in this workspace to your own accounts only", nil
	}
	return a, "", nil
}

func (s *Service) recordProxyLimit(a sharedRow, res *http.Response) {
	l := cliworker.Limit{Window: "api", Status: "rejected", Detail: "the provider answered 429 (rate limited)"}
	if at, ok := resetOf(res.Header, time.Now()); ok {
		l.ResetsAt = &at
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.recordSharedLimits(ctx, a.ID, []cliworker.Limit{l}); err != nil {
		slog.Error("cli proxy: recording a limit", "account", a.ID, "err", err)
	}
}

func resetOf(h http.Header, now time.Time) (time.Time, bool) {
	if v := h.Get("Retry-After"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return now.Add(time.Duration(n) * time.Second).UTC(), true
		}
		if t, err := http.ParseTime(v); err == nil {
			return t.UTC(), true
		}
	}
	var latest time.Time
	for name, values := range h {
		lower := strings.ToLower(name)
		if len(values) == 0 {
			continue
		}
		switch {
		case strings.HasPrefix(lower, "anthropic-ratelimit-") && strings.HasSuffix(lower, "-reset"):
			if t, err := time.Parse(time.RFC3339, values[0]); err == nil && t.After(latest) {
				latest = t
			}
		case strings.HasPrefix(lower, "x-ratelimit-reset-"):
			if d, err := time.ParseDuration(values[0]); err == nil && now.Add(d).After(latest) {
				latest = now.Add(d)
			}
		}
	}
	return latest.UTC(), !latest.IsZero()
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

var _ io.Writer = (*statusRecorder)(nil)
