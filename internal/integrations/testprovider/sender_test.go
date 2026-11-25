package testprovider_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/integrations/testprovider"
)

// verifyStripe is the check Stripe documents for verifying a webhook by hand, written from the
// documentation and not from the sender's code: split the header, keep t and every v1, ignore
// the rest (v0 and the debug entries), HMAC "<t>.<body>" with SHA-256, compare in constant time
// against every v1, then check the timestamp's age.
func verifyStripe(header string, body []byte, secrets []string, now time.Time, tolerance time.Duration) error {
	if header == "" {
		return errors.New("no signature header")
	}
	var t string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return errors.New("malformed header")
		}
		switch k {
		case "t":
			t = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if t == "" || len(sigs) == 0 {
		return errors.New("no timestamp or no v1 signature")
	}
	secs, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return errors.New("timestamp isn't a number")
	}
	for _, secret := range secrets {
		h := hmac.New(sha256.New, []byte(secret))
		h.Write([]byte(t + "." + string(body)))
		want := hex.EncodeToString(h.Sum(nil))
		for _, got := range sigs {
			if hmac.Equal([]byte(want), []byte(got)) {
				if d := now.Sub(time.Unix(secs, 0)); d > tolerance || d < -tolerance {
					return errors.New("timestamp outside the tolerance")
				}
				return nil
			}
		}
	}
	return errors.New("no signature matches")
}

// hook is a webhook handler written the way Stripe's documentation asks, with the dedupe of
// event ids: the test's stand-in for the real hook handler.
type hook struct {
	secrets []string
	clock   testprovider.Clock

	mu      sync.Mutex
	applied []string // the event ids applied, once each
	seen    map[string]int
	refused int
}

func newHook(clock testprovider.Clock, secrets ...string) *hook {
	if len(secrets) == 0 {
		secrets = []string{testprovider.TestWebhookSecret}
	}
	return &hook{secrets: secrets, clock: clock, seen: map[string]int{}}
}

func (h *hook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	buf := make([]byte, 1<<20)
	n, _ := r.Body.Read(buf)
	body := buf[:n]
	if err := verifyStripe(r.Header.Get(testprovider.SignatureHeader), body, h.secrets, h.clock.Now(), testprovider.Tolerance); err != nil {
		h.mu.Lock()
		h.refused++
		h.mu.Unlock()
		http.Error(w, "refused", http.StatusBadRequest)
		return
	}
	var ev struct{ ID string }
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	h.seen[ev.ID]++
	if h.seen[ev.ID] == 1 {
		h.applied = append(h.applied, ev.ID)
	}
	h.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (h *hook) count() (applied, refused int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.applied), h.refused
}

func someEvent(clock testprovider.Clock) testprovider.Event {
	return testprovider.NewEvent("checkout.session.completed", map[string]any{"id": "cs_test_x", "object": "checkout.session"}, clock.Now())
}

// Both ways of delivering: to a handler in the process and to a server over HTTP.
func senders(t *testing.T, h http.Handler, clock testprovider.Clock, opts ...testprovider.SenderOption) map[string]*testprovider.Sender {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	opts = append([]testprovider.SenderOption{testprovider.WithSenderClock(clock)}, opts...)
	return map[string]*testprovider.Sender{
		"to a handler": testprovider.NewHandlerSender(h, "/hooks/abc", opts...),
		"to a server":  testprovider.NewSender(srv.URL+"/hooks/abc", opts...),
	}
}

func TestSignatureFormat(t *testing.T) {
	at := time.Date(2030, 3, 1, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"id":"evt_1"}`)
	got := testprovider.Sign(testprovider.TestWebhookSecret, at, body)
	t1, v1, ok := strings.Cut(got, ",")
	if !ok || t1 != "t="+strconv.FormatInt(at.Unix(), 10) || !strings.HasPrefix(v1, "v1=") || len(v1) != len("v1=")+64 {
		t.Fatalf("header %q", got)
	}
	if err := verifyStripe(got, body, []string{testprovider.TestWebhookSecret}, at, testprovider.Tolerance); err != nil {
		t.Errorf("the reference verifier refuses it: %v", err)
	}
	// The signed string is "<t>.<body>": any change to the secret, the time or the body breaks it.
	for name, header := range map[string]string{
		"another secret": testprovider.Sign(testprovider.OtherWebhookSecret, at, body),
		// The time is part of what is signed: a header whose t was changed afterwards is refused.
		"a changed time": strings.Replace(got, "t="+strconv.FormatInt(at.Unix(), 10), "t="+strconv.FormatInt(at.Unix()+1, 10), 1),
		"another body":   testprovider.Sign(testprovider.TestWebhookSecret, at, append(body, ' ')),
	} {
		if header == got {
			t.Errorf("%s gives the same signature", name)
		}
		if err := verifyStripe(header, body, []string{testprovider.TestWebhookSecret}, at, testprovider.Tolerance); err == nil {
			t.Errorf("%s verifies", name)
		}
	}
	// The key is the secret's text as it is: a verifier that strips whsec_ or decodes it refuses.
	h := hmac.New(sha256.New, []byte(strings.TrimPrefix(testprovider.TestWebhookSecret, "whsec_")))
	h.Write([]byte(strconv.FormatInt(at.Unix(), 10) + "." + string(body)))
	if strings.HasSuffix(got, hex.EncodeToString(h.Sum(nil))) {
		t.Error("the sender signs with the secret without its prefix")
	}
}

func TestSignedDeliveriesAreAcceptedAndBadOnesAreNot(t *testing.T) {
	clock := testprovider.NewManualClock(time.Time{})
	tests := []struct {
		name string
		opts []testprovider.SignOption
		ok   bool
	}{
		{"the right secret", nil, true},
		{"the unsigned debug entries and a v0", []testprovider.SignOption{testprovider.WithDebugEntries()}, true},
		{"a wrong secret", []testprovider.SignOption{testprovider.WrongSecret()}, false},
		{"a timestamp 4 minutes old", []testprovider.SignOption{testprovider.OldBy(4 * time.Minute)}, true},
		{"a timestamp 6 minutes old", []testprovider.SignOption{testprovider.OldBy(6 * time.Minute)}, false},
		{"a timestamp a day old", []testprovider.SignOption{testprovider.OldBy(24 * time.Hour)}, false},
		{"a timestamp in the future", []testprovider.SignOption{testprovider.SignedAt(clock.Now().Add(time.Hour))}, false},
		{"no signature", []testprovider.SignOption{testprovider.Unsigned()}, false},
		{"a malformed signature", []testprovider.SignOption{testprovider.MalformedSignature()}, false},
		{"only the v0 scheme", []testprovider.SignOption{testprovider.V0Only()}, false},
		{"a body changed after signing", []testprovider.SignOption{testprovider.TamperedBody()}, false},
		{"a wrong secret and the right one (a rotation)", []testprovider.SignOption{testprovider.WrongSecret(), testprovider.AlsoSignedWith(testprovider.TestWebhookSecret)}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHook(clock)
			for kind, s := range senders(t, h, clock) {
				d, err := s.Send(context.Background(), someEvent(clock), tt.opts...)
				if err != nil {
					t.Fatalf("%s: %v", kind, err)
				}
				if d.OK() != tt.ok {
					t.Errorf("%s: status %d, want ok=%v", kind, d.Status, tt.ok)
				}
				if d.Type != "checkout.session.completed" || !strings.HasPrefix(d.EventID, "evt_") {
					t.Errorf("%s: delivery %+v", kind, d)
				}
			}
			applied, refused := h.count()
			// Two senders, one delivery each.
			if tt.ok && (applied != 2 || refused != 0) || !tt.ok && (applied != 0 || refused != 2) {
				t.Errorf("applied %d refused %d", applied, refused)
			}
		})
	}
}

func TestARolledSecretIsAcceptedUntilItExpires(t *testing.T) {
	clock := testprovider.NewManualClock(time.Time{})
	// Stripe signs once per active secret while a rolled one is still valid: a handler that
	// knows only the old secret must accept it, and one that knows only the new one too.
	oldOnly := newHook(clock, testprovider.OtherWebhookSecret)
	newOnly := newHook(clock, testprovider.TestWebhookSecret)
	for kind, s := range senders(t, oldOnly, clock, testprovider.WithSigningSecret(testprovider.TestWebhookSecret)) {
		d, _ := s.Send(context.Background(), someEvent(clock), testprovider.AlsoSignedWith(testprovider.OtherWebhookSecret))
		if !d.OK() {
			t.Errorf("%s: a handler with the old secret: %d", kind, d.Status)
		}
	}
	for kind, s := range senders(t, newOnly, clock) {
		d, _ := s.Send(context.Background(), someEvent(clock), testprovider.AlsoSignedWith(testprovider.OtherWebhookSecret))
		if !d.OK() {
			t.Errorf("%s: a handler with the new secret: %d", kind, d.Status)
		}
	}
	// After the overlap there is one signature, and the old secret's handler refuses it.
	d, _ := testprovider.NewHandlerSender(oldOnly, "/h", testprovider.WithSenderClock(clock)).Send(context.Background(), someEvent(clock))
	if d.OK() {
		t.Error("the old secret accepted a signature of the new one only")
	}
}

func TestDuplicatesAreTheSameEventDeliveredAgain(t *testing.T) {
	clock := testprovider.NewManualClock(time.Time{})
	h := newHook(clock)
	for kind, s := range senders(t, h, clock) {
		ev := someEvent(clock)
		ds, err := s.Duplicate(context.Background(), ev, 3, testprovider.Delayed(time.Minute))
		if err != nil || len(ds) != 3 {
			t.Fatalf("%s: %v %d", kind, err, len(ds))
		}
		for i, d := range ds {
			if !d.OK() || d.EventID != ev.ID || string(d.Body) != string(ds[0].Body) {
				t.Errorf("%s: delivery %d: %+v", kind, i, d)
			}
			// A retry is signed again, with its own timestamp.
			if i > 0 && d.Signature == ds[i-1].Signature {
				t.Errorf("%s: delivery %d has the signature of the one before", kind, i)
			}
		}
	}
	applied, refused := h.count()
	if applied != 2 || refused != 0 {
		t.Errorf("two events (one per sender) applied %d, refused %d", applied, refused)
	}
	for id, n := range h.seen {
		if n != 3 {
			t.Errorf("%s was seen %d times", id, n)
		}
	}
}

func TestAReplayIsTheSameBytesAndTheOldSignature(t *testing.T) {
	clock := testprovider.NewManualClock(time.Time{})
	h := newHook(clock)
	for kind, s := range senders(t, h, clock) {
		first, err := s.Send(context.Background(), someEvent(clock))
		if err != nil || !first.OK() {
			t.Fatalf("%s: %v %d", kind, err, first.Status)
		}
		quick, _ := s.Replay(context.Background(), first)
		if !quick.OK() || quick.Signature != first.Signature || string(quick.Body) != string(first.Body) {
			t.Errorf("%s: a replay within the tolerance: %d", kind, quick.Status)
		}
		clock.Advance(testprovider.Tolerance + time.Second)
		late, _ := s.Replay(context.Background(), first)
		if late.OK() {
			t.Errorf("%s: a replay after the tolerance was accepted", kind)
		}
		if late.Signature != first.Signature || !late.SignedAt.Equal(first.SignedAt) {
			t.Errorf("%s: the replay changed the signature", kind)
		}
		clock.Set(testprovider.DefaultStart)
	}
}

func TestADelayedDeliveryIsSignedWhenItIsSent(t *testing.T) {
	clock := testprovider.NewManualClock(time.Time{})
	h := newHook(clock)
	s := testprovider.NewHandlerSender(h, "/h", testprovider.WithSenderClock(clock))
	start := clock.Now()
	d, err := s.Send(context.Background(), someEvent(clock), testprovider.Delayed(2*time.Hour))
	if err != nil || !d.OK() {
		t.Fatalf("%v %d", err, d.Status)
	}
	if !clock.Now().Equal(start.Add(2*time.Hour)) || !d.SignedAt.Equal(start.Add(2*time.Hour)) {
		t.Errorf("clock %v, signed at %v", clock.Now(), d.SignedAt)
	}
}

func TestOutOfOrderDelivery(t *testing.T) {
	clock := testprovider.NewManualClock(time.Time{})
	f := testprovider.NewStripe(t, testprovider.WithClock(clock))
	a := newAPI(t, f)
	sess := a.newSession(12000, "eur", "ref_order")
	evs, err := f.Pay(sess.ID)
	if err != nil {
		t.Fatal(err)
	}

	ids := func(es []testprovider.Event) string {
		var out []string
		for _, e := range es {
			out = append(out, e.ID)
		}
		return strings.Join(out, " ")
	}
	rev := testprovider.Reversed(evs)
	if rev[0].Type != "checkout.session.completed" || rev[4].Type != "payment_intent.created" || ids(rev) == ids(evs) {
		t.Errorf("reversed: %v", ids(rev))
	}
	if ids(testprovider.Reversed(rev)) != ids(evs) || evs[0].Type != "payment_intent.created" {
		t.Error("Reversed changed its argument")
	}
	shuf := testprovider.Shuffled(evs, 7)
	if ids(shuf) == ids(evs) || len(shuf) != len(evs) || ids(shuf) != ids(testprovider.Shuffled(evs, 7)) {
		t.Errorf("shuffled: %v", ids(shuf))
	}
	if ids(testprovider.Shuffled(evs[:1], 7)) != ids(evs[:1]) || len(testprovider.Shuffled(nil, 1)) != 0 {
		t.Error("shuffling one event or none")
	}
	same := []testprovider.Event{evs[0], evs[0]}
	_ = testprovider.Shuffled(same, 1) // must end

	// Delivered to a handler in the order given, all of them.
	h := newHook(clock)
	s := testprovider.NewHandlerSender(h, "/h", testprovider.WithSenderClock(clock))
	ds, err := s.SendAll(context.Background(), rev)
	if err != nil || len(ds) != 5 {
		t.Fatalf("%v %d", err, len(ds))
	}
	var order []string
	for _, d := range ds {
		order = append(order, d.Type)
		if !d.OK() {
			t.Errorf("%s: %d", d.Type, d.Status)
		}
	}
	if got := strings.Join(order, " "); !strings.HasPrefix(got, "checkout.session.completed") || !strings.HasSuffix(got, "payment_intent.created") {
		t.Errorf("delivery order: %s", got)
	}
	if applied, _ := h.count(); applied != 5 {
		t.Errorf("applied %d", applied)
	}
	if got := len(s.Deliveries()); got != 5 {
		t.Errorf("the sender kept %d deliveries", got)
	}
}

func TestOddBodies(t *testing.T) {
	clock := testprovider.NewManualClock(time.Time{})
	h := newHook(clock)
	s := testprovider.NewHandlerSender(h, "/h", testprovider.WithSenderClock(clock))
	// A signed body that isn't JSON passes the signature and is the handler's to refuse.
	d, err := s.SendBody(context.Background(), "", "broken", []byte(`{"id": `))
	if err != nil || d.Status != http.StatusBadRequest {
		t.Errorf("broken JSON: %v %d", err, d.Status)
	}
	if _, refused := h.count(); refused != 0 {
		t.Errorf("the signature of a broken body was refused: %d", refused)
	}
	// A body past the handler's cap is for it to cut; the sender sends what it is told.
	big := []byte(`{"id":"evt_big","pad":"` + strings.Repeat("x", 600<<10) + `"}`)
	d, err = s.SendBody(context.Background(), "evt_big", "big", big)
	if err != nil || len(d.Body) != len(big) {
		t.Errorf("a big body: %v %d", err, len(d.Body))
	}
}

func TestADeliveryFailureIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	s := testprovider.NewSender(url + "/hooks/x")
	if _, err := s.Send(context.Background(), someEvent(testprovider.NewManualClock(time.Time{}))); err == nil {
		t.Error("delivering to a closed server succeeded")
	}
	if len(s.Deliveries()) != 0 {
		t.Error("a delivery that wasn't made was kept")
	}
}

// What the fake makes and what the sender sends are meant for each other: every event of a
// payment, signed, to a handler that verifies as Stripe documents.
func TestTheFakesEventsReachAHandlerSigned(t *testing.T) {
	clock := testprovider.NewManualClock(time.Time{})
	f := testprovider.NewStripe(t, testprovider.WithClock(clock))
	a := newAPI(t, f)
	sess := a.newSession(12000, "eur", "ref_e2e")
	if _, err := f.Pay(sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Refund(sess.ID, 0); err != nil {
		t.Fatal(err)
	}
	h := newHook(clock)
	s := testprovider.NewHandlerSender(h, "/hooks/abc", testprovider.WithSenderClock(clock))
	ds, err := s.SendAll(context.Background(), f.Events())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		if !d.OK() {
			t.Errorf("%s: %d", d.Type, d.Status)
		}
		var ev testprovider.Event
		if err := json.Unmarshal(d.Body, &ev); err != nil || ev.Type != d.Type || ev.Created != clock.Now().Unix() {
			t.Errorf("%s: body: %v", d.Type, err)
		}
	}
	if applied, refused := h.count(); applied != len(f.Events()) || refused != 0 {
		t.Errorf("applied %d of %d, refused %d", applied, len(f.Events()), refused)
	}
	// An event of a type the integration doesn't act on still arrives well formed.
	odd := testprovider.NewEvent("customer.created", map[string]any{"id": "cus_x", "object": "customer"}, clock.Now())
	if d, _ := s.Send(context.Background(), odd); !d.OK() || !strings.Contains(string(d.Body), `"type": "customer.created"`) {
		t.Errorf("an unrelated event: %d", d.Status)
	}
	_ = fmt.Sprint
}
