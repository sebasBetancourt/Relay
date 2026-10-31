package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/sebasBetancourt/relay/internal/oauth"
)

func TestWindowsZone(t *testing.T) {
	tests := []struct{ iana, want string }{
		{"America/Bogota", "SA Pacific Standard Time"},
		{"America/New_York", "Eastern Standard Time"},
		{"America/Los_Angeles", "Pacific Standard Time"},
		{"America/Mexico_City", "Central Standard Time (Mexico)"},
		{"America/Sao_Paulo", "E. South America Standard Time"},
		{"Europe/London", "GMT Standard Time"},
		{"Europe/Paris", "Romance Standard Time"},
		{"Asia/Tokyo", "Tokyo Standard Time"},
		{"UTC", "UTC"},
		{"Asia/Kolkata", "India Standard Time"}, // CLDR lists Asia/Calcutta
		{"Europe/Kyiv", "FLE Standard Time"},    // CLDR lists Europe/Kiev
	}
	for _, tt := range tests {
		if got, ok := windowsZone(tt.iana); !ok || got != tt.want {
			t.Errorf("windowsZone(%q) = %q, %v, want %q", tt.iana, got, ok, tt.want)
		}
	}
}

func TestWindowsZoneRefusesWhatCLDRDoesNotList(t *testing.T) {
	if got, ok := windowsZone("Mars/Olympus"); ok {
		t.Fatalf("got %q, want no guess", got)
	}
}

// Every alias must land on a real entry, or a rename silently breaks a zone.
func TestCLDRAliasesExist(t *testing.T) {
	for modern, old := range cldrAliases {
		if _, ok := ianaWindowsZones[old]; !ok {
			t.Errorf("%s points at %s, which CLDR does not list", modern, old)
		}
	}
}

// IANA → Windows → IANA reaches a zone with the same rules, for the primary
// zone of each Windows name.
func TestZoneRoundTrip(t *testing.T) {
	for windows, iana := range windowsZones {
		if got, ok := ianaWindowsZones[iana]; !ok || got != windows {
			t.Errorf("%s: reverse = %q, %v, want %q", iana, got, ok, windows)
		}
	}
}

func validInput() EventInput {
	return EventInput{
		Title: "Planning", Start: "2026-11-02T09:00:00-05:00", End: "2026-11-02T10:00:00-05:00",
		TimeZone: "America/Bogota",
	}
}

func TestChangeFromInputRefused(t *testing.T) {
	tests := []struct {
		name string
		edit func(*EventInput)
		code string
	}{
		{"end before start", func(in *EventInput) { in.End = "2026-11-02T08:00:00-05:00" }, CodeInvalidEvent},
		{"end equals start", func(in *EventInput) { in.End = in.Start }, CodeInvalidEvent},
		{"no timezone", func(in *EventInput) { in.TimeZone = "" }, CodeInvalidEvent},
		{"not iana", func(in *EventInput) { in.TimeZone = "SA Pacific Standard Time" }, CodeInvalidEvent},
		{"no offset", func(in *EventInput) { in.Start = "2026-11-02T09:00:00" }, CodeInvalidEvent},
		{"no windows zone", func(in *EventInput) { in.TimeZone = "Australia/Tasmania" }, CodeUnsupportedTimezone},
		{"date on a timed event", func(in *EventInput) { in.Start = "2026-11-02" }, CodeInvalidEvent},
		{"time on an all-day event", func(in *EventInput) { in.AllDay = true }, CodeInvalidEvent},
		{"same day all-day", func(in *EventInput) {
			in.AllDay, in.Start, in.End = true, "2026-11-02", "2026-11-02"
		}, CodeInvalidEvent},
		{"attendee with a name", func(in *EventInput) { in.Attendees = []string{"Ana <ana@example.com>"} }, CodeInvalidEvent},
		{"bad attendee", func(in *EventInput) { in.Attendees = []string{"nope"} }, CodeInvalidEvent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validInput()
			tt.edit(&in)
			_, err := ChangeFromInput(in)
			var v *ValidationError
			if !errors.As(err, &v) || v.Code != tt.code {
				t.Fatalf("err = %v, want %s", err, tt.code)
			}
		})
	}
}

func TestAttendeesAreLowercasedAndDeduped(t *testing.T) {
	in := validInput()
	in.Attendees = []string{"Ana@Example.com", "ana@example.com ", "beto@example.com"}
	c, err := ChangeFromInput(in)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ana@example.com", "beto@example.com"}; !reflect.DeepEqual(*c.Attendees, want) {
		t.Fatalf("attendees = %v", *c.Attendees)
	}
}

func TestChangeFromPatchRefused(t *testing.T) {
	start := "2026-11-02T09:00:00-05:00"
	tests := []struct {
		name string
		p    EventPatch
		code string
	}{
		{"empty", EventPatch{}, CodeInvalidEvent},
		{"start alone", EventPatch{Start: &start}, CodeInvalidEvent},
		{"timezone alone", EventPatch{TimeZone: strPtr("America/Bogota")}, CodeInvalidEvent},
		{"recurrence without a time", EventPatch{Recurrence: &Recurrence{Freq: "DAILY"}}, CodeUnsupportedRecurrence},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ChangeFromPatch(tt.p)
			var v *ValidationError
			if !errors.As(err, &v) || v.Code != tt.code {
				t.Fatalf("err = %v, want %s", err, tt.code)
			}
		})
	}
}

// Both providers get the same local time, each in its own zone vocabulary.
func TestBodiesKeepTheLocalTime(t *testing.T) {
	c, err := ChangeFromInput(validInput())
	if err != nil {
		t.Fatal(err)
	}
	g := googleBody(c, false)["start"].(map[string]any)
	if g["dateTime"] != "2026-11-02T09:00:00-05:00" || g["timeZone"] != "America/Bogota" {
		t.Errorf("google start = %v", g)
	}
	m := graphBody(c)["start"].(graphDateTime)
	if m.DateTime != "2026-11-02T09:00:00" || m.TimeZone != "SA Pacific Standard Time" {
		t.Errorf("graph start = %+v", m)
	}
}

// A client may send the instant in any offset: the wall clock follows the zone.
func TestStartInUTCStillBecomesLocalTime(t *testing.T) {
	in := validInput()
	in.Start, in.End = "2026-11-02T14:00:00Z", "2026-11-02T15:00:00Z"
	c, err := ChangeFromInput(in)
	if err != nil {
		t.Fatal(err)
	}
	if m := graphBody(c)["start"].(graphDateTime); m.DateTime != "2026-11-02T09:00:00" {
		t.Fatalf("graph start = %+v", m)
	}
}

func TestAllDayBodies(t *testing.T) {
	c, err := ChangeFromInput(EventInput{Title: "Off", AllDay: true, Start: "2026-11-02", End: "2026-11-03"})
	if err != nil {
		t.Fatal(err)
	}
	if got := googleBody(c, false)["end"].(map[string]any); got["date"] != "2026-11-03" {
		t.Errorf("google end = %v", got)
	}
	body := graphBody(c)
	if body["isAllDay"] != true {
		t.Error("isAllDay must be true")
	}
	if got := body["end"].(graphDateTime); got.DateTime != "2026-11-03T00:00:00" {
		t.Errorf("graph end = %+v", got)
	}
}

// events.patch merges into start, so switching kinds must clear the other field.
func TestGooglePatchClearsTheOtherTimeField(t *testing.T) {
	allDay := true
	c, err := ChangeFromPatch(EventPatch{Start: strPtr("2026-11-02"), End: strPtr("2026-11-03"), AllDay: &allDay})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(googleBody(c, true)["start"])
	if got, want := string(raw), `{"date":"2026-11-02","dateTime":null,"timeZone":null}`; got != want {
		t.Fatalf("start = %s, want %s", got, want)
	}
}

// fakeWriter records calls and answers with a fixed kind.
type fakeWriter struct {
	fakeSource
	kind    EventKind
	err     error
	errOnce error
	creates int
	updates int
	deletes int
	lastVer string
	tokens  []string
}

func (f *fakeWriter) CreateEvent(_ context.Context, token string, _ EventChange) (Event, error) {
	f.creates++
	f.tokens = append(f.tokens, token)
	if err := f.errOnce; err != nil {
		f.errOnce = nil
		return Event{}, err
	}
	return Event{ID: "e1"}, f.err
}
func (f *fakeWriter) EventKind(context.Context, string, string) (EventKind, error) {
	return f.kind, nil
}
func (f *fakeWriter) UpdateEvent(_ context.Context, _, _ string, _ EventChange, version string) (Event, error) {
	f.updates++
	f.lastVer = version
	return Event{ID: "e1"}, f.err
}
func (f *fakeWriter) DeleteEvent(context.Context, string, string) error {
	f.deletes++
	return f.err
}

func writeService(w *fakeWriter, scopes ...string) (*Service, *fakeTokens) {
	tok := &fakeTokens{
		access:  oauth.AccessToken{Value: "a1", Provider: oauth.ProviderGoogle, Scopes: scopes},
		refresh: oauth.AccessToken{Value: "a2", Provider: oauth.ProviderGoogle, Scopes: scopes},
	}
	return NewService(tok, map[oauth.Provider]Source{oauth.ProviderGoogle: w}), tok
}

func TestWriteNeedsTheWriteScope(t *testing.T) {
	w := &fakeWriter{}
	svc, _ := writeService(w, oauth.GoogleCalendarReadonly)
	_, err := svc.CreateEvent(context.Background(), "t", "a", validInput())
	var scope *InsufficientScopeError
	if !errors.As(err, &scope) || scope.Required[0] != oauth.GoogleCalendarEvents {
		t.Fatalf("err = %v", err)
	}
	if w.creates != 0 {
		t.Fatal("nothing may reach the provider")
	}
}

func TestProviderScopeRefusalBecomesInsufficientScope(t *testing.T) {
	svc, _ := writeService(&fakeWriter{err: ErrScopeRejected}, oauth.GoogleCalendarEvents)
	_, err := svc.CreateEvent(context.Background(), "t", "a", validInput())
	var scope *InsufficientScopeError
	if !errors.As(err, &scope) {
		t.Fatalf("err = %v", err)
	}
}

func TestWriteRetriesOnceAfter401(t *testing.T) {
	w := &fakeWriter{errOnce: ErrTokenRejected}
	svc, _ := writeService(w, oauth.GoogleCalendarEvents)
	if _, err := svc.CreateEvent(context.Background(), "t", "a", validInput()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w.tokens, []string{"a1", "a2"}) {
		t.Fatalf("tokens = %v", w.tokens)
	}
}

func TestInvalidInputNeverReachesTheProvider(t *testing.T) {
	w := &fakeWriter{}
	svc, _ := writeService(w, oauth.GoogleCalendarEvents)
	in := validInput()
	in.Recurrence = &Recurrence{Freq: "HOURLY"}
	if _, err := svc.CreateEvent(context.Background(), "t", "a", in); err == nil {
		t.Fatal("expected a validation error")
	}
	if w.creates != 0 {
		t.Fatal("nothing may be created")
	}
}

func TestSingleOccurrenceIsRefused(t *testing.T) {
	w := &fakeWriter{kind: KindOccurrence}
	svc, _ := writeService(w, oauth.GoogleCalendarEvents)
	title := "New"
	if _, err := svc.UpdateEvent(context.Background(), "t", "a", "e", EventPatch{Title: &title}, ""); !errors.Is(err, errOccurrence) {
		t.Fatalf("update err = %v", err)
	}
	if err := svc.DeleteEvent(context.Background(), "t", "a", "e"); !errors.Is(err, errOccurrence) {
		t.Fatalf("delete err = %v", err)
	}
	if w.updates != 0 || w.deletes != 0 {
		t.Fatal("the series must stay intact")
	}
}

func TestMovingASeriesNeedsItsRecurrence(t *testing.T) {
	w := &fakeWriter{kind: KindSeries}
	svc, _ := writeService(w, oauth.GoogleCalendarEvents)
	p := EventPatch{Start: strPtr("2026-11-03T09:00:00-05:00"), End: strPtr("2026-11-03T10:00:00-05:00"), TimeZone: strPtr("America/Bogota")}
	_, err := svc.UpdateEvent(context.Background(), "t", "a", "e", p, "")
	var v *ValidationError
	if !errors.As(err, &v) || v.Code != CodeUnsupportedRecurrence {
		t.Fatalf("err = %v", err)
	}
	title := "Renamed"
	if _, err := svc.UpdateEvent(context.Background(), "t", "a", "e", EventPatch{Title: &title}, "v1"); err != nil {
		t.Fatalf("renaming a series: %v", err)
	}
	if w.lastVer != "v1" {
		t.Fatalf("version = %q", w.lastVer)
	}
}
