package apps

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/auth"
)

const (
	ApprovalRequested = "requested"
	ApprovalApproved  = "approved"
	ApprovalRejected  = "rejected"
	ApprovalRevoked   = "revoked"
	ApprovalNone      = "none"
)

var approvalStates = []string{ApprovalRequested, ApprovalApproved, ApprovalRejected, ApprovalRevoked}

type BoundIntegration struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Usable      bool   `json:"usable"`
}

type SlotLive struct {
	ContractHash string `json:"contract_hash"`
	Status       string `json:"status"`
	Usable       bool   `json:"usable"`
}

type SlotStatus struct {
	Slot         string            `json:"slot"`
	Kind         string            `json:"kind"`
	Contract     Contract          `json:"contract"`
	ContractHash string            `json:"contract_hash"`
	Status       string            `json:"status"`
	ApprovalID   *int64            `json:"approval_id"`
	Integration  *BoundIntegration `json:"integration"`
	Live         *SlotLive         `json:"live"`
}

type IntegrationStatus struct {
	Slots []SlotStatus `json:"slots" apitypes:"nonnull"`
}

type IntegrationApproval struct {
	ID           int64             `json:"id"`
	AppID        int64             `json:"app_id"`
	AppName      string            `json:"app_name"`
	SpaceID      int64             `json:"space_id"`
	SpaceName    string            `json:"space_name"`
	SpaceKind    string            `json:"space_kind"`
	Slot         string            `json:"slot"`
	Kind         string            `json:"kind"`
	Contract     Contract          `json:"contract"`
	ContractHash string            `json:"contract_hash"`
	Status       string            `json:"status"`
	Live         bool              `json:"live"`
	Integration  *BoundIntegration `json:"integration"`
	SpaceGrant   string            `json:"space_grant"`
	RequestedBy  *Author           `json:"requested_by"`
	RequestedAt  string            `json:"requested_at"`
	DecidedBy    *Author           `json:"decided_by"`
	DecidedAt    *string           `json:"decided_at"`
}

type ApprovalList struct {
	Approvals  []IntegrationApproval `json:"approvals" apitypes:"nonnull"`
	NextCursor *string               `json:"next_cursor"`
}

type Decision struct {
	IntegrationID int64 `json:"integration_id"`
	AllowSpace    bool  `json:"allow_space"`
}

var (
	ErrIntegrationUnapproved = errors.New("the app's use of this integration isn't approved")

	errApprovalNotFound = apierr.New(http.StatusNotFound, apierr.CodeAppIntegrationNotFound, "approval not found")
)

func unapproved(slot string) error {
	return apierr.New(http.StatusConflict, apierr.CodeAppIntegrationUnapproved,
		fmt.Sprintf("the integration %q has no approval for this contract: ask an admin to approve it before publishing", slot),
		apierr.Params{"slot": slot})
}

type approvalRow struct {
	id, appID, spaceID, workspaceID    int64
	appName, spaceName, spaceKind      string
	slot, kind, hash, contract, status string
	integrationID                      sql.NullInt64
	integrationName, integrationLabel  sql.NullString
	usable                             bool
	requestedBy, decidedBy             sql.NullInt64
	requestedByName, decidedByName     string
	requestedAt                        string
	decidedAt                          sql.NullString
	live                               bool
	spaceGrant                         string
}

const approvalSelect = `
	SELECT a.id, a.app_id, ap.space_id, sp.workspace_id, ap.name, sp.name, sp.kind,
		a.slot, a.kind, a.contract_hash, a.contract, a.status,
		a.integration_id, i.name, i.display_name,
		coalesce(i.disabled_at IS NULL AND EXISTS (SELECT 1 FROM integration_spaces x
			WHERE x.integration_id = i.id AND x.space_id = ap.space_id), 0),
		coalesce((SELECT CASE x.from_approval WHEN 1 THEN 'approval' ELSE 'settings' END FROM integration_spaces x
			WHERE x.integration_id = a.integration_id AND x.space_id = ap.space_id), 'none'),
		a.requested_by, coalesce(ru.name, ''), a.requested_at, a.decided_by, coalesce(du.name, ''), a.decided_at
	FROM app_integration_approvals a
	JOIN apps ap ON ap.id = a.app_id
	JOIN spaces sp ON sp.id = ap.space_id
	LEFT JOIN integrations i ON i.id = a.integration_id
	LEFT JOIN users ru ON ru.id = a.requested_by
	LEFT JOIN users du ON du.id = a.decided_by`

func scanApproval(row scanner) (approvalRow, error) {
	var r approvalRow
	err := row.Scan(&r.id, &r.appID, &r.spaceID, &r.workspaceID, &r.appName, &r.spaceName, &r.spaceKind,
		&r.slot, &r.kind, &r.hash, &r.contract, &r.status,
		&r.integrationID, &r.integrationName, &r.integrationLabel, &r.usable, &r.spaceGrant,
		&r.requestedBy, &r.requestedByName, &r.requestedAt, &r.decidedBy, &r.decidedByName, &r.decidedAt)
	return r, err
}

func (r approvalRow) view() IntegrationApproval {
	v := IntegrationApproval{ID: r.id, AppID: r.appID, AppName: r.appName, SpaceID: r.spaceID, SpaceName: r.spaceName, SpaceKind: r.spaceKind,
		Slot: r.slot, Kind: r.kind, ContractHash: r.hash, Status: r.status, Live: r.live, Integration: r.bound(), SpaceGrant: r.spaceGrant,
		RequestedBy: author(r.requestedBy, r.requestedByName, false, sql.NullString{}), RequestedAt: r.requestedAt,
		DecidedBy: author(r.decidedBy, r.decidedByName, false, sql.NullString{})}
	if r.decidedAt.Valid {
		v.DecidedAt = &r.decidedAt.String
	}
	if err := json.Unmarshal([]byte(r.contract), &v.Contract); err != nil {
		v.Contract = Contract{}
	}
	v.Contract.normalize()
	return v
}

func (r approvalRow) bound() *BoundIntegration {
	if !r.integrationID.Valid {
		return nil
	}
	return &BoundIntegration{ID: r.integrationID.Int64, Name: r.integrationName.String, DisplayName: r.integrationLabel.String, Usable: r.usable}
}

func (c *Contract) normalize() {
	if c.Operations == nil {
		c.Operations = []string{}
	}
	if c.Sends == nil {
		c.Sends = []string{}
	}
	if c.Currencies == nil {
		c.Currencies = []string{}
	}
	if c.Events == nil {
		c.Events = []string{}
	}
	if c.Writes == nil {
		c.Writes = []ContractWrite{}
	}
	if c.OnlyIf == nil {
		c.OnlyIf = []ContractRule{}
	}
	if c.Links == nil {
		c.Links = []ContractLink{}
	}
	if c.Handlers == nil {
		c.Handlers = []ContractHandler{}
	}
	if c.Fields == nil {
		c.Fields = []ContractField{}
	}
}

func approvalsOf(ctx context.Context, q queryer, appID int64) ([]approvalRow, error) {
	rows, err := q.QueryContext(ctx, approvalSelect+` WHERE a.app_id = ? ORDER BY a.id DESC`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []approvalRow
	for rows.Next() {
		r, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func findApproval(rows []approvalRow, slot, hash string) (approvalRow, bool) {
	var latest approvalRow
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

func (s *Service) checkApprovals(ctx context.Context, q queryer, appID int64, def Definition) error {
	if len(def.Integrations) == 0 {
		return nil
	}
	rows, err := approvalsOf(ctx, q, appID)
	if err != nil {
		return err
	}
	for _, slot := range sortedKeys(def.Integrations) {
		r, ok := findApproval(rows, slot, def.contract(slot).Hash())
		if !ok || r.status != ApprovalApproved || !r.usable {
			return unapproved(slot)
		}
	}
	return nil
}

type Binding struct {
	IntegrationID int64
	Kind          string
	ContractHash  string
	Contract      Contract
}

func (s *Service) Binding(ctx context.Context, appID int64, slot string) (out Binding, err error) {
	err = s.snapshot(ctx, func(q queryer) error {
		out, err = liveBinding(ctx, q, appID, slot)
		return err
	})
	return out, err
}

func (s *Service) snapshot(ctx context.Context, fn func(q queryer) error) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return fn(tx)
}

func liveBinding(ctx context.Context, q queryer, appID int64, slot string) (Binding, error) {
	var archived sql.NullString
	var live sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT archived_at, live_release FROM apps WHERE id = ?`, appID).Scan(&archived, &live); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Binding{}, ErrNotFound
		}
		return Binding{}, err
	}
	if archived.Valid {
		return Binding{}, ErrOffline
	}
	if !live.Valid {
		return Binding{}, ErrNotPublished
	}
	rel, err := loadRelease(ctx, q, appID, live.Int64, true)
	if err != nil {
		return Binding{}, err
	}
	if _, ok := rel.Definition.Integrations[slot]; !ok {
		return Binding{}, ErrIntegrationUnapproved
	}
	contract := rel.Definition.contract(slot)
	rows, err := approvalsOf(ctx, q, appID)
	if err != nil {
		return Binding{}, err
	}
	r, ok := findApproval(rows, slot, contract.Hash())
	if !ok || r.status != ApprovalApproved || !r.usable {
		return Binding{}, ErrIntegrationUnapproved
	}
	return Binding{IntegrationID: r.integrationID.Int64, Kind: contract.Kind, ContractHash: r.hash, Contract: contract}, nil
}

func (s *Service) integrationStatus(ctx context.Context, sp access.Space, a Actor, appID int64) (IntegrationStatus, error) {
	if err := may(sp, access.Read); err != nil {
		return IntegrationStatus{}, err
	}
	acc, err := s.appAccess(ctx, a, sp.ID, appID)
	if err != nil {
		return IntegrationStatus{}, err
	}
	if !acc.Has(access.CapDesign) && !acc.Has(access.CapPublish) {
		return IntegrationStatus{}, forbidden(string(access.CapDesign))
	}
	var cur App
	var rows []approvalRow
	if err := s.snapshot(ctx, func(q queryer) (err error) {
		if cur, err = app(ctx, q, sp.ID, appID); err != nil {
			return err
		}
		rows, err = approvalsOf(ctx, q, appID)
		return err
	}); err != nil {
		return IntegrationStatus{}, err
	}
	out := IntegrationStatus{Slots: []SlotStatus{}}
	for _, slot := range sortedKeys(cur.draftDef.Integrations) {
		c := cur.draftDef.contract(slot)
		st := SlotStatus{Slot: slot, Kind: c.Kind, Contract: c, ContractHash: c.Hash(), Status: ApprovalNone}
		if r, ok := findApproval(rows, slot, st.ContractHash); ok {
			st.Status, st.ApprovalID, st.Integration = r.status, &r.id, r.bound()
		}
		if cur.liveDef != nil {
			if _, declared := cur.liveDef.Integrations[slot]; declared {
				live := SlotLive{ContractHash: cur.liveDef.contract(slot).Hash(), Status: ApprovalNone}
				if r, ok := findApproval(rows, slot, live.ContractHash); ok {
					live.Status, live.Usable = r.status, r.status == ApprovalApproved && r.usable
				}
				st.Live = &live
			}
		}
		out.Slots = append(out.Slots, st)
	}
	return out, nil
}

func (s *Service) requestApproval(ctx context.Context, sp access.Space, a Actor, appID int64, slot, hash string) (IntegrationApproval, error) {
	if err := may(sp, access.Write); err != nil {
		return IntegrationApproval{}, err
	}
	var out IntegrationApproval
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
		if _, ok := cur.draftDef.Integrations[slot]; !ok {
			return apierr.New(http.StatusNotFound, apierr.CodeAppIntegrationSlotUnknown,
				fmt.Sprintf("the draft declares no integration %q", slot), apierr.Params{"slot": slot})
		}
		contract := cur.draftDef.contract(slot)
		if contract.Hash() != hash {
			return apierr.New(http.StatusConflict, apierr.CodeAppIntegrationContractChanged,
				"the draft's contract changed since you read it: read it again and ask for that one")
		}
		raw, err := json.Marshal(contract)
		if err != nil {
			return err
		}
		rows, err := approvalsOf(ctx, tx, appID)
		if err != nil {
			return err
		}
		existing, found := findApproval(rows, slot, hash)
		var id int64
		if found && (existing.status == ApprovalRequested || existing.status == ApprovalApproved) {
			id = existing.id
		} else {
			if err := tx.QueryRowContext(ctx, `INSERT INTO app_integration_approvals (app_id, slot, kind, contract_hash, contract, status, requested_by, requested_at)
				VALUES (?, ?, ?, ?, ?, 'requested', ?, ?) RETURNING id`, appID, slot, contract.Kind, hash, string(raw), a.UserID, now()).Scan(&id); err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, audit.Event{Workspace: sp.WorkspaceID, Actor: a.UserID, Action: audit.AppIntegrationRequested,
				TargetType: audit.TargetAppIntegration, Target: id,
				Details: audit.Details{"kind": contract.Kind, "app_id": appID}}); err != nil {
				return err
			}
		}
		out, err = approvalView(ctx, tx, sp.WorkspaceID, id)
		return err
	})
	return out, err
}

func approvalView(ctx context.Context, q queryer, workspace, id int64) (IntegrationApproval, error) {
	r, err := scanApproval(q.QueryRowContext(ctx, approvalSelect+` WHERE a.id = ? AND sp.workspace_id = ?`, id, workspace))
	if errors.Is(err, sql.ErrNoRows) {
		return IntegrationApproval{}, errApprovalNotFound
	}
	if err != nil {
		return IntegrationApproval{}, err
	}
	r.live, err = isLive(ctx, q, r)
	if err != nil {
		return IntegrationApproval{}, err
	}
	return r.view(), nil
}

func isLive(ctx context.Context, q queryer, r approvalRow) (bool, error) {
	var number sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT live_release FROM apps WHERE id = ?`, r.appID).Scan(&number); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if !number.Valid {
		return false, nil
	}
	rel, err := loadRelease(ctx, q, r.appID, number.Int64, true)
	if err != nil {
		return false, err
	}
	if _, ok := rel.Definition.Integrations[r.slot]; !ok {
		return false, nil
	}
	return rel.Definition.contract(r.slot).Hash() == r.hash, nil
}

const maxApprovals = 200

func (s *Service) listApprovals(ctx context.Context, workspace int64, status, cursor string) (ApprovalList, error) {
	if status == "" {
		status = ApprovalRequested
	}
	if status != "all" && !slices.Contains(approvalStates, status) {
		return ApprovalList{}, invalidf("status is one of %v, or all", approvalStates)
	}
	before := int64(math.MaxInt64)
	if cursor != "" {
		n, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || n < 1 {
			return ApprovalList{}, invalidf("cursor is the next_cursor of a page")
		}
		before = n
	}
	var out ApprovalList
	err := s.snapshot(ctx, func(q queryer) error {
		rows, err := q.QueryContext(ctx, approvalSelect+` WHERE sp.workspace_id = ? AND (? = 'all' OR a.status = ?) AND a.id < ? ORDER BY a.id DESC LIMIT ?`,
			workspace, status, status, before, maxApprovals+1)
		if err != nil {
			return err
		}
		var all []approvalRow
		for rows.Next() {
			r, err := scanApproval(rows)
			if err != nil {
				_ = rows.Close()
				return err
			}
			all = append(all, r)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
		out = ApprovalList{Approvals: make([]IntegrationApproval, 0, len(all))}
		if len(all) > maxApprovals {
			all = all[:maxApprovals]
			next := strconv.FormatInt(all[len(all)-1].id, 10)
			out.NextCursor = &next
		}
		for _, r := range all {
			if r.live, err = isLive(ctx, q, r); err != nil {
				return err
			}
			out.Approvals = append(out.Approvals, r.view())
		}
		return nil
	})
	return out, err
}

func decide(ctx context.Context, tx *sql.Tx, workspace, id int64) (approvalRow, error) {
	r, err := scanApproval(tx.QueryRowContext(ctx, approvalSelect+` WHERE a.id = ? AND sp.workspace_id = ?`, id, workspace))
	if errors.Is(err, sql.ErrNoRows) {
		return approvalRow{}, errApprovalNotFound
	}
	return r, err
}

func notPending() error {
	return apierr.New(http.StatusConflict, apierr.CodeAppIntegrationNotPending, "this request was already decided")
}

func ensureDecider(ctx context.Context, tx *sql.Tx, workspace, admin int64) error {
	if err := auth.EnsureActorEnabled(ctx, tx); err != nil {
		return err
	}
	var isAdmin bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM memberships WHERE workspace_id = ? AND user_id = ? AND role = 'admin')`,
		workspace, admin).Scan(&isAdmin); err != nil {
		return err
	}
	if !isAdmin {
		return apierr.New(http.StatusForbidden, apierr.CodeAdminOnly, "only a workspace admin can do this")
	}
	return nil
}

func (s *Service) approve(ctx context.Context, workspace, admin, id int64, d Decision) (IntegrationApproval, error) {
	var out IntegrationApproval
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := ensureDecider(ctx, tx, workspace, admin); err != nil {
			return err
		}
		r, err := decide(ctx, tx, workspace, id)
		if err != nil {
			return err
		}
		if r.status != ApprovalRequested {
			return notPending()
		}
		var provider string
		var disabled bool
		var allowed bool
		err = tx.QueryRowContext(ctx, `SELECT i.provider, i.disabled_at IS NOT NULL,
				EXISTS (SELECT 1 FROM integration_spaces x WHERE x.integration_id = i.id AND x.space_id = ?)
			FROM integrations i WHERE i.id = ? AND i.workspace_id = ?`, r.spaceID, d.IntegrationID, workspace).Scan(&provider, &disabled, &allowed)
		if errors.Is(err, sql.ErrNoRows) {
			return apierr.New(http.StatusNotFound, apierr.CodeIntegrationNotFound, "integration not found")
		}
		if err != nil {
			return err
		}
		if s.platform.ProviderKinds == nil || !slices.Contains(s.platform.ProviderKinds(provider), r.kind) {
			return apierr.New(http.StatusUnprocessableEntity, apierr.CodeAppIntegrationKindMismatch,
				fmt.Sprintf("this integration can't be used for %s", r.kind), apierr.Params{"kind": r.kind})
		}
		if disabled {
			return apierr.New(http.StatusConflict, apierr.CodeAppIntegrationDisabled, "this integration is switched off")
		}
		spaceAllowed := false
		if !allowed {
			if !d.AllowSpace {
				return apierr.New(http.StatusConflict, apierr.CodeAppIntegrationSpaceNotAllowed, "this integration isn't allowed in the app's space")
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO integration_spaces (integration_id, space_id, allowed_by, allowed_at, from_approval) VALUES (?, ?, ?, ?, 1)`,
				d.IntegrationID, r.spaceID, admin, now()); err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, audit.Event{Workspace: workspace, Actor: admin, Action: audit.IntegrationSpacesChanged,
				TargetType: audit.TargetIntegration, Target: d.IntegrationID,
				Details: audit.Details{"provider": provider, "added": 1, "removed": 0}}); err != nil {
				return err
			}
			spaceAllowed = true
		}
		if _, err := tx.ExecContext(ctx, `UPDATE app_integration_approvals
			SET status = 'approved', integration_id = ?, decided_by = ?, decided_at = ? WHERE id = ?`, d.IntegrationID, admin, now(), id); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Event{Workspace: workspace, Actor: admin, Action: audit.AppIntegrationApproved,
			TargetType: audit.TargetAppIntegration, Target: id,
			Details: audit.Details{"kind": r.kind, "app_id": r.appID, "integration_id": d.IntegrationID, "provider": provider, "space_allowed": spaceAllowed}}); err != nil {
			return err
		}
		out, err = approvalView(ctx, tx, workspace, id)
		return err
	})
	return out, err
}

func (s *Service) reject(ctx context.Context, workspace, admin, id int64) (IntegrationApproval, error) {
	var out IntegrationApproval
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := ensureDecider(ctx, tx, workspace, admin); err != nil {
			return err
		}
		r, err := decide(ctx, tx, workspace, id)
		if err != nil {
			return err
		}
		if r.status != ApprovalRequested {
			return notPending()
		}
		if _, err := tx.ExecContext(ctx, `UPDATE app_integration_approvals SET status = 'rejected', decided_by = ?, decided_at = ? WHERE id = ?`,
			admin, now(), id); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Event{Workspace: workspace, Actor: admin, Action: audit.AppIntegrationRejected,
			TargetType: audit.TargetAppIntegration, Target: id, Details: audit.Details{"kind": r.kind, "app_id": r.appID}}); err != nil {
			return err
		}
		out, err = approvalView(ctx, tx, workspace, id)
		return err
	})
	return out, err
}

func (s *Service) revoke(ctx context.Context, workspace, admin, id int64) (IntegrationApproval, error) {
	var out IntegrationApproval
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := ensureDecider(ctx, tx, workspace, admin); err != nil {
			return err
		}
		r, err := decide(ctx, tx, workspace, id)
		if err != nil {
			return err
		}
		if r.status != ApprovalApproved {
			return apierr.New(http.StatusConflict, apierr.CodeAppIntegrationNotApproved, "only an approval can be revoked")
		}
		grant := func() (bool, error) {
			var ok bool
			err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM integration_spaces WHERE integration_id = ? AND space_id = ?)`,
				r.integrationID.Int64, r.spaceID).Scan(&ok)
			return ok, err
		}
		had, err := grant()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE app_integration_approvals
			SET status = 'revoked', integration_id = NULL, decided_by = ?, decided_at = ? WHERE id = ?`, admin, now(), id); err != nil {
			return err
		}
		if has, err := grant(); err != nil {
			return err
		} else if had && !has {
			if err := audit.Record(ctx, tx, audit.Event{Workspace: workspace, Actor: admin, Action: audit.IntegrationSpacesChanged,
				TargetType: audit.TargetIntegration, Target: r.integrationID.Int64,
				Details: audit.Details{"added": 0, "removed": 1}}); err != nil {
				return err
			}
		}
		if err := audit.Record(ctx, tx, audit.Event{Workspace: workspace, Actor: admin, Action: audit.AppIntegrationRevoked,
			TargetType: audit.TargetAppIntegration, Target: id,
			Details: audit.Details{"kind": r.kind, "app_id": r.appID, "integration_id": r.integrationID.Int64}}); err != nil {
			return err
		}
		out, err = approvalView(ctx, tx, workspace, id)
		return err
	})
	return out, err
}

func (s *Service) ApprovalRoutes(mux *http.ServeMux, a *auth.Service) {
	admin := func(h http.HandlerFunc) http.Handler { return a.RequireAdmin(h) }
	const base = "/api/workspaces/{workspace}/integration-approvals"
	mux.Handle("GET "+base, admin(s.handleApprovals))
	mux.Handle("POST "+base+"/{approval}/approve", admin(s.handleApprove))
	mux.Handle("POST "+base+"/{approval}/reject", admin(s.handleReject))
	mux.Handle("POST "+base+"/{approval}/revoke", admin(s.handleRevoke))
}

func approvalCaller(r *http.Request) (workspace, user int64) {
	m, _ := auth.MembershipFrom(r.Context())
	u, _ := auth.UserFrom(r.Context())
	return m.WorkspaceID, u.ID
}

func approvalID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("approval"), 10, 64)
	if err != nil || id < 1 {
		apierr.WriteError(w, errApprovalNotFound)
		return 0, false
	}
	return id, true
}

func (s *Service) handleApprovals(w http.ResponseWriter, r *http.Request) {
	workspace, _ := approvalCaller(r)
	q := r.URL.Query()
	out, err := s.listApprovals(r.Context(), workspace, q.Get("status"), q.Get("cursor"))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) handleApprove(w http.ResponseWriter, r *http.Request) {
	id, ok := approvalID(w, r)
	if !ok {
		return
	}
	var d Decision
	if !readJSON(w, r, &d) {
		return
	}
	if d.IntegrationID < 1 {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "integration_id is the integration to connect the slot to")
		return
	}
	workspace, admin := approvalCaller(r)
	out, err := s.approve(r.Context(), workspace, admin, id, d)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) handleReject(w http.ResponseWriter, r *http.Request) {
	s.decided(w, r, s.reject)
}

func (s *Service) handleRevoke(w http.ResponseWriter, r *http.Request) {
	s.decided(w, r, s.revoke)
}

func (s *Service) decided(w http.ResponseWriter, r *http.Request, do func(ctx context.Context, workspace, admin, id int64) (IntegrationApproval, error)) {
	id, ok := approvalID(w, r)
	if !ok {
		return
	}
	var body struct{}
	if !readJSON(w, r, &body) {
		return
	}
	workspace, admin := approvalCaller(r)
	out, err := do(r.Context(), workspace, admin, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) handleIntegrationStatus(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	out, err := s.integrationStatus(r.Context(), req.space, req.actor, req.app)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) handleRequestApproval(w http.ResponseWriter, r *http.Request) {
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
	out, err := s.requestApproval(r.Context(), req.space, req.actor, req.app, r.PathValue("slot"), body.ContractHash)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
