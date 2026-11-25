// Package agentbrowser is the server's side of the agent browser (docs/agent-browser.md): the
// API under /api/me/browser, the live view's WebSocket, and the agent's browser tools. The
// browsers themselves run in the CLI worker (internal/browser): this package only ever acts
// on the signed-in user's own, or on the conversation owner's for the agent.
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

// ErrOff is Usable's answer when the browser is off on this server.
var ErrOff = errors.New("the agent browser is off on this server (-browser)")

// Service serves the agent browser to users and to the agent.
type Service struct {
	b      browser.Browsers
	usable func(context.Context) error
}

// New returns the service over b, usable when usable says so (the CLI workers' rule: the
// browser runs in their worker, under the same OS boundary). A nil b means the browser is off.
func New(b browser.Browsers, usable func(context.Context) error) *Service {
	return &Service{b: b, usable: usable}
}

// Usable reports whether the browser can be used now: nil, ErrOff, or the CLI workers' reason.
func (s *Service) Usable(ctx context.Context) error {
	if s == nil || s.b == nil {
		return ErrOff
	}
	if s.usable != nil {
		return s.usable(ctx)
	}
	return nil
}

// Routes mounts /api/me/browser on mux. Every route is the signed-in user's own: the paths
// name no user, so no one can reach another's browser.
//
//	GET  /api/me/browser        → Status
//	POST /api/me/browser/start  {"browser": "personal"} → Status
//	POST /api/me/browser/stop   {"browser": "personal"} → 204
//	GET  /api/me/browser/live   the personal browser's live view: a WebSocket of RFB
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

// Status is GET /api/me/browser.
type Status struct {
	// Enabled says whether the browser can be used now; Reason says why not, in English, and
	// ReasonCode as an error code (internal/apierr), which the web app shows translated.
	Enabled    bool                           `json:"enabled"`
	Reason     string                         `json:"reason,omitempty"`
	ReasonCode apierr.Code                    `json:"reason_code,omitempty"`
	Browsers   map[browser.Kind]browser.State `json:"browsers"`
}

// unusable is the code for why the browser can't be used now (Usable's answer).
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
	// Not usable is an answer, not a failure: the status says why.
	if why := s.Usable(ctx); why != nil {
		out.Reason, out.ReasonCode = why.Error(), unusable(why)
		return out, nil //nolint:nilerr // the reason is in the answer
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

// usableOr409 answers 409 with the reason when the browser can't be used now.
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

// handleLive is the live view: a WebSocket that carries the personal browser's RFB stream
// both ways, for noVNC in the page. The upgrade is a GET, which the CSRF guard leaves alone,
// and a browser sends cookies with a cross-site WebSocket: so the request must come from this
// server's own pages (sameOrigin) before anything is opened.
//
// The session is checked when the view opens, and the view lasts as long as the login does: it
// ends when the login is signed out, revoked or replaced by a password change, at once by the
// auth service's signal and at the latest at its next look every liveRecheck
// (auth.Service.WatchSession), so a display someone can type into is never left open to a
// browser that was signed out.
func (s *Service) handleLive(a *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.live(a, w, r)
	}
}

// liveRecheck is how often an open live view looks its login up again.
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
		// noVNC asks for the "binary" subprotocol, or none.
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
			// Closing both ends makes the bridge's copies return, so the view ends with its login.
			go func() {
				<-ctx.Done()
				_ = live.Close()
				_ = ws.Close()
			}()
			browser.Bridge(live, ws)
			_ = ws.Close()
			slog.InfoContext(r.Context(), "browser live view closed", "user", user, "after", time.Since(start).Round(time.Second))
		},
	}
	srv.ServeHTTP(w, r)
}

// sameOrigin reports whether r comes from this server's own pages. A browser says where a
// request comes from in Sec-Fetch-Site, and older ones in Origin, which must then name this
// host. A request with neither is refused: every browser sends Origin with a WebSocket.
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
	// A default port may be written or not: https://h and Host h:443 are the same origin.
	defaultPort := map[string]string{"https": "443", "http": "80"}[u.Scheme]
	strip := func(host string) string {
		if h, port, err := net.SplitHostPort(host); err == nil && port == defaultPort {
			return h
		}
		return host
	}
	return strings.EqualFold(strip(u.Host), strip(r.Host))
}

// browserError answers for an error of the browsers.
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
