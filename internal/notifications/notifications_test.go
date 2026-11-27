package notifications_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/boards"
	"taskto.ai/taskto/internal/db"
	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/notifications"
	"taskto.ai/taskto/internal/schedule"
	"taskto.ai/taskto/internal/serverkey"
)

const fakePassword = "fake-test-password"

var setupToken = auth.NewSetupToken()

type pushServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []pushRequest
	status   map[string][]int
	delay    time.Duration
}

type pushRequest struct {
	path   string
	header http.Header
	body   []byte
}

func newPushServer(t *testing.T) *pushServer {
	t.Helper()
	p := &pushServer{status: map[string][]int{}}
	p.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		delay := p.delay
		p.requests = append(p.requests, pushRequest{path: r.URL.Path, header: r.Header.Clone(), body: body})
		code := http.StatusCreated
		if queue := p.status[r.URL.Path]; len(queue) > 0 {
			code, p.status[r.URL.Path] = queue[0], queue[1:]
		}
		p.mu.Unlock()
		time.Sleep(delay)
		w.WriteHeader(code)
	}))
	p.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	p.StartTLS()
	t.Cleanup(p.Close)
	return p
}

func (p *pushServer) got() []pushRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]pushRequest(nil), p.requests...)
}

func (p *pushServer) answer(path string, codes ...int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status[path] = codes
}

type env struct {
	t      *testing.T
	db     *sql.DB
	key    *serverkey.Key
	h      http.Handler
	svc    *notifications.Service
	cards  *boards.Service
	push   *pushServer
	skew   time.Duration
	sched  *schedule.Service
	box    *mailbox
	bodies [][]byte
	hooks  *db.Hooks
}

func newEnv(t *testing.T, tls bool) *env { return newEnvWith(t, tls, nil) }

func newEnvWith(t *testing.T, tls bool, change func(*env, *notifications.Options)) *env {
	t.Helper()
	ctx := context.Background()
	hooks := &db.Hooks{}
	sqlDB, err := db.OpenHooked(ctx, filepath.Join(t.TempDir(), db.FileName), hooks)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	key, err := serverkey.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	accounts := auth.New(sqlDB, auth.Options{HashParams: auth.Params{Memory: 64, Time: 1, Threads: 1}, SetupToken: setupToken})
	spaces := access.New(sqlDB, accounts)
	e := &env{t: t, db: sqlDB, hooks: hooks, key: key, push: newPushServer(t), cards: boards.New(sqlDB, spaces)}
	opts := notifications.Options{Key: key, Now: func() time.Time { return time.Now().Add(e.skew) }}
	if tls {
		anywhere := egress.MustParse("any")
		opts.HTTPClient, opts.Egress = e.push.Client(), &anywhere
	}
	if change != nil {
		change(e, &opts)
	}
	if e.svc, err = notifications.New(ctx, sqlDB, spaces, opts); err != nil {
		t.Fatal(err)
	}
	e.sched = schedule.New(sqlDB, spaces, schedule.Options{Clock: opts.Now})
	if err := e.svc.AttachScheduler(e.sched); err != nil {
		t.Fatal(err)
	}
	accounts.OnTimeZoneChanged(e.svc.SyncTimeZone)
	mux := http.NewServeMux()
	accounts.Routes(mux)
	spaces.Routes(mux)
	e.svc.Routes(mux, accounts)
	e.h = auth.CSRF(mux)
	return e
}

type user struct {
	e      *env
	id     int64
	cookie *http.Cookie
}

func (u *user) do(method, path string, body any) *httptest.ResponseRecorder {
	u.e.t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			u.e.t.Fatal(err)
		}
		r = httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if u.cookie != nil {
		r.AddCookie(u.cookie)
	}
	rec := httptest.NewRecorder()
	u.e.h.ServeHTTP(rec, r)
	for _, ck := range rec.Result().Cookies() {
		if ck.MaxAge >= 0 {
			u.cookie = ck
		}
	}
	u.e.bodies = append(u.e.bodies, rec.Body.Bytes())
	return rec
}

func expect(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, status, rec.Body.String())
	}
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

type meReply struct {
	User       auth.User
	Workspaces []auth.Membership
}

type world struct {
	*env
	ws                  int64
	owner, bob, mallory *user
	space               access.Space
	board               int64
}

func newWorld(t *testing.T, tls bool) *world { return worldOn(t, newEnv(t, tls)) }

func worldOn(t *testing.T, e *env) *world {
	t.Helper()
	w := &world{env: e, owner: &user{e: e}}
	rec := w.owner.do(http.MethodPost, "/api/setup", map[string]string{
		"email": "owner@example.com", "name": "Owner", "password": fakePassword, "workspace_name": "Home", "setup_token": setupToken,
	})
	expect(t, rec, http.StatusCreated)
	me := decode[meReply](t, rec)
	w.owner.id, w.ws = me.User.ID, me.Workspaces[0].WorkspaceID
	w.bob, w.mallory = w.join("Bob"), w.join("Mallory")
	rec = w.owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/spaces", w.ws), map[string]string{"name": "Team"})
	expect(t, rec, http.StatusCreated)
	w.space = decode[access.Space](t, rec)
	rec = w.owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members", w.ws, w.space.ID),
		map[string]any{"user_id": w.bob.id, "role": access.RoleEditor})
	expect(t, rec, http.StatusNoContent)
	if err := e.db.QueryRow(`INSERT INTO boards (space_id, kind, name, created_by, updated_by) VALUES (?, 'team', 'Work', ?, ?) RETURNING id`,
		w.space.ID, w.owner.id, w.owner.id).Scan(&w.board); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *world) join(name string) *user {
	w.t.Helper()
	email := strings.ToLower(name) + "@example.com"
	rec := w.owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", w.ws), map[string]string{"email": email, "role": "member"})
	expect(w.t, rec, http.StatusCreated)
	token := decode[struct{ Token string }](w.t, rec).Token
	u := &user{e: w.env}
	rec = u.do(http.MethodPost, "/api/invites/accept", map[string]string{"token": token, "name": name, "password": fakePassword})
	expect(w.t, rec, http.StatusCreated)
	u.id = decode[meReply](w.t, rec).User.ID
	return u
}

func (w *world) assign(title string, who *user) int64 {
	w.t.Helper()
	c, err := w.cards.CreateCard(context.Background(), boards.Actor{UserID: w.owner.id}, w.space.ID, w.board,
		boards.NewCard{Title: title, Assignee: &boards.Assignee{Kind: boards.AssignUser, ID: who.id}})
	if err != nil {
		w.t.Fatal(err)
	}
	return c.ID
}

func (w *world) inbox(u *user, query string) notifications.List {
	w.t.Helper()
	rec := u.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/notifications%s", w.ws, query), nil)
	expect(w.t, rec, http.StatusOK)
	return decode[notifications.List](w.t, rec)
}

func (w *world) count(table, where string, args ...any) (n int) {
	w.t.Helper()
	if err := w.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE `+where, args...).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

type device struct {
	client   *notifications.Client
	endpoint string
}

func (w *world) newDevice(name string) *device {
	return &device{client: notifications.NewClient(w.t), endpoint: w.push.URL + "/push/" + name}
}

func (d *device) body() map[string]any {
	return map[string]any{"endpoint": d.endpoint, "keys": map[string]string{"p256dh": d.client.Public(), "auth": d.client.AuthSecret()}}
}

func (w *world) subscribe(u *user, d *device) {
	w.t.Helper()
	expect(w.t, u.do(http.MethodPut, "/api/me/push/subscription", d.body()), http.StatusNoContent)
}

func (w *world) pushed(d *device) (payloads []map[string]any, requests []pushRequest) {
	w.t.Helper()
	for _, r := range w.push.got() {
		if r.path != strings.TrimPrefix(d.endpoint, w.push.URL) {
			continue
		}
		plain, err := d.client.Decrypt(r.body)
		if err != nil {
			w.t.Fatalf("the message doesn't decrypt at the device: %v", err)
		}
		var p map[string]any
		if err := json.Unmarshal(plain, &p); err != nil {
			w.t.Fatal(err)
		}
		payloads, requests = append(payloads, p), append(requests, r)
	}
	return payloads, requests
}

func TestAssignedCardIsInTheInboxWithItsTitleAndLink(t *testing.T) {
	w := newWorld(t, true)
	card := w.assign("Paint the fence", w.bob)

	in := w.inbox(w.bob, "")
	if in.Unread != 1 || len(in.Notifications) != 1 || in.More {
		t.Fatalf("inbox = %+v", in)
	}
	it := in.Notifications[0]
	wantItem := fmt.Sprintf("%d:%d:%d", w.space.ID, w.board, card)
	if it.Kind != notifications.KindCardAssigned || it.Params["title"] != "Paint the fence" || it.Read ||
		it.Target.Section != "boards" || it.Target.Item != wantItem || it.WorkspaceID != w.ws {
		t.Fatalf("item = %+v", it)
	}
	if n := w.inbox(w.owner, "").Unread + w.inbox(w.mallory, "").Unread; n != 0 {
		t.Fatalf("others were told: %d", n)
	}
	if _, err := w.cards.CreateCard(context.Background(), boards.Actor{UserID: w.bob.id}, w.space.ID, w.board,
		boards.NewCard{Title: "Mine", Assignee: &boards.Assignee{Kind: boards.AssignUser, ID: w.bob.id}}); err != nil {
		t.Fatal(err)
	}
	if w.inbox(w.bob, "").Unread != 1 {
		t.Fatal("assigning yourself made a notification")
	}
}

func TestInboxPagingUnreadAndMarkingRead(t *testing.T) {
	w := newWorld(t, true)
	for i := range 5 {
		w.assign(fmt.Sprintf("Card %d", i), w.bob)
	}
	first := w.inbox(w.bob, "?limit=2")
	if len(first.Notifications) != 2 || !first.More || first.Unread != 5 || first.Notifications[0].Params["title"] != "Card 4" {
		t.Fatalf("first page = %+v", first)
	}
	next := w.inbox(w.bob, fmt.Sprintf("?limit=2&before=%d", first.Notifications[1].ID))
	if len(next.Notifications) != 2 || !next.More || next.Notifications[0].Params["title"] != "Card 2" {
		t.Fatalf("second page = %+v", next)
	}
	for _, q := range []string{"?limit=0", "?limit=51", "?limit=x", "?before=0", "?before=x"} {
		expect(t, w.bob.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/notifications%s", w.ws, q), nil), http.StatusBadRequest)
	}

	id := first.Notifications[0].ID
	read := fmt.Sprintf("/api/workspaces/%d/notifications/%d/read", w.ws, id)
	expect(t, w.bob.do(http.MethodPost, read, map[string]any{}), http.StatusNoContent)
	expect(t, w.bob.do(http.MethodPost, read, map[string]any{}), http.StatusNoContent)
	if got := w.inbox(w.bob, "?unread=true"); got.Unread != 4 || len(got.Notifications) != 4 {
		t.Fatalf("unread = %+v", got)
	}
	var count struct{ Unread int }
	rec := w.bob.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/notifications/count", w.ws), nil)
	expect(t, rec, http.StatusOK)
	if count = decode[struct{ Unread int }](t, rec); count.Unread != 4 {
		t.Fatalf("count = %d", count.Unread)
	}
	expect(t, w.bob.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/notifications/read", w.ws), map[string]any{}), http.StatusNoContent)
	if got := w.inbox(w.bob, ""); got.Unread != 0 || got.Notifications[0].Read != true {
		t.Fatalf("after reading all: %+v", got)
	}
}

func TestNotificationsBelongToTheirRecipient(t *testing.T) {
	w := newWorld(t, true)
	w.assign("Private to Bob", w.bob)
	id := w.inbox(w.bob, "").Notifications[0].ID

	for _, u := range []*user{w.mallory, w.owner} {
		expect(t, u.do(http.MethodGet, fmt.Sprintf("/api/me/notifications/%d", id), nil), http.StatusNotFound)
		expect(t, u.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/notifications/%d/read", w.ws, id), map[string]any{}), http.StatusNotFound)
		if got := w.inbox(u, ""); len(got.Notifications) != 0 {
			t.Fatalf("%d saw %+v", u.id, got)
		}
	}
	if w.inbox(w.bob, "").Unread != 1 {
		t.Fatal("someone else marked Bob's notification read")
	}
	expect(t, w.bob.do(http.MethodGet, fmt.Sprintf("/api/me/notifications/%d", id), nil), http.StatusOK)
	expect(t, w.bob.do(http.MethodGet, "/api/me/notifications/999999", nil), http.StatusNotFound)
	stranger := &user{e: w.env}
	expect(t, stranger.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/notifications", w.ws), nil), http.StatusUnauthorized)
	expect(t, stranger.do(http.MethodGet, "/api/me/push/key", nil), http.StatusUnauthorized)
	expect(t, stranger.do(http.MethodPut, "/api/me/push/subscription", map[string]any{}), http.StatusUnauthorized)
	expect(t, w.bob.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/notifications", w.ws+100), nil), http.StatusNotFound)
}

func TestPushCarriesOnlyTheIDAndKindEncryptedToTheDevice(t *testing.T) {
	w := newWorld(t, true)
	d := w.newDevice("phone")
	w.assign("Before opting in", w.bob)
	w.subscribe(w.bob, d)
	w.assign("Fix the secret thing", w.bob)
	w.svc.DrainOnce(context.Background())

	payloads, requests := w.pushed(d)
	if len(payloads) != 1 {
		t.Fatalf("%d pushes, want only the one after opting in", len(payloads))
	}
	newest := w.inbox(w.bob, "").Notifications[0]
	if got := payloads[0]; len(got) != 2 || got["id"] != float64(newest.ID) || got["kind"] != notifications.KindCardAssigned {
		t.Fatalf("payload = %v, want only id %d and the kind", got, newest.ID)
	}
	r := requests[0]
	if bytes.Contains(r.body, []byte("secret")) || bytes.Contains(r.body, []byte("card_assigned")) {
		t.Fatal("the message is in clear on the wire")
	}
	if r.header.Get("Content-Encoding") != "aes128gcm" || r.header.Get("TTL") != "3600" || r.header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("headers = %v", r.header)
	}
	if auth := r.header.Get("Authorization"); !strings.HasPrefix(auth, "vapid t=") || !strings.Contains(auth, ", k=") {
		t.Fatalf("Authorization = %q", auth)
	}
	rec := w.bob.do(http.MethodGet, "/api/me/push/key", nil)
	expect(t, rec, http.StatusOK)
	if key := decode[struct {
		PublicKey string `json:"public_key"`
	}](t, rec).PublicKey; !strings.HasSuffix(r.header.Get("Authorization"), "k="+key) {
		t.Fatalf("Authorization = %q, public key %s", r.header.Get("Authorization"), key)
	}
	w.svc.DrainOnce(context.Background())
	if n, _ := w.pushed(d); len(n) != 1 {
		t.Fatalf("%d pushes after draining again", len(n))
	}
}

func TestNothingIsPushedWithoutOptingIn(t *testing.T) {
	w := newWorld(t, true)
	w.assign("Quiet", w.bob)
	w.svc.DrainOnce(context.Background())
	if got := w.push.got(); len(got) != 0 {
		t.Fatalf("%d pushes with no subscription", len(got))
	}
	if w.count("push_deliveries", "1=1") != 0 {
		t.Fatal("a push was queued with no subscription")
	}
	other := w.newDevice("owner-phone")
	w.subscribe(w.owner, other)
	w.assign("Still quiet for the owner", w.bob)
	w.svc.DrainOnce(context.Background())
	if got := w.push.got(); len(got) != 0 {
		t.Fatalf("a push went to a device of someone who wasn't notified: %d", len(got))
	}
}

func TestAccessRevokedAfterTheNotificationShowsNothingOfTheCard(t *testing.T) {
	w := newWorld(t, true)
	d := w.newDevice("phone")
	w.subscribe(w.bob, d)
	card := w.assign("Quarterly layoffs plan", w.bob)
	id := w.inbox(w.bob, "").Notifications[0].ID

	expect(t, w.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", w.ws, w.space.ID, w.bob.id), nil), http.StatusNoContent)

	it := w.inbox(w.bob, "").Notifications[0]
	if it.Kind != notifications.KindChanged || len(it.Params) != 0 || it.Target.Section != "home" || it.Target.Item != "" || it.ID != id {
		t.Fatalf("the item after access ended = %+v", it)
	}
	rec := w.bob.do(http.MethodGet, fmt.Sprintf("/api/me/notifications/%d", id), nil)
	expect(t, rec, http.StatusOK)
	for _, leak := range []string{"Quarterly", "layoffs", fmt.Sprintf("%d:%d:%d", w.space.ID, w.board, card), "boards", "card_assigned"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Fatalf("the notification still says %q: %s", leak, rec.Body)
		}
	}
	if list := w.bob.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/notifications", w.ws), nil); strings.Contains(list.Body.String(), "Quarterly") {
		t.Fatalf("the inbox still shows the title: %s", list.Body)
	}

	w.svc.DrainOnce(context.Background())
	payloads, requests := w.pushed(d)
	if len(payloads) != 1 || payloads[0]["kind"] != notifications.KindChanged || len(payloads[0]) != 2 {
		t.Fatalf("payload = %v, want a push that says only that something changed", payloads)
	}
	if bytes.Contains(requests[0].body, []byte("Quarterly")) {
		t.Fatal("the title is on the wire")
	}
	expect(t, w.bob.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces/%d/boards/%d/cards/%d", w.ws, w.space.ID, w.board, card), nil), http.StatusNotFound)
}

func TestAnAppsFailedEffectNotificationNamesTheAppOnlyWhileItIsViewable(t *testing.T) {
	w := newWorld(t, true)
	var app int64
	if err := w.db.QueryRow(`INSERT INTO apps (space_id, name, definition) VALUES (?, 'Inspections', '{}') RETURNING id`, w.space.ID).Scan(&app); err != nil {
		t.Fatal(err)
	}
	tx, err := w.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	notice := notifications.Notice{UserID: w.bob.id, WorkspaceID: w.ws, Kind: notifications.KindAppEffectFailed,
		Ref: notifications.Ref{Space: w.space.ID, App: app, Effect: 7}, DedupeKey: "effect:7:1"}
	if err := notifications.Add(context.Background(), tx, notice); err != nil {
		t.Fatal(err)
	}
	if err := notifications.Add(context.Background(), tx, notice); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got := w.inbox(w.bob, "").Notifications
	if len(got) != 1 || got[0].Kind != notifications.KindAppEffectFailed || got[0].Params["app"] != "Inspections" ||
		got[0].Target.Section != "apps" || got[0].Target.Item != fmt.Sprintf("%d-%d", w.space.ID, app) {
		t.Fatalf("inbox = %+v", got)
	}
	if _, err := w.db.Exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, w.space.ID, w.bob.id); err != nil {
		t.Fatal(err)
	}
	got = w.inbox(w.bob, "").Notifications
	if len(got) != 1 || got[0].Kind != notifications.KindChanged || len(got[0].Params) != 0 {
		t.Fatalf("inbox after losing the space = %+v", got)
	}
}

func TestAddRefusesWhoIsNotInTheWorkspaceAndUnknownKinds(t *testing.T) {
	w := newWorld(t, true)
	tx, _ := w.db.Begin()
	defer func() { _ = tx.Rollback() }()
	if err := notifications.Add(context.Background(), tx, notifications.Notice{UserID: w.bob.id, WorkspaceID: w.ws + 50, Kind: notifications.KindCardAssigned}); err != nil {
		t.Fatal(err)
	}
	if err := notifications.Add(context.Background(), tx, notifications.Notice{UserID: w.bob.id, WorkspaceID: w.ws, Kind: notifications.KindChanged}); err == nil {
		t.Fatal("a notification of the generic kind was stored")
	}
	var n int
	_ = tx.QueryRow(`SELECT count(*) FROM notifications`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d notifications for a workspace the person isn't in", n)
	}
}

func TestSubscriptionIsRemovedWhenThePushServiceSaysItIsGone(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			w := newWorld(t, true)
			d, keep := w.newDevice("gone"), w.newDevice("kept")
			w.subscribe(w.bob, d)
			w.subscribe(w.bob, keep)
			w.push.answer("/push/gone", code)
			w.assign("Ping", w.bob)
			w.svc.DrainOnce(context.Background())
			if w.count("push_subscriptions", "endpoint = ?", d.endpoint) != 0 {
				t.Fatalf("a subscription the push service answered %d to is still kept", code)
			}
			if w.count("push_subscriptions", "endpoint = ?", keep.endpoint) != 1 {
				t.Fatal("the other device's subscription went too")
			}
			if n, _ := w.pushed(keep); len(n) != 1 {
				t.Fatalf("the other device got %d pushes", len(n))
			}
			if w.count("push_deliveries", "state = 'pending'") != 0 {
				t.Fatal("a push is still queued for a subscription that is gone")
			}
		})
	}
}

func TestPushesAreRetriedWithBoundsAndNotRetriedWhenRefused(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, true)
	flaky, refused, down := w.newDevice("flaky"), w.newDevice("refused"), w.newDevice("down")
	for _, d := range []*device{flaky, refused, down} {
		w.subscribe(w.bob, d)
	}
	w.push.answer("/push/flaky", http.StatusServiceUnavailable, http.StatusTooManyRequests)
	w.push.answer("/push/refused", http.StatusForbidden)
	w.push.answer("/push/down", 500, 500, 500, 500, 500, 500, 500)
	w.assign("Ping", w.bob)

	w.svc.DrainOnce(ctx)
	reached := func(d *device) int { _, r := w.pushed(d); return len(r) }
	if reached(flaky) != 1 || reached(refused) != 1 || reached(down) != 1 {
		t.Fatalf("tries = %d, %d, %d", reached(flaky), reached(refused), reached(down))
	}
	if w.count("push_deliveries", "state = 'failed'") != 1 || w.count("push_deliveries", "state = 'pending'") != 2 {
		t.Fatal("a refused push should have failed and the others wait")
	}
	w.svc.DrainOnce(ctx)
	if reached(flaky) != 1 {
		t.Fatal("a push was tried again before its wait was over")
	}
	for _, wait := range []time.Duration{time.Minute, 3 * time.Minute, 10 * time.Minute, 40 * time.Minute, 2 * time.Hour} {
		w.skew += wait
		w.svc.DrainOnce(ctx)
	}
	if reached(flaky) != 3 {
		t.Fatalf("a flaky service was tried %d times, want 3 (two refusals, then it took it)", reached(flaky))
	}
	if reached(down) != 5 {
		t.Fatalf("a service that never answers was tried %d times, want the bound of 5", reached(down))
	}
	if reached(refused) != 1 {
		t.Fatalf("a refused push was tried %d times", reached(refused))
	}
	if w.count("push_deliveries", "state = 'pending'") != 0 || w.count("push_deliveries", "state = 'sent'") != 1 ||
		w.count("push_deliveries", "state = 'failed'") != 2 {
		t.Fatal("the pushes did not all settle: one sent, two given up")
	}
	if w.count("push_subscriptions", "1=1") != 3 {
		t.Fatal("a subscription was removed for a failed push")
	}
}

func TestAPushIsNotSentForANotificationThatWasReadOrIsStale(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, true)
	d := w.newDevice("phone")
	w.subscribe(w.bob, d)
	w.assign("Read already", w.bob)
	id := w.inbox(w.bob, "").Notifications[0].ID
	expect(t, w.bob.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/notifications/%d/read", w.ws, id), map[string]any{}), http.StatusNoContent)
	w.svc.DrainOnce(ctx)
	if len(w.push.got()) != 0 {
		t.Fatal("a notification that was read was pushed")
	}
	w.assign("Stale", w.bob)
	w.skew = 2 * time.Hour
	w.svc.DrainOnce(ctx)
	if len(w.push.got()) != 0 {
		t.Fatal("a push older than its lifetime was sent")
	}
}

func TestSigningOutRemovesThatDevicesSubscription(t *testing.T) {
	w := newWorld(t, true)
	d, other := w.newDevice("phone"), w.newDevice("laptop")
	w.subscribe(w.bob, d)
	laptop := &user{e: w.env}
	rec := laptop.do(http.MethodPost, "/api/login", map[string]string{"email": "bob@example.com", "password": fakePassword})
	expect(t, rec, http.StatusOK)
	laptop.id = w.bob.id
	w.subscribe(laptop, other)
	if w.count("push_subscriptions", "user_id = ?", w.bob.id) != 2 {
		t.Fatal("both devices should be subscribed")
	}

	expect(t, w.bob.do(http.MethodPost, "/api/logout", nil), http.StatusNoContent)
	if w.count("push_subscriptions", "endpoint = ?", d.endpoint) != 0 {
		t.Fatal("the signed-out device is still subscribed")
	}
	if w.count("push_subscriptions", "endpoint = ?", other.endpoint) != 1 {
		t.Fatal("another device's subscription went with this one's sign-out")
	}
	w.assign("After sign-out", w.bob)
	w.svc.DrainOnce(context.Background())
	if n, _ := w.pushed(d); len(n) != 0 {
		t.Fatal("a push went to a device that signed out")
	}
	if n, _ := w.pushed(other); len(n) != 1 {
		t.Fatalf("the other device got %d pushes", len(n))
	}
}

func TestSessionsEndingRemoveSubscriptions(t *testing.T) {
	w := newWorld(t, true)
	d := w.newDevice("phone")
	w.subscribe(w.bob, d)
	if _, err := w.db.Exec(`UPDATE sessions SET expires_at = '2000-01-01T00:00:00.000Z' WHERE user_id = ?`, w.bob.id); err != nil {
		t.Fatal(err)
	}
	extra := &user{e: w.env}
	expect(t, extra.do(http.MethodPost, "/api/login", map[string]string{"email": "mallory@example.com", "password": fakePassword}), http.StatusOK)
	if w.count("push_subscriptions", "1=1") != 0 {
		t.Fatal("a subscription outlived its session")
	}
}

func TestLeavingAWorkspaceStopsItsNotificationsAndAtTheLastOneEndsPush(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, true)
	d := w.newDevice("phone")
	w.subscribe(w.bob, d)
	var second int64
	if err := w.db.QueryRow(`INSERT INTO workspaces (name) VALUES ('Other') RETURNING id`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if _, err := w.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, second, w.bob.id); err != nil {
		t.Fatal(err)
	}
	w.assign("Home card", w.bob)
	if w.count("push_deliveries", "state = 'pending'") != 1 {
		t.Fatal("no push queued")
	}

	expect(t, w.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", w.ws, w.bob.id), nil), http.StatusNoContent)
	if w.count("notifications", "user_id = ?", w.bob.id) != 0 || w.count("push_deliveries", "1=1") != 0 {
		t.Fatal("notifications of a workspace the person left are still there, or pushed")
	}
	w.svc.DrainOnce(ctx)
	if len(w.push.got()) != 0 {
		t.Fatal("a push of a workspace the person left was sent")
	}
	if w.count("push_subscriptions", "user_id = ?", w.bob.id) != 1 {
		t.Fatal("the device was unsubscribed though Bob is still in a workspace")
	}
	if _, err := w.db.Exec(`DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, second, w.bob.id); err != nil {
		t.Fatal(err)
	}
	if w.count("push_subscriptions", "user_id = ?", w.bob.id) != 0 {
		t.Fatal("a person in no workspace still has push subscriptions")
	}
}

func TestSubscriptionsAreValidatedBoundedAndOwnedByTheirPerson(t *testing.T) {
	w := newWorld(t, true)
	good := w.newDevice("phone")
	put := func(u *user, mutate func(map[string]any)) *httptest.ResponseRecorder {
		body := good.body()
		mutate(body)
		return u.do(http.MethodPut, "/api/me/push/subscription", body)
	}
	codeOf := func(rec *httptest.ResponseRecorder) string {
		return decode[struct{ Code string }](t, rec).Code
	}
	bad := []struct {
		name   string
		mutate func(map[string]any)
		code   string
	}{
		{"http", func(b map[string]any) { b["endpoint"] = "http://push.example.net/x" }, "push_endpoint_invalid"},
		{"no scheme", func(b map[string]any) { b["endpoint"] = "push.example.net/x" }, "push_endpoint_invalid"},
		{"empty", func(b map[string]any) { b["endpoint"] = "" }, "push_endpoint_invalid"},
		{"too long", func(b map[string]any) { b["endpoint"] = "https://push.example.net/" + strings.Repeat("a", 2100) }, "push_endpoint_invalid"},
		{"credentials", func(b map[string]any) { b["endpoint"] = "https://user:pass@push.example.net/x" }, "push_endpoint_invalid"},
		{"fragment", func(b map[string]any) { b["endpoint"] = "https://push.example.net/x#y" }, "push_endpoint_invalid"},
		{"file", func(b map[string]any) { b["endpoint"] = "file:///etc/passwd" }, "push_endpoint_invalid"},
		{"short key", func(b map[string]any) {
			b["keys"] = map[string]string{"p256dh": "AAAA", "auth": good.client.AuthSecret()}
		}, "push_keys_invalid"},
		{"key off the curve", func(b map[string]any) {
			off := make([]byte, 65)
			off[0] = 4
			b["keys"] = map[string]string{"p256dh": base64.RawURLEncoding.EncodeToString(off), "auth": good.client.AuthSecret()}
		}, "push_keys_invalid"},
		{"short auth", func(b map[string]any) { b["keys"] = map[string]string{"p256dh": good.client.Public(), "auth": "AAAA"} }, "push_keys_invalid"},
		{"not base64", func(b map[string]any) { b["keys"] = map[string]string{"p256dh": "***", "auth": "***"} }, "push_keys_invalid"},
	}
	for _, c := range bad {
		rec := put(w.bob, c.mutate)
		if rec.Code != http.StatusBadRequest || codeOf(rec) != c.code {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
	}
	expect(t, w.bob.do(http.MethodPut, "/api/me/push/subscription", map[string]any{"endpoint": good.endpoint, "unknown": 1}), http.StatusBadRequest)
	if w.count("push_subscriptions", "1=1") != 0 {
		t.Fatal("an invalid subscription was stored")
	}

	w.subscribe(w.bob, good)
	w.subscribe(w.bob, good)
	if w.count("push_subscriptions", "user_id = ?", w.bob.id) != 1 {
		t.Fatal("subscribing twice made two")
	}
	var sealed []byte
	if err := w.db.QueryRow(`SELECT auth_sealed FROM push_subscriptions`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, good.client.Auth) {
		t.Fatal("the auth secret is stored in clear")
	}

	expect(t, w.mallory.do(http.MethodDelete, "/api/me/push/subscription", map[string]any{"endpoint": good.endpoint}), http.StatusNoContent)
	if w.count("push_subscriptions", "1=1") != 1 {
		t.Fatal("another person removed Bob's device")
	}
	expect(t, w.bob.do(http.MethodDelete, "/api/me/push/subscription", map[string]any{"endpoint": good.endpoint}), http.StatusNoContent)
	expect(t, w.bob.do(http.MethodDelete, "/api/me/push/subscription", map[string]any{"endpoint": good.endpoint}), http.StatusNoContent)
	if w.count("push_subscriptions", "1=1") != 0 {
		t.Fatal("the device was not removed")
	}

	w.subscribe(w.bob, good)
	w.assign("For Bob", w.bob)
	w.subscribe(w.mallory, good)
	if w.count("push_subscriptions", "user_id = ?", w.mallory.id) != 1 || w.count("push_deliveries", "1=1") != 0 {
		t.Fatal("the device did not change hands cleanly")
	}
	w.svc.DrainOnce(context.Background())
	if len(w.push.got()) != 0 {
		t.Fatal("Mallory's browser got Bob's notification")
	}

	for i := range notifications.MaxSubscriptions {
		w.subscribe(w.bob, w.newDevice(fmt.Sprintf("d%d", i)))
	}
	rec := put(w.bob, func(b map[string]any) { b["endpoint"] = w.push.URL + "/push/one-too-many" })
	if rec.Code != http.StatusConflict || codeOf(rec) != "push_too_many_subscriptions" {
		t.Fatalf("past the limit: %d %s", rec.Code, rec.Body)
	}
	w.subscribe(w.bob, w.newDevice("d0"))
}

func TestAnEndpointOnThePrivateNetworkIsRefusedAtRegistrationAndNeverConnectedTo(t *testing.T) {
	public := newWorld(t, false)
	d := public.newDevice("loopback")
	for _, endpoint := range []string{d.endpoint, "https://169.254.169.254/latest", "https://[::1]:8443/x", "https://10.1.2.3/x", "https://localhost/x"} {
		body := d.body()
		body["endpoint"] = endpoint
		rec := public.bob.do(http.MethodPut, "/api/me/push/subscription", body)
		if rec.Code != http.StatusBadRequest || decode[struct{ Code string }](t, rec).Code != "egress_refused" {
			t.Errorf("%s: %d %s", endpoint, rec.Code, rec.Body)
		}
	}
	if public.count("push_subscriptions", "1=1") != 0 {
		t.Fatal("a private endpoint was stored")
	}

	w := newWorld(t, true)
	d = w.newDevice("loopback")
	w.subscribe(w.bob, d)
	w.assign("Ping", w.bob)
	strict, err := notifications.New(context.Background(), w.db, access.New(w.db, nil), notifications.Options{Key: w.key})
	if err != nil {
		t.Fatal(err)
	}
	strict.DrainOnce(context.Background())
	if len(w.push.got()) != 0 {
		t.Fatal("the server connected to a loopback address")
	}
	if w.count("push_deliveries", "state = 'failed' AND attempts = 1") != 1 {
		t.Fatal("a refused address should fail the push at once, without retries")
	}
}

func TestTheVAPIDPrivateKeyIsNeverReturnedOrLogged(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	w := newWorld(t, true)
	d := w.newDevice("phone")
	w.subscribe(w.bob, d)
	w.push.answer("/push/phone", http.StatusForbidden, 500)
	w.assign("Ping", w.bob)
	w.svc.DrainOnce(context.Background())
	w.assign("Ping again", w.bob)
	w.svc.DrainOnce(context.Background())
	w.skew += time.Hour
	w.svc.DrainOnce(context.Background())
	expect(t, w.bob.do(http.MethodGet, "/api/me/push/key", nil), http.StatusOK)

	var sealed []byte
	if err := w.db.QueryRow(`SELECT private_sealed FROM push_vapid`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	private, err := w.key.Open(sealed, []byte("taskto push_vapid v1"))
	if err != nil || len(private) != 32 {
		t.Fatalf("the sealed private key: %d bytes, %v", len(private), err)
	}
	forms := [][]byte{private, []byte(hex.EncodeToString(private)), []byte(base64.RawURLEncoding.EncodeToString(private)), []byte(base64.StdEncoding.EncodeToString(private))}
	var everything []byte
	for _, b := range w.bodies {
		everything = append(everything, b...)
	}
	for _, r := range w.push.got() {
		everything = append(everything, r.body...)
		everything = append(everything, fmt.Sprint(r.header)...)
	}
	everything = append(everything, logs.Bytes()...)
	for _, form := range forms {
		if bytes.Contains(everything, form) {
			t.Fatal("the VAPID private key is in an API answer, a push or the log")
		}
	}
	if strings.Contains(logs.String(), "/push/phone") {
		t.Fatalf("the log has a subscription's endpoint: %s", logs.String())
	}
}

func TestOneVAPIDKeyPairPerServer(t *testing.T) {
	w := newWorld(t, true)
	get := func() string {
		rec := w.bob.do(http.MethodGet, "/api/me/push/key", nil)
		expect(t, rec, http.StatusOK)
		return decode[struct {
			PublicKey string `json:"public_key"`
		}](t, rec).PublicKey
	}
	first := get()
	raw, err := base64.RawURLEncoding.DecodeString(first)
	if err != nil || len(raw) != 65 || raw[0] != 4 {
		t.Fatalf("public key %q is not an uncompressed P-256 point: %v", first, err)
	}
	again, err := notifications.New(context.Background(), w.db, access.New(w.db, nil), notifications.Options{Key: w.key})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	acc := auth.New(w.db, auth.Options{})
	again.Routes(mux, acc)
	if w.count("push_vapid", "1=1") != 1 || get() != first {
		t.Fatal("a second start made another key pair")
	}
}

func TestOldNotificationsAndFinishedPushesArePurgedAndAPersonsListIsBounded(t *testing.T) {
	w := newWorld(t, true)
	add := func(created, read string, n int) {
		t.Helper()
		for range n {
			var readAt any
			if read != "" {
				readAt = read
			}
			if _, err := w.db.Exec(`INSERT INTO notifications (user_id, workspace_id, kind, ref, created_at, read_at) VALUES (?, ?, 'card_assigned', '{}', ?, ?)`,
				w.bob.id, w.ws, created, readAt); err != nil {
				t.Fatal(err)
			}
		}
	}
	now := time.Now()
	day := func(ago int) string {
		return now.Add(-time.Duration(ago) * 24 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	}
	add(day(200), "", 2)
	add(day(100), day(95), 3)
	add(day(100), "", 4)
	add(day(1), "", 500)
	add(day(0), "", 5)
	if err := w.svc.Purge(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := w.count("notifications", "created_at < ?", day(150)); n != 0 {
		t.Fatalf("%d notifications past the longest keep", n)
	}
	if n := w.count("notifications", "read_at IS NOT NULL"); n != 0 {
		t.Fatalf("%d notifications read long ago are kept", n)
	}
	if n := w.count("notifications", "user_id = ?", w.bob.id); n != 500 {
		t.Fatalf("%d notifications kept, want the person's most, 500", n)
	}
	if n := w.count("notifications", "created_at >= ?", day(0)); n != 5 {
		t.Fatalf("%d of today's notifications kept, want all 5", n)
	}
}

func TestAPushIsNotSentToASubscriptionWhoseSessionExpired(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, true)
	d, kept := w.newDevice("expired"), w.newDevice("kept")
	w.subscribe(w.bob, d)
	other := &user{e: w.env}
	expect(t, other.do(http.MethodPost, "/api/login", map[string]string{"email": "bob@example.com", "password": fakePassword}), http.StatusOK)
	w.subscribe(other, kept)
	if _, err := w.db.Exec(`UPDATE sessions SET expires_at = '2000-01-01T00:00:00.000Z' WHERE id = (SELECT session_id FROM push_subscriptions WHERE endpoint = ?)`, d.endpoint); err != nil {
		t.Fatal(err)
	}
	w.assign("After the session ended", w.bob)
	w.svc.DrainOnce(ctx)
	if got, _ := w.pushed(d); len(got) != 0 {
		t.Fatal("a push went to a device whose session had expired")
	}
	if got, _ := w.pushed(kept); len(got) != 1 {
		t.Fatalf("the device with a live session got %d pushes", len(got))
	}
	if w.count("push_subscriptions", "endpoint = ?", d.endpoint) != 0 {
		t.Fatal("the subscription of an expired session is still there")
	}
}

func TestExpiredSessionsSubscriptionsAreSweptOnTheWorkersSchedule(t *testing.T) {
	w := newWorld(t, true)
	gone, live := w.newDevice("gone"), w.newDevice("live")
	w.subscribe(w.bob, gone)
	other := &user{e: w.env}
	expect(t, other.do(http.MethodPost, "/api/login", map[string]string{"email": "bob@example.com", "password": fakePassword}), http.StatusOK)
	w.subscribe(other, live)
	w.assign("Queued", w.bob)
	if _, err := w.db.Exec(`UPDATE sessions SET expires_at = '2000-01-01T00:00:00.000Z' WHERE id = (SELECT session_id FROM push_subscriptions WHERE endpoint = ?)`, gone.endpoint); err != nil {
		t.Fatal(err)
	}
	w.svc.Tidy(context.Background())
	if w.count("push_subscriptions", "endpoint = ?", gone.endpoint) != 0 || w.count("push_subscriptions", "endpoint = ?", live.endpoint) != 1 {
		t.Fatal("the sweep must end the expired session's subscription and only that one")
	}
	if w.count("push_deliveries", "1=1") != 1 {
		t.Fatal("the expired subscription's queued push should go with it")
	}
}

func TestAPushThatUsedAllItsTriesIsGivenUpAfterARestart(t *testing.T) {
	w := newWorld(t, true)
	d := w.newDevice("phone")
	w.subscribe(w.bob, d)
	w.assign("Ping", w.bob)
	if _, err := w.db.Exec(`UPDATE push_deliveries SET attempts = 5`); err != nil {
		t.Fatal(err)
	}
	w.svc.DrainOnce(context.Background())
	if len(w.push.got()) != 0 {
		t.Fatal("a push was tried a sixth time")
	}
	if w.count("push_deliveries", "state = 'failed' AND attempts = 5") != 1 {
		t.Fatal("a push out of tries should be marked failed, not left pending")
	}
}

func TestNoSubscriptionIsMadeForSomeoneInNoWorkspace(t *testing.T) {
	w := newWorld(t, true)
	d := w.newDevice("phone")
	w.subscribe(w.mallory, d)
	if _, err := w.db.Exec(`DELETE FROM memberships WHERE user_id = ?`, w.mallory.id); err != nil {
		t.Fatal(err)
	}
	if w.count("push_subscriptions", "user_id = ?", w.mallory.id) != 0 {
		t.Fatal("leaving the last workspace should have ended the subscription")
	}
	rec := w.mallory.do(http.MethodPut, "/api/me/push/subscription", d.body())
	if rec.Code != http.StatusForbidden || decode[struct{ Code string }](t, rec).Code != "push_no_workspace" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if w.count("push_subscriptions", "1=1") != 0 {
		t.Fatal("a person in no workspace has a push subscription again")
	}
}

func TestADeviceChangingHandsCountsAgainstTheNewOwnersLimit(t *testing.T) {
	w := newWorld(t, true)
	for i := range notifications.MaxSubscriptions {
		w.subscribe(w.bob, w.newDevice(fmt.Sprintf("bob%d", i)))
	}
	shared := w.newDevice("shared")
	w.subscribe(w.mallory, shared)
	rec := w.bob.do(http.MethodPut, "/api/me/push/subscription", shared.body())
	if rec.Code != http.StatusConflict || decode[struct{ Code string }](t, rec).Code != "push_too_many_subscriptions" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if w.count("push_subscriptions", "user_id = ?", w.bob.id) != notifications.MaxSubscriptions ||
		w.count("push_subscriptions", "user_id = ?", w.mallory.id) != 1 {
		t.Fatal("a device moved to an account that was at the limit")
	}
}

func TestRetentionRunsOnItsOwnScheduleWhileAPushBacklogIsSent(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, true)
	for i := range notifications.MaxSubscriptions {
		w.subscribe(w.bob, w.newDevice(fmt.Sprintf("d%d", i)))
	}
	for i := range 5 {
		w.assign(fmt.Sprintf("Card %d", i), w.bob)
	}
	w.push.mu.Lock()
	w.push.delay = 15 * time.Millisecond
	w.push.mu.Unlock()
	w.svc.TidyEvery(20 * time.Millisecond)
	stop := w.svc.Start(ctx)
	defer stop()

	deadline := time.Now().Add(10 * time.Second)
	for len(w.push.got()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the worker sent nothing")
		}
		time.Sleep(5 * time.Millisecond)
	}
	old := time.Now().Add(-200 * 24 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	if _, err := w.db.Exec(`INSERT INTO notifications (user_id, workspace_id, kind, ref, created_at) VALUES (?, ?, 'card_assigned', '{}', ?)`, w.bob.id, w.ws, old); err != nil {
		t.Fatal(err)
	}
	for w.count("notifications", "created_at = ?", old) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the old notification was never purged")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if w.count("push_deliveries", "state = 'pending'") == 0 {
		t.Fatal("the backlog was sent before the purge ran: the test shows nothing")
	}
}

func TestThePushContactIsTheVAPIDSubjectAndMustBeValid(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, true)
	d := w.newDevice("phone")
	w.subscribe(w.bob, d)
	w.assign("Ping", w.bob)
	anywhere := egress.MustParse("any")
	svc, err := notifications.New(ctx, w.db, access.New(w.db, nil), notifications.Options{
		Key: w.key, HTTPClient: w.push.Client(), Egress: &anywhere, Contact: "mailto:ops@example.org"})
	if err != nil {
		t.Fatal(err)
	}
	svc.DrainOnce(ctx)
	_, requests := w.pushed(d)
	if len(requests) != 1 {
		t.Fatalf("%d pushes", len(requests))
	}
	token, _, _ := strings.Cut(strings.TrimPrefix(requests[0].header.Get("Authorization"), "vapid t="), ", k=")
	raw, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct{ Sub string }
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Sub != "mailto:ops@example.org" {
		t.Fatalf("subject = %q, %v", claims.Sub, err)
	}
	for _, bad := range []string{"http://example.org", "ops@example.org", "mailto:"} {
		if _, err := notifications.New(ctx, w.db, access.New(w.db, nil), notifications.Options{Key: w.key, Contact: bad}); err == nil {
			t.Errorf("contact %q accepted", bad)
		}
	}
}

func TestDueAndOverdueNotificationsNameTheCardAndDateOnlyWhileItIsReadable(t *testing.T) {
	w := newWorld(t, true)
	d := w.newDevice("phone")
	w.subscribe(w.bob, d)
	card := w.assign("Renew the lease", w.bob)
	if _, err := w.db.Exec(`UPDATE cards SET due = '2030-01-10' WHERE id = ?`, card); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{notifications.KindCardDue, notifications.KindCardOverdue} {
		tx, err := w.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		notice := notifications.Notice{UserID: w.bob.id, WorkspaceID: w.ws, Kind: kind,
			Ref: notifications.Ref{Space: w.space.ID, Board: w.board, Card: card, Due: "2030-01-10"}, DedupeKey: "job:1:r0:" + kind}
		for range 2 {
			if err := notifications.Add(context.Background(), tx, notice); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	items := w.inbox(w.bob, "").Notifications
	if len(items) != 3 {
		t.Fatalf("inbox = %+v", items)
	}
	wantItem := fmt.Sprintf("%d:%d:%d", w.space.ID, w.board, card)
	for i, kind := range []string{notifications.KindCardOverdue, notifications.KindCardDue} {
		it := items[i]
		if it.Kind != kind || it.Params["title"] != "Renew the lease" || it.Params["due"] != "2030-01-10" ||
			it.Target.Section != "boards" || it.Target.Item != wantItem {
			t.Fatalf("item %d = %+v", i, it)
		}
	}
	if n := len(w.inbox(w.mallory, "").Notifications); n != 0 {
		t.Fatalf("mallory's inbox has %d items", n)
	}

	for _, due := range []string{"2030-02-01", ""} {
		if due == "" {
			if _, err := w.db.Exec(`UPDATE cards SET due = NULL WHERE id = ?`, card); err != nil {
				t.Fatal(err)
			}
		} else if _, err := w.db.Exec(`UPDATE cards SET due = ? WHERE id = ?`, due, card); err != nil {
			t.Fatal(err)
		}
		for _, i := range []int{0, 1} {
			if it := w.inbox(w.bob, "").Notifications[i]; it.Kind != notifications.KindChanged || len(it.Params) != 0 {
				t.Fatalf("item %d for a card whose date is %q now = %+v", i, due, it)
			}
		}
	}
	if _, err := w.db.Exec(`UPDATE cards SET due = '2030-01-10' WHERE id = ?`, card); err != nil {
		t.Fatal(err)
	}

	w.svc.DrainOnce(context.Background())
	payloads, _ := w.pushed(d)
	if len(payloads) != 3 {
		t.Fatalf("payloads = %v", payloads)
	}
	kinds := map[string]bool{}
	for _, p := range payloads {
		if len(p) != 2 || p["id"] == nil || p["kind"] == nil {
			t.Fatalf("a push carries more than its ID and kind: %v", p)
		}
		kinds[p["kind"].(string)] = true
	}
	for _, kind := range []string{notifications.KindCardAssigned, notifications.KindCardDue, notifications.KindCardOverdue} {
		if !kinds[kind] {
			t.Fatalf("no push of kind %s: %v", kind, payloads)
		}
	}
	expect(t, w.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", w.ws, w.space.ID, w.bob.id), nil), http.StatusNoContent)
	for _, it := range w.inbox(w.bob, "").Notifications {
		if it.Kind != notifications.KindChanged || len(it.Params) != 0 {
			t.Fatalf("an item after access ended = %+v", it)
		}
	}
}

func (w *world) comment(u *user, card int64, body string) {
	w.t.Helper()
	if _, err := w.cards.Comment(context.Background(), boards.Actor{UserID: u.id}, w.space.ID, w.board, card, body); err != nil {
		w.t.Fatal(err)
	}
}

func mention(name string, u *user) string { return fmt.Sprintf("@[%s](user:%d)", name, u.id) }

func TestACommentTellsTheAssigneeAndNotTheCommenter(t *testing.T) {
	w := newWorld(t, true)
	card := w.assign("Paint the fence", w.bob)
	before := w.inbox(w.bob, "").Unread

	w.comment(w.owner, card, "The paint is in the shed")
	in := w.inbox(w.bob, "")
	if in.Unread != before+1 {
		t.Fatalf("bob's inbox = %+v", in)
	}
	it := in.Notifications[0]
	wantItem := fmt.Sprintf("%d:%d:%d", w.space.ID, w.board, card)
	if it.Kind != notifications.KindCardComment || it.Params["title"] != "Paint the fence" || it.Target.Section != "boards" || it.Target.Item != wantItem {
		t.Fatalf("item = %+v", it)
	}
	if raw, _ := json.Marshal(in); strings.Contains(string(raw), "shed") {
		t.Fatalf("the comment's text is in the inbox: %s", raw)
	}
	if n := w.inbox(w.owner, "").Unread + w.inbox(w.mallory, "").Unread; n != 0 {
		t.Fatalf("others were told: %d", n)
	}
	w.comment(w.bob, card, "Mine, on it")
	if w.inbox(w.bob, "").Unread != before+1 {
		t.Fatal("commenting on your own card made a notification")
	}
	unowned, err := w.cards.CreateCard(context.Background(), boards.Actor{UserID: w.owner.id}, w.space.ID, w.board, boards.NewCard{Title: "Unowned"})
	if err != nil {
		t.Fatal(err)
	}
	was := [3]int{w.inbox(w.owner, "").Unread, w.inbox(w.bob, "").Unread, w.inbox(w.mallory, "").Unread}
	w.comment(w.bob, unowned.ID, "hello")
	w.comment(w.owner, unowned.ID, "hello again")
	if after := [3]int{w.inbox(w.owner, "").Unread, w.inbox(w.bob, "").Unread, w.inbox(w.mallory, "").Unread}; after != was {
		t.Fatalf("unread before %v, after %v: a card of nobody told someone", was, after)
	}
}

func TestAMentionTellsThePersonOnceAndOnlyIfTheyMayReadTheBoard(t *testing.T) {
	w := newWorld(t, true)
	card := w.assign("Plan", w.owner)
	base := w.inbox(w.owner, "").Unread

	w.comment(w.bob, card, "Hi "+mention("Bob", w.bob)+" and "+mention("Mallory", w.mallory)+" and "+mention("Owner", w.owner)+
		" "+mention("Bob", w.bob)+" @[Nobody](user:99999)")
	if n := w.inbox(w.owner, "").Unread - base; n != 1 {
		t.Fatalf("the assignee, also mentioned, was told %d times", n)
	}
	if it := w.inbox(w.owner, "").Notifications[0]; it.Kind != notifications.KindCardMention {
		t.Fatalf("item = %+v: a mention takes the place of the comment's", it)
	}
	if w.inbox(w.bob, "").Unread != 0 {
		t.Fatalf("bob = %+v", w.inbox(w.bob, ""))
	}
	if n := w.count("notifications", "user_id = ?", w.mallory.id); n != 0 {
		t.Fatalf("someone who can't read the board has %d notifications", n)
	}
	w.comment(w.owner, card, "Looping in "+mention("Bob", w.bob))
	in := w.inbox(w.bob, "")
	if in.Unread != 1 || in.Notifications[0].Kind != notifications.KindCardMention || in.Notifications[0].Params["title"] != "Plan" {
		t.Fatalf("bob = %+v", in)
	}
	w.comment(w.owner, card, "@[Someone else](user:"+fmt.Sprint(w.bob.id)+")")
	if w.inbox(w.bob, "").Unread != 2 {
		t.Fatal("a mention with another name didn't tell the person of that ID")
	}
	w.comment(w.owner, card, "@Bob @bob@example.com [Bob](user:"+fmt.Sprint(w.bob.id)+")")
	if w.inbox(w.bob, "").Unread != 2 {
		t.Fatal("plain text told someone")
	}
}

func TestACommentNotificationShowsNothingOnceAccessEnds(t *testing.T) {
	w := newWorld(t, true)
	card := w.assign("Quarterly layoffs plan", w.owner)
	w.comment(w.owner, card, "Please read "+mention("Bob", w.bob))
	if w.inbox(w.bob, "").Unread != 1 {
		t.Fatal("bob wasn't told")
	}
	expect(t, w.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", w.ws, w.space.ID, w.bob.id), nil), http.StatusNoContent)

	w.comment(w.owner, card, "Bob is out: "+mention("Bob", w.bob))
	raw := w.bob.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/notifications", w.ws), nil)
	expect(t, raw, http.StatusOK)
	in := decode[notifications.List](t, raw)
	if len(in.Notifications) != 1 || in.Notifications[0].Kind != notifications.KindChanged || len(in.Notifications[0].Params) != 0 {
		t.Fatalf("inbox = %s", raw.Body)
	}
	for _, leak := range []string{"Quarterly", "layoffs", "card_mention"} {
		if strings.Contains(raw.Body.String(), leak) {
			t.Fatalf("the inbox still says %q: %s", leak, raw.Body)
		}
	}
}

func (w *world) whileRendering(fn func()) {
	w.hooks.BeforeQuery("title", fn)
}

func TestARenamedCardShowsNothingToSomeoneWhoseAccessEndedAtTheSameTime(t *testing.T) {
	for _, kind := range []string{notifications.KindCardAssigned, notifications.KindCardComment, notifications.KindCardMention} {
		t.Run(kind, func(t *testing.T) {
			w := newWorld(t, true)
			card := w.assign("Quarterly layoffs plan", w.bob)
			if kind == notifications.KindCardComment {
				w.comment(w.owner, card, "Please read")
			}
			if kind == notifications.KindCardMention {
				w.comment(w.owner, card, "Please read "+mention("Bob", w.bob))
			}
			w.whileRendering(func() {
				if _, err := w.db.Exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, w.space.ID, w.bob.id); err != nil {
					t.Error(err)
				}
				if _, err := w.db.Exec(`UPDATE cards SET title = 'Secret renamed' WHERE id = ?`, card); err != nil {
					t.Error(err)
				}
			})
			raw := w.bob.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/notifications", w.ws), nil)
			expect(t, raw, http.StatusOK)
			if strings.Contains(raw.Body.String(), "Secret") || strings.Contains(raw.Body.String(), "Quarterly") {
				t.Fatalf("the inbox shows a title to someone who lost access: %s", raw.Body)
			}
			assertOnlyGeneric(t, raw, 1+btoi(kind != notifications.KindCardAssigned))
			if strings.Contains(w.bob.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/notifications", w.ws), nil).Body.String(), "Secret") {
				t.Fatal("the title shows once the rename has settled")
			}
		})
	}
}

func TestAnAppNameShowsNothingToSomeoneWhoseAccessEndedAtTheSameTime(t *testing.T) {
	w := newWorld(t, true)
	var app int64
	if err := w.db.QueryRow(`INSERT INTO apps (space_id, name, definition) VALUES (?, 'Inspections', '{}') RETURNING id`, w.space.ID).Scan(&app); err != nil {
		t.Fatal(err)
	}
	tx, err := w.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := notifications.Add(context.Background(), tx, notifications.Notice{UserID: w.bob.id, WorkspaceID: w.ws, Kind: notifications.KindAppEffectFailed,
		Ref: notifications.Ref{Space: w.space.ID, App: app, Effect: 7}, DedupeKey: "effect:7:1"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	w.hooks.BeforeQuery("FROM spaces s", func() {
		if _, err := w.db.Exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, w.space.ID, w.bob.id); err != nil {
			t.Error(err)
		}
		if _, err := w.db.Exec(`UPDATE apps SET name = 'Secret renamed' WHERE id = ?`, app); err != nil {
			t.Error(err)
		}
	})
	raw := w.bob.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/notifications", w.ws), nil)
	expect(t, raw, http.StatusOK)
	if strings.Contains(raw.Body.String(), "Secret") || strings.Contains(raw.Body.String(), "Inspections") {
		t.Fatalf("the inbox shows an app's name to someone who lost access: %s", raw.Body)
	}
	assertOnlyGeneric(t, raw, 1)
}

func assertOnlyGeneric(t *testing.T, raw *httptest.ResponseRecorder, want int) {
	t.Helper()
	expect(t, raw, http.StatusOK)
	in := decode[notifications.List](t, raw)
	if len(in.Notifications) != want {
		t.Fatalf("%d items, want %d: %s", len(in.Notifications), want, raw.Body)
	}
	for _, it := range in.Notifications {
		if it.Kind != notifications.KindChanged || len(it.Params) != 0 || it.Target.Section != "home" || it.Target.Item != "" {
			t.Fatalf("item = %+v, want the generic one", it)
		}
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestAnAppNameAndTheRightToViewItAreOneSnapshot(t *testing.T) {
	w := newWorld(t, true)
	var app int64
	if err := w.db.QueryRow(`INSERT INTO apps (space_id, name, definition) VALUES (?, 'Inspections', '{}') RETURNING id`, w.space.ID).Scan(&app); err != nil {
		t.Fatal(err)
	}
	if _, err := w.db.Exec(`INSERT INTO app_roles (app_id, user_id, role) VALUES (?, ?, 'builder')`, app, w.owner.id); err != nil {
		t.Fatal(err)
	}
	tx, err := w.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := notifications.Add(context.Background(), tx, notifications.Notice{UserID: w.bob.id, WorkspaceID: w.ws, Kind: notifications.KindAppEffectFailed,
		Ref: notifications.Ref{Space: w.space.ID, App: app, Effect: 7}, DedupeKey: "effect:7:1"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	w.hooks.BeforeQuery("FROM app_roles", func() {
		if _, err := w.db.Exec(`INSERT INTO app_roles (app_id, user_id, role) VALUES (?, ?, 'user')`, app, w.bob.id); err != nil {
			t.Error(err)
		}
		if _, err := w.db.Exec(`UPDATE apps SET name = 'Renamed' WHERE id = ?`, app); err != nil {
			t.Error(err)
		}
	})
	raw := w.bob.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/notifications", w.ws), nil)
	if strings.Contains(raw.Body.String(), "Inspections") || strings.Contains(raw.Body.String(), "Renamed") {
		t.Fatalf("a name from another moment than the right to view it: %s", raw.Body)
	}
	assertOnlyGeneric(t, raw, 1)
}

func TestACommentInCodeOrEscapedTellsNobodyAndOnePersonCanBeMentionedBeside(t *testing.T) {
	w := newWorld(t, true)
	card := w.assign("Plan", w.owner)
	w.comment(w.owner, card, "```\n"+mention("Bob", w.bob)+"\n```\n`"+mention("Bob", w.bob)+"` \\"+mention("Bob", w.bob))
	if n := w.count("notifications", "user_id = ?", w.bob.id); n != 0 {
		t.Fatalf("a mention in code or escaped made %d notifications", n)
	}
	w.comment(w.owner, card, "```\n"+mention("Bob", w.bob)+"\n```\nbut "+mention("Bob", w.bob))
	if n := w.count("notifications", "user_id = ?", w.bob.id); n != 1 {
		t.Fatalf("a mention beside code made %d notifications", n)
	}
}
