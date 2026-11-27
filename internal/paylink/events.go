package paylink

import (
	"context"
	"database/sql"
	"time"
)

type EventWriter interface {
	WriteEvent(ctx context.Context, w EventWrite) (EventResult, error)
}

type EventTargetChecker interface {
	EventBinding(ctx context.Context, appID int64, slot string) (integrationID int64, ok bool, err error)
}

type EventWrite struct {
	WorkspaceID, IntegrationID, AppID, RecordID int64
	Slot                                        string
	Event                                       string
	AmountMinor                                 int64
	Currency                                    string
	OccurredAt                                  time.Time
	PaymentID                                   string
	Settle                                      func(ctx context.Context, tx *sql.Tx) error
	Done                                        func(ctx context.Context, tx *sql.Tx, res EventResult) error
}

type EventResult struct {
	Written        bool
	NoHandler      bool
	ConditionUnmet bool
}
