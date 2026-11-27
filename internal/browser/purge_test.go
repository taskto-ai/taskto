//go:build unix

package browser

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPurgeRemovesReadOnlyPartsAndNoLinksTarget(t *testing.T) {
	for name, remove := range map[string]func(Paths) error{"purge": Purge, "clean": Clean} {
		t.Run(name, func(t *testing.T) {
			p := Paths{Root: filepath.Join(t.TempDir(), "browser")}
			target := p.Root
			if name == "clean" {
				target = p.Testing()
			}
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.MkdirAll(outside, 0o700); err != nil {
				t.Fatal(err)
			}
			kept := filepath.Join(outside, "kept.txt")
			if err := os.WriteFile(kept, []byte("kept"), 0o600); err != nil {
				t.Fatal(err)
			}
			locked := filepath.Join(target, "a", "locked")
			if err := os.MkdirAll(filepath.Join(locked, "inner"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(locked, "inner", "f"), []byte("x"), 0o400); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(target, "a", "link")); err != nil {
				t.Fatal(err)
			}
			for _, d := range []string{filepath.Join(locked, "inner"), locked} {
				if err := os.Chmod(d, 0o500); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				_ = os.Chmod(locked, 0o700)
				_ = os.Chmod(filepath.Join(locked, "inner"), 0o700)
			})
			if err := remove(p); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Errorf("%s left %s: %v", name, target, err)
			}
			if _, err := os.Stat(kept); err != nil {
				t.Errorf("%s followed a link out of the directory: %v", name, err)
			}
		})
	}
}
