//go:build unix

package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliaccounts"
	"taskto.ai/taskto/internal/config"
)

// In container mode the worker's network can't reach the server (docs/cli-workers.md), so
// the shared accounts' proxy listens on a socket in the volume both mount, and the worker
// relays the CLIs' connections to it from 127.0.0.1. The server may start before the worker
// has made the socket's directory: it waits for it.
func TestCLIProxyOverTheWorkersRelay(t *testing.T) {
	// Short: a Unix socket's path has a length limit, and macOS's temporary directory is long.
	base, err := os.MkdirTemp("/tmp", "taskto-proxy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	dir := filepath.Join(base, "proxy")
	socket := filepath.Join(dir, "proxy.sock")

	cfg := config.Default()
	cfg.CLIWorkers = "container"
	cfg.CLIProxyListen = "unix:" + socket
	cfg.CLIProxyURL = "http://127.0.0.1:8081"
	svc := cliaccounts.New(cliWorkersDB(t), cliaccounts.Container, nil)
	defer svc.Close()
	stop, err := startCLIProxy(context.Background(), cfg, svc, testKey(t), cliaccounts.Container)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := stop(); err != nil {
			t.Errorf("stop: %v", err)
		}
	}()

	// The worker starts later: it makes the directory, the server's, and starts its relay.
	if err := prepareProxyDir(dir, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o711 {
		t.Fatalf("the socket's directory: %v %v", info.Mode(), err)
	}
	relay, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	go relayProxy(relay, socket)

	// A request without a token, through the relay, gets the proxy's own refusal.
	url := "http://" + relay.Addr().String()
	var status int
	var body string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		resp, err := http.Get(url + "/cli-proxy/v1/models") //nolint:noctx // a test
		if err != nil {
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		status, body = resp.StatusCode, string(raw)
		break
	}
	if status != http.StatusUnauthorized || !strings.Contains(body, "taskto_proxy_error") {
		t.Fatalf("through the relay: %d %s", status, body)
	}
	if info, err := os.Stat(socket); err != nil || info.Mode().Perm() != 0o666 {
		t.Errorf("the socket: %v %v", info, err)
	}
	// Only the proxy's routes.
	resp, err := http.Get(url + "/api/setup") //nolint:noctx // a test
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/api/setup through the relay: %d", resp.StatusCode)
	}

	// A restarted server replaces the socket the last one left.
	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	stop2, err := startCLIProxy(context.Background(), cfg, svc, testKey(t), cliaccounts.Container)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := stop2(); err != nil {
			t.Errorf("stop: %v", err)
		}
	}()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		resp, err := http.Get(url + "/cli-proxy/v1/models") //nolint:noctx // a test
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("after a restart: %v", err)
		}
	}
}

// The relay doesn't make up an answer when the server isn't there: the CLI sees the
// connection close, and retries as it would any network error.
func TestCLIProxyRelayWithoutTheServer(t *testing.T) {
	relay, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	go relayProxy(relay, filepath.Join(t.TempDir(), "none.sock"))
	resp, err := http.Get("http://" + relay.Addr().String() + "/cli-proxy/v1/models") //nolint:noctx // a test
	if err == nil {
		_ = resp.Body.Close()
		t.Errorf("an answer with no server behind the relay: %d", resp.StatusCode)
	}
}

func TestPrepareProxyDir(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareProxyDir(file, os.Getuid(), os.Getgid()); err == nil {
		t.Error("a file taken for the directory")
	}
	// Again on the directory it made: nothing to change.
	dir := filepath.Join(base, "proxy")
	for range 2 {
		if err := prepareProxyDir(dir, os.Getuid(), os.Getgid()); err != nil {
			t.Fatal(err)
		}
	}
	if os.Getuid() != 0 {
		// Someone else's directory: not root's, and not the server's.
		if err := prepareProxyDir(dir, os.Getuid()+1, os.Getgid()); err == nil {
			t.Error("a directory of another user's accepted")
		}
	}
}
