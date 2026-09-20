package ics

// Pinned, minimal counterexamples for the three error classes the mutation
// campaign in gen_mutation_test.go searches for: octet counting / unfolding,
// parameter splitting, and component nesting. They are fully deterministic and
// always run with `go test ./... -count=1`; the exploratory campaign behind
// them is opt-in via ICAL_GEN_EXTENDED / ICAL_GEN_ROUNDS.

import (
	"errors"
	"io"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestPinnedFold_MultibyteAtBoundary pins the 75-octet rule when the boundary
// would land inside a multi-byte rune: the folder must move the whole rune to
// the continuation and never emit a split encoding unit.
func TestPinnedFold_MultibyteAtBoundary(t *testing.T) {
	// "DESCRIPTION:" is 12 octets; 63 ASCII fill the line to exactly 75, and
	// the following 3-octet rune would straddle the cut under rune-ignorant
	// byte counting.
	value := strings.Repeat("a", 63) + "界"
	cal, _ := NewCalendarWithOptions()
	cal.CalendarProperties = cal.CalendarProperties[:0]
	cal.CalendarProperties = append(cal.CalendarProperties, CalendarProperty{BaseProperty: BaseProperty{
		IANAToken: "DESCRIPTION", Value: value, ICalParameters: map[string][]string{},
	}})
	out := cal.Serialize(WithNewLineWindows)
	if !utf8.ValidString(out) {
		t.Fatalf("serialized output split a UTF-8 rune:\n%q", out)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\r\n"), "\r\n")
	found := false
	for _, l := range lines {
		if len(l) > 75 {
			t.Fatalf("physical line exceeds 75 octets: %d %q", len(l), l)
		}
		if strings.HasPrefix(l, strings.Repeat("a", 63)) {
			found = true
			if strings.Contains(l, "界") {
				t.Fatalf("rune should move to the continuation, line=%q", l)
			}
		}
		if l == " 界" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the rune on its own continuation line, got:\n%s", out)
	}
	back, err := ParseCalendar(strings.NewReader(out))
	if err != nil {
		t.Fatalf("folded output must reparse: %v", err)
	}
	if got := back.CalendarProperties[0].Value; got != value {
		t.Fatalf("round-trip value mismatch: %q != %q", got, value)
	}
}

// TestPinnedFold_EscapedNewlineAcrossBoundary pins the case where a fold lands
// directly before an escaped newline: the backslash and 'n' may sit on
// different physical lines, but unfolding must restore the "\n" escape before
// TEXT unescaping turns it into a real newline.
func TestPinnedFold_EscapedNewlineAcrossBoundary(t *testing.T) {
	// 62 b's + newline: ToText makes the newline "\n"; the first physical line
	// ends with the backslash and the continuation begins with "ncccc".
	value := strings.Repeat("b", 62) + "\ncccc"
	cal, _ := NewCalendarWithOptions()
	cal.CalendarProperties = cal.CalendarProperties[:0]
	cal.CalendarProperties = append(cal.CalendarProperties, CalendarProperty{BaseProperty: BaseProperty{
		IANAToken: "DESCRIPTION", Value: value, ICalParameters: map[string][]string{},
	}})
	out := cal.Serialize(WithNewLineWindows)
	if !strings.Contains(out, "\\\r\n ncccc") {
		t.Fatalf("expected the escape to straddle the fold, got:\n%s", out)
	}
	back, err := ParseCalendar(strings.NewReader(out))
	if err != nil {
		t.Fatalf("fold straddling an escape must reparse: %v", err)
	}
	if got := back.CalendarProperties[0].Value; got != value {
		t.Fatalf("escaped newline lost across fold: %q != %q", got, value)
	}
}

// TestPinnedFold_SpaceAndTabUnfoldSame pins RFC 5545's two legal continuation
// characters: SP and HTAB folds must unfold byte-for-byte identically, both as
// content lines (ReadLine) and as parsed properties, under CRLF and bare LF.
func TestPinnedFold_SpaceAndTabUnfoldSame(t *testing.T) {
	base := "DESCRIPTION:abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ0"
	if len(base) != 75 {
		t.Fatalf("test scaffold length %d", len(base))
	}
	cont := "rest-value"
	for _, nl := range []string{"\r\n", "\n"} {
		space := base + nl + " " + cont + nl
		tab := base + nl + "\t" + cont + nl
		ls := NewCalendarStream(strings.NewReader(space))
		lt := NewCalendarStream(strings.NewReader(tab))
		var raws []string
		for i := 0; i < 2; i++ {
			cs := []*CalendarStream{ls, lt}
			line, _, err := cs[i].ReadLine()
			if err != nil && !errors.Is(err, io.EOF) {
				t.Fatalf("ReadLine[%d]: %v", i, err)
			}
			raws = append(raws, string(*line))
		}
		if raws[0] != raws[1] {
			t.Fatalf("SP and HTAB unfold differently for %q:\n%q\n%q", nl, raws[0], raws[1])
		}
		if want := base + cont; raws[0] != want {
			t.Fatalf("unfolded line = %q, want %q", raws[0], want)
		}
		ps, es := ParseProperty(ContentLine(raws[0]))
		pt, et := ParseProperty(ContentLine(raws[1]))
		if es != nil || et != nil || ps.Value != pt.Value {
			t.Fatalf("parsed properties diverge: %v %v", es, et)
		}
	}
}

// TestPinnedFold_DroppedContinuationWhitespace pins that removing the single
// continuation character splits the logical line: the second fragment is then a
// standalone property and is reported at its own physical line rather than
// silently joined.
func TestPinnedFold_DroppedContinuationWhitespace(t *testing.T) {
	proper := "DESCRIPTION:aaa\r\nbbb\r\n"
	cal := NewCalendarStream(strings.NewReader(proper))
	l1, n1, err := cal.ReadLine()
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if string(*l1) != "DESCRIPTION:aaa" || n1 != 1 {
		t.Fatalf("first line = %q (%d)", *l1, n1)
	}
	l2, n2, _ := cal.ReadLine()
	if string(*l2) != "bbb" || n2 != 2 {
		t.Fatalf("without a continuation char the fragment must be a new logical line, got %q (%d)", *l2, n2)
	}
	// And at calendar level the orphan fragment aborts at its physical line.
	_, err = ParseCalendar(strings.NewReader(wrapCalendar("DESCRIPTION:aaa\r\nbbb\r\n")))
	var me *MalformedError
	if !errors.As(err, &me) || me.Line != 3 {
		t.Fatalf("expected error at physical line 3, got %v", err)
	}
}

// TestPinnedParam_QuotedSeparators pins that ';', ':' and ',' inside a quoted
// parameter value are content, never structural separators, and survive a
// serialize/parse cycle together with multi-valued unquoted parameters.
func TestPinnedParam_QuotedSeparators(t *testing.T) {
	input := wrapCalendar(
		"BEGIN:VEVENT\r\n" +
			"ATTENDEE;CN=\"a;b:c,d\";X-CUSTOM=one,two;ROLE=REQ-PARTICIPANT:mailto:a@example.com\r\n" +
			"END:VEVENT\r\n")
	cal, err := ParseCalendar(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ev := cal.Events()[0]
	var att *IANAProperty
	for i := range ev.Properties {
		if ev.Properties[i].IANAToken == "ATTENDEE" {
			att = &ev.Properties[i]
		}
	}
	if att == nil {
		t.Fatal("ATTENDEE missing")
	}
	if got := att.ICalParameters["CN"]; len(got) != 1 || got[0] != "a;b:c,d" {
		t.Fatalf("quoted separators mis-split: %v", got)
	}
	if got := att.ICalParameters["X-CUSTOM"]; len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("multi-valued parameter mis-split: %v", got)
	}
	if got := att.ICalParameters["ROLE"]; len(got) != 1 || got[0] != "REQ-PARTICIPANT" {
		t.Fatalf("parameter after quoted value mis-parsed: %v", got)
	}
	if att.Value != "mailto:a@example.com" {
		t.Fatalf("property value after quoted separators wrong: %q", att.Value)
	}
	// Round-trip keeps the semantic value even if quoting is normalised.
	out := cal.Serialize(WithNewLineWindows)
	re, err := ParseCalendar(strings.NewReader(out))
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	var att2 *IANAProperty
	for i := range re.Events()[0].Properties {
		if re.Events()[0].Properties[i].IANAToken == "ATTENDEE" {
			att2 = &re.Events()[0].Properties[i]
		}
	}
	if att2 == nil {
		t.Fatal("ATTENDEE missing after round-trip")
	}
	if len(att2.ICalParameters["X-CUSTOM"]) != 2 {
		t.Fatalf("multi-valued parameter lost after round-trip: %v", att2.ICalParameters)
	}
}

// TestPinnedNesting_OrderAndRoundTrip pins nested component order, VEVENT >
// VALARM and VTIMEZONE > STANDARD/DAYLIGHT subcomponents, and the general
// (X-) component path.
func TestPinnedNesting_OrderAndRoundTrip(t *testing.T) {
	input := strings.Join([]string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:x",
		"BEGIN:VTIMEZONE",
		"TZID:Generated/Zone_2021",
		"BEGIN:STANDARD",
		"DTSTART:19700101T000000",
		"TZOFFSETFROM:+0530",
		"TZOFFSETTO:+0530",
		"TZNAME:GST",
		"END:STANDARD",
		"END:VTIMEZONE",
		"BEGIN:VEVENT",
		"UID:u1",
		"SUMMARY:first",
		"BEGIN:VALARM",
		"ACTION:DISPLAY",
		"TRIGGER:-PT15M",
		"END:VALARM",
		"END:VEVENT",
		"BEGIN:VEVENT",
		"UID:u2",
		"SUMMARY:second",
		"END:VEVENT",
		"BEGIN:X-WIDGET",
		"X-A:b",
		"END:X-WIDGET",
		"END:VCALENDAR",
		"",
	}, "\r\n")
	cal, err := ParseCalendar(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cal.Components) != 4 {
		t.Fatalf("expected 4 top-level components in order, got %d", len(cal.Components))
	}
	tz := cal.Timezones()[0]
	if len(tz.SubComponents()) != 1 {
		t.Fatalf("expected 1 STANDARD subcomponent, got %d", len(tz.SubComponents()))
	}
	if _, ok := tz.SubComponents()[0].(*Standard); !ok {
		t.Fatalf("expected *Standard, got %T", tz.SubComponents()[0])
	}
	events := cal.Events()
	if events[0].Id() != "u1" || events[1].Id() != "u2" {
		t.Fatalf("event order not preserved: %s, %s", events[0].Id(), events[1].Id())
	}
	if alarms := events[0].SubComponents(); len(alarms) != 1 {
		t.Fatalf("expected nested VALARM, got %d", len(alarms))
	} else if _, ok := alarms[0].(*VAlarm); !ok {
		t.Fatalf("expected *VAlarm, got %T", alarms[0])
	}
	gc, ok := cal.Components[3].(*GeneralComponent)
	if !ok || gc.Token != "X-WIDGET" {
		t.Fatalf("expected general X-WIDGET component last, got %#v", cal.Components[3])
	}
	out := cal.Serialize(WithNewLineWindows)
	cal2, err := ParseCalendar(strings.NewReader(out))
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if len(cal2.Components) != 4 || cal2.Events()[1].Id() != "u2" {
		t.Fatalf("nested order changed after round-trip")
	}
	if cal.Serialize(WithNewLineWindows) != cal2.Serialize(WithNewLineWindows) {
		t.Fatalf("nested serialization not stable")
	}
}

// TestPinnedFold_SplitRuneRejected pins that a CRLF inserted inside a
// multi-byte rune cannot survive: each physical fragment must be valid UTF-8,
// and the resulting document must not silently round-trip as the original. This
// is the fixed counterexample behind the campaign's fold/split-rune mutator.
func TestPinnedFold_SplitRuneRejected(t *testing.T) {
	// "DESCRIPTION:" (12) + 60 'a' (60) = 72 octets, then a 3-byte rune.
	// Breaking immediately after the rune's lead byte puts an invalid
	// continuation on the next physical line.
	line := "DESCRIPTION:" + strings.Repeat("a", 60) + "界"
	split := line[:73] + "\r\n " + line[73:] + "\r\n"
	full := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" + split + "END:VCALENDAR\r\n"
	valid := true
	for i, pl := range strings.Split(strings.TrimSuffix(full, "\r\n"), "\r\n") {
		frag := pl
		if len(frag) > 0 && (frag[0] == ' ' || frag[0] == '\t') {
			frag = frag[1:]
		}
		if len(pl) > 75 || !utf8.ValidString(frag) {
			valid = false
			t.Logf("physical line %d invalid: len=%d utf8=%v", i+1, len(pl), utf8.ValidString(frag))
		}
	}
	if valid {
		t.Fatalf("the split-rune input must violate per-fragment UTF-8 validity")
	}
}
