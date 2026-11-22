package access

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

// Each space keeps its files in its own directory under the data directory:
//
//	<data>/workspaces/<workspace ID>/spaces/<space ID>/
//
// The path is built from IDs only, never from names people type. The knowledge base's
// git repository and the agent's file access work inside it, and reach files by names
// that come from people and models: those go through SafeJoin or an *os.Root from
// OpenRoot, never through filepath.Join.

// SpaceDir is the directory of a space's files. It doesn't create it.
func SpaceDir(dataDir string, workspaceID, spaceID int64) string {
	return filepath.Join(dataDir, "workspaces", strconv.FormatInt(workspaceID, 10),
		"spaces", strconv.FormatInt(spaceID, 10))
}

// Dir is SpaceDir for s.
func (s Space) Dir(dataDir string) string { return SpaceDir(dataDir, s.WorkspaceID, s.ID) }

// OpenRoot creates s's directory if needed, readable by the server's user only, and opens
// it as an *os.Root. Prefer it to SafeJoin for reading and writing files: an os.Root
// checks every open as it happens, so a symlink swapped in after a check can't lead out.
func OpenRoot(dataDir string, s Space) (*os.Root, error) {
	dir := s.Dir(dataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("space directory: %w", err)
	}
	return os.OpenRoot(dir)
}

// ErrUnsafePath is the error for a name that could lead outside its space's directory,
// or into the parts of it nobody may touch directly.
var ErrUnsafePath = errors.New("unsafe path")

func unsafe(name, why string) error {
	return fmt.Errorf("%w %q: %s", ErrUnsafePath, name, why)
}

// CheckName checks a file name relative to a space's directory, written with forward
// slashes like io/fs names ("notes/2026/plan.md"). It refuses:
//
//   - empty names, ".", and names that aren't in their shortest form ("a//b", "a/./b",
//     trailing slashes), so one file has one name;
//   - absolute names and Windows volumes ("/etc/passwd", "C:\x", "\\host\share");
//   - any ".." element, even one that would stay inside;
//   - backslashes, NUL and other control characters;
//   - Windows' reserved names (NUL, COM1, ...), on Windows;
//   - any ".git" element, also spelled as the case-insensitive file systems of macOS and
//     Windows would read it (".GIT", ".git.", ".g\u200cit", "git~1"): a space's knowledge
//     base is a git repository, and writing its hooks or config would run code on the
//     server.
func CheckName(name string) error {
	switch {
	case name == "" || name == ".":
		return unsafe(name, "empty")
	case strings.HasPrefix(name, "/"):
		return unsafe(name, "absolute")
	}
	for _, r := range name {
		if r == '\\' {
			return unsafe(name, "backslash")
		}
		if unicode.IsControl(r) {
			return unsafe(name, "control character")
		}
	}
	if path.Clean(name) != name {
		return unsafe(name, "not in its shortest form")
	}
	for elem := range strings.SplitSeq(name, "/") {
		if elem == ".." {
			return unsafe(name, "parent directory")
		}
		if isGitDir(elem) {
			return unsafe(name, "git metadata")
		}
	}
	if !filepath.IsLocal(filepath.FromSlash(name)) {
		return unsafe(name, "not a local name on this system")
	}
	return nil
}

// isGitDir reports whether a path element names a git directory on some file system:
// case-insensitive, ignoring trailing dots and spaces (Windows), the code points HFS+
// ignores (macOS), and the 8.3 short name (Windows).
func isGitDir(elem string) bool {
	elem = strings.Map(func(r rune) rune {
		if hfsIgnorable(r) {
			return -1
		}
		return r
	}, strings.ToLower(elem))
	elem = strings.TrimRight(elem, ". ")
	return elem == ".git" || elem == "git~1"
}

// hfsIgnorable reports the code points HFS+ drops when comparing names, the list git
// itself checks (see is_hfs_dotgit in git's utf8.c).
func hfsIgnorable(r rune) bool {
	return (r >= 0x200c && r <= 0x200f) || (r >= 0x202a && r <= 0x202e) ||
		(r >= 0x206a && r <= 0x206f) || r == 0xfeff
}

// SafeJoin returns root joined with name, for tools that need a path rather than an
// open file (git, the agent's workers). It refuses names CheckName refuses, and names
// that go through a symlink leading outside root or to nothing (a write through a
// dangling link would create its target, wherever that is). root must exist.
//
// The check reflects the disk when it runs: when a file is then opened by this process,
// open it through OpenRoot instead, which holds against links changed in between.
func SafeJoin(root, name string) (string, error) {
	if err := CheckName(name); err != nil {
		return "", err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	p := root
	for elem := range strings.SplitSeq(name, "/") {
		p = filepath.Join(p, elem)
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			break // nothing below a missing element exists, symlinks included
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			continue
		}
		target, err := filepath.EvalSymlinks(p)
		if errors.Is(err, fs.ErrNotExist) {
			return "", unsafe(name, "dangling symlink")
		}
		if err != nil {
			return "", err
		}
		if !within(realRoot, target) {
			return "", unsafe(name, "symlink out of the space")
		}
	}
	return filepath.Join(root, filepath.FromSlash(name)), nil
}

// within reports whether p is dir or inside it. Both must be clean and absolute, or
// relative to the same directory.
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}
