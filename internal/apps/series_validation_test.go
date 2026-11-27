package apps

import (
	"slices"
	"strings"
	"testing"
)

func TestEveryMalformedSeriesDeclarationIsRefused(t *testing.T) {
	d := example(t)
	d.Collection.Fields = append(d.Collection.Fields,
		Field{Name: "status", Type: TypeChoice, Options: []string{"open", "closed"}},
		Field{Name: "active", Type: TypeBoolean},
		Field{Name: "day", Type: TypeDate},
		Field{Name: "at", Type: TypeTime},
	)
	for _, tc := range []struct {
		name  string
		name2 string
		s     Series
		want  string
	}{
		{"a name with a space", "Bad Name", Series{Kind: "count", GroupBy: "status"}, "invalid or reserved name"},
		{"a reserved name", "items", Series{Kind: "count", GroupBy: "status"}, "invalid or reserved name"},
		{"the screen's name", ScreenKey, Series{Kind: "count", GroupBy: "status"}, "invalid or reserved name"},
		{"the record's name", RecordKey, Series{Kind: "count", GroupBy: "status"}, "invalid or reserved name"},
		{"a count with a field", "s", Series{Kind: "count", Field: "amount", GroupBy: "status"}, "count has no field"},
		{"a sum with no field", "s", Series{Kind: "sum", GroupBy: "status"}, "not a number field"},
		{"an average of a text field", "s", Series{Kind: "avg", Field: "description", GroupBy: "status"}, "not a number field"},
		{"an unknown kind", "s", Series{Kind: "median", Field: "amount", GroupBy: "status"}, "kind must be"},
		{"a missing group_by", "s", Series{Kind: "count"}, "group_by names"},
		{"an unknown group_by", "s", Series{Kind: "count", GroupBy: "nope"}, "group_by names"},
		{"a number group_by", "s", Series{Kind: "count", GroupBy: "amount"}, "group_by names"},
		{"a time group_by", "s", Series{Kind: "count", GroupBy: "at"}, "group_by names"},
		{"an unknown bucket", "s", Series{Kind: "count", GroupBy: "day", Bucket: "year"}, "needs bucket"},
		{"a negative last", "s", Series{Kind: "count", GroupBy: "day", Bucket: "day", Last: -1}, "last is 1 to 90"},
		{"last on a choice", "s", Series{Kind: "count", GroupBy: "status", Last: 5}, "bucket and last require"},
		{"split by a text field", "s", Series{Kind: "count", GroupBy: "status", SplitBy: "description"}, "split_by names"},
		{"split by an unknown field", "s", Series{Kind: "count", GroupBy: "status", SplitBy: "nope"}, "split_by names"},
		{"a where on an unknown field", "s", Series{Kind: "count", GroupBy: "status", Where: &Rule{Field: "nope", Op: "eq", Value: "x"}}, "where"},
		{"a where with the wrong type", "s", Series{Kind: "count", GroupBy: "status", Where: &Rule{Field: "active", Op: "eq", Value: "yes"}}, "where"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := d
			copy.Series = map[string]Series{tc.name2: tc.s}
			_, err := copy.Check()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestASeriesDefinitionRequiresItsCapabilityAndOnlyThen(t *testing.T) {
	d := example(t)
	d.Collection.Fields = append(d.Collection.Fields, Field{Name: "status", Type: TypeChoice, Options: []string{"open"}})
	plain, err := d.Check()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(plain.Requires, SeriesCapability) {
		t.Fatalf("a definition with no series requires %v", plain.Requires)
	}
	d.Series = map[string]Series{"by_status": {Kind: "count", GroupBy: "status"}}
	checked, err := d.Check()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(checked.Requires, SeriesCapability) {
		t.Fatalf("requires = %v, want %s", checked.Requires, SeriesCapability)
	}
	if !slices.Contains(Capabilities(), SeriesCapability) {
		t.Fatalf("the catalog doesn't offer %s", SeriesCapability)
	}
}

func TestSeriesAreRefusedByAVersionOneDefinitionThatUsesNothingElseNew(t *testing.T) {
	d := example(t)
	d.DefinitionVersion = 1
	d.Collection.Fields = append(d.Collection.Fields, Field{Name: "day", Type: TypeDate})
	if _, err := d.Check(); err != nil {
		t.Fatalf("the version 1 definition without series: %v", err)
	}
	d.Series = map[string]Series{"by_day": {Kind: "count", GroupBy: "day", Bucket: "day"}}
	_, err := d.Check()
	if err == nil || !strings.Contains(err.Error(), "series") {
		t.Fatalf("error = %v, want a refusal naming series", err)
	}
}
