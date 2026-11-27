package apps

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/db"
	"taskto.ai/taskto/internal/paylink"
)

type payable struct {
	paylink.Opener
	fields []paylink.MoneyFields
}

func (p payable) PayableMoney(_ context.Context, _ paylink.Querier, _, record int64) (map[int64][]paylink.MoneyFields, error) {
	return map[int64][]paylink.MoneyFields{record: p.fields}, nil
}

func TestTheMoneyLockComparesWhatTheFieldHolds(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	def := Definition{Collection: Collection{Fields: []Field{{Name: "total", Type: "string"}, {Name: "note", Type: "string"}}}}
	links := payable{fields: []paylink.MoneyFields{{Amount: "total"}}}
	for name, tc := range map[string]struct {
		before, after map[string]any
		locked        bool
	}{
		"the amount, now text":     {map[string]any{"total": "10"}, map[string]any{"total": "12"}, true},
		"the same amount, as text": {map[string]any{"total": "10"}, map[string]any{"total": "10"}, false},
		"another field":            {map[string]any{"total": "10", "note": "a"}, map[string]any{"total": "10", "note": "b"}, false},
	} {
		err := paymentMoneyLocked(ctx, tx, links, 1, 1, def, tc.before, tc.after)
		var bad *invalidError
		if got := errors.As(err, &bad) && bad.code == apierr.CodeAppPaymentLinkOpen; got != tc.locked || (!tc.locked && err != nil) {
			t.Errorf("%s: %v, want locked %v", name, err, tc.locked)
		}
	}
}
