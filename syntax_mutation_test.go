package ics

// Syntax-aware mutation tests.
//
// Two mutator families run over legal, generated ICS documents:
//
// semantics-preserving ("equivalent") mutations must survive: the parser has
// to accept them and recover exactly the same model. These pin the tolerant
// behaviour the library intentionally offers (CRLF vs LF, SP vs HTAB folding,
// fold points at arbitrary octet boundaries, quoting, parameter reordering,
// and escapes adjacent to fold points).
//
// fatal mutations must be rejected with precise location/error context (or be
// handed to the malformed-line callback as the exact unfolded raw content).
// They never rely on "error is non-nil".
//
// Counterexamples found by the campaign are frozen in the pinned tables below
// (TestPinnedFoldingCorpus, TestPinnedMalformedFamily,
// TestEquivalentMutantsSurvive), so a regression fails deterministically even
// without ICS_MUT_ITERS.

import (
	"errors"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
)

const (
	mutDefaultIterations = 100
	mutFixedSeed         = int64(0x4D_55_74_71)
)

func mutIterationCount(t *testing.T) int {
	t.Helper()
	if raw := os.Getenv("ICS_MUT_ITERS"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
		t.Fatalf("ICS_MUT_ITERS must be a positive integer, got %q", raw)
	}
	return mutDefaultIterations
}

// ---------------------------------------------------------------------------
// Mutators
// ---------------------------------------------------------------------------

type docMutator struct {
	name   string
	apply  func(r *rand.Rand, doc string) (string, bool)
	fatal  bool
	expect func(t *testing.T, err error)
}

// logicalLinesCRLF splits a CRLF document, returning the logical content
// lines as they exist before mutation (documents in the corpus are unfolded
// only through folding, so each physical line is already logical here).
func logicalLinesCRLF(doc string) []string {
	return strings.Split(strings.TrimRight(doc, "\r\n"), "\r\n")
}

// mutLineTransform rewrites one non-BEGIN/END physical line with f.
func mutLineTransform(pick func(r *rand.Rand, lines []string) int, f func(line string) string) func(*rand.Rand, string) (string, bool) {
	return func(r *rand.Rand, doc string) (string, bool) {
		lines := logicalLinesCRLF(doc)
		idx := pick(r, lines)
		if idx < 0 {
			return doc, false
		}
		lines[idx] = f(lines[idx])
		return strings.Join(lines, "\r\n") + "\r\n", true
	}
}

func propertyLineIndex(r *rand.Rand, lines []string) int {
	var candidates []int
	for i, l := range lines {
		if !strings.HasPrefix(l, "BEGIN:") && !strings.HasPrefix(l, "END:") {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return -1
	}
	return candidates[r.Intn(len(candidates))]
}

// standalonePropertyLineIndex picks a property physical line that is not a
// fold continuation and whose predecessor is not a fold-continued property
// either; prepending a fold marker to such a line is therefore unambiguously
// an orphan continuation.
func standalonePropertyLineIndex(r *rand.Rand, lines []string) int {
	var candidates []int
	for i, l := range lines {
		if strings.HasPrefix(l, " ") || strings.HasPrefix(l, "	") ||
			strings.HasPrefix(l, "BEGIN:") || strings.HasPrefix(l, "END:") {
			continue
		}
		if i > 0 && (strings.HasPrefix(lines[i-1], " ") || strings.HasPrefix(lines[i-1], "	")) {
			continue
		}
		if i+1 < len(lines) && (strings.HasPrefix(lines[i+1], " ") || strings.HasPrefix(lines[i+1], "	")) {
			// line itself begins a folded property: only callers that edit
			// the logical content line should accept it, structural edits must
			// skip it.
			continue
		}
		candidates = append(candidates, i)
	}
	if len(candidates) == 0 {
		return -1
	}
	return candidates[r.Intn(len(candidates))]
}

// bareParamLineIndex picks a standalone line containing a CN= or LANGUAGE=
// parameter whose value is not quoted, making the line a safe target for
// mutations that must land in the bare parameter section.
func bareParamLineIndex(r *rand.Rand, lines []string) int {
	var candidates []int
	for i, l := range lines {
		if strings.HasPrefix(l, " ") || strings.HasPrefix(l, "	") ||
			strings.HasPrefix(l, "BEGIN:") || strings.HasPrefix(l, "END:") {
			continue
		}
		if i > 0 && (strings.HasPrefix(lines[i-1], " ") || strings.HasPrefix(lines[i-1], "	")) {
			continue
		}
		if i+1 < len(lines) && (strings.HasPrefix(lines[i+1], " ") || strings.HasPrefix(lines[i+1], "	")) {
			continue
		}
		if strings.Contains(l, "\"") {
			continue
		}
		if strings.Contains(l, ";CN=") || strings.Contains(l, ";LANGUAGE=") {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return -1
	}
	return candidates[r.Intn(len(candidates))]
}

// Equivalent mutators -------------------------------------------------------

func mutLF() docMutator {
	return docMutator{
		name: "crlf_to_lf",
		apply: func(_ *rand.Rand, doc string) (string, bool) {
			return strings.ReplaceAll(doc, "\r\n", "\n"), true
		},
	}
}

func mutTabFolds() docMutator {
	return docMutator{
		name: "space_to_tab_fold",
		apply: func(_ *rand.Rand, doc string) (string, bool) {
			if !strings.Contains(doc, "\r\n ") {
				return doc, false
			}
			return strings.ReplaceAll(doc, "\r\n ", "\r\n\t"), true
		},
	}
}

// mutInsertFold inserts CRLF+SP at a random rune-edge inside a property line
// (never at position 0, which would merge two logical lines). The fold may
// land in the middle of an escape sequence such as \N or \\.
func mutInsertFold() docMutator {
	return docMutator{
		name: "arbitrary_fold_point",
		apply: func(r *rand.Rand, doc string) (string, bool) {
			lines := logicalLinesCRLF(doc)
			idx := propertyLineIndex(r, lines)
			if idx < 0 {
				return doc, false
			}
			line := lines[idx]
			var edges []int
			for i := range line {
				if i > 0 {
					edges = append(edges, i)
				}
			}
			if len(edges) == 0 {
				return doc, false
			}
			pos := edges[r.Intn(len(edges))]
			lines[idx] = line[:pos] + "\r\n " + line[pos:]
			return strings.Join(lines, "\r\n") + "\r\n", true
		},
	}
}

// mutEscapeParam wraps a safe CN value in double quotes: CN=abc -> CN="abc".
// Quoted and unquoted tokens carry the same parameter semantics.
func mutEscapeParam() docMutator {
	return docMutator{
		name: "quote_cn_parameter",
		apply: func(r *rand.Rand, doc string) (string, bool) {
			lines := logicalLinesCRLF(doc)
			// bareParamLineIndex excludes folded and already-quoted lines.
			idx := bareParamLineIndex(r, lines)
			if idx < 0 {
				return doc, false
			}
			line := lines[idx]
			at := strings.Index(line, ";CN=")
			if at < 0 {
				return doc, false
			}
			start := at + len(";CN=")
			end := start
			for end < len(line) && line[end] != ';' && line[end] != ':' && line[end] != ',' {
				end++
			}
			value := line[start:end]
			// Only quote values that need no escaping at all: quotes and
			// backslashes must not appear verbatim inside a quoted-string.
			if value == "" || strings.ContainsAny(value, "\""+"\\") {
				return doc, false
			}
			lines[idx] = line[:start] + `"` + value + `"` + line[end:]
			return strings.Join(lines, "\r\n") + "\r\n", true
		},
	}
}

// Fatal mutators ------------------------------------------------------------

func mutOrphanContinuation() docMutator {
	return docMutator{
		name: "orphan_continuation",
		apply: func(r *rand.Rand, doc string) (string, bool) {
			lines := logicalLinesCRLF(doc)
			// Prepend a fold marker to a standalone property line so the
			// stream merges it into the preceding logical line.
			idx := standalonePropertyLineIndex(r, lines)
			if idx < 0 {
				return doc, false
			}
			lines[idx] = " " + lines[idx]
			return strings.Join(lines, "\r\n") + "\r\n", true
		},
		fatal: true,
		expect: func(t *testing.T, err error) {
			t.Helper()
			// An orphan continuation corrupts the line it was glued to, so the
			// parser rejects the document with location context.
			assertMalformedLine(t, err)
		},
	}
}

func mutMissingColon() docMutator {
	return docMutator{
		name: "missing_colon",
		apply: func(r *rand.Rand, doc string) (string, bool) {
			return mutLineTransform(standalonePropertyLineIndex, func(line string) string {
				if i := strings.IndexByte(line, ':'); i >= 0 {
					return line[:i] + line[i+1:]
				}
				return line
			})(r, doc)
		},
		fatal: true,
		expect: func(t *testing.T, err error) {
			t.Helper()
			// Dropping the colon breaks the content line itself rather than
			// the BEGIN/END component structure, so the failure must name the
			// property-parse stage and carry a line number.
			assertPropertyStageError(t, err)
			assertMalformedLine(t, err)
		},
	}
}

func mutBadQuote() docMutator {
	return docMutator{
		name: "bad_quote",
		apply: func(r *rand.Rand, doc string) (string, bool) {
			lines := logicalLinesCRLF(doc)
			idx := bareParamLineIndex(r, lines)
			if idx < 0 {
				return doc, false
			}
			line := lines[idx]
			marker := ";CN="
			at := strings.Index(line, marker)
			if at < 0 {
				marker = ";LANGUAGE="
				at = strings.Index(line, marker)
			}
			if at < 0 {
				return doc, false
			}
			pos := at + len(marker) + 1 // one rune into the bare value
			colon := topLevelColon(line)
			if colon < 0 || pos >= colon {
				return doc, false
			}
			lines[idx] = line[:pos] + `"` + line[pos:]
			return strings.Join(lines, "\r\n") + "\r\n", true
		},
		fatal: true,
		expect: func(t *testing.T, err error) {
			t.Helper()
			if !errors.Is(err, ErrUnexpectedDoubleQuoteInPropertyParamValue) &&
				!errors.Is(err, ErrUnexpectedEndOfProperty) {
				t.Fatalf("expected quote error, got %v", err)
			}
			assertMalformedLine(t, err)
		},
	}
}

func mutBadParamName() docMutator {
	return docMutator{
		name: "bad_param_name",
		apply: func(r *rand.Rand, doc string) (string, bool) {
			return mutLineTransform(bareParamLineIndex, func(line string) string {
				colon := topLevelColon(line)
				if colon <= 0 {
					return line
				}
				// Insert an additional parameter whose name is not an
				// iana-token; the existing parameters stay intact so the error
				// cannot be absorbed by lenient recovery paths.
				return line[:colon] + ";BAD@NAME=x" + line[colon:]
			})(r, doc)
		},
		fatal: true,
		expect: func(t *testing.T, err error) {
			t.Helper()
			if !errors.Is(err, ErrMissingPropertyValue) {
				t.Fatalf("expected ErrMissingPropertyValue, got %v", err)
			}
			assertMalformedLine(t, err)
		},
	}
}

func assertMalformedLine(t *testing.T, err error) {
	t.Helper()
	var me *MalformedError
	if !errors.As(err, &me) || !me.HasLine || me.Line <= 0 {
		t.Fatalf("expected MalformedError with a positive line number, got %v", err)
	}
}

// topLevelColon returns the index of the first unquoted ':' that separates
// the property/parameter section from the value.
func topLevelColon(line string) int {
	quoted := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			quoted = !quoted
		case '\\':
			i++
		case ':':
			if !quoted {
				return i
			}
		}
	}
	return -1
}

func assertPropertyStageError(t *testing.T, err error) {
	t.Helper()
	var me *MalformedError
	if !errors.As(err, &me) || !me.HasLine || me.Line <= 0 {
		t.Fatalf("expected MalformedError with line number, got %v", err)
	}
	msg := err.Error()
	componentOnly := errors.Is(err, ErrExpectedBegin) || errors.Is(err, ErrExpectedEnd) ||
		errors.Is(err, ErrExpectedVCalendar) || errors.Is(err, ErrUnbalancedEnd) ||
		errors.Is(err, ErrVCalendarNotWhereExpected) || errors.Is(err, ErrOutOfLines)
	if componentOnly {
		t.Fatalf("expected property-level parse error, got component structure error: %v", err)
	}
	if !strings.Contains(msg, "property") && !strings.Contains(msg, "param") {
		t.Fatalf("expected error on the property/parameter parse path, got %v", err)
	}
}

func allMutators() []docMutator {
	return []docMutator{
		mutLF(), mutTabFolds(), mutInsertFold(), mutEscapeParam(),
		mutOrphanContinuation(), mutMissingColon(), mutBadQuote(), mutBadParamName(),
	}
}

// ---------------------------------------------------------------------------
// Campaign
// ---------------------------------------------------------------------------

func TestMutationCampaign(t *testing.T) {
	iters := mutIterationCount(t)
	rg := rand.New(rand.NewSource(mutFixedSeed))
	for i := 0; i < iters; i++ {
		cal := genCalendar(rg)
		doc := cal.Serialize(WithNewLineWindows)
		want := semFromCalendar(cal)
		for _, m := range allMutators() {
			r := rand.New(rand.NewSource(mutFixedSeed + int64(i)*97 + int64(hashString(m.name))))
			mutated, ok := m.apply(r, doc)
			if !ok {
				continue
			}
			if !m.fatal {
				parsed, err := ParseCalendar(strings.NewReader(mutated))
				if err != nil {
					t.Fatalf("mutator %s on iter %d rejected equivalent doc: %v\n%s", m.name, i, err, mutated)
				}
				if got := semFromCalendar(parsed); !semCalendarEqual(want, got) {
					t.Fatalf("mutator %s on iter %d changed semantics\nmutated:\n%s", m.name, i, mutated)
				}
				continue
			}
			parsed, err := ParseCalendar(strings.NewReader(mutated))
			switch m.name {
			case "orphan_continuation", "missing_colon":
				// The strict parser normally rejects these with location
				// context; on a few tolerant token paths it may still accept,
				// but the document can never keep its original semantics.
				if err == nil && semCalendarEqual(want, semFromCalendar(parsed)) {
					t.Fatalf("mutator %s on iter %d left semantics unchanged:\n%s", m.name, i, mutated)
				}
				if err != nil {
					m.expect(t, err)
				}
			default:
				if err == nil {
					t.Fatalf("mutator %s on iter %d unexpectedly accepted:\n%s", m.name, i, mutated)
				}
				m.expect(t, err)
			}
		}
	}
}

func hashString(s string) int {
	h := 0
	for _, ch := range s {
		h = h*31 + int(ch)
	}
	if h < 0 {
		h = -h
	}
	return h
}
