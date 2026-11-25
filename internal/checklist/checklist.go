// Package checklist is the first-run checklist an admin sees on Home: the steps that make a
// new workspace useful to a team (invite people, add a model provider, make a shared space,
// add an app, and, if the server's operator has set them up, email and backups), each marked
// done by what exists, not by a tick.
//
// Only two things are stored (migration 0039): that the checklist was dismissed, and which
// steps an admin skipped because they don't apply (a one-person workspace has no team to
// invite). Both are the workspace's, shared by its admins. Everything else is read when the
// checklist is asked for, so a step done by any route, the settings pages or the agent, is done
// here too, and a step undone (the last provider removed) is open again.
//
// The checklist is for the workspace's admins only. It says whether things exist, never what
// they are: no names, addresses or counts of what other people own.
package checklist

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
)

// inviteTimeFormat is how invites store their expiry (internal/auth), so it compares as text.
const inviteTimeFormat = "2006-01-02T15:04:05.000Z"

// Item names a step.
type Item string

// The steps, in the order they are shown.
const (
	Invite   Item = "invite"   // someone besides the person who set up is a member, or has an open or accepted invite
	Provider Item = "provider" // the workspace has a model provider account
	Space    Item = "space"    // the workspace has a shared space that isn't archived
	App      Item = "app"      // an app exists in a shared space the caller is in, or in the caller's own space
	Email    Item = "email"    // the server can send email (the operator's SMTP settings)
	Backups  Item = "backups"  // the server's scheduled backups have a status
)

// Items lists the steps in order.
var Items = []Item{Invite, Provider, Space, App, Email, Backups}

// optional steps don't hold the checklist open: it is complete without them. Email and backups
// are set by whoever runs the server, outside the app.
func (i Item) optional() bool { return i == Email || i == Backups }

// State is where a step stands.
type State string

// The states of a step. A step that is done stays done whatever was skipped.
const (
	Done    State = "done"
	Todo    State = "todo"
	Skipped State = "skipped"
)

// Entry is a step as the caller sees it.
type Entry struct {
	ID       Item  `json:"id"`
	State    State `json:"state"`
	Optional bool  `json:"optional"`
}

// View is the checklist of a workspace.
type View struct {
	// Dismissed is true after an admin put the checklist away; Home then shows nothing.
	Dismissed bool `json:"dismissed"`
	// Complete is true when every step that isn't optional is done or skipped.
	Complete bool    `json:"complete"`
	Items    []Entry `json:"items"`
}

// Options say what only the server knows.
type Options struct {
	// Email reports whether the server can send email. nil means it can't.
	Email func() bool
	// Backups reports whether the server's scheduled backups have a status. nil means they
	// don't.
	Backups func() (bool, error)
}

// Service answers the checklist.
type Service struct {
	db   *sql.DB
	opts Options
}

// New returns the service over db, which must be migrated.
func New(db *sql.DB, opts Options) *Service { return &Service{db: db, opts: opts} }

// ErrUnknownItem is returned for a step that isn't in Items.
var ErrUnknownItem = errors.New("checklist: no such step")

// Parse returns the step named s.
func Parse(s string) (Item, error) {
	if i := Item(s); slices.Contains(Items, i) {
		return i, nil
	}
	return "", ErrUnknownItem
}

// View returns the checklist of workspaceID for userID, one of its admins (the routes check).
func (s *Service) View(ctx context.Context, workspaceID, userID int64) (View, error) {
	done, err := s.done(ctx, workspaceID, userID)
	if err != nil {
		return View{}, err
	}
	skipped, err := s.skipped(ctx, workspaceID)
	if err != nil {
		return View{}, err
	}
	var dismissed bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM setup_checklist_dismissed WHERE workspace_id = ?)`, workspaceID).Scan(&dismissed); err != nil {
		return View{}, err
	}
	v := View{Dismissed: dismissed, Complete: true, Items: make([]Entry, 0, len(Items))}
	for _, item := range Items {
		state := Todo
		switch {
		case done[item]:
			state = Done
		case skipped[item]:
			state = Skipped
		}
		if state == Todo && !item.optional() {
			v.Complete = false
		}
		v.Items = append(v.Items, Entry{ID: item, State: state, Optional: item.optional()})
	}
	return v, nil
}

// done works out which steps are done.
func (s *Service) done(ctx context.Context, workspaceID, userID int64) (map[Item]bool, error) {
	var invite, provider, space, app bool
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM memberships WHERE workspace_id = ?1) > 1
			OR EXISTS (SELECT 1 FROM invites WHERE workspace_id = ?1
				AND (accepted_at IS NOT NULL OR expires_at > ?3)),
		EXISTS (SELECT 1 FROM provider_accounts WHERE workspace_id = ?1),
		EXISTS (SELECT 1 FROM spaces WHERE workspace_id = ?1 AND kind = 'shared' AND archived_at IS NULL),
		EXISTS (SELECT 1 FROM apps a JOIN spaces sp ON sp.id = a.space_id
			WHERE sp.workspace_id = ?1 AND a.archived_at IS NULL AND sp.archived_at IS NULL
			  AND (sp.owner_id = ?2
			    OR (sp.kind = 'shared'
			        AND EXISTS (SELECT 1 FROM space_members m WHERE m.space_id = sp.id AND m.user_id = ?2))))`,
		workspaceID, userID, time.Now().UTC().Format(inviteTimeFormat)).Scan(&invite, &provider, &space, &app)
	if err != nil {
		return nil, err
	}
	done := map[Item]bool{Invite: invite, Provider: provider, Space: space, App: app}
	if s.opts.Email != nil {
		done[Email] = s.opts.Email()
	}
	if s.opts.Backups != nil {
		// A status that can't be read is a step still to do, not a failed checklist.
		ok, err := s.opts.Backups()
		done[Backups] = ok && err == nil
	}
	return done, nil
}

func (s *Service) skipped(ctx context.Context, workspaceID int64) (map[Item]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT item FROM setup_checklist_skipped WHERE workspace_id = ?`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[Item]bool{}
	for rows.Next() {
		var item Item
		if err := rows.Scan(&item); err != nil {
			return nil, err
		}
		out[item] = true
	}
	return out, rows.Err()
}

// Dismiss puts the checklist away for the workspace, or with false brings it back.
func (s *Service) Dismiss(ctx context.Context, workspaceID, userID int64, dismissed bool) error {
	if !dismissed {
		_, err := s.db.ExecContext(ctx, `DELETE FROM setup_checklist_dismissed WHERE workspace_id = ?`, workspaceID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO setup_checklist_dismissed (workspace_id, dismissed_by) VALUES (?, ?)
		ON CONFLICT (workspace_id) DO NOTHING`, workspaceID, userID)
	return err
}

// Skip marks a step as not applying to the workspace, or with false takes the mark off.
func (s *Service) Skip(ctx context.Context, workspaceID, userID int64, item Item, skipped bool) error {
	if !skipped {
		_, err := s.db.ExecContext(ctx, `DELETE FROM setup_checklist_skipped WHERE workspace_id = ? AND item = ?`, workspaceID, item)
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO setup_checklist_skipped (workspace_id, item, skipped_by) VALUES (?, ?, ?)
		ON CONFLICT (workspace_id, item) DO NOTHING`, workspaceID, item, userID)
	return err
}

// Routes mounts the admins' routes (docs/api.md#setup-checklist). Every one is for a workspace's
// admins only, and for that workspace.
func (s *Service) Routes(mux *http.ServeMux, accounts *auth.Service) {
	const base = "/api/workspaces/{workspace}/checklist"
	admin := func(h http.HandlerFunc) http.Handler { return accounts.RequireAdmin(h) }
	mux.Handle("GET "+base, admin(s.handleGet(accounts)))
	mux.Handle("PUT "+base+"/dismissed", admin(s.handleDismiss(true)))
	mux.Handle("DELETE "+base+"/dismissed", admin(s.handleDismiss(false)))
	mux.Handle("PUT "+base+"/skipped/{item}", admin(s.handleSkip(true)))
	mux.Handle("DELETE "+base+"/skipped/{item}", admin(s.handleSkip(false)))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// fail logs err and answers 500 without details.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() == nil {
		slog.Error("checklist: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	apierr.Internal(w)
}

func caller(r *http.Request) (workspaceID, userID int64) {
	u, _ := auth.UserFrom(r.Context())
	m, _ := auth.MembershipFrom(r.Context())
	return m.WorkspaceID, u.ID
}

// serverWide are the steps about the whole server, not the workspace: whether it can send email and
// whether its backups run. Only operators (auth.Service.IsOperator) are told; the admins of a
// workspace someone else created have no say in either, so their checklist doesn't list them.
func serverWide(i Item) bool { return i == Email || i == Backups }

func (s *Service) handleGet(accounts *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ws, user := caller(r)
		v, err := s.View(r.Context(), ws, user)
		if err != nil {
			fail(w, r, err)
			return
		}
		operator, err := accounts.IsOperator(r.Context(), user)
		if err != nil {
			fail(w, r, err)
			return
		}
		if !operator {
			v.Items = slices.DeleteFunc(v.Items, func(e Entry) bool { return serverWide(e.ID) })
		}
		writeJSON(w, v)
	}
}

func (s *Service) handleDismiss(dismissed bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ws, user := caller(r)
		if err := s.Dismiss(r.Context(), ws, user, dismissed); err != nil {
			fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Service) handleSkip(skipped bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		item, err := Parse(r.PathValue("item"))
		if err != nil {
			apierr.Write(w, http.StatusNotFound, apierr.CodeNotFound, "no such checklist step")
			return
		}
		ws, user := caller(r)
		if err := s.Skip(r.Context(), ws, user, item, skipped); err != nil {
			fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
