package apps

import (
	"context"
	"database/sql"
	"errors"

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

// ErrNotAdmin is the answer of a workspace export's read of an app when the exporter is no longer
// an admin of the workspace, as of the snapshot the app was read in.
var ErrNotAdmin = errors.New("not an admin of the workspace")

// ExportApp returns an app as a's user may see it, for the exports: its identity, its live
// release with the definition, which everyone who may open the app gets, and its records as the
// app shows them (Items). The access, the app, the definition and the records are all of one
// snapshot, so records are never of a scope the person lost meanwhile, and the definition is the
// one that shaped them, even if the app is published while the export runs.
//
// The draft, the releases before the live one, and what the app's effects act on are left out:
// they are for those who design or publish the app. An app a's user can't open is ErrNotFound,
// as it is for the app list and the agent's tools.
//
// A workspace export (workspace) holds what a person's roles give them, and asks for more, in the
// same snapshot: the app is ErrNotFound if they can open it only to set its roles right (a
// workspace admin's way back into a stranded app, see package access), which gives them no role in
// it, and ErrNotAdmin if they are no longer an admin of the workspace (nor enabled).
func (s *Service) ExportApp(ctx context.Context, a Actor, spaceID, appID int64, workspace bool) (ExportedApp, error) {
	var check func(context.Context, *sql.Tx, access.Space, access.AppAccess) error
	if workspace {
		check = func(ctx context.Context, tx *sql.Tx, sp access.Space, acc access.AppAccess) error {
			var admin bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM memberships m JOIN users u ON u.id = m.user_id
				WHERE m.workspace_id = ? AND m.user_id = ? AND m.role = 'admin' AND u.disabled_at IS NULL)`,
				sp.WorkspaceID, a.UserID).Scan(&admin); err != nil {
				return err
			}
			if !admin {
				return ErrNotAdmin
			}
			if len(acc.Roles) == 0 {
				return ErrNotFound
			}
			return nil
		}
	}
	ap, _, rows, err := s.snapshotRows(ctx, a, spaceID, appID, check)
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
