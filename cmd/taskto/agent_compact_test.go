//go:build unix

package main

import (
	"bytes"
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/lockfile"
)

func agentDB(t *testing.T) (dataDir string) {
	t.Helper()
	dataDir = t.TempDir()
	_, _, path := agentPaths(dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{"PRAGMA journal_mode = WAL", "CREATE TABLE entries (id INTEGER PRIMARY KEY, text TEXT)"} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	return dataDir
}

func TestAgentCompactRefusesARunningServer(t *testing.T) {
	dataDir := agentDB(t)
	getenv := func(string) string { return "" }
	server, err := lockfile.DataDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	err = runAgentCompact(context.Background(), []string{"-data-dir", dataDir}, getenv, &stderr)
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("with the server running: %v", err)
	}
	_ = server.Close()
	stderr.Reset()
	if err := runAgentCompact(context.Background(), []string{"-data-dir", dataDir}, getenv, &stderr); err != nil {
		t.Fatalf("with the server stopped: %v (%s)", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "recorded it") {
		t.Fatalf("output %q", stderr.String())
	}
}
