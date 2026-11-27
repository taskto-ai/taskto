package audittest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

func decoded(column any) string {
	raw, _ := column.(string)
	if b, ok := column.([]byte); ok {
		raw = string(b)
	}
	var v any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return ""
	}
	var out strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			out.WriteString(v + "\x00")
		case []any:
			for _, x := range v {
				walk(x)
			}
		case map[string]any:
			for k, x := range v {
				out.WriteString(k + "\x00")
				walk(x)
			}
		}
	}
	walk(v)
	return out.String()
}

type T interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

func NoSecrets(t T, db *sql.DB, secrets map[string]string) {
	t.Helper()
	for name, value := range secrets {
		if len(value) < 8 {
			t.Fatalf("the probe for %s is %q: use a value of at least 8 characters, or it may match by chance", name, value)
		}
	}
	rows, err := db.QueryContext(context.Background(), `SELECT id, at, coalesce(workspace_id, ''), coalesce(actor_id, ''), action, target_type,
		coalesce(target_id, ''), details, coalesce(client_addr, '') FROM audit_events`)
	if err != nil {
		t.Fatalf("%v", err)
	}
	defer rows.Close()
	for rows.Next() {
		cols := make([]any, 9)
		for i := range cols {
			cols[i] = new(any)
		}
		if err := rows.Scan(cols...); err != nil {
			t.Fatalf("%v", err)
		}
		var row strings.Builder
		for _, c := range cols {
			v := *(c.(*any))
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			fmt.Fprintf(&row, "%v\x00", v)
		}
		text := row.String() + "\x00" + decoded(*(cols[7].(*any)))
		for name, value := range secrets {
			if strings.Contains(text, value) {
				t.Errorf("the %s is in an audit row: %q", name, strings.ReplaceAll(row.String(), "\x00", " | "))
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%v", err)
	}
}
