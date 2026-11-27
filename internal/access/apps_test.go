package access

import (
	"database/sql"
	"slices"
	"testing"
)

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
		got := appAccess(Write, []AppRole{role}, true, false)
		have := caps{got.View, got.Design, got.Publish, got.Submit, got.ManageRoles, got.ReadRecords, got.ModifyRecords}
		if have != want {
			t.Errorf("%s: %+v, want %+v", role, have, want)
		}
	}
}

func TestRolesCombineAndTheSpaceCaps(t *testing.T) {
	got := appAccess(Write, []AppRole{AppUser, AppReader}, true, false)
	if got.ReadRecords != ScopeAll || got.ModifyRecords != ScopeOwn || !got.Submit {
		t.Fatalf("user and reader = %+v", got)
	}
	got = appAccess(Read, AppRoles, true, false)
	if got.Design || got.Publish || got.Submit || got.ManageRoles || got.ModifyRecords != ScopeNone || got.ReadRecords != ScopeAll {
		t.Fatalf("every role under a read-only space = %+v", got)
	}
	if got := appAccess(Read, []AppRole{AppUser}, true, false); got.ReadRecords != ScopeOwn {
		t.Fatalf("user under a read-only space = %+v", got)
	}
	got = appAccess(None, AppRoles, true, false)
	if got.View || got.Design || got.Submit || got.ReadRecords != ScopeNone || got.ModifyRecords != ScopeNone {
		t.Fatalf("no access to the space = %+v", got)
	}
}

func TestUnrestrictedAppIsTheSpaceAlone(t *testing.T) {
	w := appAccess(Write, nil, false, false)
	if w.Restricted || !w.View || !w.Design || !w.Publish || !w.Submit || !w.ManageRoles || w.ReadRecords != ScopeAll || w.ModifyRecords != ScopeAll {
		t.Fatalf("write = %+v", w)
	}
	r := appAccess(Read, nil, false, false)
	if r.Restricted || !r.View || r.Design || r.Publish || r.Submit || r.ManageRoles || r.ReadRecords != ScopeAll || r.ModifyRecords != ScopeNone {
		t.Fatalf("read = %+v", r)
	}
	if n := appAccess(None, nil, false, false); n.View || n.ReadRecords != ScopeNone {
		t.Fatalf("none = %+v", n)
	}
}

func TestResolvePicksOwnThenDefault(t *testing.T) {
	row := func(user int64, role AppRole) grantRow {
		return grantRow{userID: sql.NullInt64{Int64: user, Valid: user != 0}, role: role}
	}
	rows := []grantRow{row(0, AppReader), row(7, AppUser), row(7, AppBuilder), row(8, AppDataManager)}
	if roles, restricted := resolve(rows, 7, true); !restricted || !slices.Equal(roles, []AppRole{AppUser, AppBuilder}) {
		t.Fatalf("own = %v, %v", roles, restricted)
	}
	if roles, restricted := resolve(rows, 9, true); !restricted || !slices.Equal(roles, []AppRole{AppReader}) {
		t.Fatalf("default = %v, %v", roles, restricted)
	}
	if roles, restricted := resolve(rows[1:], 9, true); !restricted || len(roles) != 0 {
		t.Fatalf("unnamed member = %v, %v; want no roles", roles, restricted)
	}
	if a := appAccess(Write, []AppRole{}, true, false); a.View || a.Submit || a.ReadRecords != ScopeNone {
		t.Fatalf("unnamed space editor = %+v; want nothing", a)
	}
	if _, restricted := resolve(nil, 9, false); restricted {
		t.Fatal("an app without assignments is restricted")
	}
}
