// Package testprovider is a fake Stripe and a sender of signed webhook events, for tests. It is
// a normal package that only tests import (like internal/cliworker/clitest): a test in
// cmd/taskto checks that the server's binary doesn't link it. It must not import the
// integrations package, whose own tests import it.
//
// The fake is an httptest server for the few endpoints the integration uses, written against
// Stripe's public API reference at the pinned version (DefaultAPIVersion; a test checks that it
// is the one the adapter sends):
//
//   - POST /v1/checkout/sessions, GET /v1/checkout/sessions/{id}, GET /v1/checkout/sessions and
//     POST /v1/checkout/sessions/{id}/expire: hosted Checkout in payment mode, the way to a link
//     that carries a reference of ours (client_reference_id) and expires by itself.
//   - POST /v1/payment_links, GET and POST /v1/payment_links/{id}: Payment Links. A link takes
//     no client reference and never expires, and may take at most so many payments
//     (restrictions[completed_sessions][limit]: it deactivates when they are made). At Stripe a
//     payment through one is a Checkout Session that carries the link's id; the fake makes one
//     when a test calls PayLink.
//   - GET /v1/events and GET /v1/events/{id}: the events of the last 30 days, newest first.
//   - GET /v1/balance: a harmless authenticated call, for a connection check.
//   - GET /v1/account: the account the key belongs to (its id), which a test moves to another
//     account with SetAccount, the way a key of another account would answer.
//
// It honours idempotency keys as Stripe documents for API v1 (the same key and parameters get
// the saved answer with Idempotent-Replayed: true, the same key with other parameters is an
// idempotency_error, keys are forgotten after 24 hours), refuses unknown parameters, wrong keys,
// other API versions and bodies that aren't form-encoded, and fails on demand (Fail).
//
// Not modelled: prices, products and customers (a line's amount comes inline, in price_data),
// subscriptions and setup mode, expanding objects, Connect, thin events, the delivery state of
// events (a list's delivery_success is refused), the parameters nothing here would act on
// (customer_creation, billing_address_collection, a link's inactive_message and
// payment_intent_data, a session's statement_descriptor and receipt_email: refused, so a test that
// sends one finds out), and concurrent requests
// with one idempotency key (requests are served one at a time, so the second finds the first's
// answer). The money rules are Stripe's common cases: amounts in whole minor units, a maximum of
// 99999999, the documented minimum of each currency for an account that settles in it
// (minimumCharges in params.go), amounts of ISK and UGX in whole units (the last two digits 00)
// and of the three-decimal currencies ending in 0.
//
// What the sender does is in sender.go.
package testprovider

import (
	"crypto/subtle"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// DefaultAPIVersion is the Stripe API version the fake answers to: requests must send it in
// Stripe-Version, as the adapter does.
const DefaultAPIVersion = "2026-11-18.endive"

// The fake's credentials. They are obviously fake and made of parts, so no file holds a whole
// key-shaped string for a secret scanner to find.
var (
	// TestAPIKey is the secret key the fake accepts unless WithAPIKey says another.
	TestAPIKey = "sk_" + "test_" + strings.Repeat("Fake", 6)
	// TestAccountID is the id of the account the fake answers as, unless WithAccount says another.
	TestAccountID = "acct_" + strings.Repeat("Fake", 4)
	// TestWebhookSecret is the signing secret a Sender uses unless told another.
	TestWebhookSecret = "whsec_" + strings.Repeat("Fake", 6)
	// OtherWebhookSecret is a second signing secret, for wrong-secret and rotation tests.
	OtherWebhookSecret = "whsec_" + strings.Repeat("Rota", 6)
)

// Stripe is the fake. Make one with NewStripe.
type Stripe struct {
	srv   *httptest.Server
	done  chan struct{}
	clock Clock

	apiKey     string
	version    string
	anyVersion bool
	live       bool
	payer      CustomerDetails
	account    string

	mu           sync.Mutex
	sessions     map[string]*session
	sessionOrder []string
	links        map[string]*paymentLink
	linkOrder    []string
	events       []*Event
	idem         map[string]*idemEntry
	faults       []*Fault
	log          []Request
}

// Option changes how NewStripe builds the fake.
type Option func(*Stripe)

// WithClock sets the clock the fake reads: object times, expiry, the 24 hours of an idempotency
// key and the 30 days of the events list.
func WithClock(c Clock) Option { return func(s *Stripe) { s.clock = c } }

// WithAPIKey sets the secret key the fake accepts. A key with "_live_" in it makes the fake's
// objects live-mode ones.
func WithAPIKey(key string) Option {
	return func(s *Stripe) { s.apiKey = key; s.live = strings.Contains(key, "_live_") }
}

// WithAnyAPIVersion makes the fake accept any Stripe-Version, or none. The default refuses
// anything but DefaultAPIVersion, so a call that forgets to pin the version fails a test.
func WithAnyAPIVersion() Option { return func(s *Stripe) { s.anyVersion = true } }

// WithAccount sets the id of the account the key belongs to.
func WithAccount(id string) Option { return func(s *Stripe) { s.account = id } }

// SetAccount moves the key to another account from now on, as replacing an integration's key with
// one of another account does: what the fake holds stays, and is what the first account made.
func (s *Stripe) SetAccount(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.account = id
}

// NewStripe starts a fake Stripe, stopped when the test ends.
func NewStripe(t testing.TB, opts ...Option) *Stripe {
	t.Helper()
	s := &Stripe{
		done:     make(chan struct{}),
		clock:    realClock{},
		apiKey:   TestAPIKey,
		version:  DefaultAPIVersion,
		sessions: map[string]*session{},
		links:    map[string]*paymentLink{},
		idem:     map[string]*idemEntry{},
		payer:    DefaultPayer(),
		account:  TestAccountID,
	}
	for _, o := range opts {
		o(s)
	}
	s.srv = httptest.NewServer(s)
	// Cleanups run last in, first out: release the requests that hang, then close the server.
	t.Cleanup(s.srv.Close)
	t.Cleanup(func() { close(s.done) })
	return s
}

// URL is the fake's base URL (http://127.0.0.1:port), for a client's test endpoint.
func (s *Stripe) URL() string { return s.srv.URL }

// Host is the fake's host:port, for integrations.WithTestEndpoint.
func (s *Stripe) Host() string { return strings.TrimPrefix(s.srv.URL, "http://") }

// APIKey is the secret key the fake accepts.
func (s *Stripe) APIKey() string { return s.apiKey }

// APIVersion is the version the fake answers to.
func (s *Stripe) APIVersion() string { return s.version }

// Clock is the clock the fake reads.
func (s *Stripe) Clock() Clock { return s.clock }

// Request is one request the fake received, for tests to check what the client sent. The key
// itself is not kept, only how it was sent.
type Request struct {
	Method, Path   string
	Query          url.Values
	Form           url.Values
	IdempotencyKey string
	APIVersion     string
	// Auth is "bearer" or "basic" when the request carried the right key, "" when it carried
	// none or a wrong one.
	Auth string
	// Status is the status answered ("0" for a connection dropped or never answered).
	Status int
	// Replayed is true when the answer was a saved one (Idempotent-Replayed).
	Replayed bool
	// Faulted is true when an injected fault changed the request's outcome.
	Faulted bool
	At      time.Time
}

// Requests returns the requests received so far, oldest first.
func (s *Stripe) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, len(s.log))
	for i, r := range s.log {
		r.Query, r.Form = cloneValues(r.Query), cloneValues(r.Form)
		out[i] = r
	}
	return out
}

// cloneValues copies the map and the slices of v, so a caller can't change what the fake kept.
func cloneValues(v url.Values) url.Values {
	if v == nil {
		return nil
	}
	out := make(url.Values, len(v))
	for k, vs := range v {
		out[k] = slices.Clone(vs)
	}
	return out
}

// RequestsTo returns the requests to method and path.
func (s *Stripe) RequestsTo(method, path string) []Request {
	var out []Request
	for _, r := range s.Requests() {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

type idemEntry struct {
	fingerprint string
	reply       reply
	at          time.Time
}

// idemTTL is how long the fake remembers a key. Stripe keeps it at least 24 hours and may
// remove it after.
const idemTTL = 24 * time.Hour

// maxBody is the largest request body the fake reads.
const maxBody = 1 << 20

// ServeHTTP is the fake's handler. It is exported so a test may mount the fake in its own
// server; NewStripe has already done it.
func (s *Stripe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, readErr := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	_ = r.Body.Close()

	s.mu.Lock()
	rec := Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), At: s.clock.Now(),
		IdempotencyKey: r.Header.Get("Idempotency-Key"), APIVersion: r.Header.Get("Stripe-Version")}
	var rp reply
	if readErr != nil || len(body) > maxBody {
		// A body cut short or cut off at the limit must not run as if it were whole.
		rp = invalid("", "The request body couldn't be read in full, or is larger than 1 MiB.", "")
	} else {
		rp = s.handle(r, body, &rec)
	}
	rec.Status = rp.status
	if rp.drop || rp.hang {
		rec.Status = 0
	}
	rec.Replayed = rp.replayed
	s.log = append(s.log, rec)
	s.mu.Unlock()

	switch {
	case rp.hang:
		select {
		case <-r.Context().Done():
		case <-s.done:
		}
		return
	case rp.drop:
		panic(http.ErrAbortHandler)
	}
	h := w.Header()
	for k, v := range rp.header {
		h[k] = v
	}
	h.Set("Request-Id", "req_"+randomID(14))
	h.Set("Stripe-Version", s.version)
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		h.Set("Idempotency-Key", key)
	}
	w.WriteHeader(rp.status)
	_, _ = w.Write(rp.body)
}

// handle answers one request. The caller holds s.mu.
func (s *Stripe) handle(r *http.Request, body []byte, rec *Request) reply {
	s.expireDue()
	fn, id, ok := route(r)
	if !ok {
		return errorReply(http.StatusNotFound, "invalid_request_error", "",
			"Unrecognized request URL ("+r.Method+": "+r.URL.Path+"). Please see https://stripe.com/docs or we can help at https://support.stripe.com/.", "")
	}
	if rp, bad := s.authenticate(r, rec); bad {
		return rp
	}
	if !s.anyVersion && r.Header.Get("Stripe-Version") != s.version {
		return invalid("", "Invalid Stripe API version: \""+r.Header.Get("Stripe-Version")+
			"\". The fake answers to "+s.version+" only; send it in Stripe-Version.", "")
	}
	var form url.Values
	if r.Method == http.MethodPost {
		ct := r.Header.Get("Content-Type")
		if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/x-www-form-urlencoded" {
			return invalid("", "Invalid request: the API takes application/x-www-form-urlencoded bodies, not "+
				"\""+ct+"\".", "")
		}
		var err error
		if form, err = url.ParseQuery(string(body)); err != nil {
			return invalid("", "Invalid request: the body isn't a valid form.", "")
		}
	} else {
		var err error
		if form, err = url.ParseQuery(r.URL.RawQuery); err != nil {
			return invalid("", "Invalid request: the query string isn't valid.", "")
		}
	}
	rec.Form = form

	fault := s.peekFault(r)
	if fault != nil && !fault.After {
		s.useFault(fault)
		rec.Faulted = true
		return fault.reply()
	}

	key := r.Header.Get("Idempotency-Key")
	var rp reply
	switch {
	case r.Method == http.MethodPost && key != "":
		if len(key) > 255 {
			return invalid("", "Keys for idempotent requests can only be up to 255 characters.", "")
		}
		fp := r.Method + " " + r.URL.Path + "?" + form.Encode()
		if e, ok := s.idem[key]; ok && s.clock.Now().Sub(e.at) >= idemTTL {
			delete(s.idem, key) // forgotten: the request runs as a new one
		}
		if e, ok := s.idem[key]; ok {
			if e.fingerprint != fp {
				return errorReply(http.StatusBadRequest, "idempotency_error", "",
					"Keys for idempotent requests can only be used with the same parameters they were first used with. "+
						"Try using a key other than '"+key+"' if you meant to execute a different request.", "")
			}
			rp = e.reply.clone()
			rp.header.Set("Idempotent-Replayed", "true")
			rp.replayed = true
		} else {
			rp = fn(s, r, form, id)
			if rp.executed {
				s.idem[key] = &idemEntry{fingerprint: fp, reply: rp.clone(), at: s.clock.Now()}
			}
		}
	default:
		rp = fn(s, r, form, id)
	}

	// An After fault strikes only a request whose operation ran: not a replay of a saved answer
	// and not a refusal, which leave the fault queued for the request that does run.
	if fault != nil && !rp.replayed && (rp.executed || rp.status < 400) {
		s.useFault(fault)
		rec.Faulted = true
		fr := fault.reply()
		if e := s.idem[key]; e != nil && !fr.drop && !fr.hang && rp.executed && !rp.replayed {
			e.reply = fr.clone() // Stripe saves a 500 under the key too
		}
		fr.replayed = rp.replayed
		return fr
	}
	return rp
}

// authenticate checks the key: Authorization: Bearer <key>, or Basic with the key as the user.
func (s *Stripe) authenticate(r *http.Request, rec *Request) (reply, bool) {
	h := r.Header.Get("Authorization")
	var given, scheme string
	switch {
	case strings.HasPrefix(h, "Bearer "):
		given, scheme = strings.TrimPrefix(h, "Bearer "), "bearer"
	case strings.HasPrefix(h, "Basic "):
		if user, _, ok := r.BasicAuth(); ok { // the credentials must carry their separator
			given = user
		}
		scheme = "basic"
	}
	if given == "" {
		return errorReply(http.StatusUnauthorized, "invalid_request_error", "",
			"You did not provide an API key. You need to provide your API key in the Authorization header, "+
				"using Bearer auth (e.g. 'Authorization: Bearer YOUR_SECRET_KEY').", ""), true
	}
	if subtle.ConstantTimeCompare([]byte(given), []byte(s.apiKey)) != 1 {
		return errorReply(http.StatusUnauthorized, "invalid_request_error", "",
			"Invalid API Key provided: "+redactKey(given), ""), true
	}
	rec.Auth = scheme
	return reply{}, false
}

// redactKey shows a key the way Stripe's errors do: its prefix and its last four characters.
func redactKey(k string) string {
	if len(k) <= 12 {
		return "****"
	}
	return k[:8] + "****" + k[len(k)-4:]
}

type handlerFn func(s *Stripe, r *http.Request, form url.Values, id string) reply

// route picks the handler of a request by method and path.
func route(r *http.Request) (handlerFn, string, bool) {
	p, m := r.URL.Path, r.Method
	both := func(get, post handlerFn) (handlerFn, bool) {
		switch m {
		case http.MethodGet:
			return get, get != nil
		case http.MethodPost:
			return post, post != nil
		}
		return nil, false
	}
	switch p {
	case "/v1/checkout/sessions":
		fn, ok := both((*Stripe).listSessions, (*Stripe).createSession)
		return fn, "", ok
	case "/v1/payment_links":
		fn, ok := both((*Stripe).listLinks, (*Stripe).createLink)
		return fn, "", ok
	case "/v1/events":
		fn, ok := both((*Stripe).listEvents, nil)
		return fn, "", ok
	case "/v1/balance":
		fn, ok := both((*Stripe).balance, nil)
		return fn, "", ok
	case "/v1/account":
		fn, ok := both((*Stripe).retrieveAccount, nil)
		return fn, "", ok
	}
	if rest, ok := strings.CutPrefix(p, "/v1/checkout/sessions/"); ok && rest != "" {
		id, tail, _ := strings.Cut(rest, "/")
		switch {
		case tail == "" && m == http.MethodGet:
			return (*Stripe).retrieveSession, id, true
		case tail == "expire" && m == http.MethodPost:
			return (*Stripe).expireSession, id, true
		}
	}
	if rest, ok := strings.CutPrefix(p, "/v1/payment_links/"); ok && m == http.MethodGet {
		if id, tail, _ := strings.Cut(rest, "/"); id != "" && tail == "line_items" {
			return (*Stripe).linkLineItems, id, true
		}
	}
	if id, ok := strings.CutPrefix(p, "/v1/payment_links/"); ok && id != "" && !strings.Contains(id, "/") {
		fn, ok := both((*Stripe).retrieveLink, (*Stripe).updateLink)
		return fn, id, ok
	}
	if id, ok := strings.CutPrefix(p, "/v1/events/"); ok && id != "" && !strings.Contains(id, "/") && m == http.MethodGet {
		return (*Stripe).retrieveEvent, id, true
	}
	return nil, "", false
}

func (s *Stripe) livemodeWord() string {
	if s.live {
		return "live"
	}
	return "test"
}
