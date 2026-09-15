// Package dmm implements the Delta Maintainability Model: how much of the
// change a revision made is low-risk change.
//
// Everything else in this repository measures a tree at one revision and
// answers "where does this code stand". That is the question a scorecard asks,
// and it is the wrong question to put in front of a pull request. A branch
// touching forty lines of a file that already carried nine findings inherits
// all nine, and a gate reading the standing number cannot tell the author what
// their own change did.
//
// The model answers the second question directly. Every unit is either low-risk
// or not, on the same boundary the SIG risk categories draw; a change moves
// lines across that boundary in both directions; and the score is the share of
// the movement that went the right way:
//
//	score = good / (good + bad)
//
// 1.0 is a change that only added low-risk code or only removed high-risk code.
// 0.0 is a change that only did the reverse. The model is from di Biase,
// Rastogi, Bruntink and van Deursen, "The Delta Maintainability Model:
// Measuring Maintainability of Fine-Grained Code Changes" (TechDebt 2019), and
// the low-risk boundaries are the ones Alves, Ypma and Visser derived from SIG's
// benchmark, which are the same boundaries the profile in the parent package
// bins its low category on. AlignsWithSIGLowCategory in the parent package's
// tests is what holds those two readings together.
//
// # WHY THIS TAKES TWO INVENTORIES RATHER THAN A DIFF
//
// The published formulation is stated over a commit's changed files. This
// implementation takes the whole measured inventory at two revisions instead,
// and gets the same answer: a file nobody touched has the same risk profile on
// both sides, contributes a delta of zero to both good and bad, and drops out of
// the ratio. So there is no diff to parse, no rename heuristic to tune, and no
// second definition of "changed" to keep in step with the scanner's own scan
// boundary. Two runs of `qualityscan -format sigunits`, one per revision, are
// the entire input.
//
// The cost is that a RENAME reads as a deletion and an addition. Moving a clean
// 200-line file books 200 lines of good change at the new path and 200 lines of
// bad change at the old one, because the model counts a departure of low-risk
// code as bad. That drags the score towards 0.5 rather than to either end, and
// the more of a change is pure movement the closer to 0.5 it lands. Nothing here
// detects a rename, and a gate is the wrong place to argue about one, which is
// why LanguageResult carries the added and removed file counts that make it
// visible to a reader.
//
// # A DELETION IS NOT AUTOMATICALLY GOOD
//
// Removing low-risk code counts as bad change and removing high-risk code counts
// as good. That is the model's own reading and it is deliberate: deleting a
// clean unit is not an improvement in maintainability, it is a loss of clean
// code, and a model that rewarded all deletion would rank `rm -rf` as the best
// possible commit. It does mean a branch whose job is to delete a healthy
// subsystem scores low for an honest reason, and the score should be read
// alongside the LOC figures rather than on its own.
package dmm

import "sort"

// Unit is one measured unit, reduced to what the model reads off it. It is the
// sigunits CSV row: the parent package's Unit plus the language and the file
// that row was attributed to.
type Unit struct {
	Language string
	File     string
	Element  string
	Line     int
	LOC      int
	McCabe   int
	Params   int
}

// Property is one of the three unit properties the model is defined over.
//
// There are three and not eight. The model needs a per-unit risk verdict, and
// only the unit-level properties have one: duplication and the module and
// component properties are measured over other elements entirely, and a
// per-commit reading of them is a different piece of work rather than a fourth
// row in this table.
type Property struct {
	ID   string
	Name string
	// LowRiskMax is the highest value still in the model's low-risk category.
	// A unit at or below it contributes its lines to Profile.Low, and one above
	// it to Profile.High.
	//
	// These are the low category's upper bounds in the SIG risk profile: 15
	// lines, McCabe 5, 2 parameters. Interfacing reads 2 rather than 3 because
	// the model words that property inclusively -- its moderate band opens at
	// "3 or more parameters" where size and complexity open at "more than".
	LowRiskMax int
	// Measure pulls this property's number off a unit.
	Measure func(Unit) int
}

// LowRisk reports whether a unit sits in the low-risk category for this
// property.
func (p Property) LowRisk(u Unit) bool { return p.Measure(u) <= p.LowRiskMax }

// Properties are the three unit properties, low to high in the order the SIG
// profile lists them.
func Properties() []Property {
	return []Property{
		{
			ID: "unit_size", Name: "Unit size", LowRiskMax: 15,
			Measure: func(u Unit) int { return u.LOC },
		},
		{
			ID: "unit_complexity", Name: "Unit complexity", LowRiskMax: 5,
			Measure: func(u Unit) int { return u.McCabe },
		},
		{
			ID: "unit_interfacing", Name: "Unit interfacing", LowRiskMax: 2,
			Measure: func(u Unit) int { return u.Params },
		},
	}
}

// Profile is lines of code split by risk. Both halves are measured in lines
// rather than in units, for the same reason every percentage in the SIG profile
// is: a count ranks forty one-line units above one 900-line unit.
type Profile struct {
	Low  int
	High int
}

// profileOf bins one file's units for one property.
func profileOf(p Property, units []Unit) Profile {
	var prof Profile
	for _, u := range units {
		if p.LowRisk(u) {
			prof.Low += u.LOC
			continue
		}
		prof.High += u.LOC
	}
	return prof
}

// Delta is the churn one change produced, split by direction.
//
// Good is low-risk code that arrived plus high-risk code that left. Bad is the
// reverse. They are lines, and they are not a partition of the churn: a file
// whose units all shrank below the boundary contributes to Good twice, once for
// the low-risk lines that appeared and once for the high-risk lines that went.
type Delta struct {
	Good int
	Bad  int
}

// add folds one file's before-and-after profiles into the running delta.
func (d *Delta) add(before, after Profile) {
	low := after.Low - before.Low
	high := after.High - before.High
	d.Good += max(low, 0) + max(-high, 0)
	d.Bad += max(-low, 0) + max(high, 0)
}

// Churn is every line the change moved in either direction.
func (d Delta) Churn() int { return d.Good + d.Bad }

// Score is the share of the churn that went the right way, and whether there
// was any churn to take a share of.
//
// The second return is load-bearing. A change touching nothing this property
// measures has no score, and the alternatives are both wrong in a way that
// reaches a gate: 0.0 fails every threshold for a change that did no harm, and
// 1.0 passes every threshold for a change nobody measured. Callers report it as
// not applicable.
func (d Delta) Score() (float64, bool) {
	if d.Churn() == 0 {
		return 0, false
	}
	return float64(d.Good) / float64(d.Churn()), true
}

// PropertyResult is one property's delta and score for one language.
type PropertyResult struct {
	Property
	Delta
	// Score is meaningless unless Measured is true.
	Score    float64
	Measured bool
}

// LanguageResult is every property for one language, plus the file movement
// behind it.
type LanguageResult struct {
	Language string
	// FilesTouched is files whose risk profile differs between the two
	// revisions, for any property. Files that only moved lines within one risk
	// band are not counted: the model cannot see them, and claiming them here
	// would make the count disagree with the score it sits next to.
	FilesTouched int
	// FilesAdded and FilesRemoved are files present on only one side. They are
	// broken out because a rename is the one input shape that reliably distorts
	// the score, and it shows up here as one of each.
	FilesAdded   int
	FilesRemoved int
	Properties   []PropertyResult
}

// Report is the whole comparison, one entry per language seen on either side.
type Report struct {
	Languages []LanguageResult
}

// Compare computes the model over two measured inventories.
//
// Languages are kept separate and never rolled up. The risk boundaries are
// calibrated per language in the source benchmark, and a pooled score would let
// a large surface's churn decide a small one's verdict.
func Compare(before, after []Unit) Report {
	beforeByLang := groupByLanguageAndFile(before)
	afterByLang := groupByLanguageAndFile(after)

	var report Report
	for _, lang := range languages(beforeByLang, afterByLang) {
		report.Languages = append(report.Languages, compareLanguage(lang, beforeByLang[lang], afterByLang[lang]))
	}
	return report
}

// compareLanguage runs every property over one language's files.
func compareLanguage(lang string, before, after map[string][]Unit) LanguageResult {
	res := LanguageResult{Language: lang}
	props := Properties()
	deltas := make([]Delta, len(props))

	for _, file := range files(before, after) {
		touched := false
		for i, p := range props {
			b := profileOf(p, before[file])
			a := profileOf(p, after[file])
			if b != a {
				touched = true
			}
			deltas[i].add(b, a)
		}
		if touched {
			res.FilesTouched++
		}
		_, inBefore := before[file]
		_, inAfter := after[file]
		switch {
		case !inBefore:
			res.FilesAdded++
		case !inAfter:
			res.FilesRemoved++
		}
	}

	for i, p := range props {
		score, ok := deltas[i].Score()
		res.Properties = append(res.Properties, PropertyResult{
			Property: p, Delta: deltas[i], Score: score, Measured: ok,
		})
	}
	return res
}

// groupByLanguageAndFile indexes an inventory the way the comparison walks it.
func groupByLanguageAndFile(units []Unit) map[string]map[string][]Unit {
	out := map[string]map[string][]Unit{}
	for _, u := range units {
		byFile, ok := out[u.Language]
		if !ok {
			byFile = map[string][]Unit{}
			out[u.Language] = byFile
		}
		byFile[u.File] = append(byFile[u.File], u)
	}
	return out
}

// languages is every language on either side, sorted so the report is stable.
func languages(before, after map[string]map[string][]Unit) []string {
	seen := map[string]bool{}
	for l := range before {
		seen[l] = true
	}
	for l := range after {
		seen[l] = true
	}
	return sorted(seen)
}

// files is every file on either side, sorted so the report is stable.
func files(before, after map[string][]Unit) []string {
	seen := map[string]bool{}
	for f := range before {
		seen[f] = true
	}
	for f := range after {
		seen[f] = true
	}
	return sorted(seen)
}

func sorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
