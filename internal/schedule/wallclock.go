package schedule

import "time"

func LocalHour(year int, month time.Month, day, hour int, loc *time.Location) time.Time {
	want := time.Date(year, month, day, hour, 0, 0, 0, time.UTC)
	t := want.Add(-15 * time.Hour)
	for range 16 {
		local := t.In(loc)
		_, offset := local.Zone()
		at := want.Add(-time.Duration(offset) * time.Second)
		if at.Before(t) {
			at = t
		}
		_, end := local.ZoneBounds()
		if end.IsZero() || at.Before(end) {
			return at
		}
		t = end
	}
	return time.Date(year, month, day, hour, 0, 0, 0, loc)
}
