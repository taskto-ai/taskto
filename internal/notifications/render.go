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

// reader answers, for one request or one push, what a person may read, asking access.Checker once
// for each space.
type reader struct {
	s      *Service
	userID int64
	spaces map[int64]*access.Space
}

func (s *Service) readerFor(userID int64) *reader {
	return &reader{s: s, userID: userID, spaces: map[int64]*access.Space{}}
}

// space returns the space as the person sees it, nil if they can't read it, or if it isn't in the
// workspace the notification was made in.
func (r *reader) space(ctx context.Context, workspaceID, spaceID int64) (*access.Space, error) {
	sp, seen := r.spaces[spaceID]
	if !seen {
		got, err := r.s.access.Check(ctx, r.userID, spaceID)
		switch {
		case errors.Is(err, access.ErrNotFound):
		case err != nil:
			return nil, err
		case got.Access.Allows(access.Read):
			sp = &got
		}
		r.spaces[spaceID] = sp
	}
	if sp == nil || sp.WorkspaceID != workspaceID {
		return nil, nil
	}
	return sp, nil
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
	sp, err := r.space(ctx, n.workspaceID, n.ref.Space)
	if err != nil || sp == nil {
		return nil, Target{}, false, err
	}
	switch n.kind {
	case KindCardAssigned, KindCardDue, KindCardOverdue:
		var title string
		var due sql.NullString
		err := r.s.db.QueryRowContext(ctx, `
			SELECT c.title, c.due FROM cards c JOIN boards b ON b.id = c.board_id
			WHERE c.id = ? AND b.id = ? AND b.space_id = ?`, n.ref.Card, n.ref.Board, sp.ID).Scan(&title, &due)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, Target{}, false, nil
		}
		if err != nil {
			return nil, Target{}, false, err
		}
		item := strconv.FormatInt(sp.ID, 10) + ":" + strconv.FormatInt(n.ref.Board, 10) + ":" + strconv.FormatInt(n.ref.Card, 10)
		params := map[string]string{"title": title}
		if n.kind != KindCardAssigned {
			// The date the card is due now, a calendar date: the web app formats it for the reader. A
			// card whose date was cleared since has none to say, and is a plain mention of the card.
			if !due.Valid {
				return nil, Target{}, false, nil
			}
			params["due"] = due.String
		}
		return params, Target{Section: "boards", Item: item}, true, nil
	case KindAppEffectFailed:
		acc, err := r.s.access.App(ctx, r.userID, sp.ID, n.ref.App)
		if errors.Is(err, access.ErrNotFound) || errors.Is(err, access.ErrAppNotFound) {
			return nil, Target{}, false, nil
		}
		if err != nil {
			return nil, Target{}, false, err
		}
		if !acc.View {
			return nil, Target{}, false, nil
		}
		var name string
		if err := r.s.db.QueryRowContext(ctx, `SELECT name FROM apps WHERE id = ? AND space_id = ?`, n.ref.App, sp.ID).Scan(&name); err != nil {
			return nil, Target{}, false, err
		}
		return map[string]string{"app": name}, Target{Section: "apps", Item: strconv.FormatInt(sp.ID, 10) + "-" + strconv.FormatInt(n.ref.App, 10)}, true, nil
	}
	return nil, Target{}, false, nil
}
