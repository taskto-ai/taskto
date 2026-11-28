package live

import (
	"context"
	"errors"
	"net/http"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
)

func RecheckSpace(c *access.Checker, user int64, space access.Space) Recheck {
	return func(ctx context.Context) error {
		now, err := c.Check(ctx, user, space.ID)
		if errors.Is(err, access.ErrNotFound) || (err == nil && (now.Generation != space.Generation || now.WorkspaceID != space.WorkspaceID)) {
			return apierr.New(http.StatusNotFound, apierr.CodeSpaceNotFound, access.ErrNotFound.Error())
		}
		return err
	}
}
