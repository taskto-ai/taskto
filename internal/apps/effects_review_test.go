package apps

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/knowledge"
)

func (f *fx) change(u *user, name, operation string, id int64, state string) *httptest.ResponseRecorder {
	f.e.t.Helper()
	a := action(f.app, name, map[string]any{"id": id, "state": state})
	a.Operation = operation
	return u.do(http.MethodPost, appPath(f.shared, f.app.ID, "/actions"), a)
}

func (f *fx) mustChange(u *user, name, operation string, id int64, state string) {
	f.e.t.Helper()
	expect(f.e.t, f.change(u, name, operation, id, state), http.StatusOK)
}

func (f *fx) allAndOwn() {
	f.w().mustSetRoles(f.alice, f.shared, f.app,
		grant(f.alice.id, access.AppBuilder, access.AppPublisher, access.AppDataManager, access.AppUser),
		grant(f.owner.id, access.AppUser), grant(f.bob.id, access.AppReader))
}

func (f *fx) purge() {
	f.e.t.Helper()
	if err := f.e.s.purge(f.ctx()); err != nil {
		f.e.t.Fatal(err)
	}
}

func TestACleanedUpEntrysNumberIsNeverGivenAgain(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-reuse-001", "East gate", "fail")
	f.work()
	first := f.cards()
	if len(first) != 1 {
		t.Fatalf("cards = %+v", first)
	}
	f.later(keepDone + time.Hour)
	f.purge()
	if n := f.count(`SELECT count(*) FROM app_effects`); n != 0 {
		t.Fatalf("%d entries kept", n)
	}
	f.mustSubmit(f.alice, "op-reuse-002", "West gate", "fail")
	f.work()
	cards := f.cards()
	if len(cards) != 2 || cards[0].ID == cards[1].ID {
		t.Fatalf("cards = %+v: the new record has no card of its own", cards)
	}
	rec := f.next + 1
	var made EffectView
	for _, v := range f.effects(f.alice, "").Effects {
		if v.Record == rec && v.Kind == EffectCreateCard {
			made = v
		}
	}
	if made.Status != StateDone || made.Result["card_id"] != float64(cards[1].ID) {
		t.Fatalf("the new record's card effect = %+v, cards %+v", made, cards)
	}
}

func TestCleanupKeepsTheLinkOfALiveRecordToItsCard(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-link-0001", "East gate", "fail")
	f.work()
	f.later(keepDone + time.Hour)
	f.purge()
	if n := f.count(`SELECT count(*) FROM app_effects`); n != 0 {
		t.Fatalf("%d entries kept", n)
	}
	if n := f.count(`SELECT count(*) FROM app_effect_cards`); n != 1 {
		t.Fatalf("%d links kept", n)
	}
	f.mustChange(f.alice, "resolve", "op-link-0002", f.next, "resolved")
	f.work()
	if cards := f.cards(); len(cards) != 1 || cards[0].Column != "done" {
		t.Fatalf("cards = %+v", cards)
	}
	if v := only(t, f.effects(f.alice, ""), EffectUpdateCard); v.Status != StateDone {
		t.Fatalf("update effect = %+v", v)
	}
}

func TestAMoveDoneAgainLateNeverUndoesANewerOne(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-order-001", "East gate", "fail")
	f.work()
	f.mustChange(f.alice, "resolve", "op-order-002", f.next, "resolved")
	f.e.s.hooks.afterWork = func(e effectRow) error {
		if e.kind == EffectUpdateCard {
			return errCrash
		}
		return nil
	}
	f.work()
	if cards := f.cards(); len(cards) != 1 || cards[0].Column != "done" {
		t.Fatalf("cards = %+v", cards)
	}
	f.e.s.hooks.afterWork = nil
	f.mustChange(f.alice, "reopen", "op-order-003", f.next, "open")
	f.work()
	if cards := f.cards(); len(cards) != 1 || cards[0].Column != "todo" {
		t.Fatalf("cards = %+v", cards)
	}
	f.later(leaseTime + time.Second)
	f.work()
	if cards := f.cards(); len(cards) != 1 || cards[0].Column != "todo" {
		t.Fatalf("the late replay undid the newer move: %+v", cards)
	}
	for _, v := range f.effects(f.alice, "").Effects {
		if v.Kind == EffectUpdateCard && v.Status != StateDone {
			t.Fatalf("effect = %+v", v)
		}
	}
	if f.effects(f.alice, "").Active != 0 {
		t.Fatal("entries are still waiting")
	}
}

func TestQueuedUpdatesDoNotOutliveTheActorsScope(t *testing.T) {
	f := newFX(t)
	f.allAndOwn()
	f.mustSubmit(f.owner, "op-scope-0001", "East gate", "fail")
	f.work()
	f.mustChange(f.alice, "resolve", "op-scope-0002", f.next, "resolved")
	f.w().mustSetRoles(f.alice, f.shared, f.app,
		grant(f.alice.id, access.AppBuilder, access.AppPublisher, access.AppUser, access.AppReader), grant(f.owner.id, access.AppUser))
	f.work()
	if cards := f.cards(); len(cards) != 1 || cards[0].Column != "todo" {
		t.Fatalf("the card was moved for someone who no longer changes that record: %+v", cards)
	}
	v := only(t, f.effects(f.alice, ""), EffectUpdateCard)
	if v.Status != StateFailed || v.Error == nil || v.Error.Code != "app_effect_app_access" {
		t.Fatalf("update effect = %+v", v)
	}
	expectForbidden(t, f.alice.do(http.MethodPost, appPath(f.shared, f.app.ID, fmt.Sprintf("/effects/%d/retry", v.ID)), struct{}{}), capModifyRecords)
}

func TestQueuedEffectsDoNotRunInAnAppArchivedMeanwhile(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-offline-01", "East gate", "fail")
	cur := f.view(f.alice, f.shared, f.app)
	expect(t, f.alice.do(http.MethodPatch, appPath(f.shared, f.app.ID, ""), map[string]any{"version": cur.Version, "archived": true}), http.StatusOK)
	f.work()
	if n := len(f.cards()); n != 0 {
		t.Fatalf("%d cards made for an archived app", n)
	}
	for _, v := range f.effects(f.alice, "").Effects {
		if v.Status != StateFailed || v.Error.Code != "app_effect_app_offline" {
			t.Fatalf("effect = %+v", v)
		}
	}
}

func TestEffectsOfADeletedRecordFailUntilItIsRestored(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-gone-00001", "East gate", "fail")
	rm := action(f.app, "remove", map[string]any{"id": f.next})
	rm.Operation = "op-gone-00002"
	expect(t, f.alice.do(http.MethodPost, appPath(f.shared, f.app.ID, "/actions"), rm), http.StatusOK)
	f.work()
	if n := len(f.cards()); n != 0 {
		t.Fatalf("%d cards made for a deleted record", n)
	}
	l := f.effects(f.alice, "")
	if l.Failed != 3 {
		t.Fatalf("list = %+v", l)
	}
	for _, v := range l.Effects {
		if v.Error == nil || v.Error.Code != "app_effect_record_gone" {
			t.Fatalf("effect = %+v", v)
		}
	}
	var revision int64
	if err := f.e.db.QueryRow(`SELECT revision FROM app_rows WHERE id = ?`, f.next).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	expect(t, f.alice.do(http.MethodPost, appPath(f.shared, f.app.ID, fmt.Sprintf("/records/%d/restore", f.next)), map[string]any{"expected_revision": revision}), http.StatusOK)
	f.work()
	if n := len(f.cards()); n != 0 {
		t.Fatalf("a restore alone queued the effects again: %d cards", n)
	}
	for _, v := range l.Effects {
		expect(t, f.alice.do(http.MethodPost, appPath(f.shared, f.app.ID, fmt.Sprintf("/effects/%d/retry", v.ID)), struct{}{}), http.StatusOK)
	}
	f.work()
	if n := len(f.cards()); n != 1 {
		t.Fatalf("%d cards after the retries", n)
	}
}

func TestEffectDetailsAreToldOnlyToWhoReadsTheTarget(t *testing.T) {
	f := newFX(t)
	f.allAndOwn()
	f.mustSubmit(f.owner, "op-detail-001", "East gate", "fail")
	west := fmt.Sprintf("inspections/%d-west-gate.md", f.next+1)
	if _, err := f.docs.Put(f.ctx(), f.maint, west, []byte("someone else's"), "", knowledge.Author{Name: "x", Email: "x@example.test"}, "x"); err != nil {
		t.Fatal(err)
	}
	f.mustSubmit(f.owner, "op-detail-002", "West gate", "fail")
	f.work()
	own := f.effects(f.owner, "")
	var seenTitle, seenParams bool
	for _, v := range own.Effects {
		seenTitle = seenTitle || v.Detail.Title != ""
		seenParams = seenParams || (v.Error != nil && v.Error.Params["path"] == west)
	}
	if !seenTitle || !seenParams {
		t.Fatalf("someone who reads the target should see what the effects make: %+v", own)
	}
	if _, err := f.e.db.Exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, f.maint.ID, f.owner.id); err != nil {
		t.Fatal(err)
	}
	for _, v := range f.effects(f.owner, "").Effects {
		if v.Detail.Title != "" || v.Detail.Path != "" || v.Detail.Column != "" {
			t.Fatalf("detail of a target the caller can't read: %+v", v)
		}
		if v.Error != nil && v.Error.Params != nil {
			t.Fatalf("error params of a target the caller can't read: %+v", v.Error)
		}
	}
	var kept bool
	for _, v := range f.effects(f.alice, "").Effects {
		kept = kept || v.Detail.Title != ""
	}
	if !kept {
		t.Fatal("alice lost the details")
	}
}

func TestAReleaseThatRemovesAFieldHidesWhatItFilledIn(t *testing.T) {
	f := newFX(t)
	d := withTitle(f.inspections(), "Fix {{site}} ({{notes}})")
	cur := f.mustPatchDraft(f.alice, f.shared, f.view(f.alice, f.shared, f.app), d)
	f.publish(f.alice, f.shared, cur)
	f.mustSubmit(f.alice, "op-fields-001", "East gate", "fail")
	f.work()
	title := "Fix East gate (leaking)"
	if v := only(t, f.effects(f.alice, ""), EffectCreateCard); v.Detail.Title != title || v.Result["title"] != title {
		t.Fatalf("card effect = %+v", v)
	}
	next := without(d, "notes")
	act := next.Actions["submit_inspection"]
	effects := slices.Clone(act.Effects)
	effects[0].Title, effects[0].Body = "Fix {{site}}", ""
	act.Effects = effects
	next.Actions = map[string]Action{"submit_inspection": act, "resolve": next.Actions["resolve"], "reopen": next.Actions["reopen"], "remove": next.Actions["remove"]}
	cur = f.mustPatchDraft(f.alice, f.shared, f.view(f.alice, f.shared, f.app), next)
	expect(t, f.tryPublish(f.alice, f.shared, cur, SchemaChange{ChangeFieldRemoved, "notes"}), http.StatusOK)
	v := only(t, f.effects(f.alice, ""), EffectCreateCard)
	if v.Detail.Title != "" || v.Result["title"] != nil {
		t.Fatalf("card effect still says what the removed field held: %+v", v)
	}
	if v.Status != StateDone || v.Result["card_id"] == nil {
		t.Fatalf("what isn't made of the field is still told: %+v", v)
	}
}

func TestACheckedTargetIsCheckedForEveryPermissionNeeded(t *testing.T) {
	f := newFX(t)
	look := Action{Kind: ActionUpdate, Effects: []Effect{{Kind: EffectOpenRoute, Route: RouteBoard, SpaceID: f.maint.ID, BoardID: f.board}}}
	d := f.inspections()
	submit := d.Actions["submit_inspection"]
	submit.Effects = submit.Effects[:1]
	d.Actions = map[string]Action{"a_look": look, "submit_inspection": submit}
	if err := f.e.s.checkEffectTargets(f.ctx(), f.shared.WorkspaceID, Actor{UserID: f.alice.id}, d); err != nil {
		t.Fatalf("an editor of the target: %v", err)
	}
	if err := f.e.s.checkEffectTargets(f.ctx(), f.shared.WorkspaceID, Actor{UserID: f.bob.id}, d); err == nil {
		t.Fatal("a viewer was let to point an effect that writes at the target, because a route reads it")
	}
}

func TestRollbackChecksTheReleasesTargetsLikePublish(t *testing.T) {
	f := newFX(t)
	plain := f.inspections()
	plain.Actions = map[string]Action{"submit_inspection": {Kind: ActionInsert}}
	cur := f.mustPatchDraft(f.alice, f.shared, f.view(f.alice, f.shared, f.app), plain)
	cur = f.publish(f.alice, f.shared, cur)
	expect(t, f.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", f.ws, f.maint.ID, f.alice.id), nil), http.StatusNoContent)
	rec := f.alice.do(http.MethodPost, appPath(f.shared, f.app.ID, "/rollback"), map[string]any{"version": cur.Version, "release": 1})
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "app_effect_target" {
		t.Fatalf("rollback to effects she can't target: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAnOperationIsForOneRelease(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-release-01", "East gate", "fail")
	cur := f.mustPatchDraft(f.alice, f.shared, f.view(f.alice, f.shared, f.app), withTitle(f.inspections(), "Urgent: {{site}}"))
	f.publish(f.alice, f.shared, cur)
	rec := f.submit(f.alice, "op-release-01", "East gate", "fail")
	expect(t, rec, http.StatusConflict)
	if errorCode(t, rec) != "app_operation_reused" {
		t.Fatalf("code = %s", errorCode(t, rec))
	}
}

func TestARetryRespectsTheQueueBound(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-bound-0001", "East gate", "fail")
	expect(t, f.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", f.ws, f.maint.ID, f.alice.id), nil), http.StatusNoContent)
	f.work()
	failed := f.effects(f.alice, "?status=failed")
	if len(failed.Effects) != 3 {
		t.Fatalf("failed = %+v", failed)
	}
	f.e.addMember(f.owner, f.maint, f.alice, access.RoleEditor)
	for i := 0; i < maxOutstanding; i++ {
		if _, err := f.e.db.Exec(`INSERT INTO app_operations (app_id, user_id, operation, action, request_hash) VALUES (?, ?, ?, 'x', 'x')`,
			f.app.ID, f.alice.id, fmt.Sprintf("filler-%05d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.e.db.Exec(`INSERT INTO app_effects (app_id, operation_id, position, kind, record_id, record_revision, user_id, action_kind, payload, next_attempt_at)
		SELECT ?, o.id, 0, 'open_route', ?, 1, ?, 'insert', '{}', '9999-01-01T00:00:00.000Z' FROM app_operations o WHERE o.operation LIKE 'filler-%'`, f.app.ID, f.next, f.alice.id); err != nil {
		t.Fatal(err)
	}
	rec := f.alice.do(http.MethodPost, appPath(f.shared, f.app.ID, fmt.Sprintf("/effects/%d/retry", failed.Effects[0].ID)), struct{}{})
	expect(t, rec, http.StatusConflict)
	if errorCode(t, rec) != "app_effects_backlog" {
		t.Fatalf("code = %s", errorCode(t, rec))
	}
}

func TestAnExpiredLeaseAfterTheLastTryIsNotAnotherTry(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-lease-0001", "East gate", "fail")
	if _, err := f.e.db.Exec(`UPDATE app_effects SET state = 'running', attempts = ?, lease_until = '2000-01-01T00:00:00.000Z' WHERE kind = 'create_card'`, maxAttempts); err != nil {
		t.Fatal(err)
	}
	f.work()
	if n := len(f.cards()); n != 0 {
		t.Fatalf("%d cards: the entry was tried more than %d times", n, maxAttempts)
	}
	v := only(t, f.effects(f.alice, ""), EffectCreateCard)
	if v.Status != StateFailed || v.Error == nil || v.Error.Code != "app_effect_gave_up" || v.Attempts != maxAttempts {
		t.Fatalf("card effect = %+v", v)
	}
}

func TestCleanupJudgesAnEntryByItsOwnLastActivity(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-fresh-0001", "East gate", "fail")
	expect(t, f.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", f.ws, f.maint.ID, f.alice.id), nil), http.StatusNoContent)
	f.work()
	failed := f.effects(f.alice, "?status=failed")
	if len(failed.Effects) != 3 {
		t.Fatalf("failed = %+v", failed)
	}
	f.e.addMember(f.owner, f.maint, f.alice, access.RoleEditor)
	f.later(keepAll - 24*time.Hour)
	expect(t, f.alice.do(http.MethodPost, appPath(f.shared, f.app.ID, fmt.Sprintf("/effects/%d/retry", only(t, failed, EffectCreateCard).ID)), struct{}{}), http.StatusOK)
	f.later(2 * 24 * time.Hour)
	f.purge()
	if n := f.count(`SELECT count(*) FROM app_effects WHERE state = 'pending'`); n != 1 {
		t.Fatalf("%d retried entries kept", n)
	}
	f.work()
	if v := f.effects(f.alice, "?status=done"); len(v.Effects) != 1 || v.Effects[0].Kind != EffectCreateCard {
		t.Fatalf("done = %+v", v)
	}
}

func TestEffectsArePagedPastTheFirstPage(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-paged-0001", "East gate", "fail")
	f.mustSubmit(f.alice, "op-paged-0002", "West gate", "fail")
	first := f.effects(f.alice, "?limit=4")
	if len(first.Effects) != 4 || !first.More {
		t.Fatalf("first page = %+v", first)
	}
	rest := f.effects(f.alice, fmt.Sprintf("?limit=4&before=%d", first.Effects[3].ID))
	if len(rest.Effects) != 2 || rest.More {
		t.Fatalf("second page = %+v", rest)
	}
	seen := map[int64]bool{}
	for _, v := range append(first.Effects, rest.Effects...) {
		seen[v.ID] = true
	}
	if len(seen) != 6 {
		t.Fatalf("pages overlap or miss: %v", seen)
	}
}
