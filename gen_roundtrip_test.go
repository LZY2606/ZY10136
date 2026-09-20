package ics

// Syntax-aware generate -> serialize -> parse -> serialize -> parse tests.
//
// Every round builds a constrained semantic model, encodes it through the
// production serializer, and verifies:
//
//   - the folded wire form obeys RFC 5545 line length limits measured in UTF-8
//     octets and never splits a rune;
//   - CRLF folding with a space and folding with a tab unfold identically;
//   - component order, repeated properties, multi-valued parameters and TEXT
//     unescaping are preserved semantically through two parse cycles;
//   - the second serialization is byte-identical to the first (normalization
//     stability) and unknown X- properties are never dropped;
//   - DATE, local DATE-TIME, UTC and TZID time values map to the expected
//     time.Time shape regardless of the host location.

import (
	"strings"
	"testing"
	"time"
	_ "time/tzdata"
	"unicode/utf8"
)

// genParseOptions maps the generated TZID to a fixed, DST-free location so time
// resolution neither touches the host zone database nor the network.
func genParseOptions() []any {
	zone := time.FixedZone("Generated/Zone_2021", 5*3600+30*60)
	mapper := TimezoneMapper(func(tzid string) *time.Location {
		if tzid == genGeneratedTZID {
			return zone
		}
		if loc, err := time.LoadLocation(tzid); err == nil {
			return loc
		}
		return nil
	})
	return []any{mapper}
}

// validateFoldedWire checks the physical invariants of a folded ICS document:
// every physical line is at most 75 octets, CRLF terminates each line, folds
// start with SP or HTAB, and every physical fragment is valid UTF-8 (no fold
// boundary inside an encoding unit).
func validateFoldedWire(t *testing.T, ics string) {
	t.Helper()
	if !strings.HasSuffix(ics, "\r\n") {
		t.Fatalf("document does not end with CRLF: %q", tail(ics))
	}
	if !utf8.ValidString(ics) {
		t.Fatalf("folded document is not valid UTF-8")
	}
	lines := strings.Split(strings.TrimSuffix(ics, "\r\n"), "\r\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least BEGIN/END lines")
	}
	for i, line := range lines {
		if n := len(line); n > 75 {
			t.Fatalf("physical line %d is %d octets (>75): %q", i+1, n, line)
		}
		if strings.Contains(line, "\n") || strings.Contains(line, "\r") {
			t.Fatalf("physical line %d contains a bare CR/LF", i+1)
		}
		if i > 0 && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
			if len(line) < 2 {
				t.Fatalf("line %d is a bare continuation", i+1)
			}
		}
	}
}

func tail(s string) string {
	if len(s) > 40 {
		return s[len(s)-40:]
	}
	return s
}

// unfoldPhysical mimics RFC 5545 unfolding and returns the logical lines.
func unfoldPhysical(ics string) []string {
	lines := strings.Split(strings.TrimSuffix(ics, "\r\n"), "\r\n")
	logical := []string{}
	for _, l := range lines {
		if (strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t")) && len(logical) > 0 {
			logical[len(logical)-1] += l[1:]
		} else {
			logical = append(logical, l)
		}
	}
	return logical
}

func TestGeneratedRoundTrip(t *testing.T) {
	rounds := genRounds()
	if testing.Short() {
		rounds = 25
	}
	for seed := int64(0); seed < int64(rounds); seed++ {
		seed := seed
		t.Run("", func(t *testing.T) {
			g := newGenConfig(seed)
			model := g.genCalendarModel()
			cal := buildCalendar(model)
			wire := cal.Serialize(WithNewLineWindows)
			validateFoldedWire(t, wire)

			// Unfold space- and tab-folded variants: they must produce the
			// exact same logical content lines.
			tabVariant := strings.ReplaceAll(wire, "\r\n ", "\r\n\t")
			assertUnfoldEquivalence(t, wire, tabVariant)

			cal1, err := ParseCalendarWithOptions(strings.NewReader(wire), genParseOptions()...)
			if err != nil {
				t.Fatalf("first parse failed for seed %d:\n%s\nerr: %v", seed, wire, err)
			}
			wire1 := cal1.Serialize(WithNewLineWindows)
			validateFoldedWire(t, wire1)
			cal2, err := ParseCalendarWithOptions(strings.NewReader(wire1), genParseOptions()...)
			if err != nil {
				t.Fatalf("second parse failed for seed %d:\n%s\nerr: %v", seed, wire1, err)
			}

			// Normalization stability: re-serialization must be byte stable.
			wire2 := cal2.Serialize(WithNewLineWindows)
			if wire1 != wire2 {
				t.Fatalf("serialization not stable for seed %d\n--- first ---\n%s\n--- second ---\n%s", seed, wire1, wire2)
			}

			// Semantic preservation of the original model.
			want := modelPropSigs(model)
			got := calendarPropSigs(cal2)
			if !sigsEqual(want, got) {
				t.Fatalf("semantic mismatch for seed %d\nwant: %v\ngot:  %v\nwire:\n%s", seed, want, got, wire)
			}

			// Unknown X- properties (including duplicates) must survive.
			assertXPropsPreserved(t, model, cal2)

			// Time shape oracle on every time property in the model.
			assertTimeShapes(t, cal2)
		})
	}
}

func assertUnfoldEquivalence(t *testing.T, spaceWire, tabWire string) {
	t.Helper()
	a := unfoldPhysical(spaceWire)
	b := unfoldPhysical(tabWire)
	if len(a) != len(b) {
		t.Fatalf("fold variants unfold to %d vs %d logical lines", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("logical line %d differs between SP and HTAB folds:\n%q\n%q", i, a[i], b[i])
		}
	}
	// And the parsed values must match as well.
	ca, err := ParseCalendarWithOptions(strings.NewReader(spaceWire), genParseOptions()...)
	if err != nil {
		t.Fatalf("space-fold parse: %v", err)
	}
	cb, err := ParseCalendarWithOptions(strings.NewReader(tabWire), genParseOptions()...)
	if err != nil {
		t.Fatalf("tab-fold parse: %v", err)
	}
	if !sigsEqual(calendarPropSigs(ca), calendarPropSigs(cb)) {
		t.Fatalf("SP and HTAB folded input parsed differently")
	}
}

func assertXPropsPreserved(t *testing.T, m *genCalendar, cal *Calendar) {
	t.Helper()
	got := map[string]int{}
	for _, s := range calendarPropSigs(cal) {
		tok := s.token[strings.Index(s.token, "/")+1:]
		if strings.HasPrefix(tok, "X-") {
			got[tok]++
		}
	}
	want := map[string]int{}
	var walk func(c *genComp)
	count := func(tok string) {
		if strings.HasPrefix(tok, "X-") {
			want[tok]++
		}
	}
	for _, p := range m.props {
		count(p.token)
	}
	walk = func(c *genComp) {
		for _, p := range c.props {
			count(p.token)
		}
		for _, ch := range c.children {
			walk(ch)
		}
	}
	for _, c := range m.components {
		walk(c)
	}
	for tok, n := range want {
		if got[tok] != n {
			t.Fatalf("X-property %s present %d times in model but %d after round-trip", tok, n, got[tok])
		}
	}
}

// assertTimeShapes walks every parsed component and checks the four canonical
// time shapes independently of the machine's local zone.
func assertTimeShapes(t *testing.T, cal *Calendar) {
	t.Helper()
	zone := time.FixedZone("Generated/Zone_2021", 5*3600+30*60)
	var check func(cb *ComponentBase, path string)
	event := func(cb *ComponentBase) *ComponentBase { return cb }
	_ = event
	check = func(cb *ComponentBase, path string) {
		for i := range cb.Properties {
			p := &cb.Properties[i]
			if p.IANAToken != "DTSTART" && p.IANAToken != "DTEND" &&
				p.IANAToken != "EXDATE" && p.IANAToken != "RDATE" {
				continue
			}
			values := []string{p.Value}
			got, err := parseTimeValue(values[0], p.ICalParameters, false, genParseOptions()...)
			isDate := false
			if vs, ok := p.ICalParameters["VALUE"]; ok && len(vs) == 1 && vs[0] == "DATE" {
				isDate = true
			}
			switch {
			case strings.HasSuffix(p.Value, "Z"):
				if err != nil || got.Location() != time.UTC {
					t.Fatalf("%s %s: expected UTC location, got %v (%v)", path, p.Value, got.Location(), err)
				}
				assertWall(t, path, got, 2021, 6, 27, 12, 30, 45)
			case isDate:
				if err != nil || got.Hour() != 0 || got.Minute() != 0 || got.Second() != 0 {
					t.Fatalf("%s %s: expected DATE midnight, got %v (%v)", path, p.Value, got, err)
				}
				y, m, d := got.Date()
				if y != 2021 || m != 6 || d != 27 {
					t.Fatalf("%s %s: expected 2021-06-27, got %04d-%02d-%02d", path, p.Value, y, m, d)
				}
			default:
				if err != nil {
					t.Fatalf("%s %s: parse error %v", path, p.Value, err)
				}
				if tzids, ok := p.ICalParameters["TZID"]; ok {
					if len(tzids) != 1 || tzids[0] != genGeneratedTZID {
						t.Fatalf("%s: unexpected TZID %v", path, tzids)
					}
					if got.Location().String() != genGeneratedTZID {
						t.Fatalf("%s: expected mapped fixed zone, got %q", path, got.Location().String())
					}
					_, zoff := got.Zone()
					if zoff != 5*3600+30*60 {
						t.Fatalf("%s: expected +05:30 offset, got %d", path, zoff)
					}
					_ = zone
					assertWall(t, path, got, 2021, 6, 27, 21, 30, 0)
				} else {
					// Floating local time: compare wall clock only; never depend on zone.
					assertWall(t, path, got, 2021, 6, 27, 21, 30, 0)
				}
			}
		}
		for _, sub := range cb.Components {
			if s, ok := sub.(*VEvent); ok {
				check(&s.ComponentBase, path+"/VEVENT")
			}
			if s, ok := sub.(*VTodo); ok {
				check(&s.ComponentBase, path+"/VTODO")
			}
			if s, ok := sub.(*VTimezone); ok {
				check(&s.ComponentBase, path+"/VTIMEZONE")
			}
			if s, ok := sub.(*VAlarm); ok {
				check(&s.ComponentBase, path+"/VALARM")
			}
			if s, ok := sub.(*Standard); ok {
				check(&s.ComponentBase, path+"/STANDARD")
			}
			if s, ok := sub.(*Daylight); ok {
				check(&s.ComponentBase, path+"/DAYLIGHT")
			}
		}
	}
	for _, c := range cal.Components {
		switch cc := c.(type) {
		case *VEvent:
			check(&cc.ComponentBase, "VEVENT")
		case *VTodo:
			check(&cc.ComponentBase, "VTODO")
		case *VTimezone:
			check(&cc.ComponentBase, "VTIMEZONE")
		}
	}
}

func assertWall(t *testing.T, path string, got time.Time, y int, mo time.Month, d, h, mi, s int) {
	t.Helper()
	gy, gm, gd := got.Date()
	if gy != y || gm != mo || gd != d || got.Hour() != h || got.Minute() != mi || got.Second() != s {
		t.Fatalf("%s: wall clock mismatch, want %04d-%02d-%02d %02d:%02d:%02d got %04d-%02d-%02d %02d:%02d:%02d",
			path, y, mo, d, h, mi, s, gy, gm, gd, got.Hour(), got.Minute(), got.Second())
	}
}
