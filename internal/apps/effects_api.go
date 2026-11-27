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
	"taskto.ai/taskto/internal/currency"
)

type EffectView struct {
	ID             int64          `json:"id"`
	Operation      string         `json:"operation"`
	Position       int            `json:"position"`
	Kind           string         `json:"kind"`
	Name           string         `json:"name,omitempty"`
	Action         string         `json:"action"`
	Record         int64          `json:"record"`
	RecordRevision int64          `json:"record_revision"`
	Status         string         `json:"status"`
	Attempts       int            `json:"attempts"`
	NextAttemptAt  *string        `json:"next_attempt_at"`
	Error          *EffectError   `json:"error"`
	Detail         EffectDetail   `json:"detail"`
	Result         map[string]any `json:"result,omitempty" apitypes:"json"`
	CreatedAt      string         `json:"created_at,omitempty"`
	UpdatedAt      string         `json:"updated_at,omitempty"`
	DoneAt         *string        `json:"done_at"`
	CanRetry       bool           `json:"can_retry"`
}

type EffectError struct {
	Code   apierr.Code   `json:"code"`
	Params apierr.Params `json:"params,omitempty"`
}

type EffectDetail struct {
	Title    string `json:"title,omitempty"`
	Column   string `json:"column,omitempty"`
	Path     string `json:"path,omitempty"`
	Route    string `json:"route,omitempty"`
	Amount   string `json:"amount,omitempty"`
	Currency string `json:"currency,omitempty"`
}

func detailOf(p payload) EffectDetail {
	d := EffectDetail{Title: p.Title, Column: p.Column, Path: p.Path, Route: p.Route}
	if p.Slot != "" {
		d.Title, d.Currency = p.Description, p.Currency
		if minor, err := currency.Minor(p.Amount, p.Currency); err == nil {
			d.Amount = currency.Format(minor, p.Currency)
		} else {
			d.Amount = p.Amount
		}
	}
	return d
}

type EffectList struct {
	Effects []EffectView `json:"effects" apitypes:"nonnull"`
	More    bool         `json:"more"`
	Active  int          `json:"active"`
	Failed  int          `json:"failed"`
}

type EffectFilter struct {
	Record int64
	Status string
	Limit  int
	Before int64
}

var effectStatuses = []string{StatePending, StateRunning, StateDone, StateFailed, "active"}

func (s *Service) view(e effectRow, caller Actor, canRead func(int64) bool, live *Collection) (EffectView, error) {
	targetReadable := canRead(e.payload.SpaceID)
	fieldsHold := live != nil && e.payload.usesHold(*live)
	v := EffectView{
		ID: e.id, Operation: e.operation, Position: e.position, Kind: e.kind, Name: e.name, Action: e.action,
		Record: e.recordID, RecordRevision: e.recordRevision, Status: e.state, Attempts: e.attempts,
		CreatedAt: e.createdAt, UpdatedAt: e.updatedAt,
		CanRetry: e.state == StateFailed && e.userID == caller.UserID && caller.Via == "",
	}
	v.Detail = EffectDetail{Route: e.payload.Route}
	if targetReadable {
		v.Detail = detailOf(e.payload)
		if !fieldsHold {
			v.Detail.Title, v.Detail.Path, v.Detail.Amount, v.Detail.Currency = "", "", "", ""
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
			if !fieldsHold {
				delete(v.Error.Params, "path")
				delete(v.Error.Params, "title")
				delete(v.Error.Params, "currency")
				delete(v.Error.Params, "max")
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

func withheld(res map[string]any) {
	delete(res, "title")
	delete(res, "path")
	delete(res, "amount")
	delete(res, "currency")
	if route, ok := res["route"].(map[string]any); ok {
		delete(route, "path")
	}
}

func resultSpace(res map[string]any) int64 {
	if route, ok := res["route"].(map[string]any); ok {
		res = route
	}
	id, _ := res["space_id"].(float64)
	return int64(id)
}

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

func (s *Service) retryEffect(ctx context.Context, sp access.Space, a Actor, appID, id int64) (EffectView, error) {
	if err := may(sp, access.Write); err != nil {
		return EffectView{}, err
	}
	v, err := s.effectsReader(ctx, sp, a, appID)
	if err != nil {
		return EffectView{}, err
	}
	if v.scope == access.ScopeNone {
		return EffectView{}, ErrNotFound
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
		var waiting int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM app_effects WHERE app_id = ? AND state IN ('pending', 'running')`, appID).Scan(&waiting); err != nil {
			return err
		}
		if waiting >= maxOutstanding {
			return backlogError()
		}
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

type Preview struct {
	Effects []EffectView `json:"effects"`
}

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
		if !e.applies(def.Collection, rec.Data) {
			continue
		}
		id := rec.ID
		if ch.Kind == ActionInsert {
			id = 0
		}
		p := e.payload(def.Collection, rec.Data, id)
		v := EffectView{
			Position: i, Kind: e.Kind, Name: e.Name, Action: ch.Name, Record: id, Status: StateSimulated,
			Detail: detailOf(p), NextAttemptAt: nil, DoneAt: nil,
		}
		if e.Kind == EffectCreatePaymentLink {
			var code apierr.Code
			v.Detail, code = previewPaymentLink(p)
			v.Detail.Title = p.Description
			if code != "" {
				v.Error = &EffectError{Code: code}
			}
		} else if code := s.previewTarget(ctx, userID, e, p); code != "" {
			v.Error = &EffectError{Code: code}
		}
		out = append(out, v)
	}
	return out
}

func (s *Service) previewTarget(ctx context.Context, userID int64, e Effect, p payload) apierr.Code {
	if e.targetUnset() {
		return ""
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

type EffectTarget struct {
	SpaceID   int64       `json:"space_id"`
	Space     string      `json:"space,omitempty"`
	SpaceKind access.Kind `json:"space_kind,omitempty"`
	BoardID   int64       `json:"board_id,omitempty"`
	Board     string      `json:"board,omitempty"`
}

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
		if out[i].Board == "" {
			out[i].Space, out[i].SpaceKind = "", ""
		}
	}
	return out, nil
}

func (s *Service) checkEffectTargets(ctx context.Context, workspaceID int64, a Actor, def Definition) error {
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
