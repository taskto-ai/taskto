// Package audit is the audit log of security and admin actions: who did what to which thing,
// when and from where (docs/audit.md).
//
// The code that makes a change records it in the same transaction (Record), so the event and
// the change commit together or not at all. An event that has no change to commit with, such as
// a failed login, is recorded on its own (Log), and a failure to write it is logged, never
// returned: it must not turn a refusal into a server error.
//
// The log never holds a secret, and never free text. An event carries ids, a stable action code
// and the few details its action is declared to have (schema.go), each of a fixed type: a
// boolean, a number, a value from a fixed list, a host, an address. Record refuses any other
// detail or a value of another type, so a careless call fails its test rather than leaking.
// This package stores and removes events; the admin view is separate.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Event is one thing that happened. The zero value of an ID field means none.
type Event struct {
	// Workspace is the workspace the event belongs to, or 0 for one that belongs to no
	// workspace: a login, a password change.
	Workspace int64
	// Actor is the user who did it, or 0 to take the one the request is authenticated as
	// (WithActor), which is none for an anonymous attempt.
	Actor int64
	// Action is a stable snake_case code (actions.go): what the event is.
	Action string
	// TargetType and Target say what the action was done to: "user", "invite", "space"…
	TargetType string
	Target     int64
	// TargetCreated is when the target was created, as the target's own created_at holds it: with
	// the type and the id, it says which object the event is about, since SQLite hands a deleted
	// row's id to a later one. Record looks it up when it is empty and the target still exists; code
	// that deletes the target passes it from the delete's RETURNING.
	TargetCreated string
	// Details are non-secret facts about the event, of the types schemas gives its action.
	Details Details
}

// Details are an event's small set of non-secret facts: the ones its action declares (schema.go),
// of the declared types.
type Details map[string]any

// Limits of an event's details.
const (
	maxDetailList   = 16
	maxDetailsBytes = 2048
)

// secretName matches the names of things that are secrets. A detail with such a name may only be
// a boolean ("api_key_changed": true says that a key changed, not what it is); schemas is
// checked against it too.
var secretName = regexp.MustCompile(`(?i)pass(word|wd|phrase)|secret|token|key|credential|hash|link|cookie|session|body|signature`)

var validAction = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

type ctxKey int

const (
	actorKey ctxKey = iota
	clientKey
)

// WithActor returns ctx with the user the request is authenticated as, the actor of the events
// recorded with it (the auth middleware does this).
func WithActor(ctx context.Context, user int64) context.Context {
	return context.WithValue(ctx, actorKey, user)
}

// WithClient returns ctx with the address the request came from, which the events recorded with it
// carry (the server does this for every request).
func WithClient(ctx context.Context, addr string) context.Context {
	return context.WithValue(ctx, clientKey, addr)
}

// ActorFrom returns the user WithActor put on ctx, or 0.
func ActorFrom(ctx context.Context) int64 {
	id, _ := ctx.Value(actorKey).(int64)
	return id
}

func clientFrom(ctx context.Context) string {
	addr, _ := ctx.Value(clientKey).(string)
	return addr
}

// Execer is a database or a transaction.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// targetTables are the tables of the targets whose creation time an event records.
var targetTables = map[string]string{
	TargetUser:          "users",
	TargetWorkspace:     "workspaces",
	TargetInvite:        "invites",
	TargetAccessToken:   "access_tokens",
	TargetSpace:         "spaces",
	TargetProvider:      "provider_accounts",
	TargetSharedAccount: "cli_shared_accounts",
}

// Record adds e to the log through x, which should be the transaction of the change it
// describes. The actor and the client address default to the ones on ctx. It fails for an event
// without a valid action or with details its action doesn't declare; the caller then rolls the
// change back, which is the point: such an event is a bug to fix, not to hide.
func Record(ctx context.Context, x Execer, e Event) error {
	if !validAction.MatchString(e.Action) {
		return fmt.Errorf("audit: %q is not a snake_case action", e.Action)
	}
	details, err := encode(e.Action, e.Details)
	if err != nil {
		return fmt.Errorf("audit: %s: %w", e.Action, err)
	}
	if e.Actor == 0 {
		e.Actor = ActorFrom(ctx)
	}
	// The target's creation time: given, or read from the target if it still exists. The table
	// name comes from the fixed list above, never from the event.
	args := []any{nullID(e.Workspace), nullID(e.Actor), e.Action, e.TargetType, nullID(e.Target), details, nullString(clientFrom(ctx))}
	lookup := `NULL`
	table, known := targetTables[e.TargetType]
	switch {
	case e.TargetCreated != "":
		lookup = `?`
		args = append(args, e.TargetCreated)
	case known && e.Target != 0:
		lookup = `(SELECT created_at FROM ` + table + ` WHERE id = ?)`
		args = append(args, e.Target)
	}
	_, err = x.ExecContext(ctx, `
		INSERT INTO audit_events (workspace_id, actor_id, action, target_type, target_id, details, client_addr, target_created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, `+lookup+`)`, args...)
	return err
}

// Log is Record for an event with no transaction to commit with. A failure is logged and
// swallowed: the request it is about has its own answer.
func Log(ctx context.Context, x Execer, e Event) {
	// Not the request's context: a client that leaves mustn't lose the event.
	ctx = context.WithoutCancel(ctx)
	if err := Record(ctx, x, e); err != nil {
		slog.ErrorContext(ctx, "audit: recording an event", "action", e.Action, "err", err)
	}
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// encode checks details against the schema of action and returns them as JSON.
func encode(action string, d Details) (string, error) {
	schema, ok := schemas[action]
	if !ok {
		return "", errors.New("an action without a schema")
	}
	if len(d) == 0 {
		return "{}", nil
	}
	out := make(map[string]any, len(d))
	for name, v := range d {
		k, ok := schema[name]
		if !ok {
			return "", fmt.Errorf("detail %q isn't one of this action's", name)
		}
		if secretName.MatchString(name) && !k.boolean {
			return "", fmt.Errorf("detail %q is named like a secret: say only that it changed (a boolean), never its value", name)
		}
		checked, err := k.check(v)
		if err != nil {
			return "", fmt.Errorf("detail %q: %w", name, err)
		}
		out[name] = checked
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	if len(raw) > maxDetailsBytes {
		return "", errors.New("details too large")
	}
	return string(raw), nil
}

// Changed lists the names of the fields that are set, sorted, for an event's "fields" detail.
func Changed(fields map[string]bool) []string {
	var names []string
	for name, set := range fields {
		if set {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// Row is a stored event.
type Row struct {
	ID          int64  `json:"id"`
	At          string `json:"at"`
	WorkspaceID *int64 `json:"workspace_id"`
	ActorID     *int64 `json:"actor_id"`
	Action      string `json:"action"`
	TargetType  string `json:"target_type"`
	TargetID    *int64 `json:"target_id"`
	// TargetCreatedAt is when the target was created: the object an event is about is the one
	// with its type, id and creation time (docs/audit.md).
	TargetCreatedAt *string        `json:"target_created_at"`
	Details         map[string]any `json:"details"`
	ClientAddr      *string        `json:"client_addr"`
}

// Filter selects events for List. The zero value selects them all.
type Filter struct {
	// Workspace limits the list to one workspace's events; 0 is every event.
	Workspace int64
	// Action limits it to one action code.
	Action string
	// Limit is how many to return, newest first; 0 is 100.
	Limit int
}

// List returns events, newest first. It checks no access: its callers decide who may see which
// workspace's events.
func List(ctx context.Context, db *sql.DB, f Filter) ([]Row, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	q := `SELECT id, at, workspace_id, actor_id, action, target_type, target_id, target_created_at, details, client_addr FROM audit_events WHERE 1 = 1`
	var args []any
	if f.Workspace != 0 {
		q += ` AND workspace_id = ?`
		args = append(args, f.Workspace)
	}
	if f.Action != "" {
		q += ` AND action = ?`
		args = append(args, f.Action)
	}
	q += ` ORDER BY at DESC, id DESC LIMIT ?`
	rows, err := db.QueryContext(ctx, q, append(args, f.Limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := []Row{}
	for rows.Next() {
		var r Row
		var details string
		if err := rows.Scan(&r.ID, &r.At, &r.WorkspaceID, &r.ActorID, &r.Action, &r.TargetType, &r.TargetID, &r.TargetCreatedAt, &details, &r.ClientAddr); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(details), &r.Details); err != nil {
			return nil, err
		}
		all = append(all, r)
	}
	return all, rows.Err()
}

// DefaultRetention is how long events are kept unless the operator says otherwise.
const DefaultRetention = 365 * 24 * time.Hour

// pruneBatch bounds one delete, so a large backlog doesn't hold the write lock for long.
const pruneBatch = 5000

// Prune deletes the events older than cutoff and returns how many.
func Prune(ctx context.Context, db *sql.DB, cutoff time.Time) (int64, error) {
	at := cutoff.UTC().Format("2006-01-02T15:04:05.000Z")
	var total int64
	for {
		res, err := db.ExecContext(ctx, `DELETE FROM audit_events WHERE id IN
			(SELECT id FROM audit_events WHERE at < ? LIMIT ?)`, at, pruneBatch)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < pruneBatch {
			return total, nil
		}
	}
}

// Retain removes events older than keep every interval until ctx is done, once at the start. A
// keep of zero or less keeps them all, and Retain returns at once.
func Retain(ctx context.Context, db *sql.DB, keep, every time.Duration) {
	if keep <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		switch n, err := Prune(ctx, db, time.Now().Add(-keep)); {
		case err != nil && ctx.Err() == nil:
			slog.Warn("audit: removing old events", "err", err)
		case n > 0:
			slog.Info("audit: removed events past their retention", "events", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Host is the host of a URL, for a detail that says where a provider points without carrying a
// path or credentials that might be in the URL. It is "" for a value that isn't a URL.
func Host(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" {
		return ""
	}
	return u.Host
}
