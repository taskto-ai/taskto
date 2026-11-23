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

// requestHash says what a request asked for: the release it runs from (the one that decides what
// the action does, the effects included), the action and its context.
func requestHash(release int64, action ClientAction) string {
	ctx, _ := json.Marshal(action.Context)
	h := sha256.New()
	fmt.Fprintf(h, "release:%d\x00", release)
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
func beginOperation(ctx context.Context, tx *sql.Tx, actor Actor, appID int64, key string, release int64, action ClientAction) (id int64, replay bool, err error) {
	hash := requestHash(release, action)
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
		p := e.payload(def.Collection, ap.data, ap.id)
		if e.Card != "" {
			// An update_card or a route to a card has no target of its own: it is the one of the
			// effect that makes the card, kept here so who may be told about it is known.
			if maker, ok := def.cardMaker(e.Card); ok {
				p.SpaceID, p.BoardID = maker.SpaceID, maker.BoardID
			}
		}
		raw, err := json.Marshal(p)
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
	// lease is the token of the claim that holds the entry: it only grows (see claim).
	lease                                     int64
	operation, action, name, kind, actionKind string
	agent                                     bool
	via                                       sql.NullString
	payload                                   payload
	state                                     string
	nextAttemptAt                             string
	errCode, errParams, result                sql.NullString
	createdAt, updatedAt                      string
	doneAt                                    sql.NullString
}

const effectFrom = `
	FROM app_effects e
	JOIN app_operations o ON o.id = e.operation_id
	JOIN app_rows r ON r.id = e.record_id`

const effectColumns = `e.id, e.app_id, e.record_id, e.record_revision, e.user_id, e.position, e.attempts, e.lease,
	o.operation, o.action, e.name, e.kind, e.action_kind, e.acted_by_agent, e.acted_via, e.payload, e.state,
	e.next_attempt_at, e.error_code, e.error_params, e.result, e.created_at, e.updated_at, e.done_at`

func scanEffect(row scanner) (effectRow, error) {
	var e effectRow
	var raw string
	if err := row.Scan(&e.id, &e.appID, &e.recordID, &e.recordRevision, &e.userID, &e.position, &e.attempts, &e.lease,
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

// purge removes the entries and operations nobody needs any more. An entry is judged by its own
// last activity (updated_at: queued, tried, retried), never by the age of the operation that queued
// it, so an entry retried today is not erased before it runs because its request is old: finished
// ones go after keepDone, whatever their state after keepAll. An operation goes once it has no
// entries left and keepDone has passed since its request. The link of a record to the card made
// for it (app_effect_cards) is never purged: it goes with its record or app.
func (s *Service) purge(ctx context.Context) error {
	t := s.clock()
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM app_effects
		WHERE updated_at < ? OR (state = 'done' AND updated_at < ?)`,
		s.stamp(t.Add(-keepAll)), s.stamp(t.Add(-keepDone))); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM app_operations
		WHERE created_at < ? AND NOT EXISTS (SELECT 1 FROM app_effects e WHERE e.operation_id = app_operations.id)`,
		s.stamp(t.Add(-keepDone)))
	return err
}

// claim takes the next entry that is due: one that waits, or one whose worker's lease has run out.
// An entry whose lease ran out after its last permitted try isn't tried again: it has failed.
func (s *Service) claim(ctx context.Context) (effectRow, bool, error) {
	var e effectRow
	var found bool
	err := s.write(ctx, func(tx *sql.Tx) error {
		t := s.clock()
		at := s.stamp(t)
		for {
			var id int64
			var state string
			var attempts int
			err := tx.QueryRowContext(ctx, `
				SELECT id, state, attempts FROM app_effects
				WHERE (state = 'pending' AND next_attempt_at <= ?) OR (state = 'running' AND lease_until <= ?)
				ORDER BY id LIMIT 1`, at, at).Scan(&id, &state, &attempts)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if attempts >= maxAttempts {
				// Its worker died (or kept being stopped) on the last try: the bound holds.
				if _, err := tx.ExecContext(ctx, `
					UPDATE app_effects SET state = 'failed', lease_until = NULL, error_code = ?, error_params = NULL, updated_at = ?
					WHERE id = ?`, string(apierr.CodeAppEffectGaveUp), at, id); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE app_effects SET state = 'running', attempts = attempts + 1, lease = lease + 1, lease_until = ?, updated_at = ? WHERE id = ?`,
				s.stamp(t.Add(leaseTime)), at, id); err != nil {
				return err
			}
			e, err = scanEffect(tx.QueryRowContext(ctx, `SELECT `+effectColumns+effectFrom+` WHERE e.id = ?`, id))
			found = err == nil
			return err
		}
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
		WHERE id = ? AND state = 'running' AND lease = ?`, string(raw), at, at, e.id, e.lease)
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
			WHERE id = ? AND state = 'running' AND lease = ?`, s.stamp(t.Add(wait)), string(ee.code), params, s.stamp(t), e.id, e.lease)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE app_effects SET state = 'failed', lease_until = NULL, error_code = ?, error_params = ?, updated_at = ?
		WHERE id = ? AND state = 'running' AND lease = ?`, string(ee.code), params, s.stamp(t), e.id, e.lease)
	return err
}

// perform does one entry: it checks, as the user who acted and as of now, that they may still do
// what the action did and what the effect does, and then does it. The checks are repeated as close
// to the write as the data allows: inside the transaction of a card's change (where the write lock
// holds, so nothing they rely on changes before the card does), and, for a document, which git
// holds, right before it is written.
func (s *Service) perform(ctx context.Context, e effectRow) (map[string]any, error) {
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

// authorizeApp checks that the user who acted still may do, in the app, what the action did, as of
// now: the app is there and online, they may write its space and open the app, they hold the right
// the kind of action needed, over the record the entry came from (the user's own or all, as the
// role now says: an update needs the right to change that record, and to read it), and the record
// is still there. An offline app does nothing.
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
	if !acc.View {
		return refuse(apierr.CodeAppEffectAppAccess)
	}
	if archived.Valid {
		return refuse(apierr.CodeAppEffectAppOffline)
	}
	var creator sql.NullInt64
	var deleted sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT created_by, deleted_at FROM app_rows WHERE id = ? AND app_id = ?`, e.recordID, e.appID).Scan(&creator, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return refuse(apierr.CodeAppEffectRecordGone)
	}
	if err != nil {
		return err
	}
	if deleted.Valid {
		return refuse(apierr.CodeAppEffectRecordGone)
	}
	switch e.actionKind {
	case ActionInsert:
		if !acc.Submit {
			return refuse(apierr.CodeAppEffectAppAccess)
		}
	case ActionUpdate:
		if !(visibility{scope: acc.ModifyRecords, user: e.userID}).sees(creator) || !readable(acc, e.userID).sees(creator) {
			return refuse(apierr.CodeAppEffectAppAccess)
		}
	default:
		return refuse(apierr.CodeAppEffectAppAccess)
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

// cardOrigin is the key a card made by entry id carries. An entry's number is never given again
// (the table's rows are numbered with AUTOINCREMENT), so no later entry can find an earlier one's
// card as its own.
func cardOrigin(id int64) string { return fmt.Sprintf("app-effect:%d", id) }

func (s *Service) createCard(ctx context.Context, e effectRow) (map[string]any, error) {
	b, err := s.boardSvc()
	if err != nil {
		return nil, err
	}
	p := e.payload
	sp, err := s.target(ctx, e.userID, p.SpaceID, true)
	if err != nil {
		return nil, err
	}
	card, err := b.CreateCard(ctx, boards.Actor(e.actor()), p.SpaceID, p.BoardID, boards.NewCard{
		Title: p.Title, Body: p.Body, Column: boards.Column(p.Column), Docs: p.Docs, Origin: cardOrigin(e.id),
		// In the transaction that makes the card, which holds the write lock: nothing that
		// authorized the entry changes before the card is there, and the record's link to the card
		// is made with it, so there is never a card the record doesn't know of.
		Check: func(*sql.Tx) error {
			if err := s.authorizeApp(ctx, e); err != nil {
				return err
			}
			_, err := s.target(ctx, e.userID, p.SpaceID, true)
			return err
		},
		Created: func(tx *sql.Tx, cardID int64) error {
			if e.name == "" {
				return nil
			}
			// A newer card for the name replaces the link and starts its moves afresh; an older
			// entry done again late never takes it back.
			_, err := tx.ExecContext(ctx, `
				INSERT INTO app_effect_cards (app_id, record_id, name, space_id, board_id, card_id, made_by, last_move)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (app_id, record_id, name) DO UPDATE SET
					space_id = excluded.space_id, board_id = excluded.board_id, card_id = excluded.card_id,
					made_by = excluded.made_by, last_move = excluded.made_by
				WHERE excluded.made_by > app_effect_cards.made_by`,
				e.appID, e.recordID, e.name, p.SpaceID, p.BoardID, cardID, e.id, e.id)
			return err
		},
	})
	if err != nil {
		return nil, boardsError(err)
	}
	return map[string]any{"workspace_id": sp.WorkspaceID, "space_id": p.SpaceID, "board_id": p.BoardID, "card_id": card.ID, "title": card.Title}, nil
}

// cardRef is a card an entry made, as its record's link names it.
type cardRef struct {
	spaceID, boardID, cardID int64
	lastMove                 int64
}

// linkedCard finds the card the create_card effect called name made for the entry's record, in the
// record's durable link (which the cleanup of entries leaves alone). A card still to be made by an
// entry queued before this one is waited for (a transient failure); one that was not made, or
// won't be, is the entry's failure.
func (s *Service) linkedCard(ctx context.Context, e effectRow, name string) (cardRef, error) {
	var waiting bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM app_effects
			WHERE app_id = ? AND record_id = ? AND kind = 'create_card' AND name = ? AND id < ? AND state IN ('pending', 'running'))`,
		e.appID, e.recordID, name, e.id).Scan(&waiting); err != nil {
		return cardRef{}, err
	}
	if waiting {
		return cardRef{}, &effectError{code: apierr.CodeAppEffectNoCard, transient: true}
	}
	var ref cardRef
	err := s.db.QueryRowContext(ctx, `
		SELECT space_id, board_id, card_id, last_move FROM app_effect_cards WHERE app_id = ? AND record_id = ? AND name = ?`,
		e.appID, e.recordID, name).Scan(&ref.spaceID, &ref.boardID, &ref.cardID, &ref.lastMove)
	if errors.Is(err, sql.ErrNoRows) {
		return cardRef{}, refuse(apierr.CodeAppEffectNoCard)
	}
	return ref, err
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
	sp, err := s.target(ctx, e.userID, ref.spaceID, true)
	if err != nil {
		return nil, err
	}
	column := boards.Column(e.payload.Column)
	result := map[string]any{"workspace_id": sp.WorkspaceID, "space_id": ref.spaceID, "board_id": ref.boardID, "card_id": ref.cardID, "column": e.payload.Column}
	// The move is made in one transaction with the note of it on the record's link: a move is
	// made only if no later entry's move was made before, so one that is done again late (its
	// worker died before it noted it done) never undoes what a newer one did. An entry that has
	// nothing to do (superseded, or the card is already there) is a success all the same.
	_, _, err = b.MoveCardIf(ctx, boards.Actor(e.actor()), ref.spaceID, ref.boardID, ref.cardID, column, func(tx *sql.Tx, c boards.Card) (bool, error) {
		if err := s.authorizeApp(ctx, e); err != nil {
			return false, err
		}
		if _, err := s.target(ctx, e.userID, ref.spaceID, true); err != nil {
			return false, err
		}
		if c.Column == boards.Archived {
			return false, refuse(apierr.CodeAppEffectCardArchived)
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE app_effect_cards SET last_move = ?
			WHERE app_id = ? AND record_id = ? AND name = ? AND card_id = ? AND last_move < ?`,
			e.id, e.appID, e.recordID, e.payload.Card, ref.cardID, e.id)
		if err != nil {
			return false, err
		}
		n, err := res.RowsAffected()
		return n == 1, err
	})
	if err != nil {
		return nil, boardsError(err)
	}
	return result, nil
}

// documentMessage is the commit message of a document an effect makes; documentAgent is who the
// history says made it for the user.
const (
	documentMessage = "Created by an app"
	documentAgent   = "TaskTo app"
)

// documentCommitMessage names the entry in the commit it makes, so that a try that finds the entry
// again after a crash between the commit and the note that it was done can tell that the document
// was made, whatever became of the document since (edited, moved, deleted). An entry's number is
// never given again.
func documentCommitMessage(e effectRow) string {
	return fmt.Sprintf("%s [effect %d/%d]", documentMessage, e.appID, e.id)
}

func (s *Service) createDocument(ctx context.Context, e effectRow) (map[string]any, error) {
	if s.platform.Docs == nil {
		return nil, errors.New("effects on documents are not available in this server")
	}
	p := e.payload
	sp, err := s.target(ctx, e.userID, p.SpaceID, true)
	if err != nil {
		return nil, err
	}
	made := map[string]any{"workspace_id": sp.WorkspaceID, "space_id": p.SpaceID, "path": p.Path}
	message := documentCommitMessage(e)
	// An entry that was claimed before may have made its document and been stopped before it
	// noted so. Its commit is looked for by the message it was given: if it is there, the entry is
	// done, and what the document is now is none of its business.
	if e.lease > 1 {
		if _, found, err := s.platform.Docs.FindCommit(ctx, sp, message, documentAgent); err != nil {
			return nil, err
		} else if found {
			return made, nil
		}
	}
	// The checks are repeated while the space's write lock is held, right before the commit: the
	// entry may have waited for the lock, and what it was allowed when it began may have been
	// withdrawn since.
	guard := func() error {
		if err := s.authorizeApp(ctx, e); err != nil {
			return err
		}
		_, err := s.target(ctx, e.userID, p.SpaceID, true)
		return err
	}
	if s.hooks.beforeDocument != nil {
		s.hooks.beforeDocument(e)
	}
	by := knowledge.Author{Agent: documentAgent}
	if err := s.db.QueryRowContext(ctx, `SELECT name, email FROM users WHERE id = ?`, e.userID).Scan(&by.Name, &by.Email); err != nil {
		return nil, err
	}
	res, err := s.platform.Docs.PutIf(ctx, sp, p.Path, []byte(p.Content), "", by, message, guard)
	switch {
	case err == nil && res.Doc != nil:
		made["version"] = res.Doc.Version
		return made, nil
	case err == nil:
		return made, nil
	case errors.Is(err, knowledge.ErrConflict):
		// A document is there. Made by an earlier try that never noted it, if it is exactly what
		// this entry writes; someone else's if not, and never overwritten.
		doc, rerr := s.platform.Docs.Read(ctx, sp, p.Path, "")
		if rerr == nil && bytes.Equal(doc.Content, []byte(p.Content)) {
			made["version"] = doc.Version
			return made, nil
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
	if err := s.authorizeApp(ctx, e); err != nil {
		return nil, err
	}
	p := e.payload
	route := map[string]any{"kind": p.Route}
	switch p.Route {
	case RouteBoard:
		b, err := s.boardSvc()
		if err != nil {
			return nil, err
		}
		sp, err := s.target(ctx, e.userID, p.SpaceID, false)
		if err != nil {
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
		route["workspace_id"], route["space_id"], route["board_id"] = sp.WorkspaceID, p.SpaceID, p.BoardID
	case RouteCard:
		ref, err := s.linkedCard(ctx, e, p.Card)
		if err != nil {
			return nil, err
		}
		sp, err := s.target(ctx, e.userID, ref.spaceID, false)
		if err != nil {
			return nil, err
		}
		route["workspace_id"], route["space_id"], route["board_id"], route["card_id"] = sp.WorkspaceID, ref.spaceID, ref.boardID, ref.cardID
	case RouteDocument:
		sp, err := s.target(ctx, e.userID, p.SpaceID, false)
		if err != nil {
			return nil, err
		}
		route["workspace_id"], route["space_id"], route["path"] = sp.WorkspaceID, p.SpaceID, p.Path
	default:
		return nil, refuse(apierr.CodeAppEffectInvalid)
	}
	return map[string]any{"route": route}, nil
}
