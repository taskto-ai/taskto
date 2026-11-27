package integrations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/ratelimit"
)

const (
	maxHookBody    = 256 << 10
	hookIPAttempts = 600
	hookIDAttempts = 600
	hookWindow     = time.Minute
)

var hookReadTimeout = 30 * time.Second

var publicIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (s *Service) HookRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST "+HookPrefix+"{public_id}", s.handleHook)
}

func notFound(w http.ResponseWriter) {
	apierr.Write(w, http.StatusNotFound, apierr.CodeNotFound, "not found")
}

func tooMany(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(max(1, int((wait+time.Second-1)/time.Second))))
	apierr.Write(w, http.StatusTooManyRequests, apierr.CodeWebhookRateLimited, "too many requests")
}

func hookClient(r *http.Request) string {
	host := audit.ClientFrom(r.Context())
	if host == "" {
		var err error
		if host, _, err = net.SplitHostPort(r.RemoteAddr); err != nil {
			host = r.RemoteAddr
		}
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.WithZone("").Unmap()
		if addr.Is6() {
			prefix, _ := addr.Prefix(64)
			return prefix.String()
		}
		return addr.String()
	}
	return host
}

func (s *Service) handleHook(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(hookReadTimeout))
	if ok, wait := s.hookIP.Take(hookClient(r)); !ok {
		tooMany(w, wait)
		return
	}
	if r.ContentLength > maxHookBody {
		apierr.Write(w, http.StatusRequestEntityTooLarge, apierr.CodeWebhookTooLarge, "the body is too large")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHookBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			apierr.Write(w, http.StatusRequestEntityTooLarge, apierr.CodeWebhookTooLarge, "the body is too large")
		} else {
			apierr.Write(w, http.StatusBadRequest, apierr.CodeBadRequest, "the request couldn't be read")
		}
		return
	}

	ctx := r.Context()
	sub, err := s.hookSubject(ctx, r.PathValue("public_id"))
	if err != nil {
		s.hookFailed(w, r, err)
		return
	}
	verr := sub.provider.VerifyWebhook(r.Header, body, sub.secrets, s.now())
	if verr != nil || !sub.live {
		notFound(w)
		return
	}
	if ok, wait := s.hookID.Take(r.PathValue("public_id")); !ok {
		tooMany(w, wait)
		return
	}

	guard := &hookGuard{mode: sub.target.mode, revision: sub.revision}
	ev, ok := sub.provider.Normalize(body)
	if m, modeless := sub.provider.(modeOfAccount); ok && modeless && m.EventsCarryNoMode() {
		ev.Livemode = sub.target.mode == ModeLive
	}
	if !ok || ev.Livemode != (sub.target.mode == ModeLive) {
		if err := s.store.noteEvent(ctx, sub.target.id, guard); err != nil {
			s.hookWriteFailed(w, r, err)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	if _, err := s.store.recordGuarded(ctx, sub.target.id, guard, ev, SourceWebhook, body); err != nil {
		s.hookWriteFailed(w, r, err)
		return
	}
	s.poke()
	w.WriteHeader(http.StatusOK)
}

type hookSubject struct {
	target   hookTarget
	provider Receiver
	secrets  WebhookSecrets
	revision string
	live     bool
}

func (s *Service) hookSubject(ctx context.Context, publicID string) (hookSubject, error) {
	sub := hookSubject{provider: decoyReceiver, secrets: s.decoy}
	if !publicIDRe.MatchString(publicID) {
		return sub, nil
	}
	target, err := s.store.byPublicID(ctx, publicID)
	if errors.Is(err, errNotFound) {
		return sub, nil
	}
	if err != nil {
		return sub, err
	}
	if s.afterLookup != nil {
		s.afterLookup()
	}
	provider, ok := Lookup(target.provider).(Receiver)
	if !ok {
		return sub, nil
	}
	creds, revision, err := s.store.hookCredentials(ctx, target.workspace, target.id)
	if errors.Is(err, errNotFound) {
		return sub, nil
	}
	if err != nil {
		var unreadable unreadableError
		if errors.As(err, &unreadable) {
			return sub, nil
		}
		return sub, err
	}
	if creds.WebhookSecret == "" && creds.WebhookSecretPrevious == "" {
		return sub, nil
	}
	return hookSubject{
		target: target, provider: provider, revision: revision,
		secrets: WebhookSecrets{
			Current: creds.WebhookSecret, Previous: creds.WebhookSecretPrevious,
			URL:         signedURL(hookURL(s.publicURL, publicID), target.registeredURL),
			PreviousURL: signedURL(hookURL(s.publicURL, publicID), target.previousRegisteredURL),
		},
		live: !target.disabled,
	}, nil
}

func signedURL(current, registered string) string {
	if registered != "" {
		return registered
	}
	return current
}

var decoyReceiver Receiver = stripe{}

func newDecoy() WebhookSecrets {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		panic("integrations: no randomness for the webhook route: " + err.Error())
	}
	return WebhookSecrets{Current: Secret("whsec_" + hex.EncodeToString(raw))}
}

func (s *Service) hookWriteFailed(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errHookRefused) {
		notFound(w)
		return
	}
	s.hookFailed(w, r, err)
}

func (s *Service) hookFailed(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	slog.ErrorContext(r.Context(), "integrations: a webhook couldn't be stored", "err", err)
	apierr.Internal(w)
}

func newHookLimits(now func() time.Time) (ip, id *ratelimit.Limiter) {
	return ratelimit.New(hookIPAttempts, hookWindow, now), ratelimit.New(hookIDAttempts, hookWindow, now)
}
