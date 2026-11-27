package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync"

	"modernc.org/sqlite"
)

type Hooks struct {
	mu          sync.Mutex
	beforeBegin func()
	before      map[string]func()
}

func (h *Hooks) BeforeBegin(fn func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.beforeBegin = fn
}

func (h *Hooks) BeforeQuery(fragment string, fn func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.before == nil {
		h.before = map[string]func(){}
	}
	h.before[fragment] = fn
}

func (h *Hooks) takeBegin() func() {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn := h.beforeBegin
	h.beforeBegin = nil
	return fn
}

func (h *Hooks) takeQuery(query string) func() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for fragment, fn := range h.before {
		if strings.Contains(query, fragment) {
			delete(h.before, fragment)
			return fn
		}
	}
	return nil
}

func OpenHooked(ctx context.Context, path string, h *Hooks) (*sql.DB, error) {
	dsn, err := dsn(path)
	if err != nil {
		return nil, err
	}
	inner, err := sqlite.NewConnector(dsn)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	sqlDB := sql.OpenDB(hookedConnector{inner, h})
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	return sqlDB, nil
}

type hookedConnector struct {
	driver.Connector
	h *Hooks
}

func (c hookedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &hookedConn{conn, c.h}, nil
}

type hookedConn struct {
	driver.Conn
	h *Hooks
}

func (c *hookedConn) Unwrap() driver.Conn { return c.Conn }

func (c *hookedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if fn := c.h.takeBegin(); fn != nil {
		fn()
	}
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c *hookedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if fn := c.h.takeQuery(query); fn != nil {
		fn()
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *hookedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if fn := c.h.takeQuery(query); fn != nil {
		fn()
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *hookedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	return c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
}

func (c *hookedConn) Ping(ctx context.Context) error {
	return c.Conn.(driver.Pinger).Ping(ctx)
}

func (c *hookedConn) ResetSession(ctx context.Context) error {
	return c.Conn.(driver.SessionResetter).ResetSession(ctx)
}

func (c *hookedConn) IsValid() bool { return c.Conn.(driver.Validator).IsValid() }
