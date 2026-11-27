package db

import (
	"database/sql/driver"
	"strings"

	"modernc.org/sqlite"
)

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
