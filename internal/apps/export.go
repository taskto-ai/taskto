package apps

import (
	"context"
	"database/sql"
	"errors"

	"taskto.ai/taskto/internal/access"
)

type ExportedApp struct {
	App  App
	Live *Release
	Rows []map[string]any
}

var ErrNotAdmin = errors.New("not an admin of the workspace")

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
