package datasource

import (
	"context"
	"errors"
	"testing"

	"taskto.ai/taskto/internal/quota"
)

func reader(usage quota.SpaceUsage, found bool, err error) Sources {
	return Sources{Storage: func(_ context.Context, workspaceID, userID int64) (quota.SpaceUsage, bool, error) {
		if workspaceID != 7 || userID != 9 {
			return quota.SpaceUsage{}, false, errors.New("asked for another viewer")
		}
		return usage, found, err
	}}
}

func TestStorageUsageMapsTheViewersSpace(t *testing.T) {
	got, err := reader(quota.SpaceUsage{Total: 3 << 20, Limit: 12 << 20, KnowledgeVersioned: 1 << 20, KnowledgeUnversioned: 1 << 20, Attachments: 1 << 20}, true, nil).
		Read(context.Background(), Viewer{UserID: 9, WorkspaceID: 7}, StorageUsage,
			[]string{"used_bytes", "quota_bytes", "used_mb", "quota_mb", "used_percent"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"used_bytes": int64(3 << 20), "quota_bytes": int64(12 << 20), "used_mb": 3.0, "quota_mb": 12.0, "used_percent": 25.0}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s = %v, want %v", name, got[name], value)
		}
	}
}

func TestOnlyTheRequestedValuesAreReturnedAndAnUnlimitedQuotaHasNoPercent(t *testing.T) {
	got, err := reader(quota.SpaceUsage{Total: 5 << 20}, true, nil).
		Read(context.Background(), Viewer{UserID: 9, WorkspaceID: 7}, StorageUsage, []string{"used_mb", "used_percent"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["used_mb"] != 5.0 {
		t.Fatalf("got %v, want only used_mb", got)
	}
}

func TestWhatCannotBeReadIsUnavailable(t *testing.T) {
	v := Viewer{UserID: 9, WorkspaceID: 7}
	for name, c := range map[string]struct {
		s       Sources
		kind    Kind
		outputs []string
	}{
		"no source wired":   {Sources{}, StorageUsage, []string{"used_bytes"}},
		"no private space":  {reader(quota.SpaceUsage{}, false, nil), StorageUsage, []string{"used_bytes"}},
		"an unknown kind":   {reader(quota.SpaceUsage{}, true, nil), Kind("weather"), []string{"used_bytes"}},
		"an unknown output": {reader(quota.SpaceUsage{}, true, nil), StorageUsage, []string{"used_cats"}},
		"a failing source":  {reader(quota.SpaceUsage{}, true, errors.New("boom")), StorageUsage, []string{"used_bytes"}},
	} {
		if _, err := c.s.Read(context.Background(), v, c.kind, c.outputs); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := (Sources{}).Read(context.Background(), v, StorageUsage, []string{"used_bytes"}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("no source wired: %v", err)
	}
}
