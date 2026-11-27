package desk

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"taskto.ai/taskto/internal/db"
)

func deskService(t *testing.T) (*sql.DB, *Service) {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := db.Migrate(context.Background(), sqlDB); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`INSERT INTO users (email, name, password_hash) VALUES ('person@example.test', 'Person', 'hash')`); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`INSERT INTO workspaces (name) VALUES ('Workspace')`); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (1, 1, 'admin')`); err != nil {
		t.Fatal(err)
	}
	return sqlDB, New(sqlDB)
}

func TestMainDeskIsVirtualUntilSavedAndSurvivesReload(t *testing.T) {
	sqlDB, service := deskService(t)
	ctx := context.Background()
	list, err := service.List(ctx, 1, 1)
	if err != nil || len(list.Desks) != 1 || list.Desks[0].ID != 0 {
		t.Fatalf("initial list = %+v, err %v", list, err)
	}
	var count int
	if err := sqlDB.QueryRow(`SELECT count(*) FROM desks`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("GET wrote %d rows, err %v", count, err)
	}
	empty, err := service.Get(ctx, 1, 1, 0)
	if err != nil || empty.ID != 0 || empty.Version != 0 || string(empty.Layout) != string(emptyLayout) {
		t.Fatalf("initial main desk = %+v, err %v", empty, err)
	}
	raw := []byte(`{"v":1,"focus":"g1","root":{"group":"g1","active":"t1","tabs":[{"id":"t1","path":"/w/1/home"}]}}`)
	saved, err := service.SaveLayout(ctx, 1, 1, 0, 0, raw)
	if err != nil || saved.ID == 0 || saved.Version != 1 {
		t.Fatalf("first save = %+v, err %v", saved, err)
	}
	list, err = service.List(ctx, 1, 1)
	if err != nil || len(list.Desks) != 1 || list.Desks[0].ID != saved.ID || list.Desks[0].Name != "" {
		t.Fatalf("list after save = %+v, err %v", list, err)
	}
	loaded, err := New(sqlDB).Get(ctx, 1, 1, 0)
	if err != nil || loaded.ID != saved.ID || loaded.Version != 1 || string(loaded.Layout) != string(saved.Layout) {
		t.Fatalf("reload = %+v, err %v", loaded, err)
	}
	current, err := service.SaveLayout(ctx, 1, 1, 0, 0, raw)
	if !errors.Is(err, ErrChanged) || current.Version != 1 || current.ID != saved.ID {
		t.Fatalf("stale save current = %+v, err %v", current, err)
	}
}

func TestConcurrentFirstMainDeskSavesOnlyOneWins(t *testing.T) {
	sqlDB, service := deskService(t)
	raw := []byte(`{"v":1,"focus":"g1","root":{"group":"g1","active":"t1","tabs":[{"id":"t1","path":"/w/1/home"}]}}`)
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := service.SaveLayout(context.Background(), 1, 1, 0, 0, raw)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	wins, changed := 0, 0
	for err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrChanged):
			changed++
		default:
			t.Fatalf("save error = %v", err)
		}
	}
	if wins != 1 || changed != 1 {
		t.Fatalf("successes %d, conflicts %d", wins, changed)
	}
	var count int
	if err := sqlDB.QueryRow(`SELECT count(*) FROM desks WHERE user_id = 1 AND workspace_id = 1 AND name = '' AND version = 1`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("main desk rows = %d, err %v", count, err)
	}
}
