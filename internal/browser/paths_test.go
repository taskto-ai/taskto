//go:build unix

package browser

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func maxSocketPath() int {
	if runtime.GOOS == "linux" {
		return 107
	}
	return 103
}

func TestAnAccountHomesSocketsFitWhereTheyDidBefore(t *testing.T) {
	for _, n := range []string{"1", "12", "123456"} {
		root := "/data/cli"
		legacy := Paths{Root: filepath.Join(root, "u"+n, "browser")}
		now := HomePaths(filepath.Join(root, "h2147483647"))
		if len(now.VNCSocket()) > len(legacy.VNCSocket()) {
			t.Errorf("the VNC socket %s is longer than %s", now.VNCSocket(), legacy.VNCSocket())
		}
		if len(now.Tmp()) > len(legacy.Tmp()) {
			t.Errorf("$TMPDIR %s is longer than %s", now.Tmp(), legacy.Tmp())
		}
	}

	base, err := os.MkdirTemp("/tmp", "s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	short := len(HomePaths(filepath.Join(base, "x", "h2147483647")).VNCSocket())
	pad := strings.Repeat("d", maxSocketPath()-short+1)
	p := HomePaths(filepath.Join(base, pad, "h2147483647"))
	if len(p.VNCSocket()) != maxSocketPath() {
		t.Fatalf("fixture: the socket's path is %d bytes, want %d", len(p.VNCSocket()), maxSocketPath())
	}
	if err := Prepare(p, ProcSpec{Program: X11vnc, Kind: Personal, Display: 1}); err != nil {
		t.Fatal(err)
	}
	ln, err := new(net.ListenConfig).Listen(context.Background(), "unix", p.VNCSocket())
	if err != nil {
		t.Fatalf("the VNC socket at the limit: %v", err)
	}
	_ = ln.Close()
}
