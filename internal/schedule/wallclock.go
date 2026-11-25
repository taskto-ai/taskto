package schedule

import "time"

// LocalHour is hour o'clock of a calendar date in loc, as an instant. The date is counted on the
// calendar, never as a number of 24-hour days from another instant, so it is right across a change
// to or from daylight saving time; a day or a month past the end of its own (day 32, month 13) is
// normalised as time.Date does.
//
// time.Date says an hour inside a gap (the clocks skip it) is one of two moments around it,
// whichever it picks; this is always the later: the first moment the wall clock shows the hour or
// more. An hour that happens twice (the clocks go back) is its first time.
func LocalHour(year int, month time.Month, day, hour int, loc *time.Location) time.Time {
	at := time.Date(year, month, day, hour, 0, 0, 0, loc)
	want := time.Date(year, month, day, hour, 0, 0, 0, time.UTC)
	wall := func(t time.Time) time.Time {
		y, m, d := t.In(loc).Date()
		h, mi, sec := t.In(loc).Clock()
		return time.Date(y, m, d, h, mi, sec, 0, time.UTC)
	}
	// Back to before the gap if the pick was after it (so the walk below finds its start), then
	// forward until the wall clock has reached the hour.
	for i := 0; i < 8 && wall(at.Add(-15*time.Minute)).Compare(want) >= 0; i++ {
		at = at.Add(-15 * time.Minute)
	}
	for i := 0; i < 4*48 && wall(at).Before(want); i++ {
		at = at.Add(15 * time.Minute)
	}
	return at
}
