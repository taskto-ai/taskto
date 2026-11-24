package boards

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/conversations"
	"taskto.ai/taskto/internal/notifications"
)

// timeFormat matches strftime('%Y-%m-%dT%H:%M:%fZ'), the schema's format for times.
const timeFormat = "2006-01-02T15:04:05.000Z"

func now() string { return time.Now().UTC().Format(timeFormat) }

// queryer is a *sql.DB or a *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type scanner interface{ Scan(dest ...any) error }

// write runs fn in a transaction. Transactions take the write lock when they begin (see
// package db), so the checks in fn (versions, neighbours, blockers) hold for its writes.
func (s *Service) write(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if rbErr := tx.Rollback(); !errors.Is(rbErr, sql.ErrTxDone) {
				err = errors.Join(err, rbErr)
			}
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// may checks that sp, as its caller sees it, allows need: ErrNotFound if it can't be
// read, ErrReadOnly if it can't be written.
func may(sp access.Space, need access.Access) error {
	switch {
	case !sp.Access.Allows(access.Read):
		return ErrNotFound
	case !sp.Access.Allows(need) && sp.ArchivedAt != nil:
		return errSpaceArchived
	case !sp.Access.Allows(need):
		return ErrReadOnly
	}
	return nil
}

// errSpaceArchived is ErrReadOnly, saying why.
var errSpaceArchived = readOnlyError("this space is archived: it is read-only")

type readOnlyError string

func (e readOnlyError) Error() string        { return string(e) }
func (e readOnlyError) Is(target error) bool { return target == ErrReadOnly }

// author turns the nullable columns of a user reference into an Author.
func author(id sql.NullInt64, name string, agent bool, via sql.NullString) *Author {
	if !id.Valid {
		return nil
	}
	return &Author{ID: id.Int64, Name: name, Agent: agent, Via: via.String}
}

// via is the column value of an actor's access token: NULL without one.
func via(a Actor) any {
	if a.Via == "" {
		return nil
	}
	return a.Via
}

// Boards.

const boardColumns = `b.id, b.space_id, b.kind, b.name, b.archived_at, b.version,
	b.created_at, b.created_by, coalesce(cu.name, ''), b.updated_at, b.updated_by, coalesce(uu.name, '')
	FROM boards b
	LEFT JOIN users cu ON cu.id = b.created_by
	LEFT JOIN users uu ON uu.id = b.updated_by`

func scanBoard(row scanner) (Board, error) {
	var b Board
	var createdBy, updatedBy sql.NullInt64
	var createdName, updatedName string
	if err := row.Scan(&b.ID, &b.SpaceID, &b.Kind, &b.Name, &b.ArchivedAt, &b.Version,
		&b.CreatedAt, &createdBy, &createdName, &b.UpdatedAt, &updatedBy, &updatedName); err != nil {
		return Board{}, err
	}
	b.CreatedBy = author(createdBy, createdName, false, sql.NullString{})
	b.UpdatedBy = author(updatedBy, updatedName, false, sql.NullString{})
	return b, nil
}

// board returns a board of space spaceID, or ErrNotFound.
func board(ctx context.Context, q queryer, spaceID, boardID int64) (Board, error) {
	b, err := scanBoard(q.QueryRowContext(ctx, `SELECT `+boardColumns+` WHERE b.id = ? AND b.space_id = ?`, boardID, spaceID))
	if errors.Is(err, sql.ErrNoRows) {
		return Board{}, ErrNotFound
	}
	return b, err
}

// writableBoard is board, failing with ErrBoardArchived for an archived one.
func writableBoard(ctx context.Context, q queryer, spaceID, boardID int64) (Board, error) {
	b, err := board(ctx, q, spaceID, boardID)
	if err == nil && b.ArchivedAt != nil {
		return Board{}, ErrBoardArchived
	}
	return b, err
}

func (s *Service) listBoards(ctx context.Context, sp access.Space, withArchived bool) ([]Board, error) {
	if err := may(sp, access.Read); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+boardColumns+`
		WHERE b.space_id = ? AND (? OR b.archived_at IS NULL)
		ORDER BY b.kind = 'team', b.archived_at IS NOT NULL, b.name, b.id`, sp.ID, withArchived)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := []Board{}
	for rows.Next() {
		b, err := scanBoard(rows)
		if err != nil {
			return nil, err
		}
		all = append(all, b)
	}
	return all, rows.Err()
}

func (s *Service) getBoard(ctx context.Context, sp access.Space, boardID int64) (Board, error) {
	if err := may(sp, access.Read); err != nil {
		return Board{}, err
	}
	return board(ctx, s.db, sp.ID, boardID)
}

func (s *Service) createBoard(ctx context.Context, sp access.Space, a Actor, name string) (Board, error) {
	if err := may(sp, access.Write); err != nil {
		return Board{}, err
	}
	name, err := boardName(name)
	if err != nil {
		return Board{}, err
	}
	if sp.Kind != access.Shared {
		return Board{}, invalid("a private space has one board, its personal board")
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO boards (space_id, kind, name, created_by, updated_by) VALUES (?, 'team', ?, ?, ?)`,
		sp.ID, name, a.UserID, a.UserID)
	if err != nil {
		return Board{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Board{}, err
	}
	return board(ctx, s.db, sp.ID, id)
}

// BoardChanges is an update of a board. Nil fields stay as they are.
type BoardChanges struct {
	// Version is the board's version the change was made against.
	Version  int64   `json:"version"`
	Name     *string `json:"name"`
	Archived *bool   `json:"archived"`
}

func (s *Service) updateBoard(ctx context.Context, sp access.Space, a Actor, boardID int64, ch BoardChanges) (Board, error) {
	if err := may(sp, access.Write); err != nil {
		return Board{}, err
	}
	if ch.Name == nil && ch.Archived == nil {
		return Board{}, invalid("name or archived is required")
	}
	if err := checkVersion(ch.Version); err != nil {
		return Board{}, err
	}
	if ch.Name != nil {
		name, err := boardName(*ch.Name)
		if err != nil {
			return Board{}, err
		}
		ch.Name = &name
	}
	err := s.write(ctx, func(tx *sql.Tx) error {
		b, err := board(ctx, tx, sp.ID, boardID)
		if err != nil {
			return err
		}
		if b.Version != ch.Version {
			return ErrStale
		}
		if b.Kind == Personal && ch.Archived != nil && *ch.Archived {
			return invalid("a personal board can't be archived")
		}
		// An archived board is read-only: the only change it takes is being unarchived.
		// A rename has to come after (or together with) unarchiving it.
		unarchiving := ch.Archived != nil && !*ch.Archived
		if b.ArchivedAt != nil && !unarchiving {
			return ErrBoardArchived
		}
		var archivedAt any // nil: unchanged
		if ch.Archived != nil {
			archivedAt = ""
			if *ch.Archived {
				archivedAt = now()
			}
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE boards SET
				name = coalesce(?, name),
				archived_at = CASE WHEN ? IS NULL THEN archived_at
				                   WHEN ? = '' THEN NULL
				                   ELSE coalesce(archived_at, ?) END,
				version = version + 1, updated_at = ?, updated_by = ?
			WHERE id = ?`, ch.Name, archivedAt, archivedAt, archivedAt, now(), a.UserID, boardID)
		return err
	})
	if err != nil {
		return Board{}, err
	}
	return board(ctx, s.db, sp.ID, boardID)
}

// Cards.

const cardColumns = `c.id, c.board_id, c.title, c.body, c.col, c.rank, c.archived_from,
	c.assignee_id, coalesce(au.name, ''), c.assignee_agent, c.blocked_reason,
	EXISTS (SELECT 1 FROM card_blockers cb WHERE cb.card_id = c.id), c.due, c.version,
	c.created_at, c.created_by, coalesce(cu.name, ''), c.created_by_agent, c.created_via,
	c.updated_at, c.updated_by, coalesce(uu.name, ''), c.updated_by_agent, c.updated_via
	FROM cards c
	JOIN boards b ON b.id = c.board_id
	LEFT JOIN users au ON au.id = c.assignee_id
	LEFT JOIN users cu ON cu.id = c.created_by
	LEFT JOIN users uu ON uu.id = c.updated_by`

// columnOrder sorts cards in board order of their columns.
const columnOrder = `CASE c.col WHEN 'backlog' THEN 0 WHEN 'todo' THEN 1 WHEN 'doing' THEN 2
	WHEN 'review' THEN 3 WHEN 'done' THEN 4 ELSE 5 END`

func scanCard(row scanner) (Card, error) {
	var c Card
	var assigneeID, createdBy, updatedBy sql.NullInt64
	var assigneeName, createdName, updatedName string
	var createdVia, updatedVia sql.NullString
	var agent, hasBlockers, createdAgent, updatedAgent bool
	if err := row.Scan(&c.ID, &c.BoardID, &c.Title, &c.Body, &c.Column, &c.Rank, &c.ArchivedFrom,
		&assigneeID, &assigneeName, &agent, &c.BlockedReason, &hasBlockers, &c.Due, &c.Version,
		&c.CreatedAt, &createdBy, &createdName, &createdAgent, &createdVia,
		&c.UpdatedAt, &updatedBy, &updatedName, &updatedAgent, &updatedVia); err != nil {
		return Card{}, err
	}
	switch {
	case agent:
		c.Assignee = &Assignee{Kind: AssignAgent}
	case assigneeID.Valid:
		c.Assignee = &Assignee{Kind: AssignUser, ID: assigneeID.Int64, Name: assigneeName}
	}
	c.Blocked = c.BlockedReason != nil || hasBlockers
	c.CreatedBy = author(createdBy, createdName, createdAgent, createdVia)
	c.UpdatedBy = author(updatedBy, updatedName, updatedAgent, updatedVia)
	c.BlockedBy, c.Docs, c.Conversations, c.Labels = []int64{}, []string{}, []string{}, []string{}
	return c, nil
}

// card returns a card of board boardID in space spaceID, with its blockers and links,
// or ErrNotFound.
func card(ctx context.Context, q queryer, spaceID, boardID, cardID int64) (Card, error) {
	c, err := scanCard(q.QueryRowContext(ctx, `SELECT `+cardColumns+`
		WHERE c.id = ? AND c.board_id = ? AND b.space_id = ?`, cardID, boardID, spaceID))
	if errors.Is(err, sql.ErrNoRows) {
		return Card{}, ErrNotFound
	}
	if err != nil {
		return Card{}, err
	}
	cards := []Card{c}
	if err := addRelations(ctx, q, cards, `= ?`, cardID); err != nil {
		return Card{}, err
	}
	return cards[0], nil
}

// addRelations fills in the blockers and links of cards, those whose ID matches
// "card_id <cond>".
func addRelations(ctx context.Context, q queryer, cards []Card, cond string, args ...any) error {
	byID := make(map[int64]*Card, len(cards))
	for i := range cards {
		byID[cards[i].ID] = &cards[i]
	}
	rows, err := q.QueryContext(ctx, `SELECT card_id, blocker_id FROM card_blockers WHERE card_id `+cond+`
		ORDER BY card_id, blocker_id`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, blocker int64
		if err := rows.Scan(&id, &blocker); err != nil {
			return err
		}
		if c := byID[id]; c != nil {
			c.BlockedBy = append(c.BlockedBy, blocker)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = q.QueryContext(ctx, `SELECT card_id, label FROM card_labels WHERE card_id `+cond+`
		ORDER BY card_id, label`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var l string
		if err := rows.Scan(&id, &l); err != nil {
			return err
		}
		if c := byID[id]; c != nil {
			c.Labels = append(c.Labels, l)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = q.QueryContext(ctx, `SELECT card_id, kind, target FROM card_links WHERE card_id `+cond+`
		ORDER BY card_id, rowid`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var kind, target string
		if err := rows.Scan(&id, &kind, &target); err != nil {
			return err
		}
		c := byID[id]
		switch {
		case c == nil:
		case kind == "doc":
			c.Docs = append(c.Docs, target)
		default:
			c.Conversations = append(c.Conversations, target)
		}
	}
	return rows.Err()
}

// Filter selects cards. The zero Filter is every card on the board, archived ones
// excepted.
type Filter struct {
	// Column is one column; empty means all but Archived.
	Column Column
	// Assignee is AssignUser with AssigneeID, AssignAgent, or "none" for unassigned
	// cards; empty means anyone's.
	Assignee   AssigneeKind
	AssigneeID int64
	// Blocked, if set, keeps only blocked or only unblocked cards.
	Blocked *bool
	// Text keeps cards whose title or body contains it, regardless of ASCII case.
	Text string
	// Labels keeps cards that have every one of them (compared as normalized, so "Bug" is
	// "bug").
	Labels []string
	// DueBefore and DueAfter, calendar dates (YYYY-MM-DD), keep cards due on or before, or on
	// or after, that day; a card with no due date is never kept by either.
	DueBefore, DueAfter string
	// Overdue keeps cards due before Today (a date, default today in UTC) that aren't done or
	// archived. Only the clients know their people's time zone, so they can send theirs.
	Overdue bool
	Today   string
	// Sort is SortBoard (the default, "") or SortDue.
	Sort Sort
}

// Sort orders the cards a list returns.
type Sort string

// The orders of a list: board order (by column, then rank), or by due date, soonest first
// with the cards that have none last, each group in board order.
const (
	SortBoard Sort = "board"
	SortDue   Sort = "due"
)

// Unassigned is the Filter.Assignee for cards nobody is assigned to.
const Unassigned AssigneeKind = "none"

func (s *Service) listCards(ctx context.Context, sp access.Space, boardID int64, f Filter) ([]Card, error) {
	if err := may(sp, access.Read); err != nil {
		return nil, err
	}
	if _, err := board(ctx, s.db, sp.ID, boardID); err != nil {
		return nil, err
	}
	where := []string{`c.board_id = ?`, `b.space_id = ?`}
	args := []any{boardID, sp.ID}
	switch {
	case f.Column == "":
		where = append(where, `c.col != 'archived'`)
	case f.Column.valid():
		where, args = append(where, `c.col = ?`), append(args, f.Column)
	default:
		return nil, invalid("column must be one of backlog, todo, doing, review, done, archived")
	}
	switch f.Assignee {
	case "":
	case AssignUser:
		where, args = append(where, `c.assignee_id = ?`), append(args, f.AssigneeID)
	case AssignAgent:
		where = append(where, `c.assignee_agent`)
	case Unassigned:
		where = append(where, `c.assignee_id IS NULL AND NOT c.assignee_agent`)
	default:
		return nil, invalid(`assignee must be "me", "agent", "none" or a user ID`)
	}
	if f.Blocked != nil {
		cond := `(c.blocked_reason IS NOT NULL OR EXISTS (SELECT 1 FROM card_blockers cb WHERE cb.card_id = c.id))`
		if !*f.Blocked {
			cond = `NOT ` + cond
		}
		where = append(where, cond)
	}
	if f.Text != "" {
		pattern := "%" + likeEscaper.Replace(f.Text) + "%"
		where = append(where, `(c.title LIKE ? ESCAPE '\' OR c.body LIKE ? ESCAPE '\')`)
		args = append(args, pattern, pattern)
	}
	for _, l := range f.Labels {
		n, err := normalizeLabel(l)
		if err != nil {
			return nil, err
		}
		where = append(where, `EXISTS (SELECT 1 FROM card_labels cl WHERE cl.card_id = c.id AND cl.label = ?)`)
		args = append(args, n)
	}
	for _, d := range []struct{ name, value, cond string }{
		{"due_before", f.DueBefore, `c.due <= ?`},
		{"due_after", f.DueAfter, `c.due >= ?`},
	} {
		if d.value == "" {
			continue
		}
		if _, ok := checkDate(d.value); !ok {
			return nil, invalidAs(apierr.CodeCardDueInvalid, nil, d.name+` must be a calendar date like "2026-03-31"`)
		}
		where, args = append(where, d.cond), append(args, d.value)
	}
	if f.Overdue {
		today := f.Today
		if today == "" {
			today = time.Now().UTC().Format(dateLayout)
		} else if _, ok := checkDate(today); !ok {
			return nil, invalidAs(apierr.CodeCardDueInvalid, nil, `today must be a calendar date like "2026-03-31"`)
		}
		where, args = append(where, `c.due < ? AND c.col NOT IN ('done', 'archived')`), append(args, today)
	}
	order := columnOrder + `, c.rank, c.id`
	switch f.Sort {
	case "", SortBoard:
	case SortDue:
		order = `c.due IS NULL, c.due, ` + order
	default:
		return nil, invalid(`sort must be "board" or "due"`)
	}
	// #nosec G202 -- the conditions joined are constants above; values go in args.
	rows, err := s.db.QueryContext(ctx, `SELECT `+cardColumns+`
		WHERE `+strings.Join(where, ` AND `)+`
		ORDER BY `+order, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cards := []Card{}
	for rows.Next() {
		c, err := scanCard(rows)
		if err != nil {
			return nil, err
		}
		cards = append(cards, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(cards) == 0 {
		return cards, nil
	}
	err = addRelations(ctx, s.db, cards, `IN (SELECT id FROM cards WHERE board_id = ?)`, boardID)
	return cards, err
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func (s *Service) getThread(ctx context.Context, sp access.Space, boardID, cardID int64) (Thread, error) {
	if err := may(sp, access.Read); err != nil {
		return Thread{}, err
	}
	c, err := card(ctx, s.db, sp.ID, boardID, cardID)
	if err != nil {
		return Thread{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+commentColumns+` WHERE cc.card_id = ? ORDER BY cc.id`, cardID)
	if err != nil {
		return Thread{}, err
	}
	defer rows.Close()
	t := Thread{Card: c, Comments: []Comment{}}
	for rows.Next() {
		cm, err := scanComment(rows)
		if err != nil {
			return Thread{}, err
		}
		t.Comments = append(t.Comments, cm)
	}
	return t, rows.Err()
}

const commentColumns = `cc.id, cc.card_id, cc.author_id, coalesce(u.name, ''), cc.by_agent, cc.via, cc.body, cc.created_at
	FROM card_comments cc LEFT JOIN users u ON u.id = cc.author_id`

func scanComment(row scanner) (Comment, error) {
	var cm Comment
	var authorID sql.NullInt64
	var name string
	var agent bool
	var by sql.NullString
	if err := row.Scan(&cm.ID, &cm.CardID, &authorID, &name, &agent, &by, &cm.Body, &cm.CreatedAt); err != nil {
		return Comment{}, err
	}
	cm.Author = author(authorID, name, agent, by)
	return cm, nil
}

// NewCard is a card to create. Only Title is required.
type NewCard struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	// Column is where the card goes; default Backlog. It can't be Archived.
	Column Column `json:"column"`
	// Position is the card's 0-based place in its column; default (nil) or past the end
	// puts it last.
	Position      *int      `json:"position"`
	Assignee      *Assignee `json:"assignee"`
	BlockedReason *string   `json:"blocked_reason"`
	BlockedBy     []int64   `json:"blocked_by"`
	Docs          []string  `json:"docs"`
	Conversations []string  `json:"conversations"`
	// Due is a calendar date, YYYY-MM-DD; Labels are free text (normalized).
	Due    *string  `json:"due"`
	Labels []string `json:"labels"`
	// Origin makes creating the card idempotent: if the board already has a card made with
	// this origin, that card is returned and nothing is created. It is the server's own
	// (package apps' outbox uses it), never read from a request.
	Origin string `json:"-"`
	// Check, if set, runs in the card's transaction once the board is known to be writable and
	// before anything is made; an error from it makes nothing. Created, if set, runs in the same
	// transaction once the card exists (or, with an Origin, once it was found). Both are the
	// server's own (package apps' outbox), never read from a request.
	Check   func(tx *sql.Tx) error               `json:"-"`
	Created func(tx *sql.Tx, cardID int64) error `json:"-"`
}

func (s *Service) createCard(ctx context.Context, sp access.Space, a Actor, boardID int64, nc NewCard) (Card, error) {
	if err := may(sp, access.Write); err != nil {
		return Card{}, err
	}
	if nc.Column == "" {
		nc.Column = Backlog
	}
	ch := CardChanges{
		Title: &nc.Title, Body: &nc.Body,
		Assignee:      Optional[Assignee]{Set: true, Value: nc.Assignee},
		BlockedReason: Optional[string]{Set: true, Value: nc.BlockedReason},
		BlockedBy:     &nc.BlockedBy, Docs: &nc.Docs, Conversations: &nc.Conversations,
		Due:    Optional[string]{Set: true, Value: nc.Due},
		Labels: &nc.Labels,
	}
	if err := s.validate(ctx, sp, &ch); err != nil {
		return Card{}, err
	}
	if !nc.Column.onBoard() {
		return Card{}, invalid("column must be one of backlog, todo, doing, review, done")
	}
	if err := checkPosition(nc.Position); err != nil {
		return Card{}, err
	}
	var id int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		if _, err := writableBoard(ctx, tx, sp.ID, boardID); err != nil {
			return err
		}
		if nc.Check != nil {
			if err := nc.Check(tx); err != nil {
				return err
			}
		}
		if nc.Origin != "" {
			// Made before: that card, whatever has happened to it since.
			err := tx.QueryRowContext(ctx, `SELECT id FROM cards WHERE board_id = ? AND origin = ?`, boardID, nc.Origin).Scan(&id)
			if err == nil {
				if nc.Created != nil {
					return nc.Created(tx, id)
				}
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		rank, err := rankAt(ctx, tx, boardID, nc.Column, 0, nc.Position)
		if err != nil {
			return err
		}
		var origin any
		if nc.Origin != "" {
			origin = nc.Origin
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO cards (board_id, title, body, col, rank, origin,
				created_by, created_by_agent, created_via, updated_by, updated_by_agent, updated_via)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			boardID, *ch.Title, *ch.Body, nc.Column, rank, origin, a.UserID, a.Agent, via(a), a.UserID, a.Agent, via(a))
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		if err := applyChanges(ctx, tx, sp, a, boardID, id, ch); err != nil {
			return err
		}
		if nc.Created != nil {
			return nc.Created(tx, id)
		}
		return nil
	})
	if err != nil {
		return Card{}, err
	}
	return card(ctx, s.db, sp.ID, boardID, id)
}

// CardChanges is an edit of a card. Fields left out (nil, or not Set) stay as they are.
type CardChanges struct {
	// Version is the card's version the change was made against.
	Version int64   `json:"version"`
	Title   *string `json:"title"`
	Body    *string `json:"body"`
	// Assignee set to null unassigns the card.
	Assignee Optional[Assignee] `json:"assignee"`
	// BlockedReason set to null clears it.
	BlockedReason Optional[string] `json:"blocked_reason"`
	// BlockedBy, Docs and Conversations replace the whole list; [] clears it.
	BlockedBy     *[]int64  `json:"blocked_by"`
	Docs          *[]string `json:"docs"`
	Conversations *[]string `json:"conversations"`
	// Due is a calendar date, YYYY-MM-DD; null clears it.
	Due Optional[string] `json:"due"`
	// Labels replaces the card's labels; [] clears them.
	Labels *[]string `json:"labels"`
}

func (ch CardChanges) empty() bool {
	return ch.Title == nil && ch.Body == nil && !ch.Assignee.Set && !ch.BlockedReason.Set &&
		ch.BlockedBy == nil && ch.Docs == nil && ch.Conversations == nil && !ch.Due.Set && ch.Labels == nil
}

func (s *Service) updateCard(ctx context.Context, sp access.Space, a Actor, boardID, cardID int64, ch CardChanges) (Card, error) {
	if err := may(sp, access.Write); err != nil {
		return Card{}, err
	}
	if ch.empty() {
		return Card{}, invalid("nothing to change")
	}
	if err := s.validate(ctx, sp, &ch); err != nil {
		return Card{}, err
	}
	return s.changeCard(ctx, sp, a, boardID, cardID, ch.Version, func(tx *sql.Tx, _ Card) error {
		return applyChanges(ctx, tx, sp, a, boardID, cardID, ch)
	})
}

// Move puts a card in a column, at a position.
type Move struct {
	// Version is the card's version the move was made against.
	Version int64 `json:"version"`
	// Column is any but Archived (see archive and restore).
	Column Column `json:"column"`
	// Position is the card's 0-based place in the column, not counting the card itself;
	// nil or past the end puts it last.
	Position *int `json:"position"`
}

func (s *Service) moveCard(ctx context.Context, sp access.Space, a Actor, boardID, cardID int64, m Move) (Card, error) {
	if err := may(sp, access.Write); err != nil {
		return Card{}, err
	}
	if !m.Column.onBoard() {
		return Card{}, invalid("column must be one of backlog, todo, doing, review, done (archive a card to archive it)")
	}
	if err := checkPosition(m.Position); err != nil {
		return Card{}, err
	}
	return s.changeCard(ctx, sp, a, boardID, cardID, m.Version, func(tx *sql.Tx, _ Card) error {
		return place(ctx, tx, boardID, cardID, m.Column, m.Position, nil)
	})
}

// moveCardIf puts a card in column, as it is when the write lock is held, whatever its version.
// guard runs first, in the same transaction, with the card as it is: it says whether to go on, and
// what it writes is committed either way, so a caller can note, atomically with the move, that
// the move was made. A card already in column isn't touched. moved says whether the card changed.
func (s *Service) moveCardIf(ctx context.Context, sp access.Space, a Actor, boardID, cardID int64, column Column,
	guard func(tx *sql.Tx, c Card) (bool, error),
) (c Card, moved bool, err error) {
	if err := may(sp, access.Write); err != nil {
		return Card{}, false, err
	}
	if !column.onBoard() {
		return Card{}, false, invalid("column must be one of backlog, todo, doing, review, done")
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		if _, err := writableBoard(ctx, tx, sp.ID, boardID); err != nil {
			return err
		}
		cur, err := card(ctx, tx, sp.ID, boardID, cardID)
		if err != nil {
			return err
		}
		if guard != nil {
			ok, err := guard(tx, cur)
			if err != nil || !ok {
				return err
			}
		}
		if cur.Column == column || cur.Column == Archived {
			return nil
		}
		if err := place(ctx, tx, boardID, cardID, column, nil, nil); err != nil {
			return err
		}
		moved = true
		_, err = tx.ExecContext(ctx, `
			UPDATE cards SET version = version + 1, updated_at = ?, updated_by = ?, updated_by_agent = ?, updated_via = ?
			WHERE id = ?`, now(), a.UserID, a.Agent, via(a), cardID)
		return err
	})
	if err != nil {
		return Card{}, false, err
	}
	c, err = card(ctx, s.db, sp.ID, boardID, cardID)
	return c, moved, err
}

// archiveCard moves a card to Archived, remembering its column.
func (s *Service) archiveCard(ctx context.Context, sp access.Space, a Actor, boardID, cardID, version int64) (Card, error) {
	if err := may(sp, access.Write); err != nil {
		return Card{}, err
	}
	return s.changeCard(ctx, sp, a, boardID, cardID, version, func(tx *sql.Tx, c Card) error {
		if c.Column == Archived {
			return invalid("the card is already archived")
		}
		return place(ctx, tx, boardID, cardID, Archived, nil, &c.Column)
	})
}

// restoreCard puts an archived card back, last in column, or in the column it was
// archived from if column is "".
func (s *Service) restoreCard(ctx context.Context, sp access.Space, a Actor, boardID, cardID, version int64, column Column) (Card, error) {
	if err := may(sp, access.Write); err != nil {
		return Card{}, err
	}
	if column != "" && !column.onBoard() {
		return Card{}, invalid("column must be one of backlog, todo, doing, review, done")
	}
	return s.changeCard(ctx, sp, a, boardID, cardID, version, func(tx *sql.Tx, c Card) error {
		if c.Column != Archived {
			return invalid("the card isn't archived")
		}
		to := column
		if to == "" {
			to = *c.ArchivedFrom
		}
		return place(ctx, tx, boardID, cardID, to, nil, nil)
	})
}

// changeCard runs change on a card of a writable board, if the card is at version, then
// bumps its version and records a as its last author. It returns the changed card.
func (s *Service) changeCard(ctx context.Context, sp access.Space, a Actor, boardID, cardID, version int64,
	change func(*sql.Tx, Card) error,
) (Card, error) {
	if err := checkVersion(version); err != nil {
		return Card{}, err
	}
	err := s.write(ctx, func(tx *sql.Tx) error {
		if _, err := writableBoard(ctx, tx, sp.ID, boardID); err != nil {
			return err
		}
		c, err := card(ctx, tx, sp.ID, boardID, cardID)
		if err != nil {
			return err
		}
		if c.Version != version {
			return ErrStale
		}
		if err := change(tx, c); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE cards SET version = version + 1, updated_at = ?, updated_by = ?, updated_by_agent = ?, updated_via = ?
			WHERE id = ?`, now(), a.UserID, a.Agent, via(a), cardID)
		return err
	})
	if err != nil {
		return Card{}, err
	}
	return card(ctx, s.db, sp.ID, boardID, cardID)
}

// place moves a card to position in column. archivedFrom is the column it leaves, when
// column is Archived.
func place(ctx context.Context, tx *sql.Tx, boardID, cardID int64, column Column, position *int, archivedFrom *Column) error {
	rank, err := rankAt(ctx, tx, boardID, column, cardID, position)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE cards SET col = ?, rank = ?, archived_from = ? WHERE id = ?`,
		column, rank, archivedFrom, cardID)
	return err
}

// rankAt returns the rank for a card at position in a column of boardID, not counting
// the card cardID (the one being moved; 0 for a new card).
func rankAt(ctx context.Context, tx *sql.Tx, boardID int64, column Column, cardID int64, position *int) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT rank FROM cards WHERE board_id = ? AND col = ? AND id != ? ORDER BY rank`,
		boardID, column, cardID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ranks []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return "", err
		}
		ranks = append(ranks, r)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	i := len(ranks)
	if position != nil && *position < i {
		i = *position
	}
	before, after := "", ""
	if i > 0 {
		before = ranks[i-1]
	}
	if i < len(ranks) {
		after = ranks[i]
	}
	return rankBetween(before, after), nil
}

// checkVersion refuses a missing version: versions start at 1.
func checkVersion(v int64) error {
	if v < 1 {
		return invalid("version is required: the version you last read")
	}
	return nil
}

func checkPosition(p *int) error {
	if p != nil && *p < 0 {
		return invalid("position must be 0 or more")
	}
	return nil
}

// applyChanges writes the validated changes ch, which a makes, to a card of sp; it doesn't
// touch the version.
func applyChanges(ctx context.Context, tx *sql.Tx, sp access.Space, a Actor, boardID, cardID int64, ch CardChanges) error {
	if ch.Title != nil || ch.Body != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE cards SET title = coalesce(?, title), body = coalesce(?, body) WHERE id = ?`,
			ch.Title, ch.Body, cardID); err != nil {
			return err
		}
	}
	if ch.Assignee.Set {
		var userID any
		agent := false
		if as := ch.Assignee.Value; as != nil {
			agent = as.Kind == AssignAgent
			if !agent {
				userID = as.ID
			}
		}
		var previous sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT assignee_id FROM cards WHERE id = ?`, cardID).Scan(&previous); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE cards SET assignee_id = ?, assignee_agent = ? WHERE id = ?`,
			userID, agent, cardID); err != nil {
			return err
		}
		// Someone newly assigned a card by someone else is told, in their inbox (and by push, if
		// they chose that). Assigning yourself, or the same person again, tells nobody.
		if as := ch.Assignee.Value; as != nil && !agent && as.ID != a.UserID && (!previous.Valid || previous.Int64 != as.ID) {
			if err := notifications.Add(ctx, tx, notifications.Notice{
				UserID: as.ID, WorkspaceID: sp.WorkspaceID, Kind: notifications.KindCardAssigned,
				Ref: notifications.Ref{Space: sp.ID, Board: boardID, Card: cardID},
			}); err != nil {
				return err
			}
		}
	}
	if ch.BlockedReason.Set {
		if _, err := tx.ExecContext(ctx, `UPDATE cards SET blocked_reason = ? WHERE id = ?`, ch.BlockedReason.Value, cardID); err != nil {
			return err
		}
	}
	if ch.Due.Set {
		if _, err := tx.ExecContext(ctx, `UPDATE cards SET due = ? WHERE id = ?`, ch.Due.Value, cardID); err != nil {
			return err
		}
	}
	if ch.Labels != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM card_labels WHERE card_id = ?`, cardID); err != nil {
			return err
		}
		for _, l := range *ch.Labels {
			if _, err := tx.ExecContext(ctx, `INSERT INTO card_labels (card_id, label) VALUES (?, ?)`, cardID, l); err != nil {
				return err
			}
		}
	}
	if ch.BlockedBy != nil {
		if err := setBlockers(ctx, tx, boardID, cardID, *ch.BlockedBy); err != nil {
			return err
		}
	}
	if ch.Conversations != nil {
		if err := checkConversations(ctx, tx, sp, a, cardID, *ch.Conversations); err != nil {
			return err
		}
	}
	for kind, targets := range map[string]*[]string{"doc": ch.Docs, "conversation": ch.Conversations} {
		if targets == nil {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM card_links WHERE card_id = ? AND kind = ?`, cardID, kind); err != nil {
			return err
		}
		for _, t := range *targets {
			if _, err := tx.ExecContext(ctx, `INSERT INTO card_links (card_id, kind, target) VALUES (?, ?, ?)`, cardID, kind, t); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkConversations checks the conversations a card is to link to: each one it doesn't
// link to yet must be a's own, in sp's workspace. Conversations are private, so a link is
// only ever added by their owner (and the agent working for them); anyone who may edit
// the card may keep or remove it. Opening a linked conversation is the conversation API's
// owner-only check again.
func checkConversations(ctx context.Context, tx *sql.Tx, sp access.Space, a Actor, cardID int64, links []string) error {
	rows, err := tx.QueryContext(ctx, `SELECT target FROM card_links WHERE card_id = ? AND kind = 'conversation'`, cardID)
	if err != nil {
		return err
	}
	var linked []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return err
		}
		linked = append(linked, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, l := range links {
		if slices.Contains(linked, l) {
			continue
		}
		id, _ := conversationID(l) // validated
		ok, err := conversations.Owns(ctx, tx, sp.WorkspaceID, a.UserID, id)
		if err != nil {
			return err
		}
		if !ok {
			// The same answer whether it exists or not, as for a space.
			return invalid(fmt.Sprintf("conversations: %s isn't one of your conversations in this workspace", l))
		}
	}
	return nil
}

// setBlockers replaces the cards cardID waits for: other cards of its board.
func setBlockers(ctx context.Context, tx *sql.Tx, boardID, cardID int64, blockers []int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM card_blockers WHERE card_id = ?`, cardID); err != nil {
		return err
	}
	for _, id := range blockers {
		if id == cardID {
			return invalid("a card can't be blocked by itself")
		}
		var ok bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM cards WHERE id = ? AND board_id = ?)`, id, boardID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			// The card can go meanwhile: deleting it doesn't change the blocked card's version.
			return invalidAs(apierr.CodeCardBlockerNotFound, apierr.Params{"id": id}, "blocked_by: no card "+strconv.FormatInt(id, 10)+" on this board")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO card_blockers (card_id, blocker_id) VALUES (?, ?)`, cardID, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) comment(ctx context.Context, sp access.Space, a Actor, boardID, cardID int64, body string) (Comment, error) {
	if err := may(sp, access.Write); err != nil {
		return Comment{}, err
	}
	body, err := commentBody(body)
	if err != nil {
		return Comment{}, err
	}
	var id int64
	err = s.write(ctx, func(tx *sql.Tx) error {
		if _, err := writableBoard(ctx, tx, sp.ID, boardID); err != nil {
			return err
		}
		if _, err := card(ctx, tx, sp.ID, boardID, cardID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO card_comments (card_id, author_id, by_agent, via, body) VALUES (?, ?, ?, ?, ?)`,
			cardID, a.UserID, a.Agent, via(a), body)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return Comment{}, err
	}
	return scanComment(s.db.QueryRowContext(ctx, `SELECT `+commentColumns+` WHERE cc.id = ?`, id))
}
