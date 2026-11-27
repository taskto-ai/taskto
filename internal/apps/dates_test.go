package apps

import (
	"encoding/json"
	"os"
	"testing"
)

func TestDatesAreAcceptedAsTheSharedCasesSay(t *testing.T) {
	raw, err := os.ReadFile("testdata/dates.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases []struct {
			Date  string
			Valid bool
		}
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	field := Field{Name: "on", Type: TypeDate}
	collection := Collection{Fields: []Field{field}}
	for _, tt := range file.Cases {
		t.Run(tt.Date, func(t *testing.T) {
			if _, present, err := field.stored(tt.Date); (err == nil && present) != tt.Valid {
				t.Fatalf("a field stores %q: got err %v, present %v, want valid %v", tt.Date, err, present, tt.Valid)
			}
			rule := Rule{Field: "on", Op: "eq", Value: tt.Date}
			if err := rule.check(collection); (err == nil) != tt.Valid {
				t.Fatalf("a rule compares with %q: got %v, want valid %v", tt.Date, err, tt.Valid)
			}
			row := map[string]any{"on": tt.Date}
			if got := inMonth(collection, "on", row, "2026-04"); got && !tt.Valid {
				t.Fatalf("%q is in a month of a summary but is not a date", tt.Date)
			}
		})
	}
}
