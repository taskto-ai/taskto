package apps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	OpEq  = "eq"
	OpNe  = "ne"
	OpLt  = "lt"
	OpLte = "lte"
	OpGt  = "gt"
	OpGte = "gte"
)

var ruleOps = []string{OpEq, OpNe, OpLt, OpLte, OpGt, OpGte}

const (
	maxRuleDepth = 3
	maxRuleNodes = 16
	maxRuleList  = 8
)

type Rule struct {
	Field string `json:"field,omitempty"`
	Op    string `json:"op,omitempty"`
	Value any    `json:"value,omitempty" apitypes:"json"`
	All   []Rule `json:"all,omitempty"`
	Any   []Rule `json:"any,omitempty"`
}

func (r *Rule) UnmarshalJSON(b []byte) error {
	type plain Rule
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var p plain
	if err := dec.Decode(&p); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("a rule is one JSON object")
	}
	*r = Rule(p)
	return nil
}

func ruleFromValue(v any) (Rule, error) {
	if _, ok := v.(map[string]any); !ok {
		return Rule{}, fmt.Errorf("a rule is an object")
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return Rule{}, err
	}
	var r Rule
	if err := json.Unmarshal(raw, &r); err != nil {
		return Rule{}, err
	}
	return r, nil
}

func (r Rule) check(c Collection) error {
	nodes := 0
	return r.checkAt(c, 1, &nodes)
}

func (r Rule) checkAt(c Collection, depth int, nodes *int) error {
	if (r.All != nil || r.Any != nil) && depth > maxRuleDepth {
		return fmt.Errorf("a rule nests all and any at most %d deep", maxRuleDepth)
	}
	if *nodes++; *nodes > maxRuleNodes {
		return fmt.Errorf("a rule has at most %d parts", maxRuleNodes)
	}
	forms := 0
	if r.Field != "" || r.Op != "" || r.Value != nil {
		forms++
	}
	if r.All != nil {
		forms++
	}
	if r.Any != nil {
		forms++
	}
	if forms != 1 {
		return fmt.Errorf(`a rule is {"field", "op", "value"}, {"all": […]} or {"any": […]}, one of them`)
	}
	if r.All != nil || r.Any != nil {
		list, name := r.All, "all"
		if r.Any != nil {
			list, name = r.Any, "any"
		}
		if len(list) < 1 || len(list) > maxRuleList {
			return fmt.Errorf("%s holds 1 to %d rules", name, maxRuleList)
		}
		for i, sub := range list {
			if err := sub.checkAt(c, depth+1, nodes); err != nil {
				return fmt.Errorf("%s[%d]: %w", name, i, err)
			}
		}
		return nil
	}
	f, ok := c.field(r.Field)
	if !ok {
		return fmt.Errorf("the rule names field %q, which the collection doesn't declare", r.Field)
	}
	if !slices.Contains(ruleOps, r.Op) {
		return fmt.Errorf("op must be one of %s", strings.Join(ruleOps, ", "))
	}
	if f.Type == TypeAttachment || f.Type == TypeLocation || f.Type == TypeSignature {
		return fmt.Errorf("a %s field can't be compared: a rule can't read %q", f.Type, f.Name)
	}
	ordered := f.Type == TypeNumber || f.Type == TypeDate || f.Type == TypeTime
	if !ordered && r.Op != OpEq && r.Op != OpNe {
		return fmt.Errorf("a %s field is compared with eq or ne only", f.Type)
	}
	if r.Value == nil {
		return fmt.Errorf("a comparison needs its value")
	}
	if _, err := f.literal(r.Value); err != nil {
		return fmt.Errorf("the value for %q: %w", f.Name, err)
	}
	return nil
}

func (f Field) literal(v any) (any, error) {
	switch f.Type {
	case TypeString, TypeBarcode:
		s, ok := v.(string)
		if !ok || utf8.RuneCountInString(s) > DefaultMaxLength {
			return nil, fmt.Errorf("must be text of at most %d characters", DefaultMaxLength)
		}
		return s, nil
	case TypeChoice:
		s, ok := v.(string)
		if !ok || !slices.Contains(f.Options, s) {
			return nil, fmt.Errorf("must be one of the options: %s", strings.Join(f.Options, ", "))
		}
		return s, nil
	case TypeNumber:
		n, ok := v.(float64)
		if !ok || math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, fmt.Errorf("must be a number")
		}
		return n, nil
	case TypeBoolean:
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("must be true or false")
		}
		return b, nil
	case TypeDate:
		s, ok := v.(string)
		if _, err := time.Parse(time.DateOnly, s); !ok || err != nil {
			return nil, fmt.Errorf("must be a date, YYYY-MM-DD")
		}
		return s, nil
	case TypeTime:
		s, ok := v.(string)
		if !ok || !timePattern.MatchString(s) {
			return nil, fmt.Errorf("must be a time, HH:MM")
		}
		return s, nil
	}
	return nil, fmt.Errorf("has an unknown type")
}

var timePattern = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

func (c Collection) holds(r Rule, row map[string]any) bool {
	switch {
	case r.All != nil:
		for _, sub := range r.All {
			if !c.holds(sub, row) {
				return false
			}
		}
		return true
	case r.Any != nil:
		for _, sub := range r.Any {
			if c.holds(sub, row) {
				return true
			}
		}
		return false
	}
	f, ok := c.field(r.Field)
	if !ok {
		return false
	}
	var have any
	if v, set := row[f.Name]; set && ofType(f, v) {
		have, _, _ = f.stored(v)
	}
	if have == nil {
		return r.Op == OpNe
	}
	want, err := f.literal(r.Value)
	if err != nil {
		return false
	}
	var order int
	switch h := have.(type) {
	case float64:
		w, _ := want.(float64)
		order = compareFloat(h, w)
	case string:
		w, _ := want.(string)
		order = strings.Compare(h, w)
	case bool:
		w, _ := want.(bool)
		if h != w {
			order = 1
		}
	}
	switch r.Op {
	case OpEq:
		return order == 0
	case OpNe:
		return order != 0
	case OpLt:
		return order < 0
	case OpLte:
		return order <= 0
	case OpGt:
		return order > 0
	case OpGte:
		return order >= 0
	}
	return false
}

func compareFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (f Field) stored(v any) (any, bool, error) {
	f.MaxLength, f.Min, f.Max, f.Options, f.MaxFiles, f.asStored = math.MaxInt, nil, nil, nil, math.MaxInt, true
	f.Format = ""
	return f.parse(v)
}

func sameRule(a, b *Rule) bool {
	if a == nil || b == nil {
		return a == b
	}
	return reflect.DeepEqual(*a, *b)
}

func ruleFields(r Rule) []string {
	var out []string
	var walk func(Rule)
	walk = func(r Rule) {
		if r.Field != "" {
			out = append(out, r.Field)
		}
		for _, sub := range r.All {
			walk(sub)
		}
		for _, sub := range r.Any {
			walk(sub)
		}
	}
	walk(r)
	return out
}
