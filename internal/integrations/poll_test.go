package integrations

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/integrations/testprovider"
)

// ----- helpers -----

// pollNow runs one pass of the polling worker and then the event worker, as the two run in the server.
func (p *pay) pollNow() {
	p.t.Helper()
	p.svc.pollPass(context.Background())
	p.drain()
}

// due moves the clock past the first wait of every ref made so far.
func (p *pay) due() { p.later(pollFirst + time.Second) }

// mark is where the fake's request log stands now; readsSince returns the requests made after it.
func (p *pay) mark() int { return len(p.fake.Requests()) }

func (p *pay) requestsSince(mark int) []testprovider.Request { return p.fake.Requests()[mark:] }

// readsSince are the requests made after the mark. It fails if any of them isn't a read: the polling
// never makes, changes or deletes anything at the provider.
func (p *pay) readsSince(mark int) []testprovider.Request {
	p.t.Helper()
	reqs := p.requestsSince(mark)
	for _, r := range reqs {
		if r.Method != http.MethodGet {
			p.t.Fatalf("the polling sent %s %s", r.Method, r.Path)
		}
	}
	return reqs
}

type polledEvent struct {
	provider, source, kind, state, code, object string
}

func (p *pay) polledEvents() []polledEvent {
	p.t.Helper()
	rows, err := p.db.Query(`SELECT provider_event_id, source, kind, state, coalesce(error_code, ''), coalesce(object_id, '') FROM integration_events ORDER BY id`)
	if err != nil {
		p.t.Fatal(err)
	}
	defer rows.Close()
	var out []polledEvent
	for rows.Next() {
		var e polledEvent
		if err := rows.Scan(&e.provider, &e.source, &e.kind, &e.state, &e.code, &e.object); err != nil {
			p.t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func (p *pay) refPolls(id int64) (polls int, next string) {
	p.t.Helper()
	var n *string
	if err := p.db.QueryRow(`SELECT polls, next_poll_at FROM integration_refs WHERE id = ?`, id).Scan(&polls, &n); err != nil {
		p.t.Fatal(err)
	}
	if n != nil {
		next = *n
	}
	return polls, next
}

// openRef adds a ref that is open at the provider's, as far as the server knows: an object of the mode
// that the fake has no such session of, so its read finds nothing, but is made.
func (p *pay) openRef(record int64, mode string, age time.Duration) (id int64, object string) {
	p.t.Helper()
	object = "cs_" + mode + "_" + token()
	tok, err := newToken()
	if err != nil {
		p.t.Fatal(err)
	}
	created := p.svc.now().Add(-age).UTC().Format(timeFormat)
	p.exec(`INSERT INTO integration_refs (token, integration_id, app_id, record_id, slot, provider_object_id, url, amount_minor, currency,
			state, created_at, account_id, account_mode, success_url, cancel_url)
		VALUES (?, ?, ?, ?, 'payments', ?, 'https://checkout.stripe.com/c/pay/x', 10000, 'EUR', 'open', ?, 'acct_x', ?, ?, ?)`,
		tok, p.in.ID, p.app.ID, record, object, created, mode, publicURL+PaymentDonePath, publicURL+PaymentCancelledPath)
	if err := p.db.QueryRow(`SELECT id FROM integration_refs WHERE token = ?`, tok).Scan(&id); err != nil {
		p.t.Fatal(err)
	}
	return id, object
}

// ----- the schedule -----

func TestPollWaitsDoubleToAMaximum(t *testing.T) {
	for _, c := range []struct {
		reads  int
		hooked bool
		want   time.Duration
	}{
		{0, false, time.Minute}, {1, false, 2 * time.Minute}, {2, false, 4 * time.Minute}, {4, false, 16 * time.Minute},
		{5, false, pollMax}, {50, false, pollMax}, {-1, false, time.Minute}, {1 << 30, false, pollMax},
		{0, true, pollFirstHooked}, {2, true, 2 * time.Hour}, {3, true, 4 * time.Hour}, {4, true, pollMaxHooked}, {50, true, pollMaxHooked},
	} {
		if got := pollWait(c.reads, c.hooked); got != c.want {
			t.Errorf("pollWait(%d, %v) = %v, want %v", c.reads, c.hooked, got, c.want)
		}
	}
}

// ----- a payment found by polling is applied once -----

func TestPollingMarksAnInvoicePaidWithoutAnyWebhook(t *testing.T) {
	p := newPay(t, publicURL)
	record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	p.fake.SetAccount("acct_1PollTest") // whatever the fake says of itself is no concern of the poll
	if _, err := p.fake.Pay(ref.object); err != nil {
		t.Fatal(err) // paid at Stripe; no event is delivered to the server
	}
	mark := p.mark()

	// Not due yet: a ref is first read a minute after it was made.
	p.pollNow()
	if n := len(p.readsSince(mark)); n != 0 {
		t.Fatalf("%d reads before the first wait ran out", n)
	}
	if p.fields(record)["status"] != "sent" {
		t.Fatal("the invoice moved by itself")
	}

	p.due()
	p.pollNow()
	reads := p.readsSince(mark)
	if len(reads) != 1 || reads[0].Path != "/v1/checkout/sessions/"+ref.object || reads[0].Auth != "bearer" {
		t.Fatalf("reads = %+v", reads)
	}
	if got := p.fields(record); got["status"] != "paid" || got["paid_at"] == nil {
		t.Fatalf("the invoice is %v", got)
	}
	if r := p.refOf(record); r.state != "paid" {
		t.Fatalf("ref = %+v", r)
	}
	evs := p.polledEvents()
	if len(evs) != 1 || evs[0].source != SourcePoll || evs[0].provider != "poll:"+ref.object+":paid" || evs[0].kind != EventPaymentSucceeded || evs[0].state != EventApplied {
		t.Fatalf("events = %+v", evs)
	}
	// A poll says nothing of whether the webhook arrives: the admins' "last event" stays as it was.
	var last *string
	if err := p.db.QueryRow(`SELECT last_event_at FROM integrations WHERE id = ?`, p.in.ID).Scan(&last); err != nil || last != nil {
		t.Fatalf("last_event_at = %v, %v", last, err)
	}
	// A paid ref is never read again.
	rev := p.revision(record)
	p.later(24 * time.Hour)
	p.pollNow()
	if n := len(p.readsSince(mark)); n != 1 || p.revision(record) != rev {
		t.Fatalf("%d reads after the ref was paid", n)
	}
}

// A polled payment is dated when it was paid, as its webhook event is, and not when the poll found it:
// a payment made at 23:59:30 and found at 00:00:10 is paid on the day it was made. Both shapes are read
// with the provider's time of the payment in the same request.
func TestAPolledPaymentIsDatedWhenItWasPaidNotWhenItWasFound(t *testing.T) {
	for _, public := range []string{publicURL, ""} {
		name := map[string]string{publicURL: "a checkout session", "": "a payment link"}[public]
		t.Run(name, func(t *testing.T) {
			p := newPayOnOneClock(t, public)
			// The invoice is sent at 23:50:00 UTC of a day to come.
			now := p.svc.now().UTC()
			midnight := now.Truncate(24 * time.Hour).Add(24 * time.Hour)
			if midnight.Sub(now) < 15*time.Minute {
				midnight = midnight.Add(24 * time.Hour)
			}
			p.later(midnight.Add(-10 * time.Minute).Sub(now))
			record, ref := p.sentInvoice(100, "EUR", "op-a-0001")

			p.later(midnight.Add(-30 * time.Second).Sub(p.svc.now())) // 23:59:30
			paidAt := p.svc.now().UTC().Truncate(time.Second)
			var err error
			if public != "" {
				_, err = p.fake.Pay(ref.object)
			} else {
				_, err = p.fake.PayLink(ref.object)
			}
			if err != nil {
				t.Fatal(err)
			}
			p.later(40 * time.Second) // 00:00:10, the next day
			mark := p.mark()
			p.pollNow()
			if n := len(p.readsSince(mark)); n != 1 {
				t.Fatalf("%d reads", n)
			}
			got := p.fields(record)
			if got["status"] != "paid" || got["paid_at"] != paidAt.Format(time.DateOnly) {
				t.Fatalf("the invoice paid at %v and found at %v is %v", paidAt, p.svc.now().UTC(), got)
			}
			var occurred string
			if err := p.db.QueryRow(`SELECT occurred_at FROM integration_events WHERE source = ?`, SourcePoll).Scan(&occurred); err != nil {
				t.Fatal(err)
			}
			if at, err := time.Parse(timeFormat, occurred); err != nil || !at.Equal(paidAt) {
				t.Fatalf("the polled event occurred at %q, the payment was made at %v (%v)", occurred, paidAt, err)
			}
		})
	}
}

// Without a public address the server's links are Payment Links, whose payments are sessions of the link:
// this is the case polling is for, since no webhook can reach the server.
func TestPollingFindsAPaymentThroughAPaymentLinkWithNoPublicURL(t *testing.T) {
	p := newPay(t, "")
	record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	if !strings.HasPrefix(ref.object, "plink_") {
		t.Fatalf("the ref's object is %q", ref.object)
	}
	if _, err := p.fake.PayLink(ref.object); err != nil {
		t.Fatal(err)
	}
	mark := p.mark()

	p.due()
	p.pollNow()
	reads := p.readsSince(mark)
	if len(reads) != 1 || reads[0].Path != "/v1/checkout/sessions" || reads[0].Query.Get("payment_link") != ref.object || reads[0].Query.Get("status") != "complete" {
		t.Fatalf("reads = %+v", reads)
	}
	if p.fields(record)["status"] != "paid" || p.refOf(record).state != "paid" {
		t.Fatalf("the invoice is %v, ref %+v", p.fields(record), p.refOf(record))
	}
	evs := p.polledEvents()
	if len(evs) != 1 || evs[0].source != SourcePoll || evs[0].state != EventApplied || !strings.HasPrefix(evs[0].object, "cs_") {
		t.Fatalf("events = %+v", evs)
	}
	var paidObject string
	if err := p.db.QueryRow(`SELECT paid_object_id FROM integration_refs WHERE id = ?`, ref.id).Scan(&paidObject); err != nil || paidObject != evs[0].object {
		t.Fatalf("paid object %q, %v", paidObject, err)
	}
	// The one-use link was paid through its own session: it is used up, and the money is free again.
	if yes, _, err := p.svc.MayBePaid(context.Background(), p.db, p.app.ID, record); err != nil || yes {
		t.Fatalf("the paid link may still be paid: %v, %v", yes, err)
	}
}

// A link nobody has paid is read, and nothing changes.
func TestPollingAnUnpaidLinkChangesNothingAndWaitsLonger(t *testing.T) {
	p := newPay(t, publicURL)
	record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	before, rev := p.fields(record), p.revision(record)
	mark := p.mark()

	p.due()
	p.pollNow()
	if n := len(p.readsSince(mark)); n != 1 {
		t.Fatalf("%d reads", n)
	}
	if polls, next := p.refPolls(ref.id); polls != 1 || next == "" {
		t.Fatalf("polls %d, next %q", polls, next)
	}
	if fmt.Sprint(p.fields(record)) != fmt.Sprint(before) || p.revision(record) != rev || p.refOf(record).state != "open" || len(p.polledEvents()) != 0 {
		t.Fatal("something changed for a payment nobody made")
	}
}

func TestAPaymentTheWebhookAndThePollBothSeeIsAppliedExactlyOnce(t *testing.T) {
	applied := func(p *pay) (n int, rest []polledEvent) {
		for _, e := range p.polledEvents() {
			if e.kind != EventPaymentSucceeded {
				continue
			}
			if e.state == EventApplied {
				n++
			} else {
				rest = append(rest, e)
			}
		}
		return n, rest
	}
	t.Run("both are stored before the worker runs", func(t *testing.T) {
		p := newPay(t, publicURL)
		record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
		revBefore, historyBefore := p.revision(record), p.historyRows(record)
		evs, err := p.fake.Pay(ref.object)
		if err != nil {
			t.Fatal(err)
		}
		p.deliver(p.sender(p.hookID(), stripeHook), evs) // stored, not applied yet
		p.due()
		p.svc.pollPass(context.Background()) // the poll stores its own
		p.drain()

		n, rest := applied(p)
		if n != 1 || len(rest) != 1 || rest[0].state != EventIgnored || rest[0].code != string(apierr.CodeIntegrationEventDuplicate) {
			t.Fatalf("applied %d, the others %+v, all %+v", n, rest, p.polledEvents())
		}
		if p.revision(record) != revBefore+1 || p.historyRows(record) != historyBefore+1 || p.fields(record)["status"] != "paid" {
			t.Fatalf("the record was written %d times", p.revision(record)-revBefore)
		}
	})
	t.Run("the poll first, then the webhook", func(t *testing.T) {
		p := newPay(t, publicURL)
		record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
		evs, err := p.fake.Pay(ref.object)
		if err != nil {
			t.Fatal(err)
		}
		p.due()
		p.pollNow()
		if p.fields(record)["status"] != "paid" {
			t.Fatal("the poll didn't apply it")
		}
		rev, history := p.revision(record), p.historyRows(record)
		p.deliver(p.sender(p.hookID(), stripeHook), evs)
		p.drain()

		n, rest := applied(p)
		if n != 1 || len(rest) != 1 || rest[0].code != string(apierr.CodeIntegrationEventDuplicate) {
			t.Fatalf("applied %d, the others %+v", n, rest)
		}
		if p.revision(record) != rev || p.historyRows(record) != history {
			t.Fatal("the record was written again")
		}
	})
	t.Run("the webhook first, and the poll reads nothing more", func(t *testing.T) {
		p := newPay(t, publicURL)
		record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
		p.pays(ref.object)
		rev := p.revision(record)
		mark := p.mark()
		p.due()
		p.pollNow()
		if n := len(p.readsSince(mark)); n != 0 {
			t.Fatalf("%d reads of a paid ref", n)
		}
		if n, rest := applied(p); n != 1 || len(rest) != 0 || p.revision(record) != rev {
			t.Fatalf("applied %d, the others %+v", n, rest)
		}
	})
	t.Run("a payment told twice by the poll", func(t *testing.T) {
		// A poll that finds the same payment again (its first note of the read was lost) stores the same
		// event: the unique id, then the ref's state, make it once.
		p := newPay(t, publicURL)
		record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
		if _, err := p.fake.Pay(ref.object); err != nil {
			t.Fatal(err)
		}
		p.due()
		p.svc.pollPass(context.Background())
		p.exec(`UPDATE integration_refs SET next_poll_at = ? WHERE id = ?`, p.svc.now().Add(-time.Second).UTC().Format(timeFormat), ref.id)
		p.svc.pollPass(context.Background())
		p.drain()
		if evs := p.polledEvents(); len(evs) != 1 || evs[0].state != EventApplied {
			t.Fatalf("events = %+v", evs)
		}
		if p.fields(record)["status"] != "paid" {
			t.Fatal("not paid")
		}
	})
}

// A polled payment is checked as a webhook's is: the apply step is the same.
func TestAPolledPaymentOfAnotherAmountIsNotAppliedAndIsNotToldAgain(t *testing.T) {
	p := newPay(t, publicURL)
	record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	if _, err := p.fake.Pay(ref.object); err != nil {
		t.Fatal(err)
	}
	// The ref asked for something else than what was paid.
	p.exec(`UPDATE integration_refs SET amount_minor = 9000 WHERE id = ?`, ref.id)
	before := p.fields(record)
	p.due()
	p.pollNow()

	evs := p.polledEvents()
	if len(evs) != 1 || evs[0].state != EventFailed || evs[0].code != string(apierr.CodeIntegrationAmountMismatch) {
		t.Fatalf("events = %+v", evs)
	}
	if fmt.Sprint(p.fields(record)) != fmt.Sprint(before) || p.refOf(record).state != "open" {
		t.Fatal("the record or its ref moved")
	}
	if all, nonAdmin := p.adminNotices(); all == 0 || nonAdmin != 0 {
		t.Fatalf("the admins were told %d times, %d others", all, nonAdmin)
	}
	// Read again later, the same payment is the same event: no second failure, no second notice.
	notices := func() int { all, _ := p.adminNotices(); return all }
	before2 := notices()
	for range 3 {
		p.later(2 * time.Hour)
		p.pollNow()
	}
	if len(p.polledEvents()) != 1 || notices() != before2 {
		t.Fatalf("events %+v, notices %d then %d", p.polledEvents(), before2, notices())
	}
}

func TestPollingEndsARefWhoseLinkExpired(t *testing.T) {
	p := newPay(t, publicURL)
	record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	before := p.fields(record)
	if _, err := p.fake.Expire(ref.object); err != nil {
		t.Fatal(err)
	}
	mark := p.mark()
	p.due()
	p.pollNow()

	if r := p.refOf(record); r.state != "expired" {
		t.Fatalf("ref = %+v", r)
	}
	evs := p.polledEvents()
	if len(evs) != 1 || evs[0].kind != EventPaymentExpired || evs[0].source != SourcePoll || evs[0].provider != "poll:"+ref.object+":expired" {
		t.Fatalf("events = %+v", evs)
	}
	// The app declared nothing for an expired link: the ref ends and the record is left alone.
	if fmt.Sprint(p.fields(record)) != fmt.Sprint(before) {
		t.Fatalf("the invoice changed: %v", p.fields(record))
	}
	reads := len(p.readsSince(mark))
	p.later(48 * time.Hour)
	p.pollNow()
	if n := len(p.readsSince(mark)); n != reads {
		t.Fatalf("an ended ref was read again (%d reads, then %d)", reads, n)
	}
}

// ----- bounded and polite -----

func TestPollingBacksOffPerRefAndStopsAtAMaximumAge(t *testing.T) {
	p := newPay(t, "") // no webhooks arrive: the poll is the only way a payment is found
	_, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	mark := p.mark()
	reads := func() int { return len(p.readsSince(mark)) }

	p.due()
	p.pollNow()
	p.pollNow() // nothing is due a moment later
	if reads() != 1 {
		t.Fatalf("%d reads", reads())
	}
	p.later(pollWait(0, false) + time.Second) // the second read comes after a minute
	p.pollNow()
	if reads() != 2 {
		t.Fatalf("%d reads", reads())
	}
	p.later(pollWait(1, false) - time.Second) // and the third after two
	p.pollNow()
	if reads() != 2 {
		t.Fatalf("%d reads, the third came early", reads())
	}
	p.later(2 * time.Second)
	p.pollNow()
	if reads() != 3 {
		t.Fatalf("%d reads", reads())
	}
	if polls, _ := p.refPolls(ref.id); polls != 3 {
		t.Fatalf("polls = %d", polls)
	}
	// Past its maximum age a ref is read no more, though it is still open.
	p.later(maxPollAge)
	p.pollNow()
	p.later(pollMax + time.Second)
	p.pollNow()
	if reads() != 3 {
		t.Fatalf("%d reads after the maximum age", reads())
	}
	if p.refOf(ref.record).state != "open" {
		t.Fatal("the ref was ended by the age")
	}
}

// Where the provider's webhooks arrive as well, the poll is a safety net and reads rarely.
func TestPollingWhereWebhooksArriveIsASlowSafetyNet(t *testing.T) {
	p := newPay(t, publicURL)
	record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	if _, err := p.fake.Pay(ref.object); err != nil {
		t.Fatal(err) // the webhook never arrives
	}
	mark := p.mark()
	p.due()
	p.pollNow() // the first read comes after a minute whatever the install
	if len(p.readsSince(mark)) != 1 || p.fields(record)["status"] != "paid" {
		t.Fatalf("reads %d, invoice %v", len(p.readsSince(mark)), p.fields(record))
	}
	// A second ref: after its first read it waits half an hour.
	p.sentInvoice(100, "EUR", "op-b-0001")
	mark = p.mark()
	p.due()
	p.pollNow()
	if n := len(p.readsSince(mark)); n != 1 {
		t.Fatalf("%d reads of the second ref", n)
	}
	p.later(pollFirstHooked - time.Minute)
	p.pollNow()
	if n := len(p.readsSince(mark)); n != 1 {
		t.Fatalf("a hooked install read again after %v", pollFirstHooked-time.Minute)
	}
	p.later(2 * time.Minute)
	p.pollNow()
	if n := len(p.readsSince(mark)); n != 2 {
		t.Fatalf("%d reads, want 2", n)
	}
}

func TestPollingKeepsToAnIntegrationsBudgetOfReadsAMinute(t *testing.T) {
	p := newPay(t, publicURL)
	record, _ := p.sentInvoice(100, "EUR", "op-a-0001")
	var extra []int64
	for range pollCallsPerMinute + 4 { // more open refs than a minute's reads
		id, _ := p.openRef(record, ModeTest, time.Hour)
		extra = append(extra, id)
	}
	mark := p.mark()
	p.pollNow()
	if n := len(p.readsSince(mark)); n != pollCallsPerMinute {
		t.Fatalf("%d reads in one pass, the budget is %d", n, pollCallsPerMinute)
	}
	p.pollNow() // the same minute
	if n := len(p.readsSince(mark)); n != pollCallsPerMinute {
		t.Fatalf("%d reads in the same minute", n)
	}
	p.later(time.Minute + time.Second)
	p.pollNow()
	n := len(p.readsSince(mark))
	if n <= pollCallsPerMinute || n > 2*pollCallsPerMinute {
		t.Fatalf("%d reads after the minute", n)
	}
	// The refs that waited longest were read first: every one has been read by now.
	for _, id := range extra {
		if polls, _ := p.refPolls(id); polls == 0 {
			t.Errorf("ref %d was never read", id)
		}
	}
}

func TestPollingHonoursARateLimitOfTheProvider(t *testing.T) {
	p := newPay(t, publicURL)
	record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	p.openRef(record, ModeTest, time.Hour)
	p.openRef(record, ModeTest, time.Hour)
	p.fake.Fail(testprovider.RateLimited(5*time.Minute).On(http.MethodGet, "/v1/checkout/sessions"))
	mark := p.mark()

	p.due()
	p.pollNow()
	reads := p.readsSince(mark)
	if len(reads) != 1 || reads[0].Status != http.StatusTooManyRequests {
		t.Fatalf("the first pass made %d reads: %+v", len(reads), reads)
	}
	// Nothing of the integration is read while Stripe asked to wait, though its other refs are due.
	p.later(2 * time.Minute)
	p.pollNow()
	p.later(2 * time.Minute)
	p.pollNow()
	if n := len(p.readsSince(mark)); n != 1 {
		t.Fatalf("%d reads inside the wait Stripe asked for", n)
	}
	// A refused read is no read of the ref: it waits as it did.
	for _, id := range []int64{ref.id} {
		if polls, _ := p.refPolls(id); polls != 0 {
			t.Errorf("ref %d counts %d reads", id, polls)
		}
	}
	p.later(2 * time.Minute) // five minutes passed
	p.pollNow()
	if n := len(p.readsSince(mark)); n < 2 {
		t.Fatalf("%d reads after the wait", n)
	}
}

func TestAnAnswerWithNoWaitNamedStillHoldsTheIntegrationAMinute(t *testing.T) {
	p := newPay(t, publicURL)
	record, _ := p.sentInvoice(100, "EUR", "op-a-0001")
	p.openRef(record, ModeTest, time.Hour)
	p.fake.Fail(testprovider.RateLimited(0).On(http.MethodGet, "/v1/checkout/sessions"))
	mark := p.mark()
	p.due()
	p.pollNow()
	p.later(pollRateWait / 2)
	p.pollNow()
	if n := len(p.readsSince(mark)); n != 1 {
		t.Fatalf("%d reads", n)
	}
	p.later(pollRateWait)
	p.pollNow()
	if n := len(p.readsSince(mark)); n < 2 {
		t.Fatalf("%d reads", n)
	}
}

// ----- what is switched off is not read -----

func TestPollingStopsForADisabledIntegrationAndGoesOnWhenItIsEnabled(t *testing.T) {
	p := newPay(t, publicURL)
	record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	if _, err := p.fake.Pay(ref.object); err != nil {
		t.Fatal(err)
	}
	p.exec(`UPDATE integrations SET disabled_at = ? WHERE id = ?`, time.Now().UTC().Format(timeFormat), p.in.ID)
	mark := p.mark()
	p.due()
	p.pollNow()
	p.later(time.Hour)
	p.pollNow()
	if n := len(p.requestsSince(mark)); n != 0 || p.fields(record)["status"] != "sent" || len(p.polledEvents()) != 0 {
		t.Fatalf("%d requests for a disabled integration, invoice %v", n, p.fields(record))
	}
	p.exec(`UPDATE integrations SET disabled_at = NULL WHERE id = ?`, p.in.ID)
	p.pollNow()
	if p.fields(record)["status"] != "paid" {
		t.Fatal("the payment wasn't found once the integration was on again")
	}
}

func TestAnEventFoundForAnIntegrationSwitchedOffMeanwhileIsNotStored(t *testing.T) {
	p := newPay(t, publicURL)
	_, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	rec, err := p.svc.store.getSnapshot(context.Background(), p.ws, p.in.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The poll read the payment, and the integration was switched off before it stored what it found.
	p.exec(`UPDATE integrations SET disabled_at = ? WHERE id = ?`, time.Now().UTC().Format(timeFormat), p.in.ID)
	r, _, err := p.svc.refByToken(context.Background(), ref.token)
	if err != nil {
		t.Fatal(err)
	}
	p.svc.pollTell(context.Background(), rec, r, Event{ID: "poll:" + ref.object + ":paid", Kind: EventPaymentSucceeded, ObjectID: ref.object,
		AmountMinor: ref.amount, Currency: ref.currency})
	if n := len(p.polledEvents()); n != 0 {
		t.Fatalf("%d events stored for a disabled integration", n)
	}
}

func TestPollingStopsWhenTheAppsApprovalIsRevoked(t *testing.T) {
	p := newPay(t, publicURL)
	record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	if _, err := p.fake.Pay(ref.object); err != nil {
		t.Fatal(err)
	}
	expect(t, p.owner.do(http.MethodPost, wsPath(p.ws, fmt.Sprintf("integration-approvals/%d/revoke", p.approval.ID)), map[string]any{}), http.StatusOK)
	mark := p.mark()
	p.due()
	p.pollNow()
	if n := len(p.requestsSince(mark)); n != 0 {
		t.Fatalf("%d requests for an app whose approval was revoked", n)
	}
	if p.fields(record)["status"] != "sent" || p.refOf(record).state != "open" || len(p.polledEvents()) != 0 {
		t.Fatal("something was applied")
	}
	// It is looked at again later, with nothing counted as a read, and nothing sent in the meantime.
	if polls, next := p.refPolls(ref.id); polls != 0 || next == "" {
		t.Fatalf("polls %d, next %q", polls, next)
	}
	p.later(pollHold / 2)
	p.pollNow()
	if n := len(p.requestsSince(mark)); n != 0 {
		t.Fatalf("%d requests inside the wait", n)
	}
}

// ----- the right key, and only its own -----

func TestPollingNeverReadsARefOfTheOtherModeWithTheIntegrationsKey(t *testing.T) {
	p := newPay(t, publicURL)
	record, _ := p.sentInvoice(100, "EUR", "op-a-0001")
	live, liveObject := p.openRef(record, ModeLive, time.Hour)
	mark := p.mark()
	p.due()
	p.pollNow()
	p.later(time.Hour)
	p.pollNow()
	for _, r := range p.readsSince(mark) {
		if strings.Contains(r.Path, liveObject) {
			t.Fatalf("a live ref was read with a test key: %+v", r)
		}
	}
	if polls, _ := p.refPolls(live); polls != 0 {
		t.Fatalf("the live ref counts %d reads", polls)
	}
}

func TestPollingReadsAnotherWorkspacesRefsOnlyWithTheirOwnIntegrationsKey(t *testing.T) {
	p := newPay(t, publicURL)
	recordA, refA := p.sentInvoice(100, "EUR", "op-a-0001")

	// A second workspace, with an integration, an app and an invoice of its own.
	owner, ws, space, in, app, approval := p.owner, p.ws, p.space, p.in, p.app, p.approval
	_, foreignWs := p.otherWorkspace(owner)
	p.ws = foreignWs
	rec := owner.do(http.MethodPost, wsPath(foreignWs, "spaces"), map[string]string{"name": "Other"})
	expect(t, rec, http.StatusCreated)
	p.space = decode[access.Space](t, rec)
	body := stripeBody("foreign")
	body["secrets"] = map[string]string{"api_key": p.fake.APIKey(), "webhook_secret": hookFor("foreign")}
	p.in = p.create(owner, foreignWs, body)
	p.exec(`INSERT INTO integration_spaces (integration_id, space_id) VALUES (?, ?)`, p.in.ID, p.space.ID)
	p.app, p.approval = p.publishApp("Invoices", invoiceApp)
	recordB, refB := p.sentInvoice(100, "EUR", "op-b-0001")
	foreignIn := p.in
	p.owner, p.ws, p.space, p.in, p.app, p.approval = owner, ws, space, in, app, approval

	// The second workspace's key is another one now: the fake takes only the first one's, so a read with
	// it is refused, and a read of the second workspace's object with the first one's key would succeed.
	expect(t, owner.do(http.MethodPatch, wsPath(foreignWs, fmt.Sprintf("integrations/%d", foreignIn.ID)),
		map[string]any{"secrets": map[string]string{"api_key": stripeOtherKey}}), http.StatusOK)
	for _, obj := range []string{refA.object, refB.object} {
		if _, err := p.fake.Pay(obj); err != nil {
			t.Fatal(err)
		}
	}
	mark := p.mark()
	p.due()
	p.pollNow()

	var ofA, ofB []testprovider.Request
	for _, r := range p.readsSince(mark) {
		switch r.Path {
		case "/v1/checkout/sessions/" + refA.object:
			ofA = append(ofA, r)
		case "/v1/checkout/sessions/" + refB.object:
			ofB = append(ofB, r)
		default:
			t.Fatalf("a read of %s", r.Path)
		}
	}
	if len(ofA) != 1 || ofA[0].Auth != "bearer" {
		t.Fatalf("reads of the first workspace's ref: %+v", ofA)
	}
	if len(ofB) != 1 || ofB[0].Auth != "" {
		t.Fatalf("reads of the second workspace's ref: %+v (a key of the first would show as bearer)", ofB)
	}
	if p.fields(recordA)["status"] != "paid" || p.fields(recordB)["status"] != "sent" || p.refOf(recordB).state != "open" {
		t.Fatalf("A %v, B %v", p.fields(recordA), p.fields(recordB))
	}
	for _, e := range p.polledEvents() {
		if e.object == refB.object {
			t.Fatalf("an event of the second workspace's object: %+v", e)
		}
	}
}

// A ref whose integration isn't allowed in its app's space (the app is in another workspace's, here) is
// never read, with whatever key.
func TestPollingNeverReadsARefOfAnAppTheIntegrationIsNotAllowedIn(t *testing.T) {
	p := newPay(t, publicURL)
	record, _ := p.sentInvoice(100, "EUR", "op-a-0001")
	_, foreignWs := p.otherWorkspace(p.owner)
	body := stripeBody("foreign")
	body["secrets"] = map[string]string{"api_key": p.fake.APIKey(), "webhook_secret": hookFor("foreign")}
	foreignIn := p.create(p.owner, foreignWs, body)
	// A ref of the foreign integration that points at the first workspace's app and record.
	p.exec(`INSERT INTO integration_refs (token, integration_id, app_id, record_id, slot, provider_object_id, url, amount_minor, currency,
			state, created_at, account_id, account_mode, success_url, cancel_url)
		VALUES (?, ?, ?, ?, 'payments', 'cs_test_crossed', 'https://checkout.stripe.com/c/pay/x', 10000, 'EUR', 'open', ?, 'acct_x', 'test', ?, ?)`,
		token(), foreignIn.ID, p.app.ID, record, time.Now().Add(-time.Hour).UTC().Format(timeFormat), publicURL+PaymentDonePath, publicURL+PaymentCancelledPath)
	mark := p.mark()
	p.due()
	p.pollNow()
	for _, r := range p.readsSince(mark) {
		if strings.HasSuffix(r.Path, "cs_test_crossed") {
			t.Fatalf("a crossed ref was read: %+v", r)
		}
	}
}
