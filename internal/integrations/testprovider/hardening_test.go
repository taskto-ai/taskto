package testprovider_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/integrations/testprovider"
)

func linkForm(lines int) url.Values {
	form := url.Values{}
	for i := range lines {
		p := "line_items[" + strconv.Itoa(i) + "]"
		form.Set(p+"[quantity]", "1")
		form.Set(p+"[price_data][currency]", "usd")
		form.Set(p+"[price_data][unit_amount]", "100")
		form.Set(p+"[price_data][product_data][name]", "Line")
	}
	form.Set("metadata[ref]", "r")
	return form
}

func TestWhatTheFakeReturnsIsACopy(t *testing.T) {
	f, _, a := newFake(t)
	s := a.newSession(5000, "usd", "r")
	paid, err := f.Pay(s.ID)
	if err != nil {
		t.Fatal(err)
	}

	was := string(f.Events()[0].Data.Object)
	paid[0].Data.Object[0] = 'X'
	f.Events()[0].Data.Object[0] = 'X'
	if got := string(f.Events()[0].Data.Object); got != was {
		t.Errorf("a returned event changed the fake's: %s", got)
	}

	snap, _ := f.Session(s.ID)
	*snap.PaymentIntent = "changed"
	snap.CustomerDetails.Name = "changed"
	again, _ := f.Session(s.ID)
	if *again.PaymentIntent == "changed" || again.CustomerDetails.Name == "changed" {
		t.Errorf("a returned session shares its fields with the fake's: %+v", again)
	}

	reqs := f.Requests()
	reqs[0].Form.Set("mode", "changed")
	reqs[0].Form["line_items[0][quantity]"][0] = "changed"
	if got := f.Requests()[0]; got.Form.Get("mode") != "payment" || got.Form.Get("line_items[0][quantity]") != "1" {
		t.Errorf("a returned request log shares its form: %v", got.Form)
	}
}

func TestEventTypeAndTypesAreExclusive(t *testing.T) {
	_, _, a := newFake(t)
	for name, q := range map[string]url.Values{
		"types[]":  {"type": {"charge.succeeded"}, "types[]": {"charge.failed"}},
		"types[0]": {"type": {"charge.succeeded"}, "types[0]": {"charge.failed"}},
	} {
		if ans := a.get("/v1/events", q); ans.status != http.StatusBadRequest {
			t.Errorf("type with %s: %d %s", name, ans.status, ans.raw)
		}
	}
}

func TestEventsAreNewestFirstByTheirTime(t *testing.T) {
	f, clock, a := newFake(t)
	first := a.newSession(5000, "usd", "first") // expires in 24 hours
	form := sessionForm(5000, "usd", "second")
	form.Set("expires_at", strconv.FormatInt(clock.Now().Add(31*time.Minute).Unix(), 10))
	var second testprovider.Session
	a.post("/v1/checkout/sessions", form, "").into(t, &second)

	clock.Advance(25 * time.Hour) // both expire, the later-made one first in time
	f.Sessions()                  // noticed in the order the sessions were made
	var list struct{ Data []testprovider.Event }
	a.get("/v1/events", url.Values{"type": {"checkout.session.expired"}}).into(t, &list)
	var ids []string
	for _, e := range list.Data {
		var o struct{ ID string }
		if err := json.Unmarshal(e.Data.Object, &o); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, o.ID)
	}
	if len(ids) != 2 || ids[0] != first.ID || ids[1] != second.ID {
		t.Errorf("order %v, want %s (expired later) then %s", ids, first.ID, second.ID)
	}
}

func TestAFailedDelayedPaymentCanNotSettleAgain(t *testing.T) {
	f, _, a := newFake(t)
	s := a.newSession(5000, "usd", "r")
	if _, err := f.PayDelayed(s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.SettleDelayed(s.ID, false); err != nil {
		t.Fatal(err)
	}
	for _, ok := range []bool{false, true} {
		if _, err := f.SettleDelayed(s.ID, ok); !errors.Is(err, testprovider.ErrNotOpen) {
			t.Errorf("settling again with %v: %v", ok, err)
		}
	}
	if got, _ := f.Session(s.ID); got.Status != testprovider.SessionComplete || got.PaymentStatus != testprovider.PaymentUnpaid {
		t.Errorf("the failed payment's session: %s, %s", got.Status, got.PaymentStatus)
	}
}

func TestPaymentLinkLimitsAndUpdates(t *testing.T) {
	_, _, a := newFake(t)

	if ans := a.post("/v1/payment_links", linkForm(20), ""); ans.status != http.StatusOK {
		t.Fatalf("20 lines: %d %s", ans.status, ans.raw)
	}
	if ans := a.post("/v1/payment_links", linkForm(21), ""); ans.status != http.StatusBadRequest {
		t.Errorf("21 lines on a link: %d", ans.status)
	}
	big := sessionForm(100, "usd", "r") // a session takes more lines than a link
	for i := 1; i < 21; i++ {
		for k, v := range linkForm(21) {
			if strings.HasPrefix(k, "line_items["+strconv.Itoa(i)+"]") {
				big[k] = v
			}
		}
	}
	if ans := a.post("/v1/checkout/sessions", big, ""); ans.status != http.StatusOK {
		t.Errorf("21 lines on a session: %d %s", ans.status, ans.raw)
	}

	var l testprovider.PaymentLink
	a.post("/v1/payment_links", linkForm(1), "").into(t, &l)
	path := "/v1/payment_links/" + l.ID
	unchanged := func(t *testing.T) {
		t.Helper()
		var got testprovider.PaymentLink
		a.get(path, nil).into(t, &got)
		if !got.Active || len(got.Metadata) != 1 || got.Metadata["ref"] != "r" {
			t.Errorf("a refused update changed the link: %+v", got)
		}
	}

	many := url.Values{"active": {"false"}}
	for i := range 50 { // with ref, 51 keys
		many.Set("metadata[k"+strconv.Itoa(i)+"]", "v")
	}
	for name, form := range map[string]url.Values{
		"a long value":     {"active": {"false"}, "metadata[n]": {strings.Repeat("v", 501)}},
		"a long key":       {"active": {"false"}, "metadata[" + strings.Repeat("k", 41) + "]": {"v"}},
		"too many keys":    many,
		"a metadata value": {"active": {"false"}, "metadata": {"text"}},
	} {
		if ans := a.post(path, form, ""); ans.status != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, ans.status, ans.raw)
		}
		unchanged(t)
	}

	// An empty `metadata` clears it; keys beside it are set after.
	var cleared testprovider.PaymentLink
	ans := a.post(path, url.Values{"metadata": {""}, "metadata[n]": {"1"}}, "")
	ans.into(t, &cleared)
	if ans.status != http.StatusOK || len(cleared.Metadata) != 1 || cleared.Metadata["n"] != "1" {
		t.Errorf("clearing: %d %+v", ans.status, cleared)
	}
	ans = a.post(path, url.Values{"metadata": {""}}, "")
	cleared = testprovider.PaymentLink{}
	ans.into(t, &cleared)
	if ans.status != http.StatusOK || len(cleared.Metadata) != 0 {
		t.Errorf("clearing all: %d %+v", ans.status, cleared)
	}
}

func TestAFaultAfterTheWorkWaitsForTheWork(t *testing.T) {
	f, _, a := newFake(t)
	form := sessionForm(5000, "usd", "r")
	if ans := a.post("/v1/checkout/sessions", form, "k1"); ans.status != http.StatusOK {
		t.Fatalf("first: %d", ans.status)
	}

	f.Fail(testprovider.ServerError().AfterExecuting())
	// A retry served from the saved answer, and a refused request, run nothing.
	if ans := a.post("/v1/checkout/sessions", form, "k1"); ans.status != http.StatusOK || ans.header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("replay: %d replayed %q", ans.status, ans.header.Get("Idempotent-Replayed"))
	}
	if ans := a.post("/v1/checkout/sessions", url.Values{}, "k2"); ans.status != http.StatusBadRequest {
		t.Errorf("refused: %d", ans.status)
	}
	// The fault strikes the request that does run.
	if ans := a.post("/v1/checkout/sessions", sessionForm(6000, "usd", "r2"), "k3"); ans.status != http.StatusInternalServerError {
		t.Errorf("the fault: %d", ans.status)
	}
	if n := len(f.Sessions()); n != 2 {
		t.Errorf("sessions = %d, want the first and the one the fault followed", n)
	}
}

func TestRequestFraming(t *testing.T) {
	f, _, a := newFake(t)
	form := sessionForm(5000, "usd", "r")

	for name, ct := range map[string]string{
		"a longer media type": "application/x-www-form-urlencoded-extra",
		"a different type":    "text/plain",
	} {
		a.header = func(h http.Header) { h.Set("Content-Type", ct) }
		if ans := a.post("/v1/checkout/sessions", form, ""); ans.status != http.StatusBadRequest {
			t.Errorf("%s: %d", name, ans.status)
		}
	}
	a.header = func(h http.Header) { h.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8") }
	if ans := a.post("/v1/checkout/sessions", form, ""); ans.status != http.StatusOK {
		t.Errorf("a media type with parameters: %d %s", ans.status, ans.raw)
	}

	// Basic credentials without their separator are not the key.
	a.header = func(h http.Header) {
		h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(f.APIKey())))
	}
	if ans := a.post("/v1/checkout/sessions", form, ""); ans.status != http.StatusUnauthorized {
		t.Errorf("basic without a colon: %d", ans.status)
	}
	a.header = nil

	// A body of more than 1 MiB whose first 1 MiB is a whole form isn't run as that form.
	n := len(f.Sessions())
	body := form.Encode() + strings.Repeat("&", 1<<20)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, f.URL()+"/v1/checkout/sessions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.APIKey())
	req.Header.Set("Stripe-Version", testprovider.DefaultAPIVersion)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || len(f.Sessions()) != n {
		t.Errorf("a body over the limit: %d, %d sessions made", resp.StatusCode, len(f.Sessions())-n)
	}
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

func TestADelayedDeliveryEndsWhenItsContextIsDone(t *testing.T) {
	clock := wallClock{} // a clock that can't be advanced: the delay is a real wait
	h := newHook(clock)
	sender := testprovider.NewHandlerSender(h, "/hook", testprovider.WithSenderClock(clock))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := sender.Send(ctx, someEvent(clock), testprovider.Delayed(time.Hour))
	if !errors.Is(err, context.Canceled) || time.Since(start) > 5*time.Second {
		t.Errorf("err %v after %v", err, time.Since(start))
	}
}

func TestParametersStripeRefusesAtThePinnedVersion(t *testing.T) {
	_, _, a := newFake(t)
	card := sessionForm(5000, "usd", "r")
	card.Set("payment_method_types[0]", "card") // removed from Checkout Session creation
	if ans := a.post("/v1/checkout/sessions", card, ""); ans.status != http.StatusBadRequest {
		t.Errorf("payment_method_types: %d %s", ans.status, ans.raw)
	} else if _, code := ans.errorCode(); code != "parameter_unknown" {
		t.Errorf("code %q", code)
	}
	if ans := a.get("/v1/events", url.Values{"delivery_success": {"true"}}); ans.status != http.StatusBadRequest {
		t.Errorf("delivery_success is ignored, not refused: %d", ans.status)
	}
	for name, path := range map[string]string{"a session": "/v1/checkout/sessions", "a link": "/v1/payment_links"} {
		form := sessionForm(5000, "usd", "r")
		if path == "/v1/payment_links" {
			form = linkForm(1)
		}
		form.Set("submit_type", "pay")
		if ans := a.post(path, form, ""); ans.status != http.StatusOK {
			t.Errorf("%s with submit_type=pay: %d %s", name, ans.status, ans.raw)
		}
		form.Set("submit_type", "not_a_submit_type")
		if ans := a.post(path, form, ""); ans.status != http.StatusBadRequest {
			t.Errorf("%s with a bad submit_type: %d", name, ans.status)
		}
	}
}

func TestLimitsCountCharactersNotBytes(t *testing.T) {
	_, _, a := newFake(t)
	form := sessionForm(5000, "usd", strings.Repeat("é", 200))
	form.Set("metadata["+strings.Repeat("é", 40)+"]", strings.Repeat("é", 250))
	if ans := a.post("/v1/checkout/sessions", form, ""); ans.status != http.StatusOK {
		t.Errorf("limits met in characters: %d %s", ans.status, ans.raw)
	}
	for name, mutate := range map[string]func(url.Values){
		"a 41-character key":    func(v url.Values) { v.Set("metadata["+strings.Repeat("é", 41)+"]", "x") },
		"a 501-character value": func(v url.Values) { v.Set("metadata[long]", strings.Repeat("é", 501)) },
		"a 201-character ref":   func(v url.Values) { v.Set("client_reference_id", strings.Repeat("é", 201)) },
	} {
		bad := sessionForm(5000, "usd", "r")
		mutate(bad)
		if ans := a.post("/v1/checkout/sessions", bad, ""); ans.status != http.StatusBadRequest {
			t.Errorf("%s: %d", name, ans.status)
		}
	}
}

func TestADeclinedChargeIsTheIntentsLatestInBothEvents(t *testing.T) {
	f, _, a := newFake(t)
	s := a.newSession(5000, "usd", "r")
	evs, err := f.Decline(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	var chargeID, latest any
	for _, e := range evs {
		var o map[string]any
		if err := json.Unmarshal(e.Data.Object, &o); err != nil {
			t.Fatal(err)
		}
		switch e.Type {
		case testprovider.EventChargeFailed:
			chargeID = o["id"]
		case testprovider.EventIntentFailed:
			latest = o["latest_charge"]
		}
	}
	if chargeID == nil || chargeID != latest {
		t.Errorf("charge.failed is %v, the intent's latest_charge %v", chargeID, latest)
	}
}

func TestATruncatedAnswerIsAnErrorAndTheDeliveryIsKept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("short")) // fewer bytes than promised: the server drops the connection
	}))
	t.Cleanup(srv.Close)
	clock := testprovider.NewManualClock(time.Time{})
	s := testprovider.NewSender(srv.URL, testprovider.WithSenderClock(clock))
	got, err := s.SendAll(context.Background(), []testprovider.Event{someEvent(clock), someEvent(clock)})
	if err == nil {
		t.Fatal("a truncated answer reported success")
	}
	if len(got) != 1 {
		t.Errorf("SendAll returned %d deliveries beside the error, want the one that was made", len(got))
	}
	if n := len(s.Deliveries()); n != 1 {
		t.Errorf("the delivery that was made is kept, and sending stops: %d deliveries", n)
	}
}

func TestDeliveriesAreCopies(t *testing.T) {
	clock := testprovider.NewManualClock(time.Time{})
	h := newHook(clock)
	s := testprovider.NewHandlerSender(h, "/h", testprovider.WithSenderClock(clock))
	d, err := s.Send(context.Background(), someEvent(clock))
	if err != nil {
		t.Fatal(err)
	}
	was := string(d.Body)
	d.Body[0] = 'X'
	if kept := s.Deliveries(); string(kept[0].Body) != was {
		t.Fatalf("a returned delivery changed the kept one")
	} else {
		kept[0].Body[0] = 'Y'
	}
	if kept := s.Deliveries()[0]; string(kept.Body) != was {
		t.Fatalf("a listed delivery changed the kept one")
	}
	if again, err := s.Replay(context.Background(), s.Deliveries()[0]); err != nil || string(again.Body) != was {
		t.Errorf("the replay: %v %q", err, again.Body)
	}
}

func TestEventsReadInCreationOrderToo(t *testing.T) {
	f, clock, a := newFake(t)
	a.newSession(5000, "usd", "first") // expires in 24 hours
	form := sessionForm(5000, "usd", "second")
	form.Set("expires_at", strconv.FormatInt(clock.Now().Add(31*time.Minute).Unix(), 10))
	a.post("/v1/checkout/sessions", form, "")
	clock.Advance(25 * time.Hour)
	f.Sessions() // both expire when noticed, the one made first being the later in time
	var last int64
	for _, e := range f.Events() {
		if e.Created < last {
			t.Fatalf("Events isn't oldest first: %d after %d", e.Created, last)
		}
		last = e.Created
	}
	if n := len(f.EventsOfType(testprovider.EventSessionExpired)); n != 2 {
		t.Errorf("expired events: %d", n)
	}
}

func TestWildcardsAreForTheTypeFilterOnly(t *testing.T) {
	f, _, a := newFake(t)
	s := a.newSession(5000, "usd", "r")
	if _, err := f.Pay(s.ID); err != nil {
		t.Fatal(err)
	}
	count := func(q url.Values) int {
		var list struct{ Data []testprovider.Event }
		ans := a.get("/v1/events", q)
		ans.into(t, &list)
		return len(list.Data)
	}
	if n := count(url.Values{"type": {"charge.*"}}); n != 2 {
		t.Errorf("type=charge.*: %d events, want the charge's two", n)
	}
	if n := count(url.Values{"types[]": {"charge.*"}}); n != 0 {
		t.Errorf("types[]=charge.*: %d events, want none: it takes names", n)
	}
	if n := count(url.Values{"types[]": {"charge.succeeded"}}); n != 1 {
		t.Errorf("types[]=charge.succeeded: %d", n)
	}
}

func TestLinksKeepTheirConfirmationMessageAndHost(t *testing.T) {
	f, _, a := newFake(t)
	form := linkForm(1)
	form.Set("after_completion[hosted_confirmation][custom_message]", "Thank you")
	var l testprovider.PaymentLink
	a.post("/v1/payment_links", form, "").into(t, &l)
	if got := l.AfterCompletion["hosted_confirmation"].(map[string]any)["custom_message"]; got != "Thank you" {
		t.Errorf("the message: %v", got)
	}
	var again testprovider.PaymentLink
	a.get("/v1/payment_links/"+l.ID, nil).into(t, &again)
	if got := again.AfterCompletion["hosted_confirmation"].(map[string]any)["custom_message"]; got != "Thank you" {
		t.Errorf("the message on retrieve: %v", got)
	}
	for submit, host := range map[string]string{"": "buy", "pay": "buy", "donate": "donate", "book": "book"} {
		form := linkForm(1)
		if submit != "" {
			form.Set("submit_type", submit)
		}
		var l testprovider.PaymentLink
		a.post("/v1/payment_links", form, "").into(t, &l)
		if want := "https://" + host + ".stripe.com/test_"; !strings.HasPrefix(l.URL, want) {
			t.Errorf("submit_type %q: %s, want it to start %s", submit, l.URL, want)
		}
	}
	_ = f
}

func TestAnArrayIndexHasOneSpelling(t *testing.T) {
	_, _, a := newFake(t)
	form := sessionForm(100, "usd", "r") // too small to pay on its own
	form.Set("line_items[0][price_data][unit_amount]", "5000")
	form.Set("line_items[00][price_data][unit_amount]", "200")
	if ans := a.post("/v1/checkout/sessions", form, ""); ans.status != http.StatusBadRequest {
		t.Errorf("line_items[00]: %d %s", ans.status, ans.raw)
	}
}

// driftClock moves by drift each time it is read once on is set: a clock that doesn't stand still
// between two readings of one operation.
type driftClock struct {
	mu    sync.Mutex
	now   time.Time
	drift time.Duration
	on    bool
}

func (c *driftClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.now
	if c.on {
		c.now = c.now.Add(c.drift)
	}
	return t
}

func (c *driftClock) set(on bool) { c.mu.Lock(); c.on = on; c.mu.Unlock() }

func TestAPaymentIsOfTheTimeTheSessionWasFoundOpen(t *testing.T) {
	clock := &driftClock{now: testprovider.DefaultStart, drift: time.Hour}
	f := testprovider.NewStripe(t, testprovider.WithClock(clock))
	a := newAPI(t, f)
	form := sessionForm(5000, "usd", "r")
	form.Set("expires_at", strconv.FormatInt(clock.Now().Add(31*time.Minute).Unix(), 10))
	var s testprovider.Session
	a.post("/v1/checkout/sessions", form, "").into(t, &s)
	for name, pay := range map[string]func(string) ([]testprovider.Event, error){"Pay": f.Pay, "PayDelayed": f.PayDelayed} {
		_ = name
		clock.set(false)
		s = func() testprovider.Session {
			var n testprovider.Session
			form := sessionForm(5000, "usd", name)
			form.Set("expires_at", strconv.FormatInt(clock.Now().Add(31*time.Minute).Unix(), 10))
			a.post("/v1/checkout/sessions", form, "").into(t, &n)
			return n
		}()
		clock.set(true) // from here every reading is an hour later than the last
		evs, err := pay(s.ID)
		clock.set(false)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, e := range evs {
			if e.Created >= s.ExpiresAt {
				t.Errorf("%s: %s was made at or after the session's expiry", name, e.Type)
			}
		}
	}
}

func TestPromotionCodesAreABoolean(t *testing.T) {
	_, _, a := newFake(t)
	for name, path := range map[string]string{"a session": "/v1/checkout/sessions", "a link": "/v1/payment_links"} {
		form := sessionForm(5000, "usd", "r")
		if path == "/v1/payment_links" {
			form = linkForm(1)
		}
		form.Set("allow_promotion_codes", "not-a-boolean")
		if ans := a.post(path, form, ""); ans.status != http.StatusBadRequest {
			t.Errorf("%s: %d", name, ans.status)
		}
		form.Set("allow_promotion_codes", "true")
		if ans := a.post(path, form, ""); ans.status != http.StatusOK {
			t.Errorf("%s with true: %d %s", name, ans.status, ans.raw)
		}
	}
	form := sessionForm(5000, "usd", "r")
	form.Set("allow_promotion_codes", "true")
	var s struct {
		AllowPromotionCodes any `json:"allow_promotion_codes"`
	}
	a.post("/v1/checkout/sessions", form, "").into(t, &s)
	if s.AllowPromotionCodes != true {
		t.Errorf("the session's allow_promotion_codes: %v", s.AllowPromotionCodes)
	}
}

func TestAMalformedQueryIsRefused(t *testing.T) {
	f, _, a := newFake(t)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, f.URL()+"/v1/events?limit=%ZZ", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.APIKey())
	req.Header.Set("Stripe-Version", testprovider.DefaultAPIVersion)
	resp, err := a.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a malformed query: %d", resp.StatusCode)
	}
}

func TestADeliveryThatIsRedirectedFailed(t *testing.T) {
	var elsewhere int
	mux := http.NewServeMux()
	mux.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/other", http.StatusFound) })
	mux.HandleFunc("/other", func(w http.ResponseWriter, _ *http.Request) { elsewhere++; w.WriteHeader(http.StatusOK) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	clock := testprovider.NewManualClock(time.Time{})
	d, err := testprovider.NewSender(srv.URL+"/hook", testprovider.WithSenderClock(clock)).Send(context.Background(), someEvent(clock))
	if err != nil || d.Status != http.StatusFound || d.OK() || elsewhere != 0 {
		t.Errorf("status %d, ok %v, followed %d times, err %v", d.Status, d.OK(), elsewhere, err)
	}
}

func TestACanceledContextDeliversNothing(t *testing.T) {
	clock := testprovider.NewManualClock(time.Time{})
	h := newHook(clock)
	s := testprovider.NewHandlerSender(h, "/h", testprovider.WithSenderClock(clock))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := clock.Now()
	for name, opts := range map[string][]testprovider.SignOption{"no delay": nil, "a delay": {testprovider.Delayed(time.Hour)}} {
		if _, err := s.Send(ctx, someEvent(clock), opts...); !errors.Is(err, context.Canceled) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if applied, refused := h.count(); applied != 0 || refused != 0 {
		t.Errorf("the handler ran: %d applied, %d refused", applied, refused)
	}
	if !clock.Now().Equal(start) {
		t.Errorf("a canceled delivery moved the clock to %v", clock.Now())
	}
}

func TestAnAnswerAtTheCapIsKeptAndOneOverIsAnError(t *testing.T) {
	const capBytes = 1 << 20
	answer := func(declared, written int) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(declared))
			_, _ = w.Write(make([]byte, written))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	clock := testprovider.NewManualClock(time.Time{})
	send := func(srv *httptest.Server) (testprovider.Delivery, error) {
		return testprovider.NewSender(srv.URL, testprovider.WithSenderClock(clock)).Send(context.Background(), someEvent(clock))
	}
	if d, err := send(answer(capBytes, capBytes)); err != nil || len(d.Response) != capBytes {
		t.Errorf("a whole answer of the cap: %v, %d bytes", err, len(d.Response))
	}
	if _, err := send(answer(2*capBytes, capBytes)); err == nil {
		t.Error("an answer cut off at the cap reported success")
	}
	if d, err := send(answer(capBytes+1, capBytes+1)); err == nil || d.Status != http.StatusOK {
		t.Errorf("an answer over the cap: %v, status %d", err, d.Status)
	}
}

func TestParametersThatNothingActsOnAreRefused(t *testing.T) {
	_, _, a := newFake(t)
	for name, tc := range map[string]struct {
		path  string
		form  func() url.Values
		param string
	}{
		"customer_creation":      {"/v1/checkout/sessions", func() url.Values { return sessionForm(5000, "usd", "r") }, "customer_creation"},
		"billing address":        {"/v1/checkout/sessions", func() url.Values { return sessionForm(5000, "usd", "r") }, "billing_address_collection"},
		"a session's descriptor": {"/v1/checkout/sessions", func() url.Values { return sessionForm(5000, "usd", "r") }, "payment_intent_data[statement_descriptor]"},
		"a link's inactive text": {"/v1/payment_links", func() url.Values { return linkForm(1) }, "inactive_message"},
		"a link's intent data":   {"/v1/payment_links", func() url.Values { return linkForm(1) }, "payment_intent_data[description]"},
	} {
		form := tc.form()
		form.Set(tc.param, "x")
		if ans := a.post(tc.path, form, ""); ans.status != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, ans.status, ans.raw)
		}
	}
	var l testprovider.PaymentLink
	a.post("/v1/payment_links", linkForm(1), "").into(t, &l)
	if ans := a.post("/v1/payment_links/"+l.ID, url.Values{"inactive_message": {"closed"}}, ""); ans.status != http.StatusBadRequest {
		t.Errorf("a link update's inactive_message: %d", ans.status)
	}
}

func TestASignaturesAgeAndAFreshDeliveryDelayAreIndependent(t *testing.T) {
	clock := testprovider.NewManualClock(time.Time{})
	h := newHook(clock)
	s := testprovider.NewHandlerSender(h, "/h", testprovider.WithSenderClock(clock))
	start := clock.Now()
	for name, opts := range map[string][]testprovider.SignOption{
		"old then delayed": {testprovider.OldBy(6 * time.Minute), testprovider.Delayed(time.Second)},
		"delayed then old": {testprovider.Delayed(time.Second), testprovider.OldBy(6 * time.Minute)},
	} {
		clock.Set(start)
		d, err := s.Send(context.Background(), someEvent(clock), opts...)
		if err != nil {
			t.Fatal(err)
		}
		if want := start.Add(time.Second - 6*time.Minute); !d.SignedAt.Equal(want) || !clock.Now().Equal(start.Add(time.Second)) {
			t.Errorf("%s: signed at %v (want %v), clock at %v", name, d.SignedAt, want, clock.Now())
		}
		if d.OK() {
			t.Errorf("%s: a six-minute-old signature was accepted", name)
		}
	}
}
