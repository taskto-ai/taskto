package boards

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/apierr"
)

func strp(s string) *string { return &s }

func TestDueAndLabelsSetAndClear(t *testing.T) {
	w := newWorld(t)
	cards := boardPath(w.shared, w.team.ID, "/cards")

	if c := w.teamCard; c.Due != nil || c.Labels == nil || len(c.Labels) != 0 {
		t.Fatalf("a card without due or labels: due %v, labels %#v", c.Due, c.Labels)
	}

	c := w.createCard(w.alice, w.shared, w.team, map[string]any{
		"title": "Ship it", "due": "2026-03-31", "labels": []string{" Launch ", "bug", "LAUNCH", "two  words"},
	})
	if c.Due == nil || *c.Due != "2026-03-31" || !reflect.DeepEqual(c.Labels, []string{"bug", "launch", "two words"}) {
		t.Fatalf("created card: due %v, labels %v", c.Due, c.Labels)
	}
	if got := w.card(w.alice, w.shared, c); got.Due == nil || *got.Due != "2026-03-31" || len(got.Labels) != 3 {
		t.Fatalf("read back: %+v", got)
	}

	path := cardPath(w.shared, w.team.ID, c.ID, "")
	rec := w.alice.do(http.MethodPatch, path, map[string]any{"version": c.Version, "due": "2026-04-01"})
	expect(t, rec, http.StatusOK)
	c = decode[Card](t, rec)
	if *c.Due != "2026-04-01" || len(c.Labels) != 3 || c.Version != 2 {
		t.Fatalf("after changing due: %+v", c)
	}
	rec = w.alice.do(http.MethodPatch, path, map[string]any{"version": c.Version, "labels": []string{"Docs"}})
	expect(t, rec, http.StatusOK)
	c = decode[Card](t, rec)
	if *c.Due != "2026-04-01" || !reflect.DeepEqual(c.Labels, []string{"docs"}) || c.Version != 3 {
		t.Fatalf("after changing labels: %+v", c)
	}

	rec = w.owner.do(http.MethodPatch, path, map[string]any{"version": 1, "due": nil, "labels": []string{}})
	expect(t, rec, http.StatusConflict)
	reply := decode[struct {
		Code    string
		Current Card
	}](t, rec)
	if reply.Code != "card_changed" || reply.Current.Due == nil || *reply.Current.Due != "2026-04-01" || len(reply.Current.Labels) != 1 {
		t.Fatalf("409 reply: %+v", reply)
	}

	rec = w.alice.do(http.MethodPatch, path, map[string]any{"version": 3, "due": nil, "labels": []string{}})
	expect(t, rec, http.StatusOK)
	c = decode[Card](t, rec)
	if c.Due != nil || c.Labels == nil || len(c.Labels) != 0 {
		t.Fatalf("after clearing: due %v, labels %#v", c.Due, c.Labels)
	}

	rec = w.alice.do(http.MethodPatch, path, map[string]any{"version": 4, "due": "2026-05-05", "labels": []string{"a"}})
	expect(t, rec, http.StatusOK)
	rec = w.alice.do(http.MethodPatch, path, map[string]any{"version": 5, "title": "Renamed"})
	expect(t, rec, http.StatusOK)
	if c = decode[Card](t, rec); c.Due == nil || *c.Due != "2026-05-05" || !reflect.DeepEqual(c.Labels, []string{"a"}) {
		t.Fatalf("after renaming: %+v", c)
	}

	expect(t, w.alice.do(http.MethodPost, cards, map[string]any{"title": "x", "due": nil, "labels": nil}), http.StatusCreated)
}

func TestDueAndLabelsValidation(t *testing.T) {
	w := newWorld(t)
	path := cardPath(w.shared, w.team.ID, w.teamCard.ID, "")
	tooMany := make([]string, maxLabels+1)
	for i := range tooMany {
		tooMany[i] = "label" + strings.Repeat("x", i)
	}
	for name, tt := range map[string]struct {
		body   map[string]any
		code   string
		params map[string]any
	}{
		"not a date":      {map[string]any{"due": "tomorrow"}, "card_due_invalid", nil},
		"no such day":     {map[string]any{"due": "2026-02-30"}, "card_due_invalid", nil},
		"not padded":      {map[string]any{"due": "2026-3-1"}, "card_due_invalid", nil},
		"with a time":     {map[string]any{"due": "2026-03-31T10:00:00Z"}, "card_due_invalid", nil},
		"empty":           {map[string]any{"due": ""}, "card_due_invalid", nil},
		"blank label":     {map[string]any{"labels": []string{" "}}, "card_label_invalid", map[string]any{"max": float64(maxLabelLen)}},
		"long label":      {map[string]any{"labels": []string{strings.Repeat("l", maxLabelLen+1)}}, "card_label_invalid", map[string]any{"max": float64(maxLabelLen)}},
		"two-line label":  {map[string]any{"labels": []string{"a\u0007b"}}, "card_label_invalid", map[string]any{"max": float64(maxLabelLen)}},
		"too many labels": {map[string]any{"labels": tooMany}, "too_many_labels", map[string]any{"max": float64(maxLabels)}},
	} {
		t.Run(name, func(t *testing.T) {
			tt.body["version"] = 1
			rec := w.alice.do(http.MethodPatch, path, tt.body)
			expect(t, rec, http.StatusBadRequest)
			reply := decode[struct {
				Code   string
				Params map[string]any
			}](t, rec)
			if reply.Code != tt.code || !reflect.DeepEqual(reply.Params, tt.params) {
				t.Fatalf("reply = %+v, want %s %v", reply, tt.code, tt.params)
			}
			body := map[string]any{"title": "new"}
			for k, v := range tt.body {
				if k != "version" {
					body[k] = v
				}
			}
			expect(t, w.alice.do(http.MethodPost, boardPath(w.shared, w.team.ID, "/cards"), body), http.StatusBadRequest)
		})
	}
	if got := w.cards(w.alice, w.shared, w.team, ""); len(got) != 1 {
		t.Errorf("rejected creates left cards behind: %v", titles(got))
	}
	ok := make([]string, maxLabels)
	for i := range ok {
		ok[i] = strings.Repeat(string(rune('a'+i)), maxLabelLen)
	}
	expect(t, w.alice.do(http.MethodPatch, path, map[string]any{"version": 1, "due": "2024-02-29", "labels": ok}), http.StatusOK)
}

func TestFilterAndSortByDueAndLabel(t *testing.T) {
	w := newWorld(t)
	mk := func(title, col string, due *string, labels ...string) {
		t.Helper()
		body := map[string]any{"title": title, "column": col, "labels": labels}
		if due != nil {
			body["due"] = *due
		}
		w.createCard(w.alice, w.shared, w.team, body)
	}
	mk("late bug", "todo", strp("2026-03-01"), "bug")
	mk("soon bug urgent", "doing", strp("2026-03-20"), "bug", "urgent")
	mk("later", "backlog", strp("2026-04-10"), "docs")
	mk("late but done", "done", strp("2026-02-01"), "bug")
	mk("undated urgent", "todo", nil, "urgent")
	archived := w.createCard(w.alice, w.shared, w.team, map[string]any{"title": "late archived", "due": "2026-01-01"})
	expect(t, w.alice.do(http.MethodPost, cardPath(w.shared, w.team.ID, archived.ID, "/archive"), map[string]any{"version": archived.Version}), http.StatusOK)

	list := func(query string) []string {
		t.Helper()
		return titles(w.cards(w.alice, w.shared, w.team, query))
	}
	for _, tt := range []struct {
		query string
		want  []string
	}{
		{"", []string{"Team card", "later", "late bug", "undated urgent", "soon bug urgent", "late but done"}},
		{"?label=bug", []string{"late bug", "soon bug urgent", "late but done"}},
		{"?label=BUG", []string{"late bug", "soon bug urgent", "late but done"}},
		{"?label=bug&label=urgent", []string{"soon bug urgent"}},
		{"?label=nothing", []string{}},
		{"?due_before=2026-03-01", []string{"late bug", "late but done"}},
		{"?due_after=2026-03-20", []string{"later", "soon bug urgent"}},
		{"?due_after=2026-03-01&due_before=2026-03-20", []string{"late bug", "soon bug urgent"}},
		{"?overdue=true&today=2026-03-21", []string{"late bug", "soon bug urgent"}},
		{"?overdue=true&today=2026-03-01", []string{}},
		{"?overdue=true&today=2026-03-02&label=urgent", []string{}},
		{"?overdue=false", []string{"Team card", "later", "late bug", "undated urgent", "soon bug urgent", "late but done"}},
		{"?sort=due", []string{"late but done", "late bug", "soon bug urgent", "later", "Team card", "undated urgent"}},
		{"?sort=board", []string{"Team card", "later", "late bug", "undated urgent", "soon bug urgent", "late but done"}},
		{"?sort=due&label=bug&column=todo", []string{"late bug"}},
		{"?column=archived&overdue=true&today=2026-06-01", []string{}},
		{"?column=archived&due_before=2026-06-01", []string{"late archived"}},
	} {
		if got := list(tt.query); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s = %v, want %v", tt.query, got, tt.want)
		}
	}

	w.createCard(w.alice, w.shared, w.team, map[string]any{"title": "ancient", "due": "2001-01-01"})
	w.createCard(w.alice, w.shared, w.team, map[string]any{"title": "distant", "due": "2999-12-31"})
	got := list("?overdue=true")
	if !contains(got, "ancient") || contains(got, "distant") || contains(got, "late but done") {
		t.Errorf("overdue without today = %v", got)
	}

	for _, q := range []string{"?due_before=soon", "?due_after=2026-13-01", "?today=x&overdue=true", "?overdue=maybe", "?sort=rank",
		"?label=" + url.QueryEscape(strings.Repeat("l", maxLabelLen+1)), "?label=%20"} {
		expect(t, w.alice.do(http.MethodGet, boardPath(w.shared, w.team.ID, "/cards"+q), nil), http.StatusBadRequest)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestDueAndLabelsAreOnlyForWhoCanSeeTheCard(t *testing.T) {
	w := newWorld(t)
	secret := w.createCard(w.alice, w.alicePrivate, w.alicePersonal, map[string]any{
		"title": "private plan", "due": "2026-09-09", "labels": []string{"secret"},
	})
	shared := w.createCard(w.alice, w.shared, w.team, map[string]any{"title": "team plan", "due": "2026-08-08", "labels": []string{"team"}})

	for name, who := range map[string]*user{"admin": w.owner, "viewer of shared": w.bob, "member": w.mallory, "other workspace": w.stranger} {
		t.Run(name, func(t *testing.T) {
			for _, q := range []string{"", "?label=secret", "?due_before=2026-12-31", "?overdue=true&today=2027-01-01", "?sort=due"} {
				expect(t, who.do(http.MethodGet, boardPath(w.alicePrivate, w.alicePersonal.ID, "/cards"+q), nil), http.StatusNotFound)
			}
			expect(t, who.do(http.MethodGet, cardPath(w.alicePrivate, w.alicePersonal.ID, secret.ID, ""), nil), http.StatusNotFound)
			expect(t, who.do(http.MethodPatch, cardPath(w.alicePrivate, w.alicePersonal.ID, secret.ID, ""),
				map[string]any{"version": secret.Version, "due": nil, "labels": []string{"mine"}}), http.StatusNotFound)
			expect(t, who.do(http.MethodPost, boardPath(w.alicePrivate, w.alicePersonal.ID, "/cards"),
				map[string]any{"title": "x", "due": "2026-01-01", "labels": []string{"mine"}}), http.StatusNotFound)
		})
	}
	if got := w.card(w.alice, w.alicePrivate, secret); got.Version != secret.Version || *got.Due != "2026-09-09" || !reflect.DeepEqual(got.Labels, []string{"secret"}) {
		t.Errorf("the private card changed: %+v", got)
	}

	for _, who := range []*user{w.mallory, w.stranger} {
		expect(t, who.do(http.MethodGet, boardPath(w.shared, w.team.ID, "/cards?label=team"), nil), http.StatusNotFound)
		expect(t, who.do(http.MethodPatch, cardPath(w.shared, w.team.ID, shared.ID, ""),
			map[string]any{"version": shared.Version, "due": "2030-01-01"}), http.StatusNotFound)
	}
	expect(t, w.bob.do(http.MethodPatch, cardPath(w.shared, w.team.ID, shared.ID, ""),
		map[string]any{"version": shared.Version, "due": nil, "labels": []string{}}), http.StatusForbidden)
	if got := w.cards(w.bob, w.shared, w.team, "?label=team&due_before=2026-08-08"); len(got) != 1 || *got[0].Due != "2026-08-08" {
		t.Errorf("the viewer reads %v", titles(got))
	}
	if got := w.card(w.alice, w.shared, shared); got.Version != shared.Version || len(got.Labels) != 1 {
		t.Errorf("the team card changed: %+v", got)
	}

	w.createCard(w.alice, w.shared, w.team2, map[string]any{"title": "ops", "labels": []string{"team"}})
	if got := w.cards(w.alice, w.shared, w.team, "?label=team"); !reflect.DeepEqual(titles(got), []string{"team plan"}) {
		t.Errorf("label=team on the team board = %v", titles(got))
	}
	if got := w.cards(w.alice, w.alicePrivate, w.alicePersonal, "?label=team"); len(got) != 0 {
		t.Errorf("label=team on the personal board = %v", titles(got))
	}

	expect(t, w.alice.do(http.MethodPatch, cardPath(w.archived, w.old.ID, w.oldCard.ID, ""),
		map[string]any{"version": w.oldCard.Version, "labels": []string{"x"}}), http.StatusForbidden)
}

func TestDueMigrationKeepsTheShape(t *testing.T) {
	w := newWorld(t)
	for _, bad := range []string{"tomorrow", "2026-3-1", "20260301", "2026/03/01", ""} {
		if _, err := w.e.db.Exec(`UPDATE cards SET due = ? WHERE id = ?`, bad, w.teamCard.ID); err == nil {
			t.Errorf("the database took the due date %q", bad)
		}
	}
	if _, err := w.e.db.Exec(`UPDATE cards SET due = '2026-03-01' WHERE id = ?`, w.teamCard.ID); err != nil {
		t.Error(err)
	}
	if _, err := w.e.db.Exec(`INSERT INTO card_labels (card_id, label) VALUES (?, 'x')`, w.teamCard.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.db.Exec(`DELETE FROM cards WHERE id = ?`, w.teamCard.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM card_labels`).Scan(&n); err != nil || n != 0 {
		t.Errorf("labels left after their card: %d, %v", n, err)
	}
}

func TestFilteredListIsOneSnapshot(t *testing.T) {
	w := newWorld(t)
	c := w.createCard(w.alice, w.shared, w.team, map[string]any{"title": "flip", "labels": []string{"bug"}})
	path := cardPath(w.shared, w.team.ID, c.ID, "")

	stop := make(chan struct{})
	done := make(chan error, 1)
	var stopped bool
	finish := func() error {
		if !stopped {
			stopped = true
			close(stop)
		}
		return <-done
	}
	defer func() { _ = finish() }()
	go func() {
		defer close(done)
		version, label := c.Version, "docs"
		for {
			select {
			case <-stop:
				return
			default:
			}
			rec := w.alice.do(http.MethodPatch, path, map[string]any{"version": version, "labels": []string{label}})
			if rec.Code != http.StatusOK {
				done <- fmt.Errorf("edit: %d %s", rec.Code, rec.Body)
				return
			}
			version = decode[Card](t, rec).Version
			label = map[string]string{"docs": "bug", "bug": "docs"}[label]
		}
	}()
	for i := 0; i < 400; i++ {
		for _, card := range w.cards(w.alice, w.shared, w.team, "?label=bug") {
			if !slices.Contains(card.Labels, "bug") {
				t.Fatalf("label=bug returned %q with labels %v", card.Title, card.Labels)
			}
		}
	}
	if err := finish(); err != nil {
		t.Fatal(err)
	}
}

func TestTooManyFilterLabels(t *testing.T) {
	w := newWorld(t)
	many := make([]string, maxLabels+1)
	for i := range many {
		many[i] = fmt.Sprint("l", i)
	}
	_, err := w.e.s.ListCards(context.Background(), agentFor(w.alice), w.shared.ID, w.team.ID, Filter{Labels: many})
	var ie *invalidError
	if !errors.As(err, &ie) || ie.code != apierr.CodeTooManyLabels {
		t.Fatalf("ListCards with %d labels = %v, want too_many_labels", len(many), err)
	}
	if _, err := w.e.s.ListCards(context.Background(), agentFor(w.alice), w.shared.ID, w.team.ID, Filter{Labels: many[:maxLabels]}); err != nil {
		t.Fatalf("ListCards with %d labels: %v", maxLabels, err)
	}
	q := ""
	for _, l := range many {
		q += "&label=" + l
	}
	rec := w.alice.do(http.MethodGet, boardPath(w.shared, w.team.ID, "/cards?"+q[1:]), nil)
	expect(t, rec, http.StatusBadRequest)
	if got := decode[struct{ Code string }](t, rec).Code; got != "too_many_labels" {
		t.Errorf("HTTP code = %q", got)
	}
}

func TestLabelsNormalizeUnicode(t *testing.T) {
	w := newWorld(t)
	c := w.createCard(w.alice, w.shared, w.team, map[string]any{
		"title": "unicode", "labels": []string{"café", "café", "ΟΣ", "οσ", "ος", "STRASSE", "straße"},
	})
	if want := []string{"café", "strasse", "οσ"}; !reflect.DeepEqual(c.Labels, want) {
		t.Fatalf("labels = %q, want %q", c.Labels, want)
	}
	for _, f := range []string{"café", "CAFÉ", "ος", "ΟΣ", "STRASSE"} {
		if got := titles(w.cards(w.alice, w.shared, w.team, "?label="+url.QueryEscape(f))); !reflect.DeepEqual(got, []string{"unicode"}) {
			t.Errorf("label=%q = %v", f, got)
		}
	}
}
