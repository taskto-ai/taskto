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

// Item is a notification as its recipient is told of it now.
type Item struct {
	ID          int64 `json:"id"`
	WorkspaceID int64 `json:"workspace_id"`
	// Kind is the stored kind, or KindChanged when the recipient may no longer read what it was
	// about: then there is nothing else to say, and Target is the workspace's home.
	Kind string `json:"kind"`
	// Params fill the kind's message: the card's title, the app's name. Only for what the
	// recipient may read now.
	Params    map[string]string `json:"params,omitempty"`
	Target    Target            `json:"target"`
	Read      bool              `json:"read"`
	CreatedAt string            `json:"created_at"`
}

// Target is an address in the web app under /w/{workspace}/: its section and item, as its router
// reads them. The page it opens checks access again.
type Target struct {
	Section string `json:"section"`
	Item    string `json:"item,omitempty"`
}

// stored is a notification as the table has it.
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

// reader answers, for one request or one push, what a person may read.
//
// Whether the person may read a space, and the name of what the notification is about, come from
// one statement each (access.Checker.CheckWith), never from one read for the access and another
// for the name: access could end and the card be renamed between two, and the old recipient
// would see the new title. Nothing is kept from one notification to the next for the same
// reason.
type reader struct {
	s      *Service
	access *access.Checker
	userID int64
}

func (s *Service) readerFor(userID int64) *reader {
	return &reader{s: s, access: s.access, userID: userID}
}

// readerOn is readerFor reading through snap, a checker on a transaction (access.Checker.On): what
// it says about many notifications comes from the one state that transaction sees.
func (s *Service) readerOn(snap *access.Checker, userID int64) *reader {
	return &reader{s: s, access: snap, userID: userID}
}

// item renders n for its recipient as they may read things now.
func (r *reader) item(ctx context.Context, n stored) (Item, error) {
	it := Item{ID: n.id, WorkspaceID: n.workspaceID, Read: n.read, CreatedAt: n.createdAt}
	params, target, ok, err := r.describe(ctx, n)
	if err != nil {
		return Item{}, err
	}
	if !ok {
		// Nothing of what it was about: no kind, no names, no IDs.
		it.Kind, it.Target = KindChanged, Target{Section: "home"}
		return it, nil
	}
	it.Kind, it.Params, it.Target = n.kind, params, target
	return it, nil
}

// describe says what a notification is about and where it leads, if its recipient may read that.
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
			// The date the card is due, a calendar date: the web app formats it for the reader. A card
			// that is no longer what the notice was made for says nothing of it (the notice would read
			// "overdue since" a day that may be ahead, or remind of a card that is finished): its date
			// was cleared or moved, it is done or archived, it or its board or space is archived, or it
			// is not the reader's any more. These are the checks the reminder's job makes when it runs
			// (boards.remind), so the inbox, a push and an email tell the same.
			if !due.Valid || due.String != n.ref.Due || sp.ArchivedAt != nil || boardArchived.Valid ||
				agent.Bool || !assignee.Valid || assignee.Int64 != r.userID ||
				col.String == "done" || col.String == "archived" {
				return nil, Target{}, false, nil
			}
			params["due"] = due.String
		}
		return params, Target{Section: "boards", Item: item}, true, nil
	case KindIntegrationEvent:
		// For admins of the workspace, now: the integration's name, from one statement that also checks
		// the role and that the event is the integration's, so a person who is no longer an admin, or an
		// event that was purged, is shown nothing.
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
	case KindAppEffectFailed:
		// The right to view the app and its name come from one snapshot (access.Checker.AppNamed).
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
