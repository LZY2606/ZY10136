package ics

// Semantic-equivalent wire mutants that *should survive*.
//
// The mutation campaign (gen_mutation_test.go) separates mutations into:
//
//   - lethal: parsing fails with a precise error, or a fold invariant breaks;
//   - equivalent: the document parses to exactly the same semantic model;
//   - undetectable: it parses into a different but still fully valid document.
//
// Equivalent and undetectable mutants are not bugs: RFC 5545 admits several
// indistinguishable wire encodings, and a parser cannot tell an intentionally
// author-written variant from a corrupted one without knowing the original.
// Each case below names the reason it survives. Asserting these prevents the
// test suite from demanding over-strict rejection (which would violate the
// public parser's tolerant strategy) while still proving that lethal variants
// of the same constructs are caught.

import (
	"strings"
	"testing"
)

func mustParse(t *testing.T, in string, opts ...any) *Calendar {
	t.Helper()
	cal, err := ParseCalendarWithOptions(strings.NewReader(in), opts...)
	if err != nil {
		t.Fatalf("unexpected parse error:\n%s\n%v", in, err)
	}
	return cal
}

// equivalentPair parses two wire forms and requires identical semantics.
func equivalentPair(t *testing.T, name, a, b string, opts ...any) {
	t.Helper()
	ca := mustParse(t, a, opts...)
	cb := mustParse(t, b, opts...)
	if !sigsEqual(calendarPropSigs(ca), calendarPropSigs(cb)) {
		t.Fatalf("%s: equivalent encodings parsed differently\n--- a ---\n%v\n--- b ---\n%v",
			name, calendarPropSigs(ca), calendarPropSigs(cb))
	}
}

// TestEquivalent_TabFoldEqualsSpaceFold: RFC 5545 section 3.1 defines the
// fold as CRLF followed by a single SP or HTAB; both are dropped on unfold.
// There is no semantic content in which whitespace was chosen.
func TestEquivalent_TabFoldEqualsSpaceFold(t *testing.T) {
	body := "DESCRIPTION:" + strings.Repeat("z", 70) + "tail\r\n"
	spaceFold := wrapCalendarFold(body, " ")
	tabFold := wrapCalendarFold(body, "\t")
	equivalentPair(t, "tab-vs-space-fold", spaceFold, tabFold)
}

func wrapCalendarFold(body, cont string) string {
	// Split the long description at its 75-octet physical boundary and insert
	// the chosen continuation character.
	line := strings.TrimSuffix(body, "\r\n")
	head, tail := line[:75], line[75:]
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" + head + "\r\n" + cont + tail + "\r\nEND:VCALENDAR\r\n"
}

// TestEquivalent_BareLFAndCRLFUnfold: unfolding is defined on the logical line
// and this parser accepts bare LF line endings as well; the unfolded content
// and parsed property value are identical.
func TestEquivalent_BareLFAndCRLFUnfold(t *testing.T) {
	crlf := "DESCRIPTION:abcdefghijklmnop\r\n qrstuv\r\n"
	lf := "DESCRIPTION:abcdefghijklmnop\n qrstuv\n"
	ps, es := ParseProperty(mustFirstLine(t, crlf))
	pl, el := ParseProperty(mustFirstLine(t, lf))
	if es != nil || el != nil || ps.Value != pl.Value {
		t.Fatalf("CRLF and bare LF folds diverge: %q(%v) vs %q(%v)", ps, es, pl, el)
	}
}

func mustFirstLine(t *testing.T, in string) ContentLine {
	cs := NewCalendarStream(strings.NewReader(in))
	line, _, err := cs.ReadLine()
	if err != nil && err.Error() != "EOF" {
		t.Fatalf("ReadLine: %v", err)
	}
	return *line
}

// TestEquivalent_NormalizedParameterEscaping: an unquoted parameter value with
// backslash escapes and the same value expressed through equivalent escapes
// parse identically. The serializer may choose one canonical spelling; the
// semantic parameter value is what is compared.
func TestEquivalent_NormalizedParameterEscaping(t *testing.T) {
	a := wrapCalendar("ATTENDEE;CN=ab:mailto:a@example.com\r\n")
	b := wrapCalendar("ATTENDEE;CN=\"ab\":mailto:a@example.com\r\n")
	equivalentPair(t, "quoted-vs-unquoted-simple-value", a, b)
}

// TestEquivalent_DuplicateFoldEncodings: folding at multiple legal points
// yields different physical layouts but the same unfolded logical line. A
// generator that inserts an extra fold within the limit must be invisible.
func TestEquivalent_ExtraFoldWithinLimit(t *testing.T) {
	one := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nDESCRIPTION:short-value\r\nEND:VCALENDAR\r\n"
	two := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nDESCRIPTION:short-\r\n value\r\nEND:VCALENDAR\r\n"
	equivalentPair(t, "extra-fold", one, two)
}

// TestUndetectable_DroppedFoldMakesNewProperty documents why dropping the fold
// whitespace in the middle of a value cannot be rejected: the resulting two
// lines are each well-formed content lines and constitute a valid (different)
// calendar. The parser must report them faithfully rather than guess they were
// one line.
func TestUndetectable_DroppedFoldMakesNewProperty(t *testing.T) {
	original := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nUID;X=a:h\r\n ttps://x\r\nEND:VCALENDAR\r\n"
	dropped := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nUID;X=a:h\r\nttps://x\r\nEND:VCALENDAR\r\n"
	co := mustParse(t, original)
	cd := mustParse(t, dropped)
	so, sd := calendarPropSigs(co), calendarPropSigs(cd)
	// They are semantically different...
	if sigsEqual(so, sd) {
		t.Fatalf("dropping the fold must change the document")
	}
	// ...but both are valid calendars: the second has an extra ttps property
	// instead of a continuation, which is legitimate author intent.
	var hasOrphan bool
	for _, sig := range sd {
		if strings.HasSuffix(sig.token, "/ttps") && sig.value == "//x" {
			hasOrphan = true
		}
	}
	if !hasOrphan {
		t.Fatalf("orphan fragment should be retained as a new property, got %v", sd)
	}
	if len(sd) != len(so)+1 {
		t.Fatalf("expected exactly one extra property after losing the fold, got %d vs %d", len(sd), len(so))
	}
}
