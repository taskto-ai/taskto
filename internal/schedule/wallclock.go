package schedule

import "time"

func LocalHour(year int, month time.Month, day, hour int, loc *time.Location) time.Time {
	return LocalTime(year, month, day, hour, 0, loc)
}

func LocalTime(year int, month time.Month, day, hour, minute int, loc *time.Location) time.Time {
	want := time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
	wantYear, wantMonth, wantDay := want.Date()
	wantHour, wantMinute := want.Hour(), want.Minute()
	start := want.Add(-48 * time.Hour)
	limit := want.Add(48 * time.Hour)
	var first time.Time
	for t, steps := start, 0; t.Before(limit) && steps < 16; steps++ {
		local := t.In(loc)
		_, offset := local.Zone()
		at := want.Add(-time.Duration(offset) * time.Second)
		_, end := local.ZoneBounds()
		if !at.Before(t) && (end.IsZero() || at.Before(end)) {
			actual := at.In(loc)
			if actual.Year() == wantYear && actual.Month() == wantMonth && actual.Day() == wantDay && actual.Hour() == wantHour && actual.Minute() == wantMinute {
				if first.IsZero() || at.Before(first) {
					first = at
				}
			}
		}
		if end.IsZero() || !end.After(t) || !end.Before(limit) {
			break
		}
		before := end.Add(-time.Nanosecond).In(loc)
		after := end.In(loc)
		beforeWall := time.Date(before.Year(), before.Month(), before.Day(), before.Hour(), before.Minute(), before.Second(), before.Nanosecond(), time.UTC)
		afterWall := time.Date(after.Year(), after.Month(), after.Day(), after.Hour(), after.Minute(), after.Second(), after.Nanosecond(), time.UTC)
		if afterWall.After(beforeWall) && want.After(beforeWall) && want.Before(afterWall) {
			return end
		}
		t = end
	}
	if !first.IsZero() {
		return first
	}
	return time.Date(year, month, day, hour, minute, 0, 0, loc)
}
