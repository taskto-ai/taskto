package integrations

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/currency"
)

type PaymentLinkOutcome struct {
	EffectID   int64  `json:"effect_id"`
	AppID      int64  `json:"app_id"`
	SpaceID    int64  `json:"space_id"`
	RecordID   int64  `json:"record_id"`
	Amount     string `json:"amount"`
	Currency   string `json:"currency"`
	Since      string `json:"since"`
	Waiting    bool   `json:"waiting"`
	CanConfirm bool   `json:"can_confirm"`
	ObjectID   string `json:"object_id"`
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
		SELECT r.effect_id, r.app_id, a.space_id, r.record_id, r.amount_minor, r.currency, coalesce(k.unknown_since, k.created_at),
			coalesce(e.state, ''), coalesce(e.error_code, ''), `+confirmable+`,
			coalesce(k.object_id, '')
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
	for rows.Next() {
		var o PaymentLinkOutcome
		var minor int64
		var state, code string
		var clean bool
		if err := rows.Scan(&o.EffectID, &o.AppID, &o.SpaceID, &o.RecordID, &minor, &o.Currency, &o.Since, &state, &code, &clean, &o.ObjectID); err != nil {
			return nil, err
		}
		if !spaces[o.SpaceID] || len(out) >= maxOutcomesListed {
			continue
		}
		o.Amount = currency.Format(minor, o.Currency)
		o.CanConfirm = clean && state == "failed" && code == string(apierr.CodeIntegrationOutcomeUnknown)
		o.Waiting = state == "pending" || state == "running"
		out = append(out, o)
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

func (s *Service) ConfirmNoPaymentLink(ctx context.Context, workspace, user, integration, effect int64, spaces map[int64]bool) error {
	return s.store.write(ctx, func(tx *sql.Tx) error {
		rec, err := s.store.get(ctx, tx, workspace, integration)
		if err != nil {
			return err
		}
		var refID, keySeq, appID, spaceID int64
		var object, refState, state, code string
		var unknown, clean bool
		err = tx.QueryRowContext(ctx, `
			SELECT r.id, r.key_seq, r.app_id, coalesce(r.provider_object_id, ''), r.state, a.space_id,
				coalesce(k.outcome IS NULL AND k.sends > k.clean_sends, 0), coalesce(e.state, ''), coalesce(e.error_code, ''), `+confirmable+`
			FROM integration_refs r
			JOIN apps a ON a.id = r.app_id
			LEFT JOIN integration_ref_attempts k ON k.ref_id = r.id AND k.seq = r.key_seq
			LEFT JOIN app_effects e ON e.id = r.effect_id
			WHERE r.effect_id = ? AND r.integration_id = ?`, effect, integration).Scan(&refID, &keySeq, &appID, &object, &refState, &spaceID, &unknown, &state, &code, &clean)
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
		if !clean {
			return apierr.New(http.StatusConflict, apierr.CodeIntegrationLinkNotConfirmable,
				"an object of this payment link was seen at the provider, or a key of it was already resolved: no new key is allowed")
		}
		res, err := tx.ExecContext(ctx, `UPDATE integration_ref_attempts SET outcome = 'abandoned'
			WHERE ref_id = ? AND seq = ? AND outcome IS NULL AND sends > clean_sends AND object_id IS NULL
				AND NOT EXISTS (SELECT 1 FROM integration_ref_attempts o WHERE o.ref_id = ? AND (o.object_id IS NOT NULL OR o.outcome IS NOT NULL))`,
			refID, keySeq, refID)
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
	var req struct{}
	if !readJSON(w, r, &req) {
		return
	}
	ids, ok := s.manageable(w, r)
	if !ok {
		return
	}
	if err := s.ConfirmNoPaymentLink(r.Context(), workspaceOf(r), actorOf(r), id, effect, visibleTo(ids)); err != nil {
		fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
