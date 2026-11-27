package apps

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/db"
)

const (
	DefaultPageSize = 50
	MaxPageSize     = 200
	maxConditions   = 8
	maxSearchTerms  = 5
	maxTermLen      = 100
	maxAggregates   = 8
	maxCursorLen    = 4096
	sortPrefix      = 200
	maxValueLen     = 1000
)

const (
	StateLive    = "live"
	StateDeleted = "deleted"
)

const (
	AggCount = "count"
	AggSum   = "sum"
	AggMin   = "min"
	AggMax   = "max"
	AggAvg   = "avg"
)

const (
	SortUpdated = "_updated"
	SortCreated = "_created"
	SortDeleted = "_deleted"
	SortID      = "id"
)

type Query struct {
	State      string      `json:"state,omitempty"`
	Filters    []Condition `json:"filters,omitempty"`
	Search     string      `json:"search,omitempty"`
	Sort       string      `json:"sort,omitempty"`
	Dir        string      `json:"dir,omitempty"`
	Limit      int         `json:"limit,omitempty"`
	Cursor     string      `json:"cursor,omitempty"`
	Aggregates []Aggregate `json:"aggregates,omitempty"`
}

type Condition struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value"`
}

const OpContains = "contains"

type Aggregate struct {
	Kind  string `json:"kind"`
	Field string `json:"field,omitempty"`
}

type QueryResult struct {
	Records    []Record          `json:"records"`
	NextCursor *string           `json:"next_cursor"`
	Total      int               `json:"total"`
	Aggregates []AggregateResult `json:"aggregates"`
}

type AggregateResult struct {
	Kind  string `json:"kind"`
	Field string `json:"field,omitempty"`
	Value any    `json:"value"`
	Count int    `json:"count"`
}

func QueryFromValues(v url.Values) (Query, error) {
	q := Query{State: v.Get("state"), Search: v.Get("q"), Sort: v.Get("sort"), Dir: v.Get("dir"), Cursor: v.Get("cursor")}
	if s := v.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return Query{}, invalidf("limit is a number")
		}
		q.Limit = n
	}
	for _, f := range v["filter"] {
		parts := strings.SplitN(f, ":", 3)
		if len(parts) != 3 {
			return Query{}, invalidf("a filter is field:op:value")
		}
		q.Filters = append(q.Filters, Condition{Field: parts[0], Op: parts[1], Value: parts[2]})
	}
	for _, a := range v["aggregate"] {
		kind, field, _ := strings.Cut(a, ":")
		q.Aggregates = append(q.Aggregates, Aggregate{Kind: kind, Field: field})
	}
	return q, nil
}

func typed(f Field) string {
	path := "'$." + f.Name + "'"
	t := "json_type(r.data, " + path + ")"
	x := "json_extract(r.data, " + path + ")"
	switch f.Type {
	case TypeNumber:
		return "(CASE WHEN " + t + " IN ('integer', 'real') THEN " + x + " END)"
	case TypeBoolean:
		return "(CASE WHEN " + t + " IN ('true', 'false') THEN " + x + " END)"
	case TypeDate:
		return "(CASE WHEN " + t + " = 'text' AND date(" + x + ") = " + x + " THEN " + x + " END)"
	case TypeTime:
		return "(CASE WHEN " + t + " = 'text' AND " + x + " GLOB '[0-2][0-9]:[0-5][0-9]' AND substr(" + x + ", 1, 2) < '24' THEN " + x + " END)"
	case TypeLocation:
		return "(CASE WHEN " + t + " = 'object' THEN 1 END)"
	default:
		return "(CASE WHEN " + t + " = 'text' AND trim(" + x + ", " + blanks + ") <> '' THEN " + x + " END)"
	}
}

var blanks = func() string {
	var codes []string
	for r := rune(0); r <= 0x3000; r++ {
		if unicode.IsSpace(r) {
			codes = append(codes, strconv.Itoa(int(r)))
		}
	}
	return "char(" + strings.Join(codes, ", ") + ")"
}()

func fieldOf(def Definition, name string) (Field, error) {
	f, ok := def.Collection.field(name)
	if !ok || !namePattern.MatchString(name) {
		return Field{}, invalidAs(apierr.CodeAppQueryField, apierr.Params{"field": name}, "the app has no field %q", name)
	}
	return f, nil
}

type keyType int

const (
	keyNumber keyType = iota
	keyText
)

type sortKey struct {
	name     string
	expr     string
	typ      keyType
	nullable bool
	desc     bool
}

func sortOf(def Definition, name, dir string, deleted bool) (sortKey, error) {
	if name == "" {
		name = SortUpdated
		if deleted {
			name = SortDeleted
		}
	}
	k := sortKey{name: name}
	switch name {
	case SortUpdated:
		k.expr, k.typ, k.desc = "r.updated_at", keyText, true
	case SortCreated:
		k.expr, k.typ, k.desc = "r.created_at", keyText, true
	case SortDeleted:
		if !deleted {
			return sortKey{}, invalidAs(apierr.CodeAppQueryField, apierr.Params{"field": name}, "only deleted records have a deletion time to sort by")
		}
		k.expr, k.typ, k.desc = "r.deleted_at", keyText, true
	case SortID:
		k.expr, k.typ = "r.id", keyNumber
	default:
		f, err := fieldOf(def, name)
		if err != nil {
			return sortKey{}, err
		}
		if f.Type == TypeLocation || f.Type == TypeSignature || f.Type == TypeAttachment {
			return sortKey{}, invalidAs(apierr.CodeAppQueryField, apierr.Params{"field": name}, "a %s field can't be sorted by", f.Type)
		}
		k.expr, k.nullable = typed(f), true
		if f.Type == TypeNumber || f.Type == TypeBoolean {
			k.typ = keyNumber
		} else {
			k.typ = keyText
			k.expr = "substr(" + k.expr + ", 1, " + strconv.Itoa(sortPrefix) + ")"
		}
	}
	switch dir {
	case "":
	case "asc":
		k.desc = false
	case "desc":
		k.desc = true
	default:
		return sortKey{}, invalidf(`dir is "asc" or "desc"`)
	}
	return k, nil
}

func (c Condition) sql(def Definition) (string, []any, Condition, error) {
	f, err := fieldOf(def, c.Field)
	if err != nil {
		return "", nil, c, err
	}
	if f.Type == TypeLocation || f.Type == TypeSignature || f.Type == TypeAttachment {
		return "", nil, c, invalidAs(apierr.CodeAppQueryCondition, apierr.Params{"field": c.Field, "op": c.Op}, "a %s field is not something to filter by: it holds files and evidence, not a value", f.Type)
	}
	bad := func(format string, args ...any) error {
		return invalidAs(apierr.CodeAppQueryCondition, apierr.Params{"field": c.Field, "op": c.Op}, format, args...)
	}
	switch c.Op {
	case OpEq, OpNe:
	case OpLt, OpLte, OpGt, OpGte:
		if f.Type == TypeBoolean || f.Type == TypeChoice || f.Type == TypeBarcode {
			return "", nil, c, bad("a %s field is compared with eq or ne only", f.Type)
		}
	case OpContains:
		if f.Type != TypeString && f.Type != TypeBarcode {
			return "", nil, c, bad("contains is for string and barcode fields")
		}
	default:
		return "", nil, c, bad("op must be one of eq, ne, lt, lte, gt, gte or contains")
	}
	value, present, err := f.stored(c.Value)
	if err != nil || !present {
		return "", nil, c, bad("the value for %q must be a %s", c.Field, f.Type)
	}
	if s, ok := value.(string); ok && utf8.RuneCountInString(s) > maxValueLen {
		return "", nil, c, bad("the value is at most %d characters", maxValueLen)
	}
	c.Value = value
	arg := value
	if b, ok := value.(bool); ok {
		arg = 0
		if b {
			arg = 1
		}
	}
	e := typed(f)
	switch c.Op {
	case OpEq:
		return e + " = ?", []any{arg}, c, nil
	case OpNe:
		return "(" + e + " IS NULL OR " + e + " <> ?)", []any{arg}, c, nil
	case OpContains:
		return "instr(" + db.FoldFunc + "(" + e + "), ?) > 0", []any{strings.ToLower(value.(string))}, c, nil
	}
	op := map[string]string{OpLt: "<", OpLte: "<=", OpGt: ">", OpGte: ">="}[c.Op]
	return e + " " + op + " ?", []any{arg}, c, nil
}

func searchSQL(def Definition, terms []string) (string, []any) {
	var cols []string
	for _, f := range def.Collection.Fields {
		if (f.Type == TypeString || f.Type == TypeChoice || f.Type == TypeBarcode) && namePattern.MatchString(f.Name) {
			cols = append(cols, "coalesce("+typed(f)+", '')")
		}
	}
	if len(cols) == 0 {
		return "0", nil
	}
	hay := db.FoldFunc + "(" + strings.Join(cols, " || char(31) || ") + ")"
	var parts []string
	var args []any
	for _, t := range terms {
		parts = append(parts, "instr("+hay+", ?) > 0")
		args = append(args, t)
	}
	return strings.Join(parts, " AND "), args
}

type aggregatePlan struct {
	Aggregate
	cols  []string
	field Field
}

func (a Aggregate) plan(def Definition) (aggregatePlan, error) {
	p := aggregatePlan{Aggregate: a}
	switch a.Kind {
	case AggCount, AggSum, AggMin, AggMax, AggAvg:
	default:
		return p, invalidf("an aggregate is count, sum, min, max or avg")
	}
	if a.Field == "" {
		if a.Kind != AggCount {
			return p, invalidAs(apierr.CodeAppQueryField, apierr.Params{"field": a.Field}, "%s needs a field", a.Kind)
		}
		return p, nil
	}
	f, err := fieldOf(def, a.Field)
	if err != nil {
		return p, err
	}
	if f.Type == TypeAttachment {
		return p, invalidAs(apierr.CodeAppQueryField, apierr.Params{"field": a.Field}, "a %s field can't be aggregated", f.Type)
	}
	e := typed(f)
	p.field = f
	switch a.Kind {
	case AggCount:
		p.cols = []string{"count(" + e + ")"}
	case AggSum, AggAvg:
		if f.Type != TypeNumber {
			return p, invalidAs(apierr.CodeAppQueryField, apierr.Params{"field": a.Field}, "%s is of number fields", a.Kind)
		}
		fn := "total"
		if a.Kind == AggAvg {
			fn = "avg"
		}
		p.cols = []string{fn + "(" + e + ")", "count(" + e + ")"}
	case AggMin, AggMax:
		if f.Type != TypeNumber && f.Type != TypeDate && f.Type != TypeTime {
			return p, invalidAs(apierr.CodeAppQueryField, apierr.Params{"field": a.Field}, "%s is of number, date and time fields", a.Kind)
		}
		p.cols = []string{a.Kind + "(" + e + ")", "count(" + e + ")"}
	}
	return p, nil
}

type queryPlan struct {
	deleted bool
	where   string
	args    []any
	key     sortKey
	limit   int
	cursor  *cursorPos
	aggs    []aggregatePlan
	print   string
}

func (q Query) plan(def Definition, v visibility, appID int64) (queryPlan, error) {
	var p queryPlan
	switch q.State {
	case "", StateLive:
	case StateDeleted:
		p.deleted = true
	default:
		return p, invalidf(`state is "live" or "deleted"`)
	}
	switch {
	case q.Limit == 0:
		p.limit = DefaultPageSize
	case q.Limit < 0 || q.Limit > MaxPageSize:
		return p, invalidf("limit is 1 to %d", MaxPageSize)
	default:
		p.limit = q.Limit
	}
	var err error
	if p.key, err = sortOf(def, q.Sort, q.Dir, p.deleted); err != nil {
		return p, err
	}

	where := []string{"r.app_id = ?"}
	args := []any{appID}
	if p.deleted {
		where = append(where, "r.deleted_at IS NOT NULL")
	} else {
		where = append(where, "r.deleted_at IS NULL")
	}
	switch v.scope {
	case access.ScopeAll:
	case access.ScopeOwn:
		where, args = append(where, "r.created_by = ?"), append(args, v.user)
	default:
		where = append(where, "0")
	}

	if len(q.Filters) > maxConditions {
		return p, invalidf("a query has at most %d filters", maxConditions)
	}
	conds := make([]Condition, len(q.Filters))
	for i, c := range q.Filters {
		s, a, norm, err := c.sql(def)
		if err != nil {
			return p, err
		}
		where, args, conds[i] = append(where, s), append(args, a...), norm
	}
	var terms []string
	if q.Search != "" {
		for _, t := range strings.Fields(q.Search) {
			if utf8.RuneCountInString(t) > maxTermLen || strings.ContainsFunc(t, func(r rune) bool { return r < ' ' }) {
				return p, invalidf("a search word is at most %d characters", maxTermLen)
			}
			terms = append(terms, strings.ToLower(t))
		}
		if len(terms) > maxSearchTerms {
			return p, invalidf("a search has at most %d words", maxSearchTerms)
		}
	}
	if len(terms) > 0 {
		s, a := searchSQL(def, terms)
		where, args = append(where, s), append(args, a...)
	}
	p.where, p.args = strings.Join(where, " AND "), args

	if len(q.Aggregates) > maxAggregates {
		return p, invalidf("a query has at most %d aggregates", maxAggregates)
	}
	for _, a := range q.Aggregates {
		ap, err := a.plan(def)
		if err != nil {
			return p, err
		}
		p.aggs = append(p.aggs, ap)
	}

	p.print = fingerprint(appID, v, p, conds, terms)
	if q.Cursor != "" {
		if p.cursor, err = readCursor(q.Cursor, p.print, p.key); err != nil {
			return p, err
		}
	}
	return p, nil
}

func fingerprint(appID int64, v visibility, p queryPlan, conds []Condition, terms []string) string {
	raw, _ := json.Marshal(struct {
		App     int64
		User    int64
		Scope   access.Scope
		Deleted bool
		Where   string
		Conds   []Condition
		Terms   []string
		Key     string
		Type    keyType
		Desc    bool
	}{appID, v.user, v.scope, p.deleted, p.where, conds, terms, p.key.name, p.key.typ, p.key.desc})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

type cursorPos struct {
	null bool
	key  any
	id   int64
}

type cursorBody struct {
	V int    `json:"v"`
	F string `json:"f"`
	N int    `json:"n"`
	K any    `json:"k,omitempty"`
	I int64  `json:"i"`
}

func badCursor() error {
	return invalidAs(apierr.CodeAppQueryCursor, nil, "that cursor isn't one of this query: start again from the first page")
}

func writeCursor(print string, k sortKey, sk any, id int64) (string, error) {
	b := cursorBody{V: 1, F: print, I: id}
	switch x := sk.(type) {
	case nil:
		b.N = 1
	case int64:
		b.K = x
	case float64:
		b.K = x
	case string:
		b.K = x
	case []byte:
		b.K = string(x)
	default:
		return "", fmt.Errorf("sort key of type %T", sk)
	}
	_, isInt := b.K.(int64)
	if _, isFloat := b.K.(float64); b.K != nil && (isInt || isFloat) != (k.typ == keyNumber) {
		return "", fmt.Errorf("sort key %v isn't of the key's type", b.K)
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func readCursor(s, print string, k sortKey) (*cursorPos, error) {
	if len(s) > maxCursorLen {
		return nil, badCursor()
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, badCursor()
	}
	var b cursorBody
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(&b); err != nil || dec.More() || b.V != 1 || b.F != print || b.I < 0 || (b.N != 0 && b.N != 1) {
		return nil, badCursor()
	}
	pos := &cursorPos{null: b.N == 1, id: b.I}
	if pos.null {
		if b.K != nil || !k.nullable {
			return nil, badCursor()
		}
		return pos, nil
	}
	switch x := b.K.(type) {
	case json.Number:
		if k.typ != keyNumber {
			return nil, badCursor()
		}
		if i, err := x.Int64(); err == nil {
			pos.key = i
			return pos, nil
		}
		f, err := x.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, badCursor()
		}
		pos.key = f
	case string:
		if k.typ != keyText || utf8.RuneCountInString(x) > sortPrefix {
			return nil, badCursor()
		}
		pos.key = x
	default:
		return nil, badCursor()
	}
	return pos, nil
}

func (p queryPlan) after() (string, []any) {
	cmp := ">"
	if p.key.desc {
		cmp = "<"
	}
	c, e := p.cursor, p.key.expr
	if c.null {
		return "(" + e + " IS NULL AND r.id " + cmp + " ?)", []any{c.id}
	}
	return "(" + e + " IS NULL OR " + e + " " + cmp + " ? OR (" + e + " = ? AND r.id " + cmp + " ?))", []any{c.key, c.key, c.id}
}

type keyed struct {
	scanner
	key *any
}

func (k keyed) Scan(dest ...any) error { return k.scanner.Scan(append(dest, k.key)...) }

func (p queryPlan) run(ctx context.Context, q queryer, def Definition) (QueryResult, error) {
	out := QueryResult{Records: []Record{}, Aggregates: []AggregateResult{}}
	if err := p.totals(ctx, q, &out); err != nil {
		return QueryResult{}, err
	}

	where, args := p.where, slices.Clone(p.args)
	if p.cursor != nil {
		s, a := p.after()
		where, args = where+" AND "+s, append(args, a...)
	}
	dir := "ASC"
	if p.key.desc {
		dir = "DESC"
	}
	rows, err := q.QueryContext(ctx, recordColumns+", "+p.key.expr+recordFrom+` WHERE `+where+
		` ORDER BY (`+p.key.expr+` IS NULL), `+p.key.expr+` `+dir+`, r.id `+dir+` LIMIT ?`, append(args, p.limit+1)...)
	if err != nil {
		return QueryResult{}, err
	}
	defer rows.Close()
	var keys []any
	for rows.Next() {
		var sk any
		rec, _, err := scanRecord(keyed{rows, &sk}, def)
		if err != nil {
			return QueryResult{}, err
		}
		out.Records, keys = append(out.Records, rec), append(keys, sk)
	}
	if err := rows.Err(); err != nil {
		return QueryResult{}, err
	}
	if len(out.Records) > p.limit {
		out.Records, keys = out.Records[:p.limit], keys[:p.limit]
		last := len(out.Records) - 1
		c, err := writeCursor(p.print, p.key, keys[last], out.Records[last].ID)
		if err != nil {
			return QueryResult{}, err
		}
		out.NextCursor = &c
	}
	return out, nil
}

func (p queryPlan) totals(ctx context.Context, q queryer, out *QueryResult) error {
	cols := []string{"count(*)"}
	for _, a := range p.aggs {
		cols = append(cols, a.cols...)
	}
	vals := make([]any, len(cols))
	dest := make([]any, len(cols))
	for i := range vals {
		dest[i] = &vals[i]
	}
	if err := q.QueryRowContext(ctx, `SELECT `+strings.Join(cols, ", ")+` FROM app_rows r WHERE `+p.where, p.args...).Scan(dest...); err != nil {
		return err
	}
	total, _ := vals[0].(int64)
	out.Total = int(total)
	next := 1
	for _, a := range p.aggs {
		res := AggregateResult{Kind: a.Kind, Field: a.Field}
		switch {
		case len(a.cols) == 0:
			res.Value, res.Count = out.Total, out.Total
		case a.Kind == AggCount:
			n, _ := vals[next].(int64)
			res.Value, res.Count = int(n), int(n)
		default:
			n, _ := vals[next+1].(int64)
			res.Count = int(n)
			res.Value = aggregateValue(vals[next])
		}
		next += len(a.cols)
		out.Aggregates = append(out.Aggregates, res)
	}
	return nil
}

func aggregateValue(v any) any {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil
		}
		return x
	case string:
		return x
	case []byte:
		return string(x)
	}
	return nil
}

func (s *Service) queryRecords(ctx context.Context, sp access.Space, a Actor, appID int64, q Query) (QueryResult, error) {
	v, def, err := s.readableRecords(ctx, sp, a, appID)
	if err != nil {
		return QueryResult{}, err
	}
	p, err := q.plan(def, v, appID)
	if err != nil {
		return QueryResult{}, err
	}
	return p.run(ctx, s.db, def)
}
