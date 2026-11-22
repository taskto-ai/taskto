package access

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSpaceDir(t *testing.T) {
	got := Space{ID: 7, WorkspaceID: 3}.Dir(filepath.FromSlash("/srv/data"))
	if want := filepath.FromSlash("/srv/data/workspaces/3/spaces/7"); got != want {
		t.Errorf("Dir = %s, want %s", got, want)
	}
}

func TestCheckName(t *testing.T) {
	ok := []string{
		"a", "notes/plan.md", "a b/c d.md", "ü/日本語.md", "..a", "a..", "...", "a/b/c/d/e",
		// Look like git's, but aren't its directory.
		".gitignore", ".github/workflows/ci.yml", "git", "a/.gitkeep", "gitx", ".gitx",
	}
	for _, name := range ok {
		if err := CheckName(name); err != nil {
			t.Errorf("CheckName(%q) = %v, want ok", name, err)
		}
	}
	refused := []string{
		// Empty.
		"", ".",
		// Absolute.
		"/", "/etc/passwd", "//host/share",
		// Parents, also ones that would stay inside.
		"..", "../x", "a/../b", "a/..", "a/b/../../..",
		// Not the shortest form.
		"a//b", "a/./b", "./a", "a/", "a/.",
		// Backslashes: Windows separators, and names that mean other things to other tools.
		`a\b`, `..\x`, `C:\x`, `\\host\share`,
		// Control characters.
		"a\x00b", "a\nb", "a\rb", "\x7f", "a\tb",
		// git's directory, in every spelling some file system reads as it.
		".git", ".git/config", "a/.git/hooks/pre-commit", ".GIT", ".Git/x", ".git.", ".git ", ".git. .",
		".g\u200cit", "\ufeff.git", ".gi\u206at/hooks", "git~1/config", "GIT~1",
	}
	for _, name := range refused {
		if err := CheckName(name); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("CheckName(%q) = %v, want ErrUnsafePath", name, err)
		}
	}
}

// symlink makes a link, or skips the test where the system can't.
func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

// linkedSpace builds a space directory with links in and out of it:
//
//	tmp/outside/secret
//	tmp/space/inside/note.md
//	tmp/space/in-link       -> inside
//	tmp/space/abs-in        -> tmp/space/inside
//	tmp/space/self          -> .
//	tmp/space/out-link      -> ../outside
//	tmp/space/abs-out       -> tmp/outside
//	tmp/space/up            -> ..
//	tmp/space/chain         -> in-link/to-out
//	tmp/space/inside/to-out -> ../../outside
//	tmp/space/dangling      -> nowhere
//	tmp/space/to-file-out   -> ../outside/secret
//	tmp/space-link          -> space
func linkedSpace(t *testing.T) (tmp, root string) {
	t.Helper()
	tmp = t.TempDir()
	root = filepath.Join(tmp, "space")
	mkdir(t, filepath.Join(root, "inside"))
	mkdir(t, filepath.Join(tmp, "outside"))
	for _, f := range []string{filepath.Join(root, "inside", "note.md"), filepath.Join(tmp, "outside", "secret")} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	links := map[string]string{
		"in-link":     "inside",
		"abs-in":      filepath.Join(root, "inside"),
		"self":        ".",
		"out-link":    filepath.Join("..", "outside"),
		"abs-out":     filepath.Join(tmp, "outside"),
		"up":          "..",
		"chain":       filepath.Join("in-link", "to-out"),
		"dangling":    "nowhere",
		"to-file-out": filepath.Join("..", "outside", "secret"),
	}
	for link, target := range links {
		symlink(t, target, filepath.Join(root, link))
	}
	symlink(t, filepath.Join("..", "..", "outside"), filepath.Join(root, "inside", "to-out"))
	symlink(t, "space", filepath.Join(tmp, "space-link"))
	return tmp, root
}

func TestSafeJoin(t *testing.T) {
	tmp, root := linkedSpace(t)
	ok := []string{
		"inside/note.md", "inside/new.md", "new/deep/file.md", "in-link/note.md", "in-link/new.md",
		"abs-in/note.md", "self/inside/note.md", "self",
	}
	refused := []string{
		"out-link", "out-link/secret", "out-link/new.md", "abs-out/secret", "up/outside/secret", "up",
		"chain/secret", "in-link/to-out/secret", "inside/to-out", "dangling", "dangling/x",
		"to-file-out", "self/out-link/secret",
		// CheckName's refusals come first.
		"../outside/secret", "/etc/passwd", ".git/config",
	}
	// The same through a root that is itself a link, as a data directory may be.
	for _, r := range []string{root, filepath.Join(tmp, "space-link")} {
		for _, name := range ok {
			got, err := SafeJoin(r, name)
			if want := filepath.Join(r, filepath.FromSlash(name)); err != nil || got != want {
				t.Errorf("SafeJoin(%s, %q) = %q, %v; want %q", r, name, got, err, want)
			}
		}
		for _, name := range refused {
			if got, err := SafeJoin(r, name); !errors.Is(err, ErrUnsafePath) {
				t.Errorf("SafeJoin(%s, %q) = %q, %v; want ErrUnsafePath", r, name, got, err)
			}
		}
	}

	if _, err := SafeJoin(filepath.Join(tmp, "missing"), "a"); err == nil {
		t.Error("SafeJoin under a root that doesn't exist: no error")
	}
}

func TestOpenRoot(t *testing.T) {
	data := t.TempDir()
	s := Space{ID: 2, WorkspaceID: 1}
	root, err := OpenRoot(data, s)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	fi, err := os.Stat(s.Dir(data))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("space directory mode = %v, want 0700", fi.Mode().Perm())
	}

	mkdir(t, filepath.Join(data, "outside"))
	symlink(t, filepath.Join(data, "outside"), filepath.Join(s.Dir(data), "out"))
	if err := root.WriteFile("note.md", []byte("x"), 0o600); err != nil {
		t.Errorf("writing inside: %v", err)
	}
	for _, name := range []string{"../escape", "out/escape", "/etc/passwd"} {
		if err := root.WriteFile(name, []byte("x"), 0o600); err == nil {
			t.Errorf("writing %q through the root: allowed", name)
		}
	}
	if _, err := os.Stat(filepath.Join(data, "outside", "escape")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a file appeared outside the space: %v", err)
	}
}
