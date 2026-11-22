package search

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"taskto.ai/taskto/internal/knowledge"
)

func TestSegment(t *testing.T) {
	const z = "\x1F"
	for _, c := range []struct{ in, want string }{
		{"plan the orchard, café", "plan the orchard, café"},
		{"東京都庁", "東" + z + "京" + z + "都" + z + "庁"},
		{"東京 and Kyoto", "東" + z + "京 and Kyoto"},
		{"iPhone手机2台", "iPhone" + z + "手" + z + "机" + z + "2" + z + "台"},
		{"「東京」", "「東" + z + "京」"},
		{"コーヒー", "コ" + z + "ー" + z + "ヒ" + z + "ー"},
		// A Thai character keeps its vowel and tone marks.
		{"ที่นี่กิน", "ที่" + z + "นี่" + z + "กิ" + z + "น"},
		{"ລາວ ខ្មែរ မြန်မာ", "ລ" + z + "າ" + z + "ວ " + "ខ្" + z + "មែ" + z + "រ " + "မြ" + z + "န်" + z + "မာ"},
		// Korean is written with spaces: left as it is.
		{"한국어 검색", "한국어 검색"},
	} {
		if got := segment(c.in); got != c.want {
			t.Errorf("segment(%q) = %q, want %q", c.in, got, c.want)
		}
		if got := unsegment(segment(c.in)); got != c.in {
			t.Errorf("unsegment(segment(%q)) = %q", c.in, got)
		}
	}
	// A document's own separators are spaces to the index.
	if got := cleanBody([]byte("a" + z + "b")); got != "a b" {
		t.Fatalf("cleanBody: %q", got)
	}
	// The bound on its growth, at its worst.
	worst := strings.Repeat("a東", 1000)
	if got := segment(worst); len(got) > len(worst)*3/2 {
		t.Fatalf("%d bytes from %d", len(got), len(worst))
	}
}

func TestMatchExpr(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"plan orchard", `"plan"* "orchard"*`},
		{`"small citrus" fruit`, `"small citrus" "fruit"*`},
		{"京都 タワー", `"京 都"* "タ ワ ー"*`},
		{`"東京 タワー"`, `"東 京 タ ワ ー"`},
		{"กิน", `"กิ น"*`},
		{"iPhone手机", `"iPhone 手 机"*`},
	} {
		if got := matchExpr(c.in); got != c.want {
			t.Errorf("matchExpr(%q) = %s, want %s", c.in, got, c.want)
		}
	}

	// A query of as many characters as fit is bounded in the words it asks FTS5 for, in
	// a phrase or not, and so is one of many words.
	tokens := func(match string) int {
		return len(strings.FieldsFunc(match, func(r rune) bool { return r == ' ' || r == '"' || r == '*' }))
	}
	long := strings.Repeat("東", MaxQueryLen/utf8.RuneLen('東'))
	for _, q := range []string{long, `"` + long[:len(long)-3] + `"`, strings.Repeat("a ", MaxQueryLen/2), strings.Repeat("東 ", MaxQueryLen/4)} {
		m := matchExpr(q)
		if n := tokens(m); n == 0 || n > maxTokens {
			t.Errorf("%d bytes of query gave %d words: %s", len(q), n, m)
		}
		if n := strings.Count(m, `"`) / 2; n > maxTerms {
			t.Errorf("%d bytes of query gave %d terms", len(q), n)
		}
	}
}

// TestUnspacedScripts checks that a word in Chinese, Japanese or Thai is found in the
// middle of a sentence, as is a query of one or two characters.
func TestUnspacedScripts(t *testing.T) {
	w := newWorld(t)
	sp := w.team
	put := func(name, content string) {
		t.Helper()
		must[knowledge.Result](t)(w.e.kb.Put(ctx, sp, name, []byte(content), "", by, ""))
	}
	put("zh.md", "# 会议记录\n\n我们明天在北京开会讨论新的项目计划。\n")
	put("ja.md", "# 議事録\n\n来週の会議は東京都庁の近くで行います。\n")
	put("th.md", "# บันทึก\n\nพรุ่งนี้เราจะไปกินข้าวที่ร้านอาหารใกล้บ้าน\n")
	put("th-other.md", "ก้นครัว\n")
	put("mixed.md", "The new iPhone手机 is on the desk.\n")
	put("报告/季度.md", "Quarterly numbers.\n")

	for _, c := range []struct {
		q, path, matched string
	}{
		{"北京", "zh.md", "北京"},
		// Two words next to each other are marked as one.
		{"北京 开会", "zh.md", "北京开会"},
		{"项目计划", "zh.md", "项目计划"},
		{"东京", "", ""}, // simplified 东 is another character than 東
		{"東京", "ja.md", "東京"},
		{"京都", "ja.md", "京都"},
		{"都庁の近く", "ja.md", "都庁の近く"},
		{"กินข้าว", "th.md", "กินข้าว"},
		{"ร้านอาหาร", "th.md", "ร้านอาหาร"},
		// Thai marks count: กิน, eat, isn't ก้น.
		{"กิน", "th.md", "กิน"},
		{"ก้น", "th-other.md", "ก้น"},
		// One and two characters: anywhere they are.
		{"庁", "ja.md", "庁"},
		{"明天", "zh.md", "明天"},
		{"会议", "zh.md", "会议"},
		{"手机", "mixed.md", "手机"},
		{"iphone", "mixed.md", "iPhone"},
		{"季度", "报告/季度.md", ""},
		// Words of a quoted phrase in a row, in any script.
		{`"会开 北京"`, "", ""},
		{`"北京开会"`, "zh.md", "北京开会"},
	} {
		res := w.find(sp, c.q)
		if c.path == "" {
			wantPaths(t, res)
			continue
		}
		wantPaths(t, res, c.path)
		text, matched := snippetText(res.Results[0])
		if matched != c.matched || strings.ContainsRune(text, sep) {
			t.Errorf("%q: snippet %q, matched %q, want %q", c.q, text, matched, c.matched)
		}
	}
	if r := w.find(sp, "会议").Results[0]; r.Title != "会议记录" {
		t.Fatalf("title %q", r.Title)
	}
}

// TestIndexedAgainInANewTextFormat checks the upgrade to a new textFormat as a migration
// makes it: the full-text index emptied, and every space marked for a full sync. The next
// sync indexes the text again, and keeps the embeddings, which are of the same versions.
func TestIndexedAgainInANewTextFormat(t *testing.T) {
	w := newWorld(t)
	w.setModel()
	sp := w.team
	must[knowledge.Result](t)(w.e.kb.Put(ctx, sp, "ja.md", []byte("来週の会議は東京都庁の近くで行います。"), "", by, ""))
	must[knowledge.Result](t)(w.e.kb.Put(ctx, sp, "en.md", []byte("The orchard plan."), "", by, ""))
	must[string](t)(w.e.idx.Sync(ctx, sp))
	w.e.embedAll()
	if w.e.embed.sent(string(sep)) || !w.e.embed.sent("来週の会議は東京都庁の近くで行います。") {
		t.Fatalf("embedded %q", w.e.embed.inputs)
	}
	chunks := w.e.spaceChunks(sp)
	// One more, indexed but not embedded yet when the migration runs.
	must[knowledge.Result](t)(w.e.kb.Put(ctx, sp, "zh.md", []byte("我们明天在北京开会。"), "", by, ""))
	must[string](t)(w.e.idx.Sync(ctx, sp))

	db := w.e.idx.db
	for _, q := range []string{`DELETE FROM search_fts`, `UPDATE search_docs SET text_format = 0`, `UPDATE search_spaces SET partial = 1`} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	// Until the sync, the embeddings worker has nothing to do (it would find no text).
	if n, err := w.e.idx.embedPending(ctx); n != 0 || err != nil {
		t.Fatalf("the embeddings worker took %d documents: %v", n, err)
	}

	must[string](t)(w.e.idx.Sync(ctx, sp))
	var behind int
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM search_docs WHERE text_format != ?`, textFormat).Scan(&behind)
	if behind != 0 {
		t.Fatalf("%d documents still in an older format", behind)
	}
	w.e.embedAll()
	for q, path := range map[string]string{"京都": "ja.md", "orchard": "en.md", "北京": "zh.md"} {
		if res := w.find(sp, q); len(res.Results) == 0 || res.Results[0].Path != path || res.Results[0].Match != "both" {
			t.Fatalf("%s: %+v", q, res)
		}
	}
	// Since, the new document and the searches' queries: each other document's text was
	// sent once in all.
	for _, text := range []string{"来週の会議", "The orchard plan."} {
		n := 0
		for _, in := range w.e.embed.inputs {
			if strings.Contains(in, text) {
				n++
			}
		}
		if n != 1 || w.e.spaceChunks(sp) != chunks+1 {
			t.Fatalf("%q embedded %d times; %d chunks (had %d)", text, n, w.e.spaceChunks(sp), chunks)
		}
	}
}

// TestSyncWithNothingToWrite checks syncs that find every file already indexed: of a space
// whose files were all deleted before it was ever indexed, and of one marked for a full
// sync, as a migration marks every space. Each records the commit and ends.
func TestSyncWithNothingToWrite(t *testing.T) {
	w := newWorld(t)
	sp := w.team
	must[knowledge.Result](t)(w.e.kb.Put(ctx, sp, "a.md", []byte("alpha"), "", by, ""))
	last := must[knowledge.Result](t)(w.e.kb.Delete(ctx, sp, "a.md", "", by, ""))
	sync := func() {
		t.Helper()
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if head, err := w.e.idx.Sync(sctx, sp); err != nil || head != last.Commit {
			t.Fatalf("sync: %q, %v", head, err)
		}
		var partial bool
		if err := w.e.idx.db.QueryRowContext(ctx, `SELECT partial FROM search_spaces WHERE space_id = ?`, sp.ID).Scan(&partial); err != nil || partial {
			t.Fatalf("partial %v, %v", partial, err)
		}
	}
	sync()
	if _, err := w.e.idx.db.ExecContext(ctx, `UPDATE search_spaces SET partial = 1`); err != nil {
		t.Fatal(err)
	}
	sync()
}
