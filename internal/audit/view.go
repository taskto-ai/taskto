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

var ErrNoWorkspace = errors.New("audit: reading a workspace's log needs the workspace")

var ErrBadCursor = errors.New("audit: not a cursor of this log")

const (
	DefaultPage = 50
	MaxPage     = 200
)

const timeLayout = "2006-01-02T15:04:05.000Z"

type Query struct {
	Action     string
	TargetType string
	Actor      int64
	From, To   time.Time
	Cursor     string
	Limit      int
}

type Person struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type Entry struct {
	Row
	Actor      *Person `json:"actor"`
	TargetUser *Person `json:"target_user"`
	TargetName *string `json:"target_name"`
}

type Page struct {
	Events []Entry `json:"events"`
	Next   string  `json:"next"`
}

func KnownAction(code string) bool {
	_, ok := schemas[code]
	return ok
}

func KnownTarget(t string) bool {
	_, ok := targetTables[t]
	return ok
}

func upToMs(t time.Time) string {
	r := t.UTC().Add(time.Millisecond - 1).Truncate(time.Millisecond)
	if r.Year() > 9999 {
		return afterEnd
	}
	return r.Format(timeLayout)
}

const afterEnd = "9999-12-31T23:59:59.999Z~"

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
		where += ` AND (at, id) < (?, ?)`
		args = append(args, at, id)
	}
	stmt = `SELECT id, at, workspace_id, actor_id, actor_deleted, action, target_type, target_id, target_deleted, target_created_at, details, client_addr
		FROM audit_events WHERE ` + where + ` ORDER BY at DESC, id DESC LIMIT ?`
	return stmt, append(args, limit+1), limit, nil
}

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

type resolver struct {
	ctx       context.Context
	db        *sql.DB
	workspace int64
	people    map[string]*Person
	things    map[string]*string
}

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

const (
	spaceName       = `SELECT name FROM spaces WHERE id = ? AND workspace_id = ? AND created_at = ? AND kind = 'shared'`
	providerName    = `SELECT display_name FROM provider_accounts WHERE id = ? AND workspace_id = ? AND created_at = ?`
	sharedName      = `SELECT label FROM cli_shared_accounts WHERE id = ? AND workspace_id = ? AND created_at = ?`
	integrationName = `SELECT display_name FROM integrations WHERE id = ? AND workspace_id = ? AND created_at = ?`
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
			return nil, nil, nil
		}
		p, err := r.member(id, created)
		return p, nil, err
	case TargetSpace, TargetProvider, TargetSharedAccount, TargetIntegration:
		if created == "" {
			return nil, nil, nil
		}
		query := map[string]string{
			TargetSpace: spaceName, TargetProvider: providerName, TargetSharedAccount: sharedName, TargetIntegration: integrationName,
		}[row.TargetType]
		n, err := r.thing(query, id, created)
		return nil, n, err
	}
	return nil, nil, nil
}
