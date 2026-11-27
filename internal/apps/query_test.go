package apps

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
)

func tasksDef(t *testing.T) Definition {
	t.Helper()
	raw := func(items ...string) []json.RawMessage {
		var out []json.RawMessage
		for _, i := range items {
			out = append(out, json.RawMessage(i))
		}
		return out
	}
	def, err := Definition{
		Components: raw(
			`{"id":"root","component":"Column","children":["list","title","add"]}`,
			`{"id":"list","component":"List","children":{"componentId":"line","path":"/items"}}`,
			`{"id":"line","component":"Text","text":{"path":"title"}}`,
			`{"id":"title","component":"TextField","label":"Title","value":{"path":"/draft/title"}}`,
			`{"id":"add-label","component":"Text","text":"Add"}`,
			`{"id":"add","component":"Button","child":"add-label","variant":"primary","action":{"event":{"name":"add","context":{"title":{"path":"/draft/title"}}}}}`,
		),
		Collection: Collection{Fields: []Field{
			{Name: "title", Type: TypeString, Required: true, MaxLength: 200},
			{Name: "qty", Type: TypeNumber},
			{Name: "urgent", Type: TypeBoolean},
			{Name: "due", Type: TypeDate},
			{Name: "at", Type: TypeTime},
			{Name: "kind", Type: TypeChoice, Options: []string{"bug", "chore", "idea"}},
		}},
		Actions: map[string]Action{"add": {Kind: ActionInsert}, "edit": {Kind: ActionUpdate}, "remove": {Kind: ActionDelete}},
	}.Check()
	if err != nil {
		t.Fatal(err)
	}
	return def
}

func (w *world) tasks(u *user, s access.Space) App {
	w.e.t.Helper()
	rec := u.do(http.MethodPost, appsPath(s, ""), map[string]any{"name": "Tasks", "definition": tasksDef(w.e.t)})
	expect(w.e.t, rec, http.StatusCreated)
	return w.publish(u, s, decode[App](w.e.t, rec))
}

func (w *world) addTask(u *user, s access.Space, a App, ctx map[string]any) int64 {
	w.e.t.Helper()
	expect(w.e.t, u.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "add", ctx)), http.StatusOK)
	var id int64
	if err := w.e.db.QueryRow(`SELECT max(id) FROM app_rows WHERE app_id = ?`, a.ID).Scan(&id); err != nil {
		w.e.t.Fatal(err)
	}
	return id
}

func (w *world) removeTask(u *user, s access.Space, a App, id int64) {
	w.e.t.Helper()
	expect(w.e.t, u.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "remove", map[string]any{"id": id})), http.StatusOK)
}

type page struct {
	Records    []Record
	NextCursor *string `json:"next_cursor"`
	Total      int
	Aggregates []AggregateResult
}

func (p page) ids() []int64 {
	out := make([]int64, len(p.Records))
	for i, r := range p.Records {
		out[i] = r.ID
	}
	return out
}

func queryURL(s access.Space, a App, params ...string) string {
	v := url.Values{}
	for i := 0; i+1 < len(params); i += 2 {
		v.Add(params[i], params[i+1])
	}
	return appPath(s, a.ID, "/records/query?"+v.Encode())
}

func (w *world) query(u *user, s access.Space, a App, params ...string) page {
	w.e.t.Helper()
	rec := u.do(http.MethodGet, queryURL(s, a, params...), nil)
	expect(w.e.t, rec, http.StatusOK)
	return decode[page](w.e.t, rec)
}

func (w *world) walk(u *user, s access.Space, a App, size int, between func(n int, served []int64), params ...string) []int64 {
	w.e.t.Helper()
	var served []int64
	cursor := ""
	for n := 1; ; n++ {
		p := w.query(u, s, a, append(slices.Clone(params), "limit", fmt.Sprint(size), "cursor", cursor)...)
		if len(p.Records) > size {
			w.e.t.Fatalf("a page of %d records, asked for %d", len(p.Records), size)
		}
		served = append(served, p.ids()...)
		if p.NextCursor == nil {
			return served
		}
		if len(p.Records) == 0 {
			w.e.t.Fatal("an empty page with a next cursor")
		}
		cursor = *p.NextCursor
		if between != nil {
			between(n, served)
		}
		if n > 200 {
			w.e.t.Fatal("paging doesn't end")
		}
	}
}

func TestQueryPagingNeitherSkipsNorRepeatsUnderChanges(t *testing.T) {
	orders := []struct {
		name   string
		params []string
	}{
		{"most recently changed (the default)", nil},
		{"number up, with ties and records without one", []string{"sort", "qty"}},
		{"number down", []string{"sort", "qty", "dir", "desc"}},
		{"text up", []string{"sort", "title"}},
		{"date down", []string{"sort", "due", "dir", "desc"}},
		{"boolean up", []string{"sort", "urgent"}},
		{"created, oldest first", []string{"sort", "_created", "dir", "asc"}},
		{"id down", []string{"sort", "id", "dir", "desc"}},
	}
	for _, o := range orders {
		t.Run(o.name, func(t *testing.T) {
			w := newWorld(t)
			s := w.shared
			a := w.tasks(w.alice, s)
			var ids []int64
			for i := 0; i < 23; i++ {
				ctx := map[string]any{"title": fmt.Sprintf("task %02d", (i*7)%23), "due": fmt.Sprintf("2026-01-%02d", 1+i%4), "urgent": i%3 == 0}
				if i%5 != 0 {
					ctx["qty"] = i % 4
				}
				ids = append(ids, w.addTask(w.alice, s, a, ctx))
			}
			baseline := w.query(w.alice, s, a, append(slices.Clone(o.params), "limit", "200")...).ids()
			if len(baseline) != len(ids) {
				t.Fatalf("baseline = %v", baseline)
			}

			var deleted []int64
			served := w.walk(w.alice, s, a, 4, func(n int, served []int64) {
				w.addTask(w.alice, s, a, map[string]any{"title": fmt.Sprintf("late %d", n), "qty": 2})
				if n == 2 || n == 3 {
					var unseen int64
					for _, id := range baseline {
						if !slices.Contains(served, id) && !slices.Contains(deleted, id) {
							unseen = id
							break
						}
					}
					if unseen != 0 {
						w.removeTask(w.alice, s, a, unseen)
						deleted = append(deleted, unseen)
					}
					if i := slices.IndexFunc(served, func(id int64) bool { return !slices.Contains(deleted, id) }); i >= 0 {
						w.removeTask(w.alice, s, a, served[i])
						deleted = append(deleted, served[i])
					}
				}
			}, o.params...)

			seen := map[int64]bool{}
			for _, id := range served {
				if seen[id] {
					t.Fatalf("record %d was served twice: %v", id, served)
				}
				seen[id] = true
			}
			var stable []int64
			for _, id := range baseline {
				if !slices.Contains(deleted, id) {
					stable = append(stable, id)
				}
			}
			var gotStable []int64
			for _, id := range served {
				if slices.Contains(baseline, id) {
					gotStable = append(gotStable, id)
				}
			}
			for _, id := range stable {
				if !seen[id] {
					t.Fatalf("record %d was skipped; served %v, baseline %v", id, served, baseline)
				}
			}
			var want []int64
			for _, id := range baseline {
				if seen[id] {
					want = append(want, id)
				}
			}
			if !slices.Equal(gotStable, want) {
				t.Fatalf("served in the order %v, the baseline's is %v", gotStable, want)
			}
		})
	}
}

func TestQueryOrderIsSortKeyThenIDWithNothingLast(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.tasks(w.alice, s)
	a1 := w.addTask(w.alice, s, a, map[string]any{"title": "a", "qty": 2})
	a2 := w.addTask(w.alice, s, a, map[string]any{"title": "b"})
	a3 := w.addTask(w.alice, s, a, map[string]any{"title": "c", "qty": 1})
	a4 := w.addTask(w.alice, s, a, map[string]any{"title": "d", "qty": 2})
	a5 := w.addTask(w.alice, s, a, map[string]any{"title": "e"})
	if got, want := w.query(w.alice, s, a, "sort", "qty").ids(), []int64{a3, a1, a4, a2, a5}; !slices.Equal(got, want) {
		t.Fatalf("up = %v, want %v", got, want)
	}
	if got, want := w.query(w.alice, s, a, "sort", "qty", "dir", "desc").ids(), []int64{a4, a1, a3, a5, a2}; !slices.Equal(got, want) {
		t.Fatalf("down = %v, want %v", got, want)
	}
}

func TestQueryTotalsAreOfAllMatchingRecordsNotThePage(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.tasks(w.alice, s)
	sum, count, lo, hi := 0.0, 0, math.Inf(1), math.Inf(-1)
	for i := 1; i <= 30; i++ {
		kind := "bug"
		if i%3 == 0 {
			kind = "idea"
		}
		ctx := map[string]any{"title": fmt.Sprintf("t%d", i), "qty": i * 2, "kind": kind, "due": fmt.Sprintf("2026-03-%02d", i)}
		w.addTask(w.alice, s, a, ctx)
		if kind == "bug" {
			sum += float64(i * 2)
			count++
			lo, hi = math.Min(lo, float64(i*2)), math.Max(hi, float64(i*2))
		}
	}
	w.addTask(w.alice, s, a, map[string]any{"title": "no number", "kind": "bug"})
	count++

	aggs := []string{"aggregate", "count", "aggregate", "count:qty", "aggregate", "sum:qty", "aggregate", "avg:qty", "aggregate", "min:qty", "aggregate", "max:qty",
		"aggregate", "min:due", "aggregate", "max:due"}
	p := w.query(w.alice, s, a, append([]string{"filter", "kind:eq:bug", "limit", "3"}, aggs...)...)
	if len(p.Records) != 3 || p.NextCursor == nil {
		t.Fatalf("page = %d records, cursor %v", len(p.Records), p.NextCursor)
	}
	if p.Total != count {
		t.Fatalf("total = %d, want %d", p.Total, count)
	}
	by := map[string]AggregateResult{}
	for _, r := range p.Aggregates {
		by[r.Kind+":"+r.Field] = r
	}
	check := func(key string, value any, n int) {
		t.Helper()
		if r := by[key]; r.Value != value || r.Count != n {
			t.Errorf("%s = %v over %d, want %v over %d", key, r.Value, r.Count, value, n)
		}
	}
	check("count:", float64(count), count)
	check("count:qty", float64(count-1), count-1)
	check("sum:qty", sum, count-1)
	check("avg:qty", sum/float64(count-1), count-1)
	check("min:qty", lo, count-1)
	check("max:qty", hi, count-1)
	check("min:due", "2026-03-01", count-1)
	check("max:due", "2026-03-29", count-1)
	p2 := w.query(w.alice, s, a, append([]string{"filter", "kind:eq:bug", "limit", "3", "cursor", *p.NextCursor}, aggs...)...)
	if p2.Total != p.Total || fmt.Sprint(p2.Aggregates) != fmt.Sprint(p.Aggregates) {
		t.Fatalf("the second page's totals are %d %v, the first's %d %v", p2.Total, p2.Aggregates, p.Total, p.Aggregates)
	}
	none := w.query(w.alice, s, a, "filter", "title:eq:nothing", "aggregate", "sum:qty", "aggregate", "min:qty", "aggregate", "avg:qty")
	if none.Total != 0 || len(none.Records) != 0 || none.NextCursor != nil ||
		none.Aggregates[0].Value != 0.0 || none.Aggregates[1].Value != nil || none.Aggregates[2].Value != nil {
		t.Fatalf("nothing matching = %+v", none)
	}
}

func TestQuerySumAgreesWithSummaries(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	for i := 1; i <= 7; i++ {
		w.add(w.alice, s, w.sharedApp, fmt.Sprintf("e%d", i), fmt.Sprint(i*3))
	}
	want := 3.0 + 3*28
	p := w.query(w.alice, s, w.sharedApp, "aggregate", "sum:amount")
	if p.Aggregates[0].Value != want {
		t.Fatalf("sum = %v, want %v", p.Aggregates[0].Value, want)
	}
	if got := w.surfaceBody(w.alice, s, w.sharedApp, ""); !strings.Contains(got, fmt.Sprintf(`"total":"%.2f"`, want)) {
		t.Fatalf("the summary doesn't say %.2f: %s", want, got)
	}
}

func TestQueryReadScopeAppliesToPagesTotalsAndAggregates(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.tasks(w.alice, s)
	for i := 1; i <= 5; i++ {
		w.addTask(w.alice, s, a, map[string]any{"title": fmt.Sprintf("alice %d", i), "qty": 10})
	}
	for i := 1; i <= 3; i++ {
		w.addTask(w.owner, s, a, map[string]any{"title": fmt.Sprintf("owner %d", i), "qty": 1000})
	}
	w.mustSetRoles(w.alice, s, a, grant(w.owner.id, access.AppUser), grant(w.alice.id, access.AppDataManager, access.AppBuilder, access.AppPublisher))

	aggs := []string{"aggregate", "sum:qty", "aggregate", "max:qty", "aggregate", "count"}
	own := w.query(w.owner, s, a, append([]string{"limit", "100"}, aggs...)...)
	if own.Total != 3 || len(own.Records) != 3 || own.Aggregates[0].Value != 3000.0 || own.Aggregates[1].Value != 1000.0 {
		t.Fatalf("the user who reads their own gets %+v", own)
	}
	for _, r := range own.Records {
		if r.CreatedBy == nil || r.CreatedBy.ID != w.owner.id {
			t.Fatalf("a record of %+v", r.CreatedBy)
		}
	}
	all := w.query(w.alice, s, a, append([]string{"limit", "100"}, aggs...)...)
	if all.Total != 8 || all.Aggregates[0].Value != 3050.0 {
		t.Fatalf("the data manager gets %+v", all)
	}
	for _, params := range [][]string{
		{"q", "alice"}, {"filter", "title:contains:alice"}, {"filter", "qty:eq:10"}, {"sort", "qty", "filter", "qty:lt:100"}, {"state", "deleted"},
	} {
		if p := w.query(w.owner, s, a, append(params, aggs...)...); p.Total != 0 || len(p.Records) != 0 || p.Aggregates[0].Value != 0.0 {
			t.Fatalf("%v: the user who reads their own gets %+v", params, p)
		}
	}
	id := w.addTask(w.owner, s, a, map[string]any{"title": "owner gone", "qty": 7})
	w.removeTask(w.owner, s, a, id)
	if d := w.query(w.owner, s, a, "state", "deleted", "aggregate", "sum:qty"); d.Total != 1 || d.Aggregates[0].Value != 7.0 || d.Records[0].ID != id {
		t.Fatalf("their deleted = %+v", d)
	}
	if d := w.query(w.alice, s, a, "state", "deleted"); d.Total != 1 {
		t.Fatalf("the data manager's deleted = %+v", d)
	}
	expect(t, w.bob.do(http.MethodGet, queryURL(s, a), nil), http.StatusNotFound)
	w.mustSetRoles(w.alice, s, a, grant(w.owner.id, access.AppBuilder), grant(w.alice.id, access.AppRoles...))
	expectForbidden(t, w.owner.do(http.MethodGet, queryURL(s, a), nil), "read_records")
}

func TestQueryCursorsNeverWidenAccess(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.tasks(w.alice, s)
	for i := 1; i <= 6; i++ {
		w.addTask(w.alice, s, a, map[string]any{"title": fmt.Sprintf("alice %d", i), "qty": i})
		w.addTask(w.owner, s, a, map[string]any{"title": fmt.Sprintf("owner %d", i), "qty": i})
	}
	w.mustSetRoles(w.alice, s, a, grant(w.owner.id, access.AppUser), grant(w.alice.id, access.AppRoles...))

	first := w.query(w.alice, s, a, "limit", "2", "sort", "qty")
	mine := w.query(w.owner, s, a, "limit", "2", "sort", "qty")
	for name, cursor := range map[string]string{
		"a data manager's cursor, used by the user who reads their own": *first.NextCursor,
		"nonsense":   "not a cursor",
		"empty json": base64.RawURLEncoding.EncodeToString([]byte(`{}`)),
		"tampered":   tamper(*mine.NextCursor),
		"too long":   strings.Repeat("A", 5000),
	} {
		rec := w.owner.do(http.MethodGet, queryURL(s, a, "limit", "2", "sort", "qty", "cursor", cursor), nil)
		expect(t, rec, http.StatusBadRequest)
		if errorCode(t, rec) != "app_query_cursor" {
			t.Fatalf("%s: %s", name, rec.Body)
		}
	}
	rec := w.owner.do(http.MethodGet, queryURL(s, a, "limit", "2", "sort", "title", "cursor", *mine.NextCursor), nil)
	if errorCode(t, rec) != "app_query_cursor" {
		t.Fatalf("a cursor of another sort: %s", rec.Body)
	}
	v := visibility{scope: access.ScopeOwn, user: w.owner.id}
	def := tasksDef(t)
	p, err := Query{Sort: "qty"}.plan(def, v, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{0, 1, 5, 1 << 40} {
		forged, err := writeCursor(p.print, p.key, float64(0), id)
		if err != nil {
			t.Fatal(err)
		}
		got := w.query(w.owner, s, a, "sort", "qty", "limit", "200", "cursor", forged)
		for _, r := range got.Records {
			if r.CreatedBy == nil || r.CreatedBy.ID != w.owner.id {
				t.Fatalf("a forged cursor reached a record of %+v", r.CreatedBy)
			}
		}
		if got.Total != 6 {
			t.Fatalf("the total follows the scope, not the cursor: %d", got.Total)
		}
	}
}

func tamper(c string) string {
	b := []byte(c)
	i := len(b) / 2
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	return string(b)
}

func TestQueryLeavesOutHiddenDeletedAndWrongTypedValues(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.tasks(w.alice, s)
	ids := map[string]int64{}
	for _, r := range []struct {
		title string
		qty   any
	}{{"one", 1}, {"two", 2}, {"three", 3}, {"four", 4}, {"five", 5}, {"six", 6}} {
		ids[r.title] = w.addTask(w.alice, s, a, map[string]any{"title": r.title, "qty": r.qty, "kind": "bug"})
	}
	for title, v := range map[string]string{"two": `"12"`, "three": `true`, "four": `"abc"`, "five": `null`} {
		if _, err := w.e.db.Exec(`UPDATE app_rows SET data = json_set(data, '$.qty', json(?)) WHERE id = ?`, v, ids[title]); err != nil {
			t.Fatal(err)
		}
	}
	w.removeTask(w.alice, s, a, ids["six"])

	p := w.query(w.alice, s, a, "aggregate", "sum:qty", "aggregate", "count:qty", "aggregate", "max:qty", "aggregate", "avg:qty")
	if p.Total != 5 || p.Aggregates[0].Value != 1.0 || p.Aggregates[1].Value != 1.0 || p.Aggregates[2].Value != 1.0 || p.Aggregates[3].Value != 1.0 {
		t.Fatalf("aggregates over the one number and the four that aren't = %+v", p)
	}
	if got := w.query(w.alice, s, a, "filter", "qty:gte:0").ids(); len(got) != 1 || got[0] != ids["one"] {
		t.Fatalf("qty >= 0 matches %v", got)
	}
	if got := w.query(w.alice, s, a, "filter", "qty:ne:1"); got.Total != 4 {
		t.Fatalf("qty != 1 matches %d", got.Total)
	}
	if got := w.query(w.alice, s, a, "sort", "qty").ids(); got[0] != ids["one"] || len(got) != 5 {
		t.Fatalf("by qty = %v", got)
	}
	if d := w.query(w.alice, s, a, "state", "deleted", "aggregate", "sum:qty"); d.Total != 1 || d.Aggregates[0].Value != 6.0 {
		t.Fatalf("the deleted = %+v", d)
	}

	cur := w.view(w.alice, s, a)
	cur = w.mustPatchDraft(w.alice, s, cur, without(*cur.Draft, "kind"))
	for _, params := range [][]string{{"filter", "kind:eq:bug"}, {"sort", "kind"}, {"aggregate", "count:kind"}} {
		rec := w.alice.do(http.MethodGet, queryURL(s, a, params...), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("the live release still declares kind: %s", rec.Body)
		}
	}
	expect(t, w.tryPublish(w.alice, s, cur, SchemaChange{ChangeFieldRemoved, "kind"}), http.StatusOK)
	for _, params := range [][]string{{"filter", "kind:eq:bug"}, {"sort", "kind"}, {"aggregate", "count:kind"}, {"filter", "draftonly:eq:x"}} {
		rec := w.alice.do(http.MethodGet, queryURL(s, a, params...), nil)
		expect(t, rec, http.StatusBadRequest)
		if errorCode(t, rec) != "app_query_field" {
			t.Fatalf("%v: %s", params, rec.Body)
		}
	}
	if q := w.query(w.alice, s, a, "q", "bug"); q.Total != 0 {
		t.Fatalf("a search found the removed field's values: %d", q.Total)
	}
	for _, r := range w.query(w.alice, s, a).Records {
		if _, ok := r.Data["kind"]; ok {
			t.Fatalf("a record shows the removed field: %v", r.Data)
		}
	}
}

func TestQueryTypedValuesAgreeWithTheGoRules(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.tasks(w.alice, s)
	id := w.addTask(w.alice, s, a, map[string]any{"title": "x"})
	values := map[string][]string{
		TypeNumber:  {`0`, `-3`, `2.5`, `1e300`, `"12"`, `"abc"`, `true`, `null`, `[1]`, `{}`, `""`},
		TypeBoolean: {`true`, `false`, `"true"`, `1`, `0`, `null`, `"x"`},
		TypeString:  {`"x"`, `" x"`, `""`, `"  "`, `1`, `true`, `null`, `[]`, `"ação"`, `"\u00a0"`, `"\u3000\u2003"`, `"\u0085\u2028 "`, `"\u00a0x"`},
		TypeChoice:  {`"bug"`, `"gone"`, `""`, `3`, `null`, `"\u00a0"`},
		TypeDate:    {`"2026-01-05"`, `"2024-02-29"`, `"2023-02-29"`, `"2026-13-01"`, `"2026-1-5"`, `"2026-01-05T10:00"`, `"0000-01-01"`, `5`, `null`, `""`, `"tomorrow"`},
		TypeTime:    {`"00:00"`, `"23:59"`, `"24:00"`, `"12:60"`, `"7:30"`, `"07:30"`, `"07:30:00"`, `5`, `null`, `""`},
	}
	for _, f := range tasksDef(t).Collection.Fields {
		for _, v := range values[f.Type] {
			if _, err := w.e.db.Exec(`UPDATE app_rows SET data = json_set(data, ?, json(?)) WHERE id = ?`, "$."+f.Name, v, id); err != nil {
				t.Fatal(err)
			}
			var got bool
			if err := w.e.db.QueryRow(`SELECT `+typed(f)+` IS NOT NULL FROM app_rows r WHERE id = ?`, id).Scan(&got); err != nil {
				t.Fatalf("%s %s: %v", f.Type, v, err)
			}
			var decoded any
			if err := json.Unmarshal([]byte(v), &decoded); err != nil {
				t.Fatal(err)
			}
			native := false
			switch decoded.(type) {
			case float64:
				native = f.Type == TypeNumber
			case bool:
				native = f.Type == TypeBoolean
			case string:
				native = f.Type != TypeNumber && f.Type != TypeBoolean
			}
			if want := native && ofType(f, decoded); got != want {
				t.Errorf("%s field holding %s: SQL says a value is %v, Go %v", f.Type, v, got, want)
			}
		}
	}
}

func TestQueryFiltersAndSearch(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.tasks(w.alice, s)
	rows := []map[string]any{
		{"title": "Fix the printer", "qty": 3, "urgent": true, "due": "2026-03-01", "at": "09:30", "kind": "bug"},
		{"title": "AÇÃO de limpeza", "qty": 1, "urgent": false, "due": "2026-03-15", "at": "14:00", "kind": "chore"},
		{"title": "Buy printer ink", "qty": 8, "urgent": false, "due": "2026-04-01", "kind": "chore"},
		{"title": "Plan the offsite", "urgent": true, "kind": "idea"},
	}
	byTitle := map[string]int64{}
	for _, r := range rows {
		byTitle[r["title"].(string)] = w.addTask(w.alice, s, a, r)
	}
	titles := func(params ...string) []string {
		t.Helper()
		var out []string
		for _, r := range w.query(w.alice, s, a, append(params, "sort", "id")...).Records {
			out = append(out, r.Data["title"].(string))
		}
		return out
	}
	for _, c := range []struct {
		params []string
		want   []string
	}{
		{[]string{"filter", "kind:eq:chore"}, []string{"AÇÃO de limpeza", "Buy printer ink"}},
		{[]string{"filter", "kind:ne:chore"}, []string{"Fix the printer", "Plan the offsite"}},
		{[]string{"filter", "qty:gt:1", "filter", "qty:lt:8"}, []string{"Fix the printer"}},
		{[]string{"filter", "qty:lte:3"}, []string{"Fix the printer", "AÇÃO de limpeza"}},
		{[]string{"filter", "urgent:eq:true"}, []string{"Fix the printer", "Plan the offsite"}},
		{[]string{"filter", "urgent:ne:true"}, []string{"AÇÃO de limpeza", "Buy printer ink"}},
		{[]string{"filter", "due:gte:2026-03-15"}, []string{"AÇÃO de limpeza", "Buy printer ink"}},
		{[]string{"filter", "due:ne:2026-03-15"}, []string{"Fix the printer", "Buy printer ink", "Plan the offsite"}},
		{[]string{"filter", "at:lt:12:00"}, []string{"Fix the printer"}},
		{[]string{"filter", "at:eq:14:00"}, []string{"AÇÃO de limpeza"}},
		{[]string{"filter", "title:contains:PRINTER"}, []string{"Fix the printer", "Buy printer ink"}},
		{[]string{"filter", "title:contains:ação"}, []string{"AÇÃO de limpeza"}},
		{[]string{"filter", "title:gt:Fix", "filter", "title:lt:Plan"}, []string{"Fix the printer"}},
		{[]string{"q", "printer"}, []string{"Fix the printer", "Buy printer ink"}},
		{[]string{"q", "printer ink"}, []string{"Buy printer ink"}},
		{[]string{"q", "INK printer"}, []string{"Buy printer ink"}},
		{[]string{"q", "chore"}, []string{"AÇÃO de limpeza", "Buy printer ink"}},
		{[]string{"q", "limpeza", "filter", "kind:eq:bug"}, nil},
		{[]string{"q", "nothing at all"}, nil},
	} {
		if got := titles(c.params...); !slices.Equal(got, c.want) {
			t.Errorf("%v = %v, want %v", c.params, got, c.want)
		}
	}
}

func TestQueryRefusesBadQueries(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.tasks(w.alice, s)
	for _, c := range []struct {
		name   string
		params []string
		code   string
	}{
		{"an unknown field", []string{"filter", "nope:eq:1"}, "app_query_field"},
		{"a field of the draft's name that is the server's", []string{"sort", "updated_at"}, "app_query_field"},
		{"deleted-at sort of live records", []string{"sort", "_deleted"}, "app_query_field"},
		{"an operator that doesn't exist", []string{"filter", "qty:like:1"}, "app_query_condition"},
		{"ordering a boolean", []string{"filter", "urgent:lt:true"}, "app_query_condition"},
		{"ordering a choice", []string{"filter", "kind:gt:bug"}, "app_query_condition"},
		{"contains on a number", []string{"filter", "qty:contains:1"}, "app_query_condition"},
		{"a number that isn't", []string{"filter", "qty:eq:lots"}, "app_query_condition"},
		{"a date that isn't", []string{"filter", "due:eq:2026-02-30"}, "app_query_condition"},
		{"no value", []string{"filter", "qty:eq:"}, "app_query_condition"},
		{"a filter without parts", []string{"filter", "qty"}, "bad_request"},
		{"sum of text", []string{"aggregate", "sum:title"}, "app_query_field"},
		{"avg of a date", []string{"aggregate", "avg:due"}, "app_query_field"},
		{"min of a boolean", []string{"aggregate", "min:urgent"}, "app_query_field"},
		{"sum of nothing", []string{"aggregate", "sum"}, "app_query_field"},
		{"an aggregate that doesn't exist", []string{"aggregate", "median:qty"}, "bad_request"},
		{"a limit over the maximum", []string{"limit", "201"}, "bad_request"},
		{"a negative limit", []string{"limit", "-1"}, "bad_request"},
		{"a limit that isn't a number", []string{"limit", "many"}, "bad_request"},
		{"a direction that doesn't exist", []string{"dir", "sideways"}, "bad_request"},
		{"a state that doesn't exist", []string{"state", "all"}, "bad_request"},
		{"too many filters", []string{"filter", "qty:eq:1", "filter", "qty:eq:1", "filter", "qty:eq:1", "filter", "qty:eq:1", "filter", "qty:eq:1",
			"filter", "qty:eq:1", "filter", "qty:eq:1", "filter", "qty:eq:1", "filter", "qty:eq:1"}, "bad_request"},
		{"too many search words", []string{"q", "a b c d e f"}, "bad_request"},
	} {
		rec := w.alice.do(http.MethodGet, queryURL(s, a, c.params...), nil)
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != c.code {
			t.Errorf("%s: %d %s, want 400 %s", c.name, rec.Code, rec.Body, c.code)
		}
	}
	for i := 0; i < 60; i++ {
		w.addTask(w.alice, s, a, map[string]any{"title": fmt.Sprint(i)})
	}
	if p := w.query(w.alice, s, a); len(p.Records) != DefaultPageSize || p.Total != 60 || p.NextCursor == nil {
		t.Fatalf("a page = %d of %d", len(p.Records), p.Total)
	}
	if p := w.query(w.alice, s, a, "limit", "200"); len(p.Records) != 60 || p.NextCursor != nil {
		t.Fatalf("a page of 200 = %d, cursor %v", len(p.Records), p.NextCursor)
	}
}

func TestQueryRouteChecksAccess(t *testing.T) {
	w := newWorld(t)
	for _, c := range []struct {
		u      *user
		s      access.Space
		a      App
		status int
	}{
		{w.bob, w.alicePrivate, w.aliceApp, http.StatusNotFound},
		{w.mallory, w.shared, w.sharedApp, http.StatusNotFound},
		{w.stranger, in(w.other, w.shared), w.sharedApp, http.StatusNotFound},
		{w.bob, w.bobPrivate, w.aliceApp, http.StatusNotFound},
		{w.bob, w.shared, w.sharedApp, http.StatusOK},
		{w.alice, w.shared, w.sharedApp, http.StatusOK},
	} {
		if rec := c.u.do(http.MethodGet, queryURL(c.s, c.a), nil); rec.Code != c.status {
			t.Errorf("%d, want %d: %s", rec.Code, c.status, rec.Body)
		}
	}
	expect(t, w.alice.do(http.MethodGet, queryURL(w.alicePrivate, w.sharedApp), nil), http.StatusNotFound)
	expect(t, w.alice.do(http.MethodGet, appPath(w.shared, 0, "/records/query"), nil), http.StatusNotFound)
	expect(t, w.alice.do(http.MethodGet, appsPath(w.shared, "/x/records/query"), nil), http.StatusNotFound)
	d := w.draft(w.alice, w.alicePrivate, "Unpublished")
	if p := w.query(w.alice, w.alicePrivate, d); p.Total != 0 || len(p.Records) != 0 {
		t.Fatalf("an unpublished app's query = %+v", p)
	}
}

func TestQueryRecordsChangedAtTheSameTimeKeepAStableOrder(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.tasks(w.alice, s)
	var ids []int64
	for i := 0; i < 9; i++ {
		ids = append(ids, w.addTask(w.alice, s, a, map[string]any{"title": fmt.Sprint(i)}))
	}
	if _, err := w.e.db.Exec(`UPDATE app_rows SET updated_at = '2026-01-01T00:00:00.000Z', created_at = '2026-01-01T00:00:00.000Z' WHERE app_id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	for _, order := range [][]string{nil, {"sort", "_created", "dir", "asc"}} {
		got := w.walk(w.alice, s, a, 2, nil, order...)
		want := slices.Clone(ids)
		if order == nil {
			slices.Reverse(want)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%v: paged %v, want %v", order, got, want)
		}
	}
}

func TestQueryPagesPastLongTextKeys(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.tasks(w.alice, s)
	var want []int64
	for _, title := range []string{"a", "m", "z"} {
		want = append(want, w.addTask(w.alice, s, a, map[string]any{"title": title}))
	}
	long := strings.Repeat("m<", 3000)
	if _, err := w.e.db.Exec(`UPDATE app_rows SET data = json_set(data, '$.title', ?) WHERE id = ?`, long, want[1]); err != nil {
		t.Fatal(err)
	}
	if got := w.walk(w.alice, s, a, 1, nil, "sort", "title", "dir", "asc"); !slices.Equal(got, want) {
		t.Fatalf("pages by title = %v, want %v", got, want)
	}
}

func TestQueryCursorKeepsLargeIntegersExact(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.tasks(w.alice, s)
	var want []int64
	for _, qty := range []string{"9007199254740992", "9007199254740993", "9007199254740994"} {
		id := w.addTask(w.alice, s, a, map[string]any{"title": "x"})
		if _, err := w.e.db.Exec(`UPDATE app_rows SET data = json_set(data, '$.qty', json(?)) WHERE id = ?`, qty, id); err != nil {
			t.Fatal(err)
		}
		want = append(want, id)
	}
	if got := w.walk(w.alice, s, a, 1, nil, "sort", "qty", "dir", "asc"); !slices.Equal(got, want) {
		t.Fatalf("pages by qty = %v, want %v", got, want)
	}
}

func TestQueryRefusesFieldsThatHoldFilesAndEvidence(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	def, err := Definition{
		Components: []json.RawMessage{json.RawMessage(`{"id":"root","component":"Text","text":"Visits"}`)},
		Collection: Collection{Fields: []Field{
			{Name: "title", Type: TypeString},
			{Name: "photos", Type: TypeAttachment},
			{Name: "sign", Type: TypeSignature},
			{Name: "where", Type: TypeLocation},
		}},
		Actions: map[string]Action{"add": {Kind: ActionInsert}},
	}.Check()
	if err != nil {
		t.Fatal(err)
	}
	rec := w.alice.do(http.MethodPost, appsPath(s, ""), map[string]any{"name": "Visits", "definition": def})
	expect(t, rec, http.StatusCreated)
	a := w.publish(w.alice, s, decode[App](t, rec))
	for _, c := range []struct {
		name   string
		params []string
		code   string
	}{
		{"filtering by photos", []string{"filter", "photos:eq:1"}, "app_query_condition"},
		{"filtering by a photo's id", []string{"filter", "photos:eq:" + strings.Repeat("a", 32)}, "app_query_condition"},
		{"filtering out a photo's id", []string{"filter", "photos:ne:" + strings.Repeat("a", 32)}, "app_query_condition"},
		{"filtering by photos with no value", []string{"filter", "photos:ne:"}, "app_query_condition"},
		{"filtering by a signature", []string{"filter", "sign:eq:1"}, "app_query_condition"},
		{"filtering by a location", []string{"filter", "where:eq:1"}, "app_query_condition"},
		{"sorting by photos", []string{"sort", "photos"}, "app_query_field"},
		{"sorting by a signature", []string{"sort", "sign"}, "app_query_field"},
		{"sorting by a location", []string{"sort", "where"}, "app_query_field"},
		{"counting photos", []string{"aggregate", "count:photos"}, "app_query_field"},
		{"summing photos", []string{"aggregate", "sum:photos"}, "app_query_field"},
	} {
		rec := w.alice.do(http.MethodGet, queryURL(s, a, c.params...), nil)
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != c.code {
			t.Errorf("%s: %d %s, want 400 %s", c.name, rec.Code, rec.Body, c.code)
		}
	}
	expect(t, w.alice.do(http.MethodGet, queryURL(s, a, "filter", "title:eq:x"), nil), http.StatusOK)
}
