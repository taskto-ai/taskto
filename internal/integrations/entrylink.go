package integrations

import (
	"context"
	"database/sql"
	"errors"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/paylink"
)

func (s *Service) EntryLink(ctx context.Context, q paylink.Querier, effectID int64) (paylink.Link, bool, error) {
	var l paylink.Link
	var pending bool
	err := q.QueryRowContext(ctx, `SELECT coalesce(r.provider_object_id, ''), coalesce(r.url, ''), r.amount_minor, r.currency,
			r.state IN ('paid', 'refunded') OR EXISTS (SELECT 1 FROM integration_ref_attempts k WHERE k.ref_id = r.id
				AND ((k.outcome IS NULL AND k.sends > k.clean_sends) OR k.object_id IS NOT NULL OR k.outcome = 'settled'))
		FROM integration_refs r WHERE r.effect_id = ? OR r.slot_effect_id = ?
		ORDER BY r.effect_id = ? DESC, r.id DESC LIMIT 1`, effectID, effectID, effectID).
		Scan(&l.ObjectID, &l.URL, &l.AmountMinor, &l.Currency, &pending)
	if errors.Is(err, sql.ErrNoRows) {
		return paylink.Link{}, false, nil
	}
	if err != nil {
		return paylink.Link{}, false, err
	}
	if l.ObjectID == "" || l.URL == "" {
		return paylink.Link{}, l.ObjectID != "" || pending, nil
	}
	return l, false, nil
}

func (s *Service) LinkCurrent(ctx context.Context, q paylink.Querier, integrationID int64, objectID string) error {
	var made, now string
	err := q.QueryRowContext(ctx, `SELECT r.account_mode, i.mode FROM integration_refs r JOIN integrations i ON i.id = r.integration_id
		WHERE r.integration_id = ? AND r.provider_object_id = ?`, integrationID, objectID).Scan(&made, &now)
	if errors.Is(err, sql.ErrNoRows) {
		return unavailable()
	}
	if err != nil {
		return err
	}
	if made != "" && now != "" && now != made {
		return &paylink.Error{Code: apierr.CodeIntegrationAccountChanged}
	}
	return nil
}
