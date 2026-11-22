package apps

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
)

// Actor is who makes a change: a user, or the agent working for one. The agent has
// exactly its user's access, and changes it makes are marked as the agent's. An agent of
// the user's own reaching the server with an access token is one too: Via is then the
// token's name.
type Actor struct {
	UserID int64
	Agent  bool
	Via    string
}

// Author is who made something: a user, through the agent if Agent is set, and with the
// access token named Via if one was used. It is nil in replies if the user's account no
// longer exists.
type Author struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Agent bool   `json:"agent"`
	Via   string `json:"via,omitempty"`
}

// App is an app as a list shows it; Definition is set when one app is read.
type App struct {
	ID      int64  `json:"id"`
	SpaceID int64  `json:"space_id"`
	Name    string `json:"name"`
	// ArchivedAt is when the app was taken offline, or nil.
	ArchivedAt *string `json:"archived_at"`
	Version    int64   `json:"version"`
	// Rows is how many rows its collection has.
	Rows       int         `json:"rows"`
	CreatedAt  string      `json:"created_at"`
	CreatedBy  *Author     `json:"created_by"`
	UpdatedAt  string      `json:"updated_at"`
	UpdatedBy  *Author     `json:"updated_by"`
	Definition *Definition `json:"definition,omitempty"`
}

var (
	// ErrNotFound is the answer for a space or app that doesn't exist or that the user
	// can't read: callers must not tell the two apart.
	ErrNotFound = errors.New("not found")
	// ErrReadOnly is for a write to a space the user may only read.
	ErrReadOnly = errors.New("you can only read this space")
	// ErrOffline is for launching an app that is offline, or running its actions.
	ErrOffline = errors.New("this app is offline: restore it to launch it")
	// ErrStale is for an update made against a version that is no longer the current
	// one: someone else changed the app in between.
	ErrStale = errors.New("it changed since you read it: reload and try again")
	// ErrFull is for a row that would take an app over MaxRows or MaxDataSize, or an app
	// that would take a space over maxApps.
	ErrFull = errors.New("full")
)

// fullError is ErrFull, saying which limit, with the API's code for it.
type fullError struct {
	msg    string
	code   apierr.Code
	params apierr.Params
}

func (e *fullError) Error() string        { return ErrFull.Error() + ": " + e.msg }
func (e *fullError) Is(target error) bool { return target == ErrFull }

// errTooManyApps is a space at maxApps.
var errTooManyApps = &fullError{
	msg:  fmt.Sprintf("a space has at most %d apps", maxApps),
	code: apierr.CodeTooManyApps, params: apierr.Params{"max": maxApps},
}

// appFull is an app at MaxRows or MaxDataSize; msg says which.
func appFull(msg string) error {
	return &fullError{msg: msg, code: apierr.CodeAppFull, params: apierr.Params{"max_rows": MaxRows, "max_mib": MaxDataSize >> 20}}
}

// maxApps bounds the apps of one space, offline ones included.
const maxApps = 500

const maxNameLen = 100

// Service stores apps and their rows, and checks access to them through package access.
type Service struct {
	db     *sql.DB
	access *access.Checker
}

// New returns the apps service over sqlDB, which must be migrated.
func New(sqlDB *sql.DB, c *access.Checker) *Service {
	return &Service{db: sqlDB, access: c}
}

// timeFormat matches strftime('%Y-%m-%dT%H:%M:%fZ'), the schema's format for times.
const timeFormat = "2006-01-02T15:04:05.000Z"

func now() string { return time.Now().UTC().Format(timeFormat) }

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type scanner interface{ Scan(dest ...any) error }

// write runs fn in a transaction. Transactions take the write lock when they begin (see
// package db), so the checks in fn (versions, bounds) hold for its writes.
func (s *Service) write(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if rbErr := tx.Rollback(); !errors.Is(rbErr, sql.ErrTxDone) {
				err = errors.Join(err, rbErr)
			}
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// may checks that sp, as its caller sees it, allows need: ErrNotFound if it can't be
// read, ErrReadOnly if it can't be written.
// recheck checks again, inside a write transaction, that a may still write the spaces ids. A
// transaction begins with BEGIN IMMEDIATE, so the write lock is held: a membership change or
// an archive made since the request was checked has committed by now, and none can until this
// write has. The request's snapshot of the space may be stale; this can't be.
func (s *Service) recheck(ctx context.Context, a Actor, ids ...int64) error {
	for _, id := range ids {
		now, err := s.access.Check(ctx, a.UserID, id)
		if errors.Is(err, access.ErrNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := may(now, access.Write); err != nil {
			return err
		}
	}
	return nil
}

func may(sp access.Space, need access.Access) error {
	switch {
	case !sp.Access.Allows(access.Read):
		return ErrNotFound
	case !sp.Access.Allows(need) && sp.ArchivedAt != nil:
		return errSpaceArchived
	case !sp.Access.Allows(need):
		return ErrReadOnly
	}
	return nil
}

// errSpaceArchived is ErrReadOnly, saying why.
var errSpaceArchived = readOnlyError("this space is archived: it is read-only")

type readOnlyError string

func (e readOnlyError) Error() string        { return string(e) }
func (e readOnlyError) Is(target error) bool { return target == ErrReadOnly }

func author(id sql.NullInt64, name string, agent bool, via sql.NullString) *Author {
	if !id.Valid {
		return nil
	}
	return &Author{ID: id.Int64, Name: name, Agent: agent, Via: via.String}
}

// via is the column value of an actor's access token: NULL without one.
func via(a Actor) any {
	if a.Via == "" {
		return nil
	}
	return a.Via
}

// appColumns selects what scanApp reads, from apps a; appListColumns the same with an empty
// definition in its place, so a listing of up to maxApps apps doesn't read each one's (up to
// 48 KiB) only to throw it away.
const (
	appFields = `a.id, a.space_id, a.name, a.archived_at, a.version,
	(SELECT count(*) FROM app_rows r WHERE r.app_id = a.id),
	a.created_at, a.created_by, coalesce(cu.name, ''), a.created_by_agent, a.created_via,
	a.updated_at, a.updated_by, coalesce(uu.name, ''), a.updated_by_agent, a.updated_via, `
	appFrom = `
	FROM apps a
	LEFT JOIN users cu ON cu.id = a.created_by
	LEFT JOIN users uu ON uu.id = a.updated_by`
	appColumns     = appFields + `a.definition` + appFrom
	appListColumns = appFields + `''` + appFrom
)

// scanApp reads an app; withDefinition decodes its definition too.
func scanApp(row scanner, withDefinition bool) (App, error) {
	var a App
	var createdBy, updatedBy sql.NullInt64
	var createdName, updatedName, def string
	var createdAgent, updatedAgent bool
	var createdVia, updatedVia sql.NullString
	if err := row.Scan(&a.ID, &a.SpaceID, &a.Name, &a.ArchivedAt, &a.Version, &a.Rows,
		&a.CreatedAt, &createdBy, &createdName, &createdAgent, &createdVia,
		&a.UpdatedAt, &updatedBy, &updatedName, &updatedAgent, &updatedVia, &def); err != nil {
		return App{}, err
	}
	a.CreatedBy = author(createdBy, createdName, createdAgent, createdVia)
	a.UpdatedBy = author(updatedBy, updatedName, updatedAgent, updatedVia)
	if withDefinition {
		var d Definition
		if err := json.Unmarshal([]byte(def), &d); err != nil {
			return App{}, fmt.Errorf("app %d: stored definition: %w", a.ID, err)
		}
		a.Definition = &d
	}
	return a, nil
}

// app returns an app of space spaceID, with its definition, or ErrNotFound.
func app(ctx context.Context, q queryer, spaceID, appID int64) (App, error) {
	a, err := scanApp(q.QueryRowContext(ctx, `SELECT `+appColumns+` WHERE a.id = ? AND a.space_id = ?`, appID, spaceID), true)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, ErrNotFound
	}
	return a, err
}

func (s *Service) listApps(ctx context.Context, sp access.Space, withArchived bool) ([]App, error) {
	if err := may(sp, access.Read); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+appListColumns+`
		WHERE a.space_id = ? AND (? OR a.archived_at IS NULL)
		ORDER BY a.archived_at IS NOT NULL, a.name, a.id`, sp.ID, withArchived)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := []App{}
	for rows.Next() {
		a, err := scanApp(rows, false)
		if err != nil {
			return nil, err
		}
		all = append(all, a)
	}
	return all, rows.Err()
}

func (s *Service) getApp(ctx context.Context, sp access.Space, appID int64) (App, error) {
	if err := may(sp, access.Read); err != nil {
		return App{}, err
	}
	return app(ctx, s.db, sp.ID, appID)
}

// NewApp is a new app: its name and definition.
type NewApp struct {
	Name       string     `json:"name"`
	Definition Definition `json:"definition"`
}

func (s *Service) createApp(ctx context.Context, sp access.Space, a Actor, na NewApp) (App, error) {
	if err := may(sp, access.Write); err != nil {
		return App{}, err
	}
	name, err := appName(na.Name)
	if err != nil {
		return App{}, err
	}
	def, err := na.Definition.Check()
	if err != nil {
		return App{}, err
	}
	raw, err := json.Marshal(def)
	if err != nil {
		return App{}, err
	}
	var id int64
	var created App
	err = s.write(ctx, func(tx *sql.Tx) error {
		if err := s.recheck(ctx, a, sp.ID); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM apps WHERE space_id = ?`, sp.ID).Scan(&n); err != nil {
			return err
		}
		if n >= maxApps {
			return errTooManyApps
		}
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO apps (space_id, name, definition,
				created_by, created_by_agent, created_via, updated_by, updated_by_agent, updated_via)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			sp.ID, name, string(raw), a.UserID, a.Agent, via(a), a.UserID, a.Agent, via(a)).Scan(&id); err != nil {
			return err
		}
		created, err = app(ctx, tx, sp.ID, id) // as this change left it, not as a later one did
		return err
	})
	if err != nil {
		return App{}, err
	}
	return created, nil
}

// Changes is an update of an app. Nil fields stay as they are.
type Changes struct {
	// Version is the app's version the change was made against.
	Version int64   `json:"version"`
	Name    *string `json:"name"`
	// Archived takes the app offline (true) or brings it back (false).
	Archived *bool `json:"archived"`
	// Definition replaces the app's screen, collection, actions and summaries. Its rows
	// must fit the new collection.
	Definition *Definition `json:"definition"`
}

func (s *Service) updateApp(ctx context.Context, sp access.Space, a Actor, appID int64, ch Changes) (App, error) {
	if err := may(sp, access.Write); err != nil {
		return App{}, err
	}
	if ch.Name == nil && ch.Archived == nil && ch.Definition == nil {
		return App{}, invalidf("name, archived or definition is required")
	}
	if err := checkVersion(ch.Version); err != nil {
		return App{}, err
	}
	var name, def any // nil: unchanged
	if ch.Name != nil {
		n, err := appName(*ch.Name)
		if err != nil {
			return App{}, err
		}
		name = n
	}
	var checked Definition
	if ch.Definition != nil {
		var err error
		if checked, err = ch.Definition.Check(); err != nil {
			return App{}, err
		}
		raw, err := json.Marshal(checked)
		if err != nil {
			return App{}, err
		}
		def = string(raw)
	}
	var archivedAt any // nil: unchanged; "": restored
	if ch.Archived != nil {
		archivedAt = ""
		if *ch.Archived {
			archivedAt = now()
		}
	}
	var updated App
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := s.recheck(ctx, a, sp.ID); err != nil {
			return err
		}
		cur, err := app(ctx, tx, sp.ID, appID)
		if err != nil {
			return err
		}
		if cur.Version != ch.Version {
			return ErrStale
		}
		if ch.Definition != nil {
			if err := rowsFit(ctx, tx, appID, checked.Collection); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE apps SET
				name = coalesce(?, name),
				definition = coalesce(?, definition),
				archived_at = CASE WHEN ? IS NULL THEN archived_at
				                   WHEN ? = '' THEN NULL
				                   ELSE coalesce(archived_at, ?) END,
				version = version + 1, updated_at = ?, updated_by = ?, updated_by_agent = ?, updated_via = ?
			WHERE id = ?`, name, def, archivedAt, archivedAt, archivedAt, now(), a.UserID, a.Agent, via(a), appID)
		if err != nil {
			return err
		}
		updated, err = app(ctx, tx, sp.ID, appID) // as this change left it, not as a later one did
		return err
	})
	if err != nil {
		return App{}, err
	}
	return updated, nil
}

// rowsFit checks every row of appID against a new collection, so changing an app's
// definition never leaves rows its screen and actions can't handle.
func rowsFit(ctx context.Context, q queryer, appID int64, c Collection) error {
	rows, err := loadRows(ctx, q, appID)
	if err != nil {
		return err
	}
	var bad []string
	for _, r := range rows {
		if err := c.Fits(r.Data); err != nil {
			bad = append(bad, fmt.Sprintf("row %d: %v", r.ID, err))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	if len(bad) > 5 {
		bad = append(bad[:5], fmt.Sprintf("and %d more", len(bad)-5))
	}
	return invalidf("%d of the app's rows don't fit the new collection (make the field optional, or keep its type): %s",
		len(bad), strings.Join(bad, "; "))
}

// moveApp moves an app to another space of the same workspace, with its rows: the caller
// must be able to write both.
func (s *Service) moveApp(ctx context.Context, sp access.Space, a Actor, appID, version int64, to access.Space) (App, error) {
	if err := may(sp, access.Write); err != nil {
		return App{}, err
	}
	if err := checkVersion(version); err != nil {
		return App{}, err
	}
	if to.WorkspaceID != sp.WorkspaceID {
		return App{}, ErrNotFound
	}
	if err := may(to, access.Write); err != nil {
		return App{}, err
	}
	var moved App
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := s.recheck(ctx, a, sp.ID, to.ID); err != nil {
			return err
		}
		cur, err := app(ctx, tx, sp.ID, appID)
		if err != nil {
			return err
		}
		if cur.Version != version {
			return ErrStale
		}
		// Once the app is known to be there (an unknown one is a 404 first): a move to where it
		// is would change nothing but its version.
		if to.ID == sp.ID {
			return invalidf("the app is already in that space")
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM apps WHERE space_id = ?`, to.ID).Scan(&n); err != nil {
			return err
		}
		if n >= maxApps {
			return errTooManyApps
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE apps SET space_id = ?, version = version + 1, updated_at = ?, updated_by = ?, updated_by_agent = ?, updated_via = ?
			WHERE id = ?`, to.ID, now(), a.UserID, a.Agent, via(a), appID)
		if err != nil {
			return err
		}
		moved, err = app(ctx, tx, to.ID, appID) // as this move left it, not as a later change did
		return err
	})
	if err != nil {
		return App{}, err
	}
	return moved, nil
}

// loadRows returns an app's rows, oldest first.
func loadRows(ctx context.Context, q queryer, appID int64) ([]Row, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, data FROM app_rows WHERE app_id = ? ORDER BY id`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := []Row{}
	for rows.Next() {
		var r Row
		var data string
		if err := rows.Scan(&r.ID, &data); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(data), &r.Data); err != nil {
			return nil, fmt.Errorf("app %d row %d: %w", appID, r.ID, err)
		}
		all = append(all, r)
	}
	return all, rows.Err()
}

// launchable returns an app with its definition, refusing one that is offline.
func launchable(ctx context.Context, q queryer, spaceID, appID int64) (App, error) {
	a, err := app(ctx, q, spaceID, appID)
	if err == nil && a.ArchivedAt != nil {
		return App{}, ErrOffline
	}
	return a, err
}

// surface returns the messages that draw an app with its rows.
func (s *Service) surface(ctx context.Context, sp access.Space, appID int64) ([]Message, error) {
	if err := may(sp, access.Read); err != nil {
		return nil, err
	}
	a, err := launchable(ctx, s.db, sp.ID, appID)
	if err != nil {
		return nil, err
	}
	rows, err := loadRows(ctx, s.db, appID)
	if err != nil {
		return nil, err
	}
	return Surface(SurfaceID(a.ID), *a.Definition, rows), nil
}

// act runs a client action and returns the message that updates the screen. Errors in
// the action itself (an unknown action or row, a value that doesn't fit) match
// ErrInvalid.
func (s *Service) act(ctx context.Context, sp access.Space, actor Actor, appID int64, action ClientAction) ([]Message, error) {
	if err := may(sp, access.Write); err != nil {
		return nil, err
	}
	var def Definition
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := s.recheck(ctx, actor, sp.ID); err != nil {
			return err
		}
		a, err := launchable(ctx, tx, sp.ID, appID)
		if err != nil {
			return err
		}
		def = *a.Definition
		ch, err := Plan(SurfaceID(a.ID), def, action)
		if err != nil {
			return err
		}
		return apply(ctx, tx, actor, appID, def, ch)
	})
	if err != nil {
		return nil, err
	}
	// The screen's data is read after the change commits, outside the write lock, which every
	// write on the server waits for: an app's rows can be up to MaxDataSize, and the next
	// writer needn't wait while they are read and encoded. It shows the latest rows, a change
	// committed meanwhile included.
	rows, err := loadRows(ctx, s.db, appID)
	if err != nil {
		return nil, err
	}
	return []Message{DataModel(SurfaceID(appID), def, rows)}, nil
}

// apply makes a change to an app's rows, within its bounds.
func apply(ctx context.Context, tx *sql.Tx, actor Actor, appID int64, def Definition, ch Change) error {
	var count, size int
	// In bytes, as len() counts below: length() of TEXT counts characters, which would let
	// non-ASCII rows hold more than MaxDataSize.
	if err := tx.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(length(CAST(data AS BLOB))), 0) FROM app_rows WHERE app_id = ?`, appID).
		Scan(&count, &size); err != nil {
		return err
	}
	switch ch.Kind {
	case ActionInsert:
		raw, err := json.Marshal(ch.Data)
		if err != nil {
			return err
		}
		if count >= MaxRows || size+len(raw) > MaxDataSize {
			return appFull(fmt.Sprintf("an app holds at most %d rows and %d MiB", MaxRows, MaxDataSize>>20))
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO app_rows (app_id, data, created_by, updated_by) VALUES (?, ?, ?, ?)`,
			appID, string(raw), actor.UserID, actor.UserID)
		return err
	case ActionUpdate:
		var data string
		err := tx.QueryRowContext(ctx, `SELECT data FROM app_rows WHERE id = ? AND app_id = ?`, ch.ID, appID).Scan(&data)
		if errors.Is(err, sql.ErrNoRows) {
			return invalidAs(apierr.CodeAppRowNotFound, nil, "the app has no row %d", ch.ID)
		}
		if err != nil {
			return err
		}
		var cur map[string]any
		if err := json.Unmarshal([]byte(data), &cur); err != nil {
			return err
		}
		next, err := def.Collection.Merge(cur, ch.Data)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if size-len(data)+len(raw) > MaxDataSize {
			return appFull(fmt.Sprintf("an app holds at most %d MiB", MaxDataSize>>20))
		}
		_, err = tx.ExecContext(ctx, `UPDATE app_rows SET data = ?, updated_at = ?, updated_by = ? WHERE id = ?`,
			string(raw), now(), actor.UserID, ch.ID)
		return err
	case ActionDelete:
		res, err := tx.ExecContext(ctx, `DELETE FROM app_rows WHERE id = ? AND app_id = ?`, ch.ID, appID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return invalidAs(apierr.CodeAppRowNotFound, nil, "the app has no row %d", ch.ID)
		}
		return nil
	}
	return invalidf("unknown kind of action %q", ch.Kind)
}

// checkVersion refuses a missing version: versions start at 1.
func checkVersion(v int64) error {
	if v < 1 {
		return invalidf("version is required: the version you last read")
	}
	return nil
}

func appName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if n := utf8.RuneCountInString(s); n < 1 || n > maxNameLen {
		return "", invalidAs(apierr.CodeAppNameLength, apierr.Params{"max": maxNameLen}, "an app's name is 1 to %d characters", maxNameLen)
	}
	return s, nil
}
