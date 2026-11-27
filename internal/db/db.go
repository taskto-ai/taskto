package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

const FileName = "taskto.db"

var pragmas = url.Values{
	"_busy_timeout": {"5000"},
	"_foreign_keys": {"1"},
	"_journal_mode": {"WAL"},
	"_synchronous":  {"NORMAL"},
	"_txlock":       {"immediate"},
}

func Open(ctx context.Context, path string) (*sql.DB, error) {
	dsn, err := dsn(path)
	if err != nil {
		return nil, err
	}
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	return sqlDB, nil
}

func dsn(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("database path %s: %w", path, err)
	}
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p}
	return u.String() + "?" + pragmas.Encode(), nil
}
