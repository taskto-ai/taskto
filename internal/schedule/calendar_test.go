package schedule

import (
	"errors"
	"testing"
	"time"
)

func TestCalendarNext(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Lisbon")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		cal   Calendar
		after time.Time
		want  time.Time
	}{
		{
			name:  "weekday at local time",
			cal:   Calendar{Days: []string{"fri", "mon", "tue", "wed", "thu"}, At: "08:30", Zone: "Europe/Lisbon"},
			after: time.Date(2026, time.January, 2, 19, 0, 0, 0, loc),
			want:  time.Date(2026, time.January, 5, 8, 30, 0, 0, loc).UTC(),
		},
		{
			name:  "spring gap advances to first valid instant",
			cal:   Calendar{At: "01:30", Zone: "Europe/Lisbon"},
			after: time.Date(2026, time.March, 28, 23, 0, 0, 0, loc),
			want:  time.Date(2026, time.March, 29, 2, 0, 0, 0, loc).UTC(),
		},
		{
			name:  "fall overlap runs once at its first instant",
			cal:   Calendar{At: "01:30", Zone: "Europe/Lisbon"},
			after: time.Date(2026, time.October, 24, 23, 0, 0, 0, loc),
			want:  time.Date(2026, time.October, 25, 0, 30, 0, 0, time.UTC),
		},
		{
			name:  "every two hours on selected day",
			cal:   Calendar{Days: []string{"sat"}, EveryMin: 120, Zone: "Europe/Lisbon"},
			after: time.Date(2026, time.January, 3, 0, 0, 0, 0, loc),
			want:  time.Date(2026, time.January, 3, 2, 0, 0, 0, loc).UTC(),
		},
		{
			name:  "selected day advances to next week",
			cal:   Calendar{Days: []string{"sat"}, EveryMin: 120, Zone: "Europe/Lisbon"},
			after: time.Date(2026, time.January, 4, 0, 0, 0, 0, loc),
			want:  time.Date(2026, time.January, 10, 0, 0, 0, 0, loc).UTC(),
		},
		{
			name:  "daily interval advances to tomorrow",
			cal:   Calendar{EveryMin: 1440, Zone: "UTC"},
			after: time.Date(2026, time.January, 3, 1, 0, 0, 0, time.UTC),
			want:  time.Date(2026, time.January, 4, 0, 0, 0, 0, time.UTC),
		},
		{
			name:  "civil date advances across a midnight gap",
			cal:   Calendar{At: "08:30", Zone: "America/Santiago"},
			after: time.Date(2026, time.September, 5, 12, 0, 0, 0, mustLocation(t, "America/Santiago")),
			want:  time.Date(2026, time.September, 6, 11, 30, 0, 0, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.cal.Next(tt.after)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(tt.want) {
				t.Fatalf("Next = %v, want %v", got, tt.want)
			}
			if !got.After(tt.after) {
				t.Fatalf("Next = %v, not after %v", got, tt.after)
			}
		})
	}
}

func TestCalendarNormal(t *testing.T) {
	got, err := (Calendar{Days: []string{"SUN", "mon", "sun", "wed"}, At: "08:05", Zone: "Europe/Lisbon"}).Normal()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"mon", "wed", "sun"}
	if len(got.Days) != len(want) {
		t.Fatalf("Days = %v, want %v", got.Days, want)
	}
	for i := range want {
		if got.Days[i] != want[i] {
			t.Fatalf("Days = %v, want %v", got.Days, want)
		}
	}
	if got.At != "08:05" {
		t.Fatalf("At = %q, want 08:05", got.At)
	}
}

func TestCalendarRejectsInvalidValues(t *testing.T) {
	tests := []Calendar{
		{At: "08:00", Zone: "Local"},
		{At: "08:00", Zone: "Not/A_Zone"},
		{At: "24:00", Zone: "UTC"},
		{At: "8:5", Zone: "UTC"},
		{EveryMin: 30, Zone: "UTC"},
		{Days: []string{"mon"}, EveryMin: 30, Zone: "UTC"},
		{At: "08:00", EveryMin: 60, Zone: "UTC"},
		{At: "08:00", Days: []string{"monday"}, Zone: "UTC"},
	}
	for _, cal := range tests {
		if err := cal.Check(); !errors.Is(err, ErrCalendar) {
			t.Errorf("Check(%+v) = %v, want ErrCalendar", cal, err)
		}
	}
}

func TestCalendarNextReturnsStrictlyAfter(t *testing.T) {
	cal := Calendar{At: "08:30", Zone: "UTC"}
	after := time.Date(2026, time.January, 5, 8, 30, 0, 0, time.UTC)
	got, err := cal.Next(after)
	if err != nil {
		t.Fatal(err)
	}
	want := after.AddDate(0, 0, 1)
	if !got.Equal(want) || !got.After(after) {
		t.Fatalf("Next = %v, want %v strictly after %v", got, want, after)
	}
}

func TestCalendarNextClampsToSchedulerRange(t *testing.T) {
	cal := Calendar{At: "08:30", Zone: "UTC"}
	got, err := cal.Next(time.Date(1999, time.December, 31, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(MinRunAt) {
		t.Fatalf("Next = %v, want lower bound %v", got, MinRunAt)
	}
	if _, err := cal.Next(MaxRunAt); !errors.Is(err, ErrCalendar) {
		t.Fatalf("Next at upper bound = %v, want ErrCalendar", err)
	}
}

func TestLocalTimeGapAndOverlap(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Lisbon")
	if err != nil {
		t.Fatal(err)
	}
	gap := LocalTime(2026, time.March, 29, 1, 30, loc)
	wantGap := time.Date(2026, time.March, 29, 2, 0, 0, 0, loc)
	if !gap.Equal(wantGap) {
		t.Fatalf("gap LocalTime = %v, want %v", gap, wantGap)
	}
	overlap := LocalTime(2026, time.October, 25, 1, 30, loc)
	if !overlap.Equal(time.Date(2026, time.October, 25, 0, 30, 0, 0, time.UTC)) {
		t.Fatalf("overlap LocalTime = %v, want first occurrence with UTC+1", overlap)
	}
}

func TestLocalTimeNormalizesOverflowDateBeforeComparing(t *testing.T) {
	loc, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		t.Fatal(err)
	}
	got := LocalTime(2029, time.March, 32, 2, 0, loc)
	want := time.Date(2029, time.March, 31, 15, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("LocalTime = %v, want first instant of normalized date %v", got, want)
	}
}

func TestCalendarNextAdvancesAcrossAYearOfTransitions(t *testing.T) {
	zones := []string{"Europe/Lisbon", "America/Santiago", "Australia/Lord_Howe", "America/Sao_Paulo"}
	cals := []Calendar{
		{At: "00:00"},
		{At: "01:30"},
		{At: "02:30"},
		{EveryMin: 60},
		{EveryMin: 1440},
		{Days: []string{"sat", "sun"}, EveryMin: 90},
	}
	for _, zone := range zones {
		for _, cal := range cals {
			cal.Zone = zone
			after := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
			for range 24 * 365 / 5 {
				got, err := cal.Next(after)
				if err != nil {
					t.Fatalf("%s %+v after %v: %v", zone, cal, after, err)
				}
				if !got.After(after) {
					t.Fatalf("%s %+v: Next(%v) = %v, not after", zone, cal, after, got)
				}
				if got.Sub(after) > 8*24*time.Hour {
					t.Fatalf("%s %+v: Next(%v) = %v, too far", zone, cal, after, got)
				}
				after = after.Add(5 * time.Hour)
			}
		}
	}
}

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}
