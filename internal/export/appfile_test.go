package export

import (
	"encoding/json"
	"testing"

	"taskto.ai/taskto/internal/apps"
)

func TestAnAppsExportFileCarriesItsIcon(t *testing.T) {
	for _, shared := range []bool{false, true} {
		got := apps.ExportedApp{App: apps.App{ID: 7, Name: "Expenses", Icon: "🧾"}}
		b, err := json.Marshal(newAppFile(got, shared))
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			App struct {
				Icon string `json:"icon"`
			} `json:"app"`
		}
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		if out.App.Icon != "🧾" {
			t.Errorf("shared=%v: icon in %s = %q, want the app's emoji", shared, b, out.App.Icon)
		}
	}
}
