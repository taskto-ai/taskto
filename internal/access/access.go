package access

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
)

type Kind string

const (
	Private Kind = "private"
	Shared  Kind = "shared"
)

type Role string

const (
	RoleOwner  Role = "owner"
	RoleEditor Role = "editor"
	RoleViewer Role = "viewer"
)

func (r Role) validMember() bool { return r == RoleEditor || r == RoleViewer }

type Access string

const (
	None  Access = "none"
	Read  Access = "read"
	Write Access = "write"
)

func (a Access) Allows(need Access) bool { return a.rank() >= need.rank() }

func (a Access) rank() int {
	switch a {
	case Read:
		return 1
	case Write:
		return 2
	default:
		return 0
	}
}

type Space struct {
	ID               int64   `json:"id"`
	WorkspaceID      int64   `json:"workspace_id"`
	Kind             Kind    `json:"kind"`
	Name             string  `json:"name"`
	ArchivedAt       *string `json:"archived_at"`
	CreatedAt        string  `json:"created_at"`
	Generation       int64   `json:"-"`
	OfflineDownloads bool    `json:"offline_downloads"`
	Role             Role    `json:"role,omitempty"`
	Access           Access  `json:"access"`
}

func (s Space) access() Access {
	switch {
	case s.Role == RoleOwner:
		return Write
	case s.Role == RoleEditor && s.ArchivedAt == nil:
		return Write
	case s.Role == RoleEditor || s.Role == RoleViewer:
		return Read
	default:
		return None
	}
}

var ErrNotFound = errors.New("space not found")

var ErrDisabled = fmt.Errorf("%w: the account is disabled", ErrNotFound)

func (c *Checker) Disabled(ctx context.Context, userID int64) (bool, error) {
	return disabledIn(ctx, c.db, userID)
}

func disabledIn(ctx context.Context, q Querier, userID int64) (bool, error) {
	var off bool
	err := q.QueryRowContext(ctx, `SELECT disabled_at IS NOT NULL FROM users WHERE id = ?`, userID).Scan(&off)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return off, err
}

type Checker struct {
	db           *sql.DB
	reads        reader
	auth         *auth.Service
	archiveHooks []ArchiveHook
	memberHooks  []MemberHook
	grantHooks   []AppHook
	removedHooks []func(spaceID, userID int64)
}

type ArchiveHook func(ctx context.Context, tx *sql.Tx, spaceID int64) error

func (c *Checker) OnArchiveChange(h ArchiveHook) { c.archiveHooks = append(c.archiveHooks, h) }

type MemberHook func(ctx context.Context, tx *sql.Tx, spaceID, userID int64) error

func (c *Checker) OnMemberAdded(h MemberHook) { c.memberHooks = append(c.memberHooks, h) }

type AppHook func(ctx context.Context, tx *sql.Tx, spaceID, appID int64) error

func (c *Checker) OnAppGrantsChanged(h AppHook) { c.grantHooks = append(c.grantHooks, h) }

func (c *Checker) OnMemberRemoved(h func(spaceID, userID int64)) {
	c.removedHooks = append(c.removedHooks, h)
}

func New(sqlDB *sql.DB, a *auth.Service) *Checker {
	return &Checker{db: sqlDB, reads: sqlDB, auth: a}
}

const spaceColumns = `s.id, s.workspace_id, s.kind, s.name, s.archived_at, s.created_at, s.offline_downloads, s.generation,
	CASE WHEN s.kind = 'private' AND s.owner_id = m.user_id THEN 'owner' ELSE coalesce(sm.role, '') END`

const fromSpaceFor = `
	FROM spaces s
	JOIN memberships m ON m.workspace_id = s.workspace_id AND m.user_id = ?
	JOIN users du ON du.id = m.user_id AND du.disabled_at IS NULL
	LEFT JOIN space_members sm ON sm.space_id = s.id AND sm.user_id = m.user_id`

type scanner interface{ Scan(dest ...any) error }

func scanSpace(row scanner) (Space, error) {
	var s Space
	if err := row.Scan(&s.ID, &s.WorkspaceID, &s.Kind, &s.Name, &s.ArchivedAt, &s.CreatedAt, &s.OfflineDownloads, &s.Generation, &s.Role); err != nil {
		return Space{}, err
	}
	s.Access = s.access()
	return s, nil
}

func (c *Checker) lookup(ctx context.Context, userID, spaceID int64) (Space, error) {
	return lookupIn(ctx, c.reads, userID, spaceID)
}

type reader interface {
	Querier
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func lookupIn(ctx context.Context, q Querier, userID, spaceID int64) (Space, error) {
	s, err := scanSpace(q.QueryRowContext(ctx,
		`SELECT `+spaceColumns+fromSpaceFor+` WHERE s.id = ?`, userID, spaceID))
	if errors.Is(err, sql.ErrNoRows) {
		off, derr := disabledIn(ctx, q, userID)
		if derr != nil {
			return Space{}, derr
		}
		if off {
			return Space{}, ErrDisabled
		}
		s, err = scanSpace(q.QueryRowContext(ctx,
			`SELECT `+spaceColumns+fromSpaceFor+` WHERE s.id = ?`, userID, spaceID))
		if errors.Is(err, sql.ErrNoRows) {
			return Space{}, ErrNotFound
		}
	}
	return s, err
}

func (c *Checker) On(tx *sql.Tx) *Checker {
	on := *c
	on.reads = tx
	return &on
}

func (c *Checker) Check(ctx context.Context, userID, spaceID int64) (Space, error) {
	return checkIn(ctx, c.reads, userID, spaceID)
}

func checkIn(ctx context.Context, q Querier, userID, spaceID int64) (Space, error) {
	s, err := lookupIn(ctx, q, userID, spaceID)
	if err != nil {
		return Space{}, err
	}
	if !s.Access.Allows(Read) {
		return Space{}, ErrNotFound
	}
	return s, nil
}

func (c *Checker) CheckTx(ctx context.Context, tx *sql.Tx, userID, spaceID int64) (Space, error) {
	return checkIn(ctx, tx, userID, spaceID)
}

func (c *Checker) CanRead(ctx context.Context, userID, spaceID int64) (bool, error) {
	return c.can(ctx, userID, spaceID, Read)
}

func CanReadIn(ctx context.Context, q Querier, userID, spaceID int64) (bool, error) {
	s, err := lookupIn(ctx, q, userID, spaceID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return s.Access.Allows(Read), nil
}

func (c *Checker) CheckWith(ctx context.Context, userID, spaceID int64, columns, joins string, args []any, dest ...any) (Space, error) {
	var s Space
	all := append([]any{&s.ID, &s.WorkspaceID, &s.Kind, &s.Name, &s.ArchivedAt, &s.CreatedAt, &s.OfflineDownloads, &s.Generation, &s.Role}, dest...)
	query := `SELECT ` + spaceColumns + `, ` + columns + fromSpaceFor + ` ` + joins + ` WHERE s.id = ?`
	params := append(append([]any{userID}, args...), spaceID)
	err := c.reads.QueryRowContext(ctx, query, params...).Scan(all...)
	if errors.Is(err, sql.ErrNoRows) {
		return Space{}, ErrNotFound
	}
	if err != nil {
		return Space{}, err
	}
	s.Access = s.access()
	if !s.Access.Allows(Read) {
		return Space{}, ErrNotFound
	}
	return s, nil
}

func (c *Checker) CanWrite(ctx context.Context, userID, spaceID int64) (bool, error) {
	return c.can(ctx, userID, spaceID, Write)
}

func (c *Checker) can(ctx context.Context, userID, spaceID int64, need Access) (bool, error) {
	s, err := c.lookup(ctx, userID, spaceID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return s.Access.Allows(need), nil
}

func (c *Checker) Readable(ctx context.Context, userID, workspaceID int64) ([]Space, error) {
	return c.list(ctx, userID, workspaceID, false)
}

func (c *Checker) Manageable(ctx context.Context, userID, workspaceID int64) ([]Space, error) {
	return c.list(ctx, userID, workspaceID, true)
}

func (c *Checker) list(ctx context.Context, userID, workspaceID int64, everyShared bool) ([]Space, error) {
	rows, err := c.reads.QueryContext(ctx, `SELECT `+spaceColumns+fromSpaceFor+`
		WHERE s.workspace_id = ?
		  AND ((s.kind = 'private' AND s.owner_id = m.user_id) OR sm.role IS NOT NULL OR (? AND s.kind = 'shared'))
		ORDER BY s.kind = 'shared', s.name, s.id`, userID, workspaceID, everyShared)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := []Space{}
	for rows.Next() {
		s, err := scanSpace(rows)
		if err != nil {
			return nil, err
		}
		all = append(all, s)
	}
	return all, rows.Err()
}

type spaceKey struct{}

func SpaceFrom(ctx context.Context) (Space, bool) {
	s, ok := ctx.Value(spaceKey{}).(Space)
	return s, ok
}

func (c *Checker) RequireSpace(need Access, next http.Handler) http.Handler {
	return c.auth.RequireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, ok := c.spaceInPath(w, r)
		if !ok {
			return
		}
		if !s.Access.Allows(Read) {
			writeError(w, http.StatusNotFound, apierr.CodeSpaceNotFound, ErrNotFound.Error())
			return
		}
		if !s.Access.Allows(need) {
			code, msg := apierr.CodeSpaceReadOnly, "you can only read this space"
			if s.ArchivedAt != nil {
				code, msg = apierr.CodeSpaceArchived, "this space is archived: it is read-only"
			}
			writeError(w, http.StatusForbidden, code, msg)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), spaceKey{}, s)))
	}))
}

const ExpectedUserHeader = "X-Taskto-Expected-User"

func (c *Checker) ExpectUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := r.Header.Get(ExpectedUserHeader)
		if want == "" {
			next.ServeHTTP(w, r)
			return
		}
		c.auth.RequireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, err := strconv.ParseInt(want, 10, 64)
			if err != nil || id <= 0 {
				writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, ExpectedUserHeader+" is an account's id")
				return
			}
			if u, _ := auth.UserFrom(r.Context()); u.ID != id {
				writeError(w, http.StatusConflict, apierr.CodeAccountChanged, "this request is for another account than the one signed in")
				return
			}
			next.ServeHTTP(w, r)
		})).ServeHTTP(w, r)
	})
}

func (c *Checker) RequireSpaceAdmin(next http.Handler) http.Handler {
	return c.RequireSpace(Write, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m, _ := auth.MembershipFrom(r.Context()); m.Role != auth.RoleAdmin {
			writeError(w, http.StatusForbidden, apierr.CodeAdminOnly, "only a workspace admin can do this")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func (c *Checker) spaceInPath(w http.ResponseWriter, r *http.Request) (Space, bool) {
	u, _ := auth.UserFrom(r.Context())
	m, _ := auth.MembershipFrom(r.Context())
	id, err := strconv.ParseInt(r.PathValue("space"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, apierr.CodeSpaceNotFound, ErrNotFound.Error())
		return Space{}, false
	}
	s, err := c.lookup(r.Context(), u.ID, id)
	if errors.Is(err, ErrNotFound) || (err == nil && s.WorkspaceID != m.WorkspaceID) {
		writeError(w, http.StatusNotFound, apierr.CodeSpaceNotFound, ErrNotFound.Error())
		return Space{}, false
	}
	if err != nil {
		serverError(w, r, err)
		return Space{}, false
	}
	return s, true
}

func serverError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	slog.Error("access: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	apierr.Internal(w)
}
