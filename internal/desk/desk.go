package desk

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

var ErrNotFound = errors.New("desk not found")
var ErrChanged = errors.New("desk changed")

var emptyLayout = json.RawMessage(`{"v":1,"focus":"","root":null}`)

type Desk struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Layout    json.RawMessage `json:"layout" apitypes:"json"`
	Version   int64           `json:"version"`
	UpdatedAt string          `json:"updated_at"`
}

type DeskSummary struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	UpdatedAt string `json:"updated_at"`
}

type DeskList struct {
	Desks []DeskSummary `json:"desks" apitypes:"nonnull"`
}

type LayoutWrite struct {
	Version int64           `json:"version"`
	Layout  json.RawMessage `json:"layout" apitypes:"json"`
}

type Service struct {
	db  *sql.DB
	now func() time.Time
}

func New(db *sql.DB) *Service { return &Service{db: db, now: time.Now} }

func (s *Service) List(ctx context.Context, workspaceID, userID int64) (DeskList, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, updated_at FROM desks
		WHERE user_id = ? AND workspace_id = ? AND name <> ''
		ORDER BY position, id`, userID, workspaceID)
	if err != nil {
		return DeskList{}, err
	}
	defer rows.Close()
	list := DeskList{Desks: []DeskSummary{{ID: 0, Name: ""}}}
	for rows.Next() {
		var item DeskSummary
		if err := rows.Scan(&item.ID, &item.Name, &item.UpdatedAt); err != nil {
			return DeskList{}, err
		}
		list.Desks = append(list.Desks, item)
	}
	return list, rows.Err()
}

func (s *Service) Get(ctx context.Context, workspaceID, userID, id int64) (Desk, error) {
	if id == 0 {
		return s.getMain(ctx, workspaceID, userID)
	}
	return s.getByID(ctx, workspaceID, userID, id)
}

func (s *Service) SaveLayout(ctx context.Context, workspaceID, userID, id, version int64, raw []byte) (Desk, error) {
	if id != 0 {
		if _, err := s.getByID(ctx, workspaceID, userID, id); err != nil {
			return Desk{}, err
		}
	}
	checked, err := Check(workspaceID, raw)
	if err != nil {
		return Desk{}, err
	}
	if version < 0 {
		current, readErr := s.Get(ctx, workspaceID, userID, id)
		if readErr != nil {
			return Desk{}, readErr
		}
		return current, ErrChanged
	}
	if id != 0 {
		return s.updateLayout(ctx, workspaceID, userID, id, version, checked)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Desk{}, err
	}
	defer tx.Rollback()
	if version == 0 {
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO desks (user_id, workspace_id, name, layout, version)
			VALUES (?, ?, '', ?, 0) ON CONFLICT (user_id, workspace_id, name) DO NOTHING`, userID, workspaceID, string(emptyLayout)); err != nil {
			return Desk{}, err
		}
	}
	if version != 0 {
		current, readErr := getMainWith(ctx, tx, workspaceID, userID)
		if readErr != nil {
			return Desk{}, readErr
		}
		if err := tx.Commit(); err != nil {
			return Desk{}, err
		}
		return current, ErrChanged
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE desks SET layout = ?, version = version + 1, updated_at = ?
		WHERE user_id = ? AND workspace_id = ? AND name = '' AND version = ?`, string(checked), s.now().UTC().Format("2006-01-02T15:04:05.000Z"), userID, workspaceID, version)
	if err != nil {
		return Desk{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return Desk{}, err
	}
	if changed == 0 {
		current, readErr := getMainWith(ctx, tx, workspaceID, userID)
		if readErr != nil {
			return Desk{}, readErr
		}
		if err := tx.Commit(); err != nil {
			return Desk{}, err
		}
		return current, ErrChanged
	}
	updated, err := getMainWith(ctx, tx, workspaceID, userID)
	if err != nil {
		return Desk{}, err
	}
	if err := tx.Commit(); err != nil {
		return Desk{}, err
	}
	return updated, nil
}

func (s *Service) updateLayout(ctx context.Context, workspaceID, userID, id, version int64, checked []byte) (Desk, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE desks SET layout = ?, version = version + 1, updated_at = ?
		WHERE user_id = ? AND workspace_id = ? AND id = ? AND version = ?`,
		string(checked), s.now().UTC().Format("2006-01-02T15:04:05.000Z"), userID, workspaceID, id, version)
	if err != nil {
		return Desk{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return Desk{}, err
	}
	if changed == 0 {
		current, err := s.getByID(ctx, workspaceID, userID, id)
		if errors.Is(err, ErrNotFound) {
			return Desk{}, ErrNotFound
		}
		if err != nil {
			return Desk{}, err
		}
		return current, ErrChanged
	}
	return s.getByID(ctx, workspaceID, userID, id)
}

func (s *Service) getMain(ctx context.Context, workspaceID, userID int64) (Desk, error) {
	return getMainWith(ctx, s.db, workspaceID, userID)
}

func getMainWith(ctx context.Context, q queryer, workspaceID, userID int64) (Desk, error) {
	var d Desk
	var raw string
	err := q.QueryRowContext(ctx, `SELECT id, name, layout, version, updated_at FROM desks
		WHERE user_id = ? AND workspace_id = ? AND name = ''`, userID, workspaceID).Scan(&d.ID, &d.Name, &raw, &d.Version, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Desk{ID: 0, Name: "", Layout: append(json.RawMessage(nil), emptyLayout...), Version: 0}, nil
	}
	d.Layout = json.RawMessage(raw)
	return d, err
}

func (s *Service) getByID(ctx context.Context, workspaceID, userID, id int64) (Desk, error) {
	var d Desk
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT id, name, layout, version, updated_at FROM desks
		WHERE id = ? AND user_id = ? AND workspace_id = ?`, id, userID, workspaceID).Scan(&d.ID, &d.Name, &raw, &d.Version, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Desk{}, ErrNotFound
	}
	d.Layout = json.RawMessage(raw)
	return d, err
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
