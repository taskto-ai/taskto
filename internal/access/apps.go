package access

import (
	"context"
	"database/sql"
	"errors"
	"slices"
)

// App roles. Access to an app is first access to its space (Space.Access); app roles only
// narrow it further, and never widen it: what a user may do in an app is what their roles
// grant, limited to what their role in the space allows (a viewer of the space never writes,
// whatever app roles they hold; nobody writes in an archived space).
//
// An app without any role assignment is unrestricted: its users have what their role in the
// space allows. Once it has one, it is restricted for everyone: a member holds their own
// roles, else the default's, else none (they don't see the app). That is what every app had before roles existed, and what apps keep until
// someone sets roles on them.
//
// A role is a set of capabilities, and a user may hold several:
//
//	role          design  publish  submit  read records  modify records
//	builder       yes     -        -       -             -
//	publisher     -       yes      -       -             -
//	user          -       -        yes     own           own
//	data_manager  -       -        yes     all           all
//	reader        -       -        -       all           -
//
// Every role may view the app (launch it and read its live definition), and a builder may also
// manage the app's roles. design is changing the app's draft (its screen, collection, actions
// and summaries), renaming the app and moving it; publish is making the draft the
// live release (publishing it, or rolling back to an earlier release: internal/apps, release.go)
// and taking the app offline and back; submit is adding records. Reading and modifying
// records have a scope: "own" is the records the user created (the agent's included, as it
// acts for them), "all" every record, "none" no record. Every way to read records answers to
// the read scope, summaries (count, sum) and the counts in lists included.
//
// The space allows write: every capability; read: view and read all records.
//
// One more rule keeps an app from being left with nobody to set it right: in a shared space, a
// workspace admin who may write the space (a member of it, as an editor) can always view the
// app and manage its roles, whatever roles it assigns, and so can always give someone the
// data manager role. Roles are removed with the member who held them (someone leaving the
// space), so without this the last manager's departure would strand the app. It adds no
// other capability: the admin reads or changes records only through roles they give themselves.

// AppRole is a role in one app.
type AppRole string

// The roles of an app.
const (
	AppBuilder     AppRole = "builder"
	AppPublisher   AppRole = "publisher"
	AppUser        AppRole = "user"
	AppDataManager AppRole = "data_manager"
	AppReader      AppRole = "reader"
)

// AppRoles lists every role, in the order they are shown.
var AppRoles = []AppRole{AppBuilder, AppPublisher, AppUser, AppDataManager, AppReader}

func (r AppRole) valid() bool { return slices.Contains(AppRoles, r) }

// Scope is how many of an app's records a capability reaches.
type Scope string

// The scopes, each including the ones before it.
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

// Capability is something a user may do in an app, apart from the scoped reading and
// modifying of records.
type Capability string

// The capabilities.
const (
	CapView        Capability = "view"
	CapDesign      Capability = "design"
	CapPublish     Capability = "publish"
	CapSubmit      Capability = "submit"
	CapManageRoles Capability = "manage_roles"
)

// AppAccess is what one user may do in one app, as the Checker works it out.
type AppAccess struct {
	// Roles are the roles the user holds in the app: every role they could hold if the app
	// is unrestricted, or the ones assigned to them (their own, or else the default's).
	Roles []AppRole `json:"roles"`
	// Restricted is whether roles were assigned for the app at all.
	Restricted bool `json:"restricted"`

	View        bool `json:"view"`
	Design      bool `json:"design"`
	Publish     bool `json:"publish"`
	Submit      bool `json:"submit"`
	ManageRoles bool `json:"manage_roles"`
	// ReadRecords and ModifyRecords (update and delete) are scoped.
	ReadRecords   Scope `json:"read_records"`
	ModifyRecords Scope `json:"modify_records"`
}

// Has reports whether the user has capability c.
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

// roleCaps is what a set of roles grants, before the space's limit.
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

// spaceLimit is what a user's access to the space allows an app role to give: the ceiling
// that app roles never go over.
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

// defaultRoles are the roles of an unrestricted app for a user whose access to the space
// is sp.
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

// appAccess works out a user's access to an app: the capabilities of roles (nil if the app
// is unrestricted), limited to what sp allows.
//
// admin is whether the user administers the space (see the note on the rules above): it only
// adds to what roles give, view and manage_roles, and never beyond the space's limit.
func appAccess(sp Access, roles []AppRole, restricted, admin bool) AppAccess {
	limit := spaceLimit(sp)
	if !restricted {
		roles = defaultRoles(sp)
	}
	got := roleCaps(roles)
	if admin && limit.ManageRoles {
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

// AppGrant assigns roles of an app to one user, or with a nil UserID to every member of
// the space who has no assignment of their own. Once an app has any assignment, a member
// with neither theirs nor a default holds no role in it, and doesn't see it.
type AppGrant struct {
	UserID *int64    `json:"user_id"`
	Roles  []AppRole `json:"roles"`
}

// grantRow is one stored role assignment.
type grantRow struct {
	userID sql.NullInt64
	role   AppRole
}

// resolve picks the roles that apply to userID among an app's rows: their own if they have
// any, else the default's. anyGrants is whether the app has any assignment at all, anyone's:
// without one the app is unrestricted; with one, a user named nowhere and with no default
// holds no role, so restricting an app never leaves a member it doesn't name (one who joins
// the space later included) with every role.
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

// ErrAppNotFound is for an app that isn't in the space it was asked for in.
var ErrAppNotFound = errors.New("app not found")

// App returns what userID may do in appID of spaceID: ErrNotFound if they can't read the
// space, ErrAppNotFound if the space has no such app. It is the one place that answers
// app-level access; callers act on the answer for requests and for the agent alike.
func (c *Checker) App(ctx context.Context, userID, spaceID, appID int64) (AppAccess, error) {
	sp, err := c.Check(ctx, userID, spaceID)
	if err != nil {
		return AppAccess{}, err
	}
	var exists bool
	if err := c.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM apps WHERE id = ? AND space_id = ?)`,
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
	if err := c.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM app_roles WHERE app_id = ?)`,
		appID).Scan(&anyGrants); err != nil {
		return AppAccess{}, err
	}
	roles, restricted := resolve(rows, userID, anyGrants)
	admin, err := c.administers(ctx, userID, sp)
	if err != nil {
		return AppAccess{}, err
	}
	return appAccess(sp.Access, roles, restricted, admin), nil
}

// administers is whether userID is an administrator of sp: a workspace admin, in a shared space
// (a private space has its owner alone, who holds every role of an app that has none assigned).
func (c *Checker) administers(ctx context.Context, userID int64, sp Space) (bool, error) {
	if sp.Kind != Shared {
		return false, nil
	}
	var admin bool
	err := c.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM memberships WHERE workspace_id = ? AND user_id = ? AND role = 'admin')`,
		sp.WorkspaceID, userID).Scan(&admin)
	return admin, err
}

// AppsIn returns what userID may do in each app of the space, by app ID, for a space they
// can read (as Check returned it), in one query: every app of the space has an entry.
func (c *Checker) AppsIn(ctx context.Context, userID int64, sp Space) (map[int64]AppAccess, error) {
	rows, err := c.db.QueryContext(ctx, `
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
	out := make(map[int64]AppAccess, len(byApp))
	for id, gs := range byApp {
		roles, restricted := resolve(gs, userID, anyGrants[id])
		out[id] = appAccess(sp.Access, roles, restricted, admin)
	}
	return out, nil
}

// grantRows reads the role rows of an app that concern userID (theirs and the default's).
func (c *Checker) grantRows(ctx context.Context, userID, appID int64) ([]grantRow, error) {
	rows, err := c.db.QueryContext(ctx,
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

// maxAppGrants bounds the assignments of one app.
const maxAppGrants = 1000

// Errors of SetAppGrants, for the caller to answer.
var (
	// ErrBadGrants is for assignments that make no sense: an unknown role, an empty role
	// list, a user twice, or too many.
	ErrBadGrants = errors.New("invalid role assignments")
	// ErrGrantUnknownUser is for a user who can't read the space.
	ErrGrantUnknownUser = errors.New("a role can only go to a member of the space")
	// ErrGrantForbidden is for a caller who doesn't manage the app's roles.
	ErrGrantForbidden = errors.New("you can't manage this app's roles")
	// ErrGrantLockout is for assignments that would leave the person making them unable to
	// manage the app's roles.
	ErrGrantLockout = errors.New("you would lose the right to manage this app's roles")
)

// AppGrants lists the role assignments of an app of spaceID: those of single users first,
// by user ID, then the default's.
func (c *Checker) AppGrants(ctx context.Context, spaceID, appID int64) ([]AppGrant, error) {
	rows, err := c.db.QueryContext(ctx, `
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

// SetAppGrants replaces the role assignments of appID of spaceID with grants; an empty list
// makes the app unrestricted again. actorID is the user making the change, who must keep
// the right to manage the app's roles (they would be locked out otherwise: nobody could set
// them right). Every user named must be able to read the space. The actor must manage the
// app's roles now, which is checked under the write lock (ErrGrantForbidden).
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
		if !appAccess(sp.Access, roles, restricted, admin).ManageRoles {
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
