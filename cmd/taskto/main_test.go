package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"taskto.ai/taskto/internal/config"
	"taskto.ai/taskto/internal/db"
	"taskto.ai/taskto/internal/serverkey"
)

// TestRunStartsServesAndStops runs the server as the binary does: it creates the data
// directory and database, answers /healthz, and returns cleanly once ctx is cancelled.
func TestRunStartsServesAndStops(t *testing.T) {
	cfg := config.Default()
	cfg.Addr = "127.0.0.1:0"
	cfg.DataDir = filepath.Join(t.TempDir(), "nested", "data")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrc := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, func(a net.Addr) { addrc <- a }) }()

	var addr net.Addr
	select {
	case addr = <-addrc:
	case err := <-done:
		t.Fatalf("run returned before listening: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("run didn't start listening")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr.String()+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	err = json.NewDecoder(resp.Body).Decode(&body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || body["database"] != "ok" || body["agent"] != "off" {
		t.Fatalf("healthz = %d %v (%v), want 200 with the database ok and the agent runtime off", resp.StatusCode, body, err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run after cancel: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run didn't stop after cancel")
	}

	// The database was created in the data directory and migrated.
	sqlDB, err := sql.Open("sqlite", filepath.Join(cfg.DataDir, db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	var n int
	if err := sqlDB.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil || n == 0 {
		t.Fatalf("schema_migrations rows = %d (%v), want the applied migrations", n, err)
	}

	// And the server key, owner-only.
	info, err := os.Stat(filepath.Join(cfg.DataDir, serverkey.FileName))
	if err != nil || info.Mode().Perm() != 0o600 || info.Size() != 32 {
		t.Fatalf("server key: %v, %v; want 32 bytes with mode 600", info, err)
	}
}

func TestRunFailsWithoutADataDirectory(t *testing.T) {
	// A file where the data directory should be.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Addr = "127.0.0.1:0"
	cfg.DataDir = file
	if err := run(context.Background(), cfg, nil); err == nil {
		t.Fatal("run started without a usable data directory")
	}
}
