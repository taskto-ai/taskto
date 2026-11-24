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
	// The size measured is that of the reply sent, the keys that say it is cut short included.
	_, res, err := fitting(len(list), func(n int) any {
		if n < len(list) {
			return map[string]any{"apps": list[:n], "truncated": true, "next_offset": a.Offset + n}
		}
		return map[string]any{"apps": list[:n]}
	})
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
	// The size measured is that of the reply sent, the keys that say it is cut short included.
	_, res, err := fitting(len(rows), func(n int) any {
		switch {
		case n == len(rows):
			return map[string]any{"rows": rows[:n]}
		case n == 0:
			// The next row alone is over a reply: say which, and go on past it.
			return map[string]any{"rows": rows[:0], "truncated": true, "too_large": []int64{rowID(rows[0])}, "next_after": rowID(rows[0])}
		default:
			return map[string]any{"rows": rows[:n], "truncated": true, "next_after": rowID(rows[n-1])}
		}
	})
	return res, err
}

// queryAppRows is list_app_rows with filters, search, an order and totals (apps.Query): a page
// of the rows the user may read, and the totals of all that match.
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
	// A page that doesn't fit one reply is asked again with fewer rows, so the cursor it ends with
	// is that of the last row sent, in the same keyset the pages are in.
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

// listAppActions is what the app's live release offers to run on its records, and what the
// user's roles let them do.
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

// runAppAction runs one of the app's actions that change a record, as the call's user, through
// the code behind the web app's action route: their roles, the collection's rules, the locks of
// captured values, the record's history and the action's effects all apply.
func (s *Service) runAppAction(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		appRef
		Action           string         `json:"action"`
		ID               *int64         `json:"id"`
		Context          map[string]any `json:"context"`
		ExpectedRevision *int64         `json:"expected_revision"`
		Operation        string         `json:"operation"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if a.Action == "" {
		return nil, invalidf("action is the name of one of the app's actions, from list_app_actions")
	}
	if err := s.appSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	fields := map[string]any{}
	for k, v := range a.Context {
		fields[k] = v
	}
	if a.ID != nil {
		if _, has := fields["id"]; has {
			return nil, invalidf("give the row's id as id, not in context")
		}
		if *a.ID < 1 {
			return nil, invalidf("id is a row's id, from 1")
		}
		fields["id"] = float64(*a.ID)
	}
	return s.o.Apps.RunAction(ctx, appsActor(c), a.SpaceID, a.AppID, apps.ActionRun{
		Action: a.Action, Context: fields, ExpectedRevision: a.ExpectedRevision, Operation: a.Operation,
	})
}
