package search

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apps"
)

const recordBatch = 200

const recordSyncMax = 2000

const recordSyncBudget = 20 * time.Second

const recordSyncTries = 2

type RecordResult struct {
	SpaceID   int64     `json:"space_id"`
	SpaceName string    `json:"space_name"`
	AppID     int64     `json:"app_id"`
	AppName   string    `json:"app_name"`
	RecordID  int64     `json:"record_id"`
	Snippet   []Segment `json:"snippet"`

	creator sql.NullInt64
}

type RecordResults struct {
	Results    []RecordResult `json:"results"`
	NextOffset *int           `json:"next_offset,omitempty"`
	Incomplete bool           `json:"incomplete,omitempty"`
}

type recordReach struct {
	all, own []int64
}

func (r recordReach) none() bool { return len(r.all) == 0 && len(r.own) == 0 }

func (r recordReach) ids() []int64 { return append(append([]int64{}, r.all...), r.own...) }

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (x *Index) reachOf(ctx context.Context, checker *access.Checker, db queryer, userID int64, in string) (recordReach, map[int64]access.AppAccess, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, space_id FROM apps
		WHERE space_id IN (SELECT value FROM json_each(?)) AND archived_at IS NULL AND live_release IS NOT NULL
		ORDER BY id`, in)
	if err != nil {
		return recordReach{}, nil, err
	}
	type ref struct{ app, space int64 }
	var refs []ref
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.app, &r.space); err != nil {
			_ = rows.Close()
			return recordReach{}, nil, err
		}
		refs = append(refs, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return recordReach{}, nil, err
	}
	reach := recordReach{all: []int64{}, own: []int64{}}
	accs := map[int64]access.AppAccess{}
	for _, r := range refs {
		acc, err := checker.App(ctx, userID, r.space, r.app)
		switch {
		case errors.Is(err, access.ErrNotFound) || errors.Is(err, access.ErrAppNotFound):
			continue
		case err != nil:
			return recordReach{}, nil, err
		}
		switch apps.RecordScope(acc) {
		case access.ScopeAll:
			reach.all = append(reach.all, r.app)
			accs[r.app] = acc
		case access.ScopeOwn:
			reach.own = append(reach.own, r.app)
			accs[r.app] = acc
		}
	}
	return reach, accs, nil
}

func (x *Index) SearchRecords(ctx context.Context, checker *access.Checker, userID, workspaceID int64, spaces []access.Space, q Query) (RecordResults, error) {
	match, err := checkQuery(&q)
	if err != nil {
		return RecordResults{}, err
	}
	ids := []int64{}
	for _, sp := range spaces {
		if sp.WorkspaceID != workspaceID || !sp.Access.Allows(access.Read) {
			continue
		}
		ids = append(ids, sp.ID)
	}
	res := RecordResults{Results: []RecordResult{}}
	if len(ids) == 0 {
		return res, nil
	}
	spacesJSON, err := json.Marshal(ids)
	if err != nil {
		return RecordResults{}, err
	}
	before, _, err := x.reachOf(ctx, checker, x.db, userID, string(spacesJSON))
	if err != nil {
		return RecordResults{}, err
	}
	if before.none() {
		return res, nil
	}
	scopeOf := func(r recordReach) (recordScope, error) {
		all, err := json.Marshal(r.all)
		if err != nil {
			return recordScope{}, err
		}
		own, err := json.Marshal(r.own)
		if err != nil {
			return recordScope{}, err
		}
		return recordScope{spaces: string(spacesJSON), all: string(all), own: string(own), user: userID}, nil
	}
	syncIn := func(r recordReach) (string, error) {
		both, err := json.Marshal(r.ids())
		return string(both), err
	}
	appsJSON, err := syncIn(before)
	if err != nil {
		return RecordResults{}, err
	}
	var found []RecordResult
	var accs map[int64]access.AppAccess
	names := map[int64]string{}
	allowance := x.recordSyncLimit
	syncCtx, stopSync := context.WithTimeout(ctx, recordSyncBudget)
	defer stopSync()
	for try := 1; ; try++ {
		if allowance > 0 && syncCtx.Err() == nil {
			n, err := x.syncRecordsWithin(syncCtx, appsJSON, allowance)
			allowance -= n
			switch {
			case ctx.Err() != nil:
				return RecordResults{}, ctx.Err()
			case err != nil && syncCtx.Err() != nil:
			case err != nil:
				x.log.Error("search: syncing records before a search; its results may be behind", "err", err)
			}
		}
		if x.afterRecordSync != nil {
			x.afterRecordSync()
		}
		var behind, changed bool
		var now recordReach
		err := checker.Snapshot(ctx, func(snap *access.Checker, tx *sql.Tx) error {
			var err error
			if now, accs, err = x.reachOf(ctx, snap, tx, userID, string(spacesJSON)); err != nil {
				return err
			}
			if q.Offset > 0 && (!slices.Equal(now.all, before.all) || !slices.Equal(now.own, before.own)) {
				changed = true
				return nil
			}
			if now.none() {
				found = nil
				return nil
			}
			scope, err := scopeOf(now)
			if err != nil {
				return err
			}
			if found, behind, err = x.findRecords(ctx, tx, scope, match, q); err != nil {
				return err
			}
			clear(names)
			for _, r := range found {
				if _, ok := names[r.SpaceID]; ok {
					continue
				}
				sp, err := snap.Check(ctx, userID, r.SpaceID)
				switch {
				case errors.Is(err, access.ErrNotFound) || (err == nil && sp.WorkspaceID != workspaceID):
					names[r.SpaceID] = ""
				case err != nil:
					return err
				default:
					names[r.SpaceID] = sp.Name
				}
			}
			return nil
		})
		if err != nil {
			return RecordResults{}, err
		}
		if changed {
			res.Incomplete = true
			return res, nil
		}
		if now.none() {
			return res, nil
		}
		if !behind {
			break
		}
		if try == recordSyncTries {
			res.Incomplete = true
			break
		}
		if appsJSON, err = syncIn(now); err != nil {
			return RecordResults{}, err
		}
	}
	more := len(found) > q.Limit
	if more {
		found = found[:q.Limit]
	}
	dropped := false
	for _, r := range found {
		acc, ok := accs[r.AppID]
		name := names[r.SpaceID]
		if !ok || name == "" || !apps.ReadsRecord(acc, userID, r.creator) {
			dropped = true
			continue
		}
		r.SpaceName = name
		res.Results = append(res.Results, r)
	}
	if dropped {
		res.Incomplete = true
	}
	if next := q.Offset + q.Limit; more && !res.Incomplete && next <= MaxOffset {
		res.NextOffset = &next
	}
	return res, nil
}

type recordScope struct {
	spaces, all, own string
	user             int64
}

const recordScopeSQL = `a.space_id IN (SELECT value FROM json_each(?))
	AND (r.app_id IN (SELECT value FROM json_each(?))
		OR (r.created_by = ? AND r.app_id IN (SELECT value FROM json_each(?))))`

func (s recordScope) args() []any { return []any{s.spaces, s.all, s.user, s.own} }

const queuedRecordsSQL = `FROM search_record_queue sq
	JOIN app_rows r ON r.id = sq.row_id
	JOIN apps a ON a.id = r.app_id
	WHERE r.deleted_at IS NULL AND a.archived_at IS NULL AND a.live_release IS NOT NULL AND ` + recordScopeSQL

const recordSnippetSQL = `snippet(search_record_fts, 0, char(2), char(3), '…',
	CASE WHEN instr(search_record_fts.body, char(31)) THEN 64 ELSE 24 END)`

func (x *Index) findRecords(ctx context.Context, db queryer, scope recordScope, match string, q Query) ([]RecordResult, bool, error) {
	args := append(scope.args(), match)
	args = append(args, scope.args()...)
	args = append(args, q.Limit+1, q.Offset)
	rows, err := db.QueryContext(ctx, `SELECT st.behind, p.id, p.space_id, p.app_id, p.app_name, p.created_by, p.snippet, p.updated_at
		FROM (SELECT EXISTS (SELECT 1 `+queuedRecordsSQL+`) AS behind) st
		LEFT JOIN (
			SELECT r.id AS id, a.space_id AS space_id, a.id AS app_id, a.name AS app_name, r.created_by AS created_by,
				r.updated_at AS updated_at,
				`+recordSnippetSQL+` AS snippet
			FROM search_record_fts
			JOIN app_rows r ON r.id = search_record_fts.rowid
			JOIN apps a ON a.id = r.app_id
			WHERE search_record_fts MATCH ? AND r.deleted_at IS NULL AND a.archived_at IS NULL AND a.live_release IS NOT NULL
				AND `+recordScopeSQL+`
				AND NOT EXISTS (SELECT 1 FROM search_record_queue WHERE row_id = r.id)
			ORDER BY r.updated_at DESC, r.id DESC LIMIT ? OFFSET ?) p ON true
		ORDER BY p.updated_at DESC, p.id DESC`,
		args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var found []RecordResult
	var behind bool
	for rows.Next() {
		var id, space, app, creator sql.NullInt64
		var appName, snippet, updatedAt sql.NullString
		if err := rows.Scan(&behind, &id, &space, &app, &appName, &creator, &snippet, &updatedAt); err != nil {
			return nil, false, err
		}
		if !id.Valid {
			continue
		}
		found = append(found, RecordResult{RecordID: id.Int64, SpaceID: space.Int64, AppID: app.Int64,
			AppName: appName.String, Snippet: segments(snippet.String), creator: creator})
	}
	return found, behind, rows.Err()
}

const queuedRecordIDsSQL = `SELECT sq.row_id
	FROM search_record_queue sq
	JOIN app_rows r ON r.id = sq.row_id
	WHERE sq.row_id > ? AND (? OR r.app_id IN (SELECT value FROM json_each(?)))
	ORDER BY sq.row_id LIMIT ?`

func (x *Index) queueRecords(ctx context.Context) error {
	return x.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO search_record_queue (row_id)
			SELECT r.id FROM app_rows r
			JOIN apps a ON a.id = r.app_id
			LEFT JOIN search_records s ON s.row_id = r.id
			WHERE (s.row_id IS NULL AND r.deleted_at IS NULL AND a.archived_at IS NULL AND a.live_release IS NOT NULL) OR s.text_format != ?`, textFormat)
		return err
	})
}

func (x *Index) syncRecords(ctx context.Context, in string) error {
	_, err := x.syncRecordsWithin(ctx, in, 0)
	return err
}

func (x *Index) syncRecordsWithin(ctx context.Context, in string, limit int) (indexed int, err error) {
	all := in == ""
	if all {
		in = "[]"
	}
	var after int64
	for limit == 0 || indexed < limit {
		size := recordBatch
		if limit > 0 {
			size = min(size, limit-indexed)
		}
		rows, err := x.db.QueryContext(ctx, queuedRecordIDsSQL, after, all, in, size)
		if err != nil {
			return indexed, err
		}
		var stale []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return indexed, err
			}
			stale = append(stale, id)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return indexed, err
		}
		if len(stale) == 0 {
			return indexed, nil
		}
		after = stale[len(stale)-1]
		if err := x.write(ctx, func(tx *sql.Tx) error {
			for _, id := range stale {
				if err := indexRecord(ctx, tx, id); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return indexed, err
		}
		indexed += len(stale)
	}
	return indexed, nil
}

func indexRecord(ctx context.Context, tx *sql.Tx, id int64) error {
	text, indexed, err := apps.SearchText(ctx, tx, id)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM search_record_fts WHERE rowid = ?`, id); err != nil {
		return err
	}
	if indexed {
		if _, err := tx.ExecContext(ctx, `INSERT INTO search_record_fts (rowid, body) VALUES (?, ?)`,
			id, segment(cleanBody([]byte(text)))); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO search_records (row_id, text_format) VALUES (?, ?)
			ON CONFLICT (row_id) DO UPDATE SET text_format = excluded.text_format`, id, textFormat); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `DELETE FROM search_records WHERE row_id = ?`, id); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM search_record_queue WHERE row_id = ?`, id)
	return err
}
