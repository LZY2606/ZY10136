package ics

// Time-shape tests: DATE, floating local DATE-TIME, UTC and TZID values must
// map deterministically and serializing must not be influenced by the host
// location or the TZ environment variable. The generated round-trip test
// (gen_roundtrip_test.go) checks the four shapes via the typed getter; this
// file pins serialization invariance across process time zones.

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"
)

// TestTimeShapes_FourForms parses the four canonical DTSTART forms and checks
// each one independently. Floating time is compared by wall clock only; it
// never asserts a location the host would have to provide.
func TestTimeShapes_FourForms(t *testing.T) {
	zone := time.FixedZone("Generated/Zone_2021", 5*3600+30*60)
	mapper := func(tzid string) *time.Location {
		if tzid == genGeneratedTZID {
			return zone
		}
		return nil
	}
	cases := []struct {
		name  string
		line  string
		check func(t *testing.T, got time.Time)
	}{
		{
			name: "utc",
			line: "DTSTART:20210627T123045Z",
			check: func(t *testing.T, got time.Time) {
				if got.Location() != time.UTC {
					t.Fatalf("UTC location expected, got %v", got.Location())
				}
				y, m, d := got.Date()
				if y != 2021 || m != 6 || d != 27 || got.Hour() != 12 || got.Minute() != 30 || got.Second() != 45 {
					t.Fatalf("UTC wall clock wrong: %v", got)
				}
			},
		},
		{
			name: "floating-local",
			line: "DTSTART:20210627T213000",
			check: func(t *testing.T, got time.Time) {
				// Floating time attaches to time.Local for arithmetic, but the
				// serialized raw value is fixed and must not shift.
				y, m, d := got.Date()
				if y != 2021 || m != 6 || d != 27 || got.Hour() != 21 || got.Minute() != 30 || got.Second() != 0 {
					t.Fatalf("floating wall clock wrong: %v", got)
				}
			},
		},
		{
			name: "date",
			line: "DTSTART;VALUE=DATE:20210627",
			check: func(t *testing.T, got time.Time) {
				if got.Hour() != 0 || got.Minute() != 0 || got.Second() != 0 {
					t.Fatalf("DATE must be midnight: %v", got)
				}
				y, m, d := got.Date()
				if y != 2021 || m != 6 || d != 27 {
					t.Fatalf("DATE wrong: %v", got)
				}
			},
		},
		{
			name: "tzid",
			line: "DTSTART;TZID=" + genGeneratedTZID + ":20210627T213000",
			check: func(t *testing.T, got time.Time) {
				if got.Location().String() != genGeneratedTZID {
					t.Fatalf("TZID location expected, got %v", got.Location())
				}
				_, off := got.Zone()
				if off != 5*3600+30*60 {
					t.Fatalf("mapped offset wrong: %d", off)
				}
				y, m, d := got.Date()
				if y != 2021 || m != 6 || d != 27 || got.Hour() != 21 || got.Minute() != 30 || got.Second() != 0 {
					t.Fatalf("TZID wall clock wrong: %v", got)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := wrapCalendar("BEGIN:VEVENT\r\nUID:u\r\n" + tc.line + "\r\nEND:VEVENT\r\n")
			cal := mustParse(t, input, TimezoneMapper(mapper))
			prop := cal.Events()[0].GetProperty(ComponentPropertyDtStart)
			got, err := parseTimeValue(prop.Value, prop.ICalParameters, false, TimezoneMapper(mapper))
			if err != nil {
				t.Fatalf("parseTimeValue: %v", err)
			}
			tc.check(t, got)
		})
	}
}

// TestTimeShapes_SerializationTimezoneInvariant ensures the raw ICS for all
// four shapes is byte-identical regardless of process TZ. The serialized form
// is driven by the raw property values, never by time.Local.
func TestTimeShapes_SerializationTimezoneInvariant(t *testing.T) {
	if os.Getenv("ICAL_TZ_HELPER") == "1" {
		runTZHelper()
		return
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable for TZ subprocess check")
	}
	want := timeShapeWire()
	for _, tz := range []string{"UTC", "America/Los_Angeles", "Asia/Tokyo", "Pacific/Apia"} {
		tz := tz
		t.Run(tz, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run", "TestTimeShapes_SerializationTimezoneInvariant")
			cmd.Env = append(os.Environ(), "ICAL_TZ_HELPER=1", "TZ="+tz)
			var out bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &out
			if err := cmd.Run(); err != nil {
				t.Fatalf("helper under TZ=%s failed: %v\n%s", tz, err, out.String())
			}
			if strings.TrimSpace(out.String()) != want {
				t.Fatalf("serialization under TZ=%s differs\nwant:\n%s\ngot:\n%s", tz, want, out.String())
			}
		})
	}
}

// timeShapeWire builds one calendar containing every time shape and returns
// its CRLF serialization. All values are raw strings, so the result is
// independent of the process zone.
func timeShapeWire() string {
	cal, _ := NewCalendarWithOptions()
	cal.CalendarProperties = cal.CalendarProperties[:0]
	cal.CalendarProperties = append(cal.CalendarProperties, CalendarProperty{BaseProperty: BaseProperty{
		IANAToken: "VERSION", Value: "2.0", ICalParameters: map[string][]string{},
	}})
	add := func(token, value string, params map[string][]string) {
		ev := NewEvent("uid-" + token)
		ev.AddProperty(ComponentProperty(token), value)
		if params != nil {
			ev.Properties[len(ev.Properties)-1].ICalParameters = params
		}
		cal.AddVEvent(ev)
	}
	add("DTSTART", "20210627T123045Z", nil)
	add("DTEND", "20210627T213000", nil)
	add("DTSTART", "20210627", map[string][]string{"VALUE": {"DATE"}})
	add("DTEND", "20210627T213000", map[string][]string{"TZID": {genGeneratedTZID}})
	return strings.TrimSpace(cal.Serialize(WithNewLineWindows))
}

func runTZHelper() {
	os.Stdout.WriteString(timeShapeWire())
	os.Exit(0)
}

// TestTimeShapes_TZIDWriteBackMapper pins serialization-side TZID mapping:
// when the zone database resolves a TZID and a serialization mapper is given,
// the mapped identifier is written to both the DTSTART parameter and a
// referenced VTIMEZONE TZID; unknown TZIDs are passed through verbatim.
func TestTimeShapes_TZIDWriteBackMapper(t *testing.T) {
	mapper := TimezoneSerializationMapper(func(loc *time.Location) (string, bool) {
		if loc.String() == "America/New_York" {
			return "Custom/New_York", true
		}
		return "", false
	})
	input := strings.Join([]string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"BEGIN:VTIMEZONE",
		"TZID:America/New_York",
		"END:VTIMEZONE",
		"BEGIN:VEVENT",
		"UID:u",
		"DTSTART;TZID=America/New_York:20210115T120000",
		"END:VEVENT",
		"BEGIN:VEVENT",
		"UID:u2",
		"DTSTART;TZID=Nowhere/Unmapped:20210115T120000",
		"END:VEVENT",
		"END:VCALENDAR",
		"",
	}, "\r\n")
	cal, err := ParseCalendar(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out := cal.Serialize(WithNewLineWindows, mapper)
	if !strings.Contains(out, "DTSTART;TZID=Custom/New_York:20210115T120000") {
		t.Fatalf("DTSTART TZID not remapped:\n%s", out)
	}
	if !strings.Contains(out, "TZID:Custom/New_York\r\n") {
		t.Fatalf("VTIMEZONE TZID not remapped:\n%s", out)
	}
	if !strings.Contains(out, "DTSTART;TZID=Nowhere/Unmapped:20210115T120000") {
		t.Fatalf("unresolvable TZID must be written back verbatim:\n%s", out)
	}
	// Second serialize without the mapper keeps the mapped text stable.
	cal2, err := ParseCalendar(strings.NewReader(out))
	if err != nil {
		t.Fatalf("reparse mapped output: %v", err)
	}
	if cal2.Serialize(WithNewLineWindows, mapper) != out {
		t.Fatalf("mapped serialization not stable")
	}
}
