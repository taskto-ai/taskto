// Package paylink is what the apps and the integrations say to each other about payment links:
// the create_payment_link effect of an app (package apps) asks an Opener, which package
// integrations implements. The two packages each need these types and neither may import the
// other (the apps' tests build an integrations service), so they live here and depend on nothing
// but the API's error codes.
package paylink

import (
	"context"
	"database/sql"
	"time"

	"taskto.ai/taskto/internal/apierr"
)

// Opener is what the integrations offer create_payment_link; the server hands it to the apps
// service in Platform.
type Opener interface {
	// OpenPaymentLink returns the link to pay what the request asks, making it at the provider only
	// if the request's entry has not made one already. Its failures are *Error.
	OpenPaymentLink(ctx context.Context, req Request) (Link, error)
	// EntryLink is the link the entry's reference saved (the entry's number is effectID), read through
	// q with no call to anyone: link.ObjectID is "" while it has none. pending says that, with none saved,
	// the reference may still hold an object nobody has given yet (a call whose outcome is unknown, an
	// object seen and not resolved, a payment that came first): only when it doesn't was nothing made.
	EntryLink(ctx context.Context, q Querier, effectID int64) (link Link, pending bool, err error)
	// LinkCurrent says whether a link the integration made may be written to a record now, read
	// through q (the write's own transaction, under its lock): it is refused (an *Error with
	// integration_account_changed) when the integration's key is of another mode now than the one the
	// link was made in, so a test link never lands on a live integration's record, nor a live one on a
	// test integration's.
	LinkCurrent(ctx context.Context, q Querier, integrationID int64, objectID string) error
	// PayableMoney says which records of the app have a payment link that may still take a payment,
	// or may still come to exist, as far as the server knows: one that is open, still being made or
	// whose outcome is unknown, read through q (the transaction that changes the records, under its
	// write lock). Only a link confirmed expired, or paid on that very link, is no longer one. While a
	// record has one, its money (the amount and currency it asks) can't change. recordID 0 is every
	// record of the app.
	// For each such record it says the fields the money of each such link was read from, whatever the
	// app's live release declares now: MoneyFields{} for a link made before links kept them.
	PayableMoney(ctx context.Context, q Querier, appID, recordID int64) (map[int64][]MoneyFields, error)
}

// MoneyFields are the record's fields a payment link's amount and currency were read from (Currency ""
// for an effect's own currency, and both "" when it isn't known which).
type MoneyFields struct{ Amount, Currency string }

// Querier is what the Opener reads with: a transaction or the database.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Request is a payment link as the integration is asked for it. The integration of the
// workspace is the one the approval bound the slot to, and the limits it checks are its own.
type Request struct {
	WorkspaceID, IntegrationID, SpaceID, AppID, RecordID int64
	// EffectID is the outbox entry's number, which the integration's reference is unique by.
	EffectID int64
	Slot     string
	// URLField is the record's field the link is written to. A record has one payable link at a time,
	// whatever field it is written to: an entry with no reference of its own first reads the record's.
	URLField string
	// AmountField and CurrencyField are the record's fields the money was read from ("" for an
	// effect's own currency): the reference keeps them, so the record's money stays as it is while the
	// link may be paid whatever the app's live release declares by then.
	AmountField, CurrencyField string
	// Amount is the amount as decimal text and Currency its code, as the record held them
	// (payload): nothing in them was rounded, converted or checked yet.
	Amount, Currency string
	// Description is the text the payer reads.
	Description string
	// LookupOnly asks only for what the entry's reference already made: a link it saved, or the object
	// a call whose outcome is unknown may have made, looked for by its token. Nothing is sent to the
	// provider that could make a link, under the same key or a new one (the record no longer meets the
	// action's condition). With nothing to find it fails: the lookup's wait while a key's outcome is
	// unknown, and integration_condition_changed once nothing can exist.
	LookupOnly bool
}

// Link is what the provider made.
type Link struct {
	// URL is the address the payer opens, on the provider's own domain; ObjectID is the provider's
	// id of what it made.
	URL, ObjectID string
	// AmountMinor and Currency are what the link asks, exactly.
	AmountMinor int64
	Currency    string
}

// Error is a failure of OpenPaymentLink with the code the person who acted is told.
// Transient failures (the provider didn't answer) are tried again by the outbox before they fail
// for good.
type Error struct {
	Code      apierr.Code
	Params    apierr.Params
	Transient bool
	// Wait, when positive, asks for the entry to be looked at again after that long without the try
	// counting against its tries: the integration is waiting for the provider to say whether an earlier
	// call made the link, and bounds that wait itself.
	Wait time.Duration
}

func (e *Error) Error() string { return string(e.Code) }
