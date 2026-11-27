package auth

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"regexp"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/audit"
)

var localeRe = regexp.MustCompile(`^[a-z]{2,3}(-[A-Z][a-z]{3})?(-([A-Z]{2}|[0-9]{3}))?$`)

func ValidLocale(tag string) bool {
	return tag == "" || localeRe.MatchString(tag)
}

const errBadLocale = `locale must be a BCP 47 tag such as "pt-BR", or "" for none`

type MeUser struct {
	User
	Locale   string `json:"locale"`
	TimeZone string `json:"time_zone"`
}

func (s *Service) UserLocale(ctx context.Context, userID int64) (string, error) {
	var locale string
	err := s.db.QueryRowContext(ctx, `SELECT locale FROM users WHERE id = ?`, userID).Scan(&locale)
	return locale, err
}

func (s *Service) handleUpdateMe(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	var req struct {
		Locale          *string `json:"locale"`
		TimeZone        *string `json:"time_zone"`
		BrowserTimeZone *string `json:"browser_time_zone"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Locale != nil && !ValidLocale(*req.Locale) {
		writeError(w, http.StatusBadRequest, apierr.CodeLocaleInvalid, errBadLocale)
		return
	}
	if !s.updateTimeZone(w, r, u.ID, req.TimeZone, req.BrowserTimeZone) {
		return
	}
	if req.Locale != nil {
		if _, err := s.db.ExecContext(r.Context(), `UPDATE users SET locale = ? WHERE id = ?`, *req.Locale, u.ID); err != nil {
			serverError(w, r, err)
			return
		}
	}
	s.writeMe(w, r, http.StatusOK, u)
}

func (s *Service) handleUpdateWorkspace(w http.ResponseWriter, r *http.Request) {
	m, _ := MembershipFrom(r.Context())
	var req struct {
		Locale *string `json:"locale"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Locale != nil {
		if !ValidLocale(*req.Locale) {
			writeError(w, http.StatusBadRequest, apierr.CodeLocaleInvalid, errBadLocale)
			return
		}
		err := s.write(r.Context(), func(tx *sql.Tx) error {
			if err := EnsureActorEnabled(r.Context(), tx); err != nil {
				return err
			}
			if _, err := tx.ExecContext(r.Context(), `UPDATE workspaces SET locale = ? WHERE id = ?`, *req.Locale, m.WorkspaceID); err != nil {
				return err
			}
			return audit.Record(r.Context(), tx, audit.Event{Workspace: m.WorkspaceID, Action: audit.WorkspaceUpdated,
				TargetType: audit.TargetWorkspace, Target: m.WorkspaceID, Details: localeDetails(*req.Locale)})
		})
		if errors.Is(err, errAccountDisabled) {
			accountDisabled(w)
			return
		}
		if err != nil {
			serverError(w, r, err)
			return
		}
		m.Locale = *req.Locale
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Service) Language(r *http.Request, browser string) string {
	u, _ := UserFrom(r.Context())
	picked, err := s.UserLocale(r.Context(), u.ID)
	if err != nil {
		slog.WarnContext(r.Context(), "auth: reading the user's language", "user", u.ID, "err", err)
	}
	if picked != "" {
		return picked
	}
	if browser != "" {
		return browser
	}
	m, _ := MembershipFrom(r.Context())
	return m.Locale
}

func localeDetails(locale string) audit.Details {
	d := audit.Details{"fields": []string{"locale"}}
	if locale != "" {
		d["locale"] = locale
	}
	return d
}
