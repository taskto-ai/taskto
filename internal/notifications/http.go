package notifications

import (
	"context"
	"crypto/ecdh"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/egress"
)

// Routes mounts the inbox's and push's endpoints on mux. The inbox of a workspace is behind a's
// RequireMember, and a person's own notification, the server's push key and a device's
// subscription are behind RequireUser: all of them need a browser session, never an access token,
// and the CSRF guard checks every one that isn't a GET. docs/api.md describes them.
func (s *Service) Routes(mux *http.ServeMux, a *auth.Service) {
	member := func(h http.HandlerFunc) http.Handler { return a.RequireMember(h) }
	user := func(h http.HandlerFunc) http.Handler { return a.RequireUser(h) }
	const inbox = "/api/workspaces/{workspace}/notifications"

	mux.Handle("GET "+inbox, member(s.handleList))
	mux.Handle("GET "+inbox+"/count", member(s.handleCount))
	mux.Handle("POST "+inbox+"/read", member(s.handleReadAll))
	mux.Handle("POST "+inbox+"/{notification}/read", member(s.handleRead))
	mux.Handle("GET /api/me/notifications/{notification}", user(s.handleGet))
	mux.Handle("GET /api/me/push/key", user(s.handleKey))
	mux.Handle("PUT /api/me/push/subscription", user(func(w http.ResponseWriter, r *http.Request) { s.handleSubscribe(w, r, a) }))
	mux.Handle("DELETE /api/me/push/subscription", user(s.handleUnsubscribe))
}

func callerAndWorkspace(r *http.Request) (userID, workspaceID int64) {
	u, _ := auth.UserFrom(r.Context())
	m, _ := auth.MembershipFrom(r.Context())
	return u.ID, m.WorkspaceID
}

func notFound(w http.ResponseWriter) {
	apierr.Write(w, http.StatusNotFound, apierr.CodeNotificationNotFound, "notification not found")
}

func (s *Service) serverError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return // the client went away
	}
	slog.Error("notifications: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	apierr.Internal(w)
}

// List is a page of the inbox, newest first.
type List struct {
	Notifications []Item `json:"notifications"`
	// More says whether older ones than the page's last exist (ask again with before).
	More bool `json:"more"`
	// Unread counts all the person's unread ones in the workspace, not only the page's.
	Unread int `json:"unread"`
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	userID, workspaceID := callerAndWorkspace(r)
	q := r.URL.Query()
	limit := DefaultListed
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxListed {
			apierr.Write(w, http.StatusBadRequest, apierr.CodeBadRequest, "limit is 1 to "+strconv.Itoa(MaxListed))
			return
		}
		limit = n
	}
	var before int64
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			apierr.Write(w, http.StatusBadRequest, apierr.CodeBadRequest, "before is a notification's ID")
			return
		}
		before = n
	}
	unreadOnly := q.Get("unread") == "true"

	// One static query: before = 0 is no bound, and unread = 0 is every notification.
	unreadFlag := 0
	if unreadOnly {
		unreadFlag = 1
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, user_id, workspace_id, kind, ref, created_at, read_at IS NOT NULL FROM notifications
		WHERE user_id = ? AND workspace_id = ? AND (? = 0 OR id < ?) AND (? = 0 OR read_at IS NULL)
		ORDER BY id DESC LIMIT ?`, userID, workspaceID, before, before, unreadFlag, limit+1)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var page []stored
	for rows.Next() {
		n, err := scanStored(rows)
		if err != nil {
			rows.Close()
			s.serverError(w, r, err)
			return
		}
		page = append(page, n)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		s.serverError(w, r, err)
		return
	}
	out := List{Notifications: []Item{}, More: len(page) > limit}
	if out.More {
		page = page[:limit]
	}
	rd := s.readerFor(userID)
	for _, n := range page {
		it, err := rd.item(r.Context(), n)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		out.Notifications = append(out.Notifications, it)
	}
	if out.Unread, err = s.unread(r.Context(), userID, workspaceID); err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) unread(ctx context.Context, userID, workspaceID int64) (n int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM notifications WHERE user_id = ? AND workspace_id = ? AND read_at IS NULL`,
		userID, workspaceID).Scan(&n)
	return n, err
}

func (s *Service) handleCount(w http.ResponseWriter, r *http.Request) {
	userID, workspaceID := callerAndWorkspace(r)
	n, err := s.unread(r.Context(), userID, workspaceID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"unread": n})
}

func notificationID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("notification"), 10, 64)
	if err != nil || id < 1 {
		notFound(w)
		return 0, false
	}
	return id, true
}

func (s *Service) handleRead(w http.ResponseWriter, r *http.Request) {
	userID, workspaceID := callerAndWorkspace(r)
	id, ok := notificationID(w, r)
	if !ok {
		return
	}
	// Only the person's own, in this workspace; reading one again is nothing to do.
	var found int64
	err := s.db.QueryRowContext(r.Context(), `
		UPDATE notifications SET read_at = coalesce(read_at, ?) WHERE id = ? AND user_id = ? AND workspace_id = ? RETURNING id`,
		s.now(), id, userID, workspaceID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		notFound(w)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleReadAll(w http.ResponseWriter, r *http.Request) {
	userID, workspaceID := callerAndWorkspace(r)
	if !readEmptyJSON(w, r) {
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `
		UPDATE notifications SET read_at = ? WHERE user_id = ? AND workspace_id = ? AND read_at IS NULL`,
		s.now(), userID, workspaceID); err != nil {
		s.serverError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleGet is one notification of the caller's, whatever its workspace: the address a push
// opens, which holds nothing but the notification's own ID. Someone else's is as missing as one
// that never was.
func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	id, ok := notificationID(w, r)
	if !ok {
		return
	}
	n, err := scanStored(s.db.QueryRowContext(r.Context(), `SELECT `+storedColumns+` FROM notifications WHERE id = ? AND user_id = ?`, id, u.ID))
	if errors.Is(err, sql.ErrNoRows) {
		notFound(w)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	it, err := s.readerFor(u.ID).item(r.Context(), n)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

func (s *Service) handleKey(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"public_key": s.vapid.publicKey()})
}

// subscription is what a browser's PushSubscription.toJSON() says.
type subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// maxEndpoint bounds an endpoint's length; the push services' are a few hundred bytes.
const maxEndpoint = 2048

// checkEndpoint accepts an https address of a push service: no credentials, no fragment.
func checkEndpoint(raw string) (*url.URL, bool) {
	if raw == "" || len(raw) > maxEndpoint {
		return nil, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return nil, false
	}
	return u, true
}

// parseKeys reads a browser's key and secret: base64url, an uncompressed P-256 point and 16 bytes.
func parseKeys(p256dh, authSecret string) (pub, secret []byte, ok bool) {
	pub, err := b64.DecodeString(strings.TrimRight(p256dh, "="))
	if err != nil {
		return nil, nil, false
	}
	if _, err := ecdh.P256().NewPublicKey(pub); err != nil {
		return nil, nil, false
	}
	secret, err = b64.DecodeString(strings.TrimRight(authSecret, "="))
	if err != nil || len(secret) != authSize {
		return nil, nil, false
	}
	return pub, secret, true
}

var authAD = []byte("taskto push_subscriptions.auth v1")

func (s *Service) handleSubscribe(w http.ResponseWriter, r *http.Request, a *auth.Service) {
	u, _ := auth.UserFrom(r.Context())
	var req subscription
	if !readJSON(w, r, &req) {
		return
	}
	target, ok := checkEndpoint(req.Endpoint)
	if !ok {
		apierr.Write(w, http.StatusBadRequest, apierr.CodePushEndpointInvalid, "the endpoint must be an https address of at most "+strconv.Itoa(maxEndpoint)+" characters")
		return
	}
	// The addresses the sender may connect to, and no others: refused here, where the person
	// sees why, and again on every push.
	lookup, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	err := s.egress.Check(lookup, target.Hostname(), nil)
	cancel()
	if refused := egress.Refusal(err); refused != nil {
		apierr.WriteError(w, refused.APIError("endpoint"))
		return
	}
	pub, secret, ok := parseKeys(req.Keys.P256dh, req.Keys.Auth)
	if !ok {
		apierr.Write(w, http.StatusBadRequest, apierr.CodePushKeysInvalid, "the subscription's keys are not valid")
		return
	}
	session, ok, err := a.SessionID(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !ok {
		apierr.Write(w, http.StatusUnauthorized, apierr.CodeNotLoggedIn, "log in first")
		return
	}
	sealed, err := s.key.Seal(secret, authAD)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	err = s.write(r.Context(), func(tx *sql.Tx) error {
		// Push is for people in a workspace: leaving the last one ended their subscriptions, and
		// the app opening afterwards must not make one again.
		var member bool
		if err := tx.QueryRowContext(r.Context(), `SELECT EXISTS (SELECT 1 FROM memberships WHERE user_id = ?)`, u.ID).Scan(&member); err != nil {
			return err
		}
		if !member {
			return errNoWorkspace
		}
		var owner sql.NullInt64
		err := tx.QueryRowContext(r.Context(), `SELECT user_id FROM push_subscriptions WHERE endpoint = ?`, req.Endpoint).Scan(&owner)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !owner.Valid || owner.Int64 != u.ID {
			// A new device, or one that changes hands: either way it is one more of this person's.
			var have int
			if err := tx.QueryRowContext(r.Context(), `SELECT count(*) FROM push_subscriptions WHERE user_id = ?`, u.ID).Scan(&have); err != nil {
				return err
			}
			if have >= MaxSubscriptions {
				return errTooMany
			}
		}
		if owner.Valid && owner.Int64 != u.ID {
			// The browser is now someone else's: what was queued for the one before is not sent.
			if _, err := tx.ExecContext(r.Context(), `
				DELETE FROM push_deliveries WHERE subscription_id IN (SELECT id FROM push_subscriptions WHERE endpoint = ?)`, req.Endpoint); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(r.Context(), `
			INSERT INTO push_subscriptions (user_id, session_id, endpoint, p256dh, auth_sealed) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (endpoint) DO UPDATE SET user_id = excluded.user_id, session_id = excluded.session_id,
				p256dh = excluded.p256dh, auth_sealed = excluded.auth_sealed`,
			u.ID, session, req.Endpoint, pub, sealed)
		return err
	})
	switch {
	case errors.Is(err, errNoWorkspace):
		apierr.Write(w, http.StatusForbidden, apierr.CodePushNoWorkspace, "push is for people who are in a workspace")
	case errors.Is(err, errTooMany):
		apierr.Write(w, http.StatusConflict, apierr.CodePushTooManySubscriptions, "too many devices are subscribed", apierr.Params{"max": MaxSubscriptions})
	case err != nil:
		s.serverError(w, r, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

var (
	errTooMany     = errors.New("too many subscriptions")
	errNoWorkspace = errors.New("no workspace")
)

func (s *Service) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	// Only the caller's own; one that isn't there, or is someone else's, is the same answer.
	if _, err := s.db.ExecContext(r.Context(), `DELETE FROM push_subscriptions WHERE endpoint = ? AND user_id = ?`, req.Endpoint, u.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

const maxBody = 8 << 10

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		apierr.Write(w, http.StatusBadRequest, apierr.CodeInvalidJSON, "invalid JSON body")
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		apierr.Write(w, http.StatusBadRequest, apierr.CodeInvalidJSON, "invalid JSON body: one object expected")
		return false
	}
	return true
}

// readEmptyJSON takes the empty object the web app sends with a request that has no fields.
func readEmptyJSON(w http.ResponseWriter, r *http.Request) bool {
	if r.ContentLength == 0 && len(r.TransferEncoding) == 0 {
		return true
	}
	var none struct{}
	return readJSON(w, r, &none)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.Error("notifications: encode response", "err", err)
		apierr.Internal(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}
