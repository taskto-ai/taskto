package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestWorkspaceCreatorsMarksTheOldestWorkspaceAsTheSetupOne(t *testing.T) {
	ctx := context.Background()
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	if _, err := migrate(ctx, sqlDB, migrationsBefore(t, "_workspace_creators.sql")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"First", "Second", "Third"} {
		if _, err := sqlDB.Exec(`INSERT INTO workspaces (name) VALUES (?)`, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	rows, err := sqlDB.Query(`SELECT name, is_setup, created_by IS NULL FROM workspaces ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	got := map[string]int{}
	for rows.Next() {
		var name string
		var setup, noCreator int
		if err := rows.Scan(&name, &setup, &noCreator); err != nil {
			t.Fatal(err)
		}
		got[name] = setup
		if noCreator != 1 {
			t.Errorf("%s has a creator", name)
		}
	}
	if got["First"] != 1 || got["Second"] != 0 || got["Third"] != 0 {
		t.Fatalf("is_setup = %v, want only First", got)
	}
	if _, err := sqlDB.Exec(`UPDATE workspaces SET is_setup = 1 WHERE name = 'Second'`); err == nil {
		t.Fatal("a second workspace became the setup one")
	}
}
