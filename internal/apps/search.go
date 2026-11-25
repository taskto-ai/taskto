package apps

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"taskto.ai/taskto/internal/access"
)

// What the search index (package search, records.go) takes from the apps. The index holds the
// text of each live record's fields, and a search finds the records of the apps the caller
// reads; who reads which record, and which fields people see, are decided here, once, as for
// every other read of the records, and the index asks.

// MaxSearchText is the most text of one record the index holds, in bytes: the values of its
// fields up to it, in the collection's order.
const MaxSearchText = 64 << 10

// RecordScope is how many of an app's records acc may read: access.ScopeNone for someone who may
// not open the app at all, as for one whose roles read no record. It is what the list of records
// and its history, deleted records and queries go by (readable).
func RecordScope(acc access.AppAccess) access.Scope {
	if !acc.View {
		return access.ScopeNone
	}
	return acc.ReadRecords
}

// ReadsRecord reports whether the person acc is for, userID, may read a record that creator made
// (invalid: its creator's account is gone, which only someone who reads every record sees): the
// check every list of records makes of each of them.
func ReadsRecord(acc access.AppAccess, userID int64, creator sql.NullInt64) bool {
	return acc.View && readable(acc, userID).sees(creator)
}

// SearchText returns the text of record id that the search index holds: the values of the fields
// of strings, choices (as people read them: their labels, and the stored value too, if it is
// another) and barcodes that the live release's collection declares, one per line. Never a field
// name, and never anything the collection doesn't declare (stored under another release, or of
// another type than its field's), as no read of the record shows it either.
//
// indexed is false for a record that is gone, is deleted, or belongs to an app that is archived
// (taken offline) or has no live release (only a draft: no record is shown), whose text the
// index must not hold.
func SearchText(ctx context.Context, q queryer, id int64) (text string, indexed bool, err error) {
	var data string
	var deleted, archived, definition, requires sql.NullString
	var number, version sql.NullInt64
	err = q.QueryRowContext(ctx, `SELECT r.data, r.deleted_at, a.archived_at, rel.number, rel.definition_version, rel.requires, rel.definition
		FROM app_rows r
		JOIN apps a ON a.id = r.app_id
		LEFT JOIN app_releases rel ON rel.app_id = a.id AND rel.number = a.live_release
		WHERE r.id = ?`, id).Scan(&data, &deleted, &archived, &number, &version, &requires, &definition)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if deleted.Valid || archived.Valid || !number.Valid {
		return "", false, nil
	}
	rel, err := newRelease(number.Int64, int(version.Int64), requires.String, "", sql.NullInt64{}, "")
	if err != nil {
		return "", false, err
	}
	def, err := decodeDefinition(definition.String, rel.DefinitionVersion, rel.Requires)
	if err != nil {
		return "", false, fmt.Errorf("row %d: stored release %d: %w", id, number.Int64, err)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(data), &fields); err != nil {
		return "", false, fmt.Errorf("row %d: %w", id, err)
	}
	var b strings.Builder
	for _, f := range def.Collection.Fields {
		if f.Type != TypeString && f.Type != TypeChoice && f.Type != TypeBarcode {
			continue
		}
		v, ok := fields[f.Name]
		if !ok || !ofType(f, v) {
			continue
		}
		s, ok := v.(string)
		if !ok || s == "" {
			continue
		}
		if f.Type == TypeChoice {
			if label := f.label(s); label != s {
				s = label + "\n" + s
			}
		}
		if b.Len()+len(s)+1 > MaxSearchText {
			break
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(s)
	}
	return b.String(), true, nil
}
