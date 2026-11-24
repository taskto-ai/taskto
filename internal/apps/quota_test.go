package apps

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/quota"
)

// meterFiles stands for a knowledge base that stores nothing, and lets a test hold the quota's
// measurement: while hook is set, every measurement calls it.
type meterFiles struct{ hook atomic.Pointer[func()] }

func (m *meterFiles) Usage(access.Space) (int64, int64, error) {
	if h := m.hook.Load(); h != nil {
		(*h)()
	}
	return 0, 0, nil
}

// meet makes the first two callers wait for each other, for at most a moment: two writers that
// both measure before either decides, if they can run side by side at all.
func meet() func() {
	var mu sync.Mutex
	arrived := 0
	both := make(chan struct{})
	return func() {
		mu.Lock()
		arrived++
		mine := arrived
		if arrived == 2 {
			close(both)
		}
		mu.Unlock()
		if mine <= 2 {
			select {
			case <-both:
			case <-time.After(400 * time.Millisecond):
			}
		}
	}
}

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

// Two pictures that each fit alone but not together: finalizing both at once keeps only one, since
// the growth over what was declared is checked where the row changes, not before.
func TestFinalizingTwoPicturesAtOnceCannotBothGrowPastTheQuota(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	picture := testPNG(t, 640, 480)
	stripped, err := stripMetadata(picture, "png")
	if err != nil {
		t.Fatal(err)
	}
	thumb, err := thumbnail(picture, "png")
	if err != nil {
		t.Fatal(err)
	}
	grow := int64(len(stripped)+len(thumb)) - int64(len(picture))
	if grow < 2 {
		t.Fatalf("the picture grows by %d bytes when finalized; the test needs it to", grow)
	}
	// Room for both pictures as declared, and for the growth of one of them.
	files := &meterFiles{}
	w.e.s.WithQuota(quota.New(w.e.db, quota.Limits{User: 2*int64(len(picture)) + grow + grow/2}, files))

	first := w.start(w.alice, s, a, "photos", len(picture))
	second := w.start(w.alice, s, a, "photos", len(picture))
	w.send(w.alice, s, a, first, picture, 4096)
	w.send(w.alice, s, a, second, picture, 4096)

	hold := meet()
	files.hook.Store(&hold)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, id := range []string{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = w.alice.do(http.MethodPost, appPath(s, a.ID, "/uploads/"+id+"/finalize"), map[string]any{}).Code
		}()
	}
	wg.Wait()
	ok, full := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusInsufficientStorage:
			full++
		}
	}
	if ok != 1 || full != 1 {
		t.Fatalf("finalize answers = %v, want one 200 and one 507", codes)
	}
	var ready int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM app_attachments WHERE app_id = ? AND status = 'ready'`, a.ID).Scan(&ready); err != nil || ready != 1 {
		t.Fatalf("ready pictures = %d, %v", ready, err)
	}
}

// Bytes another write has reserved (a document being stored, say) count against an upload, and
// give their place back when released.
func TestAnUploadCountsWhatAWriteHasReservedAndNotYetStored(t *testing.T) {
	w := newWorld(t)
	q := quota.New(w.e.db, quota.Limits{Workspace: 1000}, &meterFiles{})
	w.e.s.WithQuota(q)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	start := func() *httptest.ResponseRecorder {
		return w.alice.do(http.MethodPost, appPath(s, a.ID, "/uploads"), map[string]any{"field": "photos", "size": 300})
	}

	// A document write in another space of the workspace holds 800 bytes until it publishes.
	res, err := q.ReserveDB(context.Background(), w.shared, 800)
	if err != nil {
		t.Fatal(err)
	}
	expectCode(t, start(), http.StatusInsufficientStorage, "quota_workspace")
	res.Release()
	expect(t, start(), http.StatusCreated)
}

// A refused upload leaves no reservation behind, and neither does one that was let through once
// its transaction is over.
func TestUploadsHoldNothingOnceTheyAreRecorded(t *testing.T) {
	w := newWorld(t)
	w.e.s.WithQuota(quota.New(w.e.db, quota.Limits{User: 1000}, &meterFiles{}))
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	start := func(size int) *httptest.ResponseRecorder {
		return w.alice.do(http.MethodPost, appPath(s, a.ID, "/uploads"), map[string]any{"field": "photos", "size": size})
	}
	expect(t, start(600), http.StatusCreated)
	expectCode(t, start(600), http.StatusInsufficientStorage, "quota_user")
	// Only the 600 recorded count: 400 fit, and then nothing more would if something were held twice.
	expect(t, start(400), http.StatusCreated)
}

// An effect that makes a document the quota refuses fails with the quota's own code: the person is
// told it is the storage, not that the document was wrong.
func TestADocumentEffectOverTheQuotaFailsAsTheQuota(t *testing.T) {
	f := newFX(t)
	f.docs.SetQuota(func(access.Space, int64) (func(), error) {
		return nil, &quota.Error{Scope: quota.ScopeWorkspace, Limit: 1 << 20, Used: 1 << 20}
	})
	f.mustSubmit(f.alice, "op-quota-001", "East gate", "fail")
	f.work()
	v := only(t, f.effects(f.alice, ""), EffectCreateDocument)
	if v.Status != StateFailed || v.Error.Code != "app_effect_quota" {
		t.Fatalf("document effect = %+v, want failed as app_effect_quota", v)
	}
	// A knowledge base that can't be measured is not the person's to fix: it is tried again, not failed.
	f2 := newFX(t)
	f2.docs.SetQuota(func(access.Space, int64) (func(), error) {
		return nil, &quota.UnavailableError{Err: errors.New("busy")}
	})
	f2.mustSubmit(f2.alice, "op-quota-002", "East gate", "fail")
	f2.work()
	if v := only(t, f2.effects(f2.alice, ""), EffectCreateDocument); v.Status == StateFailed && v.Error.Code == "app_effect_quota" {
		t.Fatalf("an unmeasurable space failed the effect as the quota: %+v", v)
	}
}

