package integrations

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"

	"modernc.org/sqlite"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/currency"
)

// The create_payment_link effect's side of the integrations (apps/paymentlink.go asks for it).
//
// One outbox entry makes at most one payment link. The entry has one ref (integration_refs, unique
// by the entry's number) whose random token is the provider's idempotency key and the reference its
// payment echoes back, and the ref keeps the provider's answer. So an entry done again after a
// crash finds its ref, and
//
//   - if the ref has the provider's object, the provider isn't asked again: the link is the ref's;
//   - if it has none, the call may or may not have been made: it is made again under the same key,
//     which the provider answers with the first call's object when there was one (Stripe keeps a
//     key for at least a day; a retry after longer than that, with no object noted, is the one case
//     that can make a second payable link, and its payment is recorded as a duplicate).
//
// A provider that answered an error it keeps under the key is asked again under a key of its own
// (NewLink.KeySeq), as the kept failure would be answered again for ever.
//
// The ref is made before the call and the money is checked before the ref, so a refused amount
// leaves nothing behind. The key of the integration is opened here, for the call, and goes nowhere
// else: not into an error, a log line, the ref or the record.

// The server's own pages the payer returns to from the provider's hosted page (the web app's
// routes), when the server has a public URL: never a definition's, never a request's. Without one
// the payer stays on a confirmation page the provider hosts.
const (
	PaymentDonePath      = "/payment/done"
	PaymentCancelledPath = "/payment/cancelled"
)

func unavailable() error {
	return &apps.PaymentLinkError{Code: apierr.CodeIntegrationUnavailable}
}

// OpenPaymentLink returns the payment link for the request's entry, asking the provider for it only
// if the entry hasn't made one. It checks, as of now and against the workspace of the app, that the
// integration exists, is switched on and is allowed in the app's space, and then the admin's
// limits on the amount and the currency.
func (s *Service) OpenPaymentLink(ctx context.Context, req apps.PaymentLinkRequest) (apps.PaymentLink, error) {
	// The integration is looked for in the app's workspace only: another workspace's id is not found.
	rec, err := s.store.getSnapshot(ctx, req.WorkspaceID, req.IntegrationID)
	if err != nil {
		if errors.Is(err, errNotFound) {
			return apps.PaymentLink{}, unavailable()
		}
		return apps.PaymentLink{}, err
	}
	usable, err := s.store.usable(ctx, req.WorkspaceID, req.IntegrationID, req.SpaceID)
	if err != nil {
		return apps.PaymentLink{}, err
	}
	provider, _ := Lookup(rec.Provider).(PaymentProvider)
	if !usable || provider == nil || !providerDoes(rec.Provider, KindPayments) {
		return apps.PaymentLink{}, unavailable()
	}

	// What the entry already made.
	ref, found, err := s.refOf(ctx, req.EffectID)
	if err != nil {
		return apps.PaymentLink{}, err
	}
	if found && (ref.appID != req.AppID || ref.recordID != req.RecordID) {
		return apps.PaymentLink{}, unavailable() // an entry's number is its own: never another's
	}
	if found && ref.objectID != "" {
		return ref.link(), nil
	}

	cur := strings.ToUpper(strings.TrimSpace(req.Currency))
	minor, err := s.checkMoney(rec, req.Amount, cur)
	if err != nil {
		return apps.PaymentLink{}, err
	}
	// With no public URL there are no pages of ours: both are empty and the provider hosts the
	// payer's confirmation (a link is made all the same).
	success, cancel := s.pageURLs()
	creds, err := s.store.credentials(ctx, req.WorkspaceID, req.IntegrationID)
	if err != nil || creds.APIKey == "" {
		var unreadable unreadableError
		if err == nil || errors.As(err, &unreadable) {
			return apps.PaymentLink{}, unavailable() // no key, or one the server's key can't open
		}
		return apps.PaymentLink{}, err
	}

	ref, err = s.beginRef(ctx, req, minor, cur)
	if err != nil {
		return apps.PaymentLink{}, err
	}
	if ref.objectID != "" { // made by a call that finished while this one began
		return ref.link(), nil
	}

	link, err := provider.CreatePaymentLink(ctx, creds, s.Client(Lookup(rec.Provider), rec.Settings, s.clientOpts...), NewLink{
		Token: ref.token, KeySeq: ref.keySeq, AmountMinor: minor, Currency: cur, Description: req.Description,
		SuccessURL: success, CancelURL: cancel,
	})
	if err != nil {
		return apps.PaymentLink{}, s.callFailed(ctx, ref, rec, err)
	}
	if err := s.noteLink(ctx, ref, link); err != nil {
		return apps.PaymentLink{}, err
	}
	return apps.PaymentLink{URL: link.URL, ObjectID: link.ObjectID, AmountMinor: minor, Currency: cur}, nil
}

// providerDoes says whether the registry's provider id implements the kind.
func providerDoes(id, kind string) bool {
	p := Lookup(id)
	if p == nil {
		return false
	}
	for _, k := range p.Kinds() {
		if k == kind {
			return true
		}
	}
	return false
}

// pageURLs are the addresses the payer returns to, on the server's public URL: "" when it has none.
func (s *Service) pageURLs() (success, cancel string) {
	base := strings.TrimRight(s.publicURL, "/")
	if base == "" {
		return "", ""
	}
	return base + PaymentDonePath, base + PaymentCancelledPath
}

// checkMoney converts the amount of the request to the currency's smallest unit, exactly, and
// checks it against the integration's limits. It never rounds: an amount with more decimals than the
// currency has, one of zero or less, one that isn't a number, one above the admin's ceiling and a
// currency the admin didn't list are each refused with a code that says which.
func (s *Service) checkMoney(rec record, amount, cur string) (int64, error) {
	refused := func(code apierr.Code, params apierr.Params) error {
		return &apps.PaymentLinkError{Code: code, Params: params}
	}
	if !currency.Known(cur) || !containsString(rec.Currencies, cur) {
		return 0, refused(apierr.CodeIntegrationCurrencyRefused, apierr.Params{"currency": cur})
	}
	minor, err := currency.Minor(amount, cur)
	switch {
	case errors.Is(err, currency.ErrTooLarge):
		return 0, refused(apierr.CodeIntegrationAmountAboveMax, ceilingParams(rec.MaxAmountMinor, cur))
	case err != nil:
		return 0, refused(apierr.CodeIntegrationAmountRefused, nil)
	}
	if exp, _ := currency.Exponent(cur); WholeUnitsOnly(cur) && minor%pow10(exp) != 0 {
		return 0, refused(apierr.CodeIntegrationAmountRefused, nil)
	}
	if minor > rec.MaxAmountMinor {
		return 0, refused(apierr.CodeIntegrationAmountAboveMax, ceilingParams(rec.MaxAmountMinor, cur))
	}
	return minor, nil
}

func ceilingParams(maxMinor int64, cur string) apierr.Params {
	return apierr.Params{"max": currency.Format(maxMinor, cur), "currency": cur}
}

func pow10(n int) int64 {
	v := int64(1)
	for ; n > 0; n-- {
		v *= 10
	}
	return v
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ----- the refs -----

// ref is a row of integration_refs.
type ref struct {
	id, integrationID, appID, recordID int64
	token, slot                        string
	objectID, url                      string
	amountMinor                        int64
	currency                           string
	keySeq                             int
	state                              string
}

func (r ref) link() apps.PaymentLink {
	return apps.PaymentLink{URL: r.url, ObjectID: r.objectID, AmountMinor: r.amountMinor, Currency: r.currency}
}

const refColumns = `id, token, integration_id, app_id, record_id, slot, coalesce(provider_object_id, ''), coalesce(url, ''),
	amount_minor, currency, key_seq, state`

func scanRef(row interface{ Scan(...any) error }) (ref, error) {
	var r ref
	err := row.Scan(&r.id, &r.token, &r.integrationID, &r.appID, &r.recordID, &r.slot, &r.objectID, &r.url,
		&r.amountMinor, &r.currency, &r.keySeq, &r.state)
	return r, err
}

// refOf finds the ref of the outbox entry.
func (s *Service) refOf(ctx context.Context, effectID int64) (ref, bool, error) {
	r, err := scanRef(s.store.db.QueryRowContext(ctx, `SELECT `+refColumns+` FROM integration_refs WHERE effect_id = ?`, effectID))
	if errors.Is(err, sql.ErrNoRows) {
		return ref{}, false, nil
	}
	return r, err == nil, err
}

func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// beginRef makes the entry's ref, or finds it, ready for a call: in one transaction that holds the
// write lock, so two workers that took the same entry find one ref. A ref that has no object is
// updated to what this try asks (the entry's amount is fixed, but the integration it is bound to
// may be another since); a failed one starts anew with a key of its own.
func (s *Service) beginRef(ctx context.Context, req apps.PaymentLinkRequest, minor int64, cur string) (ref, error) {
	token, err := newToken()
	if err != nil {
		return ref{}, err
	}
	var out ref
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return ref{}, err
	}
	defer func() { _ = tx.Rollback() }()
	at := s.store.stamp(s.store.now())
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO integration_refs (token, integration_id, app_id, record_id, slot, effect_id, amount_minor, currency, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (effect_id) DO NOTHING`,
		token, req.IntegrationID, req.AppID, req.RecordID, req.Slot, req.EffectID, minor, cur, at); err != nil {
		return ref{}, err
	}
	out, err = scanRef(tx.QueryRowContext(ctx, `SELECT `+refColumns+` FROM integration_refs WHERE effect_id = ?`, req.EffectID))
	if err != nil {
		return ref{}, err
	}
	if out.objectID == "" && (out.integrationID != req.IntegrationID || out.amountMinor != minor || out.currency != cur || out.state == "failed") {
		if _, err := tx.ExecContext(ctx, `UPDATE integration_refs SET integration_id = ?, amount_minor = ?, currency = ?, state = 'creating'
			WHERE id = ? AND provider_object_id IS NULL`, req.IntegrationID, minor, cur, out.id); err != nil {
			return ref{}, err
		}
		out.integrationID, out.amountMinor, out.currency, out.state = req.IntegrationID, minor, cur, "creating"
	}
	return out, tx.Commit()
}

// noteLink keeps the provider's answer on the ref: the object, the address, and that it is open.
func (s *Service) noteLink(ctx context.Context, r ref, l Link) error {
	res, err := s.store.db.ExecContext(ctx, `
		UPDATE integration_refs SET provider_object_id = ?, url = ?, state = 'open'
		WHERE id = ? AND provider_object_id IS NULL AND state = 'creating'`, l.ObjectID, l.URL, r.id)
	if err != nil {
		var se *sqlite.Error
		if errors.As(err, &se) && se.Code() == 2067 { // SQLITE_CONSTRAINT_UNIQUE: the object is another ref's
			slog.Error("integrations: a provider answered with an object that belongs to another reference", "integration", r.integrationID)
		}
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		return err
	}
	// Another try noted it first: it is the same object, since the key is the same.
	return nil
}

// callFailed notes what a failed call means for the ref and turns it into the entry's failure.
func (s *Service) callFailed(ctx context.Context, r ref, rec record, err error) error {
	ce, ok := IsCall(err)
	if !ok {
		return err
	}
	switch {
	case ce.Kind != CallUnreachable:
		// A refusal is for good: the ref is failed (a retry of the entry starts it anew).
		if _, uerr := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET state = 'failed', key_seq = key_seq + ?
			WHERE id = ? AND provider_object_id IS NULL`, boolInt(ce.KeyKept), r.id); uerr != nil {
			return uerr
		}
	case ce.KeyKept:
		if _, uerr := s.store.db.ExecContext(ctx, `UPDATE integration_refs SET key_seq = key_seq + 1 WHERE id = ? AND provider_object_id IS NULL`, r.id); uerr != nil {
			return uerr
		}
	}
	// What is logged is what happened, never what was said: not the key, not the provider's words.
	slog.Warn("integrations: a payment link wasn't made", "integration", r.integrationID, "provider", rec.Provider,
		"kind", int(ce.Kind), "status", ce.Status)
	switch ce.Kind {
	case CallCredentials:
		return &apps.PaymentLinkError{Code: apierr.CodeIntegrationCredentialsRefused}
	case CallRefused:
		return &apps.PaymentLinkError{Code: apierr.CodeIntegrationProviderRefused}
	}
	return &apps.PaymentLinkError{Code: apierr.CodeIntegrationProviderUnreachable, Transient: true}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
