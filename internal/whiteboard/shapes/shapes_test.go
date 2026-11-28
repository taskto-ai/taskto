package shapes

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func shape(id string) Shape { return Shape{ID: id, Type: "rect", Color: "blue", W: 10, H: 10} }

func ops(t *testing.T, raw string) []Op {
	t.Helper()
	var out []Op
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func ids(b Board) string {
	var out []string
	for _, s := range b.Shapes {
		out = append(out, s.ID)
	}
	return strings.Join(out, ",")
}

func TestApply(t *testing.T) {
	base := Board{Version: Version, Shapes: []Shape{shape("a"), shape("b"), shape("c")}}
	for name, c := range map[string]struct {
		ops  string
		want string
	}{
		"add appends":                                   {`[{"op":"add","shape":{"id":"d","type":"sticky","color":"amber"}}]`, "a,b,c,d"},
		"a second add of an id is ignored":              {`[{"op":"add","shape":{"id":"a","type":"ellipse","color":"pink"}}]`, "a,b,c"},
		"delete":                                        {`[{"op":"delete","id":"b"}]`, "a,c"},
		"delete of a missing id is a no-op":             {`[{"op":"delete","id":"zz"}]`, "a,b,c"},
		"update of a missing id is a no-op":             {`[{"op":"update","id":"zz","set":{"x":1}}]`, "a,b,c"},
		"order to the front":                            {`[{"op":"order","id":"a","to":"front"}]`, "b,c,a"},
		"order to the back":                             {`[{"op":"order","id":"c","to":"back"}]`, "c,a,b"},
		"order of a missing id is a no-op":              {`[{"op":"order","id":"zz","to":"back"}]`, "a,b,c"},
		"a delete then an add keeps the new shape last": {`[{"op":"delete","id":"a"},{"op":"add","shape":{"id":"a","type":"text","color":"teal"}}]`, "b,c,a"},
	} {
		got, err := Apply(base, ops(t, c.ops))
		if err != nil || ids(got) != c.want {
			t.Errorf("%s: got %q, %v; want %q", name, ids(got), err, c.want)
		}
	}
	if ids(base) != "a,b,c" {
		t.Fatalf("Apply changed its input: %s", ids(base))
	}
}

func TestLastWriterWinsPerProperty(t *testing.T) {
	b := Board{Version: Version, Shapes: []Shape{shape("a")}}
	b, err := Apply(b, ops(t, `[{"op":"update","id":"a","set":{"x":5,"color":"green"}}]`))
	if err != nil {
		t.Fatal(err)
	}
	b, err = Apply(b, ops(t, `[{"op":"update","id":"a","set":{"x":9}}]`))
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Shapes[0]; got.X != 9 || got.Color != "green" || got.W != 10 {
		t.Fatalf("got %+v: the later x wins and the other properties stay", got)
	}
}

func TestUpdatingPointsNeverChangesTheShapeItCameFrom(t *testing.T) {
	first := Shape{ID: "d", Type: "draw", Color: "blue", Points: [][2]float64{{1, 1}, {2, 2}, {3, 3}}}
	base := Board{Version: Version, Shapes: []Shape{first}}
	next, err := Apply(base, ops(t, `[{"op":"update","id":"d","set":{"points":[[9,9]]}}]`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(base.Shapes[0].Points, [][2]float64{{1, 1}, {2, 2}, {3, 3}}) || len(next.Shapes[0].Points) != 1 {
		t.Fatalf("base %v next %v", base.Shapes[0].Points, next.Shapes[0].Points)
	}
}

func TestABatchIsAllOrNothing(t *testing.T) {
	base := Board{Version: Version, Shapes: []Shape{shape("a")}}
	_, err := Apply(base, ops(t, `[{"op":"delete","id":"a"},{"op":"add","shape":{"id":"b","type":"nope","color":"blue"}}]`))
	if err == nil {
		t.Fatal("a batch with a bad shape was accepted")
	}
	if ids(base) != "a" {
		t.Fatalf("the failed batch changed the board: %s", ids(base))
	}
}

func TestRefusals(t *testing.T) {
	base := Board{Version: Version, Shapes: []Shape{shape("a")}}
	long := strings.Repeat("x", 5000)
	for name, raw := range map[string]string{
		"an unknown operation":      `[{"op":"explode","id":"a"}]`,
		"an unknown type":           `[{"op":"add","shape":{"id":"b","type":"star","color":"blue"}}]`,
		"a literal colour":          `[{"op":"add","shape":{"id":"b","type":"rect","color":"#ff0000"}}]`,
		"an id with a slash":        `[{"op":"add","shape":{"id":"a/b","type":"rect","color":"blue"}}]`,
		"points on a rectangle":     `[{"op":"add","shape":{"id":"b","type":"rect","color":"blue","points":[[1,1]]}}]`,
		"a negative size":           `[{"op":"add","shape":{"id":"b","type":"rect","color":"blue","w":-1}}]`,
		"a coordinate out of range": `[{"op":"add","shape":{"id":"b","type":"rect","color":"blue","x":1e9}}]`,
		"an id changed":             `[{"op":"update","id":"a","set":{"id":"z"}}]`,
		"a type changed":            `[{"op":"update","id":"a","set":{"type":"ellipse"}}]`,
		"a value of the wrong kind": `[{"op":"update","id":"a","set":{"x":"far"}}]`,
		"an empty update":           `[{"op":"update","id":"a","set":{}}]`,
		"an add with an id beside":  `[{"op":"add","id":"a","shape":{"id":"b","type":"rect","color":"blue"}}]`,
		"an order to nowhere":       `[{"op":"order","id":"a","to":"middle"}]`,
		"no operations":             `[]`,
		"too much text in a shape":  fmt.Sprintf(`[{"op":"add","shape":{"id":"b","type":"text","color":"blue","text":%q}}]`, strings.Repeat("x", MaxText+1)),
	} {
		if _, err := Apply(base, ops(t, raw)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := Apply(base, ops(t, fmt.Sprintf(`[{"op":"add","shape":{"id":"b","type":"text","color":"blue","text":%q}}]`, long))); err != nil {
		t.Errorf("5000 bytes of text in one shape was refused: %v", err)
	}
}

func TestLimits(t *testing.T) {
	full := Board{Version: Version}
	for i := range MaxShapes {
		full.Shapes = append(full.Shapes, shape(fmt.Sprint("s", i)))
	}
	if _, err := Apply(full, ops(t, `[{"op":"add","shape":{"id":"one-more","type":"rect","color":"blue"}}]`)); err == nil {
		t.Error("shape 2001 was accepted")
	}
	text := Board{Version: Version}
	chunk := strings.Repeat("x", 4096)
	for i := range MaxText / len(chunk) {
		s := shape(fmt.Sprint("t", i))
		s.Text = chunk
		text.Shapes = append(text.Shapes, s)
	}
	if _, err := Apply(text, ops(t, `[{"op":"add","shape":{"id":"tip","type":"text","color":"blue","text":"x"}}]`)); err == nil {
		t.Error("text beyond 64 KiB in total was accepted")
	}
	heavy := Board{Version: Version}
	pts := make([][2]float64, 2000)
	for i := range pts {
		pts[i] = [2]float64{123456.789012, 654321.987654}
	}
	for i := range 40 {
		heavy.Shapes = append(heavy.Shapes, Shape{ID: fmt.Sprint("d", i), Type: "draw", Color: "blue", Points: pts})
	}
	if _, err := Encode(heavy); err == nil {
		t.Error("a board over 1 MiB encoded")
	}
	if _, err := Apply(Board{Version: Version, Shapes: heavy.Shapes[:30]}, ops(t, `[{"op":"add","shape":{"id":"x","type":"rect","color":"blue"}}]`)); err == nil {
		t.Error("an op that takes the file over 1 MiB was accepted")
	}
}

func TestParse(t *testing.T) {
	good := `{"version":1,"shapes":[{"id":"a","type":"rect","x":1,"y":2,"w":3,"h":4,"rotation":0,"color":"blue"}]}`
	if b, err := Parse([]byte(good)); err != nil || len(b.Shapes) != 1 {
		t.Fatalf("a good board: %v %v", b, err)
	}
	if b, err := Parse([]byte(`{"version":1,"shapes":null}`)); err != nil || b.Shapes == nil {
		t.Fatalf("null shapes should be an empty list: %v %v", b, err)
	}
	for name, raw := range map[string]string{
		"a wrong version":  `{"version":2,"shapes":[]}`,
		"an unknown field": `{"version":1,"shapes":[],"extra":1}`,
		"trailing data":    `{"version":1,"shapes":[]} {}`,
		"a duplicate id":   `{"version":1,"shapes":[{"id":"a","type":"rect","color":"blue"},{"id":"a","type":"rect","color":"blue"}]}`,
		"not JSON":         `hello`,
		"empty":            ``,
		"too large":        `{"version":1,"shapes":[],"pad":"` + strings.Repeat("x", MaxFileBytes) + `"}`,
	} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestIsPath(t *testing.T) {
	for name, want := range map[string]bool{"a.whiteboard.json": true, "dir/B.Whiteboard.JSON": true, "a.json": false, "a.whiteboard.json.md": false, "whiteboard.json": false} {
		if IsPath(name) != want {
			t.Errorf("IsPath(%q) = %v", name, !want)
		}
	}
}
