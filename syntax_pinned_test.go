package ics

// Frozen counterexamples and pinned malformed inputs.
//
// These minimal documents are the concrete findings of the generation and
// mutation campaigns (syntax_gen_test.go, syntax_mutation_test.go). They are
// kept independent of the random generators so a regression in byte counting,
// unfolding, parameter splitting, or component nesting fails deterministically
// under the default short test set.
//
// Findings fixed here:
//
//   - Folding: the 75-octet budget lands on a multibyte boundary (界 rune),
//     fold markers may be SP or HTAB, terminators may be CRLF or LF (even
//     mixed within one logical line), the fold may sit directly after an
//     escaped newline or split the \N escape itself, and a continuation
//     line whose own content starts with SP carries two leading SP octets.
//   - Parameter splitting: a quoted ALTREP value keeps ; : , as data while a
//     following bare CN keeps its comma-separated multi-values; quoted and
//     bare simple values plus parameter reordering parse to one model.
//   - Nesting: STANDARD must precede DAYLIGHT inside VTIMEZONE, repeated
//     CATEGORIES keep multiplicity, and unknown X- properties survive at
//     every component level (they must not be normalised away).
//   - Malformed family: orphan continuations, missing colon/operator, bad
//     quotes, duplicate or mismatched BEGIN/END, nested VCALENDAR, and
//     unresolvable TZID each fail with an identifiable line/error identity;
//     the recovery callback receives the exact unfolded raw content.
//
// Why the equivalent mutations in TestEquivalentMutantsSurvive survive:
// terminator choice (LF vs CRLF), fold marker (SP vs HTAB), fold location,
// quoting of a simple parameter value, and parameter ordering are syntax the
// RFC declares interchangeable, so a correct implementation must map all of
// them to the identical semantic model. A mutation-testing run cannot kill a
// check that accepts them, which is the intended guarantee of the parser's
// tolerant policy.

import (
	"errors"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Folding corpus: every physical line must stay within 75 UTF-8 octets,
// runes must stay intact, and unfold must reproduce the logical content line.
// ---------------------------------------------------------------------------

// foldAtByte splits a logical content line into physical pieces at rune
// edges so the first piece fits in maxFirst octets and continuations in
// maxCont octets (75 and 74 per RFC 5545). It never splits a UTF-8 rune.
func foldAtByte(line string, maxFirst, maxCont int) []string {
	var pieces []string
	rest := line
	max := maxFirst
	for len(rest) > max {
		cut := max
		for cut > 0 && (rest[cut]&0xC0) == 0x80 {
			cut--
		}
		pieces = append(pieces, rest[:cut])
		rest = rest[cut:]
		max = maxCont
	}
	pieces = append(pieces, rest)
	return pieces
}

func renderFolded(pieces []string, term, mark string) string {
	var b strings.Builder
	for i, piece := range pieces {
		if i > 0 {
			b.WriteString(mark)
		}
		b.WriteString(piece)
		b.WriteString(term)
	}
	return b.String()
}

func TestPinnedFoldingCorpus(t *testing.T) {
	const (
		crlf = "\r\n"
		lf   = "\n"
	)
	mk := func(logical string, term, mark string) string {
		pieces := foldAtByte(logical, 75, 74)
		return "BEGIN:VCALENDAR" + term + renderFolded(pieces, term, mark) + "END:VCALENDAR" + term
	}
	cases := []struct {
		name    string
		logical string
		doc     string
	}{
		{
			name:    "fold_boundary_inside_multibyte_75th_octet",
			logical: "X-FOLD:" + strings.Repeat("界", 30),
			doc:     mk("X-FOLD:"+strings.Repeat("界", 30), crlf, " "),
		},
		{
			name:    "fold_uses_tab_continuation",
			logical: "X-FOLD:" + strings.Repeat("a", 140),
			doc:     mk("X-FOLD:"+strings.Repeat("a", 140), crlf, "\t"),
		},
		{
			name: "fold_immediately_after_escaped_newline",
			// Fold marker lands directly after the \N escape sequence.
			logical: "DESCRIPTION:" + strings.Repeat("a", 61) + `\n` + strings.Repeat("b", 60),
			doc: func() string {
				head := "DESCRIPTION:" + strings.Repeat("a", 61) + `\n`
				tail := strings.Repeat("b", 60)
				return "BEGIN:VCALENDAR" + crlf + head + crlf + " " + tail + crlf + "END:VCALENDAR" + crlf
			}(),
		},
		{
			name: "fold_between_backslash_and_n",
			// The fold splits the two characters of the \N escape itself;
			// unfold joins bytes before TEXT escapes are decoded.
			logical: "DESCRIPTION:" + strings.Repeat("a", 62) + `\n` + strings.Repeat("b", 59),
			doc: func() string {
				head := "DESCRIPTION:" + strings.Repeat("a", 62) + `\`
				tail := "n" + strings.Repeat("b", 59)
				return "BEGIN:VCALENDAR" + crlf + head + crlf + " " + tail + crlf + "END:VCALENDAR" + crlf
			}(),
		},
		{
			name:    "fold_inside_quoted_parameter",
			logical: `X-FOLD;ALTREP="http://example.com/` + strings.Repeat("q", 80) + `":v`,
			doc:     mk(`X-FOLD;ALTREP="http://example.com/`+strings.Repeat("q", 80)+`":v`, crlf, " "),
		},
		{
			name:    "fold_inside_multibyte_quoted_parameter",
			logical: `X-FOLD;ALTREP="` + strings.Repeat("界", 40) + `":v`,
			doc:     mk(`X-FOLD;ALTREP="`+strings.Repeat("界", 40)+`":v`, crlf, " "),
		},
		{
			name:    "lf_terminated_folds",
			logical: "X-FOLD:" + strings.Repeat("a", 140),
			doc:     mk("X-FOLD:"+strings.Repeat("a", 140), lf, " "),
		},
		{
			name: "continuation_content_starts_with_space",
			// A run of spaces straddles the fold: the continuation physical
			// line begins with two SP octets, one fold marker plus one
			// content space, and both octets must survive unfold.
			logical: "X-FOLD:" + strings.Repeat("a", 68) + strings.Repeat(" ", 4) + "tail",
			doc: func() string {
				first := "X-FOLD:" + strings.Repeat("a", 68) // 75 octets
				cont := "  " + strings.Repeat(" ", 3) + "tail"
				return "BEGIN:VCALENDAR" + crlf + first + crlf + cont + crlf + "END:VCALENDAR" + crlf
			}(),
		},
		{
			name:    "mixed_crlf_and_lf_folds_with_tab",
			logical: "X-FOLD:" + strings.Repeat("a", 210),
			doc: func() string {
				pieces := foldAtByte("X-FOLD:"+strings.Repeat("a", 210), 75, 74)
				if len(pieces) != 3 {
					t.Fatalf("expected 3 pieces, got %d", len(pieces))
				}
				return "BEGIN:VCALENDAR" + crlf +
					pieces[0] + lf + " " + pieces[1] + crlf + "\t" + pieces[2] + crlf +
					"END:VCALENDAR" + crlf
			}(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertFoldingStructure(t, tc.doc)
			logical := unfoldPhysical(tc.doc)
			found := false
			for _, l := range logical {
				if l == tc.logical {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("unfold did not reproduce logical line\nwant %q\ngot  %q", tc.logical, logical)
			}
			cal, err := ParseCalendar(strings.NewReader(tc.doc))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			out := cal.Serialize(WithNewLineWindows)
			assertFoldingStructure(t, out)
			parsed2, err := ParseCalendar(strings.NewReader(out))
			if err != nil {
				t.Fatalf("reparse: %v", err)
			}
			if out != parsed2.Serialize(WithNewLineWindows) {
				t.Fatalf("serialization not idempotent\nfirst:\n%s\nsecond:\n%s", out, parsed2.Serialize(WithNewLineWindows))
			}
		})
	}
}

// TestPinnedFoldingEscapedNewlineSemantics checks the specific combination
// "折叠后紧接转义换行": a fold point directly before \N must decode to a newline
// in the TEXT value, regardless of SP/HTAB marker.
func TestPinnedFoldingEscapedNewlineSemantics(t *testing.T) {
	for _, marker := range []string{" ", "\t"} {
		doc := "BEGIN:VCALENDAR\r\nDESCRIPTION:line\r\n" + marker + `\Nend` + "\r\nEND:VCALENDAR\r\n"
		cal, err := ParseCalendar(strings.NewReader(doc))
		if err != nil {
			t.Fatalf("marker %q: %v", marker, err)
		}
		prop := cal.CalendarProperties[0]
		if prop.Value != "line\nend" {
			t.Fatalf("marker %q: expected decoded newline, got %q", marker, prop.Value)
		}
	}
}

// ---------------------------------------------------------------------------
// Malformed family: precise location and error identity, not just non-nil.
// ---------------------------------------------------------------------------

type malformedCase struct {
	name         string
	doc          string
	wantErr      error
	wantLine     int
	wantContains string // optional substring in message
}

func TestPinnedMalformedFamily(t *testing.T) {
	cases := []malformedCase{
		{
			name:     "orphan_continuation_first_line",
			doc:      " SUMMARY:orphan\r\nEND:VCALENDAR\r\n",
			wantErr:  ErrExpectedBegin,
			wantLine: 1,
		},
		{
			name:     "orphan_continuation_after_begin",
			doc:      "BEGIN:VCALENDAR\r\n X-FOO:v\r\nEND:VCALENDAR\r\n",
			wantErr:  ErrExpectedVCalendar,
			wantLine: 1,
		},
		{
			name: "missing_colon_property",
			doc:  "BEGIN:VCALENDAR\r\nX-FOO\r\nEND:VCALENDAR\r\n",
			// The strict parser reports this path as a literal message rather
			// than a wrapped sentinel; pin the text and location explicitly.
			wantContains: "unexpected end of property X-FOO",
			wantLine:     2,
		},
		{
			name:     "missing_param_operator",
			doc:      "BEGIN:VCALENDAR\r\nX-FOO;NOEQ\r\nEND:VCALENDAR\r\n",
			wantErr:  ErrMissingPropertyParamOperator,
			wantLine: 2,
		},
		{
			name:     "bad_quote_bare_param_value",
			doc:      "BEGIN:VCALENDAR\r\nX-FOO;CN=ab\"c:v\r\nEND:VCALENDAR\r\n",
			wantErr:  ErrUnexpectedDoubleQuoteInPropertyParamValue,
			wantLine: 2,
		},
		{
			name:     "unterminated_quoted_param",
			doc:      "BEGIN:VCALENDAR\r\nX-FOO;ALTREP=\"http://x:v\r\nEND:VCALENDAR\r\n",
			wantErr:  ErrUnexpectedEndOfProperty,
			wantLine: 2,
		},
		{
			name:     "bad_param_name",
			doc:      "BEGIN:VCALENDAR\r\nX-FOO;CN=x;BAD@NAME=y:v\r\nEND:VCALENDAR\r\n",
			wantErr:  ErrMissingPropertyValue,
			wantLine: 2,
		},
		{
			name:     "duplicate_begin_missing_end",
			doc:      "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nBEGIN:VEVENT\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
			wantErr:  ErrUnbalancedEnd,
			wantLine: 5,
		},
		{
			name:     "wrong_nesting_end",
			doc:      "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nEND:VTODO\r\nEND:VCALENDAR\r\n",
			wantErr:  ErrUnbalancedEnd,
			wantLine: 3,
		},
		{
			name:     "end_without_begin",
			doc:      "BEGIN:VCALENDAR\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
			wantErr:  ErrExpectedEnd,
			wantLine: 2,
		},
		{
			name:    "nested_vcalendar",
			doc:     "BEGIN:VCALENDAR\r\nBEGIN:VCALENDAR\r\nEND:VCALENDAR\r\nEND:VCALENDAR\r\n",
			wantErr: ErrVCalendarNotWhereExpected,
		},
		{
			name: "unparseable_tzid_property",
			doc: "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:x@example.com\r\n" +
				"DTSTART;TZID=No/Such_Zone:20200101T120000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
			wantContains: "No/Such_Zone",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cal, err := ParseCalendar(strings.NewReader(tc.doc))
			if tc.wantContains != "" {
				if err == nil && tc.wantErr == nil {
					// Some failures (bad TZID) only surface when reading time.
					evs := cal.Events()
					if len(evs) == 0 {
						t.Fatalf("expected an event for %s", tc.name)
					}
					_, terr := evs[0].GetStartAt()
					if terr == nil || !strings.Contains(terr.Error(), tc.wantContains) {
						t.Fatalf("expected time error containing %q, got %v", tc.wantContains, terr)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), tc.wantContains) {
					t.Fatalf("error %v does not contain %q", err, tc.wantContains)
				}
				if tc.wantLine > 0 {
					var me *MalformedError
					if !errors.As(err, &me) || me.Line != tc.wantLine {
						t.Fatalf("line mismatch want %d in %v", tc.wantLine, err)
					}
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error %v, got nil", tc.wantErr)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error identity mismatch:\nwant %v\ngot  %v", tc.wantErr, err)
			}
			var me *MalformedError
			if tc.wantLine > 0 {
				if !errors.As(err, &me) || !me.HasLine {
					t.Fatalf("expected MalformedError with line, got %T %v", err, err)
				}
				if me.Line != tc.wantLine {
					t.Fatalf("line mismatch: want %d got %d (%v)", tc.wantLine, me.Line, err)
				}
			}
		})
	}
}

// TestMalformedCallbackReceivesRawUnfoldedContent asserts the property parser
// callback is invoked with the exact unfolded raw content line, including the
// bytes that were joined through folding.
func TestMalformedCallbackReceivesRawUnfoldedContent(t *testing.T) {
	doc := "BEGIN:VCALENDAR\r\n" +
		"X-FOO;BAD@NAME=x:v\r\n" +
		"X-BAR;BAD@NAME=y:\r\n" +
		" part-two\r\n" +
		"END:VCALENDAR\r\n"
	var raw []string
	cal, err := ParseCalendarWithOptions(strings.NewReader(doc),
		PropertyParser(func(line ContentLine) (*BaseProperty, error) {
			if _, perr := parseProperty(line); perr != nil {
				raw = append(raw, string(line))
				return nil, nil
			}
			return parseProperty(line)
		}))
	if err != nil {
		t.Fatalf("skipping parser must not abort: %v", err)
	}
	if cal == nil {
		t.Fatal("calendar nil")
	}
	want := []string{"X-FOO;BAD@NAME=x:v", "X-BAR;BAD@NAME=y:part-two"}
	if len(raw) != len(want) {
		t.Fatalf("callback invoked %d times, want %d (raw=%q)", len(raw), len(want), raw)
	}
	for i := range want {
		if raw[i] != want[i] {
			t.Fatalf("callback raw line %d = %q want %q", i, raw[i], want[i])
		}
	}
}

// TestPinnedFoldEscapesDecode checks both fold placements around an escaped
// newline decode to the same single '\n' in the TEXT value.
func TestPinnedFoldEscapesDecode(t *testing.T) {
	docs := map[string]string{
		"after_escape": "BEGIN:VCALENDAR\r\nDESCRIPTION:" + strings.Repeat("a", 61) + `\n` + "\r\n " + strings.Repeat("b", 60) + "\r\nEND:VCALENDAR\r\n",
		"split_escape": "BEGIN:VCALENDAR\r\nDESCRIPTION:" + strings.Repeat("a", 62) + "\\" + "\r\n n" + strings.Repeat("b", 59) + "\r\nEND:VCALENDAR\r\n",
	}
	want := strings.Repeat("a", 61) + "\n" + strings.Repeat("b", 60)
	for name, doc := range docs {
		if name == "split_escape" {
			want = strings.Repeat("a", 62) + "\n" + strings.Repeat("b", 59)
		}
		cal, err := ParseCalendar(strings.NewReader(doc))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := cal.CalendarProperties[0].Value
		if got != want {
			t.Fatalf("%s: decoded %q want %q", name, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Semantically equivalent mutations that must survive.
//
// Each pair below parses to the same model. They "survive" (i.e. mutation
// testing cannot kill them) because the differences are in syntax that RFC
// 5545 declares equivalent: line terminators, fold markers, fold locations,
// quoting of simple parameter values, and parameter ordering. Pinning them
// documents that the parser's tolerance is intentional rather than accidental.
// ---------------------------------------------------------------------------

func TestEquivalentMutantsSurvive(t *testing.T) {
	pairs := []struct {
		name  string
		left  string
		right string
	}{
		{
			name:  "lf_vs_crlf",
			left:  "BEGIN:VCALENDAR\nX-FOO:bar\nEND:VCALENDAR\n",
			right: "BEGIN:VCALENDAR\r\nX-FOO:bar\r\nEND:VCALENDAR\r\n",
		},
		{
			name:  "space_vs_tab_fold_marker",
			left:  "BEGIN:VCALENDAR\r\nX-FOO:" + strings.Repeat("a", 80) + "\r\nEND:VCALENDAR\r\n",
			right: "BEGIN:VCALENDAR\r\nX-FOLD-DUMMY\r\nEND:VCALENDAR\r\n", // replaced below
		},
		{
			name:  "arbitrary_fold_point",
			left:  "BEGIN:VCALENDAR\r\nX-FOO:abcdefghijklmnop\r\nEND:VCALENDAR\r\n",
			right: "BEGIN:VCALENDAR\r\nX-FOO:abc\r\n defghijklmnop\r\nEND:VCALENDAR\r\n",
		},
		{
			name:  "quoted_vs_bare_simple_param",
			left:  "BEGIN:VCALENDAR\r\nX-FOO;CN=abc:v\r\nEND:VCALENDAR\r\n",
			right: "BEGIN:VCALENDAR\r\nX-FOO;CN=\"abc\":v\r\nEND:VCALENDAR\r\n",
		},
		{
			name:  "quoted_param_keeps_separators",
			left:  "BEGIN:VCALENDAR\r\nX-FOO;ALTREP=\"a;b,c:d\":v\r\nEND:VCALENDAR\r\n",
			right: "BEGIN:VCALENDAR\r\nX-FOO;ALTREP=a\\;b\\,c\\:d:v\r\nEND:VCALENDAR\r\n",
		},
		{
			name:  "parameter_order_insignificant",
			left:  "BEGIN:VCALENDAR\r\nX-FOO;CN=a;LANGUAGE=en:v\r\nEND:VCALENDAR\r\n",
			right: "BEGIN:VCALENDAR\r\nX-FOO;LANGUAGE=en;CN=a:v\r\nEND:VCALENDAR\r\n",
		},
	}
	// Fix the space-vs-tab pair to the same long logical line.
	long := "X-FOO:" + strings.Repeat("a", 80)
	pairs[1].left = "BEGIN:VCALENDAR\r\n" + long[:75] + "\r\n " + long[75:] + "\r\nEND:VCALENDAR\r\n"
	pairs[1].right = "BEGIN:VCALENDAR\r\n" + long[:75] + "\r\n\t" + long[75:] + "\r\nEND:VCALENDAR\r\n"

	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			a, err := ParseCalendar(strings.NewReader(p.left))
			if err != nil {
				t.Fatalf("left parse: %v", err)
			}
			b, err := ParseCalendar(strings.NewReader(p.right))
			if err != nil {
				t.Fatalf("right parse: %v", err)
			}
			if !semCalendarEqual(semFromCalendar(a), semFromCalendar(b)) {
				t.Fatalf("equivalent mutants produced different models\nleft:  %#v\nright: %#v",
					semFromCalendar(a), semFromCalendar(b))
			}
		})
	}
}

// TestPinnedQuotedParameterSplitting pins the parameter-splitting edge that
// random text fuzzing rarely constructs on purpose: a quoted value contains
// ; : , and the parser must treat them as data, while the following bare
// parameter keeps its own multi-value commas.
func TestPinnedQuotedParameterSplitting(t *testing.T) {
	doc := "BEGIN:VCALENDAR\r\n" +
		"X-FOO;ALTREP=\"a;b:c,d\";CN=one,two;X-BAR=\"x;y\":v\r\n" +
		"END:VCALENDAR\r\n"
	cal, err := ParseCalendar(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p := cal.CalendarProperties[0]
	if got := p.ICalParameters["ALTREP"]; len(got) != 1 || got[0] != "a;b:c,d" {
		t.Fatalf("quoted separators split incorrectly: %v", got)
	}
	if got := p.ICalParameters["CN"]; len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("bare multi-value split incorrectly: %v", got)
	}
	if got := p.ICalParameters["X-BAR"]; len(got) != 1 || got[0] != "x;y" {
		t.Fatalf("second quoted parameter lost: %v", got)
	}
	out := cal.Serialize(WithNewLineWindows)
	reparsed, err := ParseCalendar(strings.NewReader(out))
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if !semCalendarEqual(semFromCalendar(cal), semFromCalendar(reparsed)) {
		t.Fatalf("parameter semantics drifted after reserialize:\n%s", out)
	}
}

// TestPinnedNestedComponentOrder pins nested component order, repeated
// properties, and unknown X- preservation across VTIMEZONE >
// STANDARD/DAYLIGHT and VEVENT > VALARM.
func TestPinnedNestedComponentOrder(t *testing.T) {
	cal := NewCalendar()
	tz := NewTimezone("America/New_York")
	std := NewStandard()
	std.Properties = []IANAProperty{
		{BaseProperty: BaseProperty{IANAToken: "TZOFFSETFROM", Value: "-0400", ICalParameters: map[string][]string{}}},
		{BaseProperty: BaseProperty{IANAToken: "TZOFFSETTO", Value: "-0500", ICalParameters: map[string][]string{}}},
		{BaseProperty: BaseProperty{IANAToken: "X-GEN-KEEP", Value: "first;second,third", ICalParameters: map[string][]string{}}},
	}
	dl := &Daylight{}
	dl.Properties = []IANAProperty{
		{BaseProperty: BaseProperty{IANAToken: "TZOFFSETFROM", Value: "-0500", ICalParameters: map[string][]string{}}},
		{BaseProperty: BaseProperty{IANAToken: "TZOFFSETTO", Value: "-0400", ICalParameters: map[string][]string{}}},
	}
	tz.addComponent(std)
	tz.addComponent(dl)
	cal.addComponent(tz)

	ev := NewEvent("order@example.com")
	ev.Properties = append(ev.Properties,
		IANAProperty{BaseProperty: BaseProperty{IANAToken: "CATEGORIES", Value: "a,b", ICalParameters: map[string][]string{}}},
		IANAProperty{BaseProperty: BaseProperty{IANAToken: "CATEGORIES", Value: "c,d", ICalParameters: map[string][]string{}}},
		IANAProperty{BaseProperty: BaseProperty{IANAToken: "X-GEN-UNKNOWN", Value: "kept\nverbatim", ICalParameters: map[string][]string{}}},
	)
	alarm := &VAlarm{}
	alarm.Properties = []IANAProperty{
		{BaseProperty: BaseProperty{IANAToken: "ACTION", Value: "DISPLAY", ICalParameters: map[string][]string{}}},
		{BaseProperty: BaseProperty{IANAToken: "X-GEN-ALARM", Value: "z", ICalParameters: map[string][]string{}}},
	}
	ev.addComponent(alarm)
	cal.addComponent(ev)

	doc := cal.Serialize(WithNewLineWindows)
	assertFoldingStructure(t, doc)
	parsed, err := ParseCalendar(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := semFromCalendar(parsed)
	want := semFromCalendar(cal)
	if !semCalendarEqual(want, got) {
		t.Fatalf("nested model changed:\nwant=%#v\ngot =%#v", want, got)
	}
	// Explicit structural pinning: ordering and counts.
	if len(parsed.Components) != 2 {
		t.Fatalf("expected 2 top components, got %d", len(parsed.Components))
	}
	gotTz := parsed.Timezones()[0]
	if len(gotTz.SubComponents()) != 2 {
		t.Fatalf("expected 2 timezone subcomponents in order, got %d", len(gotTz.SubComponents()))
	}
	if _, ok := gotTz.SubComponents()[0].(*Standard); !ok {
		t.Fatalf("first timezone subcomponent must remain STANDARD")
	}
	if _, ok := gotTz.SubComponents()[1].(*Daylight); !ok {
		t.Fatalf("second timezone subcomponent must remain DAYLIGHT")
	}
	gotEv := parsed.Events()[0]
	if len(gotEv.GetProperties(ComponentPropertyCategories)) != 2 {
		t.Fatalf("expected 2 repeated CATEGORIES properties")
	}
	if !hasIANAProperty(gotTz.SubComponents()[0].UnknownPropertiesIANAProperties(), "X-GEN-KEEP") {
		t.Fatalf("unknown X- property inside STANDARD was swallowed")
	}
	if !hasIANAProperty(gotEv.Alarms()[0].UnknownPropertiesIANAProperties(), "X-GEN-ALARM") {
		t.Fatalf("unknown X- property inside VALARM was swallowed")
	}
}

func hasIANAProperty(props []IANAProperty, token string) bool {
	for _, p := range props {
		if p.IANAToken == token {
			return true
		}
	}
	return false
}
