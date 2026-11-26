package integrations

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
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
	"taskto.ai/taskto/internal/notifications"
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
	fake *testprovider.Stripe
	// sq is the fake Square of a rig made with newSquarePay (fake is then unused).
	sq    *testprovider.Square
	svc   *Service
	apps  *apps.Service
	owner *client
	ws    int64
	space access.Space
	in    Integration
	app   apps.App
	// approval is the admin's approval of the app's payments contract.
	approval apps.IntegrationApproval
	// offset moves the apps' clock on, in nanoseconds.
	offset *atomic.Int64
	stop   func()
}

func providerKinds(provider string) []string {
	if p := Lookup(provider); p != nil {
		return p.Kinds()
	}
	return nil
}

func newPay(t *testing.T, public string) *pay { return newPayWith(t, public, invoiceApp) }

// newPayWith is newPay with the invoicing app's definition, for a test that needs another.
func newPayWith(t *testing.T, public, definition string) *pay {
	return newPayFor(t, public, definition, "stripe")
}

// newSquarePay is newPay with a sandbox Square integration (the fake Square) instead of Stripe's.
func newSquarePay(t *testing.T, public string) *pay {
	return newPayFor(t, public, invoiceApp, "square")
}

func newPayFor(t *testing.T, public, definition, provider string) *pay {
	t.Helper()
	ctx := context.Background()
	e := newEnv(t, public) // the database, the key and the logs: the env's own routes are left unused
	fake := testprovider.NewStripe(t)
	var sq *testprovider.Square
	endpoint := fake.Host()
	if provider == "square" {
		sq = testprovider.NewSquare(t)
		endpoint = sq.Host()
	}
	// One clock moves the integrations' and the apps' alike.
	offset := new(atomic.Int64)
	svc := New(e.store, Options{PublicURL: public, Egress: ptr(egress.MustParse("127.0.0.1")),
		Now:           func() time.Time { return time.Now().Add(time.Duration(offset.Load())) },
		ClientOptions: []ClientOption{WithTestEndpoint(endpoint), WithoutConnectionReuse()}})

	mux := http.NewServeMux()
	a := auth.New(e.db, auth.Options{HashParams: auth.Params{Memory: 64, Time: 1, Threads: 1}, SetupToken: setupToken})
	a.Routes(mux)
	spaces := access.New(e.db, a)
	spaces.Routes(mux)
	svc.Routes(mux, a, spaces)
	svc.HookRoutes(mux)
	p := &pay{env: e, fake: fake, sq: sq, svc: svc, offset: offset}
	p.apps = apps.New(e.db, spaces).WithPlatform(apps.Platform{ProviderKinds: providerKinds, Payments: svc}).
		WithClock(func() time.Time { return time.Now().Add(time.Duration(p.offset.Load())) })
	p.apps.Routes(mux)
	p.apps.ApprovalRoutes(mux, a)
	svc.UseRecords(p.apps) // stored events are applied (by drainEvents: the tests run the worker by hand)
	inbox, err := notifications.New(ctx, e.db, spaces, notifications.Options{Key: e.key})
	if err != nil {
		t.Fatal(err)
	}
	inbox.Routes(mux, a)
	e.h = a.ClientAddr(auth.CSRF(mux))

	p.owner, p.ws = e.setup()
	rec := p.owner.do(http.MethodPost, wsPath(p.ws, "spaces"), map[string]string{"name": "Team"})
	expect(t, rec, http.StatusCreated)
	p.space = decode[access.Space](t, rec)

	body := stripeBody("main")
	body["secrets"] = map[string]string{"api_key": fake.APIKey(), "webhook_secret": stripeHook}
	body["currencies"] = []string{"EUR", "JPY"}
	if provider == "square" {
		body = squareBody("main")
		body["settings"] = squareSettings()
		body["secrets"] = map[string]string{"api_key": testprovider.SquareTestToken, "webhook_secret": testprovider.SquareSignatureKey}
	}
	p.in = e.create(p.owner, p.ws, body)
	if _, err := e.db.ExecContext(ctx, `INSERT INTO integration_spaces (integration_id, space_id) VALUES (?, ?)`, p.in.ID, p.space.ID); err != nil {
		t.Fatal(err)
	}
	p.app, p.approval = p.publishApp("Invoices", definition)
	return p
}

// publishApp builds an app from the definition in the pay space, has its payments contract approved
// for the integration and publishes it.
func (p *pay) publishApp(name, definition string) (apps.App, apps.IntegrationApproval) {
	p.t.Helper()
	rec := p.owner.do(http.MethodPost, p.appsPath(""), map[string]any{"name": name, "definition": rawJSON(definition)})
	expect(p.t, rec, http.StatusCreated)
	app := decode[apps.App](p.t, rec)
	rec = p.owner.do(http.MethodGet, p.appsPath(fmt.Sprintf("/%d/integrations", app.ID)), nil)
	expect(p.t, rec, http.StatusOK)
	hash := decode[apps.IntegrationStatus](p.t, rec).Slots[0].ContractHash
	rec = p.owner.do(http.MethodPost, p.appsPath(fmt.Sprintf("/%d/integrations/payments/request", app.ID)), map[string]any{"contract_hash": hash})
	expect(p.t, rec, http.StatusOK)
	approval := decode[apps.IntegrationApproval](p.t, rec)
	expect(p.t, p.owner.do(http.MethodPost, wsPath(p.ws, fmt.Sprintf("integration-approvals/%d/approve", approval.ID)),
		map[string]any{"integration_id": p.in.ID}), http.StatusOK)
	expect(p.t, p.owner.do(http.MethodPost, p.appsPath(fmt.Sprintf("/%d/publish", app.ID)),
		map[string]any{"version": app.Version, "acknowledge": []any{}}), http.StatusOK)
	return app, approval
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

// waitsUnknown waits for the only entry to wait on the reconciliation's schedule: a key's outcome is
// unknown, and the wait counts against none of its tries.
func (p *pay) waitsUnknown() {
	p.t.Helper()
	p.waitFor("the entry waits for the outcome", func() bool {
		es := p.entries()
		return len(es) == 1 && es[0].state == "pending" && es[0].code == string(apierr.CodeIntegrationLinkLookupPending)
	})
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
	p.waitsUnknown()
	if r := p.refs(); len(r) != 1 || r[0].object != "" || r[0].state != "creating" {
		t.Fatalf("refs after the dropped answer = %+v", r)
	}
	p.later(time.Minute)
	p.settled(apps.StateDone)

	// The outcome of the dropped call was unknown, so the retry made no call: it read what Stripe holds
	// and adopted the object the call made.
	if reqs := p.posts(); len(reqs) != 1 || reqs[0].Replayed {
		t.Fatalf("calls = %+v: the retry should only have looked", reqs)
	}
	if n := len(p.fake.RequestsTo(http.MethodGet, "/v1/checkout/sessions")); n != 1 {
		t.Fatalf("%d lookups, want 1", n)
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

	// And one that crashed after its call was written ahead and before the answer was noted finds the
	// object by the key's token and makes nothing.
	p.exec(`UPDATE integration_refs SET provider_object_id = NULL, url = NULL, state = 'creating'`)
	p.exec(`UPDATE integration_ref_attempts SET outcome = NULL, object_id = NULL, url = NULL, object_verified = 0`)
	third, err := p.svc.OpenPaymentLink(context.Background(), req)
	if err != nil || third.ObjectID != first.ObjectID || third.URL != first.URL {
		t.Fatalf("third = %+v, %v", third, err)
	}
	reqs := p.posts()
	if len(reqs) != 1 || len(p.fake.Sessions()) != 1 {
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
			p.waitsUnknown()
			if r := p.refs(); len(r) != 1 || r[0].keySeq != 0 || r[0].object != "" {
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
			if len(refs) != 1 || refs[0].keySeq != 0 || refs[0].state != "open" || refs[0].effect.Int64 != e.id || refs[0].object != p.objectID(shape.path) {
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

// While Stripe can't say whether an earlier call made the object, nothing is made under another key:
// the lookups that fail tell nothing, the call is asked again only under its own key (which Stripe
// answers with the 500 it kept), the entry waits without using up its tries and, once the lookup
// works, adopts the object.
func TestWhileTheLookupFailsNothingIsMade(t *testing.T) {
	for name, shape := range linkShapes {
		t.Run(name, func(t *testing.T) {
			p := newPay(t, shape.public)
			record := p.invoice(10, "EUR")
			p.fake.Fail(testprovider.ServerError().AfterExecuting().On(http.MethodPost, shape.path))
			p.fake.Fail(testprovider.Unavailable().Times(2).On(http.MethodGet, shape.path))
			p.startWorker()
			p.send(record, "op-send-0006")
			p.waitsUnknown()
			for try := 2; try <= 3; try++ {
				p.later(time.Hour)
				p.waitFor("the lookup fails", func() bool {
					return len(p.fake.RequestsTo(http.MethodPost, shape.path)) >= try && p.entries()[0].state == "pending"
				})
				p.waitsUnknown()
				if r := p.refs(); len(r) != 1 || r[0].keySeq != 0 || r[0].object != "" || r[0].state != "creating" {
					t.Fatalf("try %d: refs = %+v", try, r)
				}
				if es := p.entries(); es[0].attempts != 0 {
					t.Fatalf("try %d: the wait used up tries: %+v", try, es[0])
				}
			}
			p.later(time.Hour)
			p.settled(apps.StateDone)
			posts := p.fake.RequestsTo(http.MethodPost, shape.path)
			for i, r := range posts {
				if r.IdempotencyKey != posts[0].IdempotencyKey || (i > 0 && !r.Replayed) {
					t.Fatalf("a call under another key, or one that ran again: %+v", posts)
				}
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
	p.fake.Fail(testprovider.ServerError().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	p.startWorker()
	p.send(record, "op-send-0007")
	p.waitsUnknown()
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

// The answer is lost after Stripe made the link: the outcome is unknown, so the entry makes no call
// again; it looks, and adopts the link Stripe made, so the payee has one payable link.
func TestWithoutAPublicURLACrashBeforeTheAnswerIsNotedMakesNoSecondLink(t *testing.T) {
	p := newPay(t, "")
	record := p.invoice(50, "EUR")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/payment_links"))
	p.startWorker()
	p.send(record, "op-hosted-0003")
	p.waitFor("the first call", func() bool { return len(p.linkPosts()) == 1 })
	p.waitsUnknown()
	if r := p.refs(); len(r) != 1 || r[0].object != "" || r[0].state != "creating" {
		t.Fatalf("refs after the dropped answer = %+v", r)
	}
	p.later(time.Minute)
	p.settled(apps.StateDone)

	if reqs := p.linkPosts(); len(reqs) != 1 || reqs[0].Replayed {
		t.Fatalf("calls = %+v: the retry should only have looked", reqs)
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
// crashed before it noted the answer finds the link by its key's token and makes none.
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
	p.exec(`UPDATE integration_refs SET provider_object_id = NULL, url = NULL, state = 'creating'`)
	p.exec(`UPDATE integration_ref_attempts SET outcome = NULL, object_id = NULL, url = NULL, object_verified = 0`)
	third, err := p.svc.OpenPaymentLink(context.Background(), req)
	if err != nil || third != first {
		t.Fatalf("third = %+v, %v", third, err)
	}
	reqs := p.linkPosts()
	if len(reqs) != 1 || len(p.fake.Links()) != 1 {
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
				_, found, _, err := stripe{}.FindPaymentLink(context.Background(), Credentials{APIKey: Secret("not-a-key")}, c, nl, time.Now())
				if ce, ok := IsCall(err); !ok || ce.Kind != CallUnreachable || found {
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
	_, found, _, err := stripe{}.FindPaymentLink(context.Background(), Credentials{APIKey: Secret("not-a-key")}, c, NewLink{Token: strings.Repeat("a", 32), AmountMinor: 100, Currency: "EUR"}, time.Now())
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
			link, found, _, err := stripe{}.FindPaymentLink(context.Background(), creds, c, nl, time.Now())
			if err != nil || !found || link.ObjectID == "" || link.URL == "" {
				t.Fatalf("the object of the terms: %+v, %v, %v", link, found, err)
			}
			for what, change := range map[string]func(*NewLink){
				"another amount":   func(n *NewLink) { n.AmountMinor = 100 },
				"another currency": func(n *NewLink) { n.Currency = "USD" },
			} {
				other := nl
				change(&other)
				got, found, _, err := stripe{}.FindPaymentLink(context.Background(), creds, c, other, time.Now())
				// Refused, and the object seen is named, for its key to keep: never a link to give.
				if ce, ok := IsCall(err); !ok || ce.Kind != CallRefused || found || got.URL != "" || got.ObjectID != link.ObjectID {
					t.Fatalf("%s: %+v, %v, %v", what, got, found, err)
				}
			}
			// Another reference's token is no match at all.
			nl.Token = strings.Repeat("c", 32)
			if _, found, _, err := (stripe{}).FindPaymentLink(context.Background(), creds, c, nl, time.Now()); err != nil || found {
				t.Fatalf("another token: %v, %v", found, err)
			}
		})
	}
}

// A key moves on only from the key a try resolved: a late try that resolved an older key doesn't skip
// the key a later one is using, and a key whose outcome is unknown never moves.
func TestTheKeyMovesOnOnlyFromAResolvedKeyItUsed(t *testing.T) {
	ctx := context.Background()
	p := newPay(t, publicURL)
	invoice := p.invoice(10, "EUR")
	req := p.direct(invoice, "op-send-0008")
	if _, err := p.svc.OpenPaymentLink(ctx, req); err != nil {
		t.Fatal(err)
	}
	p.exec(`UPDATE integration_refs SET provider_object_id = NULL, url = NULL, state = 'creating'`)
	p.exec(`UPDATE integration_ref_attempts SET outcome = NULL`)
	r, _, err := p.svc.refOf(ctx, req.EffectID)
	if err != nil {
		t.Fatal(err)
	}
	a0, err := p.svc.attemptByKey(ctx, r.id, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Unknown (a call was written ahead and nothing resolved it): the key stays.
	if r, err = p.svc.nextAttempt(ctx, r, a0); err != nil || r.keySeq != 0 {
		t.Fatalf("an unknown key moved on: %+v, %v", r, err)
	}
	if err := p.svc.markAttempt(ctx, a0, attemptExpired); err != nil {
		t.Fatal(err)
	}
	if r, err = p.svc.nextAttempt(ctx, r, a0); err != nil || r.keySeq != 1 {
		t.Fatalf("an expired key didn't move on: %+v, %v", r, err)
	}
	// The same late resolution again: the key the later try uses stays.
	if r, err = p.svc.nextAttempt(ctx, r, a0); err != nil || r.keySeq != 1 {
		t.Fatalf("a stale try moved the key to %d, %v", r.keySeq, err)
	}
}

// A key the provider may have forgotten (it keeps one for a day, at least) is never called again: its
// object is looked for, so a call under the same key can't make a second one.
func TestAnOldRefWithNoObjectLooksBeforeItCallsAgain(t *testing.T) {
	for name, shape := range linkShapes {
		t.Run(name, func(t *testing.T) {
			p := newPay(t, shape.public)
			record := p.invoice(10, "EUR")
			p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, shape.path))
			p.startWorker()
			p.send(record, "op-send-0009")
			p.waitsUnknown()
			if r := p.refs(); len(r) != 1 || r[0].keySeq != 0 || r[0].object != "" {
				t.Fatalf("refs = %+v", r)
			}
			p.exec(`UPDATE integration_refs SET created_at = ?`, p.svc.store.stamp(time.Now().Add(-25*time.Hour)))
			p.exec(`UPDATE integration_ref_attempts SET first_sent_at = ?`, p.svc.store.stamp(time.Now().Add(-25*time.Hour)))
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
		// A refusal of the call may come after its execution began (Stripe keeps it under the key): its
		// outcome is unknown, and the entry waits; asked again under the same key, the call that never
		// ran runs.
		"a refused request": {testprovider.Refusal("amount_too_small").On(http.MethodPost, "/v1/checkout/sessions"), apierr.CodeIntegrationLinkLookupPending, "pending"},
		// A key Stripe doesn't take is refused before anything (whose account the key is).
		"a revoked key":        {testprovider.Unauthorized().Forever(), apierr.CodeIntegrationCredentialsRefused, "failed"},
		"stripe down for good": {testprovider.Unavailable().Forever().On(http.MethodPost, "/v1/checkout/sessions"), apierr.CodeIntegrationLinkLookupPending, "pending"},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPay(t, publicURL)
			record := p.invoice(10, "EUR")
			p.fake.Fail(tc.fault)
			p.startWorker()
			p.send(record, "op-fail-0001")
			p.waitFor("the entry's failure", func() bool {
				es := p.entries()
				return len(es) == 1 && es[0].code == string(tc.code) && (es[0].state == tc.state)
			})
			if tc.state == "failed" {
				if n := p.count(`SELECT count(*) FROM integration_refs`); n != 0 {
					t.Fatalf("%d refs for a key Stripe refused", n)
				}
				// A retry by the person who acted, once the key works, makes the link.
				p.fake.ClearFaults()
				expect(t, p.owner.do(http.MethodPost, p.appsPath(fmt.Sprintf("/%d/effects/%d/retry", p.app.ID, p.entries()[0].id)), map[string]any{}), http.StatusOK)
				p.settled(apps.StateDone)
				if r := p.refs(); len(r) != 1 || r[0].state != "open" || len(p.fake.Sessions()) != 1 {
					t.Fatalf("after the retry: refs = %+v, sessions %d", r, len(p.fake.Sessions()))
				}
			}
			if name == "a refused request" {
				// The same key is asked again, and Stripe, which kept nothing of the refusal, runs it.
				p.later(time.Hour)
				p.settled(apps.StateDone)
				posts := p.posts()
				if len(posts) != 2 || posts[0].IdempotencyKey != posts[1].IdempotencyKey || len(p.fake.Sessions()) != 1 {
					t.Fatalf("calls = %+v, sessions %d", posts, len(p.fake.Sessions()))
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
	// Rate limited: the call was not executed, so asking again under its key is safe (a dropped
	// connection would leave the outcome unknown: TestAnUnknownOutcomeIsOnlyLookedFor...).
	p.fake.Fail(testprovider.RateLimited(0).Forever().On(http.MethodPost, "/v1/checkout/sessions"))
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
				// Found by the look, in the kind of objects the first try made, not by a call.
				if posts := p.fake.RequestsTo(http.MethodPost, tc.path); len(posts) != 1 || len(p.fake.RequestsTo(http.MethodGet, tc.path)) != 1 {
					t.Fatalf("posts %d, lists %d", len(posts), len(p.fake.RequestsTo(http.MethodGet, tc.path)))
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
	p.waitsUnknown()
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

// More active links than a lookup reads, none of them the token's: the lookup can't say, and asking
// again reads the same. It tells nothing of the key's outcome, which stays unknown: no key of its own is
// taken on it, the call is asked again only under its key (which Stripe answers with the 500 it kept),
// and the entry ends, at the end of the wait, with the code an admin confirms from.
func TestALookupThatCannotSeeEverythingLeavesTheOutcomeUnknown(t *testing.T) {
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
	// The call's execution began and made nothing, and Stripe keeps its 500 under the key.
	p.fake.Fail(testprovider.ServerError().KeptUnderKey().On(http.MethodPost, "/v1/payment_links"))
	p.startWorker()
	p.send(record, "op-cap-0001")
	p.waitsUnknown()
	p.later(time.Minute)
	p.looked("/v1/payment_links", findMaxPages)
	if es := p.entries(); es[0].state != apps.StatePending || es[0].code != string(apierr.CodeIntegrationLinkLookupPending) {
		t.Fatalf("entry = %+v", es[0])
	}
	p.later(7 * time.Hour)
	e := p.settled(apps.StateFailed)
	if e.code != string(apierr.CodeIntegrationOutcomeUnknown) {
		t.Fatalf("entry = %+v", e)
	}
	posts := p.linkPosts()[others:]
	for _, r := range posts {
		if r.IdempotencyKey != posts[0].IdempotencyKey {
			t.Fatalf("a call under another key on a lookup that couldn't see everything: %+v", posts)
		}
	}
	if n := len(p.fake.Links()); n != others {
		t.Fatalf("%d links, want only the others", n)
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
	a, err := p.svc.attemptByKey(context.Background(), r.id, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Another try marked the ref failed...
	p.exec(`UPDATE integration_refs SET state = 'failed'`)
	// ...and this try's answer, which made the object, is noted all the same, with its key's outcome.
	if _, err := p.svc.noteLink(context.Background(), stale, a, req, Link{ObjectID: p.fake.Sessions()[0].ID, URL: *p.fake.Sessions()[0].URL}); err != nil {
		t.Fatal(err)
	}
	if got := p.refs()[0]; got.state != "open" || got.object != p.fake.Sessions()[0].ID {
		t.Fatalf("the answer was not recorded: %+v", got)
	}
	if a, err := p.svc.attemptByKey(context.Background(), r.id, 0); err != nil || a.outcome != attemptMade || a.objectID != p.fake.Sessions()[0].ID {
		t.Fatalf("the key = %+v, %v", a, err)
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
			got, found, _, err := stripe{}.FindPaymentLink(context.Background(), Credentials{APIKey: Secret(p.fake.APIKey())}, c, nl, time.Now())
			if found != tc.found {
				t.Fatalf("found = %v, err = %v", found, err)
			}
			if tc.kind == 0 {
				if err != nil || (tc.found && got.URL == "") {
					t.Fatalf("link = %+v, err = %v", got, err)
				}
				return
			}
			if ce, ok := IsCall(err); !ok || ce.Kind != tc.kind || got.URL != "" {
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
	if len(reqs) != 2 || reqs[0].IdempotencyKey == reqs[1].IdempotencyKey || reqs[1].IdempotencyKey != p.keyToken(1) ||
		reqs[1].Form.Get("client_reference_id") != p.keyToken(1) {
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

// Stripe may keep any answer of a call whose execution began under its key, a refusal of 4xx
// included, so a refusal leaves the key's outcome unknown and never moves the ref to another key: the
// next try looks for what the call made and asks again only under the same key.
func TestARefusalNeverMovesTheKey(t *testing.T) {
	for name, shape := range linkShapes {
		t.Run(name+" that made nothing", func(t *testing.T) {
			p := newPay(t, shape.public)
			record := p.invoice(10, "EUR")
			req := p.direct(record, "op-failed-0001")
			p.fake.Fail(testprovider.Refusal("amount_too_small").On(http.MethodPost, shape.path))
			var pe *paylink.Error
			if _, err := p.svc.OpenPaymentLink(context.Background(), req); !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationLinkLookupPending {
				t.Fatalf("err = %v", err)
			}
			if r := p.refs()[0]; r.state != "creating" || r.keySeq != 0 {
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
			if !looked || len(posts) != 2 || posts[1].IdempotencyKey != posts[0].IdempotencyKey || p.refs()[0].keySeq != 0 {
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
				t.Fatal("the fake made no object")
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
	a, err := p.svc.attemptByKey(context.Background(), r.id, 0)
	if err != nil {
		t.Fatal(err)
	}
	made := p.fake.Sessions()[0]
	// The other try noted the object under the first integration.
	p.exec(`UPDATE integration_refs SET provider_object_id = ?, url = ?, state = 'open'`, made.ID, *made.URL)
	other := Link{ObjectID: "cs_test_" + strings.Repeat("z", 20), URL: "https://checkout.stripe.com/c/pay/x"}
	link, err := p.svc.noteLink(context.Background(), stale, a, req, other)
	if err != nil || link.ObjectID != made.ID || link.URL != *made.URL {
		t.Fatalf("link = %+v, %v: not the ref's own", link, err)
	}
	req.IntegrationID = p.in.ID + 100 // another integration is bound now
	_, err = p.svc.noteLink(context.Background(), stale, a, req, other)
	var pe *paylink.Error
	if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationChanged {
		t.Fatalf("err = %v", err)
	}
}

// ----- an unknown outcome is never answered with a second link -----

// keyToken is the token of the only ref's key number seq.
func (p *pay) keyToken(seq int) string {
	p.t.Helper()
	var token string
	if err := p.db.QueryRow(`SELECT token FROM integration_ref_attempts WHERE seq = ?`, seq).Scan(&token); err != nil {
		p.t.Fatal(err)
	}
	return token
}

// keysUsed is the distinct idempotency keys the calls to path were made under, in the order first used.
func (p *pay) keysUsed(path string) []string {
	var out []string
	for _, r := range p.fake.RequestsTo(http.MethodPost, path) {
		if r.IdempotencyKey != "" && !slices.Contains(out, r.IdempotencyKey) { // "": made by the test, not the server
			out = append(out, r.IdempotencyKey)
		}
	}
	return out
}

// appear makes, at Stripe, the object the n-th call to path asked for, with its token: the object a
// call whose outcome was unknown made after all, as Stripe's own reconciliation of an incident would.
func (p *pay) appear(path string, n int) {
	p.t.Helper()
	p.mustPost(path, p.fake.RequestsTo(http.MethodPost, path)[n].Form)
}

// unknownOutcome sends an invoice whose first call is answered with a 500 that Stripe keeps under the
// key, having made nothing (yet), and returns once the entry waits for the outcome.
func (p *pay) unknownOutcome(shape struct {
	public string
	path   string
}, operation string) int64 {
	p.t.Helper()
	record := p.invoice(10, "EUR")
	p.fake.Fail(testprovider.ServerError().KeptUnderKey().On(http.MethodPost, shape.path))
	p.startWorker()
	p.send(record, operation)
	p.waitFor("the first call", func() bool { return len(p.fake.RequestsTo(http.MethodPost, shape.path)) == 1 })
	p.waitsUnknown()
	return record
}

// looked waits for the n-th read of what Stripe holds and for the entry to be waiting (or failed) again.
func (p *pay) looked(path string, n int) {
	p.t.Helper()
	p.waitFor(fmt.Sprintf("lookup %d", n), func() bool {
		es := p.entries()
		return len(p.fake.RequestsTo(http.MethodGet, path)) >= n && len(es) == 1 && es[0].state != "running" &&
			(es[0].state == "failed" || es[0].code == string(apierr.CodeIntegrationLinkLookupPending))
	})
}

// Stripe documents a 5xx as indeterminate: the object may be made after the answer, and a call under a
// new key then makes a second payable link for one invoice. After the 5xx and an empty lookup only the
// same key is asked again (Stripe answers it with the 500 it kept); when the first object turns up, it
// is the link, and the invoice has one.
func TestAnEmptyLookupAfterA5xxNeverMakesASecondLinkWhenTheFirstAppearsLater(t *testing.T) {
	for name, shape := range linkShapes {
		t.Run(name, func(t *testing.T) {
			p := newPay(t, shape.public)
			record := p.unknownOutcome(shape, "op-late-0001")
			p.later(time.Minute)
			p.looked(shape.path, 1)
			if keys := p.keysUsed(shape.path); len(keys) != 1 {
				t.Fatalf("calls under %d keys after an empty lookup, want the first only", len(keys))
			}
			if es := p.entries(); es[0].state != "pending" || es[0].code != string(apierr.CodeIntegrationLinkLookupPending) {
				t.Fatalf("entry = %+v", es[0])
			}
			if r := p.refs(); len(r) != 1 || r[0].keySeq != 0 || r[0].object != "" {
				t.Fatalf("refs = %+v: the ref moved on from its key", r)
			}

			p.appear(shape.path, 0) // Stripe completes the first call
			p.later(time.Hour)
			e := p.settled(apps.StateDone)

			if keys := p.keysUsed(shape.path); len(keys) != 1 {
				t.Fatalf("calls under %d keys", len(keys))
			}
			if n := p.objects(shape.path); n != 1 {
				t.Fatalf("%d objects at Stripe, want 1", n)
			}
			refs := p.refs()
			if len(refs) != 1 || refs[0].state != "open" || refs[0].object != p.objectID(shape.path) || refs[0].effect.Int64 != e.id {
				t.Fatalf("refs = %+v", refs)
			}
			if data, _ := p.record(record); !strings.Contains(data, refs[0].object) {
				t.Fatalf("record = %s", data)
			}
		})
	}
}

// A 5xx before the call ran keeps nothing under the key: asked again under the same key, the call
// runs, and the invoice has its link at once, under one key.
func TestA5xxThatRanNothingIsAskedAgainUnderItsOwnKey(t *testing.T) {
	for name, shape := range linkShapes {
		t.Run(name, func(t *testing.T) {
			p := newPay(t, shape.public)
			record := p.invoice(10, "EUR")
			p.fake.Fail(testprovider.ServerError().On(http.MethodPost, shape.path))
			p.startWorker()
			p.send(record, "op-late-0002")
			p.waitsUnknown()
			p.later(time.Minute)
			p.settled(apps.StateDone)
			posts := p.fake.RequestsTo(http.MethodPost, shape.path)
			if len(posts) != 2 || posts[1].IdempotencyKey != posts[0].IdempotencyKey || posts[1].Replayed || p.objects(shape.path) != 1 {
				t.Fatalf("calls = %+v, objects %d", posts, p.objects(shape.path))
			}
		})
	}
}

// The wait is bounded: after outcomeWait with nothing found the entry fails with the code that tells
// an admin to check Stripe, and no call under another key was made.
func TestTheBoundedWaitEndsInTheOutcomeUnknownCodeWithNoOtherKey(t *testing.T) {
	for name, shape := range linkShapes {
		t.Run(name, func(t *testing.T) {
			p := newPay(t, shape.public)
			p.unknownOutcome(shape, "op-bound-0001")
			p.later(time.Minute)
			p.looked(shape.path, 1)
			p.later(5 * time.Hour) // still within the wait
			p.looked(shape.path, 2)
			if es := p.entries(); es[0].state != "pending" || es[0].code != string(apierr.CodeIntegrationLinkLookupPending) || es[0].attempts != 0 {
				t.Fatalf("within the wait: entry = %+v", es[0])
			}
			p.later(2 * time.Hour) // past it
			e := p.settled(apps.StateFailed)
			if e.code != string(apierr.CodeIntegrationOutcomeUnknown) {
				t.Fatalf("entry = %+v", e)
			}
			if keys := p.keysUsed(shape.path); len(keys) != 1 {
				t.Fatalf("calls under %d keys, want the first only", len(keys))
			}
			if n := p.objects(shape.path); n != 0 {
				t.Fatalf("%d objects at Stripe", n)
			}
			if r := p.refs(); len(r) != 1 || r[0].keySeq != 0 || r[0].object != "" {
				t.Fatalf("refs = %+v", r)
			}
			// A retry of the entry looks once more and, finding nothing, fails again at once.
			expect(t, p.owner.do(http.MethodPost, p.appsPath(fmt.Sprintf("/%d/effects/%d/retry", p.app.ID, e.id)), map[string]any{}), http.StatusOK)
			p.looked(shape.path, 4)
			if es := p.settled(apps.StateFailed); es.code != string(apierr.CodeIntegrationOutcomeUnknown) || len(p.keysUsed(shape.path)) != 1 {
				t.Fatalf("after the retry: %+v", es)
			}
		})
	}
}

func (p *pay) confirmPath(effect int64, tail string) string {
	return wsPath(p.ws, fmt.Sprintf("integrations/%d/payment-links/%s%s", p.in.ID, tail, ""))
}

// An admin who checked Stripe and found nothing confirms it: exactly one new key is allowed, the entry
// runs again and makes exactly one link; confirming again does nothing, and the audit log says who.
func TestAnAdminConfirmingNoLinkAllowsExactlyOneNewKey(t *testing.T) {
	for name, shape := range linkShapes {
		t.Run(name, func(t *testing.T) {
			p := newPay(t, shape.public)
			record := p.unknownOutcome(shape, "op-confirm-0001")
			p.later(time.Minute)
			p.looked(shape.path, 1)
			p.later(7 * time.Hour)
			e := p.settled(apps.StateFailed)
			if e.code != string(apierr.CodeIntegrationOutcomeUnknown) {
				t.Fatalf("entry = %+v", e)
			}

			rec := p.owner.do(http.MethodGet, p.confirmPath(e.id, "unresolved"), nil)
			expect(t, rec, http.StatusOK)
			list := decode[PaymentLinkOutcomes](t, rec)
			if len(list.PaymentLinks) != 1 || list.PaymentLinks[0].EffectID != e.id || !list.PaymentLinks[0].CanConfirm || list.PaymentLinks[0].Waiting ||
				list.PaymentLinks[0].Amount != "10.00" || list.PaymentLinks[0].Currency != "EUR" || list.PaymentLinks[0].SpaceID != p.space.ID ||
				list.PaymentLinks[0].Since == "" {
				t.Fatalf("list = %+v", list)
			}

			expect(t, p.owner.do(http.MethodPost, p.confirmPath(e.id, fmt.Sprintf("%d/confirm-no-link", e.id)), map[string]any{}), http.StatusNoContent)
			// A second confirmation finds nothing to decide: no second key.
			rec = p.owner.do(http.MethodPost, p.confirmPath(e.id, fmt.Sprintf("%d/confirm-no-link", e.id)), map[string]any{})
			expect(t, rec, http.StatusConflict)
			if got := decode[struct {
				Code string `json:"code"`
			}](t, rec).Code; got != string(apierr.CodeIntegrationNothingToConfirm) {
				t.Fatalf("code = %q", got)
			}
			p.later(time.Hour)
			p.settled(apps.StateDone)

			keys := p.keysUsed(shape.path)
			if len(keys) != 2 || keys[1] != p.keyToken(1) || keys[0] != p.keyToken(0) {
				t.Fatalf("keys = %v: want the first and exactly one more", keys)
			}
			var underNew int
			for _, r := range p.fake.RequestsTo(http.MethodPost, shape.path) {
				if r.IdempotencyKey == keys[1] {
					underNew++
				}
			}
			if underNew != 1 {
				t.Fatalf("%d calls under the new key", underNew)
			}
			if n := p.objects(shape.path); n != 1 {
				t.Fatalf("%d objects at Stripe, want 1", n)
			}
			refs := p.refs()
			if len(refs) != 1 || refs[0].keySeq != 1 || refs[0].state != "open" || refs[0].object != p.objectID(shape.path) {
				t.Fatalf("refs = %+v", refs)
			}
			if data, _ := p.record(record); !strings.Contains(data, refs[0].object) {
				t.Fatalf("record = %s", data)
			}
			ev := p.events(p.ws, "integration_new_link_allowed")
			if len(ev) != 1 || ev[0].ActorID == nil || *ev[0].ActorID != p.owner.id {
				t.Fatalf("audit events = %+v", ev)
			}
			// What was confirmed is no longer waiting for anyone, and the abandoned key stays on record.
			rec = p.owner.do(http.MethodGet, p.confirmPath(e.id, "unresolved"), nil)
			expect(t, rec, http.StatusOK)
			if list := decode[PaymentLinkOutcomes](t, rec); len(list.PaymentLinks) != 0 {
				t.Fatalf("list = %+v", list)
			}
			if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE seq = 0 AND outcome = 'abandoned' AND sends > clean_sends`); n != 1 {
				t.Fatalf("the abandoned key's record: %d", n)
			}
		})
	}
}

// A lookup that stopped for another reason than the wait ending (the integration's account changed)
// is neither waiting nor ready for a decision: the list says so and names the code the entry failed
// with, and confirming is refused.
func TestAStoppedLookupIsListedWithItsCodeAndCannotBeConfirmed(t *testing.T) {
	shape := linkShapes["a checkout session"]
	p := newPay(t, shape.public)
	p.unknownOutcome(shape, "op-stopped-0001")
	p.later(time.Minute)
	p.looked(shape.path, 1)
	p.later(7 * time.Hour)
	e := p.settled(apps.StateFailed)
	p.exec(`UPDATE app_effects SET error_code = ? WHERE id = ?`, string(apierr.CodeIntegrationAccountChanged), e.id)

	rec := p.owner.do(http.MethodGet, p.confirmPath(e.id, "unresolved"), nil)
	expect(t, rec, http.StatusOK)
	list := decode[PaymentLinkOutcomes](t, rec)
	if len(list.PaymentLinks) != 1 {
		t.Fatalf("list = %+v", list)
	}
	if got := list.PaymentLinks[0]; got.Waiting || got.CanConfirm || got.Code != string(apierr.CodeIntegrationAccountChanged) {
		t.Fatalf("link = %+v: want stopped, with the entry's code", got)
	}
	expect(t, p.owner.do(http.MethodPost, p.confirmPath(e.id, fmt.Sprintf("%d/confirm-no-link", e.id)), map[string]any{}), http.StatusConflict)

	// A code whose message names values (the ceiling that refused the amount) is listed with them, as
	// the entry failed with them.
	p.exec(`UPDATE app_effects SET error_code = ?, error_params = ? WHERE id = ?`,
		string(apierr.CodeIntegrationAmountAboveMax), `{"max":"5.00","currency":"EUR"}`, e.id)
	rec = p.owner.do(http.MethodGet, p.confirmPath(e.id, "unresolved"), nil)
	expect(t, rec, http.StatusOK)
	if got := decode[PaymentLinkOutcomes](t, rec).PaymentLinks[0]; got.Code != string(apierr.CodeIntegrationAmountAboveMax) ||
		got.Params["max"] != "5.00" || got.Params["currency"] != "EUR" {
		t.Fatalf("link = %+v: want the code with its params", got)
	}

	// A link ready for a decision carries no code.
	p.exec(`UPDATE app_effects SET error_code = ? WHERE id = ?`, string(apierr.CodeIntegrationOutcomeUnknown), e.id)
	rec = p.owner.do(http.MethodGet, p.confirmPath(e.id, "unresolved"), nil)
	expect(t, rec, http.StatusOK)
	if got := decode[PaymentLinkOutcomes](t, rec).PaymentLinks[0]; !got.CanConfirm || got.Code != "" || got.Params != nil {
		t.Fatalf("link = %+v", got)
	}
}

// While the server still looks, nothing is asked of an admin: confirming then is refused, and the key
// stays where it is.
func TestAnAdminCannotConfirmWhileTheServerStillLooks(t *testing.T) {
	shape := linkShapes["a checkout session"]
	p := newPay(t, shape.public)
	p.unknownOutcome(shape, "op-confirm-0002")
	e := p.entries()[0]
	rec := p.owner.do(http.MethodPost, p.confirmPath(e.id, fmt.Sprintf("%d/confirm-no-link", e.id)), map[string]any{})
	expect(t, rec, http.StatusConflict)
	if r := p.refs(); r[0].keySeq != 0 {
		t.Fatalf("refs = %+v", r)
	}
	if n := p.count(`SELECT count(*) FROM audit_events WHERE action = 'integration_new_link_allowed'`); n != 0 {
		t.Fatalf("%d audit events for a refused confirmation", n)
	}
	if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE outcome IS NOT NULL`); n != 0 {
		t.Fatalf("a refused confirmation resolved %d keys", n)
	}
}

// The routes are an admin's, for the spaces the admin manages, and an entry of another workspace's
// integration is never reached.
func TestOnlyAnAdminOfTheWorkspaceMayConfirmNoLink(t *testing.T) {
	shape := linkShapes["a checkout session"]
	p := newPay(t, shape.public)
	p.unknownOutcome(shape, "op-confirm-0003")
	p.later(time.Minute)
	p.looked(shape.path, 1)
	p.later(7 * time.Hour)
	e := p.settled(apps.StateFailed)
	confirm := p.confirmPath(e.id, fmt.Sprintf("%d/confirm-no-link", e.id))
	list := p.confirmPath(e.id, "unresolved")

	member := p.invite(p.owner, p.ws, "member@example.com", "member")
	expect(t, member.do(http.MethodPost, confirm, map[string]any{}), http.StatusForbidden)
	expect(t, member.do(http.MethodGet, list, nil), http.StatusForbidden)
	var anyone client
	anyone.e = p.env
	expect(t, anyone.do(http.MethodPost, confirm, map[string]any{}), http.StatusUnauthorized)

	// Another workspace's admin, naming this workspace's integration in their own workspace.
	_, other := p.otherWorkspace(p.owner)
	rec := p.owner.do(http.MethodPost, wsPath(other, fmt.Sprintf("integrations/%d/payment-links/%d/confirm-no-link", p.in.ID, e.id)), map[string]any{})
	expect(t, rec, http.StatusNotFound)
	rec = p.owner.do(http.MethodGet, wsPath(other, fmt.Sprintf("integrations/%d/payment-links/unresolved", p.in.ID)), nil)
	expect(t, rec, http.StatusNotFound)

	// An admin who doesn't manage the entry's space is told it isn't there, and nothing changes.
	if err := p.svc.ConfirmNoPaymentLink(context.Background(), p.ws, p.owner.id, p.in.ID, e.id, map[int64]bool{}); !errors.Is(err, errNotFound) {
		t.Fatalf("err = %v", err)
	}
	if got, err := p.svc.unresolved(context.Background(), p.ws, p.in.ID, map[int64]bool{}); err != nil || len(got) != 0 {
		t.Fatalf("unresolved = %+v, %v", got, err)
	}
	if r := p.refs(); r[0].keySeq != 0 {
		t.Fatalf("refs = %+v", r)
	}
	if es := p.entries(); es[0].state != "failed" {
		t.Fatalf("entry = %+v", es[0])
	}
	if n := p.count(`SELECT count(*) FROM audit_events WHERE action = 'integration_new_link_allowed'`); n != 0 {
		t.Fatalf("%d audit events for refused confirmations", n)
	}
}

// A lookup that finds a Payment Link carrying the key's token but can't read its line items can't check
// its amount: the link is never adopted unchecked, and never taken for "none" either. It is kept on the
// key, the entry waits and then fails for a person, the admins' list names it, and an admin can't
// confirm that no link exists (which would allow a new key and a second payable link for the invoice).
// Once its line items can be read, a retry checks it and adopts it: one key, one link.
func TestALinkSeenWhoseAmountCannotBeReadBlocksANewKeyUntilItIsAdopted(t *testing.T) {
	shape := linkShapes["a payment link"]
	p := newPay(t, shape.public)
	record := p.unknownOutcome(shape, "op-unchecked-0001")
	p.appear(shape.path, 0) // Stripe completes the first call after all
	object := p.objectID(shape.path)
	p.fake.Fail(testprovider.Unavailable().Forever().On(http.MethodGet, shape.path+"/").
		When(func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/line_items") }))
	serverPosts := func() (n int) {
		for _, r := range p.fake.RequestsTo(http.MethodPost, shape.path) {
			if r.IdempotencyKey != "" {
				n++
			}
		}
		return n
	}
	seen := func() (id string, verified int) {
		t.Helper()
		if err := p.db.QueryRow(`SELECT coalesce(object_id, ''), object_verified FROM integration_ref_attempts WHERE seq = 0`).Scan(&id, &verified); err != nil {
			t.Fatal(err)
		}
		return id, verified
	}

	p.later(time.Minute)
	p.waitFor("the line items were asked for", func() bool {
		return len(p.fake.RequestsTo(http.MethodGet, shape.path+"/"+object+"/line_items")) >= 1
	})
	p.looked(shape.path, 1)
	if id, verified := seen(); id != object || verified != 0 {
		t.Fatalf("the key's object = %q (verified %d), want %q unchecked", id, verified, object)
	}
	if es := p.entries(); es[0].state != apps.StatePending || es[0].code != string(apierr.CodeIntegrationLinkLookupPending) {
		t.Fatalf("entry = %+v", es[0])
	}
	if n := serverPosts(); n != 1 {
		t.Fatalf("%d calls, want the first only: nothing is asked on top of an object seen", n)
	}

	p.later(7 * time.Hour)
	e := p.settled(apps.StateFailed)
	if e.code != string(apierr.CodeIntegrationOutcomeUnknown) {
		t.Fatalf("entry = %+v", e)
	}
	rec := p.owner.do(http.MethodGet, p.confirmPath(e.id, "unresolved"), nil)
	expect(t, rec, http.StatusOK)
	if list := decode[PaymentLinkOutcomes](t, rec); len(list.PaymentLinks) != 1 || list.PaymentLinks[0].ObjectID != object ||
		list.PaymentLinks[0].CanConfirm || list.PaymentLinks[0].Waiting {
		t.Fatalf("list = %+v", list)
	}
	rec = p.owner.do(http.MethodPost, p.confirmPath(e.id, fmt.Sprintf("%d/confirm-no-link", e.id)), map[string]any{})
	expect(t, rec, http.StatusConflict)
	if got := decode[struct {
		Code string `json:"code"`
	}](t, rec).Code; got != string(apierr.CodeIntegrationLinkNotConfirmable) {
		t.Fatalf("code = %q", got)
	}
	if r := p.refs(); len(r) != 1 || r[0].keySeq != 0 || r[0].object != "" {
		t.Fatalf("refs = %+v", r)
	}
	if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE outcome IS NOT NULL OR seq > 0`); n != 0 {
		t.Fatalf("a refused confirmation resolved or added %d keys", n)
	}
	if n := p.count(`SELECT count(*) FROM audit_events WHERE action = 'integration_new_link_allowed'`); n != 0 {
		t.Fatalf("%d audit events for a refused confirmation", n)
	}
	if keys, n := p.keysUsed(shape.path), p.objects(shape.path); len(keys) != 1 || n != 1 {
		t.Fatalf("keys %v, %d objects at Stripe", keys, n)
	}

	// The line items can be read again: a retry checks the link and adopts it.
	p.fake.ClearFaults()
	expect(t, p.owner.do(http.MethodPost, p.appsPath(fmt.Sprintf("/%d/effects/%d/retry", p.app.ID, e.id)), map[string]any{}), http.StatusOK)
	p.settled(apps.StateDone)
	refs := p.refs()
	if len(refs) != 1 || refs[0].state != "open" || refs[0].object != object || refs[0].keySeq != 0 {
		t.Fatalf("refs = %+v", refs)
	}
	if id, verified := seen(); id != object || verified != 1 {
		t.Fatalf("the key's object = %q (verified %d)", id, verified)
	}
	if keys, n := p.keysUsed(shape.path), p.objects(shape.path); len(keys) != 1 || n != 1 || serverPosts() != 1 {
		t.Fatalf("keys %v, %d objects at Stripe, %d calls", keys, n, serverPosts())
	}
	if data, _ := p.record(record); !strings.Contains(data, object) {
		t.Fatalf("record = %s", data)
	}
}

// ----- nothing that may have run is taken for something that didn't -----

// A lookup that fails tells nothing of a key whose outcome is unknown: the next call is under the same
// key, never a new one, and Stripe, which kept nothing of a refusal before the call ran, runs it.
func TestAFailedLookupTellsNothingAndOnlyTheSameKeyIsAskedAgain(t *testing.T) {
	for name, shape := range linkShapes {
		t.Run(name, func(t *testing.T) {
			p := newPay(t, shape.public)
			record := p.invoice(10, "EUR")
			req := p.direct(record, "op-lookup-0001")
			p.fake.Fail(testprovider.Refusal("amount_too_small").On(http.MethodPost, shape.path))
			if _, err := p.svc.OpenPaymentLink(context.Background(), req); err == nil {
				t.Fatal("the refusal was no error")
			}
			// The retry's lookup can't be read.
			p.fake.Fail(testprovider.Unavailable().On(http.MethodGet, shape.path))
			link, err := p.svc.OpenPaymentLink(context.Background(), req)
			if err != nil || link.ObjectID != p.objectID(shape.path) {
				t.Fatalf("link = %+v, %v", link, err)
			}
			posts := p.fake.RequestsTo(http.MethodPost, shape.path)
			if len(posts) != 2 || posts[1].IdempotencyKey != posts[0].IdempotencyKey || p.objects(shape.path) != 1 {
				t.Fatalf("keys: %+v", posts)
			}
		})
	}
}

// What the provider replays for a kept key is the object as it was made: a session that has expired
// since still says it is open. The state is read from the provider before anything is saved, and an
// expired object is not the link: the call is made again under a key of its own, once.
func TestAReplayedSessionThatExpiredIsNeverSavedAsTheLink(t *testing.T) {
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-replay-0001")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	if _, err := p.svc.OpenPaymentLink(context.Background(), req); err == nil {
		t.Fatal("the lost answer was no error")
	}
	dead := p.fake.Sessions()[0].ID
	if _, err := p.fake.Expire(dead); err != nil {
		t.Fatal(err)
	}
	// The lookup can't be read, so the call is asked again under its key, and Stripe answers with the
	// creation it kept.
	p.fake.Fail(testprovider.Unavailable().On(http.MethodGet, "/v1/checkout/sessions"))
	link, err := p.svc.OpenPaymentLink(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if link.ObjectID == "" || link.ObjectID == dead {
		t.Fatalf("link = %+v: the expired session %s was saved", link, dead)
	}
	posts := p.posts()
	if len(posts) != 3 || !posts[1].Replayed || posts[1].IdempotencyKey != posts[0].IdempotencyKey || posts[2].IdempotencyKey != p.keyToken(1) {
		t.Fatalf("calls = %+v: want the first, its replay, and one under the next key", posts)
	}
	if ss, ok := p.fake.Session(link.ObjectID); !ok || ss.Status != "open" {
		t.Fatalf("the link's session = %+v, %v", ss, ok)
	}
	if refs := p.refs(); len(refs) != 1 || refs[0].object != link.ObjectID || refs[0].keySeq != 1 {
		t.Fatalf("refs = %+v", refs)
	}
}

// settle saves an object only if a payer can use it now, whichever way the object came: a completed
// session and a Payment Link that is no longer active are never the link and stop the entry, an
// expired session is never the link, and one whose state can't be read is not saved either, with the
// key's outcome left unknown and its object kept on the key.
func TestNothingThatCannotTakePaymentIsSavedAsTheLink(t *testing.T) {
	ctx := context.Background()
	tryOf := func(p *pay, req paylink.Request, hosted bool) try {
		rec, _ := p.svc.store.getSnapshot(ctx, p.ws, p.in.ID)
		creds, _ := p.svc.store.credentials(ctx, p.ws, p.in.ID)
		client := p.svc.Client(Lookup(rec.Provider), rec.Settings, p.svc.clientOpts...)
		return try{s: p.svc, provider: Lookup(rec.Provider).(PaymentProvider), creds: creds, client: client, rec: rec, req: req, hosted: hosted}
	}
	for name, shape := range linkShapes {
		hosted := shape.public == ""
		for state, tc := range map[string]struct {
			make    func(p *pay, id string)
			outcome string
		}{
			"paid": {func(p *pay, id string) {
				if hosted {
					if _, err := p.fake.PayLink(id); err != nil {
						p.t.Fatal(err)
					}
					return
				}
				if _, err := p.fake.Pay(id); err != nil {
					p.t.Fatal(err)
				}
			}, attemptSettled},
			"expired": {func(p *pay, id string) {
				if _, err := p.fake.Expire(id); err != nil {
					p.t.Fatal(err)
				}
			}, attemptExpired},
		} {
			if hosted && state == "expired" {
				continue // a Payment Link has no expiry: deactivated, it is settled as far as anyone can say
			}
			t.Run(name+" "+state, func(t *testing.T) {
				p := newPay(t, shape.public)
				record := p.invoice(10, "EUR")
				req := p.direct(record, "op-state-0001")
				p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, shape.path))
				if _, err := p.svc.OpenPaymentLink(ctx, req); err == nil {
					t.Fatal("the lost answer was no error")
				}
				id := p.objectID(shape.path)
				var made Link // as the call answered it, before the object changed
				if hosted {
					l := p.fake.Links()[0]
					made = Link{ObjectID: l.ID, URL: l.URL}
				} else {
					ss := p.fake.Sessions()[0]
					made = Link{ObjectID: ss.ID, URL: *ss.URL}
				}
				tc.make(p, id)
				r, found, err := p.svc.refOf(ctx, req.EffectID)
				if err != nil || !found {
					t.Fatal(found, err)
				}
				a, err := p.svc.attemptByKey(ctx, r.id, r.keySeq)
				if err != nil {
					t.Fatal(err)
				}
				res, link, err := tryOf(p, req, hosted).settle(ctx, &r, a, made)
				if tc.outcome == attemptSettled {
					var pe *paylink.Error
					if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationLinkAlreadyPaid {
						t.Fatalf("err = %v", err)
					}
				} else if err != nil || res != resolvedExpired {
					t.Fatalf("got %v, %v", res, err)
				}
				if link.ObjectID != "" {
					t.Fatalf("link = %+v: saved", link)
				}
				if refs := p.refs(); len(refs) != 1 || refs[0].object != "" {
					t.Fatalf("refs = %+v: an object that cannot take payment was saved", refs)
				}
				if a, err := p.svc.attemptByKey(ctx, r.id, r.keySeq); err != nil || a.outcome != tc.outcome {
					t.Fatalf("the key = %+v, %v", a, err)
				}
			})
		}
	}

	t.Run("a state that cannot be read", func(t *testing.T) {
		p := newPay(t, publicURL)
		record := p.invoice(10, "EUR")
		req := p.direct(record, "op-state-0002")
		p.fake.Fail(testprovider.ServerError().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
		p.fake.Fail(testprovider.Unavailable().Forever().On(http.MethodGet, "/v1/checkout/sessions/"))
		_, _ = p.svc.OpenPaymentLink(ctx, req)
		ss := p.fake.Sessions()[0]
		// The lookup finds the session, and its state can't be read.
		_, err := p.svc.OpenPaymentLink(ctx, req)
		var pe *paylink.Error
		if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationLinkLookupPending {
			t.Fatalf("err = %v", err)
		}
		if refs := p.refs(); refs[0].object != "" {
			t.Fatalf("refs = %+v: saved", refs)
		}
		if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE outcome IS NULL AND sends > clean_sends`); n != 1 {
			t.Fatalf("the unreadable state resolved the key (%d)", n)
		}
		// Once the state reads, the session is the link, and nothing else was ever made.
		p.fake.ClearFaults()
		link, err := p.svc.OpenPaymentLink(ctx, req)
		if err != nil || link.ObjectID != ss.ID || len(p.posts()) != 1 {
			t.Fatalf("link = %+v, %v, %d calls", link, err, len(p.posts()))
		}
	})
}

// ----- the reviewer's cases: at most one payable link, and no evidence lost -----

// Every key of a ref has its own token. Key 0's session expired, and key 1's call ended in a 500 that
// made nothing yet: the lookups for key 1 see key 0's expired session in the account, which says
// nothing of key 1, so no third key is ever taken. When key 1's object turns up it is the link: one
// payable link.
func TestAnExpiredObjectOfOneKeyNeverResolvesAnotherKeysUnknownOutcome(t *testing.T) {
	ctx := context.Background()
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-keys-0001")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	if _, err := p.svc.OpenPaymentLink(ctx, req); err == nil {
		t.Fatal("the lost answer was no error")
	}
	first := p.fake.Sessions()[0].ID
	if _, err := p.fake.Expire(first); err != nil {
		t.Fatal(err)
	}
	p.fake.Fail(testprovider.ServerError().KeptUnderKey().On(http.MethodPost, "/v1/checkout/sessions"))
	pending := func(what string) {
		t.Helper()
		_, err := p.svc.OpenPaymentLink(ctx, req)
		var pe *paylink.Error
		if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationLinkLookupPending {
			t.Fatalf("%s: err = %v", what, err)
		}
	}
	pending("key 1's 500")
	for i := range 3 {
		pending(fmt.Sprintf("try %d", i))
	}
	keys := p.keysUsed("/v1/checkout/sessions")
	if len(keys) != 2 || keys[0] != p.keyToken(0) || keys[1] != p.keyToken(1) || keys[0] == keys[1] {
		t.Fatalf("keys = %v, want key 0's and key 1's only", keys)
	}
	if r := p.refs(); len(r) != 1 || r[0].keySeq != 1 || r[0].object != "" {
		t.Fatalf("refs = %+v", r)
	}
	// Each key's call carries its own token, which its object keeps.
	posts := p.posts()
	if posts[0].Form.Get("client_reference_id") != keys[0] || posts[len(posts)-1].Form.Get("client_reference_id") != keys[1] {
		t.Fatalf("tokens: %+v", posts)
	}

	p.appear("/v1/checkout/sessions", len(posts)-1) // key 1's call made its session after all
	link, err := p.svc.OpenPaymentLink(ctx, req)
	if err != nil || link.ObjectID == first {
		t.Fatalf("link = %+v, %v", link, err)
	}
	open := 0
	for _, ss := range p.fake.Sessions() {
		if ss.Status == "open" {
			open++
		}
	}
	if open != 1 || len(p.fake.Sessions()) != 2 {
		t.Fatalf("%d payable sessions of %d", open, len(p.fake.Sessions()))
	}
	// A payment echoes the token of the key whose object it paid, and that finds the ref.
	for _, token := range keys {
		if got, found, err := p.svc.refByToken(ctx, token); err != nil || !found || got.id != p.refs()[0].id {
			t.Fatalf("refByToken(%s) = %+v, %v, %v", token, got, found, err)
		}
	}
	if _, found, err := p.svc.refByToken(ctx, strings.Repeat("e", 32)); err != nil || found {
		t.Fatalf("a token the server never issued found a ref: %v, %v", found, err)
	}
}

// Stripe saves the result of a request whose execution began under its key, a 400 included, and the
// lookup finds nothing: the refusal is not taken for a request that never ran. The key never moves by
// itself, nor on a person's retry; the entry ends with the code an admin confirms from, and only that
// confirmation gives the ref a new key.
func TestARefusalKeptUnderTheKeyNeverRotatesTheKey(t *testing.T) {
	for name, shape := range linkShapes {
		t.Run(name, func(t *testing.T) {
			p := newPay(t, shape.public)
			record := p.invoice(10, "EUR")
			p.fake.Fail(testprovider.Refusal("amount_too_small").KeptUnderKey().On(http.MethodPost, shape.path))
			p.startWorker()
			p.send(record, "op-cached-0001")
			p.waitsUnknown()
			p.later(time.Minute)
			p.looked(shape.path, 1)
			p.later(7 * time.Hour)
			e := p.settled(apps.StateFailed)
			if e.code != string(apierr.CodeIntegrationOutcomeUnknown) {
				t.Fatalf("entry = %+v", e)
			}
			expect(t, p.owner.do(http.MethodPost, p.appsPath(fmt.Sprintf("/%d/effects/%d/retry", p.app.ID, e.id)), map[string]any{}), http.StatusOK)
			p.looked(shape.path, 3)
			if e = p.settled(apps.StateFailed); e.code != string(apierr.CodeIntegrationOutcomeUnknown) {
				t.Fatalf("after the retry: %+v", e)
			}
			if keys := p.keysUsed(shape.path); len(keys) != 1 || p.objects(shape.path) != 0 {
				t.Fatalf("keys = %v, objects %d", keys, p.objects(shape.path))
			}
			expect(t, p.owner.do(http.MethodPost, p.confirmPath(e.id, fmt.Sprintf("%d/confirm-no-link", e.id)), map[string]any{}), http.StatusNoContent)
			p.later(time.Hour)
			p.settled(apps.StateDone)
			if keys := p.keysUsed(shape.path); len(keys) != 2 || p.objects(shape.path) != 1 {
				t.Fatalf("keys = %v, objects %d", keys, p.objects(shape.path))
			}
		})
	}
}

// Only what Stripe documents as refused before the request ran leaves a call's outcome known: a rate
// limit or a lock timeout (429 with those codes) and the key in use by a request still running (409
// idempotency_key_in_use). Any other answer, a 429 or a 409 of another kind, a 408 and every 4xx and
// 5xx included, and an answer that can't be read, leave it unknown.
func TestOnlyWhatStripeDocumentsAsNotRunLeavesTheOutcomeKnown(t *testing.T) {
	for name, tc := range map[string]struct {
		status      int
		body        string
		notExecuted bool
		kind        CallKind
	}{
		"rate limited":           {429, `{"error":{"type":"invalid_request_error","code":"rate_limit"}}`, true, CallUnreachable},
		"a lock timeout":         {429, `{"error":{"type":"invalid_request_error","code":"lock_timeout"}}`, true, CallUnreachable},
		"the key in use":         {409, `{"error":{"type":"invalid_request_error","code":"idempotency_key_in_use"}}`, true, CallUnreachable},
		"a 429 of another kind":  {429, `{"error":{"type":"invalid_request_error"}}`, false, CallUnreachable},
		"a 409 of another kind":  {409, `{"error":{"type":"idempotency_error"}}`, false, CallUnreachable},
		"a conflict":             {409, `{"error":{"type":"invalid_request_error","code":"resource_already_exists"}}`, false, CallUnreachable},
		"a 408":                  {408, `{}`, false, CallUnreachable},
		"a refusal":              {400, `{"error":{"type":"invalid_request_error","code":"amount_too_small"}}`, false, CallRefused},
		"parameters of a key":    {400, `{"error":{"type":"idempotency_error"}}`, false, CallRefused},
		"a key not taken":        {401, `{"error":{"type":"invalid_request_error"}}`, false, CallCredentials},
		"a 500":                  {500, `{"error":{"type":"api_error"}}`, false, CallUnreachable},
		"a 503":                  {503, `not json`, false, CallUnreachable},
		"a success not readable": {200, `{"id":"cs_test_x"`, false, CallRefused},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := NewClient(Lookup("stripe"), nil, egress.MustParse("127.0.0.1"), WithTestEndpoint(strings.TrimPrefix(srv.URL, "http://")))
			_, err := stripe{}.CreatePaymentLink(context.Background(), Credentials{APIKey: Secret("not-a-key")}, c,
				NewLink{Token: strings.Repeat("a", 32), AmountMinor: 100, Currency: "EUR", SuccessURL: publicURL + PaymentDonePath, CancelURL: publicURL + PaymentCancelledPath})
			ce, ok := IsCall(err)
			if !ok || ce.NotExecuted != tc.notExecuted || ce.OutcomeUnknown == tc.notExecuted || ce.Kind != tc.kind {
				t.Fatalf("err = %+v", ce)
			}
		})
	}
}

// A 409 for the key in use by another request: nothing ran under it, so the call is asked again later
// under the same key, and never a new one. While an earlier call under the key is still unknown, the
// 409 resolves nothing.
func TestAKeyInUseIsAskedAgainUnderTheSameKey(t *testing.T) {
	ctx := context.Background()
	t.Run("alone", func(t *testing.T) {
		p := newPay(t, publicURL)
		record := p.invoice(10, "EUR")
		req := p.direct(record, "op-inuse-0001")
		p.fake.Fail(testprovider.KeyInUse().On(http.MethodPost, "/v1/checkout/sessions"))
		_, err := p.svc.OpenPaymentLink(ctx, req)
		var pe *paylink.Error
		if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationProviderUnreachable || !pe.Transient || pe.Wait != 0 {
			t.Fatalf("err = %v", err)
		}
		if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE sends = 1 AND clean_sends = 1 AND unknown_since IS NULL AND first_sent_at IS NULL`); n != 1 {
			t.Fatal("the 409 left the key's outcome unknown")
		}
		link, err := p.svc.OpenPaymentLink(ctx, req)
		if err != nil || link.ObjectID == "" {
			t.Fatalf("link = %+v, %v", link, err)
		}
		posts := p.posts()
		if len(posts) != 2 || posts[1].IdempotencyKey != posts[0].IdempotencyKey || len(p.fake.Sessions()) != 1 ||
			len(p.fake.RequestsTo(http.MethodGet, "/v1/checkout/sessions")) != 0 {
			t.Fatalf("calls = %+v", posts)
		}
	})
	t.Run("after an unknown outcome", func(t *testing.T) {
		p := newPay(t, publicURL)
		record := p.invoice(10, "EUR")
		req := p.direct(record, "op-inuse-0002")
		p.fake.Fail(testprovider.Timeout().On(http.MethodPost, "/v1/checkout/sessions"))
		short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		_, _ = p.svc.OpenPaymentLink(short, req)
		cancel()
		p.fake.Fail(testprovider.KeyInUse().On(http.MethodPost, "/v1/checkout/sessions"))
		_, err := p.svc.OpenPaymentLink(ctx, req)
		var pe *paylink.Error
		if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationLinkLookupPending {
			t.Fatalf("err = %v", err)
		}
		if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE outcome IS NULL AND sends = 2 AND clean_sends = 1 AND unknown_since IS NOT NULL`); n != 1 {
			t.Fatal("the 409 resolved an earlier call that may have run")
		}
		if keys := p.keysUsed("/v1/checkout/sessions"); len(keys) != 1 {
			t.Fatalf("keys = %v", keys)
		}
	})
}

// The call that makes a link is sent once. Go's transport sends a request with an Idempotency-Key
// again by itself when a reused connection breaks before the answer, and returns the second copy's
// error: a failed dial that said nothing left, though the first copy reached the provider.
func TestACallIsNeverSentTwiceByTheHTTPClient(t *testing.T) {
	const account = `{"object":"account","id":"acct_Fake0123"}`
	session := `{"id":"cs_test_abc","object":"checkout.session","url":"https://checkout.stripe.com/c/pay/cs_test_abc"}`
	nl := NewLink{Token: strings.Repeat("a", 32), AmountMinor: 100, Currency: "EUR", SuccessURL: publicURL + PaymentDonePath, CancelURL: publicURL + PaymentCancelledPath}
	answer := func(w io.Writer, body string) {
		_, _ = fmt.Fprintf(w, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	}
	// serve answers a GET on the first connection, then reads the POST that follows on it, runs it, and
	// drops the connection with no answer. Any later connection gets the POST's answer, if listening
	// goes on.
	serve := func(ln net.Listener, keepListening bool, posts *atomic.Int32) {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		br := bufio.NewReader(conn)
		if req, err := http.ReadRequest(br); err == nil {
			_, _ = io.Copy(io.Discard, req.Body)
			answer(conn, account)
		}
		if req, err := http.ReadRequest(br); err == nil {
			_, _ = io.Copy(io.Discard, req.Body)
			posts.Add(1)
		}
		if !keepListening {
			_ = ln.Close()
		}
		_ = conn.Close()
		for keepListening {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			br := bufio.NewReader(conn)
			if req, err := http.ReadRequest(br); err == nil {
				_, _ = io.Copy(io.Discard, req.Body)
				posts.Add(1)
				answer(conn, session)
			}
			_ = conn.Close()
		}
	}
	for name, keep := range map[string]bool{"nothing listens any more": false, "the provider answers again": true} {
		t.Run(name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			var posts atomic.Int32
			go serve(ln, keep, &posts)
			c := NewClient(Lookup("stripe"), nil, egress.MustParse("127.0.0.1"), WithTestEndpoint(ln.Addr().String()))
			creds := Credentials{APIKey: Secret("not-a-key")}
			if _, err := (stripe{}).AccountID(context.Background(), creds, c); err != nil {
				t.Fatal(err)
			}
			_, err = stripe{}.CreatePaymentLink(context.Background(), creds, c, nl)
			ce, ok := IsCall(err)
			if !ok || !ce.OutcomeUnknown || ce.NotExecuted {
				t.Fatalf("err = %v (%+v): a call that reached the provider was taken for one that never left", err, ce)
			}
			if n := posts.Load(); n != 1 {
				t.Fatalf("the call was sent %d times", n)
			}
		})
	}
	// A call that never got a connection never left.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	c := NewClient(Lookup("stripe"), nil, egress.MustParse("127.0.0.1"), WithTestEndpoint(addr))
	_, err = stripe{}.CreatePaymentLink(context.Background(), Credentials{APIKey: Secret("not-a-key")}, c, nl)
	if ce, ok := IsCall(err); !ok || !ce.NotExecuted || ce.OutcomeUnknown {
		t.Fatalf("err = %v", err)
	}
}

// The provider is unreachable for hours, the list and the account read included: every failure stays on
// the reconciliation's schedule and uses up none of the entry's tries, so the reconciliation is never
// ended early with integration_provider_unreachable; it ends at its own bound with
// integration_outcome_unknown, which an admin can confirm.
func TestRepeatedFailedReadsNeverEndTheReconciliationEarly(t *testing.T) {
	for name, shape := range linkShapes {
		t.Run(name, func(t *testing.T) {
			p := newPay(t, shape.public)
			p.unknownOutcome(shape, "op-down-0002")
			p.fake.Fail(testprovider.Unavailable().Forever().On(http.MethodGet, shape.path))
			p.fake.Fail(testprovider.Unavailable().Times(3).On(http.MethodGet, "/v1/account"))
			reads := func() int { return len(p.fake.RequestsTo(http.MethodGet, "/v1/account")) }
			for i := range 10 {
				before := reads()
				p.later(31 * time.Minute)
				p.waitFor("another try", func() bool {
					es := p.entries()
					return reads() > before && es[0].state == "pending" && es[0].code == string(apierr.CodeIntegrationLinkLookupPending)
				})
				if es := p.entries(); es[0].attempts != 0 {
					t.Fatalf("try %d used up the entry's tries: %+v", i, es[0])
				}
			}
			p.later(time.Hour)
			e := p.settled(apps.StateFailed)
			if e.code != string(apierr.CodeIntegrationOutcomeUnknown) {
				t.Fatalf("entry = %+v", e)
			}
			if keys := p.keysUsed(shape.path); len(keys) != 1 {
				t.Fatalf("keys = %v", keys)
			}
			p.fake.ClearFaults()
			expect(t, p.owner.do(http.MethodPost, p.confirmPath(e.id, fmt.Sprintf("%d/confirm-no-link", e.id)), map[string]any{}), http.StatusNoContent)
			p.later(time.Hour)
			p.settled(apps.StateDone)
			if p.objects(shape.path) != 1 {
				t.Fatalf("%d objects", p.objects(shape.path))
			}
		})
	}
}

// The key's window is checked again after the write-ahead: a try that passed the check just before the
// window closed and reached the call after it never sends under a key the provider may have forgotten.
func TestAKeyPastItsWindowAfterTheWriteAheadIsNotSent(t *testing.T) {
	for name, shape := range linkShapes {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			p := newPay(t, shape.public)
			record := p.invoice(10, "EUR")
			req := p.direct(record, "op-window-0001")
			p.fake.Fail(testprovider.ServerError().KeptUnderKey().On(http.MethodPost, shape.path))
			if _, err := p.svc.OpenPaymentLink(ctx, req); err == nil {
				t.Fatal("the 5xx was no error")
			}
			p.later(stripeKeyKept - time.Minute)
			p.svc.afterWriteAhead = func() error { p.later(2 * time.Minute); return nil }
			// Still unknown (the wait for it is shorter than Stripe's window, so it has ended): not sent.
			if _, err := p.svc.OpenPaymentLink(ctx, req); !hasCode(err, apierr.CodeIntegrationOutcomeUnknown) {
				t.Fatalf("err = %v", err)
			}
			p.svc.afterWriteAhead = nil
			if n := len(p.fake.RequestsTo(http.MethodPost, shape.path)); n != 1 {
				t.Fatalf("%d calls, want the first only", n)
			}
		})
	}
}

// The key's window bounds the call itself: a call that is about to leave as the window closes and is
// delayed past it never reaches the provider.
func TestAKeyWhoseWindowClosesAsTheCallLeavesIsNotSent(t *testing.T) {
	ctx := context.Background()
	shape := linkShapes["a checkout session"]
	p := newPay(t, shape.public)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-window-0002")
	p.fake.Fail(testprovider.ServerError().KeptUnderKey().On(http.MethodPost, shape.path))
	if _, err := p.svc.OpenPaymentLink(ctx, req); err == nil {
		t.Fatal("the 5xx was no error")
	}
	p.later(stripeKeyKept - 300*time.Millisecond)
	p.svc.beforeCall = func() { time.Sleep(600 * time.Millisecond) }
	if _, err := p.svc.OpenPaymentLink(ctx, req); err == nil {
		t.Fatal("a call past the key's window answered")
	}
	p.svc.beforeCall = nil
	if n := len(p.fake.RequestsTo(http.MethodPost, shape.path)); n != 1 {
		t.Fatalf("%d calls, want the first only", n)
	}
}

// A crash between the write-ahead and the call: nothing left, and the key's outcome reads unknown all
// the same. The next try finds nothing and asks again under the same key, which makes the one link.
func TestACrashBetweenTheWriteAheadAndTheCallMakesOneLink(t *testing.T) {
	for name, shape := range linkShapes {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			p := newPay(t, shape.public)
			record := p.invoice(10, "EUR")
			req := p.direct(record, "op-crash-0001")
			crash := errors.New("crash")
			p.svc.afterWriteAhead = func() error { return crash }
			if _, err := p.svc.OpenPaymentLink(ctx, req); !errors.Is(err, crash) {
				t.Fatalf("err = %v", err)
			}
			p.svc.afterWriteAhead = nil
			if n := len(p.fake.RequestsTo(http.MethodPost, shape.path)); n != 0 {
				t.Fatalf("%d calls", n)
			}
			if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE sends = 1 AND clean_sends = 0 AND outcome IS NULL`); n != 1 {
				t.Fatal("the write-ahead was not recorded")
			}
			link, err := p.svc.OpenPaymentLink(ctx, req)
			if err != nil || link.ObjectID != p.objectID(shape.path) {
				t.Fatalf("link = %+v, %v", link, err)
			}
			if n := len(p.fake.RequestsTo(http.MethodPost, shape.path)); n != 1 || p.objects(shape.path) != 1 {
				t.Fatalf("%d calls, %d objects", n, p.objects(shape.path))
			}
		})
	}
}

// A crash after the call made the object and before anything of its answer was noted: the next try
// finds the object by the key's token, even once the key is past the provider's window, and makes
// nothing.
func TestACrashAfterTheCallBeforeItsAnswerIsNotedMakesOneLink(t *testing.T) {
	for name, shape := range linkShapes {
		for _, aged := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, key forgotten %v", name, aged), func(t *testing.T) {
				ctx := context.Background()
				p := newPay(t, shape.public)
				record := p.invoice(10, "EUR")
				req := p.direct(record, "op-crash-0002")
				crash := errors.New("crash")
				p.svc.afterCall = func() error { return crash }
				if _, err := p.svc.OpenPaymentLink(ctx, req); !errors.Is(err, crash) {
					t.Fatalf("err = %v", err)
				}
				p.svc.afterCall = nil
				if aged {
					p.exec(`UPDATE integration_ref_attempts SET first_sent_at = ?`, p.svc.store.stamp(time.Now().Add(-25*time.Hour)))
				}
				link, err := p.svc.OpenPaymentLink(ctx, req)
				if err != nil || link.ObjectID != p.objectID(shape.path) {
					t.Fatalf("link = %+v, %v", link, err)
				}
				if n := len(p.fake.RequestsTo(http.MethodPost, shape.path)); n != 1 || p.objects(shape.path) != 1 {
					t.Fatalf("%d calls, %d objects", n, p.objects(shape.path))
				}
			})
		}
	}
}

// A link saved on an earlier try is given again only while a payer can use it: its state is read at
// the provider first. An expired or paid one is not given, and nothing else is made; a read that fails
// gives nothing and makes nothing.
func TestASavedLinkIsGivenAgainOnlyWhileAPayerCanUseIt(t *testing.T) {
	for name, end := range map[string]func(p *pay, session string) error{
		"expired": func(p *pay, session string) error { _, err := p.fake.Expire(session); return err },
		"paid":    func(p *pay, session string) error { _, err := p.fake.Pay(session); return err },
	} {
		t.Run(name, func(t *testing.T) {
			p := newPay(t, publicURL)
			record := p.invoice(10, "EUR")
			req := p.direct(record, "op-saved-0001")
			ctx := context.Background()
			first, err := p.svc.OpenPaymentLink(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			reads := len(p.fake.RequestsTo(http.MethodGet, "/v1/checkout/sessions/"+first.ObjectID))
			if again, err := p.svc.OpenPaymentLink(ctx, req); err != nil || again != first {
				t.Fatalf("again = %+v, %v", again, err)
			}
			if n := len(p.fake.RequestsTo(http.MethodGet, "/v1/checkout/sessions/"+first.ObjectID)); n != reads+1 {
				t.Fatalf("the saved link was given with %d reads of its state, want %d", n, reads+1)
			}

			// The read fails: nothing is given, and nothing made.
			p.fake.Fail(testprovider.ServerError().On(http.MethodGet, "/v1/checkout/sessions/"+first.ObjectID))
			link, err := p.svc.OpenPaymentLink(ctx, req)
			var pe *paylink.Error
			if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationProviderUnreachable || !pe.Transient || link.URL != "" {
				t.Fatalf("with the read failing: %+v, %v", link, err)
			}

			// A key of another account (same mode) never reads it: nothing given, nothing read.
			p.fake.SetAccount(otherAccount)
			reads = len(p.fake.RequestsTo(http.MethodGet, "/v1/checkout/sessions/"+first.ObjectID))
			link, err = p.svc.OpenPaymentLink(ctx, req)
			if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationAccountChanged || link.URL != "" {
				t.Fatalf("under another account: %+v, %v", link, err)
			}
			if n := len(p.fake.RequestsTo(http.MethodGet, "/v1/checkout/sessions/"+first.ObjectID)); n != reads {
				t.Fatal("another account's key read the saved session")
			}
			p.fake.SetAccount(testprovider.TestAccountID)

			if err := end(p, first.ObjectID); err != nil {
				t.Fatal(err)
			}
			link, err = p.svc.OpenPaymentLink(ctx, req)
			if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationLinkAlreadyPaid || link.URL != "" {
				t.Fatalf("the %s link: %+v, %v", name, link, err)
			}
			if n := len(p.posts()); n != 1 || len(p.fake.Sessions()) != 1 {
				t.Fatalf("%d calls, %d sessions: something else was made", n, len(p.fake.Sessions()))
			}
		})
	}
}

// An admin looks for an unresolved payment at the provider by each attempt's reference, so the list
// gives every attempt (not only the current one) with what to search and where: the reference, the kind
// of object (which says the field it is in), any object seen and whether it was verified, the account and
// mode it was made under, and when it was first sent.
func TestTheUnresolvedListGivesEveryAttemptsReferenceAndWhereToLookForIt(t *testing.T) {
	for name, shape := range linkShapes {
		t.Run(name, func(t *testing.T) {
			p := newPay(t, shape.public)
			p.unknownOutcome(shape, "op-attempts-0001")
			p.later(time.Minute)
			p.looked(shape.path, 1)
			p.later(7 * time.Hour)
			e := p.settled(apps.StateFailed)
			kind := AttemptPaymentLink
			if shape.public != "" {
				kind = AttemptCheckoutSession
			}
			var first, mode string
			if err := p.db.QueryRow(`SELECT a.token, r.account_mode FROM integration_ref_attempts a JOIN integration_refs r ON r.id = a.ref_id WHERE a.seq = 0`).Scan(&first, &mode); err != nil {
				t.Fatal(err)
			}

			list := func() PaymentLinkOutcome {
				t.Helper()
				rec := p.owner.do(http.MethodGet, p.confirmPath(e.id, "unresolved"), nil)
				expect(t, rec, http.StatusOK)
				got := decode[PaymentLinkOutcomes](t, rec).PaymentLinks
				if len(got) != 1 {
					t.Fatalf("list = %+v", got)
				}
				return got[0]
			}

			got := list().Attempts
			if len(got) != 1 {
				t.Fatalf("attempts = %+v", got)
			}
			if a := got[0]; a.Reference != first || a.Kind != kind || a.AccountID != testprovider.TestAccountID || a.Mode != mode || mode == "" ||
				a.FirstSentAt == "" || a.ObjectID != "" || a.ObjectVerified {
				t.Fatalf("attempt = %+v, want reference %q of a %s under %s (%s)", a, first, kind, testprovider.TestAccountID, mode)
			}

			// The reference moved to a second key after an abandoned one, and the first saw an object that
			// couldn't be checked: both are listed, the oldest first, each with its own reference.
			second := strings.Repeat("c", 16) + strings.Repeat("d", 16)
			p.exec(`UPDATE integration_ref_attempts SET outcome = 'abandoned', object_id = 'obj_seen', object_verified = 0 WHERE seq = 0`)
			p.exec(`INSERT INTO integration_ref_attempts (ref_id, seq, token, sends, clean_sends, first_sent_at, unknown_since)
				SELECT id, 1, ?, 1, 0, '2026-11-26T12:00:00.000Z', '2026-11-26T12:00:00.000Z' FROM integration_refs`, second)
			p.exec(`UPDATE integration_refs SET key_seq = 1`)
			p.exec(`UPDATE integration_ref_attempts SET object_id = 'obj_checked', object_verified = 1 WHERE seq = 1`)
			got = list().Attempts
			if len(got) != 2 || got[0].Reference != first || got[1].Reference != second ||
				got[0].ObjectID != "obj_seen" || got[0].ObjectVerified || got[1].ObjectID != "obj_checked" || !got[1].ObjectVerified ||
				got[1].FirstSentAt != "2026-11-26T12:00:00.000Z" || got[1].Kind != kind || got[1].AccountID != got[0].AccountID {
				t.Fatalf("attempts = %+v", got)
			}
		})
	}
}

// The list never carries what is sealed or sent to the provider: only the fields of an attempt that are
// named, and nothing of the integration's keys, whatever the response holds.
func TestTheUnresolvedListHoldsNoKeysOrOtherFields(t *testing.T) {
	shape := linkShapes["a checkout session"]
	p := newPay(t, shape.public)
	p.unknownOutcome(shape, "op-attempts-0002")
	p.later(time.Minute)
	p.looked(shape.path, 1)
	p.later(7 * time.Hour)
	e := p.settled(apps.StateFailed)

	rec := p.owner.do(http.MethodGet, p.confirmPath(e.id, "unresolved"), nil)
	expect(t, rec, http.StatusOK)
	for _, leaked := range []string{p.fake.APIKey(), stripeHook, "whsec_", "api_key", "secret", "success_url", "cancel_url", "idempotency"} {
		if strings.Contains(rec.Body.String(), leaked) {
			t.Fatalf("the list holds %q: %s", leaked, rec.Body)
		}
	}
	var raw struct {
		Links []struct {
			Attempts []map[string]any `json:"attempts"`
		} `json:"payment_links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil || len(raw.Links) != 1 || len(raw.Links[0].Attempts) != 1 {
		t.Fatalf("list = %s, %v", rec.Body, err)
	}
	var keys []string
	for k := range raw.Links[0].Attempts[0] {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{"account_id", "first_sent_at", "kind", "mode", "object_id", "object_verified", "reference"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("an attempt's fields = %v, want %v", keys, want)
	}
}

// An admin of another workspace, who is not a member of this one, gets neither this workspace's payments
// nor any attempt's reference: not by this workspace's route, and not by naming this integration's number
// in their own.
func TestAnotherWorkspacesAdminGetsNoAttemptsOfUnresolvedPayments(t *testing.T) {
	shape := linkShapes["a checkout session"]
	p := newPay(t, shape.public)
	p.unknownOutcome(shape, "op-attempts-0003")
	p.later(time.Minute)
	p.looked(shape.path, 1)
	p.later(7 * time.Hour)
	e := p.settled(apps.StateFailed)
	var reference string
	if err := p.db.QueryRow(`SELECT token FROM integration_ref_attempts WHERE seq = 0`).Scan(&reference); err != nil {
		t.Fatal(err)
	}
	// The same call, for the workspace's own admin, does hold it: the refusals below are about who asks.
	own := p.owner.do(http.MethodGet, p.confirmPath(e.id, "unresolved"), nil)
	expect(t, own, http.StatusOK)
	if !strings.Contains(own.Body.String(), reference) {
		t.Fatalf("the admin's own list lacks the reference: %s", own.Body)
	}

	_, other := p.otherWorkspace(p.owner)
	stranger := p.invite(p.owner, other, "second-admin@example.com", "admin")
	for name, path := range map[string]string{
		"this workspace's route":            p.confirmPath(e.id, "unresolved"),
		"their own, naming its integration": wsPath(other, fmt.Sprintf("integrations/%d/payment-links/unresolved", p.in.ID)),
	} {
		rec := stranger.do(http.MethodGet, path, nil)
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d: %s", name, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), reference) || strings.Contains(rec.Body.String(), "attempts") {
			t.Fatalf("%s: the answer holds attempts: %s", name, rec.Body)
		}
	}
}
