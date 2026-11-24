// Package audittest is what tests of the audit log share: the check that no known secret value
// ended up in any audit row.
package audittest

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// T is the part of *testing.T the check uses, so the check itself can be tested.
type T interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// NoSecrets fails the test if any value in secrets (a name for the message, and the secret's
// value) appears in any column of any audit event. A secret shorter than eight characters is
// refused as a probe: it would match by chance.
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
		for name, value := range secrets {
			if strings.Contains(row.String(), value) {
				t.Errorf("the %s is in an audit row: %q", name, strings.ReplaceAll(row.String(), "\x00", " | "))
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%v", err)
	}
}
