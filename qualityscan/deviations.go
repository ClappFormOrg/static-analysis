package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Permanent deviations from the SIG 4-star caps.
//
// Every other number in the profile is a measurement, and the remedy for one
// over its cap is to change the code. A deviation is the case where the system
// owner has decided not to: the property measures what it says it measures, the
// figure is right, and bringing it under the cap would cost more than it buys.
//
// The distinction that makes this not a suppression: a deviation NEVER changes a
// number or a verdict. The tail still measures what it measures, still prints
// `fail`, and still carries its LOC-to-move figure. The deviation is rendered underneath, so
// the scorecard answers "why does this fail" in the same place it says that it
// does. Suppressing the number would leave a reader unable to tell a deviation
// from a property nobody had looked at, which is the failure mode accepted.go
// documents at length for the two review rules.
//
// This is the move components.json already makes for the component boundary:
// where the model leaves an answer to the system owner rather than to the
// measurement, the answer and its reasoning live in the tree instead of being
// re-derived, and re-argued, on every pass over the report.

// Deviation is one recorded decision not to bring a tail under its cap.
type Deviation struct {
	// Language and Property identify the measured property, matching the
	// language name in the profile and the property ID in sigProperties.
	Language string `json:"language"`
	Property string `json:"property"`
	// Tail is the cumulative tail this covers, written exactly as the report
	// labels it (">= 3", "> 15"). A deviation covers ONE tail: the tiers above
	// it keep their caps and their verdicts, so a system that deviates on the
	// moderate band is still held to the high one.
	Tail string `json:"tail"`
	// Decided is the ISO date the decision was made, printed with it. A
	// deviation that predates half the codebase is still a deviation, and a
	// reader is entitled to weigh it.
	Decided string `json:"decided"`
	// Measured is the figure as it stood when the decision was made, in prose.
	// The report prints today's measurement beside it, so a deviation whose
	// premise has moved is visible rather than assumed still true.
	Measured string `json:"measured"`
	// Ref points at the fuller write-up, normally the issue where it was argued.
	Ref string `json:"ref"`
	// Summary is the one-paragraph answer to "why does this fail".
	Summary string `json:"summary"`
	// Reasoning is the argument, one paragraph per step. Required and
	// non-empty: a deviation with a conclusion and no argument is a suppression
	// with a date on it.
	Reasoning []string `json:"reasoning"`
	// StillGated states what this deviation does NOT cover, so the scope cannot
	// quietly widen to the whole property later.
	StillGated string `json:"still_gated"`
}

// DeviationFile is the on-disk shape.
type DeviationFile struct {
	// Note is free text describing the file. Ignored by the tool; present so the
	// file explains itself to whoever opens it first.
	Note       string      `json:"note,omitempty"`
	Deviations []Deviation `json:"deviations"`
}

// Deviations is a loaded, validated set, keyed for lookup during rendering.
type Deviations struct {
	byTail map[string]Deviation
}

func deviationKey(language, property, tail string) string {
	return language + "\x00" + property + "\x00" + tail
}

// LoadDeviations reads the deviation record. An empty path returns an empty set,
// which renders nothing: a report with no deviation file is a report where every
// failing tail is simply a failing tail.
func LoadDeviations(path string) (Deviations, error) {
	if path == "" {
		return Deviations{}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Deviations{}, err
	}
	var f DeviationFile
	if err := json.Unmarshal(b, &f); err != nil {
		return Deviations{}, fmt.Errorf("parse %s: %w", path, err)
	}
	out := Deviations{byTail: map[string]Deviation{}}
	for i, d := range f.Deviations {
		if err := validateDeviation(path, i, d); err != nil {
			return Deviations{}, err
		}
		k := deviationKey(d.Language, d.Property, d.Tail)
		if _, dup := out.byTail[k]; dup {
			return Deviations{}, fmt.Errorf("%s: %s %s %s is declared twice", path, d.Language, d.Property, d.Tail)
		}
		out.byTail[k] = d
	}
	return out, nil
}

// validateDeviation holds one entry to the rules that make it a decision rather
// than a licence. Every field it insists on is one a later reader needs in order
// to weigh the deviation without re-running the analysis.
func validateDeviation(path string, i int, d Deviation) error {
	switch {
	case d.Language == "":
		return fmt.Errorf("%s: deviation %d needs a language", path, i)
	case d.Property == "":
		return fmt.Errorf("%s: deviation %d needs a property", path, i)
	case d.Tail == "":
		return fmt.Errorf("%s: deviation %d needs a tail -- a deviation covers one "+
			"tail, so that the tiers above it keep their caps", path, i)
	case d.Decided == "":
		return fmt.Errorf("%s: deviation %q needs a decided date", path, d.Property)
	case d.Summary == "":
		return fmt.Errorf("%s: deviation %q needs a summary", path, d.Property)
	case len(d.Reasoning) == 0:
		return fmt.Errorf("%s: deviation %q needs its reasoning -- a conclusion with "+
			"no argument is a suppression with a date on it", path, d.Property)
	case d.Ref == "":
		return fmt.Errorf("%s: deviation %q needs a ref to where it was argued", path, d.Property)
	}
	// Module-level properties carry no deviation today, so an unknown ID is far
	// more likely a typo than a property this does not know about, and a typo
	// would silently render nothing.
	for _, p := range sigProperties() {
		if p.Property.ID == d.Property {
			return nil
		}
	}
	return fmt.Errorf("%s: deviation %d names unknown property %q", path, i, d.Property)
}

// For returns the deviation covering one tail of one property, if there is one.
func (d Deviations) For(language, property, tail string) (Deviation, bool) {
	if d.byTail == nil {
		return Deviation{}, false
	}
	dev, ok := d.byTail[deviationKey(language, property, tail)]
	return dev, ok
}

// Any reports whether a language carries any deviation at all, so the report can
// skip the section header rather than print an empty one.
func (d Deviations) Any(language string) bool {
	for _, dev := range d.byTail {
		if dev.Language == language {
			return true
		}
	}
	return false
}

// writeDeviation renders one deviation beneath the property table it explains.
// The verdict above it still reads `fail`; this says why that is the intended
// state rather than an outstanding piece of work.
func writeDeviation(b *strings.Builder, d Deviation) {
	fmt.Fprintf(b, "**Permanent deviation on the `%s` tail, decided %s.** %s\n\n", d.Tail, d.Decided, d.Summary)
	if d.Measured != "" {
		fmt.Fprintf(b, "Measured when decided: %s.\n\n", d.Measured)
	}
	for _, r := range d.Reasoning {
		fmt.Fprintf(b, "- %s\n", r)
	}
	b.WriteString("\n")
	if d.StillGated != "" {
		fmt.Fprintf(b, "%s\n\n", d.StillGated)
	}
	if d.Ref != "" {
		fmt.Fprintf(b, "Argued in full: %s\n\n", d.Ref)
	}
}
