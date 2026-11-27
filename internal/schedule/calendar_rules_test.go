package schedule

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestCalendarRejectsMoreInvalidValues(t *testing.T) {
	for name, cal := range map[string]Calendar{
		"no zone":                  {At: "08:00"},
		"blank zone":               {At: "08:00", Zone: "  "},
		"a path as the zone":       {At: "08:00", Zone: "../etc/passwd"},
		"neither at nor interval":  {Zone: "UTC"},
		"neither, with days":       {Days: []string{"mon"}, Zone: "UTC"},
		"at without a colon":       {At: "08-30", Zone: "UTC"},
		"at without a separator":   {At: "0830", Zone: "UTC"},
		"at with a long hour":      {At: "008:30", Zone: "UTC"},
		"minute 60":                {At: "08:60", Zone: "UTC"},
		"negative hour":            {At: "-1:00", Zone: "UTC"},
		"negative minute":          {At: "08:-1", Zone: "UTC"},
		"plus sign":                {At: "+8:00", Zone: "UTC"},
		"blank at":                 {At: " 8:00", Zone: "UTC"},
		"plus before the minute":   {At: "08:+5", Zone: "UTC"},
		"letters":                  {At: "ab:cd", Zone: "UTC"},
		"interval of 59":           {EveryMin: 59, Zone: "UTC"},
		"interval of 1441":         {EveryMin: 1441, Zone: "UTC"},
		"negative interval":        {EveryMin: -60, Zone: "UTC"},
		"unknown day with a valid": {Days: []string{"mon", "funday"}, At: "08:00", Zone: "UTC"},
		"empty day":                {Days: []string{""}, At: "08:00", Zone: "UTC"},
	} {
		if err := cal.Check(); !errors.Is(err, ErrCalendar) {
			t.Errorf("%s: Check = %v, want ErrCalendar", name, err)
		}
		if _, err := cal.Next(time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)); !errors.Is(err, ErrCalendar) {
			t.Errorf("%s: Next = %v, want ErrCalendar", name, err)
		}
		if got := cal.Describe(); len(got.Days) != 0 || got.At != "" || got.Every != 0 || got.Zone != "" {
			t.Errorf("%s: Describe = %+v, want nothing", name, got)
		}
	}
}

func TestCalendarAcceptsItsEdges(t *testing.T) {
	for name, cal := range map[string]Calendar{
		"midnight":      {At: "00:00", Zone: "UTC"},
		"last minute":   {At: "23:59", Zone: "UTC"},
		"60 minutes":    {EveryMin: 60, Zone: "UTC"},
		"1440 minutes":  {EveryMin: 1440, Zone: "UTC"},
		"a padded zone": {At: "08:00", Zone: " Europe/Lisbon "},
		"every day":     {At: "08:00", Zone: "UTC"},
		"mixed case":    {Days: []string{"MoN"}, At: "08:00", Zone: "UTC"},
	} {
		if err := cal.Check(); err != nil {
			t.Errorf("%s: Check = %v", name, err)
		}
	}
}

func TestCalendarNormalSortsDeduplicatesAndDoesNotTouchTheCallersDays(t *testing.T) {
	days := []string{"sun", "SAT", "mon", "Sun", "mon"}
	cal := Calendar{Days: days, EveryMin: 60, Zone: " UTC "}
	got, err := cal.Normal()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"mon", "sat", "sun"}; !reflect.DeepEqual(got.Days, want) {
		t.Fatalf("Days = %v, want %v", got.Days, want)
	}
	if got.Zone != "UTC" {
		t.Fatalf("Zone = %q", got.Zone)
	}
	if !reflect.DeepEqual(days, []string{"sun", "SAT", "mon", "Sun", "mon"}) {
		t.Fatalf("the caller's days were changed: %v", days)
	}
	again, err := got.Normal()
	if err != nil || !reflect.DeepEqual(again, got) {
		t.Fatalf("Normal is not stable: %+v, %v", again, err)
	}
}

func TestCalendarDescribesWhatItIsWithoutProse(t *testing.T) {
	got := Calendar{Days: []string{"fri", "MON"}, At: "08:30", Zone: "Europe/Lisbon"}.Describe()
	if want := (CalendarText{Days: []string{"mon", "fri"}, At: "08:30", Zone: "Europe/Lisbon"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("Describe = %+v, want %+v", got, want)
	}
	got = Calendar{EveryMin: 120, Zone: "UTC"}.Describe()
	if len(got.Days) != 0 || got.At != "" || got.Every != 120 || got.Zone != "UTC" {
		t.Fatalf("Describe = %+v", got)
	}
}

func TestCalendarIsStoredWithTheNamesTheDocsGive(t *testing.T) {
	raw, err := json.Marshal(Calendar{Days: []string{"mon"}, At: "08:30", Zone: "UTC"})
	if err != nil || string(raw) != `{"days":["mon"],"at":"08:30","time_zone":"UTC"}` {
		t.Fatalf("Calendar = %s, %v", raw, err)
	}
	raw, err = json.Marshal(Calendar{EveryMin: 90, Zone: "UTC"})
	if err != nil || string(raw) != `{"every_minutes":90,"time_zone":"UTC"}` {
		t.Fatalf("Calendar = %s, %v", raw, err)
	}
	var back Calendar
	if err := json.Unmarshal([]byte(`{"days":["sat"],"every_minutes":120,"time_zone":"Europe/Lisbon"}`), &back); err != nil ||
		!reflect.DeepEqual(back, Calendar{Days: []string{"sat"}, EveryMin: 120, Zone: "Europe/Lisbon"}) {
		t.Fatalf("Unmarshal = %+v, %v", back, err)
	}
	raw, err = json.Marshal(Calendar{Days: []string{"mon"}, At: "08:30", Zone: "UTC"}.Describe())
	if err != nil || string(raw) != `{"days":["mon"],"at":"08:30","zone":"UTC"}` {
		t.Fatalf("CalendarText = %s, %v", raw, err)
	}
}

func TestCalendarNextStopsAtTheUpperBoundAndFollowsTheDays(t *testing.T) {
	cal := Calendar{At: "08:30", Zone: "UTC"}
	if _, err := cal.Next(MaxRunAt.Add(-time.Hour)); !errors.Is(err, ErrCalendar) {
		t.Fatalf("an occurrence after the upper bound = %v, want ErrCalendar", err)
	}
	if _, err := cal.Next(MaxRunAt.Add(time.Hour)); !errors.Is(err, ErrCalendar) {
		t.Fatalf("after past the upper bound = %v, want ErrCalendar", err)
	}
	if got, err := cal.Next(time.Date(8999, time.December, 30, 9, 0, 0, 0, time.UTC)); err != nil || !got.Equal(time.Date(8999, time.December, 31, 8, 30, 0, 0, time.UTC)) {
		t.Fatalf("the last day = %v, %v", got, err)
	}
	for _, day := range []time.Weekday{time.Sunday, time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday} {
		names := [...]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}
		only := Calendar{Days: []string{names[day]}, At: "08:30", Zone: "UTC"}
		after := time.Date(2026, time.March, 4, 12, 0, 0, 0, time.UTC)
		got, err := only.Next(after)
		if err != nil || got.Weekday() != day || got.Hour() != 8 || got.Minute() != 30 || !got.After(after) || got.Sub(after) > 8*24*time.Hour {
			t.Fatalf("%s: Next = %v, %v", names[day], got, err)
		}
	}
}

func TestCalendarNextUsesTheZoneOfTheCalendarNotOfTheCaller(t *testing.T) {
	cal := Calendar{At: "08:30", Zone: "Asia/Tokyo"}
	for _, loc := range []*time.Location{time.UTC, mustLocation(t, "America/Los_Angeles"), mustLocation(t, "Asia/Tokyo")} {
		got, err := cal.Next(time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC).In(loc))
		if err != nil || !got.Equal(time.Date(2026, time.June, 1, 23, 30, 0, 0, time.UTC)) || got.Location() != time.UTC {
			t.Fatalf("Next = %v (%v), %v", got, got.Location(), err)
		}
	}
}

func TestCalendarNextCrossesYearAndLeapDayBoundaries(t *testing.T) {
	cal := Calendar{Days: []string{"sun"}, At: "00:00", Zone: "UTC"}
	got, err := cal.Next(time.Date(2026, time.December, 31, 12, 0, 0, 0, time.UTC))
	if err != nil || !got.Equal(time.Date(2027, time.January, 3, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("across the year = %v, %v", got, err)
	}
	got, err = Calendar{At: "12:00", Zone: "UTC"}.Next(time.Date(2028, time.February, 28, 13, 0, 0, 0, time.UTC))
	if err != nil || !got.Equal(time.Date(2028, time.February, 29, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("leap day = %v, %v", got, err)
	}
}

type wallOracle struct {
	loc *time.Location
}

func (o wallOracle) instants(year int, month time.Month, day int, wallMinutes []int) []time.Time {
	start := time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Add(-15 * time.Hour)
	end := start.Add(54 * time.Hour)
	out := make([]time.Time, len(wallMinutes))
	next := 0
	for t := start; t.Before(end) && next < len(wallMinutes); t = t.Add(time.Minute) {
		local := t.In(o.loc)
		y, m, d := local.Date()
		if y != year || m != month || d != day {
			continue
		}
		minute := local.Hour()*60 + local.Minute()
		for next < len(wallMinutes) && minute >= wallMinutes[next] {
			out[next] = t
			next++
		}
	}
	return out
}

func (o wallOracle) next(cal Calendar, after time.Time) time.Time {
	var wall []int
	if cal.At != "" {
		wall = []int{int(cal.At[0]-'0')*600 + int(cal.At[1]-'0')*60 + int(cal.At[3]-'0')*10 + int(cal.At[4]-'0')}
	} else {
		for minute := 0; minute < 1440; minute += cal.EveryMin {
			wall = append(wall, minute)
		}
	}
	local := after.In(o.loc)
	date := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	for offset := 0; offset < 9; offset++ {
		d := date.AddDate(0, 0, offset)
		if len(cal.Days) > 0 {
			ok := false
			for _, name := range cal.Days {
				if weekdays[name] == d.Weekday() {
					ok = true
				}
			}
			if !ok {
				continue
			}
		}
		for _, instant := range o.instants(d.Year(), d.Month(), d.Day(), wall) {
			if !instant.IsZero() && instant.After(after) {
				return instant
			}
		}
	}
	return time.Time{}
}

func TestCalendarNextAgreesWithAMinuteByMinuteScanOfTheWallClock(t *testing.T) {
	zones := []string{"Europe/Lisbon", "America/Santiago", "Australia/Lord_Howe", "America/Sao_Paulo", "Asia/Tokyo"}
	cals := []Calendar{
		{At: "00:00"},
		{At: "01:30"},
		{At: "02:30"},
		{At: "23:30"},
		{Days: []string{"sat", "sun"}, At: "01:45"},
		{EveryMin: 60},
		{EveryMin: 90},
		{Days: []string{"sun"}, EveryMin: 1440},
	}
	for _, zone := range zones {
		oracle := wallOracle{loc: mustLocation(t, zone)}
		for _, cal := range cals {
			cal.Zone = zone
			after := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
			for range 160 {
				got, err := cal.Next(after)
				want := oracle.next(cal, after)
				if err != nil || !got.Equal(want) {
					t.Fatalf("%s %+v after %v: Next = %v, %v; the scan found %v", zone, cal, after, got, err, want)
				}
				after = after.Add(53*time.Hour + 17*time.Minute)
			}
		}
	}
}
