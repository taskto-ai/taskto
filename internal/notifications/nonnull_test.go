package notifications_test

import (
	"testing"

	"taskto.ai/taskto/internal/apitypes/nonnull"
)

func TestEmptyInboxIsNeverNil(t *testing.T) {
	w := newWorld(t, false)
	for _, query := range []string{"", "?unread=1", "?before=1"} {
		got := w.inbox(w.mallory, query)
		if len(got.Notifications) != 0 {
			t.Fatalf("%s: %+v", query, got)
		}
		nonnull.Require(t, got)
	}
	w.assign("Something", w.bob)
	got := w.inbox(w.bob, "")
	if len(got.Notifications) == 0 {
		t.Fatal("the inbox is empty")
	}
	nonnull.Require(t, got)
}
