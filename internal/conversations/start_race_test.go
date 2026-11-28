package conversations

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net/url"
	"strings"
	"sync"
	"testing"

	"taskto.ai/taskto/internal/agentrt"
)

type afterFirstOriginLookup struct {
	driver.Connector
	once sync.Once
	run  func()
}

func (c *afterFirstOriginLookup) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &hookedConn{Conn: conn, hook: c}, nil
}

func (c *afterFirstOriginLookup) Driver() driver.Driver { return c.Connector.Driver() }

type hookedConn struct {
	driver.Conn
	hook *afterFirstOriginLookup
}

func (c *hookedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err != nil || !strings.Contains(query, "FROM conversations") || !strings.Contains(query, "origin = ?") {
		return rows, err
	}
	return &hookedRows{Rows: rows, hook: c.hook}, nil
}

type hookedRows struct {
	driver.Rows
	hook *afterFirstOriginLookup
}

func (r *hookedRows) Next(dest []driver.Value) error {
	err := r.Rows.Next(dest)
	if errors.Is(err, io.EOF) {
		r.hook.once.Do(r.hook.run)
	}
	return err
}

type dsnConnector struct {
	dsn string
	drv driver.Driver
}

func (c dsnConnector) Connect(context.Context) (driver.Conn, error) { return c.drv.Open(c.dsn) }
func (c dsnConnector) Driver() driver.Driver                        { return c.drv }

func (e *env) hookAfterFirstOriginLookup(run func()) {
	e.t.Helper()
	var file string
	if err := e.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&file); err != nil {
		e.t.Fatal(err)
	}
	u := url.URL{Scheme: "file", Path: file, RawQuery: url.Values{
		"_busy_timeout": {"5000"}, "_foreign_keys": {"1"}, "_journal_mode": {"WAL"}, "_txlock": {"immediate"},
	}.Encode()}
	hooked := sql.OpenDB(&afterFirstOriginLookup{Connector: dsnConnector{dsn: u.String(), drv: e.db.Driver()}, run: run})
	e.t.Cleanup(func() { _ = hooked.Close() })
	e.svc.db = hooked
}

func TestARetryOfTheOwnKeyThatLandsBetweenLookupAndClaimGetsTheSameConversation(t *testing.T) {
	e := newEnv(t, false)
	owner, workspace := e.setup()
	st := Start{Workspace: workspace, User: owner.id, Title: "Report", Prompt: "Do work", Origin: Origin{Kind: "app", Key: "app-effect:200", AppID: 2}}
	var winner Conversation
	e.hookAfterFirstOriginLookup(func() {
		var err error
		if winner, err = (&Service{db: e.db}).insertWithOrigin(context.Background(), st, 900); err != nil {
			t.Error(err)
		}
	})
	started, err := e.svc.Start(context.Background(), st)
	if err != nil {
		t.Fatalf("a retry of the person's own key = %v", err)
	}
	if winner.ID == 0 || started.Conversation.ID != winner.ID {
		t.Fatalf("started %+v, the competing start made %+v", started.Conversation, winner)
	}
	var rows int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("%d conversations, %v", rows, err)
	}
}

type gatedCreate struct {
	*fakeRuntime
	arrived sync.WaitGroup
	release chan struct{}
}

func (g *gatedCreate) CreateConversationOn(ctx context.Context, model agentrt.ModelRef) (int64, error) {
	id, err := g.fakeRuntime.CreateConversationOn(ctx, model)
	g.arrived.Done()
	<-g.release
	return id, err
}

func TestConcurrentStartsOfOneKeyShareOneConversationAndStartOnce(t *testing.T) {
	const starts = 8
	e := newEnv(t, false)
	owner, workspace := e.setup()
	gate := &gatedCreate{fakeRuntime: e.rt, release: make(chan struct{})}
	gate.arrived.Add(starts)
	e.svc.agent = func(context.Context) (Runtime, error) { return gate, nil }
	st := Start{Workspace: workspace, User: owner.id, Prompt: "Do work", Origin: Origin{Kind: "routine", Key: "routine-run:210", RoutineID: 3}}
	results := make([]Started, starts)
	errs := make([]error, starts)
	var done sync.WaitGroup
	for i := range starts {
		done.Add(1)
		go func() {
			defer done.Done()
			results[i], errs[i] = e.svc.Start(context.Background(), st)
		}()
	}
	gate.arrived.Wait()
	close(gate.release)
	done.Wait()
	for i := range starts {
		if errs[i] != nil {
			t.Fatalf("start %d = %v", i, errs[i])
		}
		if results[i].Conversation.ID != results[0].Conversation.ID || results[i].Submission != results[0].Submission {
			t.Fatalf("start %d = %+v, start 0 = %+v", i, results[i], results[0])
		}
	}
	var rows int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("%d conversations, %v", rows, err)
	}
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	turns := 0
	for _, inputs := range e.rt.inputs {
		turns += len(inputs)
	}
	if turns != 1 {
		t.Fatalf("the runtime started %d turns, want 1: %v", turns, e.rt.inputs)
	}
}
