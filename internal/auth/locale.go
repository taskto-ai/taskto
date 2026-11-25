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

// Languages (docs/i18n.md). The server stores which language each user picked, and each
// workspace's default, as BCP 47 tags; it doesn't hold the translations, which are the web
// app's. Any tag of a language, a script and a region is accepted, not only the web app's,
// so the agent can reply in a language the web app isn't translated into yet. Variants and
// extensions ("de-CH-1901", "en-US-u-ca-buddhist") aren't: a language is named without
// them, and the web app's files are too.

// localeRe is the subset of BCP 47 the web app's locale files are named in: a language,
// then optionally a script and a region, in their canonical case ("pt-BR", "zh-Hant-TW",
// "sr-Latn", "es-419").
var localeRe = regexp.MustCompile(`^[a-z]{2,3}(-[A-Z][a-z]{3})?(-([A-Z]{2}|[0-9]{3}))?$`)

// ValidLocale reports whether tag is a locale the server stores: "" (none picked) or a tag
// of localeRe's form, in canonical case.
func ValidLocale(tag string) bool {
	return tag == "" || localeRe.MatchString(tag)
}

const errBadLocale = `locale must be a BCP 47 tag such as "pt-BR", or "" for none`

// MeUser is the user in GET /api/me: their own settings come with them there, and only
// there (a workspace's member list doesn't show anyone's).
type MeUser struct {
	User
	Locale string `json:"locale"`
}

// UserLocale returns the language userID picked: a BCP 47 tag, or "" for none.
func (s *Service) UserLocale(ctx context.Context, userID int64) (string, error) {
	var locale string
	err := s.db.QueryRowContext(ctx, `SELECT locale FROM users WHERE id = ?`, userID).Scan(&locale)
	return locale, err
}

// handleUpdateMe changes the caller's own settings: for now, their language.
func (s *Service) handleUpdateMe(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
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
		if _, err := s.db.ExecContext(r.Context(), `UPDATE users SET locale = ? WHERE id = ?`, *req.Locale, u.ID); err != nil {
			serverError(w, r, err)
			return
		}
	}
	s.writeMe(w, r, http.StatusOK, u)
}

// handleUpdateWorkspace changes a workspace's settings, for its admins: for now, its
// default language.
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

// Language is the language to use by default for the caller of r, a member of a workspace
// (RequireMember), whose browser asks for browser (a tag of ValidLocale's form, or ""): the
// language they picked, else their browser's, else their workspace's default. "" is none
// known. The agent replies in it and dictation listens for it (docs/i18n.md#the-agent-and-dictation).
func (s *Service) Language(r *http.Request, browser string) string {
	u, _ := UserFrom(r.Context())
	picked, err := s.UserLocale(r.Context(), u.ID)
	if err != nil {
		// A default, never a reason to refuse the request.
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

// localeDetails are the details of a workspace's new default language: the tag, or only that the
// language was cleared.
func localeDetails(locale string) audit.Details {
	d := audit.Details{"fields": []string{"locale"}}
	if locale != "" {
		d["locale"] = locale
	}
	return d
}
