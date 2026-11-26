package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/integrations"
	"taskto.ai/taskto/internal/integrations/testprovider"
	"taskto.ai/taskto/internal/serverkey"
)

// The end-to-end and isolation scenarios of payments, through the real server: its whole mux, SQLite,
// the apps' outbox worker, the integrations' event worker, a clock the test moves and a fake Stripe
// that listens on a socket. A person's request, the provider's call, the signed webhook and the
// worker's apply all go the way they go in production; the only thing replaced is where the provider
// is (integrations.WithTestEndpoint), which no configuration can do.
//
// The people are the server tests' stack: alice administers workspace one, bob edits its Team space,
// carol has a Secret space of her own, and workspace two is alice's alone.

// payInvoiceApp is an invoicing app: sending an invoice asks for a payment link of its total in
// euros, a payment marks it paid, and the refund of all of it marks it refunded.
const payInvoiceApp = `{
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

// The credentials of the integrations the scenarios connect. They are obviously fake and made of parts,
// so no file holds a whole key-shaped string for a secret scanner to find.
var (
	keyA  = testprovider.TestAPIKey
	keyB  = "sk_" + "test_" + strings.Repeat("Rota", 6)
	keyLv = "sk_" + "live_" + strings.Repeat("Fake", 6)

	hookA  = testprovider.TestWebhookSecret
	hookB  = testprovider.OtherWebhookSecret
	hookLv = "whsec_" + strings.Repeat("Live", 6)
	hookW2 = "whsec_" + strings.Repeat("Twoo", 6)
)

// lockedBuffer is a log sink the workers write to while a test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// answers keeps every answer the API gave in-process (a status, a header and a body), so a test can
// search them all for what must never be in one.
type answers struct {
	mu   sync.Mutex
	list []string
}

func (a *answers) add(s string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.list = append(a.list, s)
}

func (a *answers) all() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.list)
}

// recording serves the handler and keeps what it answered.
type recording struct {
	next http.Handler
	into *answers
}

func (r recording) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	rec := httptest.NewRecorder()
	r.next.ServeHTTP(rec, req)
	res := rec.Result()
	var head bytes.Buffer
	_ = res.Header.Write(&head)
	r.into.add(fmt.Sprintf("%s %s -> %d\n%s\n%s", req.Method, req.URL.Path, rec.Code, head.String(), rec.Body.String()))
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

// movableClock is the real time plus an offset the test moves (Advance). It runs on, because the
// workers' own waits do, and so it never falls behind what the database stamps as the real time. It is
// the clock of the fake provider, the integrations, the apps and the senders alike.
type movableClock struct{ offset atomic.Int64 }

func (c *movableClock) Now() time.Time { return time.Now().Add(time.Duration(c.offset.Load())) }

func (c *movableClock) Advance(d time.Duration) { c.offset.Add(int64(d)) }

// shop is an integration the admin connected.
type shop struct {
	id     int64
	ws     int64
	hook   string // the public id of its webhook route
	secret string // its webhook secret
	key    string // its API key
}

// payStack is the server with the fake Stripe, the workers running and the clock.
type payStack struct {
	*stack
	fake  *testprovider.Stripe
	clock *movableClock
	svc   *integrations.Service
	apps  *apps.Service
	base  string // the server's address, as a provider reaches it
	logs  *lockedBuffer
	seen  *answers
	ops   int
	// public is the server's public URL, "" for none.
	public string
}

func newPayStack(t *testing.T) *payStack { return newPayStackAt(t, hooksOrigin) }

// newPayStackAt is newPayStack for a server whose public URL is publicURL: "" for one with none, whose
// payment links are hosted by the provider.
func newPayStackAt(t *testing.T, publicURL string) *payStack {
	t.Helper()
	p := &payStack{logs: &lockedBuffer{}, seen: &answers{}, public: publicURL}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(p.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// One clock moves the fake provider's, the integrations', the apps' and the senders' alike.
	p.clock = &movableClock{}
	p.fake = testprovider.NewStripe(t, testprovider.WithClock(p.clock))
	p.stack = newStackDB(t, func(o *Options, sqlDB *sql.DB) {
		key, err := serverkey.Load(o.BackupDataDir)
		if err != nil {
			t.Fatal(err)
		}
		policy := egress.MustParse("127.0.0.1")
		p.svc = integrations.New(integrations.NewStore(sqlDB, key), integrations.Options{
			PublicURL: publicURL, Egress: &policy, Now: p.clock.Now,
			ClientOptions: []integrations.ClientOption{integrations.WithTestEndpoint(p.fake.Host()), integrations.WithoutConnectionReuse()},
		})
		o.Integrations = p.svc
		ready := o.AppsReady
		o.AppsReady = func(a *apps.Service) {
			ready(a)
			a.WithClock(p.clock.Now)
			p.apps = a
			// A verified payment event is applied to the app's record by the apps, as the integration.
			p.svc.UseRecords(a)
		}
	}, func(next http.Handler) http.Handler { return recording{next: next, into: p.seen} })
	p.base = strings.TrimSuffix(p.mcpURL, "/mcp")

	// The server's workers, as the command starts them, stopped before the database closes.
	ctx, cancel := context.WithCancel(context.Background())
	stopOutbox := p.apps.StartOutbox(ctx)
	stopEvents := p.svc.StartEvents(ctx)
	t.Cleanup(func() {
		cancel()
		stopEvents()
		stopOutbox()
	})
	return p
}

// eventually waits for cond, which polls.
func (p *payStack) eventually(what string, cond func() bool) {
	p.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	p.t.Fatalf("never: %s\nthe apps' entries: %s\nthe log: %.1500s", what, p.entryDump(), p.logs.String())
}

// entryDump says where the apps' entries stand, for the message of a test that gave up waiting.
func (p *payStack) entryDump() string {
	rows, err := p.db.Query(`SELECT id, state, attempts, coalesce(error_code, ''), coalesce(error_params, '') FROM app_effects ORDER BY id`)
	if err != nil {
		return err.Error()
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, attempts int64
		var state, code, params string
		if err := rows.Scan(&id, &state, &attempts, &code, &params); err != nil {
			return err.Error()
		}
		out = append(out, fmt.Sprintf("#%d %s attempts=%d code=%q %s", id, state, attempts, code, params))
	}
	return strings.Join(out, "; ")
}

// call makes a browser's request and decodes the answer into out, if it isn't nil.
func (p *payStack) call(cookie, method, path string, body, out any) int {
	p.t.Helper()
	rec := p.request(method, path, body, sameOrigin(cookie))
	if out != nil && rec.Code < 300 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			p.t.Fatalf("%s %s: %v in %s", method, path, err, rec.Body)
		}
	}
	return rec.Code
}

// mustCall is call for a request that must be answered with status.
func (p *payStack) mustCall(status int, cookie, method, path string, body, out any) {
	p.t.Helper()
	if got := p.call(cookie, method, path, body, out); got != status {
		rec := p.request(method, path, body, sameOrigin(cookie))
		p.t.Fatalf("%s %s = %d, want %d: %s", method, path, got, status, rec.Body)
	}
}

// code is the error code of a refused request.
func (p *payStack) code(cookie, method, path string, body any) (int, string) {
	p.t.Helper()
	rec := p.request(method, path, body, sameOrigin(cookie))
	return rec.Code, fmt.Sprint(decode(rec)["code"])
}

func (p *payStack) ws(ws int64, rest string) string {
	return fmt.Sprintf("/api/workspaces/%d/%s", ws, rest)
}

// connect has the admin connect a Stripe integration with the key and webhook secret.
func (p *payStack) connect(admin person, ws int64, name, key, secret string) *shop {
	p.t.Helper()
	var out struct {
		ID         int64  `json:"id"`
		WebhookURL string `json:"webhook_url"`
		Mode       string `json:"mode"`
	}
	p.mustCall(http.StatusCreated, admin.cookie, http.MethodPost, p.ws(ws, "integrations"), map[string]any{
		"provider": "stripe", "name": name, "secrets": map[string]string{"api_key": key, "webhook_secret": secret},
		"max_amount_minor": 100000, "currencies": []string{"EUR"},
	}, &out)
	if out.WebhookURL == "" && p.public == "" {
		// A server with no public URL shows no address; its route is there all the same, which a test
		// reaches by the id the database holds.
		return &shop{id: out.ID, ws: ws, hook: p.text(`SELECT public_id FROM integrations WHERE id = ?`, out.ID), secret: secret, key: key}
	}
	if !strings.HasPrefix(out.WebhookURL, hooksOrigin+integrations.HookPrefix) {
		p.t.Fatalf("webhook_url = %q", out.WebhookURL)
	}
	return &shop{id: out.ID, ws: ws, hook: strings.TrimPrefix(out.WebhookURL, hooksOrigin+integrations.HookPrefix), secret: secret, key: key}
}

// invoicing is an app in a space: its ids and the approval of its contract.
type invoicing struct {
	space, app, version int64
	approval            int64
}

func (p *payStack) appPath(space int64, rest string) string {
	return fmt.Sprintf("/api/workspaces/%d/spaces/%d/apps%s", p.one, space, rest)
}

type rawDefinition string

func (r rawDefinition) MarshalJSON() ([]byte, error) { return []byte(r), nil }

// draft has who makes the app in a space of workspace one, as a draft.
func (p *payStack) draft(who person, space int64, name, definition string) invoicing {
	p.t.Helper()
	var a struct {
		ID      int64 `json:"id"`
		Version int64 `json:"version"`
	}
	p.mustCall(http.StatusCreated, who.cookie, http.MethodPost, p.appPath(space, ""), map[string]any{"name": name, "definition": rawDefinition(definition)}, &a)
	return invoicing{space: space, app: a.ID, version: a.Version}
}

// askApproval has who submit the draft's payments contract for approval.
func (p *payStack) askApproval(who person, inv *invoicing) {
	p.t.Helper()
	var st apps.IntegrationStatus
	p.mustCall(http.StatusOK, who.cookie, http.MethodGet, p.appPath(inv.space, fmt.Sprintf("/%d/integrations", inv.app)), nil, &st)
	if len(st.Slots) != 1 {
		p.t.Fatalf("slots = %+v", st.Slots)
	}
	var ap apps.IntegrationApproval
	p.mustCall(http.StatusOK, who.cookie, http.MethodPost, p.appPath(inv.space, fmt.Sprintf("/%d/integrations/payments/request", inv.app)),
		map[string]any{"contract_hash": st.Slots[0].ContractHash}, &ap)
	inv.approval = ap.ID
}

// allowSpaces has the admin allow the integration in exactly these spaces.
func (p *payStack) allowSpaces(admin person, sh *shop, spaces ...int64) {
	p.t.Helper()
	p.mustCall(http.StatusOK, admin.cookie, http.MethodPut, p.ws(sh.ws, fmt.Sprintf("integrations/%d/spaces", sh.id)), map[string]any{"spaces": spaces}, nil)
}

// approve has the admin approve the approval for the integration, which must be allowed in the app's
// space already.
func (p *payStack) approve(admin person, sh *shop, inv invoicing) {
	p.t.Helper()
	p.mustCall(http.StatusOK, admin.cookie, http.MethodPost, p.ws(sh.ws, fmt.Sprintf("integration-approvals/%d/approve", inv.approval)),
		map[string]any{"integration_id": sh.id}, nil)
}

// publish has who publish the draft.
func (p *payStack) publish(who person, inv invoicing) {
	p.t.Helper()
	p.mustCall(http.StatusOK, who.cookie, http.MethodPost, p.appPath(inv.space, fmt.Sprintf("/%d/publish", inv.app)),
		map[string]any{"version": inv.version, "acknowledge": []any{}}, nil)
}

// build is the whole way to a published app: drafted by who, its contract requested, approved by the
// admin for the integration, published.
func (p *payStack) build(who, admin person, sh *shop, space int64, name, definition string) invoicing {
	p.t.Helper()
	inv := p.draft(who, space, name, definition)
	p.askApproval(who, &inv)
	p.approve(admin, sh, inv)
	p.publish(who, inv)
	return inv
}

// act runs an action of the app as who, and returns the status.
func (p *payStack) act(who person, inv invoicing, operation, name string, ctx map[string]any) int {
	p.t.Helper()
	return p.call(who.cookie, http.MethodPost, p.appPath(inv.space, fmt.Sprintf("/%d/actions", inv.app)), map[string]any{
		"name": name, "surfaceId": apps.SurfaceID(inv.app), "sourceComponentId": "add", "context": ctx, "operation": operation,
	}, nil)
}

func (p *payStack) records(who person, inv invoicing) []apps.Record {
	p.t.Helper()
	var out apps.Records
	p.mustCall(http.StatusOK, who.cookie, http.MethodGet, p.appPath(inv.space, fmt.Sprintf("/%d/records", inv.app)), nil, &out)
	return out.Records
}

func (p *payStack) record(who person, inv invoicing, id int64) apps.Record {
	p.t.Helper()
	for _, r := range p.records(who, inv) {
		if r.ID == id {
			return r
		}
	}
	p.t.Fatalf("no record %d", id)
	return apps.Record{}
}

func (p *payStack) history(who person, inv invoicing, id int64) apps.History {
	p.t.Helper()
	var out apps.History
	p.mustCall(http.StatusOK, who.cookie, http.MethodGet, p.appPath(inv.space, fmt.Sprintf("/%d/records/%d/history", inv.app, id)), nil, &out)
	return out
}

// draftInvoice adds an invoice of the total (a JSON number) in euros and returns its record.
func (p *payStack) draftInvoice(who person, inv invoicing, number string, total any) int64 {
	p.t.Helper()
	if got := p.act(who, inv, "", "add", map[string]any{"number": number, "customer": "Acme", "total": total, "currency": "EUR", "status": "draft"}); got != http.StatusOK {
		p.t.Fatalf("add = %d", got)
	}
	for _, r := range p.records(who, inv) {
		if r.Data["number"] == number {
			return r.ID
		}
	}
	p.t.Fatalf("no invoice %s", number)
	return 0
}

// send has who send the invoice, and returns the status of the request.
func (p *payStack) send(who person, inv invoicing, record int64) int {
	p.t.Helper()
	p.ops++
	return p.act(who, inv, fmt.Sprintf("op-send-%04d", p.ops), "send", map[string]any{"id": record, "status": "sent"})
}

// sentInvoice adds an invoice and sends it, and waits for the payment link in the record: the Stripe
// Checkout Session's id.
func (p *payStack) sentInvoice(who person, inv invoicing, number string, total any) (record int64, session string) {
	p.t.Helper()
	record = p.draftInvoice(who, inv, number, total)
	if got := p.send(who, inv, record); got != http.StatusOK {
		p.t.Fatalf("send = %d", got)
	}
	p.eventually("the payment link is in the record "+number, func() bool {
		session, _ = p.record(who, inv, record).Data["payment_id"].(string)
		return session != ""
	})
	return record, session
}

// hookAnswers are the recorded answers of the webhook routes, which the listener serves to the provider,
// as a control for a check of what the server answered: a check that finds nothing in them proves
// something only if they were recorded at all.
func (p *payStack) hookAnswers() []string {
	var out []string
	for _, a := range p.seen.all() {
		if strings.HasPrefix(a, http.MethodPost+" "+integrations.HookPrefix) {
			out = append(out, a)
		}
	}
	return out
}

// sender delivers signed events to the integration's route, over HTTP, as its provider does.
func (p *payStack) sender(sh *shop) *testprovider.Sender {
	return testprovider.NewSender(p.base+integrations.HookPrefix+sh.hook,
		testprovider.WithSigningSecret(sh.secret), testprovider.WithSenderClock(p.clock))
}

// deliver sends the events, each of which must be answered 200 as Stripe needs.
func (p *payStack) deliver(sh *shop, evs []testprovider.Event, opts ...testprovider.SignOption) {
	p.t.Helper()
	ds, err := p.sender(sh).SendAll(context.Background(), evs, opts...)
	if err != nil {
		p.t.Fatal(err)
	}
	for _, d := range ds {
		if d.Status != http.StatusOK {
			p.t.Fatalf("the route answered %d to %s %s", d.Status, d.Type, d.Response)
		}
	}
}

// eventsOf lists the integration's events, newest first, as the admin sees them.
func (p *payStack) eventsOf(admin person, sh *shop) []integrations.EventView {
	p.t.Helper()
	var out integrations.EventList
	p.mustCall(http.StatusOK, admin.cookie, http.MethodGet, p.ws(sh.ws, fmt.Sprintf("integrations/%d/events", sh.id)), nil, &out)
	return out.Events
}

// settled waits for the integration to have n events, none still waiting for the worker, and returns
// them, newest first.
func (p *payStack) settled(admin person, sh *shop, n int) []integrations.EventView {
	p.t.Helper()
	var evs []integrations.EventView
	p.eventually(fmt.Sprintf("%d events of integration %d are done", n, sh.id), func() bool {
		evs = p.eventsOf(admin, sh)
		if len(evs) != n {
			return false
		}
		for _, e := range evs {
			if e.State == integrations.EventPending || e.State == integrations.EventRunning {
				return false
			}
		}
		return true
	})
	return evs
}

// completed is a payment's "checkout.session.completed", made by hand for what the fake would not
// make: another amount, another reference, another mode.
func (p *payStack) completed(session string, mutate func(obj map[string]any)) testprovider.Event {
	p.t.Helper()
	s, ok := p.fake.Session(session)
	if !ok {
		p.t.Fatalf("no session %s", session)
	}
	raw, _ := json.Marshal(s)
	var obj map[string]any
	_ = json.Unmarshal(raw, &obj)
	obj["status"], obj["payment_status"] = testprovider.SessionComplete, testprovider.PaymentPaid
	if obj["payment_intent"] == nil {
		obj["payment_intent"] = "pi_" + strings.Repeat("a", 24)
	}
	if mutate != nil {
		mutate(obj)
	}
	return testprovider.NewEvent(testprovider.EventSessionCompleted, obj, p.clock.Now())
}

// dbHolds counts the cells of every table of the database that hold needle, as text.
func (p *payStack) dbHolds(needle string) int {
	p.t.Helper()
	rows, err := p.db.Query(`SELECT m.name, c.name FROM sqlite_master m, pragma_table_info(m.name) c
		WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%' AND m.name NOT LIKE '%_fts%'`)
	if err != nil {
		p.t.Fatal(err)
	}
	type cell struct{ table, column string }
	var cells []cell
	for rows.Next() {
		var c cell
		if err := rows.Scan(&c.table, &c.column); err != nil {
			p.t.Fatal(err)
		}
		cells = append(cells, c)
	}
	if err := rows.Err(); err != nil {
		p.t.Fatal(err)
	}
	_ = rows.Close()
	total := 0
	for _, c := range cells {
		var n int
		if err := p.db.QueryRow(fmt.Sprintf(`SELECT count(*) FROM "%s" WHERE CAST("%s" AS TEXT) LIKE ?`, c.table, c.column), "%"+needle+"%").Scan(&n); err != nil {
			p.t.Fatal(err)
		}
		total += n
	}
	return total
}

// text reads one value of the database as text.
func (p *payStack) text(q string, args ...any) string {
	p.t.Helper()
	var v sql.NullString
	if err := p.db.QueryRow(q, args...).Scan(&v); err != nil {
		p.t.Fatal(err)
	}
	return v.String
}

func (p *payStack) refRows(workspace int64) (n int) {
	return int(p.scalar(`SELECT count(*) FROM integration_refs r JOIN integrations i ON i.id = r.integration_id WHERE i.workspace_id = ?`, workspace))
}

func (p *payStack) sessionPosts() []testprovider.Request {
	return p.fake.RequestsTo(http.MethodPost, "/v1/checkout/sessions")
}

// invoicingStack is the common start: alice connects Stripe, bob builds the invoicing app in the Team
// space and has it approved, and publishes it.
type invoicingStack struct {
	*payStack
	in  *shop
	app invoicing
}

func newInvoicing(t *testing.T) *invoicingStack {
	t.Helper()
	p := newPayStack(t)
	in := p.connect(p.alice, p.one, "shop", keyA, hookA)
	p.allowSpaces(p.alice, in, p.team)
	app := p.build(p.bob, p.alice, in, p.team, "Invoices", payInvoiceApp)
	return &invoicingStack{payStack: p, in: in, app: app}
}

// ----- the whole way from "send" to "paid" -----

func TestAnInvoiceGoesFromSentToPaidThroughTheWholeServer(t *testing.T) {
	s := newInvoicing(t)

	// The admin connected the integration: the secrets are write-only, and the check works against
	// the (fake) provider.
	var got integrations.Integration
	s.mustCall(http.StatusOK, s.alice.cookie, http.MethodGet, s.ws(s.one, fmt.Sprintf("integrations/%d", s.in.id)), nil, &got)
	if got.Secrets["api_key"] != "set" || got.Secrets["webhook_secret"] != "set" || got.Mode != "test" {
		t.Fatalf("integration = %+v", got)
	}

	// Bob, who edits the Team space, sends an invoice.
	record, session := s.sentInvoice(s.bob, s.app, "A-1", 120.10)

	// One link was made, once: the call carried the reference's token as its idempotency key and as the
	// reference the provider echoes back, the amount in minor units, the server's own pages.
	posts := s.sessionPosts()
	if len(posts) != 1 {
		t.Fatalf("%d calls to the provider: %+v", len(posts), posts)
	}
	call := posts[0]
	token := call.Form.Get("client_reference_id")
	if len(token) != 32 || call.IdempotencyKey != token {
		t.Fatalf("the call's key %q and reference %q", call.IdempotencyKey, token)
	}
	if call.Form.Get("line_items[0][price_data][unit_amount]") != "12010" || call.Form.Get("line_items[0][price_data][currency]") != "eur" {
		t.Fatalf("the amount: %v", call.Form)
	}
	if call.Form.Get("success_url") != hooksOrigin+integrations.PaymentDonePath || call.Form.Get("cancel_url") != hooksOrigin+integrations.PaymentCancelledPath {
		t.Fatalf("the pages are the server's own: %q %q", call.Form.Get("success_url"), call.Form.Get("cancel_url"))
	}
	if call.APIVersion != integrations.StripeAPIVersion {
		t.Fatalf("the API version %q isn't the pinned one", call.APIVersion)
	}
	sessions := s.fake.Sessions()
	if len(sessions) != 1 || sessions[0].ID != session || sessions[0].URL == nil {
		t.Fatalf("sessions = %+v", sessions)
	}

	// The link is in the record, written by the integration, not by a person, and the invoice is as
	// the person left it: sent.
	rec := s.record(s.bob, s.app, record)
	if rec.Data["status"] != "sent" || rec.Data["pay_url"] != *sessions[0].URL || rec.Data["payment_id"] != session {
		t.Fatalf("record = %+v", rec.Data)
	}
	if rec.UpdatedBy == nil || rec.UpdatedBy.System == nil || rec.UpdatedBy.System.Kind != "integration" || rec.UpdatedBy.System.ID != s.in.id || rec.UpdatedBy.ID != 0 {
		t.Fatalf("the link was written by %+v", rec.UpdatedBy)
	}
	if rec.CreatedBy == nil || rec.CreatedBy.ID != s.bob.id {
		t.Fatalf("the invoice's creator changed: %+v", rec.CreatedBy)
	}
	// Doing the same send again, under the same operation, makes no second link.
	sendRevision := rec.Revision
	queued := s.scalar(`SELECT count(*) FROM app_effects`)
	if got := s.act(s.bob, s.app, fmt.Sprintf("op-send-%04d", s.ops), "send", map[string]any{"id": record, "status": "sent"}); got != http.StatusOK {
		t.Fatalf("the repeated send = %d", got)
	}
	// Nothing more was queued (the stored work, not a wait for something that might still come), and
	// nothing is left in flight that could still make a call.
	if n := s.scalar(`SELECT count(*) FROM app_effects`); n != queued {
		t.Fatalf("a repeated request queued %d more effects", n-queued)
	}
	s.eventually("the queue holds no work", func() bool {
		return s.scalar(`SELECT count(*) FROM app_effects WHERE state IN ('pending', 'running')`) == 0
	})
	if n := len(s.sessionPosts()); n != 1 {
		t.Fatalf("a repeated request made %d calls", n)
	}

	// The payer pays on the provider's page; the provider sends its events, signed, to the hook.
	evs, err := s.fake.Pay(session)
	if err != nil {
		t.Fatal(err)
	}
	s.deliver(s.in, evs)
	events := s.settled(s.alice, s.in, 1)
	if e := events[0]; e.Kind != integrations.EventPaymentSucceeded || e.State != integrations.EventApplied || e.AmountMinor != 12010 || e.Currency != "EUR" || e.Livemode {
		t.Fatalf("event = %+v", e)
	}

	// The invoice is paid, by the integration, with the day the payment happened: the day of the
	// delivered event's own time, not of the clock when this runs (which a midnight could move).
	var happened time.Time
	for _, e := range evs {
		if e.Type == testprovider.EventSessionCompleted {
			happened = time.Unix(e.Created, 0).UTC()
		}
	}
	if happened.IsZero() {
		t.Fatal("the payment's events have no completed session")
	}
	paid := s.record(s.bob, s.app, record)
	if paid.Data["status"] != "paid" || paid.Data["paid_at"] != happened.Format("2006-01-02") {
		t.Fatalf("record = %+v", paid.Data)
	}
	if paid.Revision != sendRevision+1 || paid.UpdatedBy == nil || paid.UpdatedBy.System == nil || paid.UpdatedBy.System.ID != s.in.id || paid.UpdatedAction != integrations.EventPaymentSucceeded {
		t.Fatalf("revision %d (was %d), updated by %+v as %q", paid.Revision, sendRevision, paid.UpdatedBy, paid.UpdatedAction)
	}
	// What the history says: the integration did it, as the normalized event, and the person's own
	// changes are still theirs.
	h := s.history(s.bob, s.app, record)
	if len(h.History) < 4 {
		t.Fatalf("history = %+v", h.History)
	}
	last := h.History[0]
	if last.Operation != "update" || last.Action != integrations.EventPaymentSucceeded || last.By == nil || last.By.System == nil ||
		last.By.System.Kind != "integration" || last.By.System.ID != s.in.id || last.By.ID != 0 || last.Data["status"] != "paid" {
		t.Fatalf("the last change = %+v by %+v (%+v)", last, last.By, last.By.System)
	}
	for _, v := range h.History[1:] {
		if v.By == nil || (v.By.ID != s.bob.id && (v.By.System == nil || v.Action == integrations.EventPaymentSucceeded)) {
			t.Fatalf("an earlier change was not bob's or the link's: %+v by %+v", v, v.By)
		}
	}

	// A replay changes nothing: the same deliveries again are answered 200, are stored once, and leave
	// the record, its revision and its history as they were.
	history := len(h.History)
	for range 2 {
		s.deliver(s.in, evs)
	}
	// Each delivery was answered after it was stored, so the stored state says what the replay did.
	if n := s.scalar(`SELECT count(*) FROM integration_events WHERE integration_id = ?`, s.in.id); n != 1 {
		t.Fatalf("a replay stored %d events", n)
	}
	if n := len(s.settled(s.alice, s.in, 1)); n != 1 {
		t.Fatalf("a replay left %d events", n)
	}
	s.eventually("the queue holds no work", func() bool {
		return s.scalar(`SELECT count(*) FROM app_effects WHERE state IN ('pending', 'running')`) == 0
	})
	again := s.record(s.bob, s.app, record)
	if again.Revision != paid.Revision || len(s.history(s.bob, s.app, record).History) != history {
		t.Fatalf("a replay changed the record: revision %d, was %d", again.Revision, paid.Revision)
	}
	if n := len(s.sessionPosts()); n != 1 {
		t.Fatalf("%d calls to the provider after the payment", n)
	}
}

// ----- what the provider's extra events, repeats and lateness do -----

func TestTheSamePaymentToldTwiceUnderTwoEventIdsAppliesOnce(t *testing.T) {
	s := newInvoicing(t)
	record, session := s.sentInvoice(s.bob, s.app, "A-1", 50)
	evs, err := s.fake.Pay(session)
	if err != nil {
		t.Fatal(err)
	}
	s.deliver(s.in, evs)
	s.settled(s.alice, s.in, 1)
	before := s.record(s.bob, s.app, record)

	// Stripe may tell a payment again as a new event; it is stored, and ignored as a duplicate.
	again := s.completed(session, nil)
	s.deliver(s.in, []testprovider.Event{again})
	events := s.settled(s.alice, s.in, 2)
	if events[0].State != integrations.EventIgnored || events[0].Code != "integration_event_duplicate" || events[1].State != integrations.EventApplied {
		t.Fatalf("events = %+v", events)
	}
	if after := s.record(s.bob, s.app, record); after.Revision != before.Revision {
		t.Fatalf("revision %d, was %d", after.Revision, before.Revision)
	}
}

func TestALateEventChangesNothing(t *testing.T) {
	s := newInvoicing(t)
	record, session := s.sentInvoice(s.bob, s.app, "A-1", 50)
	evs, err := s.fake.Pay(session)
	if err != nil {
		t.Fatal(err)
	}
	s.deliver(s.in, evs)
	s.settled(s.alice, s.in, 1)
	paid := s.record(s.bob, s.app, record)

	// The session's expiry arrives after its payment (events don't arrive in order): the payment stands.
	expired := testprovider.NewEvent(testprovider.EventSessionExpired, map[string]any{
		"id": session, "object": "checkout.session", "mode": "payment", "payment_status": "unpaid", "status": "expired",
		"client_reference_id": s.sessionPosts()[0].Form.Get("client_reference_id"), "amount_total": 5000, "currency": "eur",
	}, s.clock.Now())
	s.deliver(s.in, []testprovider.Event{expired})
	events := s.settled(s.alice, s.in, 2)
	if events[0].Kind != integrations.EventPaymentExpired || events[0].State != integrations.EventIgnored || events[0].Code != "integration_event_late" {
		t.Fatalf("events = %+v", events)
	}
	if after := s.record(s.bob, s.app, record); after.Revision != paid.Revision || after.Data["status"] != "paid" {
		t.Fatalf("a late event changed the record: %+v", after)
	}

	// A delivery signed longer ago than the provider's tolerance is not authentic any more: it is the
	// route's plain 404, and nothing is stored, though the signature itself is right.
	d, err := s.sender(s.in).Send(context.Background(), expired, testprovider.OldBy(testprovider.Tolerance+time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != http.StatusNotFound {
		t.Fatalf("a stale delivery was answered %d", d.Status)
	}
	// And a recorded delivery replayed after the window, signature and all, is refused the same way.
	first := s.sender(s.in)
	good, err := first.Send(context.Background(), s.completed(session, nil))
	if err != nil || good.Status != http.StatusOK {
		t.Fatalf("a fresh delivery: %v %d", err, good.Status)
	}
	s.clock.Advance(testprovider.Tolerance + time.Minute)
	replay, err := first.Replay(context.Background(), good)
	if err != nil || replay.Status != http.StatusNotFound {
		t.Fatalf("a replay after the window: %v %d", err, replay.Status)
	}
	if n := len(s.settled(s.alice, s.in, 3)); n != 3 {
		t.Fatalf("%d events", n)
	}
}

func TestAPaymentOfAnotherAmountIsNotAppliedAndTheAdminIsTold(t *testing.T) {
	s := newInvoicing(t)
	record, session := s.sentInvoice(s.bob, s.app, "A-1", 120.10)
	before := s.record(s.bob, s.app, record)

	// What the event says was paid is not what the link asked: 100.00 for 120.10, a tampered amount.
	s.deliver(s.in, []testprovider.Event{s.completed(session, func(o map[string]any) { o["amount_total"] = 10000 })})
	events := s.settled(s.alice, s.in, 1)
	if e := events[0]; e.State != integrations.EventFailed || e.Code != "integration_amount_mismatch" || e.AmountMinor != 10000 {
		t.Fatalf("event = %+v", e)
	}
	// The same in another currency is no better.
	s.deliver(s.in, []testprovider.Event{s.completed(session, func(o map[string]any) { o["currency"] = "usd" })})
	events = s.settled(s.alice, s.in, 2)
	if events[0].State != integrations.EventFailed || events[0].Code != "integration_amount_mismatch" {
		t.Fatalf("events = %+v", events)
	}
	if after := s.record(s.bob, s.app, record); after.Revision != before.Revision || after.Data["status"] != "sent" {
		t.Fatalf("a payment of the wrong amount changed the record: %+v", after)
	}
	// Money that arrives is never silently dropped: the admins are told, and bob, who is no admin, isn't.
	if n := s.scalar(`SELECT count(*) FROM notifications WHERE kind = 'integration_event' AND user_id = ?`, s.alice.id); n != 2 {
		t.Fatalf("the admin has %d notices", n)
	}
	if n := s.scalar(`SELECT count(*) FROM notifications WHERE kind = 'integration_event' AND user_id != ?`, s.alice.id); n != 0 {
		t.Fatalf("%d notices went to people who aren't admins", n)
	}
}

// ----- refunds -----

func TestARefundMarksTheInvoiceRefundedOnlyWhenItIsWhole(t *testing.T) {
	s := newInvoicing(t)
	record, session := s.sentInvoice(s.bob, s.app, "A-1", 120.10)
	evs, err := s.fake.Pay(session)
	if err != nil {
		t.Fatal(err)
	}
	s.deliver(s.in, evs)
	s.settled(s.alice, s.in, 1)

	// A part of the payment is refunded: the invoice stays paid, and the event says why it changed nothing.
	part, err := s.fake.Refund(session, 2010)
	if err != nil {
		t.Fatal(err)
	}
	s.deliver(s.in, part)
	events := s.settled(s.alice, s.in, 2)
	if e := events[0]; e.Kind != integrations.EventPaymentRefunded || e.State != integrations.EventIgnored || e.Code != "integration_event_partial_refund" || e.AmountMinor != 2010 {
		t.Fatalf("event = %+v", e)
	}
	if got := s.record(s.bob, s.app, record).Data["status"]; got != "paid" {
		t.Fatalf("status after a partial refund = %v", got)
	}

	// The rest is refunded: what has been refunded so far is the whole, and the invoice is refunded, by
	// the integration.
	rest, err := s.fake.Refund(session, 0)
	if err != nil {
		t.Fatal(err)
	}
	s.deliver(s.in, rest)
	events = s.settled(s.alice, s.in, 3)
	if e := events[0]; e.Kind != integrations.EventPaymentRefunded || e.State != integrations.EventApplied || e.AmountMinor != 12010 {
		t.Fatalf("event = %+v", e)
	}
	refunded := s.record(s.bob, s.app, record)
	if refunded.Data["status"] != "refunded" || refunded.UpdatedAction != integrations.EventPaymentRefunded || refunded.UpdatedBy == nil || refunded.UpdatedBy.System == nil || refunded.UpdatedBy.System.ID != s.in.id {
		t.Fatalf("record = %+v by %+v", refunded.Data, refunded.UpdatedBy)
	}
	// The same refund told again changes nothing.
	s.deliver(s.in, rest)
	s.settled(s.alice, s.in, 3)
	if s.record(s.bob, s.app, record).Revision != refunded.Revision {
		t.Fatal("a repeated refund changed the record")
	}
}

// ----- what is kept -----

// What the payer typed at the provider, and the provider's own words, are never kept: an event is
// stored as closed, typed fields and the body's digest, and nothing of it reaches a record.
func TestNoRawWebhookBodyAndNoPayerDataIsKept(t *testing.T) {
	s := newInvoicing(t)
	payer := s.fake.Payer()
	record, session := s.sentInvoice(s.bob, s.app, "A-1", 50)
	evs, err := s.fake.Pay(session)
	if err != nil {
		t.Fatal(err)
	}
	// The payer's details really are in what the provider sends.
	if body := string(evs[len(evs)-1].Body()); !strings.Contains(body, payer.Email) || !strings.Contains(body, payer.Name) {
		t.Fatalf("the fake's event has no payer's details: %s", body)
	}
	s.deliver(s.in, evs)
	s.settled(s.alice, s.in, 1)
	if s.record(s.bob, s.app, record).Data["status"] != "paid" {
		t.Fatal("the invoice isn't paid")
	}

	// Not in any table of the database (whose scan finds what a table does hold: the invoice's customer)...
	if s.dbHolds("Acme") == 0 {
		t.Fatal("the scan of the database found nothing it should: it proves nothing")
	}
	for _, needle := range []string{payer.Email, payer.Name} {
		if n := s.dbHolds(needle); n != 0 {
			t.Errorf("the database holds the payer's detail %q in %d places", needle, n)
		}
	}
	// ...and not in anything the server answered or logged, webhook replies included: the recorder
	// must have seen the replies to the webhook deliveries, or this would find nothing for lack of looking.
	if n := len(s.hookAnswers()); n < len(evs) {
		t.Fatalf("%d webhook replies were recorded for %d deliveries: the check of the answers proves nothing", n, len(evs))
	}
	for _, a := range s.seen.all() {
		if strings.Contains(a, payer.Email) || strings.Contains(a, payer.Name) {
			t.Errorf("an answer holds the payer's details: %.200s", a)
		}
	}
	if logs := s.logs.String(); strings.Contains(logs, payer.Email) || strings.Contains(logs, payer.Name) {
		t.Errorf("the log holds the payer's details")
	}
	// An event has a digest of its body, and no body.
	if n := s.scalar(`SELECT count(*) FROM pragma_table_info('integration_events') WHERE name LIKE '%body%' AND name != 'body_sha256'`); n != 0 {
		t.Fatalf("integration_events has a column that holds a body")
	}
}

// ----- a payment link the provider hosts -----

// refMetadata is the key of a payment link's metadata that holds the reference's token: the
// integration's own choice, which this checks from outside.
const refMetadata = "taskto_ref"

// A server with no public URL has no page of its own for a payer to return to, so its invoices get a
// hosted payment link, with the reference's token in the link's address. A payer who opens the address
// and pays makes a session that carries the token, and the payment finds its invoice by it: the other
// invoice, whose link is of the same shape, is left as it was.
func TestAPaymentThroughAHostedPaymentLinkFindsItsInvoiceByToken(t *testing.T) {
	p := newPayStackAt(t, "")
	in := p.connect(p.alice, p.one, "shop", keyA, hookA)
	p.allowSpaces(p.alice, in, p.team)
	app := p.build(p.bob, p.alice, in, p.team, "Invoices", payInvoiceApp)

	first, firstLink := p.sentInvoice(p.bob, app, "A-1", 50)
	second, secondLink := p.sentInvoice(p.bob, app, "A-2", 70)

	// Links, not sessions: made once each, with no page of ours and the token in the link's metadata,
	// and the record holds the link's address and id.
	if n := len(p.sessionPosts()); n != 0 {
		t.Fatalf("%d checkout sessions were made by a server with no public URL", n)
	}
	posts := p.fake.RequestsTo(http.MethodPost, "/v1/payment_links")
	if len(posts) != 2 {
		t.Fatalf("%d payment links were made: %+v", len(posts), posts)
	}
	tokens := map[string]string{}
	for _, l := range []struct{ record, id string }{{"A-1", firstLink}, {"A-2", secondLink}} {
		link, ok := p.fake.Link(l.id)
		if !ok || len(link.Metadata[refMetadata]) != 32 || !link.Active {
			t.Fatalf("the link of %s: %+v", l.record, link)
		}
		tokens[l.id] = link.Metadata[refMetadata]
	}
	if tokens[firstLink] == tokens[secondLink] {
		t.Fatal("two invoices share a token")
	}
	payURL := func(record int64) string {
		u, _ := p.record(p.bob, app, record).Data["pay_url"].(string)
		return u
	}
	for record, link := range map[int64]string{first: firstLink, second: secondLink} {
		u := payURL(record)
		if !strings.HasSuffix(u, "?client_reference_id="+tokens[link]) {
			t.Fatalf("the address %q doesn't carry the token of its link", u)
		}
	}

	// The payer of the second invoice opens its address and pays. The first is not touched.
	evs, err := p.fake.PayLinkAt(payURL(second))
	if err != nil {
		t.Fatal(err)
	}
	p.deliver(in, evs)
	events := p.settled(p.alice, in, 1)
	if e := events[0]; e.Kind != integrations.EventPaymentSucceeded || e.State != integrations.EventApplied || e.AmountMinor != 7000 {
		t.Fatalf("event = %+v", e)
	}
	if got := p.record(p.bob, app, second).Data["status"]; got != "paid" {
		t.Fatalf("the invoice that was paid is %v", got)
	}
	if got := p.record(p.bob, app, first).Data["status"]; got != "sent" {
		t.Fatalf("the other invoice is %v: the payment went to the wrong one", got)
	}
	// The session carried the token the address had.
	var carried []string
	for _, ss := range p.fake.Sessions() {
		if ss.ClientReferenceID != nil {
			carried = append(carried, *ss.ClientReferenceID)
		}
	}
	if len(carried) != 1 || carried[0] != tokens[secondLink] {
		t.Fatalf("the sessions carry %v, want the token of the second link", carried)
	}
	// The link takes no second payment, so the invoice can't be paid twice.
	if _, err := p.fake.PayLinkAt(payURL(second)); !errors.Is(err, testprovider.ErrLinkInactive) {
		t.Fatalf("a second payment through the same link: %v", err)
	}

	// Then the first one.
	evs, err = p.fake.PayLinkAt(payURL(first))
	if err != nil {
		t.Fatal(err)
	}
	p.deliver(in, evs)
	p.settled(p.alice, in, 2)
	if got := p.record(p.bob, app, first).Data["status"]; got != "paid" {
		t.Fatalf("the first invoice is %v", got)
	}
}
