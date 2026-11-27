package apps

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func (f *fx) failedNotices(userID int64) (refs []string) {
	f.e.t.Helper()
	rows, err := f.e.db.Query(`SELECT ref FROM notifications WHERE user_id = ? AND kind = 'app_effect_failed' AND workspace_id = ? ORDER BY id`, userID, f.ws)
	if err != nil {
		f.e.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			f.e.t.Fatal(err)
		}
		refs = append(refs, ref)
	}
	return refs
}

func TestAnEffectThatFailsForGoodTellsTheUserWhoActed(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-notify-01", "East gate", "fail")
	f.work()
	if n := len(f.failedNotices(f.alice.id)); n != 0 {
		t.Fatalf("%d notifications for entries that were done", n)
	}

	f.mustSubmit(f.alice, "op-notify-02", "West gate", "fail")
	expect(t, f.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", f.ws, f.maint.ID, f.alice.id), nil), http.StatusNoContent)
	f.work()
	refs := f.failedNotices(f.alice.id)
	if len(refs) == 0 {
		t.Fatal("the failed entries told no one")
	}
	want := fmt.Sprintf(`"space":%d,"app":%d`, f.shared.ID, f.app.ID)
	for _, ref := range refs {
		if got := ref; len(got) < len(want) || got[1:1+len(want)] != want {
			t.Fatalf("ref = %s, want the app's space and ID first (%s)", ref, want)
		}
	}
	for _, other := range []*user{f.owner, f.bob} {
		if n := len(f.failedNotices(other.id)); n != 0 {
			t.Fatalf("user %d, who didn't act, was told of %d failures", other.id, n)
		}
	}
	before := len(refs)
	f.work()
	if n := len(f.failedNotices(f.alice.id)); n != before {
		t.Fatalf("%d notifications, then %d with no new failure", before, n)
	}
}

func TestAnEntryThatGivesUpAfterItsTriesTellsTheUserOnceAndAgainAfterARetry(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-notify-03", "East gate", "fail")
	broken := true
	f.e.s.hooks.afterWork = func(e effectRow) error {
		if e.kind == EffectCreateCard && broken {
			return errors.New("the disk is full")
		}
		return nil
	}
	f.work()
	if n := len(f.failedNotices(f.alice.id)); n != 0 {
		t.Fatalf("%d notifications while the entry still had tries", n)
	}
	for range maxAttempts + 2 {
		f.work()
		f.later(backoffMax + time.Second)
	}
	if n := len(f.failedNotices(f.alice.id)); n != 1 {
		t.Fatalf("%d notifications for one entry that gave up, want 1", n)
	}
	card := only(t, f.effects(f.alice, ""), EffectCreateCard)
	rec := f.alice.do(http.MethodPost, appPath(f.shared, f.app.ID, fmt.Sprintf("/effects/%d/retry", card.ID)), struct{}{})
	expect(t, rec, http.StatusOK)
	for range maxAttempts + 2 {
		f.work()
		f.later(backoffMax + time.Second)
	}
	if n := len(f.failedNotices(f.alice.id)); n != 2 {
		t.Fatalf("%d notifications after it failed again, want one for each failure", n)
	}
}
