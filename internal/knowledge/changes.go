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

// What indexes kept beside the repositories (the search index) need: to hear of every
// change, and to find what differs between the commit they reflect and main.

// Change is a file that differs between two commits: its path, and its version at the
// later one, or "" if it is gone.
type Change struct {
	Path    string
	Version string
}

// Diff is how main's head differs from an earlier commit.
type Diff struct {
	// Head is main's last commit, or "" without one.
	Head string
	// Changes are the files that differ, by path.
	Changes []Change
	// Full is set when the earlier commit was empty or unknown: Changes then lists every
	// file at Head, and nothing that is gone.
	Full bool
}

// OnChange has fn called after every change to a space's repository, whatever its outcome
// (a change can fail after main has moved), from the goroutine that made it, once the
// space is unlocked: fn must return quickly. Call it before the Store is used.
func (s *Store) OnChange(fn func(access.Space)) { s.onChange = append(s.onChange, fn) }

func (s *Store) changed(sp access.Space) {
	for _, fn := range s.onChange {
		fn(sp)
	}
}

// Head returns main's last commit in sp, or "" before the first.
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

// Changes returns how main's head in sp differs from the commit since: every file if
// since is empty, or a commit the repository doesn't have (Full). Paths are as git has
// them: a change made outside the server can hold names the Store would refuse.
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

// diff appends the files that differ between the trees a and b (zero for none), whose
// path is dir. Folders that are the same in both are skipped whole.
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

// entries returns the tree h's entries by name; none for the zero hash.
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
