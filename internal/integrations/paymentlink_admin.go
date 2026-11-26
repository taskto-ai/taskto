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

// The admin's side of a payment link whose outcome is unknown (paymentlink.go has the rule; the
// flow is in docs/integrations.md). A ref whose key's outcome is unknown never gets a new key by
// itself: its entry looks for the key's object and asks again under the same key, for outcomeWait, and
// then fails with integration_outcome_unknown. An admin who checked the provider's account and found
// no link confirms it here, which allows exactly one new key and runs the entry again; one who found
// the link has the entry retried once it is visible, and the lookup adopts it.
//
// A confirmation is allowed only for a ref of which nothing was ever seen or settled: no key of it has
// an object on record (an answer named one, or a lookup found one carrying its token, checked or not: one
// whose amount couldn't be read is kept all the same, before anything else) and no key of it has an
// outcome. Otherwise the admin's "none exists" can contradict what
// the server itself saw, and the new key could make a second payable link: it is refused
// (integration_link_not_confirmable), checked in the transaction that would allow the new key.
//
// Both routes are for an admin, and only for the spaces they manage: a private space's payments
// are its owner's, who is told nothing of them by another admin's request (a space the admin may not
// manage is "not found", as for any other thing of the integration).

// PaymentLinkOutcome is a payment link whose outcome at the provider is unknown, as an admin sees it.
type PaymentLinkOutcome struct {
	// EffectID is the number of the entry of the app's effect that asked for the link (the number the
	// person who acted sees in the app's effects).
	EffectID int64 `json:"effect_id"`
	AppID    int64 `json:"app_id"`
	SpaceID  int64 `json:"space_id"`
	RecordID int64 `json:"record_id"`
	// Amount is the amount asked for as decimal text with the currency's decimals, and Currency its code.
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
	// Since is when the outcome became unknown.
	Since string `json:"since"`
	// Waiting is whether the server is still looking for the link: the entry waits, and nothing is
	// asked of the admin yet. When it is false the wait ended and the entry failed.
	Waiting bool `json:"waiting"`
	// CanConfirm is whether the admin may confirm that no link exists: the wait ended, and nothing of the
	// ref was ever seen at the provider or settled (confirmable).
	CanConfirm bool `json:"can_confirm"`
	// ObjectID is the provider's id of an object of the waiting key that the server saw (an answer named
	// it, or a lookup found it carrying the key's token) and couldn't resolve, such as a Payment Link whose
	// line items couldn't be read: "" when none was seen. When set, the admin can't confirm that no link
	// exists; they check this object at the provider, and the entry adopts it once a lookup can.
	ObjectID string `json:"object_id"`
	// Code is why the lookup stopped, when it is neither waiting nor ready for a decision: the code
	// the entry failed with (for example integration_account_changed), which the web app says in
	// words. It is empty while the server waits and when the admin may confirm.
	Code string `json:"code,omitempty"`
	// Params are the values the code's message names (for integration_amount_above_max, the ceiling and
	// its currency), as the entry failed with them; left out when there are none.
	Params apierr.Params `json:"params,omitempty"`
	// Attempts is every try at this payment (every idempotency key of the reference, oldest first, not
	// only the current one): what an admin searches the provider's account for. Never anything sealed.
	Attempts []PaymentLinkAttempt `json:"attempts" apitypes:"nonnull"`
}

// The kinds of object an attempt asked the provider for, which say which of its fields holds the
// attempt's reference.
const (
	// AttemptCheckoutSession: a Checkout Session, whose client_reference_id holds the reference.
	AttemptCheckoutSession = "checkout_session"
	// AttemptPaymentLink: a Payment Link, whose metadata[taskto_ref] holds the reference.
	AttemptPaymentLink = "payment_link"
)

// PaymentLinkAttempt is one attempt at a payment link, as an admin looks for it at the provider.
type PaymentLinkAttempt struct {
	// Reference is the attempt's random token: what the call carried as client_reference_id (a Checkout
	// Session) or metadata[taskto_ref] (a Payment Link), and the provider echoes back with a payment. It
	// is a reference TaskTo made up, never a key of the provider's account.
	Reference string `json:"reference"`
	// Kind is the kind of object the attempt asked for: AttemptCheckoutSession or AttemptPaymentLink.
	Kind string `json:"kind"`
	// ObjectID is the provider's id of an object seen for this attempt, "" when none was, and
	// ObjectVerified whether that object was checked to ask this payment's amount and currency.
	ObjectID       string `json:"object_id"`
	ObjectVerified bool   `json:"object_verified"`
	// AccountID is the provider's account the reference was made under and Mode the key's mode there
	// ("test" or "live"), both as recorded when it was made ("" when it wasn't recorded).
	AccountID string `json:"account_id"`
	Mode      string `json:"mode"`
	// FirstSentAt is when the first call under the attempt's reference went out, "" when none did.
	FirstSentAt string `json:"first_sent_at"`
}

// PaymentLinkOutcomes is the answer of the list.
type PaymentLinkOutcomes struct {
	PaymentLinks []PaymentLinkOutcome `json:"payment_links" apitypes:"nonnull"`
}

const maxOutcomesListed = 200

// confirmable is the SQL condition, on a ref aliased r, that no key of it has an object on record or
// an outcome: nothing of it was seen at the provider or settled, so an admin's confirmation that none
// exists contradicts nothing the server knows.
const confirmable = `NOT EXISTS (SELECT 1 FROM integration_ref_attempts o
	WHERE o.ref_id = r.id AND (o.object_id IS NOT NULL OR o.outcome IS NOT NULL))`

// unresolved lists the integration's refs whose outcome is unknown and that have no object, in the
// spaces named.
func (s *Service) unresolved(ctx context.Context, workspace, integration int64, spaces map[int64]bool) ([]PaymentLinkOutcome, error) {
	if _, err := s.store.getSnapshot(ctx, workspace, integration); err != nil {
		return nil, err
	}
	rows, err := s.store.db.QueryContext(ctx, `
		SELECT r.id, r.success_url, r.account_id, r.account_mode, r.effect_id, r.app_id, a.space_id, r.record_id, r.amount_minor, r.currency, coalesce(k.unknown_since, k.created_at),
			coalesce(e.state, ''), coalesce(e.error_code, ''), e.error_params, `+confirmable+`,
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
	var refs []refView
	for rows.Next() {
		var o PaymentLinkOutcome
		var v refView
		var minor int64
		var state, code string
		var params sql.NullString
		var clean bool
		if err := rows.Scan(&v.id, &v.successURL, &v.account, &v.mode, &o.EffectID, &o.AppID, &o.SpaceID, &o.RecordID, &minor, &o.Currency, &o.Since, &state, &code, &params, &clean, &o.ObjectID); err != nil {
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
				// Written by the outbox as JSON; params that can't be read leave the code without them.
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

// refView is what of a ref the attempts' listing needs.
type refView struct {
	id         int64
	successURL string
	account    string
	mode       string
}

// attempts lists every key of a ref, oldest first. A ref that asked for the server's own pages
// (success_url set) made Checkout Sessions, and one that didn't made Payment Links: the same choice
// the provider call makes (payments.go), so the kind always names the field the reference is in.
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

// ConfirmNoPaymentLink is the admin's confirmation that the provider holds no payment link for the
// entry whose key's outcome never became known: the key is abandoned and the ref moves to one new key
// (once: it is allowed only while the outcome is unknown and the wait has ended, and the confirmation
// ends the unknown), the entry is queued again as it was (its attempts counted afresh), and the audit
// log records it. It is the only way to a new key for a ref whose key's outcome is unknown, and it is
// refused (integration_link_not_confirmable) for a ref any key of which has an object on record or an
// outcome, checked in this transaction, which holds the write lock from its start.
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
			return errNotFound // another space's, or none: nothing says which
		}
		if err != nil {
			return err
		}
		nothing := apierr.New(http.StatusConflict, apierr.CodeIntegrationNothingToConfirm,
			"this payment link isn't waiting for a decision about an unknown outcome")
		// A payment settled the ref (a verified event, for any of its keys' tokens): nothing is unknown any more.
		if object != "" || !unknown || refState == "paid" || refState == "refunded" || state != "failed" || code != string(apierr.CodeIntegrationOutcomeUnknown) {
			return nothing
		}
		// Something of the ref was seen at the provider (an object an answer named or a lookup found, kept
		// on its key) or a key of it was settled: "none exists" can't be taken from anyone then.
		if !clean {
			return apierr.New(http.StatusConflict, apierr.CodeIntegrationLinkNotConfirmable,
				"an object of this payment link was seen at the provider, or a key of it was already resolved: no new key is allowed")
		}
		// The key is abandoned (its row stays, with every call made under it) and the ref takes the next.
		// The conditions are repeated in the statement, so nothing seen can be abandoned whatever ran before.
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
		// The entry runs again as the person who acted, with the rights they have then.
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
