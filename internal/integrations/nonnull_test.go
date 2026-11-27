package integrations

import (
	"fmt"
	"net/http"
	"testing"

	"taskto.ai/taskto/internal/apitypes/nonnull"
)

func TestIntegrationListsAreNeverNil(t *testing.T) {
	e := newEnv(t, "")
	owner, ws := e.setup()

	stripeIn := e.create(owner, ws, stripeBody("bare"))
	nonnull.Require(t, stripeIn)
	nonnull.Require(t, e.create(owner, ws, squareBody("shop")))

	one := wsPath(ws, fmt.Sprintf("integrations/%d", stripeIn.ID))
	nonnull.Require(t, decode[Integration](t, owner.do(http.MethodGet, one, nil)))
	list := decode[struct {
		Integrations []Integration `json:"integrations"`
	}](t, owner.do(http.MethodGet, wsPath(ws, "integrations"), nil))
	if len(list.Integrations) != 2 {
		t.Fatalf("list = %+v", list)
	}
	for _, in := range list.Integrations {
		nonnull.Require(t, in)
	}

	rec := owner.do(http.MethodPut, one+"/spaces", map[string]any{"spaces": []int64{}})
	expect(t, rec, http.StatusOK)
	nonnull.Require(t, decode[Integration](t, rec))
	rec = owner.do(http.MethodPatch, one, map[string]any{"disabled": true, "settings": map[string]string{}})
	expect(t, rec, http.StatusOK)
	nonnull.Require(t, decode[Integration](t, rec))

	nonnull.Require(t, decode[Registry](t, owner.do(http.MethodGet, wsPath(ws, "integration-providers"), nil)))
}
