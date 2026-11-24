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
)

// What accounts email (docs/api.md#email-links): a reset link a person asks for from the login
// page, and the invite link an admin chooses to send. It uses the server's one SMTP server (docs/install.md#email) and is on only when the
// operator also set the server's public address, which the link is built on: a link built from
// the request's Host header could be made to point anywhere by whoever asks, and the person who
// asks for a reset email isn't signed in.
//
// The link is a secret: it is never logged, and a failed send logs the step and the SMTP
// reply's number, not the server's words.

// Limits of the emailed reset link, per window. Both are keyed by what the client typed, the same
// way whether an account has the address or not, so an answer never says which addresses have
// one:
//
//   - per client address: stops one client sending mail to many people;
//   - per email: stops anyone flooding one inbox, from any number of addresses.
const (
	resetIPAttempts    = 20
	resetEmailAttempts = 5
	resetWindow        = time.Hour
	// resetStartBurst and resetStartEvery are the server's own limit on reset emails it starts,
	// for everyone together: a burst, then one more a second. It counts requests taken in, never
	// emails still being made, so a refusal says nothing about whether any address has an account.
	resetStartBurst = 30
	resetStartEvery = time.Second
	// mailConcurrency bounds the emails being made at once on the server. It works after the
	// request was answered: an email over it waits its turn, and is dropped, logged without an
	// address, if its time runs out first.
	mailConcurrency = 8
	// mailTimeout bounds one email from the answer to the end of the send: the wait for a turn,
	// the database work and the send.
	mailTimeout = time.Minute
)

// Limits of invitations sent by email, per window. The admin is signed in, so they are not about
// guessing; they keep a stolen session or a mistake from using the operator's relay to write to
// strangers:
//
//   - per admin, across workspaces: how many people one admin writes to;
//   - per invited address: how many times one inbox is written to, by anyone.
const (
	inviteAdminAttempts     = 30
	inviteRecipientAttempts = 3
	inviteWindow            = time.Hour
	// inviteMailTimeout bounds the send, on top of the sender's own.
	inviteMailTimeout = 45 * time.Second
)

// EmailLinks reports whether the server can email links: it has an SMTP server and a public
// address to build them on.
func (s *Service) EmailLinks() bool {
	return s.opts.Mail.Configured() && s.opts.PublicURL != ""
}

// ResetByEmail reports whether people can ask for a reset link by email from the login page.
func (s *Service) ResetByEmail() bool {
	return s.EmailLinks() && !s.opts.DisablePasswordLogin
}

// WaitIdle waits for the emails being made in the background. Tests use it; a server that is
// shutting down uses Shutdown.
func (s *Service) WaitIdle() { s.mailWork.Wait() }

// Shutdown ends the emails being made in the background, which stop at their next step, and waits
// for them until ctx ends, so the caller can close the database after it returns. It reports ctx's
// error if some were still running then.
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

// background makes an email after a request was answered. Its context is made here, before the
// goroutine starts, so the goroutine never reads the request: it has the request's values (the
// client address of the audit log) but not its end, and it ends when the server stops
// (Shutdown) or after mailTimeout, which counts from now. It waits for one of mailConcurrency
// turns, and drops the email, logging it without the address, if it can't get one in time.
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

// handleRequestReset is POST /api/password-reset/request: someone who forgot their password
// gives their email address, and if an account with a password has it, gets a reset link by
// email. The answer is the same whether or not it does, and takes the same time: the account is
// looked up and the email sent after the answer, in the background.
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
		l   *limiter
		key string
	}{
		{s.resetPerIP, ip},
		{s.resetPerEmail, email},
	}
	refund := func(n int) {
		for _, lim := range limits[:n] {
			lim.l.give(lim.key)
		}
	}
	for i, lim := range limits {
		if ok, wait := lim.l.take(lim.key); !ok {
			refund(i)
			limited(w, wait, apierr.CodeTooManyResets, "too many reset requests: try again later")
			return
		}
	}
	// The server's own work is bounded too, by how many requests it takes in, never by how many
	// emails are still being made: that would show which addresses have an account. Full means
	// busy for everyone, whatever they asked.
	if ok, wait := s.resetStarts.take(); !ok {
		refund(len(limits))
		limited(w, wait, apierr.CodeTooManyResets, "too many reset requests: try again later")
		return
	}
	s.background(r.Context(), func(ctx context.Context) { s.emailReset(ctx, email) })
	writeJSON(w, http.StatusAccepted, struct{}{})
}

// emailReset emails a reset link to the account with this address, if there is one and it has a
// password. An account made by signing in with a provider has none to reset, and a link would
// give it one it never asked for.
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
		// The link nobody was sent is removed, and the earlier one, an admin's or the last one
		// emailed, still works: asking for a reset can't take away a link the person has.
		logMailFailure(ctx, "auth: the reset email wasn't sent", u.ID, err)
		s.dropReset(ctx, linkID)
		return
	}
	// The relay took the new link: the earlier ones are replaced.
	if err := s.dropOlderResets(ctx, u.ID, linkID); err != nil {
		slog.WarnContext(ctx, "auth: removing the reset links an emailed one replaces", "user_id", u.ID, "err", err)
	}
	slog.InfoContext(ctx, "password reset link emailed", "user_id", u.ID, "expires_at", expires)
}

// logMailFailure logs a failed send by its step and the SMTP reply's number, as the settings
// page's test does: the server's words can name hosts or repeat what was sent.
func logMailFailure(ctx context.Context, msg string, userID int64, err error) {
	var se *mail.SendError
	if errors.As(err, &se) {
		slog.WarnContext(ctx, msg, "user_id", userID, "stage", se.Stage, "smtp_code", se.Code)
		return
	}
	slog.ErrorContext(ctx, msg, "user_id", userID, "err", err)
}

// createSelfReset makes a reset link for userID at their own request, and returns its row's id.
// Nobody made it: created_by is null. It leaves the earlier links alone: they are replaced only
// once the email with this one is accepted (dropOlderResets), so a request, which anyone can make
// for any address, can't end a link the person already has, and a failed send loses nothing.
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

// dropOlderResets deletes userID's reset links made before the one with this id. The email was
// accepted, so this runs on a bounded context of its own, which doesn't end with ctx: a send
// that took nearly all of its time, or a server that is stopping, must not leave the old links valid.
func (s *Service) dropOlderResets(ctx context.Context, userID, id int64) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `DELETE FROM password_resets WHERE user_id = ? AND id < ?`, userID, id)
	return err
}

// dropReset deletes the reset link with this id, whose email wasn't sent. It doesn't stop for
// ctx, which may be what ended the send.
func (s *Service) dropReset(ctx context.Context, id int64) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM password_resets WHERE id = ?`, id); err != nil {
		slog.WarnContext(ctx, "auth: removing a reset link that wasn't emailed", "err", err)
	}
}

// recipientLocale is the language of an email to userID, who isn't making a request: the one
// they picked, else the default of the first of their workspaces that has one, else "" (English).
func (s *Service) recipientLocale(ctx context.Context, userID int64) (string, error) {
	var locale string
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(NULLIF(u.locale, ''), (
			SELECT w.locale FROM memberships m JOIN workspaces w ON w.id = m.workspace_id
			WHERE m.user_id = u.id AND w.locale <> '' ORDER BY w.id LIMIT 1), '')
		FROM users u WHERE u.id = ?`, userID).Scan(&locale)
	return locale, err
}

// takeInviteMail counts an invitation email by admin to email against both limits. It returns
// a function that gives the attempts back, for an invite that wasn't made after all, or nil and
// how long to wait.
func (s *Service) takeInviteMail(admin int64, email string) (release func(), wait time.Duration) {
	adminKey := strconv.FormatInt(admin, 10)
	if ok, wait := s.invitePerAdmin.take(adminKey); !ok {
		return nil, wait
	}
	if ok, wait := s.invitePerRecipient.take(email); !ok {
		s.invitePerAdmin.give(adminKey)
		return nil, wait
	}
	return func() {
		s.invitePerAdmin.give(adminKey)
		s.invitePerRecipient.give(email)
	}, 0
}

// emailInvite emails the invite's link to its address, from the admin by, and reports whether
// the relay took it. A failure is logged by its step and is no reason to refuse the invite,
// whose link the admin has to pass on by hand then. The invite is already committed, so the
// send isn't tied to the request: a client that disconnects during a slow relay doesn't abort it
// (the handler still waits, so a client that stays gets the answer), and the timeout bounds it.
func (s *Service) emailInvite(ctx context.Context, m Membership, by User, inv Invite, token string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), inviteMailTimeout)
	defer cancel()
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

// inviteLocale is the language of an invitation: the invited person's, if they already have an
// account that picked one, else the workspace's default, else the inviting admin's own.
func (s *Service) inviteLocale(ctx context.Context, email string, m Membership, admin int64) string {
	if u, _, err := s.userByEmail(ctx, email); err == nil {
		// Only the language the person picked: another workspace's default is not theirs, and
		// shouldn't outrank the default of the workspace that invites them.
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
