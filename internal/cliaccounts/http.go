package cliaccounts

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/cliworker"
)

func (s *Service) Routes(mux *http.ServeMux, a *auth.Service) {
	me := func(h http.HandlerFunc) http.Handler { return a.RequireUser(h) }
	const base = "/api/me/cli-accounts"
	mux.Handle("GET "+base, me(s.handleList))
	mux.Handle("POST "+base+"/{account}/login", me(s.withAccount(s.handleStartLogin)))
	mux.Handle("GET "+base+"/{account}/login", me(s.withAccount(s.handleLogin)))
	mux.Handle("POST "+base+"/{account}/login/code", me(s.withAccount(s.handleCode)))
	mux.Handle("DELETE "+base+"/{account}/login", me(s.withAccount(s.handleCancel)))
	mux.Handle("POST "+base+"/{account}/check", me(s.withAccount(s.handleCheck)))
	mux.Handle("PATCH "+base+"/{account}", me(s.withAccount(s.handleUpdate)))
	mux.Handle("DELETE "+base+"/{account}", me(s.withAccount(s.handleDisconnect)))
}

func caller(r *http.Request) int64 {
	u, _ := auth.UserFrom(r.Context())
	return u.ID
}

type accountHandler func(w http.ResponseWriter, r *http.Request, user int64, a ref)

func (s *Service) withAccount(h accountHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, ok := parseHandle(r.PathValue("account"))
		if !ok {
			writeError(w, http.StatusNotFound, apierr.CodeNotFound, "unknown CLI account: it is a CLI (claude, codex, grok), or one followed by -2 to -9")
			return
		}
		if !s.usable(w, r) {
			return
		}
		if !s.worker.Available(a.cli) {
			writeError(w, http.StatusServiceUnavailable, apierr.CodeCLINotInstalled, a.cli.Name()+" is not installed on this server",
				apierr.Params{"cli": a.cli.Name()})
			return
		}
		h(w, r, caller(r), a)
	}
}

func (s *Service) usable(w http.ResponseWriter, r *http.Request) bool {
	err := s.Usable(r.Context())
	switch {
	case err == nil:
		return true
	case errors.Is(err, ErrOff), errors.Is(err, ErrNoBoundary):
		writeError(w, http.StatusConflict, unusableCode(err), err.Error())
	default:
		serverError(w, r, err)
	}
	return false
}

type list struct {
	Enabled    bool        `json:"enabled"`
	Mode       string      `json:"mode"`
	Reason     string      `json:"reason,omitempty"`
	ReasonCode apierr.Code `json:"reason_code,omitempty"`
	Accounts   []Account   `json:"accounts"`
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	out := list{Mode: string(s.mode), Accounts: []Account{}}
	if s.mode == Off {
		out.Mode = "off"
	}
	switch err := s.Usable(r.Context()); {
	case err == nil:
		out.Enabled = true
	case errors.Is(err, ErrOff), errors.Is(err, ErrNoBoundary):
		out.Reason, out.ReasonCode = err.Error(), unusableCode(err)
	default:
		serverError(w, r, err)
		return
	}
	if s.mode != Off {
		refs, err := s.listed(r.Context(), caller(r))
		if err != nil {
			serverError(w, r, err)
			return
		}
		for _, a := range refs {
			acct, err := s.account(r.Context(), caller(r), a)
			if err != nil {
				serverError(w, r, err)
				return
			}
			out.Accounts = append(out.Accounts, acct)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

const recentLogin = 20 * time.Minute

func (s *Service) listed(ctx context.Context, user int64) ([]ref, error) {
	rows, err := s.own(ctx, user, "")
	if err != nil {
		return nil, err
	}
	shown := map[ref]bool{}
	for _, cli := range cliworker.CLIs {
		shown[ref{cli: cli}] = true
	}
	for _, r := range rows {
		shown[r.ref] = true
	}
	s.mu.Lock()
	for k, at := range s.started {
		if k.user == user && time.Since(at) < recentLogin {
			shown[k.ref] = true
		}
	}
	s.mu.Unlock()
	var out []ref
	for _, cli := range cliworker.CLIs {
		for n := 1; n <= maxSlot; n++ {
			if a := refOf(cli, n); shown[a] {
				out = append(out, a)
			}
		}
	}
	return out, nil
}

func (s *Service) handleStartLogin(w http.ResponseWriter, r *http.Request, user int64, a ref) {
	info, err := s.worker.StartLogin(user, a.cli, a.slot)
	if err != nil {
		workerError(w, r, a, err)
		return
	}
	s.watch(user, a)
	slog.InfoContext(r.Context(), "cli login started", "user", user, "account", a.handle())
	writeJSON(w, http.StatusOK, info)
}

func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request, user int64, a ref) {
	info, ok := s.worker.LoginInfo(user, a.cli, a.slot)
	if !ok {
		writeError(w, http.StatusNotFound, apierr.CodeCLINoLogin, cliworker.ErrNoLogin.Error())
		return
	}
	if err := s.settle(r.Context(), user, info, false); err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Service) handleCode(w http.ResponseWriter, r *http.Request, user int64, a ref) {
	var req struct {
		Code string `json:"code"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.worker.SubmitLoginCode(user, a.cli, a.slot, req.Code); err != nil {
		workerError(w, r, a, err)
		return
	}
	info, ok := s.worker.LoginInfo(user, a.cli, a.slot)
	if !ok {
		writeError(w, http.StatusNotFound, apierr.CodeCLINoLogin, cliworker.ErrNoLogin.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, info)
}

func (s *Service) handleCancel(w http.ResponseWriter, _ *http.Request, user int64, a ref) {
	s.worker.CancelLogin(user, a.cli, a.slot)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleCheck(w http.ResponseWriter, r *http.Request, user int64, a ref) {
	st, err := s.worker.Status(r.Context(), user, a.cli, a.slot)
	if err != nil {
		workerError(w, r, a, err)
		return
	}
	unlock := s.lockAccount(watchKey{user, a})
	if st.LoggedIn {
		err = s.setConnectedIfKnown(r.Context(), user, a, st.Method)
	} else {
		var known row
		var ok bool
		if known, ok, err = s.load(r.Context(), user, a); err == nil && ok {
			err = s.setReconnect(r.Context(), known.id)
		}
	}
	unlock()
	if err != nil {
		serverError(w, r, err)
		return
	}
	acct, err := s.account(r.Context(), user, a)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, acct)
}

const maxLabel = 40

func (s *Service) handleUpdate(w http.ResponseWriter, r *http.Request, user int64, a ref) {
	var req struct {
		Label *string `json:"label"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Label == nil {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "nothing to change: label is the only field")
		return
	}
	label := strings.TrimSpace(*req.Label)
	if utf8.RuneCountInString(label) > maxLabel || strings.IndexFunc(label, unicode.IsControl) >= 0 {
		writeError(w, http.StatusBadRequest, apierr.CodeCLILabelInvalid, "label: up to 40 characters, on one line", apierr.Params{"max": maxLabel})
		return
	}
	res, err := s.db.ExecContext(r.Context(), `UPDATE cli_accounts SET label = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE user_id = ? AND cli = ? AND slot = ?`, label, user, string(a.cli), a.number())
	if err != nil {
		serverError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, apierr.CodeCLIAccountNotConnected, "this account isn't connected: connect it first")
		return
	}
	acct, err := s.account(r.Context(), user, a)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, acct)
}

func (s *Service) handleDisconnect(w http.ResponseWriter, r *http.Request, user int64, a ref) {
	ctx := context.WithoutCancel(r.Context())
	if err := s.worker.Logout(ctx, user, a.cli, a.slot); err != nil {
		workerError(w, r, a, err)
		return
	}
	key := watchKey{user, a}
	unlock := s.lockAccount(key)
	defer unlock()
	info, ok := s.worker.LoginInfo(user, a.cli, a.slot)
	s.forget(key, info, ok)
	s.mu.Lock()
	delete(s.started, key)
	s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM cli_accounts WHERE user_id = ? AND cli = ? AND slot = ?`, user, string(a.cli), a.number()); err != nil {
		serverError(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "cli account disconnected", "user", user, "account", a.handle())
	w.WriteHeader(http.StatusNoContent)
}

func workerError(w http.ResponseWriter, r *http.Request, a ref, err error) {
	switch {
	case errors.Is(err, cliworker.ErrNoLogin):
		writeError(w, http.StatusNotFound, apierr.CodeCLINoLogin, err.Error())
	case errors.Is(err, cliworker.ErrBadCode):
		writeError(w, http.StatusBadRequest, apierr.CodeCLILoginCodeInvalid, err.Error(), apierr.Params{"max": maxLoginCode})
	case errors.Is(err, cliworker.ErrNoCodeNeeded):
		writeError(w, http.StatusConflict, apierr.CodeCLILoginNoCode, err.Error())
	case errors.Is(err, cliworker.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, apierr.CodeCLINotInstalled, err.Error(), apierr.Params{"cli": a.cli.Name()})
	case errors.Is(err, cliworker.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, apierr.CodeCLIWorkerClosing, err.Error())
	case errors.Is(err, cliworker.ErrForgotten):
		writeError(w, http.StatusGone, apierr.CodeCLIAccountForgotten, err.Error())
	case errors.Is(err, cliworker.ErrRemoving):
		writeError(w, http.StatusConflict, apierr.CodeCLIAccountRemoving, err.Error())
	case errors.Is(err, cliworker.ErrBusy):
		writeError(w, http.StatusConflict, apierr.CodeCLIAccountBusy, err.Error())
	default:
		serverError(w, r, err)
	}
}

const maxLoginCode = 512

func unusableCode(err error) apierr.Code {
	if errors.Is(err, ErrNoBoundary) {
		return apierr.CodeCLIWorkersNoBoundary
	}
	return apierr.CodeCLIWorkersOff
}

const maxBody = 4 << 10

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	return readJSONLimit(w, r, v, maxBody)
}

func readJSONLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, apierr.CodeInvalidJSON, "invalid JSON body")
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, apierr.CodeInvalidJSON, "invalid JSON body: one object expected")
		return false
	}
	return true
}

func serverError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	slog.ErrorContext(r.Context(), "cliaccounts: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	apierr.Internal(w)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.Error("cliaccounts: encode response", "err", err)
		status, body = http.StatusInternalServerError, []byte(`{"error":"could not encode the response"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func writeError(w http.ResponseWriter, status int, code apierr.Code, msg string, params ...apierr.Params) {
	apierr.Write(w, status, code, msg, params...)
}
