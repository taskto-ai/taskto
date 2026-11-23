package apps

import (
	"context"
	"net/http"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
)

// Offline use (docs/offline.md).
//
// A person who may open an app can download it for offline use: its live release, as much as a
// client needs to draw and use it, and the records they may read. The download is a snapshot: it
// says the release it was taken from and when, and what it holds is what the caller reads and
// nothing more (the rows through the same filter as every screen, without deleted ones, and each
// as the screen shows it: no field the collection doesn't declare, none of another type). The
// server keeps nothing of it: it is the client's copy, and the client's writes made against it are
// the app's ordinary actions, sent again with their operation keys (outbox.go, Service.act), which
// make a retried one apply once.
//
// An admin may turn downloads off for a shared space (Space.OfflineDownloads), for a space whose
// data shouldn't stay on devices. Turning it off refuses new downloads; what a device already
// holds is removed by that device when it next asks the server and is refused.

// MaxOfflineRows bounds the records of a download: the latest ones, when the app holds more. The
// download says how many the caller reads in all, so a client never presents its copy as the
// whole.
const MaxOfflineRows = 2000

// OfflineScreen is a screen of a downloaded app: its name, and whether it shows one record.
type OfflineScreen struct {
	Name   string `json:"name"`
	Record bool   `json:"record,omitempty"`
}

// OfflineAction is what an action of a downloaded app does, and nothing of what it sets going on
// the server (the effects are the server's to do, when the record arrives).
type OfflineAction struct {
	Kind   string `json:"kind"`
	Screen string `json:"screen,omitempty"`
	Then   string `json:"then,omitempty"`
}

// OfflineDefinition is the part of a definition a client needs to use a downloaded app: the
// format's version and what it requires of a client, the collection, the actions' kinds, the
// summaries and the screens. The components are in the download's messages.
type OfflineDefinition struct {
	DefinitionVersion int                      `json:"definition_version"`
	Requires          []string                 `json:"requires"`
	Collection        Collection               `json:"collection"`
	Entry             string                   `json:"entry,omitempty"`
	Screens           []OfflineScreen          `json:"screens,omitempty"`
	Actions           map[string]OfflineAction `json:"actions"`
	Summaries         map[string]Summary       `json:"summaries,omitempty"`
}

// OfflineApp is what GET …/apps/{app}/offline answers.
type OfflineApp struct {
	AppID   int64  `json:"app_id"`
	SpaceID int64  `json:"space_id"`
	Name    string `json:"name"`
	// Release is the live release the download was taken from; its definition's version and
	// requirements are in Definition.
	Release    int64             `json:"release"`
	Definition OfflineDefinition `json:"definition"`
	// Messages draw the app as it is at CapturedAt: the A2UI messages the surface route sends,
	// with the records downloaded and the summaries over them.
	Messages []Message `json:"messages"`
	// Scope is whose records the download holds: "all" of the app's, or only the caller's "own".
	Scope string `json:"scope"`
	// Records is how many are downloaded; Total how many the caller reads in all. Truncated is
	// true when the latest MaxOfflineRows were downloaded and the rest were not. Summaries in
	// Messages count the downloaded records only.
	Records   int  `json:"records"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
	// CanInsert is whether the caller may add records now: the app lets them, and the space is
	// writable.
	CanInsert  bool   `json:"can_insert"`
	CapturedAt string `json:"captured_at"`
}

// offlineApp answers a download: the caller's view of the live release and their records.
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
	return out, nil
}

func offlineDefinition(def Definition) OfflineDefinition {
	out := OfflineDefinition{
		DefinitionVersion: def.DefinitionVersion,
		Requires:          def.Requires,
		Collection:        def.Collection,
		Entry:             def.Entry,
		Actions:           map[string]OfflineAction{},
		Summaries:         def.Summaries,
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
