package apps

import (
	"slices"
	"strings"
	"testing"
)

const goodChart = `{"id":"stages","component":"Chart","data":{"path":"/by_category"},"label":"Tasks per category","variant":"bar","stacked":true,"unit":"tasks"}`

func withChart(t *testing.T, chart string) (Definition, error) {
	t.Helper()
	return withTable(t, chart, func(d *Definition) {
		d.Series = map[string]Series{"by_category": {Kind: "count", GroupBy: "category"}}
	})
}

func TestAChartBoundToASeriesIsAccepted(t *testing.T) {
	checked, err := withChart(t, goodChart)
	if err != nil {
		t.Fatalf("a chart: %v", err)
	}
	if !slices.Contains(checked.Requires, "component.Chart") || !slices.Contains(checked.Requires, SeriesCapability) {
		t.Fatalf("requires %v lacks component.Chart or %s", checked.Requires, SeriesCapability)
	}
	minimal := `{"id":"stages","component":"Chart","data":{"path":"/by_category"},"label":"Tasks"}`
	if _, err := withChart(t, minimal); err != nil {
		t.Fatalf("a chart with only what it needs: %v", err)
	}
	for _, variant := range []string{"line", "bar", "area"} {
		chart := strings.Replace(goodChart, `"bar"`, `"`+variant+`"`, 1)
		if _, err := withChart(t, chart); err != nil {
			t.Fatalf("a %s chart: %v", variant, err)
		}
	}
}

func TestAChartThatCouldReachOutsideTheAppIsRefused(t *testing.T) {
	bad := map[string]struct{ chart, want string }{
		"a variant that is not drawn": {strings.Replace(goodChart, `"bar"`, `"pie"`, 1), "variant: must be one of line, bar, area"},
		"no label":                    {strings.Replace(goodChart, `"label":"Tasks per category",`, "", 1), `Chart needs "label"`},
		"no data":                     {strings.Replace(goodChart, `"data":{"path":"/by_category"},`, "", 1), `Chart needs "data"`},
		"stacked as text":             {strings.Replace(goodChart, `"stacked":true`, `"stacked":"yes"`, 1), "stacked: must be true or false"},
		"a colour":                    {strings.Replace(goodChart, `"stacked":true`, `"color":"#fff"`, 1), `Chart has no prop "color"`},
		"a link":                      {strings.Replace(goodChart, `"stacked":true`, `"href":"https://example.com"`, 1), `Chart has no prop "href"`},
		"a style":                     {strings.Replace(goodChart, `"stacked":true`, `"style":"x"`, 1), `Chart has no prop "style"`},
		"data that is not a binding":  {strings.Replace(goodChart, `{"path":"/by_category"}`, `{"path":"/by_category","extra":1}`, 1), "data binding"},
		"a unit that is not text":     {strings.Replace(goodChart, `"unit":"tasks"`, `"unit":5`, 1), "unit:"},
	}
	for name, tc := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := withChart(t, tc.chart)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestTheCatalogReferenceTellsTheAgentHowToFeedAChart(t *testing.T) {
	ref, err := Reference()
	if err != nil {
		t.Fatal(err)
	}
	chart, ok := ref.Components["Chart"]
	if !ok {
		t.Fatal("app_catalog has no Chart")
	}
	for prop, want := range map[string]string{
		"data":    "required",
		"variant": `"line"`,
		"label":   "required",
	} {
		if !strings.Contains(chart[prop], want) {
			t.Errorf("Chart.%s = %q, want it to say %q", prop, chart[prop], want)
		}
	}
	found := false
	for _, rule := range ref.Rules {
		found = found || strings.HasPrefix(rule, "Chart draws a declared series")
	}
	if !found {
		t.Fatal("app_catalog's rules don't describe the Chart")
	}
}
