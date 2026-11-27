package access

import (
	"context"
	"database/sql"
	"errors"
	"slices"
)

type AppRole string

const (
	AppBuilder     AppRole = "builder"
	AppPublisher   AppRole = "publisher"
	AppUser        AppRole = "user"
	AppDataManager AppRole = "data_manager"
	AppReader      AppRole = "reader"
)

var AppRoles = []AppRole{AppBuilder, AppPublisher, AppUser, AppDataManager, AppReader}

func (r AppRole) valid() bool { return slices.Contains(AppRoles, r) }

type Scope string

const (
	ScopeNone Scope = "none"
	ScopeOwn  Scope = "own"
	ScopeAll  Scope = "all"
)

func (s Scope) rank() int {
	switch s {
	case ScopeOwn:
		return 1
	case ScopeAll:
		return 2
	default:
		return 0
	}
}

func minScope(a, b Scope) Scope {
	if a.rank() <= b.rank() {
		return a
	}
	return b
}

func maxScope(a, b Scope) Scope {
	if a.rank() >= b.rank() {
		return a
	}
	return b
}

type Capability string

const (
	CapView        Capability = "view"
	CapDesign      Capability = "design"
	CapPublish     Capability = "publish"
	CapSubmit      Capability = "submit"
	CapManageRoles Capability = "manage_roles"
)

type AppAccess struct {
	Roles      []AppRole `json:"roles" apitypes:"nonnull"`
	Restricted bool      `json:"restricted"`

	View          bool  `json:"view"`
	Design        bool  `json:"design"`
	Publish       bool  `json:"publish"`
	Submit        bool  `json:"submit"`
	ManageRoles   bool  `json:"manage_roles"`
	ReadRecords   Scope `json:"read_records"`
	ModifyRecords Scope `json:"modify_records"`
}

func (a AppAccess) Has(c Capability) bool {
	switch c {
	case CapView:
		return a.View
	case CapDesign:
		return a.Design
	case CapPublish:
		return a.Publish
	case CapSubmit:
		return a.Submit
	case CapManageRoles:
		return a.ManageRoles
	}
	return false
}

func roleCaps(roles []AppRole) AppAccess {
	a := AppAccess{ReadRecords: ScopeNone, ModifyRecords: ScopeNone}
	for _, r := range roles {
		switch r {
		case AppBuilder:
			a.View, a.Design, a.ManageRoles = true, true, true
		case AppPublisher:
			a.View, a.Publish = true, true
		case AppUser:
			a.View, a.Submit = true, true
			a.ReadRecords = maxScope(a.ReadRecords, ScopeOwn)
			a.ModifyRecords = maxScope(a.ModifyRecords, ScopeOwn)
		case AppDataManager:
			a.View, a.Submit = true, true
			a.ReadRecords = maxScope(a.ReadRecords, ScopeAll)
			a.ModifyRecords = maxScope(a.ModifyRecords, ScopeAll)
		case AppReader:
			a.View = true
			a.ReadRecords = maxScope(a.ReadRecords, ScopeAll)
		}
	}
	return a
}

func spaceLimit(sp Access) AppAccess {
	switch sp {
	case Write:
		return AppAccess{View: true, Design: true, Publish: true, Submit: true, ManageRoles: true, ReadRecords: ScopeAll, ModifyRecords: ScopeAll}
	case Read:
		return AppAccess{View: true, ReadRecords: ScopeAll, ModifyRecords: ScopeNone}
	default:
		return AppAccess{ReadRecords: ScopeNone, ModifyRecords: ScopeNone}
	}
}

func defaultRoles(sp Access) []AppRole {
	switch sp {
	case Write:
		return slices.Clone(AppRoles)
	case Read:
		return []AppRole{AppReader}
	default:
		return []AppRole{}
	}
}

func appAccess(sp Access, roles []AppRole, restricted, rescue bool) AppAccess {
	limit := spaceLimit(sp)
	if !restricted {
		roles = defaultRoles(sp)
	}
	got := roleCaps(roles)
	if rescue && limit.ManageRoles {
		got.View, got.ManageRoles = true, true
	}
	return AppAccess{
		Roles:         sortedRoles(roles),
		Restricted:    restricted,
		View:          got.View && limit.View,
		Design:        got.Design && limit.Design,
		Publish:       got.Publish && limit.Publish,
		Submit:        got.Submit && limit.Submit,
		ManageRoles:   got.ManageRoles && limit.ManageRoles,
		ReadRecords:   minScope(got.ReadRecords, limit.ReadRecords),
		ModifyRecords: minScope(got.ModifyRecords, limit.ModifyRecords),
	}
}

func sortedRoles(roles []AppRole) []AppRole {
	out := []AppRole{}
	for _, r := range AppRoles {
		if slices.Contains(roles, r) {
			out = append(out, r)
		}
	}
	return out
}

type AppGrant struct {
	UserID *int64    `json:"user_id"`
	Roles  []AppRole `json:"roles"`
}

type grantRow struct {
	userID sql.NullInt64
	role   AppRole
}

func resolve(rows []grantRow, userID int64, anyGrants bool) (roles []AppRole, restricted bool) {
	var own, def []AppRole
	for _, r := range rows {
		switch {
		case r.userID.Valid && r.userID.Int64 == userID:
			own = append(own, r.role)
		case !r.userID.Valid:
			def = append(def, r.role)
		}
	}
	switch {
	case len(own) > 0:
		return own, true
	case len(def) > 0:
		return def, true
	case anyGrants:
		return []AppRole{}, true
	}
	return nil, false
}

var ErrAppNotFound = errors.New("app not found")

func (c *Checker) App(ctx context.Context, userID, spaceID, appID int64) (AppAccess, error) {
	sp, err := c.Check(ctx, userID, spaceID)
	if err != nil {
		return AppAccess{}, err
	}
	var exists bool
	if err := c.reads.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM apps WHERE id = ? AND space_id = ?)`,
		appID, spaceID).Scan(&exists); err != nil {
		return AppAccess{}, err
	}
	if !exists {
		return AppAccess{}, ErrAppNotFound
	}
	rows, err := c.grantRows(ctx, userID, appID)
	if err != nil {
		return AppAccess{}, err
	}
	var anyGrants bool
	if err := c.reads.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM app_roles WHERE app_id = ?)`,
		appID).Scan(&anyGrants); err != nil {
		return AppAccess{}, err
	}
	roles, restricted := resolve(rows, userID, anyGrants)
	rescue, err := c.rescues(ctx, userID, sp, appID)
	if err != nil {
		return AppAccess{}, err
	}
	return appAccess(sp.Access, roles, restricted, rescue), nil
}

func (c *Checker) AppNamed(ctx context.Context, userID, spaceID, appID int64) (Space, AppAccess, string, error) {
	var sp Space
	var acc AppAccess
	var name string
	err := c.Snapshot(ctx, func(snap *Checker, tx *sql.Tx) (err error) {
		if sp, err = snap.Check(ctx, userID, spaceID); err != nil {
			return err
		}
		if acc, err = snap.App(ctx, userID, spaceID, appID); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT name FROM apps WHERE id = ? AND space_id = ?`, appID, spaceID).Scan(&name)
	})
	if err != nil {
		return Space{}, AppAccess{}, "", err
	}
	return sp, acc, name, nil
}

func (c *Checker) Snapshot(ctx context.Context, fn func(snap *Checker, tx *sql.Tx) error) error {
	tx, held := c.reads.(*sql.Tx)
	if !held {
		var err error
		tx, err = c.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
	}
	return fn(&Checker{db: c.db, reads: tx, auth: c.auth}, tx)
}

func stranded(all []grantRow) bool {
	if len(all) == 0 {
		return false
	}
	for _, r := range all {
		if r.role == AppBuilder {
			return false
		}
	}
	return true
}

const strandedApps = `
	SELECT a.id FROM apps a
	WHERE a.space_id = ?
	  AND EXISTS (SELECT 1 FROM app_roles r WHERE r.app_id = a.id)
	  AND NOT EXISTS (SELECT 1 FROM app_roles r WHERE r.app_id = a.id AND r.role = 'builder')`

func (c *Checker) rescues(ctx context.Context, userID int64, sp Space, appID int64) (bool, error) {
	admin, err := c.administers(ctx, userID, sp)
	if err != nil || !admin {
		return false, err
	}
	var isStranded bool
	err = c.reads.QueryRowContext(ctx, `SELECT EXISTS (`+strandedApps+` AND a.id = ?)`, sp.ID, appID).Scan(&isStranded)
	return isStranded, err
}

func (c *Checker) administers(ctx context.Context, userID int64, sp Space) (bool, error) {
	if sp.Kind != Shared {
		return false, nil
	}
	var admin bool
	err := c.reads.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM memberships WHERE workspace_id = ? AND user_id = ? AND role = 'admin')`,
		sp.WorkspaceID, userID).Scan(&admin)
	return admin, err
}

func (c *Checker) AppsIn(ctx context.Context, userID int64, sp Space) (map[int64]AppAccess, error) {
	rows, err := c.reads.QueryContext(ctx, `
		SELECT a.id, EXISTS (SELECT 1 FROM app_roles x WHERE x.app_id = a.id), r.user_id, r.role
		FROM apps a LEFT JOIN app_roles r ON r.app_id = a.id AND (r.user_id = ? OR r.user_id IS NULL)
		WHERE a.space_id = ?`, userID, sp.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byApp := map[int64][]grantRow{}
	anyGrants := map[int64]bool{}
	for rows.Next() {
		var id int64
		var has bool
		var g grantRow
		var role sql.NullString
		if err := rows.Scan(&id, &has, &g.userID, &role); err != nil {
			return nil, err
		}
		anyGrants[id] = has
		if _, seen := byApp[id]; !seen {
			byApp[id] = nil
		}
		if role.Valid {
			g.role = AppRole(role.String)
			byApp[id] = append(byApp[id], g)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	admin, err := c.administers(ctx, userID, sp)
	if err != nil {
		return nil, err
	}
	rescue := map[int64]bool{}
	if admin {
		ids, err := c.reads.QueryContext(ctx, strandedApps, sp.ID)
		if err != nil {
			return nil, err
		}
		for ids.Next() {
			var id int64
			if err := ids.Scan(&id); err != nil {
				_ = ids.Close()
				return nil, err
			}
			rescue[id] = true
		}
		if err := errors.Join(ids.Err(), ids.Close()); err != nil {
			return nil, err
		}
	}
	out := make(map[int64]AppAccess, len(byApp))
	for id, gs := range byApp {
		roles, restricted := resolve(gs, userID, anyGrants[id])
		out[id] = appAccess(sp.Access, roles, restricted, rescue[id])
	}
	return out, nil
}

func (c *Checker) grantRows(ctx context.Context, userID, appID int64) ([]grantRow, error) {
	rows, err := c.reads.QueryContext(ctx,
		`SELECT user_id, role FROM app_roles WHERE app_id = ? AND (user_id = ? OR user_id IS NULL)`, appID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []grantRow
	for rows.Next() {
		var g grantRow
		if err := rows.Scan(&g.userID, &g.role); err != nil {
			return nil, err
		}
		all = append(all, g)
	}
	return all, rows.Err()
}

const maxAppGrants = 1000

var (
	ErrBadGrants        = errors.New("invalid role assignments")
	ErrGrantUnknownUser = errors.New("a role can only go to a member of the space")
	ErrGrantForbidden   = errors.New("you can't manage this app's roles")
	ErrGrantLockout     = errors.New("you would lose the right to manage this app's roles")
)

func (c *Checker) AppGrants(ctx context.Context, spaceID, appID int64) ([]AppGrant, error) {
	rows, err := c.reads.QueryContext(ctx, `
		SELECT r.user_id, r.role FROM app_roles r JOIN apps a ON a.id = r.app_id
		WHERE r.app_id = ? AND a.space_id = ?
		ORDER BY r.user_id IS NULL, r.user_id`, appID, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []grantRow
	for rows.Next() {
		var g grantRow
		if err := rows.Scan(&g.userID, &g.role); err != nil {
			return nil, err
		}
		all = append(all, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []AppGrant{}
	for _, g := range all {
		if n := len(out); n > 0 && sameUser(out[n-1].UserID, g.userID) {
			out[n-1].Roles = append(out[n-1].Roles, g.role)
			continue
		}
		var uid *int64
		if g.userID.Valid {
			v := g.userID.Int64
			uid = &v
		}
		out = append(out, AppGrant{UserID: uid, Roles: []AppRole{g.role}})
	}
	for i := range out {
		out[i].Roles = sortedRoles(out[i].Roles)
	}
	return out, nil
}

func sameUser(a *int64, b sql.NullInt64) bool {
	if a == nil || !b.Valid {
		return a == nil && !b.Valid
	}
	return *a == b.Int64
}

func (c *Checker) SetAppGrants(ctx context.Context, actorID, spaceID, appID int64, grants []AppGrant) error {
	if len(grants) > maxAppGrants {
		return ErrBadGrants
	}
	seen := map[int64]bool{}
	sawDefault := false
	var rows []grantRow
	for _, g := range grants {
		if len(g.Roles) == 0 {
			return ErrBadGrants
		}
		if g.UserID == nil {
			if sawDefault {
				return ErrBadGrants
			}
			sawDefault = true
		} else {
			if *g.UserID < 1 || seen[*g.UserID] {
				return ErrBadGrants
			}
			seen[*g.UserID] = true
		}
		roles := map[AppRole]bool{}
		for _, r := range g.Roles {
			if !r.valid() || roles[r] {
				return ErrBadGrants
			}
			roles[r] = true
			row := grantRow{role: r}
			if g.UserID != nil {
				row.userID = sql.NullInt64{Int64: *g.UserID, Valid: true}
			}
			rows = append(rows, row)
		}
	}
	return c.write(ctx, func(tx *sql.Tx) error {
		sp, err := c.Check(ctx, actorID, spaceID)
		if err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM apps WHERE id = ? AND space_id = ?)`,
			appID, spaceID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrAppNotFound
		}
		if now, err := c.App(ctx, actorID, spaceID, appID); err != nil {
			return err
		} else if !now.ManageRoles {
			return ErrGrantForbidden
		}
		for id := range seen {
			if _, err := c.Check(ctx, id, spaceID); errors.Is(err, ErrNotFound) {
				return ErrGrantUnknownUser
			} else if err != nil {
				return err
			}
		}
		roles, restricted := resolve(rows, actorID, len(rows) > 0)
		admin, err := c.administers(ctx, actorID, sp)
		if err != nil {
			return err
		}
		rescue := admin && stranded(rows)
		handedOver := admin && !stranded(rows)
		if !appAccess(sp.Access, roles, restricted, rescue).ManageRoles && !handedOver {
			return ErrGrantLockout
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM app_roles WHERE app_id = ?`, appID); err != nil {
			return err
		}
		for _, r := range rows {
			if _, err := tx.ExecContext(ctx, `INSERT INTO app_roles (app_id, user_id, role) VALUES (?, ?, ?)`,
				appID, r.userID, r.role); err != nil {
				return err
			}
		}
		return nil
	})
}
