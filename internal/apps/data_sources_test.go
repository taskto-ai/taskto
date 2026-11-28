package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/datasource"
	"taskto.ai/taskto/internal/quota"
)

const megabyte = 1 << 20

type sourceWorld struct {
	*world
	app    App
	failed bool
}

func newSourceWorld(t *testing.T) *sourceWorld {
	t.Helper()
	x := &sourceWorld{world: newWorld(t)}
	x.e.s.WithPlatform(Platform{Sources: datasource.Sources{Storage: func(_ context.Context, _, userID int64) (quota.SpaceUsage, bool, error) {
		if x.failed {
			return quota.SpaceUsage{}, false, errors.New("the disk is gone")
		}
		return quota.SpaceUsage{Total: userID * megabyte, Limit: 100 * megabyte, KnowledgeVersioned: userID * megabyte}, true, nil
	}}})
	x.app = x.create(x.alice, x.shared, []string{"used_bytes", "quota_bytes"})
	return x
}

func sourceDefinition(t *testing.T, read ...string) Definition {
	t.Helper()
	d := example(t)
	d.DataSources = map[string]DataSourceSlot{"usage": {Kind: "storage_usage", Read: read}}
	return d
}

func (x *sourceWorld) create(u *user, s access.Space, read []string) App {
	x.e.t.Helper()
	rec := u.do(http.MethodPost, appsPath(s, ""), map[string]any{"name": "Usage", "definition": sourceDefinition(x.e.t, read...)})
	expect(x.e.t, rec, http.StatusCreated)
	return decode[App](x.e.t, rec)
}

func (x *sourceWorld) slot(u *user, s access.Space, a App) DataSourceSlotStatus {
	x.e.t.Helper()
	rec := u.do(http.MethodGet, appPath(s, a.ID, "/data-sources"), nil)
	expect(x.e.t, rec, http.StatusOK)
	st := decode[DataSourceStatus](x.e.t, rec)
	if len(st.Slots) != 1 {
		x.e.t.Fatalf("slots = %+v", st.Slots)
	}
	return st.Slots[0]
}

func (x *sourceWorld) request(u *user, s access.Space, a App, hash string) *httptest.ResponseRecorder {
	x.e.t.Helper()
	return u.do(http.MethodPost, appPath(s, a.ID, "/data-sources/usage/request"), map[string]any{"contract_hash": hash})
}

func (x *sourceWorld) mustRequest(u *user, s access.Space, a App) DataSourceApproval {
	x.e.t.Helper()
	rec := x.request(u, s, a, x.slot(u, s, a).ContractHash)
	expect(x.e.t, rec, http.StatusOK)
	return decode[DataSourceApproval](x.e.t, rec)
}

func (x *sourceWorld) decide(u *user, workspace, approval int64, verb string) *httptest.ResponseRecorder {
	x.e.t.Helper()
	return u.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/data-source-approvals/%d/%s", workspace, approval, verb), map[string]any{})
}

func (x *sourceWorld) approved(a App) DataSourceApproval {
	x.e.t.Helper()
	req := x.mustRequest(x.alice, x.shared, a)
	expect(x.e.t, x.decide(x.owner, x.ws, req.ID, "approve"), http.StatusOK)
	return req
}

func (x *sourceWorld) publishTry(u *user, s access.Space, a App) *httptest.ResponseRecorder {
	x.e.t.Helper()
	cur := decode[App](x.e.t, u.do(http.MethodGet, appPath(s, a.ID, ""), nil))
	return u.do(http.MethodPost, appPath(s, a.ID, "/publish"), map[string]any{"version": cur.Version, "acknowledge": []SchemaChange{}})
}

func (x *sourceWorld) redraft(u *user, s access.Space, a App, read ...string) {
	x.e.t.Helper()
	cur := decode[App](x.e.t, u.do(http.MethodGet, appPath(s, a.ID, ""), nil))
	expect(x.e.t, u.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": cur.Version, "definition": sourceDefinition(x.e.t, read...)}), http.StatusOK)
}

func (x *sourceWorld) values(u *user, s access.Space, a App) map[string]any {
	x.e.t.Helper()
	rec := u.do(http.MethodGet, appPath(s, a.ID, "/data-sources/values"), nil)
	expect(x.e.t, rec, http.StatusOK)
	return sourcesOf(x.e.t, decode[[]map[string]any](x.e.t, rec))
}

func (x *sourceWorld) surfaceValues(u *user, s access.Space, a App) map[string]any {
	x.e.t.Helper()
	rec := u.do(http.MethodGet, appPath(s, a.ID, "/surface"), nil)
	expect(x.e.t, rec, http.StatusOK)
	return sourcesOf(x.e.t, decode[[]map[string]any](x.e.t, rec))
}

func sourcesOf(t *testing.T, msgs []map[string]any) map[string]any {
	t.Helper()
	for _, m := range msgs {
		upd, ok := m["updateDataModel"].(map[string]any)
		if !ok {
			continue
		}
		switch upd["path"] {
		case "/":
			if value, ok := upd["value"].(map[string]any); ok {
				if sources, ok := value[SourcesKey].(map[string]any); ok {
					return sources
				}
			}
		case "/" + SourcesKey:
			return upd["value"].(map[string]any)
		}
	}
	return nil
}

func usage(t *testing.T, values map[string]any) map[string]any {
	t.Helper()
	slot, ok := values["usage"].(map[string]any)
	if !ok {
		t.Fatalf("no usage slot in %v", values)
	}
	return slot
}

func (x *sourceWorld) count(query string, args ...any) (n int) {
	x.e.t.Helper()
	if err := x.e.db.QueryRow(query, args...).Scan(&n); err != nil {
		x.e.t.Fatal(err)
	}
	return n
}

func TestADataSourceDeclarationIsStrict(t *testing.T) {
	for name, mutate := range map[string]func(*Definition){
		"an unknown kind": func(d *Definition) {
			d.DataSources["usage"] = DataSourceSlot{Kind: "weather", Read: []string{"used_bytes"}}
		},
		"an unknown output": func(d *Definition) {
			d.DataSources["usage"] = DataSourceSlot{Kind: "storage_usage", Read: []string{"used_cats"}}
		},
		"a value read twice": func(d *Definition) {
			d.DataSources["usage"] = DataSourceSlot{Kind: "storage_usage", Read: []string{"used_bytes", "used_bytes"}}
		},
		"nothing read": func(d *Definition) { d.DataSources["usage"] = DataSourceSlot{Kind: "storage_usage"} },
		"a bad slot name": func(d *Definition) {
			d.DataSources["Us age"] = DataSourceSlot{Kind: "storage_usage", Read: []string{"used_bytes"}}
		},
		"too many slots":         func(d *Definition) { fiveSlots(d) },
		"definition version two": func(d *Definition) { d.DefinitionVersion = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			d := sourceDefinition(t, "used_bytes")
			mutate(&d)
			if _, err := d.Check(); err == nil {
				t.Fatal("the definition was accepted")
			}
		})
	}
	var slot DataSourceSlot
	if err := slot.UnmarshalJSON([]byte(`{"kind": "storage_usage", "read": ["used_bytes"], "user": 7}`)); err == nil {
		t.Fatal("a slot naming a user was accepted")
	}
	field := example(t)
	field.Collection.Fields[0].Name = SourcesKey
	if _, err := field.Check(); err == nil {
		t.Fatal("a field called _sources was accepted")
	}
	ok, err := sourceDefinition(t, "used_bytes").Check()
	if err != nil || !slices.Contains(ok.Requires, DataSourcesCapability) {
		t.Fatalf("a valid declaration: requires %v, error %v", ok.Requires, err)
	}
}

func fiveSlots(d *Definition) {
	for i := range 5 {
		d.DataSources[fmt.Sprintf("slot%d", i)] = DataSourceSlot{Kind: "storage_usage", Read: []string{"used_bytes"}}
	}
}

func TestTheContractHashFollowsWhatTheAppReads(t *testing.T) {
	hash := func(read ...string) string {
		d := sourceDefinition(t, read...)
		return d.dataSourceContract("usage").Hash()
	}
	if hash("used_bytes", "quota_bytes") != hash("quota_bytes", "used_bytes") {
		t.Fatal("the order of the reads changed the hash")
	}
	if hash("used_bytes", "quota_bytes") == hash("used_bytes") {
		t.Fatal("reading fewer values kept the hash")
	}
	if hash("used_bytes") == hash("used_mb") {
		t.Fatal("reading another value kept the hash")
	}
}

func TestAnAppCannotBePublishedBeforItsDataSourceIsApproved(t *testing.T) {
	x := newSourceWorld(t)
	expectCode(t, x.publishTry(x.alice, x.shared, x.app), http.StatusConflict, "app_data_source_unapproved")
	req := x.mustRequest(x.alice, x.shared, x.app)
	expectCode(t, x.publishTry(x.alice, x.shared, x.app), http.StatusConflict, "app_data_source_unapproved")
	expect(t, x.decide(x.owner, x.ws, req.ID, "approve"), http.StatusOK)
	expect(t, x.publishTry(x.alice, x.shared, x.app), http.StatusOK)

	x.redraft(x.alice, x.shared, x.app, "used_bytes", "quota_bytes", "used_percent")
	expectCode(t, x.publishTry(x.alice, x.shared, x.app), http.StatusConflict, "app_data_source_unapproved")
	other := x.approved(x.app)
	if other.ID == req.ID || other.ContractHash == req.ContractHash {
		t.Fatalf("a changed read list reused the earlier approval: %+v", other)
	}
	expect(t, x.publishTry(x.alice, x.shared, x.app), http.StatusOK)
}

func TestARejectedOrRevokedContractCannotBePublished(t *testing.T) {
	x := newSourceWorld(t)
	req := x.mustRequest(x.alice, x.shared, x.app)
	expect(t, x.decide(x.owner, x.ws, req.ID, "reject"), http.StatusOK)
	expectCode(t, x.publishTry(x.alice, x.shared, x.app), http.StatusConflict, "app_data_source_unapproved")
	expectCode(t, x.decide(x.owner, x.ws, req.ID, "approve"), http.StatusConflict, "app_integration_not_pending")
	expectCode(t, x.decide(x.owner, x.ws, req.ID, "revoke"), http.StatusConflict, "app_integration_not_approved")
	renewed := x.approved(x.app)
	expect(t, x.decide(x.owner, x.ws, renewed.ID, "revoke"), http.StatusOK)
	expectCode(t, x.publishTry(x.alice, x.shared, x.app), http.StatusConflict, "app_data_source_unapproved")
}

func TestEachViewerSeesTheirOwnValues(t *testing.T) {
	x := newSourceWorld(t)
	x.approved(x.app)
	expect(t, x.publishTry(x.alice, x.shared, x.app), http.StatusOK)
	want := func(u *user) float64 { return float64(u.id * megabyte) }
	for _, u := range []*user{x.alice, x.bob} {
		for name, got := range map[string]map[string]any{"surface": x.surfaceValues(u, x.shared, x.app), "values": x.values(u, x.shared, x.app)} {
			if used := usage(t, got)["used_bytes"]; used != want(u) {
				t.Errorf("%s: user %d sees used_bytes %v, want %v", name, u.id, used, want(u))
			}
			if _, leaked := usage(t, got)["used_mb"]; leaked {
				t.Errorf("%s: a value that wasn't declared was sent", name)
			}
		}
	}
	msgs, err := x.e.s.sourceMessages(context.Background(), x.shared, Actor{UserID: x.bob.id, Agent: true, Via: "laptop"}, x.app.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	var round []map[string]any
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatal(err)
	}
	if used := usage(t, sourcesOf(t, round))["used_bytes"]; used != want(x.bob) {
		t.Errorf("a token of user %d sees used_bytes %v, want %v", x.bob.id, used, want(x.bob))
	}
}

func TestOnlyThoseWhoMayOpenTheAppReadItsValues(t *testing.T) {
	x := newSourceWorld(t)
	x.approved(x.app)
	expect(t, x.publishTry(x.alice, x.shared, x.app), http.StatusOK)
	path := appPath(x.shared, x.app.ID, "/data-sources/values")
	expect(t, x.mallory.do(http.MethodGet, path, nil), http.StatusNotFound)
	if _, err := x.e.s.sourceMessages(context.Background(), access.Space{ID: x.shared.ID, WorkspaceID: x.ws}, Actor{UserID: x.mallory.id}, x.app.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("a caller with no access to the space: %v", err)
	}
	grants := []access.AppGrant{{UserID: &x.alice.id, Roles: []access.AppRole{access.AppBuilder, access.AppPublisher}}, {UserID: &x.owner.id, Roles: []access.AppRole{access.AppUser}}}
	expect(t, x.alice.do(http.MethodPut, appPath(x.shared, x.app.ID, "/roles"), map[string]any{"grants": grants}), http.StatusOK)
	if rec := x.bob.do(http.MethodGet, path, nil); rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Errorf("a person the app's roles don't let view it: status %d: %s", rec.Code, rec.Body)
	}
	if x.values(x.owner, x.shared, x.app) == nil {
		t.Error("the app's user lost the values")
	}
	if rec := x.stranger.do(http.MethodGet, path, nil); rec.Code != http.StatusNotFound && rec.Code != http.StatusForbidden {
		t.Errorf("a person of another workspace: status %d: %s", rec.Code, rec.Body)
	}
	if x.surfaceValues(x.alice, x.shared, x.app) == nil {
		t.Fatal("the member lost the values")
	}
}

func TestRevokingTheApprovalStopsTheValuesAtOnce(t *testing.T) {
	x := newSourceWorld(t)
	req := x.approved(x.app)
	expect(t, x.publishTry(x.alice, x.shared, x.app), http.StatusOK)
	if _, ok := usage(t, x.surfaceValues(x.bob, x.shared, x.app))["used_bytes"]; !ok {
		t.Fatal("an approved slot gave no values")
	}
	expect(t, x.decide(x.owner, x.ws, req.ID, "revoke"), http.StatusOK)
	for name, got := range map[string]map[string]any{"surface": x.surfaceValues(x.bob, x.shared, x.app), "values": x.values(x.bob, x.shared, x.app)} {
		slot := usage(t, got)
		if slot[UnavailableKey] != true || slot["used_bytes"] != nil {
			t.Errorf("%s after the revocation: %v", name, slot)
		}
	}
}

func TestAnUnapprovedDraftShowsNoValues(t *testing.T) {
	x := newSourceWorld(t)
	rec := x.alice.do(http.MethodGet, appPath(x.shared, x.app.ID, "/data-sources/values?release=draft"), nil)
	expect(t, rec, http.StatusOK)
	slot := usage(t, sourcesOf(t, decode[[]map[string]any](t, rec)))
	if slot[UnavailableKey] != true || slot["used_bytes"] != nil {
		t.Fatalf("an unapproved contract gave %v", slot)
	}
}

func TestADraftWhoseContractChangedShowsNoValues(t *testing.T) {
	x := newSourceWorld(t)
	x.approved(x.app)
	expect(t, x.publishTry(x.alice, x.shared, x.app), http.StatusOK)
	x.redraft(x.alice, x.shared, x.app, "used_bytes", "used_percent")
	rec := x.alice.do(http.MethodGet, appPath(x.shared, x.app.ID, "/data-sources/values?release=draft"), nil)
	expect(t, rec, http.StatusOK)
	if slot := usage(t, sourcesOf(t, decode[[]map[string]any](t, rec))); slot[UnavailableKey] != true || slot["used_bytes"] != nil {
		t.Fatalf("a contract nobody approved gave %v", slot)
	}
	if slot := usage(t, x.values(x.bob, x.shared, x.app)); slot["used_bytes"] == nil {
		t.Fatalf("the live release lost its values: %v", slot)
	}
}

func TestAFailingSourceMakesOnlyItsSlotUnavailable(t *testing.T) {
	x := newSourceWorld(t)
	x.approved(x.app)
	expect(t, x.publishTry(x.alice, x.shared, x.app), http.StatusOK)
	x.failed = true
	if slot := usage(t, x.surfaceValues(x.alice, x.shared, x.app)); slot[UnavailableKey] != true {
		t.Fatalf("a failing read gave %v", slot)
	}
}

func TestARollbackToAnUnapprovedContractIsRefused(t *testing.T) {
	x := newSourceWorld(t)
	first := x.approved(x.app)
	expect(t, x.publishTry(x.alice, x.shared, x.app), http.StatusOK)
	x.redraft(x.alice, x.shared, x.app, "used_bytes", "used_percent")
	x.approved(x.app)
	expect(t, x.publishTry(x.alice, x.shared, x.app), http.StatusOK)
	expect(t, x.decide(x.owner, x.ws, first.ID, "revoke"), http.StatusOK)
	cur := decode[App](t, x.alice.do(http.MethodGet, appPath(x.shared, x.app.ID, ""), nil))
	rec := x.alice.do(http.MethodPost, appPath(x.shared, x.app.ID, "/rollback"), map[string]any{"version": cur.Version, "release": 1})
	expectCode(t, rec, http.StatusConflict, "app_data_source_unapproved")
}

func TestOnlyAWorkspaceAdminDecides(t *testing.T) {
	x := newSourceWorld(t)
	req := x.mustRequest(x.alice, x.shared, x.app)
	for _, verb := range []string{"approve", "reject", "revoke"} {
		expect(t, x.decide(x.bob, x.ws, req.ID, verb), http.StatusForbidden)
		expect(t, x.decide(x.alice, x.ws, req.ID, verb), http.StatusForbidden)
	}
	expect(t, x.bob.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/data-source-approvals", x.ws), nil), http.StatusForbidden)
	for _, verb := range []string{"approve", "reject", "revoke"} {
		rec := x.decide(x.stranger, x.other, req.ID, verb)
		expect(t, rec, http.StatusNotFound)
		expect(t, x.decide(x.stranger, x.ws, req.ID, verb), http.StatusNotFound)
	}
	if got := x.count(`SELECT count(*) FROM app_data_source_approvals WHERE status = 'requested'`); got != 1 {
		t.Fatalf("%d requests still wait, want 1", got)
	}
	list := x.owner.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/data-source-approvals", x.ws), nil)
	expect(t, list, http.StatusOK)
	if got := decode[DataSourceApprovalList](t, list); len(got.Approvals) != 1 || got.Approvals[0].ID != req.ID || got.Approvals[0].Contract.Kind != "storage_usage" {
		t.Fatalf("the queue = %+v", got)
	}
	foreign := x.stranger.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/data-source-approvals", x.other), nil)
	expect(t, foreign, http.StatusOK)
	if got := decode[DataSourceApprovalList](t, foreign); len(got.Approvals) != 0 {
		t.Fatalf("another workspace's admin reads %+v", got)
	}
}

func TestAskingNeedsTheRightToPublishAndTheCurrentContract(t *testing.T) {
	x := newSourceWorld(t)
	hash := x.slot(x.alice, x.shared, x.app).ContractHash
	expect(t, x.request(x.bob, x.shared, x.app, hash), http.StatusForbidden)
	expect(t, x.request(x.mallory, x.shared, x.app, hash), http.StatusNotFound)
	builder := []access.AppGrant{{UserID: &x.alice.id, Roles: []access.AppRole{access.AppBuilder}}}
	expect(t, x.alice.do(http.MethodPut, appPath(x.shared, x.app.ID, "/roles"), map[string]any{"grants": builder}), http.StatusOK)
	expect(t, x.request(x.alice, x.shared, x.app, hash), http.StatusForbidden)
	publisher := []access.AppGrant{{UserID: &x.alice.id, Roles: []access.AppRole{access.AppBuilder, access.AppPublisher}}}
	expect(t, x.alice.do(http.MethodPut, appPath(x.shared, x.app.ID, "/roles"), map[string]any{"grants": publisher}), http.StatusOK)
	expectCode(t, x.request(x.alice, x.shared, x.app, strings.Repeat("0", 64)), http.StatusConflict, "app_data_source_contract_changed")
	expectCode(t, x.alice.do(http.MethodPost, appPath(x.shared, x.app.ID, "/data-sources/nothing/request"), map[string]any{"contract_hash": hash}),
		http.StatusNotFound, "app_data_source_slot_unknown")
	if got := x.count(`SELECT count(*) FROM app_data_source_approvals`); got != 0 {
		t.Fatalf("refused requests left %d rows", got)
	}
	first := x.mustRequest(x.alice, x.shared, x.app)
	if again := x.mustRequest(x.alice, x.shared, x.app); again.ID != first.ID {
		t.Fatalf("asking again made request %d, want %d", again.ID, first.ID)
	}
	expect(t, x.bob.do(http.MethodGet, appPath(x.shared, x.app.ID, "/data-sources"), nil), http.StatusNotFound)
}

func TestEveryDataSourceDecisionIsAudited(t *testing.T) {
	x := newSourceWorld(t)
	audited := func() int {
		return x.count(`SELECT count(*) FROM audit_events WHERE action LIKE 'app_data_source_%'`)
	}
	hash := x.slot(x.alice, x.shared, x.app).ContractHash
	expect(t, x.request(x.bob, x.shared, x.app, hash), http.StatusForbidden)
	expect(t, x.request(x.alice, x.shared, x.app, strings.Repeat("0", 64)), http.StatusConflict)
	req := x.mustRequest(x.alice, x.shared, x.app)
	x.mustRequest(x.alice, x.shared, x.app)
	before := audited()
	for _, verb := range []string{"approve", "reject", "revoke"} {
		expect(t, x.decide(x.bob, x.ws, req.ID, verb), http.StatusForbidden)
		expect(t, x.decide(x.stranger, x.other, req.ID, verb), http.StatusNotFound)
	}
	expectCode(t, x.decide(x.owner, x.ws, req.ID, "revoke"), http.StatusConflict, "app_integration_not_approved")
	if got := audited(); got != before {
		t.Fatalf("refused calls wrote %d audit events", got-before)
	}
	expect(t, x.decide(x.owner, x.ws, req.ID, "reject"), http.StatusOK)
	expectCode(t, x.decide(x.owner, x.ws, req.ID, "reject"), http.StatusConflict, "app_integration_not_pending")
	renewed := x.mustRequest(x.alice, x.shared, x.app)
	expect(t, x.decide(x.owner, x.ws, renewed.ID, "approve"), http.StatusOK)
	expect(t, x.decide(x.owner, x.ws, renewed.ID, "revoke"), http.StatusOK)

	rows, err := x.e.db.Query(`SELECT action, actor_id, target_type, target_id, details FROM audit_events WHERE action LIKE 'app_data_source_%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var actions []string
	targets := []int64{req.ID, req.ID, renewed.ID, renewed.ID, renewed.ID}
	for rows.Next() {
		var action, targetType, details string
		var actor, target int64
		if err := rows.Scan(&action, &actor, &targetType, &target, &details); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, action)
		if targetType != "app_data_source_approval" || len(actions) > len(targets) || target != targets[len(actions)-1] {
			t.Errorf("%s targets %s %d", action, targetType, target)
		}
		want := x.owner.id
		if strings.HasSuffix(action, "requested") {
			want = x.alice.id
		}
		if actor != want {
			t.Errorf("%s by %d, want %d", action, actor, want)
		}
		if strings.Contains(details, `"usage"`) || strings.Contains(details, "used_bytes") {
			t.Errorf("%s details name the slot or what it reads: %s", action, details)
		}
	}
	want := []string{"app_data_source_requested", "app_data_source_rejected", "app_data_source_requested", "app_data_source_approved", "app_data_source_revoked"}
	if !slices.Equal(actions, want) {
		t.Fatalf("audited %v, want %v", actions, want)
	}
}

func TestAMovedCopiedOrDeletedAppCarriesNoApproval(t *testing.T) {
	x := newSourceWorld(t)
	x.approved(x.app)
	expect(t, x.publishTry(x.alice, x.shared, x.app), http.StatusOK)
	rec := x.alice.do(http.MethodPost, appPath(x.shared, x.app.ID, "/duplicate"), map[string]any{"space_id": x.alicePrivate.ID})
	expect(t, rec, http.StatusCreated)
	copied := decode[App](t, rec)
	if got := x.count(`SELECT count(*) FROM app_data_source_approvals WHERE app_id = ?`, copied.ID); got != 0 {
		t.Fatalf("the copy has %d approvals", got)
	}
	expectCode(t, x.publishTry(x.alice, x.alicePrivate, copied), http.StatusConflict, "app_data_source_unapproved")

	cur := decode[App](t, x.alice.do(http.MethodGet, appPath(x.shared, x.app.ID, ""), nil))
	expect(t, x.alice.do(http.MethodPost, appPath(x.shared, x.app.ID, "/move"), map[string]any{"version": cur.Version, "space_id": x.alicePrivate.ID}), http.StatusOK)
	if got := x.count(`SELECT count(*) FROM app_data_source_approvals WHERE app_id = ?`, x.app.ID); got != 0 {
		t.Fatalf("the moved app kept %d approvals", got)
	}

	x.approved2(copied)
	if _, err := x.e.db.Exec(`DELETE FROM apps WHERE id = ?`, copied.ID); err != nil {
		t.Fatal(err)
	}
	if got := x.count(`SELECT count(*) FROM app_data_source_approvals WHERE app_id = ?`, copied.ID); got != 0 {
		t.Fatalf("a deleted app left %d approvals", got)
	}
}

func (x *sourceWorld) approved2(a App) {
	x.e.t.Helper()
	req := x.mustRequest(x.alice, x.alicePrivate, a)
	if _, err := x.e.db.Exec(`UPDATE app_data_source_approvals SET status = 'approved', decided_at = '2026-01-01T00:00:00Z' WHERE id = ?`, req.ID); err != nil {
		x.e.t.Fatal(err)
	}
}

func TestTheOfflineCopyHasNoSources(t *testing.T) {
	x := newSourceWorld(t)
	x.approved(x.app)
	expect(t, x.publishTry(x.alice, x.shared, x.app), http.StatusOK)
	rec := x.download(x.alice, x.shared, x.app)
	expect(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), `"`+SourcesKey+`"`) || strings.Contains(rec.Body.String(), "used_bytes\":") {
		t.Fatalf("the offline copy carries sources: %s", rec.Body)
	}
}
