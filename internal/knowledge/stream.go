package knowledge

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"taskto.ai/taskto/internal/access"
)

// Stream is Open for a reader that copies the file through and never needs it whole: a
// versioned file's bytes are read from git as they are copied, not loaded first, so the memory
// it takes doesn't grow with the file. It reads only: nothing here writes the repository.
//
// The file is as it was at the commit at (the head if empty). An unversioned file's bytes come
// from its own storage, which no change locks, so for one the space is not held while it is
// read. A versioned file's bytes come from the repository, which is held for readers until Close
// (a writer to the space waits meanwhile), so the caller copies it at once, with a deadline of
// its own, and closes it, and holds no other call of the Store open.
func (s *Store) Stream(_ context.Context, sp access.Space, name, at string) (Entry, io.ReadCloser, error) {
	if err := checkPath(name); err != nil {
		return Entry{}, nil, err
	}
	if !sp.Access.Allows(access.Read) {
		return Entry{}, nil, access.ErrNotFound
	}
	st := s.state(sp.ID)
	st.mu.RLock()
	r, err := openRepo(s.dataDir, sp, st, false)
	if err == nil && r == nil {
		err = errNoRepo
	}
	if err != nil {
		st.mu.RUnlock()
		return Entry{}, nil, err
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			r.close()
			st.mu.RUnlock()
		})
	}
	entry, rc, hold, err := streamIn(r, name, at)
	if err != nil || !hold {
		release()
	}
	if err != nil {
		return Entry{}, nil, err
	}
	if !hold {
		return entry, rc, nil
	}
	return entry, &heldReader{ReadCloser: rc, release: release}, nil
}

// streamIn opens name at the commit at in r. hold says the reader needs r kept open.
func streamIn(r *repo, name, at string) (entry Entry, rc io.ReadCloser, hold bool, err error) {
	c, err := r.revision(at)
	if err != nil {
		return Entry{}, nil, false, err
	}
	e, err := r.entryAt(c, name)
	if err != nil {
		return Entry{}, nil, false, err
	}
	if e.Mode == filemode.Dir {
		return Entry{}, nil, false, fmt.Errorf("%w: %s is a folder", ErrNotFound, name)
	}
	entry, p, err := r.describe(name, e.Hash)
	if err != nil {
		return Entry{}, nil, false, err
	}
	if p != nil {
		f, err := r.openObject(p, name)
		if err != nil {
			return Entry{}, nil, false, err
		}
		return entry, f, false, nil // an open file stays readable after r is closed
	}
	blob, err := object.GetBlob(r.st, e.Hash)
	if err != nil {
		return Entry{}, nil, false, err
	}
	br, err := blob.Reader()
	if err != nil {
		return Entry{}, nil, false, err
	}
	return entry, br, true, nil
}

// heldReader closes a versioned file's reader and then lets the repository go.
type heldReader struct {
	io.ReadCloser
	release func()
}

func (h *heldReader) Close() error {
	err := h.ReadCloser.Close()
	h.release()
	return err
}
