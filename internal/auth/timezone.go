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

// A person's time zone. A card's due date is a calendar date with no zone, so "the morning of the
// due day" means something only in somebody's zone: each user has one, an IANA name such as
// "Europe/Lisbon". It is kept on the user, so what is timed for a person (the due-date reminders in
// package boards, the daily email summary in package notifications) is timed by their own clock.
//
// "" means none is known. The web app then sends the browser's once (browser_time_zone, which only
// ever fills an empty value), and until there is one the server uses UTC (UserLocation).

// MaxTimeZoneLen is the longest name stored.
const MaxTimeZoneLen = 64

// DefaultTimeZone is the zone of a person who has none.
const DefaultTimeZone = "UTC"

const errBadTimeZone = `time_zone must be an IANA time zone name such as "Europe/Lisbon", or "" for none`

// LoadTimeZone returns the zone an IANA name stands for, or an error for a name that isn't one:
// the empty name, "Local" (the server's own zone, which isn't a person's) and names the zone
// database doesn't have.
func LoadTimeZone(name string) (*time.Location, error) {
	if name == "" || name == "Local" || len(name) > MaxTimeZoneLen {
		return nil, errors.New("not an IANA time zone name")
	}
	return time.LoadLocation(name)
}

// ValidTimeZone reports whether name is a zone the server stores: "" (none) or an IANA name.
func ValidTimeZone(name string) bool {
	if name == "" {
		return true
	}
	_, err := LoadTimeZone(name)
	return err == nil
}

// Locator is what reads a person's zone: the database, or a transaction.
type Locator interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// UserTimeZone returns the zone name userID has, or "" for none.
func UserTimeZone(ctx context.Context, q Locator, userID int64) (string, error) {
	var zone string
	err := q.QueryRowContext(ctx, `SELECT time_zone FROM users WHERE id = ?`, userID).Scan(&zone)
	return zone, err
}

// UserLocation returns the zone to time things for userID by: their own, else UTC. A name the
// zone database no longer knows (a server moved to a machine with older data) is UTC too, and
// logged: a reminder at the wrong hour is better than none.
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
		return time.UTC, nil //nolint:nilerr // the fallback is the answer: an unknown name is no reason to fail
	}
	return loc, nil
}

// TimeZoneHook is told, in the transaction that changes a person's time zone, that it changed, with
// the person's ID; the new zone is already stored. What the hook writes is committed with the change,
// and an error it returns undoes it. It lets the features that time work for a person (boards'
// reminders, the email summary) move what they scheduled without this package knowing them. It is
// not called when the zone is set to what it was.
type TimeZoneHook func(ctx context.Context, tx *sql.Tx, userID int64) error

// OnTimeZoneChanged adds a hook. It is for startup, before the service serves requests.
func (s *Service) OnTimeZoneChanged(h TimeZoneHook) { s.zoneHooks = append(s.zoneHooks, h) }

// setTimeZone stores zone as userID's time zone and tells the hooks, in one transaction. With
// onlyIfUnset it stores it only if the person has none yet (the browser's, sent on every load, never
// takes the place of one a person or an earlier load set).
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

// updateTimeZone is handleUpdateMe's part for the two time zone fields: time_zone (an explicit
// choice, "" clears it) and browser_time_zone (only fills an empty one). It returns false when it
// has already written the error's answer.
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
	// The browser's zone is a guess to start from: one this server doesn't know is left out, not an
	// error, and an explicit time_zone in the same request already filled the value.
	if browser != nil && *browser != "" && ValidTimeZone(*browser) {
		if err := s.setTimeZone(r.Context(), userID, *browser, true); err != nil {
			return fail(err)
		}
	}
	return true
}
