package integrations

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/integrations/testprovider"
)

const hookOrigin = "https://taskto.example.test"

type rig struct {
	*env
	clock *testprovider.ManualClock
	fake  *testprovider.Stripe
	owner *client
	ws    int64
	in    Integration
	id    string
	send  *testprovider.Sender
}

func newRig(t *testing.T) *rig {
	t.Helper()
	clock := testprovider.NewManualClock(time.Time{})
	e := newEnvWith(t, Options{PublicURL: hookOrigin, Now: clock.Now})
	owner, ws := e.setup()
	r := &rig{env: e, clock: clock, fake: testprovider.NewStripe(t, testprovider.WithClock(clock)), owner: owner, ws: ws}
	r.in, r.id, r.send = r.integration(stripeBody("shop"), stripeHook)
	return r
}

func (r *rig) integration(body map[string]any, secret string) (Integration, string, *testprovider.Sender) {
	r.t.Helper()
	in := r.create(r.owner, r.ws, body)
	if in.WebhookURL == nil {
		r.t.Fatal("the integration has no webhook address")
	}
	id := strings.TrimPrefix(*in.WebhookURL, hookOrigin+HookPrefix)
	return in, id, r.sender(id, secret)
}

func (r *rig) sender(id, secret string) *testprovider.Sender {
	return testprovider.NewHandlerSender(r.h, HookPrefix+id, testprovider.WithSigningSecret(secret), testprovider.WithSenderClock(r.clock))
}

func token() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (r *rig) session(amount int, currency string) (testprovider.Session, string) {
	r.t.Helper()
	ref := token()
	v := url.Values{}
	v.Set("mode", "payment")
	v.Set("success_url", "https://app.example.test/paid")
	v.Set("cancel_url", "https://app.example.test/cancelled")
	v.Set("client_reference_id", ref)
	v.Set("line_items[0][quantity]", "1")
	v.Set("line_items[0][price_data][currency]", currency)
	v.Set("line_items[0][price_data][unit_amount]", strconv.Itoa(amount))
	v.Set("line_items[0][price_data][product_data][name]", "Invoice 7")
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, r.fake.URL()+"/v1/checkout/sessions", strings.NewReader(v.Encode()))
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+r.fake.APIKey())
	req.Header.Set("Stripe-Version", testprovider.DefaultAPIVersion)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		r.t.Fatalf("the fake refused a session: %d %s", resp.StatusCode, raw)
	}
	var s testprovider.Session
	if err := json.Unmarshal(raw, &s); err != nil {
		r.t.Fatal(err)
	}
	return s, ref
}

func (r *rig) deliver(s *testprovider.Sender, evs []testprovider.Event, opts ...testprovider.SignOption) []testprovider.Delivery {
	r.t.Helper()
	ds, err := s.SendAll(context.Background(), evs, opts...)
	if err != nil {
		r.t.Fatal(err)
	}
	return ds
}

func (r *rig) must(evs []testprovider.Event, err error) []testprovider.Event {
	r.t.Helper()
	if err != nil {
		r.t.Fatal(err)
	}
	return evs
}

type stored struct {
	ID       int64
	Event    string
	Kind     string
	Ref      sql.NullString
	Object   sql.NullString
	Payment  sql.NullString
	Amount   int64
	Currency string
	Sum      string
	State    string
	Source   string
	Attempts int
	Code     sql.NullString
}

func (r *rig) rows(integration int64) []stored {
	r.t.Helper()
	rs, err := r.db.Query(`SELECT id, provider_event_id, kind, ref_token, object_id, provider_payment_id, amount_minor, currency,
		body_sha256, state, source, attempts, error_code FROM integration_events WHERE integration_id = ? ORDER BY id`, integration)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rs.Close()
	var out []stored
	for rs.Next() {
		var s stored
		if err := rs.Scan(&s.ID, &s.Event, &s.Kind, &s.Ref, &s.Object, &s.Payment, &s.Amount, &s.Currency, &s.Sum, &s.State, &s.Source,
			&s.Attempts, &s.Code); err != nil {
			r.t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rs.Err(); err != nil {
		r.t.Fatal(err)
	}
	return out
}

func (r *rig) total() int {
	r.t.Helper()
	var n int
	if err := r.db.QueryRow(`SELECT count(*) FROM integration_events`).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

func (r *rig) lastEvent(id int64) sql.NullString {
	r.t.Helper()
	var at sql.NullString
	if err := r.db.QueryRow(`SELECT last_event_at FROM integrations WHERE id = ?`, id).Scan(&at); err != nil {
		r.t.Fatal(err)
	}
	return at
}

func (r *rig) completed() testprovider.Event {
	r.t.Helper()
	var ev testprovider.Event
	for _, e := range r.fake.Events() {
		if e.Type == testprovider.EventSessionCompleted {
			ev = e
		}
	}
	if ev.ID == "" {
		r.t.Fatal("the fake made no checkout.session.completed")
	}
	return ev
}

func statuses(ds []testprovider.Delivery) []int {
	out := make([]int, len(ds))
	for i, d := range ds {
		out[i] = d.Status
	}
	return out
}

func (r *rig) post(path string, body []byte, h http.Header, remote string) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	for k, v := range h {
		req.Header[k] = v
	}
	if remote != "" {
		req.RemoteAddr = remote
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

func isNotFound(t *testing.T, label string, status int, body []byte) {
	t.Helper()
	if status != http.StatusNotFound || !strings.Contains(string(body), `"code":"not_found"`) {
		t.Errorf("%s = %d %q, want the plain 404", label, status, body)
	}
}

func TestAValidEventIsAcceptedOnce(t *testing.T) {
	r := newRig(t)
	s, ref := r.session(12000, "eur")
	evs := r.must(r.fake.Pay(s.ID))
	if len(evs) < 4 {
		t.Fatalf("a card payment made %d events", len(evs))
	}

	ds := r.deliver(r.send, evs)
	for _, d := range ds {
		if d.Status != http.StatusOK {
			t.Fatalf("%s answered %d %s", d.Type, d.Status, d.Response)
		}
	}
	rows := r.rows(r.in.ID)
	if len(rows) != 1 {
		t.Fatalf("%d events stored, want 1: %+v", len(rows), rows)
	}
	got := rows[0]
	var completed testprovider.Event
	for _, e := range evs {
		if e.Type == testprovider.EventSessionCompleted {
			completed = e
		}
	}
	sum := sha256.Sum256(completed.Body())
	if got.Event != completed.ID || got.Kind != EventPaymentSucceeded || got.Ref.String != ref || got.Object.String != s.ID ||
		!got.Payment.Valid || !strings.HasPrefix(got.Payment.String, "pi_") || got.Amount != 12000 || got.Currency != "EUR" ||
		got.Sum != hex.EncodeToString(sum[:]) || got.State != EventPending || got.Source != SourceWebhook || got.Attempts != 0 {
		t.Fatalf("stored %+v, want the session's event %s of %s for %s", got, completed.ID, s.ID, ref)
	}
	if !r.lastEvent(r.in.ID).Valid {
		t.Error("last_event_at was not set by a verified event")
	}

	r.clock.Advance(time.Second)
	ds, err := r.send.Duplicate(context.Background(), completed, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := statuses(ds); fmt.Sprint(got) != "[200 200 200]" {
		t.Fatalf("duplicates answered %v", got)
	}
	if d, err := r.send.Replay(context.Background(), ds[0]); err != nil || d.Status != http.StatusOK {
		t.Fatalf("a replay inside the window = %d, %v", d.Status, err)
	}
	if n := len(r.rows(r.in.ID)); n != 1 {
		t.Fatalf("%d events after duplicates and a replay, want 1", n)
	}

	r.clock.Advance(StripeTolerance + time.Second)
	d, err := r.send.Replay(context.Background(), ds[0])
	if err != nil {
		t.Fatal(err)
	}
	isNotFound(t, "a replay after the window", d.Status, d.Response)
	if n := len(r.rows(r.in.ID)); n != 1 {
		t.Fatalf("%d events after a late replay, want 1", n)
	}
}

func TestOnePaymentAsEachNormalizedEvent(t *testing.T) {
	r := newRig(t)

	s, _ := r.session(5000, "usd")
	r.deliver(r.send, r.must(r.fake.Decline(s.ID)))
	if n := r.total(); n != 0 {
		t.Fatalf("a declined card stored %d events", n)
	}

	s, ref := r.session(7500, "eur")
	r.deliver(r.send, r.must(r.fake.PayDelayed(s.ID)))
	if n := r.total(); n != 0 {
		t.Fatalf("an unpaid completed session stored %d events", n)
	}
	if !r.lastEvent(r.in.ID).Valid {
		t.Error("a verified event of a type nobody acts on didn't move last_event_at")
	}
	r.deliver(r.send, r.must(r.fake.SettleDelayed(s.ID, true)))
	rows := r.rows(r.in.ID)
	if len(rows) != 1 || rows[0].Kind != EventPaymentSucceeded || rows[0].Ref.String != ref || rows[0].Amount != 7500 {
		t.Fatalf("after the delayed payment settled: %+v", rows)
	}

	s, ref = r.session(900, "eur")
	r.deliver(r.send, r.must(r.fake.PayDelayed(s.ID)))
	r.deliver(r.send, r.must(r.fake.SettleDelayed(s.ID, false)))
	last := r.rows(r.in.ID)[1]
	if last.Kind != EventPaymentFailed || last.Ref.String != ref || last.Amount != 900 {
		t.Fatalf("a failed delayed payment: %+v", last)
	}

	s, ref = r.session(1500, "gbp")
	r.deliver(r.send, r.must(r.fake.Expire(s.ID)))
	last = r.rows(r.in.ID)[2]
	if last.Kind != EventPaymentExpired || last.Ref.String != ref || last.Currency != "GBP" {
		t.Fatalf("an expired session: %+v", last)
	}

	s, _ = r.session(4000, "eur")
	paid := r.must(r.fake.Pay(s.ID))
	r.deliver(r.send, paid)
	r.deliver(r.send, r.must(r.fake.Refund(s.ID, 1000)))
	rows = r.rows(r.in.ID)
	refund, paidRow := rows[len(rows)-1], rows[len(rows)-2]
	if refund.Kind != EventPaymentRefunded || refund.Ref.Valid || refund.Amount != 1000 || !strings.HasPrefix(refund.Object.String, "ch_") ||
		refund.Payment.String != paidRow.Payment.String || !paidRow.Payment.Valid {
		t.Fatalf("a refund: %+v after %+v", refund, paidRow)
	}
}

func TestBadDeliveriesAreRefusedAlike(t *testing.T) {
	r := newRig(t)
	s, _ := r.session(2500, "eur")
	r.must(r.fake.Pay(s.ID))
	ev := r.completed()

	unknown := r.sender(token(), stripeHook)
	want, err := unknown.Send(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}
	isNotFound(t, "an unknown id", want.Status, want.Response)

	cases := []struct {
		name string
		opts []testprovider.SignOption
	}{
		{"wrong secret", []testprovider.SignOption{testprovider.WrongSecret()}},
		{"unsigned", []testprovider.SignOption{testprovider.Unsigned()}},
		{"malformed header", []testprovider.SignOption{testprovider.MalformedSignature()}},
		{"the test scheme only", []testprovider.SignOption{testprovider.V0Only()}},
		{"tampered body", []testprovider.SignOption{testprovider.TamperedBody()}},
		{"a byte changed", []testprovider.SignOption{testprovider.TamperedWith(func(b []byte) []byte {
			c := bytes.Clone(b)
			c[len(c)/2] ^= 1
			return c
		})}},
		{"too old", []testprovider.SignOption{testprovider.OldBy(StripeTolerance + time.Second)}},
		{"from the future", []testprovider.SignOption{testprovider.SignedAt(r.clock.Now().Add(StripeTolerance + time.Second))}},
		{"a secret of the other kind", []testprovider.SignOption{testprovider.WithSecret("whsec_" + strings.Repeat("Nope", 6))}},
	}
	for _, c := range cases {
		d, err := r.send.Send(context.Background(), ev, c.opts...)
		if err != nil {
			t.Fatal(err)
		}
		isNotFound(t, c.name, d.Status, d.Response)
		if !bytes.Equal(d.Response, want.Response) || d.Header.Get("Content-Type") != want.Header.Get("Content-Type") ||
			d.Header.Get("Cache-Control") != want.Header.Get("Cache-Control") {
			t.Errorf("%s: the answer %q %v differs from an unknown id's %q %v", c.name, d.Response, d.Header, want.Response, want.Header)
		}
	}
	if n := r.total(); n != 0 {
		t.Fatalf("%d events stored from refused deliveries", n)
	}
	if r.lastEvent(r.in.ID).Valid {
		t.Error("a refused delivery moved last_event_at")
	}

	for name, opt := range map[string]testprovider.SignOption{
		"debug entries":               testprovider.WithDebugEntries(),
		"a second v1 of a rolled one": testprovider.AlsoSignedWith(testprovider.OtherWebhookSecret),
		"just inside the tolerance":   testprovider.OldBy(StripeTolerance),
		"just inside, in the future":  testprovider.SignedAt(r.clock.Now().Add(StripeTolerance)),
	} {
		d, err := r.send.Send(context.Background(), ev, opt)
		if err != nil || d.Status != http.StatusOK {
			t.Errorf("%s = %d, %v; want 200", name, d.Status, err)
		}
	}
}

func TestEveryNotFoundIsOneAnswer(t *testing.T) {
	r := newRig(t)
	s, _ := r.session(2500, "eur")
	r.must(r.fake.Pay(s.ID))
	ev := r.completed()

	noSecret := stripeBody("nosecret")
	noSecret["secrets"] = map[string]string{"api_key": stripeTestKey}
	_, _, noSecretSender := r.integration(noSecret, stripeHook)

	sq := squareBody("shop2")
	squareHook := "sqhook" + strings.Repeat("Fake", 6)
	sq["secrets"] = map[string]string{"api_key": squareToken, "webhook_secret": squareHook}
	_, _, squareSender := r.integration(sq, squareHook)

	disabled, _, disabledSender := r.integration(stripeBody("off"), hookFor("off"))
	expect(t, r.owner.do(http.MethodPatch, wsPath(r.ws, fmt.Sprintf("integrations/%d", disabled.ID)), map[string]any{"disabled": true}), http.StatusOK)

	broken, _, brokenSender := r.integration(stripeBody("broken"), hookFor("broken"))
	if _, err := r.db.Exec(`UPDATE integration_secrets SET sealed = x'00' WHERE integration_id = ? AND name = 'webhook_secret'`, broken.ID); err != nil {
		t.Fatal(err)
	}

	senders := map[string]*testprovider.Sender{
		"unknown":                      r.sender(token(), stripeHook),
		"uppercase id":                 r.sender(strings.ToUpper(r.id), stripeHook),
		"short id":                     r.sender(r.id[:31], stripeHook),
		"long id":                      r.sender(r.id+"0", stripeHook),
		"not hex":                      r.sender(strings.Repeat("z", 32), stripeHook),
		"no webhook secret":            noSecretSender,
		"another provider's delivery":  squareSender,
		"disabled":                     disabledSender,
		"secrets that can't be opened": brokenSender,
		"bad signature":                r.sender(r.id, testprovider.OtherWebhookSecret),
	}
	var ref *testprovider.Delivery
	for name, snd := range senders {
		d, err := snd.Send(context.Background(), ev)
		if err != nil {
			t.Fatal(err)
		}
		isNotFound(t, name, d.Status, d.Response)
		if ref == nil {
			ref = &d
		} else if !bytes.Equal(d.Response, ref.Response) || d.Header.Get("Content-Type") != ref.Header.Get("Content-Type") {
			t.Errorf("%s: answered %q, another reason answered %q", name, d.Response, ref.Response)
		}
	}
	if n := r.total(); n != 0 {
		t.Fatalf("%d events stored from refused deliveries", n)
	}
}

func TestASignatureOfOneIntegrationOpensNoOther(t *testing.T) {
	r := newRig(t)
	other := stripeBody("other")
	other["secrets"] = map[string]string{"api_key": stripeTestKey, "webhook_secret": testprovider.OtherWebhookSecret}
	otherIn, otherID, otherSender := r.integration(other, testprovider.OtherWebhookSecret)

	_, ws2 := r.otherWorkspace(r.owner)
	third := stripeBody("third")
	third["secrets"] = map[string]string{"api_key": stripeTestKey, "webhook_secret": "whsec_" + strings.Repeat("Thrd", 6)}
	in3 := r.create(r.owner, ws2, third)
	id3 := strings.TrimPrefix(*in3.WebhookURL, hookOrigin+HookPrefix)

	s, _ := r.session(3000, "eur")
	r.must(r.fake.Pay(s.ID))
	ev := r.completed()

	for name, id := range map[string]string{"the same workspace's": otherID, "another workspace's": id3} {
		d, err := r.sender(id, stripeHook).Send(context.Background(), ev)
		if err != nil {
			t.Fatal(err)
		}
		isNotFound(t, "a signature of the first against "+name, d.Status, d.Response)
	}
	if n := r.total(); n != 0 {
		t.Fatalf("%d events stored by a signature used against another integration", n)
	}
	d, err := r.send.Send(context.Background(), ev, testprovider.WithSecret(testprovider.OtherWebhookSecret))
	if err != nil {
		t.Fatal(err)
	}
	isNotFound(t, "the second's secret against the first", d.Status, d.Response)

	for _, snd := range []*testprovider.Sender{r.send, otherSender} {
		if d, err := snd.Send(context.Background(), ev); err != nil || d.Status != http.StatusOK {
			t.Fatalf("own secret: %d, %v", d.Status, err)
		}
	}
	if len(r.rows(r.in.ID)) != 1 || len(r.rows(otherIn.ID)) != 1 || len(r.rows(in3.ID)) != 0 {
		t.Fatalf("rows: %d, %d, %d; want 1, 1, 0", len(r.rows(r.in.ID)), len(r.rows(otherIn.ID)), len(r.rows(in3.ID)))
	}
}

func TestARotatedSecretOverlaps(t *testing.T) {
	r := newRig(t)
	s, _ := r.session(3000, "eur")
	r.must(r.fake.Pay(s.ID))
	ev := r.completed()
	expect(t, r.owner.do(http.MethodPatch, wsPath(r.ws, fmt.Sprintf("integrations/%d", r.in.ID)),
		map[string]any{"secrets": map[string]string{"webhook_secret": stripeHookNew}}), http.StatusOK)

	send := func(secret string) int {
		d, err := r.send.Send(context.Background(), ev, testprovider.WithSecret(secret))
		if err != nil {
			t.Fatal(err)
		}
		return d.Status
	}
	if got := send(stripeHookNew); got != http.StatusOK {
		t.Errorf("the new secret = %d", got)
	}
	if got := send(stripeHook); got != http.StatusOK {
		t.Errorf("the replaced secret inside the overlap = %d", got)
	}
	if d, err := r.send.Send(context.Background(), ev, testprovider.WithSecret(stripeHookNew), testprovider.AlsoSignedWith(stripeHook)); err != nil || d.Status != http.StatusOK {
		t.Errorf("signed by both = %d, %v", d.Status, err)
	}
	r.clock.Advance(previousOverlap + time.Minute)
	if got := send(stripeHook); got != http.StatusNotFound {
		t.Errorf("the replaced secret after the overlap = %d, want 404", got)
	}
	if got := send(stripeHookNew); got != http.StatusOK {
		t.Errorf("the new secret after the overlap = %d", got)
	}
}

func TestTheBodyIsBounded(t *testing.T) {
	r := newRig(t)
	big := bytes.Repeat([]byte("x"), maxHookBody+1)
	unknown := token()

	for name, path := range map[string]string{"known": HookPrefix + r.id, "unknown": HookPrefix + unknown} {
		rec := r.post(path, big, nil, "")
		if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), `"webhook_too_large"`) {
			t.Errorf("%s id, announced length: %d %s", name, rec.Code, rec.Body.String())
		}
		req := httptest.NewRequest(http.MethodPost, path, io.NopCloser(bytes.NewReader(big)))
		req.ContentLength = -1
		rec = httptest.NewRecorder()
		r.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s id, unknown length: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	atCap := bytes.Repeat([]byte("x"), maxHookBody)
	d, err := r.send.SendBody(context.Background(), "evt_big", "big", atCap)
	if err != nil || d.Status != http.StatusOK {
		t.Fatalf("a body at the cap = %d, %v", d.Status, err)
	}
	if n := r.total(); n != 0 {
		t.Fatalf("%d events stored from a body that isn't an event", n)
	}
}

func TestTheRouteIsLimited(t *testing.T) {
	r := newRig(t)
	from := func(i int) string { return fmt.Sprintf("198.51.%d.%d:4000", i/250, i%250+1) }

	var last *httptest.ResponseRecorder
	for i := range hookIPAttempts + 1 {
		last = r.post(HookPrefix+token(), []byte("{}"), nil, "203.0.113.9:4000")
		if i < hookIPAttempts && last.Code != http.StatusNotFound {
			t.Fatalf("attempt %d from one address = %d", i, last.Code)
		}
	}
	if last.Code != http.StatusTooManyRequests || last.Header().Get("Retry-After") == "" || !strings.Contains(last.Body.String(), `"webhook_rate_limited"`) {
		t.Fatalf("over the address's limit: %d %v %s", last.Code, last.Header(), last.Body.String())
	}
	if rec := r.post(HookPrefix+token(), []byte("{}"), nil, "203.0.113.10:4000"); rec.Code != http.StatusNotFound {
		t.Errorf("another address = %d", rec.Code)
	}
	r.clock.Advance(hookWindow)
	if rec := r.post(HookPrefix+token(), []byte("{}"), nil, "203.0.113.9:4000"); rec.Code != http.StatusNotFound {
		t.Errorf("after the window = %d", rec.Code)
	}

	for name, id := range map[string]string{"known": r.id, "unknown": token()} {
		for i := range hookIDAttempts + 1 {
			if rec := r.post(HookPrefix+id, []byte("{}"), nil, from(i)); rec.Code != http.StatusNotFound {
				t.Fatalf("%s id, unsigned attempt %d = %d, want 404", name, i, rec.Code)
			}
		}
	}
	r.clock.Advance(hookWindow)
	r.must(r.fake.Pay(func() string { s, _ := r.session(1000, "eur"); return s.ID }()))
	ev := r.completed()
	signed := http.Header{StripeSignatureHeader: {testprovider.Sign(stripeHook, r.clock.Now(), ev.Body())}}
	for i := range hookIDAttempts + 1 {
		want := http.StatusOK
		if i == hookIDAttempts {
			want = http.StatusTooManyRequests
		}
		if rec := r.post(HookPrefix+r.id, ev.Body(), signed, from(i)); rec.Code != want {
			t.Fatalf("signed delivery %d = %d, want %d", i, rec.Code, want)
		}
	}
	r.clock.Advance(hookWindow)
	for i := range hookIPAttempts {
		rec := r.post(HookPrefix+token(), []byte("{}"), nil, fmt.Sprintf("[2001:db8:1:2::%x]:4000", i+1))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("v6 attempt %d = %d", i, rec.Code)
		}
	}
	if rec := r.post(HookPrefix+token(), []byte("{}"), nil, "[2001:db8:1:2:ffff::1]:4000"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("another address of the same /64 = %d, want 429", rec.Code)
	}
}

func TestTheRouteIgnoresSessionsAndCookies(t *testing.T) {
	r := newRig(t)
	s, _ := r.session(2500, "eur")
	r.must(r.fake.Pay(s.ID))
	ev := r.completed()
	body := ev.Body()
	signed := http.Header{testprovider.SignatureHeader: {testprovider.Sign(stripeHook, r.clock.Now(), body)}}

	adminCookie := http.Header{"Cookie": {r.owner.cookie.Name + "=" + r.owner.cookie.Value}, "Sec-Fetch-Site": {"same-origin"}}
	with := r.post(HookPrefix+r.id, body, adminCookie, "")
	without := r.post(HookPrefix+r.id, body, nil, "")
	isNotFound(t, "a session and no signature", with.Code, with.Body.Bytes())
	if with.Body.String() != without.Body.String() {
		t.Errorf("the answer changed with a session: %q against %q", with.Body.String(), without.Body.String())
	}
	if n := r.total(); n != 0 {
		t.Fatalf("a session stored %d events", n)
	}

	for name, h := range map[string]http.Header{
		"no cookie":        {},
		"a garbage cookie": {"Cookie": {"taskto_session=garbage"}},
		"another origin":   {"Origin": {"https://elsewhere.example"}, "Sec-Fetch-Site": {"cross-site"}},
		"plain text":       {"Content-Type": {"text/plain"}},
	} {
		h = h.Clone()
		h.Set(testprovider.SignatureHeader, signed.Get(testprovider.SignatureHeader))
		rec := r.post(HookPrefix+r.id, body, h, "")
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Values("Set-Cookie"); len(got) != 0 {
			t.Errorf("%s set cookies %v", name, got)
		}
	}
	if n := len(r.rows(r.in.ID)); n != 1 {
		t.Fatalf("%d events, want the one", n)
	}
	if rec := r.post("/api/workspaces/1/integrations", body, signed, ""); rec.Code == http.StatusOK {
		t.Errorf("a signed body on an API route = %d", rec.Code)
	}
}

func TestNothingPrivateIsKept(t *testing.T) {
	r := newRig(t)
	s, _ := r.session(2500, "eur")
	evs := r.must(r.fake.Pay(s.ID))
	ds := r.deliver(r.send, evs)
	r.deliver(r.send, evs, testprovider.WrongSecret())
	r.deliver(r.send, evs, testprovider.TamperedBody())
	r.deliver(r.sender(token(), stripeHook), evs)

	var dump strings.Builder
	rs, err := r.db.Query(`SELECT * FROM integration_events`)
	if err != nil {
		t.Fatal(err)
	}
	cols, _ := rs.Columns()
	for rs.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(&dump, vals...)
	}
	_ = rs.Close()
	for _, col := range cols {
		for _, bad := range []string{"body", "payload", "raw", "json", "email", "name"} {
			if strings.Contains(col, bad) && col != "body_sha256" && col != "provider_event_id" {
				t.Errorf("integration_events has a column %q: nothing of the body is kept", col)
			}
		}
	}
	answers := new(strings.Builder)
	for _, d := range ds {
		answers.Write(d.Response)
	}
	headers := []string{}
	for _, d := range r.send.Deliveries() {
		headers = append(headers, d.Signature)
	}
	haystacks := map[string]string{"the table": dump.String(), "the log": r.logs.String(), "the answers": answers.String() + r.bodies.String()}
	for name, hay := range haystacks {
		for _, secret := range []string{stripeHook, stripeHookNew, stripeTestKey, r.fake.Payer().Email, r.fake.Payer().Name} {
			if strings.Contains(hay, secret) {
				t.Errorf("%s holds %q", name, secret)
			}
		}
		for _, sig := range headers {
			for _, entry := range strings.Split(sig, ",") {
				if _, v, ok := strings.Cut(entry, "="); ok && len(v) >= 32 && strings.Contains(hay, v) {
					t.Errorf("%s holds a signature", name)
				}
			}
		}
		if strings.Contains(hay, string(evs[len(evs)-1].Body())) {
			t.Errorf("%s holds a body", name)
		}
	}
}

func TestAFailureToStoreIsRetried(t *testing.T) {
	r := newRig(t)
	s, _ := r.session(2500, "eur")
	evs := r.must(r.fake.Pay(s.ID))
	if _, err := r.db.Exec(`DROP TABLE integration_events`); err != nil {
		t.Fatal(err)
	}
	var sawFailure bool
	for _, d := range r.deliver(r.send, evs) {
		if d.Status == http.StatusInternalServerError {
			sawFailure = true
			if strings.Contains(string(d.Response), "integration_events") {
				t.Errorf("the answer names a table: %s", d.Response)
			}
		}
	}
	if !sawFailure {
		t.Fatal("no delivery answered 500 when the event couldn't be stored")
	}
}

func TestAnotherModesEventIsStored(t *testing.T) {
	r := newRig(t)
	ev := testprovider.NewEvent("checkout.session.completed", map[string]any{
		"id": "cs_live_abc123", "object": "checkout.session", "mode": "payment", "payment_status": "paid",
		"client_reference_id": token(), "amount_total": 1000, "currency": "eur",
	}, r.clock.Now())
	ev.Livemode = true
	d, err := r.send.Send(context.Background(), ev)
	if err != nil || d.Status != http.StatusOK {
		t.Fatalf("= %d, %v", d.Status, err)
	}
	if n := r.total(); n != 1 {
		t.Fatalf("%d events stored: the live-mode event was dropped", n)
	}
}

func TestTheHookRouteIsDocumented(t *testing.T) {
	read := func(path string) string {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	api := read("../../docs/api.md")
	if !strings.Contains(api, "### `POST "+HookPrefix+"{public_id}`") {
		t.Error("docs/api.md has no section for POST /hooks/{public_id}")
	}
	for _, code := range []string{"webhook_rate_limited", "webhook_too_large", "integration_event_gave_up"} {
		if !strings.Contains(api, "`"+code+"`") {
			t.Errorf("docs/api.md doesn't list %q", code)
		}
	}
	install := read("../../docs/install.md")
	if !strings.Contains(install, "### Webhooks from payment providers") || !strings.Contains(install, "POST /hooks/*") {
		t.Error("docs/install.md has no note on forwarding POST /hooks/*")
	}
	if !strings.Contains(read("../../docs/integrations.md"), "### What the adapter was written against (Stripe)") {
		t.Error("docs/integrations.md doesn't pin what the Stripe adapter was written against")
	}
}
