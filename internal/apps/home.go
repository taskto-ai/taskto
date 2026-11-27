package apps

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
)

const (
	MaxHomeWidgets    = 24
	MaxHomePins       = 20
	MaxDefaultWidgets = 12
	MaxDefaultPins    = 8
)

const (
	KindWidget = "widget"
	KindPin    = "pin"

	SourcePersonal = "personal"
	SourceDefault  = "default"

	StateReady       = "ready"
	StateUnavailable = "unavailable"

	ReasonOffline     = "offline"
	ReasonUnpublished = "unpublished"
	ReasonRemoved     = "removed"
)

type HomeItem struct {
	ID        int64        `json:"id,omitempty"`
	DefaultID int64        `json:"default_id,omitempty"`
	Source    string       `json:"source"`
	Kind      string       `json:"kind"`
	SpaceID   int64        `json:"space_id"`
	AppID     int64        `json:"app_id"`
	AppName   string       `json:"app_name"`
	WidgetID  string       `json:"widget_id,omitempty"`
	Widget    *Widget      `json:"widget,omitempty"`
	Label     string       `json:"label,omitempty"`
	Screen    string       `json:"screen,omitempty"`
	State     string       `json:"state"`
	Reason    string       `json:"reason,omitempty"`
	Release   *HomeRelease `json:"release,omitempty"`
	CanManage bool         `json:"can_manage,omitempty"`
}

type HomeRelease struct {
	Number            int64    `json:"number"`
	DefinitionVersion int      `json:"definition_version"`
	Requires          []string `json:"requires" apitypes:"nonnull"`
}

type HomeView struct {
	Widgets []HomeItem `json:"widgets" apitypes:"nonnull"`
	Pins    []HomeItem `json:"pins" apitypes:"nonnull"`
	Hidden  []HomeItem `json:"hidden" apitypes:"nonnull"`
	Limits  struct {
		Widgets int `json:"widgets"`
		Pins    int `json:"pins"`
	} `json:"limits"`
}

type NewHomeItem struct {
	Kind    string `json:"kind"`
	SpaceID int64  `json:"space_id"`
	AppID   int64  `json:"app_id"`
	Widget  string `json:"widget"`
}

type AvailableApp struct {
	SpaceID        int64              `json:"space_id"`
	SpaceName      string             `json:"space_name"`
	SpaceKind      string             `json:"space_kind"`
	AppID          int64              `json:"app_id"`
	Name           string             `json:"name"`
	Widgets        []Widget           `json:"widgets" apitypes:"nonnull"`
	Pin            *Pin               `json:"pin"`
	CanSetDefaults bool               `json:"can_set_defaults"`
	Defaults       []AvailableDefault `json:"defaults" apitypes:"nonnull"`
}

type AvailableDefault struct {
	ID     int64  `json:"id"`
	Kind   string `json:"kind"`
	Widget string `json:"widget,omitempty"`
}

func (s *Service) HomeRoutes(mux *http.ServeMux, a *auth.Service) {
	member := func(h http.HandlerFunc) http.Handler { return a.RequireMember(h) }
	read := func(h http.HandlerFunc) http.Handler { return s.access.RequireSpace(access.Read, h) }
	write := func(h http.HandlerFunc) http.Handler { return s.access.RequireSpace(access.Write, h) }
	const home = "/api/workspaces/{workspace}/home"
	const space = "/api/workspaces/{workspace}/spaces/{space}/home/defaults"

	mux.Handle("GET "+home, member(s.handleHome))
	mux.Handle("GET "+home+"/available", member(s.handleAvailable))
	mux.Handle("POST "+home+"/items", member(s.handleHomeAdd))
	mux.Handle("DELETE "+home+"/items/{item}", member(s.handleHomeRemove))
	mux.Handle("PUT "+home+"/order", member(s.handleHomeOrder))
	mux.Handle("PUT "+home+"/hidden/{default}", member(s.handleHide))
	mux.Handle("DELETE "+home+"/hidden/{default}", member(s.handleUnhide))
	mux.Handle("GET "+space, read(s.handleDefaults))
	mux.Handle("POST "+space, write(s.handleDefaultAdd))
	mux.Handle("DELETE "+space+"/{default}", write(s.handleDefaultRemove))
}

func homeCaller(r *http.Request) (user, workspace int64) {
	u, _ := auth.UserFrom(r.Context())
	m, _ := auth.MembershipFrom(r.Context())
	return u.ID, m.WorkspaceID
}

type rawHome struct {
	id, defaultID  int64
	kind, widget   string
	hidden         bool
	appID, spaceID int64
	appName        string
	archived       bool
	live           sql.NullInt64
	defVersion     sql.NullInt64
	requires       sql.NullString
	contributions  sql.NullString
}

const homeAppColumns = `a.id, a.space_id, a.name, a.archived_at IS NOT NULL, a.live_release,
	r.definition_version, r.requires, json_extract(r.definition, '$.contributions')`
const (
	homeItemFrom = `
	FROM home_items i
	JOIN apps a ON a.id = i.app_id
	LEFT JOIN app_releases r ON r.app_id = a.id AND r.number = a.live_release`
	homeDefaultFrom = `
	FROM home_defaults d
	JOIN apps a ON a.id = d.app_id
	LEFT JOIN app_releases r ON r.app_id = a.id AND r.number = a.live_release`
)

func (r *rawHome) scanApp(row scanner, dest ...any) error {
	return row.Scan(append(dest, &r.appID, &r.spaceID, &r.appName, &r.archived, &r.live, &r.defVersion, &r.requires, &r.contributions)...)
}

func (s *Service) loadHome(ctx context.Context, userID, workspaceID, only int64) ([]rawHome, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, i.kind, i.widget, `+homeAppColumns+`
		`+homeItemFrom+`
		JOIN spaces sp ON sp.id = a.space_id AND sp.workspace_id = ?
		WHERE i.user_id = ? AND (? = 0 OR i.id = ?)
		ORDER BY i.kind, i.position, i.id`, workspaceID, userID, only, only)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []rawHome
	for rows.Next() {
		var r rawHome
		if err := r.scanApp(rows, &r.id, &r.kind, &r.widget); err != nil {
			return nil, err
		}
		all = append(all, r)
	}
	return all, rows.Err()
}

func (s *Service) loadDefaults(ctx context.Context, userID, workspaceID, spaceID, only int64) ([]rawHome, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.id, d.kind, d.widget, EXISTS (SELECT 1 FROM home_hidden h WHERE h.default_id = d.id AND h.user_id = ?), `+homeAppColumns+`
		`+homeDefaultFrom+`
		JOIN spaces sp ON sp.id = a.space_id AND sp.workspace_id = ? AND sp.kind = 'shared'
		WHERE d.space_id = a.space_id AND (? = 0 OR a.space_id = ?) AND (? = 0 OR d.id = ?)
		ORDER BY d.id`, userID, workspaceID, spaceID, spaceID, only, only)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []rawHome
	for rows.Next() {
		var r rawHome
		if err := r.scanApp(rows, &r.defaultID, &r.kind, &r.widget, &r.hidden); err != nil {
			return nil, err
		}
		all = append(all, r)
	}
	return all, rows.Err()
}

type homeResolver struct {
	s      *Service
	ctx    context.Context
	userID int64
	spaces map[int64]map[int64]access.AppAccess
}

func (s *Service) resolver(ctx context.Context, userID int64) *homeResolver {
	return &homeResolver{s: s, ctx: ctx, userID: userID, spaces: map[int64]map[int64]access.AppAccess{}}
}

func (h *homeResolver) access(spaceID, appID int64) (acc access.AppAccess, ok bool, err error) {
	accs, cached := h.spaces[spaceID]
	if !cached {
		sp, err := h.s.access.Check(h.ctx, h.userID, spaceID)
		switch {
		case errors.Is(err, access.ErrNotFound):
			h.spaces[spaceID] = nil
			return access.AppAccess{}, false, nil
		case err != nil:
			return access.AppAccess{}, false, err
		}
		if accs, err = h.s.access.AppsIn(h.ctx, h.userID, sp); err != nil {
			return access.AppAccess{}, false, err
		}
		h.spaces[spaceID] = accs
	}
	acc, found := accs[appID]
	return acc, found && acc.View, nil
}

func (h *homeResolver) resolve(raw rawHome) (item HomeItem, acc access.AppAccess, ok bool, err error) {
	acc, ok, err = h.access(raw.spaceID, raw.appID)
	if err != nil || !ok {
		return HomeItem{}, acc, false, err
	}
	item = HomeItem{
		ID: raw.id, DefaultID: raw.defaultID, Source: SourcePersonal, Kind: raw.kind, SpaceID: raw.spaceID, AppID: raw.appID,
		AppName: raw.appName, WidgetID: raw.widget, State: StateUnavailable,
	}
	if raw.defaultID != 0 {
		item.ID, item.Source = 0, SourceDefault
	}
	switch {
	case raw.archived:
		item.Reason = ReasonOffline
		return item, acc, true, nil
	case !raw.live.Valid:
		item.Reason = ReasonUnpublished
		return item, acc, true, nil
	}
	var c *Contributions
	if raw.contributions.Valid {
		var parsed Contributions
		if json.Unmarshal([]byte(raw.contributions.String), &parsed) == nil {
			c = &parsed
		}
	}
	if raw.kind == KindWidget {
		w, found := c.widget(raw.widget)
		if !found {
			item.Reason = ReasonRemoved
			return item, acc, true, nil
		}
		item.Widget = &w
	} else {
		item.Label = raw.appName
		if c != nil && c.Pin != nil {
			item.Label, item.Screen = c.Pin.Label, c.Pin.Screen
		}
	}
	rel := HomeRelease{Number: raw.live.Int64, DefinitionVersion: int(raw.defVersion.Int64), Requires: []string{}}
	if raw.requires.Valid {
		_ = json.Unmarshal([]byte(raw.requires.String), &rel.Requires)
	}
	item.State, item.Release = StateReady, &rel
	return item, acc, true, nil
}

func (s *Service) homeView(ctx context.Context, userID, workspaceID int64) (HomeView, error) {
	view := HomeView{Widgets: []HomeItem{}, Pins: []HomeItem{}, Hidden: []HomeItem{}}
	view.Limits.Widgets, view.Limits.Pins = MaxHomeWidgets, MaxHomePins
	own, err := s.loadHome(ctx, userID, workspaceID, 0)
	if err != nil {
		return HomeView{}, err
	}
	defaults, err := s.loadDefaults(ctx, userID, workspaceID, 0, 0)
	if err != nil {
		return HomeView{}, err
	}
	res := s.resolver(ctx, userID)
	type key struct {
		app  int64
		kind string
		w    string
	}
	placed := map[key]bool{}
	var mine []HomeItem
	for _, raw := range own {
		item, _, ok, err := res.resolve(raw)
		if err != nil {
			return HomeView{}, err
		}
		if ok {
			placed[key{raw.appID, raw.kind, raw.widget}] = true
			mine = append(mine, item)
		}
	}
	add := func(item HomeItem) {
		if item.Kind == KindWidget {
			view.Widgets = append(view.Widgets, item)
		} else {
			view.Pins = append(view.Pins, item)
		}
	}
	for _, raw := range defaults {
		item, _, ok, err := res.resolve(raw)
		if err != nil {
			return HomeView{}, err
		}
		switch {
		case !ok || placed[key{raw.appID, raw.kind, raw.widget}]:
		case raw.hidden:
			view.Hidden = append(view.Hidden, item)
		default:
			add(item)
		}
	}
	for _, item := range mine {
		add(item)
	}
	return view, nil
}

func (s *Service) checkNewItem(ctx context.Context, userID, workspaceID int64, in NewHomeItem) (access.Space, access.AppAccess, error) {
	switch {
	case in.Kind == KindWidget && in.Widget == "", in.Kind == KindPin && in.Widget != "", in.Kind != KindWidget && in.Kind != KindPin:
		return access.Space{}, access.AppAccess{}, invalidf(`a widget names its widget; a pin has none: {"kind": "widget" or "pin", "space_id", "app_id", "widget"}`)
	}
	sp, err := s.access.Check(ctx, userID, in.SpaceID)
	if errors.Is(err, access.ErrNotFound) || (err == nil && sp.WorkspaceID != workspaceID) {
		return access.Space{}, access.AppAccess{}, ErrNotFound
	}
	if err != nil {
		return access.Space{}, access.AppAccess{}, err
	}
	acc, err := s.appAccess(ctx, Actor{UserID: userID}, sp.ID, in.AppID)
	if err != nil {
		return access.Space{}, access.AppAccess{}, err
	}
	a, err := launchable(ctx, s.db, sp.ID, in.AppID)
	if err != nil {
		return access.Space{}, access.AppAccess{}, err
	}
	if a.liveDef == nil {
		return access.Space{}, access.AppAccess{}, ErrNotPublished
	}
	if in.Kind == KindWidget {
		if _, ok := a.liveDef.Contributions.widget(in.Widget); !ok {
			return access.Space{}, access.AppAccess{}, apierr.New(http.StatusNotFound, apierr.CodeHomeWidgetNotFound, "the app offers no such widget")
		}
	}
	return sp, acc, nil
}

func (s *Service) addHomeItem(ctx context.Context, userID, workspaceID int64, in NewHomeItem) (HomeItem, error) {
	if _, _, err := s.checkNewItem(ctx, userID, workspaceID, in); err != nil {
		return HomeItem{}, err
	}
	limit := MaxHomeWidgets
	if in.Kind == KindPin {
		limit = MaxHomePins
	}
	var id int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		if _, _, err := s.checkNewItem(ctx, userID, workspaceID, in); err != nil {
			return err
		}
		err := tx.QueryRowContext(ctx, `SELECT id FROM home_items WHERE user_id = ? AND app_id = ? AND kind = ? AND widget = ?`,
			userID, in.AppID, in.Kind, in.Widget).Scan(&id)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `
			SELECT count(*) FROM home_items i JOIN apps a ON a.id = i.app_id JOIN spaces sp ON sp.id = a.space_id
			WHERE i.user_id = ? AND i.kind = ? AND sp.workspace_id = ?`, userID, in.Kind, workspaceID).Scan(&n); err != nil {
			return err
		}
		if n >= limit {
			return apierr.New(http.StatusConflict, apierr.CodeHomeLimit, fmt.Sprintf("at most %d of these", limit), apierr.Params{"max": limit})
		}
		return tx.QueryRowContext(ctx, `
			INSERT INTO home_items (user_id, app_id, kind, widget, position)
			VALUES (?, ?, ?, ?, (SELECT coalesce(max(position) + 1, 0) FROM home_items WHERE user_id = ? AND kind = ?))
			RETURNING id`, userID, in.AppID, in.Kind, in.Widget, userID, in.Kind).Scan(&id)
	})
	if err != nil {
		return HomeItem{}, err
	}
	return s.placed(ctx, userID, workspaceID, id)
}

func (s *Service) placed(ctx context.Context, userID, workspaceID, id int64) (HomeItem, error) {
	raws, err := s.loadHome(ctx, userID, workspaceID, id)
	if err != nil {
		return HomeItem{}, err
	}
	if len(raws) == 0 {
		return HomeItem{}, ErrNotFound
	}
	item, _, ok, err := s.resolver(ctx, userID).resolve(raws[0])
	if err != nil {
		return HomeItem{}, err
	}
	if !ok {
		return HomeItem{}, ErrNotFound
	}
	return item, nil
}

func (s *Service) removeHomeItem(ctx context.Context, userID, workspaceID, id int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			DELETE FROM home_items WHERE id = ? AND user_id = ?
			  AND app_id IN (SELECT a.id FROM apps a JOIN spaces sp ON sp.id = a.space_id WHERE sp.workspace_id = ?)`,
			id, userID, workspaceID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func (s *Service) orderHome(ctx context.Context, userID, workspaceID int64, kind string, ids []int64) error {
	if kind != KindWidget && kind != KindPin {
		return invalidf("kind is widget or pin")
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT i.id FROM home_items i JOIN apps a ON a.id = i.app_id JOIN spaces sp ON sp.id = a.space_id
			WHERE i.user_id = ? AND i.kind = ? AND sp.workspace_id = ? ORDER BY i.position, i.id`, userID, kind, workspaceID)
		if err != nil {
			return err
		}
		var current []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			current = append(current, id)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		seen := map[int64]bool{}
		for _, id := range ids {
			if !slices.Contains(current, id) || seen[id] {
				return apierr.New(http.StatusBadRequest, apierr.CodeHomeOrderInvalid, "the ids are not your placements of that kind")
			}
			seen[id] = true
		}
		next := slices.Clone(ids)
		for _, id := range current {
			if !seen[id] {
				next = append(next, id)
			}
		}
		for pos, id := range next {
			if _, err := tx.ExecContext(ctx, `UPDATE home_items SET position = ? WHERE id = ? AND user_id = ?`, pos, id, userID); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Service) hideDefault(ctx context.Context, userID, workspaceID, defaultID int64, hide bool) error {
	raws, err := s.loadDefaults(ctx, userID, workspaceID, 0, defaultID)
	if err != nil {
		return err
	}
	if len(raws) == 0 {
		return ErrNotFound
	}
	if _, ok, err := s.resolver(ctx, userID).access(raws[0].spaceID, raws[0].appID); err != nil {
		return err
	} else if !ok {
		return ErrNotFound
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		if hide {
			_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO home_hidden (user_id, default_id) VALUES (?, ?)`, userID, defaultID)
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM home_hidden WHERE user_id = ? AND default_id = ?`, userID, defaultID)
		return err
	})
}

func (s *Service) listDefaults(ctx context.Context, userID int64, sp access.Space) ([]HomeItem, error) {
	out := []HomeItem{}
	if sp.Kind != access.Shared {
		return out, nil
	}
	raws, err := s.loadDefaults(ctx, userID, sp.WorkspaceID, sp.ID, 0)
	if err != nil {
		return nil, err
	}
	res := s.resolver(ctx, userID)
	for _, raw := range raws {
		item, acc, ok, err := res.resolve(raw)
		if err != nil {
			return nil, err
		}
		if ok {
			item.CanManage = acc.Publish && sp.Access.Allows(access.Write)
			out = append(out, item)
		}
	}
	return out, nil
}

func (s *Service) addDefault(ctx context.Context, sp access.Space, userID int64, in NewHomeItem) (HomeItem, error) {
	if sp.Kind != access.Shared {
		return HomeItem{}, apierr.New(http.StatusConflict, apierr.CodeHomeDefaultsShared, "defaults are for the apps of shared spaces")
	}
	if err := may(sp, access.Write); err != nil {
		return HomeItem{}, err
	}
	in.SpaceID = sp.ID
	limit := MaxDefaultWidgets
	if in.Kind == KindPin {
		limit = MaxDefaultPins
	}
	var id int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		acc, err := s.recheckApp(ctx, Actor{UserID: userID}, sp.ID, in.AppID)
		if err != nil {
			return err
		}
		if err := need(acc, access.CapPublish); err != nil {
			return err
		}
		if _, _, err := s.checkNewItem(ctx, userID, sp.WorkspaceID, in); err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, `SELECT id FROM home_defaults WHERE app_id = ? AND kind = ? AND widget = ?`, in.AppID, in.Kind, in.Widget).Scan(&id)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM home_defaults WHERE space_id = ? AND kind = ?`, sp.ID, in.Kind).Scan(&n); err != nil {
			return err
		}
		if n >= limit {
			return apierr.New(http.StatusConflict, apierr.CodeHomeLimit, fmt.Sprintf("at most %d of these", limit), apierr.Params{"max": limit})
		}
		return tx.QueryRowContext(ctx, `
			INSERT INTO home_defaults (space_id, app_id, kind, widget, set_by) VALUES (?, ?, ?, ?, ?) RETURNING id`,
			sp.ID, in.AppID, in.Kind, in.Widget, userID).Scan(&id)
	})
	if err != nil {
		return HomeItem{}, err
	}
	raws, err := s.loadDefaults(ctx, userID, sp.WorkspaceID, sp.ID, id)
	if err != nil || len(raws) == 0 {
		return HomeItem{}, errors.Join(err, ErrNotFound)
	}
	item, acc, ok, err := s.resolver(ctx, userID).resolve(raws[0])
	if err != nil || !ok {
		return HomeItem{}, errors.Join(err, ErrNotFound)
	}
	item.CanManage = acc.Publish
	return item, nil
}

func (s *Service) removeDefault(ctx context.Context, sp access.Space, userID, id int64) error {
	if err := may(sp, access.Write); err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var appID int64
		err := tx.QueryRowContext(ctx, `SELECT app_id FROM home_defaults WHERE id = ? AND space_id = ?`, id, sp.ID).Scan(&appID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		acc, err := s.recheckApp(ctx, Actor{UserID: userID}, sp.ID, appID)
		if err != nil {
			return err
		}
		if err := need(acc, access.CapPublish); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM home_defaults WHERE id = ? AND space_id = ?`, id, sp.ID)
		return err
	})
}

func (s *Service) available(ctx context.Context, userID, workspaceID int64) ([]AvailableApp, error) {
	spaces, err := s.access.Readable(ctx, userID, workspaceID)
	if err != nil {
		return nil, err
	}
	out := []AvailableApp{}
	for _, sp := range spaces {
		accs, err := s.access.AppsIn(ctx, userID, sp)
		if err != nil {
			return nil, err
		}
		defaults := map[int64][]AvailableDefault{}
		if sp.Kind == access.Shared {
			rows, err := s.db.QueryContext(ctx, `SELECT id, app_id, kind, widget FROM home_defaults WHERE space_id = ? ORDER BY id`, sp.ID)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var app int64
				var d AvailableDefault
				if err := rows.Scan(&d.ID, &app, &d.Kind, &d.Widget); err != nil {
					_ = rows.Close()
					return nil, err
				}
				defaults[app] = append(defaults[app], d)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return nil, err
			}
		}
		rows, err := s.db.QueryContext(ctx, `
			SELECT a.id, a.name, json_extract(r.definition, '$.contributions')
			FROM apps a JOIN app_releases r ON r.app_id = a.id AND r.number = a.live_release
			WHERE a.space_id = ? AND a.archived_at IS NULL ORDER BY a.name, a.id`, sp.ID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var name string
			var contributions sql.NullString
			if err := rows.Scan(&id, &name, &contributions); err != nil {
				_ = rows.Close()
				return nil, err
			}
			acc := accs[id]
			if !acc.View {
				continue
			}
			app := AvailableApp{
				SpaceID: sp.ID, SpaceName: sp.Name, SpaceKind: string(sp.Kind), AppID: id, Name: name, Widgets: []Widget{},
				CanSetDefaults: sp.Kind == access.Shared && sp.Access.Allows(access.Write) && acc.Publish,
				Defaults:       []AvailableDefault{},
			}
			if app.CanSetDefaults {
				app.Defaults = append(app.Defaults, defaults[id]...)
			}
			if contributions.Valid {
				var c Contributions
				if json.Unmarshal([]byte(contributions.String), &c) == nil {
					app.Widgets = append(app.Widgets, c.Widgets...)
					app.Pin = c.Pin
				}
			}
			out = append(out, app)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Service) widgetSurface(ctx context.Context, sp access.Space, actor Actor, appID int64, widget string) ([]Message, error) {
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
	if a.liveDef == nil {
		return nil, ErrNotPublished
	}
	w, ok := a.liveDef.Contributions.widget(widget)
	if !ok {
		return nil, apierr.New(http.StatusNotFound, apierr.CodeHomeWidgetNotFound, "the app offers no such widget")
	}
	var rows []Row
	if w.Kind != WidgetKindShortcut {
		if rows, err = loadRows(ctx, s.db, appID, readable(acc, actor.UserID)); err != nil {
			return nil, err
		}
	}
	return WidgetSurface(SurfaceID(a.ID), *a.liveDef, w, rows), nil
}

func (s *Service) handleHome(w http.ResponseWriter, r *http.Request) {
	user, workspace := homeCaller(r)
	view, err := s.homeView(r.Context(), user, workspace)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Service) handleAvailable(w http.ResponseWriter, r *http.Request) {
	user, workspace := homeCaller(r)
	all, err := s.available(r.Context(), user, workspace)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]AvailableApp{"apps": all})
}

func (s *Service) handleHomeAdd(w http.ResponseWriter, r *http.Request) {
	user, workspace := homeCaller(r)
	var in NewHomeItem
	if !readJSON(w, r, &in) {
		return
	}
	item, err := s.addHomeItem(r.Context(), user, workspace, in)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusNotFound, apierr.CodeNotFound, ErrNotFound.Error())
		return 0, false
	}
	return id, true
}

func (s *Service) handleHomeRemove(w http.ResponseWriter, r *http.Request) {
	user, workspace := homeCaller(r)
	id, ok := pathID(w, r, "item")
	if !ok {
		return
	}
	if err := s.removeHomeItem(r.Context(), user, workspace, id); err != nil {
		fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleHomeOrder(w http.ResponseWriter, r *http.Request) {
	user, workspace := homeCaller(r)
	var body struct {
		Kind string  `json:"kind"`
		IDs  []int64 `json:"ids"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if err := s.orderHome(r.Context(), user, workspace, body.Kind, body.IDs); err != nil {
		fail(w, r, err)
		return
	}
	s.handleHome(w, r)
}

func (s *Service) handleHide(w http.ResponseWriter, r *http.Request)   { s.hide(w, r, true) }
func (s *Service) handleUnhide(w http.ResponseWriter, r *http.Request) { s.hide(w, r, false) }

func (s *Service) hide(w http.ResponseWriter, r *http.Request, hide bool) {
	user, workspace := homeCaller(r)
	id, ok := pathID(w, r, "default")
	if !ok {
		return
	}
	if err := s.hideDefault(r.Context(), user, workspace, id, hide); err != nil {
		fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleDefaults(w http.ResponseWriter, r *http.Request) {
	req, _ := parse(w, r, false)
	all, err := s.listDefaults(r.Context(), req.actor.UserID, req.space)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]HomeItem{"defaults": all})
}

func (s *Service) handleDefaultAdd(w http.ResponseWriter, r *http.Request) {
	req, _ := parse(w, r, false)
	var in NewHomeItem
	if !readJSON(w, r, &in) {
		return
	}
	item, err := s.addDefault(r.Context(), req.space, req.actor.UserID, in)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Service) handleDefaultRemove(w http.ResponseWriter, r *http.Request) {
	req, _ := parse(w, r, false)
	id, ok := pathID(w, r, "default")
	if !ok {
		return
	}
	if err := s.removeDefault(r.Context(), req.space, req.actor.UserID, id); err != nil {
		fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
