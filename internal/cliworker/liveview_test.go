//go:build unix

package cliworker_test

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"taskto.ai/taskto/internal/browser"
	"taskto.ai/taskto/internal/cliworker"
)

func TestALiveViewHandedOutAsItsAccountGoesEnds(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "lv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	h := newHarness(t, func(c *cliworker.Config) { c.Layout.Root = filepath.Join(root, "cli") })
	h.introduce(t, 1)
	p := h.layout.BrowserPaths(h.w.HomeForTest(acct(1)))
	if err := os.MkdirAll(p.Run(), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := new(net.ListenConfig).Listen(context.Background(), "unix", p.VNCSocket())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c) }()
		}
	}()

	ctx := context.Background()
	forgot := make(chan error, 1)
	h.w.SetHandOffHookForTest(func() {
		go func() { forgot <- h.w.Forget(ctx, acct(1)) }()
		eventually(t, "the removal didn't begin", func() bool { return h.w.GatedForTest(1) })
	})
	host := h.w.BrowserHost(map[browser.Program]string{})
	conn, err := host.DialVNC(ctx, acct(1))
	if err != nil {
		t.Fatalf("the dial: %v", err)
	}
	select {
	case err := <-forgot:
		if err != nil {
			t.Fatalf("the removal: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the removal waits for a live view that was handed out as it began")
	}
	if _, err := conn.Write([]byte("x")); err == nil {
		if _, err := conn.Read(make([]byte, 1)); err == nil {
			t.Error("the live view still works after its account's removal")
		}
	}
	_ = conn.Close()
}
