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

// The webhook route's bounds.
const (
	// maxHookBody is the most of a delivery's body the server reads. Stripe's events are a few
	// KiB; a body over this is refused before anything is read of it or looked up.
	maxHookBody = 256 << 10
	// The limits, per window: a client address, counted before anything is looked up or verified,
	// so a refusal says nothing of whether an integration exists; and a public id, counted only
	// for deliveries that verified, so unsigned traffic from anywhere can't use up the quota of
	// the real ones. A provider sends from a few addresses and a busy account sends a few events
	// a second at most; an answer 429 makes it retry later (Stripe retries for days).
	hookIPAttempts = 600
	hookIDAttempts = 600
	hookWindow     = time.Minute
)

// hookReadTimeout is how long a delivery's body may take to arrive; a variable for the tests.
var hookReadTimeout = 30 * time.Second

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
	// A body that trickles in or never comes can't hold the connection: the read has a deadline of
	// its own, set before any early return so a refused request is bounded too. A connection that can't take one (a test's
	// recorder) is read without.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(hookReadTimeout))
	// The address's limit is the only one before the signature is verified: a flood of unsigned
	// requests is held back by where it comes from, and can't use up the quota of an id's real
	// deliveries.
	if ok, wait := s.hookIP.Take(hookClient(r)); !ok {
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
	sub, err := s.hookSubject(ctx, r.PathValue("public_id"))
	if err != nil {
		s.hookFailed(w, r, err)
		return
	}
	// The signature is verified on every path, against a key of the server's own when there is no
	// usable one, so a refusal costs the same whatever the id is. What the check says counts only
	// when there is an integration to deliver to, and is never logged: the caller gets one answer.
	verr := sub.provider.VerifyWebhook(r.Header, body, sub.secrets, s.now())
	if verr != nil || !sub.live {
		notFound(w)
		return
	}
	// The delivery is authentic: now it counts against the id's quota, which signed deliveries only
	// can use up.
	if ok, wait := s.hookID.Take(r.PathValue("public_id")); !ok {
		tooMany(w, wait)
		return
	}

	// From here the delivery is authentic: what is stored or ignored is about the event, and the
	// answer is 200 either way, since a provider disables an endpoint that answers errors. The
	// writes check again that the integration is the one that was verified against.
	guard := &hookGuard{mode: sub.target.mode, revision: sub.revision}
	ev, ok := sub.provider.Normalize(body)
	if !ok {
		// A type the server doesn't act on.
		//
		// An event of the other mode than the integration's is stored all the same: the mode that
		// counts is the ref's (apply.go checks the event against the mode its ref was made in), and a
		// key replaced by one of the other mode after a payer paid, before the event came, leaves a ref
		// of the old mode whose payment must still be recorded, never acknowledged and dropped.
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

// hookSubject is what a delivery to a public id is verified against.
type hookSubject struct {
	target   hookTarget
	provider Receiver
	secrets  WebhookSecrets
	// revision is the webhook secrets' revision the secrets were read at (see hookGuard).
	revision string
	// live is whether a delivery that verifies may be accepted: the integration exists, is
	// enabled, has a webhook secret and a provider that receives webhooks.
	live bool
}

// hookSubject finds what to verify a delivery to publicID against. An id that can't be delivered
// to, for any reason, gets the server's decoy secret and a Receiver to run it with, so the work is
// the same and the answer is decided afterwards. An error is a failure of the server's own.
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
		s.afterLookup() // a test's seam: the moment between the two reads
	}
	provider, ok := Lookup(target.provider).(Receiver)
	if !ok {
		return sub, nil
	}
	creds, revision, err := s.store.hookCredentials(ctx, target.workspace, target.id)
	if errors.Is(err, errNotFound) {
		// Removed between the two reads: it is an id with nothing behind it, like the first lookup's.
		return sub, nil
	}
	if err != nil {
		var unreadable unreadableError
		if errors.As(err, &unreadable) {
			// The server key isn't the one that sealed the secrets. The admin is told by the
			// integration's own check; here it is one more answer that verifies nothing, and a
			// line in the log would name an integration to whoever asked.
			return sub, nil
		}
		return sub, err
	}
	if creds.WebhookSecret == "" && creds.WebhookSecretPrevious == "" {
		return sub, nil
	}
	return hookSubject{
		target: target, provider: provider, revision: revision,
		secrets: WebhookSecrets{Current: creds.WebhookSecret, Previous: creds.WebhookSecretPrevious},
		live:    !target.disabled,
	}, nil
}

// decoyReceiver runs the verification of a delivery that has no integration behind it.
var decoyReceiver Receiver = stripe{}

// newDecoy makes the secret that deliveries with no usable secret of their own are verified
// against: random, kept in memory only and known to nobody, so nothing signs for it.
func newDecoy() WebhookSecrets {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		panic("integrations: no randomness for the webhook route: " + err.Error())
	}
	return WebhookSecrets{Current: Secret("whsec_" + hex.EncodeToString(raw))}
}

// hookWriteFailed answers a failed write of a verified delivery: one the integration changed
// under is a refusal like the others, and any other failure is the server's, retried.
func (s *Service) hookWriteFailed(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errHookRefused) {
		notFound(w)
		return
	}
	s.hookFailed(w, r, err)
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
