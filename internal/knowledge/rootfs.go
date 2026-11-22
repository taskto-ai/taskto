package knowledge

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/go-git/go-billy/v5"
)

// rootFS is a billy.Filesystem over an *os.Root, so go-git's own file access (objects,
// refs, the index) goes through the space's os.Root like everything else: it can't follow
// a link out of the space, whatever names it builds. Every name is taken relative to
// prefix inside the root (".git" for the repository).
//
// It does what go-git's storage needs, the way billy's osfs does it: creating a file or
// renaming one onto a path creates the missing parent directories. Whatever permissions
// go-git asks for, files and directories are for the server's user only.
type rootFS struct {
	root   *os.Root
	prefix string
}

var _ billy.Filesystem = rootFS{}

func (f rootFS) name(name string) string {
	return path.Join(f.prefix, filepath.ToSlash(name))
}

func (f rootFS) Create(name string) (billy.File, error) {
	return f.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o666)
}

func (f rootFS) Open(name string) (billy.File, error) {
	return f.OpenFile(name, os.O_RDONLY, 0)
}

func (f rootFS) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	full := f.name(name)
	if flag&os.O_CREATE != 0 {
		if err := f.root.MkdirAll(path.Dir(full), 0o700); err != nil {
			return nil, err
		}
	}
	file, err := f.root.OpenFile(full, flag, perm&0o700)
	if err != nil {
		return nil, err
	}
	return rootFile{File: file, name: name}, nil
}

func (f rootFS) Stat(name string) (os.FileInfo, error)  { return f.root.Stat(f.name(name)) }
func (f rootFS) Lstat(name string) (os.FileInfo, error) { return f.root.Lstat(f.name(name)) }

func (f rootFS) Rename(oldpath, newpath string) error {
	to := f.name(newpath)
	if err := f.root.MkdirAll(path.Dir(to), 0o700); err != nil {
		return err
	}
	return f.root.Rename(f.name(oldpath), to)
}

func (f rootFS) Remove(name string) error { return f.root.Remove(f.name(name)) }

func (f rootFS) Join(elem ...string) string { return path.Join(elem...) }

func (f rootFS) TempFile(dir, prefix string) (billy.File, error) {
	for {
		name := path.Join(dir, prefix+randomSuffix())
		file, err := f.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return file, err
	}
}

func (f rootFS) ReadDir(name string) ([]os.FileInfo, error) {
	dir, err := f.root.Open(f.name(name))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	infos := make([]os.FileInfo, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue // removed since it was listed
		}
		if err != nil {
			return nil, err
		}
		infos = append(infos, info)
	}
	return infos, nil
}

func (f rootFS) MkdirAll(name string, _ os.FileMode) error {
	return f.root.MkdirAll(f.name(name), 0o700)
}

func (f rootFS) Symlink(target, link string) error {
	return f.root.Symlink(target, f.name(link))
}

func (f rootFS) Readlink(link string) (string, error) { return f.root.Readlink(f.name(link)) }

// Chmod lets go-git make loose objects read-only, as git does.
func (f rootFS) Chmod(name string, mode os.FileMode) error {
	return f.root.Chmod(f.name(name), mode)
}

func (f rootFS) Chroot(name string) (billy.Filesystem, error) {
	return rootFS{root: f.root, prefix: f.name(name)}, nil
}

// Root is only used by go-git to describe paths; every access goes through the os.Root.
func (f rootFS) Root() string { return filepath.Join(f.root.Name(), filepath.FromSlash(f.prefix)) }

// rootFile is an open file of a rootFS. Its Name is the name it was opened with, which
// go-git passes back to Rename and Remove.
type rootFile struct {
	*os.File
	name string
}

func (f rootFile) Name() string { return f.name }

// Lock and Unlock are no-ops: the Store lets one writer at a time into a space's
// repository, and nothing else in the server writes there.
func (f rootFile) Lock() error   { return nil }
func (f rootFile) Unlock() error { return nil }

func randomSuffix() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return hex.EncodeToString(b)
}
