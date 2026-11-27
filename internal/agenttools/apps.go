package agenttools

import (
	"context"
	"encoding/json"
	"sort"

	"taskto.ai/taskto/internal/apps"
)

func appsActor(c *call) apps.Actor { return apps.Actor{UserID: c.User, Agent: true, Via: c.Token} }

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
		Offset   int   `json:"offset"`
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
	_, res, err := fitting(len(list), func(n int) any {
		if n < len(list) {
			return map[string]any{"apps": list[:n], "truncated": true, "next_offset": a.Offset + n}
		}
		return map[string]any{"apps": list[:n]}
	})
	return res, err
}

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
	_, res, err := fitting(len(rows), func(n int) any {
		switch {
		case n == len(rows):
			return map[string]any{"rows": rows[:n]}
		case n == 0:
			return map[string]any{"rows": rows[:0], "truncated": true, "too_large": []int64{rowID(rows[0])}, "next_after": rowID(rows[0])}
		default:
			return map[string]any{"rows": rows[:n], "truncated": true, "next_after": rowID(rows[n-1])}
		}
	})
	return res, err
}

func (s *Service) queryAppRows(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		appRef
		apps.Query
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := s.appSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	q := a.Query
	if q.Limit == 0 {
		q.Limit = apps.DefaultPageSize
	}
	for {
		page, err := s.o.Apps.QueryRows(ctx, appsActor(c), a.SpaceID, a.AppID, q)
		if err != nil {
			return nil, err
		}
		body, err := json.Marshal(page)
		if err != nil {
			return nil, err
		}
		if len(body) <= MaxResult {
			return page, nil
		}
		if q.Limit == 1 {
			return nil, invalidf("the next row alone is over a reply: read it with list_app_rows, or ask for fewer aggregates")
		}
		q.Limit /= 2
	}
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

func (s *Service) listAppActions(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a appRef
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := s.appSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	return s.o.Apps.Actions(ctx, appsActor(c), a.SpaceID, a.AppID)
}

func (s *Service) runAppAction(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		appRef
		Action           string         `json:"action"`
		ID               *int64         `json:"id"`
		Context          map[string]any `json:"context"`
		ExpectedRevision *int64         `json:"expected_revision"`
		ExpectedRelease  *int64         `json:"expected_release"`
		Operation        *string        `json:"operation"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if a.Action == "" {
		return nil, invalidf("action is the name of one of the app's actions, from list_app_actions")
	}
	if _, has := a.Context["id"]; has {
		return nil, invalidf("give the row's id as id, not in context")
	}
	if a.Operation != nil && *a.Operation == "" {
		return nil, invalidf("operation is 8 to 64 letters, digits, - or _: leave it out to make no key")
	}
	if err := s.appSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	if a.ExpectedRelease == nil || *a.ExpectedRelease < 1 {
		return nil, invalidf("expected_release is the release number list_app_actions gave: the action runs only if it is still the live one")
	}
	fields := map[string]any{}
	for k, v := range a.Context {
		fields[k] = v
	}
	if a.ID != nil {
		if *a.ID < 1 {
			return nil, invalidf("id is a row's id, from 1")
		}
		fields["id"] = float64(*a.ID)
	}
	operation := ""
	if a.Operation != nil {
		operation = *a.Operation
	}
	return s.o.Apps.RunAction(ctx, appsActor(c), a.SpaceID, a.AppID, apps.ActionRun{
		Action: a.Action, Context: fields, ExpectedRevision: a.ExpectedRevision, ExpectedRelease: *a.ExpectedRelease, Operation: operation,
	})
}
