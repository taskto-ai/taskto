package apps

import (
	"context"
	"errors"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
)

// Roles are the role assignments of an app (docs/api.md, package access's apps.go): who may
// build it, publish it, add records to it and read or change its records, inside the
// space's boundary. They are set by people, with the API, and only by whoever manages the
// app's roles: neither a definition nor the agent's tools can grant any, so nothing an
// agent writes into an app gives anyone more than they had.

// appRoles lists the role assignments of appID, for a caller who manages them.
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

// setAppRoles replaces the role assignments of appID: none makes it unrestricted again.
func (s *Service) setAppRoles(ctx context.Context, sp access.Space, a Actor, appID int64, grants []access.AppGrant) ([]access.AppGrant, error) {
	if err := may(sp, access.Write); err != nil {
		return nil, err
	}
	// SetAppGrants checks, under the write lock, that the caller manages the app's roles now.
	if err := s.access.SetAppGrants(ctx, a.UserID, sp.ID, appID, grants); err != nil {
		return nil, roleError(err)
	}
	return s.access.AppGrants(ctx, sp.ID, appID)
}

// roleError turns package access's refusals into this package's.
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
