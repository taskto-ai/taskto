package integrations

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/apierr"
)

func TestAKeyOfTheOtherModeWaitsForTheLinksOfThisOne(t *testing.T) {
	p := newPay(t, publicURL)
	p.setKey(stripeLiveKey)
	record, _ := p.sentInvoice(100, "EUR", "op-switch-0001")
	path := wsPath(p.ws, fmt.Sprintf("integrations/%d", p.in.ID))
	patch := func(key string) (int, string, int) {
		rec := p.owner.do(http.MethodPatch, path, map[string]any{"secrets": map[string]string{"api_key": key}})
		body := decode[struct {
			Code   string `json:"code"`
			Params struct {
				Count int `json:"count"`
			} `json:"params"`
		}](t, rec)
		return rec.Code, body.Code, body.Params.Count
	}
	if status, code, count := patch(stripeTestKey); status != http.StatusConflict || code != string(apierr.CodeIntegrationModeChangeRefused) || count != 1 {
		t.Fatalf("switch to test = %d %q %d", status, code, count)
	}
	var mode string
	if err := p.db.QueryRow(`SELECT mode FROM integrations WHERE id = ?`, p.in.ID).Scan(&mode); err != nil || mode != ModeLive {
		t.Fatalf("mode = %q, %v", mode, err)
	}
	rotated := "sk_" + "live_" + strings.Repeat("Rota", 6)
	if status, code, _ := patch(rotated); status != http.StatusOK {
		t.Fatalf("a rotation of the live key = %d %q", status, code)
	}
	p.exec(`UPDATE integration_refs SET state = 'expired' WHERE record_id = ?`, record)
	if status, code, _ := patch(stripeTestKey); status != http.StatusOK {
		t.Fatalf("switch after the link ended = %d %q", status, code)
	}
}
