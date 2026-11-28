package desk

import (
	"context"
	"errors"
	"testing"
)

func TestADeskIdIsNeverGivenTwiceAfterLeavingAndRejoining(t *testing.T) {
	sqlDB, service := deskService(t)
	ctx := context.Background()
	old, err := service.SaveLayout(ctx, 1, 1, 0, 0, homeLayout(1, "home"))
	if err != nil || old.Version != 1 {
		t.Fatalf("first save = %+v, err %v", old, err)
	}
	if _, err := sqlDB.Exec(`DELETE FROM memberships WHERE workspace_id = 1 AND user_id = 1`); err != nil {
		t.Fatal(err)
	}
	join(t, sqlDB, 1, 1)
	fresh, err := service.SaveLayout(ctx, 1, 1, 0, 0, homeLayout(1, "boards"))
	if err != nil || fresh.Version != 1 {
		t.Fatalf("save after rejoining = %+v, err %v", fresh, err)
	}
	if fresh.ID == old.ID {
		t.Fatalf("the new desk got the old desk's id %d", old.ID)
	}
	want := storedOf(t, sqlDB, fresh.ID)
	stale, err := service.SaveLayout(ctx, 1, 1, old.ID, old.Version, homeLayout(1, "apps"))
	if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrChanged) {
		t.Fatalf("a save with the old id and version = %+v, err %v, want not found or desk changed", stale, err)
	}
	if got := storedOf(t, sqlDB, fresh.ID); got != want {
		t.Fatalf("a save with the old id changed the new desk from %+v to %+v", want, got)
	}
}

func TestASaveAdmittedBeforeALeaveLeavesNoDeskForANonMember(t *testing.T) {
	sqlDB, service := deskService(t)
	ctx := context.Background()
	if _, err := sqlDB.Exec(`DELETE FROM memberships WHERE workspace_id = 1 AND user_id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SaveLayout(ctx, 1, 1, 0, 0, homeLayout(1, "home")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a save by a non-member: err = %v, want not found", err)
	}
	if rows := deskRows(t, sqlDB, `1 = 1`); rows != 0 {
		t.Fatalf("a save by a non-member left %d desks", rows)
	}
	join(t, sqlDB, 1, 1)
	current, err := service.Get(ctx, 1, 1, 0)
	if err != nil || current.ID != 0 || current.Version != 0 {
		t.Fatalf("after rejoining the main desk = %+v, err %v", current, err)
	}
}
