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
	userID int64
}

func (s *Service) readerFor(userID int64) *reader {
	return &reader{s: s, userID: userID}
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
		var title, due sql.NullString
		sp, err := r.s.access.CheckWith(ctx, r.userID, n.ref.Space, `c.title, c.due`, `
			LEFT JOIN boards b ON b.id = ? AND b.space_id = s.id
			LEFT JOIN cards c ON c.id = ? AND c.board_id = b.id`,
			[]any{n.ref.Board, n.ref.Card}, &title, &due)
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
			// whose date was cleared or moved since the notice was made says nothing of it (the notice
			// would read "overdue since" a day that may be ahead): it is a plain mention of the card.
			if !due.Valid || due.String != n.ref.Due {
				return nil, Target{}, false, nil
			}
			params["due"] = due.String
		}
		return params, Target{Section: "boards", Item: item}, true, nil
	case KindAppEffectFailed:
		// The right to view the app and its name come from one snapshot (access.Checker.AppNamed).
		sp, acc, name, err := r.s.access.AppNamed(ctx, r.userID, n.ref.Space, n.ref.App)
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
