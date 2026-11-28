package agenttools

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/apps"
)

func expensesWithSeries(t *testing.T, series map[string]any) map[string]any {
	t.Helper()
	def := expenses(t)
	def["series"] = map[string]any{"by_description": series}
	return def
}

func TestASeriesWithAnUnknownFieldIsRefusedByTheAppTools(t *testing.T) {
	w := newWorld(t)
	good := map[string]any{"kind": "count", "group_by": "description"}
	bad := map[string]any{"kind": "count", "group_by": "description", "limit": 5}

	refusal := func(out map[string]any) {
		t.Helper()
		if msg, _ := out["error"].(string); !strings.Contains(msg, `unknown field "limit"`) {
			t.Fatalf("the refusal does not name the field: %v", out)
		}
	}

	t.Run("create_app", func(t *testing.T) {
		refusal(w.status(http.StatusBadRequest, w.aliceConv, "create_app",
			map[string]any{"space_id": w.alicePrivate, "name": "Series", "definition": expensesWithSeries(t, bad)}))
		if apps := w.ok(w.aliceConv, "list_apps", map[string]any{"space_id": w.alicePrivate})["apps"].([]any); len(apps) != 0 {
			t.Fatalf("a refused create_app left %v", apps)
		}
		w.ok(w.aliceConv, "create_app", map[string]any{"space_id": w.alicePrivate, "name": "Series", "definition": expensesWithSeries(t, good)})
	})

	t.Run("update_app", func(t *testing.T) {
		app := id(w.ok(w.aliceConv, "create_app", map[string]any{"space_id": w.alicePrivate, "name": "Plain", "definition": expenses(t)})["id"])
		refusal(w.status(http.StatusBadRequest, w.aliceConv, "update_app",
			map[string]any{"space_id": w.alicePrivate, "app_id": app, "version": 1, "definition": expensesWithSeries(t, bad)}))
		updated := w.ok(w.aliceConv, "update_app",
			map[string]any{"space_id": w.alicePrivate, "app_id": app, "version": 1, "definition": expensesWithSeries(t, good)})
		if id(updated["version"]) != 2 {
			t.Fatalf("a refused update_app moved the version: %v", updated)
		}
	})
}

func TestTheAppToolSchemasDeclareEverySeriesFieldAndNoOther(t *testing.T) {
	var fields []string
	typ := reflect.TypeOf(apps.Series{})
	for i := 0; i < typ.NumField(); i++ {
		fields = append(fields, strings.Split(typ.Field(i).Tag.Get("json"), ",")[0])
	}
	sort.Strings(fields)

	seen := 0
	for _, d := range Definitions() {
		if d.Name != "create_app" && d.Name != "update_app" {
			continue
		}
		seen++
		var schema struct {
			Properties struct {
				Definition struct {
					Properties struct {
						Series struct {
							AdditionalProperties struct {
								Properties           map[string]json.RawMessage `json:"properties"`
								AdditionalProperties *bool                      `json:"additionalProperties"`
							} `json:"additionalProperties"`
						} `json:"series"`
					} `json:"properties"`
				} `json:"definition"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(d.InputSchema, &schema); err != nil {
			t.Fatalf("%s: %v", d.Name, err)
		}
		item := schema.Properties.Definition.Properties.Series.AdditionalProperties
		if item.AdditionalProperties == nil || *item.AdditionalProperties {
			t.Errorf("%s: a series may carry fields the schema doesn't declare", d.Name)
		}
		var declared []string
		for name := range item.Properties {
			declared = append(declared, name)
		}
		sort.Strings(declared)
		if !reflect.DeepEqual(declared, fields) {
			t.Errorf("%s: the schema's series fields are %v, the server reads %v", d.Name, declared, fields)
		}
	}
	if seen != 2 {
		t.Fatalf("found %d of create_app and update_app in tools.json", seen)
	}
}
