// Package cliaccounts is the server's side of CLI workers (docs/cli-workers.md): each user's
// logins to the official Claude Code, Codex and Grok Build CLIs, their state and usage limits,
// and the API under /api/me/cli-accounts that connects them with each CLI's own login flow.
//
// The logins themselves never come here: the CLI keeps its credentials in the user's own
// directory on the worker (internal/cliworker), and the server only asks the worker to start
// a login, pass a code to it, check it or sign out. Every call names the signed-in user, and
// only ever touches that user's logins.
package cliaccounts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/providers"
)

// Mode is how CLI workers run (the -cli-workers setting).
type Mode string

// The modes.
const (
	Off Mode = ""
	// Local runs the CLIs as the server's own OS user, with no OS boundary between users:
	// allowed only while the instance has a single user.
	Local Mode = "local"
	// LocalUnsafe is Local without that check, for development and tests.
	LocalUnsafe Mode = "local-unsafe"
	// Container runs each user's CLIs as a Unix user of their own, in a worker of its own.
	Container Mode = "container"
)

// ParseMode reads the -cli-workers setting.
func ParseMode(s string) (Mode, error) {
	switch s {
	case "", "off":
		return Off, nil
	case "local":
		return Local, nil
	case "local-unsafe":
		return LocalUnsafe, nil
	case "container":
		return Container, nil
	}
	return Off, errors.New("cli workers: unknown mode " + s)
}

// Worker is the CLI worker as accounts use it: Local's adapter of a *cliworker.Worker, or a
// fake in tests.
type Worker interface {
	Available(cli cliworker.CLI) bool
	StartLogin(user int64, cli cliworker.CLI, slot cliworker.Slot) (cliworker.LoginInfo, error)
	LoginInfo(user int64, cli cliworker.CLI, slot cliworker.Slot) (cliworker.LoginInfo, bool)
	SubmitLoginCode(user int64, cli cliworker.CLI, slot cliworker.Slot, code string) error
	CancelLogin(user int64, cli cliworker.CLI, slot cliworker.Slot)
	Status(ctx context.Context, user int64, cli cliworker.CLI, slot cliworker.Slot) (cliworker.AuthStatus, error)
	Logout(ctx context.Context, user int64, cli cliworker.CLI, slot cliworker.Slot) error

	// StartTurn starts spec's turn, or returns the one with its ID (attached true).
	StartTurn(spec cliworker.TurnSpec) (t Turn, attached bool, err error)
	FindTurn(id string) (Turn, bool)
	StopTurn(id string)

	// Permissions lists the permission questions waiting in user's conversation, and Answer
	// answers one (cliworker.ErrNoPermission if it isn't waiting there).
	Permissions(ctx context.Context, user, conversation int64) ([]cliworker.PermissionRequest, error)
	Answer(ctx context.Context, user, conversation int64, id string, allow bool) error
}

// LocalWorker adapts a worker running in this process.
func LocalWorker(w *cliworker.Worker) Worker { return localWorker{w} }

type localWorker struct{ *cliworker.Worker }

func (l localWorker) StartLogin(user int64, cli cliworker.CLI, slot cliworker.Slot) (cliworker.LoginInfo, error) {
	login, err := l.Worker.StartLogin(user, cli, slot)
	if err != nil {
		return cliworker.LoginInfo{}, err
	}
	return login.Info(), nil
}

func (l localWorker) LoginInfo(user int64, cli cliworker.CLI, slot cliworker.Slot) (cliworker.LoginInfo, bool) {
	login, ok := l.Login(user, cli, slot)
	if !ok {
		return cliworker.LoginInfo{}, false
	}
	return login.Info(), true
}

func (l localWorker) Permissions(_ context.Context, user, conversation int64) ([]cliworker.PermissionRequest, error) {
	return l.Worker.Permissions(user, conversation), nil
}

func (l localWorker) Answer(_ context.Context, user, conversation int64, id string, allow bool) error {
	return l.Worker.Answer(user, conversation, id, allow)
}

// RemoteWorker adapts the client of a worker in its own process (container mode).
func RemoteWorker(r *cliworker.Remote) Worker { return remoteWorker{r} }

type remoteWorker struct{ *cliworker.Remote }

func (r remoteWorker) StartTurn(spec cliworker.TurnSpec) (Turn, bool, error) {
	t, attached, err := r.Remote.StartTurn(spec)
	if err != nil {
		return nil, false, err
	}
	return t, attached, nil
}

func (r remoteWorker) FindTurn(id string) (Turn, bool) {
	t, ok := r.Remote.FindTurn(id)
	if !ok {
		return nil, false
	}
	return t, true
}

// Service keeps the users' CLI accounts and serves their API.
type Service struct {
	db     *sql.DB
	mode   Mode
	worker Worker // nil when Off

	// watching holds the logins a goroutine is following to their end, and started when each
	// account's last login started: an account that isn't connected yet is listed while its
	// login may still be on the worker.
	mu       sync.Mutex
	watching map[watchKey]bool
	started  map[watchKey]time.Time
	// settled holds, per user and CLI, when the login last recorded as connected started:
	// each login is recorded once, so a finished login the worker still reports can't mark
	// an account connected again after a turn found it expired.
	settled map[watchKey]time.Time
	// locks serialize recording and disconnecting an account (lockAccount).
	locks map[watchKey]*sync.Mutex
	// changed is closed, and replaced, when an account is connected: it wakes the turns
	// waiting for a Reconnect (/cli/wait) instead of each polling the database.
	changed chan struct{}
	wg      sync.WaitGroup
	closing chan struct{}
	poll    time.Duration

	// shared is the shared accounts' configuration (SetShared); nil, they are off.
	shared *Shared
	// upstream is the proxy's transport, and testClient the accounts' tests' client, both
	// under the workspace accounts' egress policy.
	upstream   http.RoundTripper
	testClient *http.Client
	// tokens holds each running turn's proxy and tools tokens (their nonces, by "<kind> <turn>"),
	// and ended the tokens that stopped working lately: their turn ended, or a replay of it has
	// a new one.
	tokens map[string]string
	ended  map[string]time.Time
	// tools gives turns taskto's own tools (SetTools); nil, they get none.
	tools atomic.Pointer[Tools]
	// setModel changes a conversation's model after a switch to another CLI; switches holds
	// the notice of each switch a turn made, by the worker turn it switched to.
	setModel ModelSetter
	switches map[string]string
	// stops holds the turns the user stopped lately: they switch no more.
	stops map[string]time.Time
}

type watchKey struct {
	user int64
	ref
}

// ref names one of a user's own accounts: a CLI and the slot of its config directory.
type ref struct {
	cli  cliworker.CLI
	slot cliworker.Slot
}

// maxSlot is how many accounts a user may connect per CLI.
const maxSlot = 9

// handle is the account's name in the API: the CLI for the first, "<cli>-<n>" for the others.
func (r ref) handle() string {
	if r.slot == "" {
		return string(r.cli)
	}
	return string(r.cli) + "-" + string(r.slot)
}

// number is the slot as the database keeps it: 1 for the first.
func (r ref) number() int {
	if r.slot == "" {
		return 1
	}
	return int(r.slot[0] - '0')
}

// refOf returns the account with slot number n (1 to maxSlot) of cli.
func refOf(cli cliworker.CLI, n int) ref {
	if n <= 1 || n > maxSlot {
		return ref{cli: cli}
	}
	return ref{cli: cli, slot: cliworker.Slot(strconv.Itoa(n))}
}

// parseHandle reads an account's handle: "claude", "codex-2", …
func parseHandle(s string) (ref, bool) {
	name, n, found := strings.Cut(s, "-")
	cli := cliworker.CLI(name)
	if !cli.Valid() {
		return ref{}, false
	}
	if !found {
		return ref{cli: cli}, true
	}
	if len(n) != 1 || n[0] < '2' || n[0] > '0'+maxSlot {
		return ref{}, false
	}
	return ref{cli: cli, slot: cliworker.Slot(n)}, true
}

// New returns the service over the migrated database sqlDB. worker is nil when mode is Off.
func New(sqlDB *sql.DB, mode Mode, worker Worker) *Service {
	if mode == Off {
		worker = nil
	}
	return &Service{db: sqlDB, mode: mode, worker: worker, watching: map[watchKey]bool{}, started: map[watchKey]time.Time{}, settled: map[watchKey]time.Time{}, locks: map[watchKey]*sync.Mutex{}, changed: make(chan struct{}), closing: make(chan struct{}), poll: time.Second, ended: map[string]time.Time{}, tokens: map[string]string{}, switches: map[string]string{}, stops: map[string]time.Time{}}
}

// Mode returns how CLI workers run: "off", "container", "local" or "local-unsafe".
func (s *Service) Mode() string {
	if s.mode == Off {
		return "off"
	}
	return string(s.mode)
}

// Providers are the CLI workers the agent runtime offers as models: one per CLI installed
// on the worker, none while CLI workers are off.
func (s *Service) Providers() []providers.CLIProvider {
	if s.worker == nil {
		return nil
	}
	var out []providers.CLIProvider
	for _, cli := range cliworker.CLIs {
		if !s.worker.Available(cli) {
			continue
		}
		p := providers.CLIProvider{CLI: string(cli), Name: cli.Name()}
		for _, m := range Models[cli] {
			name := m
			if m == "default" {
				name = cli.Name() + " (its default model)"
			}
			p.Models = append(p.Models, providers.ModelChoice{ID: m, Name: name})
		}
		out = append(out, p)
	}
	return out
}

// Close stops following logins.
func (s *Service) Close() {
	s.mu.Lock()
	select {
	case <-s.closing:
	default:
		close(s.closing)
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// ErrOff is Usable's answer when CLI workers are off.
var ErrOff = errors.New("CLI workers are off on this server")

// ErrNoBoundary is Usable's answer in local mode once the instance has more than one user.
var ErrNoBoundary = errors.New("CLI workers run in local mode on this server, with no OS boundary between users, " +
	"so they are allowed only while the server has a single user; an operator can run the CLI worker container instead (docs/cli-workers.md)")

// Usable reports whether CLI workers may be used now: nil, ErrOff, or ErrNoBoundary.
func (s *Service) Usable(ctx context.Context) error {
	switch s.mode {
	case Off:
		return ErrOff
	case LocalUnsafe, Container:
		return nil
	}
	var users int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&users); err != nil {
		return err
	}
	if users > 1 {
		return ErrNoBoundary
	}
	return nil
}

// State is an account's state; Disconnected has no row.
type State string

// The states.
const (
	Connected    State = "connected"
	Reconnect    State = "reconnect"
	Disconnected State = "disconnected"
)

// Account is one of a user's accounts for a CLI, as the API shows it.
type Account struct {
	// ID is the account's handle: the CLI for the first, "<cli>-<n>" for the others.
	ID   string        `json:"id"`
	CLI  cliworker.CLI `json:"cli"`
	Slot int           `json:"slot"`
	// Label is the user's name for it; empty for none.
	Label      string            `json:"label"`
	Name       string            `json:"name"`
	Available  bool              `json:"available"`
	State      State             `json:"state"`
	AuthMethod string            `json:"auth_method"`
	Limits     []cliworker.Limit `json:"limits"`
	LimitsAt   *string           `json:"limits_at"`
	// Login is the sign-in in progress or just finished, if any.
	Login *cliworker.LoginInfo `json:"login"`
}

type row struct {
	id         int64
	ref        ref
	label      string
	state      State
	authMethod string
	limits     sql.NullString
	limitsAt   sql.NullString
}

const rowColumns = `id, cli, slot, label, state, auth_method, limits, limits_at, shared_account_id`

func scanRow(sc interface{ Scan(...any) error }) (row, error) {
	var r row
	var cli string
	var slot, shared sql.NullInt64
	err := sc.Scan(&r.id, &cli, &slot, &r.label, &r.state, &r.authMethod, &r.limits, &r.limitsAt, &shared)
	r.ref = refOf(cliworker.CLI(cli), int(slot.Int64))
	if shared.Valid {
		r.ref.slot = sharedSlot(shared.Int64)
	}
	return r, err
}

// load returns user's own account a, if it is connected (or needs a new sign-in).
func (s *Service) load(ctx context.Context, user int64, a ref) (row, bool, error) {
	r, err := scanRow(s.db.QueryRowContext(ctx, `SELECT `+rowColumns+` FROM cli_accounts WHERE user_id = ? AND cli = ? AND slot = ?`,
		user, string(a.cli), a.number()))
	if errors.Is(err, sql.ErrNoRows) {
		return row{}, false, nil
	}
	return r, err == nil, err
}

// own returns user's own accounts for cli (all CLIs if cli is empty), by CLI and slot.
func (s *Service) own(ctx context.Context, user int64, cli cliworker.CLI) ([]row, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+rowColumns+` FROM cli_accounts
		WHERE user_id = ? AND slot IS NOT NULL AND (? = '' OR cli = ?) ORDER BY cli, slot`, user, string(cli), string(cli))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// account returns user's account a, with its login if the worker has one.
func (s *Service) account(ctx context.Context, user int64, a ref) (Account, error) {
	out := Account{ID: a.handle(), CLI: a.cli, Slot: a.number(), Name: a.cli.Name(), State: Disconnected, Limits: []cliworker.Limit{}}
	if s.worker != nil {
		out.Available = s.worker.Available(a.cli)
		if info, ok := s.worker.LoginInfo(user, a.cli, a.slot); ok {
			if err := s.settle(ctx, user, info, false); err != nil {
				return Account{}, err
			}
			out.Login = &info
		}
	}
	r, ok, err := s.load(ctx, user, a)
	if err != nil || !ok {
		return out, err
	}
	return r.fill(out), nil
}

// fill sets what r records on a.
func (r row) fill(a Account) Account {
	a.Label, a.State, a.AuthMethod = r.label, r.state, r.authMethod
	if r.limits.Valid {
		_ = json.Unmarshal([]byte(r.limits.String), &a.Limits)
	}
	if r.limitsAt.Valid {
		a.LimitsAt = &r.limitsAt.String
	}
	return a
}

// settle records a login that ended connected, once: the account is connected from then on.
// With status, it asks the CLI how it is signed in first (a CLI run: the watcher does that,
// in the background); without, the request that noticed the login records it at once, and
// the watcher fills the method in.
func (s *Service) settle(ctx context.Context, user int64, info cliworker.LoginInfo, status bool) error {
	if info.State != cliworker.LoginConnected {
		return nil
	}
	a := ref{info.CLI, info.Slot}
	key := watchKey{user, a}
	method := ""
	if status && s.worker != nil {
		if st, err := s.worker.Status(ctx, user, a.cli, a.slot); err == nil && st.LoggedIn {
			method = st.Method
		}
	}
	unlock := s.lockAccount(key)
	defer unlock()
	s.mu.Lock()
	done := s.settled[key].Equal(info.StartedAt)
	s.mu.Unlock()
	if done {
		if method != "" {
			return s.setMethod(ctx, user, a, method)
		}
		return nil
	}
	if err := s.setConnected(ctx, user, a, method); err != nil {
		return err
	}
	s.mu.Lock()
	s.settled[key] = info.StartedAt
	s.mu.Unlock()
	slog.InfoContext(ctx, "cli account connected", "user", user, "account", a.handle())
	return nil
}

// setConnectedIfKnown marks an account the CLI says is signed in connected, unless it is
// gone (disconnected): a check never creates one, a login does.
func (s *Service) setConnectedIfKnown(ctx context.Context, user int64, a ref, method string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE cli_accounts SET state = 'connected',
		auth_method = CASE WHEN ? = '' THEN auth_method ELSE ? END,
		updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE user_id = ? AND cli = ? AND slot = ?`, method, method, user, string(a.cli), a.number())
	if err == nil {
		s.notifyChanged()
	}
	return err
}

// accountChanged is closed when an account is connected next.
func (s *Service) accountChanged() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

func (s *Service) notifyChanged() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.changed)
	s.changed = make(chan struct{})
}

// setMethod records how a connected account is signed in.
func (s *Service) setMethod(ctx context.Context, user int64, a ref, method string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE cli_accounts SET auth_method = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE user_id = ? AND cli = ? AND slot = ? AND state = 'connected' AND auth_method <> ?`, method, user, string(a.cli), a.number(), method)
	return err
}

// lockAccount serializes recording a login and disconnecting, per user and account, so a login
// recorded in the background can't bring back an account just disconnected.
func (s *Service) lockAccount(key watchKey) func() {
	s.mu.Lock()
	l := s.locks[key]
	if l == nil {
		l = &sync.Mutex{}
		s.locks[key] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// forget makes the finished login the worker still reports for key one that is never
// recorded again: the account was disconnected after it.
func (s *Service) forget(key watchKey, info cliworker.LoginInfo, ok bool) {
	if !ok {
		return
	}
	s.mu.Lock()
	s.settled[key] = info.StartedAt
	s.mu.Unlock()
}

func (s *Service) setConnected(ctx context.Context, user int64, a ref, method string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO cli_accounts (user_id, cli, slot, state, auth_method) VALUES (?, ?, ?, 'connected', ?)
		ON CONFLICT (user_id, cli, slot) DO UPDATE SET
			state = 'connected',
			auth_method = CASE WHEN excluded.auth_method = '' THEN cli_accounts.auth_method ELSE excluded.auth_method END,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE cli_accounts.state <> 'connected' OR (excluded.auth_method <> '' AND cli_accounts.auth_method <> excluded.auth_method)`,
		user, string(a.cli), a.number(), method)
	if err == nil {
		s.notifyChanged()
	}
	return err
}

// setReconnect marks account id as needing a new login, when a turn or a check found it
// expired or revoked. An account that isn't connected stays as it is.
func (s *Service) setReconnect(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE cli_accounts SET state = 'reconnect', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ? AND state = 'connected'`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		slog.InfoContext(ctx, "cli account needs a new login", "account", id)
	}
	return nil
}

// recordLimits keeps the newest usage limits account id reported, merged by window with the
// ones kept before: a turn may report only some of them.
func (s *Service) recordLimits(ctx context.Context, id int64, limits []cliworker.Limit) error {
	return s.mergeLimits(ctx, "cli_accounts", id, limits)
}

// mergeLimits merges limits into row id of table (cli_accounts or cli_shared_accounts).
func (s *Service) mergeLimits(ctx context.Context, table string, id int64, limits []cliworker.Limit) error {
	if len(limits) == 0 {
		return nil
	}
	read, write := `SELECT limits FROM cli_accounts WHERE id = ?`,
		`UPDATE cli_accounts SET limits = ?, limits_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`
	switch table {
	case "cli_accounts":
	case "cli_shared_accounts":
		read, write = `SELECT limits FROM cli_shared_accounts WHERE id = ?`,
			`UPDATE cli_shared_accounts SET limits = ?, limits_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`
	default:
		return errors.New("mergeLimits: unknown table")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // after a commit, a no-op
	var old sql.NullString
	err = tx.QueryRowContext(ctx, read, id).Scan(&old)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // not connected: nothing to keep them on
	}
	if err != nil {
		return err
	}
	var merged []cliworker.Limit
	if old.Valid {
		_ = json.Unmarshal([]byte(old.String), &merged)
	}
	for _, l := range limits {
		replaced := false
		for i := range merged {
			if merged[i].Window == l.Window {
				merged[i], replaced = l, true
			}
		}
		if !replaced {
			merged = append(merged, l)
		}
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, write, string(raw), id); err != nil {
		return err
	}
	return tx.Commit()
}

// watch follows user's login for account a until it ends, and records it if it connected, so
// an account is connected even if no one asks about the login again.
func (s *Service) watch(user int64, a ref) {
	key := watchKey{user, a}
	s.mu.Lock()
	s.started[key] = time.Now()
	select {
	case <-s.closing:
		s.mu.Unlock()
		return
	default:
	}
	if s.watching[key] {
		s.mu.Unlock()
		return
	}
	s.watching[key] = true
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.watching, key)
			s.mu.Unlock()
		}()
		tick := time.NewTicker(s.poll)
		defer tick.Stop()
		for {
			select {
			case <-s.closing:
				return
			case <-tick.C:
			}
			info, ok := s.worker.LoginInfo(user, a.cli, a.slot)
			if !ok || !info.State.Final() {
				if !ok {
					return
				}
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := s.settle(ctx, user, info, true); err != nil {
				slog.Error("cliaccounts: recording a login", "user", user, "account", a.handle(), "err", err)
			}
			cancel()
			return
		}
	}()
}
