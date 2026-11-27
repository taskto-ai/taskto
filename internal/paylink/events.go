package paylink

import (
	"context"
	"database/sql"
	"time"
)

// EventWriter is what the apps offer the integrations' event worker: it writes what a verified
// payment event says to the record it is about, as the integration and only as the app's approved
// contract allows. The server hands the apps service to the integrations (package integrations,
// Service.UseRecords). Its failures are *Error: a code for the admins, and whether it may pass.
type EventWriter interface {
	WriteEvent(ctx context.Context, w EventWrite) (EventResult, error)
}

// EventTargetChecker is what an EventWriter may also offer: whether events of an app's slot may be
// written now, which is what the reconciliation polling asks before it reads anything at a provider
// for the app's references. It is the check WriteEvent makes in its own transaction, read alone: the
// app is online, its live release declares the slot, the contract is approved and the integration it
// is bound to is switched on and allowed in the app's space. The answer is only a reason not to
// call the provider: the write checks everything again.
type EventTargetChecker interface {
	// EventBinding returns the integration the slot of the app is bound to, and ok false when the
	// app's slot may not be written to now (it is not an error). Any other failure is an error.
	EventBinding(ctx context.Context, appID int64, slot string) (integrationID int64, ok bool, err error)
}

// EventWrite is one verified event for one record. Every id in it comes from the server's own
// reference (integration_refs) and never from the provider's event.
type EventWrite struct {
	// WorkspaceID is the integration's workspace: the app must be in a space of it.
	WorkspaceID, IntegrationID, AppID, RecordID int64
	// Slot is the app's slot the reference was made for.
	Slot string
	// Event is the normalized event (payment.succeeded...).
	Event string
	// AmountMinor and Currency are what the verified event says was paid (or, of a refund,
	// refunded), exactly.
	AmountMinor int64
	Currency    string
	OccurredAt  time.Time
	// PaymentID is the provider's id of the payment, "" when the event had none.
	PaymentID string
	// Settle runs first in the transaction that writes the record, once the app, its approval and
	// the integration have been checked as of now, and before any change: it moves the reference's
	// state, so the state and the record change together or not at all. An error it returns ends
	// the write with nothing changed, and is returned as it is.
	Settle func(ctx context.Context, tx *sql.Tx) error
	// Done runs last in that same transaction, once the record has been written (or left as it is),
	// with what became of it: it records the event's own outcome, so the outcome and the write
	// commit together, and a worker that stops after the commit leaves nothing to redo or lose. An
	// error it returns ends the write with nothing changed, and is returned as it is.
	Done func(ctx context.Context, tx *sql.Tx, res EventResult) error
}

// EventResult says what became of the record.
type EventResult struct {
	// Written is whether the record changed.
	Written bool
	// NoHandler is true when the app's live release declares nothing for the event: the reference
	// was settled and the record left as it is.
	NoHandler bool
	// ConditionUnmet is true when the handler's only_if rule did not hold of the record: the
	// reference was settled and the record left as it is.
	ConditionUnmet bool
}
