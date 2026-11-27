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

const SignatureHeader = "Stripe-Signature"

const Tolerance = 5 * time.Minute

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

type Signing struct {
	Secret       string
	At           time.Time
	Also         []string
	Unsigned     bool
	Malformed    bool
	V0Only       bool
	TestModeV0   bool
	ForgedV1     bool
	DebugEntries bool
	Tamper       func([]byte) []byte
	Delay        time.Duration
	Age          time.Duration
}

type SignOption func(*Signing)

func WithSecret(secret string) SignOption { return func(s *Signing) { s.Secret = secret } }

func WrongSecret() SignOption { return WithSecret(OtherWebhookSecret) }

func SignedAt(t time.Time) SignOption { return func(s *Signing) { s.At = t } }

func OldBy(age time.Duration) SignOption {
	return func(s *Signing) { s.At = time.Time{}; s.Age = age }
}

func AlsoSignedWith(secrets ...string) SignOption {
	return func(s *Signing) { s.Also = append(s.Also, secrets...) }
}

func Unsigned() SignOption { return func(s *Signing) { s.Unsigned = true } }

func MalformedSignature() SignOption { return func(s *Signing) { s.Malformed = true } }

func V0Only() SignOption { return func(s *Signing) { s.V0Only = true } }

func WithTestModeV0() SignOption { return func(s *Signing) { s.TestModeV0 = true } }

func ForgedV1WithValidV0() SignOption { return func(s *Signing) { s.ForgedV1 = true } }

func WithDebugEntries() SignOption { return func(s *Signing) { s.DebugEntries = true } }

func TamperedBody() SignOption {
	return func(s *Signing) {
		s.Tamper = func(b []byte) []byte { return append(bytes.Clone(b), '\n') }
	}
}

func TamperedWith(f func([]byte) []byte) SignOption { return func(s *Signing) { s.Tamper = f } }

func Delayed(d time.Duration) SignOption { return func(s *Signing) { s.Delay = d } }

type Delivery struct {
	EventID   string
	Type      string
	Body      []byte
	Signature string
	SignedAt  time.Time
	Status    int
	Response  []byte
	Header    http.Header
}

func (d Delivery) OK() bool { return d.Status >= 200 && d.Status < 300 }

type Sender struct {
	do     func(*http.Request) (*http.Response, error)
	url    string
	path   string
	secret string
	clock  Clock

	mu         sync.Mutex
	deliveries []Delivery
}

type SenderOption func(*Sender)

func WithSigningSecret(secret string) SenderOption { return func(s *Sender) { s.secret = secret } }

func WithSenderClock(c Clock) SenderOption { return func(s *Sender) { s.clock = c } }

func WithHTTPClient(c *http.Client) SenderOption {
	return func(s *Sender) { s.do = c.Do }
}

func NewSender(hookURL string, opts ...SenderOption) *Sender {
	s := &Sender{url: hookURL, secret: TestWebhookSecret, clock: realClock{},
		do: (&http.Client{
			Timeout:       30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}).Do}
	for _, o := range opts {
		o(s)
	}
	return s
}

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

type Advancer interface{ Advance(time.Duration) }

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

func (s *Sender) Send(ctx context.Context, ev Event, opts ...SignOption) (Delivery, error) {
	return s.SendBody(ctx, ev.ID, ev.Type, ev.Body(), opts...)
}

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
		at = at.Add(-sg.Age)
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
	case sg.ForgedV1:
		t := strconv.FormatInt(at.Unix(), 10)
		header = "t=" + t + ",v1=" + mac(OtherWebhookSecret+"forged", t, body) + ",v0=" + mac(secret, t, body)
	default:
		t := strconv.FormatInt(at.Unix(), 10)
		parts := []string{"t=" + t, "v1=" + mac(secret, t, body)}
		for _, also := range sg.Also {
			parts = append(parts, "v1="+mac(also, t, body))
		}
		if sg.TestModeV0 {
			parts = append(parts, "v0="+mac("v0:"+secret, t, body))
		}
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
	if err := ctx.Err(); err != nil {
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
	s.deliveries = append(s.deliveries, d.clone())
	s.mu.Unlock()
	if readErr != nil {
		return d, fmt.Errorf("testprovider: reading the answer to %s: %w", typ, readErr)
	}
	return d, nil
}

const maxAnswer = 1 << 20

func (d Delivery) clone() Delivery {
	d.Body, d.Response, d.Header = bytes.Clone(d.Body), bytes.Clone(d.Response), d.Header.Clone()
	return d
}

func (s *Sender) Deliveries() []Delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Delivery, len(s.deliveries))
	for i, d := range s.deliveries {
		out[i] = d.clone()
	}
	return out
}

func (s *Sender) SendAll(ctx context.Context, evs []Event, opts ...SignOption) ([]Delivery, error) {
	out := make([]Delivery, 0, len(evs))
	for _, ev := range evs {
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

func (s *Sender) Replay(ctx context.Context, d Delivery) (Delivery, error) {
	return s.post(ctx, d.EventID, d.Type, d.Body, d.Signature, d.SignedAt)
}

func Reversed(evs []Event) []Event {
	out := make([]Event, len(evs))
	for i, e := range evs {
		out[len(evs)-1-i] = e
	}
	return out
}

func Shuffled(evs []Event, seed uint64) []Event {
	out := append([]Event(nil), evs...)
	if len(out) < 2 {
		return out
	}
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	for range 100 {
		r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		for i := range out {
			if out[i].ID != evs[i].ID {
				return out
			}
		}
	}
	return out
}
