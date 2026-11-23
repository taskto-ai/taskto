package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/boards"
	"taskto.ai/taskto/internal/knowledge"
)

// fx is a world with an inspections app in the Team space whose actions set effects going on a
// maintenance board and in the Maintenance space's knowledge base:
//
//   - alice: editor of Team and of Maintenance, builds and publishes the app;
//   - owner: admin and editor of both;
//   - bob: viewer of both;
//   - mallory: in neither.
type fx struct {
	*world
	maint  access.Space
	board  int64
	boards *boards.Service
	docs   *knowledge.Store
	app    App
	// skew is how far the worker's clock is ahead of the real one.
	skew time.Duration
	// next is the ID the app's first record gets.
	next int64
}

// east is the path of the document the first inspection of East gate makes.
func (f *fx) east() string { return fmt.Sprintf("inspections/%d-east-gate.md", f.next) }

func (f *fx) ctx() context.Context { return context.Background() }

func newFX(t *testing.T) *fx {
	t.Helper()
	w := newWorld(t)
	f := &fx{world: w}
	f.maint = w.e.createShared(w.owner, w.ws, "Maintenance")
	w.e.addMember(w.owner, f.maint, w.alice, access.RoleEditor)
	w.e.addMember(w.owner, f.maint, w.bob, access.RoleViewer)
	if err := w.e.db.QueryRow(`INSERT INTO boards (space_id, kind, name, created_by, updated_by) VALUES (?, 'team', 'Repairs', ?, ?) RETURNING id`,
		f.maint.ID, w.owner.id, w.owner.id).Scan(&f.board); err != nil {
		t.Fatal(err)
	}
	f.boards = boards.New(w.e.db, w.e.s.access)
	f.docs = knowledge.New(filepath.Join(t.TempDir(), "kb"), knowledge.Options{})
	w.e.s.WithPlatform(Platform{Boards: f.boards, Docs: f.docs})
	w.e.s.clock = func() time.Time { return time.Now().Add(f.skew) }
	f.app = f.publishApp(w.alice, f.inspections())
	if err := w.e.db.QueryRow(`SELECT coalesce(max(id), 0) + 1 FROM app_rows`).Scan(&f.next); err != nil {
		t.Fatal(err)
	}
	return f
}

// inspections is the app: a failed inspection makes a maintenance card, a document and a route to
// the card; resolving one moves the card to done.
func (f *fx) inspections() Definition {
	failed := &Rule{Field: "result", Op: OpEq, Value: "fail"}
	comp := func(s string) json.RawMessage { return json.RawMessage(s) }
	return Definition{
		Components: []json.RawMessage{
			comp(`{"id":"root","component":"Column","children":["title","submit"]}`),
			comp(`{"id":"title","component":"Text","text":"Inspections"}`),
			comp(`{"id":"submit-label","component":"Text","text":"Submit"}`),
			comp(`{"id":"submit","component":"Button","child":"submit-label","action":{"event":{"name":"submit_inspection","context":{
				"site":{"path":"/draft/site"},"result":{"path":"/draft/result"},"notes":{"path":"/draft/notes"}}}}}`),
		},
		Collection: Collection{Fields: []Field{
			{Name: "site", Type: TypeString, Required: true, MaxLength: 100},
			{Name: "result", Type: TypeChoice, Required: true, Options: []string{"pass", "fail"}},
			{Name: "notes", Type: TypeString},
			{Name: "state", Type: TypeChoice, Options: []string{"open", "resolved"}},
		}},
		Actions: map[string]Action{
			"submit_inspection": {Kind: ActionInsert, Effects: []Effect{
				{Kind: EffectCreateCard, Name: "maintenance", When: failed, SpaceID: f.maint.ID, BoardID: f.board, Column: "todo",
					Title: "Fix {{site}} (inspection {{id}})", Body: "Notes: {{notes}}"},
				{Kind: EffectCreateDocument, When: failed, SpaceID: f.maint.ID, Path: "inspections/{{id}}-{{site}}.md",
					Content: "# Inspection {{id}}\n\nSite: {{site}}\nResult: {{result}}\n"},
				{Kind: EffectOpenRoute, When: failed, Route: RouteCard, Card: "maintenance"},
			}},
			"resolve": {Kind: ActionUpdate, Effects: []Effect{{Kind: EffectUpdateCard, Card: "maintenance", Column: "done"}}},
		},
	}
}

func (f *fx) publishApp(u *user, def Definition) App {
	f.e.t.Helper()
	rec := u.do(http.MethodPost, appsPath(f.shared, ""), map[string]any{"name": "Inspections", "definition": def})
	expect(f.e.t, rec, http.StatusCreated)
	return f.publish(u, f.shared, decode[App](f.e.t, rec))
}

// submit presses the submit button as u, under operation.
func (f *fx) submit(u *user, operation, site, result string) *httptest.ResponseRecorder {
	f.e.t.Helper()
	a := action(f.app, "submit_inspection", map[string]any{"site": site, "result": result, "notes": "leaking"})
	a.Operation = operation
	return u.do(http.MethodPost, appPath(f.shared, f.app.ID, "/actions"), a)
}

func (f *fx) mustSubmit(u *user, operation, site, result string) {
	f.e.t.Helper()
	expect(f.e.t, f.submit(u, operation, site, result), http.StatusOK)
}

// work does every entry that is due, as the server's worker would.
func (f *fx) work() { f.e.s.drain(f.ctx()) }

// later moves the worker's clock on.
func (f *fx) later(d time.Duration) { f.skew += d }

type cardRow struct {
	ID            int64
	Title, Column string
}

func (f *fx) cards() []cardRow {
	f.e.t.Helper()
	rows, err := f.e.db.Query(`SELECT id, title, col FROM cards WHERE board_id = ? ORDER BY id`, f.board)
	if err != nil {
		f.e.t.Fatal(err)
	}
	defer rows.Close()
	var out []cardRow
	for rows.Next() {
		var c cardRow
		if err := rows.Scan(&c.ID, &c.Title, &c.Column); err != nil {
			f.e.t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func (f *fx) count(query string, args ...any) int {
	f.e.t.Helper()
	var n int
	if err := f.e.db.QueryRow(query, args...).Scan(&n); err != nil {
		f.e.t.Fatal(err)
	}
	return n
}

func (f *fx) effects(u *user, query string) EffectList {
	f.e.t.Helper()
	rec := u.do(http.MethodGet, appPath(f.shared, f.app.ID, "/effects"+query), nil)
	expect(f.e.t, rec, http.StatusOK)
	return decode[EffectList](f.e.t, rec)
}

// only returns the one effect of a kind of a list.
func only(t *testing.T, l EffectList, kind string) EffectView {
	t.Helper()
	var found []EffectView
	for _, v := range l.Effects {
		if v.Kind == kind {
			found = append(found, v)
		}
	}
	if len(found) != 1 {
		t.Fatalf("effects of kind %s = %+v, want exactly one", kind, found)
	}
	return found[0]
}

func TestFailedInspectionMakesOneCardWhateverIsRepeated(t *testing.T) {
	f := newFX(t)
	// A passing inspection sets nothing going.
	f.mustSubmit(f.alice, "op-pass-0001", "North gate", "pass")
	f.next++ // the record the passing inspection made
	if n := f.count(`SELECT count(*) FROM app_effects`); n != 0 {
		t.Fatalf("a passing inspection queued %d effects", n)
	}

	f.mustSubmit(f.alice, "op-fail-0001", "East gate", "fail")
	// The client never saw the answer: it sends the request again, twice.
	f.mustSubmit(f.alice, "op-fail-0001", "East gate", "fail")
	f.mustSubmit(f.alice, "op-fail-0001", "East gate", "fail")
	if n := f.count(`SELECT count(*) FROM app_rows WHERE app_id = ? AND data LIKE '%East gate%'`, f.app.ID); n != 1 {
		t.Fatalf("repeating the request made %d records", n)
	}
	l := f.effects(f.alice, "")
	if len(l.Effects) != 3 || l.Active != 3 {
		t.Fatalf("effects = %+v", l)
	}
	for _, v := range l.Effects {
		if v.Status != StatePending || v.Operation != "op-fail-0001" {
			t.Fatalf("effect = %+v", v)
		}
	}

	// The worker runs, and runs again, and the entries are never redone.
	f.work()
	f.work()
	cards := f.cards()
	if len(cards) != 1 || cards[0].Title != fmt.Sprintf("Fix East gate (inspection %d)", f.next) || cards[0].Column != "todo" {
		t.Fatalf("cards = %+v", cards)
	}
	l = f.effects(f.alice, "")
	if l.Active != 0 || l.Failed != 0 {
		t.Fatalf("after the worker: %+v", l)
	}
	card := only(t, l, EffectCreateCard)
	if card.Status != StateDone || card.Result["card_id"] != float64(cards[0].ID) || card.Result["board_id"] != float64(f.board) {
		t.Fatalf("card effect = %+v", card)
	}
	doc := only(t, l, EffectCreateDocument)
	if doc.Status != StateDone || doc.Result["path"] != f.east() {
		t.Fatalf("document effect = %+v", doc)
	}
	d, err := f.docs.Read(f.ctx(), f.maint, f.east(), "")
	if err != nil || string(d.Content) != fmt.Sprintf("# Inspection %d\n\nSite: East gate\nResult: fail\n", f.next) {
		t.Fatalf("document = %q, %v", d.Content, err)
	}
	route := only(t, l, EffectOpenRoute)
	r, _ := route.Result["route"].(map[string]any)
	if route.Status != StateDone || r["kind"] != RouteCard || r["card_id"] != float64(cards[0].ID) || r["space_id"] != float64(f.maint.ID) {
		t.Fatalf("route effect = %+v", route)
	}

	// The same key for another request is refused, and changes nothing.
	rec := f.submit(f.alice, "op-fail-0001", "West gate", "fail")
	expect(t, rec, http.StatusConflict)
	if errorCode(t, rec) != "app_operation_reused" {
		t.Fatalf("code = %s", errorCode(t, rec))
	}
	// A key that isn't one is refused.
	rec = f.submit(f.alice, "short", "West gate", "fail")
	expect(t, rec, http.StatusUnprocessableEntity)
	if errorCode(t, rec) != "app_operation_invalid" {
		t.Fatalf("code = %s", errorCode(t, rec))
	}
	f.work()
	if n := len(f.cards()); n != 1 {
		t.Fatalf("%d cards", n)
	}
}

func TestARequestWithoutAnOperationIsGivenOne(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "", "East gate", "fail")
	l := f.effects(f.alice, "")
	if len(l.Effects) != 3 || !strings.HasPrefix(l.Effects[0].Operation, "srv-") {
		t.Fatalf("effects = %+v", l)
	}
	for _, v := range l.Effects {
		if v.Operation != l.Effects[0].Operation {
			t.Fatalf("operations differ: %+v", l.Effects)
		}
	}
}

func TestACrashBetweenMakingTheCardAndNotingItMakesNoSecondCard(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-crash-001", "East gate", "fail")

	// The server dies right after the card is made, before the entry says so.
	f.e.s.hooks.afterWork = func(e effectRow) error {
		if e.kind == EffectCreateCard {
			return errCrash
		}
		return nil
	}
	f.work()
	if n := len(f.cards()); n != 1 {
		t.Fatalf("%d cards after the crash", n)
	}
	if got := f.count(`SELECT count(*) FROM app_effects WHERE state = 'running'`); got != 1 {
		t.Fatalf("%d running", got)
	}
	// Until the dead worker's lease runs out nobody else takes it: the card is not made twice
	// meanwhile either.
	f.e.s.hooks.afterWork = nil
	f.work()
	if got := f.count(`SELECT count(*) FROM app_effects WHERE state = 'running'`); got != 1 {
		t.Fatalf("the entry was taken before its lease ran out")
	}

	f.later(leaseTime + time.Second)
	f.work()
	cards := f.cards()
	if len(cards) != 1 {
		t.Fatalf("cards = %+v: the entry made a second card", cards)
	}
	l := f.effects(f.alice, "")
	card := only(t, l, EffectCreateCard)
	if card.Status != StateDone || card.Result["card_id"] != float64(cards[0].ID) {
		t.Fatalf("card effect = %+v", card)
	}
	if l.Active != 0 || l.Failed != 0 {
		t.Fatalf("list = %+v", l)
	}
}

func TestADocumentIsNotMadeTwiceAfterACrashEither(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-crash-002", "East gate", "fail")
	f.e.s.hooks.afterWork = func(e effectRow) error {
		if e.kind == EffectCreateDocument {
			return errCrash
		}
		return nil
	}
	f.work()
	f.e.s.hooks.afterWork = nil
	f.later(leaseTime + time.Second)
	f.work()
	l := f.effects(f.alice, "")
	if doc := only(t, l, EffectCreateDocument); doc.Status != StateDone {
		t.Fatalf("document effect = %+v", doc)
	}
	if h, err := f.docs.History(f.ctx(), f.maint, f.east(), "", 10); err != nil || len(h.Commits) != 1 {
		t.Fatalf("history = %+v, %v", h, err)
	}
}

func TestAFailedTryIsRetriedWithBackoffAndMakesOneCard(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-retry-001", "East gate", "fail")
	// The first try makes the card and then fails for a reason that passes (a timeout, say).
	tries := 0
	f.e.s.hooks.afterWork = func(e effectRow) error {
		if e.kind == EffectCreateCard {
			if tries++; tries == 1 {
				return errors.New("the connection dropped")
			}
		}
		return nil
	}
	f.work()
	l := f.effects(f.alice, "")
	card := only(t, l, EffectCreateCard)
	if card.Status != StatePending || card.Attempts != 1 || card.NextAttemptAt == nil || card.Error == nil || card.Error.Code != "app_effect_gave_up" {
		t.Fatalf("after the first try: %+v", card)
	}
	// Not before the wait is over.
	f.work()
	if tries != 1 {
		t.Fatalf("tried again before the wait was over")
	}
	f.later(backoffBase + time.Second)
	f.work()
	if n := len(f.cards()); n != 1 {
		t.Fatalf("%d cards", n)
	}
	if card = only(t, f.effects(f.alice, ""), EffectCreateCard); card.Status != StateDone || card.Attempts != 2 || card.Error != nil {
		t.Fatalf("after the retry: %+v", card)
	}
}

func TestAnEntryThatKeepsFailingFailsForGoodAndIsRetriedOnPurpose(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-giveup-01", "East gate", "fail")
	broken := true
	f.e.s.hooks.afterWork = func(e effectRow) error {
		if e.kind == EffectCreateCard && broken {
			return errors.New("the disk is full")
		}
		return nil
	}
	for range maxAttempts + 2 {
		f.work()
		f.later(backoffMax + time.Second)
	}
	l := f.effects(f.alice, "")
	card := only(t, l, EffectCreateCard)
	if card.Status != StateFailed || card.Attempts != maxAttempts || card.Error == nil || card.Error.Code != "app_effect_gave_up" || !card.CanRetry || l.Failed != 1 {
		t.Fatalf("card effect = %+v, list %+v", card, l)
	}
	// The card was made before the failure was noted, and the record's link knows it: the route
	// to it is done, not failed for a card nobody noted.
	if route := only(t, l, EffectOpenRoute); route.Status != StateDone {
		t.Fatalf("route effect = %+v", route)
	}
	// Nothing tries a failed entry again by itself.
	f.work()
	if card = only(t, f.effects(f.alice, ""), EffectCreateCard); card.Attempts != maxAttempts {
		t.Fatalf("attempts = %d", card.Attempts)
	}
	if n := len(f.cards()); n != 1 {
		t.Fatalf("%d cards (made before the failure noted)", n)
	}

	// Only the user who acted may retry it; someone else who sees it is refused.
	other := f.effects(f.owner, "")
	if v := only(t, other, EffectCreateCard); v.CanRetry {
		t.Fatalf("another user can retry: %+v", v)
	}
	expectForbidden(t, f.owner.do(http.MethodPost, appPath(f.shared, f.app.ID, fmt.Sprintf("/effects/%d/retry", card.ID)), struct{}{}), "retry_effect")
	// A viewer can't retry anything.
	expect(t, f.bob.do(http.MethodPost, appPath(f.shared, f.app.ID, fmt.Sprintf("/effects/%d/retry", card.ID)), struct{}{}), http.StatusForbidden)

	broken = false
	rec := f.alice.do(http.MethodPost, appPath(f.shared, f.app.ID, fmt.Sprintf("/effects/%d/retry", card.ID)), struct{}{})
	expect(t, rec, http.StatusOK)
	if v := decode[EffectView](t, rec); v.Status != StatePending || v.Attempts != 0 || v.Error != nil {
		t.Fatalf("retried = %+v", v)
	}
	// Retrying what isn't failed is refused.
	rec = f.alice.do(http.MethodPost, appPath(f.shared, f.app.ID, fmt.Sprintf("/effects/%d/retry", card.ID)), struct{}{})
	expect(t, rec, http.StatusConflict)
	if errorCode(t, rec) != "app_effect_not_failed" {
		t.Fatalf("code = %s", errorCode(t, rec))
	}
	f.work()
	cards := f.cards()
	if len(cards) != 1 {
		t.Fatalf("%d cards: the retry made another", len(cards))
	}
	if card = only(t, f.effects(f.alice, ""), EffectCreateCard); card.Status != StateDone || card.Result["card_id"] != float64(cards[0].ID) {
		t.Fatalf("after the retry: %+v", card)
	}
}

func TestRemovedAccessToTheAppStopsTheEffects(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-access-01", "East gate", "fail")
	// Alice leaves the space the app is in before the worker gets to it.
	expect(t, f.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", f.ws, f.shared.ID, f.alice.id), nil), http.StatusNoContent)
	f.work()
	if n := len(f.cards()); n != 0 {
		t.Fatalf("%d cards made for a user who lost the app", n)
	}
	l := f.effects(f.owner, "")
	if l.Failed != 3 {
		t.Fatalf("list = %+v", l)
	}
	for _, v := range l.Effects {
		if v.Status != StateFailed || v.Error == nil || v.Error.Code != "app_effect_app_access" {
			t.Fatalf("effect = %+v", v)
		}
	}
	if _, err := f.docs.Read(f.ctx(), f.maint, f.east(), ""); err == nil {
		t.Fatalf("a document was made")
	}
}

func TestRemovedRoleInTheAppStopsTheEffects(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.owner, "op-role-0001", "East gate", "fail")
	// Alice manages the roles: the owner is left a reader, who can't submit.
	f.w().mustSetRoles(f.alice, f.shared, f.app, grant(f.alice.id, access.AppBuilder, access.AppPublisher, access.AppDataManager, access.AppUser), grant(f.owner.id, access.AppReader))
	f.work()
	if n := len(f.cards()); n != 0 {
		t.Fatalf("%d cards made for a user who lost the right to submit", n)
	}
	for _, v := range f.effects(f.alice, "").Effects {
		if v.Status != StateFailed || v.Error.Code != "app_effect_app_access" {
			t.Fatalf("effect = %+v", v)
		}
	}
}

func (f *fx) w() *world { return f.world }

func TestRemovedAccessToTheTargetStopsTheEffectsUntilRestored(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-target-01", "East gate", "fail")
	// Alice still has the app, but loses the board's space (cross-space: both are needed).
	expect(t, f.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", f.ws, f.maint.ID, f.alice.id), nil), http.StatusNoContent)
	f.work()
	if n := len(f.cards()); n != 0 {
		t.Fatalf("%d cards made without access to the board", n)
	}
	l := f.effects(f.alice, "")
	if l.Failed != 3 {
		t.Fatalf("list = %+v", l)
	}
	card := only(t, l, EffectCreateCard)
	if card.Status != StateFailed || card.Error.Code != "app_effect_target_access" {
		t.Fatalf("card effect = %+v", card)
	}
	// Read-only access to the target is not enough to write it.
	f.e.addMember(f.owner, f.maint, f.alice, access.RoleViewer)
	rec := f.alice.do(http.MethodPost, appPath(f.shared, f.app.ID, fmt.Sprintf("/effects/%d/retry", card.ID)), struct{}{})
	expect(t, rec, http.StatusOK)
	f.work()
	if n := len(f.cards()); n != 0 {
		t.Fatalf("%d cards made with read access only", n)
	}
	// With the access back, a retry makes the card, once.
	expect(t, f.owner.do(http.MethodPatch, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", f.ws, f.maint.ID, f.alice.id), map[string]any{"role": access.RoleEditor}), http.StatusNoContent)
	rec = f.alice.do(http.MethodPost, appPath(f.shared, f.app.ID, fmt.Sprintf("/effects/%d/retry", card.ID)), struct{}{})
	expect(t, rec, http.StatusOK)
	f.work()
	f.work()
	if n := len(f.cards()); n != 1 {
		t.Fatalf("%d cards", n)
	}
}

func TestAnArchivedBoardRefusesTheCard(t *testing.T) {
	f := newFX(t)
	if _, err := f.e.db.Exec(`UPDATE boards SET archived_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`, f.board); err != nil {
		t.Fatal(err)
	}
	f.mustSubmit(f.alice, "op-archive-01", "East gate", "fail")
	f.work()
	if card := only(t, f.effects(f.alice, ""), EffectCreateCard); card.Status != StateFailed || card.Error.Code != "app_effect_board_offline" {
		t.Fatalf("card effect = %+v", card)
	}
	if len(f.cards()) != 0 {
		t.Fatalf("a card was made on an archived board")
	}
}

func TestUpdateCardMovesTheCardTheRecordMadeAndIsIdempotent(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-move-0001", "East gate", "fail")
	// Resolving straight away: the card is still to be made, and the move waits for it.
	resolve := func(op string) {
		a := action(f.app, "resolve", map[string]any{"id": f.next, "state": "resolved"})
		a.Operation = op
		expect(t, f.alice.do(http.MethodPost, appPath(f.shared, f.app.ID, "/actions"), a), http.StatusOK)
	}
	resolve("op-move-0002")
	f.work()
	cards := f.cards()
	if len(cards) != 1 || cards[0].Column != "done" {
		t.Fatalf("cards = %+v", cards)
	}
	l := f.effects(f.alice, "")
	if v := only(t, l, EffectUpdateCard); v.Status != StateDone || v.Result["column"] != "done" {
		t.Fatalf("update effect = %+v", v)
	}
	// A card moved back by someone is moved again by another resolve, and none is ever added.
	if _, err := f.e.db.Exec(`UPDATE cards SET col = 'doing', rank = 'a1' WHERE id = ?`, cards[0].ID); err != nil {
		t.Fatal(err)
	}
	expect(t, f.alice.do(http.MethodPost, appPath(f.shared, f.app.ID, "/actions"),
		func() ClientAction {
			a := action(f.app, "resolve", map[string]any{"id": f.next, "state": "open"})
			a.Operation = "op-move-0003"
			return a
		}()), http.StatusOK)
	resolve("op-move-0004")
	f.work()
	if cards = f.cards(); len(cards) != 1 || cards[0].Column != "done" {
		t.Fatalf("cards = %+v", cards)
	}

	// An archived card is not touched, and the entry says why.
	if _, err := f.e.db.Exec(`UPDATE cards SET col = 'archived', archived_from = 'done' WHERE id = ?`, cards[0].ID); err != nil {
		t.Fatal(err)
	}
	expect(t, f.alice.do(http.MethodPost, appPath(f.shared, f.app.ID, "/actions"),
		func() ClientAction {
			a := action(f.app, "resolve", map[string]any{"id": f.next, "state": "open"})
			a.Operation = "op-move-0005"
			return a
		}()), http.StatusOK)
	resolve("op-move-0006")
	f.work()
	failed := f.effects(f.alice, "?status=failed")
	if len(failed.Effects) != 2 || failed.Effects[0].Error.Code != "app_effect_card_archived" {
		t.Fatalf("failed = %+v", failed)
	}
}

func TestUpdateCardWithoutACardForTheRecordFails(t *testing.T) {
	f := newFX(t)
	// A record that passed has no maintenance card: resolving it has nothing to move.
	f.mustSubmit(f.alice, "op-nocard-01", "East gate", "pass")
	a := action(f.app, "resolve", map[string]any{"id": f.next, "state": "resolved"})
	a.Operation = "op-nocard-02"
	expect(t, f.alice.do(http.MethodPost, appPath(f.shared, f.app.ID, "/actions"), a), http.StatusOK)
	f.work()
	v := only(t, f.effects(f.alice, ""), EffectUpdateCard)
	if v.Status != StateFailed || v.Error.Code != "app_effect_no_card" {
		t.Fatalf("update effect = %+v", v)
	}
}

func TestADocumentOfAnotherIsNeverOverwritten(t *testing.T) {
	f := newFX(t)
	by := knowledge.Author{Name: "Owner", Email: "owner@example.com"}
	if _, err := f.docs.Put(f.ctx(), f.maint, f.east(), []byte("someone else's"), "", by, "x"); err != nil {
		t.Fatal(err)
	}
	f.mustSubmit(f.alice, "op-doc-00001", "East gate", "fail")
	f.work()
	v := only(t, f.effects(f.alice, ""), EffectCreateDocument)
	if v.Status != StateFailed || v.Error.Code != "app_effect_doc_exists" || v.Error.Params["path"] != f.east() {
		t.Fatalf("document effect = %+v", v)
	}
	if d, _ := f.docs.Read(f.ctx(), f.maint, f.east(), ""); string(d.Content) != "someone else's" {
		t.Fatalf("the document was overwritten: %q", d.Content)
	}
	// The card was made all the same: effects don't hold each other up.
	if len(f.cards()) != 1 {
		t.Fatalf("cards = %+v", f.cards())
	}
}

func TestWhatIsPutInATemplateIsNeverReadAsOne(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-tmpl-0001", "{{notes}} ../../etc/{{id}}", "fail")
	f.work()
	cards := f.cards()
	if len(cards) != 1 || cards[0].Title != fmt.Sprintf("Fix {{notes}} ../../etc/{{id}} (inspection %d)", f.next) {
		t.Fatalf("cards = %+v", cards)
	}
	// In a path the same text is made a safe name: no folder to climb out of.
	doc := only(t, f.effects(f.alice, ""), EffectCreateDocument)
	if doc.Status != StateDone || doc.Result["path"] != fmt.Sprintf("inspections/%d-notes-etc-id.md", f.next) {
		t.Fatalf("document effect = %+v", doc)
	}
}

func TestAPreviewSimulatesAndChangesNothing(t *testing.T) {
	f := newFX(t)
	preview := func(u *user, ctx map[string]any) *httptest.ResponseRecorder {
		return u.do(http.MethodPost, appPath(f.shared, f.app.ID, "/actions/preview"), action(f.app, "submit_inspection", ctx))
	}
	rec := preview(f.alice, map[string]any{"site": "East gate", "result": "fail", "notes": "x"})
	expect(t, rec, http.StatusOK)
	p := decode[Preview](t, rec)
	if len(p.Effects) != 3 {
		t.Fatalf("preview = %+v", p)
	}
	for _, v := range p.Effects {
		if v.Status != StateSimulated || v.Error != nil {
			t.Fatalf("effect = %+v", v)
		}
	}
	if p.Effects[0].Detail.Title != "Fix East gate (inspection new)" || p.Effects[1].Detail.Path != "inspections/new-east-gate.md" {
		t.Fatalf("details = %+v", p.Effects)
	}
	// A passing one sets nothing going.
	rec = preview(f.alice, map[string]any{"site": "East gate", "result": "pass"})
	expect(t, rec, http.StatusOK)
	if p = decode[Preview](t, rec); len(p.Effects) != 0 {
		t.Fatalf("preview = %+v", p)
	}
	f.work()
	for _, table := range []string{"cards", "app_effects", "app_operations"} {
		if n := f.count(`SELECT count(*) FROM ` + table + ` WHERE ` + map[string]string{"cards": "board_id = " + fmt.Sprint(f.board), "app_effects": "1", "app_operations": "1"}[table]); n != 0 {
			t.Fatalf("a preview left %d rows in %s", n, table)
		}
	}
	if n := f.count(`SELECT count(*) FROM app_rows WHERE app_id = ?`, f.app.ID); n != 0 {
		t.Fatalf("a preview made %d records", n)
	}
	if _, err := f.docs.Read(f.ctx(), f.maint, "inspections/new-east-gate.md", ""); err == nil {
		t.Fatal("a preview made a document")
	}

	// It is for those who may design or publish; the others are told no, and a stranger finds nothing.
	expect(t, preview(f.bob, map[string]any{"site": "x", "result": "fail"}), http.StatusForbidden)
	expect(t, preview(f.mallory, map[string]any{"site": "x", "result": "fail"}), http.StatusNotFound)
	// One whose target the previewer couldn't write says so.
	if _, err := f.e.db.Exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, f.maint.ID, f.alice.id); err != nil {
		t.Fatal(err)
	}
	p = decode[Preview](t, preview(f.alice, map[string]any{"site": "East gate", "result": "fail"}))
	if len(p.Effects) != 3 || p.Effects[0].Error == nil || p.Effects[0].Error.Code != "app_effect_target_access" {
		t.Fatalf("preview = %+v", p)
	}
}

func TestEffectsOfADraftAreNeverQueued(t *testing.T) {
	f := newFX(t)
	// A draft with other effects is not live: actions run as the live release says.
	cur := f.view(f.alice, f.shared, f.app)
	f.mustPatchDraft(f.alice, f.shared, cur, withTitle(f.inspections(), "DRAFT {{site}}"))
	f.mustSubmit(f.alice, "op-draft-0001", "East gate", "fail")
	f.work()
	cards := f.cards()
	if len(cards) != 1 || strings.Contains(cards[0].Title, "DRAFT") {
		t.Fatalf("cards = %+v: the draft's effects ran", cards)
	}
	// An app that was never published has no live release: its actions don't run at all, so
	// they queue nothing.
	d := f.draft(f.alice, f.shared, "Drafted")
	rec := f.alice.do(http.MethodPost, appPath(f.shared, d.ID, "/actions"), action(d, "add_expense", map[string]any{"description": "x", "amount": "1"}))
	expect(t, rec, http.StatusConflict)
	if n := f.count(`SELECT count(*) FROM app_operations WHERE app_id = ?`, d.ID); n != 0 {
		t.Fatalf("%d operations of a draft", n)
	}
}

func TestEffectsAreSeenByWhoeverReadsTheRecord(t *testing.T) {
	f := newFX(t)
	// The owner submits as a plain user (own records), alice reads and changes all.
	f.w().mustSetRoles(f.alice, f.shared, f.app, grant(f.alice.id, access.AppBuilder, access.AppPublisher, access.AppDataManager, access.AppUser), grant(f.owner.id, access.AppUser), grant(f.bob.id, access.AppReader))
	f.mustSubmit(f.owner, "op-own-00001", "East gate", "fail")
	f.mustSubmit(f.alice, "op-own-00002", "West gate", "fail")
	f.work()
	if n := len(f.cards()); n != 2 {
		t.Fatalf("%d cards", n)
	}
	if l := f.effects(f.alice, ""); len(l.Effects) != 6 {
		t.Fatalf("alice sees %d effects", len(l.Effects))
	}
	own := f.effects(f.owner, "")
	if len(own.Effects) != 3 {
		t.Fatalf("a user who reads their own records sees %d effects", len(own.Effects))
	}
	for _, v := range own.Effects {
		if v.Record != f.next {
			t.Fatalf("effect = %+v", v)
		}
	}
	// By record, and a record that isn't theirs is empty to them.
	rec := f.owner.do(http.MethodGet, appPath(f.shared, f.app.ID, fmt.Sprintf("/records/%d/effects", f.next+1)), nil)
	expect(t, rec, http.StatusOK)
	if got := decode[EffectList](t, rec); len(got.Effects) != 0 || got.Active != 0 || got.Failed != 0 {
		t.Fatalf("effects of another's record = %+v", got)
	}
	rec = f.owner.do(http.MethodGet, appPath(f.shared, f.app.ID, fmt.Sprintf("/records/%d/effects", f.next)), nil)
	expect(t, rec, http.StatusOK)
	if got := decode[EffectList](t, rec); len(got.Effects) != 3 {
		t.Fatalf("effects of own record = %+v", got)
	}
	// Strangers and people outside the space find nothing; a viewer of the space may read.
	expect(t, f.mallory.do(http.MethodGet, appPath(f.shared, f.app.ID, "/effects"), nil), http.StatusNotFound)
	expect(t, f.stranger.do(http.MethodGet, appPath(in(f.other, f.shared), f.app.ID, "/effects"), nil), http.StatusNotFound)
	expect(t, f.bob.do(http.MethodGet, appPath(f.shared, f.app.ID, "/effects"), nil), http.StatusOK)
	// And a retry from outside is not found either.
	expect(t, f.mallory.do(http.MethodPost, appPath(f.shared, f.app.ID, "/effects/1/retry"), struct{}{}), http.StatusNotFound)
	// What an effect made is told only to someone who may read where it is.
	if _, err := f.e.db.Exec(`UPDATE space_members SET role = 'viewer' WHERE space_id = ? AND user_id = ?`, f.maint.ID, f.owner.id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.db.Exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, f.maint.ID, f.owner.id); err != nil {
		t.Fatal(err)
	}
	for _, v := range f.effects(f.owner, "").Effects {
		if v.Result != nil {
			t.Fatalf("a user who can't read the target was told %+v", v.Result)
		}
	}
	for _, v := range f.effects(f.alice, "").Effects {
		if v.Result == nil {
			t.Fatalf("a user who can read the target wasn't told what was made: %+v", v)
		}
	}
}

func TestAnotherUsersOperationKeyIsTheirOwn(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-shared-001", "East gate", "fail")
	f.mustSubmit(f.owner, "op-shared-001", "West gate", "fail")
	f.work()
	if n := len(f.cards()); n != 2 {
		t.Fatalf("%d cards: one user's key held up another's", n)
	}
}

func TestDefinitionsNameOnlyWhatTheRegistryHas(t *testing.T) {
	f := newFX(t)
	good := f.inspections()
	with := func(e ...Effect) Definition {
		d := f.inspections()
		d.Actions = map[string]Action{"submit_inspection": {Kind: ActionInsert, Effects: e}}
		return d
	}
	card := Effect{Kind: EffectCreateCard, SpaceID: f.maint.ID, BoardID: f.board, Title: "x {{site}}"}
	if _, err := good.Check(); err != nil {
		t.Fatal(err)
	}
	checked, err := good.Check()
	if err != nil || !contains(checked.Requires, "effect.create_card") || !contains(checked.Requires, "effect.update_card") ||
		!contains(checked.Requires, "effect.create_document") || !contains(checked.Requires, "effect.open_route") {
		t.Fatalf("requires = %v, %v", checked.Requires, err)
	}
	for name, e := range map[string]Effect{
		"a tool name":             {Kind: "run_tool"},
		"a url":                   {Kind: "http_request"},
		"no kind":                 {},
		"an unknown route":        {Kind: EffectOpenRoute, Route: "https://example.com/"},
		"a field it doesn't take": {Kind: EffectCreateCard, SpaceID: 1, BoardID: 1, Title: "x", Path: "a.md"},
		"no board":                {Kind: EffectCreateCard, SpaceID: 1, Title: "x"},
		"no title":                {Kind: EffectCreateCard, SpaceID: 1, BoardID: 1},
		"a bad column":            {Kind: EffectCreateCard, SpaceID: 1, BoardID: 1, Title: "x", Column: "archived"},
		"an unknown field hole":   {Kind: EffectCreateCard, SpaceID: 1, BoardID: 1, Title: "x {{nope}}"},
		"an expression":           {Kind: EffectCreateCard, SpaceID: 1, BoardID: 1, Title: "x {{site + 1}}"},
		"a call":                  {Kind: EffectCreateCard, SpaceID: 1, BoardID: 1, Title: "{{upper(site)}}"},
		"an open brace":           {Kind: EffectCreateCard, SpaceID: 1, BoardID: 1, Title: "x {{site"},
		"a stray close":           {Kind: EffectCreateCard, SpaceID: 1, BoardID: 1, Title: "x }}"},
		"an empty hole":           {Kind: EffectCreateCard, SpaceID: 1, BoardID: 1, Title: "x {{}}"},
		"a nested hole":           {Kind: EffectCreateCard, SpaceID: 1, BoardID: 1, Title: "x {{{{site}}}}"},
		"a card nobody makes":     {Kind: EffectUpdateCard, Card: "nobody", Column: "done"},
		"a path out of the space": {Kind: EffectCreateDocument, SpaceID: 1, Path: "../x.md"},
		"an absolute path":        {Kind: EffectCreateDocument, SpaceID: 1, Path: "/x.md"},
		"a path not Markdown":     {Kind: EffectCreateDocument, SpaceID: 1, Path: "x.sh"},
		"a rule on a missing field": {Kind: EffectCreateCard, SpaceID: 1, BoardID: 1, Title: "x",
			When: &Rule{Field: "nope", Op: OpEq, Value: "x"}},
		"a route with another's field": {Kind: EffectOpenRoute, Route: RouteBoard, SpaceID: 1, BoardID: 1, Path: "x.md"},
	} {
		if _, err := with(card, e).Check(); err == nil || !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Check = %v, want an invalid definition", name, err)
		}
	}
	// Only an insert or an update has effects, and only version 2 has them.
	d := with(card)
	d.Actions["delete_it"] = Action{Kind: ActionDelete, Effects: []Effect{card}}
	if _, err := d.Check(); err == nil {
		t.Error("a delete has effects")
	}
	d = with(card)
	d.DefinitionVersion = 1
	if _, err := d.Check(); err == nil || !strings.Contains(err.Error(), "effects") {
		t.Errorf("version 1: %v", err)
	}
	// Names are unique, and too many effects are refused.
	d = with(Effect{Kind: EffectCreateCard, Name: "a", SpaceID: 1, BoardID: 1, Title: "x"}, Effect{Kind: EffectCreateCard, Name: "a", SpaceID: 1, BoardID: 1, Title: "x"})
	if _, err := d.Check(); err == nil {
		t.Error("two effects named a")
	}
	many := make([]Effect, maxEffectsPerAction+1)
	for i := range many {
		many[i] = card
	}
	if _, err := with(many...).Check(); err == nil {
		t.Error("too many effects")
	}
}


func TestAnEffectIsStrictAboutItsKeys(t *testing.T) {
	f := newFX(t)
	for _, extra := range []string{`"url": "https://example.com/hook"`, `"tool": "delete_doc"`, `"command": "rm -rf /"`, `"arguments": {}`} {
		body := fmt.Sprintf(`{"name": "Bad", "definition": {"components": [{"id":"root","component":"Text","text":"x"}], "collection": {"fields": []},
			"actions": {"go": {"kind": "insert", "effects": [{"kind": "create_card", "space_id": %d, "board_id": %d, "title": "x", %s}]}}}}`, f.maint.ID, f.board, extra)
		r := httptest.NewRequest(http.MethodPost, appsPath(f.shared, ""), strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.AddCookie(f.alice.cookie)
		rec := httptest.NewRecorder()
		f.e.h.ServeHTTP(rec, r)
		expect(t, rec, http.StatusBadRequest)
		if errorCode(t, rec) != "invalid_json" {
			t.Fatalf("%s: code = %s", extra, errorCode(t, rec))
		}
	}
}

func TestAnAppCantTargetWhatItsAuthorCantWrite(t *testing.T) {
	f := newFX(t)
	// Bob only reads the maintenance space, mallory is not in it: neither's draft may point at it.
	otherBoard := f.board + 100
	for name, c := range map[string]struct {
		space, board int64
		by           *user
	}{
		"a space the author only reads":  {f.maint.ID, f.board, f.bob},
		"a space the author is not in":   {f.maint.ID, f.board, f.mallory},
		"a space of another workspace":   {f.otherShared.ID, f.board, f.alice},
		"a board the space doesn't have": {f.maint.ID, otherBoard, f.alice},
	} {
		d := f.inspections()
		act := d.Actions["submit_inspection"]
		act.Effects = []Effect{{Kind: EffectCreateCard, SpaceID: c.space, BoardID: c.board, Title: "x"}}
		d.Actions = map[string]Action{"submit_inspection": act}
		// Bob and mallory can't write the shared space's apps at all, so they make their own.
		space := f.shared
		if c.by == f.bob || c.by == f.mallory {
			space = f.e.private(c.by, f.ws)
		}
		rec := c.by.do(http.MethodPost, appsPath(space, ""), map[string]any{"name": "Sneaky", "definition": d})
		expect(t, rec, http.StatusBadRequest)
		if errorCode(t, rec) != "app_effect_target" {
			t.Errorf("%s: code = %s", name, errorCode(t, rec))
		}
	}
	// Publishing checks again: a draft saved while the author could write the target is not
	// published by someone who can't.
	d := f.inspections()
	cur := f.view(f.alice, f.shared, f.app)
	cur = f.mustPatchDraft(f.alice, f.shared, cur, withTitle(d, "Fix {{site}} soon"))
	expect(t, f.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", f.ws, f.maint.ID, f.owner.id), nil), http.StatusNoContent)
	rec := f.owner.do(http.MethodPost, appPath(f.shared, f.app.ID, "/publish"), map[string]any{"version": cur.Version, "acknowledge": []SchemaChange{}})
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "app_effect_target" {
		t.Fatalf("publish by someone who can't write the target: %d %s", rec.Code, rec.Body.String())
	}
}

func withTitle(d Definition, title string) Definition {
	act := d.Actions["submit_inspection"]
	effects := append([]Effect(nil), act.Effects...)
	effects[0].Title = title
	act.Effects = effects
	d.Actions["submit_inspection"] = act
	return d
}

func TestAnOlderScreenRunsTheLiveReleasesEffects(t *testing.T) {
	f := newFX(t)
	// Publishing a release with other effects: an action of a screen drawn from release 1 is
	// either refused as outdated or run as the live release says, never as the old one did.
	d := withTitle(f.inspections(), "Urgent: {{site}}")
	cur := f.mustPatchDraft(f.alice, f.shared, f.view(f.alice, f.shared, f.app), d)
	f.publish(f.alice, f.shared, cur)
	a := action(f.app, "submit_inspection", map[string]any{"site": "East gate", "result": "fail"})
	a.Operation = "op-older-0001"
	one := int64(1)
	a.Release = &one
	rec := f.alice.do(http.MethodPost, appPath(f.shared, f.app.ID, "/actions"), a)
	if rec.Code == http.StatusOK {
		t.Fatalf("an action of a release with other effects ran: %s", rec.Body.String())
	}
	expect(t, rec, http.StatusConflict)
	f.work()
	if len(f.cards()) != 0 {
		t.Fatal("a card was made")
	}
}

func TestOutboxPurgeKeepsWhatIsPending(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-purge-001", "East gate", "fail")
	f.work()
	f.mustSubmit(f.alice, "op-purge-002", "West gate", "fail")
	f.later(keepDone + time.Hour)
	if err := f.e.s.purge(f.ctx()); err != nil {
		t.Fatal(err)
	}
	// The finished operation is gone, the one still waiting is kept.
	if n := f.count(`SELECT count(*) FROM app_operations WHERE app_id = ?`, f.app.ID); n != 1 {
		t.Fatalf("%d operations kept", n)
	}
	if n := f.count(`SELECT count(*) FROM app_effects WHERE state = 'pending'`); n != 3 {
		t.Fatalf("%d pending kept", n)
	}
}

func TestTheWorkerRunsInTheBackgroundAndStops(t *testing.T) {
	f := newFX(t)
	stop := f.e.s.StartOutbox(f.ctx())
	f.mustSubmit(f.alice, "op-worker-01", "East gate", "fail")
	deadline := time.Now().Add(10 * time.Second)
	for f.count(`SELECT count(*) FROM app_effects WHERE state = 'done'`) != 3 {
		if time.Now().After(deadline) {
			t.Fatal("the worker did not do the entries")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	if n := len(f.cards()); n != 1 {
		t.Fatalf("%d cards", n)
	}
}

func TestTooManyWaitingEffectsAreRefused(t *testing.T) {
	f := newFX(t)
	// Fill the app's queue directly, then one more request does not queue.
	for i := 0; i < maxOutstanding-2; i++ {
		if _, err := f.e.db.Exec(`INSERT INTO app_operations (app_id, user_id, operation, action, request_hash) VALUES (?, ?, ?, 'x', 'x')`,
			f.app.ID, f.alice.id, fmt.Sprintf("filler-%05d", i)); err != nil {
			t.Fatal(err)
		}
	}
	f.mustSubmit(f.alice, "op-first-0001", "East gate", "pass")
	var rowID int64
	if err := f.e.db.QueryRow(`SELECT id FROM app_rows WHERE app_id = ?`, f.app.ID).Scan(&rowID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.db.Exec(`INSERT INTO app_effects (app_id, operation_id, position, kind, record_id, record_revision, user_id, action_kind, payload, next_attempt_at)
		SELECT ?, o.id, 0, 'open_route', ?, 1, ?, 'insert', '{}', '9999-01-01T00:00:00.000Z' FROM app_operations o WHERE o.operation LIKE 'filler-%'`, f.app.ID, rowID, f.alice.id); err != nil {
		t.Fatal(err)
	}
	rec := f.submit(f.alice, "op-second-001", "West gate", "fail")
	expect(t, rec, http.StatusConflict)
	if errorCode(t, rec) != "app_effects_backlog" {
		t.Fatalf("code = %s", errorCode(t, rec))
	}
	if n := f.count(`SELECT count(*) FROM app_rows WHERE app_id = ?`, f.app.ID); n != 1 {
		t.Fatalf("the refused request left %d records", n)
	}
}
