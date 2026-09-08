package main

import (
	"strings"
	"testing"
)

// The fixtures below are the figures of the source document, so the
// implementation is checked against the model's own worked examples rather than
// against a reading of its prose.
//
// SIG/TUViT Evaluation Criteria Trusted Product Maintainability, Guidance for
// Producers, v17.0 (2025-03-12), section 3.7.

func edge(from, to string, weight int) ComponentEdge {
	return ComponentEdge{From: from, To: to, Weight: weight}
}

// violationsOf indexes the measured violations by the line they claim.
func violationsOf(t *testing.T, p *EntanglementProfile) map[string]EntanglementViolation {
	t.Helper()
	if p == nil {
		t.Fatal("measureEntanglement returned nothing for a non-empty graph")
	}
	out := map[string]EntanglementViolation{}
	for _, v := range p.Violations {
		out[v.Edge.String()] = v
	}
	return out
}

// TestDirectCycleWeighsTheLightestLine is Figure 9: A and B depend on each
// other, with lines of weight 1 and 10, and the violation weighs 1 because
// removing that line is what breaks the cycle for the least effort.
func TestDirectCycleWeighsTheLightestLine(t *testing.T) {
	p := measureEntanglement([]ComponentEdge{edge("A", "B", 1), edge("B", "A", 10)})

	if len(p.Violations) != 1 {
		t.Fatalf("got %d violations, want 1: %+v", len(p.Violations), p.Violations)
	}
	v := p.Violations[0]
	if v.Kind != violationCyclic {
		t.Errorf("kind = %q, want %q", v.Kind, violationCyclic)
	}
	if v.Weight != 1 {
		t.Errorf("weight = %d, want 1: the lightest line involved", v.Weight)
	}
	if v.Edge.String() != "A -> B" {
		t.Errorf("violation claims %s, want the lighter line A -> B", v.Edge)
	}
	// 1 of 11 total weight.
	if got := p.ViolationDegree; got < 0.09 || got > 0.10 {
		t.Errorf("violation degree = %.4f, want 1/11", got)
	}
}

// TestEqualWeightedCycleClaimsBothLines holds the rule the model states for the
// indirect pass: when both lines of a cycle weigh the same, both are removed,
// because either could be the unintended one.
func TestEqualWeightedCycleClaimsBothLines(t *testing.T) {
	// A and B are cyclic at weight 4 each. A -> C -> B also exists, so if the
	// A -> B line were left live it could be claimed a second time as a
	// transitive dependency.
	p := measureEntanglement([]ComponentEdge{
		edge("A", "B", 4), edge("B", "A", 4),
		edge("A", "C", 9), edge("C", "B", 9),
	})

	if len(p.Violations) != 1 {
		t.Fatalf("got %d violations, want 1: %+v", len(p.Violations), p.Violations)
	}
	if p.Violations[0].Weight != 4 {
		t.Errorf("weight = %d, want 4", p.Violations[0].Weight)
	}
	if p.Violations[0].Kind != violationCyclic {
		t.Errorf("kind = %q, want %q", p.Violations[0].Kind, violationCyclic)
	}
}

// TestIndirectCycleWeighsItsLightestLine is Figure 10: A -> B -> C -> D -> A,
// with no two components directly cyclic, weighted 10, 15, 3 and 8. The
// violation weighs 3.
func TestIndirectCycleWeighsItsLightestLine(t *testing.T) {
	p := measureEntanglement([]ComponentEdge{
		edge("A", "B", 10), edge("B", "C", 15), edge("C", "D", 3), edge("D", "A", 8),
	})

	if len(p.Violations) != 1 {
		t.Fatalf("got %d violations, want 1: %+v", len(p.Violations), p.Violations)
	}
	v := p.Violations[0]
	if v.Kind != violationIndirectCyclic {
		t.Errorf("kind = %q, want %q", v.Kind, violationIndirectCyclic)
	}
	if v.Weight != 3 || v.Edge.String() != "C -> D" {
		t.Errorf("violation = %s at weight %d, want C -> D at 3", v.Edge, v.Weight)
	}
}

// TestTransitiveDependencyNeedsAllThreeCriteria is Figures 11 and 12. B -> D
// bypasses B -> C -> D and is lighter than every line on that path, so it
// violates. B -> E does not, because E has no outgoing line of its own: a
// component nothing depends on from within is a library, and depending on a
// library directly is what libraries are for.
func TestTransitiveDependencyNeedsAllThreeCriteria(t *testing.T) {
	p := measureEntanglement([]ComponentEdge{
		edge("A", "B", 10),
		edge("B", "C", 18), edge("C", "D", 14),
		edge("B", "D", 1),
		edge("D", "E", 5),
		edge("B", "E", 2),
	})

	byLine := violationsOf(t, p)
	v, ok := byLine["B -> D"]
	if !ok {
		t.Fatalf("B -> D is transitive and lighter than the path it bypasses; violations: %+v", p.Violations)
	}
	if v.Kind != violationTransitive {
		t.Errorf("kind = %q, want %q", v.Kind, violationTransitive)
	}
	if v.Weight != 1 {
		t.Errorf("weight = %d, want 1: a transitive line weighs its own weight", v.Weight)
	}
	if _, bad := byLine["B -> E"]; bad {
		t.Error("B -> E must not violate: E has no outgoing line, so it is a leaf")
	}
}

// TestTransitiveNeedsToBeTheLightestPath is the exception Figure 12 draws: a
// direct line heavier than the path it parallels is not bypassing the normal
// flow, so it is not a violation.
func TestTransitiveNeedsToBeTheLightestPath(t *testing.T) {
	p := measureEntanglement([]ComponentEdge{
		edge("B", "C", 4), edge("C", "D", 4), edge("D", "E", 9),
		// Heavier than the lightest line on B -> C -> D, so it is the normal
		// flow rather than a bypass of it.
		edge("B", "D", 20),
	})

	if byLine := violationsOf(t, p); len(byLine) != 0 {
		t.Errorf("got violations %+v, want none: the direct line is heavier than the path it parallels",
			p.Violations)
	}
}

// TestALineIsClaimedByOnlyOneViolationType holds the model's ordering. A line
// that is part of a cycle is not counted a second time as a transitive
// dependency, or its weight would land in the violation degree twice.
func TestALineIsClaimedByOnlyOneViolationType(t *testing.T) {
	// A -> B is the light half of a cycle with B -> A, and it also parallels
	// A -> C -> B. Only the cycle may claim it.
	p := measureEntanglement([]ComponentEdge{
		edge("A", "B", 2), edge("B", "A", 30),
		edge("A", "C", 9), edge("C", "B", 9),
	})

	seen := map[string]int{}
	for _, v := range p.Violations {
		seen[v.Edge.String()]++
	}
	if seen["A -> B"] != 1 {
		t.Errorf("A -> B claimed %d times, want exactly 1: %+v", seen["A -> B"], p.Violations)
	}
	byLine := violationsOf(t, p)
	if v := byLine["A -> B"]; v.Kind != violationCyclic || v.Weight != 2 {
		t.Errorf("A -> B claimed as %q at weight %d, want %q at 2", v.Kind, v.Weight, violationCyclic)
	}

	// The graph also holds an indirect cycle, A -> C -> B -> A, which survives
	// the direct pass because none of its lines was the one claimed there. It is
	// a second violation on a DIFFERENT line, which is the distinction this test
	// is about: two violations, never two claims on one line.
	if len(p.Violations) != 2 {
		t.Fatalf("got %d violations, want 2: %+v", len(p.Violations), p.Violations)
	}
	if p.Violations[1].Edge.String() == "A -> B" {
		t.Error("the second violation claims the line the first already did")
	}
}

// TestDensityIsLinesOverConnectedComponents pins the denominator. Components
// with no line at either end do not participate, which is why the model says
// connected components rather than components.
func TestDensityIsLinesOverConnectedComponents(t *testing.T) {
	p := measureEntanglement([]ComponentEdge{
		edge("A", "B", 1), edge("B", "C", 1), edge("A", "C", 1),
	})
	if p.Connected != 3 {
		t.Errorf("connected = %d, want 3", p.Connected)
	}
	if p.Lines != 3 {
		t.Errorf("lines = %d, want 3", p.Lines)
	}
	if p.Density != 1 {
		t.Errorf("density = %.2f, want 1.00 (3 lines over 3 connected components)", p.Density)
	}
}

// TestEntanglementPrintsNoVerdict is the point of the whole section. The
// published cap is on a score normalised against benchmark maxima SIG does not
// publish, so a verdict here would be invented. The report has to say that
// rather than leave a reader to assume the property passed.
func TestEntanglementPrintsNoVerdict(t *testing.T) {
	l := LanguageProfile{
		Language: LanguageGo,
		Entanglement: measureEntanglement([]ComponentEdge{
			edge("A", "B", 1), edge("B", "A", 10),
		}),
	}

	b := &strings.Builder{}
	writeEntanglement(b, l)
	out := b.String()

	for _, forbidden := range []string{"Verdict: pass", "Verdict: fail", "| pass |", "| fail |"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("the report renders %q for a property that cannot be judged locally:\n%s",
				forbidden, out)
		}
	}
	if !strings.Contains(out, "No verdict") {
		t.Errorf("the report must say there is no verdict, got:\n%s", out)
	}
	if !strings.Contains(out, "0.077") {
		t.Errorf("the report must name the cap it cannot evaluate, got:\n%s", out)
	}
}

// TestEntanglementIsAbsentWithoutAGraph keeps the degradation consistent with
// every other property here.
func TestEntanglementIsAbsentWithoutAGraph(t *testing.T) {
	if got := measureEntanglement(nil); got != nil {
		t.Errorf("measureEntanglement(nil) = %+v, want nil", got)
	}

	b := &strings.Builder{}
	writeEntanglement(b, LanguageProfile{Language: LanguageTypeScript})
	if !strings.Contains(b.String(), "Not measured") {
		t.Errorf("the report must say the property is absent, got:\n%s", b.String())
	}
}

// TestComponentEdgesCollapseDependencies holds what a communication line is:
// one per ordered pair of components, weighing the module-to-module
// dependencies crossing it, and nothing for dependencies that stay inside a
// component.
func TestComponentEdgesCollapseDependencies(t *testing.T) {
	edges := componentEdges([]ModuleDependency{
		{From: "a.go", To: "b.go", FromComponent: "service", ToComponent: "store", Refs: 40},
		{From: "c.go", To: "b.go", FromComponent: "service", ToComponent: "store", Refs: 1},
		{From: "d.go", To: "e.go", FromComponent: "store", ToComponent: "store", Refs: 9},
		{From: "f.go", To: "g.go", FromComponent: "cmd", ToComponent: "store", Refs: 2},
	})

	if len(edges) != 2 {
		t.Fatalf("got %d lines, want 2: %+v", len(edges), edges)
	}
	byLine := map[string]int{}
	for _, e := range edges {
		byLine[e.String()] = e.Weight
	}
	if byLine["service -> store"] != 2 {
		t.Errorf("service -> store weighs %d, want 2: one per dependency, not per reference",
			byLine["service -> store"])
	}
	if _, inner := byLine["store -> store"]; inner {
		t.Error("a dependency inside one component is not a communication line")
	}
}
