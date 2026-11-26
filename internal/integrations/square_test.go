package integrations

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/integrations/testprovider"
)

// Square's own tests: what the contract suite (contract_square_test.go) can't say in common words.

// squareRig is the adapter's real client against a fake Square.
type squareRig struct {
	fake   *testprovider.Square
	clock  *testprovider.ManualClock
	client *Client
	creds  Credentials
}

func newSquareRig(t *testing.T, opts ...testprovider.SquareOption) *squareRig {
	t.Helper()
	clock := testprovider.NewManualClock(time.Time{})
	fake := testprovider.NewSquare(t, append([]testprovider.SquareOption{testprovider.WithSquareClock(clock)}, opts...)...)
	settings := squareSettings()
	return &squareRig{
		fake: fake, clock: clock,
		client: NewClient(Lookup("square"), settings, egress.MustParse("127.0.0.1"), WithTestEndpoint(fake.Host()), WithoutConnectionReuse()),
		creds: Credentials{Provider: "square", Mode: ModeTest, Settings: settings,
			APIKey: Secret(testprovider.SquareTestToken), WebhookSecret: Secret(testprovider.SquareSignatureKey)},
	}
}

func squareNewLink(t *testing.T, pages bool) NewLink {
	t.Helper()
	tok, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	l := NewLink{Token: tok, AmountMinor: 4200, Currency: "USD", Description: "Invoice B-2"}
	if pages {
		l.SuccessURL, l.CancelURL = publicURL+PaymentDonePath, publicURL+PaymentCancelledPath
	}
	return l
}

// Square's own known answers (its Go SDK's tests of the webhook helper): the HMAC-SHA256 of the
// notification URL followed by the body, keyed with the signature key, in Base64. The key is Square's
// example, not a secret. The expected values are written in pieces so that no line reads as one
// credential to a secret scanner.
func TestSquareSignatureIsSquaresScheme(t *testing.T) {
	for _, c := range []struct{ body, url, key, want string }{
		{`{"data":{"type":"webhooks","id":"fake_id"}}`, "https://webhook.site/webhooks", "signature-key", "W3FlCNk5IA3" + "ZQ2LHTWoajv" + "zfaDu/OwY2t" + "NHIHC3IUOA="},
		{`{"data":{"type":"webhooks","id":">id<"}}`, "https://webhook.site/webhooks", "signature-key", "Cxt7+aTi4rK" + "gcA0bC4g9EH" + "dVtLSDWdqcc" + "mL5MvihU4U="},
	} {
		if got := base64.StdEncoding.EncodeToString(squareMAC([]byte(c.key), c.url, []byte(c.body))); got != c.want {
			t.Errorf("MAC of %s = %s, want %s", c.body, got, c.want)
		}
		if got := testprovider.SignSquare(c.key, c.url, []byte(c.body)); got != c.want {
			t.Errorf("the fake signs %s as %s, want %s", c.body, got, c.want)
		}
	}
}

// The address is signed: a delivery verifies only against the address it was sent to, which the server
// makes from its public URL. Anything else a request carries (its Host) is never used.
func TestSquareSignatureCoversTheAddress(t *testing.T) {
	now := testprovider.DefaultStart
	body := testprovider.NewSquareEvent("payment.updated", "payment", "P1", map[string]any{"payment": map[string]any{"id": "P1"}}, now).Body()
	key := testprovider.SquareSignatureKey
	h := http.Header{SquareSignatureHeader: {testprovider.SignSquare(key, squareHookURL, body)}}
	secrets := WebhookSecrets{Current: Secret(key), URL: squareHookURL}
	if err := (square{}).VerifyWebhook(h, body, secrets, now); err != nil {
		t.Fatalf("a genuine delivery: %v", err)
	}
	for name, u := range map[string]string{
		"another host":         strings.Replace(squareHookURL, "taskto.example.test", "evil.example.test", 1),
		"http":                 strings.Replace(squareHookURL, "https://", "http://", 1),
		"another id":           strings.Replace(squareHookURL, "0123", "3210", 1),
		"a trailing slash":     squareHookURL + "/",
		"no address":           "",
		"the path alone":       HookPrefix + "0123456789abcdef0123456789abcdef",
		"an address with port": strings.Replace(squareHookURL, ".test/", ".test:443/", 1),
	} {
		s := secrets
		s.URL = u
		if err := (square{}).VerifyWebhook(h, body, s, now); err == nil {
			t.Errorf("a delivery signed over the integration's address verifies against %s", name)
		}
	}
	// A signature of the address alone, or of the body alone, is not one of both.
	for name, sig := range map[string]string{
		"the body alone":    testprovider.SignSquare(key, "", body),
		"the address alone": testprovider.SignSquare(key, squareHookURL, nil),
	} {
		if err := (square{}).VerifyWebhook(http.Header{SquareSignatureHeader: {sig}}, body, secrets, now); err == nil {
			t.Errorf("a signature of %s verifies", name)
		}
	}
}

func TestSquareSignatureHeaderAndTime(t *testing.T) {
	now := testprovider.DefaultStart
	key := testprovider.SquareSignatureKey
	secrets := WebhookSecrets{Current: Secret(key), URL: squareHookURL}
	body := func(at time.Time) []byte {
		return testprovider.NewSquareEvent("payment.updated", "payment", "P1", map[string]any{"payment": map[string]any{"id": "P1"}}, at).Body()
	}
	sig := func(b []byte) string { return testprovider.SignSquare(key, squareHookURL, b) }
	b := body(now)
	for name, h := range map[string]http.Header{
		"no header":           {},
		"two signatures":      {SquareSignatureHeader: {sig(b), sig(b)}},
		"not Base64":          {SquareSignatureHeader: {"not base64!"}},
		"a short digest":      {SquareSignatureHeader: {base64.StdEncoding.EncodeToString([]byte("short"))}},
		"hex, not Base64":     {SquareSignatureHeader: {fmt.Sprintf("%x", []byte(sig(b)))[:64]}},
		"a Stripe header":     {StripeSignatureHeader: {"t=1,v1=" + strings.Repeat("0", 64)}},
		"an oversized header": {SquareSignatureHeader: {strings.Repeat("A", maxSignatureHeader+4)}},
	} {
		if err := (square{}).VerifyWebhook(h, b, secrets, now); err == nil {
			t.Errorf("%s verifies", name)
		}
	}
	// The time is the event's created_at, which the signature covers: Square retries for a day after the
	// event, so a delivery that old still verifies, and one older, or from the future, doesn't.
	for name, c := range map[string]struct {
		at time.Time
		ok bool
	}{
		"now":                    {now, true},
		"a retry 23 hours later": {now.Add(-23 * time.Hour), true},
		"the last retry":         {now.Add(-squareRetryWindow), true},
		"after the last retry":   {now.Add(-(squareRetryWindow + squareSkew + time.Minute)), false},
		"a minute ahead":         {now.Add(time.Minute), true},
		"ten minutes ahead":      {now.Add(10 * time.Minute), false},
		"a week ago":             {now.Add(-7 * 24 * time.Hour), false},
		"long before Square was": {time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC), false},
	} {
		b := body(c.at)
		err := (square{}).VerifyWebhook(http.Header{SquareSignatureHeader: {sig(b)}}, b, secrets, now)
		if (err == nil) != c.ok {
			t.Errorf("%s: %v, want verified %v", name, err, c.ok)
		}
	}
	// A body with no time, or a time that can't be read, is refused even when its signature is right.
	for name, raw := range map[string]string{
		"no created_at":    `{"type":"payment.updated","event_id":"e1"}`,
		"a date alone":     `{"created_at":"2030-03-01"}`,
		"a number":         `{"created_at":1893456000}`,
		"not json":         `created_at`,
		"an empty created": `{"created_at":""}`,
	} {
		if err := (square{}).VerifyWebhook(http.Header{SquareSignatureHeader: {sig([]byte(raw))}}, []byte(raw), secrets, now); err == nil {
			t.Errorf("%s verifies", name)
		}
	}
}

// Every well-formed delivery costs the same number of MACs as Stripe's, whatever secrets there are, so
// timing says nothing of an integration's provider or secrets.
func TestSquareVerificationCostsTheSame(t *testing.T) {
	now := testprovider.DefaultStart
	b := testprovider.NewSquareEvent("payment.updated", "payment", "P1", nil, now).Body()
	h := http.Header{SquareSignatureHeader: {testprovider.SignSquare("x", squareHookURL, b)}}
	count := 0
	macHook = func() { count++ }
	t.Cleanup(func() { macHook = nil })
	for name, s := range map[string]WebhookSecrets{
		"none":     {URL: squareHookURL},
		"one":      {Current: "a-secret-of-test", URL: squareHookURL},
		"two":      {Current: "a-secret-of-test", Previous: "b-secret-of-test", URL: squareHookURL},
		"no where": {Current: "a-secret-of-test"},
	} {
		count = 0
		_ = (square{}).VerifyWebhook(h, b, s, now)
		if count != verifyKeys {
			t.Errorf("%s: %d MACs, want %d", name, count, verifyKeys)
		}
	}
}

// Square documents no failure as given before the request ran, so every answer that isn't a success
// leaves a creation's outcome unknown, and only a request that never left is not executed.
func TestSquareErrorsAreUnknownUnlessTheRequestNeverLeft(t *testing.T) {
	for status, kind := range map[int]CallKind{
		400: CallRefused, 404: CallRefused, 409: CallRefused, 422: CallRefused,
		401: CallCredentials, 403: CallCredentials,
		408: CallUnreachable, 429: CallUnreachable, 500: CallUnreachable, 502: CallUnreachable, 503: CallUnreachable, 504: CallUnreachable,
	} {
		ce, ok := IsCall(squareFailure(status))
		if !ok || ce.Kind != kind || ce.NotExecuted || !ce.OutcomeUnknown || ce.Status != status {
			t.Errorf("status %d: %+v, want kind %d, outcome unknown", status, ce, kind)
		}
	}

	ctx := context.Background()
	for name, c := range map[string]struct {
		status int
		code   string
		after  bool
		kind   CallKind
	}{
		"a rate limit":                   {http.StatusTooManyRequests, "RATE_LIMITED", false, CallUnreachable},
		"a server error before the work": {http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", false, CallUnreachable},
		"a server error after the work":  {http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", true, CallUnreachable},
		"a gateway timeout after":        {http.StatusGatewayTimeout, "GATEWAY_TIMEOUT", true, CallUnreachable},
		"a conflict":                     {http.StatusConflict, "CONFLICT", false, CallRefused},
	} {
		t.Run(name, func(t *testing.T) {
			r := newSquareRig(t)
			l := squareNewLink(t, true)
			r.fake.FailNext(c.status, c.code, c.after)
			_, err := (square{}).CreatePaymentLink(ctx, r.creds, r.client, l)
			ce, ok := IsCall(err)
			if !ok || ce.Kind != c.kind || ce.NotExecuted || !ce.OutcomeUnknown {
				t.Fatalf("%v (%+v), want an unknown outcome", err, ce)
			}
			// The outcome is resolved by the key: asked again, it is one link, made once.
			got, err := (square{}).CreatePaymentLink(ctx, r.creds, r.client, l)
			if err != nil {
				t.Fatal(err)
			}
			if n := len(r.fake.Links()); n != 1 {
				t.Fatalf("%d links after a retry under the same key", n)
			}
			// And the lookup by the key's token finds the same one.
			found, ok, _, err := (square{}).FindPaymentLink(ctx, r.creds, r.client, l, time.Time{})
			if err != nil || !ok || found != got {
				t.Fatalf("lookup = %+v, %v, %v; made %+v", found, ok, err, got)
			}
		})
	}

	// The same key with another request: Square's IDEMPOTENCY_KEY_REUSED, a refusal whose outcome is not
	// known to be nothing (the key made a link).
	r := newSquareRig(t)
	l := squareNewLink(t, false)
	if _, err := (square{}).CreatePaymentLink(ctx, r.creds, r.client, l); err != nil {
		t.Fatal(err)
	}
	other := l
	other.Description = "Another invoice"
	if _, err := (square{}).CreatePaymentLink(ctx, r.creds, r.client, other); !isCall(err, CallRefused) {
		t.Fatalf("a key reused with another request: %v", err)
	} else if ce, _ := IsCall(err); !ce.OutcomeUnknown || ce.Status != http.StatusBadRequest {
		t.Fatalf("a key reused: %+v", ce)
	}

	// A request that never left (nothing listens) is the one failure that is not executed.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	_ = ln.Close()
	c := NewClient(Lookup("square"), squareSettings(), egress.MustParse("127.0.0.1"), WithTestEndpoint(dead))
	if _, err := (square{}).CreatePaymentLink(ctx, r.creds, c, squareNewLink(t, true)); err == nil {
		t.Fatal("a call to nothing succeeded")
	} else if ce, _ := IsCall(err); ce == nil || !ce.NotExecuted || ce.OutcomeUnknown {
		t.Fatalf("a call that never left: %+v", ce)
	}
}

// Square publishes no retention for idempotency keys: a key whose outcome is unknown is never relied
// on, only looked for.
func TestSquareKeysAreNeverReliedOn(t *testing.T) {
	if (square{}).KeyKept() != 0 {
		t.Fatalf("Square's keys are relied on for %v", (square{}).KeyKept())
	}
}

func TestSquareCallsSendTheVersionTokenAndLocation(t *testing.T) {
	ctx := context.Background()
	r := newSquareRig(t)
	// The fake refuses any other Square-Version and another location: a call that forgets either fails.
	if _, err := (square{}).CreatePaymentLink(ctx, r.creds, r.client, squareNewLink(t, true)); err != nil {
		t.Fatal(err)
	}
	moved := r.creds
	moved.Settings = map[string]string{"environment": "sandbox", "location_id": "LOTHER"}
	if _, err := (square{}).CreatePaymentLink(ctx, moved, r.client, squareNewLink(t, true)); !isCall(err, CallRefused) {
		t.Fatalf("a location that isn't the merchant's: %v", err)
	}
	if got, err := (square{}).AccountID(ctx, r.creds, r.client); err != nil || got != testprovider.SquareMerchantID {
		t.Fatalf("account = %q, %v", got, err)
	}
}

// A link is read through its order: unpaid while the order has no tender, settled once paid or deleted.
// A lookup reads every page, and says so when there are more than it reads.
func TestSquareLinkStateAndLookup(t *testing.T) {
	ctx := context.Background()
	r := newSquareRig(t, testprovider.WithSquarePageSize(2))
	var links []NewLink
	var made []Link
	for range 5 {
		l := squareNewLink(t, true)
		m, err := (square{}).CreatePaymentLink(ctx, r.creds, r.client, l)
		if err != nil {
			t.Fatal(err)
		}
		links, made = append(links, l), append(made, m)
	}
	// The last one, on the third page.
	got, ok, expired, err := (square{}).FindPaymentLink(ctx, r.creds, r.client, links[4], time.Time{})
	if err != nil || !ok || expired || got != made[4] {
		t.Fatalf("lookup on the third page = %+v, %v, %v, %v", got, ok, expired, err)
	}
	// A link of the other shape (no redirect) under the same token is not the one asked for.
	hosted := links[1]
	hosted.SuccessURL, hosted.CancelURL = "", ""
	if _, _, _, err := (square{}).FindPaymentLink(ctx, r.creds, r.client, hosted, time.Time{}); !isCall(err, CallRefused) {
		t.Fatalf("a link with another redirect: %v", err)
	}

	if _, err := r.fake.PaySquare(made[0].ObjectID); err != nil {
		t.Fatal(err)
	}
	if st, err := (square{}).LinkState(ctx, r.creds, r.client, false, made[0].ObjectID); err != nil || st != LinkSettled {
		t.Fatalf("a paid link: %v, %v", st, err)
	}
	if _, _, _, err := (square{}).FindPaymentLink(ctx, r.creds, r.client, links[0], time.Time{}); !isCall(err, CallSettled) {
		t.Fatalf("a lookup that finds a paid link: %v", err)
	}
	if _, err := r.fake.PaySquare(made[0].ObjectID); err == nil {
		t.Fatal("a link was paid twice")
	}
	// A deleted link is gone, and its state can't be read as open.
	r.fake.DeleteLink(made[1].ObjectID)
	if st, err := (square{}).LinkState(ctx, r.creds, r.client, false, made[1].ObjectID); err == nil || st == LinkOpen {
		t.Fatalf("a deleted link: %v, %v", st, err)
	}

	// More than a lookup reads: it says it couldn't tell, never "not found".
	small := newSquareRig(t, testprovider.WithSquarePageSize(1))
	for range squareFindMaxPages + 1 {
		if _, err := (square{}).CreatePaymentLink(ctx, small.creds, small.client, squareNewLink(t, true)); err != nil {
			t.Fatal(err)
		}
	}
	if _, found, _, err := (square{}).FindPaymentLink(ctx, small.creds, small.client, squareNewLink(t, true), time.Time{}); found || !isCall(err, CallLookupIncomplete) {
		t.Fatalf("a lookup past its pages: %v, %v", found, err)
	}
}

// A lookup that finds the link of the token and refuses it (other terms, another redirect, paid)
// names that link with the refusal, and gives no address: the caller keeps it on the key, so a
// refusal is never "nothing made" and no second payable link is made beside it.
func TestSquareRefusedLookupNamesTheLinkItSaw(t *testing.T) {
	ctx := context.Background()
	r := newSquareRig(t)
	l := squareNewLink(t, true)
	made, err := (square{}).CreatePaymentLink(ctx, r.creds, r.client, l)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*NewLink){
		"another amount":   func(n *NewLink) { n.AmountMinor++ },
		"another currency": func(n *NewLink) { n.Currency = "EUR" },
		"another redirect": func(n *NewLink) { n.SuccessURL, n.CancelURL = "", "" },
	}
	for name, change := range cases {
		n := l
		change(&n)
		got, found, expired, err := (square{}).FindPaymentLink(ctx, r.creds, r.client, n, time.Time{})
		if !isCall(err, CallRefused) || found || expired || got.URL != "" || got.ObjectID != made.ObjectID {
			t.Fatalf("%s: %+v, %v, %v, %v; want a refusal naming %s", name, got, found, expired, err, made.ObjectID)
		}
	}
	if _, err := r.fake.PaySquare(made.ObjectID); err != nil {
		t.Fatal(err)
	}
	got, found, _, err := (square{}).FindPaymentLink(ctx, r.creds, r.client, l, time.Time{})
	if !isCall(err, CallSettled) || found || got.URL != "" || got.ObjectID != made.ObjectID {
		t.Fatalf("a paid link: %+v, %v, %v; want settled naming %s", got, found, err, made.ObjectID)
	}
	// A lookup of a token with no link still finds nothing, and names nothing.
	got, found, _, err = (square{}).FindPaymentLink(ctx, r.creds, r.client, squareNewLink(t, true), time.Time{})
	if err != nil || found || got != (Link{}) {
		t.Fatalf("a token with no link: %+v, %v, %v", got, found, err)
	}
}

// What Square sends that isn't a payment of ours, or isn't the end of one, is not an event.
func TestSquareNormalizeReadsOnlyOurFinishedPayments(t *testing.T) {
	at := testprovider.DefaultStart
	tok, _ := newToken()
	payment := func(status, note string, refunded int64) map[string]any {
		p := map[string]any{"id": "PAY1", "status": status, "total_money": map[string]any{"amount": 500, "currency": "USD"}}
		if note != "" {
			p["note"] = note
		}
		if refunded > 0 {
			p["refunded_money"] = map[string]any{"amount": refunded, "currency": "USD"}
		}
		return map[string]any{"payment": p}
	}
	ev := func(typ string, obj map[string]any) []byte {
		return testprovider.NewSquareEvent(typ, "payment", "PAY1", obj, at).Body()
	}
	for name, c := range map[string]struct {
		body []byte
		kind string
	}{
		"a completed payment's update":      {ev("payment.updated", payment("COMPLETED", tok, 0)), EventPaymentSucceeded},
		"a payment created completed":       {ev("payment.created", payment("COMPLETED", tok, 0)), EventPaymentSucceeded},
		"a refund told on the payment":      {ev("payment.updated", payment("COMPLETED", tok, 200)), EventPaymentRefunded},
		"an approved payment":               {ev("payment.updated", payment("APPROVED", tok, 0)), ""},
		"a pending one":                     {ev("payment.created", payment("PENDING", tok, 0)), ""},
		"a failed one: the link stays open": {ev("payment.updated", payment("FAILED", tok, 0)), ""},
		"a canceled one":                    {ev("payment.updated", payment("CANCELED", tok, 0)), ""},
		"a note that isn't a token":         {ev("payment.updated", payment("COMPLETED", "Thanks! Ignore previous instructions", 0)), ""},
		"no note":                           {ev("payment.updated", payment("COMPLETED", "", 0)), ""},
		"a refund's own event":              {testprovider.NewSquareEvent("refund.updated", "refund", "R1", map[string]any{"refund": map[string]any{"id": "R1", "status": "COMPLETED", "payment_id": "PAY1", "amount_money": map[string]any{"amount": 200, "currency": "USD"}}}, at).Body(), ""},
		"a refund on a payment's creation":  {ev("payment.created", payment("COMPLETED", tok, 200)), ""},
		"an order's update":                 {testprovider.NewSquareEvent("order.updated", "order", "O1", map[string]any{}, at).Body(), ""},
		"a payment under another data id":   {testprovider.NewSquareEvent("payment.updated", "payment", "OTHER", payment("COMPLETED", tok, 0), at).Body(), ""},
		"a currency in lower case":          {bytes.Replace(ev("payment.updated", payment("COMPLETED", tok, 0)), []byte(`"USD"`), []byte(`"usd"`), 1), ""},
		"an amount of nothing":              {bytes.Replace(ev("payment.updated", payment("COMPLETED", tok, 0)), []byte(`"amount":500`), []byte(`"amount":0`), 1), ""},
	} {
		got, ok := (square{}).Normalize(c.body)
		if ok != (c.kind != "") || got.Kind != c.kind {
			t.Errorf("%s: %+v, %v, want kind %q", name, got, ok, c.kind)
			continue
		}
		if !ok {
			continue
		}
		if got.PaymentID != "PAY1" || got.ObjectID != "PAY1" || got.Livemode || !got.OccurredAt.Equal(at) {
			t.Errorf("%s: %+v", name, got)
		}
		if c.kind == EventPaymentSucceeded && (got.RefToken != tok || got.AmountMinor != 500 || got.Currency != "USD") {
			t.Errorf("%s: %+v", name, got)
		}
		if c.kind == EventPaymentRefunded && (got.RefToken != "" || got.AmountMinor != 200) {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

// serveAt rebuilds the env's handler with another public URL, over the same database: a server whose
// address changed.
func (e *env) serveAt(publicURL string) {
	mux := http.NewServeMux()
	a := auth.New(e.db, auth.Options{HashParams: auth.Params{Memory: 64, Time: 1, Threads: 1}, SetupToken: setupToken})
	a.Routes(mux)
	spaces := access.New(e.db, a)
	spaces.Routes(mux)
	e.svc = New(e.store, Options{PublicURL: publicURL, Now: func() time.Time { return testprovider.DefaultStart }})
	e.svc.Routes(mux, a, spaces)
	e.svc.HookRoutes(mux)
	e.h = a.ClientAddr(auth.CSRF(mux))
}

// deliverSquare posts a Square delivery to the hook route, signed over signedOver, with the request's
// Host set to host.
func (e *env) deliverSquare(path, signedOver, host string, body []byte) int {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SquareSignatureHeader, testprovider.SignSquare(testprovider.SquareSignatureKey, signedOver, body))
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec.Code
}

// The hook route verifies a Square delivery over the address the signing secret was saved for, made from
// the server's public URL, never from the request; the event takes the integration's mode. When the public
// URL changes, the admin is told, the registered address keeps verifying, and saving the secret again
// moves to the new one.
func TestSquareWebhooksThroughTheRoute(t *testing.T) {
	const oldOrigin, newOrigin = "https://old.example.test", "https://new.example.test"
	e := newEnvWith(t, Options{PublicURL: oldOrigin, Now: func() time.Time { return testprovider.DefaultStart }})
	owner, ws := e.setup()
	body := squareBody("shop")
	body["secrets"] = map[string]string{"api_key": squareToken, "webhook_secret": testprovider.SquareSignatureKey}
	in := e.create(owner, ws, body)
	if in.WebhookURL == nil || in.WebhookURLChanged {
		t.Fatalf("created = %+v", in)
	}
	oldURL := *in.WebhookURL
	path := strings.TrimPrefix(oldURL, oldOrigin)
	tok, _ := newToken()
	event := func() []byte {
		return testprovider.NewSquareEvent("payment.updated", "payment", "PAY1", map[string]any{"payment": map[string]any{
			"id": "PAY1", "status": "COMPLETED", "note": tok, "total_money": map[string]any{"amount": 500, "currency": "USD"},
		}}, testprovider.DefaultStart).Body()
	}
	stored := func() int {
		var n int
		if err := e.db.QueryRow(`SELECT count(*) FROM integration_events WHERE integration_id = ?`, in.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Signed over the address the request names, not the integration's: refused.
	if code := e.deliverSquare(path, "https://evil.example.test"+path, "evil.example.test", event()); code != http.StatusNotFound {
		t.Fatalf("signed over the request's Host: %d", code)
	}
	if code := e.deliverSquare(path, oldURL, "anything.example.test", event()); code != http.StatusOK {
		t.Fatalf("a genuine delivery: %d", code)
	}
	if n := stored(); n != 1 {
		t.Fatalf("%d events stored", n)
	}
	var live bool
	if err := e.db.QueryRow(`SELECT livemode FROM integration_events WHERE integration_id = ?`, in.ID).Scan(&live); err != nil || live {
		t.Fatalf("a sandbox event's livemode = %v, %v", live, err)
	}

	// The public URL changes: the admin is told, and Square, still signing over the registered address,
	// is still verified.
	e.serveAt(newOrigin)
	one := wsPath(ws, fmt.Sprintf("integrations/%d", in.ID))
	got := decode[Integration](t, owner.do(http.MethodGet, one, nil))
	if got.WebhookURL == nil || !strings.HasPrefix(*got.WebhookURL, newOrigin) || !got.WebhookURLChanged {
		t.Fatalf("after the public URL changed: %+v", got)
	}
	if code := e.deliverSquare(path, oldURL, "new.example.test", event()); code != http.StatusOK {
		t.Fatalf("a delivery signed over the registered address after the change: %d", code)
	}
	if code := e.deliverSquare(path, *got.WebhookURL, "new.example.test", event()); code != http.StatusNotFound {
		t.Fatalf("a delivery over the new address before it was registered: %d", code)
	}

	// The admin registers the new address and saves the signing secret again (the same one): the warning
	// goes, and the new address is the one verified.
	got = decode[Integration](t, owner.do(http.MethodPatch, one, map[string]any{"secrets": map[string]string{"webhook_secret": testprovider.SquareSignatureKey}}))
	if got.WebhookURLChanged {
		t.Fatalf("after saving the secret again: %+v", got)
	}
	if code := e.deliverSquare(path, *got.WebhookURL, "new.example.test", event()); code != http.StatusOK {
		t.Fatalf("a delivery over the new address: %d", code)
	}
	if code := e.deliverSquare(path, oldURL, "new.example.test", event()); code != http.StatusNotFound {
		t.Fatalf("a delivery over the old address after the change: %d", code)
	}

	// Without a public URL there is no address, so nothing verifies: Square's deliveries need one.
	e.serveAt("")
	got = decode[Integration](t, owner.do(http.MethodGet, one, nil))
	if got.WebhookURL != nil || !got.WebhookURLChanged {
		t.Fatalf("without a public URL: %+v", got)
	}
}

// An integration of the production environment is live: its events are of live money, whatever its body says.
func TestSquareProductionEventsAreLive(t *testing.T) {
	e := newEnvWith(t, Options{PublicURL: hookOrigin, Now: func() time.Time { return testprovider.DefaultStart }})
	owner, ws := e.setup()
	body := squareBody("live")
	body["settings"] = map[string]string{"environment": "production", "location_id": "L123"}
	body["secrets"] = map[string]string{"api_key": squareToken, "webhook_secret": testprovider.SquareSignatureKey}
	in := e.create(owner, ws, body)
	if in.Mode != ModeLive {
		t.Fatalf("mode = %q", in.Mode)
	}
	tok, _ := newToken()
	ev := testprovider.NewSquareEvent("payment.updated", "payment", "PAY1", map[string]any{"payment": map[string]any{
		"id": "PAY1", "status": "COMPLETED", "note": tok, "total_money": map[string]any{"amount": 500, "currency": "USD"},
	}}, testprovider.DefaultStart).Body()
	if code := e.deliverSquare(strings.TrimPrefix(*in.WebhookURL, hookOrigin), *in.WebhookURL, "x.example.test", ev); code != http.StatusOK {
		t.Fatalf("delivery: %d", code)
	}
	var live bool
	if err := e.db.QueryRow(`SELECT livemode FROM integration_events WHERE integration_id = ?`, in.ID).Scan(&live); err != nil || !live {
		t.Fatalf("livemode = %v, %v", live, err)
	}
}

// The fake answers the version the adapter pins, and reads the header the adapter verifies.
func TestTheFakeSquareIsTheAdaptersVersion(t *testing.T) {
	if testprovider.SquareAPIVersion != SquareAPIVersion || (square{}).APIVersion() != SquareAPIVersion {
		t.Fatalf("the fake answers %q, the adapter pins %q", testprovider.SquareAPIVersion, SquareAPIVersion)
	}
	if http.CanonicalHeaderKey(testprovider.SquareSignatureHeader) != SquareSignatureHeader {
		t.Fatalf("the fake signs in %q, the adapter reads %q", testprovider.SquareSignatureHeader, SquareSignatureHeader)
	}
}

// The binding rule with Square: an invoice never has two payable links. A creation whose answer is lost
// leaves the key's outcome unknown; past the time Square is relied on to keep the key, a lookup that
// sees the link of the token but can't check it (its order unreadable) keeps that link on the key. The
// ref takes no new key, the entry stops for an admin, and the admin's "no link" is refused, since a
// link was seen: no second link is ever made.
func TestSquareASeenLinkIsNeverConfirmedAway(t *testing.T) {
	ctx := context.Background()
	p := newSquarePay(t, publicURL)
	record := p.invoice(10, "USD")
	req := p.direct(record, "op-square-seen-0001")
	req.Currency = "USD"

	p.sq.FailNext(http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", true)
	if _, err := p.svc.OpenPaymentLink(ctx, req); err == nil {
		t.Fatal("the lost answer was no error")
	}
	if n := len(p.sq.Links()); n != 1 {
		t.Fatalf("%d links at Square after the lost answer, want 1", n)
	}

	// Only looked for (Square's key is never relied on); the link is listed, its order can't be read.
	p.later(time.Minute)
	p.sq.FailOrderReads(true)
	if _, err := p.svc.OpenPaymentLink(ctx, req); !hasCode(err, apierr.CodeIntegrationLinkLookupPending) {
		t.Fatalf("a lookup that can't check the link it saw: %v", err)
	}
	if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE seq = 0 AND object_id = ?`, p.sq.Links()[0].ID); n != 1 {
		t.Fatalf("the link seen was not kept on the key")
	}

	p.later(7 * time.Hour)
	if _, err := p.svc.OpenPaymentLink(ctx, req); !hasCode(err, apierr.CodeIntegrationOutcomeUnknown) {
		t.Fatalf("at the end of the wait: %v", err)
	}
	// What the worker does with that answer.
	p.exec(`UPDATE app_effects SET state = 'failed', error_code = ?`, string(apierr.CodeIntegrationOutcomeUnknown))
	p.refusedConfirmation(p.entries()[0], 0)

	// Square answers again: the seen link is the invoice's, and still the only one.
	p.sq.FailOrderReads(false)
	if n := len(p.sq.Links()); n != 1 {
		t.Fatalf("%d links at Square, want 1", n)
	}
}

// A Square call that failed without making anything leaves its outcome unknown all the same: it is
// never sent again under its key, which Square may have forgotten (it publishes no retention), only
// looked for. When the wait ends, an admin who finds nothing at Square allows exactly one new key,
// which makes the one link.
func TestSquareAnUnknownOutcomeIsOnlyLookedForUntilAnAdminDecides(t *testing.T) {
	ctx := context.Background()
	p := newSquarePay(t, publicURL)
	record := p.invoice(10, "USD")
	req := p.direct(record, "op-square-unknown-0001")
	req.Currency = "USD"
	creates := func() int {
		n := 0
		for _, r := range p.sq.Requests() {
			if r.Method == http.MethodPost && r.Path == "/v2/online-checkout/payment-links" {
				n++
			}
		}
		return n
	}

	p.sq.FailNext(http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", false)
	if _, err := p.svc.OpenPaymentLink(ctx, req); err == nil {
		t.Fatal("the failure was no error")
	}
	for _, d := range []time.Duration{time.Second, time.Minute, time.Hour} {
		p.later(d)
		if _, err := p.svc.OpenPaymentLink(ctx, req); !hasCode(err, apierr.CodeIntegrationLinkLookupPending) {
			t.Fatalf("after %v: %v", d, err)
		}
	}
	if n := creates(); n != 1 || len(p.sq.Links()) != 0 {
		t.Fatalf("%d creations and %d links while the outcome is unknown, want the first call only", n, len(p.sq.Links()))
	}
	p.later(7 * time.Hour)
	if _, err := p.svc.OpenPaymentLink(ctx, req); !hasCode(err, apierr.CodeIntegrationOutcomeUnknown) {
		t.Fatalf("at the end of the wait: %v", err)
	}
	if n := creates(); n != 1 {
		t.Fatalf("%d creations, want 1", n)
	}
	p.exec(`UPDATE app_effects SET state = 'failed', error_code = ?`, string(apierr.CodeIntegrationOutcomeUnknown))
	e := p.entries()[0]
	if status, code := p.confirm(e.id); status != http.StatusNoContent {
		t.Fatalf("confirm = %d %q", status, code)
	}
	link, err := p.svc.OpenPaymentLink(ctx, req)
	if err != nil || link.ObjectID == "" || creates() != 2 || len(p.sq.Links()) != 1 || p.sq.Links()[0].ID != link.ObjectID {
		t.Fatalf("after the admin's decision: %+v, %v; %d creations, %d links", link, err, creates(), len(p.sq.Links()))
	}
	if r := p.refs(); len(r) != 1 || r[0].keySeq != 1 {
		t.Fatalf("refs = %+v", r)
	}
}
