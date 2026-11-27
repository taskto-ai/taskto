package apps

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"taskto.ai/taskto/internal/access"
)

func TestAReplayOfACommittedOperationGetsItsReceiptAfterAccessIsLost(t *testing.T) {
	w := newWorld(t)
	s, a := w.shared, w.sharedApp
	live := w.view(w.alice, s, a).Live.Number
	press := func(operation, description string) *httptest.ResponseRecorder {
		act := action(a, "add_expense", map[string]any{"description": description, "amount": "3"})
		act.Operation = operation
		act.Release = &live
		return w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), act)
	}
	expect(t, press("op-receipt-0001", "Lunch"), http.StatusOK)

	w.mustSetRoles(w.owner, s, a, grant(w.owner.id, access.AppRoles...), grant(w.alice.id, access.AppReader))
	expectForbidden(t, press("op-receipt-0002", "Dinner"), "submit")

	rec := press("op-receipt-0001", "Lunch")
	expect(t, rec, http.StatusOK)
	if got := rec.Body.String(); got != "[]\n" && got != "[]" {
		t.Fatalf("the receipt carries data: %s", got)
	}
	expectForbidden(t, press("op-receipt-0001", "Something else"), "submit")

	w.mustSetRoles(w.owner, s, a, grant(w.owner.id, access.AppRoles...), grant(w.bob.id, access.AppReader))
	expect(t, press("op-receipt-0001", "Lunch"), http.StatusOK)
	expect(t, press("op-receipt-0003", "Lunch"), http.StatusNotFound)
	other := action(a, "add_expense", map[string]any{"description": "Lunch", "amount": "3"})
	other.Operation = "op-receipt-0001"
	other.Release = &live
	expect(t, w.bob.do(http.MethodPost, appPath(s, a.ID, "/actions"), other), http.StatusForbidden)
}

func TestAReceiptAnswersOnlyOnTheRouteOfTheSpaceItWasMadeIn(t *testing.T) {
	w := newWorld(t)
	s, a := w.shared, w.sharedApp
	live := w.view(w.alice, s, a).Live.Number
	act := action(a, "add_expense", map[string]any{"description": "Lunch", "amount": "3"})
	act.Operation = "op-receipt-0101"
	act.Release = &live
	expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), act), http.StatusOK)

	expect(t, w.alice.do(http.MethodPost, appPath(w.alicePrivate, a.ID, "/actions"), act), http.StatusNotFound)
	expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), act), http.StatusOK)
}

func TestAReplayOfACommittedOperationGetsItsReceiptAfterTheAppIsArchived(t *testing.T) {
	w := newWorld(t)
	s, a := w.shared, w.sharedApp
	live := w.view(w.alice, s, a).Live.Number
	press := func(operation, description string) *httptest.ResponseRecorder {
		act := action(a, "add_expense", map[string]any{"description": description, "amount": "3"})
		act.Operation = operation
		act.Release = &live
		return w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), act)
	}
	expect(t, press("op-receipt-0201", "Lunch"), http.StatusOK)
	cur := w.view(w.owner, s, a)
	expect(t, w.owner.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": cur.Version, "archived": true}), http.StatusOK)

	rec := press("op-receipt-0201", "Lunch")
	expect(t, rec, http.StatusOK)
	if got := rec.Body.String(); got != "[]\n" && got != "[]" {
		t.Fatalf("the receipt carries data: %s", got)
	}
	expect(t, press("op-receipt-0201", "Something else"), http.StatusConflict)
	expect(t, press("op-receipt-0202", "Lunch"), http.StatusConflict)
}
