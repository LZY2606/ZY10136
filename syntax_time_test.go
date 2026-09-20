package ics

// Time-shape and timezone write-back invariants.
//
// The serialization mapping of DATE / floating DATE-TIME / UTC / TZID values
// must not depend on the process-local time.Location or the TZ environment
// variable. These tests construct each shape directly, swap time.Local, and
// re-run the serialization in a subprocess under a hostile $TZ.

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func buildTimeShapeCalendar() *Calendar {
	cal := NewCalendar()

	date := NewEvent("date@example.com")
	date.SetAllDayStartAt(time.Date(2021, 6, 15, 10, 0, 0, 0, time.UTC))
	cal.addComponent(date)

	floating := NewEvent("floating@example.com")
	floating.SetProperty(ComponentPropertyDtStart, "20210615T133000")
	cal.addComponent(floating)

	utc := NewEvent("utc@example.com")
	utc.SetStartAt(time.Date(2021, 6, 15, 13, 30, 0, 0, time.UTC))
	cal.addComponent(utc)

	tzid := NewEvent("tzid@example.com")
	tzid.SetProperty(ComponentPropertyDtStart, "20210314T133000", WithTZID("America/New_York"))
	cal.addComponent(tzid)

	// Direct raw properties cover every branch without relying on setters.
	raw := NewEvent("raw@example.com")
	raw.SetProperty(ComponentPropertyDtStart, "20210615", WithValue(string(ValueDataTypeDate)))
	raw.SetProperty(ComponentPropertyDtEnd, "20210615T133000")
	cal.addComponent(raw)

	return cal
}

var timeShapeExpectedFragments = []string{
	"DTSTART;VALUE=DATE:20210615",
	"DTSTART:20210615T133000\r\n",
	"DTSTART:20210615T133000Z",
	"DTSTART;TZID=America/New_York:20210314T133000",
}

func TestTimeSerializationLocationIndependent(t *testing.T) {
	cal := buildTimeShapeCalendar()
	baseline := cal.Serialize(WithNewLineWindows)
	for _, frag := range timeShapeExpectedFragments {
		if !strings.Contains(baseline, frag) {
			t.Fatalf("baseline missing %q in:\n%s", frag, baseline)
		}
	}

	original := time.Local
	t.Cleanup(func() { time.Local = original })
	for _, zone := range []string{"America/Honolulu", "Asia/Tokyo", "Pacific/Chatham"} {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Skipf("timezone database missing %q: %v", zone, err)
		}
		time.Local = loc
		got := cal.Serialize(WithNewLineWindows)
		if got != baseline {
			t.Fatalf("serialization changed under time.Local=%s\n--- baseline\n%s\n--- got\n%s", zone, baseline, got)
		}
		// Floating values must parse back to the same wall clock regardless
		// of the ambient zone, and UTC values keep an absolute instant.
		parsed, err := ParseCalendar(strings.NewReader(got))
		if err != nil {
			t.Fatalf("reparse under %s: %v", zone, err)
		}
		fl := parsed.Events()[1].GetProperty(ComponentPropertyDtStart)
		if fl.Value != "20210615T133000" {
			t.Fatalf("floating raw value drifted under %s: %q", zone, fl.Value)
		}
		uStart, err := parsed.Events()[2].GetStartAt()
		if err != nil {
			t.Fatalf("utc GetStartAt under %s: %v", zone, err)
		}
		if uStart.Location() != time.UTC || uStart.Hour() != 13 {
			t.Fatalf("utc value drifted under %s: %v", zone, uStart)
		}
	}
}

// TestTimeSerializationTZEnvIndependent re-executes the serialization in a
// process where TZ selects an unusual zone, ensuring no ambient zone leaks.
func TestTimeSerializationTZEnvIndependent(t *testing.T) {
	if os.Getenv("ICS_TZ_HELPER") == "1" {
		cal := buildTimeShapeCalendar()
		// fd 3 is the pipe handed over through ExtraFiles; using it keeps the
		// serialized document clear of the test runner's own stdout chatter.
		if f := os.NewFile(3, "ics"); f != nil {
			_, _ = f.WriteString(cal.Serialize(WithNewLineWindows))
			_ = f.Close()
		}
		return
	}
	if _, err := exec.LookPath(os.Args[0]); err != nil {
		t.Skipf("test binary unavailable for re-exec: %v", err)
	}
	baseline := buildTimeShapeCalendar().Serialize(WithNewLineWindows)
	for _, tz := range []string{"America/Honolulu", "Asia/Tokyo", "Pacific/Chatham"} {
		cmd := exec.Command(os.Args[0], "-test.run", "^TestTimeSerializationTZEnvIndependent$")
		cmd.Env = append(os.Environ(), "ICS_TZ_HELPER=1", "TZ="+tz)
		// The child inherits the write end as fd 3 (ExtraFiles slot 0).
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("pipe: %v", err)
		}
		cmd.ExtraFiles = []*os.File{w}
		var doc bytes.Buffer
		done := make(chan struct{})
		go func() {
			_, _ = doc.ReadFrom(r)
			close(done)
		}()
		if err := cmd.Run(); err != nil {
			t.Fatalf("helper under TZ=%s failed: %v", tz, err)
		}
		_ = w.Close()
		<-done
		_ = r.Close()
		if doc.String() != baseline {
			t.Fatalf("serialization under TZ=%s differs\nwant:\n%s\ngot:\n%s", tz, baseline, doc.String())
		}
	}
}

// TestUnparseableTZIDWriteBack ensures a TZID that cannot be resolved still
// round-trips verbatim (no network lookup, no silent rewrite, no parse error).
func TestUnparseableTZIDWriteBack(t *testing.T) {
	const doc = "BEGIN:VCALENDAR\r\n" +
		"VERSION:2.0\r\n" +
		"PRODID:-//test//\r\n" +
		"BEGIN:VTIMEZONE\r\n" +
		"TZID:No/Such_Generated_Zone\r\n" +
		"END:VTIMEZONE\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:tz@example.com\r\n" +
		"DTSTART;TZID=No/Such_Generated_Zone:20210314T133000\r\n" +
		"END:VEVENT\r\n" +
		"END:VCALENDAR\r\n"

	cal, err := ParseCalendar(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("parse must not resolve TZID: %v", err)
	}
	out := cal.Serialize(WithNewLineWindows)
	if !strings.Contains(out, "TZID:No/Such_Generated_Zone") ||
		!strings.Contains(out, "DTSTART;TZID=No/Such_Generated_Zone:20210314T133000") {
		t.Fatalf("unknown TZID was rewritten or dropped:\n%s", out)
	}
	parsed, err := ParseCalendar(strings.NewReader(out))
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	start, err := parsed.Events()[0].GetStartAt()
	if err == nil {
		t.Fatalf("expected unresolvable TZID to surface at time read, got %v", start)
	}
	if !strings.Contains(err.Error(), "No/Such_Generated_Zone") {
		t.Fatalf("error should name the offending TZID, got %v", err)
	}
}

// TestSerializeTZIDMapperBack verifies the optional serialization mapper
// rewrites every TZID occurrence (parameter and TZID property) consistently.
func TestSerializeTZIDMapperBack(t *testing.T) {
	cal := NewCalendar()
	cal.AddTimezone("Pacific/Auckland")
	ev := NewEvent("mapper@example.com")
	ev.SetProperty(ComponentPropertyDtStart, "20260407T090000", WithTZID("Pacific/Auckland"))
	cal.addComponent(ev)

	out := cal.Serialize(WithNewLineWindows, WithWindowsTimezoneMappingForSerialization())
	if strings.Count(out, "New Zealand Standard Time") < 2 {
		t.Fatalf("expected both TZID occurrences rewritten:\n%s", out)
	}
	if strings.Contains(out, "Pacific/Auckland") {
		t.Fatalf("IANA zone leaked despite mapper:\n%s", out)
	}
	// Without the mapper the IANA name survives untouched.
	plain := cal.Serialize(WithNewLineWindows)
	if !strings.Contains(plain, "TZID:Pacific/Auckland") ||
		!strings.Contains(plain, "DTSTART;TZID=Pacific/Auckland:") {
		t.Fatalf("IANA name must survive without mapper:\n%s", plain)
	}
}
