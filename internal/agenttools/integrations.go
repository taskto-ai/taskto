package agenttools

import (
	"context"
	"encoding/json"

	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/integrations"
)

type integrationView struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
}

type slotView struct {
	Slot       string  `json:"slot"`
	Kind       string  `json:"kind"`
	Status     string  `json:"status"`
	LiveStatus *string `json:"live_status"`
	LiveUsable *bool   `json:"live_usable"`
}

func (s *Service) listIntegrations(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		SpaceID int64 `json:"space_id"`
		AppID   int64 `json:"app_id"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	sp, err := s.space(ctx, c, a.SpaceID)
	if err != nil {
		return nil, err
	}
	out := struct {
		Integrations []integrationView `json:"integrations"`
		Slots        []slotView        `json:"slots"`
	}{Integrations: []integrationView{}, Slots: []slotView{}}
	if s.o.Integrations != nil {
		list, err := s.o.Integrations.ForSpace(ctx, sp.WorkspaceID, sp.ID)
		if err != nil {
			return nil, err
		}
		out.Integrations = make([]integrationView, 0, len(list))
		for _, i := range list {
			out.Integrations = append(out.Integrations, integrationOf(i))
		}
	}
	if a.AppID != 0 {
		st, err := s.o.Apps.AppIntegrations(ctx, appsActor(c), sp.ID, a.AppID)
		if err != nil {
			return nil, err
		}
		out.Slots = make([]slotView, 0, len(st.Slots))
		for _, slot := range st.Slots {
			out.Slots = append(out.Slots, slotOf(slot))
		}
	}
	return out, nil
}

func integrationOf(i integrations.SpaceIntegration) integrationView {
	return integrationView{Name: i.Name, DisplayName: i.DisplayName, Kind: i.Kind, Status: i.Status}
}

func slotOf(s apps.SlotStatus) slotView {
	v := slotView{Slot: s.Slot, Kind: s.Kind, Status: s.Status}
	if s.Live != nil {
		v.LiveStatus, v.LiveUsable = &s.Live.Status, &s.Live.Usable
	}
	return v
}
