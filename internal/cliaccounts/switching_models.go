package cliaccounts

import (
	"context"
	"strings"

	"taskto.ai/taskto/internal/conversations"
	"taskto.ai/taskto/internal/providers"
)

const maxModelRef = 256

const cliProviderPrefix = "cli-"

func queueableRef(provider, model string) bool {
	return provider != "" && model != "" && len(provider) <= maxModelRef && len(model) <= maxModelRef &&
		!strings.HasPrefix(provider, cliProviderPrefix)
}

func (s *Service) modelQueueable(ctx context.Context, user int64, provider, model string) (bool, error) {
	if !queueableRef(provider, model) {
		return false, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT workspace_id FROM memberships WHERE user_id = ?`, user)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var ws int64
		if err := rows.Scan(&ws); err != nil {
			return false, err
		}
		if providers.Usable(provider, user, ws) {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Service) modelsAfter(ctx context.Context, p conversations.Principal) ([]queueEntry, error) {
	q, err := s.queue(ctx, p.User)
	if err != nil {
		return nil, err
	}
	var out []queueEntry
	for _, e := range q {
		if e.isModel() && queueableRef(e.provider, e.model) && providers.Usable(e.provider, p.User, p.Workspace) {
			out = append(out, e)
		}
	}
	return out, nil
}
