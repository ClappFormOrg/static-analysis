package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// unit builds a measured unit. Only the three measurements matter to the model,
// so the identity fields are left empty.
func unit(loc, mccabe, params int) Unit {
	return Unit{LOC: loc, McCabe: mccabe, Params: params}
}

// profileFor bins units and returns the named property's distribution.
func profileFor(t *testing.T, id string, units []Unit) PropertyProfile {
	t.Helper()
	l := profileOf(UnitSet{Language: "test", Units: units}, true, defaultModuleCouplingCounts)
	for _, p := range l.Properties {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("property %q not in profile", id)
	return PropertyProfile{}
}

// band returns the named risk category's share.
func band(t *testing.T, p PropertyProfile, name string) CategoryShare {
	t.Helper()
	for _, c := range p.Categories {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("category %q not in %s", name, p.ID)
	return CategoryShare{}
}

// tail returns the cumulative tail beginning at the named category.
func tail(t *testing.T, p PropertyProfile, category string) TailShare {
	t.Helper()
	c := band(t, p, category)
	if c.Tail == nil {
		t.Fatalf("category %q of %s carries no tail", category, p.ID)
	}
	return *c.Tail
}

// TestSIGCapsAreV17 pins every published number to the source document.
//
// The caps decide every verdict the scorecard prints, and a cap edited by
// accident is invisible: the report still renders, the arithmetic is still
// right, and the conclusion is silently wrong. So they are asserted literally
// here rather than only being read from sigProperties.
func TestSIGCapsAreV17(t *testing.T) {
	const source = "SIG/TUViT Evaluation Criteria Trusted Product Maintainability, " +
		"Guidance for Producers, v17.0 (2025-03-12)"

	type wantTail struct {
		threshold int
		inclusive bool
		cap       float64
	}
	want := map[string]struct {
		categories []Category
		tails      []wantTail
	}{
		"unit_size": {
			categories: []Category{
				{"low", 1, 15}, {"moderate", 16, 30}, {"high", 31, 60}, {"very high", 61, unbounded},
			},
			tails: []wantTail{{15, false, 47.1}, {30, false, 23.1}, {60, false, 8.3}},
		},
		"unit_complexity": {
			categories: []Category{
				{"low", 1, 5}, {"moderate", 6, 10}, {"high", 11, 25}, {"very high", 26, unbounded},
			},
			tails: []wantTail{{5, false, 20.2}, {10, false, 7.3}, {25, false, 1.1}},
		},
		"unit_interfacing": {
			categories: []Category{
				{"low", 0, 2}, {"moderate", 3, 4}, {"high", 5, 6}, {"very high", 7, unbounded},
			},
			// Worded inclusively in the source ("3 or more parameters") where
			// size and complexity are worded exclusively ("more than 15 lines").
			tails: []wantTail{{3, true, 15.0}, {5, true, 3.3}, {7, true, 0.9}},
		},
	}

	props := sigProperties()
	if len(props) != len(want) {
		t.Fatalf("sigProperties has %d properties, want %d", len(props), len(want))
	}
	if SIGModelVersion != "v17.0 (2025-03-12)" {
		t.Errorf("SIGModelVersion = %q; the caps below are from %s", SIGModelVersion, source)
	}
	if SIGStarTarget != 4 {
		t.Errorf("SIGStarTarget = %d, want 4; every cap below is the 4-star cap", SIGStarTarget)
	}

	for _, p := range props {
		w, ok := want[p.ID]
		if !ok {
			t.Errorf("unexpected property %q -- add its caps here, from %s", p.ID, source)
			continue
		}
		if len(p.Categories) != len(w.categories) {
			t.Errorf("%s: %d categories, want %d", p.ID, len(p.Categories), len(w.categories))
			continue
		}
		for i, c := range p.Categories {
			if c != w.categories[i] {
				t.Errorf("%s category %d = %+v, want %+v (per %s)", p.ID, i, c, w.categories[i], source)
			}
		}
		if len(p.Tails) != len(w.tails) {
			t.Errorf("%s: %d tails, want %d", p.ID, len(p.Tails), len(w.tails))
			continue
		}
		for i, tl := range p.Tails {
			e := w.tails[i]
			if tl.Threshold != e.threshold || tl.Inclusive != e.inclusive || tl.CapPct != e.cap {
				t.Errorf("%s tail %d = %+v, want threshold %d inclusive %v cap %.1f%% (per %s)",
					p.ID, i, tl, e.threshold, e.inclusive, e.cap, source)
			}
		}
	}
}

// TestTailsAlignWithCategories holds the structural invariant the report's
// single-table layout depends on: the tail at index i-1 begins exactly where the
// category at index i begins. If that ever stops being true, a row would show a
// band's share beside a cap that does not govern it.
func TestTailsAlignWithCategories(t *testing.T) {
	for _, p := range sigProperties() {
		if len(p.Tails) != len(p.Categories)-1 {
			t.Errorf("%s: %d tails for %d categories, want one fewer tail than categories",
				p.ID, len(p.Tails), len(p.Categories))
			continue
		}
		for i, tl := range p.Tails {
			c := p.Categories[i+1]
			// An exclusive tail of "> 15" starts at 16; an inclusive tail of
			// ">= 3" starts at 3.
			start := tl.Threshold
			if !tl.Inclusive {
				start++
			}
			if start != c.Min {
				t.Errorf("%s: tail %s starts at %d but category %q starts at %d",
					p.ID, tl.Label(), start, c.Name, c.Min)
			}
		}
	}
}

// TestCategoryBoundaries checks both sides of every published cut. An
// off-by-one here moves real code between risk bands and changes the verdict.
func TestCategoryBoundaries(t *testing.T) {
	tests := []struct {
		property string
		value    int
		want     string
	}{
		{"unit_size", 1, "low"}, {"unit_size", 15, "low"},
		{"unit_size", 16, "moderate"}, {"unit_size", 30, "moderate"},
		{"unit_size", 31, "high"}, {"unit_size", 60, "high"},
		{"unit_size", 61, "very high"},

		{"unit_complexity", 1, "low"}, {"unit_complexity", 5, "low"},
		{"unit_complexity", 6, "moderate"}, {"unit_complexity", 10, "moderate"},
		{"unit_complexity", 11, "high"}, {"unit_complexity", 25, "high"},
		{"unit_complexity", 26, "very high"},

		{"unit_interfacing", 0, "low"}, {"unit_interfacing", 2, "low"},
		{"unit_interfacing", 3, "moderate"}, {"unit_interfacing", 4, "moderate"},
		{"unit_interfacing", 5, "high"}, {"unit_interfacing", 6, "high"},
		{"unit_interfacing", 7, "very high"},
	}

	byID := map[string]Property{}
	for _, p := range sigProperties() {
		byID[p.ID] = p.Property
	}
	for _, tc := range tests {
		p, ok := byID[tc.property]
		if !ok {
			t.Fatalf("no property %q", tc.property)
		}
		if got := categoryOf(p, tc.value); got != tc.want {
			t.Errorf("%s of %d = %q, want %q", tc.property, tc.value, got, tc.want)
		}
	}
}

// TestInterfacingTailsAreInclusive is the one place the three properties differ
// in wording, and getting it wrong shifts every parameter verdict by one.
func TestInterfacingTailsAreInclusive(t *testing.T) {
	// A single unit with 3 parameters: inside the ">= 3" tail, outside "> 3".
	p := profileFor(t, "unit_interfacing", []Unit{unit(10, 1, 3)})
	if got := tail(t, p, "moderate"); got.Pct != 100 {
		t.Errorf("a 3-parameter unit gives the >= 3 tail %.1f%%, want 100%%: the tail is inclusive", got.Pct)
	}

	// Unit size at exactly 15 is NOT in the "> 15" tail.
	s := profileFor(t, "unit_size", []Unit{unit(15, 1, 0)})
	if got := tail(t, s, "moderate"); got.Pct != 0 {
		t.Errorf("a 15-line unit gives the > 15 tail %.1f%%, want 0%%: the tail is exclusive", got.Pct)
	}

	// Complexity at exactly 5 is NOT in the "> 5" tail.
	c := profileFor(t, "unit_complexity", []Unit{unit(10, 5, 0)})
	if got := tail(t, c, "moderate"); got.Pct != 0 {
		t.Errorf("a McCabe-5 unit gives the > 5 tail %.1f%%, want 0%%: the tail is exclusive", got.Pct)
	}
}

// TestTailsAreNested proves the tails are cumulative rather than a partition. A
// very large unit counts in all three, and the shares therefore do not sum to
// 100%. Presenting them as a partition is the misreading the report's preamble
// exists to prevent.
func TestTailsAreNested(t *testing.T) {
	p := profileFor(t, "unit_size", []Unit{unit(70, 1, 0)})
	for _, name := range []string{"moderate", "high", "very high"} {
		if got := tail(t, p, name); got.Pct != 100 {
			t.Errorf("70-line unit: tail at %q = %.1f%%, want 100%% in all three", name, got.Pct)
		}
	}
	// And the bands themselves ARE a partition: the unit sits in exactly one.
	if got := band(t, p, "very high"); got.Elements != 1 {
		t.Errorf("very high band holds %d units, want 1", got.Elements)
	}
	if got := band(t, p, "moderate"); got.Elements != 0 {
		t.Errorf("moderate band holds %d units, want 0 -- bands must not overlap", got.Elements)
	}
}

// TestVolumeWeighting is the discriminator between this metric and the gate
// counts it replaces. Forty one-line units at very high complexity against one
// 400-line simple unit is forty violations by count and a rounding error by
// volume. A count-based implementation passes every other test in this file and
// fails this one.
func TestVolumeWeighting(t *testing.T) {
	units := []Unit{unit(400, 1, 0)}
	for range 40 {
		units = append(units, unit(1, 30, 0))
	}

	p := profileFor(t, "unit_complexity", units)
	vh := tail(t, p, "very high")

	if vh.Elements != 40 {
		t.Fatalf("very high tail holds %d units, want 40", vh.Elements)
	}
	// 40 of 440 lines.
	if want := 40.0 / 440.0 * 100; math.Abs(vh.Pct-want) > 0.001 {
		t.Errorf("very high tail = %.3f%%, want %.3f%% (40 of 440 lines, not 40 of 41 units)", vh.Pct, want)
	}
	if vh.Pct > 10 {
		t.Errorf("very high tail = %.1f%%; a volume-weighted profile must not let 40 one-line "+
			"units dominate 400 lines of simple code", vh.Pct)
	}
	// Inverted: one enormous complex unit is the serious case a count under-rates.
	inverted := profileFor(t, "unit_complexity", []Unit{unit(400, 30, 0), unit(1, 1, 0)})
	if got := tail(t, inverted, "very high"); got.Pct < 99 {
		t.Errorf("one 400-line very-complex unit = %.1f%%, want ~99.8%%: volume is what the model caps", got.Pct)
	}
}

// filler builds n one-line units. A one-line unit is in the low band of every
// property, so it adds to the denominator without entering any tail -- which is
// what lets a test put an exact percentage in a tail.
func filler(n int) []Unit {
	out := make([]Unit, 0, n)
	for range n {
		out = append(out, unit(1, 1, 0))
	}
	return out
}

// TestVerdictPrecision covers the gap between what is compared and what is
// printed. The caps carry one decimal, so a tail just over a cap renders
// identically to one just under it.
func TestVerdictPrecision(t *testing.T) {
	// unit_size "> 60" caps at 8.3%. One 83-line unit against 917 one-line
	// units is 83 of 1000 lines: exactly 8.3%.
	onCap := profileFor(t, "unit_size", append([]Unit{unit(83, 1, 0)}, filler(917)...))
	got := tail(t, onCap, "very high")
	if !got.Pass {
		t.Errorf("a tail exactly on the cap (%.4f%% vs %.1f%%) must pass: the model caps with <=", got.Pct, got.CapPct)
	}
	if !got.AtCap {
		t.Error("a tail exactly on the cap must be flagged AtCap, or the report reads as an unambiguous pass")
	}

	// 84 of 1000 is 8.4%: over the cap, and visibly so.
	over := profileFor(t, "unit_size", append([]Unit{unit(84, 1, 0)}, filler(916)...))
	if got := tail(t, over, "very high"); got.Pass {
		t.Errorf("a tail of %.4f%% must fail a %.1f%% cap", got.Pct, got.CapPct)
	}

	// 831 of 10000 is 8.31%, which renders as "8.3%" and is over the cap. This
	// is the exact case AtCap exists to disclose, and it must still fail.
	hair := profileFor(t, "unit_size", append([]Unit{unit(831, 1, 0)}, filler(9169)...))
	h := tail(t, hair, "very high")
	if h.Pass {
		t.Errorf("a tail of %.4f%% must fail a %.1f%% cap even though it prints as 8.3%%", h.Pct, h.CapPct)
	}
	if !h.AtCap {
		t.Error("a failing tail that prints equal to its cap must be flagged AtCap")
	}
}

// TestUnmeasuredLanguageIsNotAPass is the most consequential case in this file.
// A language with no units has a zero denominator; reporting 0% would print a
// pass against all nine caps for a surface nobody scanned.
func TestUnmeasuredLanguageIsNotAPass(t *testing.T) {
	r := BuildSIGProfile([]UnitSet{{Language: "Nothing", FilesScanned: 12}}, "", defaultModuleCouplingCounts)

	if len(r.Languages) != 1 {
		t.Fatalf("got %d languages, want 1", len(r.Languages))
	}
	l := r.Languages[0]
	if l.Measured {
		t.Error("a language with no units must not be reported as measured")
	}
	if len(l.Properties) != 0 {
		t.Errorf("an unmeasured language produced %d property profiles; it must produce none, "+
			"because every percentage would be 0%% and read as a pass", len(l.Properties))
	}
	if got := r.Unmeasured(); len(got) != 1 || got[0] != "Nothing" {
		t.Errorf("Unmeasured() = %v, want [Nothing] so the run can exit non-zero", got)
	}

	// A set whose units all measure zero lines is the same hazard by another route.
	zero := BuildSIGProfile([]UnitSet{{Language: "Empty", Units: []Unit{unit(0, 1, 0)}}}, "", defaultModuleCouplingCounts)
	if zero.Languages[0].Measured {
		t.Error("units totalling zero lines give a zero denominator and must not count as measured")
	}
}

// TestCombinedCarriesNoVerdict holds the model's "for each programming language
// used" wording: a cross-language roll-up is informational.
func TestCombinedCarriesNoVerdict(t *testing.T) {
	r := BuildSIGProfile([]UnitSet{
		{Language: LanguageGo, Units: []Unit{unit(100, 30, 9)}, NonUnitLOC: 10, FilesScanned: 1},
		{Language: LanguageTypeScript, Units: []Unit{unit(4, 1, 1)}, NonUnitLOC: 5, FilesScanned: 2},
	}, "", defaultModuleCouplingCounts)

	for _, l := range r.Languages {
		if !l.Verdicts {
			t.Errorf("%s must carry verdicts: the caps are defined per language", l.Language)
		}
	}
	if r.Combined.Verdicts {
		t.Error("the combined roll-up must not carry verdicts")
	}
	if r.Combined.Units != 2 || r.Combined.UnitLOC != 104 {
		t.Errorf("combined = %d units / %d LOC, want 2 / 104", r.Combined.Units, r.Combined.UnitLOC)
	}
	if r.Combined.NonUnitLOC != 15 || r.Combined.FilesScanned != 3 {
		t.Errorf("combined non-unit LOC %d and files %d, want 15 and 3",
			r.Combined.NonUnitLOC, r.Combined.FilesScanned)
	}
	// Languages are ordered so two runs of one tree diff cleanly.
	if r.Languages[0].Language != LanguageGo || r.Languages[1].Language != LanguageTypeScript {
		t.Errorf("languages not sorted: %s then %s", r.Languages[0].Language, r.Languages[1].Language)
	}
	// The combined table must not be rendered with a verdict column filled in.
	out := &strings.Builder{}
	if err := WriteSIGProfile(out, r, nil, Deviations{}); err != nil {
		t.Fatalf("WriteSIGProfile: %v", err)
	}
	if !strings.Contains(out.String(), "no verdict") {
		t.Error("the rendered report must say the combined roll-up carries no verdict")
	}
}

// TestNonUnitLOCIsReportedNotFolded pins the denominator. Folding non-unit lines
// in would understate every tail, and omitting the figure entirely would hide a
// profile that speaks for a fraction of the tree -- which is exactly how the
// 2026-07-31 vendor export went unnoticed.
func TestNonUnitLOCIsReportedNotFolded(t *testing.T) {
	l := profileOf(UnitSet{
		Language: "x", Units: []Unit{unit(50, 30, 0), unit(50, 1, 0)}, NonUnitLOC: 900,
	}, true, defaultModuleCouplingCounts)

	if l.UnitLOC != 100 {
		t.Errorf("UnitLOC = %d, want 100: the denominator is unit lines only", l.UnitLOC)
	}
	if l.MeasuredLOC() != 1000 {
		t.Errorf("MeasuredLOC = %d, want 1000", l.MeasuredLOC())
	}
	if math.Abs(l.NonUnitPct()-90) > 0.001 {
		t.Errorf("NonUnitPct = %.2f, want 90", l.NonUnitPct())
	}
	p := profileFor(t, "unit_complexity", []Unit{unit(50, 30, 0), unit(50, 1, 0)})
	if got := tail(t, p, "very high"); math.Abs(got.Pct-50) > 0.001 {
		t.Errorf("very high tail = %.2f%%, want 50%% (50 of 100 unit lines, not 50 of 1000)", got.Pct)
	}
}

func TestLoadUnitSets(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	ok := write("ok.json", `{"language":"TypeScript","non_unit_loc":7,"files_scanned":2,
	  "units":[{"element":"useThing","file":"app/x.ts","line":3,"loc":9,"mccabe":2,"params":1}]}`)
	sets, err := LoadUnitSets([]string{ok})
	if err != nil {
		t.Fatalf("LoadUnitSets: %v", err)
	}
	if len(sets) != 1 || sets[0].Language != LanguageTypeScript || len(sets[0].Units) != 1 {
		t.Fatalf("got %+v", sets)
	}
	if u := sets[0].Units[0]; u.LOC != 9 || u.McCabe != 2 || u.Params != 1 || u.Element != "useThing" {
		t.Errorf("unit round-tripped as %+v", u)
	}

	// Malformed input is a hard error. Skipping it would drop a whole language
	// silently, and a missing language reports nothing, which reads as fine.
	if _, err := LoadUnitSets([]string{write("bad.json", `{"language":`)}); err == nil {
		t.Error("a malformed units file must be an error, not a skipped language")
	}
	// Unattributable measurements are likewise refused rather than guessed at.
	if _, err := LoadUnitSets([]string{write("nolang.json", `{"units":[]}`)}); err == nil {
		t.Error("a units file with no language must be an error")
	}
	if _, err := LoadUnitSets([]string{filepath.Join(dir, "absent.json")}); err == nil {
		t.Error("a missing units file must be an error")
	}
}

// TestGoUnitsSkipsBodylessDeclarations keeps unexecutable declarations out of the
// denominator. An assembly stub counted as a 1-line, McCabe-1 unit would dilute
// every tail downward.
func TestGoUnitsSkipsBodylessDeclarations(t *testing.T) {
	idx := load(t, map[string]string{
		"a/a.go": `package a

import "fmt"

type T struct{ N int }

// Stub has no Go body.
func Stub(n int) int

func Real(n int) int {
	if n > 0 {
		return n
	}
	return fmt.Sprint(n)[0:0][0]
}
`,
	}, DefaultConfig())

	set := units(t, idx)
	if len(set.Units) != 1 {
		t.Fatalf("got %d units, want 1 (Stub has no body): %+v", len(set.Units), set.Units)
	}
	got := set.Units[0]
	if got.Element != "Real" {
		t.Errorf("measured %q, want Real", got.Element)
	}
	if got.McCabe != 2 {
		t.Errorf("Real McCabe = %d, want 2 (one `if`)", got.McCabe)
	}
	if got.Params != 1 {
		t.Errorf("Real params = %d, want 1", got.Params)
	}
	// The import, the type declaration and Stub's signature reside in no unit.
	if set.NonUnitLOC == 0 {
		t.Error("non-unit LOC = 0; the import, type and bodyless signature all reside outside a unit")
	}
	if set.FilesScanned != 1 {
		t.Errorf("FilesScanned = %d, want 1", set.FilesScanned)
	}
}

// TestSIGUnitsCSVLabelsEveryUnit covers the audit view: the report's percentages
// have to be re-derivable from it.
func TestSIGUnitsCSVLabelsEveryUnit(t *testing.T) {
	out := &strings.Builder{}
	err := WriteSIGUnits(out, []UnitSet{{
		Language: LanguageGo,
		Units:    []Unit{{Element: "Big", File: "a.go", Line: 4, LOC: 70, McCabe: 30, Params: 8}},
	}})
	if err != nil {
		t.Fatalf("WriteSIGUnits: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"language,element,file,line,loc,mccabe,params,unit_size_category,unit_complexity_category,unit_interfacing_category",
		"Go,Big,a.go,4,70,30,8,very high,very high,very high",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("CSV missing %q\ngot:\n%s", want, got)
		}
	}
}

// TestReportNamesWhenTheEditionWasChecked holds that the profile says when a
// person last confirmed the model edition is current, not only which edition the
// caps came from.
//
// The two are different facts and only one of them can go stale on its own. SIG
// recalibrates its benchmark yearly, so a cap can move with no change in what
// the metric means, and a report naming v17.0 forever gives a reader no way to
// tell a checked edition from an unexamined constant.
func TestReportNamesWhenTheEditionWasChecked(t *testing.T) {
	if SIGModelChecked == "" {
		t.Fatal("SIGModelChecked is empty: the report cannot say the edition was ever confirmed")
	}
	if _, err := time.Parse("2006-01-02", SIGModelChecked); err != nil {
		t.Errorf("SIGModelChecked = %q, want an ISO date: %v", SIGModelChecked, err)
	}

	r := BuildSIGProfile([]UnitSet{
		{Language: LanguageGo, Units: []Unit{unit(10, 1, 0)}, FilesScanned: 1},
	}, "", defaultModuleCouplingCounts)

	b := &strings.Builder{}
	if err := WriteSIGProfile(b, r, nil, Deviations{}); err != nil {
		t.Fatalf("WriteSIGProfile: %v", err)
	}
	for _, want := range []string{SIGModelVersion, SIGModelChecked} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("the report header does not carry %q", want)
		}
	}
}
