package testprovider_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
