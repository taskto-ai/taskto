package integrations

import (
	"strings"
	"testing"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/apps"
)

func TestALinkOfTheModeBeforeTheKeyChangedIsNotWritten(t *testing.T) {
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	p.svc.afterCall = func() error {
		p.svc.afterCall = nil
		p.exec(`UPDATE integrations SET mode = ? WHERE id = ?`, ModeLive, p.in.ID)
		return nil
	}
	p.startWorker()
	p.send(record, "op-mode-0002")
	if e := p.settled(apps.StateFailed); e.code != string(apierr.CodeIntegrationAccountChanged) {
		t.Fatalf("entry = %+v", e)
	}
	refs := p.refs()
	if len(refs) != 1 || refs[0].object == "" {
		t.Fatalf("refs = %+v", refs)
	}
	if data, _ := p.record(record); strings.Contains(data, refs[0].object) {
		t.Fatalf("record = %s: a test link on a live integration's record", data)
	}
}
