package dictation

import (
	"net/http"
	"testing"

	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/audit/audittest"
	"taskto.ai/taskto/internal/egress"
)

func (e *env) events(workspace int64) []audit.Row {
	e.t.Helper()
	rows, err := audit.List(ctx, e.db, audit.Filter{Workspace: workspace, Action: audit.DictationSettingsChanged, Limit: 100})
	if err != nil {
		e.t.Fatal(err)
	}
	return rows
}

func TestAuditDictationSettings(t *testing.T) {
	e := newEnv(t, &fakeLocal{state: LocalState{State: LocalReady}}, egress.Policy{})
	owner, ws := e.setup()

	expect(t, owner.do(http.MethodPatch, route(ws, ""), map[string]any{"language": "pt", "api": map[string]any{
		"base_url": e.api.URL + "/v1/", "model": "whisper-large-v3", "api_key": fakeKey,
	}}), http.StatusOK)
	rows := e.events(ws)
	if len(rows) != 1 {
		t.Fatalf("dictation_settings_changed = %+v", rows)
	}
	r := rows[0]
	if *r.ActorID != owner.id || *r.TargetID != ws || r.TargetType != audit.TargetWorkspace ||
		r.Details["engine"] != "local" || r.Details["language"] != "pt" || r.Details["api_model"] != nil ||
		r.Details["api_key_changed"] != true || r.Details["api_key_set"] != true || r.Details["base_host"] == "" {
		t.Fatalf("event = %+v", r)
	}

	expect(t, owner.do(http.MethodPatch, route(ws, ""), map[string]any{"engine": "api"}), http.StatusOK)
	rows = e.events(ws)
	if len(rows) != 2 || rows[0].Details["engine"] != "api" || rows[0].Details["api_key_changed"] != false || rows[0].Details["api_key_set"] != true {
		t.Fatalf("events = %+v", rows)
	}

	audittest.NoSecrets(t, e.db, map[string]string{"speech key": fakeKey})
}

func TestAuditDictationRefusalsRecordNothing(t *testing.T) {
	e := newEnv(t, &fakeLocal{state: LocalState{State: LocalReady}}, egress.Policy{})
	owner, ws := e.setup()
	member := e.join(owner, ws, "member")
	stranger, other := e.otherWorkspace(owner, ws)

	expect(t, member.do(http.MethodPatch, route(ws, ""), map[string]any{"language": "pt"}), http.StatusForbidden)
	expect(t, stranger.do(http.MethodPatch, route(ws, ""), map[string]any{"language": "pt"}), http.StatusNotFound)
	expect(t, owner.do(http.MethodPatch, route(ws, ""), map[string]any{"language": "not a language!"}), http.StatusBadRequest)
	expect(t, owner.do(http.MethodPatch, route(ws, ""), map[string]any{"api": map[string]any{"base_url": "http://speech.example.com/v1", "api_key": fakeKey}}), http.StatusBadRequest)
	if rows := e.events(0); len(rows) != 0 {
		t.Fatalf("refusals left %+v", rows)
	}

	expect(t, stranger.do(http.MethodPatch, route(other, ""), map[string]any{"language": "de"}), http.StatusOK)
	if len(e.events(other)) != 1 || len(e.events(ws)) != 0 {
		t.Fatalf("events: other %d, this %d", len(e.events(other)), len(e.events(ws)))
	}
}
