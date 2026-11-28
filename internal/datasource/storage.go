package datasource

import (
	"context"
	"math"

	"taskto.ai/taskto/internal/quota"
)

type StorageFunc func(ctx context.Context, workspaceID, userID int64) (quota.SpaceUsage, bool, error)

var storageSpec = Spec{
	Scope: ScopeViewer,
	Outputs: map[string]string{
		"used_bytes":   "number",
		"quota_bytes":  "number",
		"used_mb":      "number",
		"quota_mb":     "number",
		"used_percent": "number",
	},
}

const megabyte = 1 << 20

func tenths(bytes int64) float64 {
	return math.Round(float64(bytes)/megabyte*10) / 10
}

func (s Sources) storage(ctx context.Context, v Viewer) (map[string]any, error) {
	if s.Storage == nil {
		return nil, ErrUnavailable
	}
	usage, found, err := s.Storage(ctx, v.WorkspaceID, v.UserID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrUnavailable
	}
	out := map[string]any{
		"used_bytes":  usage.Total,
		"quota_bytes": usage.Limit,
		"used_mb":     tenths(usage.Total),
		"quota_mb":    tenths(usage.Limit),
	}
	if usage.Limit > 0 {
		out["used_percent"] = math.Round(float64(usage.Total)/float64(usage.Limit)*1000) / 10
	}
	return out, nil
}
