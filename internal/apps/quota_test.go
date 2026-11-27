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

type meterFiles struct{ hook atomic.Pointer[func()] }

func (m *meterFiles) Usage(access.Space) (int64, int64, error) {
	if h := m.hook.Load(); h != nil {
		(*h)()
	}
	return 0, 0, nil
}

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

	up := decode[Attachment](t, first)
	expect(t, w.alice.do(http.MethodDelete, appPath(s, a.ID, "/uploads/"+up.ID), nil), http.StatusNoContent)
	expect(t, start(600), http.StatusCreated)

	b := w.createPhotos(w.bob, w.bobPrivate, "Photos")
	expect(t, w.bob.do(http.MethodPost, appPath(w.bobPrivate, b.ID, "/uploads"), map[string]any{"field": "photos", "size": 600}), http.StatusCreated)
}

func TestFinalizeCountsTheThumbnail(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	picture := testPNG(t, 640, 480)
	w.e.s.WithQuota(quota.New(w.e.db, quota.Limits{User: int64(len(picture)) + 10}, nil))

	id := w.start(w.alice, s, a, "photos", len(picture))
	w.send(w.alice, s, a, id, picture, 4096)
	rec := w.alice.do(http.MethodPost, appPath(s, a.ID, "/uploads/"+id+"/finalize"), map[string]any{})
	expectCode(t, rec, http.StatusInsufficientStorage, "quota_user")
	var status string
	if err := w.e.db.QueryRow(`SELECT status FROM app_attachments WHERE id = ?`, id).Scan(&status); err != nil || status != "uploading" {
		t.Fatalf("status = %q, %v", status, err)
	}
	w.e.s.WithQuota(quota.New(w.e.db, quota.Limits{User: int64(len(picture)) * 2}, nil))
	expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/uploads/"+id+"/finalize"), map[string]any{}), http.StatusOK)
}

func TestUploadsCountAgainstTheWorkspaceQuota(t *testing.T) {
	w := newWorld(t)
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
	expectCode(t, post(shared, 0, 600), http.StatusInsufficientStorage, "quota_workspace")
	expectCode(t, post(mine, 1, 600), http.StatusInsufficientStorage, "quota_workspace")
	expect(t, post(mine, 1, 400), http.StatusCreated)

	theirs := w.createPhotos(w.stranger, w.otherShared, "Their photos")
	expect(t, w.stranger.do(http.MethodPost, appPath(w.otherShared, theirs.ID, "/uploads"), map[string]any{"field": "photos", "size": 2900}), http.StatusCreated)
}

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

type failingFiles struct{}

func (failingFiles) Usage(access.Space) (int64, int64, error) {
	return 0, 0, errors.New("disk trouble")
}

func TestFinalizeThatCannotBeMeasuredLeavesNoFilesAndCanBeRetried(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	picture := testPNG(t, 640, 480)
	id := w.start(w.alice, s, a, "photos", len(picture))
	w.send(w.alice, s, a, id, picture, 4096)
	w.e.s.WithQuota(quota.New(w.e.db, quota.Limits{User: 1 << 30}, failingFiles{}))
	expectCode(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/uploads/"+id+"/finalize"), map[string]any{}), http.StatusServiceUnavailable, "quota_unavailable")
	root, done, err := w.e.s.filesRoot(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	for _, ext := range []string{".thumb", ""} {
		if _, err := root.Stat(fileName(a.ID, id, ext)); err == nil {
			t.Errorf("the attempt left %q behind", ext)
		}
	}
	w.e.s.WithQuota(quota.New(w.e.db, quota.Limits{User: 1 << 30}, &meterFiles{}))
	expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/uploads/"+id+"/finalize"), map[string]any{}), http.StatusOK)
}

func TestAnUploadCountsWhatAWriteHasReservedAndNotYetStored(t *testing.T) {
	w := newWorld(t)
	q := quota.New(w.e.db, quota.Limits{Workspace: 1000}, &meterFiles{})
	w.e.s.WithQuota(q)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	start := func() *httptest.ResponseRecorder {
		return w.alice.do(http.MethodPost, appPath(s, a.ID, "/uploads"), map[string]any{"field": "photos", "size": 300})
	}

	res, err := q.ReserveDB(context.Background(), w.shared, 800)
	if err != nil {
		t.Fatal(err)
	}
	expectCode(t, start(), http.StatusInsufficientStorage, "quota_workspace")
	res.Release()
	expect(t, start(), http.StatusCreated)
}

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
	expect(t, start(400), http.StatusCreated)
}

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
	f2 := newFX(t)
	f2.docs.SetQuota(func(access.Space, int64) (func(), error) {
		return nil, &quota.UnavailableError{Err: errors.New("busy")}
	})
	f2.mustSubmit(f2.alice, "op-quota-002", "East gate", "fail")
	f2.work()
	if v := only(t, f2.effects(f2.alice, ""), EffectCreateDocument); v.Status == StateFailed || v.Status == StateDone {
		t.Fatalf("an unmeasurable space left the effect %+v, want it waiting to be tried again", v)
	}
}
