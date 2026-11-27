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

var ErrSystemField = errors.New("a system write may only set the fields its contract declares")

type systemFieldError struct{ field, why string }

func (e *systemFieldError) Error() string {
	return ErrSystemField.Error() + ": " + e.field + " " + e.why
}
func (e *systemFieldError) Is(target error) bool { return target == ErrSystemField }

const SystemKindIntegration = "integration"

type SystemActor struct {
	Kind string
	ID   int64
}

func (a SystemActor) column() string { return a.Kind + ":" + strconv.FormatInt(a.ID, 10) }

var systemActionPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

type SystemWrite struct {
	Actor                    SystemActor
	SpaceID, AppID, RecordID int64
	Action                   string
	Declared                 []string
	Set                      map[string]any
	Expected                 *int64
	Guard                    func(ctx context.Context, tx *sql.Tx) error
}

type SystemResult struct {
	Revision int64
	Changed  bool
	Skipped  bool
}

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

type systemDecision struct {
	set      map[string]any
	declared []string
	skip     bool
}

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
