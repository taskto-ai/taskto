package db

import (
	"context"
	"database/sql"
	"testing"
)

// MigrateUpTo applies the migrations numbered up to version, as an older binary had them, for the
// tests of package db_test.
func MigrateUpTo(t *testing.T, sqlDB *sql.DB, version int) {
	t.Helper()
	if _, err := migrate(context.Background(), sqlDB, upTo(t, version)); err != nil {
		t.Fatal(err)
	}
}
