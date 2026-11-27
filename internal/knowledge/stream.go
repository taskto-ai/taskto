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

var streamMemory int64 = 1 << 20

func (s *Store) Stream(ctx context.Context, sp access.Space, name, at string) (Entry, io.ReadCloser, error) {
	if err := checkPath(name); err != nil {
		return Entry{}, nil, err
	}
	var entry Entry
	var rc io.ReadCloser
	err := s.read(sp, func(r *repo) error {
		c, err := r.revision(ctx, at)
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
				rc = f
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

type spooled struct{ *os.File }

func (s *spooled) Close() error {
	err := s.File.Close()
	if rmErr := os.Remove(s.Name()); err == nil {
		err = rmErr
	}
	return err
}
