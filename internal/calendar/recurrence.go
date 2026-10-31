package calendar

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// weekdays is RRULE's order, with the weekday each code means. Both providers
// get the same order, so a pattern reads the same on either side.
var weekdays = []struct {
	code  string
	day   time.Weekday
	graph string
}{
	{"MO", time.Monday, "monday"},
	{"TU", time.Tuesday, "tuesday"},
	{"WE", time.Wednesday, "wednesday"},
	{"TH", time.Thursday, "thursday"},
	{"FR", time.Friday, "friday"},
	{"SA", time.Saturday, "saturday"},
	{"SU", time.Sunday, "sunday"},
}

// recurrenceRule is a Recurrence checked against the event's start.
type recurrenceRule struct {
	freq     string
	interval int
	days     []string // WEEKLY: codes in weekdays order
	until    string   // YYYY-MM-DD; "" when there is no end date
	count    int
	sched    schedule
}

func unsupported(format string, a ...any) *ValidationError {
	return &ValidationError{Code: CodeUnsupportedRecurrence, Detail: fmt.Sprintf(format, a...)}
}

func parseRecurrence(r Recurrence, s schedule) (recurrenceRule, error) {
	rule := recurrenceRule{freq: r.Freq, interval: r.Interval, sched: s}
	if r.Freq != "DAILY" && r.Freq != "WEEKLY" && r.Freq != "MONTHLY" && r.Freq != "YEARLY" {
		return recurrenceRule{}, unsupported("freq must be DAILY, WEEKLY, MONTHLY or YEARLY")
	}
	if rule.interval == 0 {
		rule.interval = 1
	}
	if rule.interval < 1 {
		return recurrenceRule{}, unsupported("interval must be at least 1")
	}
	if r.Until != nil && r.Count != nil {
		return recurrenceRule{}, unsupported("until and count cannot be combined")
	}
	if r.Count != nil {
		if *r.Count < 1 {
			return recurrenceRule{}, unsupported("count must be at least 1")
		}
		rule.count = *r.Count
	}

	start := s.firstDay()
	if r.Until != nil {
		until, err := time.Parse(time.DateOnly, *r.Until)
		if err != nil {
			return recurrenceRule{}, unsupported("until must be YYYY-MM-DD")
		}
		if until.Before(start) {
			return recurrenceRule{}, unsupported("until is before the event starts")
		}
		rule.until = *r.Until
	}

	if len(r.ByDay) > 0 && r.Freq != "WEEKLY" {
		return recurrenceRule{}, unsupported("by_day is only supported with WEEKLY")
	}
	switch r.Freq {
	case "WEEKLY":
		days, err := weeklyDays(r.ByDay, start)
		if err != nil {
			return recurrenceRule{}, err
		}
		rule.days = days
	case "MONTHLY":
		// Google skips months without that day; Outlook moves to the last day.
		if start.Day() > 28 {
			return recurrenceRule{}, unsupported("monthly events on day %d differ between providers in shorter months", start.Day())
		}
	case "YEARLY":
		if start.Month() == time.February && start.Day() == 29 {
			return recurrenceRule{}, unsupported("yearly events on February 29 differ between providers")
		}
	}
	return rule, nil
}

// weeklyDays orders by_day and requires the first occurrence to be one of
// them. The providers disagree on an event that starts outside its own pattern.
func weeklyDays(byDay []string, start time.Time) ([]string, error) {
	if len(byDay) == 0 {
		byDay = []string{weekdayCode(start.Weekday())}
	}
	days := make([]string, 0, len(byDay))
	for _, w := range weekdays {
		if slices.Contains(byDay, w.code) {
			days = append(days, w.code)
		}
	}
	for _, d := range byDay {
		if !slices.Contains(days, d) {
			return nil, unsupported("by_day %q must be one of MO, TU, WE, TH, FR, SA, SU", d)
		}
	}
	if !slices.Contains(days, weekdayCode(start.Weekday())) {
		return nil, unsupported("the event must start on one of by_day")
	}
	return days, nil
}

func weekdayCode(d time.Weekday) string {
	for _, w := range weekdays {
		if w.day == d {
			return w.code
		}
	}
	return ""
}

// firstDay is the local calendar date the event starts on.
func (s schedule) firstDay() time.Time {
	if s.allDay {
		t, _ := time.Parse(time.DateOnly, s.startDate)
		return t
	}
	return time.Date(s.start.Year(), s.start.Month(), s.start.Day(), 0, 0, 0, 0, time.UTC)
}

// rrule renders the Google form. WKST stays at its default, Monday, which is
// what graphRecurrence sets: with interval > 1 the week start changes the dates.
func (r recurrenceRule) rrule() string {
	parts := []string{"FREQ=" + r.freq}
	if r.interval > 1 {
		parts = append(parts, "INTERVAL="+strconv.Itoa(r.interval))
	}
	if len(r.days) > 0 {
		parts = append(parts, "BYDAY="+strings.Join(r.days, ","))
	}
	if r.count > 0 {
		parts = append(parts, "COUNT="+strconv.Itoa(r.count))
	}
	if r.until != "" {
		parts = append(parts, "UNTIL="+r.untilValue())
	}
	return "RRULE:" + strings.Join(parts, ";")
}

// untilValue follows RFC 5545: a DATE for all-day events, and otherwise the
// UTC instant ending that local day, so the last date stays inclusive as in Graph.
func (r recurrenceRule) untilValue() string {
	day, _ := time.Parse(time.DateOnly, r.until)
	if r.sched.allDay {
		return day.Format("20060102")
	}
	end := time.Date(day.Year(), day.Month(), day.Day(), 23, 59, 59, 0, r.sched.start.Location())
	return end.UTC().Format("20060102T150405Z")
}

type graphPattern struct {
	Type           string   `json:"type"`
	Interval       int      `json:"interval"`
	DaysOfWeek     []string `json:"daysOfWeek,omitempty"`
	FirstDayOfWeek string   `json:"firstDayOfWeek,omitempty"`
	DayOfMonth     int      `json:"dayOfMonth,omitempty"`
	Month          int      `json:"month,omitempty"`
}

type graphRange struct {
	Type                string `json:"type"`
	StartDate           string `json:"startDate"`
	EndDate             string `json:"endDate,omitempty"`
	NumberOfOccurrences int    `json:"numberOfOccurrences,omitempty"`
	RecurrenceTimeZone  string `json:"recurrenceTimeZone"`
}

type graphRecurrence struct {
	Pattern graphPattern `json:"pattern"`
	Range   graphRange   `json:"range"`
}

// patternedRecurrence renders the Graph form of the same rule.
func (r recurrenceRule) patternedRecurrence() graphRecurrence {
	start := r.sched.firstDay()
	out := graphRecurrence{
		Pattern: graphPattern{Interval: r.interval},
		Range: graphRange{
			Type:               "noEnd",
			StartDate:          start.Format(time.DateOnly),
			RecurrenceTimeZone: r.sched.windows,
		},
	}
	switch r.freq {
	case "DAILY":
		out.Pattern.Type = "daily"
	case "WEEKLY":
		out.Pattern.Type = "weekly"
		out.Pattern.FirstDayOfWeek = "monday"
		for _, w := range weekdays {
			if slices.Contains(r.days, w.code) {
				out.Pattern.DaysOfWeek = append(out.Pattern.DaysOfWeek, w.graph)
			}
		}
	case "MONTHLY":
		out.Pattern.Type = "absoluteMonthly"
		out.Pattern.DayOfMonth = start.Day()
	case "YEARLY":
		out.Pattern.Type = "absoluteYearly"
		out.Pattern.DayOfMonth = start.Day()
		out.Pattern.Month = int(start.Month())
	}
	switch {
	case r.count > 0:
		out.Range.Type = "numbered"
		out.Range.NumberOfOccurrences = r.count
	case r.until != "":
		out.Range.Type = "endDate"
		out.Range.EndDate = r.until
	}
	return out
}
