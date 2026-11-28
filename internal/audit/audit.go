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

type Event struct {
	Workspace     int64
	Actor         int64
	Action        string
	TargetType    string
	Target        int64
	TargetCreated string
	Details       Details
}

type Details map[string]any

const (
	maxDetailList   = 16
	maxDetailsBytes = 2048
)

var secretName = regexp.MustCompile(`(?i)pass(word|wd|phrase)|secret|token|key|credential|hash|link|cookie|session|body|signature`)

var validAction = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

type ctxKey int

const (
	actorKey ctxKey = iota
	clientKey
)

func WithActor(ctx context.Context, user int64) context.Context {
	return context.WithValue(ctx, actorKey, user)
}

func WithClient(ctx context.Context, addr string) context.Context {
	return context.WithValue(ctx, clientKey, addr)
}

func ActorFrom(ctx context.Context) int64 {
	id, _ := ctx.Value(actorKey).(int64)
	return id
}

func ClientFrom(ctx context.Context) string { return clientFrom(ctx) }

func clientFrom(ctx context.Context) string {
	addr, _ := ctx.Value(clientKey).(string)
	return addr
}

type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

var targetTables = map[string]string{
	TargetUser:          "users",
	TargetWorkspace:     "workspaces",
	TargetInvite:        "invites",
	TargetAccessToken:   "access_tokens",
	TargetSpace:         "spaces",
	TargetProvider:      "provider_accounts",
	TargetSharedAccount: "cli_shared_accounts",
	TargetIntegration:   "integrations",

	TargetAppIntegration: "app_integration_approvals",
	TargetAppDataSource:  "app_data_source_approvals",
}

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

func Log(ctx context.Context, x Execer, e Event) {
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

type Row struct {
	ID              int64          `json:"id"`
	At              string         `json:"at"`
	WorkspaceID     *int64         `json:"workspace_id"`
	ActorID         *int64         `json:"actor_id"`
	ActorDeleted    bool           `json:"actor_deleted"`
	Action          string         `json:"action"`
	TargetType      string         `json:"target_type"`
	TargetID        *int64         `json:"target_id"`
	TargetDeleted   bool           `json:"target_deleted"`
	TargetCreatedAt *string        `json:"target_created_at"`
	Details         map[string]any `json:"details"`
	ClientAddr      *string        `json:"client_addr"`
}

type Filter struct {
	Workspace int64
	Action    string
	Limit     int
}

func List(ctx context.Context, db *sql.DB, f Filter) ([]Row, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	q := `SELECT id, at, workspace_id, actor_id, actor_deleted, action, target_type, target_id, target_deleted, target_created_at, details, client_addr FROM audit_events WHERE 1 = 1`
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
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		all = append(all, r)
	}
	return all, rows.Err()
}

func scan(rows *sql.Rows) (Row, error) {
	var r Row
	var details string
	if err := rows.Scan(&r.ID, &r.At, &r.WorkspaceID, &r.ActorID, &r.ActorDeleted, &r.Action, &r.TargetType, &r.TargetID, &r.TargetDeleted, &r.TargetCreatedAt, &details, &r.ClientAddr); err != nil {
		return Row{}, err
	}
	if err := json.Unmarshal([]byte(details), &r.Details); err != nil {
		return Row{}, err
	}
	return r, nil
}

const DefaultRetention = 365 * 24 * time.Hour

const pruneBatch = 5000

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

func Host(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" {
		return ""
	}
	return u.Host
}
