//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckWorkerImage(t *testing.T) {
	root := t.TempDir()
	homes := filepath.Join(root, "homes")
	for _, f := range []string{filepath.Join(root, "usr", "bin", "su"), filepath.Join(homes, "u1", "mine")} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := checkWorkerImage(root, homes, false); err != nil {
		t.Fatalf("no setuid programs: %v", err)
	}
	if err := os.Chmod(filepath.Join(homes, "u1", "mine"), 0o4755); err != nil {
		t.Fatal(err)
	}
	if err := checkWorkerImage(root, homes, false); err != nil {
		t.Fatalf("a setuid file in a home: %v", err)
	}
	su := filepath.Join(root, "usr", "bin", "su")
	if err := os.Chmod(su, 0o4755); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(su); err != nil || info.Mode()&os.ModeSetuid == 0 {
		t.Skip("this file system doesn't keep the setuid bit")
	}
	err := checkWorkerImage(root, homes, false)
	if err == nil || !strings.Contains(err.Error(), su) {
		t.Fatalf("a setuid su: %v", err)
	}
	if err := checkWorkerImage(root, homes, true); err != nil {
		t.Fatalf("with the override: %v", err)
	}
}
