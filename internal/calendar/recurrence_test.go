package calendar

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func strPtr(s string) *string { return &s }
func intPtr(n int) *int       { return &n }

func bogotaSchedule(t *testing.T, start string) schedule {
	t.Helper()
	st, err := time.Parse(time.RFC3339, start)
	if err != nil {
		t.Fatal(err)
	}
	s, err := parseSchedule(start, st.Add(time.Hour).Format(time.RFC3339), "America/Bogota", false)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The core of the feature: one Recurrence, the exact text for each provider.
// 2026-11-02 is a Monday.
func TestRecurrenceTranslation(t *testing.T) {
	const monday = "2026-11-02T09:00:00-05:00"
	const windows = "SA Pacific Standard Time"
	tests := []struct {
		name  string
		start string
		in    Recurrence
		rrule string
		graph graphRecurrence
	}{
		{
			name: "every monday and wednesday, 10 times", start: monday,
			in:    Recurrence{Freq: "WEEKLY", ByDay: []string{"WE", "MO"}, Count: intPtr(10)},
			rrule: "RRULE:FREQ=WEEKLY;BYDAY=MO,WE;COUNT=10",
			graph: graphRecurrence{
				Pattern: graphPattern{Type: "weekly", Interval: 1, DaysOfWeek: []string{"monday", "wednesday"}, FirstDayOfWeek: "monday"},
				Range:   graphRange{Type: "numbered", StartDate: "2026-11-02", NumberOfOccurrences: 10, RecurrenceTimeZone: windows},
			},
		},
		{
			name: "weekly without by_day uses the start's weekday", start: monday,
			in:    Recurrence{Freq: "WEEKLY"},
			rrule: "RRULE:FREQ=WEEKLY;BYDAY=MO",
			graph: graphRecurrence{
				Pattern: graphPattern{Type: "weekly", Interval: 1, DaysOfWeek: []string{"monday"}, FirstDayOfWeek: "monday"},
				Range:   graphRange{Type: "noEnd", StartDate: "2026-11-02", RecurrenceTimeZone: windows},
			},
		},
		{
			name: "daily with interval until a date", start: monday,
			in:    Recurrence{Freq: "DAILY", Interval: 2, Until: strPtr("2026-12-31")},
			rrule: "RRULE:FREQ=DAILY;INTERVAL=2;UNTIL=20270101T045959Z", // 23:59:59 Bogota
			graph: graphRecurrence{
				Pattern: graphPattern{Type: "daily", Interval: 2},
				Range:   graphRange{Type: "endDate", StartDate: "2026-11-02", EndDate: "2026-12-31", RecurrenceTimeZone: windows},
			},
		},
		{
			name: "monthly by day of month", start: "2026-11-15T09:00:00-05:00",
			in:    Recurrence{Freq: "MONTHLY", Count: intPtr(6)},
			rrule: "RRULE:FREQ=MONTHLY;COUNT=6",
			graph: graphRecurrence{
				Pattern: graphPattern{Type: "absoluteMonthly", Interval: 1, DayOfMonth: 15},
				Range:   graphRange{Type: "numbered", StartDate: "2026-11-15", NumberOfOccurrences: 6, RecurrenceTimeZone: windows},
			},
		},
		{
			name: "yearly", start: "2026-11-15T09:00:00-05:00",
			in:    Recurrence{Freq: "YEARLY", Interval: 2},
			rrule: "RRULE:FREQ=YEARLY;INTERVAL=2",
			graph: graphRecurrence{
				Pattern: graphPattern{Type: "absoluteYearly", Interval: 2, DayOfMonth: 15, Month: 11},
				Range:   graphRange{Type: "noEnd", StartDate: "2026-11-15", RecurrenceTimeZone: windows},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule, err := parseRecurrence(tt.in, bogotaSchedule(t, tt.start))
			if err != nil {
				t.Fatal(err)
			}
			if got := rule.rrule(); got != tt.rrule {
				t.Errorf("rrule = %q, want %q", got, tt.rrule)
			}
			if got := rule.patternedRecurrence(); !reflect.DeepEqual(got, tt.graph) {
				t.Errorf("graph = %+v, want %+v", got, tt.graph)
			}
		})
	}
}

func TestRecurrenceAllDayUsesDates(t *testing.T) {
	s, err := parseSchedule("2026-11-02", "2026-11-03", "", true)
	if err != nil {
		t.Fatal(err)
	}
	rule, err := parseRecurrence(Recurrence{Freq: "WEEKLY", Until: strPtr("2026-12-28")}, s)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rule.rrule(), "RRULE:FREQ=WEEKLY;BYDAY=MO;UNTIL=20261228"; got != want {
		t.Fatalf("rrule = %q, want %q", got, want)
	}
	if got := rule.patternedRecurrence().Range.RecurrenceTimeZone; got != "UTC" {
		t.Fatalf("recurrenceTimeZone = %q", got)
	}
}

// In a zone with DST the last day must end at that day's offset, not today's.
func TestRecurrenceUntilUsesTheOffsetOfThatDay(t *testing.T) {
	s, err := parseSchedule("2026-11-02T09:00:00-05:00", "2026-11-02T10:00:00-05:00", "America/New_York", false)
	if err != nil {
		t.Fatal(err)
	}
	rule, err := parseRecurrence(Recurrence{Freq: "DAILY", Until: strPtr("2027-07-01")}, s)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rule.rrule(), "RRULE:FREQ=DAILY;UNTIL=20270702T035959Z"; got != want {
		t.Fatalf("rrule = %q, want %q (EDT is -04:00)", got, want)
	}
}

func TestRecurrenceRefused(t *testing.T) {
	const monday = "2026-11-02T09:00:00-05:00"
	tests := []struct {
		name  string
		start string
		in    Recurrence
	}{
		{"unknown freq", monday, Recurrence{Freq: "HOURLY"}},
		{"lowercase freq", monday, Recurrence{Freq: "weekly"}},
		{"negative interval", monday, Recurrence{Freq: "DAILY", Interval: -1}},
		{"until and count", monday, Recurrence{Freq: "DAILY", Until: strPtr("2026-12-01"), Count: intPtr(3)}},
		{"zero count", monday, Recurrence{Freq: "DAILY", Count: intPtr(0)}},
		{"bad until", monday, Recurrence{Freq: "DAILY", Until: strPtr("soon")}},
		{"until before start", monday, Recurrence{Freq: "DAILY", Until: strPtr("2026-01-01")}},
		{"by_day on monthly", monday, Recurrence{Freq: "MONTHLY", ByDay: []string{"MO"}}},
		{"unknown day", monday, Recurrence{Freq: "WEEKLY", ByDay: []string{"MO", "XX"}}},
		{"nth weekday", monday, Recurrence{Freq: "WEEKLY", ByDay: []string{"1MO"}}},
		{"start outside by_day", monday, Recurrence{Freq: "WEEKLY", ByDay: []string{"TU"}}},
		{"monthly on the 31st", "2026-10-31T09:00:00-05:00", Recurrence{Freq: "MONTHLY"}},
		{"yearly on february 29", "2028-02-29T09:00:00-05:00", Recurrence{Freq: "YEARLY"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseRecurrence(tt.in, bogotaSchedule(t, tt.start))
			var v *ValidationError
			if !errors.As(err, &v) || v.Code != CodeUnsupportedRecurrence {
				t.Fatalf("err = %v, want %s", err, CodeUnsupportedRecurrence)
			}
		})
	}
}

// The local date decides: 23:00 on Monday in Bogota is already Tuesday in UTC.
func TestRecurrenceWeekdayIsLocal(t *testing.T) {
	s := bogotaSchedule(t, "2026-11-03T04:00:00Z") // Monday 23:00 in Bogota
	if _, err := parseRecurrence(Recurrence{Freq: "WEEKLY", ByDay: []string{"MO"}}, s); err != nil {
		t.Fatalf("err = %v", err)
	}
}
