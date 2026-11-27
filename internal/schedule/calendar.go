package schedule

import (
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
)

var ErrCalendar = errors.New("invalid calendar")

type Calendar struct {
	Days     []string `json:"days,omitempty"`
	At       string   `json:"at,omitempty"`
	EveryMin int      `json:"every_minutes,omitempty"`
	Zone     string   `json:"time_zone"`
}

type CalendarText struct {
	Days  []string `json:"days,omitempty"`
	At    string   `json:"at,omitempty"`
	Every int      `json:"every,omitempty"`
	Zone  string   `json:"zone"`
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday,
	"mon": time.Monday,
	"tue": time.Tuesday,
	"wed": time.Wednesday,
	"thu": time.Thursday,
	"fri": time.Friday,
	"sat": time.Saturday,
}

var weekdayOrder = map[string]int{
	"mon": 0,
	"tue": 1,
	"wed": 2,
	"thu": 3,
	"fri": 4,
	"sat": 5,
	"sun": 6,
}

func (c Calendar) Normal() (Calendar, error) {
	c.Zone = strings.TrimSpace(c.Zone)
	if c.Zone == "" || c.Zone == "Local" {
		return Calendar{}, ErrCalendar
	}
	if _, err := time.LoadLocation(c.Zone); err != nil {
		return Calendar{}, ErrCalendar
	}
	if (c.At == "") == (c.EveryMin == 0) {
		return Calendar{}, ErrCalendar
	}
	if c.At != "" {
		if len(c.At) != 5 || c.At[2] != ':' {
			return Calendar{}, ErrCalendar
		}
		hour, errHour := strconv.Atoi(c.At[:2])
		minute, errMinute := strconv.Atoi(c.At[3:])
		if errHour != nil || errMinute != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 || c.At[:2] != twoDigits(hour) || c.At[3:] != twoDigits(minute) {
			return Calendar{}, ErrCalendar
		}
	}
	if c.EveryMin != 0 && (c.EveryMin < 60 || c.EveryMin > 1440) {
		return Calendar{}, ErrCalendar
	}
	days := make([]string, 0, len(c.Days))
	seen := make(map[string]bool, len(c.Days))
	for _, day := range c.Days {
		day = strings.ToLower(day)
		if _, ok := weekdays[day]; !ok {
			return Calendar{}, ErrCalendar
		}
		if !seen[day] {
			seen[day] = true
			days = append(days, day)
		}
	}
	sort.Slice(days, func(i, j int) bool { return weekdayOrder[days[i]] < weekdayOrder[days[j]] })
	c.Days = days
	return c, nil
}

func (c Calendar) Check() error {
	_, err := c.Normal()
	return err
}

func (c Calendar) Next(after time.Time) (time.Time, error) {
	normal, err := c.Normal()
	if err != nil || after.After(MaxRunAt) || !after.Before(MaxRunAt) {
		return time.Time{}, ErrCalendar
	}
	loc, err := time.LoadLocation(normal.Zone)
	if err != nil {
		return time.Time{}, ErrCalendar
	}
	localAfter := after.In(loc)
	start := time.Date(localAfter.Year(), localAfter.Month(), localAfter.Day(), 0, 0, 0, 0, loc)
	limitDays := 2
	if normal.At != "" || len(normal.Days) != 0 {
		limitDays = 8
	}
	for offset := 0; offset < limitDays; offset++ {
		date := start.AddDate(0, 0, offset)
		if !normal.matchesDay(date.Weekday()) {
			continue
		}
		if normal.At != "" {
			hour, _ := strconv.Atoi(normal.At[:2])
			minute, _ := strconv.Atoi(normal.At[3:])
			candidate := LocalTime(date.Year(), date.Month(), date.Day(), hour, minute, loc).UTC()
			if candidate.After(after) {
				return clampCalendarTime(candidate)
			}
			continue
		}
		for minute := 0; minute < 1440; minute += normal.EveryMin {
			hour, minuteOfHour := minute/60, minute%60
			candidate := LocalTime(date.Year(), date.Month(), date.Day(), hour, minuteOfHour, loc).UTC()
			if candidate.After(after) {
				return clampCalendarTime(candidate)
			}
		}
	}
	return time.Time{}, ErrCalendar
}

func (c Calendar) Describe() CalendarText {
	normal, err := c.Normal()
	if err != nil {
		return CalendarText{}
	}
	return CalendarText{Days: normal.Days, At: normal.At, Every: normal.EveryMin, Zone: normal.Zone}
}

func (c Calendar) matchesDay(day time.Weekday) bool {
	if len(c.Days) == 0 {
		return true
	}
	for _, allowed := range c.Days {
		if weekdays[allowed] == day {
			return true
		}
	}
	return false
}

func clampCalendarTime(t time.Time) (time.Time, error) {
	if t.Before(MinRunAt) {
		t = MinRunAt
	}
	if t.After(MaxRunAt) {
		return time.Time{}, ErrCalendar
	}
	return t.UTC(), nil
}

func twoDigits(value int) string {
	if value < 10 {
		return "0" + strconv.Itoa(value)
	}
	return strconv.Itoa(value)
}
