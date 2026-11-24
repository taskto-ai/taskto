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

// Hooks are what a test runs at chosen moments of a database's use, to make two things happen in
// a fixed order that only a race could otherwise order: a change between a check and the read
// after it. They are for tests; the server never sets one.
type Hooks struct {
	mu sync.Mutex
	// beforeBegin runs just before a transaction begins on the hooked database.
	beforeBegin func()
	// before maps a fragment of SQL to what runs just before a statement containing it.
	before map[string]func()
}

// BeforeBegin sets what runs, once, just before the next transaction begins.
func (h *Hooks) BeforeBegin(fn func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.beforeBegin = fn
}

// BeforeQuery sets what runs, once, just before the next statement that contains fragment is
// executed or queried. fn runs on the caller's goroutine and may use other connections.
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

// OpenHooked is Open on the same database with the same settings, through a driver that runs h's
// hooks. For tests.
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

// hookedConn is the driver's connection, with the hooks run before what they name.
type hookedConn struct {
	driver.Conn
	h *Hooks
}

// Unwrap lets the driver's own helpers reach the connection.
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
