package shapes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"

	"taskto.ai/taskto/internal/apierr"
)

const (
	Suffix       = ".whiteboard.json"
	Version      = 1
	MaxShapes    = 2000
	MaxText      = 64 << 10
	MaxFileBytes = 1 << 20
	MaxOps       = 100
	maxCoord     = 1e6
)

var (
	types    = []string{"sticky", "rect", "ellipse", "arrow", "text", "draw"}
	colors   = []string{"amber", "blue", "green", "pink", "teal", "violet"}
	settable = []string{"x", "y", "w", "h", "rotation", "text", "color", "points"}
	idForm   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

type Shape struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	X        float64      `json:"x"`
	Y        float64      `json:"y"`
	W        float64      `json:"w"`
	H        float64      `json:"h"`
	Rotation float64      `json:"rotation"`
	Text     string       `json:"text,omitempty"`
	Color    string       `json:"color"`
	Points   [][2]float64 `json:"points,omitempty"`
}

type Board struct {
	Version int     `json:"version"`
	Shapes  []Shape `json:"shapes"`
}

type Op struct {
	Op    string                     `json:"op"`
	Shape *Shape                     `json:"shape,omitempty"`
	ID    string                     `json:"id,omitempty"`
	Set   map[string]json.RawMessage `json:"set,omitempty"`
	To    string                     `json:"to,omitempty"`
}

func IsPath(name string) bool { return strings.HasSuffix(strings.ToLower(name), Suffix) }

func invalid(format string, args ...any) error {
	return apierr.New(400, apierr.CodeWhiteboardInvalid, fmt.Sprintf(format, args...))
}

func limit(format string, args ...any) error {
	return apierr.New(413, apierr.CodeWhiteboardLimit, fmt.Sprintf(format, args...))
}

func Parse(content []byte) (Board, error) {
	if len(content) > MaxFileBytes {
		return Board{}, limit("a whiteboard is at most %d bytes", MaxFileBytes)
	}
	var b Board
	dec := json.NewDecoder(bytes.NewReader(content))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return Board{}, invalid("not a whiteboard: %v", err)
	}
	if _, err := dec.Token(); err == nil {
		return Board{}, invalid("not a whiteboard: one object expected")
	}
	if err := b.check(); err != nil {
		return Board{}, err
	}
	if b.Shapes == nil {
		b.Shapes = []Shape{}
	}
	return b, nil
}

func Encode(b Board) ([]byte, error) {
	out, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	if len(out) > MaxFileBytes {
		return nil, limit("a whiteboard is at most %d bytes", MaxFileBytes)
	}
	return out, nil
}

func (b Board) check() error {
	if b.Version != Version {
		return invalid("whiteboard version %d is not supported", b.Version)
	}
	if len(b.Shapes) > MaxShapes {
		return limit("a whiteboard has at most %d shapes", MaxShapes)
	}
	seen := make(map[string]struct{}, len(b.Shapes))
	text := 0
	for _, s := range b.Shapes {
		if err := s.check(); err != nil {
			return err
		}
		if _, dup := seen[s.ID]; dup {
			return invalid("shape %q appears twice", s.ID)
		}
		seen[s.ID] = struct{}{}
		text += len(s.Text)
	}
	if text > MaxText {
		return limit("the text of a whiteboard is at most %d bytes", MaxText)
	}
	return nil
}

func (s Shape) check() error {
	switch {
	case !idForm.MatchString(s.ID):
		return invalid("a shape's id is 1 to 64 letters, digits, - or _")
	case !slices.Contains(types, s.Type):
		return invalid("shape %q has an unknown type", s.ID)
	case !slices.Contains(colors, s.Color):
		return invalid("shape %q has an unknown color", s.ID)
	case len(s.Points) > 0 && s.Type != "arrow" && s.Type != "draw":
		return invalid("only an arrow or a drawing has points")
	case s.W < 0 || s.H < 0:
		return invalid("shape %q has a negative size", s.ID)
	}
	numbers := []float64{s.X, s.Y, s.W, s.H, s.Rotation}
	for _, p := range s.Points {
		numbers = append(numbers, p[0], p[1])
	}
	for _, n := range numbers {
		if math.Abs(n) > maxCoord {
			return invalid("shape %q has a number out of range", s.ID)
		}
	}
	return nil
}

func (s Shape) merge(set map[string]json.RawMessage) (Shape, error) {
	fields := map[string]json.RawMessage{}
	current, err := json.Marshal(s)
	if err == nil {
		err = json.Unmarshal(current, &fields)
	}
	if err != nil {
		return Shape{}, err
	}
	for key, value := range set {
		if !slices.Contains(settable, key) {
			return Shape{}, invalid("%q can't be changed", key)
		}
		fields[key] = value
	}
	merged, err := json.Marshal(fields)
	if err != nil {
		return Shape{}, err
	}
	var out Shape
	dec := json.NewDecoder(bytes.NewReader(merged))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return Shape{}, invalid("a property has a value of the wrong kind")
	}
	return out, out.check()
}

func Apply(b Board, ops []Op) (Board, error) {
	if len(ops) == 0 || len(ops) > MaxOps {
		return Board{}, invalid("send between 1 and %d operations", MaxOps)
	}
	next := Board{Version: Version, Shapes: slices.Clone(b.Shapes)}
	for _, op := range ops {
		if err := next.apply(op); err != nil {
			return Board{}, err
		}
	}
	if err := next.check(); err != nil {
		return Board{}, err
	}
	if _, err := Encode(next); err != nil {
		return Board{}, err
	}
	return next, nil
}

func (b *Board) index(id string) int {
	return slices.IndexFunc(b.Shapes, func(s Shape) bool { return s.ID == id })
}

func (b *Board) apply(op Op) error {
	switch op.Op {
	case "add":
		if op.Shape == nil || op.ID != "" || op.Set != nil || op.To != "" {
			return invalid("add takes a shape")
		}
		if err := op.Shape.check(); err != nil {
			return err
		}
		if b.index(op.Shape.ID) < 0 {
			b.Shapes = append(b.Shapes, *op.Shape)
		}
	case "update":
		if op.Shape != nil || op.To != "" || len(op.Set) == 0 {
			return invalid("update takes an id and the properties to set")
		}
		i := b.index(op.ID)
		if i < 0 {
			return nil
		}
		s, err := b.Shapes[i].merge(op.Set)
		if err != nil {
			return err
		}
		b.Shapes[i] = s
	case "delete":
		if op.Shape != nil || op.Set != nil || op.To != "" {
			return invalid("delete takes an id")
		}
		if i := b.index(op.ID); i >= 0 {
			b.Shapes = slices.Delete(b.Shapes, i, i+1)
		}
	case "order":
		if op.Shape != nil || op.Set != nil || (op.To != "front" && op.To != "back") {
			return invalid("order takes an id and to: front or back")
		}
		if i := b.index(op.ID); i >= 0 {
			s := b.Shapes[i]
			b.Shapes = slices.Delete(b.Shapes, i, i+1)
			if op.To == "front" {
				b.Shapes = append(b.Shapes, s)
			} else {
				b.Shapes = slices.Insert(b.Shapes, 0, s)
			}
		}
	default:
		return invalid("unknown operation %q", op.Op)
	}
	return nil
}
