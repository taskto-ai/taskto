package cliaccounts

import (
	"context"
	"path/filepath"
	"testing"

	"taskto.ai/taskto/internal/db"
)

// Only container mode is recorded, once, and only then may a container worker hold files.
func TestWorkerModeHistory(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	may := func() bool {
		t.Helper()
		got, err := ContainerWorkerMayHoldFiles(ctx, sqlDB)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if may() {
		t.Fatal("a new install may hold a container worker's files")
	}
	for _, mode := range []Mode{Off, Local, LocalUnsafe} {
		if err := RecordWorkerMode(ctx, sqlDB, mode); err != nil {
			t.Fatal(err)
		}
		if may() {
			t.Fatalf("mode %q was recorded as a container worker", mode)
		}
	}
	for range 2 { // at every start
		if err := RecordWorkerMode(ctx, sqlDB, Container); err != nil {
			t.Fatal(err)
		}
	}
	if !may() {
		t.Fatal("container mode wasn't recorded")
	}
	// Switching to another mode later doesn't forget it.
	if err := RecordWorkerMode(ctx, sqlDB, Local); err != nil {
		t.Fatal(err)
	}
	if !may() {
		t.Fatal("the record was lost")
	}
}
