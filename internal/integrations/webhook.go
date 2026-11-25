package integrations

import (
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

// The webhook route's bounds.
const (
	// maxHookBody is the most of a delivery's body the server reads. Stripe's events are a few
	// KiB; a body over this is refused before anything is read of it or looked up.
	maxHookBody = 256 << 10
	// The limits, per window: a client address, and a public id. Both are counted before the id
	// is looked up, so a refusal says nothing of whether an integration exists. A provider
	// sends from a few addresses and a busy account sends a few events a second at most; an
	// answer 429 makes it retry later (Stripe retries for days).
	hookIPAttempts = 600
	hookIDAttempts = 600
	hookWindow     = time.Minute
)

// publicIDRe is the shape of an integration's public id: 128 random bits in hex. An id of any
// other shape can't exist and is answered like one that doesn't, without a lookup.
var publicIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// HookRoutes mounts the webhook route on mux: POST /hooks/{public_id}, outside /api. It has no
// session, no cookie and no CSRF check (the guard wraps only /api): the provider's signature over
// the exact body is the authentication, and without a valid one the request changes nothing and
// learns nothing. Every way of failing it, an id that doesn't exist, one that is disabled or has
// no webhook secret, a signature that doesn't verify, a timestamp outside the tolerance, a
// provider that has no webhooks, is the same 404.
func (s *Service) HookRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST "+HookPrefix+"{public_id}", s.handleHook)
}

// notFound is the one answer to a request that isn't an authentic delivery for an integration:
// byte for byte the same for each reason.
func notFound(w http.ResponseWriter) {
	apierr.Write(w, http.StatusNotFound, apierr.CodeNotFound, "not found")
}

func tooMany(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(max(1, int((wait+time.Second-1)/time.Second))))
	apierr.Write(w, http.StatusTooManyRequests, apierr.CodeWebhookRateLimited, "too many requests")
}

// hookClient is the key of a request's client in the limiter: the address the server found for it
// (audit.ClientFrom: behind a trusted proxy, the one it forwarded), a whole /64 for IPv6, which
// one client usually controls.
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
	// Nothing below reads the request's cookies, a session or a CSRF header, and nothing sets one.
	w.Header().Set("Cache-Control", "no-store")
	if ok, wait := s.hookIP.Take(hookClient(r)); !ok {
		tooMany(w, wait)
		return
	}
	publicID := r.PathValue("public_id")
	if !publicIDRe.MatchString(publicID) {
		notFound(w)
		return
	}
	if ok, wait := s.hookID.Take(publicID); !ok {
		tooMany(w, wait)
		return
	}
	// The body is bounded and read whole, and only then is anything looked up: an oversized body
	// is refused alike for every id.
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
	target, err := s.store.byPublicID(ctx, publicID)
	if err != nil {
		if !errors.Is(err, errNotFound) {
			s.hookFailed(w, r, err)
			return
		}
		notFound(w)
		return
	}
	provider, ok := Lookup(target.provider).(Receiver)
	if !ok {
		notFound(w)
		return
	}
	creds, err := s.store.credentials(ctx, target.workspace, target.id)
	if err != nil {
		var unreadable unreadableError
		if errors.As(err, &unreadable) {
			// The server key isn't the one that sealed the secrets. The admin is told by the
			// integration's own check; here it is one more answer that verifies nothing.
			slog.Warn("integrations: a webhook secret can't be opened", "integration", target.id)
			notFound(w)
			return
		}
		s.hookFailed(w, r, err)
		return
	}
	secrets := WebhookSecrets{Current: creds.WebhookSecret, Previous: creds.WebhookSecretPrevious}
	if err := provider.VerifyWebhook(r.Header, body, secrets, s.now()); err != nil {
		// Why is for the log at debug, with the integration and never the header, the body or a
		// secret: the caller gets the one answer.
		slog.Debug("integrations: a webhook was refused", "integration", target.id, "reason", err.Error())
		notFound(w)
		return
	}

	// From here the delivery is authentic: what is stored or ignored is about the event, and the
	// answer is 200 either way, since a provider disables an endpoint that answers errors.
	ev, ok := provider.Normalize(body)
	if !ok || ev.Livemode != (target.mode == ModeLive) {
		// A type the server doesn't act on, or an event of the other mode than the integration's
		// (which a secret of one mode can't sign: a refusal worth nothing more than ignoring).
		if err := s.store.noteEvent(ctx, target.id); err != nil {
			s.hookFailed(w, r, err)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	if _, err := s.store.record(ctx, target.id, ev, SourceWebhook, body); err != nil {
		s.hookFailed(w, r, err)
		return
	}
	s.poke()
	w.WriteHeader(http.StatusOK)
}

// hookFailed answers a failure of the server's own (the database) so that the provider retries:
// a 500, whose cause goes to the log by its error, which holds no body or secret.
func (s *Service) hookFailed(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return // the provider went away
	}
	slog.ErrorContext(r.Context(), "integrations: a webhook couldn't be stored", "err", err)
	apierr.Internal(w)
}

// newHookLimits makes the route's two limiters.
func newHookLimits(now func() time.Time) (ip, id *ratelimit.Limiter) {
	return ratelimit.New(hookIPAttempts, hookWindow, now), ratelimit.New(hookIDAttempts, hookWindow, now)
}
