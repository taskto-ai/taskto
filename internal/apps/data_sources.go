package apps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/datasource"
)

const (
	SourcesKey            = "_sources"
	UnavailableKey        = "_unavailable"
	DataSourcesCapability = "data.sources"

	dataSourcesVersion      = 3
	maxDataSourceSlots      = 4
	maxDataSourceReads      = 16
	dataSourceContractLevel = 1
)

type DataSourceSlot struct {
	Kind string   `json:"kind"`
	Read []string `json:"read"`
}

func (s *DataSourceSlot) UnmarshalJSON(b []byte) error {
	type plain DataSourceSlot
	var p plain
	if err := strictObject(b, &p, "a data source slot"); err != nil {
		return err
	}
	*s = DataSourceSlot(p)
	return nil
}

type DataSourceContract struct {
	Version int      `json:"version"`
	Kind    string   `json:"kind"`
	Scope   string   `json:"scope"`
	Read    []string `json:"read" apitypes:"nonnull"`
}

func (c DataSourceContract) Hash() string {
	raw, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (d Definition) dataSourceContract(slot string) DataSourceContract {
	s := d.DataSources[slot]
	spec, _ := datasource.Lookup(s.Kind)
	return DataSourceContract{Version: dataSourceContractLevel, Kind: s.Kind, Scope: spec.Scope, Read: sortedCopy(s.Read)}
}

func (d Definition) checkDataSources() (bool, error) {
	if len(d.DataSources) == 0 {
		return false, nil
	}
	if len(d.DataSources) > maxDataSourceSlots {
		return true, invalidf("data_sources: at most %d", maxDataSourceSlots)
	}
	for _, name := range sortedKeys(d.DataSources) {
		slot := d.DataSources[name]
		if !namePattern.MatchString(name) {
			return true, invalidf("data_sources: %q: a name is a lowercase letter, then up to 39 lowercase letters, digits or _", name)
		}
		spec, ok := datasource.Lookup(slot.Kind)
		if !ok {
			return true, invalidf("data_sources: %q: kind must be one of %s", name, strings.Join(datasource.Kinds(), ", "))
		}
		if len(slot.Read) < 1 || len(slot.Read) > maxDataSourceReads {
			return true, invalidf("data_sources: %q: read takes 1 to %d values of %s", name, maxDataSourceReads, slot.Kind)
		}
		seen := map[string]bool{}
		for _, output := range slot.Read {
			if _, known := spec.Outputs[output]; !known {
				return true, invalidf("data_sources: %q: %q is not a value of %s (%s)", name, output, slot.Kind, strings.Join(sortedKeys(spec.Outputs), ", "))
			}
			if seen[output] {
				return true, invalidf("data_sources: %q: %q is read twice", name, output)
			}
			seen[output] = true
		}
	}
	return true, nil
}

func (s *Service) viewDefinition(ctx context.Context, sp access.Space, actor Actor, appID int64, which string) (App, Definition, access.AppAccess, error) {
	if err := may(sp, access.Read); err != nil {
		return App{}, Definition{}, access.AppAccess{}, err
	}
	acc, err := s.appAccess(ctx, actor, sp.ID, appID)
	if err != nil {
		return App{}, Definition{}, acc, err
	}
	if err := need(acc, access.CapView); err != nil {
		return App{}, Definition{}, acc, err
	}
	a, err := launchable(ctx, s.db, sp.ID, appID)
	if err != nil {
		return App{}, Definition{}, acc, err
	}
	def, err := screenOf(ctx, s.db, a, acc, which)
	return a, def, acc, err
}

func (s *Service) sourceValues(ctx context.Context, sp access.Space, actor Actor, appID int64, def Definition) map[string]any {
	if len(def.DataSources) == 0 {
		return nil
	}
	approvals, err := dataSourceApprovals(ctx, s.db, appID)
	out := map[string]any{}
	for _, name := range sortedKeys(def.DataSources) {
		slot, contract := def.DataSources[name], def.dataSourceContract(name)
		out[name] = map[string]any{UnavailableKey: true}
		if err != nil || s.platform.Sources == nil {
			continue
		}
		if r, ok := findDataSourceApproval(approvals, name, contract.Hash()); !ok || r.status != ApprovalApproved {
			continue
		}
		values, readErr := s.platform.Sources.Read(ctx, datasource.Viewer{UserID: actor.UserID, WorkspaceID: sp.WorkspaceID}, datasource.Kind(slot.Kind), slot.Read)
		if readErr != nil {
			continue
		}
		out[name] = values
	}
	return out
}

func withSources(msgs []Message, values map[string]any) []Message {
	if values == nil {
		return msgs
	}
	for _, m := range msgs {
		upd, ok := m["updateDataModel"].(map[string]any)
		if !ok || upd["path"] != "/" {
			continue
		}
		if value, ok := upd["value"].(map[string]any); ok {
			value[SourcesKey] = values
		}
	}
	return msgs
}

func (s *Service) sourceMessages(ctx context.Context, sp access.Space, actor Actor, appID int64, which string) ([]Message, error) {
	a, def, _, err := s.viewDefinition(ctx, sp, actor, appID, which)
	if err != nil {
		return nil, err
	}
	values := s.sourceValues(ctx, sp, actor, appID, def)
	if values == nil {
		values = map[string]any{}
	}
	return []Message{dataMessage(SurfaceID(a.ID), "/"+SourcesKey, values)}, nil
}

func dataSourceUnapproved(slot string) error {
	return apierr.New(http.StatusConflict, apierr.CodeAppDataSourceUnapproved,
		"the data source "+slot+" has no approval for this contract: ask an admin to approve it before publishing",
		apierr.Params{"slot": slot})
}
