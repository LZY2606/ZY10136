package ics

// Malformed-family tests for the syntax-aware generator work.
//
// These cases are the minimal (single-property or smallest nesting)
// counterexamples for the parse failures the mutators in gen_mutation_test.go
// search for. Every assertion checks either:
//
//   - the concrete wrapped sentinel error *and* the reported physical line
//     number in MalformedError, or
//   - the exact raw (already unfolded) content line handed to a replacement
//     property parser (the "malformed callback"),
//
// never just "error is non-nil". The public strict parser's lenient behaviour
// (a stray leading continuation at the start of the stream) is pinned as-is
// rather than tightened.

import (
	"errors"
	"strings"
	"testing"
)

// recordingParser wraps the strict parser. Every raw content line it observes
// is recorded; when strict parsing fails it delegates to onErr.
type recordingParser struct {
	seen   []ContentLine
	strict PropertyParser
	onErr  func(raw ContentLine, err error) (*BaseProperty, error)
}

func (r *recordingParser) parse(raw ContentLine) (*BaseProperty, error) {
	r.seen = append(r.seen, raw)
	p, err := r.strict(raw)
	if err != nil && r.onErr != nil {
		return r.onErr(raw, err)
	}
	return p, err
}

func expectMalformed(t *testing.T, input string, wantSentinel error, wantLine int) {
	t.Helper()
	_, err := ParseCalendar(strings.NewReader(input))
	if err == nil {
		t.Fatalf("expected error containing %v, got nil\ninput:\n%s", wantSentinel, input)
	}
	if !errors.Is(err, wantSentinel) {
		t.Fatalf("expected sentinel %v, got %v", wantSentinel, err)
	}
	var me *MalformedError
	if !errors.As(err, &me) {
		t.Fatalf("expected *MalformedError, got %T: %v", err, err)
	}
	if me.Line != wantLine {
		t.Fatalf("expected error on physical line %d, got line %d (%v)", wantLine, me.Line, err)
	}
}

func expectMalformedContains(t *testing.T, input string, wantLine int, fragments ...string) {
	t.Helper()
	_, err := ParseCalendar(strings.NewReader(input))
	if err == nil {
		t.Fatalf("expected an error, got nil\ninput:\n%s", input)
	}
	var me *MalformedError
	if !errors.As(err, &me) || me.Line != wantLine {
		t.Fatalf("expected MalformedError on physical line %d, got %v", wantLine, err)
	}
	for _, f := range fragments {
		if !strings.Contains(err.Error(), f) {
			t.Fatalf("error %v should contain %q", err, f)
		}
	}
}

func wrapCalendar(body string) string {
	return "BEGIN:VCALENDAR\r\n" + body + "END:VCALENDAR\r\n"
}

func TestMalformed_MissingColon(t *testing.T) {
	// '-' after the token is not ';' or ':' -> strict property parse error.
	input := wrapCalendar("VERSION-2.0\r\n")
	expectMalformedContains(t, input, 2, "unexpected character", "VERSION-2")
}

func TestMalformed_BadQuote(t *testing.T) {
	// A DQUOTE in the middle of an unquoted param value.
	input := wrapCalendar("ATTENDEE;RSVP=T\"RUE:mailto:a@example.com\r\n")
	expectMalformed(t, input, ErrUnexpectedDoubleQuoteInPropertyParamValue, 2)
}

func TestMalformed_UnterminatedQuote(t *testing.T) {
	// The quoted string swallows the colon and the line ends before it closes.
	input := wrapCalendar("X-FOO;BAR=\"abc:val\r\n")
	expectMalformed(t, input, ErrUnexpectedEndOfProperty, 2)
}

func TestMalformed_BareSemicolonAtEnd(t *testing.T) {
	// Parameter name followed by end-of-line instead of '='.
	input := wrapCalendar("X-FOO;BAR\r\n")
	expectMalformed(t, input, ErrMissingPropertyParamOperator, 2)
}

func TestMalformed_WrongEndToken(t *testing.T) {
	// A component closed with the wrong token.
	input := wrapCalendar("BEGIN:VEVENT\r\nUID:x\r\nEND:VTODO\r\n")
	expectMalformed(t, input, ErrUnbalancedEnd, 4)
}

func TestMalformed_UnclosedComponent(t *testing.T) {
	// VEVENT opened; the VCALENDAR END line is what surfaces the mismatch.
	input := wrapCalendar("BEGIN:VEVENT\r\nUID:x\r\n")
	expectMalformed(t, input, ErrUnbalancedEnd, 4)
}

func TestMalformed_StrayEnd(t *testing.T) {
	input := wrapCalendar("END:VEVENT\r\n")
	expectMalformed(t, input, ErrExpectedEnd, 2)
}

func TestMalformed_ContentAfterEnd(t *testing.T) {
	input := wrapCalendar("") + "X-FOO:bar\r\n"
	expectMalformed(t, input, ErrUnexpectedCalendarEnd, 3)
}

func TestMalformed_NestedVCalendar(t *testing.T) {
	// A second BEGIN:VCALENDAR inside the calendar is rejected directly.
	input := wrapCalendar("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")
	_, err := ParseCalendar(strings.NewReader(input))
	if !errors.Is(err, ErrVCalendarNotWhereExpected) {
		t.Fatalf("expected ErrVCalendarNotWhereExpected, got %v", err)
	}
}

func TestMalformed_NestingTooDeep(t *testing.T) {
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\n")
	for i := 0; i < maxComponentNestingDepth+10; i++ {
		b.WriteString("BEGIN:VEVENT\r\n")
	}
	_, err := ParseCalendar(strings.NewReader(b.String()))
	if !errors.Is(err, ErrComponentNestingTooDeep) {
		t.Fatalf("expected ErrComponentNestingTooDeep, got %v", err)
	}
	var me *MalformedError
	if !errors.As(err, &me) || !me.HasLine || me.Line <= 0 {
		t.Fatalf("expected line-annotated MalformedError, got %v", err)
	}
}

func TestMalformed_TZIDUnresolvable(t *testing.T) {
	// Calendar parsing never resolves TZID; only the typed getter does, and
	// the raw value + unknown TZID parameter must be preserved on the wire.
	input := wrapCalendar("BEGIN:VEVENT\r\nUID:u\r\nDTSTART;TZID=Nowhere/Land:20210627T120000\r\nEND:VEVENT\r\n")
	cal, err := ParseCalendar(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse should tolerate unknown TZID, got %v", err)
	}
	if _, gerr := cal.Events()[0].GetStartAt(); gerr == nil {
		t.Fatalf("expected GetStartAt to fail for unresolvable TZID")
	} else if !strings.Contains(gerr.Error(), "Nowhere/Land") {
		t.Fatalf("error should name the TZID, got %v", gerr)
	}
	out := cal.Serialize(WithNewLineWindows)
	if !strings.Contains(out, "DTSTART;TZID=Nowhere/Land:20210627T120000") {
		t.Fatalf("unknown TZID line must be written back verbatim:\n%s", out)
	}
}

func TestMalformed_ContinuationAtStreamStart(t *testing.T) {
	// Backward-compatible leniency: a lone SP continuation before the first
	// real line is consumed by the stream layer and the document still parses.
	// Pinned so a change in the tolerant strategy is deliberate.
	input := " BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n"
	cal, err := ParseCalendar(strings.NewReader(input))
	if err != nil {
		t.Fatalf("leading continuation should remain tolerated, got %v", err)
	}
	if len(cal.CalendarProperties) == 0 || cal.CalendarProperties[0].IANAToken != "VERSION" {
		t.Fatalf("calendar body after a leading continuation must still parse")
	}
}

func TestMalformed_OrphanContinuationCallbackRaw(t *testing.T) {
	// A continuation line is unfolded onto the previous logical line. When
	// that produces a malformed content line (a DQUOTE lands in the middle of
	// a parameter value), a replacement property parser receives the exact
	// unfolded raw content, and the strict parser reports the physical line
	// of the continuation.
	input := wrapCalendar("X-FOO;RSVP=T\r\n \"RUE:v\r\n")
	var gotRaw ContentLine
	rec := &recordingParser{strict: parseProperty}
	rec.onErr = func(raw ContentLine, err error) (*BaseProperty, error) {
		gotRaw = raw
		return nil, ErrPropertySkipped
	}
	cal, err := ParseCalendarWithOptions(strings.NewReader(input), PropertyParser(rec.parse))
	if err != nil {
		t.Fatalf("skip parser should swallow the malformed line, got %v", err)
	}
	if want := ContentLine("X-FOO;RSVP=T\"RUE:v"); gotRaw != want {
		t.Fatalf("callback raw content = %q, want %q", gotRaw, want)
	}
	if len(cal.CalendarProperties) != 0 {
		t.Fatalf("merged line must have been skipped, got %d calendar properties", len(cal.CalendarProperties))
	}

	// Strict parser reports the error against the logical line's first
	// physical line (the continuation keeps the starting line number).
	expectMalformed(t, input, ErrUnexpectedDoubleQuoteInPropertyParamValue, 2)
}

func TestMalformed_CallbackReceivesRawAndLine(t *testing.T) {
	// Missing-colon line: the callback receives the exact raw line and, when
	// it chooses to abort, the strict parse error surfaces with line context.
	input := wrapCalendar("VERSION-2.0\r\n")
	rec := &recordingParser{strict: parseProperty}
	rec.onErr = func(raw ContentLine, err error) (*BaseProperty, error) {
		if raw != "VERSION-2.0" {
			t.Errorf("raw line = %q, want VERSION-2.0", raw)
		}
		return nil, err
	}
	_, err := ParseCalendarWithOptions(strings.NewReader(input), PropertyParser(rec.parse))
	if err == nil || !strings.Contains(err.Error(), "unexpected character") {
		t.Fatalf("callback-abort should surface the unexpected-character error, got %v", err)
	}
	var me *MalformedError
	if !errors.As(err, &me) || me.Line != 2 {
		t.Fatalf("want MalformedError at line 2, got %v", err)
	}
	found := false
	for _, raw := range rec.seen {
		if raw == "VERSION-2.0" {
			found = true
		}
	}
	if !found {
		t.Fatalf("callback never observed raw line %q (saw %v)", "VERSION-2.0", rec.seen)
	}
}
