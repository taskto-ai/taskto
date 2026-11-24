package knowledge

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"taskto.ai/taskto/internal/access"
)

// streamMemory is how large a versioned file may be to be held in memory by Stream; a larger one
// is spooled to a temporary file.
var streamMemory int64 = 1 << 20

// Stream is Open for a reader that copies the file through and never needs it whole in memory: a
// versioned file's bytes are read from git into a temporary file (or, when small, memory) while
// the space is held for readers, and then the space is let go, so a slow reader never holds a
// writer up. An unversioned file is read from its own storage, which no change locks. It reads
// only: nothing here writes the repository.
//
// The file is as it was at the commit at (the head if empty). The caller closes the reader, which
// removes any temporary file.
func (s *Store) Stream(_ context.Context, sp access.Space, name, at string) (Entry, io.ReadCloser, error) {
	if err := checkPath(name); err != nil {
		return Entry{}, nil, err
	}
	var entry Entry
	var rc io.ReadCloser
	err := s.read(sp, func(r *repo) error {
		c, err := r.revision(at)
		if err != nil {
			return err
		}
		e, err := r.entryAt(c, name)
		if err != nil {
			return err
		}
		if e.Mode == filemode.Dir {
			return fmt.Errorf("%w: %s is a folder", ErrNotFound, name)
		}
		var p *pointer
		if entry, p, err = r.describe(name, e.Hash); err != nil {
			return err
		}
		if p != nil {
			f, err := r.openObject(p, name)
			if err == nil {
				rc = f // an open file stays readable after the space is let go
			}
			return err
		}
		rc, err = spoolBlob(r, e.Hash, entry.Size)
		return err
	})
	if err != nil {
		return Entry{}, nil, err
	}
	return entry, rc, nil
}

// spoolBlob copies the blob h out of the repository: into memory if it is small, else into a
// temporary file that its Close removes.
func spoolBlob(r *repo, h plumbing.Hash, size int64) (io.ReadCloser, error) {
	blob, err := object.GetBlob(r.st, h)
	if err != nil {
		return nil, err
	}
	br, err := blob.Reader()
	if err != nil {
		return nil, err
	}
	defer br.Close()
	if size <= streamMemory {
		b, err := io.ReadAll(br)
		return io.NopCloser(bytes.NewReader(b)), err
	}
	f, err := os.CreateTemp("", "taskto-export-*")
	if err != nil {
		return nil, err
	}
	sp := &spooled{File: f}
	if _, err := io.Copy(f, br); err != nil {
		_ = sp.Close()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = sp.Close()
		return nil, err
	}
	return sp, nil
}

// spooled is a temporary file that is removed when it is closed.
type spooled struct{ *os.File }

func (s *spooled) Close() error {
	err := s.File.Close()
	if rmErr := os.Remove(s.Name()); err == nil {
		err = rmErr
	}
	return err
}
