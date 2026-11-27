package integrations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/currency"
)

type PaymentLinkOutcome struct {
	EffectID     int64                `json:"effect_id"`
	AppID        int64                `json:"app_id"`
	SpaceID      int64                `json:"space_id"`
	RecordID     int64                `json:"record_id"`
	Amount       string               `json:"amount"`
	Currency     string               `json:"currency"`
	Since        string               `json:"since"`
	Waiting      bool                 `json:"waiting"`
	CanConfirm   bool                 `json:"can_confirm"`
	ObjectID     string               `json:"object_id"`
	Code         string               `json:"code,omitempty"`
	Params       apierr.Params        `json:"params,omitempty"`
	AttemptSeq   int                  `json:"attempt_seq"`
	AttemptSends int                  `json:"attempt_sends"`
	Attempts     []PaymentLinkAttempt `json:"attempts" apitypes:"nonnull"`
}

const (
	AttemptCheckoutSession = "checkout_session"
	AttemptPaymentLink     = "payment_link"
)

type PaymentLinkAttempt struct {
	Reference      string `json:"reference"`
	Kind           string `json:"kind"`
	ObjectID       string `json:"object_id"`
	ObjectVerified bool   `json:"object_verified"`
	AccountID      string `json:"account_id"`
	Mode           string `json:"mode"`
	FirstSentAt    string `json:"first_sent_at"`
}

type PaymentLinkOutcomes struct {
	PaymentLinks []PaymentLinkOutcome `json:"payment_links" apitypes:"nonnull"`
}

const maxOutcomesListed = 200

const confirmable = `NOT EXISTS (SELECT 1 FROM integration_ref_attempts o
	WHERE o.ref_id = r.id AND (o.object_id IS NOT NULL OR o.outcome IS NOT NULL))`

func (s *Service) unresolved(ctx context.Context, workspace, integration int64, spaces map[int64]bool) ([]PaymentLinkOutcome, error) {
	if _, err := s.store.getSnapshot(ctx, workspace, integration); err != nil {
		return nil, err
	}
	rows, err := s.store.db.QueryContext(ctx, `
		SELECT r.id, r.success_url, r.account_id, r.account_mode, r.effect_id, r.app_id, a.space_id, r.record_id, r.amount_minor, r.currency, coalesce(k.unknown_since, k.created_at),
			coalesce(e.state, ''), coalesce(e.error_code, ''), e.error_params, `+confirmable+`,
			coalesce(k.object_id, ''), k.seq, k.sends
		FROM integration_refs r
		JOIN integration_ref_attempts k ON k.ref_id = r.id AND k.seq = r.key_seq
		JOIN apps a ON a.id = r.app_id
		LEFT JOIN app_effects e ON e.id = r.effect_id
		WHERE r.integration_id = ? AND r.provider_object_id IS NULL AND k.outcome IS NULL AND k.sends > k.clean_sends AND r.effect_id IS NOT NULL
			AND r.state NOT IN ('paid', 'refunded')
		ORDER BY r.id`, integration)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PaymentLinkOutcome{}
	var refs []refView
	for rows.Next() {
		var o PaymentLinkOutcome
		var v refView
		var minor int64
		var state, code string
		var params sql.NullString
		var clean bool
		if err := rows.Scan(&v.id, &v.successURL, &v.account, &v.mode, &o.EffectID, &o.AppID, &o.SpaceID, &o.RecordID, &minor, &o.Currency, &o.Since, &state, &code, &params, &clean, &o.ObjectID, &o.AttemptSeq, &o.AttemptSends); err != nil {
			return nil, err
		}
		if !spaces[o.SpaceID] || len(out) >= maxOutcomesListed {
			continue
		}
		o.Amount = currency.Format(minor, o.Currency)
		o.CanConfirm = clean && state == "failed" && code == string(apierr.CodeIntegrationOutcomeUnknown)
		o.Waiting = state == "pending" || state == "running"
		if !o.Waiting && !o.CanConfirm {
			o.Code = code
			if params.Valid && params.String != "" {
				_ = json.Unmarshal([]byte(params.String), &o.Params)
			}
		}
		out = append(out, o)
		refs = append(refs, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Attempts, err = s.attempts(ctx, refs[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

type refView struct {
	id         int64
	successURL string
	account    string
	mode       string
}

func (s *Service) attempts(ctx context.Context, r refView) ([]PaymentLinkAttempt, error) {
	rows, err := s.store.db.QueryContext(ctx, `
		SELECT token, coalesce(object_id, ''), object_verified, coalesce(first_sent_at, '')
		FROM integration_ref_attempts WHERE ref_id = ? ORDER BY seq`, r.id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	kind := AttemptPaymentLink
	if r.successURL != "" {
		kind = AttemptCheckoutSession
	}
	out := []PaymentLinkAttempt{}
	for rows.Next() {
		a := PaymentLinkAttempt{Kind: kind, AccountID: r.account, Mode: r.mode}
		var verified bool
		if err := rows.Scan(&a.Reference, &a.ObjectID, &verified, &a.FirstSentAt); err != nil {
			return nil, err
		}
		a.ObjectVerified = a.ObjectID != "" && verified
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Service) handleUnresolved(w http.ResponseWriter, r *http.Request) {
	id, ok := integrationID(w, r)
	if !ok {
		return
	}
	ids, ok := s.manageable(w, r)
	if !ok {
		return
	}
	list, err := s.unresolved(r.Context(), workspaceOf(r), id, visibleTo(ids))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, PaymentLinkOutcomes{PaymentLinks: list})
}

func (s *Service) ConfirmNoPaymentLink(ctx context.Context, workspace, user, integration, effect int64, checked CheckedAttempt, spaces map[int64]bool) error {
	return s.store.write(ctx, func(tx *sql.Tx) error {
		rec, err := s.store.get(ctx, tx, workspace, integration)
		if err != nil {
			return err
		}
		var refID, keySeq, appID, spaceID int64
		var object, refState, state, code string
		var unknown, clean bool
		var sends int
		err = tx.QueryRowContext(ctx, `
			SELECT r.id, r.key_seq, r.app_id, coalesce(r.provider_object_id, ''), r.state, a.space_id,
				coalesce(k.outcome IS NULL AND k.sends > k.clean_sends, 0), coalesce(e.state, ''), coalesce(e.error_code, ''), `+confirmable+`,
				coalesce(k.sends, 0)
			FROM integration_refs r
			JOIN apps a ON a.id = r.app_id
			LEFT JOIN integration_ref_attempts k ON k.ref_id = r.id AND k.seq = r.key_seq
			LEFT JOIN app_effects e ON e.id = r.effect_id
			WHERE r.effect_id = ? AND r.integration_id = ?`, effect, integration).Scan(&refID, &keySeq, &appID, &object, &refState, &spaceID, &unknown, &state, &code, &clean, &sends)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !spaces[spaceID]) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		nothing := apierr.New(http.StatusConflict, apierr.CodeIntegrationNothingToConfirm,
			"this payment link isn't waiting for a decision about an unknown outcome")
		if object != "" || !unknown || refState == "paid" || refState == "refunded" || state != "failed" || code != string(apierr.CodeIntegrationOutcomeUnknown) {
			return nothing
		}
		if checked.Seq == nil || checked.Sends == nil || int64(*checked.Seq) != keySeq || *checked.Sends != sends {
			return apierr.New(http.StatusConflict, apierr.CodeIntegrationLinkAttemptChanged,
				"this payment link was tried again since it was listed: check the provider again")
		}
		if !clean {
			return apierr.New(http.StatusConflict, apierr.CodeIntegrationLinkNotConfirmable,
				"an object of this payment link was seen at the provider, or a key of it was already resolved: no new key is allowed")
		}
		res, err := tx.ExecContext(ctx, `UPDATE integration_ref_attempts SET outcome = 'abandoned'
			WHERE ref_id = ? AND seq = ? AND sends = ? AND outcome IS NULL AND sends > clean_sends AND object_id IS NULL
				AND NOT EXISTS (SELECT 1 FROM integration_ref_attempts o WHERE o.ref_id = ? AND (o.object_id IS NOT NULL OR o.outcome IS NOT NULL))`,
			refID, keySeq, sends, refID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return nothing
		}
		if res, err = tx.ExecContext(ctx, `UPDATE integration_refs SET key_seq = key_seq + 1
			WHERE id = ? AND key_seq = ? AND provider_object_id IS NULL AND state NOT IN ('paid', 'refunded')`, refID, keySeq); err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return nothing
		}
		at := s.store.stamp(s.store.now())
		if res, err = tx.ExecContext(ctx, `UPDATE app_effects SET state = 'pending', attempts = 0, next_attempt_at = ?, lease_until = NULL,
				error_code = NULL, error_params = NULL, updated_at = ?
			WHERE id = ? AND state = 'failed'`, at, at, effect); err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return nothing
		}
		return audit.Record(ctx, tx, audit.Event{Workspace: workspace, Actor: user, Action: audit.IntegrationNewLinkAllowed,
			TargetType: audit.TargetIntegration, Target: integration,
			Details: audit.Details{"provider": rec.Provider, "app_id": appID, "effect_id": effect}})
	})
}

type CheckedAttempt struct {
	Seq   *int `json:"attempt_seq"`
	Sends *int `json:"attempt_sends"`
}

func (s *Service) handleConfirmNoLink(w http.ResponseWriter, r *http.Request) {
	id, ok := integrationID(w, r)
	if !ok {
		return
	}
	effect, err := strconv.ParseInt(r.PathValue("effect"), 10, 64)
	if err != nil || effect <= 0 {
		fail(w, r, errNotFound)
		return
	}
	var req CheckedAttempt
	if !readJSON(w, r, &req) {
		return
	}
	ids, ok := s.manageable(w, r)
	if !ok {
		return
	}
	if err := s.ConfirmNoPaymentLink(r.Context(), workspaceOf(r), actorOf(r), id, effect, req, visibleTo(ids)); err != nil {
		fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
