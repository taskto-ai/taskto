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
	"taskto.ai/taskto/internal/paylink"
	"taskto.ai/taskto/internal/quota"
	"taskto.ai/taskto/internal/schedule"
)

type Actor struct {
	UserID int64
	Agent  bool
	Via    string
}

type Author struct {
	ID     int64         `json:"id"`
	Name   string        `json:"name"`
	Agent  bool          `json:"agent"`
	Via    string        `json:"via,omitempty"`
	System *SystemAuthor `json:"system,omitempty"`
}

type SystemAuthor struct {
	Kind     string `json:"kind"`
	ID       int64  `json:"id"`
	Provider string `json:"provider,omitempty"`
	Name     string `json:"name,omitempty"`
	Removed  bool   `json:"removed,omitempty"`
}

type App struct {
	ID                 int64            `json:"id"`
	SpaceID            int64            `json:"space_id"`
	Name               string           `json:"name"`
	ArchivedAt         *string          `json:"archived_at"`
	Version            int64            `json:"version"`
	Rows               int              `json:"rows"`
	CreatedAt          string           `json:"created_at"`
	CreatedBy          *Author          `json:"created_by"`
	UpdatedAt          string           `json:"updated_at"`
	UpdatedBy          *Author          `json:"updated_by"`
	Access             access.AppAccess `json:"access"`
	Live               *Release         `json:"live"`
	UnpublishedChanges bool             `json:"unpublished_changes"`
	NeedsBoard         bool             `json:"needs_board"`
	Draft              *Definition      `json:"draft,omitempty"`
	EffectTargets      []EffectTarget   `json:"effect_targets,omitempty"`

	draftDef     Definition
	liveDef      *Definition
	draftDiffers bool
	detailed     bool
}

var (
	ErrNotFound  = errors.New("not found")
	ErrReadOnly  = errors.New("you can only read this space")
	ErrOffline   = errors.New("this app is offline: restore it to launch it")
	ErrStale     = errors.New("it changed since you read it: reload and try again")
	ErrConflict  = errors.New("the record changed since you read it")
	ErrFull      = errors.New("full")
	ErrForbidden = errors.New("your role in this app doesn't allow this")
)

type forbiddenError struct{ capability string }

func (e *forbiddenError) Error() string        { return ErrForbidden.Error() + " (" + e.capability + ")" }
func (e *forbiddenError) Is(target error) bool { return target == ErrForbidden }

func forbidden(capability string) error { return &forbiddenError{capability: capability} }

const (
	capReadRecords   = "read_records"
	capModifyRecords = "modify_records"
)

func need(acc access.AppAccess, c access.Capability) error {
	if !acc.Has(c) {
		return forbidden(string(c))
	}
	return nil
}

type visibility struct {
	scope access.Scope
	user  int64
}

func readable(acc access.AppAccess, userID int64) visibility {
	return visibility{scope: acc.ReadRecords, user: userID}
}

var everything = visibility{scope: access.ScopeAll}

func (v visibility) sees(creator sql.NullInt64) bool {
	switch v.scope {
	case access.ScopeAll:
		return true
	case access.ScopeOwn:
		return creator.Valid && creator.Int64 == v.user
	}
	return false
}

type conflictError struct {
	id      int64
	current map[string]any
}

func (e *conflictError) Error() string        { return ErrConflict.Error() }
func (e *conflictError) Is(target error) bool { return target == ErrConflict }

type fullError struct {
	msg    string
	code   apierr.Code
	params apierr.Params
}

func (e *fullError) Error() string        { return ErrFull.Error() + ": " + e.msg }
func (e *fullError) Is(target error) bool { return target == ErrFull }

var errTooManyApps = &fullError{
	msg:  fmt.Sprintf("a space has at most %d apps", maxApps),
	code: apierr.CodeTooManyApps, params: apierr.Params{"max": maxApps},
}

func appFull(msg string) error {
	return &fullError{msg: msg, code: apierr.CodeAppFull, params: apierr.Params{"max_rows": MaxRows, "max_mib": MaxDataSize >> 20}}
}

const maxApps = 500

const maxNameLen = 100

type Service struct {
	db               *sql.DB
	access           *access.Checker
	platform         Platform
	clock            func() time.Time
	wake             chan struct{}
	poll             time.Duration
	hooks            outboxHooks
	dataDir          string
	holder           SpaceHolder
	locks            fileLocks
	thumbs           chan struct{}
	quota            *quota.Service
	deletedRetention time.Duration
	budget           *tokenBudget
	sched            *schedule.Service
}

type outboxHooks struct {
	afterWork                func(effectRow) error
	beforeDocument           func(effectRow)
	afterActionsAccess       func()
	afterPaymentSnapshotRead func()
}

func New(sqlDB *sql.DB, c *access.Checker) *Service {
	return &Service{db: sqlDB, access: c, clock: time.Now, budget: sharedBudget(sqlDB), wake: make(chan struct{}, 1), thumbs: make(chan struct{}, maxThumbnails),
		deletedRetention: DefaultDeletedRetention}
}

const timeFormat = "2006-01-02T15:04:05.000Z"

func now() string { return time.Now().UTC().Format(timeFormat) }

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type scanner interface{ Scan(dest ...any) error }

func (s *Service) write(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(context.WithoutCancel(ctx), nil)
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
	if err := ctx.Err(); err != nil {
		return err
	}
	return tx.Commit()
}

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

func (s *Service) appAccess(ctx context.Context, a Actor, spaceID, appID int64) (access.AppAccess, error) {
	return appAccessBy(ctx, s.access, a, spaceID, appID)
}

func appAccessBy(ctx context.Context, checker *access.Checker, a Actor, spaceID, appID int64) (access.AppAccess, error) {
	acc, err := checker.App(ctx, a.UserID, spaceID, appID)
	if errors.Is(err, access.ErrNotFound) || errors.Is(err, access.ErrAppNotFound) {
		return access.AppAccess{}, ErrNotFound
	}
	if err == nil && !acc.View {
		return access.AppAccess{}, ErrNotFound
	}
	return acc, err
}

func (s *Service) recheckApp(ctx context.Context, a Actor, spaceID, appID int64) (access.AppAccess, error) {
	if err := s.recheck(ctx, a, spaceID); err != nil {
		return access.AppAccess{}, err
	}
	return s.appAccess(ctx, a, spaceID, appID)
}

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

func reviews(acc access.AppAccess) bool { return acc.Design || acc.Publish }

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

func via(a Actor) any {
	if a.Via == "" {
		return nil
	}
	return a.Via
}

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
	accs, err := s.access.AppsIn(ctx, a.UserID, sp)
	if err != nil {
		return nil, err
	}
	counts, err := s.rowCounts(ctx, sp.ID, a.UserID)
	if err != nil {
		return nil, err
	}
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

type NewApp struct {
	Name       string     `json:"name"`
	Definition Definition `json:"definition"`
}

func (s *Service) createApp(ctx context.Context, sp access.Space, a Actor, na NewApp) (App, error) {
	return s.createAppWith(ctx, sp, a, na, nil)
}

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
		created, err = app(ctx, tx, sp.ID, id)
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

type Changes struct {
	Version    int64       `json:"version"`
	Name       *string     `json:"name"`
	Archived   *bool       `json:"archived"`
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
	var name, def any
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
	var archivedAt any
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
		var differs any
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
		if ch.Archived != nil {
			if err := s.syncAppReminders(ctx, tx, appID, 0); err != nil {
				return err
			}
		}
		updated, err = app(ctx, tx, sp.ID, appID)
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
		if _, err := tx.ExecContext(ctx, `DELETE FROM home_defaults WHERE app_id = ?`, appID); err != nil {
			return err
		}
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
		if err := s.syncAppReminders(ctx, tx, appID, 0); err != nil {
			return err
		}
		moved, err = app(ctx, tx, to.ID, appID)
		return err
	})
	if err != nil {
		return App{}, err
	}
	if err := s.relocate(ctx, appID); err != nil {
		slog.Warn("apps: moving an app's attachments", "app", appID, "err", err)
	}
	if err := s.present(ctx, a, &moved); err != nil {
		return App{}, err
	}
	return moved, nil
}

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

func launchable(ctx context.Context, q queryer, spaceID, appID int64) (App, error) {
	a, err := app(ctx, q, spaceID, appID)
	if err == nil && a.ArchivedAt != nil {
		return App{}, ErrOffline
	}
	return a, err
}

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

func (s *Service) act(ctx context.Context, sp access.Space, actor Actor, appID int64, action ClientAction) ([]Message, error) {
	if err := s.spendTokenAction(actor, sp.WorkspaceID); err != nil {
		if IsRateLimited(err) && s.committed(ctx, sp.ID, actor, appID, action) {
			return []Message{}, nil
		}
		return nil, err
	}
	msgs, err := s.actOnce(ctx, sp, actor, appID, action)
	if err != nil &&
		(errors.Is(err, ErrNotFound) || errors.Is(err, ErrReadOnly) || errors.Is(err, ErrForbidden) || errors.Is(err, ErrOffline)) &&
		s.committed(ctx, sp.ID, actor, appID, action) {
		return []Message{}, nil
	}
	return msgs, err
}

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
	rows, err := loadRows(ctx, s.db, appID, readable(done.acc, actor.UserID))
	if err != nil {
		return nil, err
	}
	return answer(appID, shownBy(done.live, done.def), rows, done.change), nil
}

type changeDone struct {
	acc       access.AppAccess
	def, live Definition
	change    Change
	ap        applied
	replayed  bool
	effects   int
}

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
			if !acc.Submit && acc.ModifyRecords == access.ScopeNone {
				return forbidden(string(access.CapSubmit))
			}
			rel, err := loadRelease(ctx, tx, appID, *action.Release, true)
			if err != nil {
				if action.Exact && errors.Is(err, ErrReleaseNotFound) {
					return &outdatedError{release: *action.Release, live: a.Live.Number}
				}
				return err
			}
			def = *rel.Definition
		}
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
		effects := live.Actions[action.Name].Effects
		key, err := operationKey(action, len(effects) > 0)
		if err != nil {
			return err
		}
		var opID int64
		if key != "" {
			release := a.Live.Number
			if action.Release != nil {
				release = *action.Release
			}
			var replay bool
			if opID, replay, err = beginOperation(ctx, tx, actor, appID, key, release, action); err != nil {
				return err
			}
			if replay {
				out.replayed = true
				return nil
			}
		}
		if fromOlder && (action.Exact || !liveTakes(live, def, action.Name, ch)) {
			return &outdatedError{release: *action.Release, live: a.Live.Number}
		}
		ap, err := apply(ctx, tx, actor, appID, live, ch, s.platform.Payments,
			visibility{scope: acc.ModifyRecords, user: actor.UserID}, readable(acc, actor.UserID))
		if err != nil {
			return err
		}
		if opID != 0 && ap.changed {
			if _, err := tx.ExecContext(ctx, `UPDATE app_operations SET record_id = ? WHERE id = ?`, ap.id, opID); err != nil {
				return err
			}
		}
		out.ap = ap
		if ap.changed {
			if err := s.syncRecordReminders(ctx, tx, appID, ap.id); err != nil {
				return err
			}
		}
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

func apply(ctx context.Context, tx *sql.Tx, actor Actor, appID int64, def Definition, ch Change, payments paylink.Opener, modify, read visibility) (applied, error) {
	var count, size int
	if err := tx.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(length(CAST(data AS BLOB))), 0) FROM app_rows WHERE app_id = ? AND deleted_at IS NULL`, appID).
		Scan(&count, &size); err != nil {
		return applied{}, err
	}
	at := now()
	switch ch.Kind {
	case ActionInsert:
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
			INSERT INTO app_rows (id, app_id, data, revision,
				created_at, created_by, created_by_agent, created_via, created_action,
				updated_at, updated_by, updated_by_agent, updated_via, updated_action)
			VALUES (max(coalesce((SELECT max(id) FROM app_rows), 0), (SELECT high FROM app_row_ids)) + 1,
				?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
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
		next := merged(cur.fields, ch.Data)
		if err := def.Collection.lockedChange(cur.fields, next); err != nil {
			return applied{}, err
		}
		if err := paymentMoneyLocked(ctx, tx, payments, appID, ch.ID, def, cur.fields, next); err != nil {
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
			return applied{id: ch.ID, revision: cur.revision, data: next}, nil
		}
		if err := claimAttachments(ctx, tx, actor, appID, ch.ID, def, cur.fields, next); err != nil {
			return applied{}, err
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE app_rows SET data = ?, revision = ?, updated_at = ?, updated_by = ?, updated_by_agent = ?, updated_via = ?, updated_action = ?, updated_system = NULL
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

const (
	opInsert  = "insert"
	opUpdate  = "update"
	opDelete  = "delete"
	opRestore = "restore"
)

func actionColumn(ch Change) any {
	if ch.Name == "" {
		return nil
	}
	return ch.Name
}

func remember(ctx context.Context, tx *sql.Tx, appID, rowID, revision int64, op, data, at string, actor Actor, ch Change) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO app_row_history (app_id, row_id, revision, operation, data, at, actor, actor_agent, actor_via, action)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		appID, rowID, revision, op, data, at, actor.UserID, actor.Agent, via(actor), actionColumn(ch))
	if err != nil || revision <= MaxRevisions {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM app_row_history WHERE row_id = ? AND revision <= ?`, rowID, revision-MaxRevisions)
	return err
}

type liveRecord struct {
	revision  int64
	data      string
	fields    map[string]any
	canonical string
}

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

func conflict(id int64, rec liveRecord, creator sql.NullInt64, read visibility, def Definition) error {
	e := &conflictError{id: id}
	if read.sees(creator) {
		e.current = item(def, Row{ID: id, Revision: rec.revision, Data: rec.fields})
	}
	return e
}

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
