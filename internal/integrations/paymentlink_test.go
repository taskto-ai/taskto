package integrations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/integrations/testprovider"
)

const publicURL = "https://tasks.example.com"

// invoiceApp is an invoicing app: sending an invoice asks for a payment link of its total in euros.
const invoiceApp = `{
  "components": [{"id":"root","component":"Text","text":"Invoices"}],
  "collection": {"fields": [
    {"name": "number", "type": "string", "required": true},
    {"name": "customer", "type": "string"},
    {"name": "total", "type": "number", "required": true},
    {"name": "currency", "type": "choice", "options": ["EUR", "USD", "JPY"]},
    {"name": "status", "type": "choice", "options": ["draft", "sent", "overdue", "paid"]},
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

// pay is a workspace whose owner built, approved and published an invoicing app that asks Stripe
// (the fake) for payment links, with a worker that can be started and a clock that can be moved.
type pay struct {
	*env
	fake  *testprovider.Stripe
	svc   *Service
	apps  *apps.Service
	owner *client
	ws    int64
	space access.Space
	in    Integration
	app   apps.App
	// offset moves the apps' clock on, in nanoseconds.
	offset atomic.Int64
	stop   func()
}

func providerKinds(provider string) []string {
	if p := Lookup(provider); p != nil {
		return p.Kinds()
	}
	return nil
}

func newPay(t *testing.T, public string) *pay {
	t.Helper()
	ctx := context.Background()
	e := newEnv(t, public) // the database, the key and the logs: the env's own routes are left unused
	fake := testprovider.NewStripe(t)
	svc := New(e.store, Options{PublicURL: public, Egress: ptr(egress.MustParse("127.0.0.1")),
		ClientOptions: []ClientOption{WithTestEndpoint(fake.Host())}})

	mux := http.NewServeMux()
	a := auth.New(e.db, auth.Options{HashParams: auth.Params{Memory: 64, Time: 1, Threads: 1}, SetupToken: setupToken})
	a.Routes(mux)
	spaces := access.New(e.db, a)
	spaces.Routes(mux)
	svc.Routes(mux, a, spaces)
	p := &pay{env: e, fake: fake, svc: svc}
	p.apps = apps.New(e.db, spaces).WithPlatform(apps.Platform{ProviderKinds: providerKinds, Payments: svc}).
		WithClock(func() time.Time { return time.Now().Add(time.Duration(p.offset.Load())) })
	p.apps.Routes(mux)
	p.apps.ApprovalRoutes(mux, a)
	e.h = auth.CSRF(mux)

	p.owner, p.ws = e.setup()
	rec := p.owner.do(http.MethodPost, wsPath(p.ws, "spaces"), map[string]string{"name": "Team"})
	expect(t, rec, http.StatusCreated)
	p.space = decode[access.Space](t, rec)

	body := stripeBody("main")
	body["secrets"] = map[string]string{"api_key": fake.APIKey()}
	body["currencies"] = []string{"EUR", "JPY"}
	p.in = e.create(p.owner, p.ws, body)
	if _, err := e.db.ExecContext(ctx, `INSERT INTO integration_spaces (integration_id, space_id) VALUES (?, ?)`, p.in.ID, p.space.ID); err != nil {
		t.Fatal(err)
	}

	rec = p.owner.do(http.MethodPost, p.appsPath(""), map[string]any{"name": "Invoices", "definition": rawJSON(invoiceApp)})
	expect(t, rec, http.StatusCreated)
	p.app = decode[apps.App](t, rec)
	rec = p.owner.do(http.MethodGet, p.appsPath(fmt.Sprintf("/%d/integrations", p.app.ID)), nil)
	expect(t, rec, http.StatusOK)
	hash := decode[apps.IntegrationStatus](t, rec).Slots[0].ContractHash
	rec = p.owner.do(http.MethodPost, p.appsPath(fmt.Sprintf("/%d/integrations/payments/request", p.app.ID)), map[string]any{"contract_hash": hash})
	expect(t, rec, http.StatusOK)
	approval := decode[apps.IntegrationApproval](t, rec)
	expect(t, p.owner.do(http.MethodPost, wsPath(p.ws, fmt.Sprintf("integration-approvals/%d/approve", approval.ID)),
		map[string]any{"integration_id": p.in.ID}), http.StatusOK)
	expect(t, p.owner.do(http.MethodPost, p.appsPath(fmt.Sprintf("/%d/publish", p.app.ID)),
		map[string]any{"version": p.app.Version, "acknowledge": []any{}}), http.StatusOK)
	return p
}

func ptr[T any](v T) *T { return &v }

type rawJSON string

func (r rawJSON) MarshalJSON() ([]byte, error) { return []byte(r), nil }

func (p *pay) appsPath(rest string) string {
	return fmt.Sprintf("/api/workspaces/%d/spaces/%d/apps%s", p.ws, p.space.ID, rest)
}

func (p *pay) act(operation, name string, ctx map[string]any) {
	p.t.Helper()
	a := apps.ClientAction{Name: name, SurfaceID: apps.SurfaceID(p.app.ID), SourceComponentID: "add", Context: ctx, Operation: operation}
	expect(p.t, p.owner.do(http.MethodPost, p.appsPath(fmt.Sprintf("/%d/actions", p.app.ID)), a), http.StatusOK)
}

// invoice adds an invoice with the total (a JSON number) in the currency, and returns its record.
func (p *pay) invoice(total any, currency string) int64 {
	p.t.Helper()
	p.act("", "add", map[string]any{"number": "A-1", "customer": "Acme", "total": total, "currency": currency, "status": "draft"})
	var id int64
	if err := p.db.QueryRow(`SELECT max(id) FROM app_rows WHERE app_id = ?`, p.app.ID).Scan(&id); err != nil {
		p.t.Fatal(err)
	}
	return id
}

func (p *pay) send(record int64, operation string) {
	p.t.Helper()
	p.act(operation, "send", map[string]any{"id": record, "status": "sent"})
}

// startWorker runs the apps' outbox worker, which does what is queued.
func (p *pay) startWorker() {
	p.t.Helper()
	p.stop = p.apps.StartOutbox(context.Background())
	p.t.Cleanup(p.stop)
}

// later moves the apps' clock on, so entries that wait to be tried again are due.
func (p *pay) later(d time.Duration) { p.offset.Add(int64(d)) }

type entry struct {
	id       int64
	state    string
	attempts int
	code     string
	params   sql.NullString
	result   sql.NullString
}

func (p *pay) entries() []entry {
	p.t.Helper()
	rows, err := p.db.Query(`SELECT id, state, attempts, coalesce(error_code, ''), error_params, result FROM app_effects ORDER BY id`)
	if err != nil {
		p.t.Fatal(err)
	}
	defer rows.Close()
	var out []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.id, &e.state, &e.attempts, &e.code, &e.params, &e.result); err != nil {
			p.t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// settled waits for the only entry to be in one of the states, and returns it.
func (p *pay) settled(states ...string) entry {
	p.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if es := p.entries(); len(es) == 1 {
			for _, s := range states {
				if es[0].state == s {
					return es[0]
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.t.Fatalf("the entry never reached %v: %+v", states, p.entries())
	return entry{}
}

// waitFor waits for cond, which polls.
func (p *pay) waitFor(what string, cond func() bool) {
	p.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.t.Fatalf("never: %s; entries %+v", what, p.entries())
}

func (p *pay) record(id int64) (data string, revision int64) {
	p.t.Helper()
	if err := p.db.QueryRow(`SELECT data, revision FROM app_rows WHERE id = ?`, id).Scan(&data, &revision); err != nil {
		p.t.Fatal(err)
	}
	return data, revision
}

func (p *pay) count(q string, args ...any) (n int) {
	p.t.Helper()
	if err := p.db.QueryRow(q, args...).Scan(&n); err != nil {
		p.t.Fatal(err)
	}
	return n
}

func (p *pay) posts() []testprovider.Request {
	return p.fake.RequestsTo(http.MethodPost, "/v1/checkout/sessions")
}

type refRow struct {
	id                  int64
	token, state        string
	object, url         string
	amount              int64
	currency            string
	effect              sql.NullInt64
	keySeq              int
	integration, record int64
}

func (p *pay) refs() []refRow {
	p.t.Helper()
	rows, err := p.db.Query(`SELECT id, token, state, coalesce(provider_object_id, ''), coalesce(url, ''), amount_minor, currency, effect_id, key_seq, integration_id, record_id
		FROM integration_refs ORDER BY id`)
	if err != nil {
		p.t.Fatal(err)
	}
	defer rows.Close()
	var out []refRow
	for rows.Next() {
		var r refRow
		if err := rows.Scan(&r.id, &r.token, &r.state, &r.object, &r.url, &r.amount, &r.currency, &r.effect, &r.keySeq, &r.integration, &r.record); err != nil {
			p.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// ----- the link is made once -----

func TestAnInvoiceSentMakesOneStripePaymentLinkWrittenBackBySystem(t *testing.T) {
	p := newPay(t, publicURL)
	record := p.invoice(120.1, "EUR")
	p.startWorker()
	p.send(record, "op-send-0001")
	e := p.settled(apps.StateDone)

	reqs := p.posts()
	if len(reqs) != 1 {
		t.Fatalf("%d calls to Stripe: %+v", len(reqs), reqs)
	}
	refs := p.refs()
	if len(refs) != 1 || refs[0].state != "open" || refs[0].effect.Int64 != e.id || refs[0].integration != p.in.ID || refs[0].record != record ||
		refs[0].amount != 12010 || refs[0].currency != "EUR" || len(refs[0].token) != 32 {
		t.Fatalf("refs = %+v", refs)
	}
	ref, call := refs[0], reqs[0]
	// The token is the idempotency key and the reference the provider echoes back.
	if call.IdempotencyKey != ref.token || call.Form.Get("client_reference_id") != ref.token {
		t.Fatalf("the call's key and reference: %+v", call)
	}
	if call.APIVersion != StripeAPIVersion || call.Auth != "bearer" {
		t.Fatalf("the call's version and auth: %+v", call)
	}
	// The success and cancel pages are the server's own, from the public URL.
	if call.Form.Get("success_url") != publicURL+PaymentDonePath || call.Form.Get("cancel_url") != publicURL+PaymentCancelledPath {
		t.Fatalf("pages: %q %q", call.Form.Get("success_url"), call.Form.Get("cancel_url"))
	}
	// The amount is exact, in the currency's smallest unit.
	if call.Form.Get("line_items[0][price_data][unit_amount]") != "12010" || call.Form.Get("line_items[0][price_data][currency]") != "eur" ||
		call.Form.Get("line_items[0][price_data][product_data][name]") != "Invoice A-1 for Acme" || call.Form.Get("mode") != "payment" {
		t.Fatalf("form = %v", call.Form)
	}
	sessions := p.fake.Sessions()
	if len(sessions) != 1 || sessions[0].ID != ref.object || sessions[0].AmountTotal != 12010 {
		t.Fatalf("sessions = %+v", sessions)
	}
	// The link and its id are in the record, written by the integration, not by a person.
	data, _ := p.record(record)
	if !strings.Contains(data, `"payment_id":"`+ref.object+`"`) || !strings.Contains(data, ref.url) || ref.url != *sessions[0].URL {
		t.Fatalf("record = %s, ref = %+v", data, ref)
	}
	var system string
	if err := p.db.QueryRow(`SELECT updated_system FROM app_rows WHERE id = ?`, record).Scan(&system); err != nil || system != fmt.Sprintf("integration:%d", p.in.ID) {
		t.Fatalf("updated_system = %q, %v", system, err)
	}
	var actor sql.NullInt64
	var actorSystem string
	if err := p.db.QueryRow(`SELECT actor, actor_system FROM app_row_history WHERE row_id = ? ORDER BY revision DESC LIMIT 1`, record).Scan(&actor, &actorSystem); err != nil ||
		actor.Valid || actorSystem != system {
		t.Fatalf("history actor = %v %q, %v", actor, actorSystem, err)
	}
	// Doing the same send again, with the same operation, makes no second link.
	p.send(record, "op-send-0001")
	time.Sleep(200 * time.Millisecond)
	if n := len(p.posts()); n != 1 {
		t.Fatalf("a repeated request made %d calls", n)
	}
}

// The provider answered, and the server died before it noted the answer: the entry is done again,
// asks again under the same key, and Stripe answers with the object it made.
func TestACrashBeforeTheAnswerIsNotedMakesNoSecondLink(t *testing.T) {
	p := newPay(t, publicURL)
	record := p.invoice(50, "EUR")
	// The link is made at Stripe, and the connection drops before the answer reaches the server.
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	p.startWorker()
	p.send(record, "op-send-0002")
	p.waitFor("the first call", func() bool { return len(p.posts()) == 1 })
	p.waitFor("the entry waits", func() bool { es := p.entries(); return len(es) == 1 && es[0].state == "pending" && es[0].attempts == 1 })
	if r := p.refs(); len(r) != 1 || r[0].object != "" || r[0].state != "creating" {
		t.Fatalf("refs after the dropped answer = %+v", r)
	}
	p.later(time.Minute)
	p.settled(apps.StateDone)

	reqs := p.posts()
	if len(reqs) != 2 || reqs[0].IdempotencyKey != reqs[1].IdempotencyKey || !reqs[1].Replayed {
		t.Fatalf("calls = %+v: the retry should be a replay under the same key", reqs)
	}
	if n := len(p.fake.Sessions()); n != 1 {
		t.Fatalf("%d sessions at Stripe, want 1", n)
	}
	refs := p.refs()
	if len(refs) != 1 || refs[0].state != "open" || refs[0].object != p.fake.Sessions()[0].ID {
		t.Fatalf("refs = %+v", refs)
	}
}

// The link was noted and the entry crashed before it was done: it asks Stripe nothing.
func TestAnEntryDoneAgainAfterTheLinkWasNotedDoesNotCallStripe(t *testing.T) {
	p := newPay(t, publicURL)
	record := p.invoice(99.99, "EUR")
	p.send(record, "op-send-0003")
	entry := p.entries()[0]
	req := apps.PaymentLinkRequest{WorkspaceID: p.ws, IntegrationID: p.in.ID, SpaceID: p.space.ID, AppID: p.app.ID, RecordID: record, EffectID: entry.id,
		Slot: "payments", Amount: "99.99", Currency: "EUR", Description: "Invoice"}
	first, err := p.svc.OpenPaymentLink(context.Background(), req)
	if err != nil || first.AmountMinor != 9999 || first.ObjectID == "" || first.URL == "" {
		t.Fatalf("first = %+v, %v", first, err)
	}
	again, err := p.svc.OpenPaymentLink(context.Background(), req)
	if err != nil || again != first {
		t.Fatalf("again = %+v, %v", again, err)
	}
	if n := len(p.posts()); n != 1 {
		t.Fatalf("%d calls", n)
	}

	// And one that crashed before it noted the answer asks again under the same key and gets the
	// same object.
	if _, err := p.db.Exec(`UPDATE integration_refs SET provider_object_id = NULL, url = NULL, state = 'creating'`); err != nil {
		t.Fatal(err)
	}
	third, err := p.svc.OpenPaymentLink(context.Background(), req)
	if err != nil || third.ObjectID != first.ObjectID || third.URL != first.URL {
		t.Fatalf("third = %+v, %v", third, err)
	}
	reqs := p.posts()
	if len(reqs) != 2 || reqs[0].IdempotencyKey != reqs[1].IdempotencyKey || !reqs[1].Replayed || len(p.fake.Sessions()) != 1 {
		t.Fatalf("calls = %+v, sessions %d", reqs, len(p.fake.Sessions()))
	}
	if n := p.count(`SELECT count(*) FROM integration_refs`); n != 1 {
		t.Fatalf("%d refs for one entry", n)
	}
}

// Stripe keeps a failure it answered under the key, and would answer it again to the same key for
// a day: the next try has a key of its own.
func TestAFailureStripeKeepsIsNotAskedAgainUnderItsKey(t *testing.T) {
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	p.fake.Fail(testprovider.ServerError().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	p.startWorker()
	p.send(record, "op-send-0004")
	p.waitFor("the first call", func() bool { return len(p.posts()) == 1 })
	p.waitFor("the entry waits", func() bool { es := p.entries(); return len(es) == 1 && es[0].state == "pending" })
	p.later(time.Minute)
	e := p.settled(apps.StateDone)
	reqs := p.posts()
	if len(reqs) != 2 || reqs[0].IdempotencyKey == reqs[1].IdempotencyKey || !strings.HasPrefix(reqs[1].IdempotencyKey, reqs[0].IdempotencyKey+"-") {
		t.Fatalf("keys: %+v", reqs)
	}
	if refs := p.refs(); len(refs) != 1 || refs[0].keySeq != 1 || refs[0].state != "open" || refs[0].effect.Int64 != e.id {
		t.Fatalf("refs = %+v", refs)
	}
}

// ----- money -----

func TestAmountsAreExactOrRefusedNeverRounded(t *testing.T) {
	cases := map[string]struct {
		total    any
		currency string
		minor    string // what Stripe is asked for; "" when the amount is refused
		code     apierr.Code
	}{
		"cents":                  {120.1, "EUR", "12010", ""},
		"whole yen":              {5000, "JPY", "5000", ""},
		"the least Stripe takes": {0.5, "EUR", "50", ""},
		"sum of floats":          {floatNoise(), "EUR", "", apierr.CodeIntegrationAmountRefused},
		"three decimals":         {10.005, "EUR", "", apierr.CodeIntegrationAmountRefused},
		"decimals in yen":        {10.5, "JPY", "", apierr.CodeIntegrationAmountRefused},
		"zero":                   {0, "EUR", "", apierr.CodeIntegrationAmountRefused},
		"negative":               {-1, "EUR", "", apierr.CodeIntegrationAmountRefused},
		"above the ceiling":      {5000.01, "EUR", "", apierr.CodeIntegrationAmountAboveMax},
		"at the ceiling":         {5000, "EUR", "500000", ""},
		"huge":                   {1e300, "EUR", "", apierr.CodeIntegrationAmountAboveMax},
		"a currency not listed":  {10, "USD", "", apierr.CodeIntegrationCurrencyRefused},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := newPay(t, publicURL)
			record := p.invoice(tc.total, tc.currency)
			p.startWorker()
			p.send(record, "op-money-0001")
			if tc.code == "" {
				p.settled(apps.StateDone)
				reqs := p.posts()
				if len(reqs) != 1 || reqs[0].Form.Get("line_items[0][price_data][unit_amount]") != tc.minor {
					t.Fatalf("calls = %+v, want amount %s", reqs, tc.minor)
				}
				return
			}
			e := p.settled(apps.StateFailed)
			if e.code != string(tc.code) {
				t.Fatalf("code = %q, want %q", e.code, tc.code)
			}
			// Refused before anything was made or asked.
			if n := len(p.fake.Requests()); n != 0 {
				t.Fatalf("Stripe was called %d times", n)
			}
			if n := p.count(`SELECT count(*) FROM integration_refs`); n != 0 {
				t.Fatalf("a refused amount left %d refs", n)
			}
			if data, _ := p.record(record); strings.Contains(data, "pay_url\":\"h") {
				t.Fatalf("the record got a link: %s", data)
			}
			if tc.code == apierr.CodeIntegrationAmountAboveMax && !strings.Contains(e.params.String, `"max":"5000.00"`) {
				t.Fatalf("params = %q", e.params.String)
			}
		})
	}
}

func floatNoise() float64 {
	a, b := 0.1, 0.2
	return a + b
}

func TestWithoutAPublicURLNoLinkIsMade(t *testing.T) {
	p := newPay(t, "")
	record := p.invoice(10, "EUR")
	p.startWorker()
	p.send(record, "op-url-0001")
	e := p.settled(apps.StateFailed)
	if e.code != string(apierr.CodeIntegrationPublicURLMissing) || len(p.fake.Requests()) != 0 || p.count(`SELECT count(*) FROM integration_refs`) != 0 {
		t.Fatalf("entry = %+v, calls %d", e, len(p.fake.Requests()))
	}
}

// ----- failures of the provider -----

func TestWhatTheProviderRefusesFailsTheEntryWithACodeAndLeavesNoProviderText(t *testing.T) {
	for name, tc := range map[string]struct {
		fault testprovider.Fault
		code  apierr.Code
		state string
	}{
		"a refused request":    {testprovider.Refusal("amount_too_small"), apierr.CodeIntegrationProviderRefused, "failed"},
		"a revoked key":        {testprovider.Unauthorized(), apierr.CodeIntegrationCredentialsRefused, "failed"},
		"stripe down for good": {testprovider.Unavailable().Forever(), apierr.CodeIntegrationProviderUnreachable, "pending"},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPay(t, publicURL)
			record := p.invoice(10, "EUR")
			p.fake.Fail(tc.fault.On(http.MethodPost, "/v1/checkout/sessions"))
			p.startWorker()
			p.send(record, "op-fail-0001")
			p.waitFor("the entry's failure", func() bool {
				es := p.entries()
				return len(es) == 1 && es[0].code == string(tc.code) && (es[0].state == tc.state)
			})
			if tc.state == "failed" {
				// The ref is failed, and a retry by the person who acted makes the link.
				if r := p.refs(); len(r) != 1 || r[0].state != "failed" || r[0].object != "" {
					t.Fatalf("refs = %+v", r)
				}
				p.fake.ClearFaults()
				expect(t, p.owner.do(http.MethodPost, p.appsPath(fmt.Sprintf("/%d/effects/%d/retry", p.app.ID, p.entries()[0].id)), map[string]any{}), http.StatusOK)
				p.settled(apps.StateDone)
				if r := p.refs(); len(r) != 1 || r[0].state != "open" || len(p.fake.Sessions()) != 1 {
					t.Fatalf("after the retry: refs = %+v, sessions %d", r, len(p.fake.Sessions()))
				}
			}
			// What the provider said is nowhere: not in the entry, the API, or the log.
			for _, hay := range []string{p.logs.String(), p.bodies.String(), p.entries()[0].params.String, p.entries()[0].result.String} {
				for _, leaked := range []string{"Invalid API Key", "was refused", "temporarily unavailable", "amount_too_small", p.fake.APIKey()} {
					if strings.Contains(hay, leaked) {
						t.Fatalf("%q leaked", leaked)
					}
				}
			}
		})
	}
}

// A provider that doesn't answer is tried again, and the entry waits for the person at last.
func TestAProviderThatNeverAnswersExhaustsTheEntry(t *testing.T) {
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	p.fake.Fail(testprovider.ConnectionReset().Forever().On(http.MethodPost, "/v1/checkout/sessions"))
	p.startWorker()
	p.send(record, "op-down-0001")
	for i := 0; i < 6; i++ {
		p.waitFor("a try", func() bool { return len(p.posts()) >= i+1 || p.entries()[0].state == "failed" })
		p.later(10 * time.Minute)
		time.Sleep(50 * time.Millisecond)
	}
	e := p.settled(apps.StateFailed)
	if e.code != string(apierr.CodeIntegrationProviderUnreachable) || e.attempts != 5 {
		t.Fatalf("entry = %+v", e)
	}
	// Every try was under the same key: whichever of them Stripe executed is the one link there is.
	reqs := p.posts()
	for _, r := range reqs {
		if r.IdempotencyKey != reqs[0].IdempotencyKey {
			t.Fatalf("keys differ: %+v", reqs)
		}
	}
	if refs := p.refs(); len(refs) != 1 || refs[0].state != "creating" {
		t.Fatalf("refs = %+v", refs)
	}
}

// ----- isolation -----

// The integration is looked for in the app's own workspace: another workspace's integration is
// never opened, and the port refuses a workspace that isn't the integration's.
func TestAnIntegrationOfAnotherWorkspaceIsNeverUsed(t *testing.T) {
	p := newPay(t, publicURL)
	_, other := p.otherWorkspace(p.owner)
	body := stripeBody("theirs")
	body["secrets"] = map[string]string{"api_key": p.fake.APIKey()}
	theirs := p.create(p.owner, other, body)
	record := p.invoice(10, "EUR")

	// The schema keeps an integration out of another workspace's spaces and approvals.
	if _, err := p.db.Exec(`INSERT INTO integration_spaces (integration_id, space_id) VALUES (?, ?)`, theirs.ID, p.space.ID); err == nil {
		t.Fatal("their integration was allowed in this workspace's space")
	}
	if _, err := p.db.Exec(`UPDATE app_integration_approvals SET integration_id = ?`, theirs.ID); err == nil {
		t.Fatal("the approval was bound to their integration")
	}
	p.send(record, "op-cross-0001")
	e := p.entries()[0]

	// The port itself, asked for another workspace's integration, says the same.
	_, err := p.svc.OpenPaymentLink(context.Background(), apps.PaymentLinkRequest{WorkspaceID: p.ws, IntegrationID: theirs.ID, SpaceID: p.space.ID,
		AppID: p.app.ID, RecordID: record, EffectID: e.id, Slot: "payments", Amount: "10", Currency: "EUR"})
	if !isUnavailable(err) {
		t.Fatalf("err = %v", err)
	}
	_, err = p.svc.OpenPaymentLink(context.Background(), apps.PaymentLinkRequest{WorkspaceID: other, IntegrationID: p.in.ID, SpaceID: p.space.ID,
		AppID: p.app.ID, RecordID: record, EffectID: e.id, Slot: "payments", Amount: "10", Currency: "EUR"})
	if !isUnavailable(err) {
		t.Fatalf("err = %v", err)
	}
	if len(p.fake.Requests()) != 0 || p.count(`SELECT count(*) FROM integration_refs`) != 0 {
		t.Fatal("Stripe was called, or a ref was made")
	}
}

// An entry's reference is its own: another record or app asking with that entry's number gets
// nothing of it.
func TestAnEntrysRefIsNotAnotherRecordsLink(t *testing.T) {
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	p.send(record, "op-own-0001")
	entry := p.entries()[0]
	req := apps.PaymentLinkRequest{WorkspaceID: p.ws, IntegrationID: p.in.ID, SpaceID: p.space.ID, AppID: p.app.ID, RecordID: record,
		EffectID: entry.id, Slot: "payments", Amount: "10", Currency: "EUR"}
	if _, err := p.svc.OpenPaymentLink(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req.RecordID = record + 1
	if _, err := p.svc.OpenPaymentLink(context.Background(), req); err == nil {
		t.Fatal("another record was given the entry's link")
	}
}

// A link is not made for a switched-off integration or a space it isn't allowed in.
func TestTheIntegrationIsCheckedWhenTheLinkIsAsked(t *testing.T) {
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	p.send(record, "op-gate-0001")
	entry := p.entries()[0]
	req := apps.PaymentLinkRequest{WorkspaceID: p.ws, IntegrationID: p.in.ID, SpaceID: p.space.ID, AppID: p.app.ID, RecordID: record,
		EffectID: entry.id, Slot: "payments", Amount: "10", Currency: "EUR"}
	expectUnavailable := func(why string) {
		t.Helper()
		_, err := p.svc.OpenPaymentLink(context.Background(), req)
		if !isUnavailable(err) {
			t.Fatalf("%s: err = %v", why, err)
		}
	}
	if _, err := p.db.Exec(`UPDATE integrations SET disabled_at = CURRENT_TIMESTAMP`); err != nil {
		t.Fatal(err)
	}
	expectUnavailable("switched off")
	if _, err := p.db.Exec(`UPDATE integrations SET disabled_at = NULL`); err != nil {
		t.Fatal(err)
	}
	req.SpaceID = p.space.ID + 100
	expectUnavailable("another space")
	if len(p.fake.Requests()) != 0 {
		t.Fatal("Stripe was called")
	}
}

// ----- the secret -----

// The key opens for the call and goes nowhere else: not the log, not an answer of the API, not the
// record, the entry or the ref.
func TestTheKeyNeverLeavesTheCall(t *testing.T) {
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	p.startWorker()
	p.send(record, "op-secret-0001")
	e := p.settled(apps.StateDone)
	key := p.fake.APIKey()
	var raw []string
	for _, q := range []string{`SELECT data FROM app_rows`, `SELECT data FROM app_row_history`, `SELECT payload FROM app_effects`,
		`SELECT coalesce(result, '') FROM app_effects`, `SELECT token || url || coalesce(provider_object_id, '') FROM integration_refs`,
		`SELECT coalesce(details, '') FROM audit_log`} {
		rows, err := p.db.Query(q)
		if err != nil {
			continue // a table this build doesn't have
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			raw = append(raw, s)
		}
		rows.Close()
	}
	listing := p.owner.do(http.MethodGet, p.appsPath(fmt.Sprintf("/%d/effects", p.app.ID)), nil).Body.String()
	raw = append(raw, p.logs.String(), p.bodies.String(), listing)
	for _, s := range raw {
		if strings.Contains(s, key) || strings.Contains(s, "Bearer ") {
			t.Fatalf("the key is somewhere it must not be: %.200s", s)
		}
	}
	// The call carried it as a bearer key, and only there.
	if reqs := p.posts(); len(reqs) != 1 || reqs[0].Auth != "bearer" {
		t.Fatalf("calls = %+v", reqs)
	}
	_ = e
}

// isUnavailable says whether err is the port's "integration_unavailable".
func isUnavailable(err error) bool {
	var pe *apps.PaymentLinkError
	return errors.As(err, &pe) && pe.Code == apierr.CodeIntegrationUnavailable
}
