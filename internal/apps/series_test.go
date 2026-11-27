package apps

import (
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSeriesValidation(t *testing.T) {
	d := example(t)
	d.Collection.Fields = append(d.Collection.Fields,
		Field{Name: "status", Type: TypeChoice, Options: []string{"open", "closed"}, OptionLabels: map[string]string{"open": "Open"}},
		Field{Name: "active", Type: TypeBoolean},
		Field{Name: "day", Type: TypeDate},
	)
	valid := Series{Kind: "count", GroupBy: "status"}
	for _, tc := range []struct {
		name string
		edit func(*Definition)
		want string
	}{
		{"valid", func(d *Definition) { d.Series = map[string]Series{"by_status": valid} }, ""},
		{"sum on non-number", func(d *Definition) {
			d.Series = map[string]Series{"bad": {Kind: "sum", Field: "description", GroupBy: "status"}}
		}, "not a number"},
		{"bucket without date", func(d *Definition) {
			d.Series = map[string]Series{"bad": {Kind: "count", GroupBy: "status", Bucket: "day"}}
		}, "bucket and last require"},
		{"bucket on choice", func(d *Definition) {
			d.Series = map[string]Series{"bad": {Kind: "count", GroupBy: "status", Bucket: "month"}}
		}, "bucket and last require"},
		{"date without bucket", func(d *Definition) { d.Series = map[string]Series{"bad": {Kind: "count", GroupBy: "day"}} }, "needs bucket"},
		{"last out of range", func(d *Definition) {
			d.Series = map[string]Series{"bad": {Kind: "count", GroupBy: "day", Bucket: "day", Last: 91}}
		}, "last is 1 to 90"},
		{"name collision with field", func(d *Definition) { d.Series = map[string]Series{"status": valid} }, "also a field"},
		{"name collision with summary", func(d *Definition) {
			d.Summaries = map[string]Summary{"total": {Kind: "count"}}
			d.Series = map[string]Series{"total": valid}
		}, "also a summary"},
		{"version one", func(d *Definition) { d.DefinitionVersion = 1; d.Series = map[string]Series{"by_status": valid} }, "can't use"},
		{"too many", func(d *Definition) {
			d.Series = map[string]Series{}
			for i := 0; i < maxSeries+1; i++ {
				d.Series[string(rune('a'+i))] = valid
			}
		}, "at most"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := d
			copy.Series = nil
			tc.edit(&copy)
			_, err := copy.Check()
			if tc.want == "" && err != nil {
				t.Fatal(err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestComputeSeries(t *testing.T) {
	c := Collection{Fields: []Field{
		{Name: "amount", Type: TypeNumber},
		{Name: "status", Type: TypeChoice, Options: []string{"open", "closed"}, OptionLabels: map[string]string{"open": "Open", "closed": "Closed"}},
		{Name: "active", Type: TypeBoolean},
		{Name: "day", Type: TypeDate},
		{Name: "name", Type: TypeString},
	}}
	rows := []Row{
		{Data: map[string]any{"amount": float64(4), "status": "open", "active": true, "day": "2026-11-01", "name": "a"}},
		{Data: map[string]any{"amount": float64(8), "status": "open", "active": true, "day": "2026-11-02", "name": "b"}},
		{Data: map[string]any{"amount": float64(2), "status": "closed", "active": false, "day": "2026-10-01", "name": "a"}},
		{Data: map[string]any{"amount": "wrong", "status": "closed", "active": false, "day": "2026-10-03", "name": "c"}},
	}
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	was := seriesNow
	seriesNow = func() time.Time { return now }
	t.Cleanup(func() { seriesNow = was })
	choice := computeSeries(Series{Kind: "count", GroupBy: "status"}, c, rows, now)
	if got := choice.X; len(got) != 2 || got[0] != (SeriesPoint{Key: "open", Label: "Open"}) || got[1].Key != "closed" {
		t.Fatalf("choice groups = %#v", got)
	}
	if *choice.Series[0].Values[0] != 2 || *choice.Series[0].Values[1] != 2 {
		t.Fatalf("choice values = %#v", choice.Series)
	}
	monthly := computeSeries(Series{Kind: "sum", Field: "amount", GroupBy: "day", Bucket: "month", Last: 2}, c, rows, seriesNow())
	if monthly.X[0].Key != "2026-10" || monthly.X[1].Key != "2026-11" || *monthly.Series[0].Values[0] != 2 || *monthly.Series[0].Values[1] != 12 {
		t.Fatalf("monthly = %#v", monthly)
	}
	avg := computeSeries(Series{Kind: "avg", Field: "amount", GroupBy: "status"}, c, rows, now)
	if *avg.Series[0].Values[0] != 6 || *avg.Series[0].Values[1] != 2 {
		t.Fatalf("average = %#v", avg)
	}
	empty := computeSeries(Series{Kind: "avg", Field: "amount", GroupBy: "status"}, c, nil, now)
	if empty.Series[0].Values[0] != nil {
		t.Fatalf("empty average = %#v", empty.Series[0].Values)
	}
	split := computeSeries(Series{Kind: "count", GroupBy: "status", SplitBy: "active"}, c, rows, now)
	if split.Series[0].Key != "false" || *split.Series[0].Values[1] != 2 || *split.Series[1].Values[0] != 2 {
		t.Fatalf("split = %#v", split.Series)
	}
	filtered := computeSeries(Series{Kind: "count", GroupBy: "status", Where: &Rule{Field: "active", Op: "eq", Value: true}}, c, rows, now)
	if *filtered.Series[0].Values[0] != 2 || *filtered.Series[0].Values[1] != 0 {
		t.Fatalf("filtered = %#v", filtered.Series)
	}
	textRows := make([]Row, 22)
	for i := range textRows {
		textRows[i] = Row{Data: map[string]any{"name": "group-" + strconv.Itoa(i)}}
	}
	text := computeSeries(Series{Kind: "count", GroupBy: "name"}, c, textRows, now)
	if len(text.X) != 21 || text.X[len(text.X)-1].Key != "other" || *text.Series[0].Values[len(text.Series[0].Values)-1] != 2 {
		t.Fatalf("text groups = %#v", text)
	}
}

func TestDataModelSeriesUseAllRowsWhenItemsAreBounded(t *testing.T) {
	def := Definition{
		Collection: Collection{Fields: []Field{{Name: "kind", Type: TypeChoice, Options: []string{"a"}}}},
		Series:     map[string]Series{"by_kind": {Kind: "count", GroupBy: "kind"}},
	}
	rows := make([]Row, 30)
	for i := range rows {
		rows[i] = Row{ID: int64(i + 1), Revision: 1, Data: map[string]any{"kind": "a"}}
	}
	message := dataModel("app", def, rows, "", 25)
	model := message["updateDataModel"].(map[string]any)["value"].(map[string]any)
	if len(model[ItemsKey].([]map[string]any)) != 25 {
		t.Fatalf("shown items = %d", len(model[ItemsKey].([]map[string]any)))
	}
	series := model["by_kind"].(SeriesData)
	if got := *series.Series[0].Values[0]; got != 30 {
		t.Fatalf("series count = %v, want all 30 rows", got)
	}
}

type seriesFixture struct {
	Now        string                `json:"now"`
	Collection Collection            `json:"collection"`
	Rows       []map[string]any      `json:"rows"`
	Series     map[string]Series     `json:"series"`
	Expected   map[string]SeriesData `json:"expected"`
}

func TestSeriesFixtureSharedWithTheWebApp(t *testing.T) {
	now := time.Date(2027, 1, 6, 12, 0, 0, 0, time.UTC)
	fixture := seriesFixture{
		Now: now.Format(time.RFC3339),
		Collection: Collection{Fields: []Field{
			{Name: "amount", Type: TypeNumber},
			{Name: "status", Type: TypeChoice, Options: []string{"open", "closed"}, OptionLabels: map[string]string{"open": "Open", "closed": "Closed"}},
			{Name: "active", Type: TypeBoolean},
			{Name: "day", Type: TypeDate},
			{Name: "name", Type: TypeString},
		}},
		Rows: []map[string]any{
			{"id": 1, "amount": 4, "status": "open", "active": true, "day": "2026-11-30", "name": "a"},
			{"id": 2, "amount": 8, "status": "open", "active": true, "day": "2026-12-15", "name": "b"},
			{"id": 3, "amount": 2, "status": "closed", "active": false, "day": "2026-12-31", "name": "a"},
			{"id": 4, "amount": "wrong", "status": "closed", "active": false, "day": "2027-01-01", "name": "other"},
			{"id": 5, "amount": 6, "status": "open", "active": false, "day": "2027-01-05", "name": "a"},
			{"id": 6, "amount": 1, "status": "closed", "active": true, "day": "2026-10-01", "name": "c"},
			{"id": 7, "amount": 3, "status": "closed", "active": true, "day": "2026-12-28", "name": "b"},
		},
		Series: map[string]Series{
			"by_status":  {Kind: "count", GroupBy: "status", SplitBy: "active"},
			"by_month":   {Kind: "sum", Field: "amount", GroupBy: "day", Bucket: "month", Last: 3},
			"by_week":    {Kind: "count", GroupBy: "day", Bucket: "week", Last: 4},
			"by_day":     {Kind: "avg", Field: "amount", GroupBy: "day", Bucket: "day", Last: 7},
			"by_name":    {Kind: "count", GroupBy: "name"},
			"open_total": {Kind: "sum", Field: "amount", GroupBy: "active", Where: &Rule{Field: "status", Op: "eq", Value: "open"}},
		},
		Expected: map[string]SeriesData{},
	}
	rows := make([]Row, len(fixture.Rows))
	for i, data := range fixture.Rows {
		values := map[string]any{}
		for name, value := range data {
			if name == "id" {
				continue
			}
			if n, ok := value.(int); ok {
				value = float64(n)
			}
			values[name] = value
		}
		rows[i] = Row{ID: int64(i + 1), Revision: 1, Data: values}
	}
	names := make([]string, 0, len(fixture.Series))
	for name := range fixture.Series {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fixture.Expected[name] = computeSeries(fixture.Series[name], fixture.Collection, rows, now)
	}
	week := fixture.Expected["by_week"]
	if got := []string{week.X[0].Key, week.X[1].Key, week.X[2].Key, week.X[3].Key}; got[0] != "2026-W51" || got[2] != "2026-W53" || got[3] != "2027-W01" {
		t.Fatalf("week keys = %v", got)
	}
	golden(t, "series.json", fixture)
}
