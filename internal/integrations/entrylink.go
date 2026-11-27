package integrations

import (
	"context"
	"database/sql"
	"errors"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/paylink"
)

// EntryLink is the link the entry's ref saved, read through q, with no call to anyone (paylink.Opener):
// what an entry that wrote it to its record and stopped before it was noted done finds again.
func (s *Service) EntryLink(ctx context.Context, q paylink.Querier, effectID int64) (paylink.Link, bool, error) {
	var l paylink.Link
	err := q.QueryRowContext(ctx, `SELECT provider_object_id, url, amount_minor, currency FROM integration_refs
		WHERE effect_id = ? AND provider_object_id IS NOT NULL AND coalesce(url, '') != ''`, effectID).
		Scan(&l.ObjectID, &l.URL, &l.AmountMinor, &l.Currency)
	if errors.Is(err, sql.ErrNoRows) {
		return paylink.Link{}, false, nil
	}
	return l, err == nil, err
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
