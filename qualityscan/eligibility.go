package main

import (
	"fmt"
	"sort"
	"strings"
)

// The standing summary: one line per language saying how far the code is from
// four stars, computed out of the caps already reported below it.
//
// WHY THIS IS A COUNT AND NOT A RATING
//
// The obvious thing to want here is a number like "3.4 stars". It cannot be
// computed from anything published. The criteria document gives ONE cut-point
// per property -- the share allowed at four stars -- and nothing else: no rating
// curve between one star and five, no mapping from properties to ISO 25010
// sub-characteristics, and no aggregation across them. SIG holds those in its
// benchmark. An interpolated rating would therefore be this tool's invention
// wearing the model's name, which is the move components.go and volume.go
// already refuse for the component boundary and the rebuild value.
//
// So the summary says what the document itself defines. The document defines
// eligibility as a conjunction: a product is eligible at four stars when every
// capped measure is within its cap, for every language used. No property
// outranks another and there is no formula combining them, which is why there is
// no weighting to apply -- the worst measure decides. A count of measures within
// cap is that definition restated, and it adds no number the document does not
// already carry.
//
// WHAT COUNTS AS A MEASURE
//
// Every cap that carries a verdict: nine unit tails, three module coupling
// tails, duplication, and component independence. Component entanglement and
// volume are excluded because neither carries a verdict here -- entanglement is
// not evaluable from one repository and volume has no published conversion for
// Go -- and a measure with no verdict cannot be within or over a cap. The
// summary names both rather than leaving a reader to wonder why the total is
// fourteen and not sixteen.
//
// A measure nobody measured is not counted either. The denominator is measures
// with a verdict, so a language with no component map reports "13 of 13" rather
// than "13 of 14 with one mystery", and the line beneath it says what went
// unmeasured.

// Measure is one capped measure and its verdict.
type Measure struct {
	// Property is the property's display name, and Label the tail as the report
	// writes it (">= 3", "> 15"). Together they identify the measure to a
	// reader looking for it in the tables below.
	Property string
	Label    string
	Pct      float64
	CapPct   float64
	Pass     bool
	// Deviated marks a failing measure covered by a recorded deviation. It is
	// held separately from Pass on purpose: a deviation explains a fail, it does
	// not convert it into a pass, and folding the two together would let a
	// summary read as eligible for a product the model does not certify.
	Deviated bool
	// MoveLOC is the lines that have to leave this tail for it to reach its cap,
	// and is zero on a passing measure.
	MoveLOC int
}

// PropertyDistance is one property's cost to bring inside every one of its caps.
//
// It is the MAXIMUM of its tails' LOC-to-move figures, never the sum. The tails
// are nested -- a 70-line unit sits in all three unit-size tails -- so lines
// taken out of the innermost failing tail leave the outer ones at the same time,
// and moving max(excess) lines satisfies all three at once. Adding the three
// would charge the same extraction work up to three times.
type PropertyDistance struct {
	Property string
	MoveLOC  int
}

// Eligibility is one language's standing against the four-star caps.
type Eligibility struct {
	Language string
	Measures []Measure
	// WithinCap, Over and Deviated partition Measures. Deviated counts a subset
	// of Over rather than a fourth state.
	WithinCap int
	Over      int
	Deviated  int
	// Eligible is the document's own definition: every capped measure within its
	// cap. A deviation does not make it true.
	Eligible bool
	// Distances carries one entry per property that has work outstanding,
	// worst first.
	Distances []PropertyDistance
	// Unmeasured names the properties that produced no verdict for this
	// language, so the denominator above can be read without guessing.
	Unmeasured []string
}

// Total is the denominator: measures that carried a verdict.
func (e Eligibility) Total() int { return e.WithinCap + e.Over }

// Standing is every language's eligibility plus the product-level conjunction.
type Standing struct {
	Languages []Eligibility
	// Eligible is true when every language is. The caps apply "for each
	// programming language used", so one surface over a cap puts the product
	// over it.
	Eligible bool
}

// BuildStanding derives the summary from a report that has already been
// computed. It reads the same TailShare values the tables below print, so a
// verdict here and a verdict there cannot disagree.
func BuildStanding(r SIGReport, devs Deviations) Standing {
	s := Standing{Eligible: true}
	for _, l := range r.Languages {
		// The combined roll-up carries no verdicts and neither does an
		// unmeasured language, so neither has a standing to report.
		if !l.Verdicts || !l.Measured {
			continue
		}
		e := languageEligibility(l, devs)
		s.Languages = append(s.Languages, e)
		if !e.Eligible {
			s.Eligible = false
		}
	}
	// No language with a verdict is not an eligible product, it is a run that
	// measured nothing. Unmeasured() fails the run one level up; this refuses to
	// print a pass in the meantime.
	if len(s.Languages) == 0 {
		s.Eligible = false
	}
	return s
}

// languageEligibility is one language's standing: what was measured, then what
// the measurements add up to.
func languageEligibility(l LanguageProfile, devs Deviations) Eligibility {
	e := Eligibility{Language: l.Language, Eligible: true}
	e.Measures, e.Unmeasured = cappedMeasures(l, devs)
	tally(&e)
	return e
}

// cappedMeasures collects every measure that carried a verdict, and names the
// properties that produced none. The two travel together because they are the
// same decision seen from either side: a property is in the denominator or it
// is in the list of things nobody measured, never in neither and never in both.
func cappedMeasures(l LanguageProfile, devs Deviations) ([]Measure, []string) {
	var measures []Measure
	var unmeasured []string

	// The unit properties and module coupling are both tail-binned, so one loop
	// covers them. Coupling's absence is reported by the language rather than by
	// an empty tail list: a front end supplying modules for duplication alone
	// carries no incoming edges, and binning those would print three passes.
	for _, p := range append(append([]PropertyProfile{}, l.Properties...), l.ModuleProperties...) {
		for _, c := range p.Categories {
			if c.Tail == nil || !c.Tail.Applies {
				continue
			}
			_, deviated := devs.For(l.Language, p.ID, c.Tail.Label)
			measures = append(measures, Measure{
				Property: p.Name, Label: c.Tail.Label,
				Pct: c.Tail.Pct, CapPct: c.Tail.CapPct,
				Pass: c.Tail.Pass, Deviated: deviated, MoveLOC: c.Tail.ExcessLOC,
			})
		}
	}
	if !l.ModulesMeasured {
		unmeasured = append(unmeasured, "module coupling")
	}

	if d := l.Duplication; d != nil {
		measures = append(measures, Measure{
			Property: "Duplication", Label: "redundant lines",
			Pct: d.Pct, CapPct: d.CapPct, Pass: d.Pass, MoveLOC: d.ExcessLOC,
		})
	} else {
		unmeasured = append(unmeasured, "duplication")
	}

	// Independence is the one property measured against a FLOOR rather than a
	// cap, so its Pass already carries the reversed comparison and nothing here
	// re-derives it.
	if i := l.Independence; i != nil {
		measures = append(measures, Measure{
			Property: "Component independence", Label: "hidden lines",
			Pct: i.Pct, CapPct: i.FloorPct, Pass: i.Pass, MoveLOC: i.ExposedLOC,
		})
	} else {
		unmeasured = append(unmeasured, "component independence")
	}

	return measures, unmeasured
}

// tally counts the measures and derives the per-property distances, worst
// first. It takes a pointer because the counts and the distances come off one
// pass and splitting them into two functions would mean walking twice to reach
// the same two answers.
func tally(e *Eligibility) {
	worst := map[string]int{}
	var order []string

	for _, m := range e.Measures {
		if m.Pass {
			e.WithinCap++
			continue
		}
		e.Over++
		e.Eligible = false
		if m.Deviated {
			e.Deviated++
		}
		if _, seen := worst[m.Property]; !seen {
			order = append(order, m.Property)
		}
		// The max, not the sum: see PropertyDistance.
		worst[m.Property] = max(worst[m.Property], m.MoveLOC)
	}

	for _, name := range order {
		e.Distances = append(e.Distances, PropertyDistance{Property: name, MoveLOC: worst[name]})
	}
	sort.SliceStable(e.Distances, func(i, j int) bool {
		return e.Distances[i].MoveLOC > e.Distances[j].MoveLOC
	})
}

// writeStanding renders the summary.
func writeStanding(b *strings.Builder, s Standing) {
	b.WriteString("## Where this stands\n\n")
	b.WriteString(standingPreamble)

	if len(s.Languages) == 0 {
		b.WriteString("**No language carried a verdict**, so there is no standing to report.\n\n")
		return
	}

	b.WriteString("| Language | Within cap | Over | Eligible at 4 stars |\n|---|---|---|---|\n")
	for _, e := range s.Languages {
		over := fmt.Sprintf("%d", e.Over)
		if e.Deviated > 0 {
			over = fmt.Sprintf("%d (%d deviated)", e.Over, e.Deviated)
		}
		fmt.Fprintf(b, "| %s | %d of %d | %s | %s |\n",
			e.Language, e.WithinCap, e.Total(), over, yesNo(e.Eligible))
	}
	fmt.Fprintf(b, "\n**Product: %s.** The caps apply for each programming language used, so one "+
		"surface over a cap puts the product over it.\n\n", eligibleWord(s.Eligible))

	for _, e := range s.Languages {
		writeLanguageStanding(b, e)
	}
}

// writeLanguageStanding names what is over, and what it costs.
func writeLanguageStanding(b *strings.Builder, e Eligibility) {
	if len(e.Unmeasured) > 0 {
		fmt.Fprintf(b, "%s: not measured, and so not counted above -- %s.\n\n",
			e.Language, strings.Join(e.Unmeasured, ", "))
	}
	if e.Eligible {
		return
	}

	fmt.Fprintf(b, "%s, over its cap:\n\n", e.Language)
	b.WriteString("| Measure | Tail | Measured | Cap | LOC to move |\n|---|---|---|---|---|\n")
	for _, m := range e.Measures {
		if m.Pass {
			continue
		}
		note := ""
		if m.Deviated {
			note = " (recorded deviation)"
		}
		fmt.Fprintf(b, "| %s%s | %s | %.1f%% | %.1f%% | %s |\n",
			m.Property, note, m.Label, m.Pct, m.CapPct, thousands(m.MoveLOC))
	}

	b.WriteString("\nPer property, the lines that have to move to clear every one of its caps:\n\n")
	for _, d := range e.Distances {
		fmt.Fprintf(b, "- %s: %s\n", d.Property, thousands(d.MoveLOC))
	}
	b.WriteString("\n")
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func eligibleWord(v bool) string {
	if v {
		return "eligible at 4 stars"
	}
	return "not eligible at 4 stars"
}

const standingPreamble = `A count, deliberately, and not a star rating. The criteria publish one
cut-point per property -- the share allowed at four stars -- and no rating curve
between the stars, no mapping onto ISO 25010 sub-characteristics and no
aggregation across properties. An interpolated rating would be this tool's
invention under the model's name.

What the document does define is eligibility, and it defines it as a
conjunction: every capped measure within its cap, for every language used. No
property outranks another and there is no formula combining them, so there is no
weighting to apply and the worst measure decides. The count below is that
definition restated.

Component entanglement and volume carry no verdict here -- entanglement is not
evaluable from one repository, volume has no published conversion for Go -- so
neither is counted. A measure with no verdict cannot be within a cap or over
one.

**LOC to move** per property is the MAXIMUM of its tails, never their sum. The
tails are nested, so lines taken out of the innermost failing tail leave the
outer ones at the same time. There is no total across properties for the same
reason in reverse: one 90-line unit at McCabe 30 is in the excess of two
different properties, and one extraction fixes both, so adding them would charge
that work twice.

`
