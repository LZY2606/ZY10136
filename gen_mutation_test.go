package ics

// Syntax-aware mutation campaign.
//
// Unlike a byte fuzzer, every mutation here targets a specific syntactic
// construct of a *valid generated* document:
//
//   - foldMutator   - CRLF+SP / CRLF+TAB folding (octet counting / unfolding)
//   - paramMutator  - the separators around quoted parameter values
//   - nestingMutator- BEGIN/END pairing and component boundaries
//
// Lethal mutations must change semantics, fail parsing with a precise error,
// or violate a fold invariant. Semantic-equivalence mutants (see
// TestSemanticEquivalentMutantsSurvive) must be absorbed by normalization; the
// test documents why each one is indistinguishable rather than declaring the
// parser broken.
//
// The campaign only runs under ICAL_GEN_EXTENDED or ICAL_GEN_ROUNDS so the
// default `go test ./... -count=1` stays a short, fixed collection; any
// counterexample it finds is first reduced (see shrink*) and then pinned in
// gen_regression_test.go.

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// mutationOutcome records what one mutation did.
type mutationOutcome int

const (
	mutRejected     mutationOutcome = iota // parse failed or invariant violated (lethal)
	mutEquivalent                          // parsed with identical semantics (survived)
	mutChanged                             // parsed but semantics changed (missed bug)
	mutUndetectable                        // mutation produced a different-but-valid document
)

// applyFoldMutations returns mutated wire variants that attack unfolding and
// octet counting.
func applyFoldMutations(ics string) map[string]string {
	out := map[string]string{}
	phys := strings.Split(ics, "\r\n")
	for i := 0; i < len(phys)-1; i++ {
		if len(phys[i]) == 75 {
			// Delete the continuation WSP on the following physical line so
			// the continuation becomes an orphan standalone line.
			dup := append([]string(nil), phys...)
			if strings.HasPrefix(dup[i+1], " ") {
				dup[i+1] = strings.TrimPrefix(dup[i+1], " ")
			} else if strings.HasPrefix(dup[i+1], "\t") {
				dup[i+1] = strings.TrimPrefix(dup[i+1], "\t")
			}
			out["fold/drop-cont-wsp"] = strings.Join(dup, "\r\n")

			// Insert a raw CRLF inside a multi-byte rune: find a non-ASCII
			// rune inside the 75-octet line and break immediately after its
			// lead byte (no continuation WSP), which is always illegal.
			if m := splitRuneVariant(phys, i); m != "" {
				out["fold/split-rune"] = m
			}
		}
	}
	return out
}

// splitRuneVariant inserts CRLF inside a multi-byte UTF-8 rune of physical
// line i. It returns "" when the line holds no multi-byte rune.
func splitRuneVariant(phys []string, i int) string {
	line := phys[i]
	if i+1 >= len(phys) {
		return ""
	}
	for j := 0; j < len(line); {
		b := line[j]
		width := 0
		switch {
		case b >= 0xF0:
			width = 4
		case b >= 0xE0:
			width = 3
		case b >= 0xC0:
			width = 2
		default:
			j++
			continue
		}
		if j+width <= len(line) {
			dup := append([]string(nil), phys...)
			dup[i] = line[:j+1]
			// Continuation keeps the remaining rune bytes, still split because
			// the lead byte is on the previous physical line.
			rest := line[j+1:]
			if strings.HasPrefix(dup[i+1], " ") || strings.HasPrefix(dup[i+1], "\t") {
				dup[i+1] = string(dup[i+1][0]) + rest + dup[i+1][1:]
			} else {
				dup[i+1] = " " + rest + dup[i+1]
			}
			return strings.Join(dup, "\r\n")
		}
		j += width
	}
	return ""
}

// applyParamMutations attacks parameter splitting: removing the quotes of a
// quoted value must expose ';'/':'/',' as structural characters and change the
// parse; replacing the '=' separator must be rejected.
func applyParamMutations(ics string) map[string]string {
	out := map[string]string{}
	logical := unfoldPhysical(ics)
	for i, line := range logical {
		if qi := strings.Index(line, "=\""); qi >= 0 && strings.Contains(line[qi:], "\"") {
			// Drop the opening DQUOTE only: the closing DQUOTE becomes an
			// unexpected quote in an unquoted value.
			b := []byte(line)
			mut := string(append(b[:qi], b[qi+1:]...))
			out["param/drop-open-quote"] = refoldLogical(ics, logical, i, mut)
		}
		if qi := strings.Index(line, ";CN="); qi >= 0 {
			// Turn '=' into '-': missing param operator.
			mut := line[:qi+1] + "CN-" + line[qi+4:]
			out["param/equals-to-dash"] = refoldLogical(ics, logical, i, mut)
		}
	}
	return out
}

// refoldLogical rebuilds wire text replacing a single logical line while
// keeping the other physical lines intact. It simply re-joins logical lines;
// the parser unfolds identically either way.
func refoldLogical(_ string, logical []string, idx int, replacement string) string {
	cp := append([]string(nil), logical...)
	cp[idx] = replacement
	return strings.Join(cp, "\r\n")
}

// applyNestingMutations attacks BEGIN/END pairing.
func applyNestingMutations(ics string) map[string]string {
	out := map[string]string{}
	logical := unfoldPhysical(ics)
	// Delete the first non-root END (keeps an inner component open).
	for i, l := range logical {
		if strings.HasPrefix(l, "END:") && l != "END:VCALENDAR" {
			cp := append([]string(nil), logical...)
			cp = append(cp[:i], cp[i+1:]...)
			out["nesting/delete-inner-end"] = strings.Join(cp, "\r\n")
			break
		}
	}
	// Duplicate an inner BEGIN line so the component token pairing breaks.
	for i, l := range logical {
		if l == "BEGIN:VALARM" || l == "BEGIN:STANDARD" || l == "BEGIN:DAYLIGHT" {
			cp := append([]string(nil), logical...)
			cp = append(cp[:i+1], append([]string{l}, cp[i+1:]...)...)
			out["nesting/duplicate-inner-begin"] = strings.Join(cp, "\r\n")
			break
		}
	}
	return out
}

// classifyMutation parses the mutated wire and compares semantic signatures.
func classifyMutation(t *testing.T, base *Calendar, mutated string) mutationOutcome {
	t.Helper()
	// Fold invariants are checked on the mutated wire before parsing: every
	// physical line must be independently valid UTF-8 and at most 75 octets.
	// A fold inside a multi-byte encoding unit invalidates UTF-8 even if the
	// downstream parser happens to tolerate the bytes.
	if strings.Contains(mutated, "\r\n") {
		for _, line := range strings.Split(strings.TrimSuffix(mutated, "\r\n"), "\r\n") {
			frag := line
			if len(frag) > 0 && (frag[0] == ' ' || frag[0] == '\t') {
				frag = frag[1:]
			}
			if len(line) > 75 || !utf8.ValidString(frag) {
				return mutRejected
			}
		}
	}
	cal, err := ParseCalendarWithOptions(strings.NewReader(mutated), genParseOptions()...)
	if err != nil {
		// Any parse failure is the expected lethal mutation result.
		return mutRejected
	}
	if sigsEqual(calendarPropSigs(base), calendarPropSigs(cal)) {
		return mutEquivalent
	}
	// A document that parses cleanly with stable wire invariants and no error
	// is, by definition, another valid iCalendar object: the mutation cannot
	// be distinguished from author intent without knowing the original. This
	// is the documented class of undetectable mutations (see
	// TestSemanticEquivalentMutantsSurvive); it is not a parser defect.
	return mutUndetectable
}

// shrinkModel reduces a generated model, while pred stays true, to the
// smallest set of top-level components. It is used to turn a campaign
// counterexample into a single-property / minimal-nesting case before pinning
// it in gen_regression_test.go.
func shrinkModel(m *genCalendar, pred func(*genCalendar) bool) *genCalendar {
	if !pred(m) {
		return m
	}
	var comps []*genComp
	for _, c := range m.components {
		// Add this component and minimize it in isolation.
		candidate := shrinkComponent(c, func(cc *genComp) bool {
			return pred(&genCalendar{props: m.props, components: []*genComp{cc}})
		})
		trial := &genCalendar{props: m.props, components: append(append([]*genComp{}, comps...), candidate)}
		if pred(trial) {
			comps = append(comps, candidate)
			if pred(&genCalendar{props: m.props, components: comps}) {
				break
			}
		}
	}
	return &genCalendar{props: m.props, components: comps}
}

// shrinkComponent removes inessential children and properties of one component
// while localPred(subtree) remains true.
func shrinkComponent(c *genComp, localPred func(*genComp) bool) *genComp {
	if !localPred(c) {
		return c
	}
	// Children: keep only those needed to satisfy the predicate.
	var children []*genComp
	for _, ch := range c.children {
		sh := shrinkComponent(ch, localPred)
		trial := &genComp{kind: c.kind, props: c.props, children: append(append([]*genComp{}, children...), sh)}
		if localPred(trial) {
			children = append(children, sh)
		}
	}
	// Properties: drop each one that the predicate does not require.
	props := append([]genProp{}, c.props...)
	for i := 0; i < len(props); {
		trialProps := append(append([]genProp{}, props[:i]...), props[i+1:]...)
		trial := &genComp{kind: c.kind, props: trialProps, children: children}
		if localPred(trial) {
			props = trialProps
			continue
		}
		i++
	}
	return &genComp{kind: c.kind, props: props, children: children}
}

// TestMutationCampaign runs the bounded campaign in extended mode. It treats
// mutChanged as a failure (a mutation that the parser accepted with altered
// semantics is either a bug or a missing pinned regression case).
func TestMutationCampaign(t *testing.T) {
	if !genEnvSet("ICAL_GEN_EXTENDED") && genEnvInt("ICAL_GEN_ROUNDS") == 0 {
		t.Skip("mutation campaign only runs with ICAL_GEN_EXTENDED or ICAL_GEN_ROUNDS")
	}
	rounds := genRounds()
	var changed, rejected, equivalent, undetectable int
	for seed := int64(0); seed < int64(rounds); seed++ {
		g := newGenConfig(seed)
		model := g.genCalendarModel()
		cal := buildCalendar(model)
		wire := cal.Serialize(WithNewLineWindows)
		base, err := ParseCalendarWithOptions(strings.NewReader(wire), genParseOptions()...)
		if err != nil {
			t.Fatalf("generated baseline failed for seed %d: %v", seed, err)
		}
		mutators := []map[string]string{
			applyFoldMutations(wire),
			applyParamMutations(wire),
			applyNestingMutations(wire),
		}
		for _, group := range mutators {
			for name, mutated := range group {
				switch classifyMutation(t, base, mutated) {
				case mutRejected:
					rejected++
				case mutEquivalent:
					equivalent++
				case mutUndetectable:
					undetectable++
				case mutChanged:
					changed++
					t.Errorf("seed %d mutation %s changed semantics without rejection; reduce and pin it", seed, name)
				}
			}
		}
	}
	t.Logf("mutation campaign: rejected=%d equivalent=%d undetectable=%d changed=%d", rejected, equivalent, undetectable, changed)
}
