package auth

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"taskto.ai/taskto/internal/apierr"
)

const MaxTimeZoneLen = 64

const DefaultTimeZone = "UTC"

const errBadTimeZone = `time_zone must be an IANA time zone name such as "Europe/Lisbon", or "" for none`

func LoadTimeZone(name string) (*time.Location, error) {
	if name == "" || name == "Local" || len(name) > MaxTimeZoneLen {
		return nil, errors.New("not an IANA time zone name")
	}
	return time.LoadLocation(name)
}

func ValidTimeZone(name string) bool {
	if name == "" {
		return true
	}
	_, err := LoadTimeZone(name)
	return err == nil
}

type Locator interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func UserTimeZone(ctx context.Context, q Locator, userID int64) (string, error) {
	var zone string
	err := q.QueryRowContext(ctx, `SELECT time_zone FROM users WHERE id = ?`, userID).Scan(&zone)
	return zone, err
}

func UserLocation(ctx context.Context, q Locator, userID int64) (*time.Location, error) {
	name, err := UserTimeZone(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return time.UTC, nil
	}
	loc, err := LoadTimeZone(name)
	if err != nil {
		slog.WarnContext(ctx, "auth: a user's time zone is unknown here, using UTC", "user", userID, "zone", name)
		loc = time.UTC
	}
	return loc, nil
}

type TimeZoneHook func(ctx context.Context, tx *sql.Tx, userID int64) error

func (s *Service) OnTimeZoneChanged(h TimeZoneHook) { s.zoneHooks = append(s.zoneHooks, h) }

func (s *Service) setTimeZone(ctx context.Context, userID int64, zone string, onlyIfUnset bool) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if err := EnsureActorEnabled(ctx, tx); err != nil {
			return err
		}
		prev, err := UserTimeZone(ctx, tx, userID)
		if err != nil {
			return err
		}
		if prev == zone || (onlyIfUnset && prev != "") {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET time_zone = ? WHERE id = ?`, zone, userID); err != nil {
			return err
		}
		for _, h := range s.zoneHooks {
			if err := h(ctx, tx, userID); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Service) updateTimeZone(w http.ResponseWriter, r *http.Request, userID int64, zone, browser *string) bool {
	fail := func(err error) bool {
		if errors.Is(err, errAccountDisabled) {
			accountDisabled(w)
		} else {
			serverError(w, r, err)
		}
		return false
	}
	if zone != nil {
		if !ValidTimeZone(*zone) {
			writeError(w, http.StatusBadRequest, apierr.CodeTimeZoneInvalid, errBadTimeZone)
			return false
		}
		if err := s.setTimeZone(r.Context(), userID, *zone, false); err != nil {
			return fail(err)
		}
	}
	if browser != nil && *browser != "" && ValidTimeZone(*browser) {
		if err := s.setTimeZone(r.Context(), userID, *browser, true); err != nil {
			return fail(err)
		}
	}
	return true
}
