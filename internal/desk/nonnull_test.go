package desk

import (
	"context"
	"testing"

	"taskto.ai/taskto/internal/apitypes/nonnull"
)

func TestDeskListsAreNeverNil(t *testing.T) {
	_, service := deskService(t)
	list, err := service.List(context.Background(), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	nonnull.Require(t, list)
}
