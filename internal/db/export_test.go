package db

import (
	"context"
	"database/sql"
	"testing"
)

func MigrateUpTo(t *testing.T, sqlDB *sql.DB, version int) {
	t.Helper()
	if _, err := migrate(context.Background(), sqlDB, upTo(t, version)); err != nil {
		t.Fatal(err)
	}
}
