package main

import (
	"fmt"
	"strings"
	"testing"
)

// modulesByFile indexes a measured set by the path each module was reported at.
func modulesByFile(mods []Module) map[string]Module {
	out := make(map[string]Module, len(mods))
	for _, m := range mods {
		out[m.File] = m
	}
	return out
}

// TestModuleCouplingCapsAreV17 pins the published numbers to the source
// document, for the same reason TestSIGCapsAreV17 does: a cap edited by accident
// leaves a report that still renders and is silently wrong.
func TestModuleCouplingCapsAreV17(t *testing.T) {
	const source = "SIG/TUViT Evaluation Criteria Trusted Product Maintainability, " +
		"Guidance for Producers, v17.0 (2025-03-12), section 3.6"

	props := moduleProperties(defaultModuleCouplingCounts)
	if len(props) != 1 {
		t.Fatalf("moduleProperties has %d properties, want 1", len(props))
	}
	p := props[0]

	wantCategories := []Category{
		{"low", 0, 10}, {"moderate", 11, 20}, {"high", 21, 50}, {"very high", 51, unbounded},
	}
	if len(p.Categories) != len(wantCategories) {
		t.Fatalf("%s has %d categories, want %d", p.ID, len(p.Categories), len(wantCategories))
	}
	for i, want := range wantCategories {
		if p.Categories[i] != want {
			t.Errorf("category %d = %+v, want %+v (%s)", i, p.Categories[i], want, source)
		}
	}

	// Above 10, above 20 and above 50, all worded exclusively in the source.
	wantTails := []Tail{
		{Threshold: 10, CapPct: 10.0},
		{Threshold: 20, CapPct: 5.6},
		{Threshold: 50, CapPct: 1.9},
	}
	if len(p.Tails) != len(wantTails) {
		t.Fatalf("%s has %d tails, want %d", p.ID, len(p.Tails), len(wantTails))
	}
	for i, want := range wantTails {
		if p.Tails[i] != want {
			t.Errorf("tail %d = %+v, want %+v (%s)", i, p.Tails[i], want, source)
		}
	}

	// The bands must start exactly one above each cap's threshold. Bands at
	// 1-10/11-20 with a tail at "> 10" is the only arrangement in which the
	// cumulative tail beginning at a band is the same set as that band and
	// everything above it, which propertyProfileOf relies on when it puts the
	// two in one row.
	for i, tail := range p.Tails {
		if got := p.Categories[i+1].Min; got != tail.Threshold+1 {
			t.Errorf("category %d starts at %d, want %d so it aligns with the %q tail",
				i+1, got, tail.Threshold+1, tail.Label())
		}
	}
}

// TestIncomingEdgesAreCountedPerReferencingModule holds the three decisions the
// measurement makes about what an incoming dependency is.
func TestIncomingEdgesAreCountedPerReferencingModule(t *testing.T) {
	files := map[string]string{
		"target/target.go": `package target

import "fmt"

// Shared is what everything else here references.
func Shared() string { return fmt.Sprint("x") }

// Sibling references Shared from the same module, which is not incoming
// coupling: an edit to Shared does not reach another file through it.
func Sibling() string { return Shared() }
`,
		// A second file in the target's own package. A sibling reference IS
		// coupling between two modules, so this one counts.
		"target/neighbour.go": `package target

// Neighbour leans on its sibling file.
func Neighbour() string { return Shared() }
`,
	}
	// Three callers, one of which references the target twice, so the two
	// counting conventions disagree by a known amount.
	for i := range 3 {
		body := "package caller%d\n\nimport \"example.com/m/target\"\n\n// A calls across.\nfunc A() string { return target.Shared() }\n"
		if i == 0 {
			body += "\n// B calls across a second time, from the same module.\nfunc B() string { return target.Shared() }\n"
		}
		files[fmt.Sprintf("caller%d/caller.go", i)] = fmt.Sprintf(body, i)
	}

	idx := load(t, files, DefaultConfig())
	mods := modulesByFile(modules(t, idx, Components{}))

	target, ok := mods["target/target.go"]
	if !ok {
		t.Fatalf("target module missing from %v", mods)
	}

	// Three callers plus the sibling file, counted once each.
	if target.InModules != 4 {
		t.Errorf("InModules = %d, want 4: three callers and one sibling file, each counted once "+
			"however many references it makes", target.InModules)
	}
	// Five references: four from the callers (one of them twice) and one from
	// the sibling.
	if target.InRefs != 5 {
		t.Errorf("InRefs = %d, want 5: every individual reference", target.InRefs)
	}

	// A module nothing references is measured at zero rather than dropped. It
	// is part of the denominator every percentage is a share of.
	caller, ok := mods["caller1/caller.go"]
	if !ok {
		t.Fatal("an unreferenced module must still be measured")
	}
	if caller.InModules != 0 || caller.InRefs != 0 {
		t.Errorf("unreferenced module = %+v, want zero incoming", caller)
	}
	if caller.LOC == 0 {
		t.Error("an unreferenced module must still carry its LOC, or it leaves the denominator")
	}
}

// TestModuleLOCIncludesLinesOutsideUnits separates this denominator from the
// unit properties'. Module coupling is a property of a whole module, so the
// imports and package-level declarations that reside in no unit count here even
// though they are excluded from unit size.
func TestModuleLOCIncludesLinesOutsideUnits(t *testing.T) {
	idx := load(t, map[string]string{
		"a/a.go": `package a

import "fmt"

// Registry is a package-level declaration: no unit contains it.
var Registry = map[string]int{
	"one": 1,
	"two": 2,
}

// F is the only unit in this module.
func F() string { return fmt.Sprint(Registry) }
`,
	}, DefaultConfig())

	mods := modules(t, idx, Components{})
	if len(mods) != 1 {
		t.Fatalf("got %d modules, want 1", len(mods))
	}
	set := units(t, idx)
	unitLOC := 0
	for _, u := range set.Units {
		unitLOC += u.LOC
	}
	if mods[0].LOC <= unitLOC {
		t.Errorf("module LOC %d is not above unit LOC %d; the lines residing in no unit "+
			"are missing from the module denominator", mods[0].LOC, unitLOC)
	}
}

// TestModuleCouplingCountsConvention holds that the config key selects which of
// the two readings of the model's wording the caps are applied to, and that both
// numbers are measured either way.
func TestModuleCouplingCountsConvention(t *testing.T) {
	mods := []Module{
		// One module, referenced by 6 others but 30 times in total. Under
		// "modules" it is low risk; under "references" it is very high.
		{File: "a.go", LOC: 100, InModules: 6, InRefs: 60},
	}

	set := UnitSet{Language: "test", Units: []Unit{unit(10, 1, 0)}, Modules: mods,
		ModuleMetrics: []string{moduleMetricCoupling}}
	byModules := profileOf(set, true, moduleCouplingCountsModules)
	byRefs := profileOf(set, true, moduleCouplingCountsReferences)

	lowBand := func(l LanguageProfile) CategoryShare {
		t.Helper()
		if len(l.ModuleProperties) != 1 {
			t.Fatalf("got %d module properties, want 1", len(l.ModuleProperties))
		}
		return band(t, l.ModuleProperties[0], "low")
	}

	if got := lowBand(byModules); got.Elements != 1 {
		t.Errorf("counting modules: 6 incoming modules must land in the low band, got %d there", got.Elements)
	}
	if got := lowBand(byRefs); got.Elements != 0 {
		t.Errorf("counting references: 60 incoming references must not land in the low band")
	}
	if got := byRefs.ModuleProperties[0].Metric; !strings.Contains(got, "references") {
		t.Errorf("metric = %q; the report has to say which of the two readings it applied", got)
	}
}

// TestUnmeasuredModulesAreNotAPass is the module-level counterpart of
// TestUnmeasuredLanguageIsNotAPass. TypeScript supplies no modules today, and a
// language with no modules has a zero denominator: three caps against 0% would
// render as three passes for a surface nobody measured.
func TestUnmeasuredModulesAreNotAPass(t *testing.T) {
	l := profileOf(UnitSet{Language: LanguageTypeScript, Units: []Unit{unit(10, 1, 0)}},
		true, defaultModuleCouplingCounts)

	if l.ModulesMeasured {
		t.Error("a language with no modules must not be reported as measured")
	}
	if len(l.ModuleProperties) != 0 {
		t.Errorf("got %d module property profiles for a language with no modules; it must produce "+
			"none, because every percentage would be 0%% and read as a pass", len(l.ModuleProperties))
	}

	b := &strings.Builder{}
	writeModules(b, l, Deviations{})
	if !strings.Contains(b.String(), "Not measured") {
		t.Errorf("the report must say the property is absent for %s, got:\n%s", l.Language, b.String())
	}
	if strings.Contains(b.String(), "0.0%") {
		t.Error("an unmeasured property must not render a percentage at all")
	}
}

// TestExcessLOCIsTheWorkNotTheVerdict pins the distance-to-cap arithmetic. A
// verdict says a tail failed; this says how much has to move for it not to.
func TestExcessLOCIsTheWorkNotTheVerdict(t *testing.T) {
	// 400 of 1000 unit lines sit in units over 30 lines, against a cap of 23.1%.
	// 400 - 231 = 169.
	units := []Unit{unit(40, 1, 0), unit(40, 1, 0), unit(40, 1, 0), unit(40, 1, 0),
		unit(40, 1, 0), unit(40, 1, 0), unit(40, 1, 0), unit(40, 1, 0), unit(40, 1, 0), unit(40, 1, 0)}
	for range 60 {
		units = append(units, unit(10, 1, 0))
	}

	p := profileFor(t, "unit_size", units)
	high := tail(t, p, "high")
	if high.Pass {
		t.Fatalf("tail at %.1f%% should fail its %.1f%% cap", high.Pct, high.CapPct)
	}
	if high.ExcessLOC != 169 {
		t.Errorf("ExcessLOC = %d, want 169: 400 lines in the tail against 23.1%% of 1000", high.ExcessLOC)
	}

	// A passing tail carries no figure: there is no work to do, and printing a
	// negative number of lines would read as one.
	low := tail(t, p, "moderate")
	if !low.Pass {
		t.Fatalf("tail at %.1f%% should pass its %.1f%% cap", low.Pct, low.CapPct)
	}
	if low.ExcessLOC != 0 {
		t.Errorf("ExcessLOC = %d on a passing tail, want 0", low.ExcessLOC)
	}
}

// TestSIGModulesCSVLabelsEveryModule covers the module-level audit view, the
// counterpart of TestSIGUnitsCSVLabelsEveryUnit. The report states two module
// properties as percentages, and a percentage nobody can re-derive is a number
// a reader has to trust; this dump is what makes both of them checkable.
//
// The row carries both counts even though only one drives the band, which is
// the point of the file. A module referenced by 6 others 60 times is low risk
// under one reading of the model's wording and very high under the other, so a
// row showing only the band would hide that a choice was made at all.
func TestSIGModulesCSVLabelsEveryModule(t *testing.T) {
	out := &strings.Builder{}
	err := WriteSIGModules(out, []UnitSet{{
		Language: LanguageGo,
		Modules: []Module{
			// Nothing outside its own component leans on it, so it is hidden.
			// 6 incoming modules is low; the 60 references are not what the
			// default convention bands on.
			{File: "api/internal/store/store.go", Component: "store", LOC: 200,
				InModules: 6, InRefs: 60, InCrossComponent: 0},
			// Referenced from three other components, so exposed, and 25
			// incoming modules puts it in the third band.
			{File: "api/internal/server/wiring.go", Component: "server", LOC: 90,
				InModules: 25, InRefs: 25, InCrossComponent: 3},
		},
	}})
	if err != nil {
		t.Fatalf("WriteSIGModules: %v", err)
	}
	got := out.String()

	for _, want := range []string{
		"language,file,component,loc,in_modules,in_refs,in_cross_component,hidden,module_coupling_category",
		"Go,api/internal/store/store.go,store,200,6,60,0,true,low",
		"Go,api/internal/server/wiring.go,server,90,25,25,3,false,high",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("CSV missing %q\ngot:\n%s", want, got)
		}
	}
	// The negative half of the hidden column: a module referenced across a
	// component boundary must not be labelled hidden, because the independence
	// percentage is the share of lines that are.
	if strings.Contains(got, ",3,true,") {
		t.Errorf("a module with cross-component references was labelled hidden:\n%s", got)
	}
}

// TestSIGModulesCSVBandsTheConventionInForce is the other half of the row
// above. The category column has to follow `module_coupling_counts`, or the
// audit view re-derives a different percentage from the one the report printed
// and the disagreement looks like a measurement bug.
func TestSIGModulesCSVBandsTheConventionInForce(t *testing.T) {
	sets := []UnitSet{{
		Language: LanguageGo,
		Modules:  []Module{{File: "a.go", LOC: 100, InModules: 6, InRefs: 60}},
	}}

	out := &strings.Builder{}
	if err := WriteSIGModules(out, sets); err != nil {
		t.Fatalf("WriteSIGModules: %v", err)
	}
	// The scanner's default reading is modules, so 6 lands in the low band and
	// the 60 references are recorded beside it without banding it.
	if got := out.String(); !strings.Contains(got, ",6,60,0,true,low") {
		t.Errorf("the default convention bands on incoming modules, so 6 must read low:\n%s", got)
	}
	if got := out.String(); strings.Contains(got, "very high") {
		t.Errorf("the 60 incoming references were banded; the default convention counts "+
			"modules:\n%s", out.String())
	}
}

// TestQualifyPathReportsFromTheRepositoryRoot covers the small piece of glue
// every module-level path goes through. The scanner walks a subtree and knows
// paths relative to it; a reader opens the file from the repository root. A
// missing prefix is not a wrong-looking path, it is one that does not open.
func TestQualifyPathReportsFromTheRepositoryRoot(t *testing.T) {
	cases := []struct {
		prefix, rel, want string
		why               string
	}{
		{"api", "internal/store/store.go", "api/internal/store/store.go", "the ordinary case"},
		// The flag is written by hand, so a trailing slash is a spelling of the
		// same prefix rather than a request for a doubled separator.
		{"api/", "internal/store/store.go", "api/internal/store/store.go", "a trailing slash is absorbed"},
		// Scanning the repository root itself supplies no prefix, and the path
		// is already the one to report.
		{"", "internal/store/store.go", "internal/store/store.go", "no prefix leaves the path alone"},
	}
	for _, c := range cases {
		if got := qualifyPath(c.prefix, c.rel); got != c.want {
			t.Errorf("qualifyPath(%q, %q) = %q, want %q (%s)", c.prefix, c.rel, got, c.want, c.why)
		}
	}
}
