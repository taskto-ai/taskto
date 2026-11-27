package cliworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"taskto.ai/taskto/internal/cliowner"
)

// In container mode the worker is its own process (`taskto cli-worker`, as root, in a
// container of its own) and the server reaches it over HTTP on a Unix socket only the
// server's user can open. Handler is its side; Remote is the server's. The worker does no
// auth of its own: whoever can open the socket is the server.
//
// Every request about an account names it by its number (in the path, the query or the body) and
// by its identity and the server's boot, in the X-Taskto-Identity and X-Taskto-Boot headers
// (cliowner.Account): the worker keeps everything under the identity.
//
//	GET    /available                        → {"claude": true, …}
//	POST   /logins           {user, cli, slot?}     → LoginInfo
//	GET    /logins/{user}/{cli}?slot=               → LoginInfo, or 404
//	POST   /logins/{user}/{cli}/code?slot= {code}   → 204
//	DELETE /logins/{user}/{cli}?slot=               → 204
//	GET    /status/{user}/{cli}?slot=               → AuthStatus
//	POST   /logout           {user, cli, slot?}     → 204
//	POST   /forget           {user}                 → 204 (everything kept for a deleted account)
//	POST   /reconcile        Census                 → 204 (retires and tidies the accounts a restore left)
//
// slot is the account's (Slot); empty or left out, the first.
//	POST   /turns            TurnSpec        → {"attached": bool}
//	GET    /turns/{id}?user=U                → TurnSpec, or 404
//	GET    /turns/{id}/events?user=U&after=N → {"events": […], "done": bool}, waiting up to 25 s for one
//	POST   /turns/{id}/stop?user=U           → 204
//	GET    /permissions?user=U&conversation=C → {"requests": [PermissionRequest…]}
//	POST   /permissions/answer {user, conversation, id, allow} → 204
//
// Refusals are {"error", "code"}, the code naming the sentinel error (see remoteErrors).

var remoteErrors = map[string]error{
	"unavailable":     ErrUnavailable,
	"turn_clash":      ErrTurnClash,
	"closed":          ErrClosed,
	"no_login":        ErrNoLogin,
	"bad_code":        ErrBadCode,
	"no_code_needed":  ErrNoCodeNeeded,
	"no_permission":   ErrNoPermission,
	"forgotten":       ErrForgotten,
	"removing":        ErrRemoving,
	"busy":            ErrBusy,
	"incomplete":      ErrIncomplete,
	"unknown_account": ErrUnknownAccount,
	"full":            ErrFull,
	"stale_boot":      ErrStaleBoot,
}

// The headers that name a request's account by its identity and the server's boot.
const (
	identityHeader = "X-Taskto-Identity"
	bootHeader     = "X-Taskto-Boot"
)

// accountOf is the account a request names: user, and its headers.
func accountOf(r *http.Request, user int64) cliowner.Account {
	return cliowner.Account{User: user, Identity: r.Header.Get(identityHeader), Boot: r.Header.Get(bootHeader)}
}

// queryUser is the ?user= of a request, 0 if there is none.
func queryUser(r *http.Request) int64 {
	user, err := strconv.ParseInt(r.URL.Query().Get("user"), 10, 64)
	if err != nil || user <= 0 {
		return 0
	}
	return user
}

// remoteWait bounds how long an events request waits for a new event.
const remoteWait = 25 * time.Second

// Handler serves the worker's API.
func (w *Worker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /available", func(rw http.ResponseWriter, _ *http.Request) {
		out := map[CLI]bool{}
		for _, cli := range CLIs {
			out[cli] = w.Available(cli)
		}
		reply(rw, out)
	})
	mux.HandleFunc("POST /logins", func(rw http.ResponseWriter, r *http.Request) {
		var req struct {
			User int64 `json:"user"`
			CLI  CLI   `json:"cli"`
			Slot Slot  `json:"slot"`
		}
		if !decodeBody(rw, r, &req) {
			return
		}
		// Nothing starts for a server that has gone away meanwhile.
		if err := r.Context().Err(); err != nil {
			return
		}
		l, err := w.StartLogin(accountOf(r, req.User), req.CLI, req.Slot)
		if err != nil {
			refuse(rw, err)
			return
		}
		reply(rw, l.Info())
	})
	withLogin := func(h func(rw http.ResponseWriter, r *http.Request, a cliowner.Account, cli CLI, slot Slot)) http.HandlerFunc {
		return func(rw http.ResponseWriter, r *http.Request) {
			user, err := strconv.ParseInt(r.PathValue("user"), 10, 64)
			cli, slot := CLI(r.PathValue("cli")), Slot(r.URL.Query().Get("slot"))
			a := accountOf(r, user)
			if err != nil || !a.Valid() || !cli.Valid() || !slot.Valid() {
				refuse(rw, badRequest{errors.New("invalid account or CLI")})
				return
			}
			h(rw, r, a, cli, slot)
		}
	}
	mux.HandleFunc("GET /logins/{user}/{cli}", withLogin(func(rw http.ResponseWriter, _ *http.Request, a cliowner.Account, cli CLI, slot Slot) {
		l, ok := w.Login(a, cli, slot)
		if !ok {
			refuse(rw, ErrNoLogin)
			return
		}
		reply(rw, l.Info())
	}))
	mux.HandleFunc("POST /logins/{user}/{cli}/code", withLogin(func(rw http.ResponseWriter, r *http.Request, a cliowner.Account, cli CLI, slot Slot) {
		var req struct {
			Code string `json:"code"`
		}
		if !decodeBody(rw, r, &req) {
			return
		}
		if err := w.SubmitLoginCode(a, cli, slot, req.Code); err != nil {
			refuse(rw, err)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("DELETE /logins/{user}/{cli}", withLogin(func(rw http.ResponseWriter, _ *http.Request, a cliowner.Account, cli CLI, slot Slot) {
		w.CancelLogin(a, cli, slot)
		rw.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /status/{user}/{cli}", withLogin(func(rw http.ResponseWriter, r *http.Request, a cliowner.Account, cli CLI, slot Slot) {
		st, err := w.Status(r.Context(), a, cli, slot)
		if err != nil {
			refuse(rw, err)
			return
		}
		reply(rw, st)
	}))
	mux.HandleFunc("POST /logout", func(rw http.ResponseWriter, r *http.Request) {
		var req struct {
			User int64 `json:"user"`
			CLI  CLI   `json:"cli"`
			Slot Slot  `json:"slot"`
		}
		if !decodeBody(rw, r, &req) {
			return
		}
		a := accountOf(r, req.User)
		if !a.Valid() || !req.CLI.Valid() || !req.Slot.Valid() {
			refuse(rw, badRequest{errors.New("invalid account or CLI")})
			return
		}
		if err := w.Logout(r.Context(), a, req.CLI, req.Slot); err != nil {
			refuse(rw, err)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /forget", func(rw http.ResponseWriter, r *http.Request) {
		var req struct {
			User int64 `json:"user"`
		}
		if !decodeBody(rw, r, &req) {
			return
		}
		// A purge from before identities were kept names none (Forget).
		if err := w.Forget(r.Context(), accountOf(r, req.User)); err != nil {
			refuse(rw, err)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /reconcile", func(rw http.ResponseWriter, r *http.Request) {
		var req Census
		if !decodeBodyLimit(rw, r, &req, maxCensus) {
			return
		}
		if err := w.Reconcile(r.Context(), req); err != nil {
			refuse(rw, err)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /turns", func(rw http.ResponseWriter, r *http.Request) {
		var spec TurnSpec
		if !decodeBody(rw, r, &spec) {
			return
		}
		if err := spec.check(); err != nil {
			refuse(rw, badRequest{err})
			return
		}
		// Nothing starts for a server that has gone away meanwhile.
		if err := r.Context().Err(); err != nil {
			return
		}
		_, attached, err := w.Start(spec)
		if err != nil {
			refuse(rw, err)
			return
		}
		reply(rw, map[string]bool{"attached": attached})
	})
	// The routes of one turn: of the account the request names only, so one account never
	// reaches another's turn knowing its ID, the number's earlier owner's included.
	turnOf := func(r *http.Request) (*Turn, bool) {
		return w.TurnOf(accountOf(r, queryUser(r)), r.PathValue("id"))
	}
	mux.HandleFunc("GET /turns/{id}", func(rw http.ResponseWriter, r *http.Request) {
		t, ok := turnOf(r)
		if !ok {
			http.Error(rw, `{"error":"unknown turn"}`, http.StatusNotFound)
			return
		}
		// The turn's tokens stay in the worker: the server minted them and needs them back for nothing.
		spec := t.Spec()
		if spec.Proxy != nil {
			p := *spec.Proxy
			p.Token = ""
			spec.Proxy = &p
		}
		if spec.Tools != nil {
			tools := *spec.Tools
			tools.Token = ""
			spec.Tools = &tools
		}
		reply(rw, turnInfo{TurnSpec: spec, Over: t.Over()})
	})
	mux.HandleFunc("GET /turns/{id}/events", func(rw http.ResponseWriter, r *http.Request) {
		t, ok := turnOf(r)
		if !ok {
			http.Error(rw, `{"error":"unknown turn"}`, http.StatusNotFound)
			return
		}
		after, _ := strconv.Atoi(r.URL.Query().Get("after"))
		ctx, cancel := context.WithTimeout(r.Context(), remoteWait)
		defer cancel()
		events, done, err := t.Wait(ctx, after)
		if err != nil && r.Context().Err() != nil {
			return // the server left
		}
		if events == nil {
			events = []Event{}
		}
		reply(rw, map[string]any{"events": events, "done": done})
	})
	mux.HandleFunc("POST /turns/{id}/stop", func(rw http.ResponseWriter, r *http.Request) {
		// The turn matched is the one stopped: never another found again by its ID.
		if t, ok := turnOf(r); ok {
			t.stop()
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /permissions", func(rw http.ResponseWriter, r *http.Request) {
		conversation, err := strconv.ParseInt(r.URL.Query().Get("conversation"), 10, 64)
		a := accountOf(r, queryUser(r))
		if err != nil || !a.Valid() || conversation <= 0 {
			refuse(rw, badRequest{errors.New("invalid account or conversation")})
			return
		}
		reply(rw, map[string]any{"requests": w.Permissions(a, conversation)})
	})
	mux.HandleFunc("POST /permissions/answer", func(rw http.ResponseWriter, r *http.Request) {
		var req answerRequest
		if !decodeBody(rw, r, &req) {
			return
		}
		a := accountOf(r, req.User)
		if !a.Valid() || req.Conversation <= 0 || req.ID == "" {
			refuse(rw, badRequest{errors.New("invalid account, conversation or question")})
			return
		}
		if err := w.Answer(a, req.Conversation, req.ID, req.Allow); err != nil {
			refuse(rw, err)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	return mux
}

type answerRequest struct {
	User         int64  `json:"user"`
	Conversation int64  `json:"conversation"`
	ID           string `json:"id"`
	Allow        bool   `json:"allow"`
}

func reply(rw http.ResponseWriter, v any) {
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(v)
}

func refuse(rw http.ResponseWriter, err error) {
	status, code := http.StatusBadRequest, "invalid"
	for name, sentinel := range remoteErrors {
		// ErrIncomplete holds the errors of the users it is about (ErrBusy, say): it is the answer.
		if errors.Is(err, ErrIncomplete) && name != "incomplete" {
			continue
		}
		if errors.Is(err, sentinel) {
			code = name
			switch {
			case errors.Is(sentinel, ErrUnavailable), errors.Is(sentinel, ErrClosed), errors.Is(sentinel, ErrFull):
				status = http.StatusServiceUnavailable
			case errors.Is(sentinel, ErrNoLogin), errors.Is(sentinel, ErrNoPermission):
				status = http.StatusNotFound
			case errors.Is(sentinel, ErrForgotten):
				status = http.StatusGone
			case errors.Is(sentinel, ErrTurnClash), errors.Is(sentinel, ErrNoCodeNeeded), errors.Is(sentinel, ErrRemoving), errors.Is(sentinel, ErrBusy), errors.Is(sentinel, ErrIncomplete), errors.Is(sentinel, ErrUnknownAccount), errors.Is(sentinel, ErrStaleBoot):
				status = http.StatusConflict
			}
		}
	}
	var bad badRequest
	if code == "invalid" && !errors.As(err, &bad) {
		status, code = http.StatusInternalServerError, "failed"
		slog.Error("cliworker: request failed", "err", err)
	}
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(map[string]string{"error": err.Error(), "code": code})
}

// badRequest is the caller's mistake (a bad body, spec or argument), not the worker's trouble.
type badRequest struct{ error }

// maxCensus bounds a Reconcile's census: a number and a time for each user, some 60 bytes, so
// room for a few hundred thousand users.
const maxCensus = 32 << 20

func decodeBody(rw http.ResponseWriter, r *http.Request, v any) bool {
	return decodeBodyLimit(rw, r, v, 1<<20)
}

func decodeBodyLimit(rw http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	dec := json.NewDecoder(http.MaxBytesReader(rw, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		refuse(rw, badRequest{fmt.Errorf("invalid body: %w", err)})
		return false
	}
	return true
}

// Remote is the server's client of a worker in its own process, on a Unix socket.
type Remote struct {
	http *http.Client
}

// NewRemote returns a client of the worker listening on socket.
func NewRemote(socket string) *Remote {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		MaxIdleConnsPerHost: 16,
	}
	return &Remote{http: &http.Client{Transport: tr}}
}

func (c *Remote) do(ctx context.Context, a cliowner.Account, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://cli-worker"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.Identity != "" {
		req.Header.Set(identityHeader, a.Identity)
	}
	if a.Boot != "" {
		req.Header.Set(bootHeader, a.Boot)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&e)
		if sentinel, ok := remoteErrors[e.Code]; ok {
			return sentinel
		}
		if res.StatusCode == http.StatusNotFound {
			return ErrNotFound
		}
		return fmt.Errorf("the CLI worker: %s", e.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// ErrNotFound is a Remote's answer when the worker has no such thing (a turn, a sign-in).
var ErrNotFound = errors.New("not found")

// quick bounds the calls that answer at once.
func quick() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// Available reports whether cli is installed on the worker; false if it doesn't answer.
func (c *Remote) Available(cli CLI) bool {
	ok, err := c.Availability(cli)
	if err != nil {
		slog.Warn("cli worker: unreachable", "err", err)
	}
	return ok
}

// ErrUnreachable is a Remote's answer when the worker can't be reached.
var ErrUnreachable = errors.New("the CLI worker is unreachable")

// Availability reports whether cli can run on the worker, and ErrUnreachable when the worker
// can't be asked: an outage is not a CLI that isn't installed.
func (c *Remote) Availability(cli CLI) (bool, error) {
	ctx, cancel := quick()
	defer cancel()
	var out map[CLI]bool
	if err := c.do(ctx, cliowner.Account{}, http.MethodGet, "/available", nil, &out); err != nil {
		return false, err
	}
	return out[cli], nil
}

// StartLogin starts a's login for cli's account in slot, or returns the one in progress.
func (c *Remote) StartLogin(a cliowner.Account, cli CLI, slot Slot) (LoginInfo, error) {
	ctx, cancel := quick()
	defer cancel()
	var info LoginInfo
	err := c.do(ctx, a, http.MethodPost, "/logins", map[string]any{"user": a.User, "cli": cli, "slot": slot}, &info)
	return info, err
}

// LoginInfo returns a's login for cli's account in slot, if the worker has one; ErrNoLogin if it
// has none, and the worker's error if it can't be asked.
func (c *Remote) LoginInfo(a cliowner.Account, cli CLI, slot Slot) (LoginInfo, error) {
	ctx, cancel := quick()
	defer cancel()
	var info LoginInfo
	err := c.do(ctx, a, http.MethodGet, accountPath("/logins/", a.User, cli, slot, ""), nil, &info)
	return info, err
}

// accountPath is prefix, the user and CLI, then suffix, and the slot as a query.
func accountPath(prefix string, user int64, cli CLI, slot Slot, suffix string) string {
	p := prefix + strconv.FormatInt(user, 10) + "/" + url.PathEscape(string(cli)) + suffix
	if slot != "" {
		p += "?slot=" + url.QueryEscape(string(slot))
	}
	return p
}

// SubmitLoginCode passes the sign-in page's code to a's login.
func (c *Remote) SubmitLoginCode(a cliowner.Account, cli CLI, slot Slot, code string) error {
	ctx, cancel := quick()
	defer cancel()
	return c.do(ctx, a, http.MethodPost, accountPath("/logins/", a.User, cli, slot, "/code"), map[string]string{"code": code}, nil)
}

// CancelLogin stops a's login for cli's account in slot.
func (c *Remote) CancelLogin(a cliowner.Account, cli CLI, slot Slot) error {
	ctx, cancel := quick()
	defer cancel()
	return c.do(ctx, a, http.MethodDelete, accountPath("/logins/", a.User, cli, slot, ""), nil, nil)
}

// Status asks the worker whether a's cli is signed in, in the account in slot.
func (c *Remote) Status(ctx context.Context, a cliowner.Account, cli CLI, slot Slot) (AuthStatus, error) {
	var st AuthStatus
	err := c.do(ctx, a, http.MethodGet, accountPath("/status/", a.User, cli, slot, ""), nil, &st)
	return st, err
}

// Logout signs a's cli out of the account in slot and removes what it kept.
func (c *Remote) Logout(ctx context.Context, a cliowner.Account, cli CLI, slot Slot) error {
	return c.do(ctx, a, http.MethodPost, "/logout", map[string]any{"user": a.User, "cli": cli, "slot": slot}, nil)
}

// Forget retires a and removes what the worker keeps of it, for an account that is deleted.
func (c *Remote) Forget(ctx context.Context, a cliowner.Account) error {
	return c.do(ctx, a, http.MethodPost, "/forget", map[string]any{"user": a.User}, nil)
}

// Reconcile tells the worker which accounts exist (Worker.Reconcile).
func (c *Remote) Reconcile(ctx context.Context, census Census) error {
	return c.do(ctx, cliowner.Account{}, http.MethodPost, "/reconcile", census, nil)
}

// StartTurn starts spec's turn on the worker, or attaches to the one with its ID.
func (c *Remote) StartTurn(spec TurnSpec) (*RemoteTurn, bool, error) {
	ctx, cancel := quick()
	defer cancel()
	var out struct {
		Attached bool `json:"attached"`
	}
	if err := c.do(ctx, spec.Account(), http.MethodPost, "/turns", spec, &out); err != nil {
		return nil, false, err
	}
	return &RemoteTurn{c: c, spec: spec}, out.Attached, nil
}

// turnInfo is a turn as GET /turns/{id} shows it: its spec, without its tokens, and whether it
// has ended.
type turnInfo struct {
	TurnSpec
	Over bool `json:"over"`
}

func turnPath(a cliowner.Account, id, rest string) string {
	return "/turns/" + url.PathEscape(id) + rest + "?user=" + strconv.FormatInt(a.User, 10)
}

// FindTurn returns a's turn with id on the worker, running or finished recently; errNotFound if
// it has none, and the worker's error if it can't be asked.
func (c *Remote) FindTurn(a cliowner.Account, id string) (*RemoteTurn, error) {
	ctx, cancel := quick()
	defer cancel()
	var info turnInfo
	if err := c.do(ctx, a, http.MethodGet, turnPath(a, id, ""), nil, &info); err != nil {
		return nil, err
	}
	return &RemoteTurn{c: c, spec: info.TurnSpec, over: info.Over}, nil
}

// StopTurn stops a's turn id on the worker.
func (c *Remote) StopTurn(a cliowner.Account, id string) error {
	ctx, cancel := quick()
	defer cancel()
	return c.do(ctx, a, http.MethodPost, turnPath(a, id, "/stop"), nil, nil)
}

// Permissions returns the permission questions waiting in a's conversation.
func (c *Remote) Permissions(ctx context.Context, a cliowner.Account, conversation int64) ([]PermissionRequest, error) {
	var out struct {
		Requests []PermissionRequest `json:"requests"`
	}
	q := url.Values{"user": {strconv.FormatInt(a.User, 10)}, "conversation": {strconv.FormatInt(conversation, 10)}}
	if err := c.do(ctx, a, http.MethodGet, "/permissions?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Requests, nil
}

// Answer answers permission question id in a's conversation (ErrNoPermission if it isn't
// waiting there).
func (c *Remote) Answer(ctx context.Context, a cliowner.Account, conversation int64, id string, allow bool) error {
	return c.do(ctx, a, http.MethodPost, "/permissions/answer", answerRequest{User: a.User, Conversation: conversation, ID: id, Allow: allow}, nil)
}

// RemoteTurn is a turn on a worker in its own process.
type RemoteTurn struct {
	c    *Remote
	spec TurnSpec
	over bool
}

// Spec returns what the turn runs.
func (t *RemoteTurn) Spec() TurnSpec { return t.spec }

// Over reports whether the turn had ended when FindTurn found it (a turn StartTurn started
// hadn't).
func (t *RemoteTurn) Over() bool { return t.over }

// Wait returns the events after Seq after, waiting until there is one, the turn is over, or
// ctx ends.
func (t *RemoteTurn) Wait(ctx context.Context, after int) ([]Event, bool, error) {
	for {
		var out struct {
			Events []Event `json:"events"`
			Done   bool    `json:"done"`
		}
		err := t.c.do(ctx, t.spec.Account(), http.MethodGet, turnPath(t.spec.Account(), t.spec.ID, "/events")+"&after="+strconv.Itoa(after), nil, &out)
		if err != nil {
			if ctx.Err() != nil {
				return nil, false, ctx.Err()
			}
			return nil, false, err
		}
		if len(out.Events) > 0 || out.Done {
			return out.Events, out.Done, nil
		}
	}
}
