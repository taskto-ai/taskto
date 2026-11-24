package search

import (
	"fmt"
	"net/http"
	"testing"

	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/audit/audittest"
)

func (w *world) events(workspace int64, action string) []audit.Row {
	w.e.t.Helper()
	rows, err := audit.List(ctx, w.e.idx.db, audit.Filter{Workspace: workspace, Action: action, Limit: 100})
	if err != nil {
		w.e.t.Fatal(err)
	}
	return rows
}

func TestAuditEmbeddingsAndConsent(t *testing.T) {
	w := newWorld(t)
	account := w.addAccount("embed")
	settings := fmt.Sprintf("/api/workspaces/%d/search/settings", w.ws)

	// Turning embeddings on names the account and the model.
	expect(t, w.owner.do(http.MethodPut, settings, map[string]any{"embeddings": map[string]any{"account": account, "model": fakeModel}}), http.StatusOK)
	set := w.events(w.ws, audit.SearchEmbeddingsChanged)
	if len(set) != 1 {
		t.Fatalf("search_embeddings_changed = %+v", set)
	}
	if s := set[0]; *s.ActorID != w.owner.id || *s.TargetID != w.ws || s.TargetType != audit.TargetWorkspace ||
		s.Details["enabled"] != true || s.Details["account"] != float64(account) || s.Details["model"] != nil {
		t.Fatalf("set = %+v", s)
	}

	// Consenting for a space, as its editor, then withdrawing.
	expect(t, w.owner.do(http.MethodPut, embeddingURL(w.team), map[string]bool{"enabled": true}), http.StatusOK)
	consent := w.events(w.ws, audit.SearchConsentChanged)
	if len(consent) != 1 || *consent[0].ActorID != w.owner.id || *consent[0].TargetID != w.team.ID ||
		consent[0].TargetType != audit.TargetSpace || consent[0].Details["enabled"] != true {
		t.Fatalf("consent = %+v", consent)
	}
	expect(t, w.owner.do(http.MethodPut, embeddingURL(w.team), map[string]bool{"enabled": false}), http.StatusOK)
	if got := w.events(w.ws, audit.SearchConsentChanged); len(got) != 2 || got[0].Details["enabled"] != false {
		t.Fatalf("consent = %+v", got)
	}

	// Turning embeddings off.
	expect(t, w.owner.do(http.MethodPut, settings, map[string]any{}), http.StatusOK)
	if got := w.events(w.ws, audit.SearchEmbeddingsChanged); len(got) != 2 || got[0].Details["enabled"] != false {
		t.Fatalf("embeddings = %+v", got)
	}
	audittest.NoSecrets(t, w.e.idx.db, map[string]string{"provider key": fakeKey})
}

func TestAuditSearchRefusalsRecordNothing(t *testing.T) {
	w := newWorld(t)
	account := w.addAccount("embed")
	settings := fmt.Sprintf("/api/workspaces/%d/search/settings", w.ws)
	n := len(w.events(0, ""))

	// Not an admin; not an editor; not a member of the space; no embeddings model to consent to;
	// an account that isn't the workspace's.
	expect(t, w.alice.do(http.MethodPut, settings, map[string]any{"embeddings": map[string]any{"account": account, "model": fakeModel}}), http.StatusForbidden)
	expect(t, w.bob.do(http.MethodPut, embeddingURL(w.team), map[string]bool{"enabled": true}), http.StatusForbidden)
	expect(t, w.mallory.do(http.MethodPut, embeddingURL(w.team), map[string]bool{"enabled": true}), http.StatusNotFound)
	expect(t, w.owner.do(http.MethodPut, embeddingURL(w.team), map[string]bool{"enabled": true}), http.StatusConflict)
	expect(t, w.owner.do(http.MethodPut, settings, map[string]any{"embeddings": map[string]any{"account": account + 99, "model": fakeModel}}), http.StatusBadRequest)
	if got := len(w.events(0, "")); got != n {
		t.Fatalf("refusals left %d events", got-n)
	}
}

func TestAuditRemovingTheEmbeddingsAccountRecordsWhatGoesWithIt(t *testing.T) {
	w := newWorld(t)
	account := w.setModel()
	before := len(w.events(w.ws, audit.SearchEmbeddingsChanged))
	consents := len(w.events(w.ws, audit.SearchConsentChanged))

	expect(t, w.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/providers/%d", w.ws, account), nil), http.StatusNoContent)

	if n := len(w.events(w.ws, audit.ProviderAccountRemoved)); n != 1 {
		t.Fatalf("provider_account_removed = %d events, want 1", n)
	}
	set := w.events(w.ws, audit.SearchEmbeddingsChanged)
	if len(set) != before+1 || set[0].Details["enabled"] != false || set[0].Details["cascade"] != true ||
		*set[0].TargetID != w.ws || set[0].Details["account"] != float64(account) || *set[0].ActorID != w.owner.id {
		t.Fatalf("search_embeddings_changed = %+v, want the removal of the setting", set)
	}
	consent := w.events(w.ws, audit.SearchConsentChanged)
	if len(consent) != consents+1 || consent[0].Details["enabled"] != false || consent[0].Details["cascade"] != true ||
		*consent[0].TargetID != w.team.ID {
		t.Fatalf("search_consent_changed = %+v, want the removal of the team space's consent", consent)
	}
	// What was recorded is what is gone.
	var left int
	if err := w.e.idx.db.QueryRow(`SELECT (SELECT count(*) FROM search_settings) + (SELECT count(*) FROM search_embedding_consent)`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d settings and consents left, %v", left, err)
	}
}
