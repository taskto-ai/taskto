package integrations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	"taskto.ai/taskto/internal/paylink"
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
		ClientOptions: []ClientOption{WithTestEndpoint(fake.Host()), WithoutConnectionReuse()}})

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

// linkPosts are the calls that made Payment Links (what a server without a public URL asks for).
func (p *pay) linkPosts() []testprovider.Request {
	return p.fake.RequestsTo(http.MethodPost, "/v1/payment_links")
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
	if n := len(p.linkPosts()); n != 0 || len(p.fake.Links()) != 0 {
		t.Fatalf("a server with a public URL asked for %d payment links", n)
	}
	if call.Form.Get("restrictions[completed_sessions][limit]") != "" || call.Form.Has("after_completion[type]") {
		t.Fatalf("a session got a link's parameters: %v", call.Form)
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
	req := paylink.Request{WorkspaceID: p.ws, IntegrationID: p.in.ID, SpaceID: p.space.ID, AppID: p.app.ID, RecordID: record, EffectID: entry.id,
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

// What a payment link is made as, by the address the server has: a Checkout Session when it has a
// public URL, a Payment Link when it hasn't.
var linkShapes = map[string]struct {
	public string
	path   string
}{
	"a checkout session": {publicURL, "/v1/checkout/sessions"},
	"a payment link":     {"", "/v1/payment_links"},
}

// mustPost makes an object at Stripe directly, the way another system of the account would.
func (p *pay) mustPost(path string, form url.Values) {
	p.t.Helper()
	req, err := http.NewRequest(http.MethodPost, p.fake.URL()+path, strings.NewReader(form.Encode()))
	if err != nil {
		p.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+p.fake.APIKey())
	req.Header.Set("Stripe-Version", p.fake.APIVersion())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		p.t.Fatalf("POST %s: %d", path, resp.StatusCode)
	}
}

// objects is how many objects of the shape Stripe holds.
func (p *pay) exec(q string, args ...any) {
	p.t.Helper()
	if _, err := p.db.Exec(q, args...); err != nil {
		p.t.Fatal(err)
	}
}

func (p *pay) objects(path string) int {
	if path == "/v1/payment_links" {
		return len(p.fake.Links())
	}
	return len(p.fake.Sessions())
}

// objectID is the id of the only object of the shape.
func (p *pay) objectID(path string) string {
	p.t.Helper()
	if path == "/v1/payment_links" {
		return p.fake.Links()[0].ID
	}
	return p.fake.Sessions()[0].ID
}

// Stripe keeps a failure it answered under the key, and would answer it again to the same key for
// a day: the next try has a key of its own. But a 5xx is an indeterminate result: Stripe made the
// object, and the failure is all the server was told. Another under the new key would be a second
// payable link for one invoice, so the object is looked for and adopted.
func TestAFailureStripeKeepsIsNotAskedAgainUnderItsKeyAndItsObjectIsAdopted(t *testing.T) {
	for name, shape := range linkShapes {
		t.Run(name, func(t *testing.T) {
			p := newPay(t, shape.public)
			record := p.invoice(10, "EUR")
			p.fake.Fail(testprovider.ServerError().AfterExecuting().On(http.MethodPost, shape.path))
			p.startWorker()
			p.send(record, "op-send-0004")
			p.waitFor("the first call", func() bool { return len(p.fake.RequestsTo(http.MethodPost, shape.path)) == 1 })
			p.waitFor("the entry waits", func() bool { es := p.entries(); return len(es) == 1 && es[0].state == "pending" })
			if r := p.refs(); len(r) != 1 || r[0].keySeq != 1 || r[0].object != "" {
				t.Fatalf("refs after the 5xx = %+v", r)
			}
			p.later(time.Minute)
			e := p.settled(apps.StateDone)

			// No second call that makes anything: the retry only read what Stripe holds.
			if reqs := p.fake.RequestsTo(http.MethodPost, shape.path); len(reqs) != 1 {
				t.Fatalf("%d calls that make an object: %+v", len(reqs), reqs)
			}
			if reads := p.fake.RequestsTo(http.MethodGet, shape.path); len(reads) != 1 || reads[0].IdempotencyKey != "" {
				t.Fatalf("reads = %+v", reads)
			}
			if n := p.objects(shape.path); n != 1 {
				t.Fatalf("%d objects at Stripe, want 1", n)
			}
			refs := p.refs()
			if len(refs) != 1 || refs[0].keySeq != 1 || refs[0].state != "open" || refs[0].effect.Int64 != e.id || refs[0].object != p.objectID(shape.path) {
				t.Fatalf("refs = %+v", refs)
			}
			// The adopted object is written back exactly as one a call answered: its address and id.
			data, _ := p.record(record)
			if !strings.Contains(data, `"payment_id":"`+refs[0].object+`"`) || !strings.Contains(data, refs[0].url) || refs[0].url == "" {
				t.Fatalf("record = %s, ref = %+v", data, refs[0])
			}
			var system string
			if err := p.db.QueryRow(`SELECT updated_system FROM app_rows WHERE id = ?`, record).Scan(&system); err != nil || system != fmt.Sprintf("integration:%d", p.in.ID) {
				t.Fatalf("updated_system = %q, %v", system, err)
			}
		})
	}
}

// A 5xx that made nothing: the retry looks, finds nothing, and makes exactly one object, under a
// key of its own.
func TestAFailureThatMadeNothingIsMadeOnceUnderANewKey(t *testing.T) {
	for name, shape := range linkShapes {
		t.Run(name, func(t *testing.T) {
			p := newPay(t, shape.public)
			record := p.invoice(10, "EUR")
			p.fake.Fail(testprovider.ServerError().On(http.MethodPost, shape.path))
			p.startWorker()
			p.send(record, "op-send-0005")
			p.waitFor("the first call", func() bool { return len(p.fake.RequestsTo(http.MethodPost, shape.path)) == 1 })
			p.waitFor("the entry waits", func() bool { es := p.entries(); return len(es) == 1 && es[0].state == "pending" })
			if n := p.objects(shape.path); n != 0 {
				t.Fatalf("%d objects after a failure that made none", n)
			}
			p.later(time.Minute)
			e := p.settled(apps.StateDone)

			reqs := p.fake.RequestsTo(http.MethodPost, shape.path)
			if len(reqs) != 2 || reqs[0].IdempotencyKey == reqs[1].IdempotencyKey || !strings.HasPrefix(reqs[1].IdempotencyKey, reqs[0].IdempotencyKey+"-") {
				t.Fatalf("keys: %+v", reqs)
			}
			if n := p.objects(shape.path); n != 1 {
				t.Fatalf("%d objects at Stripe, want 1", n)
			}
			if refs := p.refs(); len(refs) != 1 || refs[0].keySeq != 1 || refs[0].state != "open" || refs[0].effect.Int64 != e.id || refs[0].object != p.objectID(shape.path) {
				t.Fatalf("refs = %+v", refs)
			}
		})
	}
}

// While Stripe can't say whether an earlier call made the object, no call is made: the entry waits
// and, once the lookup works, adopts it.
func TestWhileTheLookupFailsNothingIsMade(t *testing.T) {
	for name, shape := range linkShapes {
		t.Run(name, func(t *testing.T) {
			p := newPay(t, shape.public)
			record := p.invoice(10, "EUR")
			p.fake.Fail(testprovider.ServerError().AfterExecuting().On(http.MethodPost, shape.path))
			p.fake.Fail(testprovider.Unavailable().Times(2).On(http.MethodGet, shape.path))
			p.startWorker()
			p.send(record, "op-send-0006")
			p.waitFor("the entry waits after the first call", func() bool {
				es := p.entries()
				return len(es) == 1 && es[0].state == "pending" && es[0].attempts == 1
			})
			for attempt := 2; attempt <= 3; attempt++ {
				p.later(time.Hour)
				p.waitFor("the lookup fails", func() bool {
					es := p.entries()
					return len(es) == 1 && es[0].state == "pending" && es[0].attempts == attempt
				})
				// The failed lookup made no call, and didn't spend the key: the object may be there.
				if n := len(p.fake.RequestsTo(http.MethodPost, shape.path)); n != 1 {
					t.Fatalf("attempt %d: %d calls that make an object", attempt, n)
				}
				if r := p.refs(); len(r) != 1 || r[0].keySeq != 1 || r[0].object != "" || r[0].state != "creating" {
					t.Fatalf("attempt %d: refs = %+v", attempt, r)
				}
				if es := p.entries(); es[0].code != string(apierr.CodeIntegrationProviderUnreachable) {
					t.Fatalf("attempt %d: code = %q", attempt, es[0].code)
				}
			}
			p.later(time.Hour)
			p.settled(apps.StateDone)
			if n := len(p.fake.RequestsTo(http.MethodPost, shape.path)); n != 1 {
				t.Fatalf("%d calls that make an object", n)
			}
			if n := p.objects(shape.path); n != 1 {
				t.Fatalf("%d objects at Stripe, want 1", n)
			}
			if refs := p.refs(); len(refs) != 1 || refs[0].state != "open" || refs[0].object != p.objectID(shape.path) {
				t.Fatalf("refs = %+v", refs)
			}
		})
	}
}

// An object Stripe lists that is another reference's is never adopted, and one that is no longer
// open or active is not what the payer was given.
func TestOnlyAnOpenObjectOfTheRefsTokenIsAdopted(t *testing.T) {
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	// Another reference's session, made earlier at the same account, with the same amount.
	other := url.Values{}
	other.Set("mode", "payment")
	other.Set("success_url", publicURL+PaymentDonePath)
	other.Set("client_reference_id", "someone-elses-reference")
	other.Set("line_items[0][quantity]", "1")
	other.Set("line_items[0][price_data][currency]", "eur")
	other.Set("line_items[0][price_data][unit_amount]", "1000")
	other.Set("line_items[0][price_data][product_data][name]", "Invoice A-1 for Acme")
	p.mustPost("/v1/checkout/sessions", other)
	p.fake.Fail(testprovider.ServerError().On(http.MethodPost, "/v1/checkout/sessions"))
	p.startWorker()
	p.send(record, "op-send-0007")
	p.waitFor("the entry waits", func() bool { es := p.entries(); return len(es) == 1 && es[0].state == "pending" })
	p.later(time.Minute)
	p.settled(apps.StateDone)
	if n := len(p.fake.Sessions()); n != 2 {
		t.Fatalf("%d sessions, want the other's and one new", n)
	}
	refs := p.refs()
	if len(refs) != 1 || refs[0].object == p.fake.Sessions()[0].ID {
		t.Fatalf("the ref adopted another reference's session: %+v", refs)
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

// ----- without a public URL: Stripe's own confirmation page -----

// A server with no public URL still makes the link: a Payment Link whose payer stays on the page
// Stripe hosts, which takes one payment, and which carries no address of ours.
func TestWithoutAPublicURLTheLinkIsMadeWithStripesHostedConfirmation(t *testing.T) {
	p := newPay(t, "")
	record := p.invoice(120.1, "EUR")
	p.startWorker()
	p.send(record, "op-hosted-0001")
	e := p.settled(apps.StateDone)

	if n := len(p.posts()); n != 0 || len(p.fake.Sessions()) != 0 {
		t.Fatalf("%d Checkout Sessions made without a public URL", n)
	}
	reqs := p.linkPosts()
	refs := p.refs()
	if len(reqs) != 1 || len(refs) != 1 || refs[0].state != "open" || refs[0].effect.Int64 != e.id || refs[0].amount != 12010 || refs[0].currency != "EUR" {
		t.Fatalf("calls %+v, refs %+v", reqs, refs)
	}
	ref, call := refs[0], reqs[0]
	if call.IdempotencyKey != ref.token || call.APIVersion != StripeAPIVersion || call.Auth != "bearer" {
		t.Fatalf("the call: %+v", call)
	}
	// Stripe's hosted confirmation, one payment, the token in the metadata, the exact amount.
	want := map[string]string{
		"after_completion[type]":                        "hosted_confirmation",
		"restrictions[completed_sessions][limit]":       "1",
		"metadata[taskto_ref]":                          ref.token,
		"line_items[0][quantity]":                       "1",
		"line_items[0][price_data][unit_amount]":        "12010",
		"line_items[0][price_data][currency]":           "eur",
		"line_items[0][price_data][product_data][name]": "Invoice A-1 for Acme",
	}
	for k, v := range want {
		if call.Form.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, call.Form.Get(k), v)
		}
	}
	// No address of ours is sent, and none of a Checkout Session's own parameters.
	for _, k := range []string{"success_url", "cancel_url", "mode", "client_reference_id", "after_completion[redirect][url]"} {
		if call.Form.Has(k) {
			t.Errorf("the link was asked with %s", k)
		}
	}
	for k, vs := range call.Form {
		for _, v := range vs {
			if strings.Contains(v, "http") || strings.Contains(v, PaymentDonePath) || strings.Contains(v, PaymentCancelledPath) {
				t.Errorf("%s = %q holds an address", k, v)
			}
		}
	}
	links := p.fake.Links()
	if len(links) != 1 || links[0].ID != ref.object || !strings.HasPrefix(ref.object, "plink_") || links[0].Restrictions == nil ||
		links[0].AfterCompletion["type"] != "hosted_confirmation" || links[0].Metadata["taskto_ref"] != ref.token {
		t.Fatalf("links = %+v, ref %+v", links, ref)
	}
	// The address is Stripe's, with the reference the payment echoes back, in the record.
	if ref.url != links[0].URL+"?client_reference_id="+ref.token || !strings.HasPrefix(ref.url, "https://buy.stripe.com/") {
		t.Fatalf("url = %q, link %q", ref.url, links[0].URL)
	}
	data, _ := p.record(record)
	if !strings.Contains(data, `"payment_id":"`+ref.object+`"`) || !strings.Contains(data, ref.url) {
		t.Fatalf("record = %s", data)
	}
	// The same send again makes no second link.
	p.send(record, "op-hosted-0001")
	time.Sleep(200 * time.Millisecond)
	if n := len(p.linkPosts()); n != 1 {
		t.Fatalf("a repeated request made %d calls", n)
	}
}

// Money is as exact and as limited without a public URL.
func TestWithoutAPublicURLAmountsAreStillExactOrRefused(t *testing.T) {
	p := newPay(t, "")
	record := p.invoice(10.005, "EUR")
	p.startWorker()
	p.send(record, "op-hosted-0002")
	e := p.settled(apps.StateFailed)
	if e.code != string(apierr.CodeIntegrationAmountRefused) || len(p.fake.Requests()) != 0 || p.count(`SELECT count(*) FROM integration_refs`) != 0 {
		t.Fatalf("entry = %+v, calls %d", e, len(p.fake.Requests()))
	}
}

// The answer is lost after Stripe made the link: the entry asks again under the same key and
// Stripe answers with the link it made, so the payee has one payable link.
func TestWithoutAPublicURLACrashBeforeTheAnswerIsNotedMakesNoSecondLink(t *testing.T) {
	p := newPay(t, "")
	record := p.invoice(50, "EUR")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/payment_links"))
	p.startWorker()
	p.send(record, "op-hosted-0003")
	p.waitFor("the first call", func() bool { return len(p.linkPosts()) == 1 })
	p.waitFor("the entry waits", func() bool { es := p.entries(); return len(es) == 1 && es[0].state == "pending" && es[0].attempts == 1 })
	if r := p.refs(); len(r) != 1 || r[0].object != "" || r[0].state != "creating" {
		t.Fatalf("refs after the dropped answer = %+v", r)
	}
	p.later(time.Minute)
	p.settled(apps.StateDone)

	reqs := p.linkPosts()
	if len(reqs) != 2 || reqs[0].IdempotencyKey != reqs[1].IdempotencyKey || !reqs[1].Replayed {
		t.Fatalf("calls = %+v: the retry should be a replay under the same key", reqs)
	}
	links := p.fake.Links()
	if len(links) != 1 || len(p.posts()) != 0 {
		t.Fatalf("%d links, %d sessions at Stripe, want 1 and 0", len(links), len(p.posts()))
	}
	if refs := p.refs(); len(refs) != 1 || refs[0].state != "open" || refs[0].object != links[0].ID {
		t.Fatalf("refs = %+v", refs)
	}
}

// The link was noted and the entry crashed before it was done: it asks Stripe nothing; one that
// crashed before it noted the answer asks again under the same key and gets the same link.
func TestWithoutAPublicURLAnEntryDoneAgainDoesNotMakeASecondLink(t *testing.T) {
	p := newPay(t, "")
	record := p.invoice(99.99, "EUR")
	p.send(record, "op-hosted-0004")
	entry := p.entries()[0]
	req := paylink.Request{WorkspaceID: p.ws, IntegrationID: p.in.ID, SpaceID: p.space.ID, AppID: p.app.ID, RecordID: record, EffectID: entry.id,
		Slot: "payments", Amount: "99.99", Currency: "EUR", Description: "Invoice"}
	first, err := p.svc.OpenPaymentLink(context.Background(), req)
	if err != nil || first.AmountMinor != 9999 || !strings.HasPrefix(first.ObjectID, "plink_") || first.URL == "" {
		t.Fatalf("first = %+v, %v", first, err)
	}
	again, err := p.svc.OpenPaymentLink(context.Background(), req)
	if err != nil || again != first || len(p.linkPosts()) != 1 {
		t.Fatalf("again = %+v, %v, %d calls", again, err, len(p.linkPosts()))
	}
	if _, err := p.db.Exec(`UPDATE integration_refs SET provider_object_id = NULL, url = NULL, state = 'creating'`); err != nil {
		t.Fatal(err)
	}
	third, err := p.svc.OpenPaymentLink(context.Background(), req)
	if err != nil || third != first {
		t.Fatalf("third = %+v, %v", third, err)
	}
	reqs := p.linkPosts()
	if len(reqs) != 2 || reqs[0].IdempotencyKey != reqs[1].IdempotencyKey || !reqs[1].Replayed || len(p.fake.Links()) != 1 {
		t.Fatalf("calls = %+v, links %d", reqs, len(p.fake.Links()))
	}
	if n := p.count(`SELECT count(*) FROM integration_refs`); n != 1 {
		t.Fatalf("%d refs for one entry", n)
	}
}

// An answer that isn't a payment link on Stripe's own address is not taken for one.
func TestWithoutAPublicURLALinkOnAnotherAddressIsRefused(t *testing.T) {
	for name, body := range map[string]string{
		"another host":  `{"id":"plink_abc","object":"payment_link","url":"https://evil.example.com/pay"}`,
		"a session":     `{"id":"cs_test_abc","object":"checkout.session","url":"https://buy.stripe.com/test_abc"}`,
		"a query given": `{"id":"plink_abc","object":"payment_link","url":"https://buy.stripe.com/test_abc?x=1"}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer srv.Close()
			c := NewClient(Lookup("stripe"), nil, egress.MustParse("127.0.0.1"), WithTestEndpoint(strings.TrimPrefix(srv.URL, "http://")))
			_, err := stripe{}.CreatePaymentLink(context.Background(), Credentials{APIKey: Secret("not-a-key")}, c,
				NewLink{Token: strings.Repeat("a", 32), AmountMinor: 100, Currency: "EUR"})
			if ce, ok := IsCall(err); !ok || ce.Kind != CallRefused {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// A lookup that rests on an answer that isn't a whole list says nothing about the object: it is an
// error, never "not found", so no second object is made on it.
func TestALookupOnAnIncompleteListFindsNothingOut(t *testing.T) {
	for name, body := range map[string]string{
		"more and none shown": `{"object":"list","has_more":true,"data":[]}`,
		"no list":             `{}`,
		"no data":             `{"object":"list","has_more":false}`,
		"no has_more":         `{"object":"list","data":[]}`,
		"null has_more":       `{"object":"list","has_more":null,"data":[]}`,
		"more and no cursor":  `{"object":"list","has_more":true,"data":[{"id":""}]}`,
		"not a list":          `{"object":"checkout.session","data":[]}`,
		"not json":            `<html>`,
	} {
		for shape, nl := range map[string]NewLink{
			"a session": {Token: strings.Repeat("a", 32), AmountMinor: 100, Currency: "EUR", SuccessURL: publicURL + PaymentDonePath},
			"a link":    {Token: strings.Repeat("a", 32), AmountMinor: 100, Currency: "EUR"},
		} {
			t.Run(name+"/"+shape, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
				defer srv.Close()
				c := NewClient(Lookup("stripe"), nil, egress.MustParse("127.0.0.1"), WithTestEndpoint(strings.TrimPrefix(srv.URL, "http://")))
				_, found, err := stripe{}.FindPaymentLink(context.Background(), Credentials{APIKey: Secret("not-a-key")}, c, nl, time.Now())
				if ce, ok := IsCall(err); !ok || ce.Kind != CallUnreachable || ce.KeyKept || found {
					t.Fatalf("found = %v, err = %v", found, err)
				}
			})
		}
	}
	// A whole list that holds nothing of the token's is "not found".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","has_more":false,"data":[]}`))
	}))
	defer srv.Close()
	c := NewClient(Lookup("stripe"), nil, egress.MustParse("127.0.0.1"), WithTestEndpoint(strings.TrimPrefix(srv.URL, "http://")))
	_, found, err := stripe{}.FindPaymentLink(context.Background(), Credentials{APIKey: Secret("not-a-key")}, c, NewLink{Token: strings.Repeat("a", 32), AmountMinor: 100, Currency: "EUR"}, time.Now())
	if err != nil || found {
		t.Fatalf("found = %v, err = %v", found, err)
	}
}

// An object of the token is adopted only if it asks what the payment asks: the amount and the
// currency. Another is never given to a payer, and nothing else is made while it stands.
func TestALookupAdoptsOnlyAnObjectOfTheAskedTerms(t *testing.T) {
	p := newPay(t, "")
	form := url.Values{}
	form.Set("after_completion[type]", "hosted_confirmation")
	form.Set("line_items[0][quantity]", "1")
	form.Set("line_items[0][price_data][currency]", "eur")
	form.Set("line_items[0][price_data][unit_amount]", "1000")
	form.Set("line_items[0][price_data][product_data][name]", "Invoice")
	token := strings.Repeat("b", 32)
	form.Set("metadata[taskto_ref]", token)
	p.mustPost("/v1/payment_links", form)
	sess := url.Values{}
	sess.Set("mode", "payment")
	sess.Set("success_url", publicURL+PaymentDonePath)
	sess.Set("client_reference_id", token)
	sess.Set("line_items[0][quantity]", "1")
	sess.Set("line_items[0][price_data][currency]", "eur")
	sess.Set("line_items[0][price_data][unit_amount]", "1000")
	sess.Set("line_items[0][price_data][product_data][name]", "Invoice")
	p.mustPost("/v1/checkout/sessions", sess)

	c := NewClient(Lookup("stripe"), nil, egress.MustParse("127.0.0.1"), WithTestEndpoint(p.fake.Host()))
	creds := Credentials{APIKey: Secret(p.fake.APIKey())}
	for shape, nl := range map[string]NewLink{
		"a session": {Token: token, SuccessURL: publicURL + PaymentDonePath},
		"a link":    {Token: token},
	} {
		t.Run(shape, func(t *testing.T) {
			nl.AmountMinor, nl.Currency = 1000, "EUR"
			link, found, err := stripe{}.FindPaymentLink(context.Background(), creds, c, nl, time.Now())
			if err != nil || !found || link.ObjectID == "" || link.URL == "" {
				t.Fatalf("the object of the terms: %+v, %v, %v", link, found, err)
			}
			for what, change := range map[string]func(*NewLink){
				"another amount":   func(n *NewLink) { n.AmountMinor = 100 },
				"another currency": func(n *NewLink) { n.Currency = "USD" },
			} {
				other := nl
				change(&other)
				link, found, err := stripe{}.FindPaymentLink(context.Background(), creds, c, other, time.Now())
				if ce, ok := IsCall(err); !ok || ce.Kind != CallRefused || found || link.URL != "" {
					t.Fatalf("%s: %+v, %v, %v", what, link, found, err)
				}
			}
			// Another reference's token is no match at all.
			nl.Token = strings.Repeat("c", 32)
			if _, found, err := (stripe{}).FindPaymentLink(context.Background(), creds, c, nl, time.Now()); err != nil || found {
				t.Fatalf("another token: %v, %v", found, err)
			}
		})
	}
}

// A late failure of an older call moves the key on only from the key it used: the key a later call
// is using is not skipped.
func TestAFailureMovesTheKeyOnOnlyFromTheKeyItUsed(t *testing.T) {
	p := newPay(t, publicURL)
	invoice := p.invoice(10, "EUR")
	p.fake.Fail(testprovider.ServerError().On(http.MethodPost, "/v1/checkout/sessions"))
	p.startWorker()
	p.send(invoice, "op-send-0008")
	p.waitFor("the entry waits after the first call", func() bool {
		es := p.entries()
		return len(es) == 1 && es[0].state == "pending" && es[0].attempts == 1
	})
	refs := p.refs()
	if len(refs) != 1 || refs[0].keySeq != 1 {
		t.Fatalf("refs = %+v", refs)
	}
	rec := record{Provider: "stripe"}
	stale := ref{id: refs[0].id, keySeq: 0}
	for _, kind := range []CallKind{CallUnreachable, CallRefused} {
		if err := p.svc.callFailed(context.Background(), &stale, rec, &CallError{Kind: kind, Status: 500, KeyKept: true}); err == nil {
			t.Fatal("no error")
		}
		if r := p.refs(); r[0].keySeq != 1 {
			t.Fatalf("a failure of key 0 (kind %d) moved the key to %d", kind, r[0].keySeq)
		}
	}
	current := ref{id: refs[0].id, keySeq: 1}
	_ = p.svc.callFailed(context.Background(), &current, rec, &CallError{Kind: CallUnreachable, Status: 500, KeyKept: true})
	if r := p.refs(); r[0].keySeq != 2 {
		t.Fatalf("a failure of the key in use left it at %d", r[0].keySeq)
	}
}

// A key the provider may have forgotten (it keeps one for a day, at least) is not trusted to find
// the object: it is looked for first, so the call under the same key can't make a second one.
func TestAnOldRefWithNoObjectLooksBeforeItCallsAgain(t *testing.T) {
	for name, shape := range linkShapes {
		t.Run(name, func(t *testing.T) {
			p := newPay(t, shape.public)
			record := p.invoice(10, "EUR")
			p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, shape.path))
			p.startWorker()
			p.send(record, "op-send-0009")
			p.waitFor("the entry waits after the first call", func() bool {
				es := p.entries()
				return len(es) == 1 && es[0].state == "pending" && es[0].attempts == 1
			})
			if r := p.refs(); len(r) != 1 || r[0].keySeq != 0 || r[0].object != "" {
				t.Fatalf("refs = %+v", r)
			}
			p.exec(`UPDATE integration_refs SET created_at = ?`, p.svc.store.stamp(time.Now().Add(-25*time.Hour)))
			p.later(time.Minute)
			p.settled(apps.StateDone)
			if n := len(p.fake.RequestsTo(http.MethodPost, shape.path)); n != 1 {
				t.Fatalf("%d calls that make an object, want the first only", n)
			}
			if n := p.objects(shape.path); n != 1 {
				t.Fatalf("%d objects at Stripe, want 1", n)
			}
			if refs := p.refs(); len(refs) != 1 || refs[0].object != p.objectID(shape.path) || refs[0].state != "open" {
				t.Fatalf("refs = %+v", refs)
			}
		})
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
	_, err := p.svc.OpenPaymentLink(context.Background(), paylink.Request{WorkspaceID: p.ws, IntegrationID: theirs.ID, SpaceID: p.space.ID,
		AppID: p.app.ID, RecordID: record, EffectID: e.id, Slot: "payments", Amount: "10", Currency: "EUR"})
	if !isUnavailable(err) {
		t.Fatalf("err = %v", err)
	}
	_, err = p.svc.OpenPaymentLink(context.Background(), paylink.Request{WorkspaceID: other, IntegrationID: p.in.ID, SpaceID: p.space.ID,
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
	req := paylink.Request{WorkspaceID: p.ws, IntegrationID: p.in.ID, SpaceID: p.space.ID, AppID: p.app.ID, RecordID: record,
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
	req := paylink.Request{WorkspaceID: p.ws, IntegrationID: p.in.ID, SpaceID: p.space.ID, AppID: p.app.ID, RecordID: record,
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
	var pe *paylink.Error
	return errors.As(err, &pe) && pe.Code == apierr.CodeIntegrationUnavailable
}

// ----- a ref keeps what its first try was -----

// direct is the request of the only entry, for the tests that call the integration's side without
// the worker (so nothing else reads what they change).
func (p *pay) direct(record int64, op string) paylink.Request {
	p.t.Helper()
	p.send(record, op)
	entry := p.entries()[0]
	return paylink.Request{WorkspaceID: p.ws, IntegrationID: p.in.ID, SpaceID: p.space.ID, AppID: p.app.ID, RecordID: record, EffectID: entry.id,
		Slot: "payments", Amount: "10", Currency: "EUR", Description: "Invoice"}
}

// A lost answer, then a restart with another public address (or none, or one where there was none):
// the retry asks what the first try asked, so Stripe's key finds the object, and a lookup after the
// key is forgotten reads the same kind of objects.
func TestARetryAsksWhatTheFirstTryAskedWhateverTheServersAddressIsNow(t *testing.T) {
	const otherURL = "https://moved.example.com"
	for name, tc := range map[string]struct {
		first, later string
		path         string
		other        string // the path of the kind the later address would have made
		success      string
	}{
		"a session, then no address":      {publicURL, "", "/v1/checkout/sessions", "/v1/payment_links", publicURL + PaymentDonePath},
		"a session, then another":         {publicURL, otherURL, "/v1/checkout/sessions", "/v1/payment_links", publicURL + PaymentDonePath},
		"a payment link, then an address": {"", publicURL, "/v1/payment_links", "/v1/checkout/sessions", ""},
	} {
		for _, aged := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, key forgotten %v", name, aged), func(t *testing.T) {
				p := newPay(t, tc.first)
				record := p.invoice(10, "EUR")
				req := p.direct(record, "op-shape-0001")
				p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, tc.path))
				if _, err := p.svc.OpenPaymentLink(context.Background(), req); err == nil {
					t.Fatal("the lost answer was no error")
				}
				if r := p.refs(); len(r) != 1 || r[0].object != "" {
					t.Fatalf("refs = %+v", r)
				}
				var success, cancel string
				if err := p.db.QueryRow(`SELECT success_url, cancel_url FROM integration_refs`).Scan(&success, &cancel); err != nil || success != tc.success {
					t.Fatalf("the ref keeps its pages %q %q, %v; want %q", success, cancel, err, tc.success)
				}
				p.svc.publicURL = tc.later // the server restarted with another address
				if aged {
					p.exec(`UPDATE integration_refs SET created_at = ?`, p.svc.store.stamp(time.Now().Add(-25*time.Hour)))
				}
				link, err := p.svc.OpenPaymentLink(context.Background(), req)
				if err != nil || link.ObjectID == "" {
					t.Fatalf("link = %+v, %v", link, err)
				}
				if n := p.objects(tc.path); n != 1 {
					t.Fatalf("%d objects of the first kind, want the one the first try made", n)
				}
				if n := p.objects(tc.other); n != 0 || len(p.fake.RequestsTo(http.MethodPost, tc.other)) != 0 {
					t.Fatalf("%d objects of the other kind, %d calls: the retry took the address of now", n, len(p.fake.RequestsTo(http.MethodPost, tc.other)))
				}
				posts := p.fake.RequestsTo(http.MethodPost, tc.path)
				if aged {
					// Found by the look, not by a call under the old key.
					if len(posts) != 1 || len(p.fake.RequestsTo(http.MethodGet, tc.path)) != 1 {
						t.Fatalf("posts %d, lists %d", len(posts), len(p.fake.RequestsTo(http.MethodGet, tc.path)))
					}
				} else if len(posts) != 2 || posts[0].IdempotencyKey != posts[1].IdempotencyKey || !posts[1].Replayed ||
					posts[0].Form.Encode() != posts[1].Form.Encode() {
					t.Fatalf("the retry's parameters are not the first try's: %+v", posts)
				}
			})
		}
	}
}

// ----- the account a ref was made under -----

const otherAccount = "acct_" + "Other0123456789"

// A key replaced by another account's: an unfinished ref belongs to the account it was made in. The
// retry looks nowhere and makes nothing; the entry fails for an admin to settle; the ref stays.
func TestAKeyOfAnotherAccountNeverMovesAnUnfinishedRefThere(t *testing.T) {
	for name, shape := range linkShapes {
		t.Run(name, func(t *testing.T) {
			p := newPay(t, shape.public)
			record := p.invoice(10, "EUR")
			req := p.direct(record, "op-account-0001")
			p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, shape.path))
			if _, err := p.svc.OpenPaymentLink(context.Background(), req); err == nil {
				t.Fatal("the lost answer was no error")
			}
			before := p.refs()
			var account string
			if err := p.db.QueryRow(`SELECT account_id FROM integration_refs`).Scan(&account); err != nil || account != testprovider.TestAccountID {
				t.Fatalf("the ref's account = %q, %v", account, err)
			}
			if p.objects(shape.path) != 1 {
				t.Fatal("the first try made no object")
			}
			requests := len(p.fake.Requests())

			p.fake.SetAccount(otherAccount) // the integration's key is another account's now
			p.exec(`UPDATE integration_refs SET created_at = ?`, p.svc.store.stamp(time.Now().Add(-25*time.Hour)))
			_, err := p.svc.OpenPaymentLink(context.Background(), req)
			var pe *paylink.Error
			if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationAccountChanged || pe.Transient {
				t.Fatalf("err = %v", err)
			}
			// Only the account was asked: no list, no call that makes an object.
			for _, r := range p.fake.Requests()[requests:] {
				if r.Path != "/v1/account" {
					t.Fatalf("after the key changed, %s %s was called", r.Method, r.Path)
				}
			}
			if after := p.refs(); len(after) != 1 || after[0] != before[0] {
				t.Fatalf("the ref changed: %+v, was %+v", after, before)
			}
			if n := p.objects(shape.path); n != 1 {
				t.Fatalf("%d objects, want the first account's one", n)
			}
			// The same account again: the ref carries on, and finds its object.
			p.fake.SetAccount(testprovider.TestAccountID)
			link, err := p.svc.OpenPaymentLink(context.Background(), req)
			if err != nil || link.ObjectID != p.objectID(shape.path) {
				t.Fatalf("link = %+v, %v", link, err)
			}
		})
	}
}

// The entry fails with a code a person can act on, and never again asks anyone.
func TestAnEntryOfAnotherAccountsKeyFailsForAnAdmin(t *testing.T) {
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	p.startWorker()
	p.send(record, "op-account-0002")
	p.waitFor("the entry waits", func() bool { es := p.entries(); return len(es) == 1 && es[0].state == "pending" && es[0].attempts == 1 })
	p.fake.SetAccount(otherAccount)
	p.later(time.Minute)
	e := p.settled(apps.StateFailed)
	if e.code != string(apierr.CodeIntegrationAccountChanged) {
		t.Fatalf("entry = %+v", e)
	}
	if n := len(p.posts()); n != 1 || p.count(`SELECT count(*) FROM integration_refs WHERE provider_object_id IS NOT NULL`) != 0 {
		t.Fatalf("%d calls that make an object", n)
	}
}

// A link the ref made is the integration's that made it: another bound since is refused, and no
// second link is made for the entry.
func TestALinkMadeUnderOneIntegrationIsNeverGivenUnderAnother(t *testing.T) {
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-other-0001")
	first, err := p.svc.OpenPaymentLink(context.Background(), req)
	if err != nil || first.ObjectID == "" {
		t.Fatalf("first = %+v, %v", first, err)
	}
	body := stripeBody("second")
	body["secrets"] = map[string]string{"api_key": p.fake.APIKey()}
	body["currencies"] = []string{"EUR"}
	second := p.create(p.owner, p.ws, body)
	p.exec(`INSERT INTO integration_spaces (integration_id, space_id) VALUES (?, ?)`, second.ID, p.space.ID)
	req.IntegrationID = second.ID
	_, err = p.svc.OpenPaymentLink(context.Background(), req)
	var pe *paylink.Error
	if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationChanged || pe.Transient {
		t.Fatalf("err = %v", err)
	}
	if n := len(p.posts()); n != 1 || p.count(`SELECT count(*) FROM integration_refs`) != 1 {
		t.Fatalf("%d calls, a second link may have been made", n)
	}
	if r := p.refs(); r[0].integration != p.in.ID {
		t.Fatalf("the ref was bound to %d", r[0].integration)
	}
}

// ----- the lookup's reach -----

// More active links than a lookup reads, none of them the token's: the lookup can't say, asking
// again reads the same, so the entry fails for a person instead of waiting for ever.
func TestALookupThatCannotSeeEverythingFailsTheEntryForAPerson(t *testing.T) {
	p := newPay(t, "")
	record := p.invoice(10, "EUR")
	form := url.Values{}
	form.Set("after_completion[type]", "hosted_confirmation")
	form.Set("line_items[0][quantity]", "1")
	form.Set("line_items[0][price_data][currency]", "eur")
	form.Set("line_items[0][price_data][unit_amount]", "1000")
	form.Set("line_items[0][price_data][product_data][name]", "Other")
	for range findPageSize*findMaxPages + 1 {
		p.mustPost("/v1/payment_links", form)
	}
	others := len(p.linkPosts())
	// A call that made nothing answers a 5xx: the retry has to look first.
	p.fake.Fail(testprovider.ServerError().On(http.MethodPost, "/v1/payment_links"))
	p.startWorker()
	p.send(record, "op-cap-0001")
	p.waitFor("the entry waits", func() bool { es := p.entries(); return len(es) == 1 && es[0].state == "pending" })
	p.later(time.Minute)
	e := p.settled(apps.StateFailed)
	if e.code != string(apierr.CodeIntegrationLookupIncomplete) {
		t.Fatalf("entry = %+v", e)
	}
	lists := 0
	for _, r := range p.fake.RequestsTo(http.MethodGet, "/v1/payment_links") {
		if r.Query.Get("limit") != "" {
			lists++
		}
	}
	if lists != findMaxPages {
		t.Fatalf("%d pages read, want %d", lists, findMaxPages)
	}
	if n := len(p.linkPosts()) - others; n != 1 {
		t.Fatalf("%d calls that make a link: nothing may be made on a lookup that couldn't see everything", n)
	}
	// It is not tried again by itself.
	p.later(time.Hour)
	time.Sleep(200 * time.Millisecond)
	if got := p.entries()[0]; got.state != apps.StateFailed || got.attempts != e.attempts {
		t.Fatalf("entry = %+v, was %+v", got, e)
	}
}

// ----- a link noted late, and objects that are no longer open -----

// Another worker's call finishes under the integration the slot was bound to before, while this try
// is between reading the ref and making it ready: the link it returns is that integration's, and is
// refused here as it is on the early path.
func TestALinkNotedWhileATryBeginsIsRefusedUnderAnotherIntegration(t *testing.T) {
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-late-0001")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	if _, err := p.svc.OpenPaymentLink(context.Background(), req); err == nil {
		t.Fatal("the lost answer was no error")
	}
	body := stripeBody("second")
	body["secrets"] = map[string]string{"api_key": p.fake.APIKey()}
	body["currencies"] = []string{"EUR"}
	second := p.create(p.owner, p.ws, body)
	p.exec(`INSERT INTO integration_spaces (integration_id, space_id) VALUES (?, ?)`, second.ID, p.space.ID)
	req.IntegrationID = second.ID
	p.svc.beforeBeginRef = func() { // the other worker's call finishes, under the first integration
		p.exec(`UPDATE integration_refs SET provider_object_id = ?, url = ?, state = 'open'`, p.fake.Sessions()[0].ID, *p.fake.Sessions()[0].URL)
	}
	_, err := p.svc.OpenPaymentLink(context.Background(), req)
	var pe *paylink.Error
	if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationChanged {
		t.Fatalf("err = %v", err)
	}
}

// A try that failed while another's answer was on its way doesn't lose the answer: the link that is
// returned is on the ref, so a later try finds it and makes no second one.
func TestAnAnswerIsRecordedEvenIfAnotherTryMarkedTheRefFailed(t *testing.T) {
	p := newPay(t, publicURL)
	invoiceID := p.invoice(10, "EUR")
	req := p.direct(invoiceID, "op-late-0002")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	if _, err := p.svc.OpenPaymentLink(context.Background(), req); err == nil {
		t.Fatal("the lost answer was no error")
	}
	r := p.refs()[0]
	stale := ref{id: r.id, keySeq: 0, integrationID: p.in.ID}
	// The other try's refusal marks the ref failed...
	_ = p.svc.callFailed(context.Background(), &stale, record{Integration: Integration{Provider: "stripe"}}, &CallError{Kind: CallRefused, Status: 400})
	if got := p.refs()[0]; got.state != "failed" {
		t.Fatalf("state = %s", got.state)
	}
	// ...and this try's answer, which made the object, is noted all the same.
	if _, err := p.svc.noteLink(context.Background(), stale, req, Link{ObjectID: p.fake.Sessions()[0].ID, URL: *p.fake.Sessions()[0].URL}); err != nil {
		t.Fatal(err)
	}
	if got := p.refs()[0]; got.state != "open" || got.object != p.fake.Sessions()[0].ID {
		t.Fatalf("the answer was not recorded: %+v", got)
	}
	link, err := p.svc.OpenPaymentLink(context.Background(), req)
	if err != nil || link.ObjectID != p.fake.Sessions()[0].ID || len(p.posts()) != 1 {
		t.Fatalf("link = %+v, %v, %d calls", link, err, len(p.posts()))
	}
}

// An object of the token that is no longer open may have been paid, and the lookup sees it: a
// completed session or an inactive link stops the call with a refusal (nothing else is made), an
// expired session can't be paid and is passed over, and an open one is adopted.
func TestALookupSeesObjectsThatAreNoLongerOpen(t *testing.T) {
	token := strings.Repeat("d", 32)
	session := func(p *pay) string {
		f := url.Values{}
		f.Set("mode", "payment")
		f.Set("success_url", publicURL+PaymentDonePath)
		f.Set("client_reference_id", token)
		f.Set("line_items[0][quantity]", "1")
		f.Set("line_items[0][price_data][currency]", "eur")
		f.Set("line_items[0][price_data][unit_amount]", "1000")
		f.Set("line_items[0][price_data][product_data][name]", "Invoice")
		p.mustPost("/v1/checkout/sessions", f)
		return p.fake.Sessions()[0].ID
	}
	link := func(p *pay) string {
		f := url.Values{}
		f.Set("after_completion[type]", "hosted_confirmation")
		f.Set("metadata[taskto_ref]", token)
		f.Set("restrictions[completed_sessions][limit]", "1")
		f.Set("line_items[0][quantity]", "1")
		f.Set("line_items[0][price_data][currency]", "eur")
		f.Set("line_items[0][price_data][unit_amount]", "1000")
		f.Set("line_items[0][price_data][product_data][name]", "Invoice")
		p.mustPost("/v1/payment_links", f)
		return p.fake.Links()[0].ID
	}
	cases := map[string]struct {
		nl     NewLink
		make   func(*pay) string
		change func(*pay, string)
		found  bool
		kind   CallKind // of the error, 0 for none
	}{
		"an open session":    {NewLink{SuccessURL: publicURL + PaymentDonePath}, session, func(*pay, string) {}, true, 0},
		"a paid session":     {NewLink{SuccessURL: publicURL + PaymentDonePath}, session, func(p *pay, id string) { _, _ = p.fake.Pay(id) }, false, CallSettled},
		"an expired session": {NewLink{SuccessURL: publicURL + PaymentDonePath}, session, func(p *pay, id string) { _, _ = p.fake.Expire(id) }, false, 0},
		"an active link":     {NewLink{}, link, func(*pay, string) {}, true, 0},
		"a paid link":        {NewLink{}, link, func(p *pay, id string) { _, _ = p.fake.PayLink(id) }, false, CallSettled},
		"a deactivated link": {NewLink{}, link, func(p *pay, id string) { p.mustPost("/v1/payment_links/"+id, url.Values{"active": {"false"}}) }, false, CallSettled},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := newPay(t, "")
			tc.change(p, tc.make(p))
			c := NewClient(Lookup("stripe"), nil, egress.MustParse("127.0.0.1"), WithTestEndpoint(p.fake.Host()))
			nl := tc.nl
			nl.Token, nl.AmountMinor, nl.Currency = token, 1000, "EUR"
			got, found, err := stripe{}.FindPaymentLink(context.Background(), Credentials{APIKey: Secret(p.fake.APIKey())}, c, nl, time.Now())
			if found != tc.found {
				t.Fatalf("found = %v, err = %v", found, err)
			}
			if tc.kind == 0 {
				if err != nil || (tc.found && got.URL == "") {
					t.Fatalf("link = %+v, err = %v", got, err)
				}
				return
			}
			if ce, ok := IsCall(err); !ok || ce.Kind != tc.kind || ce.KeyKept || got.URL != "" {
				t.Fatalf("err = %v, link %+v", err, got)
			}
		})
	}
}

// ----- a ref belongs to its key's mode and its integration -----

// setKey replaces the integration's key, and the one the fake accepts, as an admin does.
func (p *pay) setKey(key string) {
	p.t.Helper()
	p.fake.SetAPIKey(key)
	expect(p.t, p.owner.do(http.MethodPatch, wsPath(p.ws, fmt.Sprintf("integrations/%d", p.in.ID)),
		map[string]any{"secrets": map[string]string{"api_key": key}}), http.StatusOK)
}

// One Stripe account answers with the same id to its test key and its live key, but a test key
// doesn't see what a live one made: an unfinished ref made under one mode is never looked for or
// made again under the other, in either direction.
func TestAKeyOfTheOtherModeOfTheSameAccountNeverMovesAnUnfinishedRefThere(t *testing.T) {
	for name, swap := range map[string]struct{ from, to, mode string }{
		"test to live": {stripeTestKey, stripeLiveKey, "test"},
		"live to test": {stripeLiveKey, stripeTestKey, "live"},
	} {
		for shapeName, shape := range linkShapes {
			t.Run(name+" "+shapeName, func(t *testing.T) {
				p := newPay(t, shape.public)
				p.setKey(swap.from)
				record := p.invoice(10, "EUR")
				req := p.direct(record, "op-mode-0001")
				p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, shape.path))
				if _, err := p.svc.OpenPaymentLink(context.Background(), req); err == nil {
					t.Fatal("the lost answer was no error")
				}
				var account, mode string
				if err := p.db.QueryRow(`SELECT account_id, account_mode FROM integration_refs`).Scan(&account, &mode); err != nil || account != testprovider.TestAccountID || mode != swap.mode {
					t.Fatalf("the ref's account = %q, mode = %q, %v", account, mode, err)
				}
				before := p.refs()
				requests := len(p.fake.Requests())

				p.setKey(swap.to) // the same account, the other mode
				p.exec(`UPDATE integration_refs SET created_at = ?`, p.svc.store.stamp(time.Now().Add(-25*time.Hour)))
				_, err := p.svc.OpenPaymentLink(context.Background(), req)
				var pe *paylink.Error
				if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationAccountChanged || pe.Transient {
					t.Fatalf("err = %v", err)
				}
				for _, r := range p.fake.Requests()[requests:] {
					if r.Path != "/v1/account" {
						t.Fatalf("after the key's mode changed, %s %s was called", r.Method, r.Path)
					}
				}
				if after := p.refs(); len(after) != 1 || after[0] != before[0] {
					t.Fatalf("the ref changed: %+v, was %+v", after, before)
				}
				if n := p.objects(shape.path); n != 1 {
					t.Fatalf("%d objects, want the first mode's one", n)
				}
				// The mode it was made in again: the ref carries on, and finds its object.
				p.setKey(swap.from)
				link, err := p.svc.OpenPaymentLink(context.Background(), req)
				if err != nil || link.ObjectID != p.objectID(shape.path) {
					t.Fatalf("link = %+v, %v", link, err)
				}
			})
		}
	}
}

// A slot bound to another integration since the ref's first try, even one of the same account, is
// refused while the ref has no link, as it is when it has one: the ref is not moved, and nothing is
// asked of the provider.
func TestARefWithNoLinkIsNeverMovedToAnotherIntegration(t *testing.T) {
	for name, shape := range linkShapes {
		t.Run(name, func(t *testing.T) {
			p := newPay(t, shape.public)
			record := p.invoice(10, "EUR")
			req := p.direct(record, "op-rebind-0001")
			p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, shape.path))
			if _, err := p.svc.OpenPaymentLink(context.Background(), req); err == nil {
				t.Fatal("the lost answer was no error")
			}
			before, requests := p.refs(), len(p.fake.Requests())
			body := stripeBody("second")
			body["secrets"] = map[string]string{"api_key": p.fake.APIKey()} // the same account and mode
			body["currencies"] = []string{"EUR"}
			second := p.create(p.owner, p.ws, body)
			p.exec(`INSERT INTO integration_spaces (integration_id, space_id) VALUES (?, ?)`, second.ID, p.space.ID)
			req.IntegrationID = second.ID
			for range 2 { // asked again, and again: the same answer, and never a call that makes an object
				_, err := p.svc.OpenPaymentLink(context.Background(), req)
				var pe *paylink.Error
				if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationChanged || pe.Transient {
					t.Fatalf("err = %v", err)
				}
			}
			if after := p.refs(); len(after) != 1 || after[0] != before[0] || after[0].integration != p.in.ID {
				t.Fatalf("the ref changed: %+v, was %+v", after, before)
			}
			for _, r := range p.fake.Requests()[requests:] {
				if r.Method != http.MethodGet {
					t.Fatalf("after the slot was rebound, %s %s was called", r.Method, r.Path)
				}
			}
			// Bound to the first one again, the ref carries on.
			req.IntegrationID = p.in.ID
			link, err := p.svc.OpenPaymentLink(context.Background(), req)
			if err != nil || link.ObjectID != p.objectID(shape.path) {
				t.Fatalf("link = %+v, %v", link, err)
			}
		})
	}
}

// ----- a key that may hold a dead answer is not used again -----

// A Checkout Session of the ref that expired early (a person expired it) is passed over by the lookup,
// but the key still holds Stripe's answer for the call that made it: under that key the next call
// would be answered with the dead session. The lookup having found nothing a payer can use, the call
// is made under a key of its own, once.
func TestAnExpiredSessionIsNeverReplayedAsTheNewLink(t *testing.T) {
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-expired-0001")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	if _, err := p.svc.OpenPaymentLink(context.Background(), req); err == nil {
		t.Fatal("the lost answer was no error")
	}
	dead := p.fake.Sessions()[0].ID
	if _, err := p.fake.Expire(dead); err != nil {
		t.Fatal(err)
	}
	// The ref is past the time the key is trusted and Stripe still keeps it (a day).
	p.exec(`UPDATE integration_refs SET created_at = ?`, p.svc.store.stamp(time.Now().Add(-23*time.Hour-30*time.Minute)))
	link, err := p.svc.OpenPaymentLink(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if link.ObjectID == "" || link.ObjectID == dead {
		t.Fatalf("link = %+v: the dead session %s was given", link, dead)
	}
	if ss, ok := p.fake.Session(link.ObjectID); !ok || ss.Status != "open" {
		t.Fatalf("the link's session = %+v, %v", ss, ok)
	}
	reqs := p.posts()
	if len(reqs) != 2 || reqs[0].IdempotencyKey == reqs[1].IdempotencyKey || reqs[1].IdempotencyKey != reqs[0].IdempotencyKey+"-1" {
		t.Fatalf("keys: %+v", reqs)
	}
	if refs := p.refs(); len(refs) != 1 || refs[0].keySeq != 1 || refs[0].object != link.ObjectID {
		t.Fatalf("refs = %+v", refs)
	}
	// Asked again, the ref has its link: nothing more is made.
	if again, err := p.svc.OpenPaymentLink(context.Background(), req); err != nil || again.ObjectID != link.ObjectID || len(p.posts()) != 2 {
		t.Fatalf("again = %+v, %v, %d calls", again, err, len(p.posts()))
	}
}

// Stripe may keep any answer of an executed call under its key, a refusal of 4xx included, so a ref
// that failed and is tried again looks for what the call made and then takes a key of its own.
func TestAFailedRefTriedAgainLooksAndTakesAKeyOfItsOwn(t *testing.T) {
	for name, shape := range linkShapes {
		t.Run(name+" that made nothing", func(t *testing.T) {
			p := newPay(t, shape.public)
			record := p.invoice(10, "EUR")
			req := p.direct(record, "op-failed-0001")
			p.fake.Fail(testprovider.Refusal("amount_too_small").On(http.MethodPost, shape.path))
			if _, err := p.svc.OpenPaymentLink(context.Background(), req); err == nil {
				t.Fatal("the refusal was no error")
			}
			if r := p.refs()[0]; r.state != "failed" || r.keySeq != 0 {
				t.Fatalf("ref = %+v", r)
			}
			requests := len(p.fake.Requests())
			link, err := p.svc.OpenPaymentLink(context.Background(), req)
			if err != nil || link.ObjectID != p.objectID(shape.path) {
				t.Fatalf("link = %+v, %v", link, err)
			}
			var looked bool
			for _, r := range p.fake.Requests()[requests:] {
				if r.Method == http.MethodGet && strings.HasPrefix(r.Path, shape.path) {
					looked = true
				}
			}
			posts := p.fake.RequestsTo(http.MethodPost, shape.path)
			if !looked || len(posts) != 2 || posts[1].IdempotencyKey != posts[0].IdempotencyKey+"-1" {
				t.Fatalf("looked = %v, keys = %+v", looked, posts)
			}
		})
		t.Run(name+" that made an object", func(t *testing.T) {
			p := newPay(t, shape.public)
			record := p.invoice(10, "EUR")
			req := p.direct(record, "op-failed-0002")
			p.fake.Fail(testprovider.Refusal("amount_too_small").AfterExecuting().On(http.MethodPost, shape.path))
			if _, err := p.svc.OpenPaymentLink(context.Background(), req); err == nil {
				t.Fatal("the refusal was no error")
			}
			if p.objects(shape.path) != 1 {
				t.Skip("the fake keeps no object of a refusal")
			}
			link, err := p.svc.OpenPaymentLink(context.Background(), req)
			if err != nil || link.ObjectID != p.objectID(shape.path) || p.objects(shape.path) != 1 || len(p.fake.RequestsTo(http.MethodPost, shape.path)) != 1 {
				t.Fatalf("link = %+v, %v: the object was not adopted", link, err)
			}
		})
	}
}

// An object of the ref that was paid, or that can no longer be used for another reason, is not given
// and no other is made: the entry fails with a code that asks an admin to reconcile it.
func TestAPaidObjectOfTheRefFailsTheEntryWithItsOwnCode(t *testing.T) {
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-paid-0001")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	if _, err := p.svc.OpenPaymentLink(context.Background(), req); err == nil {
		t.Fatal("the lost answer was no error")
	}
	if _, err := p.fake.Pay(p.fake.Sessions()[0].ID); err != nil {
		t.Fatal(err)
	}
	p.exec(`UPDATE integration_refs SET created_at = ?`, p.svc.store.stamp(time.Now().Add(-25*time.Hour)))
	_, err := p.svc.OpenPaymentLink(context.Background(), req)
	var pe *paylink.Error
	if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationLinkAlreadyPaid || pe.Transient {
		t.Fatalf("err = %v", err)
	}
	if n := len(p.posts()); n != 1 {
		t.Fatalf("%d calls that make an object", n)
	}
}

// A try whose answer was not recorded because another try changed the ref meanwhile gives what the
// ref holds, checked as every finished ref is: under the integration that made it.
func TestAnAnswerNotRecordedGivesWhatTheRefHolds(t *testing.T) {
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-noted-0001")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	if _, err := p.svc.OpenPaymentLink(context.Background(), req); err == nil {
		t.Fatal("the lost answer was no error")
	}
	r := p.refs()[0]
	stale := ref{id: r.id, integrationID: p.in.ID, amountMinor: 1000, currency: "EUR"}
	made := p.fake.Sessions()[0]
	// The other try noted the object under the first integration.
	p.exec(`UPDATE integration_refs SET provider_object_id = ?, url = ?, state = 'open'`, made.ID, *made.URL)
	other := Link{ObjectID: "cs_test_" + strings.Repeat("z", 20), URL: "https://checkout.stripe.com/c/pay/x"}
	link, err := p.svc.noteLink(context.Background(), stale, req, other)
	if err != nil || link.ObjectID != made.ID || link.URL != *made.URL {
		t.Fatalf("link = %+v, %v: not the ref's own", link, err)
	}
	req.IntegrationID = p.in.ID + 100 // another integration is bound now
	_, err = p.svc.noteLink(context.Background(), stale, req, other)
	var pe *paylink.Error
	if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationChanged {
		t.Fatalf("err = %v", err)
	}
}
