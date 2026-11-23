package apps

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"taskto.ai/taskto/internal/access"
)

// A change that committed is answered to whoever made it, as it was, however their access
// changed since: a device that sent it and lost the answer learns it is in. It is a receipt and no
// data, and a request that was not made (another key, another body) is refused as it ever was.
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

	// Alice may no longer add records (a reader), and may not read all of them either.
	w.mustSetRoles(w.owner, s, a, grant(w.owner.id, access.AppRoles...), grant(w.alice.id, access.AppReader))
	expectForbidden(t, press("op-receipt-0002", "Dinner"), "submit")

	rec := press("op-receipt-0001", "Lunch")
	expect(t, rec, http.StatusOK)
	if got := rec.Body.String(); got != "[]\n" && got != "[]" {
		t.Fatalf("the receipt carries data: %s", got)
	}
	// The same key for another request is not that operation's receipt.
	expectForbidden(t, press("op-receipt-0001", "Something else"), "submit")

	// Removed from the app altogether, it is still the same: she made it, and it is in.
	w.mustSetRoles(w.owner, s, a, grant(w.owner.id, access.AppRoles...), grant(w.bob.id, access.AppReader))
	expect(t, press("op-receipt-0001", "Lunch"), http.StatusOK)
	expect(t, press("op-receipt-0003", "Lunch"), http.StatusNotFound)
	// Nor is it anyone else's: the operation is the caller's own.
	other := action(a, "add_expense", map[string]any{"description": "Lunch", "amount": "3"})
	other.Operation = "op-receipt-0001"
	other.Release = &live
	expect(t, w.bob.do(http.MethodPost, appPath(s, a.ID, "/actions"), other), http.StatusForbidden)
}
