package notifications

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"taskto.ai/taskto/internal/access"
)

type Item struct {
	ID          int64             `json:"id"`
	WorkspaceID int64             `json:"workspace_id"`
	Kind        string            `json:"kind"`
	Params      map[string]string `json:"params,omitempty"`
	Target      Target            `json:"target"`
	Read        bool              `json:"read"`
	CreatedAt   string            `json:"created_at"`
}

type Target struct {
	Section string `json:"section"`
	Item    string `json:"item,omitempty"`
}

type stored struct {
	id, userID, workspaceID int64
	kind                    string
	ref                     Ref
	createdAt               string
	read                    bool
}

const storedColumns = `id, user_id, workspace_id, kind, ref, created_at, read_at IS NOT NULL`

func scanStored(row interface{ Scan(...any) error }) (stored, error) {
	var n stored
	var ref string
	if err := row.Scan(&n.id, &n.userID, &n.workspaceID, &n.kind, &ref, &n.createdAt, &n.read); err != nil {
		return stored{}, err
	}
	if err := json.Unmarshal([]byte(ref), &n.ref); err != nil {
		return stored{}, fmt.Errorf("notification %d: ref: %w", n.id, err)
	}
	return n, nil
}

type reader struct {
	s      *Service
	access *access.Checker
	userID int64
}

func (s *Service) readerFor(userID int64) *reader {
	return &reader{s: s, access: s.access, userID: userID}
}

func (s *Service) readerOn(snap *access.Checker, userID int64) *reader {
	return &reader{s: s, access: snap, userID: userID}
}

func (r *reader) item(ctx context.Context, n stored) (Item, error) {
	it := Item{ID: n.id, WorkspaceID: n.workspaceID, Read: n.read, CreatedAt: n.createdAt}
	params, target, ok, err := r.describe(ctx, n)
	if err != nil {
		return Item{}, err
	}
	if !ok {
		it.Kind, it.Target = KindChanged, Target{Section: "home"}
		return it, nil
	}
	it.Kind, it.Params, it.Target = n.kind, params, target
	return it, nil
}

func (r *reader) describe(ctx context.Context, n stored) (params map[string]string, target Target, ok bool, err error) {
	switch n.kind {
	case KindCardAssigned, KindCardComment, KindCardMention, KindCardDue, KindCardOverdue:
		var title, due, col, boardArchived sql.NullString
		var assignee sql.NullInt64
		var agent sql.NullBool
		sp, err := r.access.CheckWith(ctx, r.userID, n.ref.Space, `c.title, c.due, c.col, c.assignee_id, c.assignee_agent, b.archived_at`, `
			LEFT JOIN boards b ON b.id = ? AND b.space_id = s.id
			LEFT JOIN cards c ON c.id = ? AND c.board_id = b.id`,
			[]any{n.ref.Board, n.ref.Card}, &title, &due, &col, &assignee, &agent, &boardArchived)
		if errors.Is(err, access.ErrNotFound) || err == nil && (sp.WorkspaceID != n.workspaceID || !title.Valid) {
			return nil, Target{}, false, nil
		}
		if err != nil {
			return nil, Target{}, false, err
		}
		item := strconv.FormatInt(sp.ID, 10) + ":" + strconv.FormatInt(n.ref.Board, 10) + ":" + strconv.FormatInt(n.ref.Card, 10)
		params := map[string]string{"title": title.String}
		if n.kind == KindCardDue || n.kind == KindCardOverdue {
			if !due.Valid || due.String != n.ref.Due || sp.ArchivedAt != nil || boardArchived.Valid ||
				agent.Bool || !assignee.Valid || assignee.Int64 != r.userID ||
				col.String == "done" || col.String == "archived" {
				return nil, Target{}, false, nil
			}
			params["due"] = due.String
		}
		return params, Target{Section: "boards", Item: item}, true, nil
	case KindIntegrationEvent:
		var name string
		err := r.s.db.QueryRowContext(ctx, `SELECT i.display_name FROM integration_events e
			JOIN integrations i ON i.id = e.integration_id
			JOIN memberships m ON m.workspace_id = i.workspace_id AND m.user_id = ? AND m.role = 'admin'
			WHERE e.id = ? AND i.id = ? AND i.workspace_id = ?`, r.userID, n.ref.Event, n.ref.Integration, n.workspaceID).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, Target{}, false, nil
		}
		if err != nil {
			return nil, Target{}, false, err
		}
		return map[string]string{"integration": name}, Target{Section: "settings"}, true, nil
	case KindAppReminder:
		if r.s.reminders == nil {
			return nil, Target{}, false, nil
		}
		text, shown, err := r.s.reminders.ReminderText(ctx, r.access, r.userID, n.workspaceID, n.ref)
		if err != nil || !shown {
			return nil, Target{}, false, err
		}
		return map[string]string{"app": text.App, "title": text.Title},
			Target{Section: "apps", Item: strconv.FormatInt(n.ref.Space, 10) + "-" + strconv.FormatInt(n.ref.App, 10)}, true, nil
	case KindAppEffectFailed:
		sp, acc, name, err := r.access.AppNamed(ctx, r.userID, n.ref.Space, n.ref.App)
		if errors.Is(err, access.ErrNotFound) || errors.Is(err, access.ErrAppNotFound) {
			return nil, Target{}, false, nil
		}
		if err != nil {
			return nil, Target{}, false, err
		}
		if !acc.View || sp.WorkspaceID != n.workspaceID {
			return nil, Target{}, false, nil
		}
		return map[string]string{"app": name}, Target{Section: "apps", Item: strconv.FormatInt(n.ref.Space, 10) + "-" + strconv.FormatInt(n.ref.App, 10)}, true, nil
	}
	return nil, Target{}, false, nil
}
