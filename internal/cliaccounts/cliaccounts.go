package cliaccounts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

type Mode string

const (
	Off         Mode = ""
	Local       Mode = "local"
	LocalUnsafe Mode = "local-unsafe"
	Container   Mode = "container"
)

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

type Worker interface {
	Available(cli cliworker.CLI) bool
	StartLogin(user int64, cli cliworker.CLI, slot cliworker.Slot) (cliworker.LoginInfo, error)
	LoginInfo(user int64, cli cliworker.CLI, slot cliworker.Slot) (cliworker.LoginInfo, bool)
	SubmitLoginCode(user int64, cli cliworker.CLI, slot cliworker.Slot, code string) error
	CancelLogin(user int64, cli cliworker.CLI, slot cliworker.Slot)
	Status(ctx context.Context, user int64, cli cliworker.CLI, slot cliworker.Slot) (cliworker.AuthStatus, error)
	Logout(ctx context.Context, user int64, cli cliworker.CLI, slot cliworker.Slot) error
	Forget(ctx context.Context, user int64) error
	Reconcile(ctx context.Context, live []int64, next int64, before time.Time) error

	StartTurn(spec cliworker.TurnSpec) (t Turn, attached bool, err error)
	FindTurn(id string) (Turn, bool)
	StopTurn(id string)

	Permissions(ctx context.Context, user, conversation int64) ([]cliworker.PermissionRequest, error)
	Answer(ctx context.Context, user, conversation int64, id string, allow bool) error
}

func RecordWorker(ctx context.Context, sqlDB *sql.DB, mode Mode) error {
	kind := WorkerKind(mode)
	if kind == "" {
		return nil
	}
	_, err := sqlDB.ExecContext(ctx, `INSERT INTO cli_worker_history (worker, since) VALUES (?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		ON CONFLICT (worker) DO NOTHING`, kind)
	return err
}

func WorkerKind(mode Mode) string {
	switch mode {
	case Local, LocalUnsafe:
		return "local"
	case Container:
		return "container"
	}
	return ""
}

func (s *Service) WorkerKind() string { return WorkerKind(s.mode) }

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

type Service struct {
	db     *sql.DB
	mode   Mode
	worker Worker

	mu       sync.Mutex
	watching map[watchKey]bool
	started  map[watchKey]time.Time
	settled  map[watchKey]time.Time
	locks    map[watchKey]*sync.Mutex
	changed  chan struct{}
	wg       sync.WaitGroup
	closing  chan struct{}
	poll     time.Duration

	shared     *Shared
	upstream   http.RoundTripper
	testClient *http.Client
	tokens     map[string]string
	ended      map[string]time.Time
	tools      atomic.Pointer[Tools]
	setModel   ModelSetter
	switches   map[string]string
	stops      map[string]time.Time
}

type watchKey struct {
	user int64
	ref
}

type ref struct {
	cli  cliworker.CLI
	slot cliworker.Slot
}

const maxSlot = 9

func (r ref) handle() string {
	if r.slot == "" {
		return string(r.cli)
	}
	return string(r.cli) + "-" + string(r.slot)
}

func (r ref) number() int {
	if r.slot == "" {
		return 1
	}
	return int(r.slot[0] - '0')
}

func refOf(cli cliworker.CLI, n int) ref {
	if n <= 1 || n > maxSlot {
		return ref{cli: cli}
	}
	return ref{cli: cli, slot: cliworker.Slot(strconv.Itoa(n))}
}

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

func New(sqlDB *sql.DB, mode Mode, worker Worker) *Service {
	if mode == Off {
		worker = nil
	}
	return &Service{db: sqlDB, mode: mode, worker: worker, watching: map[watchKey]bool{}, started: map[watchKey]time.Time{}, settled: map[watchKey]time.Time{}, locks: map[watchKey]*sync.Mutex{}, changed: make(chan struct{}), closing: make(chan struct{}), poll: time.Second, ended: map[string]time.Time{}, tokens: map[string]string{}, switches: map[string]string{}, stops: map[string]time.Time{}}
}

func (s *Service) Mode() string {
	if s.mode == Off {
		return "off"
	}
	return string(s.mode)
}

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

func (s *Service) Forget(ctx context.Context, user int64) error {
	if s.worker == nil {
		return ErrOff
	}
	if err := s.worker.Forget(ctx, user); err != nil {
		return fmt.Errorf("removing the CLI worker's files of user %d: %w", user, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range []map[watchKey]time.Time{s.started, s.settled} {
		for k := range m {
			if k.user == user {
				delete(m, k)
			}
		}
	}
	for k := range s.watching {
		if k.user == user {
			delete(s.watching, k)
		}
	}
	return nil
}

func (s *Service) ReconcileForgotten(ctx context.Context) error {
	if s.worker == nil {
		return nil
	}
	before := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM users
		WHERE id NOT IN (SELECT user_id FROM account_deletions WHERE purge_started_at IS NOT NULL)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var live []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		live = append(live, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	var next int64
	if err := tx.QueryRowContext(ctx, `SELECT max(coalesce((SELECT max(id) FROM users), 0), coalesce((SELECT high FROM user_ids WHERE id = 1), 0)) + 1`).Scan(&next); err != nil {
		return err
	}
	_ = tx.Rollback()
	return s.worker.Reconcile(ctx, live, next, before)
}

func (s *Service) StartReconcile() {
	if s.worker == nil {
		return
	}
	try := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return s.ReconcileForgotten(ctx)
	}
	err := try()
	if err == nil {
		return
	}
	slog.Warn("cliaccounts: telling the CLI worker which users exist failed; trying again", "err", err)
	s.mu.Lock()
	select {
	case <-s.closing:
		s.mu.Unlock()
		return
	default:
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		delay := time.Second
		for {
			select {
			case <-s.closing:
				return
			case <-time.After(delay):
			}
			if err := try(); err == nil {
				return
			}
			delay = min(delay*2, 30*time.Second)
		}
	}()
}

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

var ErrOff = errors.New("CLI workers are off on this server")

var ErrNoBoundary = errors.New("CLI workers run in local mode on this server, with no OS boundary between users, " +
	"so they are allowed only while the server has a single user; an operator can run the CLI worker container instead (docs/cli-workers.md)")

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

type State string

const (
	Connected    State = "connected"
	Reconnect    State = "reconnect"
	Disconnected State = "disconnected"
)

type Account struct {
	ID         string               `json:"id"`
	CLI        cliworker.CLI        `json:"cli"`
	Slot       int                  `json:"slot"`
	Label      string               `json:"label"`
	Name       string               `json:"name"`
	Available  bool                 `json:"available"`
	State      State                `json:"state"`
	AuthMethod string               `json:"auth_method"`
	Limits     []cliworker.Limit    `json:"limits"`
	LimitsAt   *string              `json:"limits_at"`
	Login      *cliworker.LoginInfo `json:"login"`
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

func (s *Service) load(ctx context.Context, user int64, a ref) (row, bool, error) {
	r, err := scanRow(s.db.QueryRowContext(ctx, `SELECT `+rowColumns+` FROM cli_accounts WHERE user_id = ? AND cli = ? AND slot = ?`,
		user, string(a.cli), a.number()))
	if errors.Is(err, sql.ErrNoRows) {
		return row{}, false, nil
	}
	return r, err == nil, err
}

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

func (s *Service) setMethod(ctx context.Context, user int64, a ref, method string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE cli_accounts SET auth_method = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE user_id = ? AND cli = ? AND slot = ? AND state = 'connected' AND auth_method <> ?`, method, user, string(a.cli), a.number(), method)
	return err
}

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

func (s *Service) recordLimits(ctx context.Context, id int64, limits []cliworker.Limit) error {
	return s.mergeLimits(ctx, "cli_accounts", id, limits)
}

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
	defer func() { _ = tx.Rollback() }()
	var old sql.NullString
	err = tx.QueryRowContext(ctx, read, id).Scan(&old)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
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
