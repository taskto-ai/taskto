package agenttools

import (
	"context"
	"encoding/json"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apps"
)

// The tools for templates, copies and diagnosis of apps. Installing and duplicating make a
// new app whose DRAFT is a copy of a definition, as the API's routes do: nothing is published,
// no row, release, role assignment or secret is copied. The read tools show what the user's own
// roles in the app let them see; none of them writes, and there is no tool to set roles.

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
		Name string `json:"name"`
		// DestinationSpaceID is the space of the copy: the source's if left out.
		DestinationSpaceID int64  `json:"destination_space_id"`
		Source             string `json:"source"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := s.appSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	// The destination is checked, in the call's workspace, as the source is: a space of
	// another workspace is as unseen as one that doesn't exist.
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
	// The newest that fit; More says there are older ones, and the last ID shown is the
	// next call's before.
	_, res, err := fitting(len(list.Effects), func(n int) any {
		shown := list
		shown.Effects = list.Effects[:n]
		shown.More = list.More || n < len(list.Effects)
		return shown
	})
	return res, err
}

func (s *Service) listAppReleases(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a appRef
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := s.appSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	list, err := s.o.Apps.ListReleases(ctx, appsActor(c), a.SpaceID, a.AppID)
	if err != nil {
		return nil, err
	}
	n, res, err := fitting(len(list), func(n int) any { return map[string]any{"releases": list[:n]} })
	if err == nil && n < len(list) {
		res = map[string]any{"releases": list[:n], "truncated": true}
	}
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
