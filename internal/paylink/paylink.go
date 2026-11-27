package paylink

import (
	"context"
	"time"

	"taskto.ai/taskto/internal/apierr"
)

type Opener interface {
	OpenPaymentLink(ctx context.Context, req Request) (Link, error)
}

type Request struct {
	WorkspaceID, IntegrationID, SpaceID, AppID, RecordID int64
	EffectID                                             int64
	Slot                                                 string
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
