package integrations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/integrations/testprovider"
	"taskto.ai/taskto/internal/paylink"
)

// The apps these tests use: an invoicing app whose slot handles the events they are about.
//
// invoiceAppRefund also marks a refunded invoice, and invoiceAppNote has a note that a paid invoice
// requires, so that the write a payment makes doesn't fit the collection.
const invoiceAppRefund = `{
  "components": [{"id":"root","component":"Text","text":"Invoices"}],
  "collection": {"fields": [
    {"name": "number", "type": "string", "required": true},
    {"name": "customer", "type": "string"},
    {"name": "total", "type": "number", "required": true},
    {"name": "currency", "type": "choice", "options": ["EUR", "USD", "JPY"]},
    {"name": "status", "type": "choice", "options": ["draft", "sent", "overdue", "paid", "refunded"]},
    {"name": "paid_at", "type": "date"},
    {"name": "pay_url", "type": "string", "max_length": 2048},
    {"name": "payment_id", "type": "string", "max_length": 255}
  ]},
  "actions": {
    "add": {"kind": "insert"},
    "send": {"kind": "update", "effects": [{
      "kind": "create_payment_link", "integration": "payments", "amount_field": "total", "currency_field": "currency",
      "description": "Invoice {{number}} for {{customer}}", "url_field": "pay_url", "id_field": "payment_id"}]}
  },
  "integrations": {"payments": {"kind": "payments", "on": {
    "payment.succeeded": {"only_if": {"field": "status", "op": "eq", "value": "sent"},
      "set": {"status": {"value": "paid"}, "paid_at": {"from": "occurred_at"}}},
    "payment.refunded": {"only_if": {"field": "status", "op": "eq", "value": "paid"},
      "set": {"status": {"value": "refunded"}}}}}}
}`

const invoiceAppNote = `{
  "components": [{"id":"root","component":"Text","text":"Invoices"}],
  "collection": {"fields": [
    {"name": "number", "type": "string", "required": true},
    {"name": "customer", "type": "string"},
    {"name": "total", "type": "number", "required": true},
    {"name": "currency", "type": "choice", "options": ["EUR", "USD", "JPY"]},
    {"name": "status", "type": "choice", "options": ["draft", "sent", "overdue", "paid"]},
    {"name": "note", "type": "string", "required_when": {"field": "status", "op": "eq", "value": "paid"}},
    {"name": "paid_at", "type": "date"},
    {"name": "pay_url", "type": "string", "max_length": 2048},
    {"name": "payment_id", "type": "string", "max_length": 255}
  ]},
  "actions": {
    "add": {"kind": "insert"},
    "send": {"kind": "update", "effects": [{
      "kind": "create_payment_link", "integration": "payments", "amount_field": "total", "currency_field": "currency",
      "description": "Invoice {{number}} for {{customer}}", "url_field": "pay_url", "id_field": "payment_id"}]}
  },
  "integrations": {"payments": {"kind": "payments", "on": {
    "payment.succeeded": {"only_if": {"field": "status", "op": "eq", "value": "sent"},
      "set": {"status": {"value": "paid"}, "paid_at": {"from": "occurred_at"}}}}}}
}`

// ----- helpers -----

func (p *pay) hookID() string { return strings.TrimPrefix(*p.in.WebhookURL, publicURL+HookPrefix) }

// sender delivers signed events to the hook route of the integration with the public id.
func (p *pay) sender(id, secret string) *testprovider.Sender {
	return testprovider.NewHandlerSender(p.h, HookPrefix+id, testprovider.WithSigningSecret(secret))
}

// deliver sends the events to the route as Stripe would, each of which must be answered 200.
func (p *pay) deliver(s *testprovider.Sender, evs []testprovider.Event) {
	p.t.Helper()
	ds, err := s.SendAll(context.Background(), evs)
	if err != nil {
		p.t.Fatal(err)
	}
	for _, d := range ds {
		if d.Status != http.StatusOK {
			p.t.Fatalf("the route answered %d to %s", d.Status, d.Type)
		}
	}
}

// pays delivers what Stripe sends for the session being paid to the integration's route, and has the
// worker apply it.
func (p *pay) pays(session string) {
	p.t.Helper()
	evs, err := p.fake.Pay(session)
	if err != nil {
		p.t.Fatal(err)
	}
	p.deliver(p.sender(p.hookID(), stripeHook), evs)
	p.drain()
}

func (p *pay) drain() { p.svc.drainEvents(context.Background()) }

// sentInvoice adds an invoice, sends it and waits for its payment link, whose ref it returns.
func (p *pay) sentInvoice(total any, cur, operation string) (int64, refRow) {
	p.t.Helper()
	if p.stop == nil {
		p.startWorker()
	}
	record := p.invoice(total, cur)
	p.send(record, operation)
	var r refRow
	// The ref is open once the link is noted, and the record holds the link a moment later (its own
	// write): wait for both, so what a test reads as "before" is the settled record.
	p.waitFor("the payment link of the invoice", func() bool {
		for _, x := range p.refs() {
			if x.record == record && x.state == "open" {
				if data, _ := p.record(record); !strings.Contains(data, x.object) {
					return false
				}
				r = x
				return true
			}
		}
		return false
	})
	return record, r
}

func (p *pay) refOf(record int64) refRow {
	p.t.Helper()
	for _, r := range p.refs() {
		if r.record == record {
			return r
		}
	}
	p.t.Fatalf("no ref for record %d", record)
	return refRow{}
}

func (p *pay) fields(record int64) map[string]any {
	p.t.Helper()
	data, _ := p.record(record)
	var m map[string]any
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		p.t.Fatal(err)
	}
	return m
}

type eventRow struct {
	id                      int64
	kind, state, code       string
	amount                  int64
	currency                string
	token, object, paymentI string
}

func (p *pay) eventRows() []eventRow {
	p.t.Helper()
	rows, err := p.db.Query(`SELECT id, kind, state, coalesce(error_code, ''), amount_minor, currency, coalesce(ref_token, ''), coalesce(object_id, ''), coalesce(provider_payment_id, '')
		FROM integration_events ORDER BY id`)
	if err != nil {
		p.t.Fatal(err)
	}
	defer rows.Close()
	var out []eventRow
	for rows.Next() {
		var e eventRow
		if err := rows.Scan(&e.id, &e.kind, &e.state, &e.code, &e.amount, &e.currency, &e.token, &e.object, &e.paymentI); err != nil {
			p.t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// lastEvent is the newest event of the kind.
func (p *pay) lastEvent(kind string) eventRow {
	p.t.Helper()
	rows := p.eventRows()
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].kind == kind {
			return rows[i]
		}
	}
	p.t.Fatalf("no %s event among %+v", kind, rows)
	return eventRow{}
}

// put stores a verified event as the route would, for what the fake can't make: the amount of
// another payment, another mode.
func (p *pay) put(ev Event) {
	p.t.Helper()
	if ev.ID == "" {
		ev.ID = "evt_" + token()
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now()
	}
	if _, err := p.store.record(context.Background(), p.in.ID, ev, SourceWebhook, []byte(ev.ID)); err != nil {
		p.t.Fatal(err)
	}
}

func (p *pay) revision(record int64) int64 {
	_, rev := p.record(record)
	return rev
}

func (p *pay) historyRows(record int64) int {
	return p.count(`SELECT count(*) FROM app_row_history WHERE row_id = ?`, record)
}

// adminNotices counts the notifications of integration events, and those that went to someone who
// isn't an admin of the workspace.
func (p *pay) adminNotices() (all, toNonAdmins int) {
	p.t.Helper()
	all = p.count(`SELECT count(*) FROM notifications WHERE kind = 'integration_event'`)
	toNonAdmins = p.count(`SELECT count(*) FROM notifications n WHERE n.kind = 'integration_event' AND NOT EXISTS (
		SELECT 1 FROM memberships m WHERE m.workspace_id = n.workspace_id AND m.user_id = n.user_id AND m.role = 'admin')`)
	return all, toNonAdmins
}

func (p *pay) mustEvent(e eventRow, state string, code apierr.Code) {
	p.t.Helper()
	if e.state != state || e.code != string(code) {
		p.t.Fatalf("event %d (%s) is %s with code %q, want %s with %q", e.id, e.kind, e.state, e.code, state, code)
	}
}

// ----- a payment marks the right invoice -----

func TestAPaidEventMarksTheRightInvoicePaidAndNothingElse(t *testing.T) {
	p := newPay(t, publicURL)
	a, refA := p.sentInvoice(120.10, "EUR", "op-a-0001")
	b, refB := p.sentInvoice(80, "EUR", "op-b-0001")
	beforeA, beforeB := p.fields(a), p.fields(b)
	revA, revB := p.revision(a), p.revision(b)

	p.pays(refA.object)

	after := p.fields(a)
	if after["status"] != "paid" {
		t.Fatalf("the paid invoice is %v", after)
	}
	ev := p.lastEvent(EventPaymentSucceeded)
	p.mustEvent(ev, EventApplied, "")
	var occurred string
	if err := p.db.QueryRow(`SELECT occurred_at FROM integration_events WHERE id = ?`, ev.id).Scan(&occurred); err != nil {
		t.Fatal(err)
	}
	if after["paid_at"] != occurred[:10] {
		t.Fatalf("paid_at = %v, the event happened %s", after["paid_at"], occurred)
	}
	// Only the declared fields changed: the customer, the total, the link and the rest are as they were,
	// and nothing of what the payer typed at Stripe (the fake's payer has a name and an email) is in it.
	for k, v := range beforeA {
		if k != "status" && k != "paid_at" && after[k] != v {
			t.Errorf("field %s changed from %v to %v", k, v, after[k])
		}
	}
	for k := range after {
		if _, had := beforeA[k]; !had && k != "paid_at" {
			t.Errorf("field %s was added", k)
		}
	}
	if raw, _ := p.record(a); strings.Contains(raw, p.fake.Payer().Email) || strings.Contains(raw, p.fake.Payer().Name) {
		t.Errorf("the payer's details reached the record: %s", raw)
	}
	// The other invoice, which has a ref too, was not touched.
	if got := p.fields(b); fmt.Sprint(got) != fmt.Sprint(beforeB) || p.revision(b) != revB {
		t.Fatalf("the other invoice changed: %v", got)
	}
	if p.revision(a) != revA+1 {
		t.Fatalf("revision %d, want %d", p.revision(a), revA+1)
	}
	// The integration is who changed it, in the record and in its history.
	system := fmt.Sprintf("integration:%d", p.in.ID)
	var updatedBy sql.NullInt64
	var updatedSystem, action string
	if err := p.db.QueryRow(`SELECT updated_by, updated_system, coalesce(updated_action, '') FROM app_rows WHERE id = ?`, a).Scan(&updatedBy, &updatedSystem, &action); err != nil ||
		updatedBy.Valid || updatedSystem != system || action != EventPaymentSucceeded {
		t.Fatalf("updated by %v %q %q, %v", updatedBy, updatedSystem, action, err)
	}
	var actor sql.NullInt64
	var actorSystem, histAction string
	if err := p.db.QueryRow(`SELECT actor, actor_system, action FROM app_row_history WHERE row_id = ? ORDER BY revision DESC LIMIT 1`, a).Scan(&actor, &actorSystem, &histAction); err != nil ||
		actor.Valid || actorSystem != system || histAction != EventPaymentSucceeded {
		t.Fatalf("history actor %v %q %q, %v", actor, actorSystem, histAction, err)
	}
	// The ref is paid by this payment, and the other one still waits.
	r := p.refOf(a)
	var object, payment string
	if err := p.db.QueryRow(`SELECT coalesce(paid_object_id, ''), coalesce(payment_id, '') FROM integration_refs WHERE id = ?`, r.id).Scan(&object, &payment); err != nil {
		t.Fatal(err)
	}
	if r.state != "paid" || object != refA.object || !strings.HasPrefix(payment, "pi_") {
		t.Fatalf("ref = %+v, object %q, payment %q", r, object, payment)
	}
	if p.refOf(b).state != "open" || p.refOf(b).token != refB.token {
		t.Fatalf("the other ref moved: %+v", p.refOf(b))
	}
	// No effect was queued by a system write: effects run as the user who acted.
	if n := p.count(`SELECT count(*) FROM app_effects WHERE record_id = ? AND created_at > ?`, a, time.Now().Add(-time.Minute).UTC().Format(timeFormat)); n != 1 {
		t.Errorf("%d entries for the paid invoice: its own payment link, and no other", n)
	}
	if all, _ := p.adminNotices(); all != 0 {
		t.Errorf("an applied payment made %d admin notifications", all)
	}
}

// What names the record is the ref token the server issued: the provider's metadata names nothing.
func TestOnlyTheRefTokenNamesTheRecordNeverTheProvidersMetadata(t *testing.T) {
	p := newPay(t, publicURL)
	a, refA := p.sentInvoice(100, "EUR", "op-a-0001")
	b, refB := p.sentInvoice(100, "EUR", "op-b-0001")
	revA := p.revision(a)

	// A payment of B's link whose metadata, description and ids all say A.
	sess, _ := p.fake.Session(refB.object)
	obj := map[string]any{
		"id": "cs_test_" + token(), "object": "checkout.session", "mode": "payment", "payment_status": "paid",
		"client_reference_id": refB.token, "amount_total": 10000, "currency": "eur", "payment_intent": "pi_" + token()[:20],
		"metadata": map[string]string{"record_id": fmt.Sprint(a), "app_id": fmt.Sprint(p.app.ID), "space_id": fmt.Sprint(p.space.ID),
			"record": fmt.Sprint(a), "taskto_ref": refA.token},
		"description": fmt.Sprintf("invoice %d of app %d", a, p.app.ID), "customer_details": map[string]string{"name": sess.ID},
	}
	p.deliver(p.sender(p.hookID(), stripeHook), []testprovider.Event{testprovider.NewEvent("checkout.session.completed", obj, time.Now())})
	p.drain()

	if p.fields(b)["status"] != "paid" || p.fields(a)["status"] != "sent" || p.revision(a) != revA {
		t.Fatalf("A = %v, B = %v", p.fields(a), p.fields(b))
	}
	if p.refOf(a).state != "open" || p.refOf(b).state != "paid" {
		t.Fatalf("refs: %+v", p.refs())
	}
}

// ----- nothing leaves the ref's own integration, app, space and workspace -----

func TestAnEventOfAnotherIntegrationNeverAppliesToThisOnesRef(t *testing.T) {
	p := newPay(t, publicURL)
	record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	before, rev := p.fields(record), p.revision(record)

	// A second integration of the same workspace, and one of another workspace, each with a webhook of
	// its own, are sent the first one's payment, token and all.
	other := stripeBody("other")
	otherIn := p.create(p.owner, p.ws, other)
	foreignOwner, foreignWs := p.otherWorkspace(p.owner)
	foreign := stripeBody("foreign")
	foreignIn := p.create(foreignOwner, foreignWs, foreign)
	evs, err := p.fake.Pay(ref.object)
	if err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]struct {
		in     Integration
		secret string
	}{"same workspace": {otherIn, hookFor("other")}, "another workspace": {foreignIn, hookFor("foreign")}} {
		id := strings.TrimPrefix(*in.in.WebhookURL, publicURL+HookPrefix)
		p.deliver(p.sender(id, in.secret), evs)
		p.drain()
		var applied int
		if err := p.db.QueryRow(`SELECT count(*) FROM integration_events WHERE integration_id = ? AND state = 'applied'`, in.in.ID).Scan(&applied); err != nil || applied != 0 {
			t.Fatalf("%s: %d events applied, %v", name, applied, err)
		}
		var ignored string
		if err := p.db.QueryRow(`SELECT error_code FROM integration_events WHERE integration_id = ? AND kind = ?`, in.in.ID, EventPaymentSucceeded).Scan(&ignored); err != nil ||
			ignored != string(apierr.CodeIntegrationEventRefUnknown) {
			t.Fatalf("%s: the event ended with %q, %v", name, ignored, err)
		}
		if fmt.Sprint(p.fields(record)) != fmt.Sprint(before) || p.revision(record) != rev || p.refOf(record).state != "open" {
			t.Fatalf("%s: the record or its ref moved", name)
		}
	}
	// Its own integration's event does apply.
	p.deliver(p.sender(p.hookID(), stripeHook), evs)
	p.drain()
	if p.fields(record)["status"] != "paid" {
		t.Fatalf("the integration's own event didn't apply: %v", p.fields(record))
	}
}

func TestAnEventOnlyWritesTheAppOfItsRef(t *testing.T) {
	p := newPay(t, publicURL)
	first, refFirst := p.sentInvoice(100, "EUR", "op-a-0001")
	// A second app of the same space, with its own invoice and ref.
	firstApp := p.app
	p.app, _ = p.publishApp("Invoices two", invoiceApp)
	second, refSecond := p.sentInvoice(100, "EUR", "op-b-0001")
	p.app = firstApp
	revSecond, historySecond := p.revision(second), p.historyRows(second)

	p.pays(refFirst.object)

	if p.fields(first)["status"] != "paid" {
		t.Fatalf("first = %v", p.fields(first))
	}
	if p.fields(second)["status"] != "sent" || p.revision(second) != revSecond || p.historyRows(second) != historySecond || p.refOf(second).state != "open" || refSecond.object == refFirst.object {
		t.Fatalf("the other app's record moved: %v", p.fields(second))
	}
}

// The apps' side refuses an app that isn't in the integration's workspace, whatever the ref said.
func TestTheRecordWriterRefusesAnotherWorkspacesApp(t *testing.T) {
	p := newPay(t, publicURL)
	record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	_, foreignWs := p.otherWorkspace(p.owner)
	before, rev := p.fields(record), p.revision(record)

	settled := false
	_, err := p.apps.WriteEvent(context.Background(), paylink.EventWrite{
		WorkspaceID: foreignWs, IntegrationID: p.in.ID, AppID: ref.appID(p), RecordID: record, Slot: "payments",
		Event: EventPaymentSucceeded, AmountMinor: ref.amount, Currency: ref.currency, OccurredAt: time.Now(),
		Settle: func(context.Context, *sql.Tx) error { settled = true; return nil },
	})
	var pe *paylink.Error
	if !asPaylinkError(err, &pe) || pe.Code != apierr.CodeIntegrationEventRecordGone || settled {
		t.Fatalf("err = %v, settled %v", err, settled)
	}
	if fmt.Sprint(p.fields(record)) != fmt.Sprint(before) || p.revision(record) != rev {
		t.Fatal("the record changed")
	}
}

func (r refRow) appID(p *pay) int64 {
	var id int64
	if err := p.db.QueryRow(`SELECT app_id FROM integration_refs WHERE id = ?`, r.id).Scan(&id); err != nil {
		p.t.Fatal(err)
	}
	return id
}

func asPaylinkError(err error, target **paylink.Error) bool {
	return errors.As(err, target)
}

// A failing Settle leaves the record exactly as it was: the state and the record change together.
func TestARecordIsNotWrittenWhenTheRefCannotMove(t *testing.T) {
	p := newPay(t, publicURL)
	record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	before, rev := p.fields(record), p.revision(record)
	boom := fmt.Errorf("the ref moved")
	_, err := p.apps.WriteEvent(context.Background(), paylink.EventWrite{
		WorkspaceID: p.ws, IntegrationID: p.in.ID, AppID: ref.appID(p), RecordID: record, Slot: "payments",
		Event: EventPaymentSucceeded, AmountMinor: ref.amount, Currency: ref.currency, OccurredAt: time.Now(),
		Settle: func(context.Context, *sql.Tx) error { return boom },
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if fmt.Sprint(p.fields(record)) != fmt.Sprint(before) || p.revision(record) != rev {
		t.Fatal("the record changed")
	}
}

// ----- the mode of the money -----

func TestATestModeEventNeverAppliesToALiveRefAndLiveToTest(t *testing.T) {
	p := newPay(t, publicURL)
	record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	before, rev := p.fields(record), p.revision(record)

	// The ref was made with a test key and an event of live money names it.
	p.put(Event{Kind: EventPaymentSucceeded, RefToken: ref.token, ObjectID: "cs_live_" + token(), PaymentID: "pi_" + token()[:20],
		AmountMinor: ref.amount, Currency: "EUR", Livemode: true})
	p.drain()
	p.mustEvent(p.lastEvent(EventPaymentSucceeded), EventIgnored, apierr.CodeIntegrationEventModeMismatch)

	// And the other way round: a ref made with a live key, and a test-mode event for it.
	if _, err := p.db.Exec(`UPDATE integration_refs SET account_mode = 'live' WHERE id = ?`, ref.id); err != nil {
		t.Fatal(err)
	}
	p.put(Event{Kind: EventPaymentSucceeded, RefToken: ref.token, ObjectID: "cs_test_" + token(), PaymentID: "pi_" + token()[:20],
		AmountMinor: ref.amount, Currency: "EUR", Livemode: false})
	p.drain()
	p.mustEvent(p.lastEvent(EventPaymentSucceeded), EventIgnored, apierr.CodeIntegrationEventModeMismatch)

	if fmt.Sprint(p.fields(record)) != fmt.Sprint(before) || p.revision(record) != rev || p.refOf(record).state != "open" {
		t.Fatal("a payment of the other mode was applied")
	}
	// The live event, for a live ref, does apply.
	p.put(Event{Kind: EventPaymentSucceeded, RefToken: ref.token, ObjectID: "cs_live_" + token(), PaymentID: "pi_" + token()[:20],
		AmountMinor: ref.amount, Currency: "EUR", Livemode: true})
	p.drain()
	p.mustEvent(p.lastEvent(EventPaymentSucceeded), EventApplied, "")
	if p.fields(record)["status"] != "paid" {
		t.Fatalf("record = %v", p.fields(record))
	}
}

// ----- the amount and the currency -----

func TestAnAmountOrCurrencyThatIsNotWhatWasAskedIsNotApplied(t *testing.T) {
	for name, tc := range map[string]struct {
		amount   int64
		currency string
	}{
		"less":     {9999, "EUR"},
		"more":     {10001, "EUR"},
		"currency": {10000, "USD"},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPay(t, publicURL)
			record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
			before, rev := p.fields(record), p.revision(record)
			p.put(Event{Kind: EventPaymentSucceeded, RefToken: ref.token, ObjectID: "cs_test_" + token(), PaymentID: "pi_" + token()[:20],
				AmountMinor: tc.amount, Currency: tc.currency})
			p.drain()

			p.mustEvent(p.lastEvent(EventPaymentSucceeded), EventFailed, apierr.CodeIntegrationAmountMismatch)
			if fmt.Sprint(p.fields(record)) != fmt.Sprint(before) || p.revision(record) != rev || p.refOf(record).state != "open" {
				t.Fatal("a payment of another amount was applied")
			}
			// An admin is told, and only an admin.
			if all, non := p.adminNotices(); all != 1 || non != 0 {
				t.Fatalf("notifications: %d, %d to others", all, non)
			}
			// The same expired session that Stripe reports with another amount isn't ours either.
			p.put(Event{Kind: EventPaymentExpired, RefToken: ref.token, ObjectID: ref.object, AmountMinor: tc.amount, Currency: tc.currency})
			p.drain()
			p.mustEvent(p.lastEvent(EventPaymentExpired), EventFailed, apierr.CodeIntegrationAmountMismatch)
			if p.refOf(record).state != "open" {
				t.Fatal("the ref moved on an event for another amount")
			}
		})
	}
}

// ----- duplicates, late events and second payments -----

func TestADuplicateAndALateEventAreIgnoredWithACode(t *testing.T) {
	p := newPay(t, publicURL)
	record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	p.pays(ref.object)
	paid := p.fields(record)
	rev, history := p.revision(record), p.historyRows(record)
	object := p.refOf(record)

	// The same payment told again under another event id (Stripe's retry of another event of the
	// payment, or the purge having forgotten the first), and an event that ends the link after it was paid.
	var payment string
	if err := p.db.QueryRow(`SELECT payment_id FROM integration_refs WHERE id = ?`, object.id).Scan(&payment); err != nil {
		t.Fatal(err)
	}
	p.put(Event{Kind: EventPaymentSucceeded, RefToken: ref.token, ObjectID: ref.object, PaymentID: payment, AmountMinor: ref.amount, Currency: "EUR"})
	p.drain()
	p.mustEvent(p.lastEvent(EventPaymentSucceeded), EventIgnored, apierr.CodeIntegrationEventDuplicate)
	p.put(Event{Kind: EventPaymentExpired, RefToken: ref.token, ObjectID: ref.object, AmountMinor: ref.amount, Currency: "EUR"})
	p.drain()
	p.mustEvent(p.lastEvent(EventPaymentExpired), EventIgnored, apierr.CodeIntegrationEventLate)
	p.put(Event{Kind: EventPaymentFailed, RefToken: ref.token, ObjectID: ref.object, AmountMinor: ref.amount, Currency: "EUR"})
	p.drain()
	p.mustEvent(p.lastEvent(EventPaymentFailed), EventIgnored, apierr.CodeIntegrationEventLate)

	if fmt.Sprint(p.fields(record)) != fmt.Sprint(paid) || p.revision(record) != rev || p.historyRows(record) != history || p.refOf(record).state != "paid" {
		t.Fatal("a duplicate or late event changed something")
	}
	if all, _ := p.adminNotices(); all != 0 {
		t.Fatalf("%d notifications for events that change nothing and lose nothing", all)
	}
	if n := p.count(`SELECT count(*) FROM integration_ref_duplicates`); n != 0 {
		t.Fatalf("%d second payments recorded for a repeat of the first", n)
	}
}

func TestALinkThatExpiredEndsTheRefAndIsNotPaidLater(t *testing.T) {
	p := newPay(t, publicURL)
	record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	evs, err := p.fake.Expire(ref.object)
	if err != nil {
		t.Fatal(err)
	}
	p.deliver(p.sender(p.hookID(), stripeHook), evs)
	p.drain()
	// The app declares nothing for it: the ref moved and the record is as it was.
	p.mustEvent(p.lastEvent(EventPaymentExpired), EventIgnored, apierr.CodeIntegrationEventNoHandler)
	if p.refOf(record).state != "expired" || p.fields(record)["status"] != "sent" {
		t.Fatalf("ref %+v, record %v", p.refOf(record), p.fields(record))
	}
	// The same event again is a duplicate.
	p.put(Event{Kind: EventPaymentExpired, RefToken: ref.token, ObjectID: ref.object, AmountMinor: ref.amount, Currency: "EUR"})
	p.drain()
	p.mustEvent(p.lastEvent(EventPaymentExpired), EventIgnored, apierr.CodeIntegrationEventDuplicate)
}

func TestASecondPaymentOnASettledRefIsRecordedForTheAdminsAndNeverAppliedTwice(t *testing.T) {
	p := newPay(t, publicURL)
	record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	member := p.invite(p.owner, p.ws, "member@example.test", "member")
	_ = member
	p.pays(ref.object)
	paid, rev, history := p.fields(record), p.revision(record), p.historyRows(record)

	// Another object (a second link made for the same ref) is paid.
	second := Event{Kind: EventPaymentSucceeded, RefToken: ref.token, ObjectID: "cs_test_" + token(), PaymentID: "pi_" + token()[:20], AmountMinor: ref.amount, Currency: "EUR"}
	p.put(second)
	p.drain()
	p.mustEvent(p.lastEvent(EventPaymentSucceeded), EventIgnored, apierr.CodeIntegrationEventSecondPayment)
	if fmt.Sprint(p.fields(record)) != fmt.Sprint(paid) || p.revision(record) != rev || p.historyRows(record) != history {
		t.Fatal("a second payment was applied")
	}
	if n := p.count(`SELECT count(*) FROM integration_ref_duplicates WHERE ref_id = ? AND object_id = ? AND payment_id = ? AND amount_minor = ? AND currency = 'EUR'`,
		ref.id, second.ObjectID, second.PaymentID, ref.amount); n != 1 {
		t.Fatalf("%d duplicates recorded", n)
	}
	if all, non := p.adminNotices(); all != 1 || non != 0 {
		t.Fatalf("notifications: %d, %d to non-admins", all, non)
	}
	// Told again (a purge forgot it, the provider retried): recorded once.
	second.ID = "evt_" + token()
	p.put(second)
	p.drain()
	if n := p.count(`SELECT count(*) FROM integration_ref_duplicates`); n != 1 {
		t.Fatalf("%d duplicates after a repeat", n)
	}
	// What an admin sees: the event, with its code and no record.
	rec := p.owner.do(http.MethodGet, wsPath(p.ws, fmt.Sprintf("integrations/%d/events?state=ignored", p.in.ID)), nil)
	expect(t, rec, http.StatusOK)
	list := decode[EventList](t, rec)
	found := false
	for _, e := range list.Events {
		found = found || e.Code == string(apierr.CodeIntegrationEventSecondPayment) && e.AmountMinor == ref.amount
	}
	if !found {
		t.Fatalf("the admin's list lacks the second payment: %+v", list)
	}
}

// ----- the app's own rule, and the collection -----

func TestAPaymentTheAppsRuleDoesNotAcceptSettlesTheRefLeavesTheRecordAndTellsTheAdmins(t *testing.T) {
	p := newPay(t, publicURL)
	record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	// The invoice was taken back to a draft after the link was sent.
	if _, err := p.db.Exec(`UPDATE app_rows SET data = json_set(data, '$.status', 'draft') WHERE id = ?`, record); err != nil {
		t.Fatal(err)
	}
	before, rev := p.fields(record), p.revision(record)
	p.pays(ref.object)

	p.mustEvent(p.lastEvent(EventPaymentSucceeded), EventIgnored, apierr.CodeIntegrationEventConditionUnmet)
	if fmt.Sprint(p.fields(record)) != fmt.Sprint(before) || p.revision(record) != rev {
		t.Fatal("the record was written though the rule didn't hold")
	}
	// The money arrived: the ref says so, and the admins are told.
	if p.refOf(record).state != "paid" {
		t.Fatalf("ref = %+v", p.refOf(record))
	}
	if all, non := p.adminNotices(); all != 1 || non != 0 {
		t.Fatalf("notifications: %d, %d to non-admins", all, non)
	}
}

func TestAWriteThatDoesNotFitTheCollectionChangesNothingAndLeavesTheRefOpen(t *testing.T) {
	p := newPayWith(t, publicURL, invoiceAppNote)
	record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	before, rev := p.fields(record), p.revision(record)
	p.pays(ref.object)

	// The note a paid invoice requires is empty: the write is refused, and neither the record nor the
	// ref changed, since they change together.
	p.mustEvent(p.lastEvent(EventPaymentSucceeded), EventFailed, apierr.CodeIntegrationEventRecordRefused)
	if fmt.Sprint(p.fields(record)) != fmt.Sprint(before) || p.revision(record) != rev || p.refOf(record).state != "open" {
		t.Fatalf("something changed: %v, ref %+v", p.fields(record), p.refOf(record))
	}
	if all, _ := p.adminNotices(); all != 1 {
		t.Fatalf("%d notifications", all)
	}
	// Once the note is there, a retry applies it.
	if _, err := p.db.Exec(`UPDATE app_rows SET data = json_set(data, '$.note', 'checked') WHERE id = ?`, record); err != nil {
		t.Fatal(err)
	}
	p.retry(p.lastEvent(EventPaymentSucceeded).id, http.StatusOK)
	p.drain()
	p.mustEvent(p.lastEvent(EventPaymentSucceeded), EventApplied, "")
	if p.fields(record)["status"] != "paid" || p.refOf(record).state != "paid" {
		t.Fatalf("record %v, ref %+v", p.fields(record), p.refOf(record))
	}
}

// ----- what is checked again when the event is applied -----

func TestWhatIsCheckedAgainWhenAnEventIsApplied(t *testing.T) {
	for name, tc := range map[string]struct {
		breakIt func(p *pay, record int64)
		code    apierr.Code
	}{
		"the approval is revoked": {func(p *pay, _ int64) {
			expect(p.t, p.owner.do(http.MethodPost, wsPath(p.ws, fmt.Sprintf("integration-approvals/%d/revoke", p.approval.ID)), map[string]any{}), http.StatusOK)
		}, apierr.CodeIntegrationEventUnavailable},
		"the integration is switched off": {func(p *pay, _ int64) {
			p.exec(`UPDATE integrations SET disabled_at = ? WHERE id = ?`, time.Now().UTC().Format(timeFormat), p.in.ID)
		}, apierr.CodeIntegrationEventUnavailable},
		"the space is no longer allowed": {func(p *pay, _ int64) {
			p.exec(`DELETE FROM integration_spaces WHERE integration_id = ?`, p.in.ID)
		}, apierr.CodeIntegrationEventUnavailable},
		"the slot is bound to another integration": {func(p *pay, _ int64) {
			other := p.create(p.owner, p.ws, stripeBody("other"))
			p.exec(`INSERT INTO integration_spaces (integration_id, space_id) VALUES (?, ?)`, other.ID, p.space.ID)
			p.exec(`UPDATE app_integration_approvals SET integration_id = ? WHERE id = ?`, other.ID, p.approval.ID)
		}, apierr.CodeIntegrationEventIntegrationChanged},
		"the app is offline": {func(p *pay, _ int64) {
			p.exec(`UPDATE apps SET archived_at = ? WHERE id = ?`, time.Now().UTC().Format(timeFormat), p.app.ID)
		}, apierr.CodeIntegrationEventAppOffline},
		"the space is archived": {func(p *pay, _ int64) {
			p.exec(`UPDATE spaces SET archived_at = ? WHERE id = ?`, time.Now().UTC().Format(timeFormat), p.space.ID)
		}, apierr.CodeIntegrationEventAppOffline},
		"the record is deleted": {func(p *pay, record int64) {
			p.exec(`UPDATE app_rows SET deleted_at = ? WHERE id = ?`, time.Now().UTC().Format(timeFormat), record)
		}, apierr.CodeIntegrationEventRecordGone},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPay(t, publicURL)
			record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
			evs, err := p.fake.Pay(ref.object)
			if err != nil {
				t.Fatal(err)
			}
			// The event is stored (the route took it in), and what it needs changes before the worker gets to it.
			p.deliver(p.sender(p.hookID(), stripeHook), evs)
			tc.breakIt(p, record)
			before, rev := p.fields(record), p.revision(record)
			p.drain()

			p.mustEvent(p.lastEvent(EventPaymentSucceeded), EventFailed, tc.code)
			if fmt.Sprint(p.fields(record)) != fmt.Sprint(before) || p.revision(record) != rev {
				t.Fatal("the record was written")
			}
			// Money that arrived is not lost: the ref still waits for the event, which an admin can retry.
			if p.refOf(record).state != "open" {
				t.Fatalf("the ref moved to %s without the record", p.refOf(record).state)
			}
			if all, non := p.adminNotices(); all < 1 || non != 0 {
				t.Fatalf("notifications: %d, %d to non-admins", all, non)
			}
		})
	}
}

func TestAFailedEventIsRetriedOnceWhatWasWrongIsFixed(t *testing.T) {
	p := newPay(t, publicURL)
	record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	evs, err := p.fake.Pay(ref.object)
	if err != nil {
		t.Fatal(err)
	}
	p.deliver(p.sender(p.hookID(), stripeHook), evs)
	expect(t, p.owner.do(http.MethodPost, wsPath(p.ws, fmt.Sprintf("integration-approvals/%d/revoke", p.approval.ID)), map[string]any{}), http.StatusOK)
	p.drain()
	failed := p.lastEvent(EventPaymentSucceeded)
	p.mustEvent(failed, EventFailed, apierr.CodeIntegrationEventUnavailable)

	// Retrying before the fix fails again, with a notification of its own.
	p.retry(failed.id, http.StatusOK)
	p.drain()
	p.mustEvent(p.lastEvent(EventPaymentSucceeded), EventFailed, apierr.CodeIntegrationEventUnavailable)
	if all, _ := p.adminNotices(); all != 2 {
		t.Fatalf("%d notifications after two failures", all)
	}

	// The admin approves the contract again; the retry applies the payment.
	rec := p.owner.do(http.MethodPost, p.appsPath(fmt.Sprintf("/%d/integrations/payments/request", p.app.ID)), map[string]any{"contract_hash": p.approval.ContractHash})
	expect(t, rec, http.StatusOK)
	again := decode[apps.IntegrationApproval](t, rec)
	expect(t, p.owner.do(http.MethodPost, wsPath(p.ws, fmt.Sprintf("integration-approvals/%d/approve", again.ID)), map[string]any{"integration_id": p.in.ID}), http.StatusOK)
	p.retry(failed.id, http.StatusOK)
	p.drain()
	p.mustEvent(p.lastEvent(EventPaymentSucceeded), EventApplied, "")
	if p.fields(record)["status"] != "paid" || p.refOf(record).state != "paid" {
		t.Fatalf("record %v, ref %+v", p.fields(record), p.refOf(record))
	}
}

// ----- refunds -----

func TestARefundIsMappedThroughThePaymentTheRefLearnedAndAppliesOnlyForAllOfIt(t *testing.T) {
	p := newPayWith(t, publicURL, invoiceAppRefund)
	record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	p.pays(ref.object)
	if p.fields(record)["status"] != "paid" {
		t.Fatalf("record = %v", p.fields(record))
	}

	// Part of it: nothing changes.
	evs, err := p.fake.Refund(ref.object, 2500)
	if err != nil {
		t.Fatal(err)
	}
	p.deliver(p.sender(p.hookID(), stripeHook), evs)
	p.drain()
	p.mustEvent(p.lastEvent(EventPaymentRefunded), EventIgnored, apierr.CodeIntegrationEventPartialRefund)
	if p.fields(record)["status"] != "paid" || p.refOf(record).state != "paid" {
		t.Fatalf("a partial refund changed %v / %+v", p.fields(record), p.refOf(record))
	}

	// The rest: the charge has now been refunded in full, and has no token of ours.
	evs, err = p.fake.Refund(ref.object, 0)
	if err != nil {
		t.Fatal(err)
	}
	p.deliver(p.sender(p.hookID(), stripeHook), evs)
	p.drain()
	ev := p.lastEvent(EventPaymentRefunded)
	p.mustEvent(ev, EventApplied, "")
	if ev.token != "" || !strings.HasPrefix(ev.paymentI, "pi_") {
		t.Fatalf("the refund event carries %+v", ev)
	}
	if p.fields(record)["status"] != "refunded" || p.refOf(record).state != "refunded" {
		t.Fatalf("record %v, ref %+v", p.fields(record), p.refOf(record))
	}
	var action string
	if err := p.db.QueryRow(`SELECT action FROM app_row_history WHERE row_id = ? ORDER BY revision DESC LIMIT 1`, record).Scan(&action); err != nil || action != EventPaymentRefunded {
		t.Fatalf("history action %q, %v", action, err)
	}
}

func TestARefundIsNeverMappedAcrossIntegrationsOrToAPaymentNoRefLearned(t *testing.T) {
	p := newPayWith(t, publicURL, invoiceAppRefund)
	record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	p.pays(ref.object)
	other := p.create(p.owner, p.ws, stripeBody("other"))
	var payment string
	if err := p.db.QueryRow(`SELECT payment_id FROM integration_refs WHERE id = ?`, ref.id).Scan(&payment); err != nil {
		t.Fatal(err)
	}
	refund := func(integration int64, payment string, amount int64) {
		p.t.Helper()
		ev := Event{ID: "evt_" + token(), Kind: EventPaymentRefunded, ObjectID: "ch_" + token()[:20], PaymentID: payment, AmountMinor: amount, Currency: "EUR"}
		if _, err := p.store.record(context.Background(), integration, ev, SourceWebhook, []byte(ev.ID)); err != nil {
			p.t.Fatal(err)
		}
	}
	// A full refund of the first integration's payment, told to another integration.
	ev := Event{ID: "evt_" + token(), Kind: EventPaymentRefunded, ObjectID: "ch_" + token()[:20], PaymentID: payment, AmountMinor: ref.amount, Currency: "EUR", OccurredAt: time.Now()}
	if _, err := p.store.record(context.Background(), other.ID, ev, SourceWebhook, []byte(ev.ID)); err != nil {
		t.Fatal(err)
	}
	// And a refund of a payment no ref has been paid by, and none of its events was seen.
	refund(p.in.ID, "pi_"+token()[:20], ref.amount)
	p.drain()
	var codes []string
	rows, err := p.db.Query(`SELECT state || ':' || coalesce(error_code, '') FROM integration_events WHERE kind = ? ORDER BY id`, EventPaymentRefunded)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		codes = append(codes, c)
	}
	_ = rows.Close()
	want := "ignored:" + string(apierr.CodeIntegrationEventRefUnknown)
	if len(codes) != 2 || codes[0] != want || codes[1] != want {
		t.Fatalf("outcomes = %v", codes)
	}
	if p.fields(record)["status"] != "paid" || p.refOf(record).state != "paid" {
		t.Fatalf("a refund that wasn't the ref's moved it: %v / %+v", p.fields(record), p.refOf(record))
	}
	// A refund for more than was paid, or in another currency, is not applied either.
	refund(p.in.ID, payment, ref.amount+1)
	p.drain()
	p.mustEvent(p.lastEvent(EventPaymentRefunded), EventFailed, apierr.CodeIntegrationAmountMismatch)
	if p.refOf(record).state != "paid" {
		t.Fatalf("ref = %+v", p.refOf(record))
	}
}

func TestARefundOfAPaymentWhoseEventWaitsIsTriedAgain(t *testing.T) {
	p := newPayWith(t, publicURL, invoiceAppRefund)
	record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	pi := "pi_" + token()[:20]
	// The payment's event is stored but not applied yet, as when the events arrive out of order.
	p.put(Event{Kind: EventPaymentSucceeded, RefToken: ref.token, ObjectID: ref.object, PaymentID: pi, AmountMinor: ref.amount, Currency: "EUR"})
	p.put(Event{Kind: EventPaymentRefunded, ObjectID: "ch_" + token()[:20], PaymentID: pi, AmountMinor: ref.amount, Currency: "EUR"})
	// The worker takes events in order, so the payment is applied first and the refund follows at once.
	p.drain()
	p.mustEvent(p.lastEvent(EventPaymentRefunded), EventApplied, "")
	if p.refOf(record).state != "refunded" {
		t.Fatalf("ref = %+v", p.refOf(record))
	}

	// With the payment's own event refused (a wrong amount), the refund waits, then fails, and is not dropped.
	record2, ref2 := p.sentInvoice(50, "EUR", "op-b-0001")
	pi2 := "pi_" + token()[:20]
	p.put(Event{Kind: EventPaymentSucceeded, RefToken: ref2.token, ObjectID: ref2.object, PaymentID: pi2, AmountMinor: ref2.amount + 1, Currency: "EUR"})
	p.put(Event{Kind: EventPaymentRefunded, ObjectID: "ch_" + token()[:20], PaymentID: pi2, AmountMinor: ref2.amount, Currency: "EUR"})
	p.drain()
	got := p.lastEvent(EventPaymentRefunded)
	if got.state != EventPending || got.code != string(apierr.CodeIntegrationEventPaymentUnknown) {
		t.Fatalf("the refund of a payment that was never applied is %s %q", got.state, got.code)
	}
	if p.refOf(record2).state != "open" {
		t.Fatalf("ref = %+v", p.refOf(record2))
	}
}

// ----- replays -----

func TestAReplayAfterThePurgeIsANoOp(t *testing.T) {
	p := newPayWith(t, publicURL, invoiceAppRefund)
	record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	evs, err := p.fake.Pay(ref.object)
	if err != nil {
		t.Fatal(err)
	}
	sender := p.sender(p.hookID(), stripeHook)
	p.deliver(sender, evs)
	p.drain()
	p.mustEvent(p.lastEvent(EventPaymentSucceeded), EventApplied, "")
	// A person changes the invoice afterwards.
	if _, err := p.apps.SystemWrite(context.Background(), apps.SystemWrite{
		Actor: apps.SystemActor{Kind: apps.SystemKindIntegration, ID: p.in.ID}, SpaceID: p.space.ID, AppID: p.app.ID, RecordID: record,
		Action: "test.edit", Declared: []string{"status"}, Set: map[string]any{"status": "overdue"},
	}); err != nil {
		t.Fatal(err)
	}
	settled, rev, history := p.fields(record), p.revision(record), p.historyRows(record)
	paidRef := p.refOf(record)

	// The purge forgets the events, and Stripe's retry of the payment, signed afresh, is stored again.
	p.exec(`DELETE FROM integration_events`)
	p.deliver(sender, evs)
	if n := len(p.eventRows()); n == 0 {
		t.Fatal("the replay wasn't stored again: this test needs a purged event")
	}
	p.drain()

	p.mustEvent(p.lastEvent(EventPaymentSucceeded), EventIgnored, apierr.CodeIntegrationEventDuplicate)
	if fmt.Sprint(p.fields(record)) != fmt.Sprint(settled) || p.revision(record) != rev || p.historyRows(record) != history || fmt.Sprint(p.refOf(record)) != fmt.Sprint(paidRef) {
		t.Fatalf("a replay changed something: %v, %+v", p.fields(record), p.refOf(record))
	}
	if all, _ := p.adminNotices(); all != 0 {
		t.Fatalf("%d notifications for a replay", all)
	}
	// And a refund replayed after its purge: applied once, a no-op after.
	refunds, err := p.fake.Refund(ref.object, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.Exec(`UPDATE app_rows SET data = json_set(data, '$.status', 'paid') WHERE id = ?`, record); err != nil {
		t.Fatal(err)
	}
	p.deliver(sender, refunds)
	p.drain()
	p.mustEvent(p.lastEvent(EventPaymentRefunded), EventApplied, "")
	if p.fields(record)["status"] != "refunded" {
		t.Fatalf("record = %v", p.fields(record))
	}
	after, rev2 := p.fields(record), p.revision(record)
	p.exec(`DELETE FROM integration_events`)
	p.deliver(sender, refunds)
	p.drain()
	p.mustEvent(p.lastEvent(EventPaymentRefunded), EventIgnored, apierr.CodeIntegrationEventDuplicate)
	if fmt.Sprint(p.fields(record)) != fmt.Sprint(after) || p.revision(record) != rev2 {
		t.Fatal("a replayed refund changed the record")
	}
}

// An event applied twice at once (a worker whose lease ran out, still working) writes once.
func TestTheSameEventAppliedTwiceWritesOnce(t *testing.T) {
	p := newPay(t, publicURL)
	record, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	p.put(Event{Kind: EventPaymentSucceeded, RefToken: ref.token, ObjectID: ref.object, PaymentID: "pi_" + token()[:20], AmountMinor: ref.amount, Currency: "EUR"})
	row := p.lastEvent(EventPaymentSucceeded)
	stored := StoredEvent{ID: row.id, IntegrationID: p.in.ID, WorkspaceID: p.ws, ProviderEvent: "x", Source: SourceWebhook, Attempts: 1,
		Event: Event{ID: "x", Kind: EventPaymentSucceeded, RefToken: ref.token, ObjectID: ref.object, PaymentID: "pi_" + token()[:20], AmountMinor: ref.amount, Currency: "EUR", OccurredAt: time.Now()}}
	a := &eventApplier{svc: p.svc, records: p.apps}
	if err := a.ApplyEvent(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	rev, history := p.revision(record), p.historyRows(record)
	err := a.ApplyEvent(context.Background(), stored)
	var ee *EventError
	if !errors.As(err, &ee) || !ee.ignored || ee.Code != apierr.CodeIntegrationEventDuplicate {
		t.Fatalf("the second apply answered %v", err)
	}
	if p.revision(record) != rev || p.historyRows(record) != history {
		t.Fatal("the record was written twice")
	}
}

// ----- the admins' routes -----

func (p *pay) retry(event int64, want int) {
	p.t.Helper()
	expect(p.t, p.owner.do(http.MethodPost, wsPath(p.ws, fmt.Sprintf("integrations/%d/events/%d/retry", p.in.ID, event)), nil), want)
}

func TestTheEventsRoutesAreForAnAdminOfTheIntegrationsWorkspaceOnly(t *testing.T) {
	p := newPay(t, publicURL)
	_, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	p.put(Event{Kind: EventPaymentSucceeded, RefToken: ref.token, ObjectID: "cs_test_" + token(), PaymentID: "pi_" + token()[:20], AmountMinor: 1, Currency: "EUR"})
	p.drain()
	failed := p.lastEvent(EventPaymentSucceeded)
	p.mustEvent(failed, EventFailed, apierr.CodeIntegrationAmountMismatch)
	member := p.invite(p.owner, p.ws, "member@example.test", "member")
	foreign, foreignWs := p.otherWorkspace(p.owner)
	list := wsPath(p.ws, fmt.Sprintf("integrations/%d/events", p.in.ID))
	retry := list + fmt.Sprintf("/%d/retry", failed.id)

	// The admin lists it, with nothing of the ref, the record or the provider's ids.
	rec := p.owner.do(http.MethodGet, list, nil)
	expect(t, rec, http.StatusOK)
	body := rec.Body.String()
	for _, secret := range []string{ref.token, "cs_test_", "pi_", stripeHook, "ref_token", "app_id", "record"} {
		if strings.Contains(body, secret) {
			t.Fatalf("the list shows %q: %s", secret, body)
		}
	}
	got := decode[EventList](t, rec)
	if len(got.Events) != 1 || got.Events[0].Code != string(apierr.CodeIntegrationAmountMismatch) || !got.Events[0].Retriable || got.Events[0].State != EventFailed {
		t.Fatalf("list = %+v", got)
	}
	// A member sees and does nothing.
	for name, c := range map[string]*client{"a member": member} {
		for _, route := range []struct{ method, path string }{{http.MethodGet, list}, {http.MethodPost, retry}} {
			if rec := c.do(route.method, route.path, nil); rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
				t.Errorf("%s: %s %s answered %d", name, route.method, route.path, rec.Code)
			}
		}
	}
	// Through another workspace's own path (the owner is an admin of both), the integration isn't found.
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, wsPath(foreignWs, fmt.Sprintf("integrations/%d/events", p.in.ID))},
		{http.MethodPost, wsPath(foreignWs, fmt.Sprintf("integrations/%d/events/%d/retry", p.in.ID, failed.id))},
	} {
		if rec := foreign.do(route.method, route.path, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s answered %d, want 404", route.method, route.path, rec.Code)
		}
	}
	if p.lastEvent(EventPaymentSucceeded).state != EventFailed {
		t.Fatal("a refused retry changed the event")
	}
	// An event of another integration is not found under this one, and an event that didn't fail isn't retried.
	other := p.create(p.owner, p.ws, stripeBody("other"))
	expect(t, p.owner.do(http.MethodPost, wsPath(p.ws, fmt.Sprintf("integrations/%d/events/%d/retry", other.ID, failed.id)), nil), http.StatusNotFound)
	rec = p.owner.do(http.MethodPost, wsPath(p.ws, fmt.Sprintf("integrations/%d/events/%d/retry", p.in.ID, 999999)), nil)
	expect(t, rec, http.StatusNotFound)
	p.put(Event{Kind: EventPaymentExpired, RefToken: ref.token, ObjectID: ref.object, AmountMinor: ref.amount, Currency: "EUR"})
	p.drain()
	applied := p.lastEvent(EventPaymentExpired)
	p.mustEvent(applied, EventIgnored, apierr.CodeIntegrationEventNoHandler)
	rec = p.owner.do(http.MethodPost, list+fmt.Sprintf("/%d/retry", applied.id), nil)
	expect(t, rec, http.StatusConflict)
	if code := decode[map[string]any](t, rec)["code"]; code != string(apierr.CodeIntegrationEventNotRetriable) {
		t.Fatalf("code = %v", code)
	}
	// Paging and the state filter.
	for i := range 4 {
		p.put(Event{Kind: EventPaymentExpired, RefToken: ref.token, ObjectID: fmt.Sprintf("cs_test_%d%s", i, token()), AmountMinor: ref.amount, Currency: "EUR"})
	}
	rec = p.owner.do(http.MethodGet, list+"?limit=2", nil)
	expect(t, rec, http.StatusOK)
	page := decode[EventList](t, rec)
	if len(page.Events) != 2 || page.NextBefore == 0 || page.Events[0].ID < page.Events[1].ID {
		t.Fatalf("page = %+v", page)
	}
	rec = p.owner.do(http.MethodGet, list+fmt.Sprintf("?limit=100&before=%d&state=failed", page.NextBefore+1000), nil)
	expect(t, rec, http.StatusOK)
	if only := decode[EventList](t, rec); len(only.Events) != 1 || only.Events[0].State != EventFailed {
		t.Fatalf("failed = %+v", only)
	}
	for _, bad := range []string{"?state=nope", "?limit=0", "?limit=101", "?before=x"} {
		expect(t, p.owner.do(http.MethodGet, list+bad, nil), http.StatusBadRequest)
	}
}

// ----- what the admins are shown in the inbox -----

func TestTheAdminsNotificationNamesTheIntegrationAndIsForAdminsOnly(t *testing.T) {
	p := newPay(t, publicURL)
	_, ref := p.sentInvoice(100, "EUR", "op-a-0001")
	member := p.invite(p.owner, p.ws, "member@example.test", "member")
	p.put(Event{Kind: EventPaymentSucceeded, RefToken: ref.token, ObjectID: "cs_test_" + token(), PaymentID: "pi_" + token()[:20], AmountMinor: 1, Currency: "EUR"})
	p.drain()

	type item struct {
		Kind   string            `json:"kind"`
		Params map[string]string `json:"params"`
		Target struct {
			Section string `json:"section"`
		} `json:"target"`
	}
	inbox := func(c *client) []item {
		rec := c.do(http.MethodGet, wsPath(p.ws, "notifications"), nil)
		expect(t, rec, http.StatusOK)
		return decode[struct {
			Notifications []item `json:"notifications"`
		}](t, rec).Notifications
	}
	got := inbox(p.owner)
	if len(got) != 1 || got[0].Kind != "integration_event" || got[0].Params["integration"] != "Stripe main" || got[0].Target.Section != "settings" {
		t.Fatalf("the admin's inbox: %+v", got)
	}
	if b, _ := json.Marshal(got); strings.Contains(string(b), ref.token) || strings.Contains(string(b), "amount") {
		t.Fatalf("the notification says more than it should: %s", b)
	}
	if n := len(inbox(member)); n != 0 {
		t.Fatalf("a member has %d notifications", n)
	}
	// An admin who is made a member is shown nothing of it.
	p.exec(`UPDATE memberships SET role = 'member' WHERE workspace_id = ?`, p.ws)
	got = inbox(p.owner)
	if len(got) != 1 || got[0].Kind != "changed" || len(got[0].Params) != 0 {
		t.Fatalf("after the role changed: %+v", got)
	}
}

// ----- a payment on a ref whose link's outcome is unknown -----

// unknownRef is a ref whose first call got a 5xx: the entry waits and looks for the object, the ref is
// "creating" with no object and a key of unknown outcome.
func (p *pay) unknownRef(op string) (record int64, r refRow) {
	p.t.Helper()
	shape := linkShapes["a checkout session"]
	record = p.unknownOutcome(shape, op)
	p.later(time.Minute)
	p.looked(shape.path, 1)
	r = p.refOf(record)
	if r.state != "creating" || r.object != "" {
		p.t.Fatalf("ref = %+v: want one with no object yet", r)
	}
	if n := p.unknownKeys(); n != 1 {
		p.t.Fatalf("%d refs of unknown outcome", n)
	}
	return record, r
}

// The payer used a link whose creation Stripe never answered: the payment is real money. It is applied
// to the record, the ref learns the object from the event and its key is settled, no second link is
// made, and the entry that asked for the link stops with the code that says it was paid.
func TestAPaymentForARefWhoseOutcomeWasUnknownIsAppliedAndNeverDropped(t *testing.T) {
	p := newPay(t, publicURL)
	path := linkShapes["a checkout session"].path
	record, _ := p.unknownRef("op-unknown-0001")
	p.appear(path, 0) // Stripe made the link after all, and a payer paid it
	calls := len(p.fake.RequestsTo(http.MethodPost, path))
	session := p.objectID(path)
	p.pays(session)

	p.mustEvent(p.lastEvent(EventPaymentSucceeded), EventApplied, "")
	if got := p.fields(record); got["status"] != "paid" {
		t.Fatalf("the record is %v: the payment was not applied", got)
	}
	var object, paidObject, payment string
	r := p.refOf(record)
	if err := p.db.QueryRow(`SELECT coalesce(provider_object_id, ''), coalesce(paid_object_id, ''), coalesce(payment_id, '')
		FROM integration_refs WHERE id = ?`, r.id).Scan(&object, &paidObject, &payment); err != nil {
		t.Fatal(err)
	}
	if r.state != "paid" || object != session || paidObject != session || !strings.HasPrefix(payment, "pi_") {
		t.Fatalf("ref = %+v, object %q, paid object %q, payment %q", r, object, paidObject, payment)
	}
	if k := p.attemptRow(r.id, 0); k.outcome != attemptSettled || k.object != session || p.unknownKeys() != 0 {
		t.Fatalf("the key the payment came through = %+v, %d unknown", k, p.unknownKeys())
	}

	// The entry runs again and finds the invoice paid: it makes no link and writes none over the record.
	p.later(time.Hour)
	e := p.settled(apps.StateFailed, apps.StateDone)
	if e.state != apps.StateFailed || e.code != string(apierr.CodeIntegrationLinkAlreadyPaid) {
		t.Fatalf("entry = %+v", e)
	}
	if n := len(p.fake.RequestsTo(http.MethodPost, path)); n != calls {
		t.Fatalf("%d calls that make an object, want %d: the entry made a second link", n, calls)
	}
	if n := p.objects(path); n != 1 {
		t.Fatalf("%d objects at Stripe", n)
	}
	if got := p.fields(record); got["status"] != "paid" || got["pay_url"] != nil && got["pay_url"] != "" {
		t.Fatalf("the record = %v", got)
	}

	// An admin is not asked whether a link exists: the list is empty, and a confirmation finds nothing to
	// decide, so no new key is allowed.
	rec := p.owner.do(http.MethodGet, p.confirmPath(e.id, "unresolved"), nil)
	expect(t, rec, http.StatusOK)
	if list := decode[PaymentLinkOutcomes](t, rec); len(list.PaymentLinks) != 0 {
		t.Fatalf("list = %+v", list)
	}
	rec = p.owner.do(http.MethodPost, p.confirmPath(e.id, fmt.Sprintf("%d/confirm-no-link", e.id)), map[string]any{})
	expect(t, rec, http.StatusConflict)
	if got := decode[struct {
		Code string `json:"code"`
	}](t, rec).Code; got != string(apierr.CodeIntegrationNothingToConfirm) {
		t.Fatalf("code = %q", got)
	}
	// A repeat of the payment's event changes nothing.
	p.put(Event{Kind: EventPaymentSucceeded, RefToken: r.token, ObjectID: session, PaymentID: payment, AmountMinor: 1000, Currency: "EUR"})
	p.drain()
	p.mustEvent(p.lastEvent(EventPaymentSucceeded), EventIgnored, apierr.CodeIntegrationEventDuplicate)
}

// A paid event that names no object still applies, and the ref stays with none: the payment is real
// whether or not the event can say what it was on, and the admin's list no longer shows it.
func TestAPaymentThatNamesNoObjectForAnUnknownRefIsStillApplied(t *testing.T) {
	p := newPayWith(t, publicURL, invoiceAppRefund)
	record, r := p.unknownRef("op-unknown-0002")
	calls := len(p.fake.RequestsTo(http.MethodPost, linkShapes["a checkout session"].path))
	payment := "pi_" + token()[:20]
	p.put(Event{Kind: EventPaymentSucceeded, RefToken: r.token, PaymentID: payment, AmountMinor: 1000, Currency: "EUR"})
	p.drain()

	p.mustEvent(p.lastEvent(EventPaymentSucceeded), EventApplied, "")
	if got := p.fields(record); got["status"] != "paid" {
		t.Fatalf("the record is %v", got)
	}
	if now := p.refOf(record); now.state != "paid" || now.object != "" {
		t.Fatalf("ref = %+v", now)
	}
	if n := p.count(`SELECT count(*) FROM integration_refs WHERE id = ? AND payment_id = ?`, r.id, payment); n != 1 {
		t.Fatal("the ref didn't learn its payment")
	}
	if k := p.attemptRow(r.id, 0); k.outcome != attemptSettled || p.unknownKeys() != 0 {
		t.Fatalf("the key = %+v, %d unknown", k, p.unknownKeys())
	}
	// Its entry stops without a new link, whatever the wait, and the admin has nothing to confirm.
	p.later(7 * time.Hour)
	if e := p.settled(apps.StateFailed); e.code != string(apierr.CodeIntegrationLinkAlreadyPaid) {
		t.Fatalf("entry = %+v", e)
	}
	if n := len(p.fake.RequestsTo(http.MethodPost, linkShapes["a checkout session"].path)); n != calls {
		t.Fatalf("%d calls that make an object, want the %d made before the payment", n, calls)
	}
	rec := p.owner.do(http.MethodPost, p.confirmPath(0, fmt.Sprintf("%d/confirm-no-link", p.entries()[0].id)), map[string]any{})
	expect(t, rec, http.StatusConflict)
	// A refund of all of it then applies through the payment the ref learned.
	p.put(Event{Kind: EventPaymentRefunded, PaymentID: payment, AmountMinor: 1000, Currency: "EUR"})
	p.drain()
	if p.refOf(record).state != "refunded" || p.fields(record)["status"] != "refunded" {
		t.Fatalf("the refund: %+v %v", p.refOf(record), p.fields(record))
	}
}

// An end that isn't money (expired, failed) for a ref with no object is not applied: the entry still
// looks for the link and may adopt it, which a ref moved past "creating" could no longer note.
func TestAnEndForARefWithNoObjectWaitsForTheLookup(t *testing.T) {
	for _, kind := range []string{EventPaymentExpired, EventPaymentFailed} {
		t.Run(kind, func(t *testing.T) {
			p := newPay(t, publicURL)
			record, r := p.unknownRef("op-unknown-0003")
			p.put(Event{Kind: kind, RefToken: r.token, ObjectID: "cs_test_" + token(), AmountMinor: 1000, Currency: "EUR"})
			p.drain()
			p.mustEvent(p.lastEvent(kind), EventIgnored, apierr.CodeIntegrationEventLate)
			if now := p.refOf(record); now.state != "creating" {
				t.Fatalf("ref = %+v", now)
			}
		})
	}
}

// unknownKeys is how many keys have an unknown outcome (a call under them may have made an object
// nobody has seen) and belong to a ref with no object.
func (p *pay) unknownKeys() int {
	return p.count(`SELECT count(*) FROM integration_ref_attempts k JOIN integration_refs r ON r.id = k.ref_id
		WHERE k.outcome IS NULL AND k.sends > k.clean_sends AND r.provider_object_id IS NULL`)
}

type attemptState struct{ outcome, object string }

// attemptRow is key seq of the ref.
func (p *pay) attemptRow(ref int64, seq int) attemptState {
	p.t.Helper()
	var k attemptState
	if err := p.db.QueryRow(`SELECT coalesce(outcome, ''), coalesce(object_id, '') FROM integration_ref_attempts WHERE ref_id = ? AND seq = ?`,
		ref, seq).Scan(&k.outcome, &k.object); err != nil {
		p.t.Fatal(err)
	}
	return k
}

// ----- a ref's payment is found through any of its keys' tokens -----

const sessionsPath = "/v1/checkout/sessions"

// keyedRef makes a ref whose key 0's session expired and whose key 1's call got a 5xx that made nothing
// (yet): the ref has two keys, the entry waits, and the ref has no object. It returns the record, the
// request of its entry and key 0's session.
func (p *pay) keyedRef(op string) (record int64, req paylink.Request, first string) {
	p.t.Helper()
	ctx := context.Background()
	record = p.invoice(10, "EUR")
	req = p.direct(record, op)
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, sessionsPath))
	if _, err := p.svc.OpenPaymentLink(ctx, req); err == nil {
		p.t.Fatal("the lost answer was no error")
	}
	first = p.fake.Sessions()[0].ID
	if _, err := p.fake.Expire(first); err != nil {
		p.t.Fatal(err)
	}
	p.fake.Fail(testprovider.ServerError().KeptUnderKey().On(http.MethodPost, sessionsPath))
	_, err := p.svc.OpenPaymentLink(ctx, req)
	var pe *paylink.Error
	if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationLinkLookupPending {
		p.t.Fatalf("key 1's 500: err = %v", err)
	}
	if r := p.refOf(record); r.keySeq != 1 || r.object != "" || r.state != "creating" {
		p.t.Fatalf("ref = %+v", r)
	}
	return record, req, first
}

// A payment made through a later key echoes that key's token, not the ref's: it finds the ref, is
// applied once, and the key it came through stops being unknown. The ref is paid, so the entry makes
// nothing and an admin has nothing to confirm.
func TestAPaymentThroughALaterKeysTokenAppliesOnceAndSettlesThatKey(t *testing.T) {
	ctx := context.Background()
	p := newPay(t, publicURL)
	record, req, first := p.keyedRef("op-later-0001")
	p.appear(sessionsPath, len(p.posts())-1) // key 1's call made its session after all, and a payer paid it
	posts := len(p.posts())
	var second string
	for _, s := range p.fake.Sessions() {
		if s.ID != first {
			second = s.ID
		}
	}
	r := p.refOf(record)
	token1 := p.keyToken(1)
	if token1 == r.token {
		t.Fatal("key 1 has the ref's token")
	}
	p.pays(second)

	e := p.lastEvent(EventPaymentSucceeded)
	if e.token != token1 {
		t.Fatalf("the event carries %q, want key 1's token", e.token)
	}
	p.mustEvent(e, EventApplied, "")
	if got := p.fields(record); got["status"] != "paid" {
		t.Fatalf("the record is %v: a payment through a later key was not applied", got)
	}
	r = p.refOf(record)
	if r.state != "paid" || r.object != second {
		t.Fatalf("ref = %+v", r)
	}
	if k := p.attemptRow(r.id, 1); k.outcome != attemptSettled || k.object != second {
		t.Fatalf("key 1 = %+v", k)
	}
	if k := p.attemptRow(r.id, 0); k.outcome != attemptExpired {
		t.Fatalf("key 0 = %+v: it keeps its own outcome", k)
	}
	if n := p.unknownKeys(); n != 0 {
		t.Fatalf("%d keys of unknown outcome", n)
	}
	rev, history := p.revision(record), p.historyRows(record)

	// The same payment told again, whichever token it carries, changes nothing.
	for _, token := range []string{token1, r.token} {
		p.put(Event{Kind: EventPaymentSucceeded, RefToken: token, ObjectID: second, PaymentID: e.paymentI, AmountMinor: 1000, Currency: "EUR"})
		p.drain()
		p.mustEvent(p.lastEvent(EventPaymentSucceeded), EventIgnored, apierr.CodeIntegrationEventDuplicate)
	}
	if p.revision(record) != rev || p.historyRows(record) != history {
		t.Fatal("the payment was applied twice")
	}

	// The entry stops with the paid code and makes no call.
	_, err := p.svc.OpenPaymentLink(ctx, req)
	var pe *paylink.Error
	if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationLinkAlreadyPaid {
		t.Fatalf("err = %v", err)
	}
	if n := len(p.posts()); n != posts {
		t.Fatalf("%d calls, want the %d made before the payment", n, posts)
	}
}

// A payment through the token of an earlier key (an object an admin's confirmation had given up on,
// which a payer used all the same) is applied to the ref, and the ref's later key, whose outcome is
// unknown, is nobody's to confirm: the ref is paid, so the list is empty, a confirmation refuses and
// no key is taken, even when the event names no object to learn.
func TestAPaymentThroughAnEarlierKeyStopsTheConfirmationOfTheNext(t *testing.T) {
	ctx := context.Background()
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-later-0002")
	p.fake.Fail(testprovider.ServerError().KeptUnderKey().On(http.MethodPost, sessionsPath))
	if _, err := p.svc.OpenPaymentLink(ctx, req); err == nil {
		t.Fatal("the 500 was no error")
	}
	p.later(7 * time.Hour)
	_, err := p.svc.OpenPaymentLink(ctx, req)
	var pe *paylink.Error
	if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationOutcomeUnknown {
		t.Fatalf("after the wait: err = %v", err)
	}
	p.exec(`UPDATE app_effects SET state = 'failed', error_code = ? WHERE id = ?`, string(apierr.CodeIntegrationOutcomeUnknown), req.EffectID)
	spaces := map[int64]bool{p.space.ID: true}
	if err := p.svc.ConfirmNoPaymentLink(ctx, p.ws, p.owner.id, p.in.ID, req.EffectID, spaces); err != nil {
		t.Fatal(err)
	}
	// Key 1's call gets a 5xx too: the ref waits on its outcome.
	p.fake.Fail(testprovider.ServerError().KeptUnderKey().On(http.MethodPost, sessionsPath))
	if _, err := p.svc.OpenPaymentLink(ctx, req); err == nil {
		t.Fatal("key 1's 500 was no error")
	}
	r := p.refOf(record)
	if r.keySeq != 1 || p.unknownKeys() != 1 {
		t.Fatalf("ref = %+v, %d unknown keys", r, p.unknownKeys())
	}
	token0 := p.keyToken(0)

	// Key 0's call made its session after all, and the payer paid it: the event names only the payment.
	payment := "pi_" + token()[:20]
	p.put(Event{Kind: EventPaymentSucceeded, RefToken: token0, PaymentID: payment, AmountMinor: 1000, Currency: "EUR"})
	p.drain()
	p.mustEvent(p.lastEvent(EventPaymentSucceeded), EventApplied, "")
	if got := p.fields(record); got["status"] != "paid" {
		t.Fatalf("the record is %v", got)
	}
	r = p.refOf(record)
	if r.state != "paid" || r.object != "" || r.keySeq != 1 {
		t.Fatalf("ref = %+v", r)
	}
	if k := p.attemptRow(r.id, 0); k.outcome != "abandoned" {
		t.Fatalf("key 0 = %+v: it keeps its own outcome", k)
	}
	if k := p.attemptRow(r.id, 1); k.outcome != "" {
		t.Fatalf("key 1 = %+v: the payment said nothing of it", k)
	}

	// Nothing is left for an admin: the entry is failed for an unknown outcome as it was, and the ref
	// is paid.
	p.exec(`UPDATE app_effects SET state = 'failed', error_code = ? WHERE id = ?`, string(apierr.CodeIntegrationOutcomeUnknown), req.EffectID)
	rec := p.owner.do(http.MethodGet, p.confirmPath(req.EffectID, "unresolved"), nil)
	expect(t, rec, http.StatusOK)
	if list := decode[PaymentLinkOutcomes](t, rec); len(list.PaymentLinks) != 0 {
		t.Fatalf("list = %+v", list.PaymentLinks)
	}
	rec = p.owner.do(http.MethodPost, p.confirmPath(req.EffectID, fmt.Sprintf("%d/confirm-no-link", req.EffectID)), map[string]any{})
	expect(t, rec, http.StatusConflict)
	if got := decode[struct {
		Code string `json:"code"`
	}](t, rec).Code; got != string(apierr.CodeIntegrationNothingToConfirm) {
		t.Fatalf("code = %q", got)
	}
	if now := p.refOf(record); now.keySeq != 1 {
		t.Fatalf("a refused confirmation moved the key: %+v", now)
	}
	if n := p.count(`SELECT count(*) FROM audit_events WHERE action = 'integration_new_link_allowed'`); n != 1 {
		t.Fatalf("%d confirmations in the audit log, want the first only", n)
	}
}

// A payment for a token the server never issued maps to nothing, whatever else the event says: not the
// ref's object, not its payment, not a token of the same shape.
func TestAPaymentForATokenTheServerNeverIssuedIsIgnored(t *testing.T) {
	p := newPay(t, publicURL)
	record, r := p.sentInvoice(100, "EUR", "op-unissued-0001")
	before, rev := p.fields(record), p.revision(record)
	for name, ref := range map[string]string{
		"a made-up token":            strings.Repeat("e", 32),
		"the ref's, upcase":          strings.ToUpper(r.token),
		"a token of the right shape": strings.Repeat("0a", 16), // 32 lowercase hex, built here so no literal looks like a key
	} {
		p.put(Event{Kind: EventPaymentSucceeded, RefToken: ref, ObjectID: r.object, PaymentID: "pi_" + token()[:20], AmountMinor: r.amount, Currency: "EUR"})
		p.drain()
		if e := p.lastEvent(EventPaymentSucceeded); e.state != EventIgnored || e.code != string(apierr.CodeIntegrationEventRefUnknown) {
			t.Fatalf("%s: event = %+v", name, e)
		}
	}
	if now := p.refOf(record); now.state != "open" || fmt.Sprint(p.fields(record)) != fmt.Sprint(before) || p.revision(record) != rev {
		t.Fatalf("an unissued token moved something: %+v %v", now, p.fields(record))
	}
	if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE outcome = 'settled'`); n != 0 {
		t.Fatalf("%d keys settled by an event for no ref", n)
	}
}

// An expired session of an earlier key says nothing of the link the ref has now: the ref stays open.
// The later key's own expiry ends it.
func TestAnEndOfAnEarlierKeysObjectLeavesTheLaterLinkAlone(t *testing.T) {
	ctx := context.Background()
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-later-0003")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, sessionsPath))
	if _, err := p.svc.OpenPaymentLink(ctx, req); err == nil {
		t.Fatal("the lost answer was no error")
	}
	first := p.fake.Sessions()[0].ID
	evs, err := p.fake.Expire(first)
	if err != nil {
		t.Fatal(err)
	}
	link, err := p.svc.OpenPaymentLink(ctx, req)
	if err != nil || link.ObjectID == first {
		t.Fatalf("link = %+v, %v", link, err)
	}
	r := p.refOf(record)
	if r.keySeq != 1 || r.state != "open" || r.object != link.ObjectID {
		t.Fatalf("ref = %+v", r)
	}

	// Stripe tells of the first session's expiry, with the first key's token.
	p.deliver(p.sender(p.hookID(), stripeHook), evs)
	p.drain()
	if e := p.lastEvent(EventPaymentExpired); e.token != p.keyToken(0) {
		t.Fatalf("the event carries %q, want key 0's token", e.token)
	} else {
		p.mustEvent(e, EventIgnored, apierr.CodeIntegrationEventLate)
	}
	if now := p.refOf(record); now.state != "open" {
		t.Fatalf("an old key's expiry ended the ref: %+v", now)
	}

	// The live link's own expiry, with its own key's token, does.
	p.put(Event{Kind: EventPaymentExpired, RefToken: p.keyToken(1), ObjectID: link.ObjectID, AmountMinor: 1000, Currency: "EUR"})
	p.drain()
	p.mustEvent(p.lastEvent(EventPaymentExpired), EventIgnored, apierr.CodeIntegrationEventNoHandler)
	if now := p.refOf(record); now.state != "expired" {
		t.Fatalf("ref = %+v", now)
	}
}
