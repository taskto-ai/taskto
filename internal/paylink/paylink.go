package paylink

import (
	"context"
	"database/sql"
	"time"

	"taskto.ai/taskto/internal/apierr"
)

type Opener interface {
	OpenPaymentLink(ctx context.Context, req Request) (Link, error)
	PayableMoney(ctx context.Context, q Querier, appID, recordID int64) (map[int64][]MoneyFields, error)
}

type MoneyFields struct{ Amount, Currency string }

type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

type Request struct {
	WorkspaceID, IntegrationID, SpaceID, AppID, RecordID int64
	EffectID                                             int64
	Slot                                                 string
	URLField                                             string
	AmountField, CurrencyField                           string
	Amount, Currency                                     string
	Description                                          string
}

type Link struct {
	URL, ObjectID string
	AmountMinor   int64
	Currency      string
}

type Error struct {
	Code      apierr.Code
	Params    apierr.Params
	Transient bool
	Wait      time.Duration
}

func (e *Error) Error() string { return string(e.Code) }
