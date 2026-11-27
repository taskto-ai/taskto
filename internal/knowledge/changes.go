package knowledge

import (
	"context"
	"errors"
	"path"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"taskto.ai/taskto/internal/access"
)

type Change struct {
	Path    string
	Version string
}

type Diff struct {
	Head    string
	Changes []Change
	Full    bool
}

func (s *Store) OnChange(fn func(access.Space)) { s.onChange = append(s.onChange, fn) }

func (s *Store) changed(sp access.Space) {
	for _, fn := range s.onChange {
		fn(sp)
	}
}

func (s *Store) Head(_ context.Context, sp access.Space) (string, error) {
	var head string
	err := s.read(sp, func(r *repo) error {
		c, err := r.head()
		if c != nil {
			head = c.Hash.String()
		}
		return err
	})
	if errors.Is(err, errNoRepo) {
		err = nil
	}
	return head, err
}

func (s *Store) Changes(ctx context.Context, sp access.Space, since string) (Diff, error) {
	var d Diff
	err := s.read(sp, func(r *repo) error {
		head, err := r.head()
		if err != nil || head == nil {
			d.Full = true
			return err
		}
		d.Head = head.Hash.String()
		from := plumbing.ZeroHash
		if isHash(since) {
			c, err := object.GetCommit(r.st, plumbing.NewHash(since))
			switch {
			case err == nil:
				from = c.TreeHash
			case !errors.Is(err, plumbing.ErrObjectNotFound):
				return err
			}
		}
		d.Full = from.IsZero()
		if err := r.diff(ctx, from, head.TreeHash, "", &d.Changes); err != nil {
			return err
		}
		slices.SortFunc(d.Changes, func(a, b Change) int { return strings.Compare(a.Path, b.Path) })
		return nil
	})
	if errors.Is(err, errNoRepo) {
		d, err = Diff{Full: true}, nil
	}
	return d, err
}

func (r *repo) diff(ctx context.Context, a, b plumbing.Hash, dir string, out *[]Change) error {
	if a == b {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ea, err := r.entries(a)
	if err != nil {
		return err
	}
	eb, err := r.entries(b)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(ea)+len(eb))
	for name := range ea {
		names = append(names, name)
	}
	for name := range eb {
		if _, ok := ea[name]; !ok {
			names = append(names, name)
		}
	}
	for _, name := range names {
		x, inA := ea[name]
		y, inB := eb[name]
		if inA && inB && x.Hash == y.Hash && x.Mode == y.Mode {
			continue
		}
		full := path.Join(dir, name)
		var subA, subB plumbing.Hash
		if inA && x.Mode == filemode.Dir {
			subA = x.Hash
		}
		if inB && y.Mode == filemode.Dir {
			subB = y.Hash
		}
		if !subA.IsZero() || !subB.IsZero() {
			if err := r.diff(ctx, subA, subB, full, out); err != nil {
				return err
			}
		}
		fileA, fileB := inA && x.Mode != filemode.Dir, inB && y.Mode != filemode.Dir
		switch {
		case fileB && (!fileA || x.Hash != y.Hash):
			*out = append(*out, Change{Path: full, Version: y.Hash.String()})
		case fileA && !fileB:
			*out = append(*out, Change{Path: full})
		}
	}
	return nil
}

func (r *repo) entries(h plumbing.Hash) (map[string]object.TreeEntry, error) {
	if h.IsZero() {
		return nil, nil
	}
	tree, err := object.GetTree(r.st, h)
	if err != nil {
		return nil, err
	}
	m := make(map[string]object.TreeEntry, len(tree.Entries))
	for _, e := range tree.Entries {
		m[e.Name] = e
	}
	return m, nil
}
