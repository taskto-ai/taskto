package testprovider

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SignatureHeader is the header Stripe signs its webhooks in.
const SignatureHeader = "Stripe-Signature"

// Tolerance is the age of a signature that Stripe's own libraries accept by default: five
// minutes. A handler that follows Stripe's advice rejects an older timestamp.
const Tolerance = 5 * time.Minute

// Sign returns the value of a Stripe-Signature header for body, signed with secret at time at:
// "t=<unix seconds>,v1=<hex HMAC-SHA256 of "<t>.<body>">", the scheme Stripe documents for
// manual verification. The key is the secret's text as it is, whsec_ prefix included.
func Sign(secret string, at time.Time, body []byte) string {
	t := strconv.FormatInt(at.Unix(), 10)
	return "t=" + t + ",v1=" + mac(secret, t, body)
}

func mac(secret, t string, body []byte) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(t))
	h.Write([]byte("."))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// NewEvent builds a snapshot event of type typ about object, created at at, in the shape Stripe
// sends. It is for tests that need an event the fake didn't make (an unknown type, an object
// with odd fields).
func NewEvent(typ string, object any, at time.Time) Event {
	raw, err := json.Marshal(object)
	if err != nil {
		panic("testprovider: event object doesn't marshal: " + err.Error())
	}
	return Event{
		ID: "evt_" + randomID(24), Object: "event", APIVersion: DefaultAPIVersion, Created: at.Unix(),
		Data: EventData{Object: raw}, PendingWebhooks: 1, Type: typ,
	}
}

// Signing says how a delivery is signed. The zero value is the right way: the sender's secret,
// the sender's clock, one signature, the body as signed. Change it with SignOptions.
type Signing struct {
	// Secret signs the delivery; empty means the sender's.
	Secret string
	// At is the time in the signature; zero means the clock's now.
	At time.Time
	// Also adds a v1 signature per secret, as Stripe does while a rolled secret is still valid.
	Also []string
	// Unsigned leaves the header out.
	Unsigned bool
	// Malformed sends a header that isn't in the format.
	Malformed bool
	// V0Only sends only the v0 test scheme, which a verifier must ignore.
	V0Only bool
	// DebugEntries adds the secret=, destination= and payload= entries Stripe may add. They
	// aren't signed and a verifier must ignore them.
	DebugEntries bool
	// Tamper changes the body after it was signed.
	Tamper func([]byte) []byte
	// Delay passes time before the delivery is signed, as when Stripe retries later.
	Delay time.Duration
	// Age makes the signature's timestamp older than the clock's now when it is signed (after
	// Delay has passed), without moving the clock.
	Age time.Duration
}

// A SignOption changes how a delivery is signed.
type SignOption func(*Signing)

// WithSecret signs with another secret than the sender's.
func WithSecret(secret string) SignOption { return func(s *Signing) { s.Secret = secret } }

// WrongSecret signs with a secret that isn't the integration's: OtherWebhookSecret.
func WrongSecret() SignOption { return WithSecret(OtherWebhookSecret) }

// SignedAt puts t in the signature.
func SignedAt(t time.Time) SignOption { return func(s *Signing) { s.At = t } }

// OldBy signs with a timestamp that is age older than the clock's now. Older than Tolerance is
// one a handler must reject.
func OldBy(age time.Duration) SignOption {
	return func(s *Signing) { s.At = time.Time{}; s.Age = age }
}

// AlsoSignedWith adds a signature made with another secret, as during a secret's rotation.
func AlsoSignedWith(secrets ...string) SignOption {
	return func(s *Signing) { s.Also = append(s.Also, secrets...) }
}

// Unsigned sends no signature.
func Unsigned() SignOption { return func(s *Signing) { s.Unsigned = true } }

// MalformedSignature sends a header that isn't a signature in Stripe's format.
func MalformedSignature() SignOption { return func(s *Signing) { s.Malformed = true } }

// V0Only sends the v0 test scheme and no v1.
func V0Only() SignOption { return func(s *Signing) { s.V0Only = true } }

// WithDebugEntries adds the unsigned debug entries Stripe may send.
func WithDebugEntries() SignOption { return func(s *Signing) { s.DebugEntries = true } }

// TamperedBody changes the body after it was signed: a byte appended, so the JSON still parses.
func TamperedBody() SignOption {
	return func(s *Signing) {
		s.Tamper = func(b []byte) []byte { return append(bytes.Clone(b), '\n') }
	}
}

// TamperedWith changes the body after it was signed with f.
func TamperedWith(f func([]byte) []byte) SignOption { return func(s *Signing) { s.Tamper = f } }

// Delayed passes d of the clock before signing: a delivery that comes late.
func Delayed(d time.Duration) SignOption { return func(s *Signing) { s.Delay = d } }

// Delivery is one request the sender made, kept so a test can see what was sent and replay it.
type Delivery struct {
	EventID string
	Type    string
	// Body is the body as sent, Signature the header as sent ("" if none), SignedAt the time in it.
	Body      []byte
	Signature string
	SignedAt  time.Time
	// Status and Response are the handler's answer.
	Status   int
	Response []byte
	Header   http.Header
}

// OK reports whether the handler answered with a 2xx, as Stripe wants.
func (d Delivery) OK() bool { return d.Status >= 200 && d.Status < 300 }

// Sender delivers events to a webhook handler, signed as Stripe signs them. It sends to a URL
// (a real server over HTTP) or straight to an http.Handler, and keeps the deliveries it made.
type Sender struct {
	do     func(*http.Request) (*http.Response, error)
	url    string
	path   string
	secret string
	clock  Clock

	mu         sync.Mutex
	deliveries []Delivery
}

// SenderOption changes how a Sender is built.
type SenderOption func(*Sender)

// WithSigningSecret sets the secret the sender signs with. The default is TestWebhookSecret.
func WithSigningSecret(secret string) SenderOption { return func(s *Sender) { s.secret = secret } }

// WithSenderClock sets the clock the signatures' timestamps are read from. Give it the clock
// the handler under test reads, so a signature's age is what the test says it is.
func WithSenderClock(c Clock) SenderOption { return func(s *Sender) { s.clock = c } }

// WithHTTPClient sets the client a URL sender uses.
func WithHTTPClient(c *http.Client) SenderOption {
	return func(s *Sender) { s.do = c.Do }
}

// NewSender returns a sender that posts to hookURL, such as an httptest server's URL plus the
// hook's path.
func NewSender(hookURL string, opts ...SenderOption) *Sender {
	s := &Sender{url: hookURL, secret: TestWebhookSecret, clock: realClock{},
		do: (&http.Client{
			Timeout: 30 * time.Second,
			// A redirect is a failed delivery to Stripe: the answer to record is the 3xx.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}).Do}
	for _, o := range opts {
		o(s)
	}
	return s
}

// NewHandlerSender returns a sender that calls h directly with a POST to path, without a
// network. The handler sees a request from 192.0.2.1 (a documentation address).
func NewHandlerSender(h http.Handler, path string, opts ...SenderOption) *Sender {
	s := &Sender{path: path, secret: TestWebhookSecret, clock: realClock{}}
	for _, o := range opts {
		o(s)
	}
	s.do = func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		r.RemoteAddr = "192.0.2.1:4242"
		h.ServeHTTP(rec, r)
		return rec.Result(), nil
	}
	return s
}

// Advancer is a clock a sender can move to make time pass (ManualClock is one). With a clock
// that isn't, Delayed really waits.
type Advancer interface{ Advance(time.Duration) }

// wait lets d pass: a clock that can advance is moved at once; with another, the wait is real
// and ends early, with the context's error, when ctx is done.
func (s *Sender) wait(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c, ok := s.clock.(Advancer); ok {
		c.Advance(d)
		return nil
	}
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Send delivers one event, signed as the options say. The delivery is returned whatever the
// handler answers; the error is only for a request that couldn't be made or answered.
func (s *Sender) Send(ctx context.Context, ev Event, opts ...SignOption) (Delivery, error) {
	return s.SendBody(ctx, ev.ID, ev.Type, ev.Body(), opts...)
}

// SendBody delivers body as it is, signed as the options say: for a body that isn't a valid
// event (broken JSON, an unknown shape, a huge one).
func (s *Sender) SendBody(ctx context.Context, id, typ string, body []byte, opts ...SignOption) (Delivery, error) {
	var sg Signing
	for _, o := range opts {
		o(&sg)
	}
	if sg.Delay > 0 {
		if err := s.wait(ctx, sg.Delay); err != nil {
			return Delivery{}, err
		}
	}
	at := sg.At
	if at.IsZero() {
		at = s.clock.Now()
		at = at.Add(-sg.Age) // OldBy: the clock doesn't move, the signature is older
	}
	secret := sg.Secret
	if secret == "" {
		secret = s.secret
	}
	header := ""
	switch {
	case sg.Unsigned:
	case sg.Malformed:
		header = "this is not a signature"
	case sg.V0Only:
		t := strconv.FormatInt(at.Unix(), 10)
		header = "t=" + t + ",v0=" + mac(secret, t, body)
	default:
		t := strconv.FormatInt(at.Unix(), 10)
		parts := []string{"t=" + t, "v1=" + mac(secret, t, body)}
		for _, also := range sg.Also {
			parts = append(parts, "v1="+mac(also, t, body))
		}
		parts = append(parts, "v0="+mac(secret, t, body)) // Stripe adds one in sandboxes
		if sg.DebugEntries {
			sum := sha256.Sum256(body)
			parts = append(parts, "secret=whsec_****************************FAKE",
				"destination=we_FakeEndpoint", "payload="+hex.EncodeToString(sum[:])[56:])
		}
		header = strings.Join(parts, ",")
	}
	sent := body
	if sg.Tamper != nil {
		sent = sg.Tamper(body)
	}
	return s.post(ctx, id, typ, sent, header, at)
}

func (s *Sender) post(ctx context.Context, id, typ string, body []byte, header string, at time.Time) (Delivery, error) {
	if err := ctx.Err(); err != nil { // a handler that ignores its context must not run after a cancel
		return Delivery{}, err
	}
	target := s.url
	if target == "" {
		target = "http://hook.test" + s.path
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return Delivery{}, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", "Stripe/1.0 (+https://stripe.com/docs/webhooks)")
	if header != "" {
		req.Header.Set(SignatureHeader, header)
	}
	resp, err := s.do(req)
	if err != nil {
		return Delivery{}, fmt.Errorf("testprovider: delivering %s: %w", typ, err)
	}
	defer resp.Body.Close()
	answer, readErr := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if readErr == nil && len(answer) > maxAnswer {
		answer, readErr = answer[:maxAnswer], fmt.Errorf("the answer is larger than %d bytes", maxAnswer)
	}
	d := Delivery{EventID: id, Type: typ, Body: bytes.Clone(body), Signature: header, SignedAt: at,
		Status: resp.StatusCode, Response: answer, Header: resp.Header}
	s.mu.Lock()
	s.deliveries = append(s.deliveries, d.clone()) // what is kept shares nothing with what is returned
	s.mu.Unlock()
	if readErr != nil {
		// The handler got the event and may have applied it: no retry here, and the delivery is
		// kept and returned beside the error.
		return d, fmt.Errorf("testprovider: reading the answer to %s: %w", typ, readErr)
	}
	return d, nil
}

// maxAnswer is the most of a handler's answer the sender keeps.
const maxAnswer = 1 << 20

// clone is a copy of the delivery that shares nothing with it.
func (d Delivery) clone() Delivery {
	d.Body, d.Response, d.Header = bytes.Clone(d.Body), bytes.Clone(d.Response), d.Header.Clone()
	return d
}

// Deliveries returns what the sender has sent, oldest first.
func (s *Sender) Deliveries() []Delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Delivery, len(s.deliveries))
	for i, d := range s.deliveries {
		out[i] = d.clone()
	}
	return out
}

// SendAll delivers the events in the order given, each signed afresh, and returns the
// deliveries. It stops at the first delivery that couldn't be made.
func (s *Sender) SendAll(ctx context.Context, evs []Event, opts ...SignOption) ([]Delivery, error) {
	out := make([]Delivery, 0, len(evs))
	for _, ev := range evs {
		d, err := s.Send(ctx, ev, opts...)
		if d.Status != 0 { // made, and perhaps unreadable: it is in the sender's record, so in this
			out = append(out, d)
		}
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// Duplicate delivers ev n times, as Stripe does when a delivery isn't acknowledged in time or is
// retried: the same event id and body, a new signature and timestamp each time.
func (s *Sender) Duplicate(ctx context.Context, ev Event, n int, opts ...SignOption) ([]Delivery, error) {
	out := make([]Delivery, 0, n)
	for range n {
		d, err := s.Send(ctx, ev, opts...)
		if d.Status != 0 {
			out = append(out, d)
		}
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// Replay sends again, exactly, what an earlier delivery sent: the same body and the same
// signature header with the old timestamp, as an attacker who recorded it would. The handler's
// clock decides whether it is too old.
func (s *Sender) Replay(ctx context.Context, d Delivery) (Delivery, error) {
	return s.post(ctx, d.EventID, d.Type, d.Body, d.Signature, d.SignedAt)
}

// Reversed returns evs in the opposite order: the usual out-of-order delivery, a payment's
// last event first.
func Reversed(evs []Event) []Event {
	out := make([]Event, len(evs))
	for i, e := range evs {
		out[len(evs)-1-i] = e
	}
	return out
}

// Shuffled returns evs in an order drawn from seed (the same seed, the same order), never the
// order given when there is more than one event.
func Shuffled(evs []Event, seed uint64) []Event {
	out := append([]Event(nil), evs...)
	if len(out) < 2 {
		return out
	}
	// A seeded order, so a failing test replays; nothing here is secret.
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)) //nolint:gosec // a reproducible order for tests, not a secret
	for range 100 {
		r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		for i := range out {
			if out[i].ID != evs[i].ID {
				return out
			}
		}
	}
	return out // every event is the same one: no order differs
}
