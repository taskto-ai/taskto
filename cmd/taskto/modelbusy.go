package main

import (
	"errors"
	"fmt"
	"net/http"

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/cliaccounts"
)

func modelOutcome(err error) error {
	if err == nil {
		return nil
	}
	var refused *agentrt.StatusError
	switch {
	case !errors.As(err, &refused) || refused.Code >= http.StatusInternalServerError:
		return fmt.Errorf("%w: %w", cliaccounts.ErrModelUncertain, err)
	case refused.Code == http.StatusConflict && refused.Reason == "busy":
		return fmt.Errorf("%w: %w", cliaccounts.ErrModelBusy, err)
	}
	return err
}
