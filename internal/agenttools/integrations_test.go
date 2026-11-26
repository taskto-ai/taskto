package agenttools

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// invoicing declares a slot of payments, as an agent would write it.
func invoicing() map[string]any {
	var def map[string]any
	if err := json.Unmarshal([]byte(`{
  "definition_version": 3,
  "components": [{"id":"root","component":"Text","text":"Invoices"}],
  "collection": {"fields": [
    {"name": "number", "type": "string", "required": true},
    {"name": "total", "type": "number", "required": true},
    {"name": "status", "type": "choice", "options": ["draft", "sent", "paid"]},
    {"name": "pay_url", "type": "string", "max_length": 2048},
    {"name": "payment_id", "type": "string", "max_length": 255}
  ]},
  "actions": {
    "add": {"kind": "insert"},
    "send": {"kind": "update", "effects": [{
      "kind": "create_payment_link", "integration": "payments", "amount_field": "total", "currency": "EUR",
      "description": "Invoice {{number}}", "url_field": "pay_url", "id_field": "payment_id"}]}
  },
  "integrations": {"payments": {"kind": "payments", "on": {
    "payment.succeeded": {"set": {"status": {"value": "paid"}}}}}}
}`), &def); err != nil {
		panic(err)
	}
	return def
}

// integration connects an account to the workspace and allows it in the spaces, as an admin does.
// Its provider, public id and settings are what the tool must never say.
func (w *world) integration(workspace int64, name, display string, disabled bool, spaces ...int64) int64 {
	w.t.Helper()
	id := w.insert(`INSERT INTO integrations (workspace_id, public_id, provider, name, display_name, mode, settings,
			max_amount_minor, currencies, disabled_at)
		VALUES (?, ?, 'stripe', ?, ?, 'test', '{"location_id":"canary-location"}', 123456, '["EUR","USD"]',
			CASE WHEN ? THEN '2026-01-01T00:00:00.000Z' END) RETURNING id`,
		workspace, publicID(name), name, display, disabled)
	w.exec(`INSERT INTO integration_secrets (integration_id, name, sealed) VALUES (?, 'api_key', x'6e6f742d612d726561322d6b6579')`, id)
	for _, sp := range spaces {
		w.exec(`INSERT INTO integration_spaces (integration_id, space_id, allowed_by) VALUES (?, ?, ?)`, id, sp, w.alice)
	}
	return id
}

// publicID is a 32-character id that holds the name and the marker the tests search replies for.
func publicID(name string) string { return (name + "c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4")[:32] }

func listed(out map[string]any, key string) []map[string]any {
	var res []map[string]any
	for _, v := range out[key].([]any) {
		res = append(res, v.(map[string]any))
	}
	return res
}

func TestListIntegrationsIsReadOnlyWithNoAccountFields(t *testing.T) {
	var def *Definition
	for _, d := range Definitions() {
		if d.Name == "list_integrations" {
			d := d
			def = &d
		}
	}
	if def == nil || !def.ReadOnly || def.ConversationOnly || def.NoToken {
		t.Fatalf("list_integrations = %+v", def)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
		Additional bool                       `json:"additionalProperties"`
	}
	if err := json.Unmarshal(def.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	// Two ids that name a space and an app, and nothing that could hold a credential.
	if len(schema.Properties) != 2 || schema.Properties["space_id"] == nil || schema.Properties["app_id"] == nil || schema.Additional {
		t.Fatalf("schema = %s", def.InputSchema)
	}
	text := strings.ToLower(string(def.InputSchema))
	for _, word := range []string{"secret", "key", "token", "password", "credential", "provider"} {
		if strings.Contains(text, word) {
			t.Errorf("the schema mentions %q: %s", word, def.InputSchema)
		}
	}
}

func TestListIntegrationsNamesKindsAndStatusOnly(t *testing.T) {
	w := newWorld(t)
	w.integration(w.one, "shop", "Shop", false, w.shared, w.alicePrivate)
	w.integration(w.one, "old", "Old shop", true, w.shared)
	w.integration(w.one, "bobs", "Bob's", false, w.bobPrivate)

	out := w.ok(w.aliceConv, "list_integrations", map[string]any{"space_id": w.shared})
	got := listed(out, "integrations")
	want := []map[string]any{
		{"name": "shop", "display_name": "Shop", "kind": "payments", "status": "active"},
		{"name": "old", "display_name": "Old shop", "kind": "payments", "status": "disabled"},
	}
	if len(got) != len(want) {
		t.Fatalf("shared = %v", out)
	}
	for i := range want {
		if len(got[i]) != len(want[i]) {
			t.Errorf("integration %d has keys %v, want exactly %v", i, got[i], want[i])
		}
		for k, v := range want[i] {
			if got[i][k] != v {
				t.Errorf("integration %d: %s = %v, want %v", i, k, got[i][k], v)
			}
		}
	}
	// Without an app there are no slots to list, and the reply says so: the key is always there.
	if slots, ok := out["slots"].([]any); !ok || len(slots) != 0 {
		t.Errorf("slots without an app = %v, want an empty list: %v", out["slots"], out)
	}

	// Nothing of the account, however the reply is searched: the provider, the public id, a
	// setting, the limits, an id.
	raw, _ := json.Marshal(out)
	for _, leak := range []string{"stripe", "c4c4c4", "canary", "123456", "EUR", "api_key", "secret", "location", `"id"`} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the reply holds %q: %s", leak, raw)
		}
	}

	// Her private space lists what is allowed there, and a space with nothing is an empty list.
	if got := listed(w.ok(w.aliceConv, "list_integrations", map[string]any{"space_id": w.alicePrivate}), "integrations"); len(got) != 1 || got[0]["name"] != "shop" {
		t.Errorf("private = %v", got)
	}
	empty := w.ok(w.bobConv, "list_integrations", map[string]any{"space_id": w.bobPrivate})
	if got := listed(empty, "integrations"); len(got) != 1 || got[0]["name"] != "bobs" {
		t.Errorf("bob's own space = %v", got)
	}
}

func TestListIntegrationsRespectsAccess(t *testing.T) {
	w := newWorld(t)
	w.integration(w.one, "shop", "Shop", false, w.shared, w.alicePrivate, w.bobPrivate)
	w.integration(w.two, "other", "Elsewhere", false, w.alicePrivate2)

	// A space the user can't read, another workspace's space and a space that doesn't exist are
	// all "not found" alike: the list says nothing of who else has what.
	for _, space := range []struct {
		conv  int64
		space int64
		why   string
	}{
		{w.aliceConv, w.bobPrivate, "another person's private space"},
		{w.bobConv, w.alicePrivate, "another person's private space"},
		{w.aliceConv, w.alicePrivate2, "her own space of another workspace"},
		{w.aliceConv2, w.shared, "a shared space of another workspace"},
		{w.aliceConv, w.alicePrivate2 + 1000, "a space that doesn't exist"},
	} {
		if out := w.status(http.StatusNotFound, space.conv, "list_integrations", map[string]any{"space_id": space.space}); out["error"] != "not found" {
			t.Errorf("%s: %v", space.why, out)
		}
	}
	// Workspace two's conversation lists workspace two's.
	if got := listed(w.ok(w.aliceConv2, "list_integrations", map[string]any{"space_id": w.alicePrivate2}), "integrations"); len(got) != 1 || got[0]["name"] != "other" {
		t.Errorf("workspace two = %v", got)
	}

	// No token, and a capability of another conversation, don't reach it either.
	if code, _ := w.callWith("", "list_integrations", map[string]any{"space_id": w.shared}); code != http.StatusUnauthorized {
		t.Errorf("without a capability: %d", code)
	}

	// Unknown arguments are refused rather than ignored.
	w.status(http.StatusBadRequest, w.aliceConv, "list_integrations", map[string]any{"space_id": w.shared, "include_secrets": true})
	w.status(http.StatusNotFound, w.aliceConv, "list_integrations", map[string]any{})
}

func TestListIntegrationsSlotsOfAnApp(t *testing.T) {
	w := newWorld(t)
	w.integration(w.one, "shop", "Shop", false, w.shared)
	team := w.ok(w.aliceConv, "create_app", map[string]any{"space_id": w.shared, "name": "Invoices", "definition": invoicing()})
	bobs := w.ok(w.bobConv, "create_app", map[string]any{"space_id": w.bobPrivate, "name": "Bob's invoices", "definition": invoicing()})

	out := w.ok(w.aliceConv, "list_integrations", map[string]any{"space_id": w.shared, "app_id": id(team["id"])})
	slots := listed(out, "slots")
	if len(slots) != 1 || slots[0]["slot"] != "payments" || slots[0]["kind"] != "payments" || slots[0]["status"] != "none" || slots[0]["live_status"] != nil {
		t.Fatalf("slots = %v", out)
	}
	if len(slots[0]) != 5 {
		t.Errorf("a slot has keys %v: only its name, kind and statuses", slots[0])
	}
	raw, _ := json.Marshal(out)
	for _, leak := range []string{"contract", "hash", "approval", "pay_url", "total"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the reply holds %q: %s", leak, raw)
		}
	}

	// Once the draft is live, the live release's own status is there too.
	w.publish(id(team["id"]))
	slots = listed(w.ok(w.aliceConv, "list_integrations", map[string]any{"space_id": w.shared, "app_id": id(team["id"])}), "slots")
	if slots[0]["live_status"] != "none" || slots[0]["live_usable"] != false {
		t.Errorf("live = %v", slots[0])
	}

	// Bob only views the shared space: he lists its integrations, not an app's slots, which are
	// for those who may design or publish it.
	w.ok(w.bobConv, "list_integrations", map[string]any{"space_id": w.shared})
	w.status(http.StatusForbidden, w.bobConv, "list_integrations", map[string]any{"space_id": w.shared, "app_id": id(team["id"])})
	// Another person's private app is not found through any space, and neither is an app that
	// doesn't exist.
	w.status(http.StatusNotFound, w.aliceConv, "list_integrations", map[string]any{"space_id": w.bobPrivate, "app_id": id(bobs["id"])})
	w.status(http.StatusNotFound, w.aliceConv, "list_integrations", map[string]any{"space_id": w.alicePrivate, "app_id": id(bobs["id"])})
	w.status(http.StatusNotFound, w.aliceConv, "list_integrations", map[string]any{"space_id": w.shared, "app_id": id(team["id"]) + 1000})
}
