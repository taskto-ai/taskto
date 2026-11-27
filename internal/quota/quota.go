package quota

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
)

type Limits struct {
	Workspace int64 `json:"workspace"`
	User      int64 `json:"user"`
}

type Files interface {
	Usage(sp access.Space) (versioned, unversioned int64, err error)
}

type Scope string

const (
	ScopeWorkspace Scope = "workspace"
	ScopeUser      Scope = "user"
)

var ErrExceeded = errors.New("storage quota exceeded")

type Error struct {
	Scope       Scope
	Limit, Used int64
}

func (e *Error) Error() string {
	what := "the workspace's"
	if e.Scope == ScopeUser {
		what = "this private space's"
	}
	return fmt.Sprintf("%s: %s quota is %d bytes", ErrExceeded, what, e.Limit)
}

func (e *Error) Is(target error) bool { return target == ErrExceeded }

func (e *Error) API() *apierr.Error {
	code := apierr.CodeQuotaWorkspace
	if e.Scope == ScopeUser {
		code = apierr.CodeQuotaUser
	}
	return apierr.New(http.StatusInsufficientStorage, code, e.Error(), apierr.Params{"max_mib": (e.Limit + 1<<20 - 1) >> 20})
}

var ErrUnavailable = errors.New("storage usage can't be measured right now")

type UnavailableError struct{ Err error }

func (e *UnavailableError) Error() string { return fmt.Sprintf("%s: %v", ErrUnavailable, e.Err) }

func (e *UnavailableError) Is(target error) bool { return target == ErrUnavailable }

func (e *UnavailableError) Unwrap() error { return e.Err }

func (e *UnavailableError) API() *apierr.Error {
	return apierr.New(http.StatusServiceUnavailable, apierr.CodeQuotaUnavailable, ErrUnavailable.Error())
}

func AsAPI(err error) (*apierr.Error, bool) {
	var over *Error
	if errors.As(err, &over) {
		return over.API(), true
	}
	var un *UnavailableError
	if errors.As(err, &un) {
		return un.API(), true
	}
	return nil, false
}

type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

type Service struct {
	db     *sql.DB
	limits Limits
	files  Files

	mu   sync.Mutex
	held map[int64]*held
}

func New(db *sql.DB, limits Limits, files Files) *Service {
	return &Service{db: db, limits: limits, files: files, held: map[int64]*held{}}
}

func (s *Service) Limits() Limits { return s.limits }

type spaceRow struct {
	id    int64
	kind  access.Kind
	name  string
	owner *Owner
}

func (s *Service) spaces(ctx context.Context, q Queryer, workspaceID int64) ([]spaceRow, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT s.id, s.kind, s.name, u.id, u.name, u.email
		FROM spaces s LEFT JOIN users u ON u.id = s.owner_id
		WHERE s.workspace_id = ?
		ORDER BY s.kind DESC, s.name COLLATE NOCASE, s.id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []spaceRow
	for rows.Next() {
		var r spaceRow
		var uid sql.NullInt64
		var uname, uemail sql.NullString
		if err := rows.Scan(&r.id, &r.kind, &r.name, &uid, &uname, &uemail); err != nil {
			return nil, err
		}
		if uid.Valid {
			r.owner = &Owner{ID: uid.Int64, Name: uname.String, Email: uemail.String}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func attachments(ctx context.Context, q Queryer, workspaceID int64) (map[int64]int64, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT a.space_id, sum(a.size + coalesce(a.thumb_size, 0))
		FROM app_attachments a JOIN spaces s ON s.id = a.space_id
		WHERE s.workspace_id = ?
		GROUP BY a.space_id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var id, n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

func (s *Service) knowledge(workspaceID, id int64) (versioned, unversioned int64, err error) {
	if s.files == nil {
		return 0, 0, nil
	}
	return s.files.Usage(access.Space{ID: id, WorkspaceID: workspaceID})
}

const maxAttempts = 16

type Reservation struct {
	s     *Service
	ws    int64
	space int64
	n     int64
	once  sync.Once
}

func (r *Reservation) Release() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		st := r.s.held[r.ws]
		st.total -= r.n
		st.spaces[r.space] -= r.n
		if st.spaces[r.space] <= 0 {
			delete(st.spaces, r.space)
		}
		st.epoch++
	})
}

type held struct {
	total  int64
	spaces map[int64]int64
	epoch  uint64
}

func (s *Service) Reserve(ctx context.Context, q Queryer, sp access.Space, adding int64) (*Reservation, error) {
	if adding <= 0 || (s.limits.Workspace <= 0 && s.limits.User <= 0) {
		return nil, nil
	}
	for range maxAttempts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s.mu.Lock()
		epoch := s.state(sp.WorkspaceID).epoch
		s.mu.Unlock()
		total, own, private, err := s.measureWorkspace(ctx, q, sp)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		st := s.state(sp.WorkspaceID)
		if st.epoch != epoch {
			s.mu.Unlock()
			continue
		}
		ownHeld := st.spaces[sp.ID]
		var refused error
		switch {
		case private && s.limits.User > 0 && own+ownHeld+adding > s.limits.User:
			refused = &Error{Scope: ScopeUser, Limit: s.limits.User, Used: own + ownHeld}
		case s.limits.Workspace > 0 && total+st.total+adding > s.limits.Workspace:
			refused = &Error{Scope: ScopeWorkspace, Limit: s.limits.Workspace, Used: total + st.total}
		}
		if refused == nil {
			st.total += adding
			st.spaces[sp.ID] += adding
		}
		s.mu.Unlock()
		if refused != nil {
			return nil, refused
		}
		return &Reservation{s: s, ws: sp.WorkspaceID, space: sp.ID, n: adding}, nil
	}
	return nil, &UnavailableError{Err: errors.New("writes kept finishing while the usage was measured")}
}

func (s *Service) state(id int64) *held {
	st := s.held[id]
	if st == nil {
		st = &held{spaces: map[int64]int64{}}
		s.held[id] = st
	}
	return st
}

func (s *Service) measureWorkspace(ctx context.Context, q Queryer, sp access.Space) (total, own int64, private bool, err error) {
	spaces, err := s.spaces(ctx, q, sp.WorkspaceID)
	if err != nil {
		return 0, 0, false, err
	}
	photos, err := attachments(ctx, q, sp.WorkspaceID)
	if err != nil {
		return 0, 0, false, err
	}
	for _, r := range spaces {
		v, u, err := s.knowledge(sp.WorkspaceID, r.id)
		if err != nil {
			slog.Warn("quota: measuring a space", "space", r.id, "err", err)
			return 0, 0, false, &UnavailableError{Err: err}
		}
		n := v + u + photos[r.id]
		total += n
		if r.id == sp.ID {
			own, private = n, r.kind == access.Private
		}
	}
	return total, own, private, nil
}

func (s *Service) Check(ctx context.Context, q Queryer, sp access.Space, adding int64) error {
	r, err := s.Reserve(ctx, q, sp, adding)
	r.Release()
	return err
}

func (s *Service) CheckDB(ctx context.Context, sp access.Space, adding int64) error {
	return s.Check(ctx, s.db, sp, adding)
}

func (s *Service) ReserveDB(ctx context.Context, sp access.Space, adding int64) (*Reservation, error) {
	return s.Reserve(ctx, s.db, sp, adding)
}

func (s *Service) KnowledgeCheck() func(sp access.Space, adding int64) (release func(), err error) {
	return func(sp access.Space, adding int64) (func(), error) {
		r, err := s.ReserveDB(context.Background(), sp, adding)
		if err != nil {
			return nil, err
		}
		return r.Release, nil
	}
}

type Owner struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type SpaceUsage struct {
	ID                   int64       `json:"id"`
	Kind                 access.Kind `json:"kind"`
	Name                 string      `json:"name"`
	Owner                *Owner      `json:"owner,omitempty"`
	KnowledgeVersioned   int64       `json:"knowledge_versioned"`
	KnowledgeUnversioned int64       `json:"knowledge_unversioned"`
	Attachments          int64       `json:"attachments"`
	Total                int64       `json:"total"`
	Limit                int64       `json:"limit"`
}

type Report struct {
	Quota  Limits       `json:"quota"`
	Total  int64        `json:"total"`
	Spaces []SpaceUsage `json:"spaces"`
}

func (s *Service) Usage(ctx context.Context, workspaceID int64) (Report, error) {
	spaces, err := s.spaces(ctx, s.db, workspaceID)
	if err != nil {
		return Report{}, err
	}
	photos, err := attachments(ctx, s.db, workspaceID)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Quota: s.limits, Spaces: []SpaceUsage{}}
	for _, r := range spaces {
		u, err := s.spaceUsage(workspaceID, r, photos[r.id])
		if err != nil {
			return Report{}, err
		}
		rep.Total += u.Total
		rep.Spaces = append(rep.Spaces, u)
	}
	return rep, nil
}

func (s *Service) spaceUsage(workspaceID int64, r spaceRow, photos int64) (SpaceUsage, error) {
	v, un, err := s.knowledge(workspaceID, r.id)
	if err != nil {
		return SpaceUsage{}, err
	}
	u := SpaceUsage{ID: r.id, Kind: r.kind, Name: r.name, KnowledgeVersioned: v, KnowledgeUnversioned: un,
		Attachments: photos, Total: v + un + photos}
	if r.owner != nil {
		o := *r.owner
		u.Owner = &o
		u.Limit = s.limits.User
	}
	return u, nil
}

func (s *Service) Mine(ctx context.Context, workspaceID, userID int64) (SpaceUsage, bool, error) {
	spaces, err := s.spaces(ctx, s.db, workspaceID)
	if err != nil {
		return SpaceUsage{}, false, err
	}
	photos, err := attachments(ctx, s.db, workspaceID)
	if err != nil {
		return SpaceUsage{}, false, err
	}
	for _, r := range spaces {
		if r.owner != nil && r.owner.ID == userID {
			u, err := s.spaceUsage(workspaceID, r, photos[r.id])
			if err != nil {
				return SpaceUsage{}, false, err
			}
			return u, true, nil
		}
	}
	return SpaceUsage{}, false, nil
}
