package apps

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"taskto.ai/taskto/internal/access"
)

// Records, their history and their deletion. A record is a row of the app's collection. Who
// may read a record's history, or find it among the deleted, is who may read the record
// (the read scope of their role in the app: all, or only the records they created); who may
// restore it is who may change it (the modify scope), as for deleting. Whatever the caller
// can't read is answered as if it didn't exist.

// Limits of what the history and deleted routes answer in one reply.
const (
	// MaxHistory bounds the versions of one record in a reply: the newest ones.
	MaxHistory = 200
	// MaxListed bounds the records in a reply: the most recently changed live ones, or the
	// most recently deleted ones.
	MaxListed = 1000
)

// Record is one record with its provenance. Data holds the fields the live release's collection
// declares (as items do); the others stay in the row, unseen.
type Record struct {
	ID       int64          `json:"id"`
	Revision int64          `json:"revision"`
	Data     map[string]any `json:"data"`

	CreatedAt     string  `json:"created_at"`
	CreatedBy     *Author `json:"created_by"`
	CreatedAction string  `json:"created_action,omitempty"`
	UpdatedAt     string  `json:"updated_at"`
	UpdatedBy     *Author `json:"updated_by"`
	UpdatedAction string  `json:"updated_action,omitempty"`
	// DeletedAt is when the record was deleted, or nil if it is live.
	DeletedAt     *string `json:"deleted_at"`
	DeletedBy     *Author `json:"deleted_by"`
	DeletedAction string  `json:"deleted_action,omitempty"`
}

// Version is one revision of a record, as its history lists it.
type Version struct {
	Revision int64 `json:"revision"`
	// Operation is "insert", "update", "delete" or "restore".
	Operation string         `json:"operation"`
	Data      map[string]any `json:"data"`
	At        string         `json:"at"`
	By        *Author        `json:"by"`
	// Action is the app's action that made the change; empty for a restore.
	Action string `json:"action,omitempty"`
	// Synthetic marks the one entry of a record that existed before revisions were kept:
	// its data is the record as it was then, and earlier states are unknown.
	Synthetic bool `json:"synthetic,omitempty"`
}

// History is a record and its versions, newest first.
type History struct {
	Record  Record    `json:"record"`
	History []Version `json:"history"`
	// Truncated is whether older versions than MaxHistory were left out.
	Truncated bool `json:"truncated"`
}

// Records is some of an app's records the caller may read: the live ones most recently
// changed first, or the deleted ones most recently deleted first.
type Records struct {
	Records   []Record `json:"records"`
	Truncated bool     `json:"truncated"`
}

const (
	recordColumns = `
	SELECT r.id, r.revision, r.data, r.created_by,
		r.created_at, coalesce(cu.name, ''), r.created_by_agent, r.created_via, coalesce(r.created_action, ''),
		r.updated_at, r.updated_by, coalesce(uu.name, ''), r.updated_by_agent, r.updated_via, coalesce(r.updated_action, ''),
		r.deleted_at, r.deleted_by, coalesce(du.name, ''), r.deleted_by_agent, r.deleted_via, coalesce(r.deleted_action, '')`
	recordFrom = `
	FROM app_rows r
	LEFT JOIN users cu ON cu.id = r.created_by
	LEFT JOIN users uu ON uu.id = r.updated_by
	LEFT JOIN users du ON du.id = r.deleted_by`
	recordSelect = recordColumns + recordFrom
)

// scanRecord reads a record selected with recordSelect, and its creator, which decides who
// reads it.
func scanRecord(row scanner, def Definition) (Record, sql.NullInt64, error) {
	var rec Record
	var creator, updater, deleter sql.NullInt64
	var data, createdName, updatedName, deletedName string
	var createdAgent, updatedAgent, deletedAgent bool
	var createdVia, updatedVia, deletedVia sql.NullString
	if err := row.Scan(&rec.ID, &rec.Revision, &data, &creator,
		&rec.CreatedAt, &createdName, &createdAgent, &createdVia, &rec.CreatedAction,
		&rec.UpdatedAt, &updater, &updatedName, &updatedAgent, &updatedVia, &rec.UpdatedAction,
		&rec.DeletedAt, &deleter, &deletedName, &deletedAgent, &deletedVia, &rec.DeletedAction); err != nil {
		return Record{}, sql.NullInt64{}, err
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(data), &fields); err != nil {
		return Record{}, sql.NullInt64{}, fmt.Errorf("row %d: %w", rec.ID, err)
	}
	rec.Data = declared(def, fields)
	rec.CreatedBy = author(creator, createdName, createdAgent, createdVia)
	rec.UpdatedBy = author(updater, updatedName, updatedAgent, updatedVia)
	rec.DeletedBy = author(deleter, deletedName, deletedAgent, deletedVia)
	return rec, creator, nil
}

// readableRecords is what a caller may read of an app's records, for its history and its
// deleted ones: ErrNotFound for an app they can't see, ErrForbidden if their roles read no
// record at all.
func (s *Service) readableRecords(ctx context.Context, sp access.Space, a Actor, appID int64) (visibility, Definition, error) {
	if err := may(sp, access.Read); err != nil {
		return visibility{}, Definition{}, err
	}
	acc, err := s.appAccess(ctx, a, sp.ID, appID)
	if err != nil {
		return visibility{}, Definition{}, err
	}
	if acc.ReadRecords == access.ScopeNone {
		return visibility{}, Definition{}, forbidden(capReadRecords)
	}
	ap, err := app(ctx, s.db, sp.ID, appID)
	if err != nil {
		return visibility{}, Definition{}, err
	}
	// The fields shown are the live release's: what the app declares to people. An app with
	// no release has no records to show.
	var def Definition
	if ap.liveDef != nil {
		def = *ap.liveDef
	}
	return readable(acc, a.UserID), def, nil
}

// recordHistory returns record id of appID, deleted or not, and its versions.
func (s *Service) recordHistory(ctx context.Context, sp access.Space, a Actor, appID, id int64) (History, error) {
	v, def, err := s.readableRecords(ctx, sp, a, appID)
	if err != nil {
		return History{}, err
	}
	rec, creator, err := scanRecord(s.db.QueryRowContext(ctx, recordSelect+` WHERE r.id = ? AND r.app_id = ?`, id, appID), def)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !v.sees(creator)) {
		return History{}, ErrNotFound
	}
	if err != nil {
		return History{}, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT h.revision, h.operation, h.data, h.at, h.actor, coalesce(u.name, ''), h.actor_agent, h.actor_via, coalesce(h.action, ''), h.synthetic
		FROM app_row_history h LEFT JOIN users u ON u.id = h.actor
		WHERE h.row_id = ? AND h.app_id = ?
		ORDER BY h.revision DESC LIMIT ?`, id, appID, MaxHistory+1)
	if err != nil {
		return History{}, err
	}
	defer rows.Close()
	out := History{Record: rec, History: []Version{}}
	for rows.Next() {
		var ver Version
		var data, name string
		var actor sql.NullInt64
		var agent bool
		var via sql.NullString
		if err := rows.Scan(&ver.Revision, &ver.Operation, &data, &ver.At, &actor, &name, &agent, &via, &ver.Action, &ver.Synthetic); err != nil {
			return History{}, err
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(data), &fields); err != nil {
			return History{}, fmt.Errorf("row %d revision %d: %w", id, ver.Revision, err)
		}
		ver.Data = declared(def, fields)
		ver.By = author(actor, name, agent, via)
		out.History = append(out.History, ver)
	}
	if err := rows.Err(); err != nil {
		return History{}, err
	}
	if len(out.History) > MaxHistory {
		out.History, out.Truncated = out.History[:MaxHistory], true
	}
	return out, nil
}

// listRecords lists the live records of appID that the caller may read, or the deleted ones.
func (s *Service) listRecords(ctx context.Context, sp access.Space, a Actor, appID int64, deleted bool) (Records, error) {
	v, def, err := s.readableRecords(ctx, sp, a, appID)
	if err != nil {
		return Records{}, err
	}
	query, order := recordSelect+` WHERE r.app_id = ? AND r.deleted_at IS NULL`, ` ORDER BY r.updated_at DESC, r.id DESC LIMIT ?`
	if deleted {
		query, order = recordSelect+` WHERE r.app_id = ? AND r.deleted_at IS NOT NULL`, ` ORDER BY r.deleted_at DESC, r.id DESC LIMIT ?`
	}
	args := []any{appID}
	if v.scope == access.ScopeOwn {
		query += ` AND r.created_by = ?`
		args = append(args, v.user)
	}
	rows, err := s.db.QueryContext(ctx, query+order, append(args, MaxListed+1)...)
	if err != nil {
		return Records{}, err
	}
	defer rows.Close()
	out := Records{Records: []Record{}}
	for rows.Next() {
		rec, _, err := scanRecord(rows, def)
		if err != nil {
			return Records{}, err
		}
		out.Records = append(out.Records, rec)
	}
	if err := rows.Err(); err != nil {
		return Records{}, err
	}
	if len(out.Records) > MaxListed {
		out.Records, out.Truncated = out.Records[:MaxListed], true
	}
	return out, nil
}

// restoreRecord brings a deleted record back, with its history, as a new revision. It needs
// the right to change the record (as deleting did). expected, if given, is the revision the
// caller saw: the deletion's. A record that isn't deleted, or has moved on, is ErrConflict.
// The record must still fit the collection, and the app have room for it.
func (s *Service) restoreRecord(ctx context.Context, sp access.Space, a Actor, appID, id int64, expected *int64) (Record, error) {
	if err := may(sp, access.Write); err != nil {
		return Record{}, err
	}
	if expected != nil && *expected < 1 {
		return Record{}, invalidf("expected_revision is a revision, from 1")
	}
	var restored Record
	err := s.write(ctx, func(tx *sql.Tx) error {
		acc, err := s.recheckApp(ctx, a, sp.ID, appID)
		if err != nil {
			return err
		}
		if acc.ModifyRecords == access.ScopeNone {
			return forbidden(capModifyRecords)
		}
		ap, err := launchable(ctx, tx, sp.ID, appID)
		if err != nil {
			return err
		}
		if ap.liveDef == nil {
			return ErrNotPublished
		}
		def := *ap.liveDef // the record must fit what the app declares now
		var data string
		var revision int64
		var creator sql.NullInt64
		var deletedAt sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT data, revision, created_by, deleted_at FROM app_rows WHERE id = ? AND app_id = ?`, id, appID).
			Scan(&data, &revision, &creator, &deletedAt)
		modify := visibility{scope: acc.ModifyRecords, user: a.UserID}
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !modify.sees(creator)) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(data), &fields); err != nil {
			return err
		}
		if !deletedAt.Valid || (expected != nil && *expected != revision) {
			e := &conflictError{id: id}
			// A live record is shown as it is now, if the caller reads it. A deleted one is
			// in no ordinary read, and the caller learns of it from the list of deleted ones.
			if !deletedAt.Valid && readable(acc, a.UserID).sees(creator) {
				e.current = item(def, Row{ID: id, Revision: revision, Data: fields})
			}
			return e
		}
		if err := def.Collection.Fits(fields); err != nil {
			return err // the collection changed since: the record no longer fits it
		}
		var count, size int
		if err := tx.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(length(CAST(data AS BLOB))), 0) FROM app_rows WHERE app_id = ? AND deleted_at IS NULL`, appID).
			Scan(&count, &size); err != nil {
			return err
		}
		if count >= MaxRows || size+len(data) > MaxDataSize {
			return appFull(fmt.Sprintf("an app holds at most %d rows and %d MiB", MaxRows, MaxDataSize>>20))
		}
		at := now()
		if _, err := tx.ExecContext(ctx, `
			UPDATE app_rows SET revision = ?, deleted_at = NULL, deleted_by = NULL, deleted_by_agent = 0, deleted_via = NULL, deleted_action = NULL,
				updated_at = ?, updated_by = ?, updated_by_agent = ?, updated_via = ?, updated_action = NULL
			WHERE id = ? AND app_id = ?`,
			revision+1, at, a.UserID, a.Agent, via(a), id, appID); err != nil {
			return err
		}
		if err := remember(ctx, tx, appID, id, revision+1, opRestore, data, at, a, Change{}); err != nil {
			return err
		}
		restored, _, err = scanRecord(tx.QueryRowContext(ctx, recordSelect+` WHERE r.id = ? AND r.app_id = ?`, id, appID), def)
		return err
	})
	return restored, err
}
