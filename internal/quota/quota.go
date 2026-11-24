// Package quota is the storage quotas: the most a workspace may store, and the most a
// member's private space may, and the usage view that shows what each space takes.
//
// The operator sets two defaults (internal/config: the workspace's and the private space's),
// each in bytes, zero meaning unlimited, which is what an install has until the operator
// says otherwise. The workspace's quota counts everything its spaces store, shared and
// private; the private space's counts that space alone, so a member's private space in each
// workspace they belong to has its own allowance.
//
// What counts, per space: the knowledge base's bytes (the repository's history and the
// unversioned files, internal/knowledge) and the apps' photo attachments, with their
// thumbnails (internal/apps). Rows of apps, boards and conversations live in the database,
// which no per-space figure covers, and aren't counted.
//
// The knowledge base's figures come from counters it keeps up to date as it commits, never
// from walking git on a request; the attachments' from one indexed query. A write that would
// take a space or its workspace over a quota is refused with an *Error, which the HTTP
// routes answer with 507 and a code the web app translates; a change that adds nothing
// (deleting, moving) is never refused, so a full space can always make room.
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

// Limits are the quotas, in bytes. Zero is unlimited.
type Limits struct {
	// Workspace caps the bytes all of a workspace's spaces store together.
	Workspace int64 `json:"workspace"`
	// User caps the bytes of each member's private space.
	User int64 `json:"user"`
}

// Files tells what a space's knowledge base takes on disk: the bytes git keeps (every
// version) and the bytes of its unversioned files. internal/knowledge's Store is one.
type Files interface {
	Usage(sp access.Space) (versioned, unversioned int64, err error)
}

// Scope is which quota was reached.
type Scope string

// The quotas.
const (
	ScopeWorkspace Scope = "workspace"
	ScopeUser      Scope = "user"
)

// ErrExceeded is what every *Error is.
var ErrExceeded = errors.New("storage quota exceeded")

// Error is a write refused for taking a space or its workspace over a quota.
type Error struct {
	Scope Scope
	// Limit is the quota, and Used what was stored when the write was refused, in bytes.
	Limit, Used int64
}

func (e *Error) Error() string {
	what := "the workspace's"
	if e.Scope == ScopeUser {
		what = "this private space's"
	}
	return fmt.Sprintf("%s: %s quota is %d bytes", ErrExceeded, what, e.Limit)
}

// Is makes errors.Is(err, ErrExceeded) true.
func (e *Error) Is(target error) bool { return target == ErrExceeded }

// API is the answer to give for the error: 507, with the code of its scope.
func (e *Error) API() *apierr.Error {
	code := apierr.CodeQuotaWorkspace
	if e.Scope == ScopeUser {
		code = apierr.CodeQuotaUser
	}
	return apierr.New(http.StatusInsufficientStorage, code, e.Error(), apierr.Params{"max_mib": (e.Limit + 1<<20 - 1) >> 20})
}

// ErrUnavailable is what every *UnavailableError is.
var ErrUnavailable = errors.New("storage usage can't be measured right now")

// UnavailableError is a write refused because the usage it would be checked against couldn't
// be measured: trying again may work.
type UnavailableError struct{ Err error }

func (e *UnavailableError) Error() string { return fmt.Sprintf("%s: %v", ErrUnavailable, e.Err) }

// Is makes errors.Is(err, ErrUnavailable) true.
func (e *UnavailableError) Is(target error) bool { return target == ErrUnavailable }

// Unwrap returns the failure.
func (e *UnavailableError) Unwrap() error { return e.Err }

// API is the answer to give for the error: 503, which the web app translates.
func (e *UnavailableError) API() *apierr.Error {
	return apierr.New(http.StatusServiceUnavailable, apierr.CodeQuotaUnavailable, ErrUnavailable.Error())
}

// AsAPI is the answer to give for err if it is a refusal of this package's, either kind.
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

// Queryer is a database or a transaction.
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Service checks writes against the quotas and reports usage.
type Service struct {
	db     *sql.DB
	limits Limits
	files  Files

	mu   sync.Mutex // guards held, never held across a measurement or a query
	held map[int64]*held
}

// New returns the quotas over db, which must be migrated. files measures the knowledge
// base; it may be nil, for a server with none (its bytes are then counted as nothing).
func New(db *sql.DB, limits Limits, files Files) *Service {
	return &Service{db: db, limits: limits, files: files, held: map[int64]*held{}}
}

// Limits returns the quotas.
func (s *Service) Limits() Limits { return s.limits }

// spaceRow is a space of a workspace, as the usage queries read them.
type spaceRow struct {
	id    int64
	kind  access.Kind
	name  string
	owner *Owner // for a private space
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

// attachments is the bytes of each space's photo attachments (the uploads on their way, at
// the size they declared, count: they hold their place), by space ID.
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

// knowledge measures space id's knowledge base.
func (s *Service) knowledge(workspaceID, id int64) (versioned, unversioned int64, err error) {
	if s.files == nil {
		return 0, 0, nil
	}
	return s.files.Usage(access.Space{ID: id, WorkspaceID: workspaceID})
}

// maxAttempts bounds how often a reservation measures again because another write finished
// meanwhile; past it the write is refused as unavailable, to be tried again.
const maxAttempts = 16

// Reservation is the bytes a write holds while it runs: they count against the quotas, with
// every other write's, from the moment it is granted until Release, which a write calls once
// its real usage is published (or it failed). The zero of it, nil, holds nothing.
type Reservation struct {
	s     *Service
	ws    int64
	space int64
	n     int64
	once  sync.Once
}

// Release gives the bytes back. It is safe to call twice, and on nil.
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
		st.epoch++ // what a check measured before this may not count what this write stored
	})
}

// held is what a workspace's writes have reserved.
type held struct {
	total  int64
	spaces map[int64]int64
	// epoch counts the releases: a check whose measurement began before one measures again.
	epoch uint64
}

// Reserve refuses with an *Error a write of adding bytes to space sp that would take it, or
// its workspace, over a quota, counting what the writes that are running have reserved and
// not yet stored; otherwise it reserves the bytes, atomically with the check, and the caller
// releases them when the write's usage is published or the write failed. q is where the
// attachments are read: the caller's transaction if it has one, so the check sees what the
// transaction has reserved. Zero or fewer bytes are never refused, and reserve nothing.
//
// The reservations are kept in memory under a mutex held only to read and change them: never
// while a space is measured or a database is read, so no lock order is involved. A write that
// finishes while a check measures makes the check measure again.
//
// If a space can't be measured the write is refused with ErrUnavailable, never allowed on a
// guess.
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

// state is the reservations of workspace id, with s.mu held.
func (s *Service) state(id int64) *held {
	st := s.held[id]
	if st == nil {
		st = &held{spaces: map[int64]int64{}}
		s.held[id] = st
	}
	return st
}

// measureWorkspace is what the workspace's spaces store now, and sp's own share of it.
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
			// A space that can't be measured is not zero: the write waits, and is tried again.
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

// Check is Reserve that holds nothing: it refuses what couldn't be stored now.
func (s *Service) Check(ctx context.Context, q Queryer, sp access.Space, adding int64) error {
	r, err := s.Reserve(ctx, q, sp, adding)
	r.Release()
	return err
}

// CheckDB is Check reading the attachments from the database.
func (s *Service) CheckDB(ctx context.Context, sp access.Space, adding int64) error {
	return s.Check(ctx, s.db, sp, adding)
}

// ReserveDB is Reserve reading the attachments from the database.
func (s *Service) ReserveDB(ctx context.Context, sp access.Space, adding int64) (*Reservation, error) {
	return s.Reserve(ctx, s.db, sp, adding)
}

// KnowledgeCheck is the check for knowledge.Store.SetQuota: it reserves what a change adds,
// reading the attachments from the database, and answers what releases the reservation.
func (s *Service) KnowledgeCheck() func(sp access.Space, adding int64) (release func(), err error) {
	return func(sp access.Space, adding int64) (func(), error) {
		r, err := s.ReserveDB(context.Background(), sp, adding)
		if err != nil {
			return nil, err
		}
		return r.Release, nil
	}
}

// Owner is the member whose private space it is.
type Owner struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// SpaceUsage is what one space stores, in bytes.
type SpaceUsage struct {
	ID    int64       `json:"id"`
	Kind  access.Kind `json:"kind"`
	Name  string      `json:"name"`
	Owner *Owner      `json:"owner,omitempty"`
	// KnowledgeVersioned is what git keeps of the knowledge base, every version included;
	// KnowledgeUnversioned the bytes of its unversioned files.
	KnowledgeVersioned   int64 `json:"knowledge_versioned"`
	KnowledgeUnversioned int64 `json:"knowledge_unversioned"`
	// Attachments is the apps' photos, thumbnails included.
	Attachments int64 `json:"attachments"`
	Total       int64 `json:"total"`
	// Limit is the quota of the space itself: the private space's for a private space, zero
	// (none) for a shared one, which only the workspace's quota bounds.
	Limit int64 `json:"limit"`
}

// Report is a workspace's usage.
type Report struct {
	Quota Limits `json:"quota"`
	// Total is what all the workspace's spaces store.
	Total  int64        `json:"total"`
	Spaces []SpaceUsage `json:"spaces"`
}

// Usage reports what every space of workspaceID stores: the shared ones, then each member's
// private one.
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

// Mine reports what the private space of userID in workspaceID stores; false if they have
// none there.
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
