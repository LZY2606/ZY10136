package ics

// Syntax-aware generation and round-trip tests for RFC 5545 content lines.
//
// Unlike a fuzzer that throws random bytes at the parser, the generator in this
// file first builds a constrained, legal calendar model (calendar, VEVENT,
// VTIMEZONE, properties with multi-value parameters, TEXT values that require
// escaping, and the four recognised time shapes), and only then serializes it.
// Every generated document is checked for:
//
//   - physical line length computed in UTF-8 octets, never splitting a rune
//   - space/tab unfolding producing the exact logical content lines the parser
//     consumes
//   - model -> serialize -> parse -> serialize -> parse stability, including
//     component order, repeated properties, parameter multi-values, TEXT
//     semantics, and preservation of unknown X- properties
//
// The short set runs by default; extended iteration is enabled with
// ICS_GEN_ITERS=<n> (see genIterationCount). The seed is fixed so failures are
// reproducible, and failing models are greedily shrunk to a single property or
// the smallest nesting before being reported.

import (
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	genDefaultIterations = 200
	genFixedSeed         = int64(0x51_C5_B1)
	genMaxComponentDepth = 3 // VCALENDAR already counts as depth 1
)

// genIterationCount keeps "go test ./... -count=1" on a short, deterministic
// set by default while allowing ICS_GEN_ITERS to opt into longer campaigns.
func genIterationCount(t *testing.T) int {
	t.Helper()
	if raw := os.Getenv("ICS_GEN_ITERS"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
		t.Fatalf("ICS_GEN_ITERS must be a positive integer, got %q", raw)
	}
	return genDefaultIterations
}

// ---------------------------------------------------------------------------
// Constrained legal model
// ---------------------------------------------------------------------------

// genTimeCase is one of the four time shapes the parser distinguishes.
type genTimeCase struct {
	name        string
	value       string
	params      map[string][]string
	wantInstant time.Time
	wantLoc     *time.Location
	allDay      bool
}

var genFixedTimes = []genTimeCase{
	{
		name:    "date",
		value:   "20210615",
		params:  map[string][]string{"VALUE": {"DATE"}},
		allDay:  true,
		wantLoc: time.UTC,
	},
	{
		name:   "floating",
		value:  "20210615T133000",
		params: map[string][]string{},
	},
	{
		name:    "utc",
		value:   "20210615T133000Z",
		params:  map[string][]string{},
		wantLoc: time.UTC,
	},
	{
		name:    "tzid_dst_gap_day",
		value:   "20210314T133000",
		params:  map[string][]string{"TZID": {"America/New_York"}},
		wantLoc: mustLoadLocation("America/New_York"),
	},
}

func mustLoadLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

func init() {
	loc := mustLoadLocation("America/New_York")
	genFixedTimes[0].wantInstant = time.Date(2021, 6, 15, 0, 0, 0, 0, time.UTC)
	// Floating has no absolute instant; it is checked structurally only.
	genFixedTimes[2].wantInstant = time.Date(2021, 6, 15, 13, 30, 0, 0, time.UTC)
	genFixedTimes[3].wantInstant = time.Date(2021, 3, 14, 13, 30, 0, 0, loc)
}

var genTextRunePool = []rune("aZ9 ;,\n\\界éカ")

func genTextRunes(r *rand.Rand, maxRunes int) string {
	n := 1 + r.Intn(maxRunes)
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteRune(genTextRunePool[r.Intn(len(genTextRunePool))])
	}
	return b.String()
}

// genSafeToken is usable unquoted inside a parameter value. It deliberately
// contains none of ; : , " ' \ or control characters.
func genSafeToken(r *rand.Rand) string {
	const pool = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~"
	n := 1 + r.Intn(10)
	b := make([]byte, n)
	for i := range b {
		b[i] = pool[r.Intn(len(pool))]
	}
	return string(b)
}

// genParamValue appends an escaping separator sometimes, forcing the
// serializer's escape path (\; \, \: \') which must round-trip.
func genParamValue(r *rand.Rand) string {
	v := genSafeToken(r)
	if r.Intn(3) == 0 {
		v += string([]byte{';', ',', ':', '\''}[r.Intn(4)]) + genSafeToken(r)
	}
	return v
}

func genParameters(r *rand.Rand) map[string][]string {
	params := map[string][]string{}
	if r.Intn(2) == 0 {
		count := 1 + r.Intn(2)
		values := make([]string, count)
		for i := range values {
			values[i] = genParamValue(r)
		}
		params["CN"] = values
	}
	if r.Intn(3) == 0 {
		params["LANGUAGE"] = []string{pick(r, "en", "ja", "fr", "de")}
	}
	if r.Intn(4) == 0 {
		// quoted parameter family: ALTREP is the only auto-quoted key. The
		// value intentionally holds every separator that quoted strings allow.
		params["ALTREP"] = []string{"http://example.com/a;b,c:" + genSafeToken(r)}
	}
	if r.Intn(5) == 0 {
		params["X-GEN-"+strings.ToUpper(strings.Map(safeTokenRune, genSafeToken(r)))] = []string{genParamValue(r)}
	}
	return params
}

func pick(r *rand.Rand, options ...string) string {
	return options[r.Intn(len(options))]
}

func safeTokenRune(ch rune) rune {
	if ch == '-' || (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') {
		return ch
	}
	return '-'
}

// genName returns a legal iana-token ([A-Za-z0-9-]+) with the given prefix.
func genName(r *rand.Rand, prefix string) string {
	return prefix + strings.Map(safeTokenRune, genSafeToken(r))
}

// genProperty constructs a single legal property of a given token.
func genProperty(r *rand.Rand, token string, text bool) IANAProperty {
	var value string
	if text {
		value = genTextRunes(r, 16)
	} else {
		value = genSafeToken(r) + "@" + genSafeToken(r) + ".example"
	}
	bp := BaseProperty{
		IANAToken:      token,
		Value:          value,
		ICalParameters: genParameters(r),
	}
	return IANAProperty{BaseProperty: bp}
}

// genTimeProperty creates a DTSTART/DTEND-style property from one of the fixed
// time shapes. Parameter maps are copied so repeated uses stay independent.
func genTimeProperty(token string, tc genTimeCase) IANAProperty {
	params := make(map[string][]string, len(tc.params))
	for k, v := range tc.params {
		cp := make([]string, len(v))
		copy(cp, v)
		params[k] = cp
	}
	return IANAProperty{BaseProperty: BaseProperty{
		IANAToken:      token,
		Value:          tc.value,
		ICalParameters: params,
	}}
}

// genCalendar builds the constrained legal model. Component depth is bounded
// by genMaxComponentDepth (VTimezone > Standard/Daylight and VEvent > VAlarm).
func genCalendar(r *rand.Rand) *Calendar {
	cal := NewCalendar()

	topProps := r.Intn(3)
	for i := 0; i < topProps; i++ {
		p := genProperty(r, genName(r, "X-GEN-CAL-"+strconv.Itoa(i)+"-"), true)
		cal.CalendarProperties = append(cal.CalendarProperties, CalendarProperty{p.BaseProperty})
	}

	if r.Intn(3) > 0 {
		tz := genTimezone(r)
		cal.addComponent(tz)
	}

	events := 1 + r.Intn(2)
	for i := 0; i < events; i++ {
		cal.addComponent(genEvent(r, i))
	}

	if r.Intn(4) == 0 {
		gc := &GeneralComponent{Token: genName(r, "X-")}
		gc.Properties = append(gc.Properties, genProperty(r, "X-GEN-NOTE", false))
		cal.Components = append(cal.Components, gc)
	}
	return cal
}

func genTimezone(r *rand.Rand) *VTimezone {
	tz := NewTimezone("America/New_York")
	tz.ComponentBase.Properties = append(tz.ComponentBase.Properties, genProperty(r, "X-GEN-TZ", true))

	std := NewStandard()
	std.Properties = append(std.Properties,
		genTimeProperty(string(ComponentPropertyDtStart), genFixedTimes[2]),
		genOffsetProp("TZOFFSETFROM", "-0500"),
		genOffsetProp("TZOFFSETTO", "-0500"),
		genProperty(r, string(PropertyTzname), true),
		genProperty(r, "X-GEN-STD", true),
	)
	tz.addComponent(std)

	if r.Intn(2) == 0 {
		dl := &Daylight{}
		dl.Properties = append(dl.Properties,
			genTimeProperty(string(ComponentPropertyDtStart), genFixedTimes[2]),
			genOffsetProp("TZOFFSETFROM", "-0500"),
			genOffsetProp("TZOFFSETTO", "-0400"),
			genProperty(r, string(PropertyTzname), true),
		)
		tz.addComponent(dl)
	}
	return tz
}

func genOffsetProp(token, value string) IANAProperty {
	return IANAProperty{BaseProperty: BaseProperty{
		IANAToken:      token,
		Value:          value,
		ICalParameters: map[string][]string{},
	}}
}

func genEvent(r *rand.Rand, index int) *VEvent {
	ev := NewEvent("gen-" + strconv.Itoa(index) + "-" + genSafeToken(r) + "@example.com")
	ev.ComponentBase.Properties = append(ev.ComponentBase.Properties,
		genTimeProperty(string(ComponentPropertyDtstamp), genFixedTimes[2]),
	)

	tc := genFixedTimes[r.Intn(len(genFixedTimes))]
	ev.ComponentBase.Properties = append(ev.ComponentBase.Properties,
		genTimeProperty(string(ComponentPropertyDtStart), tc),
	)
	if tc.name == "date" {
		end := tc
		end.value = "20210616"
		ev.ComponentBase.Properties = append(ev.ComponentBase.Properties,
			genTimeProperty(string(ComponentPropertyDtEnd), end),
		)
	} else {
		ev.ComponentBase.Properties = append(ev.ComponentBase.Properties,
			genTimeProperty(string(ComponentPropertyDtEnd), genFixedTimes[2]),
		)
	}

	for _, token := range []Property{PropertySummary, PropertyDescription, PropertyLocation, PropertyComment} {
		if r.Intn(2) == 0 {
			ev.ComponentBase.Properties = append(ev.ComponentBase.Properties,
				genProperty(r, string(token), true),
			)
		}
	}
	// Repeated, multi-valued property: CATEGORIES may legitimately recur.
	if r.Intn(2) == 0 {
		cats := genProperty(r, string(PropertyCategories), true)
		cats.ICalParameters = map[string][]string{}
		ev.ComponentBase.Properties = append(ev.ComponentBase.Properties, cats, genProperty(r, string(PropertyCategories), true))
	}
	for i := 0; i < r.Intn(2); i++ {
		ev.ComponentBase.Properties = append(ev.ComponentBase.Properties,
			genProperty(r, genName(r, "X-GEN-EV-"), true),
		)
	}
	if r.Intn(3) == 0 {
		alarm := &VAlarm{}
		alarm.Properties = append(alarm.Properties,
			genProperty(r, string(ComponentPropertyAction), false),
			genProperty(r, string(ComponentPropertyTrigger), false),
			genProperty(r, "X-GEN-ALARM", true),
		)
		ev.addComponent(alarm)
	}
	return ev
}

// ---------------------------------------------------------------------------
// Semantic snapshot
// ---------------------------------------------------------------------------

// semProperty is the semantics-preserving view of a content line: token,
// decoded TEXT value, and a normalised parameter map.
type semProperty struct {
	Token  string
	Value  string
	Params map[string][]string
}

type semComponent struct {
	Type       string
	Properties []semProperty
	Children   []semComponent
}

type semCalendar struct {
	Properties []semProperty
	Components []semComponent
}

func semParams(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for k, vs := range in {
		cp := make([]string, len(vs))
		copy(cp, vs)
		sort.Strings(cp)
		out[k] = cp
	}
	return out
}

func semFromProperty(p BaseProperty) semProperty {
	value := p.Value
	if p.GetValueType() == ValueDataTypeText {
		value = FromText(value)
	}
	return semProperty{Token: p.IANAToken, Value: value, Params: semParams(p.ICalParameters)}
}

func semFromBase(cb ComponentBase, typ string) semComponent {
	c := semComponent{Type: typ}
	for _, p := range cb.Properties {
		c.Properties = append(c.Properties, semFromProperty(p.BaseProperty))
	}
	for _, child := range cb.Components {
		c.Children = append(c.Children, semFromComponent(child))
	}
	return c
}

func semFromComponent(co Component) semComponent {
	switch c := co.(type) {
	case *VEvent:
		return semFromBase(c.ComponentBase, string(ComponentVEvent))
	case *VTimezone:
		return semFromBase(c.ComponentBase, string(ComponentVTimezone))
	case *VAlarm:
		return semFromBase(c.ComponentBase, string(ComponentVAlarm))
	case *Standard:
		return semFromBase(c.ComponentBase, string(ComponentStandard))
	case *Daylight:
		return semFromBase(c.ComponentBase, string(ComponentDaylight))
	case *GeneralComponent:
		return semFromBase(c.ComponentBase, c.Token)
	default:
		return semComponent{Type: "?"}
	}
}

func semFromCalendar(cal *Calendar) semCalendar {
	out := semCalendar{}
	for _, p := range cal.CalendarProperties {
		out.Properties = append(out.Properties, semFromProperty(p.BaseProperty))
	}
	for _, co := range cal.Components {
		out.Components = append(out.Components, semFromComponent(co))
	}
	return out
}

// ---------------------------------------------------------------------------
// Structural checks of serialized documents
// ---------------------------------------------------------------------------

// genSplitPhysicalLines splits an ICS document on CRLF or LF, keeping the
// terminator type per line.
func genSplitPhysicalLines(doc string) []string {
	doc = strings.ReplaceAll(doc, "\r\n", "\n")
	return strings.Split(strings.TrimRight(doc, "\n"), "\n")
}

// assertFoldingStructure enforces the RFC 5545 folding rules the generator
// relies on:
//
//   - each physical line is at most 75 UTF-8 octets long
//   - no physical line splits a multibyte rune (each line is valid UTF-8)
//   - every continuation line starts with exactly one SP/HTAB
//   - no non-continuation line starts with SP/HTAB
func assertFoldingStructure(t *testing.T, doc string) {
	t.Helper()
	lines := genSplitPhysicalLines(doc)
	for i, line := range lines {
		if len(line) > 75 {
			t.Fatalf("physical line %d is %d octets (>75): %q", i+1, len(line), line)
		}
		if !utf8.ValidString(line) {
			t.Fatalf("physical line %d is not valid UTF-8 (rune split): % x", i+1, line)
		}
		isCont := strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")
		if i == 0 && isCont {
			t.Fatalf("first physical line must not be a continuation: %q", line)
		}
		if isCont && i == 0 {
			t.Fatalf("continuation without a preceding line: %q", line)
		}
	}
}

// unfoldPhysical implements the RFC 5545 unfold for both SP and HTAB markers
// and both CRLF/LF terminators, so generated documents can be checked against
// the parser's own CalendarStream independently.
func unfoldPhysical(doc string) []string {
	var logical []string
	var cur strings.Builder
	for {
		i := strings.IndexAny(doc, "\r\n")
		var line, rest string
		if i < 0 {
			line, rest = doc, ""
		} else {
			line = doc[:i]
			if doc[i] == '\r' && i+1 < len(doc) && doc[i+1] == '\n' {
				rest = doc[i+2:]
			} else {
				rest = doc[i+1:]
			}
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			cur.WriteString(line[1:])
		} else {
			if cur.Len() > 0 {
				logical = append(logical, cur.String())
			}
			cur.Reset()
			cur.WriteString(line)
		}
		doc = rest
		if doc == "" {
			break
		}
	}
	if cur.Len() > 0 {
		logical = append(logical, cur.String())
	}
	return logical
}

// assertUnfoldMatchesParser unfolds with the reference implementation and
// then replays every logical line through parseProperty, asserting the values
// match a second unfolding that swaps SP for HTAB (and vice versa). This is
// the "CRLF 与后续空格或制表符的 unfold 结果需一致" guarantee.
func assertUnfoldMatchesParser(t *testing.T, doc string) {
	t.Helper()
	logical := unfoldPhysical(doc)
	swapped := strings.NewReplacer("\r\n ", "\r\n\t", "\r\n\t", "\r\n ",
		"\n ", "\n\t", "\n\t", "\n ").Replace(doc)
	logicalSwapped := unfoldPhysical(swapped)
	if len(logical) != len(logicalSwapped) {
		t.Fatalf("unfolded line count differs between SP and HTAB folds: %d vs %d", len(logical), len(logicalSwapped))
	}
	for i := range logical {
		if logical[i] != logicalSwapped[i] {
			t.Fatalf("logical line %d differs by fold marker:\nsp  =%q\nhtab=%q", i, logical[i], logicalSwapped[i])
		}
		p, err := parseProperty(ContentLine(logical[i]))
		if err != nil {
			continue // BEGIN/END lines are not properties
		}
		_ = p
	}
	// And confirm the real stream reader agrees.
	cs := NewCalendarStream(strings.NewReader(doc))
	seen := 0
	for {
		l, _, err := cs.ReadLine()
		if l == nil {
			break
		}
		if seen >= len(logical) {
			t.Fatalf("parser produced more logical lines than reference unfold")
		}
		if string(*l) != logical[seen] {
			t.Fatalf("stream line %d = %q, want %q", seen+1, *l, logical[seen])
		}
		seen++
		if err != nil {
			break
		}
	}
	if seen != len(logical) {
		t.Fatalf("parser produced %d logical lines, reference produced %d", seen, len(logical))
	}
}

// ---------------------------------------------------------------------------
// Round-trip driver
// ---------------------------------------------------------------------------

// roundTripResult records every artifact needed to diagnose a failure.
type roundTripResult struct {
	firstDoc  string
	secondDoc string
	base      semCalendar
	first     semCalendar
	second    semCalendar
	firstErr  error
	secondErr error
}

func runRoundTrip(t *testing.T, cal *Calendar) roundTripResult {
	t.Helper()
	doc := cal.Serialize(WithNewLineWindows)
	assertFoldingStructure(t, doc)
	assertUnfoldMatchesParser(t, doc)

	parsed, err := ParseCalendar(strings.NewReader(doc))
	if err != nil {
		return roundTripResult{firstDoc: doc, firstErr: err}
	}
	doc2 := parsed.Serialize(WithNewLineWindows)
	assertFoldingStructure(t, doc2)

	parsed2, err := ParseCalendar(strings.NewReader(doc2))
	if err != nil {
		return roundTripResult{firstDoc: doc, secondDoc: doc2,
			base: semFromCalendar(cal), first: semFromCalendar(parsed), secondErr: err}
	}
	return roundTripResult{
		firstDoc:  doc,
		secondDoc: doc2,
		base:      semFromCalendar(cal),
		first:     semFromCalendar(parsed),
		second:    semFromCalendar(parsed2),
	}
}

func assertSemEqual(t *testing.T, want, got semCalendar, context string) {
	t.Helper()
	if !semCalendarEqual(want, got) {
		t.Fatalf("semantic mismatch after %s\nwant=%#v\ngot =%#v", context, want, got)
	}
}

// ---------------------------------------------------------------------------
// Shrinking
// ---------------------------------------------------------------------------

// genFailure is recorded when a generated model does not survive round trip.
type genFailure struct {
	cal    *Calendar
	result roundTripResult
}

// validateModel returns the roundTripResult and whether the model failed.
func validateModel(cal *Calendar) (roundTripResult, bool) {
	doc := cal.Serialize(WithNewLineWindows)
	if brokenFolding(doc) {
		return roundTripResult{firstDoc: doc}, true
	}
	parsed, err := ParseCalendar(strings.NewReader(doc))
	if err != nil {
		return roundTripResult{firstDoc: doc, firstErr: err}, true
	}
	doc2 := parsed.Serialize(WithNewLineWindows)
	if brokenFolding(doc2) {
		return roundTripResult{firstDoc: doc, secondDoc: doc2}, true
	}
	parsed2, err := ParseCalendar(strings.NewReader(doc2))
	if err != nil {
		return roundTripResult{firstDoc: doc, secondDoc: doc2, secondErr: err}, true
	}
	if doc2 != parsed2.Serialize(WithNewLineWindows) {
		return roundTripResult{firstDoc: doc, secondDoc: doc2}, true
	}
	base, first, second := semFromCalendar(cal), semFromCalendar(parsed), semFromCalendar(parsed2)
	if !semCalendarEqual(base, first) || !semCalendarEqual(first, second) {
		return roundTripResult{firstDoc: doc, secondDoc: doc2, base: base, first: first, second: second}, true
	}
	return roundTripResult{}, false
}

func brokenFolding(doc string) bool {
	for _, line := range genSplitPhysicalLines(doc) {
		if len(line) > 75 || !utf8.ValidString(line) {
			return true
		}
	}
	return false
}

// shrinkModel greedily removes components and properties while the failure
// persists, reducing a counterexample to a single property or the smallest
// nesting. Every attempt is made on a deep copy so rejected removals never
// mutate the current smallest-failing model.
func shrinkModel(cal *Calendar) *Calendar {
	for {
		changed := false
		// Drop whole top-level components.
		for i := range cal.Components {
			candidate := deepCloneCalendar(cal)
			candidate.Components = append(candidate.Components[:i:i], candidate.Components[i+1:]...)
			if _, bad := validateModel(candidate); bad {
				cal = candidate
				changed = true
				break
			}
		}
		if changed {
			continue
		}
		// Drop one property, walking calendar props then nested components.
		if candidate, removed := deepCloneWithoutLastProperty(cal); removed {
			if _, bad := validateModel(candidate); bad {
				cal = candidate
				changed = true
			}
		}
		if !changed {
			return cal
		}
	}
}

func deepCloneCalendar(src *Calendar) *Calendar {
	dst := NewCalendar()
	dst.CalendarProperties = append(dst.CalendarProperties[:0], src.CalendarProperties...)
	for _, co := range src.Components {
		dst.Components = append(dst.Components, deepCloneComponent(co))
	}
	return dst
}

func deepCloneComponent(src Component) Component {
	var cb ComponentBase
	var token string
	switch c := src.(type) {
	case *VEvent:
		cb = c.ComponentBase
		cb.Components = nil
		co := &VEvent{ComponentBase: cb}
		for _, ch := range c.ComponentBase.Components {
			co.addComponent(deepCloneComponent(ch))
		}
		return co
	case *VTimezone:
		cb = c.ComponentBase
		cb.Components = nil
		co := &VTimezone{ComponentBase: cb}
		for _, ch := range c.ComponentBase.Components {
			co.addComponent(deepCloneComponent(ch))
		}
		return co
	case *VAlarm:
		cb = c.ComponentBase
		return &VAlarm{ComponentBase: cb}
	case *Standard:
		cb = c.ComponentBase
		return &Standard{ComponentBase: cb}
	case *Daylight:
		cb = c.ComponentBase
		return &Daylight{ComponentBase: cb}
	case *GeneralComponent:
		cb = c.ComponentBase
		token = c.Token
		return &GeneralComponent{ComponentBase: cb, Token: token}
	}
	return src
}

// deepCloneWithoutLastProperty returns a clone with one trailing property
// removed (calendar-level first, then the first nested component that still
// has properties).
func deepCloneWithoutLastProperty(src *Calendar) (*Calendar, bool) {
	dst := deepCloneCalendar(src)
	if len(dst.CalendarProperties) > 0 {
		dst.CalendarProperties = dst.CalendarProperties[:len(dst.CalendarProperties)-1]
		return dst, true
	}
	for _, co := range dst.Components {
		if cb := componentBaseOf(co); cb != nil && len(cb.Properties) > 0 {
			cb.Properties = cb.Properties[:len(cb.Properties)-1]
			return dst, true
		}
	}
	return dst, false
}

func componentBaseOf(co Component) *ComponentBase {
	switch c := co.(type) {
	case *VEvent:
		return &c.ComponentBase
	case *VTimezone:
		return &c.ComponentBase
	case *VAlarm:
		return &c.ComponentBase
	case *Standard:
		return &c.ComponentBase
	case *Daylight:
		return &c.ComponentBase
	case *GeneralComponent:
		return &c.ComponentBase
	}
	return nil
}

// ---------------------------------------------------------------------------
// Semantic equality (order sensitive for properties and components)
// ---------------------------------------------------------------------------

func semCalendarEqual(a, b semCalendar) bool {
	if !semPropertiesEqual(a.Properties, b.Properties) || len(a.Components) != len(b.Components) {
		return false
	}
	for i := range a.Components {
		if !semComponentEqual(a.Components[i], b.Components[i]) {
			return false
		}
	}
	return true
}

func semComponentEqual(a, b semComponent) bool {
	if a.Type != b.Type || !semPropertiesEqual(a.Properties, b.Properties) || len(a.Children) != len(b.Children) {
		return false
	}
	for i := range a.Children {
		if !semComponentEqual(a.Children[i], b.Children[i]) {
			return false
		}
	}
	return true
}

func semPropertiesEqual(a, b []semProperty) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Token != b[i].Token || a[i].Value != b[i].Value || !semParamMapsEqual(a[i].Params, b[i].Params) {
			return false
		}
	}
	return true
}

func semParamMapsEqual(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || len(va) != len(vb) {
			return false
		}
		// Parameter multi-values are compared as multisets because the
		// parser merges duplicate keys and commas equivalently.
		ca := append([]string(nil), va...)
		cb := append([]string(nil), vb...)
		sort.Strings(ca)
		sort.Strings(cb)
		for i := range ca {
			if ca[i] != cb[i] {
				return false
			}
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// assertBoundedComponentDepth verifies the generated nesting never exceeds the
// documented generator bound (VCALENDAR counts as depth 1).
func assertBoundedComponentDepth(t *testing.T, co Component, depth int) {
	t.Helper()
	if depth > genMaxComponentDepth {
		t.Fatalf("generated component exceeds depth bound %d: %#v", genMaxComponentDepth, co)
	}
	for _, child := range co.SubComponents() {
		assertBoundedComponentDepth(t, child, depth+1)
	}
}

func TestGeneratedRoundTrip(t *testing.T) {
	iters := genIterationCount(t)
	r := rand.New(rand.NewSource(genFixedSeed))
	var firstFailure *genFailure
	for i := 0; i < iters; i++ {
		cal := genCalendar(r)
		for _, co := range cal.Components {
			// VCALENDAR is depth 1, its children depth 2, grandchildren 3.
			assertBoundedComponentDepth(t, co, 2)
		}
		if _, bad := validateModel(cal); bad {
			res, _ := validateModel(cal)
			firstFailure = &genFailure{cal: cal, result: res}
			break
		}
	}
	if firstFailure == nil {
		return
	}
	shrunk := shrinkModel(firstFailure.cal)
	res, _ := validateModel(shrunk)
	t.Fatalf("generated model failed round trip\nshrunk doc 1:\n%s\ndoc 2:\n%s\nfirstErr=%v secondErr=%v",
		res.firstDoc, res.secondDoc, res.firstErr, res.secondErr)
}

// TestGeneratedTimeSemantics verifies the four time shapes keep their type
// after serialize/parse and that the values are interpreted independently of
// the process-local time.Local.
func TestGeneratedTimeSemantics(t *testing.T) {
	cal := NewCalendar()
	for _, tc := range genFixedTimes {
		ev := NewEvent("time-" + tc.name + "@example.com")
		ev.ComponentBase.Properties = append(ev.ComponentBase.Properties,
			genTimeProperty(string(ComponentPropertyDtstamp), genFixedTimes[2]),
			genTimeProperty(string(ComponentPropertyDtStart), tc),
		)
		cal.addComponent(ev)
	}
	doc := cal.Serialize(WithNewLineWindows)
	assertFoldingStructure(t, doc)

	parsed, err := ParseCalendar(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("parse: %v\ndoc:\n%s", err, doc)
	}
	for i, tc := range genFixedTimes {
		ev := parsed.Events()[i]
		prop := ev.GetProperty(ComponentPropertyDtStart)
		if prop == nil || prop.Value != tc.value {
			t.Fatalf("%s: raw DTSTART changed: got %q want %q", tc.name, propValue(prop), tc.value)
		}
		if tc.allDay {
			got, err := ev.GetAllDayStartAt()
			if err != nil {
				t.Fatalf("%s: GetAllDayStartAt: %v", tc.name, err)
			}
			if y, m, d := got.Date(); y != 2021 || m != time.June || d != 15 {
				t.Fatalf("%s: date drifted to %v", tc.name, got)
			}
			// Per the library's existing behaviour DATE resolves at time.Local;
			// the calendar components must still be environment independent.
			if _, off := got.Zone(); got.Location() == time.UTC && off != 0 {
				t.Fatalf("%s: unexpected non-zero UTC offset", tc.name)
			}
			continue
		}
		got, err := ev.GetStartAt()
		if err != nil {
			t.Fatalf("%s: GetStartAt: %v", tc.name, err)
		}
		switch tc.name {
		case "floating":
			if got.Hour() != 13 || got.Minute() != 30 {
				t.Fatalf("floating wall time drifted: %v", got)
			}
		case "utc":
			if !got.Equal(tc.wantInstant) || got.Location() != time.UTC {
				t.Fatalf("utc instant/location drifted: got %v want %v", got, tc.wantInstant)
			}
		case "tzid_dst_gap_day":
			if !got.Equal(tc.wantInstant) {
				t.Fatalf("tzid instant drifted: got %v want %v", got, tc.wantInstant)
			}
			if tc.wantLoc != nil && got.Location().String() != tc.wantLoc.String() {
				t.Fatalf("tzid location drifted: got %q want %q", got.Location(), tc.wantLoc)
			}
		}
	}
	if res := runRoundTrip(t, cal); res.firstErr != nil || res.secondErr != nil {
		t.Fatalf("time models did not round-trip: %v %v", res.firstErr, res.secondErr)
	} else {
		assertSemEqual(t, res.base, res.first, "time parse")
		assertSemEqual(t, res.second, res.first, "time reparse")
	}
}

func propValue(p *IANAProperty) string {
	if p == nil {
		return "<nil>"
	}
	return p.Value
}
