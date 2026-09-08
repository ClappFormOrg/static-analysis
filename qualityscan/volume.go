package main

import (
	"fmt"
	"sort"
	"strings"
)

// Volume: how much code there is, and why it carries no verdict here.
//
// The model estimates a product's rebuild value in person-years from its lines
// of code, normalising each language by an industry average, and four stars
// needs the total to stay at or below 3.9 person-years. Two things stop that
// being a verdict this tool can print:
//
//   - The published conversion table has no Go row. Go is a little over half
//     the measured code here, so the product total cannot be computed at all,
//     and a total missing its largest term is worse than no total.
//   - The property is system-level, not per-language: it is one rebuild value
//     for the whole product. A per-language verdict would be a different
//     measurement wearing this one's threshold.
//
// SIG's own 2026 quality model (2026-05-01) removed Volume outright, on the
// grounds that system size is a risk indicator without an action attached to it.
// The TUViT criteria this tool measures against still carry it, so it is
// reported; it is reported as a figure rather than as a judgement.

// volumeYears is the person-years a 4-star product may be worth rebuilding.
const volumeYears = 3.9

// volumeLinesPerBudget is the model's own table: lines of code produced on
// average in volumeYears person-years, per language. Quoted verbatim from
// SIGModel SIGModelVersion, section 3.1. Go is absent from the source, which is
// the whole reason this property carries no verdict.
func volumeLinesPerBudget() map[string]int {
	return map[string]int{
		"C++":        32000,
		"C#":         39000,
		"Java":       35100,
		"JavaScript": 36300,
		"Python":     25000,
		"Ruby":       24600,
		"TypeScript": 31600,
	}
}

// VolumeShare is one language's contribution to the product's size.
type VolumeShare struct {
	Language string
	Files    int
	LOC      int
	// LinesPerBudget is the model's figure for this language, or zero when the
	// table does not name it.
	LinesPerBudget int
	// Years is what this language alone would cost to rebuild, in person-years,
	// and is zero when the language has no published factor.
	Years float64
}

// Rated reports whether the model publishes a conversion for this language.
func (v VolumeShare) Rated() bool { return v.LinesPerBudget > 0 }

// measureVolume turns the per-language line counts into the model's terms.
func measureVolume(langs []LanguageProfile) []VolumeShare {
	table := volumeLinesPerBudget()
	out := make([]VolumeShare, 0, len(langs))
	for _, l := range langs {
		if !l.Measured {
			continue
		}
		share := VolumeShare{
			Language:       l.Language,
			Files:          l.FilesScanned,
			LOC:            l.MeasuredLOC(),
			LinesPerBudget: table[l.Language],
		}
		if share.Rated() {
			share.Years = float64(share.LOC) / float64(share.LinesPerBudget) * volumeYears
		}
		out = append(out, share)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LOC > out[j].LOC })
	return out
}

// writeVolume renders the sizes and states, once, why there is no verdict.
func writeVolume(b *strings.Builder, shares []VolumeShare) {
	if len(shares) == 0 {
		return
	}
	b.WriteString("## Volume (informational)\n\n")

	total, unrated := 0, []string{}
	for _, s := range shares {
		total += s.LOC
		if !s.Rated() {
			unrated = append(unrated, s.Language)
		}
	}

	plural := "languages"
	if len(shares) == 1 {
		plural = "language"
	}
	fmt.Fprintf(b, "%s lines of code across %d %s.\n\n", thousands(total), len(shares), plural)
	b.WriteString("| Language | Files | LOC | Lines per 3.9 person-years | Rebuild value |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, s := range shares {
		if !s.Rated() {
			fmt.Fprintf(b, "| %s | %d | %s | not published | not computable |\n",
				s.Language, s.Files, thousands(s.LOC))
			continue
		}
		fmt.Fprintf(b, "| %s | %d | %s | %s | %.1f person-years |\n",
			s.Language, s.Files, thousands(s.LOC), thousands(s.LinesPerBudget), s.Years)
	}

	b.WriteString("\n**No verdict.** The model caps a product's total rebuild value at " +
		"3.9 person-years, which is one figure for the whole system rather than one per " +
		"language.")
	if len(unrated) > 0 {
		fmt.Fprintf(b, " It cannot be totalled here: the published conversion table names "+
			"seven languages and %s is not among them, so the largest term is missing.",
			strings.Join(unrated, " and "))
	}
	b.WriteString(" The per-language rebuild values above are what the table does support, and " +
		"they are shares of the same 3.9-person-year budget, not budgets of their own.\n\n" +
		"SIG's own 2026 quality model removed this property, on the grounds that system size " +
		"is a risk indicator with no action attached to it. The criteria measured against here " +
		"still carry it, so it is reported as a figure.\n\n")
}
