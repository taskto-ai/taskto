//go:build unix

package cliaccounts

import (
	"context"
	"testing"

	"taskto.ai/taskto/internal/apitypes/nonnull"
)

func TestSwitchingListsAreNeverNil(t *testing.T) {
	e := newEnv(t, LocalUnsafe, nil)
	owner, _ := e.setup()
	got, err := e.svc.state(context.Background(), owner.id)
	if err != nil {
		t.Fatal(err)
	}
	nonnull.Require(t, got)
}
