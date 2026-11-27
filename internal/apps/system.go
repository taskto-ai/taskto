package apps

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The system writer: the one path that changes a record with no user behind it, for the
// server's own work (writing back the link of a payment, applying an event of an integration).
// It is a Go API of this package and has no route, no tool and no way for a definition, a
// record or a model to name it.
//
// Its whole authority is what its caller declares: the fields an approved contract allows it to
// write. There are no roles and no user's rights in it. It is checked at the moment of the
// write, against the app as it is then: the app is in the space the caller names, is online,
// and its live release still declares each field, with a type the server may write (a photo, a
// signature or a captured location is what a person captured, never the server's to set). The
// record still has to fit the live collection, so a system write can't make a record invalid;
// a write that wouldn't is refused, with nothing changed.
//
// Its changes are records' like any other: the revision goes up by one, in the same transaction
// and under the same write lock as a person's change, so a person's stale edit gets the usual
// conflict, and the history keeps the version with the actor named in its own column
// (actor_system, never a user's id: a NULL user means an account that was deleted). A system
// write queues no effect: effects run as the user who acted, and there is none.

// ErrSystemField is for a field a system write may not set: one the caller didn't declare, one
// the live release no longer has, or one of a type only a person captures.
var ErrSystemField = errors.New("a system write may only set the fields its contract declares")

// systemFieldError is ErrSystemField, saying which field and why.
type systemFieldError struct{ field, why string }

func (e *systemFieldError) Error() string {
	return ErrSystemField.Error() + ": " + e.field + " " + e.why
}
func (e *systemFieldError) Is(target error) bool { return target == ErrSystemField }

// SystemKindIntegration is the kind of system actor the first writers are: an integration of
// a workspace.
const SystemKindIntegration = "integration"

// SystemActor is who the server acts as: a kind of actor and its id, kept on the record and in
// its history as "<kind>:<id>".
type SystemActor struct {
	Kind string
	ID   int64
}

// column is the actor as the schema stores it.
func (a SystemActor) column() string { return a.Kind + ":" + strconv.FormatInt(a.ID, 10) }

// systemActionPattern is what action names of a system write look like: a normalized event, such
// as "payment.succeeded" (or the name of the server's step), never text meant for people.
var systemActionPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

// SystemWrite is one change to one record.
type SystemWrite struct {
	Actor SystemActor
	// SpaceID and AppID say where the record is: an app of another space is not found.
	SpaceID, AppID, RecordID int64
	// Action names the change in the record's history, the normalized event that caused it.
	Action string
	// Declared is the fields the caller's approved contract allows it to write. Set may name
	// no other.
	Declared []string
	// Set holds the new values, by field; nil clears a field.
	Set map[string]any
	// Expected, if given, is the revision the caller based the write on: a record that has
	// moved on is an ErrConflict and nothing is written. Without it the write applies to the
	// record as it is, which is right for a write that sets what an event says, whatever
	// else changed.
	Expected *int64
	// Guard, if given, runs in the write's own transaction before anything is changed, and a
	// non-nil error it returns is the write's: nothing is written. It is for a write that is
	// only good while something else holds (the writer is not superseded by a newer one), checked
	// with the same lock the change is made under, so nothing can move in between.
	Guard func(ctx context.Context, tx *sql.Tx) error
}

// SystemResult is the record after a system write.
type SystemResult struct {
	Revision int64
	// Changed is whether the write changed the record. One that set what was already there is
	// no change: no new revision, nothing in the history (so a replay is harmless).
	Changed bool
	// Skipped is true when the caller's decision was to write nothing (an event whose rule did not
	// hold of the record): the transaction's other work, the Guard's, was done all the same.
	Skipped bool
}

// SystemWrite changes a record as the server would, as its w says. It returns ErrNotFound for
// an app that isn't in w.SpaceID, a record that doesn't exist or is deleted, ErrOffline for an
// app that is offline, ErrReadOnly for an archived space, ErrNotPublished for an app with no
// live release, ErrSystemField for a field that may not be set, ErrConflict for an Expected
// revision that is not the record's, and the collection's own refusal of a value or a record
// that doesn't fit.
func (s *Service) SystemWrite(ctx context.Context, w SystemWrite) (SystemResult, error) {
	if w.Actor.Kind != SystemKindIntegration || w.Actor.ID < 1 {
		return SystemResult{}, invalidf("a system write needs its actor: an integration and its id")
	}
	if !systemActionPattern.MatchString(w.Action) {
		return SystemResult{}, invalidf("a system write needs the action that caused it, such as payment.succeeded")
	}
	if len(w.Set) == 0 {
		return SystemResult{}, invalidf("a system write sets at least one field")
	}
	if w.Expected != nil && *w.Expected < 1 {
		return SystemResult{}, invalidf("expected revision is a revision, from 1")
	}
	var res SystemResult
	err := s.write(ctx, func(tx *sql.Tx) error {
		var err error
		res, err = s.systemWriteTx(ctx, tx, w, nil)
		return err
	})
	return res, err
}

// systemDecision is what decide (systemWriteTx) says of a record: the fields to set, which of them
// the caller declared, or that nothing is to be written (skip).
type systemDecision struct {
	set      map[string]any
	declared []string
	skip     bool
}

// systemWriteTx is SystemWrite inside the transaction tx, which holds the write lock. With decide,
// the fields to set are worked out from the live definition and the record as it is (an event's
// handler and its rule), after both are read, and w.Set and w.Declared are not used.
func (s *Service) systemWriteTx(ctx context.Context, tx *sql.Tx, w SystemWrite, decide func(def Definition, fields map[string]any) (systemDecision, error)) (SystemResult, error) {
	if w.Guard != nil {
		if err := w.Guard(ctx, tx); err != nil {
			return SystemResult{}, err
		}
	}
	ap, err := launchable(ctx, tx, w.SpaceID, w.AppID)
	if err != nil {
		return SystemResult{}, err
	}
	var archived sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT archived_at FROM spaces WHERE id = ?`, w.SpaceID).Scan(&archived); err != nil {
		return SystemResult{}, err
	}
	if archived.Valid {
		return SystemResult{}, errSpaceArchived
	}
	if ap.liveDef == nil {
		return SystemResult{}, ErrNotPublished
	}
	def := *ap.liveDef
	checkSet := func(set map[string]any, declared []string) error {
		for name := range set {
			if !slices.Contains(declared, name) {
				return &systemFieldError{field: name, why: "isn't declared"}
			}
			f, ok := def.Collection.field(name)
			if !ok {
				return &systemFieldError{field: name, why: "isn't in the app any more"}
			}
			switch f.Type {
			case TypeAttachment, TypeSignature, TypeLocation:
				return &systemFieldError{field: name, why: "is captured by a person"}
			}
		}
		return nil
	}
	if decide == nil {
		if err := checkSet(w.Set, w.Declared); err != nil {
			return SystemResult{}, err
		}
	}

	var data string
	var revision int64
	var creator sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT data, revision, created_by FROM app_rows WHERE id = ? AND app_id = ? AND deleted_at IS NULL`, w.RecordID, w.AppID).
		Scan(&data, &revision, &creator)
	if errors.Is(err, sql.ErrNoRows) {
		return SystemResult{}, ErrNotFound
	}
	if err != nil {
		return SystemResult{}, err
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(data), &fields); err != nil {
		return SystemResult{}, fmt.Errorf("app %d row %d: %w", w.AppID, w.RecordID, err)
	}
	set := w.Set
	if decide != nil {
		d, err := decide(def, fields)
		if err != nil {
			return SystemResult{}, err
		}
		if d.skip {
			return SystemResult{Revision: revision, Skipped: true}, nil
		}
		if len(d.set) == 0 {
			return SystemResult{}, invalidf("a system write sets at least one field")
		}
		if err := checkSet(d.set, d.declared); err != nil {
			return SystemResult{}, err
		}
		set = d.set
	}
	current, err := json.Marshal(fields)
	if err != nil {
		return SystemResult{}, err
	}
	if w.Expected != nil && *w.Expected != revision {
		return SystemResult{}, conflict(w.RecordID, liveRecord{revision: revision, fields: fields}, creator, everything, def)
	}
	next := merged(fields, set)
	if err := def.Collection.Fits(next); err != nil {
		return SystemResult{}, err
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return SystemResult{}, err
	}
	if string(raw) == string(current) {
		return SystemResult{Revision: revision}, nil
	}
	var size int
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(sum(length(CAST(data AS BLOB))), 0) FROM app_rows WHERE app_id = ? AND deleted_at IS NULL`, w.AppID).
		Scan(&size); err != nil {
		return SystemResult{}, err
	}
	if size-len(data)+len(raw) > MaxDataSize {
		return SystemResult{}, appFull(fmt.Sprintf("an app holds at most %d MiB", MaxDataSize>>20))
	}
	at, actor := now(), w.Actor.column()
	if _, err := tx.ExecContext(ctx, `
		UPDATE app_rows SET data = ?, revision = ?, updated_at = ?, updated_by = NULL, updated_by_agent = 0, updated_via = NULL,
			updated_action = ?, updated_system = ?
		WHERE id = ? AND app_id = ?`,
		string(raw), revision+1, at, w.Action, actor, w.RecordID, w.AppID); err != nil {
		return SystemResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO app_row_history (app_id, row_id, revision, operation, data, at, actor, actor_agent, actor_via, actor_system, action)
		VALUES (?, ?, ?, ?, ?, ?, NULL, 0, NULL, ?, ?)`,
		w.AppID, w.RecordID, revision+1, opUpdate, string(raw), at, actor, w.Action); err != nil {
		return SystemResult{}, err
	}
	return SystemResult{Revision: revision + 1, Changed: true}, nil
}

// authorOrSystem is the author of a change: the user's, or the system actor's if the change was a
// system write (the user columns are then NULL, which for a change without one means a
// deleted account).
func authorOrSystem(id sql.NullInt64, name string, agent bool, via, system sql.NullString) *Author {
	if system.Valid {
		if kind, rest, ok := strings.Cut(system.String, ":"); ok {
			if n, err := strconv.ParseInt(rest, 10, 64); err == nil {
				return &Author{System: &SystemAuthor{Kind: kind, ID: n}}
			}
		}
	}
	return author(id, name, agent, via)
}
