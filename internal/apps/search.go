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

const MaxSearchText = 64 << 10

func RecordScope(acc access.AppAccess) access.Scope {
	if !acc.View {
		return access.ScopeNone
	}
	return acc.ReadRecords
}

func ReadsRecord(acc access.AppAccess, userID int64, creator sql.NullInt64) bool {
	return acc.View && readable(acc, userID).sees(creator)
}

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
