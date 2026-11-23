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

// The receipt answers on the route it was made on: the space and the app of the route are the ones
// the operation was made in. A caller who lost their access to that space does not get it through
// another space they can still reach, whose route names the same app id.
func TestAReceiptAnswersOnlyOnTheRouteOfTheSpaceItWasMadeIn(t *testing.T) {
	w := newWorld(t)
	s, a := w.shared, w.sharedApp
	live := w.view(w.alice, s, a).Live.Number
	act := action(a, "add_expense", map[string]any{"description": "Lunch", "amount": "3"})
	act.Operation = "op-receipt-0101"
	act.Release = &live
	expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), act), http.StatusOK)

	// The same request through Alice's own private space, which she reads and writes, with the id of
	// the shared app: the app is not there, and nothing says it was made.
	expect(t, w.alice.do(http.MethodPost, appPath(w.alicePrivate, a.ID, "/actions"), act), http.StatusNotFound)
	// And through the space it was made in it is still answered.
	expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), act), http.StatusOK)
}

// An exact replay of a change that committed is answered from its receipt after the app was
// archived, as after access was lost; a request that was not made is refused as an archived app
// refuses it.
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
	// Another body under the key, and another key, are the archived app's refusal.
	expect(t, press("op-receipt-0201", "Something else"), http.StatusConflict)
	expect(t, press("op-receipt-0202", "Lunch"), http.StatusConflict)
}
