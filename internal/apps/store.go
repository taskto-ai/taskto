package apps

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/quota"
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

// App is an app as a list shows it; Live and Draft carry definitions when one app is read.
//
// An app has a draft, the definition people with the design capability edit, and releases,
// immutable copies of drafts that were published (release.go). It serves its live release:
// editing never changes what people use, publishing does.
type App struct {
	ID      int64  `json:"id"`
	SpaceID int64  `json:"space_id"`
	Name    string `json:"name"`
	// ArchivedAt is when the app was taken offline, or nil.
	ArchivedAt *string `json:"archived_at"`
	// Version is the app's version for optimistic changes: bumped by every change to the
	// app, the draft's, a publish and a rollback included.
	Version int64 `json:"version"`
	// Rows is how many rows its collection has that the caller may read: all of them, only
	// their own, or none (see Access.ReadRecords).
	Rows      int     `json:"rows"`
	CreatedAt string  `json:"created_at"`
	CreatedBy *Author `json:"created_by"`
	UpdatedAt string  `json:"updated_at"`
	UpdatedBy *Author `json:"updated_by"`
	// Access is what the caller may do in the app: their roles and capabilities.
	Access access.AppAccess `json:"access"`
	// Live is the release the app serves, or nil before its first publish (it can't be
	// launched). Its Definition is in a single app's reply.
	Live *Release `json:"live"`
	// UnpublishedChanges is whether the draft is not the live release's definition (it is
	// always so before the first publish). Only callers who may design or publish are told.
	UnpublishedChanges bool `json:"unpublished_changes"`
	// NeedsBoard is whether the draft has effects with no board chosen yet, which can't be
	// published until one is (a template's, or a copy's whose boards its copier can't write).
	// Only callers who may design or publish are told.
	NeedsBoard bool `json:"needs_board"`
	// Draft is the definition being edited, in a single app's reply, for callers who may design
	// or publish: others see the live release's definition alone.
	Draft *Definition `json:"draft,omitempty"`
	// EffectTargets names the spaces and boards the draft's effects act on, as far as the caller may
	// read them, so what the publish panel says about them can say which ones (effects_api.go). It
	// comes with Draft, for the same callers.
	EffectTargets []EffectTarget `json:"effect_targets,omitempty"`

	// What the store read, for the service's own use: the draft, the live release's definition
	// and whether they differ. They reach a reply only through present, by the caller's access.
	draftDef     Definition
	liveDef      *Definition
	draftDiffers bool
	detailed     bool
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
	// ErrConflict is for an update, delete or restore of a record that has changed since
	// the caller read it (or, for a restore, isn't deleted).
	ErrConflict = errors.New("the record changed since you read it")
	// ErrFull is for a row that would take an app over MaxRows or MaxDataSize, or an app
	// that would take a space over maxApps.
	ErrFull = errors.New("full")
	// ErrForbidden is for something the caller's roles in the app don't allow, though the
	// space does: a user who may submit records may not change the app, for instance.
	ErrForbidden = errors.New("your role in this app doesn't allow this")
)

// forbiddenError is ErrForbidden, saying which capability the caller lacks.
type forbiddenError struct{ capability string }

func (e *forbiddenError) Error() string        { return ErrForbidden.Error() + " (" + e.capability + ")" }
func (e *forbiddenError) Is(target error) bool { return target == ErrForbidden }

func forbidden(capability string) error { return &forbiddenError{capability: capability} }

// records names the capability of reading or changing the app's records, for ErrForbidden.
const (
	capReadRecords   = "read_records"
	capModifyRecords = "modify_records"
)

// need checks that acc has capability c.
func need(acc access.AppAccess, c access.Capability) error {
	if !acc.Has(c) {
		return forbidden(string(c))
	}
	return nil
}

// visibility says which of an app's rows a caller reads: all of them, those they created, or
// none. Every way to read rows goes through it, so what a screen shows, its summaries, the
// counts in lists and the agent's reads agree.
type visibility struct {
	scope access.Scope
	user  int64
}

func readable(acc access.AppAccess, userID int64) visibility {
	return visibility{scope: acc.ReadRecords, user: userID}
}

// everything is the view of the server's own checks, which read every row.
var everything = visibility{scope: access.ScopeAll}

// sees reports whether the caller reads a row created by creator (nil: its creator's
// account is gone, which only a caller who reads all rows sees).
func (v visibility) sees(creator sql.NullInt64) bool {
	switch v.scope {
	case access.ScopeAll:
		return true
	case access.ScopeOwn:
		return creator.Valid && creator.Int64 == v.user
	}
	return false
}

// conflictError is ErrConflict for record id. Current is the record as it is now, as an
// item, only if the caller may read it: a refusal gives away nothing they couldn't see.
type conflictError struct {
	id      int64
	current map[string]any
}

func (e *conflictError) Error() string        { return ErrConflict.Error() }
func (e *conflictError) Is(target error) bool { return target == ErrConflict }

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
	// platform is what the app's effects act on (outbox.go).
	platform Platform
	// clock is the time the outbox's worker goes by: the real one, a test's own otherwise.
	clock func() time.Time
	// wake tells the worker, if it runs in this process, that an entry was queued.
	wake  chan struct{}
	hooks outboxHooks
	// dataDir holds the spaces' directories, where attachments are kept (WithFiles); empty if
	// the service keeps no files.
	dataDir string
	locks   fileLocks
	thumbs  chan struct{}
	// quota refuses an upload that would take its space or workspace over a storage quota
	// (WithQuota); nil for none.
	quota *quota.Service
}

// outboxHooks are for tests to stop the worker where a crash would.
type outboxHooks struct {
	// afterWork is called once an entry's work is done and before it is noted as done.
	afterWork func(effectRow) error
	// beforeDocument is called once a document entry has passed its checks, right before it asks
	// for the space's write lock to commit.
	beforeDocument func(effectRow)
	// afterActionsAccess is called by Actions once it has read what the user may do, before it
	// reads the release.
	afterActionsAccess func()
}

// New returns the apps service over sqlDB, which must be migrated.
func New(sqlDB *sql.DB, c *access.Checker) *Service {
	return &Service{db: sqlDB, access: c, clock: time.Now, wake: make(chan struct{}, 1), thumbs: make(chan struct{}, maxThumbnails)}
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

// appAccess returns what a may do in appID of spaceID now: ErrNotFound if they can't read
// the space, it has no such app, or they hold no role in it (an app restricted to others is
// as unseen as one of a space they can't open). It asks package access, which answers for
// requests and the agent alike.
func (s *Service) appAccess(ctx context.Context, a Actor, spaceID, appID int64) (access.AppAccess, error) {
	acc, err := s.access.App(ctx, a.UserID, spaceID, appID)
	if errors.Is(err, access.ErrNotFound) || errors.Is(err, access.ErrAppNotFound) {
		return access.AppAccess{}, ErrNotFound
	}
	if err == nil && !acc.View {
		return access.AppAccess{}, ErrNotFound
	}
	return acc, err
}

// recheckApp is recheck for one app: a may still write its space, and what they may do in it
// is returned, as of now.
func (s *Service) recheckApp(ctx context.Context, a Actor, spaceID, appID int64) (access.AppAccess, error) {
	if err := s.recheck(ctx, a, spaceID); err != nil {
		return access.AppAccess{}, err
	}
	return s.appAccess(ctx, a, spaceID, appID)
}

// present fills in what only the caller's access decides about an app they were handed: what
// they may do in it, and how many of its rows they may read.
func (s *Service) present(ctx context.Context, a Actor, ap *App) error {
	acc, err := s.appAccess(ctx, a, ap.SpaceID, ap.ID)
	if err != nil {
		return err
	}
	ap.Access = acc
	ap.finish(acc)
	ap.Rows, err = countRows(ctx, s.db, ap.ID, readable(acc, a.UserID))
	if err != nil {
		return err
	}
	if ap.Draft != nil {
		ap.EffectTargets, err = s.effectTargets(ctx, a, ap.SpaceID, ap.Draft)
	}
	return err
}

// reviews is whether acc may see what is not live yet: the draft and the releases before it.
func reviews(acc access.AppAccess) bool { return acc.Design || acc.Publish }

// finish puts in a reply what the caller's access lets them know of the app's drafts: the
// draft and whether it differs only for those who may design or publish, the definition of
// the live release for everyone who may view the app.
func (a *App) finish(acc access.AppAccess) {
	a.UnpublishedChanges = reviews(acc) && a.draftDiffers
	a.NeedsBoard = reviews(acc) && len(a.draftDef.unsetTargets()) > 0
	if !a.detailed {
		return
	}
	if reviews(acc) {
		d := a.draftDef
		a.Draft = &d
	}
	if a.Live != nil && a.liveDef != nil {
		d := *a.liveDef
		a.Live.Definition = &d
	}
}

// countRows counts the live rows of appID that v reads: deleted ones are not rows.
func countRows(ctx context.Context, q queryer, appID int64, v visibility) (int, error) {
	var n int
	switch v.scope {
	case access.ScopeAll:
		err := q.QueryRowContext(ctx, `SELECT count(*) FROM app_rows WHERE app_id = ? AND deleted_at IS NULL`, appID).Scan(&n)
		return n, err
	case access.ScopeOwn:
		err := q.QueryRowContext(ctx, `SELECT count(*) FROM app_rows WHERE app_id = ? AND deleted_at IS NULL AND created_by = ?`, appID, v.user).Scan(&n)
		return n, err
	}
	return 0, nil
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

// appColumns selects what scanApp reads, from apps a; appListColumns the same with empty
// definitions in their place, so a listing of up to maxApps apps doesn't read each one's (up to
// 48 KiB, twice) only to throw them away.
const (
	appFields = `a.id, a.space_id, a.name, a.archived_at, a.version,
		a.created_at, a.created_by, coalesce(cu.name, ''), a.created_by_agent, a.created_via,
		a.updated_at, a.updated_by, coalesce(uu.name, ''), a.updated_by_agent, a.updated_via,
		a.draft_differs, r.number, r.definition_version, r.requires, r.published_at, r.published_by, coalesce(pu.name, ''), a.live_at, `
	appFrom = `
		FROM apps a
		LEFT JOIN users cu ON cu.id = a.created_by
		LEFT JOIN users uu ON uu.id = a.updated_by
		LEFT JOIN app_releases r ON r.app_id = a.id AND r.number = a.live_release
		LEFT JOIN users pu ON pu.id = r.published_by`
	appColumns     = appFields + `a.definition, coalesce(r.definition, '')` + appFrom
	appListColumns = appFields + `'', ''` + appFrom
)

// scanApp reads an app; withDefinition decodes its draft and live definitions too.
func scanApp(row scanner, withDefinition bool) (App, error) {
	var a App
	var createdBy, updatedBy, relNumber, relVersion, relBy sql.NullInt64
	var createdName, updatedName, relByName, draft, live string
	var relRequires, relAt, liveAt sql.NullString
	var createdAgent, updatedAgent bool
	var createdVia, updatedVia sql.NullString
	if err := row.Scan(&a.ID, &a.SpaceID, &a.Name, &a.ArchivedAt, &a.Version,
		&a.CreatedAt, &createdBy, &createdName, &createdAgent, &createdVia,
		&a.UpdatedAt, &updatedBy, &updatedName, &updatedAgent, &updatedVia,
		&a.draftDiffers, &relNumber, &relVersion, &relRequires, &relAt, &relBy, &relByName, &liveAt, &draft, &live); err != nil {
		return App{}, err
	}
	a.CreatedBy = author(createdBy, createdName, createdAgent, createdVia)
	a.UpdatedBy = author(updatedBy, updatedName, updatedAgent, updatedVia)
	if relNumber.Valid {
		rel, err := newRelease(relNumber.Int64, int(relVersion.Int64), relRequires.String, relAt.String, relBy, relByName)
		if err != nil {
			return App{}, fmt.Errorf("app %d: %w", a.ID, err)
		}
		rel.Live, rel.LiveSince = true, liveAt.String
		a.Live = &rel
	}
	if withDefinition {
		d, err := decodeDefinition(draft, 0, nil)
		if err != nil {
			return App{}, fmt.Errorf("app %d: stored draft: %w", a.ID, err)
		}
		a.draftDef, a.detailed = d, true
		if a.Live != nil {
			l, err := decodeDefinition(live, a.Live.DefinitionVersion, a.Live.Requires)
			if err != nil {
				return App{}, fmt.Errorf("app %d: stored release %d: %w", a.ID, a.Live.Number, err)
			}
			a.liveDef = &l
		}
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

func (s *Service) listApps(ctx context.Context, sp access.Space, a Actor, withArchived bool) ([]App, error) {
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
		ap, err := scanApp(rows, false)
		if err != nil {
			return nil, err
		}
		all = append(all, ap)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// What the caller may do in each app, and so how many of its rows they may count: a
	// total over rows they can't read would tell them what those hold.
	accs, err := s.access.AppsIn(ctx, a.UserID, sp)
	if err != nil {
		return nil, err
	}
	counts, err := s.rowCounts(ctx, sp.ID, a.UserID)
	if err != nil {
		return nil, err
	}
	// Apps the caller holds no role in are left out, as appAccess answers them ErrNotFound.
	visible := all[:0]
	for _, ap := range all {
		if accs[ap.ID].View {
			visible = append(visible, ap)
		}
	}
	all = visible
	for i := range all {
		acc := accs[all[i].ID]
		all[i].Access = acc
		all[i].finish(acc)
		switch acc.ReadRecords {
		case access.ScopeAll:
			all[i].Rows = counts[all[i].ID].all
		case access.ScopeOwn:
			all[i].Rows = counts[all[i].ID].own
		}
	}
	return all, nil
}

type rowCount struct{ all, own int }

// rowCounts counts each app of spaceID's rows: all of them, and those userID created.
func (s *Service) rowCounts(ctx context.Context, spaceID, userID int64) (map[int64]rowCount, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.app_id, count(*), coalesce(sum(r.created_by = ?), 0)
		FROM app_rows r JOIN apps a ON a.id = r.app_id
		WHERE a.space_id = ? AND r.deleted_at IS NULL GROUP BY r.app_id`, userID, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[int64]rowCount{}
	for rows.Next() {
		var id int64
		var c rowCount
		if err := rows.Scan(&id, &c.all, &c.own); err != nil {
			return nil, err
		}
		counts[id] = c
	}
	return counts, rows.Err()
}

func (s *Service) getApp(ctx context.Context, sp access.Space, a Actor, appID int64) (App, error) {
	if err := may(sp, access.Read); err != nil {
		return App{}, err
	}
	ap, err := app(ctx, s.db, sp.ID, appID)
	if err != nil {
		return App{}, err
	}
	if err := s.present(ctx, a, &ap); err != nil {
		return App{}, err
	}
	return ap, nil
}

// NewApp is a new app: its name and its first draft. It has no release, so it can't be
// launched, until someone who may publish it does.
type NewApp struct {
	Name       string     `json:"name"`
	Definition Definition `json:"definition"`
}

func (s *Service) createApp(ctx context.Context, sp access.Space, a Actor, na NewApp) (App, error) {
	return s.createAppWith(ctx, sp, a, na, nil)
}

// createAppWith is createApp that also makes the app's first role assignments, in the same
// transaction as the app, so there is no moment the app exists without them.
func (s *Service) createAppWith(ctx context.Context, sp access.Space, a Actor, na NewApp, grants []access.AppGrant) (App, error) {
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
	if err := s.checkEffectTargets(ctx, sp.WorkspaceID, a, def); err != nil {
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
			INSERT INTO apps (space_id, name, definition, draft_differs,
				created_by, created_by_agent, created_via, updated_by, updated_by_agent, updated_via)
			VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?, ?) RETURNING id`,
			sp.ID, name, string(raw), a.UserID, a.Agent, via(a), a.UserID, a.Agent, via(a)).Scan(&id); err != nil {
			return err
		}
		for _, g := range grants {
			for _, r := range g.Roles {
				if _, err := tx.ExecContext(ctx, `INSERT INTO app_roles (app_id, user_id, role) VALUES (?, ?, ?)`, id, g.UserID, r); err != nil {
					return err
				}
			}
		}
		created, err = app(ctx, tx, sp.ID, id) // as this change left it, not as a later one did
		return err
	})
	if err != nil {
		return App{}, err
	}
	if err := s.present(ctx, a, &created); err != nil {
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
	// Definition replaces the app's DRAFT: the screen, collection, actions and summaries
	// builders edit. It is checked against the catalog, not against the rows, and changes
	// nothing people use until it is published (Publish, which checks the rows).
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
		if err := s.checkEffectTargets(ctx, sp.WorkspaceID, a, checked); err != nil {
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
		acc, err := s.recheckApp(ctx, a, sp.ID, appID)
		if err != nil {
			return err
		}
		// Changing what the app is, and taking it offline or back, are different rights.
		if ch.Name != nil || ch.Definition != nil {
			if err := need(acc, access.CapDesign); err != nil {
				return err
			}
		}
		if ch.Archived != nil {
			if err := need(acc, access.CapPublish); err != nil {
				return err
			}
		}
		cur, err := app(ctx, tx, sp.ID, appID)
		if err != nil {
			return err
		}
		if cur.Version != ch.Version {
			return ErrStale
		}
		// Only the draft changes: the live release, and so what people use, stays as it is.
		// Whether the draft still differs from it is kept for the lists.
		var differs any // nil: unchanged
		if ch.Definition != nil {
			differs = cur.liveDef == nil || !sameDefinition(*cur.liveDef, checked)
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE apps SET
				name = coalesce(?, name),
				definition = coalesce(?, definition),
				draft_differs = coalesce(?, draft_differs),
				archived_at = CASE WHEN ? IS NULL THEN archived_at
				                   WHEN ? = '' THEN NULL
				                   ELSE coalesce(archived_at, ?) END,
				version = version + 1, updated_at = ?, updated_by = ?, updated_by_agent = ?, updated_via = ?
			WHERE id = ?`, name, def, differs, archivedAt, archivedAt, archivedAt, now(), a.UserID, a.Agent, via(a), appID)
		if err != nil {
			return err
		}
		updated, err = app(ctx, tx, sp.ID, appID) // as this change left it, not as a later one did
		return err
	})
	if err != nil {
		return App{}, err
	}
	if err := s.present(ctx, a, &updated); err != nil {
		return App{}, err
	}
	return updated, nil
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
		acc, err := s.recheckApp(ctx, a, sp.ID, appID)
		if err != nil {
			return err
		}
		// Moving an app changes who can reach it: it is the builder's to do.
		if err := need(acc, access.CapDesign); err != nil {
			return err
		}
		if err := s.recheck(ctx, a, to.ID); err != nil {
			return err
		}
		cur, err := app(ctx, tx, sp.ID, appID)
		if err != nil {
			return err
		}
		if cur.Version != version {
			return ErrStale
		}
		// What the space's publishers set as defaults for its members stays in the space: the
		// app's new members never chose it.
		if _, err := tx.ExecContext(ctx, `DELETE FROM home_defaults WHERE app_id = ?`, appID); err != nil {
			return err
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
	// The files follow the app to its new space. The app already works meanwhile, and if this
	// stops half way Sweep goes on (attachments.go).
	if err := s.relocate(ctx, appID); err != nil {
		slog.Warn("apps: moving an app's attachments", "app", appID, "err", err)
	}
	if err := s.present(ctx, a, &moved); err != nil {
		return App{}, err
	}
	return moved, nil
}

// loadRows returns the live rows of an app that v reads, oldest first: deleted ones are
// left out, so they are in no screen, summary or check.
func loadRows(ctx context.Context, q queryer, appID int64, v visibility) ([]Row, error) {
	var rows *sql.Rows
	var err error
	switch v.scope {
	case access.ScopeAll:
		rows, err = q.QueryContext(ctx, `SELECT id, revision, data FROM app_rows WHERE app_id = ? AND deleted_at IS NULL ORDER BY id`, appID)
	case access.ScopeOwn:
		rows, err = q.QueryContext(ctx, `SELECT id, revision, data FROM app_rows WHERE app_id = ? AND deleted_at IS NULL AND created_by = ? ORDER BY id`, appID, v.user)
	default:
		return []Row{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := []Row{}
	for rows.Next() {
		var r Row
		var data string
		if err := rows.Scan(&r.ID, &r.Revision, &data); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(data), &r.Data); err != nil {
			return nil, fmt.Errorf("app %d row %d: %w", appID, r.ID, err)
		}
		all = append(all, r)
	}
	return all, rows.Err()
}

// loadSpaceRows is loadRows for an app that must be in space spaceID: the same statement checks
// it, so rows of an app that moved to another space are not read as this space's.
func loadSpaceRows(ctx context.Context, q queryer, spaceID, appID int64, v visibility) ([]Row, error) {
	var rows *sql.Rows
	var err error
	const from = `SELECT r.id, r.revision, r.data FROM app_rows r JOIN apps a ON a.id = r.app_id
		WHERE r.app_id = ? AND a.space_id = ? AND r.deleted_at IS NULL`
	switch v.scope {
	case access.ScopeAll:
		rows, err = q.QueryContext(ctx, from+` ORDER BY r.id`, appID, spaceID)
	case access.ScopeOwn:
		rows, err = q.QueryContext(ctx, from+` AND r.created_by = ? ORDER BY r.id`, appID, spaceID, v.user)
	default:
		return []Row{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := []Row{}
	for rows.Next() {
		var r Row
		var data string
		if err := rows.Scan(&r.ID, &r.Revision, &data); err != nil {
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

// surface returns the messages that draw an app with its rows. which is the screen: "" for
// the live release (ErrNotPublished before the first publish), "draft" for the draft, or a
// release number; the draft and releases that are not live are for those who may design or
// publish. Every screen is drawn with the same rows, those the caller reads.
//
// screen, for an app with screens, is the screen it opens on instead of its entry (what a pin
// that names one asks for); a name that isn't one of the screens that don't show a record is
// refused.
func (s *Service) surface(ctx context.Context, sp access.Space, actor Actor, appID int64, which, screen string) ([]Message, error) {
	if err := may(sp, access.Read); err != nil {
		return nil, err
	}
	acc, err := s.appAccess(ctx, actor, sp.ID, appID)
	if err != nil {
		return nil, err
	}
	if err := need(acc, access.CapView); err != nil {
		return nil, err
	}
	a, err := launchable(ctx, s.db, sp.ID, appID)
	if err != nil {
		return nil, err
	}
	def, err := screenOf(ctx, s.db, a, acc, which)
	if err != nil {
		return nil, err
	}
	// The screen's rows, and with them its summaries, are those the caller reads.
	rows, err := loadRows(ctx, s.db, appID, readable(acc, actor.UserID))
	if err != nil {
		return nil, err
	}
	if screen != "" {
		target, ok := def.screen(screen)
		if !ok || target.Record {
			return nil, invalidf("screen must name one of the app's screens that doesn't show a record")
		}
	}
	return openedAt(Surface(SurfaceID(a.ID), def, rows), SurfaceID(a.ID), def, rows, screen), nil
}

// act runs a client action and returns the message that updates the screen. Errors in
// the action itself (an unknown action or row, a value that doesn't fit) match
// ErrInvalid.
//
// An action names the release its screen was drawn from (ClientAction.Release; left out, the
// live one). From the live release it is run as the live definition says. From another one,
// older because someone published or rolled back since the screen was drawn, it is run as that
// release defines it, and applied only if the live release can take it as it is: the live
// release declares the same action, and every field the action sets is one the live collection
// declares, of the same type. The row it would write must still fit the live collection (a
// field the live release requires and that release's screen doesn't send is the usual
// reason: the client gets the field's own error). Otherwise nothing changes and the answer
// is ErrReleaseOutdated, which tells the client to reload the app. A release the app doesn't
// have is ErrReleaseNotFound, but only to a caller who may run actions at all. The data model
// sent back is drawn by the release the action came from, limited to the fields the live
// release declares (shownBy): nothing the live app leaves unseen is returned.
func (s *Service) act(ctx context.Context, sp access.Space, actor Actor, appID int64, action ClientAction) ([]Message, error) {
	msgs, err := s.actOnce(ctx, sp, actor, appID, action)
	// A request this caller made before, under the same operation and release, and that committed,
	// is answered with its receipt even when what they may do has changed since: a client that
	// sent it and lost the answer (a device that was offline) must be able to learn that it is
	// in, or a refusal of the access they have now would leave it not knowing whether it is. The
	// receipt is no data: the rows are the caller's to read, and aren't sent.
	//
	// An app archived since is the same: the change is in, and the device learns it from the receipt
	// rather than from a refusal that says nothing of it (ErrOffline).
	//
	// The receipt answers on the route it was made on only: the space and the app of the route are
	// the ones it was made in (a caller who lost their access to that space does not get it through
	// any other space they can reach).
	if err != nil &&
		(errors.Is(err, ErrNotFound) || errors.Is(err, ErrReadOnly) || errors.Is(err, ErrForbidden) || errors.Is(err, ErrOffline)) &&
		s.committed(ctx, sp.ID, actor, appID, action) {
		return []Message{}, nil
	}
	return msgs, err
}

// committed reports whether the operation the action names was made by actor, in this app and
// this space, under the same request (the release it names, the action and its context): it is
// recorded in the transaction of the change it made, so a record of it is a change that committed.
func (s *Service) committed(ctx context.Context, spaceID int64, actor Actor, appID int64, action ClientAction) bool {
	if action.Operation == "" || action.Release == nil || !operationPattern.MatchString(action.Operation) {
		return false
	}
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT o.request_hash FROM app_operations o
		JOIN apps a ON a.id = o.app_id
		WHERE a.id = ? AND a.space_id = ? AND o.user_id = ? AND o.operation = ?`,
		appID, spaceID, actor.UserID, action.Operation).Scan(&hash)
	return err == nil && hash == requestHash(*action.Release, action)
}

func (s *Service) actOnce(ctx context.Context, sp access.Space, actor Actor, appID int64, action ClientAction) ([]Message, error) {
	if err := may(sp, access.Read); err != nil {
		return nil, err
	}
	if s.navigates(ctx, sp, actor, appID, action) {
		return s.navigate(ctx, sp, actor, appID, action)
	}
	if err := may(sp, access.Write); err != nil {
		return nil, err
	}
	done, err := s.runChange(ctx, sp, actor, appID, action)
	if err != nil {
		return nil, err
	}
	// The screen's data is read after the change commits, outside the write lock, which every
	// write on the server waits for: an app's rows can be up to MaxDataSize, and the next
	// writer needn't wait while they are read and encoded. It shows the latest rows, a change
	// committed meanwhile included.
	// The answer carries the rows, and summaries, the caller reads: a user who adds a record
	// to an app where they read only their own is not shown anyone else's.
	rows, err := loadRows(ctx, s.db, appID, readable(done.acc, actor.UserID))
	if err != nil {
		return nil, err
	}
	return answer(appID, shownBy(done.live, done.def), rows, done.change), nil
}

// changeDone is what a change action did: the access the caller had when it ran, the live release's
// definition and the one the action came from (the same unless it came from an older release),
// the change planned and what it did to the record.
type changeDone struct {
	acc       access.AppAccess
	def, live Definition
	change    Change
	ap        applied
	// replayed is a request made before under its operation key: nothing was done again.
	replayed bool
	// effects is how many effects the change queued.
	effects int
}

// runChange runs a client action that changes a record, in one transaction: the caller's rights are
// checked again inside it, the change planned from the action and applied, the operation noted and
// the action's effects queued. It is the whole of what an action does, for the web app and for the
// agent alike. The caller has checked the space and that the action is not a navigation.
func (s *Service) runChange(ctx context.Context, sp access.Space, actor Actor, appID int64, action ClientAction) (changeDone, error) {
	var out changeDone
	queued := false
	err := s.write(ctx, func(tx *sql.Tx) error {
		var err error
		if out.acc, err = s.recheckApp(ctx, actor, sp.ID, appID); err != nil {
			return err
		}
		acc := out.acc
		a, err := launchable(ctx, tx, sp.ID, appID)
		if err != nil {
			return err
		}
		if a.liveDef == nil {
			return ErrNotPublished
		}
		live := *a.liveDef
		def := live
		fromOlder := action.Release != nil && *action.Release != a.Live.Number
		if fromOlder {
			// Whether a release exists is for those who may run actions at all: the 404 for one
			// that doesn't must not reach a caller the app refuses (what the action does is
			// checked below against the right it needs).
			if !acc.Submit && acc.ModifyRecords == access.ScopeNone {
				return forbidden(string(access.CapSubmit))
			}
			rel, err := loadRelease(ctx, tx, appID, *action.Release, true)
			if err != nil {
				if action.Exact && errors.Is(err, ErrReleaseNotFound) {
					// Not told which releases exist: it isn't the live one, which is all it says.
					return &outdatedError{release: *action.Release, live: a.Live.Number}
				}
				return err
			}
			def = *rel.Definition
		}
		// What the action does decides which right it needs: adding a row is the user's,
		// changing or removing one is the user's own or the data manager's all.
		if kind := def.Actions[action.Name].Kind; kind == ActionInsert && !acc.Submit {
			return forbidden(string(access.CapSubmit))
		} else if (kind == ActionUpdate || kind == ActionDelete) && acc.ModifyRecords == access.ScopeNone {
			return forbidden(capModifyRecords)
		}
		ch, err := Plan(SurfaceID(a.ID), def, action)
		if err != nil {
			return err
		}
		if actor.Agent {
			if err := agentMaySet(def, ch); err != nil {
				return err
			}
		}
		out.def, out.live, out.change = def, live, ch
		// What the action sets going besides the change is the live release's: an action of an
		// older release was taken as the live one defines it (liveTakes).
		effects := live.Actions[action.Name].Effects
		key, err := operationKey(action, len(effects) > 0)
		if err != nil {
			return err
		}
		var opID int64
		if key != "" {
			// A key is for one request: the release it runs from is part of it.
			release := a.Live.Number
			if action.Release != nil {
				release = *action.Release
			}
			var replay bool
			if opID, replay, err = beginOperation(ctx, tx, actor, appID, key, release, action); err != nil {
				return err
			}
			if replay {
				// Made before: the change and its effects are in already, and the answer is the
				// first one's, whatever the live release has become since: it is asked of the
				// live release's compatibility only for a request not yet made.
				out.replayed = true
				return nil
			}
		}
		if fromOlder && (action.Exact || !liveTakes(live, def, action.Name, ch)) {
			return &outdatedError{release: *action.Release, live: a.Live.Number}
		}
		ap, err := apply(ctx, tx, actor, appID, live, ch,
			visibility{scope: acc.ModifyRecords, user: actor.UserID}, readable(acc, actor.UserID))
		if err != nil {
			return err
		}
		// The operation remembers the record it made: a download says which of the caller's
		// operations its records are (offline.go), so a device doesn't count a record it sent
		// twice, once as sent and once as the server's.
		if opID != 0 && ap.changed {
			if _, err := tx.ExecContext(ctx, `UPDATE app_operations SET record_id = ? WHERE id = ?`, ap.id, opID); err != nil {
				return err
			}
		}
		out.ap = ap
		n, err := enqueueEffects(ctx, tx, actor, appID, opID, live, live.Actions[action.Name], ap)
		if err != nil {
			return err
		}
		out.effects = n
		queued = n > 0
		return nil
	})
	if err != nil {
		return changeDone{}, err
	}
	if queued {
		s.poke()
	}
	return out, nil
}

// answer is the messages that update a screen once change has been made to rows: the rows and
// summaries as they are now and, for an app with screens, what the action does to the screen the
// person is on. The record a detail screen shows is shown again after an update and emptied after
// a delete, and the screen the action says it goes on to (Then) is opened. The live app and the
// draft's preview (preview.go) answer alike.
func answer(appID int64, shown Definition, rows []Row, change Change) []Message {
	msgs := Refreshed(SurfaceID(appID), shown, rows, change.Kind == ActionInsert)
	if !shown.hasScreens() {
		return msgs
	}
	switch change.Kind {
	case ActionUpdate:
		record := map[string]any{}
		for _, r := range rows {
			if r.ID == change.ID {
				record = item(shown, r)
			}
		}
		msgs = append(msgs, dataMessage(SurfaceID(appID), "/"+RecordKey, record))
	case ActionDelete:
		msgs = append(msgs, dataMessage(SurfaceID(appID), "/"+RecordKey, map[string]any{}))
	}
	if change.Then != "" {
		msgs = append(msgs, dataMessage(SurfaceID(appID), "/"+ScreenKey, change.Then))
	}
	return msgs
}

// navigates reports whether the action is a navigate action of the release it comes from, which
// the caller may take as the app's reader. Anything it can't tell is left to act, which gives
// the errors it always did.
func (s *Service) navigates(ctx context.Context, sp access.Space, actor Actor, appID int64, action ClientAction) bool {
	if _, err := s.appAccess(ctx, actor, sp.ID, appID); err != nil {
		return false
	}
	a, err := launchable(ctx, s.db, sp.ID, appID)
	if err != nil || a.liveDef == nil {
		return false
	}
	def := *a.liveDef
	if action.Release != nil && *action.Release != a.Live.Number {
		rel, err := loadRelease(ctx, s.db, appID, *action.Release, true)
		if err != nil {
			return false
		}
		def = *rel.Definition
	}
	return def.Actions[action.Name].Kind == ActionNavigate
}

// navigate runs a navigate action: it changes no row, so it is the reader's, and answers with
// the screen to show and, for one that shows a record, the record, as the caller may read it
// (one they may not is answered as one the app doesn't have). The form at /draft, the rows
// and everything else in the data model are left alone. From a release that is not the live one
// it is read as that release defines it; a release that can't be found is answered as an
// outdated screen, so which releases exist isn't told.
func (s *Service) navigate(ctx context.Context, sp access.Space, actor Actor, appID int64, action ClientAction) ([]Message, error) {
	acc, err := s.appAccess(ctx, actor, sp.ID, appID)
	if err != nil {
		return nil, err
	}
	a, err := launchable(ctx, s.db, sp.ID, appID)
	if err != nil {
		return nil, err
	}
	if a.liveDef == nil {
		return nil, ErrNotPublished
	}
	def := *a.liveDef
	if action.Release != nil && *action.Release != a.Live.Number {
		rel, err := loadRelease(ctx, s.db, appID, *action.Release, true)
		if err != nil {
			return nil, &outdatedError{release: *action.Release, live: a.Live.Number}
		}
		def = *rel.Definition
	}
	ch, err := Plan(SurfaceID(a.ID), def, action)
	if err != nil {
		return nil, err
	}
	record := map[string]any{}
	if ch.ID != 0 {
		row, found, err := loadRow(ctx, s.db, appID, ch.ID, readable(acc, actor.UserID))
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, invalidAs(apierr.CodeAppRowNotFound, nil, "the app has no row %d", ch.ID)
		}
		record = item(shownBy(*a.liveDef, def), row)
	}
	return Opened(SurfaceID(appID), ch.Screen, record), nil
}

// loadRow returns the live row id of an app, if v reads it.
func loadRow(ctx context.Context, q queryer, appID, id int64, v visibility) (Row, bool, error) {
	var r Row
	var data string
	var creator sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT id, revision, data, created_by FROM app_rows WHERE app_id = ? AND id = ? AND deleted_at IS NULL`, appID, id).
		Scan(&r.ID, &r.Revision, &data, &creator)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !v.sees(creator)) {
		return Row{}, false, nil
	}
	if err != nil {
		return Row{}, false, err
	}
	if err := json.Unmarshal([]byte(data), &r.Data); err != nil {
		return Row{}, false, fmt.Errorf("app %d row %d: %w", appID, r.ID, err)
	}
	return r, true, nil
}

// shownBy is the definition an action's answer is drawn from: from, the release the action
// came from, narrowed to what the live release shows. Its collection holds only the fields the
// live release declares, as the live release declares them, and a summary is kept only if it
// reads one of them that has kept its type, so the answer to an action of an older release
// shows no value the live app doesn't (a field the live release removed, or the values stored
// before it retyped one, in whoever's row they are).
func shownBy(live, from Definition) Definition {
	shown := from
	shown.Collection = Collection{}
	for _, f := range from.Collection.Fields {
		if lf, ok := live.Collection.field(f.Name); ok {
			shown.Collection.Fields = append(shown.Collection.Fields, lf)
		}
	}
	shown.Summaries = map[string]Summary{}
	same := func(field string) bool {
		lf, kept := shown.Collection.field(field)
		ff, _ := from.Collection.field(field)
		return kept && lf.Type == ff.Type
	}
	for name, sum := range from.Summaries {
		keep := sum.Field == "" || same(sum.Field)
		if sum.MonthOf != "" {
			keep = keep && same(sum.MonthOf)
		}
		if sum.Where != nil {
			for _, field := range ruleFields(*sum.Where) {
				keep = keep && same(field)
			}
		}
		if keep {
			shown.Summaries[name] = sum
		}
	}
	return shown
}

// apply makes a change to an app's rows, within its bounds, and records it in their history.
// modify is the rows the actor may change: a row outside it is answered as one the app doesn't
// have, so which rows exist isn't told to someone who may not touch them. read is the rows
// they may read, which decides whether a conflict shows the row as it is now.
func apply(ctx context.Context, tx *sql.Tx, actor Actor, appID int64, def Definition, ch Change, modify, read visibility) (applied, error) {
	var count, size int
	// Only live rows count against the limits. In bytes, as len() counts below: length() of
	// TEXT counts characters, which would let non-ASCII rows hold more than MaxDataSize.
	if err := tx.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(length(CAST(data AS BLOB))), 0) FROM app_rows WHERE app_id = ? AND deleted_at IS NULL`, appID).
		Scan(&count, &size); err != nil {
		return applied{}, err
	}
	at := now()
	switch ch.Kind {
	case ActionInsert:
		// The row is stored as the live release's collection allows, whichever release's screen
		// the action came from: its required fields and limits are the live ones.
		if err := def.Collection.Fits(ch.Data); err != nil {
			return applied{}, err
		}
		raw, err := json.Marshal(ch.Data)
		if err != nil {
			return applied{}, err
		}
		if count >= MaxRows || size+len(raw) > MaxDataSize {
			return applied{}, appFull(fmt.Sprintf("an app holds at most %d rows and %d MiB", MaxRows, MaxDataSize>>20))
		}
		var id int64
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO app_rows (app_id, data, revision,
				created_at, created_by, created_by_agent, created_via, created_action,
				updated_at, updated_by, updated_by_agent, updated_via, updated_action)
			VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			appID, string(raw), at, actor.UserID, actor.Agent, via(actor), actionColumn(ch),
			at, actor.UserID, actor.Agent, via(actor), actionColumn(ch)).Scan(&id); err != nil {
			return applied{}, err
		}
		if err := claimAttachments(ctx, tx, actor, appID, id, def, nil, ch.Data); err != nil {
			return applied{}, err
		}
		if err := remember(ctx, tx, appID, id, 1, opInsert, string(raw), at, actor, ch); err != nil {
			return applied{}, err
		}
		return applied{id: id, revision: 1, data: ch.Data, changed: true}, nil
	case ActionUpdate, ActionDelete:
		cur, err := liveRow(ctx, tx, appID, ch, modify, read, def)
		if err != nil {
			return applied{}, err
		}
		if ch.Kind == ActionDelete {
			_, err := tx.ExecContext(ctx, `
				UPDATE app_rows SET revision = ?, deleted_at = ?, deleted_by = ?, deleted_by_agent = ?, deleted_via = ?, deleted_action = ?
				WHERE id = ? AND app_id = ?`,
				cur.revision+1, at, actor.UserID, actor.Agent, via(actor), actionColumn(ch), ch.ID, appID)
			if err != nil {
				return applied{}, err
			}
			return applied{id: ch.ID, revision: cur.revision + 1, data: cur.fields, changed: true},
				remember(ctx, tx, appID, ch.ID, cur.revision+1, opDelete, cur.data, at, actor, ch)
		}
		// A captured location and a signature stay as they were captured, whoever asks to change them
		// and by whichever screen, release or the records explorer. That is told first: clearing a
		// required one is a change that is refused for what it is, not for the field's being required.
		next := merged(cur.fields, ch.Data)
		if err := def.Collection.lockedChange(cur.fields, next); err != nil {
			return applied{}, err
		}
		if err := def.Collection.Fits(next); err != nil {
			return applied{}, err
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return applied{}, err
		}
		if size-len(cur.data)+len(raw) > MaxDataSize {
			return applied{}, appFull(fmt.Sprintf("an app holds at most %d MiB", MaxDataSize>>20))
		}
		if string(raw) == cur.canonical {
			// Nothing changed: no new revision, nothing to remember, no effects.
			return applied{id: ch.ID, revision: cur.revision, data: next}, nil
		}
		if err := claimAttachments(ctx, tx, actor, appID, ch.ID, def, cur.fields, next); err != nil {
			return applied{}, err
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE app_rows SET data = ?, revision = ?, updated_at = ?, updated_by = ?, updated_by_agent = ?, updated_via = ?, updated_action = ?
			WHERE id = ? AND app_id = ?`,
			string(raw), cur.revision+1, at, actor.UserID, actor.Agent, via(actor), actionColumn(ch), ch.ID, appID)
		if err != nil {
			return applied{}, err
		}
		if err := remember(ctx, tx, appID, ch.ID, cur.revision+1, opUpdate, string(raw), at, actor, ch); err != nil {
			return applied{}, err
		}
		return applied{id: ch.ID, revision: cur.revision + 1, data: next, changed: true}, nil
	}
	return applied{}, invalidf("unknown kind of action %q", ch.Kind)
}

// The operations a record's history lists.
const (
	opInsert  = "insert"
	opUpdate  = "update"
	opDelete  = "delete"
	opRestore = "restore"
)

// actionColumn is the column value of the action that made a change: NULL for one that wasn't an
// app's action (a restore).
func actionColumn(ch Change) any {
	if ch.Name == "" {
		return nil
	}
	return ch.Name
}

// remember adds one version of a record to its history.
func remember(ctx context.Context, tx *sql.Tx, appID, rowID, revision int64, op, data, at string, actor Actor, ch Change) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO app_row_history (app_id, row_id, revision, operation, data, at, actor, actor_agent, actor_via, action)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		appID, rowID, revision, op, data, at, actor.UserID, actor.Agent, via(actor), actionColumn(ch))
	return err
}

// liveRecord is a live row as an update or delete finds it.
type liveRecord struct {
	revision  int64
	data      string         // as stored
	fields    map[string]any // data, decoded
	canonical string         // data, encoded as a change encodes it
}

// liveRow reads the live row ch names for a change. A row that doesn't exist, is deleted or
// is outside modify is answered as one the app doesn't have; one whose revision isn't the
// one the change expected is a conflict.
func liveRow(ctx context.Context, tx *sql.Tx, appID int64, ch Change, modify, read visibility, def Definition) (liveRecord, error) {
	var rec liveRecord
	var creator sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT data, revision, created_by FROM app_rows WHERE id = ? AND app_id = ? AND deleted_at IS NULL`, ch.ID, appID).
		Scan(&rec.data, &rec.revision, &creator)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !modify.sees(creator)) {
		return liveRecord{}, invalidAs(apierr.CodeAppRowNotFound, nil, "the app has no row %d", ch.ID)
	}
	if err != nil {
		return liveRecord{}, err
	}
	if err := json.Unmarshal([]byte(rec.data), &rec.fields); err != nil {
		return liveRecord{}, err
	}
	canonical, err := json.Marshal(rec.fields)
	if err != nil {
		return liveRecord{}, err
	}
	rec.canonical = string(canonical)
	if ch.Expected != nil && *ch.Expected != rec.revision {
		return liveRecord{}, conflict(ch.ID, rec, creator, read, def)
	}
	return rec, nil
}

// conflict is the error for a record whose revision isn't the one expected: with the record
// as it is now if the caller reads it.
func conflict(id int64, rec liveRecord, creator sql.NullInt64, read visibility, def Definition) error {
	e := &conflictError{id: id}
	if read.sees(creator) {
		e.current = item(def, Row{ID: id, Revision: rec.revision, Data: rec.fields})
	}
	return e
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
