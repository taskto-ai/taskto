package apps

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/knowledge"
)

// Tests of what a second review found: authority when the write lock is waited for, a crash after
// the commit, what an error says, a replayed request, and a stale attempt.

func TestADocumentWaitingForTheWriteLockIsNotMadeAfterARevocation(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-lock-00001", "East gate", "fail")
	by := knowledge.Author{Name: "Owner", Email: "owner@example.test"}

	// Someone else holds the Maintenance space's write lock.
	held, release := make(chan struct{}), make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_, _ = f.docs.PutIf(f.ctx(), f.maint, "other.md", []byte("x"), "", by, "x", func() error {
			close(held)
			<-release
			return errors.New("not made")
		})
	}()
	<-held

	// The worker passes its checks and waits for the lock…
	var once sync.Once
	reached := make(chan struct{})
	f.e.s.hooks.beforeDocument = func(effectRow) { once.Do(func() { close(reached) }) }
	worked := make(chan struct{})
	go func() {
		defer close(worked)
		f.work()
	}()
	<-reached
	// …alice's right to write the space ends meanwhile, and then the lock is released.
	if _, err := f.e.db.Exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, f.maint.ID, f.alice.id); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-stopped
	<-worked

	if d, err := f.docs.Read(f.ctx(), f.maint, f.east(), ""); err == nil {
		t.Fatalf("a document was made for a user who lost the space while it waited: %q", d.Content)
	}
	v := only(t, f.effects(f.owner, ""), EffectCreateDocument)
	if v.Status != StateFailed || v.Error == nil || (v.Error.Code != "app_effect_target_access" && v.Error.Code != "app_effect_target_gone") {
		t.Fatalf("document effect = %+v", v)
	}
}

// crashAfterDocument makes the worker stop as if the server died right after the document's commit.
func (f *fx) crashAfterDocument() {
	f.e.s.hooks.afterWork = func(e effectRow) error {
		if e.kind == EffectCreateDocument {
			return errCrash
		}
		return nil
	}
	f.work()
	f.e.s.hooks.afterWork = nil
}

func (f *fx) commits() int {
	f.e.t.Helper()
	h, err := f.docs.History(f.ctx(), f.maint, "", "", 100)
	if err != nil {
		f.e.t.Fatal(err)
	}
	return len(h.Commits)
}

var editor = knowledge.Author{Name: "Alice", Email: "alice@example.test"}

func TestADocumentEditedAfterACrashIsStillDoneAndNotMadeAgain(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-crash-edit1", "East gate", "fail")
	f.crashAfterDocument()
	doc, err := f.docs.Read(f.ctx(), f.maint, f.east(), "")
	if err != nil {
		t.Fatalf("the document was not made: %v", err)
	}
	if _, err := f.docs.Put(f.ctx(), f.maint, f.east(), []byte("# edited by a person\n"), doc.Version, editor, "edit"); err != nil {
		t.Fatal(err)
	}
	before := f.commits()

	f.later(leaseTime + time.Second)
	f.work()
	v := only(t, f.effects(f.alice, ""), EffectCreateDocument)
	if v.Status != StateDone || v.Result["path"] != f.east() {
		t.Fatalf("document effect = %+v, want done at %s", v, f.east())
	}
	if got := f.commits(); got != before {
		t.Fatalf("%d commits, was %d: the retry wrote again", got, before)
	}
	if d, _ := f.docs.Read(f.ctx(), f.maint, f.east(), ""); string(d.Content) != "# edited by a person\n" {
		t.Fatalf("the person's edit was lost: %q", d.Content)
	}
}

func TestADocumentMovedAfterACrashIsStillDoneAndNotMadeASecondTime(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-crash-move1", "East gate", "fail")
	f.crashAfterDocument()
	if _, err := f.docs.Move(f.ctx(), f.maint, f.east(), "archive/east.md", "", editor, "move"); err != nil {
		t.Fatal(err)
	}
	before := f.commits()

	f.later(leaseTime + time.Second)
	f.work()
	v := only(t, f.effects(f.alice, ""), EffectCreateDocument)
	if v.Status != StateDone {
		t.Fatalf("document effect = %+v", v)
	}
	if _, err := f.docs.Read(f.ctx(), f.maint, f.east(), ""); err == nil {
		t.Fatal("a second copy was made at the original path")
	}
	if got := f.commits(); got != before {
		t.Fatalf("%d commits, was %d", got, before)
	}
}

func TestAnErrorsParamsDoNotKeepWhatARemovedFieldHeld(t *testing.T) {
	f := newFX(t)
	// The document's path is made of the notes.
	d := f.inspections()
	act := d.Actions["submit_inspection"]
	act.Effects = slices.Clone(act.Effects)
	act.Effects[1].Path = "inspections/{{id}}-{{notes}}.md"
	d.Actions = map[string]Action{"submit_inspection": act, "resolve": d.Actions["resolve"], "reopen": d.Actions["reopen"], "remove": d.Actions["remove"]}
	cur := f.mustPatchDraft(f.alice, f.shared, f.view(f.alice, f.shared, f.app), d)
	f.publish(f.alice, f.shared, cur)
	taken := fmt.Sprintf("inspections/%d-leaking.md", f.next)
	if _, err := f.docs.Put(f.ctx(), f.maint, taken, []byte("someone else's"), "", editor, "x"); err != nil {
		t.Fatal(err)
	}
	f.mustSubmit(f.alice, "op-params-001", "East gate", "fail")
	f.work()
	v := only(t, f.effects(f.alice, ""), EffectCreateDocument)
	if v.Error == nil || v.Error.Code != "app_effect_doc_exists" || v.Error.Params["path"] != taken {
		t.Fatalf("document effect = %+v", v)
	}

	// The next release no longer declares notes.
	next := without(d, "notes")
	nact := next.Actions["submit_inspection"]
	nact.Effects = slices.Clone(act.Effects)
	nact.Effects[0].Body = ""
	nact.Effects[1].Path = "inspections/{{id}}.md"
	next.Actions = map[string]Action{"submit_inspection": nact, "resolve": next.Actions["resolve"], "reopen": next.Actions["reopen"], "remove": next.Actions["remove"]}
	cur = f.mustPatchDraft(f.alice, f.shared, f.view(f.alice, f.shared, f.app), next)
	expect(t, f.tryPublish(f.alice, f.shared, cur, SchemaChange{ChangeFieldRemoved, "notes"}), http.StatusOK)

	v = only(t, f.effects(f.alice, ""), EffectCreateDocument)
	if v.Error == nil || v.Error.Code != "app_effect_doc_exists" {
		t.Fatalf("the error is still told: %+v", v)
	}
	if v.Error.Params["path"] != nil || v.Detail.Path != "" {
		t.Fatalf("the removed field's value is still told: %+v", v)
	}
}

func TestACommittedRequestIsAnsweredAsBeforeEvenIfTheLiveReleaseCantTakeItNow(t *testing.T) {
	f := newFX(t)
	one := int64(1)
	press := func(operation string) *httptest.ResponseRecorder {
		a := action(f.app, "submit_inspection", map[string]any{"site": "East gate", "result": "fail", "notes": "leaking"})
		a.Operation = operation
		a.Release = &one
		return f.alice.do(http.MethodPost, appPath(f.shared, f.app.ID, "/actions"), a)
	}
	expect(t, press("op-replay-0001"), http.StatusOK)
	rows := f.count(`SELECT count(*) FROM app_rows`)
	queued := f.count(`SELECT count(*) FROM app_effects`)

	// A release that drops the notes field: an action of release 1 sets it, so the live release
	// can't take a new one from release 1.
	d := without(f.inspections(), "notes")
	act := d.Actions["submit_inspection"]
	act.Effects = slices.Clone(act.Effects)
	act.Effects[0].Body = ""
	d.Actions = map[string]Action{"submit_inspection": act, "resolve": d.Actions["resolve"], "reopen": d.Actions["reopen"], "remove": d.Actions["remove"]}
	cur := f.mustPatchDraft(f.alice, f.shared, f.view(f.alice, f.shared, f.app), d)
	expect(t, f.tryPublish(f.alice, f.shared, cur, SchemaChange{ChangeFieldRemoved, "notes"}), http.StatusOK)

	// The same release, key and body again (the first answer was lost): answered, nothing more made.
	expect(t, press("op-replay-0001"), http.StatusOK)
	if got := f.count(`SELECT count(*) FROM app_rows`); got != rows {
		t.Fatalf("%d records, was %d", got, rows)
	}
	if got := f.count(`SELECT count(*) FROM app_effects`); got != queued {
		t.Fatalf("%d effects, was %d", got, queued)
	}
	// A request not yet made is still refused.
	rec := press("op-replay-0002")
	expect(t, rec, http.StatusConflict)
	if errorCode(t, rec) != "app_release_outdated" {
		t.Fatalf("code = %s", errorCode(t, rec))
	}
}

func TestAStaleAttemptCannotCompleteOrFailAManualRetry(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-stale-0001", "East gate", "fail")
	first, found, err := f.e.s.claim(f.ctx())
	if err != nil || !found {
		t.Fatalf("claim: %v %v", found, err)
	}
	// The first attempt fails for good, and its user retries it: the attempts count from 1 again.
	if err := f.e.s.fail(f.ctx(), first, refuse("app_effect_doc_exists")); err != nil {
		t.Fatal(err)
	}
	expect(t, f.alice.do(http.MethodPost, appPath(f.shared, f.app.ID, fmt.Sprintf("/effects/%d/retry", first.id)), struct{}{}), http.StatusOK)
	second, found, err := f.e.s.claim(f.ctx())
	if err != nil || !found || second.id != first.id || second.attempts != first.attempts {
		t.Fatalf("second claim = %+v, %v, %v", second, found, err)
	}

	// The first attempt's worker, slow, answers now: it holds nothing.
	if err := f.e.s.succeed(f.ctx(), first, map[string]any{"card_id": 1}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.s.fail(f.ctx(), first, refuse("app_effect_invalid")); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := f.e.db.QueryRow(`SELECT state FROM app_effects WHERE id = ?`, first.id).Scan(&state); err != nil || state != StateRunning {
		t.Fatalf("state = %q, %v: a stale attempt ended the retry", state, err)
	}
	// The current one still can.
	if err := f.e.s.succeed(f.ctx(), second, map[string]any{"card_id": 2}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.db.QueryRow(`SELECT state FROM app_effects WHERE id = ?`, first.id).Scan(&state); err != nil || state != StateDone {
		t.Fatalf("state = %q, %v", state, err)
	}
}
