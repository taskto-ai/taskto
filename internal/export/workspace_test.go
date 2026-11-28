package export

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/db"
)

func memberless(t *testing.T, extra int) (*run, *sql.DB, *db.Hooks) {
	t.Helper()
	ctx := t.Context()
	hooks := &db.Hooks{}
	sqlDB, err := db.OpenHooked(ctx, filepath.Join(t.TempDir(), db.FileName), hooks)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	var ws, admin int64
	if err := sqlDB.QueryRow(`INSERT INTO workspaces (name) VALUES ('W') RETURNING id`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	for i := range extra + 1 {
		var id int64
		if err := sqlDB.QueryRow(`INSERT INTO users (email, name, password_hash) VALUES (?, ?, 'x') RETURNING id`,
			fmt.Sprintf("member-%d@example.com", i), fmt.Sprintf("Member %04d", extra-i)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		role := "member"
		if i == 0 {
			admin, role = id, "admin"
		}
		if _, err := sqlDB.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, ?)`, ws, id, role); err != nil {
			t.Fatal(err)
		}
	}
	s := New(sqlDB, nil, nil, nil, nil, nil, nil, Options{})
	ex := &run{svc: s, user: auth.User{ID: admin}, ws: auth.Membership{WorkspaceID: ws}, at: s.now(), names: newNames()}
	ex.mf = manifest{Format: Format, Scope: "workspace", Counts: map[string]int{}, Spaces: []manifestSpace{}, Skipped: []skipped{}}
	return ex, sqlDB, hooks
}

func armNth(h *db.Hooks, fragment string, n int, fn func()) {
	count := 0
	var arm func()
	arm = func() {
		count++
		if count == n {
			fn()
			return
		}
		h.BeforeQuery(fragment, arm)
	}
	h.BeforeQuery(fragment, arm)
}

func TestWorkspaceExportChecksTheAdminBeforeTheZipIsClosed(t *testing.T) {
	ex, sqlDB, hooks := memberless(t, 0)
	armNth(hooks, "m.role = ? AND u.disabled_at IS NULL", 3, func() {
		if _, err := sqlDB.Exec(`UPDATE memberships SET role = 'member' WHERE user_id = ?`, ex.user.ID); err != nil {
			t.Error(err)
		}
	})
	var out bytes.Buffer
	if err := ex.write(t.Context(), &out); !errors.Is(err, errNotAdmin) {
		t.Fatalf("an export whose admin was demoted at its end = %v, want errNotAdmin", err)
	}
}

func TestWorkspaceExportChecksTheAdminForEachPageOfMembers(t *testing.T) {
	ex, sqlDB, hooks := memberless(t, membersPage+3)
	armNth(hooks, "m.role = ? AND u.disabled_at IS NULL", 3, func() {
		if _, err := sqlDB.Exec(`UPDATE memberships SET role = 'member' WHERE user_id = ?`, ex.user.ID); err != nil {
			t.Error(err)
		}
		pages := 0
		var arm func()
		arm = func() { pages++; hooks.BeforeQuery("FROM memberships m JOIN users u", arm) }
		hooks.BeforeQuery("FROM memberships m JOIN users u", arm)
		t.Cleanup(func() {
			if pages != 0 {
				t.Errorf("%d pages of members were read after the admin was demoted", pages)
			}
		})
	})
	var out bytes.Buffer
	if err := ex.write(t.Context(), &out); !errors.Is(err, errNotAdmin) {
		t.Fatalf("export = %v, want errNotAdmin", err)
	}
}

func TestWorkspaceExportChecksTheAdminAccountIsNotDisabled(t *testing.T) {
	ex, sqlDB, hooks := memberless(t, 0)
	armNth(hooks, "m.role = ? AND u.disabled_at IS NULL", 3, func() {
		if _, err := sqlDB.Exec(`UPDATE users SET disabled_at = CURRENT_TIMESTAMP WHERE id = ?`, ex.user.ID); err != nil {
			t.Error(err)
		}
	})
	var out bytes.Buffer
	if err := ex.write(t.Context(), &out); !errors.Is(err, errNotAdmin) {
		t.Fatalf("an export whose admin was disabled at its end = %v, want errNotAdmin", err)
	}
}
