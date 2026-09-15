package main

import (
	"strings"
	"testing"
)

// fullSet is a surface where every property has the data it needs: units for
// the three unit properties, and modules carrying their incoming edges and
// their normalised lines so module coupling and duplication are measured too.
// Without the modules, eleven of the thirteen measures report as unmeasured and
// a test asserting a count would be asserting the absence of data.
func fullSet(language string, units []Unit) UnitSet {
	return UnitSet{
		Language: language, FilesScanned: 1, NonUnitLOC: 2,
		Units: units,
		Modules: []Module{{
			File: "a", LOC: 7, InModules: 1, InRefs: 1,
			Lines: []string{"one", "two", "three"},
		}},
		ModuleMetrics: []string{moduleMetricCoupling, moduleMetricDuplication},
	}
}

// cleanSet is a surface inside every cap: one small, simple, narrow unit.
func cleanSet(language string) UnitSet {
	return fullSet(language, []Unit{unit(5, 1, 1)})
}

// dirtySet is a surface over the unit-size and unit-complexity caps.
func dirtySet(language string) UnitSet {
	return fullSet(language, []Unit{unit(200, 40, 1)})
}

// unitsOnlySet supplies no modules at all, which is the shape a front end that
// cannot resolve a reference to the file declaring it produces.
func unitsOnlySet(language string) UnitSet {
	return UnitSet{
		Language: language, FilesScanned: 1, NonUnitLOC: 2,
		Units: []Unit{unit(5, 1, 1)},
	}
}

// deviationsFor builds a loaded set covering one tail. It reaches past
// LoadDeviations on purpose: what is under test here is whether a deviation
// changes the count, and the validation that file loading applies is pinned by
// deviations_test.go.
func deviationsFor(t *testing.T, language, property, tail string) Deviations {
	t.Helper()
	return Deviations{byTail: map[string]Deviation{
		deviationKey(language, property, tail): {
			Language: language, Property: property, Tail: tail,
			Decided: "2026-01-01", Summary: "pinned by a test",
		},
	}}
}

// standingFor builds the report and reduces it to one language's eligibility.
func standingFor(t *testing.T, sets []UnitSet, devs Deviations) Standing {
	t.Helper()
	return BuildStanding(BuildSIGProfile(sets, "", defaultModuleCouplingCounts), devs)
}

func only(t *testing.T, s Standing) Eligibility {
	t.Helper()
	if len(s.Languages) != 1 {
		t.Fatalf("want 1 language with a standing, got %d", len(s.Languages))
	}
	return s.Languages[0]
}

// TestEligibilityIsAConjunction is the document's own definition and the reason
// there is no weighting to apply. One measure over its cap decides the verdict,
// whatever the other twelve say.
func TestEligibilityIsAConjunction(t *testing.T) {
	clean := only(t, standingFor(t, []UnitSet{cleanSet(LanguageGo)}, Deviations{}))
	if !clean.Eligible {
		t.Fatalf("a surface inside every cap is not eligible: %d over", clean.Over)
	}
	if clean.Over != 0 || clean.WithinCap != clean.Total() {
		t.Errorf("clean surface: %d within cap, %d over", clean.WithinCap, clean.Over)
	}

	dirty := only(t, standingFor(t, []UnitSet{dirtySet(LanguageGo)}, Deviations{}))
	if dirty.Eligible {
		t.Error("a surface over a cap reports eligible")
	}
	if dirty.Over == 0 {
		t.Error("no measure counted as over its cap")
	}
	if dirty.WithinCap+dirty.Over != dirty.Total() {
		t.Errorf("counts do not partition: %d + %d != %d", dirty.WithinCap, dirty.Over, dirty.Total())
	}
}

// TestUnmeasuredPropertiesLeaveTheDenominator is what keeps the count readable.
// A surface with no component map has no independence verdict, so independence
// is not one of the measures it is "13 of 14" on. It is named instead.
func TestUnmeasuredPropertiesLeaveTheDenominator(t *testing.T) {
	// Nine unit tails, three module coupling tails, duplication. Component
	// independence needs a boundary nobody supplied.
	full := only(t, standingFor(t, []UnitSet{cleanSet(LanguageGo)}, Deviations{}))
	if full.Total() != 13 {
		t.Errorf("Total() = %d, want 13", full.Total())
	}
	if len(full.Unmeasured) != 1 || full.Unmeasured[0] != "component independence" {
		t.Errorf("Unmeasured = %v, want [component independence]", full.Unmeasured)
	}
	for _, m := range full.Measures {
		if m.Property == "Component independence" {
			t.Error("an unmeasured property was counted as a measure")
		}
	}

	// A front end that supplies no modules loses eleven of the thirteen. The
	// denominator has to follow, or the count reports passes for three coupling
	// tails and a duplication cap nobody computed.
	units := only(t, standingFor(t, []UnitSet{unitsOnlySet(LanguageGo)}, Deviations{}))
	if units.Total() != 9 {
		t.Errorf("Total() = %d with no modules, want the 9 unit tails", units.Total())
	}
	if len(units.Unmeasured) != 3 {
		t.Errorf("Unmeasured = %v, want module coupling, duplication and independence", units.Unmeasured)
	}
}

// TestEntanglementAndVolumeAreNeverCounted holds the exclusion the preamble
// promises. Neither carries a verdict, so neither can be within a cap or over
// one, and folding either in would change a count nobody could re-derive.
func TestEntanglementAndVolumeAreNeverCounted(t *testing.T) {
	e := only(t, standingFor(t, []UnitSet{dirtySet(LanguageGo)}, Deviations{}))
	for _, m := range e.Measures {
		if strings.Contains(strings.ToLower(m.Property), "entanglement") ||
			strings.Contains(strings.ToLower(m.Property), "volume") {
			t.Errorf("%q was counted as a capped measure", m.Property)
		}
	}
}

// TestDeviationExplainsAFailWithoutErasingIt is the distinction that stops a
// summary certifying a product the model does not. A recorded deviation says
// why a fail is the intended state; it does not move the measure into the
// within-cap column.
func TestDeviationExplainsAFailWithoutErasingIt(t *testing.T) {
	devs := deviationsFor(t, LanguageGo, "unit_size", "> 60")
	e := only(t, standingFor(t, []UnitSet{dirtySet(LanguageGo)}, devs))

	if e.Eligible {
		t.Error("a deviated measure made the language eligible")
	}
	if e.Deviated != 1 {
		t.Errorf("Deviated = %d, want 1", e.Deviated)
	}
	var found bool
	for _, m := range e.Measures {
		if m.Deviated {
			found = true
			if m.Pass {
				t.Error("a deviated measure reports as passing")
			}
		}
	}
	if !found {
		t.Error("no measure carries the deviation")
	}
}

// TestPropertyDistanceIsTheMaxOfItsTails is the arithmetic the preamble commits
// to. The three unit-size tails are nested, so lines leaving the innermost one
// leave the outer two as well: the cost of clearing all three is the largest of
// them, and summing would charge one extraction up to three times.
func TestPropertyDistanceIsTheMaxOfItsTails(t *testing.T) {
	e := only(t, standingFor(t, []UnitSet{dirtySet(LanguageGo)}, Deviations{}))

	var size PropertyDistance
	for _, d := range e.Distances {
		if d.Property == "Unit size" {
			size = d
		}
	}
	if size.Property == "" {
		t.Fatalf("no unit size distance in %v", e.Distances)
	}

	var maxTail, sumTails int
	for _, m := range e.Measures {
		if m.Property != "Unit size" || m.Pass {
			continue
		}
		maxTail = max(maxTail, m.MoveLOC)
		sumTails += m.MoveLOC
	}
	if size.MoveLOC != maxTail {
		t.Errorf("distance = %d, want the largest tail %d", size.MoveLOC, maxTail)
	}
	if sumTails > maxTail && size.MoveLOC == sumTails {
		t.Errorf("distance = %d, which is the sum of the tails", size.MoveLOC)
	}
}

// TestDistancesAreWorstFirst keeps the list usable as a work order.
func TestDistancesAreWorstFirst(t *testing.T) {
	e := only(t, standingFor(t, []UnitSet{dirtySet(LanguageGo)}, Deviations{}))
	for i := 1; i < len(e.Distances); i++ {
		if e.Distances[i-1].MoveLOC < e.Distances[i].MoveLOC {
			t.Errorf("distances out of order: %v", e.Distances)
		}
	}
}

// TestProductIsEligibleOnlyWhenEveryLanguageIs holds the "for each programming
// language used" wording at the product level. A clean Go surface cannot carry
// a TypeScript one over its caps.
func TestProductIsEligibleOnlyWhenEveryLanguageIs(t *testing.T) {
	s := standingFor(t, []UnitSet{cleanSet(LanguageGo), dirtySet(LanguageTypeScript)}, Deviations{})
	if len(s.Languages) != 2 {
		t.Fatalf("want 2 languages, got %d", len(s.Languages))
	}
	if s.Eligible {
		t.Error("the product is eligible with one surface over a cap")
	}

	both := standingFor(t, []UnitSet{cleanSet(LanguageGo), cleanSet(LanguageTypeScript)}, Deviations{})
	if !both.Eligible {
		t.Error("two clean surfaces do not make an eligible product")
	}
}

// TestRollUpAndUnmeasuredLanguagesAreExcluded keeps two known non-verdicts out
// of the count. The combined roll-up carries none by design, and a language with
// no units has no denominator to carry one.
func TestRollUpAndUnmeasuredLanguagesAreExcluded(t *testing.T) {
	s := standingFor(t, []UnitSet{
		cleanSet(LanguageGo),
		{Language: LanguageTypeScript, FilesScanned: 3},
	}, Deviations{})

	for _, e := range s.Languages {
		if e.Language == combinedLabel {
			t.Error("the combined roll-up produced a standing")
		}
		if e.Language == LanguageTypeScript {
			t.Error("a language with no units produced a standing")
		}
	}
	if len(s.Languages) != 1 {
		t.Errorf("want only Go, got %d standings", len(s.Languages))
	}
}

// TestNothingMeasuredIsNotAPass is the same refusal the unmeasured-language
// check makes one level up. A run that measured no language must not print the
// word eligible.
func TestNothingMeasuredIsNotAPass(t *testing.T) {
	s := standingFor(t, []UnitSet{{Language: LanguageGo, FilesScanned: 4}}, Deviations{})
	if s.Eligible {
		t.Error("a run that measured nothing reports the product eligible")
	}

	b := &strings.Builder{}
	writeStanding(b, s)
	if !strings.Contains(b.String(), "No language carried a verdict") {
		t.Errorf("the report does not say nothing was measured:\n%s", b)
	}
}

// TestComponentIndependenceIsCountedWhenABoundaryExists is the fourteenth
// measure appearing. It is the one property whose verdict this file reads off a
// floor rather than a cap, so a sign error here would count an exposed codebase
// as within cap.
func TestComponentIndependenceIsCountedWhenABoundaryExists(t *testing.T) {
	exposed := fullSet(LanguageGo, []Unit{unit(5, 1, 1)})
	exposed.Components = "2026-01-01"
	exposed.Modules[0].Component = "api"
	// Reachable from another component, so no line is hidden and the property
	// sits far under its 93.7% floor.
	exposed.Modules[0].InCrossComponent = 1

	e := only(t, standingFor(t, []UnitSet{exposed}, Deviations{}))
	if e.Total() != 14 {
		t.Errorf("Total() = %d, want 14 with a component boundary", e.Total())
	}
	var found bool
	for _, m := range e.Measures {
		if m.Property != "Component independence" {
			continue
		}
		found = true
		if m.Pass {
			t.Errorf("a fully exposed surface is within cap at %.1f%% against %.1f%%", m.Pct, m.CapPct)
		}
	}
	if !found {
		t.Fatal("component independence is not among the measures")
	}
	if e.Eligible {
		t.Error("the language is eligible with independence under its floor")
	}
}

// TestAnEligibleLanguageRendersNoWorkList keeps the summary quiet when there is
// nothing to do. A table of failing measures with no rows in it reads as a
// rendering bug.
func TestAnEligibleLanguageRendersNoWorkList(t *testing.T) {
	b := &strings.Builder{}
	writeStanding(b, standingFor(t, []UnitSet{cleanSet(LanguageGo)}, Deviations{}))
	out := b.String()

	if !strings.Contains(out, "| Go | 13 of 13 | 0 | yes |") {
		t.Errorf("the count line is missing or wrong:\n%s", out)
	}
	if strings.Contains(out, "over its cap:") {
		t.Errorf("an eligible language rendered a work list:\n%s", out)
	}
}

// TestDeviationIsNamedInTheWorkList makes the recorded decision visible where
// the fail is, rather than only in the property table further down.
func TestDeviationIsNamedInTheWorkList(t *testing.T) {
	devs := deviationsFor(t, LanguageGo, "unit_size", "> 60")
	b := &strings.Builder{}
	writeStanding(b, standingFor(t, []UnitSet{dirtySet(LanguageGo)}, devs))
	out := b.String()

	if !strings.Contains(out, "(recorded deviation)") {
		t.Errorf("the deviation is not named in the work list:\n%s", out)
	}
	if !strings.Contains(out, "deviated)") {
		t.Errorf("the count line does not separate deviated fails:\n%s", out)
	}
}

// TestStandingNamesTheWorkAndTheGap is the rendered half: a reader has to be
// able to see which measure is over, by how much, and what it costs, without
// reading the nine tables below it.
func TestStandingNamesTheWorkAndTheGap(t *testing.T) {
	b := &strings.Builder{}
	writeStanding(b, standingFor(t, []UnitSet{dirtySet(LanguageGo)}, Deviations{}))
	out := b.String()

	for _, want := range []string{
		"## Where this stands",
		"| Language | Within cap | Over | Eligible at 4 stars |",
		"not eligible at 4 stars",
		"Unit size",
		"LOC to move",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary is missing %q:\n%s", want, out)
		}
	}
	// The count is the point. A rating would be an invention.
	if strings.Contains(out, "stars)") || strings.Contains(strings.ToLower(out), "rating of") {
		t.Errorf("the summary prints something shaped like a star rating:\n%s", out)
	}
}
