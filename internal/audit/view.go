package audit

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The admin view of the log (docs/audit.md#reading-the-log): one workspace's events, newest
// first, with filters and a cursor. Unlike List, which takes a zero workspace as "every
// workspace" for tests, everything here needs the workspace and refuses without it, so a caller
// that lost its workspace id gets an error, not another workspace's events.

// ErrNoWorkspace is what ListWorkspace answers to a workspace that is not an id: the log of
// no workspace is not the log of every workspace.
var ErrNoWorkspace = errors.New("audit: reading a workspace's log needs the workspace")

// ErrBadCursor is a cursor ListWorkspace didn't give out.
var ErrBadCursor = errors.New("audit: not a cursor of this log")

// Page limits of ListWorkspace.
const (
	DefaultPage = 50
	MaxPage     = 200
)

const timeLayout = "2006-01-02T15:04:05.000Z"

// Query selects a workspace's events. The zero value selects all of them, one page.
type Query struct {
	// Action limits it to one action code (KnownAction).
	Action string
	// TargetType limits it to events about one kind of object (KnownTarget).
	TargetType string
	// Actor limits it to what one user did; 0 is anyone.
	Actor int64
	// From and To bound the time of the events: From inclusive, To exclusive. The zero time is
	// no bound.
	From, To time.Time
	// Cursor is the Next of the previous page.
	Cursor string
	// Limit is the page size: 0 is DefaultPage, and more than MaxPage is MaxPage.
	Limit int
}

// Person is who an event's actor or target is, as the workspace's admins may know them: a
// current member of the workspace.
type Person struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Entry is an event with the names of whom and what it is about, read when the log is read.
type Entry struct {
	Row
	// Actor is the user who did it if they are a member of the workspace now; null for an
	// anonymous event, a deleted user and one who has left (the id is still ActorID).
	Actor *Person `json:"actor"`
	// TargetUser is the user an event is about, on the same terms as Actor.
	TargetUser *Person `json:"target_user"`
	// TargetName is the name a workspace admin gave a space, a provider account or a shared CLI
	// account the event is about, or the workspace's own; null once it is gone, or when the id
	// now belongs to another object.
	TargetName *string `json:"target_name"`
}

// Page is a page of a workspace's events.
type Page struct {
	Events []Entry `json:"events"`
	// Next is the cursor of the following page; empty when there is none.
	Next string `json:"next"`
}

// KnownAction reports whether code is an action of the log.
func KnownAction(code string) bool {
	_, ok := schemas[code]
	return ok
}

// KnownTarget reports whether t is a kind of object an event can be about.
func KnownTarget(t string) bool {
	_, ok := targetTables[t]
	return ok
}

// upToMs is t as the log writes times, rounded up to the millisecond it stores: events are at
// whole milliseconds, so "at >= from" and "at < to" mean the same with a bound that has finer
// digits as with the first whole millisecond at or after it.
func upToMs(t time.Time) string {
	return t.UTC().Add(time.Millisecond - 1).Truncate(time.Millisecond).Format(timeLayout)
}

func cursorFor(r Row) string {
	return base64.RawURLEncoding.EncodeToString([]byte(r.At + "|" + strconv.FormatInt(r.ID, 10)))
}

func parseCursor(c string) (at string, id int64, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return "", 0, ErrBadCursor
	}
	at, rest, ok := strings.Cut(string(raw), "|")
	if !ok {
		return "", 0, ErrBadCursor
	}
	if _, err := time.Parse(timeLayout, at); err != nil {
		return "", 0, ErrBadCursor
	}
	id, err = strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		return "", 0, ErrBadCursor
	}
	return at, id, nil
}

// listSQL is the statement of ListWorkspace for q, its arguments and the page size it clamps q.Limit to.
func listSQL(workspace int64, q Query) (stmt string, args []any, limit int, err error) {
	limit = q.Limit
	if limit <= 0 {
		limit = DefaultPage
	}
	limit = min(limit, MaxPage)

	where := `workspace_id = ?`
	args = []any{workspace}
	if q.Action != "" {
		where += ` AND action = ?`
		args = append(args, q.Action)
	}
	if q.TargetType != "" {
		where += ` AND target_type = ?`
		args = append(args, q.TargetType)
	}
	if q.Actor != 0 {
		where += ` AND actor_id = ?`
		args = append(args, q.Actor)
	}
	if !q.From.IsZero() {
		where += ` AND at >= ?`
		args = append(args, upToMs(q.From))
	}
	if !q.To.IsZero() {
		where += ` AND at < ?`
		args = append(args, upToMs(q.To))
	}
	if q.Cursor != "" {
		var at string
		var id int64
		if at, id, err = parseCursor(q.Cursor); err != nil {
			return "", nil, 0, err
		}
		// A row-value comparison, so SQLite seeks the index (workspace_id, at DESC, id DESC) to the
		// cursor; "at < ? OR (at = ? AND id < ?)" makes it walk the workspace's rows from the newest.
		where += ` AND (at, id) < (?, ?)`
		args = append(args, at, id)
	}
	stmt = `SELECT id, at, workspace_id, actor_id, action, target_type, target_id, target_created_at, details, client_addr
		FROM audit_events WHERE ` + where + ` ORDER BY at DESC, id DESC LIMIT ?`
	return stmt, append(args, limit+1), limit, nil
}

// ListWorkspace returns one page of the events of workspace, newest first, with the names of
// the people and things they are about. Only events whose workspace is this one: events of no
// workspace (logins, password changes, personal provider accounts) belong to users, not to a
// workspace, and are never in its log. It checks no access: its caller (the admin route) has
// already decided that the caller administers workspace.
func ListWorkspace(ctx context.Context, db *sql.DB, workspace int64, q Query) (Page, error) {
	if workspace <= 0 {
		return Page{}, ErrNoWorkspace
	}
	stmt, args, limit, err := listSQL(workspace, q)
	if err != nil {
		return Page{}, err
	}
	rows, err := db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	all := []Row{}
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return Page{}, err
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return Page{}, err
	}
	_ = rows.Close()

	page := Page{Events: []Entry{}}
	if len(all) > limit {
		all = all[:limit]
		page.Next = cursorFor(all[limit-1])
	}
	names := resolver{ctx: ctx, db: db, workspace: workspace}
	for _, r := range all {
		e := Entry{Row: r}
		if r.ActorID != nil {
			if e.Actor, err = names.member(*r.ActorID, ""); err != nil {
				return Page{}, err
			}
		}
		if e.TargetUser, e.TargetName, err = names.target(r); err != nil {
			return Page{}, err
		}
		page.Events = append(page.Events, e)
	}
	return page, nil
}

// resolver looks names up for one page, each once. Everything it finds is scoped to its
// workspace: a user is named only while a member of it, and an object only if it belongs to it.
type resolver struct {
	ctx       context.Context
	db        *sql.DB
	workspace int64
	people    map[string]*Person
	things    map[string]*string
}

// member is the named user while a member of the workspace. created, if not empty, must be the
// time the user was created: an id is not the same user after another took it.
func (r *resolver) member(user int64, created string) (*Person, error) {
	key := fmt.Sprintf("%d|%s", user, created)
	if p, ok := r.people[key]; ok {
		return p, nil
	}
	var p *Person
	var got Person
	err := r.db.QueryRowContext(r.ctx, `SELECT u.name, u.email FROM users u
		JOIN memberships m ON m.user_id = u.id AND m.workspace_id = ?
		WHERE u.id = ? AND (? = '' OR u.created_at = ?)`, r.workspace, user, created, created).Scan(&got.Name, &got.Email)
	switch {
	case err == nil:
		p = &got
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}
	if r.people == nil {
		r.people = map[string]*Person{}
	}
	r.people[key] = p
	return p, nil
}

// thing is the name in query, for the object of this workspace with this id and creation time.
func (r *resolver) thing(query string, id int64, created string) (*string, error) {
	key := fmt.Sprintf("%s|%d|%s", query, id, created)
	if n, ok := r.things[key]; ok {
		return n, nil
	}
	var name *string
	var got string
	err := r.db.QueryRowContext(r.ctx, query, id, r.workspace, created).Scan(&got)
	switch {
	case err == nil:
		name = &got
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}
	if r.things == nil {
		r.things = map[string]*string{}
	}
	r.things[key] = name
	return name, nil
}

// Names of objects: a workspace's own, and ones that belong to it. An access token's name is its
// member's, an invite's address is in the event's details, and a private space is its owner's:
// none is resolved.
const (
	spaceName    = `SELECT name FROM spaces WHERE id = ? AND workspace_id = ? AND created_at = ? AND kind = 'shared'`
	providerName = `SELECT display_name FROM provider_accounts WHERE id = ? AND workspace_id = ? AND created_at = ?`
	sharedName   = `SELECT label FROM cli_shared_accounts WHERE id = ? AND workspace_id = ? AND created_at = ?`
)

func (r *resolver) target(row Row) (*Person, *string, error) {
	if row.TargetID == nil {
		return nil, nil, nil
	}
	id, created := *row.TargetID, ""
	if row.TargetCreatedAt != nil {
		created = *row.TargetCreatedAt
	}
	switch row.TargetType {
	case TargetWorkspace:
		if id != r.workspace {
			return nil, nil, nil
		}
		var name string
		if err := r.db.QueryRowContext(r.ctx, `SELECT name FROM workspaces WHERE id = ?`, r.workspace).Scan(&name); err != nil {
			return nil, nil, err
		}
		return nil, &name, nil
	case TargetUser:
		if created == "" {
			return nil, nil, nil // without its creation time, the id may be another user's now
		}
		p, err := r.member(id, created)
		return p, nil, err
	case TargetSpace, TargetProvider, TargetSharedAccount:
		if created == "" {
			return nil, nil, nil
		}
		query := map[string]string{TargetSpace: spaceName, TargetProvider: providerName, TargetSharedAccount: sharedName}[row.TargetType]
		n, err := r.thing(query, id, created)
		return nil, n, err
	}
	return nil, nil, nil
}
