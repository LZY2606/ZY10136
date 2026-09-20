package ics

// Deterministic test for the counterexample minimizer used by the extended
// mutation campaign. It proves that a failing model (e.g. one carrying a given
// property token) can be reduced to a single component with a single property,
// i.e. the single-property / minimal-nesting form the pinned tests use.

import "testing"

func TestShrinkModelReducesToSingleProperty(t *testing.T) {
	for seed := int64(0); seed < 100; seed++ {
		g := newGenConfig(seed)
		model := g.genCalendarModel()
		pred := func(c *genCalendar) bool {
			var walk func(*genComp) bool
			walk = func(cc *genComp) bool {
				for _, p := range cc.props {
					if p.token == "X-DUP" {
						return true
					}
				}
				for _, ch := range cc.children {
					if walk(ch) {
						return true
					}
				}
				return false
			}
			for _, cc := range c.components {
				if walk(cc) {
					return true
				}
			}
			return false
		}
		shrunk := shrinkModel(model, pred)
		if !pred(shrunk) {
			t.Fatalf("seed %d: shrunk model no longer satisfies predicate", seed)
		}
		if len(shrunk.components) != 1 {
			t.Fatalf("seed %d: expected 1 component after shrink, got %d", seed, len(shrunk.components))
		}
		// Exactly one X-DUP remains anywhere in the minimal tree and every
		// retained component along the path carries no superfluous property.
		triggers, totalProps, maxDepth := 0, 0, 0
		var walk func(*genComp, int)
		walk = func(c *genComp, depth int) {
			if depth > maxDepth {
				maxDepth = depth
			}
			for _, p := range c.props {
				totalProps++
				if p.token == "X-DUP" {
					triggers++
				}
			}
			for _, ch := range c.children {
				walk(ch, depth+1)
			}
		}
		walk(shrunk.components[0], 1)
		if triggers != 1 || totalProps != 1 {
			t.Fatalf("seed %d: expected exactly one total property (the trigger), got %d triggers / %d props",
				seed, triggers, totalProps)
		}
		if maxDepth > genMaxDepth {
			t.Fatalf("seed %d: shrunk depth %d exceeds bound", seed, maxDepth)
		}
	}
}

func TestShrinkModelPreservesNonPredicateInput(t *testing.T) {
	g := newGenConfig(1)
	model := g.genCalendarModel()
	never := func(*genCalendar) bool { return false }
	if got := shrinkModel(model, never); got != model {
		t.Fatalf("shrink should return the original model when predicate never holds")
	}
}
