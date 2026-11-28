package cliaccounts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/serverkey"
)

type Shared struct {
	Key      *serverkey.Key
	Egress   egress.Policy
	ProxyURL string
}

func (s *Service) SetShared(c Shared) {
	s.shared = &c
	s.upstream = c.Egress.Transport(nil)
	s.testClient = c.Egress.Client(15 * time.Second)
}

type AccountMode string

const (
	OwnElseShared AccountMode = "own_else_shared"
	OwnOnly       AccountMode = "own"
	SharedOnly    AccountMode = "shared"
)

func (m AccountMode) valid() bool { return m == OwnElseShared || m == OwnOnly || m == SharedOnly }

func (m AccountMode) usesOwn() bool    { return m != SharedOnly }
func (m AccountMode) usesShared() bool { return m != OwnOnly }

var defaultBaseURL = map[cliworker.CLI]string{
	cliworker.Claude: "https://api.anthropic.com",
	cliworker.Codex:  "https://api.openai.com",
	cliworker.Grok:   "https://api.x.ai",
}

type SharedAccount struct {
	ID        int64             `json:"id"`
	CLI       cliworker.CLI     `json:"cli"`
	Name      string            `json:"name"`
	Label     string            `json:"label"`
	BaseURL   string            `json:"base_url"`
	Model     string            `json:"model"`
	APIKey    string            `json:"api_key"`
	Limits    []cliworker.Limit `json:"limits"`
	LimitsAt  *string           `json:"limits_at"`
	CreatedAt string            `json:"created_at"`
}

type sharedRow struct {
	SharedAccount
	workspace int64
	sealed    []byte
}

const sharedColumns = `id, workspace_id, cli, label, base_url, model, api_key, limits, limits_at, created_at`

func scanShared(sc interface{ Scan(...any) error }) (sharedRow, error) {
	var r sharedRow
	var cli string
	var limits, limitsAt sql.NullString
	err := sc.Scan(&r.ID, &r.workspace, &cli, &r.Label, &r.BaseURL, &r.Model, &r.sealed, &limits, &limitsAt, &r.CreatedAt)
	if err != nil {
		return r, err
	}
	r.CLI = cliworker.CLI(cli)
	r.Name = r.CLI.Name()
	r.APIKey = "set"
	r.Limits = []cliworker.Limit{}
	if limits.Valid {
		_ = json.Unmarshal([]byte(limits.String), &r.Limits)
	}
	if limitsAt.Valid {
		r.LimitsAt = &limitsAt.String
	}
	return r, nil
}

func (s *Service) sharedAccounts(ctx context.Context, workspace int64, cli cliworker.CLI) ([]sharedRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sharedColumns+` FROM cli_shared_accounts
		WHERE workspace_id = ? AND (? = '' OR cli = ?) ORDER BY id`, workspace, string(cli), string(cli))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sharedRow
	for rows.Next() {
		r, err := scanShared(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Service) sharedAccount(ctx context.Context, workspace, id int64) (sharedRow, bool, error) {
	r, err := scanShared(s.db.QueryRowContext(ctx, `SELECT `+sharedColumns+` FROM cli_shared_accounts
		WHERE id = ? AND workspace_id = ?`, id, workspace))
	if errors.Is(err, sql.ErrNoRows) {
		return sharedRow{}, false, nil
	}
	return r, err == nil, err
}

func sharedKeyAD(id int64) []byte {
	return []byte("taskto cli_shared_accounts " + strconv.FormatInt(id, 10) + " api_key")
}

func (s *Service) apiKey(r sharedRow) (string, error) {
	if s.shared == nil || r.sealed == nil {
		return "", errors.New("the account's key can't be read")
	}
	plain, err := s.shared.Key.Open(r.sealed, sharedKeyAD(r.ID))
	if err != nil {
		return "", errors.New("the account's key can't be read: was the server key replaced?")
	}
	return string(plain), nil
}

func (s *Service) modeOf(ctx context.Context, workspace, user int64) (AccountMode, error) {
	var m string
	err := s.db.QueryRowContext(ctx, `SELECT coalesce(
		(SELECT mode FROM cli_member_modes WHERE workspace_id = ? AND user_id = ?),
		(SELECT default_mode FROM cli_workspace_modes WHERE workspace_id = ?),
		'own_else_shared')`, workspace, user, workspace).Scan(&m)
	return AccountMode(m), err
}

func (s *Service) recordSharedLimits(ctx context.Context, id int64, limits []cliworker.Limit) error {
	return s.mergeLimits(ctx, "cli_shared_accounts", id, limits)
}

func (s *Service) usageRow(ctx context.Context, user int64, a sharedRow) (row, error) {
	r, err := scanRow(s.db.QueryRowContext(ctx, `SELECT `+rowColumns+` FROM cli_accounts WHERE user_id = ? AND shared_account_id = ?`, user, a.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return row{state: Connected, ref: ref{cli: a.CLI, slot: sharedSlot(a.ID)}}, nil
	}
	r.ref = ref{cli: a.CLI, slot: sharedSlot(a.ID)}
	return r, err
}

func (s *Service) createUsageRow(ctx context.Context, user int64, a sharedRow) (row, error) {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO cli_accounts (user_id, cli, slot, state, shared_account_id)
		VALUES (?, ?, NULL, 'connected', ?) ON CONFLICT (user_id, shared_account_id) DO NOTHING`, user, string(a.CLI), a.ID); err != nil {
		return row{}, err
	}
	return s.usageRow(ctx, user, a)
}

func sharedSlot(id int64) cliworker.Slot { return cliworker.Slot("s" + strconv.FormatInt(id, 10)) }

func sharedOfSlot(slot cliworker.Slot) int64 {
	if !slot.Shared() {
		return 0
	}
	id, _ := strconv.ParseInt(string(slot[1:]), 10, 64)
	return id
}

func (s *Service) SharedRoutes(mux *http.ServeMux, a *auth.Service) {
	member := func(h http.HandlerFunc) http.Handler { return a.RequireMember(h) }
	admin := func(h http.HandlerFunc) http.Handler { return a.RequireAdmin(h) }
	const base = "/api/workspaces/{workspace}/cli-accounts"
	mux.Handle("GET "+base, member(s.handleSharedList))
	mux.Handle("POST "+base, admin(s.handleSharedCreate))
	mux.Handle("GET "+base+"/modes", admin(s.handleModes))
	mux.Handle("PATCH "+base+"/modes", admin(s.handleSetModes))
	mux.Handle("PATCH "+base+"/{account}", admin(s.withShared(s.handleSharedUpdate)))
	mux.Handle("DELETE "+base+"/{account}", admin(s.withShared(s.handleSharedDelete)))
	mux.Handle("POST "+base+"/{account}/test", admin(s.withShared(s.handleSharedTest)))
}

func workspaceOf(r *http.Request) int64 {
	m, _ := auth.MembershipFrom(r.Context())
	return m.WorkspaceID
}

type sharedList struct {
	Enabled     bool            `json:"enabled"`
	Reason      string          `json:"reason,omitempty"`
	ReasonCode  apierr.Code     `json:"reason_code,omitempty"`
	Mode        AccountMode     `json:"mode"`
	DefaultMode AccountMode     `json:"default_mode"`
	Accounts    []SharedAccount `json:"accounts"`
}

func (s *Service) sharedUnusable(ctx context.Context) (string, apierr.Code, error) {
	switch err := s.Usable(ctx); {
	case errors.Is(err, ErrOff), errors.Is(err, ErrNoBoundary):
		return err.Error(), unusableCode(err), nil
	case err != nil:
		return "", "", err
	case s.shared == nil:
		return errSharedOff, apierr.CodeSharedAccountsOff, nil
	case s.shared.ProxyURL == "":
		return "this server has no proxy for the CLIs (-cli-proxy-listen, -cli-proxy-url), which shared accounts need: see docs/install.md",
			apierr.CodeSharedAccountsNoProxy, nil
	}
	return "", "", nil
}

const errSharedOff = "shared accounts are off on this server"

func (s *Service) handleSharedList(w http.ResponseWriter, r *http.Request) {
	ctx, ws := r.Context(), workspaceOf(r)
	reason, code, err := s.sharedUnusable(ctx)
	if err != nil {
		serverError(w, r, err)
		return
	}
	out := sharedList{Enabled: reason == "", Reason: reason, ReasonCode: code, Accounts: []SharedAccount{}}
	if out.Mode, err = s.modeOf(ctx, ws, caller(r)); err != nil {
		serverError(w, r, err)
		return
	}
	if out.DefaultMode, err = s.modeOf(ctx, ws, 0); err != nil {
		serverError(w, r, err)
		return
	}
	rows, err := s.sharedAccounts(ctx, ws, "")
	if err != nil {
		serverError(w, r, err)
		return
	}
	for _, a := range rows {
		out.Accounts = append(out.Accounts, a.SharedAccount)
	}
	writeJSON(w, http.StatusOK, out)
}

const maxShared = 20

type sharedDraft struct {
	CLI     *cliworker.CLI `json:"cli"`
	Label   *string        `json:"label"`
	BaseURL *string        `json:"base_url"`
	Model   *string        `json:"model"`
	APIKey  *string        `json:"api_key"`
}

var sharedModelPattern = regexp.MustCompile(`^[A-Za-z0-9._:/\[\]-]{1,128}$`)

func (d *sharedDraft) check() *apierr.Error {
	if d.Label != nil {
		l := strings.TrimSpace(*d.Label)
		if l == "" || utf8.RuneCountInString(l) > maxSharedLabel || strings.IndexFunc(l, unicode.IsControl) >= 0 {
			return badRequest(apierr.CodeSharedLabelInvalid, "label: 1 to 60 characters, on one line", apierr.Params{"max": maxSharedLabel})
		}
		d.Label = &l
	}
	if d.BaseURL != nil {
		u, bad := checkSharedURL(*d.BaseURL)
		if bad != nil {
			return bad
		}
		d.BaseURL = &u
	}
	if d.Model != nil {
		m := strings.TrimSpace(*d.Model)
		if m != "" && !sharedModelPattern.MatchString(m) {
			return badRequest(apierr.CodeSharedModelInvalid,
				"model: the model's name as the provider knows it, up to 128 of A-Z a-z 0-9 . _ : / [ ] -", apierr.Params{"max": 128})
		}
		d.Model = &m
	}
	if d.APIKey != nil {
		k := strings.TrimSpace(*d.APIKey)
		if len(k) < 8 || len(k) > 4096 || strings.IndexFunc(k, func(r rune) bool { return r <= ' ' || r >= 0x7f }) >= 0 {
			return badRequest(apierr.CodeAPIKeyInvalid, "api_key: 8 to 4096 printable characters, without spaces",
				apierr.Params{"min": 8, "max": 4096})
		}
		d.APIKey = &k
	}
	return nil
}

const maxSharedLabel = 60

func badRequest(code apierr.Code, msg string, params ...apierr.Params) *apierr.Error {
	return apierr.New(http.StatusBadRequest, code, msg, params...)
}

func checkSharedURL(raw string) (string, *apierr.Error) {
	bad := badRequest(apierr.CodeBaseURLInvalid, "base_url: the provider's API address (https://…), without credentials, a query or a fragment",
		apierr.Params{"max": 2048})
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 {
		return "", bad
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Opaque != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return "", bad
	}
	if u.User != nil {
		return "", badRequest(apierr.CodeBaseURLCredentials, "base_url must not contain credentials: the key goes in api_key")
	}
	if u.Scheme == "http" && !loopback(u.Hostname()) {
		return "", badRequest(apierr.CodeBaseURLInsecure, "base_url must be https, unless it is on this machine: the key would cross the network in clear")
	}
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/v1")
	u.RawPath = ""
	return u.String(), nil
}

func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Service) checkSharedEgress(ctx context.Context, base string) *apierr.Error {
	u, err := url.Parse(base)
	if err != nil {
		return badRequest(apierr.CodeBaseURLInvalid, "base_url: invalid", apierr.Params{"max": 2048})
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.shared.Egress.Check(ctx, u.Hostname(), nil); err != nil {
		if refused := egress.Refusal(err); refused != nil {
			return refused.APIError("base_url")
		}
		return badRequest(apierr.CodeBadRequest, "base_url: "+err.Error())
	}
	return nil
}

func (s *Service) sharedOn(w http.ResponseWriter) bool {
	if s.shared == nil {
		writeError(w, http.StatusConflict, apierr.CodeSharedAccountsOff, errSharedOff)
		return false
	}
	return true
}

func (s *Service) handleSharedCreate(w http.ResponseWriter, r *http.Request) {
	if !s.sharedOn(w) {
		return
	}
	var d sharedDraft
	if !readJSON(w, r, &d) {
		return
	}
	if d.CLI == nil || !d.CLI.Valid() || d.Label == nil || d.APIKey == nil {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "cli (claude, codex or grok), label and api_key are required")
		return
	}
	if d.BaseURL == nil || strings.TrimSpace(*d.BaseURL) == "" {
		u := defaultBaseURL[*d.CLI]
		d.BaseURL = &u
	}
	if d.Model == nil {
		d.Model = new(string)
	}
	if bad := d.check(); bad != nil {
		apierr.WriteError(w, bad)
		return
	}
	if bad := s.checkSharedEgress(r.Context(), *d.BaseURL); bad != nil {
		apierr.WriteError(w, bad)
		return
	}
	ws := workspaceOf(r)
	var id int64
	err := s.inTx(r.Context(), func(tx *sql.Tx) error {
		if err := auth.EnsureActorEnabled(r.Context(), tx); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(r.Context(), `SELECT count(*) FROM cli_shared_accounts WHERE workspace_id = ?`, ws).Scan(&n); err != nil {
			return err
		}
		if n >= maxShared {
			return errTooMany
		}
		if err := tx.QueryRowContext(r.Context(), `INSERT INTO cli_shared_accounts (workspace_id, cli, label, base_url, model, created_by)
			VALUES (?, ?, ?, ?, ?, ?) RETURNING id`, ws, string(*d.CLI), *d.Label, *d.BaseURL, *d.Model, caller(r)).Scan(&id); err != nil {
			return err
		}
		sealed, err := s.shared.Key.Seal([]byte(*d.APIKey), sharedKeyAD(id))
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(r.Context(), `UPDATE cli_shared_accounts SET api_key = ? WHERE id = ?`, sealed, id); err != nil {
			return err
		}
		return audit.Record(r.Context(), tx, audit.Event{Workspace: ws, Action: audit.SharedCLIAccountCreated,
			TargetType: audit.TargetSharedAccount, Target: id, Details: sharedDetails(*d.CLI, *d.BaseURL, audit.Details{"api_key_set": true})})
	})
	if errors.Is(err, auth.ErrAccountDisabled) {
		auth.WriteAccountDisabled(w)
		return
	}
	if errors.Is(err, errTooMany) {
		writeError(w, http.StatusConflict, apierr.CodeTooManySharedAccounts, fmt.Sprintf("a workspace has at most %d shared CLI accounts", maxShared),
			apierr.Params{"max": maxShared})
		return
	}
	if err != nil {
		serverError(w, r, err)
		return
	}
	a, _, err := s.sharedAccount(r.Context(), ws, id)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, a.SharedAccount)
}

var errTooMany = errors.New("too many")

func (s *Service) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

type sharedHandler func(w http.ResponseWriter, r *http.Request, a sharedRow)

func (s *Service) withShared(h sharedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.sharedOn(w) {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("account"), 10, 64)
		if err != nil || id <= 0 {
			writeError(w, http.StatusNotFound, apierr.CodeSharedAccountNotFound, "no such shared account")
			return
		}
		a, ok, err := s.sharedAccount(r.Context(), workspaceOf(r), id)
		if err != nil {
			serverError(w, r, err)
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, apierr.CodeSharedAccountNotFound, "no such shared account")
			return
		}
		h(w, r, a)
	}
}

func (s *Service) handleSharedUpdate(w http.ResponseWriter, r *http.Request, a sharedRow) {
	var d sharedDraft
	if !readJSON(w, r, &d) {
		return
	}
	if d.CLI != nil && *d.CLI != a.CLI {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "an account's CLI doesn't change: add another account")
		return
	}
	if bad := d.check(); bad != nil {
		apierr.WriteError(w, bad)
		return
	}
	if d.BaseURL != nil {
		if bad := s.checkSharedEgress(r.Context(), *d.BaseURL); bad != nil {
			apierr.WriteError(w, bad)
			return
		}
	}
	var changed sharedRow
	err := s.inTx(r.Context(), func(tx *sql.Tx) error {
		if err := auth.EnsureActorEnabled(r.Context(), tx); err != nil {
			return err
		}
		cur, err := scanShared(tx.QueryRowContext(r.Context(), `SELECT `+sharedColumns+` FROM cli_shared_accounts
			WHERE id = ? AND workspace_id = ?`, a.ID, a.workspace))
		if errors.Is(err, sql.ErrNoRows) {
			return errSharedGone
		}
		if err != nil {
			return err
		}
		if d.CLI != nil && *d.CLI != cur.CLI {
			return errCLIChanged
		}
		label, baseURL, model, sealed := cur.Label, cur.BaseURL, cur.Model, cur.sealed
		if d.Label != nil {
			label = *d.Label
		}
		if d.BaseURL != nil {
			baseURL = *d.BaseURL
		}
		if d.Model != nil {
			model = *d.Model
		}
		if d.APIKey != nil {
			if sealed, err = s.shared.Key.Seal([]byte(*d.APIKey), sharedKeyAD(cur.ID)); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(r.Context(), `UPDATE cli_shared_accounts SET label = ?, base_url = ?, model = ?, api_key = ?,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ? AND workspace_id = ?`,
			label, baseURL, model, sealed, cur.ID, cur.workspace)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			if err == nil {
				err = errSharedGone
			}
			return err
		}
		fields := audit.Changed(map[string]bool{
			"label": label != cur.Label, "base_url": baseURL != cur.BaseURL, "model": model != cur.Model, "api_key": d.APIKey != nil,
		})
		changed = cur
		return audit.Record(r.Context(), tx, audit.Event{Workspace: cur.workspace, Action: audit.SharedCLIAccountUpdated,
			TargetType: audit.TargetSharedAccount, Target: cur.ID, TargetCreated: cur.CreatedAt,
			Details: sharedDetails(cur.CLI, baseURL, audit.Details{"fields": fields, "api_key_changed": d.APIKey != nil})})
	})
	switch {
	case errors.Is(err, auth.ErrAccountDisabled):
		auth.WriteAccountDisabled(w)
		return
	case errors.Is(err, errSharedGone):
		writeError(w, http.StatusNotFound, apierr.CodeSharedAccountNotFound, "no such shared account")
		return
	case errors.Is(err, errCLIChanged):
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "an account's CLI doesn't change: add another account")
		return
	case err != nil:
		serverError(w, r, err)
		return
	}
	updated, _, err := s.sharedAccount(r.Context(), changed.workspace, changed.ID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, updated.SharedAccount)
}

var (
	errSharedGone = errors.New("no such shared account")
	errCLIChanged = errors.New("an account's CLI doesn't change")
)

func sharedDetails(cli cliworker.CLI, baseURL string, more audit.Details) audit.Details {
	d := audit.Details{"cli": string(cli)}
	if h := audit.Host(baseURL); h != "" {
		d["base_host"] = h
	}
	maps.Copy(d, more)
	return d
}

func (s *Service) handleSharedDelete(w http.ResponseWriter, r *http.Request, a sharedRow) {
	var users []int64
	err := s.inTx(r.Context(), func(tx *sql.Tx) error {
		if err := auth.EnsureActorEnabled(r.Context(), tx); err != nil {
			return err
		}
		var cli, created string
		err := tx.QueryRowContext(r.Context(), `SELECT cli, created_at FROM cli_shared_accounts WHERE id = ? AND workspace_id = ?`,
			a.ID, a.workspace).Scan(&cli, &created)
		if errors.Is(err, sql.ErrNoRows) {
			return errSharedGone
		}
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(r.Context(), `SELECT user_id FROM cli_accounts WHERE shared_account_id = ?`, a.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var u int64
			if err := rows.Scan(&u); err != nil {
				return err
			}
			users = append(users, u)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		res, err := tx.ExecContext(r.Context(), `DELETE FROM cli_shared_accounts WHERE id = ? AND workspace_id = ?`, a.ID, a.workspace)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			if err == nil {
				err = errSharedGone
			}
			return err
		}
		return audit.Record(r.Context(), tx, audit.Event{Workspace: a.workspace, Action: audit.SharedCLIAccountRemoved,
			TargetType: audit.TargetSharedAccount, Target: a.ID, TargetCreated: created,
			Details: audit.Details{"cli": cli, "members_signed_out": len(users)}})
	})
	if errors.Is(err, auth.ErrAccountDisabled) {
		auth.WriteAccountDisabled(w)
		return
	}
	if errors.Is(err, errSharedGone) {
		writeError(w, http.StatusNotFound, apierr.CodeSharedAccountNotFound, "no such shared account")
		return
	}
	if err != nil {
		serverError(w, r, err)
		return
	}
	if s.worker != nil && len(users) > 0 {
		s.wg.Add(1)
		background := context.WithoutCancel(r.Context())
		go func() {
			defer s.wg.Done()
			ctx, cancel := context.WithTimeout(background, 2*time.Minute)
			defer cancel()
			for _, u := range users {
				if err := s.worker.Logout(ctx, u, a.CLI, sharedSlot(a.ID)); err != nil {
					slog.Error("cliaccounts: clearing a shared account's directory", "account", a.ID, "user", u, "err", err)
				}
			}
		}()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleSharedTest(w http.ResponseWriter, r *http.Request, a sharedRow) {
	key, err := s.apiKey(a)
	if err != nil {
		writeJSON(w, http.StatusOK, sharedTest{Message: err.Error(), Code: apierr.CodeSharedKeyUnreadable})
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, a.BaseURL+"/v1/models", nil)
	if err != nil {
		serverError(w, r, err)
		return
	}
	authorize(req.Header, a.CLI, key)
	if a.CLI == cliworker.Claude {
		req.Header.Set("Anthropic-Version", "2023-06-01")
	}
	res, err := s.testClient.Do(req)
	if err != nil {
		test := sharedTest{Message: "the provider couldn't be reached", Code: apierr.CodeEndpointUnreachable}
		if refused := egress.Refusal(err); refused != nil {
			test = sharedTest{
				Message: "the server's egress policy doesn't allow this address: " + refused.Error(),
				Code:    apierr.CodeEgressRefused, Params: apierr.Params{"host": refused.Host},
			}
		}
		writeJSON(w, http.StatusOK, test)
		return
	}
	res.Body.Close()
	status := apierr.Params{"status": res.StatusCode}
	switch {
	case res.StatusCode < 300:
		writeJSON(w, http.StatusOK, sharedTest{OK: true, Message: "the provider accepted the key"})
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		writeJSON(w, http.StatusOK, sharedTest{Message: fmt.Sprintf("the provider refused the key (%d)", res.StatusCode),
			Code: apierr.CodeEndpointKeyRefused, Params: status})
	default:
		writeJSON(w, http.StatusOK, sharedTest{Message: fmt.Sprintf("the provider answered %d", res.StatusCode),
			Code: apierr.CodeEndpointStatus, Params: status})
	}
}

type sharedTest struct {
	OK      bool          `json:"ok"`
	Message string        `json:"message"`
	Code    apierr.Code   `json:"code,omitempty"`
	Params  apierr.Params `json:"params,omitempty"`
}

func authorize(h http.Header, cli cliworker.CLI, key string) {
	h.Del("Authorization")
	h.Del("X-Api-Key")
	if cli == cliworker.Claude {
		h.Set("X-Api-Key", key)
		return
	}
	h.Set("Authorization", "Bearer "+key)
}

type memberMode struct {
	UserID int64        `json:"user_id"`
	Name   string       `json:"name"`
	Email  string       `json:"email"`
	Mode   *AccountMode `json:"mode"`
}

func (s *Service) handleModes(w http.ResponseWriter, r *http.Request) {
	ws := workspaceOf(r)
	def, err := s.modeOf(r.Context(), ws, 0)
	if err != nil {
		serverError(w, r, err)
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT u.id, u.name, u.email, mm.mode FROM memberships m
		JOIN users u ON u.id = m.user_id
		LEFT JOIN cli_member_modes mm ON mm.workspace_id = m.workspace_id AND mm.user_id = m.user_id
		WHERE m.workspace_id = ? ORDER BY u.name, u.id`, ws)
	if err != nil {
		serverError(w, r, err)
		return
	}
	defer rows.Close()
	members := []memberMode{}
	for rows.Next() {
		var m memberMode
		var mode sql.NullString
		if err := rows.Scan(&m.UserID, &m.Name, &m.Email, &mode); err != nil {
			serverError(w, r, err)
			return
		}
		if mode.Valid {
			v := AccountMode(mode.String)
			m.Mode = &v
		}
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"default_mode": def, "members": members})
}

func (s *Service) handleSetModes(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DefaultMode *AccountMode            `json:"default_mode"`
		Members     map[string]*AccountMode `json:"members"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.DefaultMode != nil && !req.DefaultMode.valid() {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "default_mode: own_else_shared, own or shared")
		return
	}
	ws := workspaceOf(r)
	err := s.inTx(r.Context(), func(tx *sql.Tx) error {
		if err := auth.EnsureActorEnabled(r.Context(), tx); err != nil {
			return err
		}
		defaultChanged, membersChanged := false, 0
		if req.DefaultMode != nil {
			var old string
			err := tx.QueryRowContext(r.Context(), `SELECT default_mode FROM cli_workspace_modes WHERE workspace_id = ?`, ws).Scan(&old)
			if errors.Is(err, sql.ErrNoRows) {
				old, err = string(OwnElseShared), nil
			}
			if err != nil {
				return err
			}
			defaultChanged = old != string(*req.DefaultMode)
			if _, err := tx.ExecContext(r.Context(), `INSERT INTO cli_workspace_modes (workspace_id, default_mode) VALUES (?, ?)
				ON CONFLICT (workspace_id) DO UPDATE SET default_mode = excluded.default_mode`, ws, string(*req.DefaultMode)); err != nil {
				return err
			}
		}
		for key, mode := range req.Members {
			user, err := strconv.ParseInt(key, 10, 64)
			if err != nil || (mode != nil && !mode.valid()) {
				return errBadMode
			}
			var member bool
			if err := tx.QueryRowContext(r.Context(), `SELECT EXISTS (SELECT 1 FROM memberships WHERE workspace_id = ? AND user_id = ?)`, ws, user).Scan(&member); err != nil {
				return err
			}
			if !member {
				return errNotMember
			}
			var old sql.NullString
			err = tx.QueryRowContext(r.Context(), `SELECT mode FROM cli_member_modes WHERE workspace_id = ? AND user_id = ?`, ws, user).Scan(&old)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if mode == nil {
				_, err = tx.ExecContext(r.Context(), `DELETE FROM cli_member_modes WHERE workspace_id = ? AND user_id = ?`, ws, user)
				if old.Valid {
					membersChanged++
				}
			} else {
				_, err = tx.ExecContext(r.Context(), `INSERT INTO cli_member_modes (workspace_id, user_id, mode) VALUES (?, ?, ?)
					ON CONFLICT (workspace_id, user_id) DO UPDATE SET mode = excluded.mode`, ws, user, string(*mode))
				if !old.Valid || old.String != string(*mode) {
					membersChanged++
				}
			}
			if err != nil {
				return err
			}
		}
		if !defaultChanged && membersChanged == 0 {
			return nil
		}
		details := audit.Details{"members_changed": membersChanged}
		if defaultChanged {
			details["default_mode"] = string(*req.DefaultMode)
		}
		return audit.Record(r.Context(), tx, audit.Event{Workspace: ws, Action: audit.CLIModesChanged,
			TargetType: audit.TargetWorkspace, Target: ws, Details: details})
	})
	switch {
	case errors.Is(err, auth.ErrAccountDisabled):
		auth.WriteAccountDisabled(w)
		return
	case errors.Is(err, errBadMode):
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "members: user IDs, each with own_else_shared, own, shared or null")
		return
	case errors.Is(err, errNotMember):
		writeError(w, http.StatusNotFound, apierr.CodeMemberNotFound, "members: not every user is a member of this workspace")
		return
	case err != nil:
		serverError(w, r, err)
		return
	}
	s.handleModes(w, r)
}

var (
	errBadMode   = errors.New("bad mode")
	errNotMember = errors.New("not a member")
)
