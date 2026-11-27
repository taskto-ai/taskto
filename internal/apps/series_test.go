package apps

import (
	"encoding/json"
	"math"
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
			{Name: "tag", Type: TypeString},
			{Name: "tier", Type: TypeChoice, Options: []string{"big", "mixed", "small"}},
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
			"by_status":    {Kind: "count", GroupBy: "status", SplitBy: "active"},
			"by_month":     {Kind: "sum", Field: "amount", GroupBy: "day", Bucket: "month", Last: 3},
			"by_week":      {Kind: "count", GroupBy: "day", Bucket: "week", Last: 4},
			"by_day":       {Kind: "avg", Field: "amount", GroupBy: "day", Bucket: "day", Last: 7},
			"by_name":      {Kind: "count", GroupBy: "name"},
			"open_total":   {Kind: "sum", Field: "amount", GroupBy: "active", Where: &Rule{Field: "status", Op: "eq", Value: "open"}},
			"by_tag":       {Kind: "count", GroupBy: "tag"},
			"tier_sum":     {Kind: "sum", Field: "amount", GroupBy: "tier"},
			"tier_avg":     {Kind: "avg", Field: "amount", GroupBy: "tier"},
			"split_tier":   {Kind: "count", GroupBy: "status", SplitBy: "tier"},
			"name_closed":  {Kind: "count", GroupBy: "name", Where: &Rule{Field: "status", Op: "eq", Value: "closed"}},
			"default_last": {Kind: "count", GroupBy: "day", Bucket: "month"},
		},
		Expected: map[string]SeriesData{},
	}
	fixture.Rows = append(fixture.Rows,
		map[string]any{"id": 8, "amount": math.MaxFloat64, "tier": "big", "status": "open", "active": "true", "day": "2026-13-45"},
		map[string]any{"id": 9, "amount": math.MaxFloat64, "tier": "big", "status": "open", "active": false, "day": "2026-12-31T10:00", "tag": ""},
		map[string]any{"id": 10, "amount": math.MaxFloat64, "tier": "mixed", "status": "closed"},
		map[string]any{"id": 11, "amount": -math.MaxFloat64, "tier": "mixed", "status": "open"},
		map[string]any{"id": 12, "amount": 5, "tier": "small", "status": "closed", "active": true},
		map[string]any{"id": 54, "name": "only-open", "status": "open"},
		map[string]any{"id": 55, "status": "archived", "active": true, "day": "2027-01-05"},
		map[string]any{"id": 56, "status": "open", "tier": "giant"},
	)
	for i := 0; i < 22; i++ {
		fixture.Rows = append(fixture.Rows, map[string]any{"id": 13 + i, "tag": "t" + strconv.Itoa(100+i)})
	}
	for i := 0; i < 3; i++ {
		fixture.Rows = append(fixture.Rows, map[string]any{"id": 40 + i, "tag": "zeta"})
	}
	fixture.Rows = append(fixture.Rows,
		map[string]any{"id": 50, "tag": "\uff21"},
		map[string]any{"id": 51, "tag": "\U0001F600"},
		map[string]any{"id": 52, "tag": "other"},
		map[string]any{"id": 53, "tag": 7},
	)
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

func TestSeriesToleratesValuesOfAnOlderFieldType(t *testing.T) {
	c := Collection{Fields: []Field{
		{Name: "amount", Type: TypeNumber},
		{Name: "active", Type: TypeBoolean},
		{Name: "status", Type: TypeChoice, Options: []string{"open"}},
	}}
	rows := []Row{
		{Data: map[string]any{"amount": "4", "active": "true", "status": "open"}},
		{Data: map[string]any{"amount": float64(2), "active": true, "status": "open"}},
	}
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	data := computeSeries(Series{Kind: "sum", Field: "amount", GroupBy: "active", SplitBy: "active"}, c, rows, now)
	if got := *data.Series[1].Values[1]; got != 2 {
		t.Fatalf("sum = %v, want only the number counted", got)
	}
	counted := computeSeries(Series{Kind: "count", GroupBy: "active"}, c, rows, now)
	if got := *counted.Series[0].Values[1]; got != 1 {
		t.Fatalf("count = %v, want only the boolean counted", got)
	}
}

func TestSeriesNeverReportsANonFiniteNumber(t *testing.T) {
	c := Collection{Fields: []Field{{Name: "amount", Type: TypeNumber}, {Name: "status", Type: TypeChoice, Options: []string{"open"}}}}
	huge := math.MaxFloat64
	rows := []Row{
		{Data: map[string]any{"amount": huge, "status": "open"}},
		{Data: map[string]any{"amount": huge, "status": "open"}},
	}
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	avg := computeSeries(Series{Kind: "avg", Field: "amount", GroupBy: "status"}, c, rows, now)
	if got := avg.Series[0].Values[0]; got == nil || *got != huge {
		t.Fatalf("average = %v, want %v", got, huge)
	}
	opposite := computeSeries(Series{Kind: "avg", Field: "amount", GroupBy: "status"}, c, []Row{
		{Data: map[string]any{"amount": huge, "status": "open"}},
		{Data: map[string]any{"amount": -huge, "status": "open"}},
	}, now)
	if got := opposite.Series[0].Values[0]; got == nil || *got != 0 {
		t.Fatalf("opposite average = %v, want 0", got)
	}
	sum := computeSeries(Series{Kind: "sum", Field: "amount", GroupBy: "status"}, c, rows, now)
	if sum.Series[0].Values[0] != nil {
		t.Fatalf("overflowing sum = %v, want null", *sum.Series[0].Values[0])
	}
	if _, err := json.Marshal(sum); err != nil {
		t.Fatal(err)
	}
}

func TestSeriesTextTiesBreakByBytes(t *testing.T) {
	c := Collection{Fields: []Field{{Name: "name", Type: TypeString}}}
	var rows []Row
	for _, name := range []string{"Ａ", "\U0001F600"} {
		rows = append(rows, Row{Data: map[string]any{"name": name}})
	}
	for i := 0; i < 19; i++ {
		rows = append(rows, Row{Data: map[string]any{"name": "k" + strconv.Itoa(100+i)}})
	}
	data := computeSeries(Series{Kind: "count", GroupBy: "name"}, c, rows, time.Now())
	if got := data.X[19].Key; got != "Ａ" {
		t.Fatalf("20th group = %q, want the lower byte sequence", got)
	}
}
