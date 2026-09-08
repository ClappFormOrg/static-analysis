package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// The SIG risk-profile distribution.
//
// The rest of this tool reimplements the external vendor's best-practice rules,
// which report violation COUNTS against a single gate threshold. That is not how
// the SIG maintainability model works, and reading a gate count as if it were a
// SIG measure is a specific mistake worth carrying a caveat about wherever both
// kinds of number appear in one report.
//
// The model bins every unit into four risk categories and caps the share of code
// VOLUME allowed in each. A count cannot answer it: forty one-line units at very
// high complexity are a rounding error against a large codebase, and one
// 900-line unit is not, yet a count ranks the forty as forty times worse.
//
// Everything here is volume-weighted for that reason: a unit contributes its own
// lines of code, and every percentage is a share of the total lines residing in
// units.
const (
	// SIGModel is the source of record for every threshold in this file. Quoted
	// in the report so a reader can check the numbers against the document
	// rather than trusting the code.
	SIGModel = "SIG/TUViT Evaluation Criteria Trusted Product Maintainability"
	// SIGModelVersion is the edition these caps come from.
	SIGModelVersion = "v17.0 (2025-03-12)"
	// SIGModelChecked is when a person last confirmed that edition is still
	// the current one. The version constant says which document the caps come
	// from; it cannot say whether a newer document exists, and SIG
	// recalibrates its benchmark yearly, so a cap can move with no change in
	// what the metric means.
	//
	// Last check: the public Guidance for Producers PDF still carried
	// "Version 17.0 (March 12, 2025)" and every threshold here matched it. SIG's
	// own 2026 quality model (2026-05-01) removed the Volume property and moved
	// property aggregation to a power mean, neither of which touches a cap in
	// this file.
	SIGModelChecked = "2026-08-28"
	// SIGStarTarget is the certification level the caps below are for. The
	// model publishes a cap per star level; this targets 4.
	//
	// A target, and deliberately not a gate: nothing here blocks on these
	// numbers, and the report says so. The distinction matters for how a fail
	// reads. A codebase that has never measured itself against the model will be
	// over several caps at once, and treating that as a breach would make the
	// profile something to argue with rather than something to steer by;
	// treating it as a distance is what makes
	// the LOC-to-move figures the useful half of the report.
	SIGStarTarget = 4

	// LanguageGo and LanguageTypeScript are the two surfaces. A .vue script
	// block reports as TypeScript, matching the convention clone detectors use,
	// where only a .vue template counts as a separate language.
	LanguageGo         = "Go"
	LanguageTypeScript = "TypeScript"

	// moduleCouplingCountsModules and moduleCouplingCountsReferences are the
	// two readings of "incoming dependency" the model's wording allows. See
	// Config.ModuleCouplingCounts.
	moduleCouplingCountsModules    = "modules"
	moduleCouplingCountsReferences = "references"
	defaultModuleCouplingCounts    = moduleCouplingCountsModules

	// combinedLabel names the cross-language roll-up. The model's caps apply
	// "for each programming language used", so this row carries no verdict; see
	// SIGReport.
	combinedLabel = "combined (informational)"

	// unbounded is the open upper end of the very-high category.
	unbounded = math.MaxInt

	// atCapMargin is how close a measured percentage has to be to its cap
	// before the report says so. Verdicts compare full precision but the report
	// prints one decimal, so without this a tail of 47.14% would render as
	// "47.1% against 47.1%" and read as a pass while failing.
	atCapMargin = 0.05
)

// Unit is one measured unit: the smallest NAMED piece of executable code, which
// is the model's own definition. "Named" is load-bearing -- see sigProperties.
type Unit struct {
	Element string `json:"element"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	// LOC is lines of code in the unit's OWN body: lines belonging to a nested
	// unit that is itself named are not counted here, they are counted there.
	LOC    int `json:"loc"`
	McCabe int `json:"mccabe"`
	Params int `json:"params"`
}

// UnitSet is one language's measurements. The Go set is built by goUnits; other
// languages arrive as JSON through -units, emitted by a front end that knows how
// to parse them. See LoadUnitSets for the contract.
type UnitSet struct {
	Language string `json:"language"`
	Root     string `json:"root"`
	// NonUnitLOC is lines of code that reside in no unit at all: imports, type
	// declarations, package-level var blocks, the top level of a <script setup>.
	// Reported rather than folded in, because the caps are about code residing
	// IN units, and because a profile that silently covers a third of the tree
	// looks exactly like one that covers all of it. That is not hypothetical: a
	// vendor export has been found to have read a fraction of a tree's files,
	// with only its volume metrics giving it away.
	NonUnitLOC   int    `json:"non_unit_loc"`
	FilesScanned int    `json:"files_scanned"`
	Units        []Unit `json:"units"`
	// Modules is the same surface measured at the model's module level, which
	// is one file. It is optional: a front end that cannot resolve a reference
	// to the file declaring it cannot produce these, and module coupling is
	// then reported as not measured for that language rather than as zero.
	Modules []Module `json:"modules,omitempty"`
	// Components is the date the component boundary behind Modules was
	// decided. Empty means no map was supplied and component independence is
	// not measured for this language.
	Components string `json:"components_decided,omitempty"`
	// ModuleMetrics names the module-level properties this front end actually
	// measured. It is what separates "this language has no coupling" from
	// "nobody measured this language's coupling", and the two have to be
	// separable: modules supplied for duplication alone carry no incoming
	// edges, so binning them for coupling would report every line in the low
	// band and print three passes for a property nothing computed.
	//
	// LoadUnitSets rejects an unknown entry and a claim the data does not
	// back, so a front end cannot assert a property it did not measure.
	ModuleMetrics []string `json:"module_metrics,omitempty"`
	// ComponentEdges is the component graph: every dependency crossing from
	// one component to another, collapsed into one weighted line per pair.
	// Component entanglement is measured over it.
	ComponentEdges []ComponentEdge `json:"component_edges,omitempty"`
}

// Measures reports whether this set claims to have measured the named
// module-level property.
func (s UnitSet) Measures(metric string) bool {
	return slices.Contains(s.ModuleMetrics, metric)
}

// Category is one of the model's four risk bands. Max is inclusive; the very
// high band carries unbounded.
type Category struct {
	Name string
	Min  int
	Max  int
}

// Range renders the band the way the model's own figures label it.
func (c Category) Range() string {
	if c.Max == unbounded {
		return fmt.Sprintf("> %d", c.Min-1)
	}
	return fmt.Sprintf("%d-%d", c.Min, c.Max)
}

// Tail is one published cap: the share of unit lines of code allowed to sit in
// units at or beyond Threshold.
//
// The three tails of a property are NESTED, not disjoint. A 70-line unit counts
// in all three unit-size tails, so the tail percentages do not sum to 100 and
// must never be presented as a partition.
type Tail struct {
	Threshold int
	// Inclusive makes the tail >= Threshold rather than > Threshold. Unit
	// interfacing is worded inclusively in the model ("units with 3 or more
	// parameters") where unit size and unit complexity are worded exclusively
	// ("more than 15 lines of code"). That one word is why the parameter
	// categories below break at 3/5/7 and not at 4/6/8.
	Inclusive bool
	// CapPct is the maximum percentage of unit lines of code for SIGStarTarget.
	CapPct float64
}

// Holds reports whether a measurement falls in the tail.
func (t Tail) Holds(v int) bool {
	if t.Inclusive {
		return v >= t.Threshold
	}
	return v > t.Threshold
}

// Label renders the tail as the model words it.
func (t Tail) Label() string {
	if t.Inclusive {
		return fmt.Sprintf(">= %d", t.Threshold)
	}
	return fmt.Sprintf("> %d", t.Threshold)
}

// Property is one of the model's system properties: the risk bands it is
// measured in, the caps on its tails, and how a report should name it.
//
// A property does not carry what it measures over. Unit size bins units and
// module coupling bins modules, so the measurement function lives on the
// element-specific wrapper below and every binner here works on Elements. One
// binning implementation covers every property, which is what stops a new
// property's tails being cumulative in a subtly different way from the rest.
type Property struct {
	ID   string
	Name string
	// Metric names what is counted, for the report.
	Metric string
	// SubChars are the ISO 25010 sub-characteristics this property feeds, so a
	// generated row can be pasted into a scorecard's metric table unchanged.
	SubChars string
	// ElementNoun is what one measured thing is called, for the table header:
	// "Units" for the three unit properties, "Modules" for module coupling.
	ElementNoun string
	// Categories run low to very high. Tails run alongside categories 1..n: the
	// tail at index i starts at the category at index i+1, which is why they can
	// share a table row. That correspondence is exact by construction and
	// TestTailsAlignWithCategories holds it that way.
	Categories []Category
	Tails      []Tail
}

// Element is one measured thing reduced to the two numbers binning needs: the
// value of the property's metric, and the lines of code it contributes. Every
// percentage in this file is a share of code volume, so an element handed over
// without its LOC cannot be binned at all.
type Element struct {
	Value int
	LOC   int
}

// UnitProperty is a property measured over units.
type UnitProperty struct {
	Property
	// Measure pulls this property's number off a unit.
	Measure func(Unit) int
}

// elements reduces units to the pairs the binner needs.
func (p UnitProperty) elements(units []Unit) []Element {
	out := make([]Element, 0, len(units))
	for _, u := range units {
		out = append(out, Element{Value: p.Measure(u), LOC: u.LOC})
	}
	return out
}

// sigProperties are the three unit-level properties, with the boundaries and the
// 4-star caps quoted from SIGModel SIGModelVersion.
//
// Two of these metrics are NOT the ones the vendor-parity rules in report.go
// measure, and the difference is deliberate rather than an inconsistency:
//
//   - Unit size here is lines of code. FUNCTION_SIZE_RISK counts lexical tokens,
//     because that is what the vendor counts and that rule is calibrated against
//     vendor exports to a median ratio of 1.000.
//   - Unit complexity here is McCabe. FUNCTION_COMPLEXITY_RISK reports cognitive
//     complexity, which README.md documents at length as not reconcilable with
//     the vendor's own complexity number.
//
// So both live side by side. Nothing in config.go moves: this profile adds a
// measurement, it does not retune an existing one.
func sigProperties() []UnitProperty {
	return []UnitProperty{{
		Property: Property{
			ID:          "unit_size",
			Name:        "Unit size",
			Metric:      "lines of code per unit",
			SubChars:    "Analysability",
			ElementNoun: "Units",
			Categories: []Category{
				{"low", 1, 15}, {"moderate", 16, 30}, {"high", 31, 60}, {"very high", 61, unbounded},
			},
			Tails: []Tail{
				{Threshold: 15, CapPct: 47.1},
				{Threshold: 30, CapPct: 23.1},
				{Threshold: 60, CapPct: 8.3},
			},
		},
		Measure: func(u Unit) int { return u.LOC },
	}, {
		Property: Property{
			ID:          "unit_complexity",
			Name:        "Unit complexity",
			Metric:      "McCabe cyclomatic complexity",
			SubChars:    "Modifiability, Testability",
			ElementNoun: "Units",
			Categories: []Category{
				{"low", 1, 5}, {"moderate", 6, 10}, {"high", 11, 25}, {"very high", 26, unbounded},
			},
			Tails: []Tail{
				{Threshold: 5, CapPct: 20.2},
				{Threshold: 10, CapPct: 7.3},
				{Threshold: 25, CapPct: 1.1},
			},
		},
		Measure: func(u Unit) int { return u.McCabe },
	}, {
		Property: Property{
			ID:          "unit_interfacing",
			Name:        "Unit interfacing",
			Metric:      "declared parameters",
			SubChars:    "Modularity, Modifiability",
			ElementNoun: "Units",
			Categories: []Category{
				{"low", 0, 2}, {"moderate", 3, 4}, {"high", 5, 6}, {"very high", 7, unbounded},
			},
			Tails: []Tail{
				{Threshold: 3, Inclusive: true, CapPct: 15.0},
				{Threshold: 5, Inclusive: true, CapPct: 3.3},
				{Threshold: 7, Inclusive: true, CapPct: 0.9},
			},
		},
		Measure: func(u Unit) int { return u.Params },
	}}
}

// TailShare is a measured cumulative tail against its cap.
type TailShare struct {
	Label string
	// Elements is how many measured things sit in the tail: units for a unit
	// property, modules for module coupling.
	Elements int
	LOC      int
	Pct      float64
	CapPct   float64
	// Applies is false on the combined roll-up, where a cap defined per
	// language has no meaning.
	Applies bool
	Pass    bool
	// AtCap marks a tail within atCapMargin of its cap, so a reader is never
	// left guessing whether a displayed equality passed.
	AtCap bool
	// ExcessLOC is how many lines have to leave this tail for it to reach its
	// cap, and is zero on a passing tail. It answers the question a verdict
	// raises and does not settle: a tail well over its cap is not "fail", it is
	// a quantity of extraction work.
	//
	// The denominator is held fixed, which makes this a first-order estimate
	// rather than an identity. Splitting a 90-line unit into three 30-line ones
	// moves 90 lines out of the "> 60" tail and leaves total unit LOC where it
	// was, so the estimate is close for the move it describes; a move that
	// deletes code instead beats it.
	ExcessLOC int
}

// CategoryShare is one risk band's measured share of lines of code.
type CategoryShare struct {
	Name  string
	Range string
	// Elements is how many measured things fall in this band.
	Elements int
	LOC      int
	Pct      float64
	// Tail is the cumulative cap beginning at this band. The low band has none:
	// the model caps the tails, not the bucket everything should be in.
	Tail *TailShare
}

// PropertyProfile is one property's full distribution for one language.
type PropertyProfile struct {
	Property
	Categories []CategoryShare
}

// LanguageProfile is every property for one language.
type LanguageProfile struct {
	Language     string
	FilesScanned int
	Units        int
	UnitLOC      int
	NonUnitLOC   int
	// Measured is false when the language contributed no units. Such a language
	// has a zero denominator, and reporting 0.0% against every cap would print
	// nine passes for a surface nobody measured -- the worst failure available
	// here, and the one the vendor export actually shipped. So it reports "not
	// measured" instead and the run exits non-zero.
	Measured bool
	// Verdicts is false on the combined roll-up.
	Verdicts   bool
	Properties []PropertyProfile

	// Modules, ModuleLOC and ModuleProperties carry the module-level half of
	// the model: module coupling is a share of lines residing in modules, not
	// of lines residing in units, so it has its own denominator.
	Modules          int
	ModuleLOC        int
	ModulesMeasured  bool
	ModuleProperties []PropertyProfile
	// Independence is nil when no component map was supplied, which the
	// report states rather than leaving the property out.
	Independence *IndependenceProfile
	// Duplication is nil when no module supplied its normalised lines.
	Duplication *DuplicationProfile
	// Entanglement is nil without a component graph, and carries no verdict
	// even when present. See entanglement.go.
	Entanglement *EntanglementProfile
}

// MeasuredLOC is every line this language accounted for, in a unit or not. The
// denominator for the profile is UnitLOC; this is the number that says how much
// of the surface the profile speaks for.
func (l LanguageProfile) MeasuredLOC() int { return l.UnitLOC + l.NonUnitLOC }

// NonUnitPct is the share of measured lines residing in no unit.
func (l LanguageProfile) NonUnitPct() float64 {
	return pct(l.NonUnitLOC, l.MeasuredLOC())
}

// SIGReport is the whole distribution: one profile per language, plus the
// cross-language roll-up.
type SIGReport struct {
	Rev       string
	Languages []LanguageProfile
	Combined  LanguageProfile
}

// Unmeasured names the languages that produced no units.
func (r SIGReport) Unmeasured() []string {
	var out []string
	for _, l := range r.Languages {
		if !l.Measured {
			out = append(out, l.Language)
		}
	}
	return out
}

// BuildSIGProfile bins every unit of every set and computes the shares.
//
// Sets are ordered by language name so two runs of the same tree produce
// byte-identical reports and a diff between runs is signal.
func BuildSIGProfile(sets []UnitSet, rev, counts string) SIGReport {
	if counts == "" {
		counts = defaultModuleCouplingCounts
	}
	sorted := append([]UnitSet(nil), sets...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Language < sorted[j].Language })

	report := SIGReport{Rev: rev}
	all := UnitSet{Language: combinedLabel}
	// The roll-up may only claim a property EVERY language measured. One
	// surface supplying modules for duplication alone would otherwise drag
	// its unmeasured coupling into the combined figure as a pile of zeroes.
	first := true
	for _, s := range sorted {
		report.Languages = append(report.Languages, profileOf(s, true, counts))
		all.Units = append(all.Units, s.Units...)
		all.Modules = append(all.Modules, s.Modules...)
		all.ModuleMetrics = intersectMetrics(first, all.ModuleMetrics, s.ModuleMetrics)
		first = false
		if all.Components == "" {
			all.Components = s.Components
		}
		all.NonUnitLOC += s.NonUnitLOC
		all.FilesScanned += s.FilesScanned
	}
	report.Combined = profileOf(all, false, counts)
	return report
}

// intersectMetrics keeps the metrics common to every set folded in so far.
func intersectMetrics(first bool, acc, next []string) []string {
	if first {
		return slices.Clone(next)
	}
	var out []string
	for _, m := range acc {
		if slices.Contains(next, m) {
			out = append(out, m)
		}
	}
	return out
}

func profileOf(s UnitSet, verdicts bool, counts string) LanguageProfile {
	total := 0
	for _, u := range s.Units {
		total += u.LOC
	}
	l := LanguageProfile{
		Language:     s.Language,
		FilesScanned: s.FilesScanned,
		Units:        len(s.Units),
		UnitLOC:      total,
		NonUnitLOC:   s.NonUnitLOC,
		Measured:     len(s.Units) > 0 && total > 0,
		Verdicts:     verdicts,
	}
	if !l.Measured {
		return l
	}
	for _, p := range sigProperties() {
		l.Properties = append(l.Properties, propertyProfileOf(p.Property, p.elements(s.Units), total, verdicts))
	}

	// Module coupling is measured over whole modules, so it is absent for a
	// language whose front end does not resolve references yet. Absent is
	// reported as absent: the alternative is a table of zeroes that reads as
	// nine passes, which is the failure LanguageProfile.Measured exists to
	// prevent one level up.
	moduleLOC := 0
	for _, m := range s.Modules {
		moduleLOC += m.LOC
	}
	l.Modules = len(s.Modules)
	l.ModuleLOC = moduleLOC
	l.ModulesMeasured = len(s.Modules) > 0 && moduleLOC > 0 && s.Measures(moduleMetricCoupling)
	if l.ModulesMeasured {
		for _, p := range moduleProperties(counts) {
			l.ModuleProperties = append(l.ModuleProperties, propertyProfileOf(p.Property, p.elements(s.Modules), moduleLOC, verdicts))
		}
		// Component independence needs a boundary, and a module only carries
		// one when a map was supplied. Without it the property is absent, not
		// zero: an invented boundary produces a real-looking percentage that
		// nothing downstream could tell apart from a measured one.
		if s.Components != "" {
			l.Independence = measureIndependence(s.Modules, s.Components, verdicts)
			l.Entanglement = measureEntanglement(s.ComponentEdges)
		}
	}
	if s.Measures(moduleMetricDuplication) {
		l.Duplication = measureDuplication(s.Modules, verdicts)
	}
	return l
}

func propertyProfileOf(p Property, els []Element, total int, verdicts bool) PropertyProfile {
	out := PropertyProfile{Property: p}
	for i, c := range p.Categories {
		share := CategoryShare{Name: c.Name, Range: c.Range()}
		for _, e := range els {
			if e.Value >= c.Min && e.Value <= c.Max {
				share.Elements++
				share.LOC += e.LOC
			}
		}
		share.Pct = pct(share.LOC, total)

		// The tail at index i-1 starts at this category, so the cumulative cap
		// and the band share the row.
		if i > 0 && i-1 < len(p.Tails) {
			t := p.Tails[i-1]
			share.Tail = tailShareOf(t, els, total, verdicts)
			// The open-ended band and its tail are the same set, so label it
			// the way the model words that tail -- "> 60" for unit size,
			// ">= 7" for parameters. Deriving it from Min-1 instead would
			// render the parameter band as "> 6" beside a ">= 7" tail and read
			// as an off-by-one.
			if c.Max == unbounded {
				share.Range = t.Label()
			}
		}
		out.Categories = append(out.Categories, share)
	}
	return out
}

func tailShareOf(t Tail, els []Element, total int, verdicts bool) *TailShare {
	ts := &TailShare{Label: t.Label(), CapPct: t.CapPct, Applies: verdicts}
	for _, e := range els {
		if t.Holds(e.Value) {
			ts.Elements++
			ts.LOC += e.LOC
		}
	}
	ts.Pct = pct(ts.LOC, total)
	// Compared at full precision, printed to one decimal. A tail of 47.14%
	// fails a 47.1% cap even though the two render identically, which is what
	// AtCap exists to disclose.
	ts.Pass = ts.Pct <= t.CapPct
	ts.AtCap = math.Abs(ts.Pct-t.CapPct) <= atCapMargin
	if !ts.Pass {
		// Rounded up: half a line cannot leave a tail, and rounding down would
		// report a figure that still breaches the cap once it is done.
		ts.ExcessLOC = int(math.Ceil(float64(ts.LOC) - t.CapPct/100*float64(total)))
	}
	return ts
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
}

// goUnits measures the Go surface. Only top-level function and method
// declarations are units: a Go function literal is anonymous even when it is
// assigned to a variable, and the model's unit is the smallest NAMED piece of
// executable code, so a literal's lines and branches bill to the declaration
// enclosing it. mccabeComplexity does the same.
//
// The webapp emitter deliberately does NOT follow that rule for a named arrow
// function, and the asymmetry is the point rather than an oversight. In Go,
// `func` at top level is how you declare a function and a named closure is a
// rare local idiom. In TypeScript, `const f = () => {}` at module scope IS how
// you declare a function, and treating it as anonymous makes a scan almost
// entirely measurement artifact: a Vue composable is itself a factory closure,
// so the whole module body bills to one "unit", the effective very-high
// threshold lands stricter than any enforced budget, and a file already split
// many ways still scores as one giant unit. Extracting a helper cannot improve
// the number, which makes the metric unactionable.
func (idx *Index) goUnits(prefix string, comps Components) (UnitSet, error) {
	qualify := func(rel string) string {
		if prefix == "" {
			return rel
		}
		return strings.TrimSuffix(prefix, "/") + "/" + rel
	}

	set := UnitSet{
		Language:     LanguageGo,
		Root:         idx.Root,
		FilesScanned: len(idx.Files),
	}
	measured, err := idx.measureModules(prefix, comps)
	if err != nil {
		return set, err
	}
	set.Modules = measured.Modules
	set.ComponentEdges = componentEdges(measured.Dependencies)
	set.Components = comps.Decided
	set.ModuleMetrics = []string{moduleMetricCoupling, moduleMetricDuplication}
	unitLOC := map[string]int{}
	for _, m := range idx.measureFuncs() {
		// A declaration with no Go body is an assembly or cgo stub. It is not
		// executable code, so counting it as a 1-line, McCabe-1 unit would
		// dilute every percentage by padding the denominator.
		if !m.HasBody {
			continue
		}
		set.Units = append(set.Units, Unit{
			Element: m.Element,
			File:    qualify(m.File.Rel),
			Line:    m.Line,
			LOC:     m.LOC,
			McCabe:  m.McCabe,
			Params:  m.Params,
		})
		unitLOC[m.File.Rel] += m.LOC
	}
	for _, f := range idx.Files {
		if n := countLOCIn(f.Src) - unitLOC[f.Rel]; n > 0 {
			set.NonUnitLOC += n
		}
	}
	return set, nil
}

// LoadUnitSets reads externally measured units, one set per -units file.
//
// A parse failure is a hard error rather than a skipped file: a language that
// silently vanishes reports nothing, and nothing reads as fine.
func LoadUnitSets(paths []string) ([]UnitSet, error) {
	var out []UnitSet
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read units %s: %w", p, err)
		}
		var s UnitSet
		if err := json.Unmarshal(b, &s); err != nil {
			return nil, fmt.Errorf("parse units %s: %w", p, err)
		}
		if strings.TrimSpace(s.Language) == "" {
			return nil, fmt.Errorf("units %s: no language field, so its numbers cannot be attributed", p)
		}
		if err := validateModuleMetrics(p, s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// validateModuleMetrics rejects a front end claiming a property its data does
// not back. A claim nobody checks is how a surface comes to report three
// passes for something that was never computed, which is the failure the
// unmeasured-language check exists to prevent one level up.
func validateModuleMetrics(path string, s UnitSet) error {
	for _, m := range s.ModuleMetrics {
		switch m {
		case moduleMetricCoupling:
			if len(s.Modules) == 0 {
				return fmt.Errorf("units %s: claims to measure %q but supplies no modules", path, m)
			}
		case moduleMetricDuplication:
			if !slices.ContainsFunc(s.Modules, func(mod Module) bool { return len(mod.Lines) > 0 }) {
				return fmt.Errorf("units %s: claims to measure %q but no module carries its "+
					"normalised lines", path, m)
			}
		default:
			return fmt.Errorf("units %s: unknown module metric %q (want %q or %q)",
				path, m, moduleMetricCoupling, moduleMetricDuplication)
		}
	}
	return nil
}

// WriteSIGProfile renders the distribution as markdown, shaped to be read on its
// own and to be pasted into a dated scorecard's metric table.
func WriteSIGProfile(w io.Writer, r SIGReport, base *Snapshot, devs Deviations) error {
	b := &strings.Builder{}

	fmt.Fprintf(b, "# SIG risk-profile distribution\n\n")
	fmt.Fprintf(b, "Model: %s %s   Target: %d stars   Edition confirmed current: %s",
		SIGModel, SIGModelVersion, SIGStarTarget, SIGModelChecked)
	if r.Rev != "" {
		fmt.Fprintf(b, "   Commit: %s", r.Rev)
	}
	b.WriteString("\n\n")
	b.WriteString(profilePreamble)
	writeMovement(b, base, compareSnapshots(base, snapshotOf(r, "", "")))

	for _, l := range r.Languages {
		writeLanguage(b, l, devs)
	}
	// The roll-up carries no verdicts, so it carries no deviations either: a
	// deviation explains a verdict, and there is none here to explain.
	writeLanguage(b, r.Combined, Deviations{})
	writeVolume(b, measureVolume(r.Languages))

	_, err := io.WriteString(w, b.String())
	return err
}

const profilePreamble = `The three unit properties are shares of **unit lines of code** -- lines residing
in a unit, where a unit is the smallest named piece of executable code. Lines
residing in no unit (imports, type declarations, package-level vars, the top
level of a ` + "`<script setup>`" + `) are reported separately per language and never folded
into that denominator, because those caps are about code residing in units.

Module coupling is a share of **module lines of code** instead: it is a property
of a whole module, so the lines residing in no unit count towards it. The two
denominators reconcile -- module LOC is unit LOC plus non-unit LOC, exactly --
and a test holds them that way.

The three caps of a property are **cumulative and nested**: a 70-line unit counts
in all three unit-size tails. They do not sum to 100%. Each row therefore carries
both its own band's share and the cumulative tail that begins at it.

Verdicts compare full precision; percentages print to one decimal. A tail within
0.05pp of its cap is marked ` + "`at the cap`" + ` so a displayed equality is never
ambiguous. Element counts and line counts sit beside every percentage so the
arithmetic is checkable by inspection.

**LOC to move** is what a failing tail costs to fix: the lines that have to leave
it for the tail to reach its cap, holding the denominator fixed. It is an
estimate of the work, not an identity, and it is blank wherever a tail passes.

Four stars is the **target, not a requirement**. Pass and fail below are the
model's own eligibility wording -- a tail at or under its cap is eligible at four
stars, one over it is not -- and nothing here blocks on them. A
fail is a distance to close, which is what the LOC to move column is for, rather
than a breach to answer for.

`

func writeLanguage(b *strings.Builder, l LanguageProfile, devs Deviations) {
	fmt.Fprintf(b, "## %s\n\n", l.Language)

	if !l.Measured {
		fmt.Fprintf(b, "**Not measured.** No units were found, so there is no denominator and no "+
			"verdict. This is reported rather than shown as 0%% against every cap, because "+
			"0%% would read as nine passes for a surface that was never scanned.\n\n")
		return
	}

	fmt.Fprintf(b, "Files scanned: %d   Units: %d   Unit LOC: %s   Non-unit LOC: %s (%.1f%% of measured LOC)\n\n",
		l.FilesScanned, l.Units, thousands(l.UnitLOC), thousands(l.NonUnitLOC), l.NonUnitPct())

	if !l.Verdicts {
		b.WriteString("The model's caps apply \"for each programming language used\", so this " +
			"roll-up carries **no verdict**. It is here because it is the figure that moves " +
			"when one surface grows relative to the other.\n\n")
	}

	for _, p := range l.Properties {
		writeProperty(b, p, l.Language, devs)
	}

	// Each module-level property renders itself, present or absent. They are
	// called side by side rather than nested inside the coupling section: a
	// language can measure duplication and not coupling, and an early return in
	// one would take the others off the report with it.
	writeModules(b, l, devs)
	writeIndependence(b, l)
	writeEntanglement(b, l)
	writeDuplication(b, l)
}

// writeModules renders the module-level half of the model, or says why it is
// missing. A language whose front end supplies no modules gets the sentence
// rather than a table of zeroes, for the same reason an unmeasured language
// does: zeroes against three caps render as three passes.
func writeModules(b *strings.Builder, l LanguageProfile, devs Deviations) {
	if !l.ModulesMeasured {
		fmt.Fprintf(b, "### Module coupling -- incoming dependencies per module\n\n"+
			"**Not measured for %s.** Module coupling counts the dependencies pointing AT a "+
			"module, which needs every reference resolved to the file that declares it. The Go "+
			"scanner does that; the front end for this language does not, so the property is "+
			"reported as absent rather than as 0%%.\n\n", l.Language)
		return
	}
	fmt.Fprintf(b, "Modules: %d   Module LOC: %s\n\n", l.Modules, thousands(l.ModuleLOC))
	for _, p := range l.ModuleProperties {
		writeProperty(b, p, l.Language, devs)
	}
}

// writeIndependence renders component independence, which is a single share
// against a FLOOR rather than a distribution against caps: more hidden code is
// better, so the verdict compares the other way round from every other
// property in this report.
func writeIndependence(b *strings.Builder, l LanguageProfile) {
	fmt.Fprintf(b, "### Component independence -- share of LOC in modules with no incoming cross-component dependency\n\n")
	if l.Independence == nil {
		b.WriteString("**Not measured.** This property needs the top-level component boundary, " +
			"which the model makes the system owner's scoping decision rather than the " +
			"measurement's. Pass `-components` to supply it. Nothing is assumed here, because " +
			"a guessed boundary produces a percentage that reads exactly like a measured one: " +
			"treating every internal package as its own component reports a hidden share an " +
			"order of magnitude under the floor, which says nothing about the code.\n\n")
		return
	}

	p := l.Independence
	verdict := "n/a"
	if p.Applies {
		verdict = "fail"
		if p.Pass {
			verdict = "pass"
		}
	}
	fmt.Fprintf(b, "Hidden: %s of %s LOC (%.1f%%)   4-star floor: >= %.1f%%   Verdict: %s",
		thousands(p.HiddenLOC), thousands(p.TotalLOC), p.Pct, p.FloorPct, verdict)
	if p.ExposedLOC > 0 && p.Applies {
		fmt.Fprintf(b, "   LOC to hide: %s", thousands(p.ExposedLOC))
	}
	fmt.Fprintf(b, "\n\nComponent boundary decided %s; see components.json for what each component is and why.\n\n", p.Decided)

	b.WriteString("| Component | Modules | LOC | Exposed LOC | Exposed % |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, c := range p.Components {
		fmt.Fprintf(b, "| %s | %d | %s | %s | %.1f%% |\n",
			c.Name, c.Modules, thousands(c.LOC), thousands(c.Exposed), c.ExposedPct)
	}
	b.WriteString("\nOne row per component, worst first. `-format sigmodules` lists every module " +
		"with the counts behind these figures.\n\n")
}

// writeEntanglement renders the component graph, the two figures the model
// combines, and the lines it judges unintended. It prints NO verdict, and says
// so: the published cap is on a score normalised by benchmark maxima SIG does
// not publish, so there is nothing here to compare against 0.077.
func writeEntanglement(b *strings.Builder, l LanguageProfile) {
	fmt.Fprintf(b, "### Component entanglement -- communication density and violation degree\n\n")
	if l.Entanglement == nil {
		b.WriteString("**Not measured.** This needs the component graph, which needs both a " +
			"component map (`-components`) and a front end that resolves references.\n\n")
		return
	}

	p := l.Entanglement
	fmt.Fprintf(b, "Communication lines: %d across %d connected components   "+
		"Density: %.2f   Violation degree: %.3f (%s of %s dependency weight)\n\n",
		p.Lines, p.Connected, p.Density, p.ViolationDegree,
		thousands(p.ViolationWeight), thousands(p.TotalWeight))
	fmt.Fprintf(b, "**No verdict.** The model combines these two figures by normalising each "+
		"against the maximum in SIG's own benchmark, and those maxima are not published, so the "+
		"%.3f cap cannot be evaluated locally. A number computed without them would be a "+
		"different metric wearing the model's threshold. The two inputs and the violations "+
		"behind them are what this reports.\n\n", entanglementCap)

	b.WriteString("| Communication line | Weight |\n|---|---|\n")
	for _, e := range p.Edges {
		fmt.Fprintf(b, "| %s | %d |\n", e, e.Weight)
	}
	b.WriteString("\n")

	if len(p.Violations) == 0 {
		b.WriteString("No cyclic, indirect cyclic or transitive dependency between components.\n\n")
		return
	}
	b.WriteString("| Violation | Line | Weight | Why |\n|---|---|---|---|\n")
	for _, v := range p.Violations {
		fmt.Fprintf(b, "| %s | %s | %d | %s |\n", v.Kind, v.Edge, v.Weight, v.Detail)
	}
	b.WriteString("\nChecked in that order, and a line can be marked as only one type.\n\n")
}

// writeDuplication renders the share of redundant lines of code. Like
// component independence it is a single figure rather than a distribution, and
// like module coupling it is absent for a language whose front end does not
// supply what it needs.
func writeDuplication(b *strings.Builder, l LanguageProfile) {
	fmt.Fprintf(b, "### Duplication -- redundant lines of code\n\n")
	if l.Duplication == nil {
		fmt.Fprintf(b, "**Not measured for %s.** Duplication compares normalised lines of code, "+
			"and the front end for this language does not supply them. `make duplication-scan` "+
			"covers this surface with jscpd, under a different definition; see README.md.\n\n",
			l.Language)
		return
	}

	p := l.Duplication
	verdict := "n/a"
	if p.Applies {
		verdict = "fail"
		if p.Pass {
			verdict = "pass"
		}
		if p.AtCap {
			verdict += " (at the cap)"
		}
	}
	fmt.Fprintf(b, "Redundant: %s of %s LOC (%.1f%%)   4-star cap: <= %.1f%%   Verdict: %s",
		thousands(p.RedundantLOC), thousands(p.TotalLOC), p.Pct, p.CapPct, verdict)
	if p.ExcessLOC > 0 && p.Applies {
		fmt.Fprintf(b, "   LOC to remove: %s", thousands(p.ExcessLOC))
	}
	fmt.Fprintf(b, "\n\nA fragment counts when at least %d lines of code repeat literally, modulo "+
		"whitespace, anywhere in this language. Every occurrence after the first is redundant, "+
		"so a fragment written three times contributes two copies. This is not jscpd's number "+
		"and the two are not comparable; see README.md.\n\n", p.MinLines)

	if len(p.Modules) == 0 {
		b.WriteString("No fragment repeats.\n\n")
		return
	}

	shown := p.Modules
	if len(shown) > duplicationTableLimit {
		shown = shown[:duplicationTableLimit]
	}
	b.WriteString("| Module | LOC | Redundant LOC | Redundant % |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, m := range shown {
		fmt.Fprintf(b, "| %s | %s | %s | %.1f%% |\n",
			m.File, thousands(m.LOC), thousands(m.RedundantLOC), m.Pct)
	}
	fmt.Fprintf(b, "\n%d of %d modules carrying redundancy shown, worst first. jscpd's report "+
		"(`make duplication-scan`) is where the matching pairs are.\n\n",
		len(shown), p.ModulesWithRedundancy)
}

// writeProperty renders one property's distribution: every risk band, the
// cumulative tail beginning at it, and what it would take to bring a failing
// tail back under its cap.
func writeProperty(b *strings.Builder, p PropertyProfile, language string, devs Deviations) {
	fmt.Fprintf(b, "### %s -- %s\n\n", p.Name, p.Metric)
	fmt.Fprintf(b, "| Risk category | Range | %s | LOC | %% of measured LOC | Cumulative tail | Tail %% | 4-star cap | Verdict | LOC to move |\n", p.ElementNoun)
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")
	for _, c := range p.Categories {
		fmt.Fprintf(b, "| %s | %s | %d | %s | %.1f%% | %s |\n",
			c.Name, c.Range, c.Elements, thousands(c.LOC), c.Pct, tailCells(c.Tail))
	}
	b.WriteString("\n")

	// A deviation renders after the table, never in place of a row in it. The
	// tail above still carries its measured share, its verdict and its LOC to
	// move; this explains why that verdict is the state the system intends.
	for _, c := range p.Categories {
		if c.Tail == nil || !c.Tail.Applies {
			continue
		}
		if d, ok := devs.For(language, p.ID, c.Tail.Label); ok {
			writeDeviation(b, d)
		}
	}
}

// tailCells renders the four cumulative-tail cells of a category row.
func tailCells(t *TailShare) string {
	if t == nil {
		// The low band is the one the model does not cap.
		return "-- | -- | -- | -- | --"
	}
	verdict := "n/a"
	if t.Applies {
		verdict = "fail"
		if t.Pass {
			verdict = "pass"
		}
		if t.AtCap {
			verdict += " (at the cap)"
		}
	}
	move := "--"
	if t.ExcessLOC > 0 && t.Applies {
		move = thousands(t.ExcessLOC)
	}
	return fmt.Sprintf("%s | %.1f%% | <= %.1f%% | %s | %s", t.Label, t.Pct, t.CapPct, verdict, move)
}

// thousands groups digits so a five-figure line count is readable at a glance.
func thousands(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return s
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	return strings.Join(append([]string{s}, parts...), ",")
}

// WriteSIGUnits dumps every measured unit with its three category labels. This
// is the audit and calibration view: exact values, no rounding, one row per
// unit, so a percentage in the report can be re-derived from it.
func WriteSIGUnits(w io.Writer, sets []UnitSet) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()

	props := sigProperties()
	header := []string{"language", "element", "file", "line", "loc", "mccabe", "params"}
	for _, p := range props {
		header = append(header, p.ID+"_category")
	}
	if err := cw.Write(header); err != nil {
		return err
	}

	for _, s := range sets {
		for _, u := range s.Units {
			row := []string{
				s.Language, u.Element, u.File, strconv.Itoa(u.Line),
				strconv.Itoa(u.LOC), strconv.Itoa(u.McCabe), strconv.Itoa(u.Params),
			}
			for _, p := range props {
				row = append(row, categoryOf(p.Property, p.Measure(u)))
			}
			if err := cw.Write(row); err != nil {
				return err
			}
		}
	}
	return cw.Error()
}

// categoryOf names the risk band a measurement falls in.
func categoryOf(p Property, v int) string {
	for _, c := range p.Categories {
		if v >= c.Min && v <= c.Max {
			return c.Name
		}
	}
	// Below the lowest band's Min. Only reachable for a property whose low band
	// starts above zero, i.e. a unit measured at 0 lines or complexity 0, which
	// goUnits filters out. Named rather than left blank so it cannot be read as
	// a missing value.
	return "none"
}
