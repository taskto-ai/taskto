package db_test

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"taskto.ai/taskto/internal/accountdeletion"
	"taskto.ai/taskto/internal/db"
)

// worker is a CLI worker in container mode that notes whom it forgot, and whether that person's
// browser was still running then.
type worker struct {
	mu       sync.Mutex
	forgot   []int64
	browsers *browsers
	early    []int64
}

func (w *worker) WorkerKind() string { return "container" }

func (w *worker) Forget(_ context.Context, user int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.browsers.running(user) {
		w.early = append(w.early, user)
	}
	w.forgot = append(w.forgot, user)
	return nil
}

type browsers struct {
	mu     sync.Mutex
	forgot []int64
}

func (b *browsers) Forget(_ context.Context, user int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.forgot = append(b.forgot, user)
	return nil
}

func (b *browsers) running(user int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !slices.Contains(b.forgot, user)
}

// A purge's CLI step recorded before the browser step (0058) and the CLI worker history (0059):
// after both migrations, in that order, the cleanup runs to the end. The browser step 0058 made has
// the user's id, which it reads; the CLI steps 0059 made from the old one, one per kind of worker,
// wait for it and then are done.
func TestAnOldCLIStepIsCleanedUpAfterBothMigrations(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sqlDB, err := db.Open(ctx, filepath.Join(dir, db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	db.MigrateUpTo(t, sqlDB, 53)
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO purge_cleanups (user_id, kind, target, attempts) VALUES (7, 'cli_home', '7', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	b := &browsers{}
	w := &worker{browsers: b}
	svc := accountdeletion.New(accountdeletion.Options{DB: sqlDB, DataDir: dir, CLI: w, Browsers: b})
	if err := svc.RunCleanups(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(b.forgot, []int64{7}) {
		t.Errorf("browsers forgotten for %v, want [7]", b.forgot)
	}
	if !slices.Equal(w.forgot, []int64{7}) || len(w.early) != 0 {
		t.Errorf("the CLI worker forgot %v (with the browser still running for %v)", w.forgot, w.early)
	}
	if n, err := svc.Pending(ctx, 0); err != nil || n != 0 {
		t.Errorf("%d steps left (%v)", n, err)
	}
}
