package testprovider_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/integrations"
	"taskto.ai/taskto/internal/integrations/testprovider"
)

// api is a client of the fake the way the adapter will be: form bodies, a bearer key, the pinned
// version.
type api struct {
	t   *testing.T
	f   *testprovider.Stripe
	key string
	// header changes the request's headers before it is sent.
	header func(http.Header)
	client *http.Client
}

func newAPI(t *testing.T, f *testprovider.Stripe) *api {
	return &api{t: t, f: f, key: f.APIKey(), client: &http.Client{Timeout: 5 * time.Second}}
}

type answer struct {
	status int
	header http.Header
	raw    []byte
}

func (a answer) into(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(a.raw, v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, a.raw)
	}
}

func (a answer) errorCode() (typ, code string) {
	var e struct {
		Error struct{ Type, Code string }
	}
	_ = json.Unmarshal(a.raw, &e)
	return e.Error.Type, e.Error.Code
}

func (a *api) try(method, path string, form url.Values, idem string) (answer, error) {
	a.t.Helper()
	var body io.Reader
	target := a.f.URL() + path
	if method == http.MethodPost {
		body = strings.NewReader(form.Encode())
	} else if len(form) > 0 {
		target += "?" + form.Encode()
	}
	req, err := http.NewRequestWithContext(context.Background(), method, target, body)
	if err != nil {
		a.t.Fatal(err)
	}
	if a.key != "" {
		req.Header.Set("Authorization", "Bearer "+a.key)
	}
	req.Header.Set("Stripe-Version", testprovider.DefaultAPIVersion)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	if a.header != nil {
		a.header(req.Header)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return answer{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return answer{status: resp.StatusCode, header: resp.Header, raw: raw}, nil
}

func (a *api) do(method, path string, form url.Values, idem string) answer {
	a.t.Helper()
	ans, err := a.try(method, path, form, idem)
	if err != nil {
		a.t.Fatalf("%s %s: %v", method, path, err)
	}
	return ans
}

func (a *api) post(path string, form url.Values, idem string) answer {
	a.t.Helper()
	return a.do(http.MethodPost, path, form, idem)
}

func (a *api) get(path string, query url.Values) answer {
	a.t.Helper()
	return a.do(http.MethodGet, path, query, "")
}

func (a *api) newSession(amount int, currency, ref string) testprovider.Session {
	a.t.Helper()
	ans := a.post("/v1/checkout/sessions", sessionForm(amount, currency, ref), "")
	if ans.status != http.StatusOK {
		a.t.Fatalf("create: %d %s", ans.status, ans.raw)
	}
	var s testprovider.Session
	ans.into(a.t, &s)
	return s
}

func sessionForm(amount int, currency, ref string) url.Values {
	v := url.Values{}
	v.Set("mode", "payment")
	v.Set("success_url", "https://app.example.test/paid")
	v.Set("cancel_url", "https://app.example.test/cancelled")
	v.Set("client_reference_id", ref)
	v.Set("metadata[ref]", ref)
	v.Set("payment_intent_data[metadata][ref]", ref)
	v.Set("line_items[0][quantity]", "1")
	v.Set("line_items[0][price_data][currency]", currency)
	v.Set("line_items[0][price_data][unit_amount]", strconv.Itoa(amount))
	v.Set("line_items[0][price_data][product_data][name]", "Invoice 7")
	return v
}

func newFake(t *testing.T, opts ...testprovider.Option) (*testprovider.Stripe, *testprovider.ManualClock, *api) {
	t.Helper()
	clock := testprovider.NewManualClock(time.Time{})
	f := testprovider.NewStripe(t, append([]testprovider.Option{testprovider.WithClock(clock)}, opts...)...)
	return f, clock, newAPI(t, f)
}

func TestCreateAndRetrieveASession(t *testing.T) {
	f, clock, a := newFake(t)
	ans := a.post("/v1/checkout/sessions", sessionForm(12000, "eur", "ref_one"), "")
	if ans.status != http.StatusOK {
		t.Fatalf("create: %d %s", ans.status, ans.raw)
	}
	var s testprovider.Session
	ans.into(t, &s)
	if !strings.HasPrefix(s.ID, "cs_test_") || s.Object != "checkout.session" {
		t.Errorf("id %q, object %q", s.ID, s.Object)
	}
	if s.Status != "open" || s.PaymentStatus != "unpaid" || s.Mode != "payment" || s.UIMode != "hosted_page" {
		t.Errorf("states: %+v", s)
	}
	if s.AmountTotal != 12000 || s.AmountSubtotal != 12000 || s.Currency != "eur" || s.Livemode {
		t.Errorf("money: %d %s live=%v", s.AmountTotal, s.Currency, s.Livemode)
	}
	if s.ClientReferenceID == nil || *s.ClientReferenceID != "ref_one" || s.Metadata["ref"] != "ref_one" {
		t.Errorf("reference: %v %v", s.ClientReferenceID, s.Metadata)
	}
	if s.URL == nil || !strings.HasPrefix(*s.URL, "https://checkout.stripe.com/c/pay/"+s.ID) {
		t.Errorf("url: %v", s.URL)
	}
	if s.Created != clock.Now().Unix() || s.ExpiresAt != clock.Now().Add(24*time.Hour).Unix() {
		t.Errorf("times: created %d expires %d, now %d", s.Created, s.ExpiresAt, clock.Now().Unix())
	}
	if ans.header.Get("Stripe-Version") != testprovider.DefaultAPIVersion || !strings.HasPrefix(ans.header.Get("Request-Id"), "req_") {
		t.Errorf("headers: %v", ans.header)
	}
	// The raw answer has the keys Stripe's object has, including the ones that are null.
	var keys map[string]json.RawMessage
	ans.into(t, &keys)
	for _, k := range []string{"id", "object", "amount_total", "client_reference_id", "customer_details", "expires_at",
		"livemode", "metadata", "mode", "payment_intent", "payment_link", "payment_status", "status", "success_url", "url"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("answer has no %q", k)
		}
	}

	got := a.get("/v1/checkout/sessions/"+s.ID, nil)
	if got.status != http.StatusOK || string(got.raw) != string(ans.raw) {
		t.Errorf("retrieve: %d, differs from the created object", got.status)
	}
	if missing := a.get("/v1/checkout/sessions/cs_test_nope", nil); missing.status != http.StatusNotFound {
		t.Errorf("unknown session: %d", missing.status)
	} else if typ, code := missing.errorCode(); typ != "invalid_request_error" || code != "resource_missing" {
		t.Errorf("unknown session: %s %s", typ, code)
	}
	if n := len(f.Sessions()); n != 1 {
		t.Errorf("sessions = %d", n)
	}
}

func TestALiveKeyMakesLiveObjects(t *testing.T) {
	live := "sk_" + "live_" + strings.Repeat("Fake", 6)
	_, _, a := newFake(t, testprovider.WithAPIKey(live))
	s := a.newSession(5000, "usd", "r")
	if !strings.HasPrefix(s.ID, "cs_live_") || !s.Livemode {
		t.Errorf("id %q live=%v", s.ID, s.Livemode)
	}
}

func TestKeysVersionsAndBodies(t *testing.T) {
	f, _, a := newFake(t)
	form := sessionForm(5000, "usd", "r")

	a.key = ""
	if ans := a.post("/v1/checkout/sessions", form, ""); ans.status != http.StatusUnauthorized {
		t.Errorf("no key: %d", ans.status)
	}
	a.key = "sk_" + "test_" + strings.Repeat("Rota", 6)
	ans := a.post("/v1/checkout/sessions", form, "")
	if ans.status != http.StatusUnauthorized {
		t.Errorf("wrong key: %d", ans.status)
	}
	if strings.Contains(string(ans.raw), a.key) {
		t.Errorf("the refusal quotes the whole key: %s", ans.raw)
	}

	a.key = f.APIKey()
	a.header = func(h http.Header) { // basic auth: the key as the user name
		h.Del("Authorization")
		req := &http.Request{Header: h}
		req.SetBasicAuth(f.APIKey(), "")
	}
	if ans := a.post("/v1/checkout/sessions", form, ""); ans.status != http.StatusOK {
		t.Errorf("basic auth: %d %s", ans.status, ans.raw)
	}

	for name, tweak := range map[string]func(http.Header){
		"no version":    func(h http.Header) { h.Del("Stripe-Version") },
		"other version": func(h http.Header) { h.Set("Stripe-Version", "2020-08-27") },
		"a JSON body":   func(h http.Header) { h.Set("Content-Type", "application/json") },
	} {
		a.header = tweak
		if ans := a.post("/v1/checkout/sessions", form, ""); ans.status != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, ans.status, ans.raw)
		}
	}
	a.header = nil

	n := len(f.Sessions())
	var auths []string
	for _, r := range f.Requests() {
		auths = append(auths, r.Auth)
	}
	if n != 1 || strings.Join(auths, ",") != ",,basic,bearer,bearer,bearer" {
		t.Errorf("sessions %d, auth log %q", n, auths)
	}

	_, _, b := newFake(t, testprovider.WithAnyAPIVersion())
	b.header = func(h http.Header) { h.Del("Stripe-Version") }
	if ans := b.post("/v1/checkout/sessions", form, ""); ans.status != http.StatusOK {
		t.Errorf("any version: %d %s", ans.status, ans.raw)
	}
}

func TestTheFakeRefusesWhatStripeRefuses(t *testing.T) {
	_, clock, a := newFake(t)
	tests := []struct {
		name   string
		change func(url.Values)
		code   string
	}{
		{"an unknown parameter", func(v url.Values) { v.Set("success_urls", "x") }, "parameter_unknown"},
		{"a price id", func(v url.Values) {
			v.Del("line_items[0][price_data][currency]")
			v.Del("line_items[0][price_data][unit_amount]")
			v.Del("line_items[0][price_data][product_data][name]")
			v.Set("line_items[0][price]", "price_x")
		}, "resource_missing"},
		{"no mode", func(v url.Values) { v.Del("mode") }, "parameter_missing"},
		{"no line items", func(v url.Values) {
			for k := range v {
				if strings.HasPrefix(k, "line_items") {
					v.Del(k)
				}
			}
		}, "parameter_missing"},
		{"no success url", func(v url.Values) { v.Del("success_url") }, "parameter_missing"},
		{"a success url that isn't one", func(v url.Values) { v.Set("success_url", "not a url") }, "url_invalid"},
		{"a javascript url", func(v url.Values) { v.Set("cancel_url", "javascript:alert(1)") }, "url_invalid"},
		{"the subscription mode", func(v url.Values) { v.Set("mode", "subscription") }, ""},
		{"an amount below the minimum", func(v url.Values) { v.Set("line_items[0][price_data][unit_amount]", "49") }, "amount_too_small"},
		{"an amount above the maximum", func(v url.Values) { v.Set("line_items[0][price_data][unit_amount]", "100000000") }, "amount_too_large"},
		{"a fraction", func(v url.Values) { v.Set("line_items[0][price_data][unit_amount]", "12.5") }, "parameter_invalid_integer"},
		{"a negative amount", func(v url.Values) { v.Set("line_items[0][price_data][unit_amount]", "-5") }, "parameter_invalid_integer"},
		{"an unknown currency", func(v url.Values) { v.Set("line_items[0][price_data][currency]", "xyz") }, ""},
		{"an upper-case currency", func(v url.Values) { v.Set("line_items[0][price_data][currency]", "EUR") }, ""},
		{"a three-decimal currency with a wrong last digit", func(v url.Values) {
			v.Set("line_items[0][price_data][currency]", "kwd")
			v.Set("line_items[0][price_data][unit_amount]", "12345")
		}, ""},
		{"a quantity of zero", func(v url.Values) { v.Set("line_items[0][quantity]", "0") }, ""},
		{"no quantity", func(v url.Values) { v.Del("line_items[0][quantity]") }, "parameter_missing"},
		{"an index that skips", func(v url.Values) {
			for k, val := range v {
				if strings.HasPrefix(k, "line_items[0]") {
					v.Set(strings.Replace(k, "[0]", "[2]", 1), val[0])
					v.Del(k)
				}
			}
		}, ""},
		{"a metadata key that is too long", func(v url.Values) { v.Set("metadata["+strings.Repeat("k", 41)+"]", "x") }, ""},
		{"a metadata value that is too long", func(v url.Values) { v.Set("metadata[k]", strings.Repeat("v", 501)) }, ""},
		{"a reference that is too long", func(v url.Values) { v.Set("client_reference_id", strings.Repeat("r", 201)) }, "string_too_long"},
		{"an expiry that is too soon", func(v url.Values) {
			v.Set("expires_at", strconv.FormatInt(clock.Now().Add(10*time.Minute).Unix(), 10))
		}, ""},
		{"an expiry that is too late", func(v url.Values) {
			v.Set("expires_at", strconv.FormatInt(clock.Now().Add(25*time.Hour).Unix(), 10))
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			form := sessionForm(12000, "eur", "r")
			tt.change(form)
			ans := a.post("/v1/checkout/sessions", form, "")
			if ans.status != http.StatusBadRequest {
				t.Fatalf("status %d: %s", ans.status, ans.raw)
			}
			typ, code := ans.errorCode()
			if typ != "invalid_request_error" || (tt.code != "" && code != tt.code) {
				t.Errorf("error %s %s: %s", typ, code, ans.raw)
			}
		})
	}
	// What is allowed.
	form := sessionForm(12000, "jpy", "r")
	form.Set("expires_at", strconv.FormatInt(clock.Now().Add(time.Hour).Unix(), 10))
	form.Set("metadata[ref]", "")
	if ans := a.post("/v1/checkout/sessions", form, ""); ans.status != http.StatusOK {
		t.Errorf("a valid session with an expiry: %d %s", ans.status, ans.raw)
	} else {
		var s testprovider.Session
		ans.into(t, &s)
		if s.ExpiresAt != clock.Now().Add(time.Hour).Unix() || len(s.Metadata) != 0 {
			t.Errorf("expiry %d, metadata %v", s.ExpiresAt, s.Metadata)
		}
	}
}

func TestIdempotencyKeys(t *testing.T) {
	f, clock, a := newFake(t)
	form := sessionForm(12000, "eur", "r1")

	first := a.post("/v1/checkout/sessions", form, "key-1")
	again := a.post("/v1/checkout/sessions", form, "key-1")
	if first.status != 200 || string(again.raw) != string(first.raw) {
		t.Fatalf("a retry with the same key and parameters: %d, same body %v", again.status, string(again.raw) == string(first.raw))
	}
	if first.header.Get("Idempotent-Replayed") != "" || again.header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("Idempotent-Replayed: first %q, retry %q", first.header.Get("Idempotent-Replayed"), again.header.Get("Idempotent-Replayed"))
	}
	if again.header.Get("Idempotency-Key") != "key-1" {
		t.Errorf("the key isn't echoed: %q", again.header.Get("Idempotency-Key"))
	}
	if n := len(f.Sessions()); n != 1 {
		t.Errorf("a retry made a second session: %d", n)
	}

	// The same key with other parameters is Stripe's conflict, and makes nothing.
	other := sessionForm(13000, "eur", "r1")
	conflict := a.post("/v1/checkout/sessions", other, "key-1")
	if typ, _ := conflict.errorCode(); conflict.status != http.StatusBadRequest || typ != "idempotency_error" {
		t.Errorf("other parameters: %d %s", conflict.status, conflict.raw)
	}
	// ... and so is the same key on another endpoint.
	if ans := a.post("/v1/payment_links", other, "key-1"); ans.status != http.StatusBadRequest {
		t.Errorf("another endpoint: %d", ans.status)
	} else if typ, _ := ans.errorCode(); typ != "idempotency_error" {
		t.Errorf("another endpoint: %s", ans.raw)
	}
	if n := len(f.Sessions()); n != 1 {
		t.Errorf("sessions = %d", n)
	}

	// A request refused before it began leaves nothing under its key: the corrected one runs.
	bad := sessionForm(10, "eur", "r2")
	if ans := a.post("/v1/checkout/sessions", bad, "key-2"); ans.status != http.StatusBadRequest {
		t.Fatalf("too small: %d", ans.status)
	}
	good := sessionForm(12000, "eur", "r2")
	if ans := a.post("/v1/checkout/sessions", good, "key-2"); ans.status != http.StatusOK {
		t.Errorf("the corrected request under the same key: %d %s", ans.status, ans.raw)
	}

	// No key, no idempotency.
	a.post("/v1/checkout/sessions", form, "")
	a.post("/v1/checkout/sessions", form, "")
	if n := len(f.Sessions()); n != 4 {
		t.Errorf("sessions = %d, want 4 (two with keys, two without)", n)
	}

	// Keys are forgotten after 24 hours: the same key then makes a new object.
	clock.Advance(24*time.Hour + time.Minute)
	later := a.post("/v1/checkout/sessions", form, "key-1")
	if later.status != http.StatusOK || later.header.Get("Idempotent-Replayed") != "" || string(later.raw) == string(first.raw) {
		t.Errorf("after 24 hours: %d replayed %q", later.status, later.header.Get("Idempotent-Replayed"))
	}

	if ans := a.post("/v1/checkout/sessions", form, strings.Repeat("k", 256)); ans.status != http.StatusBadRequest {
		t.Errorf("a key of 256 characters: %d", ans.status)
	}
	if ans := a.post("/v1/checkout/sessions", form, strings.Repeat("k", 255)); ans.status != http.StatusOK {
		t.Errorf("a key of 255 characters: %d", ans.status)
	}
}

func TestFailuresOnDemand(t *testing.T) {
	f, _, a := newFake(t)
	form := sessionForm(12000, "eur", "r")

	t.Run("a server error, once", func(t *testing.T) {
		f.Fail(testprovider.ServerError())
		ans := a.post("/v1/checkout/sessions", form, "")
		if typ, _ := ans.errorCode(); ans.status != 500 || typ != "api_error" || ans.header.Get("Stripe-Should-Retry") != "true" {
			t.Errorf("%d %s %v", ans.status, ans.raw, ans.header)
		}
		if n := len(f.Sessions()); n != 0 {
			t.Errorf("a fault before the operation made a session")
		}
		if ans := a.post("/v1/checkout/sessions", form, ""); ans.status != 200 {
			t.Errorf("the fault was one-shot: %d", ans.status)
		}
	})

	t.Run("rate limiting says when to come back", func(t *testing.T) {
		f.Fail(testprovider.RateLimited(1500 * time.Millisecond))
		ans := a.post("/v1/checkout/sessions", form, "")
		if ans.status != 429 || ans.header.Get("Retry-After") != "2" {
			t.Errorf("%d Retry-After %q", ans.status, ans.header.Get("Retry-After"))
		}
	})

	t.Run("several times, on one endpoint only", func(t *testing.T) {
		f.Fail(testprovider.Unavailable().Times(2).On(http.MethodPost, "/v1/checkout"))
		if ans := a.get("/v1/balance", nil); ans.status != 200 {
			t.Errorf("another endpoint was hit: %d", ans.status)
		}
		for i := range 2 {
			if ans := a.post("/v1/checkout/sessions", form, ""); ans.status != 503 {
				t.Errorf("try %d: %d", i, ans.status)
			}
		}
		if ans := a.post("/v1/checkout/sessions", form, ""); ans.status != 200 {
			t.Errorf("third try: %d", ans.status)
		}
	})

	t.Run("a refusal and a revoked key", func(t *testing.T) {
		f.Fail(testprovider.Refusal("currency_not_supported"))
		ans := a.post("/v1/checkout/sessions", form, "")
		if _, code := ans.errorCode(); ans.status != 400 || code != "currency_not_supported" {
			t.Errorf("%d %s", ans.status, ans.raw)
		}
		f.Fail(testprovider.Unauthorized())
		if ans := a.get("/v1/balance", nil); ans.status != 401 {
			t.Errorf("revoked: %d", ans.status)
		}
	})

	t.Run("forever, until cleared", func(t *testing.T) {
		f.Fail(testprovider.ServerError().Forever())
		for range 3 {
			if ans := a.get("/v1/balance", nil); ans.status != 500 {
				t.Fatalf("%d", ans.status)
			}
		}
		f.ClearFaults()
		if ans := a.get("/v1/balance", nil); ans.status != 200 {
			t.Errorf("after ClearFaults: %d", ans.status)
		}
	})

	t.Run("silence and a dropped connection", func(t *testing.T) {
		short := newAPI(t, f)
		short.client.Timeout = 200 * time.Millisecond
		before := len(f.Sessions())
		f.Fail(testprovider.Timeout())
		if _, err := short.try(http.MethodPost, "/v1/checkout/sessions", form, ""); err == nil {
			t.Error("a timeout fault got an answer")
		}
		f.Fail(testprovider.ConnectionReset())
		if _, err := short.try(http.MethodPost, "/v1/checkout/sessions", form, ""); err == nil {
			t.Error("a dropped connection got an answer")
		}
		if len(f.Sessions()) != before {
			t.Error("a fault before the operation made a session")
		}
		reqs := f.Requests()
		if last := reqs[len(reqs)-1]; last.Status != 0 || !last.Faulted {
			t.Errorf("the log of a dropped request: %+v", last)
		}
	})
}

// The case that matters: the provider did the work and the client never heard. A retry under the
// same idempotency key must find the object, and the payee must not get two links.
func TestAFailureAfterTheWorkWasDone(t *testing.T) {
	short := func(t *testing.T) (*testprovider.Stripe, *api) {
		f, _, a := newFake(t)
		a.client.Timeout = 200 * time.Millisecond
		return f, a
	}
	form := sessionForm(12000, "eur", "r")

	for name, fault := range map[string]testprovider.Fault{
		"a timeout":          testprovider.Timeout().AfterExecuting(),
		"a dropped response": testprovider.ConnectionReset().AfterExecuting(),
	} {
		t.Run(name, func(t *testing.T) {
			f, a := short(t)
			f.Fail(fault)
			if _, err := a.try(http.MethodPost, "/v1/checkout/sessions", form, "key-a"); err == nil {
				t.Fatal("the client heard back")
			}
			if n := len(f.Sessions()); n != 1 {
				t.Fatalf("the operation didn't run: %d sessions", n)
			}
			retry := a.post("/v1/checkout/sessions", form, "key-a")
			var s testprovider.Session
			retry.into(t, &s)
			if retry.status != 200 || retry.header.Get("Idempotent-Replayed") != "true" || s.ID != f.Sessions()[0].ID {
				t.Errorf("the retry: %d replayed %q id %q", retry.status, retry.header.Get("Idempotent-Replayed"), s.ID)
			}
			if n := len(f.Sessions()); n != 1 {
				t.Errorf("a second session: %d", n)
			}
		})
	}

	t.Run("a server error is saved under the key too", func(t *testing.T) {
		f, a := short(t)
		f.Fail(testprovider.ServerError().AfterExecuting())
		first := a.post("/v1/checkout/sessions", form, "key-b")
		retry := a.post("/v1/checkout/sessions", form, "key-b")
		if first.status != 500 || retry.status != 500 || retry.header.Get("Idempotent-Replayed") != "true" {
			t.Errorf("first %d, retry %d replayed %q", first.status, retry.status, retry.header.Get("Idempotent-Replayed"))
		}
		if n := len(f.Sessions()); n != 1 {
			t.Fatalf("sessions = %d", n)
		}
		// The way out is to find the object that was made, not to ask again with a new key.
		if s := f.Sessions()[0]; s.ClientReferenceID == nil || *s.ClientReferenceID != "r" {
			t.Errorf("the session made: %+v", s)
		}
	})
}

func TestASessionThroughItsStates(t *testing.T) {
	f, clock, a := newFake(t)
	retrieve := func(id string) testprovider.Session {
		var s testprovider.Session
		a.get("/v1/checkout/sessions/"+id, nil).into(t, &s)
		return s
	}

	open := a.newSession(12000, "eur", "open")
	if s := retrieve(open.ID); s.Status != "open" || s.PaymentStatus != "unpaid" || s.URL == nil {
		t.Errorf("open: %+v", s)
	}

	paid := a.newSession(12000, "eur", "paid")
	if _, err := f.Pay(paid.ID); err != nil {
		t.Fatal(err)
	}
	s := retrieve(paid.ID)
	if s.Status != "complete" || s.PaymentStatus != "paid" || s.URL != nil || s.PaymentIntent == nil ||
		!strings.HasPrefix(*s.PaymentIntent, "pi_") || s.CustomerDetails == nil || s.CustomerDetails.Email == "" {
		t.Errorf("paid: %+v", s)
	}
	if _, err := f.Pay(paid.ID); err == nil {
		t.Error("a session was paid twice")
	}

	delayed := a.newSession(12000, "eur", "delayed")
	if _, err := f.PayDelayed(delayed.ID); err != nil {
		t.Fatal(err)
	}
	if s := retrieve(delayed.ID); s.Status != "complete" || s.PaymentStatus != "unpaid" {
		t.Errorf("delayed, waiting: %+v", s)
	}
	if _, err := f.SettleDelayed(delayed.ID, true); err != nil {
		t.Fatal(err)
	}
	if s := retrieve(delayed.ID); s.Status != "complete" || s.PaymentStatus != "paid" {
		t.Errorf("delayed, settled: %+v", s)
	}
	failed := a.newSession(12000, "eur", "delayed-failed")
	if _, err := f.PayDelayed(failed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.SettleDelayed(failed.ID, false); err != nil {
		t.Fatal(err)
	}
	if s := retrieve(failed.ID); s.Status != "complete" || s.PaymentStatus != "unpaid" {
		t.Errorf("delayed, failed: %+v", s)
	}
	if _, err := f.SettleDelayed(open.ID, true); err == nil {
		t.Error("an open session was settled")
	}

	declined := a.newSession(12000, "eur", "declined")
	if _, err := f.Decline(declined.ID); err != nil {
		t.Fatal(err)
	}
	if s := retrieve(declined.ID); s.Status != "open" || s.PaymentStatus != "unpaid" {
		t.Errorf("a declined attempt leaves the session open: %+v", s)
	}

	byAPI := a.newSession(12000, "eur", "expired-by-api")
	ans := a.post("/v1/checkout/sessions/"+byAPI.ID+"/expire", nil, "")
	var expired testprovider.Session
	ans.into(t, &expired)
	if ans.status != 200 || expired.Status != "expired" || expired.URL != nil {
		t.Errorf("expire: %d %+v", ans.status, expired)
	}
	if again := a.post("/v1/checkout/sessions/"+byAPI.ID+"/expire", nil, ""); again.status != 400 {
		t.Errorf("expiring an expired session: %d", again.status)
	}
	if _, err := f.Pay(byAPI.ID); err == nil {
		t.Error("an expired session was paid")
	}
	if ans := a.post("/v1/checkout/sessions/"+paid.ID+"/expire", nil, ""); ans.status != 400 {
		t.Errorf("expiring a paid session: %d", ans.status)
	}

	// Time expires a session by itself, and says so in an event, at the time it expired.
	timed := a.newSession(12000, "eur", "expired-by-time")
	clock.Advance(24*time.Hour + time.Second)
	if s := retrieve(timed.ID); s.Status != "expired" || s.URL != nil {
		t.Errorf("after its time: %+v", s)
	}
	if s := retrieve(open.ID); s.Status != "expired" {
		t.Errorf("the open session after its time: %+v", s)
	}
	var times []int64
	for _, e := range f.EventsOfType(testprovider.EventSessionExpired) {
		var o testprovider.Session
		if err := e.ObjectAs(&o); err != nil {
			t.Fatal(err)
		}
		if o.ID == timed.ID {
			times = append(times, e.Created)
		}
	}
	if len(times) != 1 || times[0] != timed.ExpiresAt {
		t.Errorf("expired events of the timed session: %v, want one at %d", times, timed.ExpiresAt)
	}
}

func TestListingSessions(t *testing.T) {
	f, _, a := newFake(t)
	var ids []string
	for i := range 5 {
		ids = append(ids, a.newSession(12000, "eur", "r"+strconv.Itoa(i)).ID)
	}
	if _, err := f.Pay(ids[1]); err != nil {
		t.Fatal(err)
	}
	type list struct {
		Object  string
		URL     string
		HasMore bool `json:"has_more"`
		Data    []testprovider.Session
	}
	var l list
	a.get("/v1/checkout/sessions", url.Values{"limit": {"3"}}).into(t, &l)
	if l.Object != "list" || l.URL != "/v1/checkout/sessions" || !l.HasMore || len(l.Data) != 3 || l.Data[0].ID != ids[4] {
		t.Errorf("first page: %+v", l)
	}
	a.get("/v1/checkout/sessions", url.Values{"limit": {"3"}, "starting_after": {l.Data[2].ID}}).into(t, &l)
	if l.HasMore || len(l.Data) != 2 || l.Data[1].ID != ids[0] {
		t.Errorf("second page: %+v", l)
	}
	a.get("/v1/checkout/sessions", url.Values{"status": {"complete"}}).into(t, &l)
	if len(l.Data) != 1 || l.Data[0].ID != ids[1] {
		t.Errorf("status filter: %+v", l)
	}
	if ans := a.get("/v1/checkout/sessions", url.Values{"limit": {"101"}}); ans.status != 400 {
		t.Errorf("limit 101: %d", ans.status)
	}
	if ans := a.get("/v1/checkout/sessions", url.Values{"starting_after": {"cs_test_nope"}}); ans.status != 400 {
		t.Errorf("a cursor that doesn't exist: %d", ans.status)
	}
	if ans := a.get("/v1/checkout/sessions", url.Values{"colour": {"red"}}); ans.status != 400 {
		t.Errorf("an unknown filter: %d", ans.status)
	}
}

func TestPaymentLinks(t *testing.T) {
	f, _, a := newFake(t)
	form := url.Values{}
	form.Set("line_items[0][quantity]", "1")
	form.Set("line_items[0][price_data][currency]", "usd")
	form.Set("line_items[0][price_data][unit_amount]", "5000")
	form.Set("line_items[0][price_data][product_data][name]", "Invoice 8")
	form.Set("metadata[ref]", "ref_link")
	ans := a.post("/v1/payment_links", form, "link-key")
	var l testprovider.PaymentLink
	ans.into(t, &l)
	if ans.status != 200 || !strings.HasPrefix(l.ID, "plink_") || l.Object != "payment_link" || !l.Active ||
		!strings.HasPrefix(l.URL, "https://buy.stripe.com/test_") || l.Metadata["ref"] != "ref_link" || l.Livemode {
		t.Fatalf("create: %d %+v", ans.status, l)
	}
	if again := a.post("/v1/payment_links", form, "link-key"); string(again.raw) != string(ans.raw) {
		t.Error("the idempotent retry made another link")
	}
	if got := a.get("/v1/payment_links/"+l.ID, nil); string(got.raw) != string(ans.raw) {
		t.Errorf("retrieve differs")
	}
	// A link takes no client reference, and never expires by itself.
	withRef := url.Values{}
	for k, v := range form {
		withRef[k] = v
	}
	withRef.Set("client_reference_id", "r")
	if ans := a.post("/v1/payment_links", withRef, ""); ans.status != 400 {
		t.Errorf("a client reference on a link: %d", ans.status)
	} else if _, code := ans.errorCode(); code != "parameter_unknown" {
		t.Errorf("code %q", code)
	}
	withExpiry := url.Values{"expires_at": {"1"}}
	for k, v := range form {
		withExpiry[k] = v
	}
	if ans := a.post("/v1/payment_links", withExpiry, ""); ans.status != 400 {
		t.Errorf("an expiry on a link: %d", ans.status)
	}

	// Deactivating is the way to stop a link taking payments.
	up := a.post("/v1/payment_links/"+l.ID, url.Values{"active": {"false"}, "metadata[note]": {"stopped"}}, "")
	up.into(t, &l)
	if up.status != 200 || l.Active || l.Metadata["note"] != "stopped" || l.Metadata["ref"] != "ref_link" {
		t.Errorf("update: %d %+v", up.status, l)
	}
	if ans := a.get("/v1/payment_links/plink_nope", nil); ans.status != 404 {
		t.Errorf("unknown link: %d", ans.status)
	}
	if n := len(f.Sessions()); n != 0 {
		t.Errorf("a link made a session")
	}
}

func TestEventsOfOnePayment(t *testing.T) {
	f, _, a := newFake(t)
	types := func(evs []testprovider.Event) []string {
		var out []string
		for _, e := range evs {
			out = append(out, e.Type)
		}
		return out
	}
	want := func(t *testing.T, got []testprovider.Event, w ...string) {
		t.Helper()
		if g := strings.Join(types(got), " "); g != strings.Join(w, " ") {
			t.Errorf("events:\n got %s\nwant %s", g, strings.Join(w, " "))
		}
	}

	s := a.newSession(12000, "eur", "ref_pay")
	evs, err := f.Pay(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	// One card payment, five events: Stripe sends several for one payment, in no promised order.
	want(t, evs, "payment_intent.created", "charge.succeeded", "payment_intent.succeeded", "charge.updated", "checkout.session.completed")
	for _, e := range evs {
		if !strings.HasPrefix(e.ID, "evt_") || e.Object != "event" || e.APIVersion != testprovider.DefaultAPIVersion || e.Livemode {
			t.Errorf("event %s: %+v", e.Type, e)
		}
	}
	var done testprovider.Session
	if err := evs[4].ObjectAs(&done); err != nil {
		t.Fatal(err)
	}
	if done.ID != s.ID || done.PaymentStatus != "paid" || *done.ClientReferenceID != "ref_pay" || done.Metadata["ref"] != "ref_pay" {
		t.Errorf("the completed event's session: %+v", done)
	}
	var intent struct {
		ID, Status string
		Amount     int64
		Metadata   map[string]string
	}
	if err := evs[2].ObjectAs(&intent); err != nil {
		t.Fatal(err)
	}
	if intent.ID != *done.PaymentIntent || intent.Status != "succeeded" || intent.Amount != 12000 || intent.Metadata["ref"] != "ref_pay" {
		t.Errorf("the intent: %+v", intent)
	}
	if got := f.Events(); len(got) != 5 {
		t.Errorf("the fake has %d events", len(got))
	}

	d := a.newSession(12000, "eur", "ref_delayed")
	first, _ := f.PayDelayed(d.ID)
	want(t, first, "payment_intent.created", "payment_intent.processing", "checkout.session.completed")
	var pending testprovider.Session
	if err := first[2].ObjectAs(&pending); err != nil {
		t.Fatal(err)
	}
	if pending.PaymentStatus != "unpaid" {
		t.Errorf("a delayed payment's completed event says %q", pending.PaymentStatus)
	}
	ok, _ := f.SettleDelayed(d.ID, true)
	want(t, ok, "charge.succeeded", "payment_intent.succeeded", "checkout.session.async_payment_succeeded")
	d2 := a.newSession(12000, "eur", "ref_delayed_2")
	if _, err := f.PayDelayed(d2.ID); err != nil {
		t.Fatal(err)
	}
	bad, _ := f.SettleDelayed(d2.ID, false)
	want(t, bad, "payment_intent.payment_failed", "checkout.session.async_payment_failed")

	x := a.newSession(12000, "eur", "ref_declined")
	dec, _ := f.Decline(x.ID)
	want(t, dec, "payment_intent.created", "charge.failed", "payment_intent.payment_failed")
	again, _ := f.Decline(x.ID)
	want(t, again, "charge.failed", "payment_intent.payment_failed")

	e := a.newSession(12000, "eur", "ref_expire")
	exp, _ := f.Expire(e.ID)
	want(t, exp, "checkout.session.expired")

	// Refunds: partial, then the rest; more than is left is refused.
	r1, err := f.Refund(s.ID, 5000)
	if err != nil {
		t.Fatal(err)
	}
	want(t, r1, "refund.created", "charge.refunded", "charge.updated")
	if _, err := f.Refund(s.ID, 7001); err == nil {
		t.Error("refunded more than was left")
	}
	if _, err := f.Refund(s.ID, 0); err != nil {
		t.Errorf("refunding the rest: %v", err)
	}
	if _, err := f.Refund(e.ID, 0); err == nil {
		t.Error("refunded an unpaid session")
	}
	var charge struct {
		AmountRefunded int64 `json:"amount_refunded"`
		Refunded       bool
	}
	last := f.Events()
	if err := last[len(last)-1].ObjectAs(&charge); err != nil {
		t.Fatal(err)
	}
	if charge.AmountRefunded != 12000 || !charge.Refunded {
		t.Errorf("the charge after the refunds: %+v", charge)
	}
}

func TestListingEventsLikeThePollingWillNeed(t *testing.T) {
	f, clock, a := newFake(t)
	s := a.newSession(12000, "eur", "r")
	if _, err := f.Pay(s.ID); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour)
	x := a.newSession(12000, "eur", "r2")
	if _, err := f.Expire(x.ID); err != nil {
		t.Fatal(err)
	}

	type list struct {
		Object  string
		URL     string
		HasMore bool `json:"has_more"`
		Data    []testprovider.Event
	}
	var l list
	a.get("/v1/events", url.Values{"limit": {"100"}}).into(t, &l)
	if l.Object != "list" || l.URL != "/v1/events" || len(l.Data) != 6 || l.Data[0].Type != "checkout.session.expired" {
		t.Fatalf("all events: %d, first %q", len(l.Data), l.Data[0].Type)
	}
	a.get("/v1/events", url.Values{"type": {"checkout.session.*"}}).into(t, &l)
	if len(l.Data) != 2 {
		t.Errorf("a wildcard: %d", len(l.Data))
	}
	a.get("/v1/events", url.Values{"types[]": {"charge.succeeded", "payment_intent.succeeded"}}).into(t, &l)
	if len(l.Data) != 2 {
		t.Errorf("two types: %d", len(l.Data))
	}
	a.get("/v1/events", url.Values{"created[gt]": {strconv.FormatInt(clock.Now().Add(-30*time.Minute).Unix(), 10)}}).into(t, &l)
	if len(l.Data) != 1 || l.Data[0].Type != "checkout.session.expired" {
		t.Errorf("created[gt]: %d", len(l.Data))
	}
	a.get("/v1/events", url.Values{"limit": {"4"}}).into(t, &l)
	if !l.HasMore || len(l.Data) != 4 {
		t.Fatalf("a page: %d, more %v", len(l.Data), l.HasMore)
	}
	var next list
	a.get("/v1/events", url.Values{"limit": {"4"}, "starting_after": {l.Data[3].ID}}).into(t, &next)
	if next.HasMore || len(next.Data) != 2 {
		t.Errorf("the next page: %d, more %v", len(next.Data), next.HasMore)
	}
	var one testprovider.Event
	if ans := a.get("/v1/events/"+l.Data[1].ID, nil); ans.status != 200 {
		t.Errorf("retrieve: %d", ans.status)
	} else if ans.into(t, &one); one.ID != l.Data[1].ID {
		t.Errorf("retrieved %s", one.ID)
	}
	if ans := a.get("/v1/events", url.Values{"type": {"a"}, "types[]": {"b"}}); ans.status != 400 {
		t.Errorf("type and types: %d", ans.status)
	}
	if ans := a.get("/v1/events", url.Values{"object_id": {"x"}}); ans.status != 400 {
		t.Errorf("an unknown filter: %d", ans.status)
	}
	if ans := a.get("/v1/events/evt_nope", nil); ans.status != 404 {
		t.Errorf("unknown event: %d", ans.status)
	}

	// Events older than 30 days are gone, as at Stripe.
	clock.Advance(31 * 24 * time.Hour)
	a.get("/v1/events", nil).into(t, &l)
	if len(l.Data) != 0 {
		t.Errorf("after 31 days: %d events", len(l.Data))
	}
	if ans := a.get("/v1/events/"+one.ID, nil); ans.status != 404 {
		t.Errorf("an old event: %d", ans.status)
	}
}

func TestTheConnectionCheckCall(t *testing.T) {
	_, _, a := newFake(t)
	ans := a.get("/v1/balance", nil)
	var b struct {
		Object    string
		Available []map[string]any
		Livemode  bool
	}
	ans.into(t, &b)
	if ans.status != 200 || b.Object != "balance" || len(b.Available) == 0 {
		t.Errorf("%d %s", ans.status, ans.raw)
	}
	if ans := a.get("/v1/nothing", nil); ans.status != 404 {
		t.Errorf("an unknown path: %d", ans.status)
	}
	if ans := a.do(http.MethodDelete, "/v1/balance", nil, ""); ans.status != 404 {
		t.Errorf("a method the endpoint doesn't take: %d", ans.status)
	}
}

func TestTheRequestLogHoldsNoKey(t *testing.T) {
	f, _, a := newFake(t)
	a.post("/v1/checkout/sessions", sessionForm(12000, "eur", "logged"), "the-key")
	a.post("/v1/checkout/sessions", sessionForm(12000, "eur", "logged"), "the-key")
	reqs := f.RequestsTo(http.MethodPost, "/v1/checkout/sessions")
	if len(reqs) != 2 || reqs[0].IdempotencyKey != "the-key" || reqs[0].Replayed || !reqs[1].Replayed ||
		reqs[0].Form.Get("client_reference_id") != "logged" || reqs[0].APIVersion != testprovider.DefaultAPIVersion ||
		reqs[0].Auth != "bearer" || reqs[0].Status != 200 {
		t.Errorf("requests: %+v", reqs)
	}
	if raw, _ := json.Marshal(f.Requests()); strings.Contains(string(raw), f.APIKey()) {
		t.Error("the log holds the API key")
	}
}

// The fake is meant to be called by the adapter's Client. That client refuses every host the
// adapter doesn't own and sends to a test endpoint only by an option, so this proves the fake
// is reachable the way the adapter will reach it, and that the pinned version is the adapter's.
func TestTheRealClientReachesTheFake(t *testing.T) {
	f, _, _ := newFake(t)
	stripe := integrations.Lookup("stripe")
	if stripe.APIVersion() != testprovider.DefaultAPIVersion {
		t.Fatalf("the adapter pins %q, the fake answers to %q", stripe.APIVersion(), testprovider.DefaultAPIVersion)
	}
	for _, name := range []string{"api_key"} {
		if err := stripe.CheckSecret(name, f.APIKey()); err != nil {
			t.Errorf("the fake's key isn't accepted as %s: %v", name, err)
		}
	}
	if err := stripe.CheckSecret("webhook_secret", testprovider.TestWebhookSecret); err != nil {
		t.Errorf("the fake's webhook secret isn't accepted: %v", err)
	}
	if err := stripe.CheckSecret("webhook_secret", testprovider.OtherWebhookSecret); err != nil {
		t.Errorf("the other webhook secret isn't accepted: %v", err)
	}
	if got := stripe.Mode(nil, f.APIKey()); got != integrations.ModeTest {
		t.Errorf("mode %q", got)
	}

	c := integrations.NewClient(stripe, nil, egress.MustParse("any"), integrations.WithTestEndpoint(f.Host()))
	req, err := c.NewRequest(context.Background(), http.MethodGet, f.URL()+"/v1/balance", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.APIKey())
	req.Header.Set("Stripe-Version", stripe.APIVersion())
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status %d", resp.StatusCode)
	}
}
