package whiteboard

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/knowledge"
	"taskto.ai/taskto/internal/live"
	"taskto.ai/taskto/internal/ratelimit"
	"taskto.ai/taskto/internal/whiteboard/shapes"
)

const (
	Kind         = "whiteboard"
	EventOps     = "ops"
	FlushEvery   = 2 * time.Minute
	SnapshotNote = "Whiteboard edited"
	rateBatches  = 120
	rateWindow   = 10 * time.Second
)

var errGone = apierr.New(http.StatusNotFound, apierr.CodeDocNotFound, "that whiteboard doesn't exist, or no longer does")

type Options struct {
	Now         func() time.Time
	After       func(d time.Duration, f func()) (stop func())
	BeforeApply func(path string)
}

type Snapshot struct {
	shapes.Board
	Seq int64 `json:"seq"`
}

type Service struct {
	db      *sql.DB
	kb      *knowledge.Store
	hub     *live.Hub
	checker *access.Checker
	limit   *ratelimit.Limiter
	opts    Options

	loadMu sync.Mutex
	mu     sync.Mutex
	open   map[key]*board
}

type key struct {
	space, generation int64
	path              string
}

type board struct {
	svc  *Service
	id   int64
	key  key
	sp   access.Space
	mu   sync.Mutex
	gone bool

	state     shapes.Board
	seq       int64
	committed int64
	version   string
	editor    int64
	stop      func()
}

func New(db *sql.DB, kb *knowledge.Store, hub *live.Hub, checker *access.Checker, opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.After == nil {
		opts.After = func(d time.Duration, f func()) func() {
			t := time.AfterFunc(d, f)
			return func() { t.Stop() }
		}
	}
	return &Service{db: db, kb: kb, hub: hub, checker: checker, opts: opts, open: map[key]*board{},
		limit: ratelimit.New(rateBatches, rateWindow, opts.Now)}
}

func (b *board) topic() live.Topic {
	return live.Topic{Kind: Kind, SpaceID: b.key.space, Generation: b.key.generation, ResourceID: b.id}
}

func (s *Service) board(ctx context.Context, sp access.Space, path string) (*board, error) {
	k := key{sp.ID, sp.Generation, path}
	if b := s.cached(k); b != nil {
		return b, nil
	}
	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	if b := s.cached(k); b != nil {
		return b, nil
	}
	doc, err := s.kb.Read(ctx, sp, path, "")
	if err != nil {
		return nil, err
	}
	state, err := shapes.Parse(doc.Content)
	if err != nil {
		return nil, err
	}
	b := &board{svc: s, key: k, sp: sp, state: state, version: doc.Version}
	if err := b.restore(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.open[k] = b
	s.mu.Unlock()
	return b, nil
}

func (s *Service) cached(k key) *board {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.open[k]
}

func (b *board) restore(ctx context.Context) error {
	tx, err := b.svc.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var version string
	var editor sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT id, version, seq, committed_seq, editor_id FROM whiteboards
		WHERE space_id = ? AND generation = ? AND path = ?`, b.key.space, b.key.generation, b.key.path).
		Scan(&b.id, &version, &b.seq, &b.committed, &editor)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		err = tx.QueryRowContext(ctx, `INSERT INTO whiteboards (space_id, generation, path, version)
			SELECT id, generation, ?, ? FROM spaces WHERE id = ? AND generation = ?
			RETURNING id`, b.key.path, b.version, b.key.space, b.key.generation).Scan(&b.id)
		if errors.Is(err, sql.ErrNoRows) {
			err = access.ErrNotFound
		}
	case err == nil && version != b.version:
		b.seq++
		b.committed = b.seq
		_, err = tx.ExecContext(ctx, `DELETE FROM whiteboard_ops WHERE board_id = ?`, b.id)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE whiteboards SET version = ?, seq = ?, committed_seq = ? WHERE id = ?`, b.version, b.seq, b.seq, b.id)
		}
	case err == nil:
		b.editor = editor.Int64
		err = b.replay(ctx, tx)
	}
	if err == nil {
		err = tx.Commit()
	}
	return err
}

func (b *board) replay(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT seq, ops FROM whiteboard_ops WHERE board_id = ? AND seq > ? ORDER BY seq`, b.id, b.committed)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var raw string
		var ops []shapes.Op
		if err := rows.Scan(&seq, &raw); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(raw), &ops); err != nil {
			return fmt.Errorf("whiteboard %d: the log at %d is damaged: %w", b.id, seq, err)
		}
		if b.state, err = shapes.Apply(b.state, ops); err != nil {
			return fmt.Errorf("whiteboard %d: the log at %d doesn't apply: %w", b.id, seq, err)
		}
	}
	return rows.Err()
}

func (s *Service) Snapshot(ctx context.Context, sp access.Space, path string) (Snapshot, error) {
	for {
		b, err := s.board(ctx, sp, path)
		if err != nil {
			return Snapshot{}, err
		}
		b.mu.Lock()
		snap, dead := Snapshot{Board: b.state, Seq: b.seq}, b.gone
		b.mu.Unlock()
		if !dead {
			return snap, nil
		}
	}
}

var errRetired = errors.New("whiteboard: the board was put away while the request waited")

func (s *Service) Apply(ctx context.Context, user int64, sp access.Space, path string, ops []shapes.Op) (int64, error) {
	if ok, _ := s.limit.Take(fmt.Sprint(user)); !ok {
		return 0, apierr.New(http.StatusTooManyRequests, apierr.CodeWhiteboardRateLimited, "too many changes at once: slow down")
	}
	if s.opts.BeforeApply != nil {
		s.opts.BeforeApply(path)
	}
	for {
		b, err := s.board(ctx, sp, path)
		if err != nil {
			return 0, err
		}
		if seq, err := b.apply(ctx, user, ops); !errors.Is(err, errRetired) {
			return seq, err
		}
	}
}

func (b *board) apply(ctx context.Context, user int64, ops []shapes.Op) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.gone {
		return 0, errRetired
	}
	now, err := b.svc.checker.Check(ctx, user, b.key.space)
	switch {
	case errors.Is(err, access.ErrNotFound):
		return 0, err
	case err != nil:
		return 0, err
	case !now.Access.Allows(access.Write):
		return 0, apierr.New(http.StatusForbidden, apierr.CodeSpaceReadOnly, knowledge.ErrReadOnly.Error())
	}
	next, err := shapes.Apply(b.state, ops)
	if err != nil {
		return 0, err
	}
	raw, err := json.Marshal(ops)
	if err != nil {
		return 0, err
	}
	seq := b.seq + 1
	tx, err := b.svc.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE whiteboards SET seq = ?, editor_id = ? WHERE id = ? AND seq = ?`, seq, user, b.id, b.seq)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, errGone
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO whiteboard_ops (board_id, seq, ops) VALUES (?, ?, ?)`, b.id, seq, string(raw)); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	b.state, b.seq, b.editor = next, seq, user
	b.announce(EventOps, map[string]any{"seq": seq, "ops": json.RawMessage(raw)})
	if b.stop == nil {
		b.stop = b.svc.schedule(b)
	}
	return seq, nil
}

func (b *board) announce(typ string, data map[string]any) {
	payload, err := json.Marshal(data)
	if err == nil {
		_, err = b.svc.hub.Publish(b.topic(), typ, payload)
	}
	if err != nil {
		slog.Error("whiteboard: announcing a change", "board", b.id, "err", err)
	}
}

func (s *Service) schedule(b *board) func() {
	return s.opts.After(FlushEvery, func() { s.flush(context.Background(), b) })
}

func (s *Service) flush(ctx context.Context, b *board) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stop != nil {
		b.stop()
		b.stop = nil
	}
	if b.gone || b.seq == b.committed {
		return
	}
	if err := b.save(ctx); err != nil {
		slog.Warn("whiteboard: saving a snapshot; it is tried again", "board", b.id, "err", err)
		b.stop = s.schedule(b)
	}
}

func (b *board) save(ctx context.Context) error {
	by := knowledge.Author{Name: "Whiteboard", Email: "whiteboard@localhost"}
	err := b.svc.db.QueryRowContext(ctx, `SELECT name, email FROM users WHERE id = ?`, b.editor).Scan(&by.Name, &by.Email)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	content, err := shapes.Encode(b.state)
	if err != nil {
		return err
	}
	sp := b.sp
	sp.Access = access.Write
	res, err := b.svc.kb.Put(ctx, sp, b.key.path, content, b.version, by, SnapshotNote)
	if errors.Is(err, knowledge.ErrConflict) || errors.Is(err, access.ErrNotFound) {
		return b.drop(ctx, err)
	}
	if err != nil {
		return err
	}
	if _, err := b.svc.db.ExecContext(ctx, `UPDATE whiteboards SET version = ?, committed_seq = ? WHERE id = ?`, res.Doc.Version, b.seq, b.id); err != nil {
		return err
	}
	if _, err := b.svc.db.ExecContext(ctx, `DELETE FROM whiteboard_ops WHERE board_id = ? AND seq <= ?`, b.id, b.seq); err != nil {
		return err
	}
	b.evict()
	return nil
}

func (b *board) evict() {
	b.gone = true
	b.svc.mu.Lock()
	delete(b.svc.open, b.key)
	b.svc.mu.Unlock()
}

func (b *board) drop(ctx context.Context, cause error) error {
	slog.Warn("whiteboard: its document was changed or removed by someone else; dropping the unsaved changes", "board", b.id, "cause", cause)
	_, err := b.svc.db.ExecContext(ctx, `DELETE FROM whiteboards WHERE id = ?`, b.id)
	b.evict()
	b.svc.hub.Revoke(func(t live.Topic, _ int64) bool { return t == b.topic() })
	return err
}

func (s *Service) Close(ctx context.Context) {
	s.mu.Lock()
	boards := make([]*board, 0, len(s.open))
	for _, b := range s.open {
		boards = append(boards, b)
	}
	s.mu.Unlock()
	for _, b := range boards {
		s.flush(ctx, b)
	}
}

func (s *Service) stillThere(ctx context.Context, b *board) error {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM whiteboards WHERE id = ? AND space_id = ? AND generation = ?`,
		b.id, b.key.space, b.key.generation).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return errGone
	}
	return err
}
