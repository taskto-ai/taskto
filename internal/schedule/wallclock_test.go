package schedule

import (
	"testing"
	"time"
)

func TestLocalHourIsCountedOnTheCalendar(t *testing.T) {
	zone := func(name string) *time.Location {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Fatal(err)
		}
		return loc
	}
	utc := func(y int, m time.Month, d, h, mi int) time.Time { return time.Date(y, m, d, h, mi, 0, 0, time.UTC) }
	for _, tc := range []struct {
		name  string
		zone  string
		y     int
		m     time.Month
		d, h  int
		want  time.Time
		wallH int // the hour the wall clock shows at want
	}{
		{"an ordinary day", "Europe/Lisbon", 2030, 1, 10, 8, utc(2030, 1, 10, 8, 0), 8},
		{"east of UTC", "Asia/Tokyo", 2030, 1, 10, 8, utc(2030, 1, 9, 23, 0), 8},
		{"summer time", "America/New_York", 2030, 7, 1, 8, utc(2030, 7, 1, 12, 0), 8},
		// New York's clocks go from 02:00 to 03:00 on 2030-03-10: 02:30 doesn't exist, and 02:00 is
		// the first moment after the gap's start, that is 03:00.
		{"an hour that is skipped", "America/New_York", 2030, 3, 10, 2, utc(2030, 3, 10, 7, 0), 3},
		// And back from 02:00 to 01:00 on 2030-11-03: 01:00 happens twice, and is the first time.
		{"an hour that happens twice", "America/New_York", 2030, 11, 3, 1, utc(2030, 11, 3, 5, 0), 1},
		{"the hour after the second one", "America/New_York", 2030, 11, 3, 2, utc(2030, 11, 3, 7, 0), 2},
		// The day after a date is the date plus one, whatever length the day between is.
		{"the 31st of March plus one day, Lisbon", "Europe/Lisbon", 2030, 3, 31 + 1, 8, utc(2030, 4, 1, 7, 0), 8},
		{"past the end of a month", "UTC", 2030, 1, 32, 8, utc(2030, 2, 1, 8, 0), 8},
		{"past the end of a year", "UTC", 2030, 12, 32, 8, utc(2031, 1, 1, 8, 0), 8},
		// Samoa skipped 2011-12-30 whole.
		{"a day that is skipped", "Pacific/Apia", 2011, 12, 30, 8, utc(2011, 12, 30, 10, 0), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loc := zone(tc.zone)
			got := LocalHour(tc.y, tc.m, tc.d, tc.h, loc)
			if !got.Equal(tc.want) {
				t.Fatalf("LocalHour = %v, want %v", got.UTC(), tc.want)
			}
			if h := got.In(loc).Hour(); h != tc.wallH {
				t.Fatalf("the wall clock shows hour %d, want %d", h, tc.wallH)
			}
		})
	}
}
