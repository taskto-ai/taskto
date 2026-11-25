package apps

import (
	"context"

	"taskto.ai/taskto/internal/access"
)

// ExportedApp is an app as the export (docs/api.md#export-a-workspace) reads it: its identity,
// its live release with the definition if there is one, and the records the person may read,
// shaped by that definition.
type ExportedApp struct {
	App  App
	Live *Release
	Rows []map[string]any
}

// ExportApp returns an app as a's user may see it, for the exports: its identity, its live
// release with the definition, which everyone who may open the app gets, and its records as the
// app shows them (Items). The access, the app, the definition and the records are all of one
// snapshot, so records are never of a scope the person lost meanwhile, and the definition is the
// one that shaped them, even if the app is published while the export runs.
//
// The draft, the releases before the live one, and what the app's effects act on are left out:
// they are for those who design or publish the app. An app a's user can't open is ErrNotFound,
// as it is for the app list and the agent's tools; so is, if heldRole, an app they can open only
// to set its roles right (a workspace admin's way back into a stranded app, see package access):
// a workspace export holds what a person's roles give them, not that.
func (s *Service) ExportApp(ctx context.Context, a Actor, spaceID, appID int64, heldRole bool) (ExportedApp, error) {
	var allow func(access.AppAccess) bool
	if heldRole {
		allow = func(acc access.AppAccess) bool { return len(acc.Roles) > 0 }
	}
	ap, _, rows, err := s.snapshotRows(ctx, a, spaceID, appID, allow)
	if err != nil {
		return ExportedApp{}, err
	}
	out := ExportedApp{App: ap, Rows: rows}
	out.App.Draft, out.App.EffectTargets = nil, nil
	if ap.Live != nil && ap.liveDef != nil {
		live := *ap.Live
		def := *ap.liveDef
		live.Definition = &def
		out.Live = &live
	}
	return out, nil
}
