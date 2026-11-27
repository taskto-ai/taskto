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

func SpaceDir(dataDir string, workspaceID, spaceID int64) string {
	return filepath.Join(dataDir, "workspaces", strconv.FormatInt(workspaceID, 10),
		"spaces", strconv.FormatInt(spaceID, 10))
}

func (s Space) Dir(dataDir string) string { return SpaceDir(dataDir, s.WorkspaceID, s.ID) }

func OpenRoot(dataDir string, s Space) (*os.Root, error) {
	dir := s.Dir(dataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("space directory: %w", err)
	}
	return os.OpenRoot(dir)
}

var ErrUnsafePath = errors.New("unsafe path")

func unsafe(name, why string) error {
	return fmt.Errorf("%w %q: %s", ErrUnsafePath, name, why)
}

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
		if isManagedDir(elem) {
			return unsafe(name, "the server's managed storage")
		}
	}
	if !filepath.IsLocal(filepath.FromSlash(name)) {
		return unsafe(name, "not a local name on this system")
	}
	return nil
}

const ManagedDir = ".taskto"

func isManagedDir(elem string) bool {
	elem = normalizedElem(elem)
	return elem == ManagedDir || elem == "taskto~1"
}

func isGitDir(elem string) bool {
	elem = normalizedElem(elem)
	return elem == ".git" || elem == "git~1"
}

func normalizedElem(elem string) string {
	elem = strings.Map(func(r rune) rune {
		if hfsIgnorable(r) {
			return -1
		}
		return r
	}, strings.ToLower(elem))
	return strings.TrimRight(elem, ". ")
}

func hfsIgnorable(r rune) bool {
	return (r >= 0x200c && r <= 0x200f) || (r >= 0x202a && r <= 0x202e) ||
		(r >= 0x206a && r <= 0x206f) || r == 0xfeff
}

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
			break
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

func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}
