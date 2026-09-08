package main

import (
	"fmt"
	"strings"
	"testing"
)

// linesOf turns a block of Go source into the normalised lines duplication is
// measured over.
func linesOf(src string) []string { return normalisedLines([]byte(src)) }

// dupModule builds a module straight from its normalised lines. The arithmetic
// tests below use this rather than Go source, because a source fixture also
// contributes its package clause, signature and closing brace to the comparison
// and those lines match between fixtures too, which makes an exact expected
// count a puzzle rather than an assertion.
func dupModule(name string, lines ...string) Module {
	return Module{File: name, LOC: len(lines), Lines: lines}
}

// shared is a fragment exactly at the model's minimum length.
func shared() []string {
	return []string{
		"total := 0",
		"for _, row := range rows {",
		"if row.Skip {",
		"continue",
		"}",
		"total += row.Value",
	}
}

// unique returns filler that cannot match anything else, so a fixture only
// contains the duplication it means to.
func unique(tag string, n int) []string {
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, fmt.Sprintf("%s%d := %d", tag, i, i))
	}
	return out
}

func withFiller(tag string, lines []string) []string {
	return append(append(unique(tag, 3), lines...), unique(tag+"z", 3)...)
}

// TestDuplicationCapIsV17 pins the published cap and the fragment length to the
// source document. Both are part of the definition, so an edit to either leaves
// a number that is no longer the property.
func TestDuplicationCapIsV17(t *testing.T) {
	const source = "SIG/TUViT Evaluation Criteria Trusted Product Maintainability, " +
		"Guidance for Producers, v17.0 (2025-03-12), section 3.2"

	if duplicationCapPct != 5.6 {
		t.Errorf("duplicationCapPct = %.1f, want 5.6 (%s)", duplicationCapPct, source)
	}
	if duplicationMinLines != 6 {
		t.Errorf("duplicationMinLines = %d, want 6 (%s)", duplicationMinLines, source)
	}
}

// TestNormalisedLinesMatchLOC holds the invariant the property rests on: the
// lines duplication compares are exactly the lines countLOCIn counts, so its
// numerator and its denominator are the same measurement. A drift here would let
// the percentage exceed 100 or silently shrink.
func TestNormalisedLinesMatchLOC(t *testing.T) {
	src := `package a

import "fmt"

// Doc comment, not a line of code.
func F(n int) string {
	// A comment line on its own.
	if n > 0 { // and a trailing one
		return fmt.Sprint(n)
	}

	return ""
}
`
	if got, want := len(linesOf(src)), countLOCIn([]byte(src)); got != want {
		t.Errorf("normalisedLines produced %d lines, countLOCIn counted %d", got, want)
	}
}

// TestNormalisationIsModuloWhitespaceAndComments holds what "repeated literally,
// modulo white-space" comes to in practice. Reindenting a copy or commenting it
// differently leaves it a clone; changing an identifier does not.
func TestNormalisationIsModuloWhitespaceAndComments(t *testing.T) {
	const original = "func F(rows []Row) int {\n\ttotal := 0\n\tfor _, row := range rows {\n\t\ttotal += row.Value\n\t}\n\treturn total\n}\n"
	reindented := strings.ReplaceAll(original, "\t", "    ")
	commented := strings.Replace(original,
		"\ttotal := 0\n", "\t// Running sum.\n\ttotal := 0 // starts empty\n", 1)
	renamed := strings.ReplaceAll(original, "total", "sum")

	same := func(name, src string) {
		t.Helper()
		if a, b := linesOf(original), linesOf(src); strings.Join(a, "\n") != strings.Join(b, "\n") {
			t.Errorf("%s normalised differently:\n%v\nvs\n%v", name, a, b)
		}
	}
	same("reindented", reindented)
	same("commented", commented)

	if a, b := linesOf(original), linesOf(renamed); strings.Join(a, "\n") == strings.Join(b, "\n") {
		t.Error("a renamed identifier must not normalise to the same lines")
	}
}

// TestFragmentNeedsSixLines pins the boundary. Five identical lines are not a
// fragment and six are, which is the whole of "at least 6 lines of code long".
func TestFragmentNeedsSixLines(t *testing.T) {
	five := shared()[:5]
	short := measureDuplication([]Module{
		dupModule("a.go", withFiller("a", five)...),
		dupModule("b.go", withFiller("b", five)...),
	}, true)
	if short.RedundantLOC != 0 {
		t.Errorf("five repeated lines counted %d redundant lines, want 0", short.RedundantLOC)
	}

	long := measureDuplication([]Module{
		dupModule("a.go", withFiller("a", shared())...),
		dupModule("b.go", withFiller("b", shared())...),
	}, true)
	if long.RedundantLOC != 6 {
		t.Errorf("six repeated lines counted %d redundant lines, want 6", long.RedundantLOC)
	}
}

// TestRedundancyIsOccurrencesMinusOne is the model's own arithmetic: with
// optimal reuse the fragment would still occur once, so three occurrences of a
// six-line fragment are twelve redundant lines, not eighteen.
func TestRedundancyIsOccurrencesMinusOne(t *testing.T) {
	p := measureDuplication([]Module{
		dupModule("a.go", withFiller("a", shared())...),
		dupModule("b.go", withFiller("b", shared())...),
		dupModule("c.go", withFiller("c", shared())...),
	}, true)

	if p.RedundantLOC != 12 {
		t.Errorf("three occurrences of a six-line fragment gave %d redundant lines, want 12", p.RedundantLOC)
	}
	if p.ModulesWithRedundancy != 2 {
		t.Errorf("%d modules carry redundancy, want 2: the first occurrence is the one reuse keeps",
			p.ModulesWithRedundancy)
	}
	for _, m := range p.Modules {
		if m.File == "a.go" {
			t.Error("the earliest occurrence must not be charged for the duplication")
		}
	}
}

// TestOverlappingClonesCountEachLineOnce is the detail that keeps this a
// percentage. Summing fragment lengths would charge a line twice wherever two
// windows overlap, and a module could then report more redundant lines than it
// holds.
func TestOverlappingClonesCountEachLineOnce(t *testing.T) {
	// Twelve distinct lines shared by both modules, so seven overlapping
	// six-line windows repeat and every line of the second copy sits inside
	// more than one of them.
	block := append(shared(), unique("tail", 6)...)
	p := measureDuplication([]Module{
		dupModule("a.go", withFiller("a", block)...),
		dupModule("b.go", withFiller("b", block)...),
	}, true)

	if p.RedundantLOC != 12 {
		t.Errorf("a twelve-line clone gave %d redundant lines, want 12", p.RedundantLOC)
	}
	for _, m := range p.Modules {
		if m.RedundantLOC > m.LOC {
			t.Errorf("%s reports %d redundant lines of %d: a line was counted twice",
				m.File, m.RedundantLOC, m.LOC)
		}
		if m.Pct > 100 {
			t.Errorf("%s reports %.1f%% redundant, which is not a percentage", m.File, m.Pct)
		}
	}
}

// TestRepetitionInsideOneModuleCounts holds that a fragment repeated within one
// file is duplication. The model says "repeated literally in at least one other
// LOCATION", not in another file, and the remedy is the same either way: write it
// once and call it twice.
func TestRepetitionInsideOneModuleCounts(t *testing.T) {
	twice := append(append(unique("head", 3), shared()...), append(unique("mid", 3), shared()...)...)

	p := measureDuplication([]Module{dupModule("a.go", twice...)}, true)
	if p.RedundantLOC != 6 {
		t.Errorf("a fragment written twice in one module gave %d redundant lines, want 6", p.RedundantLOC)
	}

	// And across modules the occurrences add up the same way: four occurrences
	// of a six-line fragment are three redundant copies.
	q := measureDuplication([]Module{
		dupModule("a.go", twice...),
		dupModule("b.go", withFiller("b", shared())...),
		dupModule("c.go", withFiller("c", shared())...),
	}, true)
	if q.RedundantLOC != 18 {
		t.Errorf("four occurrences gave %d redundant lines, want 18", q.RedundantLOC)
	}
}

// TestDuplicationIsAbsentWithoutLines holds the degradation for a language whose
// front end supplies no normalised lines. Reporting 0% would render as a pass
// against the cap for a surface nobody compared.
func TestDuplicationIsAbsentWithoutLines(t *testing.T) {
	if got := measureDuplication([]Module{{File: "a.ts", LOC: 100}}, true); got != nil {
		t.Errorf("duplication measured %+v for modules carrying no lines, want nil", got)
	}

	l := profileOf(UnitSet{
		Language: LanguageTypeScript,
		Units:    []Unit{unit(10, 1, 0)},
		Modules:  []Module{{File: "a.ts", LOC: 100}},
	}, true, defaultModuleCouplingCounts)

	b := &strings.Builder{}
	writeDuplication(b, l)
	if !strings.Contains(b.String(), "Not measured") {
		t.Errorf("the report must say duplication is absent for %s, got:\n%s", l.Language, b.String())
	}
	if strings.Contains(b.String(), "0.0%") {
		t.Error("an unmeasured property must not render a percentage at all")
	}
}

// TestDuplicationTableSaysWhatItLeftOut holds the no-silent-truncation rule: a
// table showing ten of many rows has to say so, or it reads as the whole list.
func TestDuplicationTableSaysWhatItLeftOut(t *testing.T) {
	var mods []Module
	for i := range duplicationTableLimit + 5 {
		mods = append(mods, dupModule(fmt.Sprintf("m%02d.go", i), withFiller(fmt.Sprintf("t%d", i), shared())...))
	}
	l := LanguageProfile{Language: LanguageGo, Duplication: measureDuplication(mods, true)}

	b := &strings.Builder{}
	writeDuplication(b, l)
	out := b.String()
	if !strings.Contains(out, "10 of 14 modules") {
		t.Errorf("the table must name how many rows it left out, got:\n%s", out)
	}
}
