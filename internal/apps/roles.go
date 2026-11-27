package apps

import (
	"context"
	"errors"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
)

func (s *Service) appRoles(ctx context.Context, sp access.Space, a Actor, appID int64) ([]access.AppGrant, error) {
	if err := may(sp, access.Read); err != nil {
		return nil, err
	}
	acc, err := s.appAccess(ctx, a, sp.ID, appID)
	if err != nil {
		return nil, err
	}
	if err := need(acc, access.CapManageRoles); err != nil {
		return nil, err
	}
	return s.access.AppGrants(ctx, sp.ID, appID)
}

func (s *Service) setAppRoles(ctx context.Context, sp access.Space, a Actor, appID int64, grants []access.AppGrant) ([]access.AppGrant, error) {
	if err := may(sp, access.Write); err != nil {
		return nil, err
	}
	if err := s.access.SetAppGrants(ctx, a.UserID, sp.ID, appID, grants); err != nil {
		return nil, roleError(err)
	}
	return s.access.AppGrants(ctx, sp.ID, appID)
}

func roleError(err error) error {
	switch {
	case errors.Is(err, access.ErrBadGrants):
		return invalidAs(apierr.CodeAppRolesInvalid, nil, "%v", err)
	case errors.Is(err, access.ErrGrantUnknownUser):
		return invalidAs(apierr.CodeAppRolesNotMember, nil, "%v", err)
	case errors.Is(err, access.ErrGrantForbidden):
		return forbidden(string(access.CapManageRoles))
	case errors.Is(err, access.ErrGrantLockout):
		return invalidAs(apierr.CodeAppRolesLockout, nil, "%v", err)
	case errors.Is(err, access.ErrNotFound), errors.Is(err, access.ErrAppNotFound):
		return ErrNotFound
	}
	return err
}
