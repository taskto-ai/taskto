package schedule

import "time"

// LocalHour is hour o'clock of a calendar date in loc, as an instant. The date is counted on the
// calendar, never as a number of 24-hour days from another instant, so it is right across a change
// to or from daylight saving time; a day or a month past the end of its own (day 32, month 13) is
// normalised as time.Date does.
//
// time.Date says an hour inside a gap (the clocks skip it) is one of two moments around it,
// whichever it picks; this is always the later: the first moment the wall clock shows the hour or
// more, wherever the gap ends (a change of 30 minutes, or of 3 hours from 00:01, included). An
// hour that happens twice (the clocks go back) is its first time.
//
// It walks the periods in which loc's offset from UTC is constant, from before the hour could
// have come to the first one whose wall clock gets there, so the answer is the exact instant of a
// transition when the hour falls in a gap.
func LocalHour(year int, month time.Month, day, hour int, loc *time.Location) time.Time {
	want := time.Date(year, month, day, hour, 0, 0, 0, time.UTC) // the wall clock's reading, as if in UTC
	// No offset is more than 14 hours from UTC, so the wall clock at this instant is before want.
	t := want.Add(-15 * time.Hour)
	for range 16 {
		local := t.In(loc)
		_, offset := local.Zone()
		// In this period, the wall clock shows want at this instant, or from its start if it
		// began after that (the clocks jumped over want).
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
	// Not reached for any zone in the database; the standard library's pick is a fair answer.
	return time.Date(year, month, day, hour, 0, 0, 0, loc)
}
