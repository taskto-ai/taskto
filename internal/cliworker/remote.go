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
//	GET    /available                        → {"claude": true, …}
//	POST   /logins           {user, cli, slot?}     → LoginInfo
//	GET    /logins/{user}/{cli}?slot=               → LoginInfo, or 404
//	POST   /logins/{user}/{cli}/code?slot= {code}   → 204
//	DELETE /logins/{user}/{cli}?slot=               → 204
//	GET    /status/{user}/{cli}?slot=               → AuthStatus
//	POST   /logout           {user, cli, slot?}     → 204
//	POST   /forget           {user}                 → 204 (everything kept for a deleted account)
//	POST   /reconcile        {live, next, before}   → 204 (takes back the marks of users a restore made wrong)
//
// slot is the account's (Slot); empty or left out, the first.
//	POST   /turns            TurnSpec        → {"attached": bool}
//	GET    /turns/{id}                       → TurnSpec, or 404
//	GET    /turns/{id}/events?after=N        → {"events": […], "done": bool}, waiting up to 25 s for one
//	POST   /turns/{id}/stop                  → 204
//	GET    /permissions?user=U&conversation=C → {"requests": [PermissionRequest…]}
//	POST   /permissions/answer {user, conversation, id, allow} → 204
//
// Refusals are {"error", "code"}, the code naming the sentinel error (see remoteErrors).

var remoteErrors = map[string]error{
	"unavailable":    ErrUnavailable,
	"turn_clash":     ErrTurnClash,
	"closed":         ErrClosed,
	"no_login":       ErrNoLogin,
	"bad_code":       ErrBadCode,
	"no_code_needed": ErrNoCodeNeeded,
	"no_permission":  ErrNoPermission,
	"forgotten":      ErrForgotten,
	"removing":       ErrRemoving,
	"busy":           ErrBusy,
	"incomplete":     ErrIncomplete,
	"unknown_owner":  ErrUnknownOwner,
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
		l, err := w.StartLogin(req.User, req.CLI, req.Slot)
		if err != nil {
			refuse(rw, err)
			return
		}
		reply(rw, l.Info())
	})
	withLogin := func(h func(rw http.ResponseWriter, r *http.Request, user int64, cli CLI, slot Slot)) http.HandlerFunc {
		return func(rw http.ResponseWriter, r *http.Request) {
			user, err := strconv.ParseInt(r.PathValue("user"), 10, 64)
			cli, slot := CLI(r.PathValue("cli")), Slot(r.URL.Query().Get("slot"))
			if err != nil || user <= 0 || !cli.Valid() || !slot.Valid() {
				refuse(rw, badRequest{errors.New("invalid user, CLI or account")})
				return
			}
			h(rw, r, user, cli, slot)
		}
	}
	mux.HandleFunc("GET /logins/{user}/{cli}", withLogin(func(rw http.ResponseWriter, _ *http.Request, user int64, cli CLI, slot Slot) {
		l, ok := w.Login(user, cli, slot)
		if !ok {
			refuse(rw, ErrNoLogin)
			return
		}
		reply(rw, l.Info())
	}))
	mux.HandleFunc("POST /logins/{user}/{cli}/code", withLogin(func(rw http.ResponseWriter, r *http.Request, user int64, cli CLI, slot Slot) {
		var req struct {
			Code string `json:"code"`
		}
		if !decodeBody(rw, r, &req) {
			return
		}
		if err := w.SubmitLoginCode(user, cli, slot, req.Code); err != nil {
			refuse(rw, err)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("DELETE /logins/{user}/{cli}", withLogin(func(rw http.ResponseWriter, _ *http.Request, user int64, cli CLI, slot Slot) {
		w.CancelLogin(user, cli, slot)
		rw.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /status/{user}/{cli}", withLogin(func(rw http.ResponseWriter, r *http.Request, user int64, cli CLI, slot Slot) {
		st, err := w.Status(r.Context(), user, cli, slot)
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
		if req.User <= 0 || !req.CLI.Valid() || !req.Slot.Valid() {
			refuse(rw, badRequest{errors.New("invalid user, CLI or account")})
			return
		}
		if err := w.Logout(r.Context(), req.User, req.CLI, req.Slot); err != nil {
			refuse(rw, err)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /forget", func(rw http.ResponseWriter, r *http.Request) {
		var req struct {
			User int64 `json:"user"`
			// Identity is the account the call is meant for (internal/cliowner), if known.
			Identity string `json:"identity,omitempty"`
		}
		if !decodeBody(rw, r, &req) {
			return
		}
		if req.User <= 0 {
			refuse(rw, badRequest{errors.New("invalid user")})
			return
		}
		if err := w.Forget(cliowner.With(r.Context(), req.Identity), req.User); err != nil {
			refuse(rw, err)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /introduce", func(rw http.ResponseWriter, r *http.Request) {
		var req struct {
			User     int64  `json:"user"`
			Identity string `json:"identity"`
			Boot     string `json:"boot"`
		}
		if !decodeBody(rw, r, &req) {
			return
		}
		if err := w.Introduce(r.Context(), req.User, req.Identity, req.Boot); err != nil {
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
		_, attached, err := w.Start(spec)
		if err != nil {
			refuse(rw, err)
			return
		}
		reply(rw, map[string]bool{"attached": attached})
	})
	mux.HandleFunc("GET /turns/{id}", func(rw http.ResponseWriter, r *http.Request) {
		t, ok := w.Turn(r.PathValue("id"))
		if !ok || !turnOf(t, r) {
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
		t, ok := w.Turn(r.PathValue("id"))
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
		if t, ok := w.Turn(r.PathValue("id")); ok && turnOf(t, r) {
			w.Stop(r.PathValue("id"))
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /permissions", func(rw http.ResponseWriter, r *http.Request) {
		user, err1 := strconv.ParseInt(r.URL.Query().Get("user"), 10, 64)
		conversation, err2 := strconv.ParseInt(r.URL.Query().Get("conversation"), 10, 64)
		if err1 != nil || err2 != nil || user <= 0 || conversation <= 0 {
			refuse(rw, badRequest{errors.New("invalid user or conversation")})
			return
		}
		reply(rw, map[string]any{"requests": w.Permissions(user, conversation)})
	})
	mux.HandleFunc("POST /permissions/answer", func(rw http.ResponseWriter, r *http.Request) {
		var req answerRequest
		if !decodeBody(rw, r, &req) {
			return
		}
		if req.User <= 0 || req.Conversation <= 0 || req.ID == "" {
			refuse(rw, badRequest{errors.New("invalid user, conversation or question")})
			return
		}
		if err := w.Answer(req.User, req.Conversation, req.ID, req.Allow); err != nil {
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
			case errors.Is(sentinel, ErrUnavailable), errors.Is(sentinel, ErrClosed):
				status = http.StatusServiceUnavailable
			case errors.Is(sentinel, ErrNoLogin), errors.Is(sentinel, ErrNoPermission):
				status = http.StatusNotFound
			case errors.Is(sentinel, ErrForgotten):
				status = http.StatusGone
			case errors.Is(sentinel, ErrTurnClash), errors.Is(sentinel, ErrNoCodeNeeded), errors.Is(sentinel, ErrRemoving), errors.Is(sentinel, ErrBusy), errors.Is(sentinel, ErrIncomplete), errors.Is(sentinel, ErrUnknownOwner):
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

func (c *Remote) do(ctx context.Context, method, path string, in, out any) error {
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
			return errNotFound
		}
		return fmt.Errorf("the CLI worker: %s", e.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

var errNotFound = errors.New("not found")

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
	if err := c.do(ctx, http.MethodGet, "/available", nil, &out); err != nil {
		return false, err
	}
	return out[cli], nil
}

// StartLogin starts user's login for cli's account in slot, or returns the one in progress.
func (c *Remote) StartLogin(user int64, cli CLI, slot Slot) (LoginInfo, error) {
	ctx, cancel := quick()
	defer cancel()
	var info LoginInfo
	err := c.do(ctx, http.MethodPost, "/logins", map[string]any{"user": user, "cli": cli, "slot": slot}, &info)
	return info, err
}

// LoginInfo returns user's login for cli's account in slot, if the worker has one.
func (c *Remote) LoginInfo(user int64, cli CLI, slot Slot) (LoginInfo, bool) {
	ctx, cancel := quick()
	defer cancel()
	var info LoginInfo
	if err := c.do(ctx, http.MethodGet, accountPath("/logins/", user, cli, slot, ""), nil, &info); err != nil {
		return LoginInfo{}, false
	}
	return info, true
}

// accountPath is prefix, the user and CLI, then suffix, and the slot as a query.
func accountPath(prefix string, user int64, cli CLI, slot Slot, suffix string) string {
	p := prefix + strconv.FormatInt(user, 10) + "/" + url.PathEscape(string(cli)) + suffix
	if slot != "" {
		p += "?slot=" + url.QueryEscape(string(slot))
	}
	return p
}

// SubmitLoginCode passes the sign-in page's code to user's login.
func (c *Remote) SubmitLoginCode(user int64, cli CLI, slot Slot, code string) error {
	ctx, cancel := quick()
	defer cancel()
	return c.do(ctx, http.MethodPost, accountPath("/logins/", user, cli, slot, "/code"), map[string]string{"code": code}, nil)
}

// CancelLogin stops user's login for cli's account in slot.
func (c *Remote) CancelLogin(user int64, cli CLI, slot Slot) {
	ctx, cancel := quick()
	defer cancel()
	if err := c.do(ctx, http.MethodDelete, accountPath("/logins/", user, cli, slot, ""), nil, nil); err != nil {
		slog.Warn("cli worker: cancelling a login", "err", err)
	}
}

// Status asks the worker whether user's cli is signed in, in the account in slot.
func (c *Remote) Status(ctx context.Context, user int64, cli CLI, slot Slot) (AuthStatus, error) {
	var st AuthStatus
	err := c.do(ctx, http.MethodGet, accountPath("/status/", user, cli, slot, ""), nil, &st)
	return st, err
}

// Logout signs user's cli out of the account in slot and removes what it kept.
func (c *Remote) Logout(ctx context.Context, user int64, cli CLI, slot Slot) error {
	return c.do(ctx, http.MethodPost, "/logout", map[string]any{"user": user, "cli": cli, "slot": slot}, nil)
}

// Forget removes everything the worker keeps for user, for an account that is deleted.
// The account it is meant for goes with it when ctx carries it (internal/cliowner).
func (c *Remote) Forget(ctx context.Context, user int64) error {
	body := map[string]any{"user": user}
	if id, ok := cliowner.From(ctx); ok {
		body["identity"] = id
	}
	return c.do(ctx, http.MethodPost, "/forget", body, nil)
}

// Introduce tells the worker that user is the account with identity, for the server started as
// boot, before anything is asked of it for them (Worker.Introduce).
func (c *Remote) Introduce(ctx context.Context, user int64, identity, boot string) error {
	return c.do(ctx, http.MethodPost, "/introduce", map[string]any{"user": user, "identity": identity, "boot": boot}, nil)
}

// Reconcile tells the worker which users exist, who each is and which number comes next, so that
// it removes the directories and takes back the forgotten marks a restore of the server's data
// made wrong (Worker.Reconcile).
func (c *Remote) Reconcile(ctx context.Context, census Census) error {
	return c.do(ctx, http.MethodPost, "/reconcile", census, nil)
}

// StartTurn starts spec's turn on the worker, or attaches to the one with its ID.
func (c *Remote) StartTurn(spec TurnSpec) (*RemoteTurn, bool, error) {
	ctx, cancel := quick()
	defer cancel()
	var out struct {
		Attached bool `json:"attached"`
	}
	if err := c.do(ctx, http.MethodPost, "/turns", spec, &out); err != nil {
		return nil, false, err
	}
	return &RemoteTurn{c: c, spec: spec}, out.Attached, nil
}

// turnOf reports whether turn t is the user the request names (?user=): a turn of another user's
// is none.
func turnOf(t *Turn, r *http.Request) bool {
	user, err := strconv.ParseInt(r.URL.Query().Get("user"), 10, 64)
	return err == nil && user > 0 && t.Spec().User == user
}

// turnInfo is a turn as GET /turns/{id} shows it: its spec, without its tokens, and whether it
// has ended.
type turnInfo struct {
	TurnSpec
	Over bool `json:"over"`
}

// FindTurn returns the worker's turn with id, running or finished recently.
func (c *Remote) FindTurn(user int64, id string) (*RemoteTurn, bool) {
	ctx, cancel := quick()
	defer cancel()
	var info turnInfo
	if err := c.do(ctx, http.MethodGet, "/turns/"+url.PathEscape(id)+"?user="+strconv.FormatInt(user, 10), nil, &info); err != nil {
		return nil, false
	}
	return &RemoteTurn{c: c, spec: info.TurnSpec, over: info.Over}, true
}

// StopTurn stops user's turn id on the worker.
func (c *Remote) StopTurn(user int64, id string) {
	ctx, cancel := quick()
	defer cancel()
	if err := c.do(ctx, http.MethodPost, "/turns/"+url.PathEscape(id)+"/stop?user="+strconv.FormatInt(user, 10), nil, nil); err != nil {
		slog.Warn("cli worker: stopping a turn", "err", err)
	}
}

// Permissions returns the permission questions waiting in user's conversation.
func (c *Remote) Permissions(ctx context.Context, user, conversation int64) ([]PermissionRequest, error) {
	var out struct {
		Requests []PermissionRequest `json:"requests"`
	}
	q := url.Values{"user": {strconv.FormatInt(user, 10)}, "conversation": {strconv.FormatInt(conversation, 10)}}
	if err := c.do(ctx, http.MethodGet, "/permissions?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Requests, nil
}

// Answer answers permission question id in user's conversation (ErrNoPermission if it isn't
// waiting there).
func (c *Remote) Answer(ctx context.Context, user, conversation int64, id string, allow bool) error {
	return c.do(ctx, http.MethodPost, "/permissions/answer", answerRequest{User: user, Conversation: conversation, ID: id, Allow: allow}, nil)
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
		err := t.c.do(ctx, http.MethodGet, "/turns/"+url.PathEscape(t.spec.ID)+"/events?after="+strconv.Itoa(after), nil, &out)
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
