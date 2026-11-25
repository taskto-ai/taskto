package apps

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/boards"
)

// Seeing the effects of an app, retrying the ones that failed, and trying an action out without
// doing anything (a preview).
//
// Effects are the app's records' as far as who may see them: the entries of a record are listed
// to whoever reads the record (visibility), never to someone who doesn't, and what an entry
// made (the card's IDs, the document's path) is told only to one who may read where it is. Only
// the user who acted may retry an entry, and only with the rights the action needs now: the worker
// checks again, as them, when it runs.

// EffectView is an effect as the API lists it.
type EffectView struct {
	ID int64 `json:"id"`
	// Operation is the stable operation ID of the request that queued it, Position the effect's
	// place among that action's.
	Operation string `json:"operation"`
	Position  int    `json:"position"`
	Kind      string `json:"kind"`
	Name      string `json:"name,omitempty"`
	Action    string `json:"action"`
	// Record and RecordRevision are what the action changed.
	Record         int64 `json:"record"`
	RecordRevision int64 `json:"record_revision"`
	// Status is pending, running, done, failed or, in a preview, simulated.
	Status   string `json:"status"`
	Attempts int    `json:"attempts"`
	// NextAttemptAt is when a pending effect that failed before is tried again.
	NextAttemptAt *string `json:"next_attempt_at"`
	// Error is why a failed effect failed, or why a simulated one would.
	Error *EffectError `json:"error"`
	// Detail is what the effect makes, as the record filled it in.
	Detail EffectDetail `json:"detail"`
	// Result is what a done effect made, if the caller may read where: a card's IDs, a
	// document's path, a route as kind and IDs.
	Result    map[string]any `json:"result,omitempty" apitypes:"json"`
	CreatedAt string         `json:"created_at,omitempty"`
	UpdatedAt string         `json:"updated_at,omitempty"`
	DoneAt    *string        `json:"done_at"`
	// CanRetry is whether the caller may retry it: it failed and they are who acted.
	CanRetry bool `json:"can_retry"`
}

// EffectError is the code (and params) of a failure, for the web app to say.
type EffectError struct {
	Code   apierr.Code   `json:"code"`
	Params apierr.Params `json:"params,omitempty"`
}

// EffectDetail is what an effect makes, shortly: the card's title and column, the document's
// path, the kind of route.
type EffectDetail struct {
	Title  string `json:"title,omitempty"`
	Column string `json:"column,omitempty"`
	Path   string `json:"path,omitempty"`
	Route  string `json:"route,omitempty"`
}

func detailOf(p payload) EffectDetail {
	return EffectDetail{Title: p.Title, Column: p.Column, Path: p.Path, Route: p.Route}
}

// EffectList is a page of effects, newest first, with how many of the app's wait and failed.
type EffectList struct {
	Effects []EffectView `json:"effects" apitypes:"nonnull"`
	// More is whether older effects than the page's last match too (ask again with before).
	More bool `json:"more"`
	// Active and Failed count the entries the caller may see that wait or run, and that failed.
	Active int `json:"active"`
	Failed int `json:"failed"`
}

// EffectFilter selects effects: a record's, those in a state (a status, or "active" for those
// waiting or running), at most Limit.
type EffectFilter struct {
	Record int64
	Status string
	Limit  int
	// Before, if set, lists only the effects older than this one (the ID of the last of the page
	// before), to read on past a page.
	Before int64
}

var effectStatuses = []string{StatePending, StateRunning, StateDone, StateFailed, "active"}

// view presents an entry to a caller. canRead says whether they may read a space; live is the
// collection the app declares now, nil if it has none.
//
// What an entry says of its target (what it makes, why it failed) is for those who may read the
// target now, exactly as its result is: a person whose access to the space ended is told the entry's
// status and code, and nothing of the place. And what the templates made of the record's fields is
// shown only while the live release still declares those fields as they were: a field the release
// removed or retyped is not for people to read, however it got into a title or a path.
func (s *Service) view(e effectRow, caller Actor, canRead func(int64) bool, live *Collection) (EffectView, error) {
	targetReadable := canRead(e.payload.SpaceID)
	fieldsHold := live != nil && e.payload.usesHold(*live)
	v := EffectView{
		ID: e.id, Operation: e.operation, Position: e.position, Kind: e.kind, Name: e.name, Action: e.action,
		Record: e.recordID, RecordRevision: e.recordRevision, Status: e.state, Attempts: e.attempts,
		CreatedAt: e.createdAt, UpdatedAt: e.updatedAt,
		CanRetry: e.state == StateFailed && e.userID == caller.UserID,
	}
	v.Detail = EffectDetail{Route: e.payload.Route}
	if targetReadable {
		v.Detail = detailOf(e.payload)
		if !fieldsHold {
			v.Detail.Title, v.Detail.Path = "", ""
		}
	}
	if e.doneAt.Valid {
		v.DoneAt = &e.doneAt.String
	}
	if e.state == StatePending && e.attempts > 0 {
		v.NextAttemptAt = &e.nextAttemptAt
	}
	if e.errCode.Valid && (e.state == StateFailed || e.state == StatePending) {
		v.Error = &EffectError{Code: apierr.Code(e.errCode.String)}
		if e.errParams.Valid && targetReadable {
			_ = json.Unmarshal([]byte(e.errParams.String), &v.Error.Params)
			// What the templates made of the record's fields is no more shown in an error than in
			// a detail or a result, once the live release no longer declares those fields.
			if !fieldsHold {
				delete(v.Error.Params, "path")
				delete(v.Error.Params, "title")
			}
		}
	}
	if e.state == StateDone && e.result.Valid {
		var res map[string]any
		if err := json.Unmarshal([]byte(e.result.String), &res); err != nil {
			return EffectView{}, err
		}
		if canRead(resultSpace(res)) {
			if !fieldsHold {
				withheld(res)
			}
			v.Result = res
		}
	}
	return v, nil
}

// withheld removes from a result what was made from the record's fields (a card's title, a
// document's path), at the top and in a route.
func withheld(res map[string]any) {
	delete(res, "title")
	delete(res, "path")
	if route, ok := res["route"].(map[string]any); ok {
		delete(route, "path")
	}
}

// resultSpace is the space a result points into.
func resultSpace(res map[string]any) int64 {
	if route, ok := res["route"].(map[string]any); ok {
		res = route
	}
	id, _ := res["space_id"].(float64)
	return int64(id)
}

// readability caches, for one request, whether the caller reads each space.
func (s *Service) readability(ctx context.Context, userID int64) func(int64) bool {
	known := map[int64]bool{}
	return func(spaceID int64) bool {
		ok, seen := known[spaceID]
		if !seen {
			ok, _ = s.access.CanRead(ctx, userID, spaceID)
			known[spaceID] = ok
		}
		return ok
	}
}

// effectsReader checks that the caller may read the app's records, which entries are listed with,
// and says whose.
func (s *Service) effectsReader(ctx context.Context, sp access.Space, a Actor, appID int64) (visibility, error) {
	if err := may(sp, access.Read); err != nil {
		return visibility{}, err
	}
	acc, err := s.appAccess(ctx, a, sp.ID, appID)
	if err != nil {
		return visibility{}, err
	}
	if err := need(acc, access.CapView); err != nil {
		return visibility{}, err
	}
	return readable(acc, a.UserID), nil
}

// listEffects lists an app's effects that the caller may see: those of records they may read.
func (s *Service) listEffects(ctx context.Context, sp access.Space, a Actor, appID int64, f EffectFilter) (EffectList, error) {
	v, err := s.effectsReader(ctx, sp, a, appID)
	if err != nil {
		return EffectList{}, err
	}
	out := EffectList{Effects: []EffectView{}}
	if v.scope == access.ScopeNone {
		return out, nil
	}
	if f.Status != "" && !slices.Contains(effectStatuses, f.Status) {
		return EffectList{}, invalidf("status is one of %v", effectStatuses)
	}
	if f.Before < 0 {
		return EffectList{}, invalidf("before is an effect's ID")
	}
	if f.Limit < 0 || f.Limit > maxListed {
		return EffectList{}, invalidf("limit is 1 to %d", maxListed)
	}
	if f.Limit == 0 {
		f.Limit = defaultListed
	}
	var live *Collection
	if ap, err := launchable(ctx, s.db, sp.ID, appID); err == nil && ap.liveDef != nil {
		live = &ap.liveDef.Collection
	}
	where, args := `e.app_id = ?`, []any{appID}
	if v.scope == access.ScopeOwn {
		where, args = where+` AND r.created_by = ?`, append(args, v.user)
	}
	// An app's counts are over everything the caller sees, whatever the page.
	// #nosec G202 -- the conditions joined are constants above; values go in args.
	counts, err := s.db.QueryContext(ctx, `SELECT e.state, count(*) `+effectFrom+` WHERE `+where+` AND e.state <> 'done' GROUP BY e.state`, args...)
	if err != nil {
		return EffectList{}, err
	}
	defer counts.Close()
	for counts.Next() {
		var state string
		var n int
		if err := counts.Scan(&state, &n); err != nil {
			return EffectList{}, err
		}
		if state == StateFailed {
			out.Failed += n
		} else {
			out.Active += n
		}
	}
	if err := counts.Err(); err != nil {
		return EffectList{}, err
	}
	if f.Record != 0 {
		where, args = where+` AND e.record_id = ?`, append(args, f.Record)
	}
	if f.Before != 0 {
		where, args = where+` AND e.id < ?`, append(args, f.Before)
	}
	switch f.Status {
	case "":
	case "active":
		where += ` AND e.state IN ('pending', 'running')`
	default:
		where, args = where+` AND e.state = ?`, append(args, f.Status)
	}
	// #nosec G202 -- the conditions joined are constants above; values go in args.
	rows, err := s.db.QueryContext(ctx, `SELECT `+effectColumns+effectFrom+` WHERE `+where+` ORDER BY e.id DESC LIMIT ?`, append(args, f.Limit+1)...)
	if err != nil {
		return EffectList{}, err
	}
	defer rows.Close()
	canRead := s.readability(ctx, a.UserID)
	for rows.Next() {
		e, err := scanEffect(rows)
		if err != nil {
			return EffectList{}, err
		}
		view, err := s.view(e, a, canRead, live)
		if err != nil {
			return EffectList{}, err
		}
		out.Effects = append(out.Effects, view)
	}
	if err := rows.Err(); err != nil {
		return EffectList{}, err
	}
	if len(out.Effects) > f.Limit {
		out.Effects, out.More = out.Effects[:f.Limit], true
	}
	return out, nil
}

// retryEffect puts a failed effect back to be done, with its attempts counted afresh. It is for
// the user who acted, who must still hold the right the action needed: the worker asks again,
// as them, and a retry doesn't widen what they may do.
func (s *Service) retryEffect(ctx context.Context, sp access.Space, a Actor, appID, id int64) (EffectView, error) {
	if err := may(sp, access.Write); err != nil {
		return EffectView{}, err
	}
	v, err := s.effectsReader(ctx, sp, a, appID)
	if err != nil {
		return EffectView{}, err
	}
	if v.scope == access.ScopeNone {
		return EffectView{}, ErrNotFound // no record is theirs to read, so no effect is
	}
	var e effectRow
	err = s.write(ctx, func(tx *sql.Tx) error {
		acc, err := s.recheckApp(ctx, a, sp.ID, appID)
		if err != nil {
			return err
		}
		where, args := `e.id = ? AND e.app_id = ?`, []any{id, appID}
		if v.scope != access.ScopeAll {
			where, args = where+` AND r.created_by = ?`, append(args, a.UserID)
		}
		e, err = scanEffect(tx.QueryRowContext(ctx, `SELECT `+effectColumns+effectFrom+` WHERE `+where, args...))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if e.userID != a.UserID {
			return forbidden("retry_effect")
		}
		// The right the action needed, over the record as the role says now.
		var creator sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT created_by FROM app_rows WHERE id = ?`, e.recordID).Scan(&creator); err != nil {
			return err
		}
		if e.actionKind == ActionInsert {
			if err := need(acc, access.CapSubmit); err != nil {
				return err
			}
		} else if !(visibility{scope: acc.ModifyRecords, user: a.UserID}).sees(creator) {
			return forbidden(capModifyRecords)
		}
		// A retry is waiting work like any other: the app's bound holds.
		var waiting int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM app_effects WHERE app_id = ? AND state IN ('pending', 'running')`, appID).Scan(&waiting); err != nil {
			return err
		}
		if waiting >= maxOutstanding {
			return backlogError()
		}
		// By the worker's clock, which the queue's times are in.
		at := s.stamp(s.clock())
		res, err := tx.ExecContext(ctx, `
			UPDATE app_effects SET state = 'pending', attempts = 0, next_attempt_at = ?, lease_until = NULL,
				error_code = NULL, error_params = NULL, updated_at = ?
			WHERE id = ? AND state = 'failed'`, at, at, id)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			if err != nil {
				return err
			}
			return errEffectNotFailed
		}
		e, err = scanEffect(tx.QueryRowContext(ctx, `SELECT `+effectColumns+effectFrom+` WHERE e.id = ?`, id))
		return err
	})
	if err != nil {
		return EffectView{}, err
	}
	s.poke()
	var live *Collection
	if ap, err := launchable(ctx, s.db, sp.ID, appID); err == nil && ap.liveDef != nil {
		live = &ap.liveDef.Collection
	}
	return s.view(e, a, s.readability(ctx, a.UserID), live)
}

// Preview is what an action would set going, simulated.
type Preview struct {
	Effects []EffectView `json:"effects"`
}

// previewAction works out what an action of the app's DRAFT would do about effects, and does
// nothing: no record changes, no operation is kept, no entry is queued, no card or document is
// made, and none of the app's rows is read. The action is tried, as in a preview, on the rows the
// caller sends or else on sample rows made from the draft's collection (a preview holds no photos
// or signatures, so those fields are never required). It is for those who may design or publish
// the app, who look at the draft. An effect whose target the caller couldn't write is reported
// with the code it would fail with.
func (s *Service) previewAction(ctx context.Context, sp access.Space, a Actor, appID int64, req PreviewRequest) (Preview, error) {
	if err := may(sp, access.Read); err != nil {
		return Preview{}, err
	}
	acc, err := s.appAccess(ctx, a, sp.ID, appID)
	if err != nil {
		return Preview{}, err
	}
	if !reviews(acc) {
		return Preview{}, forbidden(string(access.CapDesign))
	}
	if req.Action == nil {
		return Preview{}, invalidf("an action to preview is needed")
	}
	ap, err := launchable(ctx, s.db, sp.ID, appID)
	if err != nil {
		return Preview{}, err
	}
	def := ap.draftDef
	ch, err := Plan(SurfaceID(ap.ID), previewDefinition(def), *req.Action)
	if err != nil {
		return Preview{}, err
	}
	out := Preview{Effects: []EffectView{}}
	if ch.Kind != ActionInsert && ch.Kind != ActionUpdate {
		return out, nil
	}
	if ch.Kind == ActionInsert && !acc.Submit {
		return Preview{}, forbidden(string(access.CapSubmit))
	}
	if ch.Kind == ActionUpdate && acc.ModifyRecords == access.ScopeNone {
		return Preview{}, forbidden(capModifyRecords)
	}
	var rows []Row
	if req.Rows == nil {
		rows = SampleRows(def.Collection)
	} else if rows, err = previewRows(def, req.Rows); err != nil {
		return Preview{}, err
	}
	if rows, err = simulate(def, rows, ch); err != nil {
		return Preview{}, err
	}
	out.Effects = s.simulatedEffects(ctx, a.UserID, def, ch, rows)
	return out, nil
}

// simulatedEffects is what ch, an insert or an update already made to rows (simulate), would set
// going: the effects of its action that apply to the record it made, as a preview shows them.
func (s *Service) simulatedEffects(ctx context.Context, userID int64, def Definition, ch Change, rows []Row) []EffectView {
	out := []EffectView{}
	var rec Row
	for _, r := range rows {
		if (ch.Kind == ActionInsert && r.ID > rec.ID) || (ch.Kind == ActionUpdate && r.ID == ch.ID) {
			rec = r
		}
	}
	if rec.ID == 0 {
		return out
	}
	for i, e := range def.Actions[ch.Name].Effects {
		// A payment link is an integration's to simulate (the preview never calls one).
		if e.Kind == EffectCreatePaymentLink || !e.applies(def.Collection, rec.Data) {
			continue
		}
		// A new record has no number yet: its details say "new".
		id := rec.ID
		if ch.Kind == ActionInsert {
			id = 0
		}
		p := e.payload(def.Collection, rec.Data, id)
		v := EffectView{
			Position: i, Kind: e.Kind, Name: e.Name, Action: ch.Name, Record: id, Status: StateSimulated,
			Detail: detailOf(p), NextAttemptAt: nil, DoneAt: nil,
		}
		if code := s.previewTarget(ctx, userID, e, p); code != "" {
			v.Error = &EffectError{Code: code}
		}
		out = append(out, v)
	}
	return out
}

// previewTarget is the code an effect would fail with for lack of access to its target, or "".
func (s *Service) previewTarget(ctx context.Context, userID int64, e Effect, p payload) apierr.Code {
	if e.targetUnset() {
		return "" // a draft whose board isn't chosen yet: nothing is aimed at, so nothing is refused
	}
	targetsSpace := e.Kind == EffectCreateCard || e.Kind == EffectCreateDocument || (e.Kind == EffectOpenRoute && p.SpaceID != 0)
	if !targetsSpace {
		return ""
	}
	if _, err := s.target(ctx, userID, p.SpaceID, e.Kind != EffectOpenRoute); err != nil {
		var ee *effectError
		if errors.As(err, &ee) {
			return ee.code
		}
	}
	return ""
}

// EffectTarget names a space, and a board of it, that an effect of the app's draft acts on or
// opens. The names are told only to a caller who may read the target: the same workspace as the
// app, and a space and board they can read. Otherwise the target is listed by its IDs alone, and
// nothing is told of what it is, or whether it exists.
type EffectTarget struct {
	SpaceID int64 `json:"space_id"`
	// Space and SpaceKind are the space's name and kind, when the caller may read it.
	Space     string      `json:"space,omitempty"`
	SpaceKind access.Kind `json:"space_kind,omitempty"`
	BoardID   int64       `json:"board_id,omitempty"`
	// Board is the board's name, when the caller may read the space and the board is one of its.
	Board string `json:"board,omitempty"`
}

// effectTargets lists the targets of def's effects, each once, in the order of the actions'
// names, with the names the caller may be told (EffectTarget).
func (s *Service) effectTargets(ctx context.Context, a Actor, appSpace int64, def *Definition) ([]EffectTarget, error) {
	var out []EffectTarget
	boardsOf := map[int64][]boards.Board{}
	for _, name := range sortedKeys(def.Actions) {
		for _, e := range def.Actions[name].Effects {
			if e.SpaceID == 0 {
				continue
			}
			t := EffectTarget{SpaceID: e.SpaceID, BoardID: e.BoardID}
			if slices.ContainsFunc(out, func(o EffectTarget) bool { return o.SpaceID == t.SpaceID && o.BoardID == t.BoardID }) {
				continue
			}
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	// The workspace of the app's own space: a target is always in it.
	home, err := s.access.Check(ctx, a.UserID, appSpace)
	if err != nil {
		return nil, err
	}
	for i, t := range out {
		sp, err := s.access.Check(ctx, a.UserID, t.SpaceID)
		if errors.Is(err, access.ErrNotFound) || (err == nil && (sp.WorkspaceID != home.WorkspaceID || !sp.Access.Allows(access.Read))) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[i].Space, out[i].SpaceKind = sp.Name, sp.Kind
		if t.BoardID == 0 || s.platform.Boards == nil {
			continue
		}
		all, ok := boardsOf[t.SpaceID]
		if !ok {
			if all, err = s.platform.Boards.ListBoards(ctx, boards.Actor(a), t.SpaceID); err != nil {
				return nil, err
			}
			boardsOf[t.SpaceID] = all
		}
		for _, b := range all {
			if b.ID == t.BoardID {
				out[i].Board = b.Name
			}
		}
		// A board that isn't one of the space's (archived, or gone) is the IDs alone, like a target
		// the caller can't read: nothing of the space is told for it.
		if out[i].Board == "" {
			out[i].Space, out[i].SpaceKind = "", ""
		}
	}
	return out, nil
}

// checkEffectTargets checks, when an app is saved or published, that the one saving it may write
// every space and board its effects name: a definition can't point at what its author couldn't use
// themselves. It tells nothing about a space the author can't read that they couldn't tell from
// the refusal for any other. (Whoever acts is checked again when an effect runs.)
//
// A target is also always in workspaceID, the workspace of the app's space, whatever the author
// can reach: an app doesn't act across workspaces, even for someone who belongs to both.
func (s *Service) checkEffectTargets(ctx context.Context, workspaceID int64, a Actor, def Definition) error {
	// A target is checked for each permission an effect needs of it: a route that only reads a
	// board must not stand in for a card made on the same board, which writes.
	type target struct {
		space, board int64
		write        bool
	}
	var seen []target
	for _, name := range sortedKeys(def.Actions) {
		for _, e := range def.Actions[name].Effects {
			if e.SpaceID == 0 {
				continue
			}
			write := e.Kind == EffectCreateCard || e.Kind == EffectCreateDocument
			t := target{e.SpaceID, e.BoardID, write}
			if slices.Contains(seen, t) {
				continue
			}
			seen = append(seen, t)
			if err := s.targetOfDefinition(ctx, workspaceID, a, t.space, t.board, t.write); err != nil {
				return err
			}
		}
	}
	return nil
}

// targetOfDefinition checks that a may use the space and board an effect of an app of workspaceID
// names: the space is in that workspace, and a reads it (writes it, for an effect that writes).
func (s *Service) targetOfDefinition(ctx context.Context, workspaceID int64, a Actor, spaceID, boardID int64, write bool) error {
	refused := invalidAs(apierr.CodeAppEffectTarget, apierr.Params{"space_id": spaceID},
		"an effect targets space %d, which you can't %s: choose a space and board you can", spaceID, map[bool]string{true: "write", false: "read"}[write])
	sp, err := s.access.Check(ctx, a.UserID, spaceID)
	if errors.Is(err, access.ErrNotFound) {
		return refused
	}
	if err != nil {
		return err
	}
	if sp.WorkspaceID != workspaceID || !sp.Access.Allows(access.Read) || (write && !sp.Access.Allows(access.Write)) {
		return refused
	}
	if boardID == 0 || s.platform.Boards == nil {
		return nil
	}
	all, err := s.platform.Boards.ListBoards(ctx, boards.Actor(a), spaceID)
	if err != nil {
		return err
	}
	for _, b := range all {
		if b.ID == boardID {
			return nil
		}
	}
	return refused
}

func (s *Service) handleEffects(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	s.listed(w, r, req, 0)
}

func (s *Service) handleRecordEffects(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	id, ok := recordID(w, r)
	if !ok {
		return
	}
	s.listed(w, r, req, id)
}

func (s *Service) listed(w http.ResponseWriter, r *http.Request, req request, record int64) {
	f := EffectFilter{Record: record, Status: r.URL.Query().Get("status")}
	if b := r.URL.Query().Get("before"); b != "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "before is an effect's ID")
			return
		}
		f.Before = n
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil {
			writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "limit is a number")
			return
		}
		f.Limit = n
	}
	out, err := s.listEffects(r.Context(), req.space, req.actor, req.app, f)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) handleRetryEffect(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("effect"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusNotFound, apierr.CodeNotFound, ErrNotFound.Error())
		return
	}
	var body struct{}
	if !readJSON(w, r, &body) {
		return
	}
	v, err := s.retryEffect(r.Context(), req.space, req.actor, req.app, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Service) handlePreviewAction(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	// The body is an action, with the rows of the preview it is tried on, if any.
	var body struct {
		ClientAction
		Rows []PreviewRow `json:"rows"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	out, err := s.previewAction(r.Context(), req.space, req.actor, req.app, PreviewRequest{Action: &body.ClientAction, Rows: body.Rows})
	if errors.Is(err, ErrInvalid) {
		invalid(w, http.StatusUnprocessableEntity, err)
		return
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
