package agenttools

import (
	"context"
	"encoding/json"
	"sort"

	"taskto.ai/taskto/internal/apps"
)

// appsActor is the call's user, as the agent working for them, marked with the access
// token over MCP.
func appsActor(c *call) apps.Actor { return apps.Actor{UserID: c.User, Agent: true, Via: c.Token} }

// appSpace checks the call's user may read spaceID, in the conversation's workspace.
// The apps' methods check the user's access again themselves.
func (s *Service) appSpace(ctx context.Context, c *call, spaceID int64) error {
	_, err := s.space(ctx, c, spaceID)
	return err
}

func (s *Service) appCatalog(_ context.Context, _ *call, args json.RawMessage) (any, error) {
	if err := decode(args, &struct{}{}); err != nil {
		return nil, err
	}
	return apps.Reference()
}

func (s *Service) listApps(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		SpaceID  int64 `json:"space_id"`
		Archived bool  `json:"archived"`
		// Offset is the apps to skip: a space can hold more than one reply carries.
		Offset int `json:"offset"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if a.Offset < 0 {
		return nil, invalidf("offset must be 0 or more")
	}
	if err := s.appSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	list, err := s.o.Apps.ListApps(ctx, appsActor(c), a.SpaceID, a.Archived)
	if err != nil {
		return nil, err
	}
	list = list[min(a.Offset, len(list)):]
	n, res, err := fitting(len(list), func(n int) any { return map[string]any{"apps": list[:n]} })
	if err == nil && n < len(list) {
		res = map[string]any{"apps": list[:n], "truncated": true, "next_offset": a.Offset + n}
	}
	return res, err
}

// rowID is an item's row id, as apps.Items puts it.
func rowID(item map[string]any) int64 {
	id, _ := item["id"].(int64)
	return id
}

type appRef struct {
	SpaceID int64 `json:"space_id"`
	AppID   int64 `json:"app_id"`
}

func (s *Service) showApp(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a appRef
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := s.appSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	return s.o.Apps.ShowApp(ctx, appsActor(c), a.SpaceID, a.AppID)
}

func (s *Service) listAppRows(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		appRef
		// After pages by row id, which rows are in order of: an app can hold more than one
		// reply carries, and rows added or removed between pages shift nothing.
		After int64 `json:"after"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if a.After < 0 {
		return nil, invalidf("after must be 0 or more")
	}
	if err := s.appSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	rows, err := s.o.Apps.ListRows(ctx, appsActor(c), a.SpaceID, a.AppID)
	if err != nil {
		return nil, err
	}
	from := sort.Search(len(rows), func(i int) bool { return rowID(rows[i]) > a.After })
	rows = rows[from:]
	n, res, err := fitting(len(rows), func(n int) any { return map[string]any{"rows": rows[:n]} })
	switch {
	case err != nil || n == len(rows):
	case n == 0:
		// The next row alone is over a reply: say which, and go on past it.
		res = map[string]any{"rows": rows[:0], "truncated": true, "too_large": []int64{rowID(rows[0])}, "next_after": rowID(rows[0])}
	default:
		res = map[string]any{"rows": rows[:n], "truncated": true, "next_after": rowID(rows[n-1])}
	}
	return res, err
}

func (s *Service) createApp(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		SpaceID int64 `json:"space_id"`
		apps.NewApp
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := s.appSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	return s.o.Apps.CreateApp(ctx, appsActor(c), a.SpaceID, a.NewApp)
}

func (s *Service) updateApp(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		appRef
		apps.Changes
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := s.appSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	return s.o.Apps.UpdateApp(ctx, appsActor(c), a.SpaceID, a.AppID, a.Changes)
}
