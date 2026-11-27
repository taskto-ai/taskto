package apps

import (
	"strings"
	"testing"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/paylink"
)

func TestALinkOfAModeTheIntegrationNoLongerHasIsNotWritten(t *testing.T) {
	p := newPay(t)
	p.send(p.alice, "op-send-0001")
	_, rev, _ := p.row()
	p.links.onOpen = func(paylink.Request) {
		p.links.onOpen = nil
		p.links.mu.Lock()
		p.links.otherMode = true
		p.links.mu.Unlock()
	}
	p.work()
	e := p.entries(p.alice).Effects[0]
	if e.Status != StateFailed || e.Error == nil || e.Error.Code != apierr.CodeIntegrationAccountChanged {
		t.Fatalf("entry = %+v", e)
	}
	if data, rev2, _ := p.row(); strings.Contains(data, "pay_url\":\"h") || rev2 != rev {
		t.Fatalf("the record was written: %s", data)
	}
}
