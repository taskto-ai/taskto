package boards

import (
	"context"
	"database/sql"
	"encoding/json"
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

const timeFormat = "2006-01-02T15:04:05.000Z"

func now() string { return time.Now().UTC().Format(timeFormat) }

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type scanner interface{ Scan(dest ...any) error }

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

var errSpaceArchived = readOnlyError("this space is archived: it is read-only")

type readOnlyError string

func (e readOnlyError) Error() string        { return string(e) }
func (e readOnlyError) Is(target error) bool { return target == ErrReadOnly }

func author(id sql.NullInt64, name string, agent bool, via sql.NullString) *Author {
	if !id.Valid {
		return nil
	}
	return &Author{ID: id.Int64, Name: name, Agent: agent, Via: via.String}
}

func via(a Actor) any {
	if a.Via == "" {
		return nil
	}
	return a.Via
}

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

func board(ctx context.Context, q queryer, spaceID, boardID int64) (Board, error) {
	b, err := scanBoard(q.QueryRowContext(ctx, `SELECT `+boardColumns+` WHERE b.id = ? AND b.space_id = ?`, boardID, spaceID))
	if errors.Is(err, sql.ErrNoRows) {
		return Board{}, ErrNotFound
	}
	return b, err
}

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

type BoardChanges struct {
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
		unarchiving := ch.Archived != nil && !*ch.Archived
		if b.ArchivedAt != nil && !unarchiving {
			return ErrBoardArchived
		}
		var archivedAt any
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
		if err != nil {
			return err
		}
		if ch.Archived != nil {
			return s.syncBoard(ctx, tx, boardID)
		}
		return nil
	})
	if err != nil {
		return Board{}, err
	}
	return board(ctx, s.db, sp.ID, boardID)
}

const cardColumns = `c.id, c.board_id, c.title, c.body, c.col, c.rank, c.archived_from,
	c.assignee_id, coalesce(au.name, ''), c.assignee_agent, c.assignee_deleted, c.blocked_reason,
	EXISTS (SELECT 1 FROM card_blockers cb WHERE cb.card_id = c.id), c.due,
	(SELECT json_group_array(label) FROM (SELECT label FROM card_labels cl WHERE cl.card_id = c.id ORDER BY label)),
	c.version,
	c.created_at, c.created_by, coalesce(cu.name, ''), c.created_by_agent, c.created_via,
	c.updated_at, c.updated_by, coalesce(uu.name, ''), c.updated_by_agent, c.updated_via
	FROM cards c
	JOIN boards b ON b.id = c.board_id
	LEFT JOIN users au ON au.id = c.assignee_id
	LEFT JOIN users cu ON cu.id = c.created_by
	LEFT JOIN users uu ON uu.id = c.updated_by`

const columnOrder = `CASE c.col WHEN 'backlog' THEN 0 WHEN 'todo' THEN 1 WHEN 'doing' THEN 2
	WHEN 'review' THEN 3 WHEN 'done' THEN 4 ELSE 5 END`

func scanCard(row scanner) (Card, error) {
	var c Card
	var assigneeID, createdBy, updatedBy sql.NullInt64
	var assigneeName, createdName, updatedName string
	var createdVia, updatedVia sql.NullString
	var agent, assigneeDeleted, hasBlockers, createdAgent, updatedAgent bool
	var labels string
	if err := row.Scan(&c.ID, &c.BoardID, &c.Title, &c.Body, &c.Column, &c.Rank, &c.ArchivedFrom,
		&assigneeID, &assigneeName, &agent, &assigneeDeleted, &c.BlockedReason, &hasBlockers, &c.Due, &labels, &c.Version,
		&c.CreatedAt, &createdBy, &createdName, &createdAgent, &createdVia,
		&c.UpdatedAt, &updatedBy, &updatedName, &updatedAgent, &updatedVia); err != nil {
		return Card{}, err
	}
	switch {
	case agent:
		c.Assignee = &Assignee{Kind: AssignAgent}
	case assigneeID.Valid:
		c.Assignee = &Assignee{Kind: AssignUser, ID: assigneeID.Int64, Name: assigneeName}
	case assigneeDeleted:
		c.Assignee = &Assignee{Kind: AssignDeleted}
	}
	c.Blocked = c.BlockedReason != nil || hasBlockers
	c.CreatedBy = author(createdBy, createdName, createdAgent, createdVia)
	c.UpdatedBy = author(updatedBy, updatedName, updatedAgent, updatedVia)
	c.BlockedBy, c.Docs, c.Conversations = []int64{}, []string{}, []string{}
	if err := json.Unmarshal([]byte(labels), &c.Labels); err != nil {
		return Card{}, fmt.Errorf("card %d's labels: %w", c.ID, err)
	}
	return c, nil
}

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

type Filter struct {
	Column              Column
	Assignee            AssigneeKind
	AssigneeID          int64
	Blocked             *bool
	Text                string
	Labels              []string
	DueBefore, DueAfter string
	Overdue             bool
	Today               string
	Sort                Sort
}

type Sort string

const (
	SortBoard Sort = "board"
	SortDue   Sort = "due"
)

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
	if len(f.Labels) > maxLabels {
		return nil, invalidAs(apierr.CodeTooManyLabels, apierr.Params{"max": maxLabels},
			fmt.Sprintf("a list filters by at most %d labels", maxLabels))
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

func (s *Service) getThread(ctx context.Context, sp access.Space, a Actor, boardID, cardID int64) (Thread, error) {
	if err := may(sp, access.Read); err != nil {
		return Thread{}, err
	}
	t, err := threadIn(ctx, s.db, sp.ID, boardID, cardID)
	if err != nil {
		return Thread{}, err
	}
	return t, s.withMentions(ctx, sp, a.UserID, &t)
}

func threadIn(ctx context.Context, q queryer, spaceID, boardID, cardID int64) (Thread, error) {
	c, err := card(ctx, q, spaceID, boardID, cardID)
	if err != nil {
		return Thread{}, err
	}
	rows, err := q.QueryContext(ctx, `SELECT `+commentColumns+` WHERE cc.card_id = ? ORDER BY cc.id`, cardID)
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

func (s *Service) withMentions(ctx context.Context, sp access.Space, viewer int64, t *Thread) error {
	if err := s.mentionedPeople(ctx, sp, viewer, t.Comments); err != nil {
		if errors.Is(err, access.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	return nil
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

type NewCard struct {
	Title         string                               `json:"title"`
	Body          string                               `json:"body"`
	Column        Column                               `json:"column"`
	Position      *int                                 `json:"position"`
	Assignee      *Assignee                            `json:"assignee"`
	BlockedReason *string                              `json:"blocked_reason"`
	BlockedBy     []int64                              `json:"blocked_by"`
	Docs          []string                             `json:"docs"`
	Conversations []string                             `json:"conversations"`
	Due           *string                              `json:"due"`
	Labels        []string                             `json:"labels"`
	Origin        string                               `json:"-"`
	Check         func(tx *sql.Tx) error               `json:"-"`
	Created       func(tx *sql.Tx, cardID int64) error `json:"-"`
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
		if err := s.syncReminders(ctx, tx, id, 0); err != nil {
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

type CardChanges struct {
	Version       int64              `json:"version"`
	Title         *string            `json:"title"`
	Body          *string            `json:"body"`
	Assignee      Optional[Assignee] `json:"assignee"`
	BlockedReason Optional[string]   `json:"blocked_reason"`
	BlockedBy     *[]int64           `json:"blocked_by"`
	Docs          *[]string          `json:"docs"`
	Conversations *[]string          `json:"conversations"`
	Due           Optional[string]   `json:"due"`
	Labels        *[]string          `json:"labels"`
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

type Move struct {
	Version  int64  `json:"version"`
	Column   Column `json:"column"`
	Position *int   `json:"position"`
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
		if err := s.syncReminders(ctx, tx, cardID, 0); err != nil {
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
		previous := int64(0)
		if c.Assignee != nil && c.Assignee.Kind == AssignUser {
			previous = c.Assignee.ID
		}
		if err := change(tx, c); err != nil {
			return err
		}
		if err := s.syncReminders(ctx, tx, cardID, previous); err != nil {
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

func place(ctx context.Context, tx *sql.Tx, boardID, cardID int64, column Column, position *int, archivedFrom *Column) error {
	rank, err := rankAt(ctx, tx, boardID, column, cardID, position)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE cards SET col = ?, rank = ?, archived_from = ? WHERE id = ?`,
		column, rank, archivedFrom, cardID)
	return err
}

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
		if _, err := tx.ExecContext(ctx, `UPDATE cards SET assignee_id = ?, assignee_agent = ?, assignee_deleted = 0 WHERE id = ?`,
			userID, agent, cardID); err != nil {
			return err
		}
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
		id, _ := conversationID(l)
		ok, err := conversations.Owns(ctx, tx, sp.WorkspaceID, a.UserID, id)
		if err != nil {
			return err
		}
		if !ok {
			return invalid(fmt.Sprintf("conversations: %s isn't one of your conversations in this workspace", l))
		}
	}
	return nil
}

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
		if err != nil {
			return err
		}
		tell, err := commentRecipients(ctx, tx, sp, a, boardID, cardID, body)
		if err != nil {
			return err
		}
		for _, r := range tell {
			if err := notifications.Add(ctx, tx, notifications.Notice{
				UserID: r.user, WorkspaceID: sp.WorkspaceID, Kind: r.kind,
				Ref:       notifications.Ref{Space: sp.ID, Board: boardID, Card: cardID},
				DedupeKey: "comment:" + strconv.FormatInt(id, 10),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Comment{}, err
	}
	cm, err := scanComment(s.db.QueryRowContext(ctx, `SELECT `+commentColumns+` WHERE cc.id = ?`, id))
	if err != nil {
		return Comment{}, err
	}
	one := []Comment{cm}
	if err := s.mentionedPeople(ctx, sp, a.UserID, one); err != nil {
		one[0].Mentions = []MentionedPerson{}
	}
	return one[0], nil
}
