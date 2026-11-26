package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/integrations"
)

// invoiceJSON is an invoicing app that declares a slot of payments: sending an invoice asks for a
// payment link, and a payment sets the status and the date. Each test starts from it.
const invoiceJSON = `{
  "components": [{"id":"root","component":"Text","text":"Invoices"}],
  "collection": {"fields": [
    {"name": "number", "type": "string", "required": true},
    {"name": "customer", "type": "string"},
    {"name": "total", "type": "number", "required": true},
    {"name": "currency", "type": "choice", "options": ["EUR", "USD"]},
    {"name": "status", "type": "choice", "options": ["draft", "sent", "overdue", "paid"]},
    {"name": "paid_at", "type": "date"},
    {"name": "pay_url", "type": "string", "max_length": 2048},
    {"name": "payment_id", "type": "string", "max_length": 255}
  ]},
  "actions": {
    "add": {"kind": "insert"},
    "send": {"kind": "update", "effects": [{
      "kind": "create_payment_link", "integration": "payments", "amount_field": "total", "currency": "EUR",
      "description": "Invoice {{number}} for {{customer}}", "url_field": "pay_url", "id_field": "payment_id"}]}
  },
  "integrations": {"payments": {"kind": "payments", "on": {
    "payment.succeeded": {
      "only_if": {"field": "status", "op": "eq", "value": "sent"},
      "set": {"status": {"value": "paid"}, "paid_at": {"from": "occurred_at"}}},
    "payment.expired": {"set": {"status": {"value": "sent"}}}}}}
}`

func invoices(t *testing.T) Definition {
	t.Helper()
	var d Definition
	if err := json.Unmarshal([]byte(invoiceJSON), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// changed returns the invoices' JSON with one text replaced, decoded and checked.
func changed(t *testing.T, from, to string) (Definition, error) {
	t.Helper()
	if !strings.Contains(invoiceJSON, from) {
		t.Fatalf("the invoice definition has no %q", from)
	}
	var d Definition
	if err := json.Unmarshal([]byte(strings.Replace(invoiceJSON, from, to, 1)), &d); err != nil {
		return Definition{}, err
	}
	_, err := d.Check()
	return d, err
}

func TestTheInvoiceDeclarationIsValid(t *testing.T) {
	d, err := invoices(t).Check()
	if err != nil {
		t.Fatal(err)
	}
	if d.DefinitionVersion != 3 {
		t.Fatalf("definition_version = %d, want 3", d.DefinitionVersion)
	}
	if !slices.Contains(d.Requires, "effect.create_payment_link") {
		t.Fatalf("requires = %v", d.Requires)
	}
}

// A definition holds a slot of a kind and what its events set, and nothing a service could be
// reached or paid through: every key that isn't the format's is refused, and so is anything that
// doesn't fit the collection.
func TestTheDeclarationIsStrict(t *testing.T) {
	for name, tc := range map[string]struct{ from, to string }{
		"a provider in a slot":      {`"kind": "payments", "on"`, `"kind": "payments", "provider": "stripe", "on"`},
		"an integration id":         {`"kind": "payments", "on"`, `"kind": "payments", "integration_id": 3, "on"`},
		"a url in a slot":           {`"kind": "payments", "on"`, `"kind": "payments", "url": "https://example.com", "on"`},
		"a limit in a slot":         {`"kind": "payments", "on"`, `"kind": "payments", "max_amount": 5, "on"`},
		"a header in a handler":     {`"payment.expired": {"set"`, `"payment.expired": {"headers": {"a": "b"}, "set"`},
		"a url in a set":            {`{"value": "paid"}`, `{"value": "paid", "url": "https://example.com"}`},
		"a redirect in the effect":  {`"id_field": "payment_id"}]`, `"id_field": "payment_id", "success_url": "https://example.com"}]`},
		"a url in the effect":       {`"id_field": "payment_id"}]`, `"id_field": "payment_id", "url": "https://example.com"}]`},
		"a secret in the effect":    {`"id_field": "payment_id"}]`, `"id_field": "payment_id", "api_key": "x"}]`},
		"a value and a source":      {`{"from": "occurred_at"}`, `{"from": "occurred_at", "value": "2026-01-01"}`},
		"neither a value nor from":  {`{"from": "occurred_at"}`, `{}`},
		"an unknown kind":           {`"kind": "payments", "on"`, `"kind": "email", "on"`},
		"an unknown event":          {`"payment.expired": {"set"`, `"payment.paid": {"set"`},
		"an unknown source":         {`"from": "occurred_at"`, `"from": "customer_name"`},
		"a source of another event": {`"payment.expired": {"set": {"status": {"value": "sent"}}}`, `"payment.expired": {"set": {"paid_at": {"from": "paid_at"}}}`},
		"a source in a wrong field": {`"paid_at": {"from": "occurred_at"}`, `"total": {"from": "occurred_at"}`},
		"a target that isn't one":   {`"paid_at": {"from": "occurred_at"}`, `"nothing": {"from": "occurred_at"}`},
		"a literal of another type": {`{"value": "paid"}`, `{"value": 7}`},
		"a choice not an option":    {`{"value": "paid"}`, `{"value": "refunded"}`},
		"no fields to set":          {`"payment.expired": {"set": {"status": {"value": "sent"}}}`, `"payment.expired": {"set": {}}`},
		"a bad rule":                {`"field": "status", "op": "eq"`, `"field": "missing", "op": "eq"`},
		"an unknown slot":           {`"integration": "payments"`, `"integration": "billing"`},
		"an amount that isn't one":  {`"amount_field": "total"`, `"amount_field": "number"`},
		"an unknown currency":       {`"currency": "EUR"`, `"currency": "XXX"`},
		"no currency":               {`"currency": "EUR",`, ``},
		"both currencies":           {`"currency": "EUR",`, `"currency": "EUR", "currency_field": "currency",`},
		"a link field too small":    {`"max_length": 2048`, `"max_length": 100`},
		"the same field twice":      {`"id_field": "payment_id"`, `"id_field": "pay_url"`},
		"a slot with a bad name":    {`"integrations": {"payments"`, `"integrations": {"Pay Ments": {"kind": "payments", "on": {"payment.failed": {"set": {"status": {"value": "overdue"}}}}}, "payments"`},
		"a null value and a source": {`{"from": "occurred_at"}`, `{"from": "occurred_at", "value": null}`},
		"a value and an empty from": {`{"value": "paid"}`, `{"value": "paid", "from": ""}`},
		"an empty from":             {`{"from": "occurred_at"}`, `{"from": ""}`},
		"a null value":              {`{"value": "paid"}`, `{"value": null}`},
		"a blank literal":           {`"payment.expired": {"set": {"status": {"value": "sent"}}}`, `"payment.expired": {"set": {"customer": {"value": "   "}}}`},
		"a field of another kind":   {`"paid_at": {"from": "occurred_at"}`, `"total": {"from": "currency"}`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := changed(t, tc.from, tc.to); err == nil {
				t.Fatal("the definition was accepted")
			}
		})
	}
}

func TestADefinitionOfAnEarlierVersionCannotUseIntegrations(t *testing.T) {
	d := invoices(t)
	d.DefinitionVersion = 2
	if _, err := d.Check(); err == nil || !strings.Contains(err.Error(), "integrations") {
		t.Fatalf("a version 2 definition with integrations: %v", err)
	}
}

func TestASlotNoEffectOrEventUsesIsRefused(t *testing.T) {
	d := invoices(t)
	d.Integrations["spare"] = IntegrationSlot{Kind: KindPayments}
	if _, err := d.Check(); err == nil {
		t.Fatal("a slot that does nothing was accepted")
	}
}

func TestTheContractHashIsStableAndCoversWhatIsApproved(t *testing.T) {
	base := invoices(t)
	hash := func(d Definition) string {
		t.Helper()
		d, err := d.Check()
		if err != nil {
			t.Fatal(err)
		}
		return d.contract("payments").Hash()
	}
	h := hash(base)
	if len(h) != 64 || h != hash(invoices(t)) {
		t.Fatalf("the hash of the same definition: %q", h)
	}
	c, _ := base.Check()
	contract := c.contract("payments")
	if !slices.Equal(contract.Sends, []string{"customer", "number", "total"}) || !slices.Equal(contract.Currencies, []string{"EUR"}) ||
		!slices.Equal(contract.Operations, []string{EffectCreatePaymentLink}) || !slices.Equal(contract.Events, []string{EventPaymentExpired, EventPaymentSucceeded}) {
		t.Fatalf("the contract: %+v", contract)
	}
	var writes []string
	for _, w := range contract.Writes {
		writes = append(writes, fmt.Sprintf("%s=%d/%v", w.Field, len(w.Values), w.From))
	}
	if want := []string{"paid_at=0/[occurred_at]", "pay_url=0/[link.url]", "payment_id=0/[link.id]", "status=2/[]"}; !slices.Equal(writes, want) {
		t.Fatalf("writes = %v, want %v", writes, want)
	}

	// The wording a person reads is not in it: changing it needs no new approval.
	if got, _ := changed(t, "Invoice {{number}} for {{customer}}", "Please pay invoice {{number}}, {{customer}}"); hash(got) != h {
		t.Fatal("the description's wording changed the contract")
	}
	// What is sent out, written, or let through is.
	for name, tc := range map[string]struct{ from, to string }{
		"a field sent out":      {"Invoice {{number}} for {{customer}}", "Invoice {{number}} for {{customer}} {{status}}"},
		"a currency field":      {`"currency": "EUR"`, `"currency_field": "currency"`},
		"another currency":      {`"currency": "EUR"`, `"currency": "USD"`},
		"another literal":       {`"payment.expired": {"set": {"status": {"value": "sent"}}}`, `"payment.expired": {"set": {"status": {"value": "overdue"}}}`},
		"another field written": {`"payment.expired": {"set": {"status": {"value": "sent"}}}`, `"payment.expired": {"set": {"status": {"value": "sent"}, "customer": {"value": "x"}}}`},
		"another rule":          {`"value": "sent"}`, `"value": "overdue"}`},
		"another event":         {`"payment.expired": {"set": {"status": {"value": "sent"}}}`, `"payment.failed": {"set": {"status": {"value": "sent"}}}`},
	} {
		d, err := changed(t, tc.from, tc.to)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if hash(d) == h {
			t.Errorf("%s did not change the contract", name)
		}
	}
}

// ----- approvals -----

// inv is a workspace with an invoicing app in the shared space, and two integrations of the workspace.
type inv struct {
	*world
	app    App
	stripe int64
}

func newInv(t *testing.T) *inv {
	t.Helper()
	w := newWorld(t)
	w.e.s.WithPlatform(Platform{ProviderKinds: func(provider string) []string {
		if provider == "stripe" {
			return []string{KindPayments}
		}
		return nil
	}})
	i := &inv{world: w}
	i.stripe = i.integration(w.ws, "stripe", "main")
	rec := w.alice.do(http.MethodPost, appsPath(w.shared, ""), map[string]any{"name": "Invoices", "definition": invoices(t)})
	expect(t, rec, http.StatusCreated)
	i.app = decode[App](t, rec)
	return i
}

// integration makes an integration of a workspace, switched on, with no space allowed.
func (i *inv) integration(workspace int64, provider, name string) int64 {
	i.e.t.Helper()
	var id int64
	if err := i.e.db.QueryRow(`INSERT INTO integrations (workspace_id, public_id, provider, name, display_name, mode, max_amount_minor, currencies)
		VALUES (?, lower(hex(randomblob(16))), ?, ?, ?, 'test', 100000, '["EUR"]') RETURNING id`, workspace, provider, name, name).Scan(&id); err != nil {
		i.e.t.Fatal(err)
	}
	return id
}

func (i *inv) allow(integration int64, s access.Space) {
	i.e.t.Helper()
	if _, err := i.e.db.Exec(`INSERT INTO integration_spaces (integration_id, space_id) VALUES (?, ?)`, integration, s.ID); err != nil {
		i.e.t.Fatal(err)
	}
}

func (i *inv) status(u *user, s access.Space, a App) IntegrationStatus {
	i.e.t.Helper()
	rec := u.do(http.MethodGet, appPath(s, a.ID, "/integrations"), nil)
	expect(i.e.t, rec, http.StatusOK)
	return decode[IntegrationStatus](i.e.t, rec)
}

func (i *inv) request(u *user, s access.Space, a App, slot, hash string) *httptest.ResponseRecorder {
	i.e.t.Helper()
	return u.do(http.MethodPost, appPath(s, a.ID, "/integrations/"+slot+"/request"), map[string]any{"contract_hash": hash})
}

func (i *inv) mustRequest(u *user, s access.Space, a App) IntegrationApproval {
	i.e.t.Helper()
	st := i.status(u, s, a)
	if len(st.Slots) != 1 {
		i.e.t.Fatalf("slots = %+v", st.Slots)
	}
	rec := i.request(u, s, a, "payments", st.Slots[0].ContractHash)
	expect(i.e.t, rec, http.StatusOK)
	return decode[IntegrationApproval](i.e.t, rec)
}

func (i *inv) decide(u *user, workspace, approval int64, verb string, body any) *httptest.ResponseRecorder {
	i.e.t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	return u.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/integration-approvals/%d/%s", workspace, approval, verb), body)
}

func (i *inv) approve(u *user, approval, integration int64) *httptest.ResponseRecorder {
	i.e.t.Helper()
	return i.decide(u, i.ws, approval, "approve", map[string]any{"integration_id": integration})
}

func (i *inv) queue(u *user, workspace int64, query string) *httptest.ResponseRecorder {
	i.e.t.Helper()
	return u.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/integration-approvals%s", workspace, query), nil)
}

// code is the error code of a response.
func code(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Code string `json:"code"`
		// Params carries what the message needs.
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return e.Code
}

func (i *inv) fresh(u *user, s access.Space, a App) App { return i.view(u, s, a) }

func TestAnAppCannotBePublishedBeforeItsContractIsApproved(t *testing.T) {
	i := newInv(t)
	i.allow(i.stripe, i.shared)

	rec := i.tryPublish(i.alice, i.shared, i.app)
	expect(t, rec, http.StatusConflict)
	if code(t, rec) != string(apierr.CodeAppIntegrationUnapproved) || !strings.Contains(rec.Body.String(), `"slot":"payments"`) {
		t.Fatalf("an unapproved publish: %s", rec.Body.String())
	}

	// Asking is not approval.
	req := i.mustRequest(i.alice, i.shared, i.app)
	if req.Status != ApprovalRequested || req.Slot != "payments" || req.Kind != KindPayments || req.Integration != nil {
		t.Fatalf("the request: %+v", req)
	}
	expect(t, i.tryPublish(i.alice, i.shared, i.app), http.StatusConflict)

	// An approval is of an integration of the workspace, by an admin.
	approved := decode[IntegrationApproval](t, func() *httptest.ResponseRecorder {
		rec := i.approve(i.owner, req.ID, i.stripe)
		expect(t, rec, http.StatusOK)
		return rec
	}())
	if approved.Status != ApprovalApproved || approved.Integration == nil || approved.Integration.ID != i.stripe || !approved.Integration.Usable || approved.DecidedBy == nil {
		t.Fatalf("the approval: %+v", approved)
	}
	expect(t, i.tryPublish(i.alice, i.shared, i.app), http.StatusOK)
	st := i.status(i.alice, i.shared, i.fresh(i.alice, i.shared, i.app))
	if st.Slots[0].Status != ApprovalApproved || st.Slots[0].Live == nil || !st.Slots[0].Live.Usable {
		t.Fatalf("the status after publishing: %+v", st.Slots[0])
	}
}

func TestAChangedContractNeedsANewApproval(t *testing.T) {
	i := newInv(t)
	i.allow(i.stripe, i.shared)
	expect(t, i.approve(i.owner, i.mustRequest(i.alice, i.shared, i.app).ID, i.stripe), http.StatusOK)
	expect(t, i.tryPublish(i.alice, i.shared, i.app), http.StatusOK)

	// The wording is free.
	d, _ := changed(t, "Invoice {{number}} for {{customer}}", "Pay invoice {{number}} of {{customer}}")
	app := i.fresh(i.alice, i.shared, i.app)
	expect(t, i.alice.do(http.MethodPatch, appPath(i.shared, app.ID, ""), map[string]any{"version": app.Version, "definition": d}), http.StatusOK)
	expect(t, i.tryPublish(i.alice, i.shared, i.fresh(i.alice, i.shared, i.app)), http.StatusOK)

	// What the integration may write is not.
	d, _ = changed(t, `"payment.expired": {"set": {"status": {"value": "sent"}}}`, `"payment.expired": {"set": {"status": {"value": "paid"}}}`)
	app = i.fresh(i.alice, i.shared, i.app)
	expect(t, i.alice.do(http.MethodPatch, appPath(i.shared, app.ID, ""), map[string]any{"version": app.Version, "definition": d}), http.StatusOK)
	st := i.status(i.alice, i.shared, i.fresh(i.alice, i.shared, i.app))
	if st.Slots[0].Status != ApprovalNone || st.Slots[0].Live == nil || !st.Slots[0].Live.Usable {
		t.Fatalf("the new draft's status and the live release's: %+v", st.Slots[0])
	}
	rec := i.tryPublish(i.alice, i.shared, i.fresh(i.alice, i.shared, i.app))
	expect(t, rec, http.StatusConflict)

	// A request for the contract the person read, not another.
	stale := st.Slots[0].ContractHash
	d, _ = changed(t, `"payment.expired": {"set": {"status": {"value": "sent"}}}`, `"payment.expired": {"set": {"status": {"value": "overdue"}}}`)
	app = i.fresh(i.alice, i.shared, i.app)
	expect(t, i.alice.do(http.MethodPatch, appPath(i.shared, app.ID, ""), map[string]any{"version": app.Version, "definition": d}), http.StatusOK)
	rec = i.request(i.alice, i.shared, i.app, "payments", stale)
	expect(t, rec, http.StatusConflict)
	if code(t, rec) != string(apierr.CodeAppIntegrationContractChanged) {
		t.Fatalf("a stale request: %s", rec.Body.String())
	}
	expect(t, i.request(i.alice, i.shared, i.app, "payments", strings.Repeat("0", 64)), http.StatusConflict)
	rec = i.request(i.alice, i.shared, i.app, "nothing", stale)
	expect(t, rec, http.StatusNotFound)
	if code(t, rec) != string(apierr.CodeAppIntegrationSlotUnknown) {
		t.Fatalf("an unknown slot: %s", rec.Body.String())
	}
}

func TestRequestsAreRepeatableAndDecisionsFinal(t *testing.T) {
	i := newInv(t)
	i.allow(i.stripe, i.shared)
	first := i.mustRequest(i.alice, i.shared, i.app)
	if again := i.mustRequest(i.alice, i.shared, i.app); again.ID != first.ID {
		t.Fatalf("asking twice made two requests: %d and %d", first.ID, again.ID)
	}
	if n := i.queue(i.owner, i.ws, "").Body.String(); strings.Count(n, `"slot":"payments"`) != 1 {
		t.Fatalf("the queue: %s", n)
	}

	// Turned down, it is not pending any more, and can be asked for again.
	expect(t, i.decide(i.owner, i.ws, first.ID, "reject", nil), http.StatusOK)
	rec := i.approve(i.owner, first.ID, i.stripe)
	expect(t, rec, http.StatusConflict)
	if code(t, rec) != string(apierr.CodeAppIntegrationNotPending) {
		t.Fatalf("approving a rejected one: %s", rec.Body.String())
	}
	expect(t, i.decide(i.owner, i.ws, first.ID, "reject", nil), http.StatusConflict)
	expect(t, i.tryPublish(i.alice, i.shared, i.app), http.StatusConflict)
	if st := i.status(i.alice, i.shared, i.app); st.Slots[0].Status != ApprovalRejected {
		t.Fatalf("status = %s", st.Slots[0].Status)
	}
	// Asking again after a rejection is a new request: the old one keeps its decision.
	again := i.mustRequest(i.alice, i.shared, i.app)
	if again.ID == first.ID || again.Status != ApprovalRequested || again.DecidedBy != nil {
		t.Fatalf("asking again after a rejection: %+v", again)
	}
	if retry := i.mustRequest(i.alice, i.shared, i.app); retry.ID != again.ID {
		t.Fatalf("a retry of the pending renewal made another request: %d and %d", again.ID, retry.ID)
	}

	// Only an approval can be revoked.
	rec = i.decide(i.owner, i.ws, again.ID, "revoke", nil)
	expect(t, rec, http.StatusConflict)
	if code(t, rec) != string(apierr.CodeAppIntegrationNotApproved) {
		t.Fatalf("revoking a request: %s", rec.Body.String())
	}
	expect(t, i.approve(i.owner, again.ID, i.stripe), http.StatusOK)
	expect(t, i.approve(i.owner, again.ID, i.stripe), http.StatusConflict)
	expect(t, i.decide(i.owner, i.ws, again.ID, "revoke", nil), http.StatusOK)
	expect(t, i.tryPublish(i.alice, i.shared, i.app), http.StatusConflict)
}

// An admin disabled after RequireAdmin let their request in decides nothing: each decision reads
// their account again in its transaction, and the answer is account_disabled.
func TestDecisionsOfAnAdminDisabledMeanwhile(t *testing.T) {
	i := newInv(t)
	i.allow(i.stripe, i.shared)
	req := i.mustRequest(i.alice, i.shared, i.app)
	if _, err := i.e.db.Exec(`UPDATE users SET disabled_at = '2026-01-01T00:00:00.000Z' WHERE id = ?`, i.owner.id); err != nil {
		t.Fatal(err)
	}
	ctx := audit.WithActor(context.Background(), i.owner.id)
	if _, err := i.e.s.approve(ctx, i.ws, i.owner.id, req.ID, Decision{IntegrationID: i.stripe}); !errors.Is(err, auth.ErrAccountDisabled) {
		t.Fatalf("approve by a disabled admin = %v", err)
	}
	if _, err := i.e.s.reject(ctx, i.ws, i.owner.id, req.ID); !errors.Is(err, auth.ErrAccountDisabled) {
		t.Fatalf("reject by a disabled admin = %v", err)
	}
	if _, err := i.e.s.revoke(ctx, i.ws, i.owner.id, req.ID); !errors.Is(err, auth.ErrAccountDisabled) {
		t.Fatalf("revoke by a disabled admin = %v", err)
	}
	if st := i.status(i.alice, i.shared, i.app); st.Slots[0].Status != ApprovalRequested {
		t.Fatalf("a disabled admin changed the request: %+v", st.Slots[0])
	}
	rec := httptest.NewRecorder()
	fail(rec, httptest.NewRequest(http.MethodPost, "/", nil), auth.ErrAccountDisabled)
	expect(t, rec, http.StatusForbidden)
	if code(t, rec) != string(apierr.CodeAccountDisabled) {
		t.Fatalf("the answer = %s", rec.Body.String())
	}
}

func TestApprovingChecksTheIntegration(t *testing.T) {
	i := newInv(t)
	req := i.mustRequest(i.alice, i.shared, i.app)

	// The space isn't allowed: the admin says so, or the approval is refused.
	rec := i.approve(i.owner, req.ID, i.stripe)
	expect(t, rec, http.StatusConflict)
	if code(t, rec) != string(apierr.CodeAppIntegrationSpaceNotAllowed) {
		t.Fatalf("a space that isn't allowed: %s", rec.Body.String())
	}

	// Another workspace's integration is not found, whoever asks.
	foreign := i.integration(i.other, "stripe", "theirs")
	expect(t, i.approve(i.owner, req.ID, foreign), http.StatusNotFound)
	expect(t, i.approve(i.owner, req.ID, 99999), http.StatusNotFound)
	expect(t, i.decide(i.owner, i.ws, req.ID, "approve", map[string]any{}), http.StatusBadRequest)

	// A provider that does not do the kind.
	square := i.integration(i.ws, "other", "other")
	i.allow(square, i.shared)
	rec = i.approve(i.owner, req.ID, square)
	expect(t, rec, http.StatusUnprocessableEntity)
	if code(t, rec) != string(apierr.CodeAppIntegrationKindMismatch) {
		t.Fatalf("a provider of another kind: %s", rec.Body.String())
	}

	// Switched off.
	i.allow(i.stripe, i.shared)
	if _, err := i.e.db.Exec(`UPDATE integrations SET disabled_at = CURRENT_TIMESTAMP WHERE id = ?`, i.stripe); err != nil {
		t.Fatal(err)
	}
	rec = i.approve(i.owner, req.ID, i.stripe)
	expect(t, rec, http.StatusConflict)
	if code(t, rec) != string(apierr.CodeAppIntegrationDisabled) {
		t.Fatalf("an integration that is off: %s", rec.Body.String())
	}
	if st := i.status(i.alice, i.shared, i.app); st.Slots[0].Status != ApprovalRequested {
		t.Fatalf("the refused approvals changed the request: %+v", st.Slots[0])
	}

	// Allowing the space is part of approving, when the admin says so.
	if _, err := i.e.db.Exec(`UPDATE integrations SET disabled_at = NULL WHERE id = ?`, i.stripe); err != nil {
		t.Fatal(err)
	}
	if _, err := i.e.db.Exec(`DELETE FROM integration_spaces`); err != nil {
		t.Fatal(err)
	}
	expect(t, i.decide(i.owner, i.ws, req.ID, "approve", map[string]any{"integration_id": i.stripe, "allow_space": true}), http.StatusOK)
	if n := i.count(`SELECT count(*) FROM integration_spaces WHERE integration_id = ? AND space_id = ?`, i.stripe, i.shared.ID); n != 1 {
		t.Fatalf("the space was allowed %d times", n)
	}
	expect(t, i.tryPublish(i.alice, i.shared, i.app), http.StatusOK)
}

func (i *inv) count(query string, args ...any) (n int) {
	i.e.t.Helper()
	if err := i.e.db.QueryRow(query, args...).Scan(&n); err != nil {
		i.e.t.Fatal(err)
	}
	return n
}

// Revoking, switching the integration off and taking the space away each stop the live release's
// use at once, and a rollback needs the approval of the release it returns to.
func TestTheGateHoldsForPublishRollbackAndWhatIsLive(t *testing.T) {
	i := newInv(t)
	i.allow(i.stripe, i.shared)
	first := i.mustRequest(i.alice, i.shared, i.app)
	expect(t, i.approve(i.owner, first.ID, i.stripe), http.StatusOK)
	expect(t, i.tryPublish(i.alice, i.shared, i.app), http.StatusOK)
	one := i.fresh(i.alice, i.shared, i.app).Live.Number

	// A second release with another contract, approved.
	d, _ := changed(t, `"payment.expired": {"set": {"status": {"value": "sent"}}}`, `"payment.expired": {"set": {"status": {"value": "overdue"}}}`)
	app := i.fresh(i.alice, i.shared, i.app)
	expect(t, i.alice.do(http.MethodPatch, appPath(i.shared, app.ID, ""), map[string]any{"version": app.Version, "definition": d}), http.StatusOK)
	second := i.mustRequest(i.alice, i.shared, i.app)
	if second.ID == first.ID || second.ContractHash == first.ContractHash {
		t.Fatalf("a changed contract reused an approval")
	}
	expect(t, i.approve(i.owner, second.ID, i.stripe), http.StatusOK)
	expect(t, i.tryPublish(i.alice, i.shared, i.fresh(i.alice, i.shared, i.app)), http.StatusOK)

	// The first release's contract is still approved, so the app can go back to it.
	live := func() int64 { return i.fresh(i.alice, i.shared, i.app).Live.Number }
	if live() == one {
		t.Fatal("still on the first release")
	}
	// Revoking the first approval: the live release (the second) is unaffected, and a rollback to
	// the first is refused.
	expect(t, i.decide(i.owner, i.ws, first.ID, "revoke", nil), http.StatusOK)
	rec := i.tryRollback(i.alice, i.shared, i.fresh(i.alice, i.shared, i.app), one)
	expect(t, rec, http.StatusConflict)
	if code(t, rec) != string(apierr.CodeAppIntegrationUnapproved) {
		t.Fatalf("a rollback to a revoked contract: %s", rec.Body.String())
	}
	if _, err := i.e.s.Binding(i.ctx(), i.app.ID, "payments"); err != nil {
		t.Fatalf("the live release lost its binding: %v", err)
	}

	// Approved again, the rollback works, and the binding is the first approval's.
	again := i.mustRequest(i.alice, i.shared, i.app) // the draft is the second contract: already approved
	_ = again
	d1 := invoices(t)
	app = i.fresh(i.alice, i.shared, i.app)
	expect(t, i.alice.do(http.MethodPatch, appPath(i.shared, app.ID, ""), map[string]any{"version": app.Version, "definition": d1}), http.StatusOK)
	expect(t, i.approve(i.owner, i.mustRequest(i.alice, i.shared, i.app).ID, i.stripe), http.StatusOK)
	expect(t, i.tryRollback(i.alice, i.shared, i.fresh(i.alice, i.shared, i.app), one), http.StatusOK)

	b, err := i.e.s.Binding(i.ctx(), i.app.ID, "payments")
	if err != nil || b.IntegrationID != i.stripe || b.Kind != KindPayments || b.ContractHash != first.ContractHash {
		t.Fatalf("binding = %+v, %v", b, err)
	}
	if _, err := i.e.s.Binding(i.ctx(), i.app.ID, "billing"); !errors.Is(err, ErrIntegrationUnapproved) {
		t.Fatalf("an undeclared slot: %v", err)
	}

	// Each of these stops the live release at once, and the state is told.
	stop := func(name, undo string, do func()) {
		t.Helper()
		do()
		if _, err := i.e.s.Binding(i.ctx(), i.app.ID, "payments"); !errors.Is(err, ErrIntegrationUnapproved) {
			t.Errorf("%s: binding error = %v", name, err)
		}
		if st := i.status(i.alice, i.shared, i.app); st.Slots[0].Live == nil || st.Slots[0].Live.Usable {
			t.Errorf("%s: the live release is still usable: %+v", name, st.Slots[0].Live)
		}
		if _, err := i.e.db.Exec(undo); err != nil {
			t.Fatal(err)
		}
		if _, err := i.e.s.Binding(i.ctx(), i.app.ID, "payments"); err != nil {
			t.Errorf("%s undone: %v", name, err)
		}
	}
	stop("switching the integration off", `UPDATE integrations SET disabled_at = NULL`, func() {
		if _, err := i.e.db.Exec(`UPDATE integrations SET disabled_at = CURRENT_TIMESTAMP`); err != nil {
			t.Fatal(err)
		}
	})
	stop("taking the space away", fmt.Sprintf(`INSERT INTO integration_spaces (integration_id, space_id) VALUES (%d, %d)`, i.stripe, i.shared.ID), func() {
		if _, err := i.e.db.Exec(`DELETE FROM integration_spaces`); err != nil {
			t.Fatal(err)
		}
	})

	// An app taken offline uses nothing, and a revoked approval is the last of them.
	expect(t, i.alice.do(http.MethodPatch, appPath(i.shared, i.app.ID, ""),
		map[string]any{"version": i.fresh(i.alice, i.shared, i.app).Version, "archived": true}), http.StatusOK)
	if _, err := i.e.s.Binding(i.ctx(), i.app.ID, "payments"); !errors.Is(err, ErrOffline) {
		t.Fatalf("an app that is offline: %v", err)
	}
}

func (i *inv) ctx() context.Context { return context.Background() }

// A switched-off integration, or a space taken away, also refuses a publish of a contract that
// was approved.
func TestPublishNeedsAnIntegrationThatIsUsable(t *testing.T) {
	i := newInv(t)
	i.allow(i.stripe, i.shared)
	expect(t, i.approve(i.owner, i.mustRequest(i.alice, i.shared, i.app).ID, i.stripe), http.StatusOK)
	if _, err := i.e.db.Exec(`DELETE FROM integration_spaces`); err != nil {
		t.Fatal(err)
	}
	expect(t, i.tryPublish(i.alice, i.shared, i.app), http.StatusConflict)
	i.allow(i.stripe, i.shared)
	if _, err := i.e.db.Exec(`UPDATE integrations SET disabled_at = CURRENT_TIMESTAMP`); err != nil {
		t.Fatal(err)
	}
	expect(t, i.tryPublish(i.alice, i.shared, i.app), http.StatusConflict)
	if _, err := i.e.db.Exec(`UPDATE integrations SET disabled_at = NULL`); err != nil {
		t.Fatal(err)
	}
	expect(t, i.tryPublish(i.alice, i.shared, i.app), http.StatusOK)
}

// ----- who may do what -----

func TestTheApprovalRoutesAreForTheRightPeople(t *testing.T) {
	i := newInv(t)
	i.allow(i.stripe, i.shared)
	req := i.mustRequest(i.alice, i.shared, i.app)

	// The slots and the request are for those who design or publish the app, in a space they can read.
	expect(t, i.bob.do(http.MethodGet, appPath(i.shared, i.app.ID, "/integrations"), nil), http.StatusForbidden)
	expect(t, i.bob.do(http.MethodPost, appPath(i.shared, i.app.ID, "/integrations/payments/request"),
		map[string]any{"contract_hash": req.ContractHash}), http.StatusForbidden)
	for _, u := range []*user{i.mallory, i.stranger} {
		expect(t, u.do(http.MethodGet, appPath(i.shared, i.app.ID, "/integrations"), nil), http.StatusNotFound)
		expect(t, u.do(http.MethodPost, appPath(i.shared, i.app.ID, "/integrations/payments/request"),
			map[string]any{"contract_hash": req.ContractHash}), http.StatusNotFound)
	}
	// Nobody reads another person's private space's app, an admin included.
	expect(t, i.owner.do(http.MethodGet, appPath(i.alicePrivate, i.aliceApp.ID, "/integrations"), nil), http.StatusNotFound)
	expect(t, i.bob.do(http.MethodGet, appPath(i.alicePrivate, i.aliceApp.ID, "/integrations"), nil), http.StatusNotFound)

	// The queue and the decisions are an admin's: a member of the workspace is refused, and so is
	// the one who asked.
	for _, u := range []*user{i.alice, i.bob, i.mallory} {
		expect(t, i.queue(u, i.ws, ""), http.StatusForbidden)
		expect(t, i.approve(u, req.ID, i.stripe), http.StatusForbidden)
		expect(t, i.decide(u, i.ws, req.ID, "reject", nil), http.StatusForbidden)
		expect(t, i.decide(u, i.ws, req.ID, "revoke", nil), http.StatusForbidden)
	}
	// A person with no session is no one.
	anon := &user{e: i.e}
	expect(t, i.queue(anon, i.ws, ""), http.StatusUnauthorized)
	expect(t, i.approve(anon, req.ID, i.stripe), http.StatusUnauthorized)

	// Another workspace's admin has no part in this workspace's queue, and their own never holds it.
	expect(t, i.queue(i.stranger, i.ws, ""), http.StatusNotFound)
	expect(t, i.approve(i.stranger, req.ID, i.stripe), http.StatusNotFound)
	expect(t, i.decide(i.stranger, i.ws, req.ID, "reject", nil), http.StatusNotFound)
	theirs := i.integration(i.other, "stripe", "theirs")
	expect(t, i.approve(i.stranger, req.ID, theirs), http.StatusNotFound)
	rec := i.queue(i.stranger, i.other, "")
	expect(t, rec, http.StatusOK)
	if got := decode[ApprovalList](t, rec); len(got.Approvals) != 0 {
		t.Fatalf("another workspace's queue holds %+v", got)
	}
	// Nor does the approval's id mean anything through another workspace's URL.
	expect(t, i.approve(i.stranger, req.ID, theirs), http.StatusNotFound)
	rec = i.decide(i.stranger, i.other, req.ID, "approve", map[string]any{"integration_id": theirs})
	expect(t, rec, http.StatusNotFound)
	if got := i.status(i.alice, i.shared, i.app).Slots[0].Status; got != ApprovalRequested {
		t.Fatalf("the request after those attempts: %s", got)
	}
}

// The queue holds what the requester submitted, and nothing from the app or its records: an app in
// a private space is approved the same way, though no admin can open it.
func TestAnAppInAPrivateSpaceIsApprovedThroughTheQueue(t *testing.T) {
	i := newInv(t)
	rec := i.alice.do(http.MethodPost, appsPath(i.alicePrivate, ""), map[string]any{"name": "My invoices", "definition": invoices(t)})
	expect(t, rec, http.StatusCreated)
	app := decode[App](t, rec)

	// The admin can't open it, and doesn't need to.
	expect(t, i.owner.do(http.MethodGet, appPath(i.alicePrivate, app.ID, ""), nil), http.StatusNotFound)
	req := i.mustRequest(i.alice, i.alicePrivate, app)
	expect(t, i.tryPublish(i.alice, i.alicePrivate, app), http.StatusConflict)

	rec = i.queue(i.owner, i.ws, "")
	expect(t, rec, http.StatusOK)
	list := decode[ApprovalList](t, rec)
	if len(list.Approvals) != 1 || list.Approvals[0].ID != req.ID || list.Approvals[0].AppName != "My invoices" ||
		list.Approvals[0].SpaceKind != string(access.Private) || list.Approvals[0].RequestedBy == nil || list.Approvals[0].RequestedBy.ID != i.alice.id {
		t.Fatalf("the queue: %+v", list)
	}
	// The queue is the contract and names. No component, no collection, no record.
	for _, word := range []string{"components", "Invoices", "paid_at"} {
		if word == "paid_at" { // a field NAME is in the contract: it is what the admin approves
			continue
		}
		if strings.Contains(rec.Body.String(), `"`+word+`"`) {
			t.Fatalf("the queue shows %q of the app: %s", word, rec.Body.String())
		}
	}

	// Approving is the admin's consent for the space too: they can't open another person's private
	// space to allow it, and so say so with the approval.
	expect(t, i.approve(i.owner, req.ID, i.stripe), http.StatusConflict)
	expect(t, i.decide(i.owner, i.ws, req.ID, "approve", map[string]any{"integration_id": i.stripe, "allow_space": true}), http.StatusOK)
	expect(t, i.tryPublish(i.alice, i.alicePrivate, app), http.StatusOK)
	// Another person's private space is no one else's to publish into, approval or not.
	expect(t, i.bob.do(http.MethodPost, appPath(i.alicePrivate, app.ID, "/publish"), map[string]any{"version": app.Version}), http.StatusNotFound)
}

func TestTheQueueShowsNoRecords(t *testing.T) {
	i := newInv(t)
	i.allow(i.stripe, i.shared)
	expect(t, i.approve(i.owner, i.mustRequest(i.alice, i.shared, i.app).ID, i.stripe), http.StatusOK)
	expect(t, i.tryPublish(i.alice, i.shared, i.app), http.StatusOK)
	app := i.fresh(i.alice, i.shared, i.app)
	expect(t, i.alice.do(http.MethodPost, appPath(i.shared, app.ID, "/actions"),
		action(app, "add", map[string]any{"number": "INV-1", "customer": "zzz-sentinel-customer", "total": "12.5"})), http.StatusOK)
	for _, q := range []string{"", "?status=all", "?status=approved"} {
		rec := i.queue(i.owner, i.ws, q)
		expect(t, rec, http.StatusOK)
		if strings.Contains(rec.Body.String(), "zzz-sentinel-customer") || strings.Contains(rec.Body.String(), "INV-1") {
			t.Fatalf("the queue shows a record: %s", rec.Body.String())
		}
	}
	expect(t, i.queue(i.owner, i.ws, "?status=bogus"), http.StatusBadRequest)
	if got := decode[ApprovalList](t, i.queue(i.owner, i.ws, "")); len(got.Approvals) != 0 {
		t.Fatalf("the waiting queue holds %+v", got)
	}
	if got := decode[ApprovalList](t, i.queue(i.owner, i.ws, "?status=approved")); len(got.Approvals) != 1 || !got.Approvals[0].Live {
		t.Fatalf("the approved list: %+v", got)
	}
}

// ----- copies, moves, one person -----

func TestACopyCarriesNoApproval(t *testing.T) {
	i := newInv(t)
	i.allow(i.stripe, i.shared)
	expect(t, i.approve(i.owner, i.mustRequest(i.alice, i.shared, i.app).ID, i.stripe), http.StatusOK)
	expect(t, i.tryPublish(i.alice, i.shared, i.app), http.StatusOK)

	rec := i.alice.do(http.MethodPost, appPath(i.shared, i.app.ID, "/duplicate"), map[string]any{"name": "Copy", "source": "live"})
	expect(t, rec, http.StatusCreated)
	cp := decode[App](t, rec)
	st := i.status(i.alice, i.shared, cp)
	if len(st.Slots) != 1 || st.Slots[0].Status != ApprovalNone || st.Slots[0].Integration != nil || st.Slots[0].Live != nil {
		t.Fatalf("the copy's slots: %+v", st.Slots)
	}
	if n := i.count(`SELECT count(*) FROM app_integration_approvals WHERE app_id = ?`, cp.ID); n != 0 {
		t.Fatalf("the copy has %d approvals", n)
	}
	rec = i.tryPublish(i.alice, i.shared, cp)
	expect(t, rec, http.StatusConflict)
	if code(t, rec) != string(apierr.CodeAppIntegrationUnapproved) {
		t.Fatalf("publishing a copy: %s", rec.Body.String())
	}
	// It is approved on its own, by the same contract's hash, as a request of its own.
	req := i.mustRequest(i.alice, i.shared, cp)
	if req.ContractHash != i.status(i.alice, i.shared, i.app).Slots[0].ContractHash || req.ID == 0 {
		t.Fatalf("the copy's request: %+v", req)
	}
	// An approval is of an app, not of a definition: the original's is untouched.
	if st := i.status(i.alice, i.shared, i.app); st.Slots[0].Status != ApprovalApproved {
		t.Fatalf("the original's status: %+v", st.Slots[0])
	}
}

func TestMovingAnAppDropsItsApprovals(t *testing.T) {
	i := newInv(t)
	i.allow(i.stripe, i.shared)
	expect(t, i.approve(i.owner, i.mustRequest(i.alice, i.shared, i.app).ID, i.stripe), http.StatusOK)
	expect(t, i.tryPublish(i.alice, i.shared, i.app), http.StatusOK)

	app := i.fresh(i.alice, i.shared, i.app)
	expect(t, i.alice.do(http.MethodPost, appPath(i.shared, app.ID, "/move"), map[string]any{"version": app.Version, "space_id": i.alicePrivate.ID}), http.StatusOK)
	if n := i.count(`SELECT count(*) FROM app_integration_approvals WHERE app_id = ?`, app.ID); n != 0 {
		t.Fatalf("a moved app kept %d approvals", n)
	}
	if _, err := i.e.s.Binding(i.ctx(), app.ID, "payments"); !errors.Is(err, ErrIntegrationUnapproved) {
		t.Fatalf("a moved app's binding: %v", err)
	}
	// And the integration can be removed again: nothing names it.
	if n := i.count(`SELECT count(*) FROM app_integration_approvals WHERE integration_id = ?`, i.stripe); n != 0 {
		t.Fatalf("%d approvals still name the integration", n)
	}
}

func TestAnApprovedIntegrationCannotBeDeleted(t *testing.T) {
	i := newInv(t)
	i.allow(i.stripe, i.shared)
	expect(t, i.approve(i.owner, i.mustRequest(i.alice, i.shared, i.app).ID, i.stripe), http.StatusOK)
	if _, err := i.e.db.Exec(`DELETE FROM integrations WHERE id = ?`, i.stripe); err == nil {
		t.Fatal("an integration with an approval was deleted")
	}
	// A decided approval holds nothing.
	rows, _ := i.e.db.Query(`SELECT id FROM app_integration_approvals`)
	var id int64
	for rows.Next() {
		_ = rows.Scan(&id)
	}
	_ = rows.Close()
	expect(t, i.decide(i.owner, i.ws, id, "revoke", nil), http.StatusOK)
	if _, err := i.e.db.Exec(`DELETE FROM integrations WHERE id = ?`, i.stripe); err != nil {
		t.Fatalf("an integration with only decided approvals: %v", err)
	}
}

func TestTheApprovalCannotBeBoundAcrossWorkspaces(t *testing.T) {
	i := newInv(t)
	req := i.mustRequest(i.alice, i.shared, i.app)
	foreign := i.integration(i.other, "stripe", "theirs")
	if _, err := i.e.db.Exec(`UPDATE app_integration_approvals SET status = 'approved', integration_id = ?, decided_at = CURRENT_TIMESTAMP WHERE id = ?`, foreign, req.ID); err == nil {
		t.Fatal("the database let an app use another workspace's integration")
	}
	if _, err := i.e.db.Exec(`UPDATE app_integration_approvals SET contract_hash = ? WHERE id = ?`, strings.Repeat("a", 64), req.ID); err == nil {
		t.Fatal("the database let an approval's contract change")
	}
}

// In a one-person install the publisher is the admin: the same calls, by the same person, are the
// "approve and publish" step.
func TestOnePersonApprovesAndPublishes(t *testing.T) {
	i := newInv(t)
	rec := i.owner.do(http.MethodPost, appsPath(i.shared, ""), map[string]any{"name": "Mine", "definition": invoices(t)})
	expect(t, rec, http.StatusCreated)
	app := decode[App](t, rec)
	req := i.mustRequest(i.owner, i.shared, app)
	expect(t, i.decide(i.owner, i.ws, req.ID, "approve", map[string]any{"integration_id": i.stripe, "allow_space": true}), http.StatusOK)
	expect(t, i.tryPublish(i.owner, i.shared, app), http.StatusOK)
}

func TestAnAppWithNoIntegrationsPublishesAsAlways(t *testing.T) {
	w := newWorld(t)
	if got := w.view(w.alice, w.shared, w.sharedApp); got.Live == nil {
		t.Fatal("not live")
	}
	app := w.draft(w.alice, w.shared, "Plain")
	expect(t, w.tryPublish(w.alice, w.shared, app), http.StatusOK)
}

// ----- audit -----

func TestEveryDecisionIsAudited(t *testing.T) {
	i := newInv(t)
	i.allow(i.stripe, i.shared)
	// A slot named unlike its kind, so the details can be checked for the name on its own.
	till := strings.NewReplacer(`"integration": "payments"`, `"integration": "till"`, `"integrations": {"payments"`, `"integrations": {"till"`).Replace(invoiceJSON)
	rec := i.alice.do(http.MethodPost, appsPath(i.shared, ""), map[string]any{"name": "Till", "definition": json.RawMessage(till)})
	expect(t, rec, http.StatusCreated)
	app := decode[App](t, rec)
	ask := func() IntegrationApproval {
		st := i.status(i.alice, i.shared, app)
		if len(st.Slots) != 1 || st.Slots[0].Slot != "till" {
			t.Fatalf("slots = %+v", st.Slots)
		}
		rec := i.request(i.alice, i.shared, app, "till", st.Slots[0].ContractHash)
		expect(t, rec, http.StatusOK)
		return decode[IntegrationApproval](t, rec)
	}
	req := ask()
	// A member's decision is refused and leaves no audit event, nor changes the request.
	audited := func() int { return i.count(`SELECT count(*) FROM audit_events WHERE action LIKE 'app_integration_%'`) }
	before := audited()
	expect(t, i.approve(i.bob, req.ID, i.stripe), http.StatusForbidden)
	expect(t, i.decide(i.bob, i.ws, req.ID, "reject", nil), http.StatusForbidden)
	expect(t, i.decide(i.bob, i.ws, req.ID, "revoke", nil), http.StatusForbidden)
	if got := audited(); got != before {
		t.Fatalf("refused decisions wrote %d audit events", got-before)
	}
	expect(t, i.decide(i.owner, i.ws, req.ID, "reject", nil), http.StatusOK)
	renewed := ask()
	expect(t, i.approve(i.owner, renewed.ID, i.stripe), http.StatusOK)
	expect(t, i.decide(i.owner, i.ws, renewed.ID, "revoke", nil), http.StatusOK)

	rows, err := i.e.db.Query(`SELECT action, actor_id, target_type, target_id, details FROM audit_events WHERE action LIKE 'app_integration_%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var actions []string
	targets := []int64{req.ID, req.ID, renewed.ID, renewed.ID, renewed.ID}
	for rows.Next() {
		var action, targetType, details string
		var actor, target int64
		if err := rows.Scan(&action, &actor, &targetType, &target, &details); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, action)
		if targetType != "app_integration_approval" || len(actions) > len(targets) || target != targets[len(actions)-1] {
			t.Errorf("%s targets %s %d", action, targetType, target)
		}
		want := i.owner.id
		if strings.HasSuffix(action, "requested") {
			want = i.alice.id
		}
		if actor != want {
			t.Errorf("%s by %d, want %d", action, actor, want)
		}
		// The slot's name is the builder's text and a secret is nobody's here.
		if strings.Contains(details, "till") {
			t.Errorf("%s details name the slot: %s", action, details)
		}
	}
	want := []string{"app_integration_requested", "app_integration_rejected", "app_integration_requested", "app_integration_approved", "app_integration_revoked"}
	if !slices.Equal(actions, want) {
		t.Fatalf("audited %v, want %v", actions, want)
	}
}

// ----- review follow-ups -----

// variant decodes the invoices' JSON with the pairs (old, new, old, new, …) replaced and checks it.
func variant(t *testing.T, pairs ...string) (Definition, error) {
	t.Helper()
	raw := invoiceJSON
	for rest := pairs; len(rest) >= 2; rest = rest[2:] {
		from, to := rest[0], rest[1]
		if !strings.Contains(raw, from) {
			t.Fatalf("the invoice definition has no %q", from)
		}
		raw = strings.Replace(raw, from, to, 1)
	}
	var d Definition
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return Definition{}, err
	}
	return d.Check()
}

func TestASetIsExactlyOneKey(t *testing.T) {
	for name, set := range map[string]string{
		"a literal false":  `{"value": false}`,
		"a literal number": `{"value": 0}`,
		"a source":         `{"from": "occurred_at"}`,
	} {
		var s IntegrationSet
		if err := json.Unmarshal([]byte(set), &s); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, set := range map[string]string{
		"a null value and a source": `{"value": null, "from": "occurred_at"}`,
		"a value and an empty from": `{"value": "paid", "from": ""}`,
		"an empty from":             `{"from": ""}`,
		"a null value":              `{"value": null}`,
		"a value and a null from":   `{"value": "paid", "from": null}`,
		"a null from":               `{"from": null}`,
		"nothing":                   `{}`,
	} {
		var s IntegrationSet
		if err := json.Unmarshal([]byte(set), &s); err == nil {
			t.Errorf("%s was accepted: %+v", name, s)
		}
	}
}

func TestABlankLiteralIsRefusedAndAZeroIsNot(t *testing.T) {
	_, err := variant(t, `"payment.expired": {"set": {"status": {"value": "sent"}}}`, `"payment.expired": {"set": {"customer": {"value": "  "}}}`)
	if err == nil || !strings.Contains(err.Error(), `"customer"`) || !strings.Contains(err.Error(), "blank") {
		t.Errorf("a blank literal: %v", err)
	}
	for name, set := range map[string]string{"zero": `{"total": {"value": 0}}`, "text": `{"customer": {"value": "x"}}`} {
		if _, err := variant(t, `"payment.expired": {"set": {"status": {"value": "sent"}}}`, `"payment.expired": {"set": `+set+`}`); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestACurrencyIsWrittenOnlyWhereTheChoiceOffersWhatTheAppAsksIn(t *testing.T) {
	field := `{"name": "paid_at", "type": "date"},`
	handler := `"payment.expired": {"set": {"status": {"value": "sent"}}}`
	write := `"payment.expired": {"set": {"settled_in": {"from": "currency"}}}`
	with := func(options, currency string) []string {
		return []string{field, field + `{"name": "settled_in", "type": "choice", "options": ` + options + `},`, handler, write,
			`"currency": "EUR"`, `"currency": "` + currency + `"`}
	}
	if _, err := variant(t, with(`["EUR", "USD"]`, "USD")...); err != nil {
		t.Errorf("a choice with both: %v", err)
	}
	if _, err := variant(t, with(`["EUR"]`, "EUR")...); err != nil {
		t.Errorf("a choice with the one asked in: %v", err)
	}
	if _, err := variant(t, with(`["EUR"]`, "USD")...); err == nil || !strings.Contains(err.Error(), "USD") {
		t.Errorf("a choice without USD for a slot asking in USD: %v", err)
	}
}

func TestMovingAWriteToAnotherEventIsAnotherContract(t *testing.T) {
	hash := func(d Definition) string { return d.contract("payments").Hash() }
	base, err := variant(t)
	if err != nil {
		t.Fatal(err)
	}
	// The values swap events: what each field may hold overall is the same, what each event does is not.
	raw := strings.NewReplacer(`{"value": "paid"}`, `{"value": "sent"}`, `{"value": "sent"}`, `{"value": "paid"}`).Replace(invoiceJSON)
	var d Definition
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatal(err)
	}
	swapped, err := d.Check()
	if err != nil {
		t.Fatal(err)
	}
	if a, b := base.contract("payments"), swapped.contract("payments"); !reflect.DeepEqual(a.Writes, b.Writes) {
		t.Fatalf("the writes differ, so the test proves nothing: %+v %+v", a.Writes, b.Writes)
	}
	if hash(base) == hash(swapped) {
		t.Error("swapping the values of two events left the contract's hash as it was")
	}
}

func TestTheQueueIsPagedAndReachesTheOldest(t *testing.T) {
	i := newInv(t)
	const total = maxApprovals + 5
	tx, err := i.e.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for n := range total {
		if _, err := tx.Exec(`INSERT INTO app_integration_approvals (app_id, slot, kind, contract_hash, contract, status, requested_by, decided_at)
			VALUES (?, 'payments', 'payments', printf('%064d', ?), '{}', 'rejected', ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))`, i.app.ID, n, i.alice.id); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	seen := map[int64]bool{}
	cursor, pages := "", 0
	for {
		rec := i.queue(i.owner, i.ws, "?status=all&cursor="+cursor)
		expect(t, rec, http.StatusOK)
		page := decode[ApprovalList](t, rec)
		if strings.Contains(rec.Body.String(), `"links":null`) || strings.Contains(rec.Body.String(), `"handlers":null`) {
			t.Fatalf("a stored contract with no links or handlers is sent as null: %.300s", rec.Body.String())
		}
		pages++
		if pages == 1 && len(page.Approvals) != maxApprovals {
			t.Fatalf("the first page has %d", len(page.Approvals))
		}
		for _, a := range page.Approvals {
			if seen[a.ID] {
				t.Fatalf("approval %d is on two pages", a.ID)
			}
			seen[a.ID] = true
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
		if pages > 5 {
			t.Fatal("the queue doesn't end")
		}
	}
	if len(seen) != total || pages != 2 {
		t.Errorf("reached %d approvals in %d pages, want %d in 2", len(seen), pages, total)
	}
	// The same cursor on the other statuses: all of these are rejected, none requested.
	rec := i.queue(i.owner, i.ws, "?status=rejected&cursor=1")
	if got := decode[ApprovalList](t, rec); len(got.Approvals) != 0 {
		t.Errorf("a cursor of the oldest id: %d approvals", len(got.Approvals))
	}
	if rec := i.queue(i.owner, i.ws, "?cursor=abc"); rec.Code != http.StatusBadRequest {
		t.Errorf("a bad cursor: %d", rec.Code)
	}
}

func TestDecisionsOfAnAdminDemotedMeanwhile(t *testing.T) {
	i := newInv(t)
	i.allow(i.stripe, i.shared)
	req := i.mustRequest(i.alice, i.shared, i.app)
	// A second admin, so the demotion of the first is allowed.
	if _, err := i.e.db.Exec(`UPDATE memberships SET role = 'admin' WHERE user_id = ? AND workspace_id = ?`, i.bob.id, i.ws); err != nil {
		t.Fatal(err)
	}
	if _, err := i.e.db.Exec(`UPDATE memberships SET role = 'member' WHERE user_id = ? AND workspace_id = ?`, i.owner.id, i.ws); err != nil {
		t.Fatal(err)
	}
	ctx := audit.WithActor(context.Background(), i.owner.id)
	for name, do := range map[string]func() error{
		"approve": func() error {
			_, err := i.e.s.approve(ctx, i.ws, i.owner.id, req.ID, Decision{IntegrationID: i.stripe, AllowSpace: true})
			return err
		},
		"reject": func() error { _, err := i.e.s.reject(ctx, i.ws, i.owner.id, req.ID); return err },
		"revoke": func() error { _, err := i.e.s.revoke(ctx, i.ws, i.owner.id, req.ID); return err },
	} {
		var ae *apierr.Error
		if err := do(); !errors.As(err, &ae) || ae.Code != apierr.CodeAdminOnly {
			t.Errorf("%s by a demoted admin = %v", name, err)
		}
	}
	if st := i.status(i.alice, i.shared, i.app); st.Slots[0].Status != ApprovalRequested {
		t.Fatalf("a demoted admin changed the request: %+v", st.Slots[0])
	}
}

func TestAnApprovalIdIsNeverGivenAgain(t *testing.T) {
	i := newInv(t)
	first := i.mustRequest(i.alice, i.shared, i.app)
	if _, err := i.e.db.Exec(`DELETE FROM app_integration_approvals WHERE id = ?`, first.ID); err != nil {
		t.Fatal(err)
	}
	second := i.mustRequest(i.alice, i.shared, i.app)
	if second.ID == first.ID {
		t.Errorf("the request after the deleted one got its id, %d", first.ID)
	}
	// A decision still on its way for the old id finds nothing.
	rec := i.approve(i.owner, first.ID, i.stripe)
	if rec.Code != http.StatusNotFound {
		t.Errorf("approving the deleted request's id: %d %s", rec.Code, rec.Body.String())
	}
}

func TestTheContractBindsEachLinkToItsActionCurrencyAndCondition(t *testing.T) {
	hash := func(d Definition) string { return d.contract("payments").Hash() }
	// Two actions ask for a link, in two currencies.
	twoActions := []string{`"add": {"kind": "insert"},`, `"add": {"kind": "insert"},
    "resend": {"kind": "update", "effects": [{"kind": "create_payment_link", "integration": "payments", "amount_field": "total",
      "currency": "USD", "description": "Reminder {{number}}", "url_field": "pay_url", "id_field": "payment_id"}]},`}
	base, err := variant(t, twoActions...)
	if err != nil {
		t.Fatal(err)
	}
	// The currencies swap between the actions: the same ones are asked in, by other actions.
	swapped, err := variant(t, append(slices.Clone(twoActions), `"currency": "EUR"`, `"currency": "USD"`, `"currency": "USD"`, `"currency": "EUR"`)...)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(base.contract("payments").Currencies, swapped.contract("payments").Currencies) {
		t.Fatal("the currencies differ, so the test proves nothing")
	}
	if hash(base) == hash(swapped) {
		t.Error("swapping the currencies of two actions left the hash as it was")
	}
	// The fields a link's description names are bound to the link, not only to the slot.
	swappedText, err := variant(t, append(slices.Clone(twoActions), `"Invoice {{number}} for {{customer}}"`, `"Reminder {{number}}"`, `"Reminder {{number}}"`, `"Invoice {{number}} for {{customer}}"`)...)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(base.contract("payments").Sends, swappedText.contract("payments").Sends) {
		t.Fatal("the fields sent differ, so the test proves nothing")
	}
	if hash(base) == hash(swappedText) {
		t.Error("swapping the descriptions' fields between two actions left the hash as it was")
	}
	// The condition on the link is part of what was approved: dropping it needs a new approval.
	restricted, err := variant(t, `"kind": "create_payment_link", "integration": "payments"`,
		`"kind": "create_payment_link", "when": {"field": "status", "op": "eq", "value": "draft"}, "integration": "payments"`)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := variant(t)
	if hash(restricted) == hash(plain) {
		t.Error("a link's when is not in the contract")
	}
	// Wording stays out of it.
	reworded, err := variant(t, `"Invoice {{number}} for {{customer}}"`, `"Pay {{customer}}, {{number}}"`)
	if err != nil {
		t.Fatal(err)
	}
	if hash(reworded) != hash(plain) {
		t.Error("the wording of the description changed the contract")
	}
}

func TestTheQueueSurvivesAnAppDeletedBetweenReads(t *testing.T) {
	i := newInv(t)
	req := i.mustRequest(i.alice, i.shared, i.app)
	r := approvalRow{appID: i.app.ID + 1000, slot: "payments"}
	if live, err := isLive(context.Background(), i.e.db, r); err != nil || live {
		t.Errorf("isLive of a deleted app = %v, %v", live, err)
	}
	rec := i.queue(i.owner, i.ws, "")
	expect(t, rec, http.StatusOK)
	if got := decode[ApprovalList](t, rec); len(got.Approvals) != 1 || got.Approvals[0].ID != req.ID {
		t.Errorf("queue = %+v", got)
	}
}

// ----- review fixes: the contract covers what an edit can change -----

// received is the invoices with a number field the payment's verified amount is written to.
var received = []string{
	`{"name": "total", "type": "number", "required": true},`, `{"name": "total", "type": "number", "required": true},
    {"name": "received", "type": "number", "max": 1000},`,
	`"paid_at": {"from": "occurred_at"}`, `"paid_at": {"from": "occurred_at"}, "received": {"from": "amount"}`,
}

// withSKU names a barcode of a stated format in the payment's description.
var withSKU = []string{
	`{"name": "customer", "type": "string"},`, `{"name": "customer", "type": "string"},
    {"name": "sku", "type": "barcode", "format": "ean_13", "max_length": 13},`,
	`for {{customer}}"`, `for {{customer}} {{sku}}"`,
}

// withQuantity makes the description's customer required by a rule over a quantity that the
// integration itself never reads.
var withQuantity = []string{
	`{"name": "customer", "type": "string"},`, `{"name": "customer", "type": "string", "required_when": {"field": "quantity", "op": "gt", "value": 50}},
    {"name": "quantity", "type": "number", "max": 100},`,
}

// withChain is the same one step further: the quantity's own requirement reads the batch.
var withChain = []string{
	`{"name": "customer", "type": "string"},`, `{"name": "customer", "type": "string", "required_when": {"field": "quantity", "op": "gt", "value": 50}},
    {"name": "quantity", "type": "number", "required_when": {"field": "batch", "op": "gt", "value": 5}},
    {"name": "batch", "type": "number", "max": 100},`,
}

// withReceiptNote adds a field the integration never names, required once the invoice is paid: a
// system write validates the whole row, so a sent invoice without a note refuses the integration's
// move to paid.
var withReceiptNote = []string{
	`{"name": "customer", "type": "string"},`, `{"name": "customer", "type": "string"},
    {"name": "receipt_note", "type": "string", "required_when": {"field": "status", "op": "eq", "value": "paid"}},`,
}

// Each of these edits changes what the integration may do or who can trigger it, so its approval
// must not carry over.
func TestAnEditThatChangesWhatTheIntegrationMayDoNeedsAFreshApproval(t *testing.T) {
	for name, tc := range map[string]struct{ base, edit []string }{
		"the record's id in the description":                   {nil, []string{"Invoice {{number}} for {{customer}}", "Invoice {{number}} for {{customer}} {{id}}"}},
		"an action that inserts, not updates":                  {nil, []string{`"send": {"kind": "update"`, `"send": {"kind": "insert"`}},
		"a higher maximum of a field written":                  {received, []string{`"max": 1000`, `"max": 100000`}},
		"a longer text field sent out":                         {nil, []string{`{"name": "customer", "type": "string"}`, `{"name": "customer", "type": "string", "max_length": 5000}`}},
		"another option of a choice written":                   {nil, []string{`"options": ["draft", "sent", "overdue", "paid"]`, `"options": ["draft", "sent", "overdue", "paid", "void"]`}},
		"a field no longer required":                           {nil, []string{`{"name": "total", "type": "number", "required": true}`, `{"name": "total", "type": "number"}`}},
		"a barcode's format dropped":                           {withSKU, []string{`"format": "ean_13", `, ``}},
		"a barcode's format changed":                           {withSKU, []string{`"format": "ean_13"`, `"format": "upc_a"`}},
		"a higher maximum of a field only a requirement reads": {withQuantity, []string{`"max": 100`, `"max": 1000`}},
		"a requirement's field two steps away":                 {withChain, []string{`"max": 100`, `"max": 1000`}},
		"a requirement on a field the integration writes, dropped": {withReceiptNote, []string{
			`, "required_when": {"field": "status", "op": "eq", "value": "paid"}`, ``}},
		"a requirement on a field the integration writes, added": {nil, []string{
			`{"name": "customer", "type": "string"},`, `{"name": "customer", "type": "string"},
    {"name": "receipt_note", "type": "string", "required_when": {"field": "status", "op": "eq", "value": "paid"}},`}},
	} {
		t.Run(name, func(t *testing.T) {
			i := newInv(t)
			i.allow(i.stripe, i.shared)
			patch := func(d Definition) {
				t.Helper()
				app := i.fresh(i.alice, i.shared, i.app)
				expect(t, i.alice.do(http.MethodPatch, appPath(i.shared, app.ID, ""), map[string]any{"version": app.Version, "definition": d}), http.StatusOK)
			}
			base, err := variant(t, tc.base...)
			if err != nil {
				t.Fatal(err)
			}
			patch(base)
			expect(t, i.approve(i.owner, i.mustRequest(i.alice, i.shared, i.app).ID, i.stripe), http.StatusOK)
			expect(t, i.tryPublish(i.alice, i.shared, i.fresh(i.alice, i.shared, i.app)), http.StatusOK)

			edited, err := variant(t, append(slices.Clone(tc.base), tc.edit...)...)
			if err != nil {
				t.Fatal(err)
			}
			if base.contract("payments").Hash() == edited.contract("payments").Hash() {
				t.Fatal("the edit left the contract's hash as it was")
			}
			patch(edited)
			if st := i.status(i.alice, i.shared, i.app); st.Slots[0].Status != ApprovalNone {
				t.Fatalf("the edit kept the approval: %+v", st.Slots[0])
			}
			expect(t, i.tryPublish(i.alice, i.shared, i.fresh(i.alice, i.shared, i.app)), http.StatusConflict)
		})
	}
}

// What is not an integration's business stays free: the wording of a field's options and of the
// description, and the constraints of a field it never touches.
func TestWordingAndOtherFieldsDoNotChangeTheContract(t *testing.T) {
	hash := func(d Definition, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return d.contract("payments").Hash()
	}
	h := hash(variant(t))
	for name, pairs := range map[string][]string{
		"an option's label":  {`"options": ["draft", "sent", "overdue", "paid"]}`, `"options": ["draft", "sent", "overdue", "paid"], "option_labels": {"paid": "Settled"}}`},
		"the description":    {"Invoice {{number}} for {{customer}}", "Pay {{number}}, {{customer}}"},
		"a field never used": {`{"name": "payment_id"`, `{"name": "notes", "type": "string", "max_length": 10}, {"name": "payment_id"`},
	} {
		if got := hash(variant(t, pairs...)); got != h {
			t.Errorf("%s changed the contract", name)
		}
	}
}

// ----- review fixes: renewing a request -----

func TestARenewedRequestIsANewOneAndAStaleDecisionNeverReachesIt(t *testing.T) {
	i := newInv(t)
	i.allow(i.stripe, i.shared)
	first := i.mustRequest(i.alice, i.shared, i.app)
	expect(t, i.approve(i.owner, first.ID, i.stripe), http.StatusOK)
	expect(t, i.decide(i.owner, i.ws, first.ID, "revoke", nil), http.StatusOK)

	renewed := i.mustRequest(i.alice, i.shared, i.app)
	if renewed.ID == first.ID {
		t.Fatalf("the renewal reused id %d", first.ID)
	}
	expect(t, i.approve(i.owner, renewed.ID, i.stripe), http.StatusOK)

	// A revoke still on its way for the first id is refused and leaves the new approval.
	rec := i.decide(i.owner, i.ws, first.ID, "revoke", nil)
	expect(t, rec, http.StatusConflict)
	if code(t, rec) != string(apierr.CodeAppIntegrationNotApproved) {
		t.Fatalf("a stale revoke: %s", rec.Body.String())
	}
	expect(t, i.approve(i.owner, first.ID, i.stripe), http.StatusConflict)
	if st := i.status(i.alice, i.shared, i.app); st.Slots[0].Status != ApprovalApproved {
		t.Fatalf("the stale revoke reached the new approval: %+v", st.Slots[0])
	}
	if _, err := i.e.s.Binding(i.ctx(), i.app.ID, "payments"); err == nil {
		t.Log("(not live yet: the draft isn't published)")
	}
	expect(t, i.tryPublish(i.alice, i.shared, i.app), http.StatusOK)

	// The old row keeps its decision.
	list := decode[ApprovalList](t, i.queue(i.owner, i.ws, "?status=revoked"))
	if len(list.Approvals) != 1 || list.Approvals[0].ID != first.ID {
		t.Fatalf("the revoked ones: %+v", list.Approvals)
	}
}

func TestARenewedRequestQueuesByWhenItWasMade(t *testing.T) {
	i := newInv(t)
	first := i.mustRequest(i.alice, i.shared, i.app)
	expect(t, i.decide(i.owner, i.ws, first.ID, "reject", nil), http.StatusOK)

	// Another app asks while the first's request is turned down.
	rec := i.alice.do(http.MethodPost, appsPath(i.shared, ""), map[string]any{"name": "Other", "definition": invoices(t)})
	expect(t, rec, http.StatusCreated)
	other := i.mustRequest(i.alice, i.shared, decode[App](t, rec))

	renewed := i.mustRequest(i.alice, i.shared, i.app)
	list := decode[ApprovalList](t, i.queue(i.owner, i.ws, ""))
	if len(list.Approvals) != 2 || list.Approvals[0].ID != renewed.ID || list.Approvals[1].ID != other.ID || renewed.ID < other.ID {
		t.Fatalf("the queue, newest first: %+v (renewed %d, other %d)", list.Approvals, renewed.ID, other.ID)
	}
}

// ----- review fixes: the space an approval allowed goes with it -----

func (i *inv) grants(integration int64) []int64 {
	i.e.t.Helper()
	rows, err := i.e.db.Query(`SELECT space_id FROM integration_spaces WHERE integration_id = ? ORDER BY space_id`, integration)
	if err != nil {
		i.e.t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			i.e.t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

func (i *inv) privateApp() App {
	i.e.t.Helper()
	rec := i.alice.do(http.MethodPost, appsPath(i.alicePrivate, ""), map[string]any{"name": "My invoices", "definition": invoices(i.e.t)})
	expect(i.e.t, rec, http.StatusCreated)
	return decode[App](i.e.t, rec)
}

func TestRevokingAnApprovalTakesBackTheSpaceItAllowed(t *testing.T) {
	i := newInv(t)
	app := i.privateApp()
	req := i.mustRequest(i.alice, i.alicePrivate, app)
	expect(t, i.decide(i.owner, i.ws, req.ID, "approve", map[string]any{"integration_id": i.stripe, "allow_space": true}), http.StatusOK)
	expect(t, i.tryPublish(i.alice, i.alicePrivate, app), http.StatusOK)
	if got := i.grants(i.stripe); !slices.Equal(got, []int64{i.alicePrivate.ID}) {
		t.Fatalf("grants after approving = %v", got)
	}
	if _, err := i.e.s.Binding(i.ctx(), app.ID, "payments"); err != nil {
		t.Fatalf("the approved app has no binding: %v", err)
	}

	expect(t, i.decide(i.owner, i.ws, req.ID, "revoke", nil), http.StatusOK)
	if got := i.grants(i.stripe); len(got) != 0 {
		t.Fatalf("a grant is left after the revoke: %v", got)
	}
	if _, err := i.e.s.Binding(i.ctx(), app.ID, "payments"); !errors.Is(err, ErrIntegrationUnapproved) {
		t.Fatalf("the app still has its binding: %v", err)
	}
	// Approving again, without allow_space, can't use the space: the admin has to say so again.
	renewed := i.mustRequest(i.alice, i.alicePrivate, i.fresh(i.alice, i.alicePrivate, app))
	rec := i.approve(i.owner, renewed.ID, i.stripe)
	expect(t, rec, http.StatusConflict)
	if code(t, rec) != string(apierr.CodeAppIntegrationSpaceNotAllowed) {
		t.Fatalf("approving into a space that is no longer allowed: %s", rec.Body.String())
	}
}

func TestRevokingAnApprovalLeavesASpaceAnAdminAllowed(t *testing.T) {
	i := newInv(t)
	i.allow(i.stripe, i.shared) // the admin's own grant, made in the integration's settings
	req := i.mustRequest(i.alice, i.shared, i.app)
	expect(t, i.decide(i.owner, i.ws, req.ID, "approve", map[string]any{"integration_id": i.stripe, "allow_space": true}), http.StatusOK)
	expect(t, i.decide(i.owner, i.ws, req.ID, "revoke", nil), http.StatusOK)
	if got := i.grants(i.stripe); !slices.Equal(got, []int64{i.shared.ID}) {
		t.Fatalf("the admin's grant after the revoke = %v", got)
	}
}

func TestASpaceTwoApprovalsNeedIsKeptUntilTheLastIsRevoked(t *testing.T) {
	i := newInv(t)
	one := i.privateApp()
	two := i.privateApp()
	a := i.mustRequest(i.alice, i.alicePrivate, one)
	expect(t, i.decide(i.owner, i.ws, a.ID, "approve", map[string]any{"integration_id": i.stripe, "allow_space": true}), http.StatusOK)
	b := i.mustRequest(i.alice, i.alicePrivate, two)
	expect(t, i.approve(i.owner, b.ID, i.stripe), http.StatusOK)

	expect(t, i.decide(i.owner, i.ws, a.ID, "revoke", nil), http.StatusOK)
	if got := i.grants(i.stripe); !slices.Equal(got, []int64{i.alicePrivate.ID}) {
		t.Fatalf("the other app's approval lost its space: %v", got)
	}
	expect(t, i.decide(i.owner, i.ws, b.ID, "revoke", nil), http.StatusOK)
	if got := i.grants(i.stripe); len(got) != 0 {
		t.Fatalf("a grant is left after the last revoke: %v", got)
	}
}

func TestMovingOrDeletingAnAppTakesBackTheSpaceItsApprovalAllowed(t *testing.T) {
	i := newInv(t)
	app := i.privateApp()
	req := i.mustRequest(i.alice, i.alicePrivate, app)
	expect(t, i.decide(i.owner, i.ws, req.ID, "approve", map[string]any{"integration_id": i.stripe, "allow_space": true}), http.StatusOK)
	expect(t, i.alice.do(http.MethodPost, appPath(i.alicePrivate, app.ID, "/move"), map[string]any{"version": app.Version, "space_id": i.shared.ID}), http.StatusOK)
	if got := i.grants(i.stripe); len(got) != 0 {
		t.Fatalf("a grant is left after the app moved: %v", got)
	}

	other := i.privateApp()
	req = i.mustRequest(i.alice, i.alicePrivate, other)
	expect(t, i.decide(i.owner, i.ws, req.ID, "approve", map[string]any{"integration_id": i.stripe, "allow_space": true}), http.StatusOK)
	if _, err := i.e.db.Exec(`DELETE FROM apps WHERE id = ?`, other.ID); err != nil {
		t.Fatal(err)
	}
	if got := i.grants(i.stripe); len(got) != 0 {
		t.Fatalf("a grant is left after the app was deleted: %v", got)
	}
}

// ----- review fixes: saving an integration's spaces never adopts an approval's grant -----

// approvedFromApproval has the admin approve the shared app's contract and allow its space in the
// same decision, so the grant is the approval's.
func (i *inv) approvedFromApproval() int64 {
	i.e.t.Helper()
	req := i.mustRequest(i.alice, i.shared, i.app)
	expect(i.e.t, i.decide(i.owner, i.ws, req.ID, "approve", map[string]any{"integration_id": i.stripe, "allow_space": true}), http.StatusOK)
	return req.ID
}

func (i *inv) spacesPath() string {
	return fmt.Sprintf("/api/workspaces/%d/integrations/%d/spaces", i.ws, i.stripe)
}

func (i *inv) fromApproval() []int64 {
	i.e.t.Helper()
	rows, err := i.e.db.Query(`SELECT space_id FROM integration_spaces WHERE integration_id = ? AND from_approval = 1 ORDER BY space_id`, i.stripe)
	if err != nil {
		i.e.t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			i.e.t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

func (i *inv) keptEvents() int {
	i.e.t.Helper()
	rows, err := audit.List(context.Background(), i.e.db, audit.Filter{Workspace: i.ws, Action: audit.IntegrationSpacesKept, Limit: 100})
	if err != nil {
		i.e.t.Fatal(err)
	}
	return len(rows)
}

func TestSavingTheSettingsUnchangedLeavesAnApprovalsGrantTheApprovals(t *testing.T) {
	i := newInv(t)
	req := i.approvedFromApproval()
	expect(t, i.tryPublish(i.alice, i.shared, i.app), http.StatusOK)

	// The settings form sends every checked space, the approval's included: nothing is adopted.
	rec := i.owner.do(http.MethodPut, i.spacesPath(), map[string]any{"spaces": []int64{i.shared.ID}})
	expect(t, rec, http.StatusOK)
	if got := decode[integrations.Integration](t, rec); !slices.Equal(got.ApprovalSpaces, []int64{i.shared.ID}) {
		t.Fatalf("the integration doesn't say the space is an approval's: %+v", got)
	}
	if got := i.fromApproval(); !slices.Equal(got, []int64{i.shared.ID}) {
		t.Fatalf("a save adopted the approval's grant: from_approval = %v", got)
	}
	if n := i.keptEvents(); n != 0 {
		t.Fatalf("%d adoption events for a save that adopted nothing", n)
	}

	// So revoking the approval still takes the space back.
	expect(t, i.decide(i.owner, i.ws, req, "revoke", nil), http.StatusOK)
	if got := i.grants(i.stripe); len(got) != 0 {
		t.Fatalf("a grant is left after the revoke: %v", got)
	}
}

func TestKeepingAnApprovalsSpaceIsExplicitAndAudited(t *testing.T) {
	i := newInv(t)
	req := i.approvedFromApproval()

	// keep names only spaces the list holds.
	expect(t, i.owner.do(http.MethodPut, i.spacesPath(), map[string]any{"spaces": []int64{}, "keep": []int64{i.shared.ID}}), http.StatusBadRequest)
	if got := i.fromApproval(); !slices.Equal(got, []int64{i.shared.ID}) {
		t.Fatalf("a refused request changed the grant: %v", got)
	}

	rec := i.owner.do(http.MethodPut, i.spacesPath(), map[string]any{"spaces": []int64{i.shared.ID}, "keep": []int64{i.shared.ID}})
	expect(t, rec, http.StatusOK)
	if got := decode[integrations.Integration](t, rec); len(got.ApprovalSpaces) != 0 || !slices.Equal(got.Spaces, []int64{i.shared.ID}) {
		t.Fatalf("after keeping: %+v", got)
	}
	if got := i.fromApproval(); len(got) != 0 {
		t.Fatalf("the kept space is still the approval's: %v", got)
	}
	if n := i.keptEvents(); n != 1 {
		t.Fatalf("%d adoption events, want 1", n)
	}
	// Asking again changes nothing and records nothing.
	expect(t, i.owner.do(http.MethodPut, i.spacesPath(), map[string]any{"spaces": []int64{i.shared.ID}, "keep": []int64{i.shared.ID}}), http.StatusOK)
	if n := i.keptEvents(); n != 1 {
		t.Fatalf("%d adoption events after asking again, want 1", n)
	}

	// The approval ending leaves the admin's own grant.
	expect(t, i.decide(i.owner, i.ws, req, "revoke", nil), http.StatusOK)
	if got := i.grants(i.stripe); !slices.Equal(got, []int64{i.shared.ID}) {
		t.Fatalf("the kept grant after the revoke = %v", got)
	}
}

func TestOnlyAnAdminMayKeepAnApprovalsSpace(t *testing.T) {
	i := newInv(t)
	i.approvedFromApproval()
	expect(t, i.alice.do(http.MethodPut, i.spacesPath(), map[string]any{"spaces": []int64{i.shared.ID}, "keep": []int64{i.shared.ID}}), http.StatusForbidden)
	if got := i.fromApproval(); !slices.Equal(got, []int64{i.shared.ID}) {
		t.Fatalf("a member kept the grant: %v", got)
	}
}

// The builder's preview says what a payment link would ask for, from the sample record, and does
// nothing else: no entry queued, no approval needed, no row written.
func TestThePreviewSaysThePaymentLinkWithoutMakingOne(t *testing.T) {
	i := newInv(t)
	s := i.shared
	rows := []PreviewRow{{ID: 1, Revision: 1, Data: map[string]any{
		"number": "A-1", "customer": "Ada", "total": float64(120), "currency": "EUR", "status": "draft"}}}
	send := &ClientAction{Name: "send", SurfaceID: SurfaceID(i.app.ID), Context: map[string]any{"id": float64(1)}}
	before := everythingStored(t, i.e)

	// Nothing is approved, and the preview works all the same.
	reply := i.mustPreview(i.alice, s, i.app, PreviewRequest{Rows: rows, Action: send})
	if len(reply.Effects) != 1 {
		t.Fatalf("effects = %+v", reply.Effects)
	}
	e := reply.Effects[0]
	if e.Kind != EffectCreatePaymentLink || e.Status != StateSimulated || e.Error != nil || e.Detail.Currency != "EUR" ||
		e.Detail.Amount != "120.00" {
		t.Fatalf("the simulated link = %+v", e)
	}
	// The same through the endpoint that only simulates the effects.
	rec := i.alice.do(http.MethodPost, appPath(s, i.app.ID, "/actions/preview"), map[string]any{
		"name": "send", "surfaceId": SurfaceID(i.app.ID), "context": map[string]any{"id": float64(1)}, "rows": rows})
	expect(t, rec, http.StatusOK)
	p := decode[Preview](t, rec)
	if len(p.Effects) != 1 {
		t.Fatalf("simulated = %+v", p)
	}
	if e := p.Effects[0]; e.Kind != EffectCreatePaymentLink || e.Status != StateSimulated || e.Detail.Currency != "EUR" ||
		e.Detail.Amount != "120.00" {
		t.Fatalf("the simulated link through the actions route = %+v", e)
	}

	// It made nothing real and queued nothing.
	if got := everythingStored(t, i.e); got != before {
		t.Fatalf("the preview changed what is stored:\n%s\n--- now\n%s", before, got)
	}
	if n := i.count(`SELECT count(*) FROM app_effects WHERE app_id = ?`, i.app.ID); n != 0 {
		t.Fatalf("a preview queued %d effects", n)
	}

	// Only those who may design or publish the app preview it: another member of the space, and
	// someone outside it, are refused as before.
	expect(t, i.bob.do(http.MethodPost, appPath(s, i.app.ID, "/preview"), PreviewRequest{Rows: rows, Action: send}), http.StatusForbidden)
	expect(t, i.mallory.do(http.MethodPost, appPath(s, i.app.ID, "/preview"), PreviewRequest{Rows: rows, Action: send}), http.StatusNotFound)
}

// ----- review fixes: the queue says, for an approval's own space only, how the space is allowed -----

// queued is the approval with this id as the admin's queue shows it, read from the raw reply.
func (i *inv) queued(id int64) map[string]any {
	i.e.t.Helper()
	rec := i.queue(i.owner, i.ws, "?status=all")
	expect(i.e.t, rec, http.StatusOK)
	var page struct {
		Approvals []map[string]any `json:"approvals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		i.e.t.Fatal(err)
	}
	for _, a := range page.Approvals {
		if int64(a["id"].(float64)) == id {
			return a
		}
	}
	i.e.t.Fatalf("approval %d is not in the queue: %s", id, rec.Body.String())
	return nil
}

func (i *inv) spaceGrant(id int64) string {
	i.e.t.Helper()
	got, _ := i.queued(id)["space_grant"].(string)
	return got
}

func TestTheQueueSaysHowAnApprovalsSpaceIsAllowed(t *testing.T) {
	i := newInv(t)
	private := i.privateApp()

	// Waiting: nothing is bound, so there is no grant to speak of, even when an integration is allowed.
	waiting := i.mustRequest(i.alice, i.alicePrivate, private)
	if got := i.spaceGrant(waiting.ID); got != "none" {
		t.Fatalf("a waiting request: space_grant = %q", got)
	}

	// Approved with the space allowed by the approval itself (another person's private space, which
	// the admin can't open): the grant is the approval's.
	expect(t, i.decide(i.owner, i.ws, waiting.ID, "approve", map[string]any{"integration_id": i.stripe, "allow_space": true}), http.StatusOK)
	if got := i.spaceGrant(waiting.ID); got != "approval" {
		t.Fatalf("approved with allow_space: space_grant = %q", got)
	}
	// Approved in a space the admin had allowed in the settings: an approval never takes that away.
	i.allow(i.stripe, i.shared)
	shared := i.mustRequest(i.alice, i.shared, i.app)
	expect(t, i.approve(i.owner, shared.ID, i.stripe), http.StatusOK)
	if got := i.spaceGrant(shared.ID); got != "settings" {
		t.Fatalf("approved in a space the admin allowed: space_grant = %q", got)
	}
	// Each approval says it of its own space: the private one is still the approval's.
	if got := i.spaceGrant(waiting.ID); got != "approval" {
		t.Fatalf("the private space's approval after the shared one: space_grant = %q", got)
	}

	// An approval whose space an admin took the grant away from (the integration's settings) says none.
	if _, err := i.e.db.Exec(`DELETE FROM integration_spaces WHERE integration_id = ? AND space_id = ?`, i.stripe, i.shared.ID); err != nil {
		t.Fatal(err)
	}
	if got := i.spaceGrant(shared.ID); got != "none" {
		t.Fatalf("approved, the space no longer allowed: space_grant = %q", got)
	}

	// Another integration allowed in the space is not this approval's: it reads the one it is bound to.
	other := i.integration(i.ws, "stripe", "second")
	i.allow(other, i.shared)
	if got := i.spaceGrant(shared.ID); got != "none" {
		t.Fatalf("another integration allowed in the space: space_grant = %q", got)
	}

	// Revoked: unbound, and the grant it made is gone.
	expect(t, i.decide(i.owner, i.ws, waiting.ID, "revoke", nil), http.StatusOK)
	if got := i.spaceGrant(waiting.ID); got != "none" {
		t.Fatalf("revoked: space_grant = %q", got)
	}
}

// The queue's reply carries the one fact about the approval's own space, and nothing about any
// other: the keys are the approval's, and the decision's reply and the requester's carry the same.
func TestTheQueueRevealsNothingAboutOtherSpaces(t *testing.T) {
	i := newInv(t)
	private := i.privateApp()
	mine := i.mustRequest(i.alice, i.alicePrivate, private)
	expect(t, i.decide(i.owner, i.ws, mine.ID, "approve", map[string]any{"integration_id": i.stripe, "allow_space": true}), http.StatusOK)
	// The integration is also allowed in other spaces, which no approval of this queue is in.
	i.allow(i.stripe, i.shared)

	got := i.queued(mine.ID)
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{"app_id", "app_name", "contract", "contract_hash", "decided_at", "decided_by", "id", "integration", "kind", "live",
		"requested_at", "requested_by", "slot", "space_grant", "space_id", "space_kind", "space_name", "status"}
	if !slices.Equal(keys, want) {
		t.Fatalf("the approval's keys = %v, want %v", keys, want)
	}
	if got["space_grant"] != "approval" || int64(got["space_id"].(float64)) != i.alicePrivate.ID {
		t.Fatalf("the approval = %v", got)
	}

	// The decision's own reply says the grant, too.
	again := i.mustRequest(i.alice, i.shared, i.app)
	rec := i.approve(i.owner, again.ID, i.stripe)
	expect(t, rec, http.StatusOK)
	if got := decode[IntegrationApproval](t, rec); got.SpaceGrant != "settings" {
		t.Fatalf("the decision's reply: space_grant = %q", got.SpaceGrant)
	}
}

// A payment link simulated for a record an insert is making says "new", with no record number, as
// the other effects of an insert do: the record has no number yet.
func TestAPreviewedLinkOfAnInsertHasNoRecordNumberAndOfAnUpdateHasIt(t *testing.T) {
	i := newInv(t)
	def := invoices(t)
	link := def.Actions["send"].Effects
	add := def.Actions["add"]
	add.Effects = link
	def.Actions["add"] = add
	rows := []Row{{ID: 7, Revision: 1, Data: map[string]any{"number": "A-7", "total": float64(50), "currency": "EUR", "status": "draft"}}}

	got := i.e.s.simulatedEffects(i.ctx(), 1, def, Change{Kind: ActionInsert, Name: "add"}, rows)
	if len(got) != 1 || got[0].Kind != EffectCreatePaymentLink || got[0].Record != 0 {
		t.Fatalf("the link of an insert = %+v", got)
	}
	got = i.e.s.simulatedEffects(i.ctx(), 1, def, Change{Kind: ActionUpdate, Name: "send", ID: 7}, rows)
	if len(got) != 1 || got[0].Record != 7 {
		t.Fatalf("the link of an update = %+v", got)
	}
}

// Asking for an approval is the publisher's: a space editor whose roles in the app let them design it,
// or only use it, but not publish it, is refused with the capability that is missing. (A viewer of
// the space is refused earlier, by the space.)
func TestAnEditorWithoutThePublishCapabilityCannotRequestAnApproval(t *testing.T) {
	for name, roles := range map[string][]access.AppRole{
		"a builder (design)": {access.AppBuilder},
		"a user (submit)":    {access.AppUser},
	} {
		t.Run(name, func(t *testing.T) {
			i := newInv(t)
			hash := i.status(i.alice, i.shared, i.app).Slots[0].ContractHash
			// Another editor keeps the management of the roles, so the one under test is left with only hers.
			i.e.addMember(i.owner, i.shared, i.mallory, access.RoleEditor)
			i.mustSetRoles(i.mallory, i.shared, i.app, grant(i.alice.id, roles...), grant(i.mallory.id, access.AppBuilder))
			rec := i.request(i.alice, i.shared, i.app, "payments", hash)
			expectForbidden(t, rec, string(access.CapPublish))
			if n := i.count(`SELECT count(*) FROM app_integration_approvals WHERE app_id = ?`, i.app.ID); n != 0 {
				t.Fatalf("a refused request left %d approvals", n)
			}
			// With the publisher role added she can: it is the capability that decides, not the space role.
			i.mustSetRoles(i.mallory, i.shared, i.app, grant(i.alice.id, append(roles, access.AppPublisher)...), grant(i.mallory.id, access.AppBuilder))
			expect(t, i.request(i.alice, i.shared, i.app, "payments", hash), http.StatusOK)
		})
	}
}
