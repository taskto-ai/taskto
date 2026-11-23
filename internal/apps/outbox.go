package apps

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/boards"
	"taskto.ai/taskto/internal/knowledge"
)

// The transactional outbox of an app's effects.
//
// When an action that has effects changes a record, the change and one entry per effect that
// applies are committed in the same transaction. The request answers then; the effects are done
// afterwards by the worker (StartOutbox), in the server process, so they go on when the phone is
// closed and survive a crash of the server.
//
// Each entry is the n-th effect of an operation, and an operation is named: by the client (the
// idempotency key of its request, ClientAction.Operation) or, if it left it out, by the server.
// The operation and the user who acted are unique, so a request made again, by a client that never
// saw the answer, changes nothing and queues nothing; one made again for a different request is
// refused. An entry is therefore queued once, and the worker does it at most once to effect:
//
//   - a card is made with the entry's own key (boards.NewCard.Origin), so doing the entry again
//     after a crash between making the card and noting it finds the card and makes no second one;
//   - a document is made only where none is, and one that is already there with the very content
//     the entry would write is the entry's own;
//   - moving a card to a column it is in already is nothing to do.
//
// An entry runs as the user who acted, with that user's access at that time. Both the app (the
// right the action needed) and the target are checked every time; if either was taken away the
// entry fails with a code that says so and does nothing. The definition gave it no authority.
// Failures that may pass (the database was busy, the card was being changed) are tried again with
// growing waits, up to maxAttempts; the others fail at once. A failed entry stays, listed with its
// code, until its user retries it; nothing else makes it run again.

// The states of an entry, and "simulated", which is of an effect a preview reports and never
// stores.
const (
	StatePending   = "pending"
	StateRunning   = "running"
	StateDone      = "done"
	StateFailed    = "failed"
	StateSimulated = "simulated"
)

// Bounds and timings of the outbox.
const (
	// maxAttempts bounds the tries of an entry before it fails for good.
	maxAttempts = 5
	// maxOutstanding bounds an app's entries that wait or run.
	maxOutstanding = 500
	// leaseTime is how long a worker owns the entry it took: after it, a worker that died has
	// lost it.
	leaseTime = 2 * time.Minute
	// entryTimeout bounds one try.
	entryTimeout = time.Minute
	outboxPoll   = 2 * time.Second
	// How long finished entries (and the operations of requests) are kept.
	keepDone = 30 * 24 * time.Hour
	keepAll  = 90 * 24 * time.Hour
	// backoffBase is the wait after the first failed try, doubled for each next.
	backoffBase = 5 * time.Second
	backoffMax  = 5 * time.Minute
	// listLimit is the most entries a listing returns.
	maxListed     = 200
	defaultListed = 100
)

// Platform is the rest of TaskTo.AI that effects act on. Without one an app's effects can't be done
// (they stay queued); the server builds the apps service with one.
type Platform struct {
	Boards *boards.Service
	Docs   *knowledge.Store
}

// WithPlatform gives the service what its effects act on, and returns it.
func (s *Service) WithPlatform(p Platform) *Service {
	s.platform = p
	return s
}

var (
	// errOperationReused is for an operation (idempotency key) used before for another request.
	errOperationReused = errors.New("that operation was used for a different request")
	// errEffectNotFailed is for retrying an effect that didn't fail.
	errEffectNotFailed = errors.New("only a failed effect can be retried")
)

var operationPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// operationKey is the operation an action is made under: the client's, checked, or, for an action
// with effects whose client named none, one the server makes. "" is an action with no operation to
// remember.
func operationKey(action ClientAction, hasEffects bool) (string, error) {
	if action.Operation != "" {
		if !operationPattern.MatchString(action.Operation) {
			return "", invalidAs(apierr.CodeAppOperationInvalid, nil, "operation is 8 to 64 letters, digits, - or _")
		}
		return action.Operation, nil
	}
	if !hasEffects {
		return "", nil
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "srv-" + hex.EncodeToString(b[:]), nil
}

// requestHash says what a request asked for: the action and its context.
func requestHash(action ClientAction) string {
	ctx, _ := json.Marshal(action.Context)
	h := sha256.New()
	h.Write([]byte(action.Name))
	h.Write([]byte{0})
	h.Write(ctx)
	if action.ExpectedRevision != nil {
		fmt.Fprintf(h, "\x00%d", *action.ExpectedRevision)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// beginOperation records the operation of a request, in the request's transaction. replay is true
// if the same request was made under it before (its change and effects are in already), and a
// different one is errOperationReused.
func beginOperation(ctx context.Context, tx *sql.Tx, actor Actor, appID int64, key string, action ClientAction) (id int64, replay bool, err error) {
	hash := requestHash(action)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO app_operations (app_id, user_id, operation, action, request_hash, created_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (app_id, user_id, operation) DO NOTHING`,
		appID, actor.UserID, key, action.Name, hash, now())
	if err != nil {
		return 0, false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		if err != nil {
			return 0, false, err
		}
		id, err = res.LastInsertId()
		return id, false, err
	}
	var prev string
	if err := tx.QueryRowContext(ctx, `SELECT id, request_hash FROM app_operations WHERE app_id = ? AND user_id = ? AND operation = ?`,
		appID, actor.UserID, key).Scan(&id, &prev); err != nil {
		return 0, false, err
	}
	if prev != hash {
		return 0, false, errOperationReused
	}
	return id, true, nil
}

// applied is what a change did to a record, for the effects it sets going.
type applied struct {
	id       int64
	revision int64
	data     map[string]any
	// changed is false for an update that left the record as it was.
	changed bool
}

// backlogError is an app with too many effects waiting.
func backlogError() error {
	return &fullError{
		msg:  fmt.Sprintf("an app has at most %d effects waiting", maxOutstanding),
		code: apierr.CodeAppEffectsBacklog, params: apierr.Params{"max": maxOutstanding},
	}
}

// enqueueEffects adds, in the transaction of the change, an entry for each effect of the action
// that applies to the record as the change left it.
func enqueueEffects(ctx context.Context, tx *sql.Tx, actor Actor, appID, opID int64, def Definition, action Action, ap applied) error {
	if len(action.Effects) == 0 || !ap.changed {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE app_operations SET record_id = ? WHERE id = ?`, ap.id, opID); err != nil {
		return err
	}
	var queued []int
	for i, e := range action.Effects {
		if e.applies(def.Collection, ap.data) {
			queued = append(queued, i)
		}
	}
	if len(queued) == 0 {
		return nil
	}
	var waiting int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM app_effects WHERE app_id = ? AND state IN ('pending', 'running')`, appID).Scan(&waiting); err != nil {
		return err
	}
	if waiting+len(queued) > maxOutstanding {
		return backlogError()
	}
	at := now()
	for _, i := range queued {
		e := action.Effects[i]
		raw, err := json.Marshal(e.payload(def.Collection, ap.data, ap.id))
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO app_effects (app_id, operation_id, position, name, kind, record_id, record_revision,
				user_id, acted_by_agent, acted_via, action_kind, payload, state, next_attempt_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?)`,
			appID, opID, i, e.Name, e.Kind, ap.id, ap.revision, actor.UserID, actor.Agent, via(actor), action.Kind, string(raw), at, at, at); err != nil {
			return err
		}
	}
	return nil
}

// effectRow is an entry as the worker and the listings read it.
type effectRow struct {
	id, appID, recordID, recordRevision, userID int64
	position, attempts                          int
	operation, action, name, kind, actionKind   string
	agent                                       bool
	via                                         sql.NullString
	payload                                     payload
	state                                       string
	nextAttemptAt                               string
	errCode, errParams, result                  sql.NullString
	createdAt, updatedAt                        string
	doneAt                                      sql.NullString
}

const effectFrom = `
	FROM app_effects e
	JOIN app_operations o ON o.id = e.operation_id
	JOIN app_rows r ON r.id = e.record_id`

const effectColumns = `e.id, e.app_id, e.record_id, e.record_revision, e.user_id, e.position, e.attempts,
	o.operation, o.action, e.name, e.kind, e.action_kind, e.acted_by_agent, e.acted_via, e.payload, e.state,
	e.next_attempt_at, e.error_code, e.error_params, e.result, e.created_at, e.updated_at, e.done_at`

func scanEffect(row scanner) (effectRow, error) {
	var e effectRow
	var raw string
	if err := row.Scan(&e.id, &e.appID, &e.recordID, &e.recordRevision, &e.userID, &e.position, &e.attempts,
		&e.operation, &e.action, &e.name, &e.kind, &e.actionKind, &e.agent, &e.via, &raw, &e.state,
		&e.nextAttemptAt, &e.errCode, &e.errParams, &e.result, &e.createdAt, &e.updatedAt, &e.doneAt); err != nil {
		return effectRow{}, err
	}
	if err := json.Unmarshal([]byte(raw), &e.payload); err != nil {
		return effectRow{}, fmt.Errorf("effect %d: payload: %w", e.id, err)
	}
	return e, nil
}

func (e effectRow) actor() Actor { return Actor{UserID: e.userID, Agent: e.agent, Via: e.via.String} }

// effectError is why an entry could not be done, as a code to tell people. transient is for a
// failure that may pass, which is tried again.
type effectError struct {
	code      apierr.Code
	params    apierr.Params
	transient bool
}

func (e *effectError) Error() string { return string(e.code) }

func refuse(code apierr.Code, params ...apierr.Params) error {
	e := &effectError{code: code}
	if len(params) > 0 {
		e.params = params[0]
	}
	return e
}

func (s *Service) stamp(t time.Time) string { return t.UTC().Format(timeFormat) }

// errCrash is what a test's hook returns to make the worker stop as if the server had died where
// the hook is called.
var errCrash = errors.New("simulated crash")

// StartOutbox starts the worker that does the entries, and returns the function that stops it
// and waits for it. It runs in the server process: the data directory has one, since it has one
// server (package lockfile).
func (s *Service) StartOutbox(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		poll := time.NewTicker(outboxPoll)
		defer poll.Stop()
		tidy := time.NewTicker(time.Hour)
		defer tidy.Stop()
		for {
			s.drain(ctx)
			select {
			case <-ctx.Done():
				return
			case <-poll.C:
			case <-s.wake:
			case <-tidy.C:
				if err := s.purge(ctx); err != nil && ctx.Err() == nil {
					slog.Warn("apps: purging old effects failed", "err", err)
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// poke wakes the worker, if it runs in this process.
func (s *Service) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// drain does the entries that are due, one after another.
func (s *Service) drain(ctx context.Context) {
	for ctx.Err() == nil {
		ok, err := s.runOne(ctx)
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, errCrash) {
				slog.Error("apps: outbox", "err", err)
			}
			return
		}
		if !ok {
			return
		}
	}
}

// purge removes the entries and operations nobody needs any more: finished ones after keepDone,
// whatever their state after keepAll.
func (s *Service) purge(ctx context.Context) error {
	t := s.clock()
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM app_operations
		WHERE created_at < ? OR (created_at < ? AND NOT EXISTS (
			SELECT 1 FROM app_effects e WHERE e.operation_id = app_operations.id AND e.state <> 'done'))`,
		s.stamp(t.Add(-keepAll)), s.stamp(t.Add(-keepDone)))
	return err
}

// claim takes the next entry that is due: one that waits, or one whose worker's lease has run out.
func (s *Service) claim(ctx context.Context) (effectRow, bool, error) {
	var e effectRow
	var found bool
	err := s.write(ctx, func(tx *sql.Tx) error {
		t := s.clock()
		at := s.stamp(t)
		var id int64
		err := tx.QueryRowContext(ctx, `
			SELECT id FROM app_effects
			WHERE (state = 'pending' AND next_attempt_at <= ?) OR (state = 'running' AND lease_until <= ?)
			ORDER BY id LIMIT 1`, at, at).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE app_effects SET state = 'running', attempts = attempts + 1, lease_until = ?, updated_at = ? WHERE id = ?`,
			s.stamp(t.Add(leaseTime)), at, id); err != nil {
			return err
		}
		e, err = scanEffect(tx.QueryRowContext(ctx, `SELECT `+effectColumns+effectFrom+` WHERE e.id = ?`, id))
		found = err == nil
		return err
	})
	return e, found, err
}

// runOne does the next entry that is due, if there is one.
func (s *Service) runOne(ctx context.Context) (bool, error) {
	e, found, err := s.claim(ctx)
	if err != nil || !found {
		return false, err
	}
	try, cancel := context.WithTimeout(ctx, entryTimeout)
	defer cancel()
	result, err := s.perform(try, e)
	if err == nil && s.hooks.afterWork != nil {
		err = s.hooks.afterWork(e)
	}
	if errors.Is(err, errCrash) {
		return false, err // the server "died": the entry is left running, as it would be
	}
	if ctx.Err() != nil {
		return false, ctx.Err() // stopping: the lease runs out and the entry is taken again
	}
	if err != nil {
		return true, s.fail(ctx, e, err)
	}
	return true, s.succeed(ctx, e, result)
}

// succeed notes an entry as done, if its worker still owns it.
func (s *Service) succeed(ctx context.Context, e effectRow, result map[string]any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	at := s.stamp(s.clock())
	_, err = s.db.ExecContext(ctx, `
		UPDATE app_effects SET state = 'done', result = ?, error_code = NULL, error_params = NULL, lease_until = NULL, updated_at = ?, done_at = ?
		WHERE id = ? AND state = 'running' AND attempts = ?`, string(raw), at, at, e.id, e.attempts)
	return err
}

// fail notes why a try failed: the entry waits for the next one, or has failed for good.
func (s *Service) fail(ctx context.Context, e effectRow, cause error) error {
	var ee *effectError
	if !errors.As(cause, &ee) {
		// Not one of the reasons the entry was written for: the database, the disk, a timeout.
		slog.Warn("apps: effect failed", "effect", e.id, "kind", e.kind, "attempt", e.attempts, "err", cause)
		ee = &effectError{code: apierr.CodeAppEffectGaveUp, transient: true}
	}
	t := s.clock()
	var params any
	if ee.params != nil {
		raw, _ := json.Marshal(ee.params)
		params = string(raw)
	}
	if ee.transient && e.attempts < maxAttempts {
		wait := min(backoffBase<<(e.attempts-1), backoffMax)
		_, err := s.db.ExecContext(ctx, `
			UPDATE app_effects SET state = 'pending', next_attempt_at = ?, lease_until = NULL, error_code = ?, error_params = ?, updated_at = ?
			WHERE id = ? AND state = 'running' AND attempts = ?`, s.stamp(t.Add(wait)), string(ee.code), params, s.stamp(t), e.id, e.attempts)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE app_effects SET state = 'failed', lease_until = NULL, error_code = ?, error_params = ?, updated_at = ?
		WHERE id = ? AND state = 'running' AND attempts = ?`, string(ee.code), params, s.stamp(t), e.id, e.attempts)
	return err
}

// perform does one entry: it checks, as the user who acted and as of now, that they may still do
// what the action did and what the effect does, and then does it.
func (s *Service) perform(ctx context.Context, e effectRow) (map[string]any, error) {
	if err := s.authorizeApp(ctx, e); err != nil {
		return nil, err
	}
	switch e.kind {
	case EffectCreateCard:
		return s.createCard(ctx, e)
	case EffectUpdateCard:
		return s.updateCard(ctx, e)
	case EffectCreateDocument:
		return s.createDocument(ctx, e)
	case EffectOpenRoute:
		return s.openRoute(ctx, e)
	}
	return nil, refuse(apierr.CodeAppEffectInvalid)
}

// authorizeApp checks that the user who acted still may do, in the app, what the action did: they
// may write its space and hold the right the kind of action needs. An offline app does nothing.
func (s *Service) authorizeApp(ctx context.Context, e effectRow) error {
	var spaceID int64
	var archived sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT space_id, archived_at FROM apps WHERE id = ?`, e.appID).Scan(&spaceID, &archived)
	if errors.Is(err, sql.ErrNoRows) {
		return refuse(apierr.CodeAppEffectAppAccess)
	}
	if err != nil {
		return err
	}
	sp, err := s.access.Check(ctx, e.userID, spaceID)
	if errors.Is(err, access.ErrNotFound) {
		return refuse(apierr.CodeAppEffectAppAccess)
	}
	if err != nil {
		return err
	}
	if !sp.Access.Allows(access.Write) {
		if sp.ArchivedAt != nil {
			return refuse(apierr.CodeAppEffectAppOffline)
		}
		return refuse(apierr.CodeAppEffectAppAccess)
	}
	acc, err := s.access.App(ctx, e.userID, spaceID, e.appID)
	if errors.Is(err, access.ErrNotFound) || errors.Is(err, access.ErrAppNotFound) {
		return refuse(apierr.CodeAppEffectAppAccess)
	}
	if err != nil {
		return err
	}
	allowed := acc.View && ((e.actionKind == ActionInsert && acc.Submit) || (e.actionKind == ActionUpdate && acc.ModifyRecords != access.ScopeNone))
	if !allowed {
		return refuse(apierr.CodeAppEffectAppAccess)
	}
	if archived.Valid {
		return refuse(apierr.CodeAppEffectAppOffline)
	}
	return nil
}

// target checks the user's access to the space an effect acts in: write for one that changes
// something, read to open it.
func (s *Service) target(ctx context.Context, userID, spaceID int64, write bool) (access.Space, error) {
	sp, err := s.access.Check(ctx, userID, spaceID)
	if errors.Is(err, access.ErrNotFound) {
		return access.Space{}, refuse(apierr.CodeAppEffectTargetAccess)
	}
	if err != nil {
		return access.Space{}, err
	}
	switch {
	case !sp.Access.Allows(access.Read):
		return access.Space{}, refuse(apierr.CodeAppEffectTargetAccess)
	case write && !sp.Access.Allows(access.Write) && sp.ArchivedAt != nil:
		return access.Space{}, refuse(apierr.CodeAppEffectBoardOffline)
	case write && !sp.Access.Allows(access.Write):
		return access.Space{}, refuse(apierr.CodeAppEffectTargetAccess)
	}
	return sp, nil
}

// boardsError turns a refusal of package boards into the entry's.
func boardsError(err error) error {
	switch {
	case errors.Is(err, boards.ErrNotFound):
		return refuse(apierr.CodeAppEffectTargetGone)
	case errors.Is(err, boards.ErrBoardArchived):
		return refuse(apierr.CodeAppEffectBoardOffline)
	case errors.Is(err, boards.ErrReadOnly):
		return refuse(apierr.CodeAppEffectTargetAccess)
	case errors.Is(err, boards.ErrInvalid):
		return refuse(apierr.CodeAppEffectInvalid)
	}
	return err
}

func (s *Service) boardSvc() (*boards.Service, error) {
	if s.platform.Boards == nil {
		return nil, errors.New("effects on boards are not available in this server")
	}
	return s.platform.Boards, nil
}

// cardOrigin is the key a card made by entry id carries.
func cardOrigin(id int64) string { return fmt.Sprintf("app-effect:%d", id) }

func (s *Service) createCard(ctx context.Context, e effectRow) (map[string]any, error) {
	b, err := s.boardSvc()
	if err != nil {
		return nil, err
	}
	p := e.payload
	if _, err := s.target(ctx, e.userID, p.SpaceID, true); err != nil {
		return nil, err
	}
	card, err := b.CreateCard(ctx, boards.Actor(e.actor()), p.SpaceID, p.BoardID, boards.NewCard{
		Title: p.Title, Body: p.Body, Column: boards.Column(p.Column), Docs: p.Docs, Origin: cardOrigin(e.id),
	})
	if err != nil {
		return nil, boardsError(err)
	}
	return map[string]any{"space_id": p.SpaceID, "board_id": p.BoardID, "card_id": card.ID, "title": card.Title}, nil
}

// cardRef is a card an entry made.
type cardRef struct{ spaceID, boardID, cardID int64 }

// linkedCard finds the card the create_card effect called name made for the entry's record. A card
// that is still being made is waited for (a transient failure); one that wasn't made, or won't be,
// is the entry's failure.
func (s *Service) linkedCard(ctx context.Context, e effectRow, name string) (cardRef, error) {
	var state string
	var result sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT state, result FROM app_effects
		WHERE app_id = ? AND record_id = ? AND kind = 'create_card' AND name = ? AND id <> ?
		ORDER BY (state = 'done') DESC, id DESC LIMIT 1`, e.appID, e.recordID, name, e.id).Scan(&state, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return cardRef{}, refuse(apierr.CodeAppEffectNoCard)
	}
	if err != nil {
		return cardRef{}, err
	}
	switch state {
	case StateDone:
	case StatePending, StateRunning:
		return cardRef{}, &effectError{code: apierr.CodeAppEffectNoCard, transient: true}
	default:
		return cardRef{}, refuse(apierr.CodeAppEffectNoCard)
	}
	var made struct {
		SpaceID int64 `json:"space_id"`
		BoardID int64 `json:"board_id"`
		CardID  int64 `json:"card_id"`
	}
	if err := json.Unmarshal([]byte(result.String), &made); err != nil || made.CardID == 0 {
		return cardRef{}, refuse(apierr.CodeAppEffectNoCard)
	}
	return cardRef{made.SpaceID, made.BoardID, made.CardID}, nil
}

func (s *Service) updateCard(ctx context.Context, e effectRow) (map[string]any, error) {
	b, err := s.boardSvc()
	if err != nil {
		return nil, err
	}
	ref, err := s.linkedCard(ctx, e, e.payload.Card)
	if err != nil {
		return nil, err
	}
	if _, err := s.target(ctx, e.userID, ref.spaceID, true); err != nil {
		return nil, err
	}
	column := boards.Column(e.payload.Column)
	result := map[string]any{"space_id": ref.spaceID, "board_id": ref.boardID, "card_id": ref.cardID, "column": e.payload.Column}
	// A card changed by someone meanwhile is read again: the move is made against what is there.
	for range 3 {
		th, err := b.ShowCard(ctx, boards.Actor(e.actor()), ref.spaceID, ref.boardID, ref.cardID)
		if err != nil {
			return nil, boardsError(err)
		}
		if th.Column == boards.Archived {
			return nil, refuse(apierr.CodeAppEffectCardArchived)
		}
		if th.Column == column {
			return result, nil // already there: nothing to do
		}
		_, err = b.MoveCard(ctx, boards.Actor(e.actor()), ref.spaceID, ref.boardID, ref.cardID, boards.Move{Version: th.Version, Column: column})
		if errors.Is(err, boards.ErrStale) {
			continue
		}
		if err != nil {
			return nil, boardsError(err)
		}
		return result, nil
	}
	return nil, &effectError{code: apierr.CodeAppEffectGaveUp, transient: true}
}

// documentMessage is the commit message of a document an effect makes.
const documentMessage = "Created by an app"

func (s *Service) createDocument(ctx context.Context, e effectRow) (map[string]any, error) {
	if s.platform.Docs == nil {
		return nil, errors.New("effects on documents are not available in this server")
	}
	p := e.payload
	sp, err := s.target(ctx, e.userID, p.SpaceID, true)
	if err != nil {
		return nil, err
	}
	by := knowledge.Author{Agent: "TaskTo app"}
	if err := s.db.QueryRowContext(ctx, `SELECT name, email FROM users WHERE id = ?`, e.userID).Scan(&by.Name, &by.Email); err != nil {
		return nil, err
	}
	res, err := s.platform.Docs.Put(ctx, sp, p.Path, []byte(p.Content), "", by, documentMessage)
	switch {
	case err == nil && res.Doc != nil:
		return map[string]any{"space_id": p.SpaceID, "path": p.Path, "version": res.Doc.Version}, nil
	case err == nil:
		return map[string]any{"space_id": p.SpaceID, "path": p.Path}, nil
	case errors.Is(err, knowledge.ErrConflict):
		// A document is there. Made by an earlier try that never noted it, if it is exactly what
		// this entry writes; someone else's if not, and never overwritten.
		doc, rerr := s.platform.Docs.Read(ctx, sp, p.Path, "")
		if rerr == nil && bytes.Equal(doc.Content, []byte(p.Content)) {
			return map[string]any{"space_id": p.SpaceID, "path": p.Path, "version": doc.Version}, nil
		}
		return nil, refuse(apierr.CodeAppEffectDocExists, apierr.Params{"path": p.Path})
	case errors.Is(err, knowledge.ErrInvalid), errors.Is(err, knowledge.ErrTooLarge):
		return nil, refuse(apierr.CodeAppEffectInvalid)
	case errors.Is(err, knowledge.ErrReadOnly):
		return nil, refuse(apierr.CodeAppEffectTargetAccess)
	case errors.Is(err, knowledge.ErrRepoFull):
		return nil, refuse(apierr.CodeAppEffectInvalid)
	}
	return nil, err
}

// openRoute resolves a route: the user must be able to read where it goes, and what it says is
// kind and IDs, which the web app turns into its own address. It makes nothing.
func (s *Service) openRoute(ctx context.Context, e effectRow) (map[string]any, error) {
	p := e.payload
	route := map[string]any{"kind": p.Route}
	switch p.Route {
	case RouteBoard:
		b, err := s.boardSvc()
		if err != nil {
			return nil, err
		}
		if _, err := s.target(ctx, e.userID, p.SpaceID, false); err != nil {
			return nil, err
		}
		all, err := b.ListBoards(ctx, boards.Actor(e.actor()), p.SpaceID)
		if err != nil {
			return nil, boardsError(err)
		}
		found := false
		for _, bd := range all {
			found = found || bd.ID == p.BoardID
		}
		if !found {
			return nil, refuse(apierr.CodeAppEffectTargetGone)
		}
		route["space_id"], route["board_id"] = p.SpaceID, p.BoardID
	case RouteCard:
		ref, err := s.linkedCard(ctx, e, p.Card)
		if err != nil {
			return nil, err
		}
		if _, err := s.target(ctx, e.userID, ref.spaceID, false); err != nil {
			return nil, err
		}
		route["space_id"], route["board_id"], route["card_id"] = ref.spaceID, ref.boardID, ref.cardID
	case RouteDocument:
		if _, err := s.target(ctx, e.userID, p.SpaceID, false); err != nil {
			return nil, err
		}
		route["space_id"], route["path"] = p.SpaceID, p.Path
	default:
		return nil, refuse(apierr.CodeAppEffectInvalid)
	}
	return map[string]any{"route": route}, nil
}
