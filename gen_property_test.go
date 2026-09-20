// Syntax-aware generation and mutation tests.
//
// Running the suite:
//
// The default command runs a short, fixed collection (200 deterministic seeds
// plus the pinned regression cases):
//
//	go test ./... -count=1
//	go test -short ./...   // 25 seeds, even faster
//
// Larger exploratory rounds are opt-in so CI without an env override stays
// deterministic and quick:
//
//	ICAL_GEN_ROUNDS=4000 go test ./...
//	ICAL_GEN_EXTENDED=1   go test ./...   // 4000 rounds incl. mutation campaign
//
// The mutation campaign (TestMutationCampaign) only runs in extended mode.
// When it finds a counterexample, shrinkModel reduces it to a single property
// or minimal nesting and the result is pinned in gen_regression_test.go.
//
// This file defines the syntax-aware model, the constrained generator and the
// semantic oracles shared by gen_roundtrip_test.go and gen_mutation_test.go.
//
// The generator never emits random bytes: it builds a constrained semantic
// model (calendar -> components -> properties -> typed values), serializes it
// with the production serializer, and only mutates the wire form at
// syntactically meaningful positions (folds, parameter separators, BEGIN/END
// pairs). The goal is to detect tiny implementation mistakes in octet counting,
// line unfolding, parameter splitting and TZID write-back rather than inflating
// coverage with random invalid input.

package ics

import (
	"fmt"
	"math/rand"
	"strings"
)

// genKind classifies the four time value shapes distinguished by RFC 5545.
type genKind int

const (
	kindText genKind = iota
	kindPlain
	kindDate
	kindLocal
	kindUTC
	kindTZID
)

// genParam is a single property parameter. quoted controls whether the
// generator emits a quoted-string; unquoted values are drawn from an alphabet
// that needs escaping, quoted values deliberately contain ';', ':' and ','.
type genParam struct {
	key    string
	values []string
	quoted bool
}

// genProp is a semantic property: a token, typed raw value and parameters.
type genProp struct {
	token  string
	kind   genKind
	value  string
	params []genParam
}

// genComp is a semantic component. kind is the BEGIN/END token; children use
// the same structure, which lets the generator express VEVENT > VALARM and
// VTIMEZONE > STANDARD/DAYLIGHT nesting.
type genComp struct {
	kind     string
	props    []genProp
	children []*genComp
}

// genCalendar is the root model. Calendar-level properties are emitted before
// the first component.
type genCalendar struct {
	props      []genProp
	components []*genComp
}

// Generator alphabets. They are intentionally biased towards characters that
// exercise a specific code path: fold boundaries, escapes and NON-US-ASCII.
var (
	// genAsciiRunes is plain ASCII without fold/escape-sensitive characters.
	genAsciiRunes = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 .-_/!?()[]{}@#$%&*+=~")
	// genMultibyteRunes mixes 2/3/4-byte UTF-8 sequences so the 75 octet
	// boundary lands inside a rune for most generated long values.
	genMultibyteRunes = []rune("é界世日𠀀€¢🙂")
	// genEscapeRunes are exactly the TEXT value characters that ToText escapes.
	genEscapeRunes = []rune{'\\', ';', ',', '\n'}
	// genSafeParamRunes is the unquoted parameter alphabet (no ";":", DQUOTE).
	genSafeParamRunes = []rune("abcdefghijklmnopqrstuvwxyz0123456789-_/.@")
)

const (
	// genMaxDepth bounds component nesting (VCALENDAR is depth 0).
	genMaxDepth = 3
	// genGeneratedTZID is a TZID that does not exist in the tz database; the
	// tests install a fixed mapper so resolution never touches the host.
	genGeneratedTZID = "Generated/Zone_2021"
)

// genConfig carries the generation knobs for one round.
type genConfig struct {
	rng *rand.Rand
}

func newGenConfig(seed int64) *genConfig {
	return &genConfig{rng: rand.New(rand.NewSource(seed))}
}

func (g *genConfig) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return g.rng.Intn(n)
}

func (g *genConfig) pickRune(rs []rune) rune {
	return rs[g.intn(len(rs))]
}

// genTextValue builds a TEXT value. long forces a value whose UTF-8 length is
// around the 75 octet fold boundary and biases the alphabet so the boundary
// regularly lands inside a multi-byte rune. noSpace removes word boundaries,
// which exercises the mid-rune / mid-token cut path of the folder.
func (g *genConfig) genTextValue(long, noSpace bool) string {
	var b strings.Builder
	target := g.intn(12) + 4
	if long {
		target = 68 + g.intn(40)
	}
	for b.Len() < target {
		var r rune
		switch g.intn(10) {
		case 0, 1, 2:
			r = g.pickRune(genMultibyteRunes)
		case 3, 4:
			r = g.pickRune(genEscapeRunes)
		default:
			r = g.pickRune(genAsciiRunes)
			if noSpace && r == ' ' {
				r = 'x'
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}

// genPlainValue builds a value that needs no TEXT unescaping (URI-ish or token).
func (g *genConfig) genPlainValue() string {
	var b strings.Builder
	switch g.intn(3) {
	case 0:
		b.WriteString("mailto:")
	case 1:
		b.WriteString("https://example.com/")
	}
	for i := 0; i < g.intn(18)+3; i++ {
		b.WriteRune(g.pickRune(genSafeParamRunes))
	}
	return b.String()
}

// genParamValue produces one parameter value. Quoted values intentionally
// contain the structural separators ';', ':' and ',' which must be ignored
// while scanning a quoted-string, and every such value round-trips exactly.
func (g *genConfig) genParamValue(quoted bool) string {
	var b strings.Builder
	// Unquoted param-value grammar starts with a SAFE-CHAR that matches the
	// token alphabet; a leading '-' terminates parsing early. Seed the value
	// with an alphanumeric character so generated parameters stay legal.
	if !quoted {
		b.WriteByte(byte('a' + g.intn(26)))
	}
	n := g.intn(8) + 1
	for i := 0; i < n; i++ {
		if quoted && g.intn(4) == 0 {
			b.WriteRune([]rune{';', ':', ','}[g.intn(3)])
			continue
		}
		r := g.pickRune(genSafeParamRunes)
		if !quoted && (r == ';' || r == ':' || r == ',') {
			r = 'z'
		}
		b.WriteRune(r)
	}
	if quoted {
		// Include an escaped backslash and an escaped quote as well so the
		// parameter escape path is exercised inside quoted strings.
		b.WriteString(`\\`)
	}
	return b.String()
}

func (g *genConfig) genParam(allowQuoted bool) genParam {
	key := []string{"CN", "ROLE", "PARTSTAT", "CUTYPE", "RSVP", "LANGUAGE", "X-CUSTOM", "X-NUM-GUESTS"}[g.intn(8)]
	quoted := allowQuoted && key == "CN" && g.intn(2) == 0
	if allowQuoted && g.intn(6) == 0 {
		key = "ALTREP"
		quoted = true
	}
	// Only a generic extension parameter is ever multi-valued: every RFC
	// parameter key in the pool is single-valued, and ALTREP in particular is
	// a quoted URI that must not be repeated.
	n := 1
	if key == "X-CUSTOM" && g.intn(2) == 0 {
		n = 2 + g.intn(2)
	}
	values := make([]string, n)
	for i := range values {
		if key == "ALTREP" {
			values[i] = "https://example.com/a;b,c:d"
		} else {
			values[i] = g.genParamValue(quoted)
		}
	}
	return genParam{key: key, values: values, quoted: quoted}
}

// genParamUnique retries key selection until it finds one not already present,
// because parsed ICalParameters live in a map[string][]string and duplicate
// keys cannot be represented distinctly.
func (g *genConfig) genParamUnique(seen map[string]bool, allowQuoted bool) genParam {
	for tries := 0; tries < 10; tries++ {
		param := g.genParam(allowQuoted)
		if !seen[param.key] {
			return param
		}
	}
	return genParam{key: "X-UNIQUE", values: []string{g.genParamValue(false)}}
}

var genTextTokens = []string{"SUMMARY", "DESCRIPTION", "LOCATION", "COMMENT", "X-Summary", "X-NOTE"}
var genPlainTokens = []string{"UID", "ATTENDEE", "URL", "ORGANIZER", "CONTACT", "X-Plain-Id", "STATUS"}

// Time values: fixed wall-clock components make the time-shape oracle
// independent of the host location and zone database.
const (
	genUTCValue   = "20210627T123045Z"
	genLocalValue = "20210627T213000"
	genDateValue  = "20210627"
)

// genTimeProp returns one of the four canonical time shapes. DTSTART/DTEND
// keep the value in the semantic model so the oracle can compare wall clock,
// date, UTC and TZID locations separately.
func (g *genConfig) genTimeProp(token string) genProp {
	p := genProp{token: token}
	switch g.intn(4) {
	case 0:
		p.kind, p.value = kindUTC, genUTCValue
	case 1:
		p.kind, p.value = kindLocal, genLocalValue
	case 2:
		p.kind, p.value = kindDate, genDateValue
		p.params = []genParam{{key: "VALUE", values: []string{"DATE"}}}
	default:
		p.kind, p.value = kindTZID, genLocalValue
		p.params = []genParam{{key: "TZID", values: []string{genGeneratedTZID}}}
	}
	return p
}

func (g *genConfig) genProperty(forceLong bool) genProp {
	p := genProp{}
	switch g.intn(10) {
	case 0:
		p = g.genTimeProp([]string{"DTSTART", "DTEND", "EXDATE", "RDATE"}[g.intn(4)])
	case 1, 2:
		p.token = genPlainTokens[g.intn(len(genPlainTokens))]
		p.kind = kindPlain
		p.value = g.genPlainValue()
	default:
		p.token = genTextTokens[g.intn(len(genTextTokens))]
		p.kind = kindText
		p.value = g.genTextValue(forceLong || g.intn(3) == 0, g.intn(4) == 0)
	}
	if p.kind == kindText || p.kind == kindPlain {
		seen := map[string]bool{}
		for i := 0; i < g.intn(3); i++ {
			param := g.genParamUnique(seen, true)
			p.params = append(p.params, param)
			seen[param.key] = true
		}
	}
	return p
}

// genComponent builds a component tree of bounded depth. childKinds lists the
// component tokens legal at this level; nesting depth is capped at genMaxDepth.
func (g *genConfig) genComponent(kind string, depth int, childKinds []string) *genComp {
	c := &genComp{kind: kind}
	n := g.intn(5) + 1
	for i := 0; i < n; i++ {
		c.props = append(c.props, g.genProperty(depth == 1 && i == 0))
	}
	// Duplicate properties (including duplicate X- props) must survive with
	// their order and distinct values preserved.
	c.props = append(c.props,
		genProp{token: "X-DUP", kind: kindText, value: "dup-" + g.genTextValue(false, false)},
		genProp{token: "X-DUP", kind: kindText, value: "dup-" + g.genTextValue(false, false)},
	)
	if depth < genMaxDepth && len(childKinds) > 0 && g.intn(2) == 0 {
		childKind := childKinds[g.intn(len(childKinds))]
		var grand []string
		switch childKind {
		case "VEVENT":
			grand = []string{"VALARM"}
		case "VTIMEZONE":
			grand = []string{"STANDARD", "DAYLIGHT"}
		}
		c.children = append(c.children, g.genComponent(childKind, depth+1, grand))
	}
	return c
}

// genCalendarModel builds a constrained calendar: properties, VEVENTs and
// VTIMEZONEs in a deterministic order.
func (g *genConfig) genCalendarModel() *genCalendar {
	cal := &genCalendar{}
	cal.props = append(cal.props,
		genProp{token: "VERSION", kind: kindText, value: "2.0"},
		genProp{token: "PRODID", kind: kindText, value: g.genTextValue(false, false)},
	)
	if g.intn(2) == 0 {
		cal.props = append(cal.props, genProp{token: "X-WR-CALNAME", kind: kindText, value: g.genTextValue(g.intn(2) == 0, false)})
	}
	topKinds := []string{"VEVENT", "VTIMEZONE", "VTODO"}
	for i := 0; i < g.intn(3)+1; i++ {
		kind := topKinds[g.intn(len(topKinds))]
		children := []string{}
		switch kind {
		case "VEVENT":
			children = []string{"VALARM"}
		case "VTIMEZONE":
			children = []string{"STANDARD", "DAYLIGHT"}
		}
		cal.components = append(cal.components, g.genComponent(kind, 1, children))
	}
	return cal
}

// basePropFor converts a semantic property into a BaseProperty using the same
// raw value and parameter storage as production serialization.
func basePropFor(p genProp) BaseProperty {
	bp := BaseProperty{
		IANAToken:      p.token,
		ICalParameters: map[string][]string{},
	}
	for _, param := range p.params {
		bp.ICalParameters[param.key] = append([]string(nil), param.values...)
	}
	bp.Value = p.value
	return bp
}

// buildCalendar serializes a model into a *Calendar using the production types,
// so the test exercises the real folder instead of a hand-rolled encoder.
func buildCalendar(m *genCalendar) *Calendar {
	cal, _ := NewCalendarWithOptions()
	cal.CalendarProperties = cal.CalendarProperties[:0]
	for _, p := range m.props {
		bp := basePropFor(p)
		cal.CalendarProperties = append(cal.CalendarProperties, CalendarProperty{BaseProperty: bp})
	}
	for _, cc := range m.components {
		cal.addComponent(buildComponent(cc))
	}
	return cal
}

func buildComponent(m *genComp) Component {
	cb := ComponentBase{}
	for _, p := range m.props {
		cb.Properties = append(cb.Properties, IANAProperty{BaseProperty: basePropFor(p)})
	}
	for _, child := range m.children {
		cb.Components = append(cb.Components, buildComponent(child))
	}
	switch m.kind {
	case "VEVENT":
		return &VEvent{ComponentBase: cb}
	case "VTODO":
		return &VTodo{ComponentBase: cb}
	case "VJOURNAL":
		return &VJournal{ComponentBase: cb}
	case "VFREEBUSY":
		return &VBusy{ComponentBase: cb}
	case "VTIMEZONE":
		return &VTimezone{ComponentBase: cb}
	case "VALARM":
		return &VAlarm{ComponentBase: cb}
	case "STANDARD":
		return &Standard{ComponentBase: cb}
	case "DAYLIGHT":
		return &Daylight{ComponentBase: cb}
	default:
		return &GeneralComponent{ComponentBase: cb, Token: m.kind}
	}
}

// ---- Semantic oracles -----------------------------------------------------

// propSig is the semantic signature of a parsed property: the post-unescape
// value plus a stable parameter representation.
type propSig struct {
	token  string
	value  string
	params string
}

func canonicalParams(in map[string][]string) string {
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	// Sort keys for a stable representation.
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		for i, v := range in[k] {
			if i > 0 {
				b.WriteByte('|')
			}
			b.WriteString(v)
		}
		b.WriteByte(';')
	}
	return b.String()
}

// genUnescapeText is an independent implementation of RFC 5545 TEXT
// unescaping (\\, \; \, \n / \N) used as the generator oracle. It is
// intentionally not the production FromText so a serializer/parser escaping
// pair that drifts together is still caught.
func genUnescapeText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '\\':
			b.WriteByte('\\')
		case ';':
			b.WriteByte(';')
		case ',':
			b.WriteByte(',')
		case 'n', 'N':
			b.WriteByte('\n')
		default:
			// Unrecognized escape: keep the following char, like the parser.
			b.WriteByte(s[i+1])
		}
		i++
	}
	return b.String()
}

// expectedPropValue mirrors the parser value semantics with an independent
// oracle: TEXT-typed properties are unescaped, other values pass through.
func expectedPropValue(p genProp) string {
	if p.kind == kindText {
		return genUnescapeText(ToText(p.value))
	}
	return p.value
}

// propSigs flattens a parsed component tree in document order.
func propSigs(tok string, props []IANAProperty, children []Component, out *[]propSig) {
	for _, p := range props {
		*out = append(*out, propSig{
			token:  tok + "/" + p.IANAToken,
			value:  p.Value,
			params: canonicalParams(p.ICalParameters),
		})
	}
	for _, c := range children {
		switch cc := c.(type) {
		case *VEvent:
			propSigs("VEVENT", cc.Properties, cc.Components, out)
		case *VTodo:
			propSigs("VTODO", cc.Properties, cc.Components, out)
		case *VTimezone:
			propSigs("VTIMEZONE", cc.Properties, cc.Components, out)
		case *VAlarm:
			propSigs("VALARM", cc.Properties, cc.Components, out)
		case *Standard:
			propSigs("STANDARD", cc.Properties, cc.Components, out)
		case *Daylight:
			propSigs("DAYLIGHT", cc.Properties, cc.Components, out)
		case *GeneralComponent:
			propSigs(cc.Token, cc.Properties, cc.Components, out)
		default:
			propSigs(fmt.Sprintf("%T", c), c.UnknownPropertiesIANAProperties(), c.SubComponents(), out)
		}
	}
}

func calendarPropSigs(cal *Calendar) []propSig {
	out := make([]propSig, 0, len(cal.CalendarProperties))
	for _, p := range cal.CalendarProperties {
		out = append(out, propSig{
			token:  "VCALENDAR/" + p.IANAToken,
			value:  p.Value,
			params: canonicalParams(p.ICalParameters),
		})
	}
	for _, c := range cal.Components {
		switch cc := c.(type) {
		case *VEvent:
			propSigs("VEVENT", cc.Properties, cc.Components, &out)
		case *VTodo:
			propSigs("VTODO", cc.Properties, cc.Components, &out)
		case *VTimezone:
			propSigs("VTIMEZONE", cc.Properties, cc.Components, &out)
		case *GeneralComponent:
			propSigs(cc.Token, cc.Properties, cc.Components, &out)
		default:
			propSigs(fmt.Sprintf("%T", c), c.UnknownPropertiesIANAProperties(), c.SubComponents(), &out)
		}
	}
	return out
}

// modelPropSigs renders the expected signatures from the semantic model.
func modelPropSigs(m *genCalendar) []propSig {
	out := make([]propSig, 0)
	for _, p := range m.props {
		out = append(out, propSig{
			token:  "VCALENDAR/" + p.token,
			value:  expectedPropValue(p),
			params: modelParamSig(p.params),
		})
	}
	for _, c := range m.components {
		modelCompSigs(c, &out)
	}
	return out
}

func modelParamSig(params []genParam) string {
	var b strings.Builder
	keys := make([]string, 0, len(params))
	idx := map[string][]string{}
	for _, p := range params {
		if _, ok := idx[p.key]; !ok {
			keys = append(keys, p.key)
		}
		idx[p.key] = append(idx[p.key], p.values...)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		for i, v := range idx[k] {
			if i > 0 {
				b.WriteByte('|')
			}
			// The model->wire direction is fixed by the production escaper;
			// the oracle independently undoes what the parser should undo so
			// a parser-side escape drift is still detected.
			b.WriteString(genUnescapeText(escapeValueString(v)))
		}
		b.WriteByte(';')
	}
	return b.String()
}

func modelCompSigs(c *genComp, out *[]propSig) {
	for _, p := range c.props {
		*out = append(*out, propSig{
			token:  c.kind + "/" + p.token,
			value:  expectedPropValue(p),
			params: modelParamSig(p.params),
		})
	}
	for _, child := range c.children {
		modelCompSigs(child, out)
	}
}

// sigsEqual compares two semantic signatures lists (order-sensitive).
func sigsEqual(a, b []propSig) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// genRounds returns the number of generation rounds: a small fixed set by
// default and a much larger set when ICAL_GEN_ROUNDS is set.
func genRounds() int {
	if v := genEnvInt("ICAL_GEN_ROUNDS"); v > 0 {
		return v
	}
	if genEnvSet("ICAL_GEN_EXTENDED") {
		return 4000
	}
	return 200
}

func genEnvSet(key string) bool {
	v, ok := genLookupEnv(key)
	return ok && v != "" && v != "0" && v != "false"
}
