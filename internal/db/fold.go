package db

import (
	"database/sql/driver"
	"strings"

	"modernc.org/sqlite"
)

// FoldFunc is the SQL function taskto_fold(text): the text in lower case, by Unicode rules
// (SQLite's own lower() folds ASCII only, so a search for "ação" would miss "AÇÃO"). Anything
// that isn't text is NULL. Searching inside an app's records (internal/apps) compares folded
// text on both sides.
const FoldFunc = "taskto_fold"

func init() {
	sqlite.MustRegisterDeterministicScalarFunction(FoldFunc, 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		switch v := args[0].(type) {
		case string:
			return strings.ToLower(v), nil
		case []byte:
			return strings.ToLower(string(v)), nil
		}
		return nil, nil
	})
}
