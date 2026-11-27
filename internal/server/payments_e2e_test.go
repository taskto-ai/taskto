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

var (
	keyA  = testprovider.TestAPIKey
	keyB  = "sk_" + "test_" + strings.Repeat("Rota", 6)
	keyLv = "sk_" + "live_" + strings.Repeat("Fake", 6)

	hookA  = testprovider.TestWebhookSecret
	hookB  = testprovider.OtherWebhookSecret
	hookLv = "whsec_" + strings.Repeat("Live", 6)
	hookW2 = "whsec_" + strings.Repeat("Twoo", 6)
)

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

type movableClock struct{ offset atomic.Int64 }

func (c *movableClock) Now() time.Time { return time.Now().Add(time.Duration(c.offset.Load())) }

func (c *movableClock) Advance(d time.Duration) { c.offset.Add(int64(d)) }

type shop struct {
	id     int64
	ws     int64
	hook   string
	secret string
	key    string
}

type payStack struct {
	*stack
	fake   *testprovider.Stripe
	clock  *movableClock
	svc    *integrations.Service
	apps   *apps.Service
	base   string
	logs   *lockedBuffer
	seen   *answers
	ops    int
	public string
}

func newPayStack(t *testing.T) *payStack { return newPayStackAt(t, hooksOrigin) }

func newPayStackAt(t *testing.T, publicURL string) *payStack {
	t.Helper()
	p := &payStack{logs: &lockedBuffer{}, seen: &answers{}, public: publicURL}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(p.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

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
			p.svc.UseRecords(a)
		}
	}, func(next http.Handler) http.Handler { return recording{next: next, into: p.seen} })
	p.base = strings.TrimSuffix(p.mcpURL, "/mcp")

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

func (p *payStack) mustCall(status int, cookie, method, path string, body, out any) {
	p.t.Helper()
	if got := p.call(cookie, method, path, body, out); got != status {
		rec := p.request(method, path, body, sameOrigin(cookie))
		p.t.Fatalf("%s %s = %d, want %d: %s", method, path, got, status, rec.Body)
	}
}

func (p *payStack) code(cookie, method, path string, body any) (int, string) {
	p.t.Helper()
	rec := p.request(method, path, body, sameOrigin(cookie))
	return rec.Code, fmt.Sprint(decode(rec)["code"])
}

func (p *payStack) ws(ws int64, rest string) string {
	return fmt.Sprintf("/api/workspaces/%d/%s", ws, rest)
}

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
		return &shop{id: out.ID, ws: ws, hook: p.text(`SELECT public_id FROM integrations WHERE id = ?`, out.ID), secret: secret, key: key}
	}
	if !strings.HasPrefix(out.WebhookURL, hooksOrigin+integrations.HookPrefix) {
		p.t.Fatalf("webhook_url = %q", out.WebhookURL)
	}
	return &shop{id: out.ID, ws: ws, hook: strings.TrimPrefix(out.WebhookURL, hooksOrigin+integrations.HookPrefix), secret: secret, key: key}
}

type invoicing struct {
	space, app, version int64
	approval            int64
}

func (p *payStack) appPath(space int64, rest string) string {
	return fmt.Sprintf("/api/workspaces/%d/spaces/%d/apps%s", p.one, space, rest)
}

type rawDefinition string

func (r rawDefinition) MarshalJSON() ([]byte, error) { return []byte(r), nil }

func (p *payStack) draft(who person, space int64, name, definition string) invoicing {
	p.t.Helper()
	var a struct {
		ID      int64 `json:"id"`
		Version int64 `json:"version"`
	}
	p.mustCall(http.StatusCreated, who.cookie, http.MethodPost, p.appPath(space, ""), map[string]any{"name": name, "definition": rawDefinition(definition)}, &a)
	return invoicing{space: space, app: a.ID, version: a.Version}
}

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

func (p *payStack) allowSpaces(admin person, sh *shop, spaces ...int64) {
	p.t.Helper()
	p.mustCall(http.StatusOK, admin.cookie, http.MethodPut, p.ws(sh.ws, fmt.Sprintf("integrations/%d/spaces", sh.id)), map[string]any{"spaces": spaces}, nil)
}

func (p *payStack) approve(admin person, sh *shop, inv invoicing) {
	p.t.Helper()
	p.mustCall(http.StatusOK, admin.cookie, http.MethodPost, p.ws(sh.ws, fmt.Sprintf("integration-approvals/%d/approve", inv.approval)),
		map[string]any{"integration_id": sh.id}, nil)
}

func (p *payStack) publish(who person, inv invoicing) {
	p.t.Helper()
	p.mustCall(http.StatusOK, who.cookie, http.MethodPost, p.appPath(inv.space, fmt.Sprintf("/%d/publish", inv.app)),
		map[string]any{"version": inv.version, "acknowledge": []any{}}, nil)
}

func (p *payStack) build(who, admin person, sh *shop, space int64, name, definition string) invoicing {
	p.t.Helper()
	inv := p.draft(who, space, name, definition)
	p.askApproval(who, &inv)
	p.approve(admin, sh, inv)
	p.publish(who, inv)
	return inv
}

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

func (p *payStack) send(who person, inv invoicing, record int64) int {
	p.t.Helper()
	p.ops++
	return p.act(who, inv, fmt.Sprintf("op-send-%04d", p.ops), "send", map[string]any{"id": record, "status": "sent"})
}

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

func (p *payStack) hookAnswers() []string {
	var out []string
	for _, a := range p.seen.all() {
		if strings.HasPrefix(a, http.MethodPost+" "+integrations.HookPrefix) {
			out = append(out, a)
		}
	}
	return out
}

func (p *payStack) sender(sh *shop) *testprovider.Sender {
	return testprovider.NewSender(p.base+integrations.HookPrefix+sh.hook,
		testprovider.WithSigningSecret(sh.secret), testprovider.WithSenderClock(p.clock))
}

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

func (p *payStack) eventsOf(admin person, sh *shop) []integrations.EventView {
	p.t.Helper()
	var out integrations.EventList
	p.mustCall(http.StatusOK, admin.cookie, http.MethodGet, p.ws(sh.ws, fmt.Sprintf("integrations/%d/events", sh.id)), nil, &out)
	return out.Events
}

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

func TestAnInvoiceGoesFromSentToPaidThroughTheWholeServer(t *testing.T) {
	s := newInvoicing(t)

	var got integrations.Integration
	s.mustCall(http.StatusOK, s.alice.cookie, http.MethodGet, s.ws(s.one, fmt.Sprintf("integrations/%d", s.in.id)), nil, &got)
	if got.Secrets["api_key"] != "set" || got.Secrets["webhook_secret"] != "set" || got.Mode != "test" {
		t.Fatalf("integration = %+v", got)
	}

	record, session := s.sentInvoice(s.bob, s.app, "A-1", 120.10)

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
	sendRevision := rec.Revision
	queued := s.scalar(`SELECT count(*) FROM app_effects`)
	if got := s.act(s.bob, s.app, fmt.Sprintf("op-send-%04d", s.ops), "send", map[string]any{"id": record, "status": "sent"}); got != http.StatusOK {
		t.Fatalf("the repeated send = %d", got)
	}
	if n := s.scalar(`SELECT count(*) FROM app_effects`); n != queued {
		t.Fatalf("a repeated request queued %d more effects", n-queued)
	}
	s.eventually("the queue holds no work", func() bool {
		return s.scalar(`SELECT count(*) FROM app_effects WHERE state IN ('pending', 'running')`) == 0
	})
	if n := len(s.sessionPosts()); n != 1 {
		t.Fatalf("a repeated request made %d calls", n)
	}

	evs, err := s.fake.Pay(session)
	if err != nil {
		t.Fatal(err)
	}
	s.deliver(s.in, evs)
	events := s.settled(s.alice, s.in, 1)
	if e := events[0]; e.Kind != integrations.EventPaymentSucceeded || e.State != integrations.EventApplied || e.AmountMinor != 12010 || e.Currency != "EUR" || e.Livemode {
		t.Fatalf("event = %+v", e)
	}

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

	history := len(h.History)
	for range 2 {
		s.deliver(s.in, evs)
	}
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

	d, err := s.sender(s.in).Send(context.Background(), expired, testprovider.OldBy(testprovider.Tolerance+time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != http.StatusNotFound {
		t.Fatalf("a stale delivery was answered %d", d.Status)
	}
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

	s.deliver(s.in, []testprovider.Event{s.completed(session, func(o map[string]any) { o["amount_total"] = 10000 })})
	events := s.settled(s.alice, s.in, 1)
	if e := events[0]; e.State != integrations.EventFailed || e.Code != "integration_amount_mismatch" || e.AmountMinor != 10000 {
		t.Fatalf("event = %+v", e)
	}
	s.deliver(s.in, []testprovider.Event{s.completed(session, func(o map[string]any) { o["currency"] = "usd" })})
	events = s.settled(s.alice, s.in, 2)
	if events[0].State != integrations.EventFailed || events[0].Code != "integration_amount_mismatch" {
		t.Fatalf("events = %+v", events)
	}
	if after := s.record(s.bob, s.app, record); after.Revision != before.Revision || after.Data["status"] != "sent" {
		t.Fatalf("a payment of the wrong amount changed the record: %+v", after)
	}
	s.eventually("the admin has a notice of each failed event", func() bool {
		return s.scalar(`SELECT count(*) FROM integration_event_notices`) == 0
	})
	if n := s.scalar(`SELECT count(*) FROM notifications WHERE kind = 'integration_event' AND user_id = ?`, s.alice.id); n != 2 {
		t.Fatalf("the admin has %d notices", n)
	}
	if n := s.scalar(`SELECT count(*) FROM notifications WHERE kind = 'integration_event' AND user_id != ?`, s.alice.id); n != 0 {
		t.Fatalf("%d notices went to people who aren't admins", n)
	}
}

func TestARefundMarksTheInvoiceRefundedOnlyWhenItIsWhole(t *testing.T) {
	s := newInvoicing(t)
	record, session := s.sentInvoice(s.bob, s.app, "A-1", 120.10)
	evs, err := s.fake.Pay(session)
	if err != nil {
		t.Fatal(err)
	}
	s.deliver(s.in, evs)
	s.settled(s.alice, s.in, 1)

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
	s.deliver(s.in, rest)
	s.settled(s.alice, s.in, 3)
	if s.record(s.bob, s.app, record).Revision != refunded.Revision {
		t.Fatal("a repeated refund changed the record")
	}
}

func TestNoRawWebhookBodyAndNoPayerDataIsKept(t *testing.T) {
	s := newInvoicing(t)
	payer := s.fake.Payer()
	record, session := s.sentInvoice(s.bob, s.app, "A-1", 50)
	evs, err := s.fake.Pay(session)
	if err != nil {
		t.Fatal(err)
	}
	if body := string(evs[len(evs)-1].Body()); !strings.Contains(body, payer.Email) || !strings.Contains(body, payer.Name) {
		t.Fatalf("the fake's event has no payer's details: %s", body)
	}
	s.deliver(s.in, evs)
	s.settled(s.alice, s.in, 1)
	if s.record(s.bob, s.app, record).Data["status"] != "paid" {
		t.Fatal("the invoice isn't paid")
	}

	if s.dbHolds("Acme") == 0 {
		t.Fatal("the scan of the database found nothing it should: it proves nothing")
	}
	for _, needle := range []string{payer.Email, payer.Name} {
		if n := s.dbHolds(needle); n != 0 {
			t.Errorf("the database holds the payer's detail %q in %d places", needle, n)
		}
	}
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
	if n := s.scalar(`SELECT count(*) FROM pragma_table_info('integration_events') WHERE name LIKE '%body%' AND name != 'body_sha256'`); n != 0 {
		t.Fatalf("integration_events has a column that holds a body")
	}
}

const refMetadata = "taskto_ref"

func TestAPaymentThroughAHostedPaymentLinkFindsItsInvoiceByToken(t *testing.T) {
	p := newPayStackAt(t, "")
	in := p.connect(p.alice, p.one, "shop", keyA, hookA)
	p.allowSpaces(p.alice, in, p.team)
	app := p.build(p.bob, p.alice, in, p.team, "Invoices", payInvoiceApp)

	first, firstLink := p.sentInvoice(p.bob, app, "A-1", 50)
	second, secondLink := p.sentInvoice(p.bob, app, "A-2", 70)

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
	var carried []string
	for _, ss := range p.fake.Sessions() {
		if ss.ClientReferenceID != nil {
			carried = append(carried, *ss.ClientReferenceID)
		}
	}
	if len(carried) != 1 || carried[0] != tokens[secondLink] {
		t.Fatalf("the sessions carry %v, want the token of the second link", carried)
	}
	if _, err := p.fake.PayLinkAt(payURL(second)); !errors.Is(err, testprovider.ErrLinkInactive) {
		t.Fatalf("a second payment through the same link: %v", err)
	}

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
