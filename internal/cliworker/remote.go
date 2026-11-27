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

var remoteErrors = map[string]error{
	"unavailable":        ErrUnavailable,
	"turn_clash":         ErrTurnClash,
	"closed":             ErrClosed,
	"no_login":           ErrNoLogin,
	"bad_code":           ErrBadCode,
	"no_code_needed":     ErrNoCodeNeeded,
	"no_permission":      ErrNoPermission,
	"forgotten":          ErrForgotten,
	"removing":           ErrRemoving,
	"busy":               ErrBusy,
	"incomplete":         ErrIncomplete,
	"unknown_account":    ErrUnknownAccount,
	"full":               ErrFull,
	"stale_boot":         ErrStaleBoot,
	"authority_lost":     cliowner.ErrAuthorityLost,
	"other_installation": cliowner.ErrOtherInstallation,
	"census_needed":      cliowner.ErrCensusNeeded,
}

const (
	identityHeader     = "X-Taskto-Identity"
	bootHeader         = "X-Taskto-Boot"
	installationHeader = "X-Taskto-Installation"
)

func accountOf(r *http.Request, user int64) cliowner.Account {
	return cliowner.Account{User: user, Identity: r.Header.Get(identityHeader), Boot: r.Header.Get(bootHeader), Installation: r.Header.Get(installationHeader)}
}

func queryUser(r *http.Request) int64 {
	user, err := strconv.ParseInt(r.URL.Query().Get("user"), 10, 64)
	if err != nil || user <= 0 {
		return 0
	}
	return user
}

const remoteWait = 25 * time.Second

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
		if err := w.Forget(r.Context(), accountOf(r, req.User)); err != nil {
			refuse(rw, err)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /authority", func(rw http.ResponseWriter, _ *http.Request) {
		reply(rw, w.Authority())
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
	turnOf := func(r *http.Request) (*Turn, bool) {
		return w.TurnOf(accountOf(r, queryUser(r)), r.PathValue("id"))
	}
	mux.HandleFunc("GET /turns/{id}", func(rw http.ResponseWriter, r *http.Request) {
		t, ok := turnOf(r)
		if !ok {
			http.Error(rw, `{"error":"unknown turn"}`, http.StatusNotFound)
			return
		}
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
			return
		}
		if events == nil {
			events = []Event{}
		}
		reply(rw, map[string]any{"events": events, "done": done})
	})
	mux.HandleFunc("POST /turns/{id}/stop", func(rw http.ResponseWriter, r *http.Request) {
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
			case errors.Is(sentinel, ErrTurnClash), errors.Is(sentinel, ErrNoCodeNeeded), errors.Is(sentinel, ErrRemoving), errors.Is(sentinel, ErrBusy), errors.Is(sentinel, ErrIncomplete), errors.Is(sentinel, ErrUnknownAccount), errors.Is(sentinel, ErrStaleBoot), errors.Is(sentinel, cliowner.ErrAuthorityLost), errors.Is(sentinel, cliowner.ErrOtherInstallation), errors.Is(sentinel, cliowner.ErrCensusNeeded):
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

type badRequest struct{ error }

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

type Remote struct {
	http *http.Client
}

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
	if a.Installation != "" {
		req.Header.Set(installationHeader, a.Installation)
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

var ErrNotFound = errors.New("not found")

func quick() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

func (c *Remote) Available(cli CLI) bool {
	ok, err := c.Availability(cli)
	if err != nil {
		slog.Warn("cli worker: unreachable", "err", err)
	}
	return ok
}

var ErrUnreachable = errors.New("the CLI worker is unreachable")

func (c *Remote) Availability(cli CLI) (bool, error) {
	ctx, cancel := quick()
	defer cancel()
	var out map[CLI]bool
	if err := c.do(ctx, cliowner.Account{}, http.MethodGet, "/available", nil, &out); err != nil {
		return false, err
	}
	return out[cli], nil
}

func (c *Remote) StartLogin(a cliowner.Account, cli CLI, slot Slot) (LoginInfo, error) {
	ctx, cancel := quick()
	defer cancel()
	var info LoginInfo
	err := c.do(ctx, a, http.MethodPost, "/logins", map[string]any{"user": a.User, "cli": cli, "slot": slot}, &info)
	return info, err
}

func (c *Remote) LoginInfo(a cliowner.Account, cli CLI, slot Slot) (LoginInfo, error) {
	ctx, cancel := quick()
	defer cancel()
	var info LoginInfo
	err := c.do(ctx, a, http.MethodGet, accountPath("/logins/", a.User, cli, slot, ""), nil, &info)
	return info, err
}

func accountPath(prefix string, user int64, cli CLI, slot Slot, suffix string) string {
	p := prefix + strconv.FormatInt(user, 10) + "/" + url.PathEscape(string(cli)) + suffix
	if slot != "" {
		p += "?slot=" + url.QueryEscape(string(slot))
	}
	return p
}

func (c *Remote) SubmitLoginCode(a cliowner.Account, cli CLI, slot Slot, code string) error {
	ctx, cancel := quick()
	defer cancel()
	return c.do(ctx, a, http.MethodPost, accountPath("/logins/", a.User, cli, slot, "/code"), map[string]string{"code": code}, nil)
}

func (c *Remote) CancelLogin(a cliowner.Account, cli CLI, slot Slot) error {
	ctx, cancel := quick()
	defer cancel()
	return c.do(ctx, a, http.MethodDelete, accountPath("/logins/", a.User, cli, slot, ""), nil, nil)
}

func (c *Remote) Status(ctx context.Context, a cliowner.Account, cli CLI, slot Slot) (AuthStatus, error) {
	var st AuthStatus
	err := c.do(ctx, a, http.MethodGet, accountPath("/status/", a.User, cli, slot, ""), nil, &st)
	return st, err
}

func (c *Remote) Logout(ctx context.Context, a cliowner.Account, cli CLI, slot Slot) error {
	return c.do(ctx, a, http.MethodPost, "/logout", map[string]any{"user": a.User, "cli": cli, "slot": slot}, nil)
}

func (c *Remote) Forget(ctx context.Context, a cliowner.Account) error {
	return c.do(ctx, a, http.MethodPost, "/forget", map[string]any{"user": a.User}, nil)
}

func (c *Remote) Reconcile(ctx context.Context, census Census) error {
	return c.do(ctx, cliowner.Account{}, http.MethodPost, "/reconcile", census, nil)
}

func (c *Remote) Authority(ctx context.Context) (Authority, error) {
	var out Authority
	err := c.do(ctx, cliowner.Account{}, http.MethodGet, "/authority", nil, &out)
	return out, err
}

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

type turnInfo struct {
	TurnSpec
	Over bool `json:"over"`
}

func turnPath(a cliowner.Account, id, rest string) string {
	return "/turns/" + url.PathEscape(id) + rest + "?user=" + strconv.FormatInt(a.User, 10)
}

func (c *Remote) FindTurn(a cliowner.Account, id string) (*RemoteTurn, error) {
	ctx, cancel := quick()
	defer cancel()
	var info turnInfo
	if err := c.do(ctx, a, http.MethodGet, turnPath(a, id, ""), nil, &info); err != nil {
		return nil, err
	}
	return &RemoteTurn{c: c, spec: info.TurnSpec, over: info.Over}, nil
}

func (c *Remote) StopTurn(a cliowner.Account, id string) error {
	ctx, cancel := quick()
	defer cancel()
	return c.do(ctx, a, http.MethodPost, turnPath(a, id, "/stop"), nil, nil)
}

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

func (c *Remote) Answer(ctx context.Context, a cliowner.Account, conversation int64, id string, allow bool) error {
	return c.do(ctx, a, http.MethodPost, "/permissions/answer", answerRequest{User: a.User, Conversation: conversation, ID: id, Allow: allow}, nil)
}

type RemoteTurn struct {
	c    *Remote
	spec TurnSpec
	over bool
}

func (t *RemoteTurn) Spec() TurnSpec { return t.spec }

func (t *RemoteTurn) Over() bool { return t.over }

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
