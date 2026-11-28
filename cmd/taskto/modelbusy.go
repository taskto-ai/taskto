package main

import (
	"errors"
	"fmt"
	"net/http"

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/cliaccounts"
)

func modelBusy(err error) error {
	var refused *agentrt.StatusError
	if errors.As(err, &refused) && refused.Code == http.StatusConflict && refused.Reason == "busy" {
		return fmt.Errorf("%w: %w", cliaccounts.ErrModelBusy, err)
	}
	return err
}
