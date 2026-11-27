package notifications

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"taskto.ai/taskto/internal/egress"
)

const (
	maxAttempts    = 5
	sendTimeout    = 15 * time.Second
	pushPoll       = 2 * time.Second
	backoffBase    = 30 * time.Second
	backoffMax     = time.Hour
	pushTTL        = time.Hour
	keepDeliveries = 7 * 24 * time.Hour
	tidyEvery      = time.Hour
)

func (s *Service) Start(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	var running sync.WaitGroup
	running.Add(3)
	go func() {
		defer running.Done()
		poll := time.NewTicker(pushPoll)
		defer poll.Stop()
		for {
			s.drain(ctx)
			select {
			case <-ctx.Done():
				return
			case <-poll.C:
			}
		}
	}()
	go func() {
		defer running.Done()
		poll := time.NewTicker(pushPoll)
		defer poll.Stop()
		for {
			s.drainEmail(ctx)
			select {
			case <-ctx.Done():
				return
			case <-poll.C:
			}
		}
	}()
	go func() {
		defer running.Done()
		tidy := time.NewTicker(s.tidyEvery)
		defer tidy.Stop()
		for {
			s.tidy(ctx)
			select {
			case <-ctx.Done():
				return
			case <-tidy.C:
			}
		}
	}()
	return func() {
		cancel()
		running.Wait()
	}
}

func (s *Service) tidy(ctx context.Context) {
	if err := errors.Join(s.sweepSessions(ctx), s.purge(ctx)); err != nil && ctx.Err() == nil {
		slog.Warn("notifications: tidying failed", "err", err)
	}
}

func (s *Service) sweepSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM push_subscriptions WHERE session_id IN (SELECT id FROM sessions WHERE expires_at <= ?)`, s.now())
	return err
}

func (s *Service) drain(ctx context.Context) {
	for ctx.Err() == nil {
		ok, err := s.runOne(ctx)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("notifications: push worker", "err", err)
			}
			return
		}
		if !ok {
			return
		}
	}
}

func (s *Service) DrainOnce(ctx context.Context) { s.drain(ctx) }

type delivery struct {
	id, notificationID, subscriptionID int64
	attempts                           int
}

func (s *Service) claim(ctx context.Context) (delivery, bool, error) {
	t := s.clock()
	if _, err := s.db.ExecContext(ctx, `
		UPDATE push_deliveries SET state = 'failed', updated_at = ?
		WHERE state = 'pending' AND attempts >= ? AND next_attempt_at <= ?`, s.stamp(t), maxAttempts, s.stamp(t)); err != nil {
		return delivery{}, false, err
	}
	var d delivery
	err := s.db.QueryRowContext(ctx, `
		UPDATE push_deliveries SET attempts = attempts + 1, next_attempt_at = ?, updated_at = ?
		WHERE id = (SELECT id FROM push_deliveries WHERE state = 'pending' AND attempts < ? AND next_attempt_at <= ? ORDER BY id LIMIT 1)
		RETURNING id, notification_id, subscription_id, attempts`,
		s.stamp(t.Add(2*sendTimeout)), s.stamp(t), maxAttempts, s.stamp(t)).Scan(&d.id, &d.notificationID, &d.subscriptionID, &d.attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return delivery{}, false, nil
	}
	return d, err == nil, err
}

type outcome int

const (
	sent outcome = iota
	retry
	failed
	gone
)

func (s *Service) runOne(ctx context.Context) (bool, error) {
	d, found, err := s.claim(ctx)
	if err != nil || !found {
		return false, err
	}
	out, status, wait := s.send(ctx, d)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return true, s.settle(ctx, d, out, status, wait)
}

func (s *Service) settle(ctx context.Context, d delivery, out outcome, status int, wait time.Duration) error {
	t := s.clock()
	switch out {
	case gone:
		_, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE id = ?`, d.subscriptionID)
		return err
	case sent:
		_, err := s.db.ExecContext(ctx, `UPDATE push_deliveries SET state = 'sent', last_status = ?, updated_at = ? WHERE id = ?`, status, s.stamp(t), d.id)
		return err
	case retry:
		if d.attempts < maxAttempts {
			after := min(max(wait, backoffBase<<(2*(d.attempts-1))), backoffMax)
			_, err := s.db.ExecContext(ctx, `UPDATE push_deliveries SET next_attempt_at = ?, last_status = ?, updated_at = ? WHERE id = ?`,
				s.stamp(t.Add(after)), status, s.stamp(t), d.id)
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE push_deliveries SET state = 'failed', last_status = ?, updated_at = ? WHERE id = ?`, status, s.stamp(t), d.id)
	return err
}

type payload struct {
	ID   int64  `json:"id"`
	Kind string `json:"kind"`
}

func (s *Service) send(ctx context.Context, d delivery) (out outcome, status int, wait time.Duration) {
	var n stored
	var ref string
	var subUser int64
	var endpoint string
	var p256dh, sealed []byte
	var member, live bool
	err := s.db.QueryRowContext(ctx, `
		SELECT n.id, n.user_id, n.workspace_id, n.kind, n.ref, n.created_at, n.read_at IS NOT NULL,
			sub.user_id, sub.endpoint, sub.p256dh, sub.auth_sealed,
			EXISTS (SELECT 1 FROM memberships m WHERE m.workspace_id = n.workspace_id AND m.user_id = n.user_id),
			EXISTS (SELECT 1 FROM sessions s WHERE s.id = sub.session_id AND s.user_id = sub.user_id AND s.expires_at > ?)
		FROM push_deliveries d
		JOIN notifications n ON n.id = d.notification_id
		JOIN push_subscriptions sub ON sub.id = d.subscription_id
		WHERE d.id = ?`, s.now(), d.id).Scan(&n.id, &n.userID, &n.workspaceID, &n.kind, &ref, &n.createdAt, &n.read,
		&subUser, &endpoint, &p256dh, &sealed, &member, &live)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("notifications: a push's notification or device is gone", "delivery", d.id, "err", err)
		}
		return failed, 0, 0
	}
	if !live {
		return gone, 0, 0
	}
	if err := json.Unmarshal([]byte(ref), &n.ref); err != nil {
		return failed, 0, 0
	}
	created, perr := time.Parse(timeFormat, n.createdAt)
	if n.read || !member || subUser != n.userID || perr != nil || s.clock().Sub(created) > pushTTL {
		return failed, 0, 0
	}

	it, err := s.readerFor(n.userID).item(ctx, n)
	if err != nil {
		slog.Warn("notifications: rendering a push failed", "delivery", d.id, "err", err)
		return retry, 0, 0
	}
	body, err := json.Marshal(payload{ID: n.id, Kind: it.Kind})
	if err != nil {
		return failed, 0, 0
	}
	secret, err := s.key.Open(sealed, authAD)
	if err != nil {
		slog.Error("notifications: a subscription's secret can't be opened", "subscription", d.subscriptionID)
		return failed, 0, 0
	}
	message, err := encrypt(p256dh, secret, body)
	if err != nil {
		slog.Warn("notifications: a subscription's key can't be used", "subscription", d.subscriptionID, "err", err)
		return gone, 0, 0
	}
	target, err := url.Parse(endpoint)
	if err != nil {
		return gone, 0, 0
	}
	authz, err := s.vapid.authorization(target, s.contact, s.clock())
	if err != nil {
		slog.Error("notifications: signing the VAPID token failed", "err", err)
		return retry, 0, 0
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(message))
	if err != nil {
		return gone, 0, 0
	}
	req.Header.Set("Authorization", authz)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(int(pushTTL/time.Second)))
	req.Header.Set("Urgency", "normal")
	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return retry, 0, 0
		}
		if egress.Refusal(err) != nil {
			slog.Warn("notifications: the egress policy refused a push endpoint", "host", target.Hostname())
			return failed, 0, 0
		}
		slog.Warn("notifications: a push service didn't answer", "host", target.Hostname(), "attempt", d.attempts)
		return retry, 0, 0
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return sent, resp.StatusCode, 0
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return gone, resp.StatusCode, 0
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		var after time.Duration
		if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
			after = time.Duration(secs) * time.Second
		}
		return retry, resp.StatusCode, after
	}
	slog.Warn("notifications: a push service refused a push", "host", target.Hostname(), "status", resp.StatusCode)
	return failed, resp.StatusCode, 0
}

func (s *Service) purge(ctx context.Context) error {
	t := s.clock()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM push_deliveries WHERE state <> 'pending' AND updated_at < ?`, s.stamp(t.Add(-keepDeliveries))); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM email_deliveries WHERE updated_at < ?`, s.stamp(t.Add(-keepDeliveries))); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM notification_email_ledger WHERE updated_at < ?`, s.stamp(t.Add(-keepDeliveries))); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM notifications WHERE created_at < ? OR (read_at IS NOT NULL AND read_at < ?)`,
		s.stamp(t.Add(-keepAll)), s.stamp(t.Add(-keepRead))); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM notifications WHERE id IN (
			SELECT id FROM (
				SELECT id, row_number() OVER (PARTITION BY user_id, workspace_id ORDER BY id DESC) AS place FROM notifications
			) WHERE place > ?)`, perUser)
	return err
}
