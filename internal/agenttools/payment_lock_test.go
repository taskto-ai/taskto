package agenttools

import (
	"net/http"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/integrations"
)

func TestRunAppActionRefusesAnOpenLinksMoneyWith422(t *testing.T) {
	tm := newTeam(t)
	row := id(tm.add(tm.aliceConv, "Invoice", 10)["id"])
	integration := tm.insert(`INSERT INTO integrations (workspace_id, public_id, provider, name, display_name, mode, max_amount_minor, currencies)
		VALUES (?, lower(hex(randomblob(16))), 'stripe', 'main', 'main', 'test', 1000, '["EUR"]') RETURNING id`, tm.one)
	tm.exec(`INSERT INTO integration_refs (token, integration_id, app_id, record_id, slot, amount_minor, currency, state, provider_object_id, amount_field)
		VALUES ('test-token-not-real-000000000001', ?, ?, ?, 'payments', 1000, 'EUR', 'open', 'cs_test_fixture', 'amount')`, integration, tm.app, row)
	tm.apps.WithPlatform(apps.Platform{Payments: integrations.New(tm.tools.o.Integrations, integrations.Options{})})
	status, body := tm.call(tm.aliceConv, "run_app_action", tm.run(map[string]any{
		"action": "edit_expense", "id": row, "context": map[string]any{"amount": 12},
	}))
	var amount float64
	if err := tm.db.QueryRow(`SELECT json_extract(data, '$.amount') FROM app_rows WHERE id = ?`, row).Scan(&amount); err != nil || amount != 10 {
		t.Fatalf("amount: %v, error: %v", amount, err)
	}
	if status != http.StatusUnprocessableEntity || body["code"] != "app_payment_link_open" {
		message, _ := body["error"].(string)
		t.Fatalf("edit was refused but returned status %d, code %v, payment message present %v", status, body["code"], strings.Contains(message, "payment link"))
	}
}
