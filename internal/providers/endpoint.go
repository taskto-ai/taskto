package providers

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
)

var ErrAccountNotFound = errNotFound

type Endpoint struct {
	Provider string
	BaseURL  string
	APIKey   string
}

func (e Endpoint) String() string { return e.Provider }

func (e Endpoint) LogValue() slog.Value { return slog.StringValue(e.Provider) }

func (s *Store) WorkspaceEndpoint(ctx context.Context, workspaceID, accountID int64) (Endpoint, error) {
	rec, err := scan(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM provider_accounts WHERE id = ? AND workspace_id = ?`,
		accountID, workspaceID))
	if errors.Is(err, sql.ErrNoRows) {
		return Endpoint{}, ErrAccountNotFound
	}
	if err != nil {
		return Endpoint{}, err
	}
	key, err := s.apiKey(rec)
	if err != nil {
		return Endpoint{}, err
	}
	return Endpoint{Provider: rec.Provider, BaseURL: rec.BaseURL, APIKey: key}, nil
}
