package knowledge

import (
	"errors"
	"io/fs"
	"os"

	"taskto.ai/taskto/internal/access"
)

// Storage usage.
//
// What a space's knowledge base takes on disk is two numbers: the bytes under .git (every
// version) and the bytes of the unversioned files (.git/lfs/objects). The quotas
// (internal/quota) ask for them on every write that adds content and for the usage view, so
// they are kept, never measured by walking git per request:
//
//   - the writer, which holds the space's lock, keeps both counted: .git's size as it
//     always has (spaceState.size, grown by the objects each commit stores), the
//     unversioned files' as spaceState.unv, dropped whenever bytes are adopted, removed or
//     moved and measured again, in the object directory alone, at the end of the write;
//   - at the end of each write (and after the repair a write begins with) it publishes the
//     two numbers as the space's usage, which readers take without any lock;
//   - a space nobody wrote since the server started has none published: the first reader
//     measures it from the file system, without the lock, and publishes that unless a
//     write got there first.

// usage is what a space's knowledge base takes on disk, in bytes.
type usage struct {
	versioned, unversioned int64
}

// usageCache is a space's published usage.
type usageCache struct {
	// gen counts the publishes and clears, so a measurement that began before one is dropped.
	gen uint64
	cur *usage
}

// published returns the space's usage, if there is one, and the generation a measurement
// started now would be stored under.
func (st *spaceState) published() (usage, uint64, bool) {
	st.cacheMu.Lock()
	defer st.cacheMu.Unlock()
	if st.cache.cur == nil {
		return usage{}, st.cache.gen, false
	}
	return *st.cache.cur, st.cache.gen, true
}

// publish sets the space's usage, whatever a measurement in flight found.
func (st *spaceState) publish(u *usage) {
	st.cacheMu.Lock()
	defer st.cacheMu.Unlock()
	st.cache.gen++
	st.cache.cur = u
}

// fill stores a measurement made from generation gen, unless something was published since.
func (st *spaceState) fill(gen uint64, u usage) {
	st.cacheMu.Lock()
	defer st.cacheMu.Unlock()
	if st.cache.gen == gen {
		st.cache.gen++
		st.cache.cur = &u
	}
}

// Usage is the bytes sp's knowledge base takes: what git keeps of it, every version
// included (the repository's size, the unversioned files' apart), and what its unversioned
// files take. A space without documents is zero. It takes no lock a writer holds: it is
// the counters the last change left, or one measurement of the files, once.
func (s *Store) Usage(sp access.Space) (versioned, unversioned int64, err error) {
	st := s.state(sp.ID)
	if u, _, ok := st.published(); ok {
		return u.versioned, u.unversioned, nil
	}
	// Taken before measuring: a write that publishes meanwhile makes this one stale.
	_, gen, _ := st.published()
	u, err := s.measure(sp)
	if err != nil {
		return 0, 0, err
	}
	st.fill(gen, u)
	return u.versioned, u.unversioned, nil
}

// measure walks sp's .git, which holds no lock: the files it counts may change under it,
// and it counts what it finds.
func (s *Store) measure(sp access.Space) (usage, error) {
	root, err := os.OpenRoot(sp.Dir(s.dataDir))
	if errors.Is(err, fs.ErrNotExist) {
		return usage{}, nil
	}
	if err != nil {
		return usage{}, err
	}
	defer root.Close()
	var u usage
	if u.versioned, err = gitSize(root.FS()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return usage{}, err
	}
	err = walkObjects(root.FS(), func(_ string, size int64) { u.unversioned += size })
	return u, err
}

// SetQuota sets the check every change that adds content makes first, inside the space's
// write lock: sp and the bytes the change adds. An error refuses the change and is its
// result, as it is. It is for the server's start, before the Store is used; internal/quota's
// Service.CheckDB is the check.
func (s *Store) SetQuota(check func(sp access.Space, adding int64) error) { s.quota = check }

// checkQuota is the quota's check for a change adding n bytes to sp.
func (s *Store) checkQuota(sp access.Space, n int64) error {
	if s.quota == nil || n <= 0 {
		return nil
	}
	return s.quota(sp, n)
}

// publishUsage records the usage of the space r is open on, with its write lock held.
func (s *Store) publishUsage(r *repo) {
	size, err := r.size()
	if err == nil && r.state.unv < 0 {
		var n int64
		if err = r.objects(func(_ string, size int64) { n += size }); err == nil {
			r.state.unv = n
		}
	}
	if err != nil {
		r.state.publish(nil) // measured again by the next reader
		return
	}
	r.state.publish(&usage{versioned: size, unversioned: r.state.unv})
}
