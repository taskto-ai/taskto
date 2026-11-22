package access

import (
	"database/sql"
	"slices"
	"testing"
)

// roleSet is the capabilities each role grants, once the space allows everything: the table
// in apps.go, checked a role at a time.
func TestRolesGrantTheirCapabilities(t *testing.T) {
	type caps struct {
		view, design, publish, submit, manage bool
		read, modify                          Scope
	}
	for role, want := range map[AppRole]caps{
		AppBuilder:     {true, true, false, false, true, ScopeNone, ScopeNone},
		AppPublisher:   {true, false, true, false, false, ScopeNone, ScopeNone},
		AppUser:        {true, false, false, true, false, ScopeOwn, ScopeOwn},
		AppDataManager: {true, false, false, true, false, ScopeAll, ScopeAll},
		AppReader:      {true, false, false, false, false, ScopeAll, ScopeNone},
	} {
		got := appAccess(Write, []AppRole{role}, true)
		have := caps{got.View, got.Design, got.Publish, got.Submit, got.ManageRoles, got.ReadRecords, got.ModifyRecords}
		if have != want {
			t.Errorf("%s: %+v, want %+v", role, have, want)
		}
	}
}

// Roles combine by union, and the space's access is a ceiling over the result.
func TestRolesCombineAndTheSpaceCaps(t *testing.T) {
	got := appAccess(Write, []AppRole{AppUser, AppReader}, true)
	if got.ReadRecords != ScopeAll || got.ModifyRecords != ScopeOwn || !got.Submit {
		t.Fatalf("user and reader = %+v", got)
	}
	// Every role of a viewer of the space is no more than reading.
	got = appAccess(Read, AppRoles, true)
	if got.Design || got.Publish || got.Submit || got.ManageRoles || got.ModifyRecords != ScopeNone || got.ReadRecords != ScopeAll {
		t.Fatalf("every role under a read-only space = %+v", got)
	}
	// And a user's own-only reading stays own-only.
	if got := appAccess(Read, []AppRole{AppUser}, true); got.ReadRecords != ScopeOwn {
		t.Fatalf("user under a read-only space = %+v", got)
	}
	// Nothing at all without access to the space.
	got = appAccess(None, AppRoles, true)
	if got.View || got.Design || got.Submit || got.ReadRecords != ScopeNone || got.ModifyRecords != ScopeNone {
		t.Fatalf("no access to the space = %+v", got)
	}
}

// An app with no assignments gives what the space gave before roles: write is every role,
// read is reading everything.
func TestUnrestrictedAppIsTheSpaceAlone(t *testing.T) {
	w := appAccess(Write, nil, false)
	if w.Restricted || !w.View || !w.Design || !w.Publish || !w.Submit || !w.ManageRoles || w.ReadRecords != ScopeAll || w.ModifyRecords != ScopeAll {
		t.Fatalf("write = %+v", w)
	}
	r := appAccess(Read, nil, false)
	if r.Restricted || !r.View || r.Design || r.Publish || r.Submit || r.ManageRoles || r.ReadRecords != ScopeAll || r.ModifyRecords != ScopeNone {
		t.Fatalf("read = %+v", r)
	}
	if n := appAccess(None, nil, false); n.View || n.ReadRecords != ScopeNone {
		t.Fatalf("none = %+v", n)
	}
}

// A user's own assignment replaces the default's; with neither, the app is unrestricted.
func TestResolvePicksOwnThenDefault(t *testing.T) {
	row := func(user int64, role AppRole) grantRow {
		return grantRow{userID: sql.NullInt64{Int64: user, Valid: user != 0}, role: role}
	}
	rows := []grantRow{row(0, AppReader), row(7, AppUser), row(7, AppBuilder), row(8, AppDataManager)}
	if roles, restricted := resolve(rows, 7); !restricted || !slices.Equal(roles, []AppRole{AppUser, AppBuilder}) {
		t.Fatalf("own = %v, %v", roles, restricted)
	}
	// Another's assignment is not theirs.
	if roles, restricted := resolve(rows, 9); !restricted || !slices.Equal(roles, []AppRole{AppReader}) {
		t.Fatalf("default = %v, %v", roles, restricted)
	}
	if _, restricted := resolve(rows[1:], 9); restricted {
		t.Fatal("another user's assignment restricted someone else")
	}
}
