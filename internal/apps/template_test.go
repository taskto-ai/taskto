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
