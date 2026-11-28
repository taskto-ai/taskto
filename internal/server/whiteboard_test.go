package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/whiteboard"
	"taskto.ai/taskto/internal/whiteboard/shapes"
)

const emptyBoard = `{"version":1,"shapes":[]}`

type wbStack struct {
	*liveStack
	svc *whiteboard.Service

	mu     sync.Mutex
	timers []*wbTimer
	now    time.Time
	hold   func(path string)
}

type wbTimer struct {
	f       func()
	stopped bool
}

func newWBStack(t *testing.T) *wbStack {
	t.Helper()
	w := &wbStack{now: time.Unix(1_800_000_000, 0)}
	w.liveStack = newLiveStackWith(t, func(_ *liveStack, o *Options) {
		o.Whiteboard = whiteboard.Options{
			After: func(_ time.Duration, f func()) func() {
				timer := &wbTimer{f: f}
				w.mu.Lock()
				w.timers = append(w.timers, timer)
				w.mu.Unlock()
				return func() {
					w.mu.Lock()
					timer.stopped = true
					w.mu.Unlock()
				}
			},
			Now: func() time.Time {
				w.mu.Lock()
				defer w.mu.Unlock()
				return w.now
			},
			BeforeApply: func(path string) {
				w.mu.Lock()
				hold := w.hold
				w.mu.Unlock()
				if hold != nil {
					hold(path)
				}
			},
		}
		o.WhiteboardsReady = func(s *whiteboard.Service) { w.svc = s }
	})
	return w
}

func (w *wbStack) save() {
	w.t.Helper()
	w.mu.Lock()
	var due []*wbTimer
	for _, timer := range w.timers {
		if !timer.stopped {
			timer.stopped = true
			due = append(due, timer)
		}
	}
	w.mu.Unlock()
	if len(due) == 0 {
		w.t.Fatal("no save was scheduled")
	}
	for _, timer := range due {
		timer.f()
	}
}

func (w *wbStack) saves() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, timer := range w.timers {
		if !timer.stopped {
			n++
		}
	}
	return n
}

func (w *wbStack) route(space int64, path, tail string) string {
	return fmt.Sprintf("/api/workspaces/%d/spaces/%d/whiteboards/%s%s", w.one, space, path, tail)
}

func (w *wbStack) doc(space int64, path string) string {
	return fmt.Sprintf("/api/workspaces/%d/spaces/%d/docs/%s", w.one, space, path)
}

func (w *wbStack) put(cookie string, space int64, path, content, base string) (int, map[string]any) {
	w.t.Helper()
	return w.browser(cookie, http.MethodPut, w.doc(space, path), map[string]any{
		"content_base64": base64.StdEncoding.EncodeToString([]byte(content)), "base_version": base,
	})
}

func (w *wbStack) create(cookie string, space int64, path string) string {
	w.t.Helper()
	code, body := w.put(cookie, space, path, emptyBoard, "")
	if code != http.StatusCreated {
		w.t.Fatalf("create %s = %d %v", path, code, body)
	}
	return body["doc"].(map[string]any)["version"].(string)
}

func (w *wbStack) version(cookie string, space int64, path string) string {
	w.t.Helper()
	code, body := w.browser(cookie, http.MethodGet, w.doc(space, path), nil)
	if code != http.StatusOK {
		w.t.Fatalf("read %s = %d %v", path, code, body)
	}
	return body["version"].(string)
}

func (w *wbStack) content(cookie string, space int64, path string) shapes.Board {
	w.t.Helper()
	rec := w.stack.request(http.MethodGet, w.doc(space, path), nil, sameOrigin(cookie))
	var doc struct {
		ContentBase64 string `json:"content_base64"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || rec.Code != http.StatusOK {
		w.t.Fatalf("read %s = %d %s", path, rec.Code, rec.Body)
	}
	raw, err := base64.StdEncoding.DecodeString(doc.ContentBase64)
	if err != nil {
		w.t.Fatal(err)
	}
	b, err := shapes.Parse(raw)
	if err != nil {
		w.t.Fatalf("the saved file is not a whiteboard: %v", err)
	}
	return b
}

func addOp(id string) map[string]any {
	return map[string]any{"op": "add", "shape": map[string]any{"id": id, "type": "rect", "color": "blue", "w": 10, "h": 10}}
}

func (w *wbStack) ops(cookie string, space int64, path string, ops ...map[string]any) (int, map[string]any) {
	w.t.Helper()
	return w.browser(cookie, http.MethodPost, w.route(space, path, "/ops"), map[string]any{"ops": ops})
}

func (w *wbStack) draw(cookie string, space int64, path string, ids ...string) {
	w.t.Helper()
	for _, id := range ids {
		if code, body := w.ops(cookie, space, path, addOp(id)); code != http.StatusOK {
			w.t.Fatalf("add %s = %d %v", id, code, body)
		}
	}
}

func (w *wbStack) snapshot(cookie string, space int64, path string) (int, map[string]any) {
	w.t.Helper()
	return w.browser(cookie, http.MethodGet, w.route(space, path, ""), nil)
}

func shapeIDs(body map[string]any) string {
	var out []string
	list, _ := body["shapes"].([]any)
	for _, s := range list {
		out = append(out, s.(map[string]any)["id"].(string))
	}
	return strings.Join(out, ",")
}

func (w *wbStack) count(q string, args ...any) int64 {
	w.t.Helper()
	return w.scalar(q, args...)
}

func (w *wbStack) addViewer(space int64, who person) {
	w.t.Helper()
	if code, body := w.browser(w.alice.cookie, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members", w.one, space),
		map[string]any{"user_id": who.id, "role": "viewer"}); code != http.StatusNoContent {
		w.t.Fatalf("add a viewer = %d %v", code, body)
	}
}

func (s *liveStream) nextOps() (string, map[string]any) {
	s.t.Helper()
	ev := s.next()
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		s.t.Fatalf("event %+v: %v", ev, err)
	}
	return ev.Type, data
}

func TestEveryWhiteboardRouteChecksTheCallersAccess(t *testing.T) {
	w := newWBStack(t)
	const path = "plan.whiteboard.json"
	w.create(w.bob.cookie, w.team, path)
	w.create(w.alice.cookie, w.alice.private, path)
	w.addViewer(w.secret, w.bob)
	w.create(w.carol.cookie, w.secret, path)

	type call struct {
		who    string
		cookie string
		space  int64
		get    int
		live   int
		post   int
	}
	for _, c := range []call{
		{"no login", "", w.team, 401, 401, 401},
		{"an editor", w.bob.cookie, w.team, 200, 200, 200},
		{"the space's owner", w.alice.cookie, w.team, 200, 200, 200},
		{"a member of other spaces only", w.carol.cookie, w.team, 404, 404, 404},
		{"a viewer", w.bob.cookie, w.secret, 200, 200, 403},
		{"somebody's private space", w.bob.cookie, w.alice.private, 404, 404, 404},
		{"an admin who is not in the space", w.alice.cookie, w.secret, 404, 404, 404},
	} {
		if code, _ := w.snapshot(c.cookie, c.space, path); code != c.get {
			t.Errorf("%s: GET = %d, want %d", c.who, code, c.get)
		}
		if got := w.status(c.cookie, w.base+w.route(c.space, path, "/live")); got != c.live {
			t.Errorf("%s: live = %d, want %d", c.who, got, c.live)
		}
		if code, _ := w.ops(c.cookie, c.space, path, addOp("mine")); code != c.post {
			t.Errorf("%s: POST ops = %d, want %d", c.who, code, c.post)
		}
	}
	if got := w.count(`SELECT count(*) FROM whiteboard_ops WHERE board_id IN (SELECT id FROM whiteboards WHERE space_id = ?)`, w.secret); got != 0 {
		t.Errorf("a refused post left %d ops in the log", got)
	}
	if code, body := w.ops(w.bob.cookie, w.team, "plan.md"); code != http.StatusNotFound {
		t.Errorf("ops on a path that is not a whiteboard = %d %v", code, body)
	}
	if code, body := w.snapshot(w.bob.cookie, w.team, "plan.md"); code != http.StatusBadRequest {
		t.Errorf("a snapshot of a path that is not a whiteboard = %d %v", code, body)
	}
	if code, _ := w.snapshot(w.bob.cookie, w.team, "missing.whiteboard.json"); code != http.StatusNotFound {
		t.Errorf("a snapshot of a missing board = %d", code)
	}
}

func TestEditorsSeeEachOthersShapesWithinTheSameSecond(t *testing.T) {
	w := newWBStack(t)
	const path = "plan.whiteboard.json"
	w.create(w.bob.cookie, w.team, path)
	bobs := w.connectTo(w.bob.cookie, w.base+w.route(w.team, path, "/live"))
	alices := w.connectTo(w.alice.cookie, w.base+w.route(w.team, path, "/live"))

	w.draw(w.bob.cookie, w.team, path, "a")
	if code, _ := w.ops(w.alice.cookie, w.team, path, map[string]any{"op": "update", "id": "a", "set": map[string]any{"x": 40}}); code != http.StatusOK {
		t.Fatal("alice's update was refused")
	}
	for name, s := range map[string]*liveStream{"bob": bobs, "alice": alices} {
		typ, first := s.nextOps()
		_, second := s.nextOps()
		if typ != whiteboard.EventOps || first["seq"] != float64(1) || second["seq"] != float64(2) {
			t.Errorf("%s saw %v then %v", name, first, second)
		}
	}
	code, snap := w.snapshot(w.alice.cookie, w.team, path)
	shapesList, _ := snap["shapes"].([]any)
	if code != http.StatusOK || snap["seq"] != float64(2) || len(shapesList) != 1 || shapesList[0].(map[string]any)["x"] != float64(40) {
		t.Fatalf("snapshot = %d %v", code, snap)
	}
}

func TestTwoWritersAtOnceGetOneOrderEverybodyAgreesOn(t *testing.T) {
	w := newWBStack(t)
	const path = "plan.whiteboard.json"
	w.create(w.bob.cookie, w.team, path)
	watcher := w.connectTo(w.alice.cookie, w.base+w.route(w.team, path, "/live"))
	var wg sync.WaitGroup
	for _, who := range []person{w.bob, w.alice} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 20 {
				code, body := w.ops(who.cookie, w.team, path, addOp(fmt.Sprintf("u%d-%d", who.id, i)))
				if code != http.StatusOK {
					t.Errorf("post = %d %v", code, body)
				}
			}
		}()
	}
	wg.Wait()
	var replay strings.Builder
	for want := 1; want <= 40; want++ {
		_, data := watcher.nextOps()
		if data["seq"] != float64(want) {
			t.Fatalf("event %d carried seq %v", want, data["seq"])
		}
		for _, op := range data["ops"].([]any) {
			replay.WriteString(op.(map[string]any)["shape"].(map[string]any)["id"].(string) + ",")
		}
	}
	_, snap := w.snapshot(w.bob.cookie, w.team, path)
	if got := strings.Join(strings.Split(strings.TrimSuffix(replay.String(), ","), ","), ","); got != shapeIDs(snap) {
		t.Fatalf("replaying the events gives %s, the snapshot has %s", got, shapeIDs(snap))
	}
}

func TestASnapshotIsACommitOfTheLastEditorAndTheLogGoesWithIt(t *testing.T) {
	w := newWBStack(t)
	const path = "plan.whiteboard.json"
	w.create(w.bob.cookie, w.team, path)
	w.draw(w.alice.cookie, w.team, path, "a")
	w.draw(w.bob.cookie, w.team, path, "b")
	if got := w.content(w.bob.cookie, w.team, path); len(got.Shapes) != 0 {
		t.Fatalf("the document changed before the save: %+v", got)
	}
	w.save()
	if got := w.content(w.bob.cookie, w.team, path); len(got.Shapes) != 2 {
		t.Fatalf("the saved document has %+v", got.Shapes)
	}
	code, history := w.browser(w.bob.cookie, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces/%d/history/%s", w.one, w.team, path), nil)
	commits, _ := history["commits"].([]any)
	if code != http.StatusOK || len(commits) != 2 {
		t.Fatalf("history = %d %v: want the creation and one snapshot", code, history)
	}
	last := commits[0].(map[string]any)
	if last["message"] != whiteboard.SnapshotNote || last["author"].(map[string]any)["name"] != "Bob" {
		t.Fatalf("the snapshot commit is %v", last)
	}
	if got := w.count(`SELECT count(*) FROM whiteboard_ops`); got != 0 {
		t.Fatalf("%d ops are still logged after the save", got)
	}
	w.draw(w.bob.cookie, w.team, path, "c")
	if _, snap := w.snapshot(w.bob.cookie, w.team, path); snap["seq"] != float64(3) || shapeIDs(snap) != "a,b,c" {
		t.Fatalf("after the save the board continues: %v", snap)
	}
	if got := w.saves(); got != 1 {
		t.Fatalf("%d saves are scheduled, want 1 for the new edits", got)
	}
}

func TestACrashLosesNothingAndTheLogIsSavedAfterIt(t *testing.T) {
	w := newWBStack(t)
	const path = "plan.whiteboard.json"
	w.create(w.bob.cookie, w.team, path)
	w.draw(w.bob.cookie, w.team, path, "a", "b")

	restarted := whiteboard.New(w.sqlDB, w.kb, w.hub, w.checker, whiteboard.Options{})
	sp, err := w.checker.Check(context.Background(), w.bob.id, w.team)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := restarted.Snapshot(context.Background(), sp, path)
	if err != nil || len(snap.Shapes) != 2 || snap.Seq != 2 {
		t.Fatalf("after a restart: %+v, %v", snap, err)
	}
	if got := w.content(w.bob.cookie, w.team, path); len(got.Shapes) != 0 {
		t.Fatal("the document had the shapes before any save")
	}
	restarted.Close(context.Background())
	if got := w.content(w.bob.cookie, w.team, path); len(got.Shapes) != 2 {
		t.Fatalf("the shutdown save wrote %+v", got.Shapes)
	}
	if got := w.count(`SELECT count(*) FROM whiteboard_ops`); got != 0 {
		t.Fatalf("%d ops are still logged", got)
	}
}

func TestAChangeOfTheDocumentWhileTheLogWaitsReplacesTheBoardOnTheNextLoad(t *testing.T) {
	w := newWBStack(t)
	const path = "plan.whiteboard.json"
	base := w.create(w.bob.cookie, w.team, path)
	w.draw(w.bob.cookie, w.team, path, "a")
	external := `{"version":1,"shapes":[{"id":"restored","type":"ellipse","color":"teal"}]}`
	if code, body := w.put(w.bob.cookie, w.team, path, external, base); code != http.StatusOK {
		t.Fatalf("external write = %d %v", code, body)
	}
	before := w.count(`SELECT seq FROM whiteboards`)

	restarted := whiteboard.New(w.sqlDB, w.kb, w.hub, w.checker, whiteboard.Options{})
	sp, _ := w.checker.Check(context.Background(), w.bob.id, w.team)
	snap, err := restarted.Snapshot(context.Background(), sp, path)
	if err != nil || len(snap.Shapes) != 1 || snap.Shapes[0].ID != "restored" || snap.Seq <= before {
		t.Fatalf("snapshot %+v (was at seq %d), %v: the log must not be laid over a document it was not made on", snap, before, err)
	}
	if got := w.count(`SELECT count(*) FROM whiteboard_ops`); got != 0 {
		t.Fatalf("%d ops of the replaced board are still logged", got)
	}
}

func TestAnExternalWriteWinsAtTheNextSaveAndTheViewersReconnect(t *testing.T) {
	w := newWBStack(t)
	const path = "plan.whiteboard.json"
	base := w.create(w.bob.cookie, w.team, path)
	stream := w.connectTo(w.alice.cookie, w.base+w.route(w.team, path, "/live"))
	w.draw(w.bob.cookie, w.team, path, "a")
	stream.nextOps()
	external := `{"version":1,"shapes":[{"id":"restored","type":"ellipse","color":"teal"}]}`
	if code, body := w.put(w.bob.cookie, w.team, path, external, base); code != http.StatusOK {
		t.Fatalf("external write = %d %v", code, body)
	}
	w.save()
	stream.endedWithoutData()
	if _, snap := w.snapshot(w.alice.cookie, w.team, path); shapeIDs(snap) != "restored" {
		t.Fatalf("snapshot %v", snap)
	}
	if got := w.content(w.bob.cookie, w.team, path); len(got.Shapes) != 1 || got.Shapes[0].ID != "restored" {
		t.Fatalf("the save overwrote the external write: %+v", got)
	}
	if got := w.count(`SELECT count(*) FROM whiteboard_ops`); got != 0 {
		t.Fatalf("%d ops of the dropped board are still logged", got)
	}
	w.draw(w.bob.cookie, w.team, path, "b")
	if _, snap := w.snapshot(w.bob.cookie, w.team, path); shapeIDs(snap) != "restored,b" {
		t.Fatalf("after the drop the board continues: %v", snap)
	}
}

func TestADeletedDocumentEndsItsStreamsAndRefusesFurtherOps(t *testing.T) {
	w := newWBStack(t)
	const path = "plan.whiteboard.json"
	base := w.create(w.bob.cookie, w.team, path)
	stream := w.connectTo(w.alice.cookie, w.base+w.route(w.team, path, "/live"))
	w.draw(w.bob.cookie, w.team, path, "a")
	stream.nextOps()
	if code, body := w.browser(w.bob.cookie, http.MethodDelete, w.doc(w.team, path)+"?base_version="+base, nil); code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("delete = %d %v", code, body)
	}
	w.save()
	stream.endedWithoutData()
	if code, _ := w.ops(w.bob.cookie, w.team, path, addOp("late")); code != http.StatusNotFound {
		t.Fatalf("an op on a deleted board = %d, want 404", code)
	}
	if code, _ := w.snapshot(w.bob.cookie, w.team, path); code != http.StatusNotFound {
		t.Fatalf("a snapshot of a deleted board = %d, want 404", code)
	}
	if got := w.count(`SELECT count(*) FROM whiteboards`) + w.count(`SELECT count(*) FROM whiteboard_ops`); got != 0 {
		t.Fatalf("%d rows of the deleted board remain", got)
	}
}

func TestOnlyAWhiteboardIsEverWrittenAtAWhiteboardPath(t *testing.T) {
	w := newWBStack(t)
	for name, content := range map[string]string{
		"not JSON":       "hello",
		"a wrong shape":  `{"version":1,"shapes":[{"id":"a","type":"star","color":"blue"}]}`,
		"a literal hue":  `{"version":1,"shapes":[{"id":"a","type":"rect","color":"#fff"}]}`,
		"an unknown key": `{"version":1,"shapes":[],"extra":true}`,
	} {
		code, body := w.put(w.bob.cookie, w.team, "bad.whiteboard.json", content, "")
		if code != http.StatusBadRequest || body["code"] != "whiteboard_invalid" {
			t.Errorf("%s through the documents route = %d %v", name, code, body)
		}
		req := httptest.NewRequest(http.MethodPut, w.doc(w.team, "bad.whiteboard.json"), strings.NewReader(content))
		req.Header.Set("Content-Type", "application/octet-stream")
		sameOrigin(w.bob.cookie)(req)
		rec := httptest.NewRecorder()
		w.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "whiteboard_invalid") {
			t.Errorf("%s as raw bytes = %d %s", name, rec.Code, rec.Body)
		}
	}
	if code, _ := w.browser(w.bob.cookie, http.MethodGet, w.doc(w.team, "bad.whiteboard.json"), nil); code != http.StatusNotFound {
		t.Fatalf("a refused write left a document (%d)", code)
	}
	big := `{"version":1,"shapes":[],"pad":"` + strings.Repeat("x", shapes.MaxFileBytes) + `"}`
	if code, body := w.put(w.bob.cookie, w.team, "big.whiteboard.json", big, ""); code == http.StatusCreated {
		t.Fatalf("a board over the size limit was stored: %v", body)
	}
	if code, body := w.browser(w.bob.cookie, http.MethodPut, w.doc(w.team, "plain.json"), map[string]any{
		"content_base64": base64.StdEncoding.EncodeToString([]byte("[1]")),
	}); code != http.StatusCreated {
		t.Fatalf("other JSON files are unaffected: %d %v", code, body)
	}
}

func TestAFileMovedOntoAWhiteboardNameIsRefusedWhenOpened(t *testing.T) {
	w := newWBStack(t)
	if code, body := w.browser(w.bob.cookie, http.MethodPut, w.doc(w.team, "notes.md"), map[string]any{"content": "# not a board"}); code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, body)
	}
	if code, body := w.browser(w.bob.cookie, http.MethodPatch, w.doc(w.team, "notes.md"), map[string]any{"to": "now.whiteboard.json"}); code != http.StatusOK {
		t.Fatalf("move = %d %v", code, body)
	}
	if code, body := w.snapshot(w.bob.cookie, w.team, "now.whiteboard.json"); code != http.StatusBadRequest || body["code"] != "whiteboard_invalid" {
		t.Fatalf("opening it = %d %v", code, body)
	}
	if got := w.count(`SELECT count(*) FROM whiteboards`); got != 0 {
		t.Fatalf("a refused open created %d board rows", got)
	}
}

func TestAWriterRemovedWhileItsRequestWaitsWritesNothing(t *testing.T) {
	w := newWBStack(t)
	const path = "plan.whiteboard.json"
	w.create(w.bob.cookie, w.team, path)
	for name, lose := range map[string]struct {
		want   int
		change func()
	}{
		"removed from the space": {http.StatusNotFound, func() {
			if code, body := w.browser(w.alice.cookie, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", w.one, w.team, w.bob.id), nil); code != http.StatusNoContent {
				t.Fatalf("remove = %d %v", code, body)
			}
		}},
		"made a viewer": {http.StatusForbidden, func() {
			w.exec(`UPDATE space_members SET role = 'viewer' WHERE space_id = ? AND user_id = ?`, w.team, w.bob.id)
		}},
	} {
		reached, release := make(chan struct{}), make(chan struct{})
		w.mu.Lock()
		w.hold = func(string) {
			w.mu.Lock()
			w.hold = nil
			w.mu.Unlock()
			close(reached)
			<-release
		}
		w.mu.Unlock()
		result := make(chan int, 1)
		go func() {
			code, _ := w.ops(w.bob.cookie, w.team, path, addOp("late-"+strings.ReplaceAll(name, " ", "-")))
			result <- code
		}()
		<-reached
		lose.change()
		close(release)
		if code := <-result; code != lose.want {
			t.Errorf("%s: the waiting write = %d, want %d", name, code, lose.want)
		}
		w.exec(`INSERT OR REPLACE INTO space_members (space_id, user_id, role) VALUES (?, ?, 'editor')`, w.team, w.bob.id)
	}
	if got := w.count(`SELECT count(*) FROM whiteboard_ops`); got != 0 {
		t.Fatalf("%d ops of a writer who lost access were logged", got)
	}
	if w.saves() != 0 {
		t.Fatal("a save was scheduled for work that was refused")
	}
}

func TestAStreamEndsWhenItsBoardIsNoLongerThere(t *testing.T) {
	w := newWBStack(t)
	const path = "plan.whiteboard.json"
	w.create(w.bob.cookie, w.team, path)
	stream := w.connectTo(w.alice.cookie, w.base+w.route(w.team, path, "/live"))
	w.exec(`DELETE FROM whiteboards`)
	w.tick(0)
	stream.endedWithoutData()
}

func TestOnePersonFloodingDoesNotStopOthersAndTheLimitPasses(t *testing.T) {
	w := newWBStack(t)
	const path = "plan.whiteboard.json"
	w.create(w.bob.cookie, w.team, path)
	for i := range 120 {
		if code, body := w.ops(w.bob.cookie, w.team, path, addOp(fmt.Sprint("s", i))); code != http.StatusOK {
			t.Fatalf("batch %d = %d %v", i, code, body)
		}
	}
	code, body := w.ops(w.bob.cookie, w.team, path, addOp("extra"))
	if code != http.StatusTooManyRequests || body["code"] != "whiteboard_rate_limited" {
		t.Fatalf("the 121st batch = %d %v", code, body)
	}
	if code, _ := w.ops(w.alice.cookie, w.team, path, addOp("hers")); code != http.StatusOK {
		t.Fatalf("another person was refused too: %d", code)
	}
	w.mu.Lock()
	w.now = w.now.Add(11 * time.Second)
	w.mu.Unlock()
	if code, _ := w.ops(w.bob.cookie, w.team, path, addOp("later")); code != http.StatusOK {
		t.Fatalf("after the window = %d", code)
	}
}

func TestAnInvalidBatchChangesNothingAndStaysSilent(t *testing.T) {
	w := newWBStack(t)
	const path = "plan.whiteboard.json"
	w.create(w.bob.cookie, w.team, path)
	stream := w.connectTo(w.alice.cookie, w.base+w.route(w.team, path, "/live"))
	code, body := w.ops(w.bob.cookie, w.team, path, addOp("ok"), map[string]any{"op": "add", "shape": map[string]any{"id": "bad", "type": "star", "color": "blue"}})
	if code != http.StatusBadRequest || body["code"] != "whiteboard_invalid" {
		t.Fatalf("a bad batch = %d %v", code, body)
	}
	if code, _ := w.browser(w.bob.cookie, http.MethodPost, w.route(w.team, path, "/ops"), map[string]any{"ops": []any{}, "extra": 1}); code != http.StatusBadRequest {
		t.Fatalf("an unknown field = %d", code)
	}
	if code, _ := w.browser(w.bob.cookie, http.MethodPost, w.route(w.team, path, "/ops"), map[string]any{"ops": make([]any, 101)}); code != http.StatusBadRequest {
		t.Fatalf("101 operations = %d", code)
	}
	if _, snap := w.snapshot(w.bob.cookie, w.team, path); snap["seq"] != float64(0) || shapeIDs(snap) != "" {
		t.Fatalf("the board changed: %v", snap)
	}
	w.draw(w.bob.cookie, w.team, path, "fine")
	if _, data := stream.nextOps(); data["seq"] != float64(1) {
		t.Fatalf("the first event the stream got was %v: a refused batch must not be announced", data)
	}
}

func TestTheShutdownSaveWritesWhatWasNotSavedYet(t *testing.T) {
	w := newWBStack(t)
	const path = "plan.whiteboard.json"
	w.create(w.bob.cookie, w.team, path)
	w.draw(w.bob.cookie, w.team, path, "a")
	w.svc.Close(context.Background())
	if got := w.content(w.bob.cookie, w.team, path); len(got.Shapes) != 1 {
		t.Fatalf("the document has %+v", got.Shapes)
	}
	w.svc.Close(context.Background())
	code, history := w.browser(w.bob.cookie, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces/%d/history/%s", w.one, w.team, path), nil)
	if commits, _ := history["commits"].([]any); code != http.StatusOK || len(commits) != 2 {
		t.Fatalf("a second shutdown save made a commit: %v", history)
	}
}

func TestRemovingASpaceTakesItsBoardsWithIt(t *testing.T) {
	w := newWBStack(t)
	w.create(w.bob.cookie, w.team, "plan.whiteboard.json")
	w.draw(w.bob.cookie, w.team, "plan.whiteboard.json", "a")
	w.kb.SetSpaceCheck(access.SpaceRowCheck(w.sqlDB))
	w.exec(`DELETE FROM spaces WHERE id = ?`, w.team)
	if got := w.count(`SELECT count(*) FROM whiteboards`) + w.count(`SELECT count(*) FROM whiteboard_ops`); got != 0 {
		t.Fatalf("%d rows of the removed space remain", got)
	}
	w.save()
	if w.saves() != 0 {
		t.Fatal("the save of a removed space was scheduled again")
	}
	if _, err := w.checker.Check(context.Background(), w.bob.id, w.team); err == nil || !strings.Contains(err.Error(), access.ErrNotFound.Error()) {
		t.Fatalf("the space is still there: %v", err)
	}
}
