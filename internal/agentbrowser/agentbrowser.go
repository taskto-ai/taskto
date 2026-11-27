package agentbrowser

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/websocket"

	"taskto.ai/taskto/internal/agenttools"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/browser"
	"taskto.ai/taskto/internal/cliaccounts"
)

var ErrOff = errors.New("the agent browser is off on this server (-browser)")

type Service struct {
	b      browser.Browsers
	usable func(context.Context) error
}

func New(b browser.Browsers, usable func(context.Context) error) *Service {
	return &Service{b: b, usable: usable}
}

func (s *Service) Usable(ctx context.Context) error {
	if s == nil || s.b == nil {
		return ErrOff
	}
	if s.usable != nil {
		return s.usable(ctx)
	}
	return nil
}

func (s *Service) Routes(mux *http.ServeMux, a *auth.Service) {
	me := func(h http.HandlerFunc) http.Handler { return a.RequireUser(h) }
	const base = "/api/me/browser"
	mux.Handle("GET "+base, me(s.handleStatus))
	mux.Handle("POST "+base+"/start", me(s.handleStart))
	mux.Handle("POST "+base+"/stop", me(s.handleStop))
	mux.Handle("GET "+base+"/live", me(s.handleLive(a)))
}

func caller(r *http.Request) int64 {
	u, _ := auth.UserFrom(r.Context())
	return u.ID
}

type Status struct {
	Enabled    bool                           `json:"enabled"`
	Reason     string                         `json:"reason,omitempty"`
	ReasonCode apierr.Code                    `json:"reason_code,omitempty"`
	Browsers   map[browser.Kind]browser.State `json:"browsers"`
}

func unusable(err error) apierr.Code {
	var e *apierr.Error
	switch {
	case errors.As(err, &e):
		return e.Code
	case errors.Is(err, ErrOff):
		return apierr.CodeBrowserOff
	case errors.Is(err, cliaccounts.ErrOff):
		return apierr.CodeBrowserCLIWorkersOff
	case errors.Is(err, cliaccounts.ErrNoBoundary):
		return apierr.CodeBrowserSingleUser
	default:
		return apierr.CodeBrowserUnusable
	}
}

func (s *Service) status(ctx context.Context, user int64) (Status, error) {
	out := Status{Browsers: map[browser.Kind]browser.State{}}
	if why := s.Usable(ctx); why != nil {
		out.Reason, out.ReasonCode = why.Error(), unusable(why)
		return out, nil
	}
	st, err := s.b.Status(ctx, user)
	if err != nil {
		return out, err
	}
	out.Enabled, out.Browsers = true, st
	if !st[browser.Personal].Available && !st[browser.Testing].Available {
		out.Enabled, out.Reason, out.ReasonCode = false, "the browser isn't installed on the CLI worker", apierr.CodeBrowserNotInstalled
	}
	return out, nil
}

func (s *Service) handleStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.status(r.Context(), caller(r))
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Service) usableOr409(w http.ResponseWriter, r *http.Request) bool {
	if err := s.Usable(r.Context()); err != nil {
		writeError(w, http.StatusConflict, unusable(err), err.Error())
		return false
	}
	return true
}

func readKind(w http.ResponseWriter, r *http.Request) (browser.Kind, bool) {
	var req struct {
		Browser browser.Kind `json:"browser"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, apierr.CodeInvalidJSON, "invalid body")
		return "", false
	}
	if req.Browser == "" {
		req.Browser = browser.Personal
	}
	if !req.Browser.Valid() {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, `browser must be "personal" or "testing"`)
		return "", false
	}
	return req.Browser, true
}

func (s *Service) handleStart(w http.ResponseWriter, r *http.Request) {
	if !s.usableOr409(w, r) {
		return
	}
	kind, ok := readKind(w, r)
	if !ok {
		return
	}
	user := caller(r)
	if err := s.b.Start(r.Context(), user, kind); err != nil {
		browserError(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "browser started", "user", user, "browser", kind)
	s.handleStatus(w, r)
}

func (s *Service) handleStop(w http.ResponseWriter, r *http.Request) {
	if !s.usableOr409(w, r) {
		return
	}
	kind, ok := readKind(w, r)
	if !ok {
		return
	}
	if err := s.b.Stop(r.Context(), caller(r), kind); err != nil {
		browserError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleLive(a *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.live(a, w, r)
	}
}

var liveRecheck = 5 * time.Second

func (s *Service) live(a *auth.Service, w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, apierr.CodeCrossOrigin, "cross-origin request refused")
		return
	}
	if !s.usableOr409(w, r) {
		return
	}
	user := caller(r)
	ctx, stop := a.WatchSession(r, liveRecheck)
	defer stop()
	if ctx.Err() != nil {
		writeError(w, http.StatusUnauthorized, apierr.CodeNotLoggedIn, "log in first")
		return
	}
	live, err := s.b.OpenLive(ctx, user)
	if err != nil {
		browserError(w, r, err)
		return
	}
	defer live.Close()
	srv := websocket.Server{
		Handshake: func(cfg *websocket.Config, _ *http.Request) error {
			for _, p := range cfg.Protocol {
				if p == "binary" {
					cfg.Protocol = []string{"binary"}
					return nil
				}
			}
			cfg.Protocol = nil
			return nil
		},
		Handler: func(ws *websocket.Conn) {
			ws.PayloadType = websocket.BinaryFrame
			ws.MaxPayloadBytes = 4 << 20
			slog.InfoContext(r.Context(), "browser live view opened", "user", user)
			start := time.Now()
			go func() {
				<-ctx.Done()
				_ = live.Close()
				closeWS(ws)
			}()
			browser.Bridge(live, ws)
			closeWS(ws)
			slog.InfoContext(r.Context(), "browser live view closed", "user", user, "after", time.Since(start).Round(time.Second))
		},
	}
	srv.ServeHTTP(w, r)
}

func closeWS(ws *websocket.Conn) {
	_ = ws.SetWriteDeadline(time.Now().Add(time.Second))
	_ = ws.Close()
}

func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin"
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	defaultPort := map[string]string{"https": "443", "http": "80"}[u.Scheme]
	strip := func(host string) string {
		if h, port, err := net.SplitHostPort(host); err == nil && port == defaultPort {
			return h
		}
		return host
	}
	return strings.EqualFold(strip(u.Host), strip(r.Host))
}

func browserError(w http.ResponseWriter, r *http.Request, err error) {
	var bad *browser.BadAction
	switch {
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, err.Error())
	case errors.Is(err, browser.ErrNotRunning):
		writeError(w, http.StatusConflict, apierr.CodeBrowserNotRunning, "the browser isn't running: start it first")
	case errors.Is(err, browser.ErrTooManyLive):
		writeError(w, http.StatusTooManyRequests, apierr.CodeBrowserTooManyLive, err.Error())
	case errors.Is(err, browser.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, apierr.CodeBrowserNotInstalled, err.Error())
	case errors.Is(err, browser.ErrBusy):
		writeError(w, http.StatusServiceUnavailable, apierr.CodeBrowserBusy, err.Error())
	case errors.Is(err, browser.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, apierr.CodeBrowserShuttingDown, err.Error())
	case errors.Is(err, browser.ErrAccountClosing):
		writeError(w, http.StatusConflict, apierr.CodeBrowserAccountClosing, err.Error())
	default:
		serverError(w, r, err)
	}
}

func serverError(w http.ResponseWriter, r *http.Request, err error) {
	slog.ErrorContext(r.Context(), "agent browser: request failed", "path", r.URL.Path, "err", err)
	apierr.Internal(w)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code apierr.Code, msg string) {
	apierr.Write(w, status, code, msg)
}

var _ agenttools.BrowserTools = (*Service)(nil)
