package knowledge

import (
	"errors"
	"io/fs"
	"os"

	"taskto.ai/taskto/internal/access"
)

type usage struct {
	versioned, unversioned int64
}

type usageCache struct {
	gen uint64
	cur *usage
}

func (st *spaceState) published() (usage, uint64, bool) {
	st.cacheMu.Lock()
	defer st.cacheMu.Unlock()
	if st.cache.cur == nil {
		return usage{}, st.cache.gen, false
	}
	return *st.cache.cur, st.cache.gen, true
}

func (st *spaceState) publish(u *usage) {
	st.cacheMu.Lock()
	defer st.cacheMu.Unlock()
	st.cache.gen++
	st.cache.cur = u
}

func (st *spaceState) fill(gen uint64, u usage) usage {
	st.cacheMu.Lock()
	defer st.cacheMu.Unlock()
	if st.cache.gen == gen {
		st.cache.gen++
		st.cache.cur = &u
		return u
	}
	if st.cache.cur != nil {
		return *st.cache.cur
	}
	return u
}

func (s *Store) Usage(sp access.Space) (versioned, unversioned int64, err error) {
	st := s.state(sp.ID)
	u, gen, ok := st.published()
	if ok {
		return u.versioned, u.unversioned, nil
	}
	u, err = s.measure(sp)
	if err != nil {
		return 0, 0, err
	}
	if s.afterMeasure != nil {
		s.afterMeasure(sp)
	}
	u = st.fill(gen, u)
	return u.versioned, u.unversioned, nil
}

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

func (s *Store) SetQuota(check func(sp access.Space, adding int64) (release func(), err error)) {
	s.quota = check
}

func (s *Store) reserveQuota(r *repo, sp access.Space, n int64) error {
	if s.quota == nil || n <= 0 {
		return nil
	}
	s.publishUsage(r)
	release, err := s.quota(sp, n)
	if err != nil {
		return err
	}
	if release != nil {
		r.releases = append(r.releases, release)
	}
	return nil
}

func (s *Store) publishUsage(r *repo) {
	size, err := r.size()
	if err == nil && r.state.unv < 0 {
		var n int64
		if err = r.objects(func(_ string, size int64) { n += size }); err == nil {
			r.state.unv = n
		}
	}
	if err != nil {
		r.state.publish(nil)
		return
	}
	r.state.publish(&usage{versioned: size, unversioned: r.state.unv})
}
