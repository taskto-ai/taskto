package apps

import "context"

// ExportApp returns an app as a's user may see it, for the workspace export
// (docs/api.md#export-a-workspace): its identity and its live release with the definition, which
// everyone who may open the app gets. The draft, and the releases before the live one, are left
// out, since they are for those who design or publish the app, and what the app's effects act on
// is left out too. An app a's user holds no role in is ErrNotFound, as it is for the app list and
// the agent's tools.
func (s *Service) ExportApp(ctx context.Context, a Actor, spaceID, appID int64) (App, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return App{}, err
	}
	ap, err := s.getApp(ctx, sp, a, appID)
	if err != nil {
		return App{}, err
	}
	ap.Draft = nil
	ap.EffectTargets = nil
	return ap, nil
}
