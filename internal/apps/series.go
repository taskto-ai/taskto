package apps

import (
	"math"
	"sort"
	"strconv"
	"time"
)

type Series struct {
	Kind    string `json:"kind"`
	Field   string `json:"field,omitempty"`
	GroupBy string `json:"group_by"`
	Bucket  string `json:"bucket,omitempty"`
	Last    int    `json:"last,omitempty"`
	SplitBy string `json:"split_by,omitempty"`
	Where   *Rule  `json:"where,omitempty"`
}

type SeriesData struct {
	X      []SeriesPoint `json:"x"`
	Series []SeriesLine  `json:"series"`
}

type SeriesPoint struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

type SeriesLine struct {
	Key    string     `json:"key"`
	Label  string     `json:"label"`
	Values []*float64 `json:"values"`
}

var seriesNow = func() time.Time { return time.Now().UTC() }

func (d Definition) checkSeries(c Collection) error {
	if len(d.Series) > maxSeries {
		return invalidf("series: at most %d", maxSeries)
	}
	for name, s := range d.Series {
		if !namePattern.MatchString(name) || reserved[name] || name == ScreenKey || name == RecordKey {
			return invalidf("series: %q: invalid or reserved name", name)
		}
		if _, ok := c.field(name); ok {
			return invalidf("series: %q is also a field", name)
		}
		if _, ok := d.Summaries[name]; ok {
			return invalidf("series: %q is also a summary", name)
		}
		switch s.Kind {
		case "count":
			if s.Field != "" {
				return invalidf("series: %q: count has no field", name)
			}
		case "sum", "avg":
			f, ok := c.field(s.Field)
			if !ok || f.Type != TypeNumber {
				return invalidf("series: %q: %s field %q is not a number field", name, s.Kind, s.Field)
			}
		default:
			return invalidf("series: %q: kind must be count, sum or avg", name)
		}
		group, ok := c.field(s.GroupBy)
		if !ok || !slicesContains([]string{TypeChoice, TypeBoolean, TypeString, TypeDate}, group.Type) {
			return invalidf("series: %q: group_by names a choice, boolean, string or date field", name)
		}
		if group.Type == TypeDate {
			if s.Bucket != "day" && s.Bucket != "week" && s.Bucket != "month" {
				return invalidf("series: %q: a date group_by needs bucket day, week or month", name)
			}
			if s.Last < 0 || s.Last > 90 {
				return invalidf("series: %q: last is 1 to 90", name)
			}
		} else if s.Bucket != "" || s.Last != 0 {
			return invalidf("series: %q: bucket and last require a date group_by", name)
		}
		if s.SplitBy != "" {
			f, ok := c.field(s.SplitBy)
			if !ok || (f.Type != TypeChoice && f.Type != TypeBoolean) {
				return invalidf("series: %q: split_by names a choice or boolean field", name)
			}
		}
		if s.Where != nil {
			if err := s.Where.check(c); err != nil {
				return invalidf("series: %q: where: %v", name, err)
			}
		}
	}
	return nil
}

type seriesGroup struct {
	key   string
	label string
}

type seriesTotal struct {
	count int
	sum   float64
	half  float64
}

func computeSeries(s Series, c Collection, rows []Row, now time.Time) SeriesData {
	groupField, _ := c.field(s.GroupBy)
	var groups []seriesGroup
	groupIndex := map[string]int{}
	switch groupField.Type {
	case TypeDate:
		last := s.Last
		if last == 0 {
			last = 12
		}
		current := bucketStart(now.UTC(), s.Bucket)
		for i := last - 1; i >= 0; i-- {
			start := shiftBucket(current, s.Bucket, -i)
			key := bucketKey(start, s.Bucket)
			groupIndex[key] = len(groups)
			groups = append(groups, seriesGroup{key: key, label: key})
		}
	case TypeChoice:
		for _, key := range groupField.Options {
			groupIndex[key] = len(groups)
			groups = append(groups, seriesGroup{key: key, label: groupField.label(key)})
		}
	case TypeBoolean:
		groups = []seriesGroup{{key: "false", label: "false"}, {key: "true", label: "true"}}
		groupIndex["false"], groupIndex["true"] = 0, 1
	}
	var textCounts map[string]int
	if groupField.Type == TypeString {
		textCounts = map[string]int{}
		for _, row := range rows {
			value, isText := row.Data[s.GroupBy].(string)
			if !seriesKeeps(s, c, row) || !isText {
				continue
			}
			textCounts[value]++
		}
		keys := make([]string, 0, len(textCounts))
		for key := range textCounts {
			if key != "other" {
				keys = append(keys, key)
			}
		}
		sort.Slice(keys, func(i, j int) bool {
			if textCounts[keys[i]] != textCounts[keys[j]] {
				return textCounts[keys[i]] > textCounts[keys[j]]
			}
			return keys[i] < keys[j]
		})
		if len(keys) > 20 {
			keys = keys[:20]
		}
		for _, key := range keys {
			groupIndex[key] = len(groups)
			groups = append(groups, seriesGroup{key: key, label: key})
		}
		groupIndex["other"] = len(groups)
		groups = append(groups, seriesGroup{key: "other", label: "other"})
	}

	splitField, hasSplit := c.field(s.SplitBy)
	var splits []seriesGroup
	if hasSplit {
		if splitField.Type == TypeChoice {
			for _, key := range splitField.Options {
				splits = append(splits, seriesGroup{key: key, label: splitField.label(key)})
			}
		} else {
			splits = []seriesGroup{{key: "false", label: "false"}, {key: "true", label: "true"}}
		}
	} else {
		splits = []seriesGroup{{key: "", label: ""}}
	}
	totals := make([][]seriesTotal, len(splits))
	for i := range totals {
		totals[i] = make([]seriesTotal, len(groups))
	}
	for _, row := range rows {
		if !seriesKeeps(s, c, row) {
			continue
		}
		groupKey, ok := seriesKey(groupField, row.Data[s.GroupBy], s.Bucket)
		if !ok {
			continue
		}
		group, exists := groupIndex[groupKey]
		if !exists && groupField.Type == TypeString {
			group, exists = groupIndex["other"], true
		}
		if !exists {
			continue
		}
		var amount float64
		if s.Kind != "count" {
			amount, ok = row.Data[s.Field].(float64)
			if !ok {
				continue
			}
		}
		split := 0
		if hasSplit {
			splitKey, ok := seriesKey(splitField, row.Data[s.SplitBy], "")
			if !ok {
				continue
			}
			split = -1
			for i := range splits {
				if splits[i].key == splitKey {
					split = i
					break
				}
			}
			if split < 0 {
				continue
			}
		}
		t := &totals[split][group]
		t.count++
		t.sum += amount
		t.half += amount / 2
	}
	data := SeriesData{}
	for _, group := range groups {
		data.X = append(data.X, SeriesPoint{Key: group.key, Label: group.label})
	}
	for i, split := range splits {
		line := SeriesLine{Key: split.key, Label: split.label, Values: make([]*float64, len(groups))}
		for j, total := range totals[i] {
			var value float64
			switch s.Kind {
			case "count":
				value = float64(total.count)
			case "sum":
				value = total.sum
			case "avg":
				if total.count == 0 {
					continue
				}
				value = total.sum / float64(total.count)
				if math.IsInf(value, 0) {
					value = total.half / float64(total.count) * 2
				}
			}
			if math.IsInf(value, 0) || math.IsNaN(value) {
				continue
			}
			line.Values[j] = &value
		}
		data.Series = append(data.Series, line)
	}
	return data
}

func seriesKey(f Field, v any, bucket string) (string, bool) {
	switch f.Type {
	case TypeDate:
		text, ok := v.(string)
		if !ok {
			return "", false
		}
		d, err := time.Parse("2006-01-02", text)
		if err != nil {
			return "", false
		}
		return bucketKey(bucketStart(d.UTC(), bucket), bucket), true
	case TypeBoolean:
		b, ok := v.(bool)
		return strconv.FormatBool(b), ok
	default:
		text, ok := v.(string)
		return text, ok
	}
}

func seriesKeeps(s Series, c Collection, row Row) bool {
	return s.Where == nil || c.holds(*s.Where, row.Data)
}

func bucketStart(t time.Time, bucket string) time.Time {
	t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	switch bucket {
	case "week":
		return t.AddDate(0, 0, -((int(t.Weekday()) + 6) % 7))
	case "month":
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return t
	}
}

func shiftBucket(t time.Time, bucket string, amount int) time.Time {
	switch bucket {
	case "week":
		return t.AddDate(0, 0, 7*amount)
	case "month":
		return t.AddDate(0, amount, 0)
	default:
		return t.AddDate(0, 0, amount)
	}
}

func bucketKey(t time.Time, bucket string) string {
	switch bucket {
	case "week":
		y, w := t.ISOWeek()
		return strconv.Itoa(y) + "-W" + twoDigits(w)
	case "month":
		return t.Format("2006-01")
	default:
		return t.Format("2006-01-02")
	}
}

func twoDigits(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

func slicesContains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
