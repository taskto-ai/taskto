package integrations

import (
	"context"
	"database/sql"
	"errors"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/paylink"
)

// EntryLink is the link the entry's ref saved, read through q, with no call to anyone (paylink.Opener):
// what an entry that wrote it to its record and stopped before it was noted done finds again: the ref
// the entry made or took over (effect_id), or the record's earlier one it gave again (slot_effect_id,
// the newest entry that did). With none
// saved, pending says whether the ref may still hold an object: a key of it whose outcome is unknown,
// one with an object on record (whatever its outcome), one a lookup found settled (paid, before the
// payment's event came), or a payment that settled the ref first. Only a ref that has neither (or no
// ref) made nothing.
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

// LinkCurrent refuses the writing of a link to a record when the integration's key is of another mode
// now than the one the link was made in (paylink.Opener), read through q, the write's own transaction:
// an admin who replaced the key while the call was out would otherwise get a link of the old mode on the
// record. A link the integration has no ref for is not its to write.
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
