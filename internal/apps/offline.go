package apps

import (
	"context"
	"net/http"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
)

const MaxOfflineRows = 2000

type OfflineScreen struct {
	Name   string `json:"name"`
	Record bool   `json:"record,omitempty"`
}

type OfflineAction struct {
	Kind   string `json:"kind"`
	Screen string `json:"screen,omitempty"`
	Then   string `json:"then,omitempty"`
}

type OfflineDefinition struct {
	DefinitionVersion int                      `json:"definition_version"`
	Requires          []string                 `json:"requires" apitypes:"nonnull"`
	Collection        Collection               `json:"collection"`
	Entry             string                   `json:"entry,omitempty"`
	Screens           []OfflineScreen          `json:"screens,omitempty"`
	Actions           map[string]OfflineAction `json:"actions" apitypes:"nonnull"`
	Summaries         map[string]Summary       `json:"summaries,omitempty"`
	Series            map[string]Series        `json:"series,omitempty"`
}

type OfflineApp struct {
	AppID      int64             `json:"app_id"`
	SpaceID    int64             `json:"space_id"`
	Name       string            `json:"name"`
	Release    int64             `json:"release"`
	Definition OfflineDefinition `json:"definition"`
	Messages   []Message         `json:"messages" apitypes:"json,nonnull"`
	Scope      string            `json:"scope"`
	Records    int               `json:"records"`
	Total      int               `json:"total"`
	Truncated  bool              `json:"truncated"`
	CanInsert  bool              `json:"can_insert"`
	CapturedAt string            `json:"captured_at"`
	Operations map[string]int64  `json:"operations" apitypes:"nonnull"`
	Modifiable []int64           `json:"modifiable" apitypes:"nonnull"`
}

func (s *Service) offlineApp(ctx context.Context, sp access.Space, actor Actor, appID int64) (OfflineApp, error) {
	if err := may(sp, access.Read); err != nil {
		return OfflineApp{}, err
	}
	acc, err := s.appAccess(ctx, actor, sp.ID, appID)
	if err != nil {
		return OfflineApp{}, err
	}
	if err := need(acc, access.CapView); err != nil {
		return OfflineApp{}, err
	}
	if !sp.OfflineDownloads {
		return OfflineApp{}, apierr.New(http.StatusForbidden, apierr.CodeOfflineDisabled, "an admin turned off offline downloads for this space")
	}
	a, err := launchable(ctx, s.db, sp.ID, appID)
	if err != nil {
		return OfflineApp{}, err
	}
	if a.liveDef == nil || a.Live == nil {
		return OfflineApp{}, ErrNotPublished
	}
	def := *a.liveDef
	vis := readable(acc, actor.UserID)
	rows, err := loadRows(ctx, s.db, appID, vis)
	if err != nil {
		return OfflineApp{}, err
	}
	total := len(rows)
	if total > MaxOfflineRows {
		rows = rows[total-MaxOfflineRows:]
	}
	scope := "own"
	if vis.scope == access.ScopeAll {
		scope = "all"
	}
	out := OfflineApp{
		AppID: a.ID, SpaceID: sp.ID, Name: a.Name,
		Release:    a.Live.Number,
		Definition: offlineDefinition(def),
		Messages:   Surface(SurfaceID(a.ID), def, rows),
		Scope:      scope,
		Records:    len(rows), Total: total, Truncated: total > len(rows),
		CanInsert:  acc.Submit && may(sp, access.Write) == nil,
		CapturedAt: now(),
	}
	if out.Operations, err = s.operationsOf(ctx, appID, actor.UserID, rows); err != nil {
		return OfflineApp{}, err
	}
	if out.Modifiable, err = s.modifiableOf(ctx, sp, appID, actor.UserID, acc.ModifyRecords, rows); err != nil {
		return OfflineApp{}, err
	}
	return out, nil
}

func (s *Service) modifiableOf(ctx context.Context, sp access.Space, appID, userID int64, scope access.Scope, rows []Row) ([]int64, error) {
	out := []int64{}
	if may(sp, access.Write) != nil {
		return out, nil
	}
	switch scope {
	case access.ScopeAll:
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out, nil
	case access.ScopeOwn:
	default:
		return out, nil
	}
	in := make(map[int64]bool, len(rows))
	for _, r := range rows {
		in[r.ID] = true
	}
	found, err := s.db.QueryContext(ctx, `SELECT id FROM app_rows WHERE app_id = ? AND deleted_at IS NULL AND created_by = ? ORDER BY id`, appID, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = found.Close() }()
	for found.Next() {
		var id int64
		if err := found.Scan(&id); err != nil {
			return nil, err
		}
		if in[id] {
			out = append(out, id)
		}
	}
	return out, found.Err()
}

func (s *Service) operationsOf(ctx context.Context, appID, userID int64, rows []Row) (map[string]int64, error) {
	out := map[string]int64{}
	if len(rows) == 0 {
		return out, nil
	}
	in := make(map[int64]bool, len(rows))
	for _, r := range rows {
		in[r.ID] = true
	}
	found, err := s.db.QueryContext(ctx, `SELECT operation, record_id FROM app_operations
		WHERE app_id = ? AND user_id = ? AND record_id IS NOT NULL`, appID, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = found.Close() }()
	for found.Next() {
		var key string
		var id int64
		if err := found.Scan(&key, &id); err != nil {
			return nil, err
		}
		if in[id] {
			out[key] = id
		}
	}
	return out, found.Err()
}

func offlineDefinition(def Definition) OfflineDefinition {
	out := OfflineDefinition{
		DefinitionVersion: def.DefinitionVersion,
		Requires:          def.Requires,
		Collection:        def.Collection,
		Entry:             def.Entry,
		Actions:           map[string]OfflineAction{},
		Summaries:         def.Summaries,
		Series:            def.Series,
	}
	if out.Requires == nil {
		out.Requires = []string{}
	}
	for _, sc := range def.Screens {
		out.Screens = append(out.Screens, OfflineScreen{Name: sc.Name, Record: sc.Record})
	}
	for name, act := range def.Actions {
		out.Actions[name] = OfflineAction{Kind: act.Kind, Screen: act.Screen, Then: act.Then}
	}
	return out
}

func (s *Service) handleOffline(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	out, err := s.offlineApp(r.Context(), req.space, req.actor, req.app)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
