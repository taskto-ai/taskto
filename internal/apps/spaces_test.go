package apps

import (
	"encoding/json"
	"os"
	"testing"
)

func TestFieldInputIsTrimmedAsTheSharedCasesSay(t *testing.T) {
	raw, err := os.ReadFile("testdata/spaces.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases []struct {
			Type    string
			Input   string
			Stored  any
			Refused bool
		}
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	for _, tt := range file.Cases {
		t.Run(tt.Type+" "+tt.Input, func(t *testing.T) {
			field := Field{Name: "f", Type: tt.Type}
			value, present, err := field.stored(tt.Input)
			if tt.Refused {
				if err == nil {
					t.Fatalf("%s field stores %q, want it refused", tt.Type, tt.Input)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s field refuses %q: %v", tt.Type, tt.Input, err)
			}
			if tt.Stored == nil {
				if present {
					t.Fatalf("%s field stores %q as %v, want nothing", tt.Type, tt.Input, value)
				}
				return
			}
			if !present || value != tt.Stored {
				t.Fatalf("%s field stores %q as %v (present %v), want %v", tt.Type, tt.Input, value, present, tt.Stored)
			}
		})
	}
}
