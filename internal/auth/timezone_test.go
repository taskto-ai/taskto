package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestValidTimeZone(t *testing.T) {
	for name, want := range map[string]bool{
		"":                                    true, // none
		"UTC":                                 true,
		"Europe/Lisbon":                       true,
		"America/Sao_Paulo":                   true,
		"Asia/Kolkata":                        true,
		"Local":                               false, // the server's own zone is no one's
		"Mars/Olympus":                        false,
		" UTC":                                false,
		"../../etc/localtime":                 false,
		"/etc/localtime":                      false,
		strings.Repeat("a", MaxTimeZoneLen+1): false,
	} {
		if got := ValidTimeZone(name); got != want {
			t.Errorf("ValidTimeZone(%q) = %v, want %v", name, got, want)
		}
	}
}

func timeZoneOf(t *testing.T, e *env, userID int64) string {
	t.Helper()
	zone, err := UserTimeZone(context.Background(), e.db, userID)
	if err != nil {
		t.Fatal(err)
	}
	return zone
}

func TestATimeZoneIsStoredPerUserAndShownOnlyToThem(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	member, memberMe := e.join(e.invite(owner, ws, "m@example.com", RoleMember), "Member")
	ownerMe := decode[me](t, owner.do(http.MethodGet, "/api/me", nil))
	if ownerMe.User.TimeZone != "" || memberMe.User.TimeZone != "" {
		t.Fatalf("zones before any was set: %q, %q", ownerMe.User.TimeZone, memberMe.User.TimeZone)
	}

	rec := owner.do(http.MethodPatch, "/api/me", map[string]string{"time_zone": "Europe/Lisbon"})
	expect(t, rec, http.StatusOK)
	if got := decode[me](t, rec).User.TimeZone; got != "Europe/Lisbon" {
		t.Fatalf("the answer's zone = %q", got)
	}
	if got := decode[me](t, owner.do(http.MethodGet, "/api/me", nil)).User.TimeZone; got != "Europe/Lisbon" {
		t.Fatalf("GET /api/me zone = %q", got)
	}
	// Nobody else's changed, and a member list doesn't show anyone's.
	if got := decode[me](t, member.do(http.MethodGet, "/api/me", nil)).User.TimeZone; got != "" {
		t.Fatalf("the member's zone = %q", got)
	}
	members := member.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/members", ws), nil)
	expect(t, members, http.StatusOK)
	if strings.Contains(members.Body.String(), "Lisbon") || strings.Contains(members.Body.String(), "time_zone") {
		t.Fatalf("a member list shows a time zone: %s", members.Body.String())
	}
	// The member's own change is theirs alone.
	expect(t, member.do(http.MethodPatch, "/api/me", map[string]string{"time_zone": "Asia/Tokyo"}), http.StatusOK)
	if got := timeZoneOf(t, e, memberMe.User.ID); got != "Asia/Tokyo" {
		t.Fatalf("the member's zone = %q", got)
	}
	if got := timeZoneOf(t, e, ownerMe.User.ID); got != "Europe/Lisbon" {
		t.Fatalf("the owner's zone moved to %q", got)
	}
	// A zone and a language are changed by the same call, and what is left out stays.
	expect(t, owner.do(http.MethodPatch, "/api/me", map[string]string{"locale": "pt-BR"}), http.StatusOK)
	if got := timeZoneOf(t, e, ownerMe.User.ID); got != "Europe/Lisbon" {
		t.Fatalf("a change of language changed the zone to %q", got)
	}
	// "" clears it.
	expect(t, owner.do(http.MethodPatch, "/api/me", map[string]string{"time_zone": ""}), http.StatusOK)
	if got := timeZoneOf(t, e, ownerMe.User.ID); got != "" {
		t.Fatalf("a cleared zone = %q", got)
	}
	// Not logged in: nothing.
	expect(t, e.client().do(http.MethodPatch, "/api/me", map[string]string{"time_zone": "UTC"}), http.StatusUnauthorized)
}

func TestABadTimeZoneIsRefusedWithACodeAndChangesNothing(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	id := decode[me](t, owner.do(http.MethodGet, "/api/me", nil)).User.ID
	expect(t, owner.do(http.MethodPatch, "/api/me", map[string]string{"time_zone": "Asia/Tokyo"}), http.StatusOK)
	for _, bad := range []string{"Mars/Olympus", "Local", "tokyo", " Asia/Tokyo", strings.Repeat("x", 65), "../../etc/passwd"} {
		rec := owner.do(http.MethodPatch, "/api/me", map[string]string{"time_zone": bad, "locale": "pt-BR"})
		expect(t, rec, http.StatusBadRequest)
		if got := decode[struct{ Code string }](t, rec).Code; got != "time_zone_invalid" {
			t.Fatalf("%q: code %q", bad, got)
		}
	}
	// The refused calls changed neither the zone nor the language sent with them.
	if got := timeZoneOf(t, e, id); got != "Asia/Tokyo" {
		t.Fatalf("zone after refused changes = %q", got)
	}
	if n := e.count(`SELECT count(*) FROM users WHERE locale <> ''`); n != 0 {
		t.Fatalf("a language was saved by a refused call: %d", n)
	}
	expect(t, owner.do(http.MethodPatch, "/api/me", map[string]any{"time_zone": 5}), http.StatusBadRequest)
}

func TestTheBrowsersZoneOnlyFillsAnEmptyOne(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	id := decode[me](t, owner.do(http.MethodGet, "/api/me", nil)).User.ID

	// None yet: the browser's is taken.
	expect(t, owner.do(http.MethodPatch, "/api/me", map[string]string{"browser_time_zone": "America/Sao_Paulo"}), http.StatusOK)
	if got := timeZoneOf(t, e, id); got != "America/Sao_Paulo" {
		t.Fatalf("the browser's zone was not taken: %q", got)
	}
	// Set: another browser (or the same one, travelling) doesn't change it.
	expect(t, owner.do(http.MethodPatch, "/api/me", map[string]string{"browser_time_zone": "Asia/Tokyo"}), http.StatusOK)
	if got := timeZoneOf(t, e, id); got != "America/Sao_Paulo" {
		t.Fatalf("the browser's zone replaced one: %q", got)
	}
	// A person's own choice does, and clearing it makes room for the browser's again.
	expect(t, owner.do(http.MethodPatch, "/api/me", map[string]string{"time_zone": "Europe/Lisbon"}), http.StatusOK)
	expect(t, owner.do(http.MethodPatch, "/api/me", map[string]string{"time_zone": ""}), http.StatusOK)
	expect(t, owner.do(http.MethodPatch, "/api/me", map[string]string{"browser_time_zone": "Asia/Tokyo"}), http.StatusOK)
	if got := timeZoneOf(t, e, id); got != "Asia/Tokyo" {
		t.Fatalf("zone = %q", got)
	}

	// A browser's zone this server doesn't know is no reason to fail: it is left out.
	e.exec(t, `UPDATE users SET time_zone = ''`)
	for _, odd := range []string{"Etc/Unknown", "Local", ""} {
		expect(t, owner.do(http.MethodPatch, "/api/me", map[string]string{"browser_time_zone": odd}), http.StatusOK)
		if got := timeZoneOf(t, e, id); got != "" {
			t.Fatalf("browser zone %q was stored as %q", odd, got)
		}
	}
}

func TestTimeZoneHooksRunInTheChangeAndCanUndoIt(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	id := decode[me](t, owner.do(http.MethodGet, "/api/me", nil)).User.ID
	var told []int64
	e.svc.OnTimeZoneChanged(func(ctx context.Context, tx *sql.Tx, userID int64) error {
		// The new zone is already there, in the transaction.
		zone, err := UserTimeZone(ctx, tx, userID)
		if err != nil || zone == "" {
			t.Errorf("hook saw zone %q, %v", zone, err)
		}
		told = append(told, userID)
		return nil
	})
	expect(t, owner.do(http.MethodPatch, "/api/me", map[string]string{"time_zone": "Asia/Tokyo"}), http.StatusOK)
	// The same zone again, and a browser's on a set one, are no change.
	expect(t, owner.do(http.MethodPatch, "/api/me", map[string]string{"time_zone": "Asia/Tokyo"}), http.StatusOK)
	expect(t, owner.do(http.MethodPatch, "/api/me", map[string]string{"browser_time_zone": "UTC"}), http.StatusOK)
	if len(told) != 1 || told[0] != id {
		t.Fatalf("hooks were told %v, want once about %d", told, id)
	}

	// A hook that fails undoes the change.
	e.svc.OnTimeZoneChanged(func(context.Context, *sql.Tx, int64) error { return errors.New("no") })
	expect(t, owner.do(http.MethodPatch, "/api/me", map[string]string{"time_zone": "Europe/Lisbon"}), http.StatusInternalServerError)
	if got := timeZoneOf(t, e, id); got != "Asia/Tokyo" {
		t.Fatalf("zone after a failed hook = %q", got)
	}
}

func TestUserLocationIsTheZoneElseUTC(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	id := decode[me](t, owner.do(http.MethodGet, "/api/me", nil)).User.ID
	ctx := context.Background()
	loc, err := UserLocation(ctx, e.db, id)
	if err != nil || loc.String() != "UTC" {
		t.Fatalf("no zone: %v, %v", loc, err)
	}
	expect(t, owner.do(http.MethodPatch, "/api/me", map[string]string{"time_zone": "Asia/Tokyo"}), http.StatusOK)
	if loc, err = UserLocation(ctx, e.db, id); err != nil || loc.String() != "Asia/Tokyo" {
		t.Fatalf("a zone: %v, %v", loc, err)
	}
	// A stored name this machine doesn't know falls back to UTC, not to an error.
	e.exec(t, `UPDATE users SET time_zone = 'Nowhere/Land' WHERE id = ?`, id)
	if loc, err = UserLocation(ctx, e.db, id); err != nil || loc.String() != "UTC" {
		t.Fatalf("an unknown zone: %v, %v", loc, err)
	}
	if _, err := UserLocation(ctx, e.db, 9999); err == nil {
		t.Fatal("a user who isn't there has a zone")
	}
}
