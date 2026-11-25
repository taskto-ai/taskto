package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
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
		"a slot with a bad name":    {`"integrations": {"payments"`, `"integrations": {"Pay Ments"`},
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
	if again := i.mustRequest(i.alice, i.shared, i.app); again.ID != first.ID || again.Status != ApprovalRequested || again.DecidedBy != nil {
		t.Fatalf("asking again after a rejection: %+v", again)
	}

	// Only an approval can be revoked.
	rec = i.decide(i.owner, i.ws, first.ID, "revoke", nil)
	expect(t, rec, http.StatusConflict)
	if code(t, rec) != string(apierr.CodeAppIntegrationNotApproved) {
		t.Fatalf("revoking a request: %s", rec.Body.String())
	}
	expect(t, i.approve(i.owner, first.ID, i.stripe), http.StatusOK)
	expect(t, i.approve(i.owner, first.ID, i.stripe), http.StatusConflict)
	expect(t, i.decide(i.owner, i.ws, first.ID, "revoke", nil), http.StatusOK)
	expect(t, i.tryPublish(i.alice, i.shared, i.app), http.StatusConflict)
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
	req := i.mustRequest(i.alice, i.shared, i.app)
	expect(t, i.decide(i.owner, i.ws, req.ID, "reject", nil), http.StatusOK)
	i.mustRequest(i.alice, i.shared, i.app)
	expect(t, i.approve(i.owner, req.ID, i.stripe), http.StatusOK)
	expect(t, i.decide(i.owner, i.ws, req.ID, "revoke", nil), http.StatusOK)

	rows, err := i.e.db.Query(`SELECT action, actor_id, target_type, target_id, details FROM audit_events WHERE action LIKE 'app_integration_%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var actions []string
	for rows.Next() {
		var action, targetType, details string
		var actor, target int64
		if err := rows.Scan(&action, &actor, &targetType, &target, &details); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, action)
		if targetType != "app_integration_approval" || target != req.ID {
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
		if strings.Contains(details, "payments") && !strings.Contains(details, `"kind":"payments"`) {
			t.Errorf("%s details name the slot: %s", action, details)
		}
	}
	want := []string{"app_integration_requested", "app_integration_rejected", "app_integration_requested", "app_integration_approved", "app_integration_revoked"}
	if !slices.Equal(actions, want) {
		t.Fatalf("audited %v, want %v", actions, want)
	}
}
