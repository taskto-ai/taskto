package cliaccounts

import (
	"context"
	"errors"
	"strings"
)

const maxModelRef = 256

const cliProviderPrefix = "cli-"

func queueableRef(provider, model string) bool {
	return provider != "" && model != "" && len(provider) <= maxModelRef && len(model) <= maxModelRef &&
		!strings.HasPrefix(provider, cliProviderPrefix)
}

type ModelCatalog interface {
	Offers(ctx context.Context, provider, model string, user, workspace int64) (bool, error)
}

func (s *Service) SetModelCatalog(c ModelCatalog) { s.catalog = c }

func (s *Service) offers(ctx context.Context, provider, model string, user, workspace int64) (bool, error) {
	if s.catalog == nil || !queueableRef(provider, model) {
		return false, nil
	}
	return s.catalog.Offers(ctx, provider, model, user, workspace)
}

func (s *Service) modelQueueable(ctx context.Context, user int64, provider, model string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT workspace_id FROM memberships WHERE user_id = ?`, user)
	if err != nil {
		return false, err
	}
	var workspaces []int64
	for rows.Next() {
		var ws int64
		if err := rows.Scan(&ws); err != nil {
			_ = rows.Close()
			return false, err
		}
		workspaces = append(workspaces, ws)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return false, err
	}
	for _, ws := range workspaces {
		if ok, err := s.offers(ctx, provider, model, user, ws); err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}
