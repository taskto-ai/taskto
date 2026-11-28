package search

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"taskto.ai/taskto/internal/access"
)

type PlaceSpace struct {
	ID   int64       `json:"id"`
	Name string      `json:"name"`
	Kind access.Kind `json:"kind"`
}

type PlaceApp struct {
	SpaceID   int64  `json:"space_id"`
	SpaceName string `json:"space_name"`
	AppID     int64  `json:"app_id"`
	Name      string `json:"name"`
}

type PlaceBoard struct {
	SpaceID   int64  `json:"space_id"`
	SpaceName string `json:"space_name"`
	BoardID   int64  `json:"board_id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
}

type PlaceResults struct {
	Spaces []PlaceSpace `json:"spaces"`
	Apps   []PlaceApp   `json:"apps"`
	Boards []PlaceBoard `json:"boards"`
}

func folded(s string) string {
	s = cases.Fold().String(norm.NFD.String(s))
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Mn, r) {
			return -1
		}
		return r
	}, s)
}

func placeMatches(name, needle string) bool {
	return strings.Contains(folded(name), needle)
}

func sortPlaces[T any](items []T, name func(T) string, needle string, limit int) []T {
	sort.Slice(items, func(i, j int) bool {
		a, b := folded(name(items[i])), folded(name(items[j]))
		ap, bp := strings.HasPrefix(a, needle), strings.HasPrefix(b, needle)
		if ap != bp {
			return ap
		}
		al, bl := utf8.RuneCountInString(a), utf8.RuneCountInString(b)
		if al != bl {
			return al < bl
		}
		return a < b
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

const (
	PaletteDefaultLimit = 5
	PaletteMaxLimit     = 10
)

const paletteLimitMessage = "limit must be a number from 1 to 10"

var errPaletteLimit = fmt.Errorf("%w: %s", ErrInvalid, paletteLimitMessage)

func (x *Index) SearchPlaces(ctx context.Context, checker *access.Checker, userID, workspaceID int64, spaces []access.Space, text string, limit int) (PlaceResults, error) {
	if limit < 1 || limit > PaletteMaxLimit {
		return PlaceResults{}, errPaletteLimit
	}
	if _, err := checkQuery(&Query{Text: text, Limit: limit}); err != nil {
		return PlaceResults{}, err
	}
	needle := folded(strings.TrimSpace(text))
	res := PlaceResults{Spaces: []PlaceSpace{}, Apps: []PlaceApp{}, Boards: []PlaceBoard{}}
	spaceByID := map[int64]access.Space{}
	for _, sp := range spaces {
		if sp.WorkspaceID != workspaceID || !sp.Access.Allows(access.Read) || sp.ArchivedAt != nil {
			continue
		}
		spaceByID[sp.ID] = sp
		if sp.Kind != access.Private && placeMatches(sp.Name, needle) {
			res.Spaces = append(res.Spaces, PlaceSpace{ID: sp.ID, Name: sp.Name, Kind: sp.Kind})
		}
		apps, err := checker.AppsIn(ctx, userID, sp)
		if err != nil {
			return PlaceResults{}, err
		}
		ids := make([]int64, 0, len(apps))
		for id, appAccess := range apps {
			if !appAccess.View && !appAccess.Design {
				continue
			}
			ids = append(ids, id)
		}
		if len(ids) > 0 {
			encoded, err := json.Marshal(ids)
			if err != nil {
				return PlaceResults{}, err
			}
			rows, err := x.db.QueryContext(ctx, `SELECT id, name FROM apps WHERE id IN (SELECT value FROM json_each(?)) AND space_id = ? AND archived_at IS NULL`, string(encoded), sp.ID)
			if err != nil {
				return PlaceResults{}, err
			}
			for rows.Next() {
				var id int64
				var name string
				if err := rows.Scan(&id, &name); err != nil {
					_ = rows.Close()
					return PlaceResults{}, err
				}
				if placeMatches(name, needle) {
					res.Apps = append(res.Apps, PlaceApp{SpaceID: sp.ID, SpaceName: sp.Name, AppID: id, Name: name})
				}
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return PlaceResults{}, err
			}
			if err := rows.Close(); err != nil {
				return PlaceResults{}, err
			}
		}
	}
	ids := make([]int64, 0, len(spaceByID))
	for id := range spaceByID {
		ids = append(ids, id)
	}
	if len(ids) > 0 {
		encoded, err := json.Marshal(ids)
		if err != nil {
			return PlaceResults{}, err
		}
		rows, err := x.db.QueryContext(ctx, `SELECT id, space_id, name, kind FROM boards WHERE space_id IN (SELECT value FROM json_each(?)) AND archived_at IS NULL`, string(encoded))
		if err != nil {
			return PlaceResults{}, err
		}
		for rows.Next() {
			var id, spaceID int64
			var name, kind string
			if err := rows.Scan(&id, &spaceID, &name, &kind); err != nil {
				_ = rows.Close()
				return PlaceResults{}, err
			}
			if placeMatches(name, needle) {
				sp := spaceByID[spaceID]
				res.Boards = append(res.Boards, PlaceBoard{SpaceID: spaceID, SpaceName: sp.Name, BoardID: id, Name: name, Kind: kind})
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return PlaceResults{}, err
		}
		if err := rows.Close(); err != nil {
			return PlaceResults{}, err
		}
	}
	if x.afterPlacesRead != nil {
		x.afterPlacesRead()
	}
	readable, err := x.readablePlaces(ctx, checker, userID, workspaceID, spaces)
	if err != nil {
		return PlaceResults{}, err
	}
	res.Spaces = slices.DeleteFunc(res.Spaces, func(p PlaceSpace) bool { return !readable.spaces[p.ID] })
	res.Apps = slices.DeleteFunc(res.Apps, func(p PlaceApp) bool { return !readable.spaces[p.SpaceID] || !readable.apps[p.AppID] })
	res.Boards = slices.DeleteFunc(res.Boards, func(p PlaceBoard) bool { return !readable.spaces[p.SpaceID] })
	res.Spaces = sortPlaces(res.Spaces, func(p PlaceSpace) string { return p.Name }, needle, limit)
	res.Apps = sortPlaces(res.Apps, func(p PlaceApp) string { return p.Name }, needle, limit)
	res.Boards = sortPlaces(res.Boards, func(p PlaceBoard) string { return p.Name }, needle, limit)
	return res, nil
}

type readablePlaces struct {
	spaces map[int64]bool
	apps   map[int64]bool
}

func (x *Index) readablePlaces(ctx context.Context, checker *access.Checker, userID, workspaceID int64, spaces []access.Space) (readablePlaces, error) {
	out := readablePlaces{spaces: map[int64]bool{}, apps: map[int64]bool{}}
	err := checker.Snapshot(ctx, func(snap *access.Checker, _ *sql.Tx) error {
		for _, was := range spaces {
			cur, err := snap.Check(ctx, userID, was.ID)
			switch {
			case errors.Is(err, access.ErrNotFound):
				continue
			case err != nil:
				return err
			}
			if cur.WorkspaceID != workspaceID || !cur.Access.Allows(access.Read) || cur.ArchivedAt != nil ||
				(was.Generation != 0 && cur.Generation != was.Generation) {
				continue
			}
			out.spaces[was.ID] = true
			if x.betweenPlaceChecks != nil {
				x.betweenPlaceChecks()
			}
			apps, err := snap.AppsIn(ctx, userID, cur)
			if err != nil {
				return err
			}
			for id, appAccess := range apps {
				out.apps[id] = appAccess.View || appAccess.Design
			}
		}
		return nil
	})
	return out, err
}
