package apps

import (
	"fmt"
	"net/http"
	"testing"

	"taskto.ai/taskto/internal/apitypes/nonnull"
)

func TestAppListsAreNeverNil(t *testing.T) {
	w := newWorld(t)
	u, s := w.alice, w.alicePrivate

	def := example(t)
	def.Requires = nil
	rec := u.do(http.MethodPost, appsPath(s, ""), map[string]any{"name": "Bare", "definition": def})
	expect(t, rec, http.StatusCreated)
	bare := decode[App](t, rec)
	nonnull.Require(t, bare)
	bare = w.publish(u, s, bare)
	nonnull.Require(t, bare)

	for _, a := range []App{w.aliceApp, bare} {
		nonnull.Require(t, decode[App](t, u.do(http.MethodGet, appPath(s, a.ID, ""), nil)))
		for _, got := range decode[struct{ Apps []App }](t, u.do(http.MethodGet, appsPath(s, ""), nil)).Apps {
			nonnull.Require(t, got)
		}
		rels := decode[struct{ Releases []Release }](t, u.do(http.MethodGet, appPath(s, a.ID, "/releases"), nil)).Releases
		if len(rels) == 0 {
			t.Fatal("no releases")
		}
		for _, r := range rels {
			nonnull.Require(t, r)
		}
		nonnull.Require(t, decode[Release](t, u.do(http.MethodGet, appPath(s, a.ID, fmt.Sprintf("/releases/%d", rels[0].Number)), nil)))
		nonnull.Require(t, decode[OfflineApp](t, w.download(u, s, a)))
		nonnull.Require(t, decode[EffectList](t, u.do(http.MethodGet, appPath(s, a.ID, "/effects"), nil)))
	}
}

func TestRecordDataIsNeverNil(t *testing.T) {
	w := newWorld(t)
	u, s := w.alice, w.shared
	a := w.editable(u, s)
	w.add(u, s, a, "lunch", "10")
	id, revision := w.idOf(u, s, a, "lunch")

	nonnull.Require(t, decode[Records](t, w.records(u, s, a)))
	nonnull.Require(t, decode[History](t, w.history(u, s, a, id)))

	expect(t, w.act(u, s, a, "edit_expense", map[string]any{"id": id, "amount": "2"}, &revision), http.StatusOK)
	nonnull.Require(t, decode[History](t, w.history(u, s, a, id)))
	expect(t, w.act(u, s, a, "delete_expense", map[string]any{"id": id}, nil), http.StatusOK)
	deleted := decode[Records](t, w.deleted(u, s, a))
	if len(deleted.Records) != 1 {
		t.Fatalf("deleted: %+v", deleted)
	}
	nonnull.Require(t, deleted)
	nonnull.Require(t, decode[History](t, w.history(u, s, a, id)))
	nonnull.Require(t, decode[Record](t, w.restore(u, s, a, id, nil)))
}

func TestHomeListsAreNeverNil(t *testing.T) {
	w := newWorld(t)
	for _, u := range []*user{w.alice, w.bob, w.mallory} {
		nonnull.Require(t, w.home(u))
		rec := u.do(http.MethodGet, homePath(w.ws, "/available"), nil)
		expect(t, rec, http.StatusOK)
		for _, got := range decode[struct{ Apps []AvailableApp }](t, rec).Apps {
			nonnull.Require(t, got)
		}
	}
	app := w.contribute(w.alice, w.shared, w.sharedApp, offers())
	w.place(w.alice, w.shared, app, "widget", "spend", http.StatusCreated)
	nonnull.Require(t, w.home(w.alice))
	rec := w.alice.do(http.MethodGet, homePath(w.ws, "/available"), nil)
	expect(t, rec, http.StatusOK)
	for _, got := range decode[struct{ Apps []AvailableApp }](t, rec).Apps {
		nonnull.Require(t, got)
	}
}

func TestTemplateListsAreNeverNil(t *testing.T) {
	w := newWorld(t)
	rec := w.alice.do(http.MethodGet, templatesPath(w.ws), nil)
	expect(t, rec, http.StatusOK)
	got := decode[struct{ Templates []TemplateInfo }](t, rec).Templates
	if len(got) == 0 {
		t.Fatal("no templates")
	}
	for _, info := range got {
		nonnull.Require(t, info)
	}
}
