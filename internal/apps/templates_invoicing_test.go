package apps

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/paylink"
)

const invoicingContractHash = "ae2432762ccb930adf45117acd1b6c9475de2b0e1eb68c6f29dd79c2343398a7"

func invoicingTemplate(t *testing.T) Template {
	t.Helper()
	all, err := Templates()
	if err != nil {
		t.Fatal(err)
	}
	for _, tpl := range all {
		if tpl.ID == "invoicing" {
			return tpl
		}
	}
	t.Fatal("no invoicing template")
	return Template{}
}

func TestTheInvoicingTemplateDeclaresAPaymentsSlotAndNothingElse(t *testing.T) {
	tpl := invoicingTemplate(t)
	def := tpl.Definition
	if def.DefinitionVersion != DefinitionVersion || !slices.Contains(def.Requires, "effect.create_payment_link") {
		t.Fatalf("version %d, requires %v", def.DefinitionVersion, def.Requires)
	}
	if len(def.Integrations) != 1 || def.Integrations["payments"].Kind != KindPayments {
		t.Fatalf("integrations = %+v", def.Integrations)
	}
	if tpl.Roles != nil {
		t.Fatalf("roles = %+v: a template for money gives nobody a role the space doesn't", tpl.Roles)
	}
	raw := strings.ToLower(mustJSON(t, tpl))
	for _, word := range []string{"stripe", "square", "http", "api_key", "secret", "webhook", "integration_id"} {
		if strings.Contains(raw, word) {
			t.Errorf("the template names %q", word)
		}
	}

	contract := def.contract("payments")
	if !slices.Equal(contract.Operations, []string{EffectCreatePaymentLink}) ||
		!slices.Equal(contract.Sends, []string{"currency", "customer", "number", "total"}) ||
		!slices.Equal(contract.Events, []string{EventPaymentRefunded, EventPaymentSucceeded}) {
		t.Fatalf("the contract: %+v", contract)
	}
	var writes []string
	for _, w := range contract.Writes {
		writes = append(writes, w.Field)
	}
	if want := []string{"paid_at", "pay_url", "payment_id", "status"}; !slices.Equal(writes, want) {
		t.Fatalf("writes = %v, want %v", writes, want)
	}
	if got := contract.Hash(); got != invoicingContractHash {
		t.Fatalf("the contract's hash is %s: if the change is intended, pin it in invoicingContractHash", got)
	}
}

func TestTheInvoicingTemplateEditsWithoutAPaymentLink(t *testing.T) {
	def := invoicingTemplate(t).Definition
	edit, ok := def.Actions["edit_invoice"]
	if !ok || edit.Kind != ActionUpdate || len(edit.Effects) != 0 {
		t.Fatalf("edit_invoice = %+v, %v: the record editor's update must have no effect", edit, ok)
	}
	for _, name := range []string{"send_invoice", "replace_link"} {
		a := def.Actions[name]
		if a.Kind != ActionUpdate || len(a.Effects) != 1 || a.Effects[0].Kind != EffectCreatePaymentLink {
			t.Fatalf("%s = %+v", name, a)
		}
		w := a.Effects[0].When
		if w == nil || w.Field != "status" || w.Op != OpEq || w.Value != "sent" {
			t.Fatalf("%s runs its payment link when %+v: only for a sent invoice", name, w)
		}
	}
	var links []string
	for _, l := range def.contract("payments").Links {
		links = append(links, l.Action)
	}
	if slices.Sort(links); !slices.Equal(links, []string{"replace_link", "send_invoice"}) {
		t.Fatalf("the contract's links are of %v", links)
	}
	buttons := map[string]map[string]any{}
	for _, sc := range def.Screens {
		for _, c := range sc.Components {
			raw := mustJSON(t, c)
			var b struct {
				ID     string `json:"id"`
				Action struct {
					Event struct {
						Name    string         `json:"name"`
						Context map[string]any `json:"context"`
					} `json:"event"`
				} `json:"action"`
			}
			if err := json.Unmarshal([]byte(raw), &b); err != nil {
				t.Fatal(err)
			}
			if b.Action.Event.Name != "" {
				buttons[b.Action.Event.Name] = b.Action.Event.Context
			}
		}
	}
	if c := buttons["send_invoice"]; len(c) != 2 || c["status"] != "sent" {
		t.Fatalf("send_invoice's context = %v", c)
	}
	if c := buttons["replace_link"]; len(c) != 3 || c["pay_url"] != "" || c["payment_id"] != "" {
		t.Fatalf("replace_link's context = %v", c)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestAnInvoicingInstallBindsNoIntegration(t *testing.T) {
	i := newInv(t)
	i.allow(i.stripe, i.alicePrivate)

	a := i.mustInstall(i.alice, i.alicePrivate, "invoicing", nil)
	if a.Live != nil || a.Rows != 0 || a.NeedsBoard {
		t.Fatalf("installed = %+v", a)
	}
	for table, want := range map[string]int{"app_rows": i.tableCount("app_rows"), "app_integration_approvals": 0, "app_effects": 0, "integration_refs": 0, "app_roles": 0} {
		if got := i.tableCount(table); got != want {
			t.Errorf("%s has %d rows after installing", table, got)
		}
	}
	st := i.status(i.alice, i.alicePrivate, a)
	if len(st.Slots) != 1 || st.Slots[0].Slot != "payments" || st.Slots[0].Kind != KindPayments ||
		st.Slots[0].Status != ApprovalNone || st.Slots[0].Integration != nil || st.Slots[0].Live != nil {
		t.Fatalf("the copy's slots: %+v", st.Slots)
	}
	if st.Slots[0].ContractHash != invoicingContractHash {
		t.Fatalf("the installed copy's hash = %s", st.Slots[0].ContractHash)
	}

	rec := i.tryPublish(i.alice, i.alicePrivate, i.view(i.alice, i.alicePrivate, a))
	expect(t, rec, http.StatusConflict)
	if code(t, rec) != string(apierr.CodeAppIntegrationUnapproved) {
		t.Fatalf("publishing an unbound copy: %s", rec.Body.String())
	}

	b := i.mustInstall(i.alice, i.alicePrivate, "invoicing", map[string]any{"name": "Second"})
	req := i.mustRequest(i.alice, i.alicePrivate, a)
	expect(t, i.approve(i.owner, req.ID, i.stripe), http.StatusOK)
	if st := i.status(i.alice, i.alicePrivate, b); st.Slots[0].Status != ApprovalNone || st.Slots[0].Integration != nil {
		t.Fatalf("the second copy after the first's approval: %+v", st.Slots[0])
	}
	expect(t, i.tryPublish(i.alice, i.alicePrivate, i.view(i.alice, i.alicePrivate, a)), http.StatusOK)
	expect(t, i.tryPublish(i.alice, i.alicePrivate, i.view(i.alice, i.alicePrivate, b)), http.StatusConflict)

	expect(t, i.bob.do(http.MethodPost, installPath(i.alicePrivate, "invoicing"), map[string]any{}), http.StatusNotFound)
}

func TestThePaymentEventsMarkAnInvoicePaidAndThenRefunded(t *testing.T) {
	i := newInv(t)
	i.allow(i.stripe, i.alicePrivate)
	a := i.mustInstall(i.alice, i.alicePrivate, "invoicing", nil)
	expect(t, i.approve(i.owner, i.mustRequest(i.alice, i.alicePrivate, a).ID, i.stripe), http.StatusOK)
	a = i.publish(i.alice, i.alicePrivate, i.view(i.alice, i.alicePrivate, a))

	invoice := func(number, status string) int64 {
		t.Helper()
		expect(t, i.act(i.alice, i.alicePrivate, a, "add_invoice", map[string]any{
			"number": number, "customer": "Acme", "total": "120", "currency": "EUR", "status": status}, nil), http.StatusOK)
		return i.latest(a)
	}
	event := func(id int64, kind string) (paylink.EventResult, error) {
		return i.e.s.WriteEvent(i.ctx(), paylink.EventWrite{
			WorkspaceID: i.ws, IntegrationID: i.stripe, AppID: a.ID, RecordID: id, Slot: "payments", Event: kind,
			AmountMinor: 12000, Currency: "EUR", OccurredAt: time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC),
		})
	}
	status := func(id int64) any { return i.rowData(a, id)["status"] }

	sent := invoice("INV-1", "sent")
	if res, err := event(sent, "payment.succeeded"); err != nil || !res.Written {
		t.Fatalf("a payment: %+v, %v", res, err)
	}
	if d := i.rowData(a, sent); d["status"] != "paid" || d["paid_at"] != "2026-11-26" {
		t.Fatalf("a paid invoice: %v", d)
	}
	h := decode[History](t, i.history(i.alice, i.alicePrivate, a, sent))
	if latest := h.History[0]; latest.By == nil || latest.By.System == nil || latest.Action != "payment.succeeded" {
		t.Fatalf("the payment's history entry: %+v", latest)
	}
	if res, err := event(sent, "payment.refunded"); err != nil || !res.Written || status(sent) != "refunded" {
		t.Fatalf("a refund: %+v, %v, status %v", res, err, status(sent))
	}

	draft := invoice("INV-2", "draft")
	if res, err := event(draft, "payment.succeeded"); err != nil || !res.Written || res.ConditionUnmet {
		t.Fatalf("a payment for a draft: %+v, %v", res, err)
	}
	if d := i.rowData(a, draft); d["status"] != "paid" || d["paid_at"] != "2026-11-26" {
		t.Fatalf("a paid draft: %v", d)
	}
	open := invoice("INV-3", "sent")
	if res, err := event(open, "payment.expired"); err != nil || res.Written || !res.NoHandler || status(open) != "sent" {
		t.Fatalf("an expiry: %+v, %v, status %v", res, err, status(open))
	}
}
