package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"taskto.ai/taskto/internal/config"
	"taskto.ai/taskto/internal/db"
	"taskto.ai/taskto/internal/serverkey"
)

func cliWorkersDB(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := db.Migrate(context.Background(), sqlDB); err != nil {
		t.Fatal(err)
	}
	return sqlDB
}

func TestStartCLIWorkers(t *testing.T) {
	for _, mode := range []string{"", "off", "local", "local-unsafe"} {
		cfg := config.Default()
		cfg.DataDir = t.TempDir()
		cfg.CLIWorkers = mode
		svc, stop, err := startCLIWorkers(context.Background(), cfg, cliWorkersDB(t), testKey(t))
		if err != nil {
			t.Fatalf("%q: %v", mode, err)
		}
		want := mode
		if want == "" {
			want = "off"
		}
		if svc.Mode() != want {
			t.Errorf("%q: mode %q", mode, svc.Mode())
		}
		if err := stop(); err != nil {
			t.Errorf("%q: stop: %v", mode, err)
		}
	}
	cfg := config.Default()
	cfg.CLIWorkers = "everywhere"
	if _, _, err := startCLIWorkers(context.Background(), cfg, cliWorkersDB(t), testKey(t)); err == nil {
		t.Error("an unknown mode was accepted")
	}
}

func testKey(t *testing.T) *serverkey.Key {
	t.Helper()
	k, err := serverkey.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return k
}
