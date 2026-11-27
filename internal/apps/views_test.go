package apps

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

const goodTable = `{"id":"tasks-table","component":"Table","items":{"path":"/items"},"label":"Tasks",` +
	`"columns":[{"field":"title","label":"Title"},{"field":"category","label":"Category"},{"field":"due_date","label":"Due"},{"field":"done","label":"Done"}],` +
	`"open":"open_task","sort":"due_date","dir":"desc"}`

func withTable(t *testing.T, table string, change func(*Definition)) (Definition, error) {
	t.Helper()
	d := planner(t)
	d.Screens = slices.Clone(d.Screens)
	d.Screens[0].Components = append(slices.Clone(d.Screens[0].Components), json.RawMessage(table))
	d.Collection.Fields = slices.Clone(d.Collection.Fields)
	d.Actions = maps2(d.Actions)
	if change != nil {
		change(&d)
	}
	return d.Check()
}

func TestATableOverTheRowsIsAccepted(t *testing.T) {
	checked, err := withTable(t, goodTable, nil)
	if err != nil {
		t.Fatalf("a table: %v", err)
	}
	if !slices.Contains(checked.Requires, "component.Table") {
		t.Fatalf("requires %v lacks component.Table", checked.Requires)
	}
	minimal := `{"id":"tasks-table","component":"Table","items":{"path":"/items"},"label":"Tasks","columns":[{"field":"title","label":"Title"}]}`
	if _, err := withTable(t, minimal, nil); err != nil {
		t.Fatalf("a table with one column and nothing else: %v", err)
	}
}

func TestCheckRefusesBadTables(t *testing.T) {
	many := make([]string, 13)
	for i := range many {
		many[i] = `{"field":"title","label":"Title"}`
	}
	table := func(columns, extra string) string {
		return `{"id":"tasks-table","component":"Table","items":{"path":"/items"},"label":"Tasks","columns":` + columns + extra + `}`
	}
	one := `[{"field":"title","label":"Title"}]`
	bad := map[string]struct {
		table  string
		change func(*Definition)
		want   string
	}{
		"an unknown column field": {table(`[{"field":"nope","label":"Nope"}]`, ""), nil, `"nope" is not a field`},
		"a photo field as a column": {table(`[{"field":"photo","label":"Photo"}]`, ""), func(d *Definition) {
			d.Collection.Fields = append(d.Collection.Fields, Field{Name: "photo", Type: TypeAttachment})
		}, "attachment field"},
		"a location field as a column": {table(`[{"field":"where","label":"Where"}]`, ""), func(d *Definition) {
			d.Collection.Fields = append(d.Collection.Fields, Field{Name: "where", Type: TypeLocation})
		}, "location field"},
		"a signature field as a column": {table(`[{"field":"sign","label":"Sign"}]`, ""), func(d *Definition) {
			d.Collection.Fields = append(d.Collection.Fields, Field{Name: "sign", Type: TypeSignature})
		}, "signature field"},
		"13 columns":                 {table("["+strings.Join(many, ",")+"]", ""), nil, "1 to 12 columns"},
		"no columns":                 {table("[]", ""), nil, "1 to 12 columns"},
		"a column without a field":   {table(`[{"label":"Title"}]`, ""), nil, `needs "field"`},
		"a column without a label":   {table(`[{"field":"title"}]`, ""), nil, `needs "label"`},
		"a column with another key":  {table(`[{"field":"title","label":"T","width":3}]`, ""), nil, `unknown key "width"`},
		"columns that aren't a list": {table(`{"field":"title","label":"T"}`, ""), nil, "1 to 12 columns"},
		"open naming an insert":      {table(one, `,"open":"add_task"`), nil, "action \"add_task\" is insert"},
		"open naming no action":      {table(one, `,"open":"wipe"`), nil, `"wipe" is not one of the app's actions`},
		"open naming a navigate to a screen with no record": {table(one, `,"open":"new_task"`), nil,
			"doesn't show a record"},
		"open that isn't text":     {table(one, `,"open":3`), nil, "name of one of the app's actions"},
		"sort by an unknown field": {table(one, `,"sort":"nope"`), nil, `"nope" is not a field`},
		"sort by a photo field": {table(one, `,"sort":"photo"`), func(d *Definition) {
			d.Collection.Fields = append(d.Collection.Fields, Field{Name: "photo", Type: TypeAttachment})
		}, "attachment field"},
		"a dir of its own":      {table(one, `,"dir":"sideways"`), nil, "must be one of asc, desc"},
		"items of another path": {strings.Replace(table(one, ""), `"/items"`, `"/draft"`, 1), nil, `items must be {"path": "/items"}`},
		"items as a list":       {strings.Replace(table(one, ""), `{"path":"/items"}`, `[1,2]`, 1), nil, `items must be {"path": "/items"}`},
		"no caption":            {strings.Replace(table(one, ""), `"label":"Tasks",`, "", 1), nil, `Table needs "label"`},
		"no items":              {strings.Replace(table(one, ""), `"items":{"path":"/items"},`, "", 1), nil, `Table needs "items"`},
		"a prop of its own":     {table(one, `,"pageSize":10`), nil, `Table has no prop "pageSize"`},
	}
	for name, tt := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := withTable(t, tt.table, tt.change); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want ErrInvalid containing %q", err, tt.want)
			}
		})
	}
}

func TestATableCanSortAndShowEveryPlainFieldType(t *testing.T) {
	for _, typ := range columnTypes {
		t.Run(typ, func(t *testing.T) {
			field := Field{Name: "extra", Type: typ}
			if typ == TypeChoice {
				field.Options = []string{"a"}
			}
			table := `{"id":"tasks-table","component":"Table","items":{"path":"/items"},"label":"Tasks","columns":[{"field":"extra","label":"E"}],"sort":"extra"}`
			_, err := withTable(t, table, func(d *Definition) {
				d.Collection.Fields = append(d.Collection.Fields, field)
			})
			if err != nil {
				t.Fatalf("a %s column: %v", typ, err)
			}
		})
	}
}

func TestACatalogReferenceDocumentsTheTable(t *testing.T) {
	ref, err := Reference()
	if err != nil {
		t.Fatal(err)
	}
	table, ok := ref.Components["Table"]
	if !ok {
		t.Fatal("app_catalog has no Table")
	}
	for prop, want := range map[string]string{
		"columns": "1 to 12",
		"open":    "of navigate",
		"sort":    "of string, number, date, time, choice, boolean, barcode",
	} {
		if !strings.Contains(table[prop], want) {
			t.Errorf("Table.%s = %q, want it to say %q", prop, table[prop], want)
		}
	}
	if !strings.Contains(table["items"], "required") {
		t.Errorf("Table.items = %q", table["items"])
	}
	found := false
	for _, rule := range ref.Rules {
		found = found || strings.HasPrefix(rule, "Table lists the rows")
	}
	if !found {
		t.Fatal("app_catalog's rules don't describe the Table")
	}
}

func TestTheSurfaceTellsEachColumnItsFieldTypeAndTheDefinitionStaysAsAuthored(t *testing.T) {
	checked, err := withTable(t, goodTable, nil)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := json.Marshal(checked.Screens[0].Components)
	if strings.Contains(string(stored), `"type"`) {
		t.Fatalf("the stored definition holds a column type: %s", stored)
	}
	var table struct {
		Component string `json:"component"`
		Columns   []struct {
			Field string `json:"field"`
			Type  string `json:"type"`
		} `json:"columns"`
		Open string `json:"open"`
		Dir  string `json:"dir"`
	}
	found := false
	for _, raw := range checked.surfaceComponents() {
		var head struct {
			Component string `json:"component"`
		}
		if err := json.Unmarshal(raw, &head); err != nil || head.Component != "Table" {
			continue
		}
		found = true
		if err := json.Unmarshal(raw, &table); err != nil {
			t.Fatal(err)
		}
	}
	if !found {
		t.Fatal("the surface has no Table")
	}
	want := map[string]string{"title": TypeString, "category": TypeChoice, "due_date": TypeDate, "done": TypeBoolean}
	if len(table.Columns) != len(want) || table.Open != "open_task" || table.Dir != "desc" {
		t.Fatalf("table = %+v", table)
	}
	for _, c := range table.Columns {
		if c.Type != want[c.Field] {
			t.Errorf("column %s has type %q, want %q", c.Field, c.Type, want[c.Field])
		}
	}
}
