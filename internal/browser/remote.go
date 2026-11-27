package browser

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"taskto.ai/taskto/internal/cliowner"
)

// Browsers is the users' browsers as the server uses them: a Manager in this process (local
// mode), or a Remote one in the CLI worker (container mode).
type Browsers interface {
	// Availability says which browsers can run.
	Availability(ctx context.Context) map[Kind]bool
	Status(ctx context.Context, a cliowner.Account) (map[Kind]State, error)
	Start(ctx context.Context, a cliowner.Account, kind Kind) error
	Stop(ctx context.Context, a cliowner.Account, kind Kind) error
	Do(ctx context.Context, a cliowner.Account, act Action) (Result, error)
	// OpenLive is the personal browser's display as an RFB stream, for a live view.
	OpenLive(ctx context.Context, a cliowner.Account) (io.ReadWriteCloser, error)
	// Forget stops a's browsers, closes their live views and removes their files, for a deleted
	// account (Manager.Forget). It returns nil only when the processes are gone and the files removed.
	Forget(ctx context.Context, a cliowner.Account) error
}

var _ Browsers = (*Manager)(nil)
var _ Browsers = (*Remote)(nil)

// In container mode the Manager runs in the CLI worker, and the server reaches it on the
// worker's socket, which only the server can open; the worker does no auth of its own
// (docs/cli-workers.md). Handler is the worker's side, Remote the server's:
//
//	GET  /browser/available                → {"personal": bool, "testing": bool}
// Every route under /browser/users/{user} names the account by its number and by its identity and
// the server's boot, in the X-Taskto-Identity and X-Taskto-Boot headers (cliowner.Account).
//
//	GET  /browser/users/{user}             → {"personal": State, "testing": State}
//	POST /browser/users/{user}/start  {browser}
//	POST /browser/users/{user}/stop   {browser}
//	POST /browser/users/{user}/do     Action → Result
//	GET  /browser/users/{user}/live   Upgrade: taskto-rfb → 101, then the RFB stream
//	POST /browser/users/{user}/forget stops every browser of the user and removes their files
//
// Refusals are {"error", "code"}, the code naming the sentinel error (remoteErrors) or
// "invalid" for a BadAction; the message is the Manager's, written for the agent.

// remoteErrorOrder is the order refuse tries remoteErrors in.
var remoteErrorOrder = []string{"closed", "unavailable", "busy", "not_running", "too_many_live", "no_tab", "stale_ref", "not_agent_tab", "account_closing"}

var remoteErrors = map[string]error{
	"unavailable":     ErrUnavailable,
	"busy":            ErrBusy,
	"not_running":     ErrNotRunning,
	"no_tab":          ErrNoTab,
	"not_agent_tab":   ErrNotAgentTab,
	"stale_ref":       ErrStaleRef,
	"closed":          ErrClosed,
	"too_many_live":   ErrTooManyLive,
	"account_closing": ErrAccountClosing,
}

// upgradeProtocol is the live view's protocol on the worker's socket.
const upgradeProtocol = "taskto-rfb"

// NewHandler serves m's API on the worker's socket.
func NewHandler(m *Manager) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /browser/available", func(w http.ResponseWriter, r *http.Request) {
		reply(w, m.Availability(r.Context()))
	})
	user := func(h func(w http.ResponseWriter, r *http.Request, a cliowner.Account)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			id, err := strconv.ParseInt(r.PathValue("user"), 10, 64)
			a := cliowner.Account{User: id, Identity: r.Header.Get(identityHeader), Boot: r.Header.Get(bootHeader)}
			// Only a purge from before identities were kept names none.
			if err != nil || id <= 0 || (a.Identity != "" && !a.Valid()) || (a.Identity == "" && !strings.HasSuffix(r.URL.Path, "/forget")) {
				refuse(w, badf("invalid account"))
				return
			}
			h(w, r, a)
		}
	}
	mux.HandleFunc("GET /browser/users/{user}", user(func(w http.ResponseWriter, r *http.Request, id cliowner.Account) {
		st, err := m.Status(r.Context(), id)
		if err != nil {
			refuse(w, err)
			return
		}
		reply(w, st)
	}))
	kindOf := func(w http.ResponseWriter, r *http.Request) (Kind, bool) {
		var req struct {
			Browser Kind `json:"browser"`
		}
		if !decode(w, r, &req) {
			return "", false
		}
		return req.Browser, true
	}
	mux.HandleFunc("POST /browser/users/{user}/start", user(func(w http.ResponseWriter, r *http.Request, id cliowner.Account) {
		if kind, ok := kindOf(w, r); ok {
			if err := m.Start(r.Context(), id, kind); err != nil {
				refuse(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	mux.HandleFunc("POST /browser/users/{user}/stop", user(func(w http.ResponseWriter, r *http.Request, id cliowner.Account) {
		if kind, ok := kindOf(w, r); ok {
			if err := m.Stop(r.Context(), id, kind); err != nil {
				refuse(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	mux.HandleFunc("POST /browser/users/{user}/forget", user(func(w http.ResponseWriter, r *http.Request, id cliowner.Account) {
		if err := m.Forget(r.Context(), id); err != nil {
			refuse(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /browser/users/{user}/do", user(func(w http.ResponseWriter, r *http.Request, id cliowner.Account) {
		var a Action
		if !decode(w, r, &a) {
			return
		}
		res, err := m.Do(r.Context(), id, a)
		if err != nil {
			refuse(w, err)
			return
		}
		reply(w, res)
	}))
	mux.HandleFunc("GET /browser/users/{user}/live", user(func(w http.ResponseWriter, r *http.Request, id cliowner.Account) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), upgradeProtocol) || !headerHasToken(r.Header, "Connection", "upgrade") {
			refuse(w, badf("the live view needs Connection: Upgrade and Upgrade: %s", upgradeProtocol))
			return
		}
		live, err := m.OpenLive(r.Context(), id)
		if err != nil {
			refuse(w, err)
			return
		}
		defer live.Close()
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			slog.Error("browser: hijacking the live view's connection", "err", err)
			// Only a writer that can't hijack at all is left as it was, to answer on.
			if errors.Is(err, http.ErrNotSupported) {
				refuse(w, fmt.Errorf("the live view: %w", err))
			}
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Time{})
		if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: "+upgradeProtocol+"\r\n\r\n"); err != nil {
			return
		}
		// What the server sent after its request is already in buf.
		Bridge(live, struct {
			io.Reader
			io.Writer
		}{buf.Reader, conn})
	}))
	return mux
}

// headerHasToken reports whether h's name header lists token (case-insensitive), as
// "Connection: keep-alive, Upgrade" does.
func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// Bridge copies a to b and b to a until either side ends, then closes a; the caller closes b,
// which ends the other copy. done has room for both, so neither copy's goroutine is left
// waiting.
func Bridge(a io.ReadWriteCloser, b io.ReadWriter) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(a, b)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(b, a)
		done <- struct{}{}
	}()
	<-done
	_ = a.Close()
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func refuse(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "failed"
	var bad *BadAction
	switch {
	case errors.As(err, &bad):
		status, code = http.StatusBadRequest, "invalid"
	default:
		// In a fixed order, the first match winning: an error wrapping two sentinels (none
		// does today) always gets the same code.
		for _, name := range remoteErrorOrder {
			if errors.Is(err, remoteErrors[name]) {
				status, code = http.StatusConflict, name
				break
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error(), "code": code})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		refuse(w, badf("invalid body: %v", err))
		return false
	}
	return true
}

// maxReply bounds a reply of the worker's. A snapshot is at most 40 KiB (maxSnapshot); a
// status or a list of tabs grows with the tabs, a few hundred bytes each.
const maxReply = 8 << 20

// Remote is the server's client of the browsers in the CLI worker, on its socket.
type Remote struct {
	socket string
	http   *http.Client
}

// NewRemote returns a client of the worker listening on socket.
func NewRemote(socket string) *Remote {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		MaxIdleConnsPerHost: 8,
	}
	return &Remote{socket: socket, http: &http.Client{Transport: tr}}
}

// The headers that name a request's account by its identity and the server's boot.
const (
	identityHeader = "X-Taskto-Identity"
	bootHeader     = "X-Taskto-Boot"
)

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
	setAccount(req, a)
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return remoteError(res)
	}
	// Read to the end (the reply's last newline too), so the connection is kept for reuse.
	drain := func() { _, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxReply)) }
	if out == nil {
		drain()
		return nil
	}
	reply := &io.LimitedReader{R: res.Body, N: maxReply}
	err = json.NewDecoder(reply).Decode(out)
	if err != nil && reply.N <= 0 {
		err = fmt.Errorf("the CLI worker's reply is over %d MiB", maxReply>>20)
	}
	drain()
	return err
}

// remoteError turns the worker's refusal back into the Manager's error.
func remoteError(res *http.Response) error {
	var e struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	_ = json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&e)
	if sentinel, ok := remoteErrors[e.Code]; ok {
		if e.Error != "" && e.Error != sentinel.Error() {
			return fmt.Errorf("%w: %s", sentinel, e.Error)
		}
		return sentinel
	}
	if e.Code == "invalid" {
		return &BadAction{e.Error}
	}
	if e.Error == "" {
		e.Error = res.Status
	}
	return errors.New(e.Error)
}

func userPath(user int64, rest string) string {
	return "/browser/users/" + strconv.FormatInt(user, 10) + rest
}

func setAccount(req *http.Request, a cliowner.Account) {
	if a.Identity != "" {
		req.Header.Set(identityHeader, a.Identity)
	}
	if a.Boot != "" {
		req.Header.Set(bootHeader, a.Boot)
	}
}

// Availability implements Browsers: none if the worker doesn't answer.
func (c *Remote) Availability(ctx context.Context) map[Kind]bool {
	out := map[Kind]bool{}
	if err := c.do(ctx, cliowner.Account{}, http.MethodGet, "/browser/available", nil, &out); err != nil {
		slog.Warn("browser: the CLI worker didn't say which browsers it has", "err", err)
		return map[Kind]bool{Personal: false, Testing: false}
	}
	return out
}

// Status implements Browsers.
func (c *Remote) Status(ctx context.Context, a cliowner.Account) (map[Kind]State, error) {
	var out map[Kind]State
	err := c.do(ctx, a, http.MethodGet, userPath(a.User, ""), nil, &out)
	return out, err
}

// Start implements Browsers.
func (c *Remote) Start(ctx context.Context, a cliowner.Account, kind Kind) error {
	return c.do(ctx, a, http.MethodPost, userPath(a.User, "/start"), map[string]Kind{"browser": kind}, nil)
}

// Stop implements Browsers.
func (c *Remote) Stop(ctx context.Context, a cliowner.Account, kind Kind) error {
	return c.do(ctx, a, http.MethodPost, userPath(a.User, "/stop"), map[string]Kind{"browser": kind}, nil)
}

// ErrUnreachable is a Remote's answer when the CLI worker, where the browsers run, can't be
// reached.
var ErrUnreachable = errors.New("the CLI worker is unreachable")

// Forget implements Browsers.
func (c *Remote) Forget(ctx context.Context, a cliowner.Account) error {
	return c.do(ctx, a, http.MethodPost, userPath(a.User, "/forget"), struct{}{}, nil)
}

// Do implements Browsers.
func (c *Remote) Do(ctx context.Context, a cliowner.Account, act Action) (Result, error) {
	var res Result
	err := c.do(ctx, a, http.MethodPost, userPath(a.User, "/do"), act, &res)
	return res, err
}

// OpenLive implements Browsers: an HTTP upgrade on its own connection to the worker, which
// then carries the RFB stream both ways.
func (c *Remote) OpenLive(ctx context.Context, a cliowner.Account) (io.ReadWriteCloser, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://cli-worker"+userPath(a.User, "/live"), nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	setAccount(req, a)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", upgradeProtocol)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	// The worker going away before it answers is an outage too, not a server error.
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		defer res.Body.Close()
		conn.Close()
		return nil, remoteError(res)
	}
	if !stop() {
		conn.Close()
		return nil, ctx.Err()
	}
	return &upgraded{Reader: br, Conn: conn}, nil
}

// upgraded is the live view's connection after the upgrade: what was read ahead of the
// response's end is in Reader.
type upgraded struct {
	io.Reader
	net.Conn
}

func (u *upgraded) Read(p []byte) (int, error) { return u.Reader.Read(p) }
