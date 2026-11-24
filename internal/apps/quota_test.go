package apps

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"taskto.ai/taskto/internal/quota"
)

func TestUploadsOverAPrivateSpaceQuotaAreRefused(t *testing.T) {
	w := newWorld(t)
	w.e.s.WithQuota(quota.New(w.e.db, quota.Limits{User: 1000}, nil))
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	start := func(size int) *httptest.ResponseRecorder {
		return w.alice.do(http.MethodPost, appPath(s, a.ID, "/uploads"), map[string]any{"field": "photos", "size": size})
	}

	// The first upload holds its place at the size it declared, so the second doesn't fit.
	first := start(600)
	expect(t, first, http.StatusCreated)
	rec := start(600)
	expectCode(t, rec, http.StatusInsufficientStorage, "quota_user")
	if got := decode[struct{ Params map[string]any }](t, rec).Params["max_mib"]; got != float64(1) {
		t.Fatalf("params = %v, want the quota in MiB", got)
	}
	var rows int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM app_attachments WHERE app_id = ?`, a.ID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("attachments = %d, %v; a refused upload leaves nothing", rows, err)
	}

	// Abandoning the first gives its place back.
	up := decode[Attachment](t, first)
	expect(t, w.alice.do(http.MethodDelete, appPath(s, a.ID, "/uploads/"+up.ID), nil), http.StatusNoContent)
	expect(t, start(600), http.StatusCreated)

	// Bob's private space has the same allowance of its own: alice's uploads don't count in it.
	b := w.createPhotos(w.bob, w.bobPrivate, "Photos")
	expect(t, w.bob.do(http.MethodPost, appPath(w.bobPrivate, b.ID, "/uploads"), map[string]any{"field": "photos", "size": 600}), http.StatusCreated)
}

func TestFinalizeCountsTheThumbnail(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	picture := testPNG(t, 640, 480)
	// Room for the picture and not for the thumbnail finalizing makes beside it.
	w.e.s.WithQuota(quota.New(w.e.db, quota.Limits{User: int64(len(picture)) + 10}, nil))

	id := w.start(w.alice, s, a, "photos", len(picture))
	w.send(w.alice, s, a, id, picture, 4096)
	rec := w.alice.do(http.MethodPost, appPath(s, a.ID, "/uploads/"+id+"/finalize"), map[string]any{})
	expectCode(t, rec, http.StatusInsufficientStorage, "quota_user")
	// It is still an upload, which finalizes once there is room.
	var status string
	if err := w.e.db.QueryRow(`SELECT status FROM app_attachments WHERE id = ?`, id).Scan(&status); err != nil || status != "uploading" {
		t.Fatalf("status = %q, %v", status, err)
	}
	w.e.s.WithQuota(quota.New(w.e.db, quota.Limits{User: int64(len(picture)) * 2}, nil))
	expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/uploads/"+id+"/finalize"), map[string]any{}), http.StatusOK)
}

func TestUploadsCountAgainstTheWorkspaceQuota(t *testing.T) {
	w := newWorld(t)
	// Every space of the workspace together, private ones included.
	w.e.s.WithQuota(quota.New(w.e.db, quota.Limits{Workspace: 3000}, nil))
	shared := w.createPhotos(w.alice, w.shared, "Team photos")
	mine := w.createPhotos(w.alice, w.alicePrivate, "Photos")
	post := func(s App, space int, size int) *httptest.ResponseRecorder {
		sp := w.shared
		if space == 1 {
			sp = w.alicePrivate
		}
		return w.alice.do(http.MethodPost, appPath(sp, s.ID, "/uploads"), map[string]any{"field": "photos", "size": size})
	}
	expect(t, post(shared, 0, 1500), http.StatusCreated)
	expect(t, post(mine, 1, 1000), http.StatusCreated)
	// 2500 are taken: 600 more won't fit, whichever space they are for.
	expectCode(t, post(shared, 0, 600), http.StatusInsufficientStorage, "quota_workspace")
	expectCode(t, post(mine, 1, 600), http.StatusInsufficientStorage, "quota_workspace")
	expect(t, post(mine, 1, 400), http.StatusCreated)

	// Another workspace has its allowance of its own: this one's uploads don't count there.
	theirs := w.createPhotos(w.stranger, w.otherShared, "Their photos")
	expect(t, w.stranger.do(http.MethodPost, appPath(w.otherShared, theirs.ID, "/uploads"), map[string]any{"field": "photos", "size": 2900}), http.StatusCreated)
}
