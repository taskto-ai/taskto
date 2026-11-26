package integrations

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/integrations/testprovider"
)

// One payable link per invoice: a record that asks for its payment link again (it was sent again, an
// edit ran the effect, its expired link is replaced) never gets a second link while the first may
// still be paid. These run the bundled Invoicing template, and the generic invoicing app whose send
// action runs the effect on every change of the record.

// invoicingDefinition is the bundled Invoicing template's definition.
func invoicingDefinition(t *testing.T) string {
	t.Helper()
	all, err := apps.Templates()
	if err != nil {
		t.Fatal(err)
	}
	for _, tpl := range all {
		if tpl.ID == "invoicing" {
			raw, err := json.Marshal(tpl.Definition)
			if err != nil {
				t.Fatal(err)
			}
			return string(raw)
		}
	}
	t.Fatal("no invoicing template")
	return ""
}

// templateInvoice adds a draft invoice through the template's own action, and returns its record.
func (p *pay) templateInvoice(total any) int64 {
	p.t.Helper()
	p.act("", "add_invoice", map[string]any{"number": "INV-1", "customer": "Acme", "total": total, "currency": "EUR", "status": "draft"})
	var id int64
	if err := p.db.QueryRow(`SELECT max(id) FROM app_rows WHERE app_id = ?`, p.app.ID).Scan(&id); err != nil {
		p.t.Fatal(err)
	}
	return id
}

// allEnded waits for there to be n entries, every one done or failed, and returns them.
func (p *pay) allEnded(n int) []entry {
	p.t.Helper()
	var es []entry
	p.waitFor("every entry ended", func() bool {
		es = p.entries()
		if len(es) != n {
			return false
		}
		for _, e := range es {
			if e.state != apps.StateDone && e.state != apps.StateFailed {
				return false
			}
		}
		return true
	})
	return es
}

// field reads one field of the record.
func (p *pay) field(record int64, name string) any {
	p.t.Helper()
	data, _ := p.record(record)
	var m map[string]any
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		p.t.Fatal(err)
	}
	return m[name]
}

func TestEditingASentInvoiceMakesNoProviderCall(t *testing.T) {
	p := newPayWith(t, publicURL, invoicingDefinition(t))
	record := p.templateInvoice(120)
	p.startWorker()
	p.act("op-inv-send-01", "send_invoice", map[string]any{"id": record, "status": "sent"})
	p.allEnded(1)
	url := p.field(record, "pay_url")
	if len(p.posts()) != 1 || url == nil || url == "" {
		t.Fatalf("the send: %d calls, pay_url %v", len(p.posts()), url)
	}

	// The record editor's action (the template's update without effects): the customer and the amount
	// change, and nothing is queued or asked of Stripe.
	p.act("op-inv-edit-01", "edit_invoice", map[string]any{"id": record, "customer": "Acme Ltd", "total": 130})
	time.Sleep(200 * time.Millisecond)
	if es := p.entries(); len(es) != 1 {
		t.Fatalf("an edit queued %d entries", len(es)-1)
	}
	if n := len(p.fake.Requests()); n == 0 || len(p.posts()) != 1 {
		t.Fatalf("an edit called Stripe: %d calls that make a link", len(p.posts()))
	}
	if p.field(record, "pay_url") != url || p.field(record, "customer") != "Acme Ltd" {
		t.Fatalf("the edited invoice: pay_url %v, customer %v", p.field(record, "pay_url"), p.field(record, "customer"))
	}
}

func TestSendingAnInvoiceTwiceMakesOneLink(t *testing.T) {
	t.Run("the template, sent again after it went back to draft", func(t *testing.T) {
		p := newPayWith(t, publicURL, invoicingDefinition(t))
		record := p.templateInvoice(120)
		p.startWorker()
		p.act("op-inv-send-01", "send_invoice", map[string]any{"id": record, "status": "sent"})
		p.allEnded(1)
		first := p.field(record, "pay_url")

		p.act("op-inv-edit-01", "edit_invoice", map[string]any{"id": record, "status": "draft", "customer": "Acme Ltd"})
		p.act("op-inv-send-02", "send_invoice", map[string]any{"id": record, "status": "sent"})
		es := p.allEnded(2)
		if es[1].state != apps.StateDone {
			t.Fatalf("the second send: %+v", es[1])
		}
		if n := len(p.posts()); n != 1 || len(p.fake.Sessions()) != 1 {
			t.Fatalf("two sends made %d links", n)
		}
		if got := p.field(record, "pay_url"); got != first {
			t.Fatalf("the record's link is %v, want the first %v", got, first)
		}
		if refs := p.refs(); len(refs) != 1 {
			t.Fatalf("refs = %+v", refs)
		}
	})

	// An app whose send action runs the effect on any change: sending with another customer is a new
	// entry, and the open link is given again.
	t.Run("an action that runs the effect on every change", func(t *testing.T) {
		p := newPay(t, publicURL)
		record := p.invoice(10, "EUR")
		p.startWorker()
		p.send(record, "op-send-twice-1")
		p.allEnded(1)
		first := p.field(record, "pay_url")
		p.act("op-send-twice-2", "send", map[string]any{"id": record, "status": "sent", "customer": "Other"})
		p.allEnded(2)
		if n := len(p.posts()); n != 1 || len(p.fake.Sessions()) != 1 {
			t.Fatalf("two entries made %d links", n)
		}
		if got := p.field(record, "pay_url"); got != first {
			t.Fatalf("the record's link is %v, want %v", got, first)
		}
	})
}

// The record's open link asks other money than a new entry: nothing is made, and the entry says so.
func TestAnOpenLinkForAnotherAmountIsNeverJoinedByASecond(t *testing.T) {
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	p.startWorker()
	p.send(record, "op-amount-0001")
	p.allEnded(1)
	first := p.field(record, "pay_url")
	p.act("op-amount-0002", "send", map[string]any{"id": record, "status": "sent", "total": 12})
	es := p.allEnded(2)
	if es[1].state != apps.StateFailed || es[1].code != string(apierr.CodeIntegrationLinkStillOpen) {
		t.Fatalf("the second entry: %+v", es[1])
	}
	if n := len(p.posts()); n != 1 || p.field(record, "pay_url") != first {
		t.Fatalf("%d links; pay_url %v", n, p.field(record, "pay_url"))
	}
}

func TestAnExpiredLinkIsReplacedByExactlyOneNewLink(t *testing.T) {
	p := newPayWith(t, publicURL, invoicingDefinition(t))
	record := p.templateInvoice(120)
	p.startWorker()
	p.act("op-inv-send-01", "send_invoice", map[string]any{"id": record, "status": "sent"})
	p.allEnded(1)
	first, firstID := p.field(record, "pay_url"), p.field(record, "payment_id")

	// Replacing a link that is still open: the button clears it, which queues the effect, and the same
	// link is written back. Nothing is made.
	replace := func(op string) {
		t.Helper()
		p.act(op, "replace_link", map[string]any{"id": record, "pay_url": "", "payment_id": ""})
	}
	replace("op-inv-repl-01")
	if es := p.allEnded(2); es[1].state != apps.StateDone {
		t.Fatalf("the replace of an open link: %+v", es[1])
	}
	if n := len(p.posts()); n != 1 || p.field(record, "pay_url") != first || p.field(record, "payment_id") != firstID {
		t.Fatalf("an open link was replaced: %d links, pay_url %v", n, p.field(record, "pay_url"))
	}

	// Once Stripe says it expired, the replace makes one new link, and the record has it.
	if _, err := p.fake.Expire(firstID.(string)); err != nil {
		t.Fatal(err)
	}
	replace("op-inv-repl-02")
	if es := p.allEnded(3); es[2].state != apps.StateDone {
		t.Fatalf("the replace of an expired link: %+v", es[2])
	}
	if n := len(p.posts()); n != 2 {
		t.Fatalf("the replacement made %d new links, want 1", n-1)
	}
	second := p.field(record, "pay_url")
	if second == first || second == "" || p.field(record, "payment_id") == firstID {
		t.Fatalf("the record after the replacement: pay_url %v", second)
	}
	refs := p.refs()
	if len(refs) != 2 || refs[0].state != "expired" || refs[1].state != "open" || refs[1].url != second {
		t.Fatalf("refs = %+v", refs)
	}

	// And replacing again while the new link is open makes nothing more.
	replace("op-inv-repl-03")
	p.allEnded(4)
	if n := len(p.posts()); n != 2 || p.field(record, "pay_url") != second {
		t.Fatalf("a third replace: %d links, pay_url %v", n, p.field(record, "pay_url"))
	}
}

// A newer entry for a record whose link's outcome is unknown takes that ref and resolves it: it finds
// the object the first call made, and makes nothing of its own.
func TestANewerEntryTakesARefWhoseOutcomeIsUnknown(t *testing.T) {
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	p.fake.Fail(testprovider.ServerError().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	p.startWorker()
	p.send(record, "op-unknown-0001")
	p.waitFor("the first call", func() bool { return len(p.posts()) == 1 })
	p.waitsUnknown()
	p.fake.ClearFaults()

	// The record is sent again, with another customer: a newer entry, the older superseded.
	p.act("op-unknown-0002", "send", map[string]any{"id": record, "status": "sent", "customer": "Other"})
	p.later(time.Minute)
	es := p.allEnded(2)
	if es[1].state != apps.StateDone {
		t.Fatalf("the newer entry: %+v", es[1])
	}
	if n := len(p.posts()); n != 1 || len(p.fake.Sessions()) != 1 {
		t.Fatalf("%d calls made a link, %d sessions at Stripe", n, len(p.fake.Sessions()))
	}
	refs := p.refs()
	if len(refs) != 1 || refs[0].effect.Int64 != es[1].id || refs[0].object != p.fake.Sessions()[0].ID {
		t.Fatalf("refs = %+v", refs)
	}
	if !strings.Contains(p.field(record, "pay_url").(string), refs[0].object) && p.field(record, "payment_id") != refs[0].object {
		t.Fatalf("the record: %v", p.field(record, "payment_id"))
	}
}

// A ref under which no call ever ran holds nothing at Stripe: a newer entry removes it and makes its
// own, once.
func TestARefUnderWhichNothingRanIsReplaced(t *testing.T) {
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	// A rate limit: Stripe says it ran nothing, and the entry waits to ask again under the same key.
	p.fake.Fail(testprovider.RateLimited(time.Second).On(http.MethodPost, "/v1/checkout/sessions"))
	p.startWorker()
	p.send(record, "op-norun-00001")
	p.waitFor("the refused call", func() bool { return len(p.posts()) >= 1 })
	p.waitFor("the entry waits", func() bool {
		es := p.entries()
		return len(es) == 1 && es[0].state == apps.StatePending && es[0].attempts >= 1
	})
	p.fake.ClearFaults()
	before := len(p.posts())
	p.act("op-norun-00002", "send", map[string]any{"id": record, "status": "sent", "customer": "Other"})
	p.later(time.Hour)
	p.allEnded(2)
	if made := len(p.fake.Sessions()); made != 1 || len(p.posts()) != before+1 {
		t.Fatalf("%d sessions, %d calls after the refused ones", made, len(p.posts())-before)
	}
	if refs := p.refs(); len(refs) != 1 || refs[0].state != "open" {
		t.Fatalf("refs = %+v", refs)
	}
}
