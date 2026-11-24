// Package audit is the audit log of security and admin actions: who did what to which thing,
// when and from where (docs/audit.md).
//
// The code that makes a change records it in the same transaction (Record), so the event and
// the change commit together or not at all. An event that has no change to commit with, such as
// a failed login, is recorded on its own (Log), and a failure to write it is logged, never
// returned: it must not turn a refusal into a server error.
//
// The log never holds a secret. An event carries ids, a stable action code and a few
// non-secret details; Record refuses details whose names say they are a password, token, key or
// link (and any value that isn't a short string, number, boolean or list of strings), so a
// careless call fails its test rather than leaking. This package stores and removes events; the
// admin view is separate.
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
	"unicode/utf8"
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
	// Details are non-secret facts about the event; see Record for what they may hold.
	Details Details
}

// Details are an event's small set of non-secret facts. Values are strings, booleans, numbers or
// lists of strings.
type Details map[string]any

// Limits of an event's details.
const (
	maxDetailKeys   = 16
	maxDetailString = 200
	maxDetailList   = 16
	maxDetailsBytes = 2048
)

// secretName matches the names of things that are secrets. A detail with such a name may only be
// a boolean ("api_key_changed": true says that a key changed, not what it is).
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

// Record adds e to the log through x, which should be the transaction of the change it
// describes. The actor and the client address default to the ones on ctx. It fails for an event
// without a valid action or with details that could hold a secret; the caller then rolls the
// change back, which is the point: such an event is a bug to fix, not to hide.
func Record(ctx context.Context, x Execer, e Event) error {
	if !validAction.MatchString(e.Action) {
		return fmt.Errorf("audit: %q is not a snake_case action", e.Action)
	}
	details, err := encode(e.Details)
	if err != nil {
		return fmt.Errorf("audit: %s: %w", e.Action, err)
	}
	if e.Actor == 0 {
		e.Actor = ActorFrom(ctx)
	}
	_, err = x.ExecContext(ctx, `
		INSERT INTO audit_events (workspace_id, actor_id, action, target_type, target_id, details, client_addr)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		nullID(e.Workspace), nullID(e.Actor), e.Action, e.TargetType, nullID(e.Target), details,
		nullString(clientFrom(ctx)))
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

// encode checks details and returns them as JSON.
func encode(d Details) (string, error) {
	if len(d) == 0 {
		return "{}", nil
	}
	if len(d) > maxDetailKeys {
		return "", errors.New("too many details")
	}
	out := make(map[string]any, len(d))
	for k, v := range d {
		switch v := v.(type) {
		case bool:
			out[k] = v
			continue
		case int, int64, float64:
			out[k] = v
		case string:
			out[k] = clip(v)
		case []string:
			if len(v) > maxDetailList {
				v = v[:maxDetailList]
			}
			list := make([]string, len(v))
			for i, s := range v {
				list[i] = clip(s)
			}
			out[k] = list
		default:
			return "", fmt.Errorf("detail %q: a %T isn't a string, number, boolean or list of strings", k, v)
		}
		if secretName.MatchString(k) {
			return "", fmt.Errorf("detail %q is named like a secret: say only that it changed (a boolean), never its value", k)
		}
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

func clip(s string) string {
	if utf8.RuneCountInString(s) <= maxDetailString {
		return s
	}
	return string([]rune(s)[:maxDetailString])
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
	ID          int64          `json:"id"`
	At          string         `json:"at"`
	WorkspaceID *int64         `json:"workspace_id"`
	ActorID     *int64         `json:"actor_id"`
	Action      string         `json:"action"`
	TargetType  string         `json:"target_type"`
	TargetID    *int64         `json:"target_id"`
	Details     map[string]any `json:"details"`
	ClientAddr  *string        `json:"client_addr"`
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
	q := `SELECT id, at, workspace_id, actor_id, action, target_type, target_id, details, client_addr FROM audit_events WHERE 1 = 1`
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
		if err := rows.Scan(&r.ID, &r.At, &r.WorkspaceID, &r.ActorID, &r.Action, &r.TargetType, &r.TargetID, &details, &r.ClientAddr); err != nil {
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
