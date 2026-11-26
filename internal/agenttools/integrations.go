package agenttools

import (
	"context"
	"encoding/json"

	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/integrations"
)

// integrationView is an integration a space may use, as the agent reads it: what an app is built
// against, and nothing about the account behind it (no id, no provider, no secret, no limit, no
// webhook address, no other space).
type integrationView struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Kind        string `json:"kind"`
	// Status is "active", or "disabled" while an admin has switched the integration off.
	Status string `json:"status"`
}

// slotView is a slot the app's draft declares, with where the approval of its contract stands.
// It holds no contract, no hash, no approval or integration id.
type slotView struct {
	Slot string `json:"slot"`
	Kind string `json:"kind"`
	// Status is that of the draft's contract: none, requested, approved, rejected or revoked.
	Status string `json:"status"`
	// LiveStatus is that of the live release's contract, when the live release declares the slot.
	LiveStatus *string `json:"live_status"`
	// LiveUsable is whether the live release may use its integration now.
	LiveUsable *bool `json:"live_usable"`
}

// listIntegrations tells the agent which integrations may be used in a space, and, for one app,
// which slots its draft declares and whether an admin has approved them: enough to build against
// what exists. It reads names, kinds and statuses only, and never the account behind them.
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
