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

// App records. The records of personal apps are searched in full text only, and apart from
// documents and cards: the values of a record's string, choice and barcode fields, as the
// app's live release declares them (apps.SearchText), and a search finds the records the caller
// may read, in the apps of the spaces they may read. Semantic search does not take records in:
// that needs consent of its own, since a record's text would go to the embeddings endpoint.
//
// What a person may read of an app's records is the apps' to say, once: the roles of the app
// (access.Checker.App), and in them the scope of what they read, every record or only those
// they made (apps.RecordScope, apps.ReadsRecord: what lists, histories and queries of records
// go by). The index holds text only, never who may read it: a search first asks which apps the
// caller reads in whole and which only their own records of, and finds in those. Not in it, as
// in no other read of the records: a draft (only a live release declares fields to people), a
// deleted record, an archived app, a field the live release doesn't declare, and any field's
// name (the index holds values only, and a result names the app and its space, which the caller
// reads, and nothing of its definition).
//
// The apps' rows are the truth, as the boards' cards are for card search (cards.go). Triggers
// (migration search_records_conversations) queue a record, in the transaction that changes it,
// when it is added, its data changes or it is deleted or restored, and every record of an app
// when another release goes live; a search first indexes the queue's records of the apps it
// looks in (syncRecords), each batch in a transaction of its own that reads the records and
// writes what it read and takes them off the queue. A record in the queue is never quoted, since
// its indexed text may be old: a search that finds some behind tries once more, and says its
// results may be incomplete if it still does. So the index is right whatever wrote the records,
// and a crash leaves nothing behind.
//
// Records are ordered by when they changed last, newest first, never by a score: a ranking
// with statistics of the whole index would let records the caller can't read change what they
// can.

// recordBatch is how many records one transaction of a sync indexes.
const recordBatch = 200

// recordSyncMax is the most records a search indexes before it answers; the rest are left for the
// background sync, and the answer says it may be incomplete if the caller reads any of them.
const recordSyncMax = 2000

// recordSyncBudget is how long a search spends indexing before it answers.
const recordSyncBudget = 20 * time.Second

// recordSyncTries is how many times a search indexes the records it looks in, and looks
// again, before it says its results may be incomplete.
const recordSyncTries = 2

// RecordResult is a record found.
type RecordResult struct {
	SpaceID   int64  `json:"space_id"`
	SpaceName string `json:"space_name"`
	AppID     int64  `json:"app_id"`
	AppName   string `json:"app_name"`
	RecordID  int64  `json:"record_id"`
	// Snippet is the passage of the record's values that matched, as plain text in pieces:
	// never markup.
	Snippet []Segment `json:"snippet"`

	// creator made the record: what decides, with the caller's roles, whether they read it.
	creator sql.NullInt64
}

// RecordResults are a page of records found, newest change first.
type RecordResults struct {
	Results []RecordResult `json:"results"`
	// NextOffset is the next page's offset, if there are more results; never with Incomplete.
	NextOffset *int `json:"next_offset,omitempty"`
	// Incomplete is true when records changed that the index couldn't catch up with: those
	// are left out, and a later search finds them. Such a reply has no NextOffset.
	Incomplete bool `json:"incomplete,omitempty"`
}

// recordReach is which apps a caller reads the records of: in whole, or only those they made.
type recordReach struct {
	all, own []int64
}

func (r recordReach) none() bool { return len(r.all) == 0 && len(r.own) == 0 }

// ids are every app in reach.
func (r recordReach) ids() []int64 { return append(append([]int64{}, r.all...), r.own...) }

// reachOf asks access.Checker, for each live and unarchived app in the spaces in (a JSON array
// of IDs), what userID may read of its records, as of now.
func (x *Index) reachOf(ctx context.Context, checker *access.Checker, userID int64, in string) (recordReach, error) {
	rows, err := x.db.QueryContext(ctx, `SELECT id, space_id FROM apps
		WHERE space_id IN (SELECT value FROM json_each(?)) AND archived_at IS NULL AND live_release IS NOT NULL
		ORDER BY id`, in)
	if err != nil {
		return recordReach{}, err
	}
	type ref struct{ app, space int64 }
	var refs []ref
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.app, &r.space); err != nil {
			_ = rows.Close()
			return recordReach{}, err
		}
		refs = append(refs, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return recordReach{}, err
	}
	reach := recordReach{all: []int64{}, own: []int64{}}
	for _, r := range refs {
		acc, err := checker.App(ctx, userID, r.space, r.app)
		switch {
		case errors.Is(err, access.ErrNotFound) || errors.Is(err, access.ErrAppNotFound):
			continue
		case err != nil:
			return recordReach{}, err
		}
		switch apps.RecordScope(acc) {
		case access.ScopeAll:
			reach.all = append(reach.all, r.app)
		case access.ScopeOwn:
			reach.own = append(reach.own, r.app)
		}
	}
	return reach, nil
}

// SearchRecords finds the records matching q in the apps of spaces, which must be of
// workspaceID and readable by userID, as for Search: the space RequireSpace checked, or the
// user's Readable spaces. Of those apps, only the records userID may read are searched.
//
// Indexing takes time, and who may read what can change meanwhile, or an app can move to
// another space: so after the records are found, the access of each result's app, in the space
// it is in now, is asked again with checker, and a record the caller no longer reads is left
// out.
func (x *Index) SearchRecords(ctx context.Context, checker *access.Checker, userID, workspaceID int64, spaces []access.Space, q Query) (RecordResults, error) {
	match, err := checkQuery(&q)
	if err != nil {
		return RecordResults{}, err
	}
	ids := []int64{}
	for _, sp := range spaces {
		// Never anything else, whatever the caller passes.
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
	reach, err := x.reachOf(ctx, checker, userID, string(spacesJSON))
	if err != nil {
		return RecordResults{}, err
	}
	if reach.none() {
		return res, nil
	}
	// What the caller reads, as the statements ask for it: the apps of each kind, and the sync's.
	scopeOf := func(r recordReach) (scope recordScope, appsJSON string, err error) {
		all, err := json.Marshal(r.all)
		if err != nil {
			return scope, "", err
		}
		own, err := json.Marshal(r.own)
		if err != nil {
			return scope, "", err
		}
		both, err := json.Marshal(r.ids())
		if err != nil {
			return scope, "", err
		}
		return recordScope{spaces: string(spacesJSON), all: string(all), own: string(own), user: userID}, string(both), nil
	}
	scope, appsJSON, err := scopeOf(reach)
	if err != nil {
		return RecordResults{}, err
	}
	var found []RecordResult
	// One allowance of records and one deadline for all the search's attempts: what is left when
	// they are used stays queued for the background sync, and the search says it is incomplete.
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
				// Out of time: the rest is for the background sync, and found behind.
			case err != nil:
				// What is indexed is still searched: some records may be missing.
				x.log.Error("search: syncing records before a search; its results may be behind", "err", err)
			}
		}
		if x.afterRecordSync != nil {
			x.afterRecordSync()
		}
		var behind bool
		if found, behind, err = x.findRecords(ctx, scope, match, q); err != nil {
			return RecordResults{}, err
		}
		// What the caller reads is asked again: if it changed while the search ran, what was found,
		// and whether records were behind, are of the old access, and the flag could say that a
		// record the caller no longer reads has changed. Look again with the new.
		now, err := x.reachOf(ctx, checker, userID, string(spacesJSON))
		if err != nil {
			return RecordResults{}, err
		}
		if !slices.Equal(now.all, reach.all) || !slices.Equal(now.own, reach.own) {
			reach = now
			if reach.none() {
				return res, nil
			}
			if scope, appsJSON, err = scopeOf(reach); err != nil {
				return RecordResults{}, err
			}
			if try == recordSyncTries {
				res.Incomplete = true
				break
			}
			continue
		}
		if !behind {
			break
		}
		if try == recordSyncTries {
			res.Incomplete = true
			break
		}
	}
	// One more than asked for was read, to know whether a page follows: that one is never
	// returned, whatever the access check leaves of the others, or the next page would
	// repeat it.
	more := len(found) > q.Limit
	if more {
		found = found[:q.Limit]
	}
	// Who may read what is asked once more, for the app as it is now, in the space it is in
	// now: an app moved to a space the caller can't read, or whose roles changed, takes its
	// records from the results.
	type appKey struct{ space, app int64 }
	allowed := map[appKey]*access.AppAccess{}
	names := map[int64]string{}
	for _, r := range found {
		k := appKey{r.SpaceID, r.AppID}
		if _, ok := allowed[k]; ok {
			continue
		}
		sp, err := checker.Check(ctx, userID, r.SpaceID)
		switch {
		case errors.Is(err, access.ErrNotFound) || (err == nil && sp.WorkspaceID != workspaceID):
			allowed[k] = nil
			continue
		case err != nil:
			return RecordResults{}, err
		}
		acc, err := checker.App(ctx, userID, r.SpaceID, r.AppID)
		switch {
		case errors.Is(err, access.ErrNotFound) || errors.Is(err, access.ErrAppNotFound):
			allowed[k] = nil
		case err != nil:
			return RecordResults{}, err
		default:
			allowed[k] = &acc
			names[r.SpaceID] = sp.Name
		}
	}
	dropped := false
	for _, r := range found {
		acc := allowed[appKey{r.SpaceID, r.AppID}]
		if acc == nil || !apps.ReadsRecord(*acc, userID, r.creator) {
			dropped = true
			continue
		}
		r.SpaceName = names[r.SpaceID]
		res.Results = append(res.Results, r)
	}
	// Access changed while the page was found: an offset counts the records the caller read when
	// the search began, so one continued from here could skip a record. Say so instead, and let
	// them search again.
	if dropped {
		res.Incomplete = true
	}
	// No next page past the deepest offset asked for: it would be refused. And none after an
	// incomplete one: an offset counts the records indexed now, and once the queued ones are
	// the same offset would repeat a record or skip one.
	if next := q.Offset + q.Limit; more && !res.Incomplete && next <= MaxOffset {
		res.NextOffset = &next
	}
	return res, nil
}

// recordScope is whose records a search may find: those of the apps in all, and of the apps in
// own that user made, in the spaces in spaces (each a JSON array of IDs). The spaces are said
// too, so an app that moved since reach was asked for isn't found in the space it is in now.
type recordScope struct {
	spaces, all, own string
	user             int64
}

// recordScopeSQL is the condition on a row r of an app a that scope allows, and its arguments.
const recordScopeSQL = `a.space_id IN (SELECT value FROM json_each(?))
	AND (r.app_id IN (SELECT value FROM json_each(?))
		OR (r.created_by = ? AND r.app_id IN (SELECT value FROM json_each(?))))`

func (s recordScope) args() []any { return []any{s.spaces, s.all, s.user, s.own} }

// queuedRecordsSQL is the queued live records that scope allows: only those the caller may
// read, so a record someone else's roles keep from them never says anything to them, not even
// that it changed.
const queuedRecordsSQL = `FROM search_record_queue sq
	JOIN app_rows r ON r.id = sq.row_id
	JOIN apps a ON a.id = r.app_id
	WHERE r.deleted_at IS NULL AND a.archived_at IS NULL AND a.live_release IS NOT NULL AND ` + recordScopeSQL

// recordSnippetSQL is the record's snippet, matches marked. It is 24 words long, or 64 in a
// text with an unspaced script (see snippetSQL).
const recordSnippetSQL = `snippet(search_record_fts, 0, char(2), char(3), '…',
	CASE WHEN instr(search_record_fts.body, char(31)) THEN 64 ELSE 24 END)`

// findRecords returns one page of the records that match the FTS5 query match and scope allows,
// newest change first, and one more if a page follows; and whether any record scope allows was
// in the queue. A record whose indexed text may be old, since it is in the queue, is left out
// of the page, so a page found while some were queued may be short, or empty. Both come from
// one statement, so they are of one state of the database (a plain read, never a transaction):
// a record queued at the moment of the page is never missed by the flag, even when the page
// has no rows.
func (x *Index) findRecords(ctx context.Context, scope recordScope, match string, q Query) ([]RecordResult, bool, error) {
	args := append(scope.args(), match)
	args = append(args, scope.args()...)
	args = append(args, q.Limit+1, q.Offset)
	rows, err := x.db.QueryContext(ctx, `SELECT st.behind, p.id, p.space_id, p.app_id, p.app_name, p.created_by, p.snippet, p.updated_at
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
		// The flag's scope, then the page's match and scope, limit and offset.
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

// queuedRecordIDsSQL is a batch of the queue's records after an ID, of every app or of those
// in a JSON array: it starts from the queue, never from every record.
const queuedRecordIDsSQL = `SELECT sq.row_id
	FROM search_record_queue sq
	JOIN app_rows r ON r.id = sq.row_id
	WHERE sq.row_id > ? AND (? OR r.app_id IN (SELECT value FROM json_each(?)))
	ORDER BY sq.row_id LIMIT ?`

// queueRecords puts in the queue the live records of unarchived apps with a live release that were never
// indexed, or in an older textFormat: what the migration, or a change of the format, leaves.
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

// syncRecords indexes again the queued records of the apps in (a JSON array of IDs; every app
// if it is empty), a batch at a time, until none is left.
func (x *Index) syncRecords(ctx context.Context, in string) error {
	_, err := x.syncRecordsWithin(ctx, in, 0)
	return err
}

// syncRecordsWithin is syncRecords for at most limit records (0: all), leaving the rest queued:
// a search that is over its allowance, or its time (ctx's deadline), leaves them to the background
// sync, and finds them behind. It returns how many it indexed.
func (x *Index) syncRecordsWithin(ctx context.Context, in string, limit int) (indexed int, err error) {
	all := in == ""
	if all {
		in = "[]"
	}
	// Batches go by ID, each from where the last stopped, so a record changed again meanwhile
	// can't keep a sync going: the search that wanted it finds it behind.
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

// indexRecord puts the record as it is now in the index, or takes it out of it if it is gone,
// deleted, or of an app with no live release.
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
	// Read and written in this transaction, so a change made after it queues the record again.
	_, err = tx.ExecContext(ctx, `DELETE FROM search_record_queue WHERE row_id = ?`, id)
	return err
}
