package apps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"taskto.ai/taskto/internal/db"
)

// Every test makes a database with all the migrations, and migrating is most of what a fixture
// costs: about two seconds of CPU on a machine that isn't busy, many times that under the race
// detector (SQLite is compiled to Go, and checks every pointer), which made the package's tests
// run past the go tool's ten-minute limit there. The migrations are applied once, to a file that
// every test copies; db.Migrate still runs on the copy and finds nothing left to do.
var template struct {
	once sync.Once
	dir  string
	path string
	err  error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if template.dir != "" {
		_ = os.RemoveAll(template.dir)
	}
	os.Exit(code)
}

// migratedDatabase returns the path of a copy of the migrated template, in dir.
func migratedDatabase(t *testing.T, dir string) string {
	t.Helper()
	template.once.Do(func() {
		ctx := context.Background()
		if template.dir, template.err = os.MkdirTemp("", "apps-template-"); template.err != nil {
			return
		}
		template.path = filepath.Join(template.dir, db.FileName)
		sqlDB, err := db.Open(ctx, template.path)
		if err != nil {
			template.err = err
			return
		}
		defer sqlDB.Close()
		if _, err := db.Migrate(ctx, sqlDB); err != nil {
			template.err = err
			return
		}
		// Everything in the one file: no write-ahead log is left to copy.
		_, template.err = sqlDB.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	})
	if template.err != nil {
		t.Fatal(template.err)
	}
	raw, err := os.ReadFile(template.path)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, db.FileName)
	if err := os.WriteFile(out, raw, 0o600); err != nil {
		t.Fatal(fmt.Errorf("copying the migrated database: %w", err))
	}
	return out
}
