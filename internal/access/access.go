// Package access decides who may see and change what: it is the one access layer that
// files, conversations, boards, apps and the agent call, rather than each writing its own
// checks.
//
// A workspace contains spaces, and everything people store lives in one space:
//
//   - every member has a private space, which only they can open. It is created when they
//     join the workspace (see migration 0003) and kept when they leave, closed to
//     everyone, until they rejoin;
//   - shared spaces have a member list, each member an editor or a viewer. Workspace
//     admins create, rename and archive them and manage their members.
//
// The rules (Access answers them):
//
//   - only members of a space's workspace have any access to it; leaving the workspace
//     ends it at once;
//   - a private space: its owner reads and writes, nobody else does, admins included;
//   - a shared space: editors read and write, viewers read; once archived, nobody writes;
//   - workspace admins don't read shared spaces they aren't members of. They see those
//     spaces' names and member lists, to manage them, and can add themselves, which every
//     member then sees in the list.
//
// Apps add a finer layer inside a space (apps.go): roles in one app (builder, publisher,
// user, data manager, reader) that only narrow what the caller's access to the space
// allows, never widen it, and say whether they read all of the app's records or only their
// own. App returns what a caller may do in an app, for requests and for the agent alike.
//
// A space that the caller can't open answers 404, never 403, whether it exists or not,
// so IDs don't reveal what other people have; 403 is only for a write to a space the
// caller may read. Routes on a space's content go under
// /api/workspaces/{workspace}/spaces/{space}/ and wrap their handler in RequireSpace;
// code without a request (the agent, background jobs) calls Check, CanRead or CanWrite
// with the user it acts for.
package access

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
)

// Kind is a space's kind.
type Kind string

// The kinds of space.
const (
	Private Kind = "private"
	Shared  Kind = "shared"
)

// Role is a user's place in a space.
type Role string

// The roles. A private space's owner is its only user; editors and viewers are the
// members of shared spaces.
const (
	RoleOwner  Role = "owner"
	RoleEditor Role = "editor"
	RoleViewer Role = "viewer"
)

func (r Role) validMember() bool { return r == RoleEditor || r == RoleViewer }

// Access is what a user may do with a space's content.
type Access string

// The levels, each including the ones before it.
const (
	None  Access = "none"
	Read  Access = "read"
	Write Access = "write"
)

// Allows reports whether a covers need.
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

// Space is a space as one user sees it: Role and Access are that user's.
type Space struct {
	ID          int64   `json:"id"`
	WorkspaceID int64   `json:"workspace_id"`
	Kind        Kind    `json:"kind"`
	Name        string  `json:"name"`
	ArchivedAt  *string `json:"archived_at"`
	CreatedAt   string  `json:"created_at"`
	// OfflineDownloads is whether the space's apps may be downloaded for offline use. A shared
	// space's admins turn it off; a private space always allows it.
	OfflineDownloads bool `json:"offline_downloads"`
	// Role is empty for a shared space the user isn't a member of.
	Role   Role   `json:"role,omitempty"`
	Access Access `json:"access"`
}

// access derives what the space's Role allows.
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

// ErrNotFound is the answer for a space that doesn't exist or that the user can't open:
// callers must not tell the two apart.
var ErrNotFound = errors.New("space not found")

// Checker answers access questions from the database.
type Checker struct {
	db *sql.DB
	// reads is where the checks read from: db, or inside one read transaction for the checks that
	// need one snapshot (AppNamed).
	reads reader
	auth  *auth.Service
	// archiveHooks are told when a shared space is archived or brought back (OnArchiveChange).
	archiveHooks []ArchiveHook
	// memberHooks are told when a person is added to a shared space (OnMemberAdded).
	memberHooks []MemberHook
}

// ArchiveHook is told, in the transaction that archives a shared space or brings it back, that it
// did, with the space's ID; the space already has its new state there. What the hook writes is
// committed with the change, and an error it returns undoes the change. It lets a feature that
// keeps timed work for the space's content (package boards' reminders) follow the space without
// this package knowing the feature. It may be called for a space whose state did not change.
type ArchiveHook func(ctx context.Context, tx *sql.Tx, spaceID int64) error

// OnArchiveChange adds a hook. It is for startup, before the checker serves requests.
func (c *Checker) OnArchiveChange(h ArchiveHook) { c.archiveHooks = append(c.archiveHooks, h) }

// MemberHook is told, in the transaction that adds a person to a shared space, that it did, with
// the space's and the person's IDs; the membership is already there. As with an ArchiveHook, what
// it writes is committed with the change and an error it returns undoes it: the schema drops a
// person's timed work with their access, and the hook lets the feature that owns it take it up
// again. It is not called for a person who already was a member.
type MemberHook func(ctx context.Context, tx *sql.Tx, spaceID, userID int64) error

// OnMemberAdded adds a hook. It is for startup, before the checker serves requests.
func (c *Checker) OnMemberAdded(h MemberHook) { c.memberHooks = append(c.memberHooks, h) }

// New returns the access layer over sqlDB, which must be migrated. a authenticates the
// requests of RequireSpace and the space routes; it may be nil for code that only calls
// Check, CanRead, CanWrite and Readable, such as the agent's tools.
func New(sqlDB *sql.DB, a *auth.Service) *Checker {
	return &Checker{db: sqlDB, reads: sqlDB, auth: a}
}

// spaceColumns are the columns scanSpace reads, from spaces s, memberships m (the user's
// membership of the space's workspace) and space_members sm (theirs in the space, if any).
const spaceColumns = `s.id, s.workspace_id, s.kind, s.name, s.archived_at, s.created_at, s.offline_downloads,
	CASE WHEN s.kind = 'private' AND s.owner_id = m.user_id THEN 'owner' ELSE coalesce(sm.role, '') END`

// fromSpaceFor joins a space to userID's workspace membership and space membership: a
// space in a workspace the user doesn't belong to yields no row at all. The user's ID is
// its only parameter.
const fromSpaceFor = `
	FROM spaces s
	JOIN memberships m ON m.workspace_id = s.workspace_id AND m.user_id = ?
	LEFT JOIN space_members sm ON sm.space_id = s.id AND sm.user_id = m.user_id`

type scanner interface{ Scan(dest ...any) error }

func scanSpace(row scanner) (Space, error) {
	var s Space
	if err := row.Scan(&s.ID, &s.WorkspaceID, &s.Kind, &s.Name, &s.ArchivedAt, &s.CreatedAt, &s.OfflineDownloads, &s.Role); err != nil {
		return Space{}, err
	}
	s.Access = s.access()
	return s, nil
}

// lookup returns spaceID as userID sees it, Access None included, or ErrNotFound if the
// space doesn't exist or isn't in one of the user's workspaces.
func (c *Checker) lookup(ctx context.Context, userID, spaceID int64) (Space, error) {
	return lookupIn(ctx, c.reads, userID, spaceID)
}

// Querier is a *sql.DB or a *sql.Tx: where a check reads from.
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
		return Space{}, ErrNotFound
	}
	return s, err
}

// Check returns spaceID as userID sees it, if they may at least read it, and ErrNotFound
// otherwise.
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

// CheckTx is Check read inside tx, the transaction of a change that depends on the answer: with
// the write lock held, the answer is still true when the change commits.
func (c *Checker) CheckTx(ctx context.Context, tx *sql.Tx, userID, spaceID int64) (Space, error) {
	return checkIn(ctx, tx, userID, spaceID)
}

// CanRead reports whether userID may read spaceID's content.
func (c *Checker) CanRead(ctx context.Context, userID, spaceID int64) (bool, error) {
	return c.can(ctx, userID, spaceID, Read)
}

// CanReadIn is CanRead on q, a transaction's reads or the database's: a caller that holds a write
// transaction asks inside it, so the answer is the state that transaction writes into.
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

// CheckWith is Check that reads more in the same statement: columns are extra select-list
// expressions scanned into dest, and joins the (LEFT) joins they come from, which may use spaces
// as s; args are the joins' parameters in order. A card's title or an app's name read this way
// and the right to read them come from one snapshot, which two reads can't give: access could
// end, and the name change, between them. columns and joins are the caller's constants, never
// anything a person sent. A LEFT JOIN that finds nothing leaves NULLs, so scan them into
// sql.Null* types. ErrNotFound, as for Check, if the space can't be read; dest is then unset.
func (c *Checker) CheckWith(ctx context.Context, userID, spaceID int64, columns, joins string, args []any, dest ...any) (Space, error) {
	var s Space
	all := append([]any{&s.ID, &s.WorkspaceID, &s.Kind, &s.Name, &s.ArchivedAt, &s.CreatedAt, &s.OfflineDownloads, &s.Role}, dest...)
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

// CanWrite reports whether userID may change spaceID's content.
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

// Readable lists the spaces of workspaceID that userID may read: their private space
// first, then the shared spaces they are a member of, by name. It is empty if the user
// isn't a member of the workspace.
func (c *Checker) Readable(ctx context.Context, userID, workspaceID int64) ([]Space, error) {
	return c.list(ctx, userID, workspaceID, false)
}

// list is Readable, plus, with everyShared, the workspace's shared spaces userID isn't a
// member of (for admins managing them).
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

// SpaceFrom returns the space RequireSpace put on ctx, with the caller's Role and Access.
func SpaceFrom(ctx context.Context) (Space, bool) {
	s, ok := ctx.Value(spaceKey{}).(Space)
	return s, ok
}

// RequireSpace is for routes with {workspace} and {space} path values: it lets through
// callers whose access to the space covers need, with the space on the context (see
// SpaceFrom) besides the user and membership auth.Service.RequireMember puts there. It
// answers 401 without a session, 404 for a workspace or space the caller can't read
// (also a space of another workspace), and 403 for a write the caller may only read.
//
// Pick need by what the handler does, not by its method: a GET that only reads uses
// Read, everything else Write.
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

// ExpectedUserHeader names the account a request is meant for: a client that holds one
// account's queued work and sends it later (the offline outbox, docs/offline.md) says whose it is,
// since the browser's session cookie may belong to someone else by then.
const ExpectedUserHeader = "X-Taskto-Expected-User"

// ExpectUser refuses a request that names, in ExpectedUserHeader, another account than the
// session's: 409 session_account_changed, before the space or anything in it is looked at, so
// nothing is read or written as the wrong person and the answer says nothing of what they may
// see. A request without the header passes unchanged, and one with an unreadable value is a 400.
// A request without a session is the usual 401.
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

// RequireSpaceAdmin is RequireSpace(Write) for workspace admins only: it answers 403 to
// the space's other writers. It is for actions on a space's content that only an admin
// may take, such as removing a file's old versions from its history.
func (c *Checker) RequireSpaceAdmin(next http.Handler) http.Handler {
	return c.RequireSpace(Write, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m, _ := auth.MembershipFrom(r.Context()); m.Role != auth.RoleAdmin {
			writeError(w, http.StatusForbidden, apierr.CodeAdminOnly, "only a workspace admin can do this")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// spaceInPath looks up the {space} of the request's {workspace} as the caller sees it,
// Access None included; it answers 404 if the space doesn't exist or is in another
// workspace. Call it behind RequireMember or RequireAdmin.
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

// serverError logs err and answers 500 without details.
func serverError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return // the client went away
	}
	slog.Error("access: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	apierr.Internal(w)
}
