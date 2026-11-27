package main

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
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

func TestStartCLIWorkersRecordsTheWorkerKind(t *testing.T) {
	for _, c := range []struct {
		mode string
		want []string
	}{
		{"", nil},
		{"off", nil},
		{"local", []string{"local"}},
		{"local-unsafe", []string{"local"}},
		{"container", []string{"container"}},
	} {
		cfg := config.Default()
		cfg.DataDir = t.TempDir()
		cfg.CLIWorkers = c.mode
		cfg.CLIWorkerSocket = filepath.Join(t.TempDir(), "worker.sock")
		sqlDB := cliWorkersDB(t)
		_, stop, err := startCLIWorkers(context.Background(), cfg, sqlDB, testKey(t))
		if err != nil {
			t.Fatalf("%q: %v", c.mode, err)
		}
		if err := stop(); err != nil {
			t.Errorf("%q: stop: %v", c.mode, err)
		}
		rows, err := sqlDB.Query(`SELECT worker FROM cli_worker_history ORDER BY worker`)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var w string
			if err := rows.Scan(&w); err != nil {
				t.Fatal(err)
			}
			got = append(got, w)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%q recorded %v, want %v", c.mode, got, c.want)
		}
	}
}
