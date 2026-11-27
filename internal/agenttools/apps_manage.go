package agenttools

import (
	"context"
	"encoding/json"
	"sort"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apps"
)

func (s *Service) listAppTemplates(_ context.Context, _ *call, args json.RawMessage) (any, error) {
	if err := decode(args, &struct{}{}); err != nil {
		return nil, err
	}
	list, err := apps.TemplateInfos()
	if err != nil {
		return nil, err
	}
	return map[string]any{"templates": list}, nil
}

func (s *Service) installAppTemplate(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		SpaceID    int64          `json:"space_id"`
		TemplateID string         `json:"template_id"`
		Name       string         `json:"name"`
		Board      *apps.BoardRef `json:"board"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := s.appSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	return s.o.Apps.InstallTemplate(ctx, appsActor(c), a.SpaceID, a.TemplateID, apps.Install{Name: a.Name, Board: a.Board})
}

func (s *Service) duplicateApp(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		appRef
		Name               string `json:"name"`
		DestinationSpaceID int64  `json:"destination_space_id"`
		Source             string `json:"source"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := s.appSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	if a.DestinationSpaceID != 0 {
		if err := s.appSpace(ctx, c, a.DestinationSpaceID); err != nil {
			return nil, err
		}
	}
	return s.o.Apps.DuplicateApp(ctx, appsActor(c), a.SpaceID, a.AppID, apps.DuplicateRequest{
		Name: a.Name, SpaceID: a.DestinationSpaceID, Source: a.Source,
	})
}

func (s *Service) listAppEffects(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		appRef
		Status string `json:"status"`
		Record int64  `json:"record"`
		Before int64  `json:"before"`
		Limit  int    `json:"limit"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := s.appSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	list, err := s.o.Apps.ListEffects(ctx, appsActor(c), a.SpaceID, a.AppID,
		apps.EffectFilter{Status: a.Status, Record: a.Record, Before: a.Before, Limit: a.Limit})
	if err != nil {
		return nil, err
	}
	_, res, err := fitting(len(list.Effects), func(n int) any {
		shown := list
		shown.Effects = list.Effects[:n]
		shown.More = list.More || n < len(list.Effects)
		return shown
	})
	return res, err
}

func (s *Service) listAppReleases(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		appRef
		Before int64 `json:"before"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if a.Before < 0 {
		return nil, invalidf("before must be 0 or more")
	}
	if err := s.appSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	list, err := s.o.Apps.ListReleases(ctx, appsActor(c), a.SpaceID, a.AppID)
	if err != nil {
		return nil, err
	}
	if a.Before > 0 {
		list = list[sort.Search(len(list), func(i int) bool { return list[i].Number < a.Before }):]
	}
	_, res, err := fitting(len(list), func(n int) any {
		if n < len(list) {
			return map[string]any{"releases": list[:n], "truncated": true, "next": list[max(n-1, 0)].Number}
		}
		return map[string]any{"releases": list[:n]}
	})
	return res, err
}

func (s *Service) showAppRoles(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a appRef
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := s.appSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	grants, err := s.o.Apps.AppRoles(ctx, appsActor(c), a.SpaceID, a.AppID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"grants": grants, "roles": access.AppRoles}, nil
}
