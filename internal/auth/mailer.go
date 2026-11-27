package auth

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/mail"
	"taskto.ai/taskto/internal/ratelimit"
)

const (
	resetIPAttempts    = 20
	resetEmailAttempts = 5
	resetWindow        = time.Hour
	resetStartBurst    = 30
	resetStartEvery    = time.Second
	mailConcurrency    = 8
	mailTimeout        = time.Minute
)

const (
	inviteAdminAttempts     = 30
	inviteRecipientAttempts = 3
	inviteWindow            = time.Hour
	inviteMailTimeout       = 45 * time.Second
)

func (s *Service) EmailLinks() bool {
	return s.opts.Mail.Configured() && s.opts.PublicURL != ""
}

func (s *Service) ResetByEmail() bool {
	return s.EmailLinks() && !s.opts.DisablePasswordLogin
}

func (s *Service) WaitIdle() { s.mailWork.Wait() }

func (s *Service) Shutdown(ctx context.Context) error {
	s.stopWork()
	idle := make(chan struct{})
	go func() { s.WaitIdle(); close(idle) }()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) background(parent context.Context, work func(ctx context.Context)) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), mailTimeout)
	stop := context.AfterFunc(s.workCtx, cancel)
	s.mailWork.Add(1)
	go func() {
		defer s.mailWork.Done()
		defer cancel()
		defer stop()
		select {
		case s.mailSlots <- struct{}{}:
			defer func() { <-s.mailSlots }()
		case <-ctx.Done():
			slog.WarnContext(ctx, "auth: an email wasn't made: the server was busy or stopping")
			return
		}
		work(ctx)
	}()
}

func (s *Service) handleRequestReset(w http.ResponseWriter, r *http.Request) {
	if s.opts.DisablePasswordLogin {
		passwordLoginDisabled(w)
		return
	}
	if !s.EmailLinks() {
		writeError(w, http.StatusConflict, apierr.CodeMailNotConfigured,
			"emailing a reset link isn't set up: ask an admin for one")
		return
	}
	var req struct {
		Email string `json:"email"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	email, ok := normalizeEmail(req.Email)
	if !ok {
		writeError(w, http.StatusBadRequest, apierr.CodeEmailInvalid, "a valid email address is required")
		return
	}
	ip := s.clientIP(r)
	limits := []struct {
		l   *ratelimit.Limiter
		key string
	}{
		{s.resetPerIP, ip},
		{s.resetPerEmail, email},
	}
	refund := func(n int) {
		for _, lim := range limits[:n] {
			lim.l.Give(lim.key)
		}
	}
	for i, lim := range limits {
		if ok, wait := lim.l.Take(lim.key); !ok {
			refund(i)
			limited(w, wait, apierr.CodeTooManyResets, "too many reset requests: try again later")
			return
		}
	}
	if ok, wait := s.resetStarts.take(); !ok {
		refund(len(limits))
		limited(w, wait, apierr.CodeTooManyResets, "too many reset requests: try again later")
		return
	}
	s.background(r.Context(), func(ctx context.Context) { s.emailReset(ctx, email) })
	writeJSON(w, http.StatusAccepted, struct{}{})
}

func (s *Service) emailReset(ctx context.Context, email string) {
	u, hash, err := s.userByEmail(ctx, email)
	switch {
	case errors.Is(err, errNotFound):
		return
	case err != nil:
		slog.ErrorContext(ctx, "auth: looking up an account to email a reset link", "err", err)
		return
	case hash == "":
		return
	}
	token, expires, linkID, err := s.createSelfReset(ctx, u.ID)
	if err != nil {
		slog.ErrorContext(ctx, "auth: making a reset link to email", "user_id", u.ID, "err", err)
		return
	}
	locale, err := s.recipientLocale(ctx, u.ID)
	if err != nil {
		slog.WarnContext(ctx, "auth: reading the language of a reset email", "user_id", u.ID, "err", err)
	}
	err = s.opts.Mail.SendTemplate(ctx, u.Email, locale, "password_reset", map[string]string{
		"name":       u.Name,
		"hours":      strconv.Itoa(int(math.Ceil(s.opts.ResetTTL.Hours()))),
		"action_url": s.opts.PublicURL + "/reset-password#" + token,
	})
	audit.Log(ctx, s.db, audit.Event{Action: audit.PasswordResetRequested, TargetType: audit.TargetUser, Target: u.ID,
		Details: audit.Details{"emailed": err == nil}})
	if err != nil {
		logMailFailure(ctx, "auth: the reset email wasn't sent", u.ID, err)
		s.dropReset(ctx, linkID)
		return
	}
	if err := s.dropOlderResets(ctx, u.ID, linkID); err != nil {
		slog.WarnContext(ctx, "auth: removing the reset links an emailed one replaces", "user_id", u.ID, "err", err)
	}
	slog.InfoContext(ctx, "password reset link emailed", "user_id", u.ID, "expires_at", expires)
}

func logMailFailure(ctx context.Context, msg string, userID int64, err error) {
	var se *mail.SendError
	if errors.As(err, &se) {
		slog.WarnContext(ctx, msg, "user_id", userID, "stage", se.Stage, "smtp_code", se.Code)
		return
	}
	slog.ErrorContext(ctx, msg, "user_id", userID, "err", err)
}

func (s *Service) createSelfReset(ctx context.Context, userID int64) (token, expires string, id int64, err error) {
	now := s.now()
	expires = stamp(now.Add(s.opts.ResetTTL))
	token, hash := newToken()
	err = s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO password_resets (user_id, token_hash, created_by, created_at, expires_at)
			VALUES (?, ?, NULL, ?, ?)`, userID, hash, stamp(now), expires)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return "", "", 0, err
	}
	return token, expires, id, nil
}

func (s *Service) dropOlderResets(ctx context.Context, userID, id int64) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `DELETE FROM password_resets WHERE user_id = ? AND id < ?`, userID, id)
	return err
}

func (s *Service) dropReset(ctx context.Context, id int64) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM password_resets WHERE id = ?`, id); err != nil {
		slog.WarnContext(ctx, "auth: removing a reset link that wasn't emailed", "err", err)
	}
}

func (s *Service) recipientLocale(ctx context.Context, userID int64) (string, error) {
	var locale string
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(NULLIF(u.locale, ''), (
			SELECT w.locale FROM memberships m JOIN workspaces w ON w.id = m.workspace_id
			WHERE m.user_id = u.id AND w.locale <> '' ORDER BY w.id LIMIT 1), '')
		FROM users u WHERE u.id = ?`, userID).Scan(&locale)
	return locale, err
}

func (s *Service) takeInviteMail(admin int64, email string) (release func(), wait time.Duration) {
	adminKey := strconv.FormatInt(admin, 10)
	if ok, wait := s.invitePerAdmin.Take(adminKey); !ok {
		return nil, wait
	}
	if ok, wait := s.invitePerRecipient.Take(email); !ok {
		s.invitePerAdmin.Give(adminKey)
		return nil, wait
	}
	return func() {
		s.invitePerAdmin.Give(adminKey)
		s.invitePerRecipient.Give(email)
	}, 0
}

func (s *Service) emailInvite(ctx context.Context, m Membership, by User, inv Invite, token string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), inviteMailTimeout)
	defer cancel()
	defer context.AfterFunc(s.workCtx, cancel)()
	s.mailWork.Add(1)
	defer s.mailWork.Done()
	days := int(math.Ceil(s.opts.InviteTTL.Hours() / 24))
	err := s.opts.Mail.SendTemplate(ctx, inv.Email, s.inviteLocale(ctx, inv.Email, m, by.ID), "invite", map[string]string{
		"inviter":    by.Name,
		"workspace":  m.WorkspaceName,
		"days":       strconv.Itoa(days),
		"action_url": s.opts.PublicURL + "/invite#" + token,
	})
	if err != nil {
		logMailFailure(ctx, "auth: the invite email wasn't sent", by.ID, err)
		return false
	}
	audit.Log(ctx, s.db, audit.Event{Workspace: m.WorkspaceID, Actor: by.ID, Action: audit.InviteEmailed,
		TargetType: audit.TargetInvite, Target: inv.ID})
	slog.InfoContext(ctx, "invite emailed", "invite_id", inv.ID, "workspace_id", m.WorkspaceID, "by", by.ID)
	return true
}

func (s *Service) inviteLocale(ctx context.Context, email string, m Membership, admin int64) string {
	if u, _, err := s.userByEmail(ctx, email); err == nil {
		if locale, err := s.UserLocale(ctx, u.ID); err == nil && locale != "" {
			return locale
		}
	}
	if m.Locale != "" {
		return m.Locale
	}
	locale, err := s.UserLocale(ctx, admin)
	if err != nil {
		slog.WarnContext(ctx, "auth: reading the language of an invite email", "user_id", admin, "err", err)
	}
	return locale
}
