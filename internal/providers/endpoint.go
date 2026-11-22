package providers

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
)

// ErrAccountNotFound is WorkspaceEndpoint's answer for an account that doesn't exist or
// isn't the workspace's.
var ErrAccountNotFound = errNotFound

// Endpoint is where an account's models answer, with its API key in clear, for the
// server's own calls to a model (the search index's embeddings). Never return it from the
// API or put it in a log or an error: it prints and logs as its provider ID only.
type Endpoint struct {
	Provider string
	BaseURL  string
	APIKey   string // "" without one
}

func (e Endpoint) String() string { return e.Provider }

// LogValue keeps the key out of logs.
func (e Endpoint) LogValue() slog.Value { return slog.StringValue(e.Provider) }

// WorkspaceEndpoint returns the endpoint of workspaceID's account accountID. A user's own
// account is never one: what it is used for serves the whole workspace.
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
