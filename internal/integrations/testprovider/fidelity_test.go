package testprovider_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"taskto.ai/taskto/internal/integrations/testprovider"
)

func oneLineLink(amount int, currency string) url.Values {
	v := url.Values{}
	v.Set("line_items[0][quantity]", "1")
	v.Set("line_items[0][price_data][currency]", currency)
	v.Set("line_items[0][price_data][unit_amount]", strconv.Itoa(amount))
	v.Set("line_items[0][price_data][product_data][name]", "Invoice 9")
	v.Set("metadata[ref]", "ref_limit")
	return v
}

func (a *api) newLink(form url.Values) testprovider.PaymentLink {
	a.t.Helper()
	ans := a.post("/v1/payment_links", form, "")
	if ans.status != http.StatusOK {
		a.t.Fatalf("create link: %d %s", ans.status, ans.raw)
	}
	var l testprovider.PaymentLink
	ans.into(a.t, &l)
	return l
}

func (a *api) link(id string) testprovider.PaymentLink {
	a.t.Helper()
	var l testprovider.PaymentLink
	a.get("/v1/payment_links/"+id, nil).into(a.t, &l)
	return l
}

// restrictionsJSON is the link's "restrictions" as the API wrote it, compacted.
func restrictionsJSON(t *testing.T, ans answer) string {
	t.Helper()
	var obj map[string]json.RawMessage
	ans.into(t, &obj)
	raw, ok := obj["restrictions"]
	if !ok {
		t.Fatalf("no restrictions in %s", ans.raw)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func count(l testprovider.PaymentLink) (limit, completed int, ok bool) {
	if l.Restrictions == nil {
		return 0, 0, false
	}
	return l.Restrictions.CompletedSessions.Limit, l.Restrictions.CompletedSessions.Count, true
}

func TestAPaymentLinkDeactivatesAtItsLimitOfCompletedSessions(t *testing.T) {
	f, _, a := newFake(t)

	// Stripe takes the parameter on create, and says so in the object.
	form := oneLineLink(5000, "usd")
	form.Set("restrictions[completed_sessions][limit]", "2")
	l := a.newLink(form)
	if limit, done, ok := count(l); !ok || limit != 2 || done != 0 || !l.Active {
		t.Fatalf("created: %+v", l)
	}
	if got := restrictionsJSON(t, a.get("/v1/payment_links/"+l.ID, nil)); got != `{"completed_sessions":{"count":0,"limit":2}}` {
		t.Errorf("the object's restrictions: %s", got)
	}

	evs, err := f.PayLink(l.ID)
	if err != nil {
		t.Fatal(err)
	}
	var completed *testprovider.Session
	for _, e := range evs {
		if e.Type == testprovider.EventSessionCompleted {
			completed = new(testprovider.Session)
			if err := json.Unmarshal(e.Data.Object, completed); err != nil {
				t.Fatal(err)
			}
		}
	}
	if completed == nil || completed.PaymentLink == nil || *completed.PaymentLink != l.ID || completed.Status != "complete" ||
		completed.PaymentStatus != "paid" || completed.AmountTotal != 5000 || completed.Metadata["ref"] != "ref_limit" {
		t.Fatalf("the session of a payment through the link: %+v", completed)
	}
	if got := a.link(l.ID); !got.Active {
		t.Errorf("one of two payments deactivated the link: %+v", got)
	} else if _, done, _ := count(got); done != 1 {
		t.Errorf("count %d after one payment", done)
	}

	// The payment that reaches the limit deactivates the link.
	if _, err := f.PayLink(l.ID); err != nil {
		t.Fatal(err)
	}
	got := a.link(l.ID)
	if _, done, _ := count(got); got.Active || done != 2 {
		t.Errorf("at the limit: %+v", got)
	}
	// A third takes nothing.
	if _, err := f.PayLink(l.ID); !errors.Is(err, testprovider.ErrLinkInactive) {
		t.Errorf("a payment past the limit: %v", err)
	}
	if n := len(f.Sessions()); n != 2 {
		t.Errorf("%d sessions", n)
	}
	var list struct{ Data []testprovider.Session }
	a.get("/v1/checkout/sessions", url.Values{"payment_link": {l.ID}}).into(t, &list)
	if len(list.Data) != 2 {
		t.Errorf("the sessions of the link: %d", len(list.Data))
	}

	// A raised limit doesn't reactivate by itself; the link's owner does.
	up := a.post("/v1/payment_links/"+l.ID, url.Values{"restrictions[completed_sessions][limit]": {"3"}}, "")
	var updated testprovider.PaymentLink
	up.into(t, &updated)
	if limit, done, _ := count(updated); up.status != 200 || limit != 3 || done != 2 || updated.Active {
		t.Errorf("raised limit: %d %+v", up.status, updated)
	}
	if _, err := f.PayLink(l.ID); !errors.Is(err, testprovider.ErrLinkInactive) {
		t.Errorf("a deactivated link took a payment: %v", err)
	}
	a.post("/v1/payment_links/"+l.ID, url.Values{"active": {"true"}}, "")
	if _, err := f.PayLink(l.ID); err != nil {
		t.Errorf("a reactivated link under its limit: %v", err)
	}
	if got := a.link(l.ID); got.Active {
		t.Errorf("the third payment of three left the link active: %+v", got)
	}
}

func TestALinkLimitOnUpdateAndWhatIsRefused(t *testing.T) {
	f, _, a := newFake(t)

	// No limit: a link takes any number of payments and says null.
	free := a.newLink(oneLineLink(5000, "usd"))
	if free.Restrictions != nil {
		t.Errorf("an unlimited link: %+v", free.Restrictions)
	}
	if got := restrictionsJSON(t, a.get("/v1/payment_links/"+free.ID, nil)); got != "null" {
		t.Errorf("an unlimited link's restrictions: %s", got)
	}
	for range 3 {
		if _, err := f.PayLink(free.ID); err != nil {
			t.Fatal(err)
		}
	}
	if got := a.link(free.ID); !got.Active || got.Restrictions != nil {
		t.Errorf("after three payments: %+v", got)
	}

	// A limit set by update is measured against the sessions the link has completed (the fake's
	// reading of the count); one already reached deactivates the link.
	up := a.post("/v1/payment_links/"+free.ID, url.Values{"restrictions[completed_sessions][limit]": {"5"}}, "")
	var got testprovider.PaymentLink
	up.into(t, &got)
	if limit, done, ok := count(got); up.status != 200 || !ok || limit != 5 || done != 3 || !got.Active {
		t.Fatalf("update: %d %s", up.status, up.raw)
	}

	// A refused limit refuses the whole request, on create and on update.
	for _, bad := range []string{"0", "-1", "x", "01", "1.5", ""} {
		form := oneLineLink(5000, "usd")
		form.Set("restrictions[completed_sessions][limit]", bad)
		before := len(f.Links())
		if ans := a.post("/v1/payment_links", form, ""); ans.status != http.StatusBadRequest {
			t.Errorf("create with limit %q: %d", bad, ans.status)
		}
		if len(f.Links()) != before {
			t.Errorf("a refused create with limit %q made a link", bad)
		}
		active := url.Values{"active": {"false"}, "restrictions[completed_sessions][limit]": {bad}}
		if ans := a.post("/v1/payment_links/"+free.ID, active, ""); ans.status != http.StatusBadRequest {
			t.Errorf("update with limit %q: %d", bad, ans.status)
		}
		if l, _ := f.Link(free.ID); !l.Active {
			t.Errorf("a refused update with limit %q changed the link", bad)
		}
	}
	// Another parameter of the family stays unknown.
	form := oneLineLink(5000, "usd")
	form.Set("restrictions[completed_sessions][count]", "1")
	if ans := a.post("/v1/payment_links", form, ""); ans.status != http.StatusBadRequest {
		t.Errorf("a count on create: %d", ans.status)
	} else if _, code := ans.errorCode(); code != "parameter_unknown" {
		t.Errorf("code %q", code)
	}

	if _, err := f.PayLink("plink_nope"); !errors.Is(err, testprovider.ErrNoSuchLink) {
		t.Errorf("an unknown link: %v", err)
	}
}

func TestALinkWhoseNewLimitWasReachedDeactivates(t *testing.T) {
	f, _, a := newFake(t)
	form := oneLineLink(5000, "usd")
	form.Set("restrictions[completed_sessions][limit]", "5")
	l := a.newLink(form)
	for range 2 {
		if _, err := f.PayLink(l.ID); err != nil {
			t.Fatal(err)
		}
	}
	a.post("/v1/payment_links/"+l.ID, url.Values{"restrictions[completed_sessions][limit]": {"2"}}, "")
	if got := a.link(l.ID); got.Active {
		t.Errorf("a limit already reached left the link active: %+v", got)
	}
}

// The minimums are Stripe's, copied here from its documentation (amounts in the smallest unit):
// https://docs.stripe.com/currencies, "Minimum charge amount by currency".
var documentedMinimums = map[string]int{
	"usd": 50, "aed": 200, "ars": 50, "aud": 50, "brl": 50, "cad": 50, "chf": 50, "cop": 50,
	"czk": 1500, "dkk": 250, "eur": 50, "gbp": 30, "hkd": 400, "huf": 17500, "idr": 50, "ils": 50,
	"inr": 50, "jpy": 50, "krw": 50, "mxn": 1000, "myr": 200, "nok": 300, "nzd": 50, "php": 50,
	"pln": 200, "ron": 200, "rub": 50, "sek": 300, "sgd": 50, "thb": 1000, "zar": 50,
}

func TestEachCurrencyHasItsDocumentedMinimum(t *testing.T) {
	_, _, a := newFake(t)
	for cur, min := range documentedMinimums {
		below := a.post("/v1/checkout/sessions", sessionForm(min-1, cur, "r"), "")
		if _, code := below.errorCode(); below.status != http.StatusBadRequest || code != "amount_too_small" {
			// RUB and a few others aren't two-decimal in the fake's list: they must be known.
			t.Errorf("%s %d: %d %s", cur, min-1, below.status, below.raw)
		}
		at := a.post("/v1/checkout/sessions", sessionForm(min, cur, "r"), "")
		if at.status != http.StatusOK {
			t.Errorf("%s %d: %d %s", cur, min, at.status, at.raw)
		}
		// A Payment Link follows the same rule.
		if ans := a.post("/v1/payment_links", oneLineLink(min-1, cur), ""); ans.status != http.StatusBadRequest {
			t.Errorf("link %s %d: %d", cur, min-1, ans.status)
		}
		if ans := a.post("/v1/payment_links", oneLineLink(min, cur), ""); ans.status != http.StatusOK {
			t.Errorf("link %s %d: %d %s", cur, min, ans.status, ans.raw)
		}
	}
	// The examples of the documentation, by name: GBP 0.30 and HKD 4.00 where USD is 0.50.
	for _, c := range []struct {
		cur string
		n   int
		ok  bool
	}{{"gbp", 29, false}, {"gbp", 30, true}, {"hkd", 399, false}, {"hkd", 400, true}, {"usd", 49, false}, {"usd", 50, true}} {
		if got := a.post("/v1/checkout/sessions", sessionForm(c.n, c.cur, "r"), "").status == http.StatusOK; got != c.ok {
			t.Errorf("%s %d: accepted %v", c.cur, c.n, got)
		}
	}
	// A charge may have any amount in HUF and TWD: whole units are a rule of their payouts, which
	// the fake doesn't have.
	for _, c := range []struct {
		cur string
		n   int
	}{{"huf", 17501}, {"huf", 123456}, {"twd", 12345}} {
		if ans := a.post("/v1/checkout/sessions", sessionForm(c.n, c.cur, "r"), ""); ans.status != http.StatusOK {
			t.Errorf("%s %d: %d %s", c.cur, c.n, ans.status, ans.raw)
		}
	}
}

func TestUGXAndISKAreWholeUnitsWrittenWithTwoDecimals(t *testing.T) {
	_, _, a := newFake(t)
	for _, cur := range []string{"ugx", "isk"} {
		for n, ok := range map[int]bool{500: true, 100: true, 12300: true, 501: false, 550: false, 99: false, 10001: false} {
			form := sessionForm(n, cur, "r")
			ans := a.post("/v1/checkout/sessions", form, "")
			if (ans.status == http.StatusOK) != ok {
				t.Errorf("%s %d: %d %s", cur, n, ans.status, ans.raw)
			}
			if !ok && ans.status == http.StatusBadRequest {
				if typ, _ := ans.errorCode(); typ != "invalid_request_error" || !strings.Contains(string(ans.raw), "unit_amount") {
					t.Errorf("%s %d: %s", cur, n, ans.raw)
				}
			}
			link := a.post("/v1/payment_links", oneLineLink(n, cur), "")
			if (link.status == http.StatusOK) != ok {
				t.Errorf("link %s %d: %d %s", cur, n, link.status, link.raw)
			}
		}
	}
	// A line of 500 UGX twice is fine; the rule is the line's, and a sum of whole lines is whole.
	form := sessionForm(500, "ugx", "r")
	form.Set("line_items[0][quantity]", "3")
	if ans := a.post("/v1/checkout/sessions", form, ""); ans.status != http.StatusOK {
		t.Errorf("a quantity of 3: %d %s", ans.status, ans.raw)
	}
	// JPY stays zero-decimal: 501 yen is a fine amount.
	if ans := a.post("/v1/checkout/sessions", sessionForm(501, "jpy", "r"), ""); ans.status != http.StatusOK {
		t.Errorf("501 JPY: %d %s", ans.status, ans.raw)
	}
}

func TestEachFakeHasItsOwnPayer(t *testing.T) {
	const sessions = 25
	type side struct {
		f   *testprovider.Stripe
		ids []string
	}
	build := func(opts ...testprovider.Option) *side {
		f, _, a := newFake(t, opts...)
		s := &side{f: f}
		for i := range sessions {
			s.ids = append(s.ids, a.newSession(12000, "eur", fmt.Sprintf("r%d", i)).ID)
		}
		return s
	}
	one, two := build(), build(testprovider.WithPayer(testprovider.CustomerDetails{Email: "w@example.test", Name: "W"}))
	if got := one.f.Payer(); got != testprovider.DefaultPayer() {
		t.Errorf("a fake's payer is %+v", got)
	}
	if got := two.f.Payer().Email; got != "w@example.test" {
		t.Errorf("WithPayer: %q", got)
	}
	// Both fakes pay their sessions at once, each changing its own payer between payments.
	var wg sync.WaitGroup
	for n, s := range []*side{one, two} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i, id := range s.ids {
				who := testprovider.CustomerDetails{Email: fmt.Sprintf("p%d-%d@example.test", n, i), Name: fmt.Sprintf("Payer %d/%d", n, i)}
				s.f.SetPayer(who)
				evs, err := s.f.Pay(id)
				if err != nil {
					t.Errorf("fake %d: %v", n, err)
					return
				}
				got, ok := s.f.Session(id)
				if !ok || got.CustomerDetails == nil || *got.CustomerDetails != who {
					t.Errorf("fake %d session %d: %+v, want %+v", n, i, got.CustomerDetails, who)
				}
				for _, e := range evs {
					if e.Type != testprovider.EventChargeSucceeded {
						continue
					}
					var ch struct {
						Billing struct{ Email, Name string } `json:"billing_details"`
					}
					if err := json.Unmarshal(e.Data.Object, &ch); err != nil || ch.Billing.Email != who.Email {
						t.Errorf("fake %d charge %d: %+v %v", n, i, ch.Billing, err)
					}
				}
			}
		}()
	}
	wg.Wait()
	// Neither fake's payer reached the other's, and a fresh fake has the default one.
	if got := one.f.Payer().Email; !strings.HasPrefix(got, "p0-") {
		t.Errorf("fake one's payer: %q", got)
	}
	if got := two.f.Payer().Email; !strings.HasPrefix(got, "p1-") {
		t.Errorf("fake two's payer: %q", got)
	}
	f3, _, a3 := newFake(t)
	s3 := a3.newSession(12000, "eur", "late")
	if _, err := f3.Pay(s3.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := f3.Session(s3.ID); got.CustomerDetails == nil || *got.CustomerDetails != testprovider.DefaultPayer() {
		t.Errorf("a new fake's payer: %+v", got.CustomerDetails)
	}
	// A delayed payment gives the payer's details too.
	d := a3.newSession(12000, "eur", "delayed")
	f3.SetPayer(testprovider.CustomerDetails{Email: "d@example.test"})
	if _, err := f3.PayDelayed(d.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := f3.Session(d.ID); got.CustomerDetails == nil || got.CustomerDetails.Email != "d@example.test" {
		t.Errorf("delayed: %+v", got.CustomerDetails)
	}
}

// A payer who opens a hosted payment link's address with ?client_reference_id= makes a session that
// carries it, as Stripe's documented URL parameter does; a value Stripe doesn't take is left out, and
// opening an address that isn't a link's takes no payment.
func TestAPayerOpeningAHostedLinkAddressBringsItsClientReferenceId(t *testing.T) {
	f, _, a := newFake(t)
	l := a.newLink(oneLineLink(5000, "usd"))

	completed := func(evs []testprovider.Event) testprovider.Session {
		t.Helper()
		for _, e := range evs {
			if e.Type == testprovider.EventSessionCompleted {
				var ss testprovider.Session
				if err := json.Unmarshal(e.Data.Object, &ss); err != nil {
					t.Fatal(err)
				}
				return ss
			}
		}
		t.Fatal("no completed session")
		return testprovider.Session{}
	}

	evs, err := f.PayLinkAt(l.URL + "?client_reference_id=inv_A-1")
	if err != nil {
		t.Fatal(err)
	}
	if ss := completed(evs); ss.ClientReferenceID == nil || *ss.ClientReferenceID != "inv_A-1" || ss.PaymentLink == nil || *ss.PaymentLink != l.ID {
		t.Errorf("the session of a payer who came by the address: %+v", ss)
	}

	// Without the parameter, and with one Stripe doesn't take, the session has none.
	for name, address := range map[string]string{"none": l.URL, "a space in it": l.URL + "?client_reference_id=a%20b", "a long one": l.URL + "?client_reference_id=" + strings.Repeat("x", 201)} {
		evs, err := f.PayLinkAt(address)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if ss := completed(evs); ss.ClientReferenceID != nil {
			t.Errorf("%s: the session has the reference %q", name, *ss.ClientReferenceID)
		}
	}
	if _, err := f.PayLinkAt("https://buy.stripe.com/nothing?client_reference_id=x"); !errors.Is(err, testprovider.ErrNoSuchLink) {
		t.Errorf("an address that isn't a link's: %v", err)
	}
}
