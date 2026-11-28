package apps

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func definitionWithSeries(t *testing.T, series map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(example(t))
	if err != nil {
		t.Fatal(err)
	}
	var def map[string]any
	if err := json.Unmarshal(raw, &def); err != nil {
		t.Fatal(err)
	}
	def["series"] = map[string]any{"by_description": series}
	return def
}

func TestASeriesWithAnUnknownFieldIsRefusedByEverySavePath(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	good := map[string]any{"kind": "count", "group_by": "description"}
	bad := map[string]any{"kind": "count", "group_by": "description", "limit": 5}

	refused := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		expect(t, rec, http.StatusBadRequest)
		if !strings.Contains(rec.Body.String(), `unknown field \"limit\"`) {
			t.Fatalf("the refusal does not name the field: %s", rec.Body.String())
		}
	}

	t.Run("creating an app", func(t *testing.T) {
		rec := w.alice.do(http.MethodPost, appsPath(s, ""), map[string]any{"name": "Series", "definition": definitionWithSeries(t, bad)})
		refused(t, rec)
		expect(t, w.alice.do(http.MethodPost, appsPath(s, ""), map[string]any{"name": "Series", "definition": definitionWithSeries(t, good)}), http.StatusCreated)
	})

	t.Run("changing an app's draft", func(t *testing.T) {
		app := w.aliceApp
		rec := w.alice.do(http.MethodPatch, appPath(s, app.ID, ""), map[string]any{"version": 1, "definition": definitionWithSeries(t, bad)})
		refused(t, rec)
		rec = w.alice.do(http.MethodPatch, appPath(s, app.ID, ""), map[string]any{"version": 1, "definition": definitionWithSeries(t, good)})
		expect(t, rec, http.StatusOK)
	})
}
