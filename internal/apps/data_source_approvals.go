package apps

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/auth"
)

type DataSourceApproval struct {
	ID           int64              `json:"id"`
	AppID        int64              `json:"app_id"`
	AppName      string             `json:"app_name"`
	SpaceID      int64              `json:"space_id"`
	SpaceName    string             `json:"space_name"`
	SpaceKind    string             `json:"space_kind"`
	Slot         string             `json:"slot"`
	Kind         string             `json:"kind"`
	Contract     DataSourceContract `json:"contract"`
	ContractHash string             `json:"contract_hash"`
	Status       string             `json:"status"`
	RequestedBy  *Author            `json:"requested_by"`
	RequestedAt  string             `json:"requested_at"`
	DecidedBy    *Author            `json:"decided_by"`
	DecidedAt    *string            `json:"decided_at"`
}

type DataSourceApprovalList struct {
	Approvals []DataSourceApproval `json:"approvals" apitypes:"nonnull"`
}

type DataSourceSlotStatus struct {
	Slot         string             `json:"slot"`
	Kind         string             `json:"kind"`
	Contract     DataSourceContract `json:"contract"`
	ContractHash string             `json:"contract_hash"`
	Status       string             `json:"status"`
	ApprovalID   *int64             `json:"approval_id"`
}

type DataSourceStatus struct {
	Slots []DataSourceSlotStatus `json:"slots" apitypes:"nonnull"`
}

type sourceRow struct {
	id, appID, spaceID, workspaceID    int64
	appName, spaceName, spaceKind      string
	slot, kind, hash, contract, status string
	requestedBy, decidedBy             sql.NullInt64
	requestedByName, decidedByName     string
	requestedAt                        string
	decidedAt                          sql.NullString
}

const sourceSelect = `
	SELECT a.id, a.app_id, ap.space_id, sp.workspace_id, ap.name, sp.name, sp.kind,
		a.slot, a.kind, a.contract_hash, a.contract, a.status,
		a.requested_by, coalesce(ru.name, ''), a.requested_at, a.decided_by, coalesce(du.name, ''), a.decided_at
	FROM app_data_source_approvals a
	JOIN apps ap ON ap.id = a.app_id
	JOIN spaces sp ON sp.id = ap.space_id
	LEFT JOIN users ru ON ru.id = a.requested_by
	LEFT JOIN users du ON du.id = a.decided_by`

func scanSource(row scanner) (sourceRow, error) {
	var r sourceRow
	err := row.Scan(&r.id, &r.appID, &r.spaceID, &r.workspaceID, &r.appName, &r.spaceName, &r.spaceKind,
		&r.slot, &r.kind, &r.hash, &r.contract, &r.status,
		&r.requestedBy, &r.requestedByName, &r.requestedAt, &r.decidedBy, &r.decidedByName, &r.decidedAt)
	return r, err
}

func (r sourceRow) view() DataSourceApproval {
	v := DataSourceApproval{ID: r.id, AppID: r.appID, AppName: r.appName, SpaceID: r.spaceID, SpaceName: r.spaceName, SpaceKind: r.spaceKind,
		Slot: r.slot, Kind: r.kind, ContractHash: r.hash, Status: r.status,
		RequestedBy: author(r.requestedBy, r.requestedByName, false, sql.NullString{}), RequestedAt: r.requestedAt,
		DecidedBy: author(r.decidedBy, r.decidedByName, false, sql.NullString{})}
	if r.decidedAt.Valid {
		v.DecidedAt = &r.decidedAt.String
	}
	if err := json.Unmarshal([]byte(r.contract), &v.Contract); err != nil || v.Contract.Read == nil {
		v.Contract.Read = []string{}
	}
	return v
}

func dataSourceApprovals(ctx context.Context, q queryer, appID int64) ([]sourceRow, error) {
	rows, err := q.QueryContext(ctx, sourceSelect+` WHERE a.app_id = ? ORDER BY a.id DESC`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sourceRow
	for rows.Next() {
		r, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func findDataSourceApproval(rows []sourceRow, slot, hash string) (sourceRow, bool) {
	var latest sourceRow
	found := false
	for _, r := range rows {
		if r.slot != slot || r.hash != hash {
			continue
		}
		if r.status == ApprovalRequested || r.status == ApprovalApproved {
			return r, true
		}
		if !found {
			latest, found = r, true
		}
	}
	return latest, found
}

func (s *Service) checkDataSourceApprovals(ctx context.Context, q queryer, appID int64, def Definition) error {
	if len(def.DataSources) == 0 {
		return nil
	}
	rows, err := dataSourceApprovals(ctx, q, appID)
	if err != nil {
		return err
	}
	for _, slot := range sortedKeys(def.DataSources) {
		if r, ok := findDataSourceApproval(rows, slot, def.dataSourceContract(slot).Hash()); !ok || r.status != ApprovalApproved {
			return dataSourceUnapproved(slot)
		}
	}
	return nil
}

func (s *Service) dataSourceStatus(ctx context.Context, sp access.Space, a Actor, appID int64) (DataSourceStatus, error) {
	if err := may(sp, access.Read); err != nil {
		return DataSourceStatus{}, err
	}
	acc, err := s.appAccess(ctx, a, sp.ID, appID)
	if err != nil {
		return DataSourceStatus{}, err
	}
	if !acc.Has(access.CapDesign) && !acc.Has(access.CapPublish) {
		return DataSourceStatus{}, forbidden(string(access.CapDesign))
	}
	var cur App
	var rows []sourceRow
	if err := s.snapshot(ctx, func(q queryer) (err error) {
		if cur, err = app(ctx, q, sp.ID, appID); err != nil {
			return err
		}
		rows, err = dataSourceApprovals(ctx, q, appID)
		return err
	}); err != nil {
		return DataSourceStatus{}, err
	}
	out := DataSourceStatus{Slots: []DataSourceSlotStatus{}}
	for _, slot := range sortedKeys(cur.draftDef.DataSources) {
		c := cur.draftDef.dataSourceContract(slot)
		st := DataSourceSlotStatus{Slot: slot, Kind: c.Kind, Contract: c, ContractHash: c.Hash(), Status: ApprovalNone}
		if r, ok := findDataSourceApproval(rows, slot, st.ContractHash); ok {
			st.Status, st.ApprovalID = r.status, &r.id
		}
		out.Slots = append(out.Slots, st)
	}
	return out, nil
}

func (s *Service) requestDataSource(ctx context.Context, sp access.Space, a Actor, appID int64, slot, hash string) (DataSourceApproval, error) {
	if err := may(sp, access.Write); err != nil {
		return DataSourceApproval{}, err
	}
	var out DataSourceApproval
	err := s.write(ctx, func(tx *sql.Tx) error {
		acc, err := s.recheckApp(ctx, a, sp.ID, appID)
		if err != nil {
			return err
		}
		if err := need(acc, access.CapPublish); err != nil {
			return err
		}
		cur, err := app(ctx, tx, sp.ID, appID)
		if err != nil {
			return err
		}
		if _, ok := cur.draftDef.DataSources[slot]; !ok {
			return apierr.New(http.StatusNotFound, apierr.CodeAppDataSourceSlotUnknown,
				fmt.Sprintf("the draft declares no data source %q", slot), apierr.Params{"slot": slot})
		}
		contract := cur.draftDef.dataSourceContract(slot)
		if contract.Hash() != hash {
			return apierr.New(http.StatusConflict, apierr.CodeAppDataSourceContractChanged,
				"the draft's contract changed since you read it: read it again and ask for that one")
		}
		raw, err := json.Marshal(contract)
		if err != nil {
			return err
		}
		rows, err := dataSourceApprovals(ctx, tx, appID)
		if err != nil {
			return err
		}
		existing, found := findDataSourceApproval(rows, slot, hash)
		id := existing.id
		if !found || (existing.status != ApprovalRequested && existing.status != ApprovalApproved) {
			if err := tx.QueryRowContext(ctx, `INSERT INTO app_data_source_approvals (app_id, slot, kind, contract_hash, contract, status, requested_by, requested_at)
				VALUES (?, ?, ?, ?, ?, 'requested', ?, ?) RETURNING id`, appID, slot, contract.Kind, hash, string(raw), a.UserID, now()).Scan(&id); err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, audit.Event{Workspace: sp.WorkspaceID, Actor: a.UserID, Action: audit.AppDataSourceRequested,
				TargetType: audit.TargetAppDataSource, Target: id,
				Details: audit.Details{"kind": contract.Kind, "app_id": appID}}); err != nil {
				return err
			}
		}
		out, err = dataSourceView(ctx, tx, sp.WorkspaceID, id)
		return err
	})
	return out, err
}

func dataSourceView(ctx context.Context, q queryer, workspace, id int64) (DataSourceApproval, error) {
	r, err := scanSource(q.QueryRowContext(ctx, sourceSelect+` WHERE a.id = ? AND sp.workspace_id = ?`, id, workspace))
	if errors.Is(err, sql.ErrNoRows) {
		return DataSourceApproval{}, errApprovalNotFound
	}
	if err != nil {
		return DataSourceApproval{}, err
	}
	return r.view(), nil
}

func (s *Service) listDataSourceApprovals(ctx context.Context, workspace int64, status string) (DataSourceApprovalList, error) {
	if status == "" {
		status = ApprovalRequested
	}
	if status != "all" && !slices.Contains(approvalStates, status) {
		return DataSourceApprovalList{}, invalidf("status is one of %v, or all", approvalStates)
	}
	out := DataSourceApprovalList{Approvals: []DataSourceApproval{}}
	err := s.snapshot(ctx, func(q queryer) error {
		rows, err := q.QueryContext(ctx, sourceSelect+` WHERE sp.workspace_id = ? AND (? = 'all' OR a.status = ?) ORDER BY a.id DESC LIMIT ?`,
			workspace, status, status, maxApprovals)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanSource(rows)
			if err != nil {
				return err
			}
			out.Approvals = append(out.Approvals, r.view())
		}
		return rows.Err()
	})
	return out, err
}

type sourceDecision struct{ from, to, action string }

var sourceDecisions = map[string]sourceDecision{
	"approve": {ApprovalRequested, ApprovalApproved, audit.AppDataSourceApproved},
	"reject":  {ApprovalRequested, ApprovalRejected, audit.AppDataSourceRejected},
	"revoke":  {ApprovalApproved, ApprovalRevoked, audit.AppDataSourceRevoked},
}

func (s *Service) decideDataSource(ctx context.Context, workspace, admin, id int64, verb string) (DataSourceApproval, error) {
	d := sourceDecisions[verb]
	var out DataSourceApproval
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := ensureDecider(ctx, tx, workspace, admin); err != nil {
			return err
		}
		r, err := scanSource(tx.QueryRowContext(ctx, sourceSelect+` WHERE a.id = ? AND sp.workspace_id = ?`, id, workspace))
		if errors.Is(err, sql.ErrNoRows) {
			return errApprovalNotFound
		}
		if err != nil {
			return err
		}
		if r.status != d.from {
			if d.to == ApprovalRevoked {
				return apierr.New(http.StatusConflict, apierr.CodeAppIntegrationNotApproved, "only an approval can be revoked")
			}
			return notPending()
		}
		if _, err := tx.ExecContext(ctx, `UPDATE app_data_source_approvals SET status = ?, decided_by = ?, decided_at = ? WHERE id = ?`,
			d.to, admin, now(), id); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Event{Workspace: workspace, Actor: admin, Action: d.action,
			TargetType: audit.TargetAppDataSource, Target: id, Details: audit.Details{"kind": r.kind, "app_id": r.appID}}); err != nil {
			return err
		}
		out, err = dataSourceView(ctx, tx, workspace, id)
		return err
	})
	return out, err
}

func (s *Service) DataSourceRoutes(mux *http.ServeMux, a *auth.Service) {
	const base = "/api/workspaces/{workspace}/data-source-approvals"
	mux.Handle("GET "+base, a.RequireAdmin(http.HandlerFunc(s.handleDataSourceApprovals)))
	for verb := range sourceDecisions {
		mux.Handle("POST "+base+"/{approval}/"+verb, a.RequireAdmin(s.handleSourceDecision(verb)))
	}
}

func (s *Service) handleDataSourceApprovals(w http.ResponseWriter, r *http.Request) {
	workspace, _ := approvalCaller(r)
	out, err := s.listDataSourceApprovals(r.Context(), workspace, r.URL.Query().Get("status"))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) handleSourceDecision(verb string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := approvalID(w, r)
		if !ok {
			return
		}
		var body struct{}
		if !readJSON(w, r, &body) {
			return
		}
		workspace, admin := approvalCaller(r)
		out, err := s.decideDataSource(r.Context(), workspace, admin, id, verb)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func (s *Service) appHandler(do func(req request, r *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, ok := parse(w, r, true)
		if !ok {
			return
		}
		out, err := do(req, r)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func (s *Service) handleRequestDataSource(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	var body struct {
		ContractHash string `json:"contract_hash"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	out, err := s.requestDataSource(r.Context(), req.space, req.actor, req.app, r.PathValue("slot"), body.ContractHash)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
