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

const DefaultAPIVersion = "2026-11-18.endive"

var (
	TestAPIKey         = "sk_" + "test_" + strings.Repeat("Fake", 6)
	TestAccountID      = "acct_" + strings.Repeat("Fake", 4)
	TestWebhookSecret  = "whsec_" + strings.Repeat("Fake", 6)
	OtherWebhookSecret = "whsec_" + strings.Repeat("Rota", 6)
)

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

type Option func(*Stripe)

func WithClock(c Clock) Option { return func(s *Stripe) { s.clock = c } }

func WithAPIKey(key string) Option {
	return func(s *Stripe) { s.apiKey = key; s.live = strings.Contains(key, "_live_") }
}

func WithAnyAPIVersion() Option { return func(s *Stripe) { s.anyVersion = true } }

func WithAccount(id string) Option { return func(s *Stripe) { s.account = id } }

func (s *Stripe) SetAccount(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.account = id
}

func (s *Stripe) SetAPIKey(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.apiKey, s.live = key, strings.Contains(key, "_live_")
}

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
	t.Cleanup(s.srv.Close)
	t.Cleanup(func() { close(s.done) })
	return s
}

func (s *Stripe) URL() string { return s.srv.URL }

func (s *Stripe) Host() string { return strings.TrimPrefix(s.srv.URL, "http://") }

func (s *Stripe) APIKey() string { return s.apiKey }

func (s *Stripe) APIVersion() string { return s.version }

func (s *Stripe) Clock() Clock { return s.clock }

type Request struct {
	Method, Path   string
	Query          url.Values
	Form           url.Values
	IdempotencyKey string
	APIVersion     string
	Auth           string
	Status         int
	Replayed       bool
	Faulted        bool
	At             time.Time
}

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

const idemTTL = 24 * time.Hour

const maxBody = 1 << 20

func (s *Stripe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, readErr := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	_ = r.Body.Close()

	s.mu.Lock()
	rec := Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), At: s.clock.Now(),
		IdempotencyKey: r.Header.Get("Idempotency-Key"), APIVersion: r.Header.Get("Stripe-Version")}
	var rp reply
	if readErr != nil || len(body) > maxBody {
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

	key := r.Header.Get("Idempotency-Key")
	fp := r.Method + " " + r.URL.Path + "?" + form.Encode()
	if e, ok := s.idem[key]; ok && s.clock.Now().Sub(e.at) >= idemTTL {
		delete(s.idem, key)
	}
	fault := s.peekFault(r)
	if fault != nil && !fault.After {
		s.useFault(fault)
		rec.Faulted = true
		fr := fault.reply()
		if _, saved := s.idem[key]; fault.Kept && r.Method == http.MethodPost && key != "" && !saved && !fr.drop && !fr.hang {
			s.idem[key] = &idemEntry{fingerprint: fp, reply: fr.clone(), at: s.clock.Now()}
		}
		return fr
	}

	var rp reply
	switch {
	case r.Method == http.MethodPost && key != "":
		if len(key) > 255 {
			return invalid("", "Keys for idempotent requests can only be up to 255 characters.", "")
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

	if fault != nil && !rp.replayed && (rp.executed || rp.status < 400) {
		s.useFault(fault)
		rec.Faulted = true
		fr := fault.reply()
		if e := s.idem[key]; e != nil && !fr.drop && !fr.hang && rp.executed && !rp.replayed {
			e.reply = fr.clone()
		}
		fr.replayed = rp.replayed
		return fr
	}
	return rp
}

func (s *Stripe) authenticate(r *http.Request, rec *Request) (reply, bool) {
	h := r.Header.Get("Authorization")
	var given, scheme string
	switch {
	case strings.HasPrefix(h, "Bearer "):
		given, scheme = strings.TrimPrefix(h, "Bearer "), "bearer"
	case strings.HasPrefix(h, "Basic "):
		if user, _, ok := r.BasicAuth(); ok {
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

func redactKey(k string) string {
	if len(k) <= 12 {
		return "****"
	}
	return k[:8] + "****" + k[len(k)-4:]
}

type handlerFn func(s *Stripe, r *http.Request, form url.Values, id string) reply

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
