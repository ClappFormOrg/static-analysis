package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Acceptances are how a REVIEW rule stops costing something once it has been
// reviewed.
//
// Most rules here answer "is this over the threshold", and the remedy is to
// change the code. Three do not. NESTED_LOOP, SQL_IN_LOOP and
// SWITCH_WITHOUT_DEFAULT flag a shape, and whether that shape is a defect
// depends on something this scanner cannot see -- where a loop's bounds come
// from, or whether a switch tag's value set is closed. A loop over a grouped map
// is not a product, a query inside a loop over a fixed status list is not an
// N+1, and a switch naming every member of its enum needs no default. The remedy
// for those is a judgement, recorded once.
//
// Without somewhere to record it, the judgement evaporates. The nested-loop
// review read every site, fixed the one real product of two
// database counts and wrote a verdict for each of the rest -- and the scan went
// on reporting 58, the tracker issue went on saying 58, and every subsequent
// pass over that ticket re-derived the same 58 verdicts from scratch. A count
// that cannot go down is not a metric, it is a tax.
//
// So an acceptance is a reviewed site: rule, file, element, how many findings
// were reviewed there, when, why, and where the reasoning is written down.
//
// Three properties keep the mechanism from becoming a blanket suppression:
//
//   - It is COUNT-BOUNDED. An acceptance covers a stated number of findings at
//     a site, not the site. Add a second nested loop to a function accepted for
//     one and the extra is reported, because the reviewer judged the loop that
//     was there, not any loop that might arrive later.
//   - It is KEYED ON ELEMENT, not on a line number. Line numbers in this
//     repository drift by tens of lines a week; a line-keyed suppression would
//     silently slide onto a neighbour.
//   - It goes STALE LOUDLY. An acceptance matching fewer findings than it claims
//     means the code moved out from under the review -- fixed, renamed or
//     deleted -- and the run fails until the record is brought back in line. A
//     suppression file nobody is forced to maintain is a suppression file that
//     is quietly wrong within a quarter.
type Acceptance struct {
	Rule    string `json:"rule"`
	File    string `json:"file"`
	Element string `json:"element"`
	// Count is how many findings of this rule at this element the review
	// covered. Almost always 1; a function with two reviewed loops has 2.
	Count int `json:"count"`
	// Reviewed is the ISO date of the review that accepted this site.
	Reviewed string `json:"reviewed"`
	// Reason is the one-line verdict. It is printed in the report and in the
	// tracker issue, so it has to stand on its own.
	Reason string `json:"reason"`
	// Ref points at the write-up carrying the full reasoning.
	Ref string `json:"ref"`
}

func (a Acceptance) key() acceptanceKey {
	return acceptanceKey{Rule: a.Rule, File: a.File, Element: a.Element}
}

type acceptanceKey struct {
	Rule    string
	File    string
	Element string
}

func (k acceptanceKey) String() string {
	return fmt.Sprintf("%s %s:%s", k.Rule, k.File, k.Element)
}

// AcceptanceFile is the on-disk shape: a note about what the file is for,
// followed by the entries.
type AcceptanceFile struct {
	// Note is free text describing the file. Ignored by the tool; present so
	// the file explains itself to whoever opens it first.
	Note        string       `json:"note,omitempty"`
	Acceptances []Acceptance `json:"acceptances"`
}

// LoadAcceptances reads an acceptance file. An empty path yields no
// acceptances, which is the default: the file describes one scanned module, and
// `quality-scan-self` scans a different one.
func LoadAcceptances(path string) ([]Acceptance, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f AcceptanceFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for i, a := range f.Acceptances {
		if a.Rule == "" || a.File == "" || a.Element == "" {
			return nil, fmt.Errorf("%s: entry %d needs rule, file and element", path, i)
		}
		if a.Count < 1 {
			return nil, fmt.Errorf("%s: entry %d (%s) needs a count of at least 1", path, i, a.key())
		}
		if a.Reviewed == "" || a.Reason == "" {
			return nil, fmt.Errorf("%s: entry %d (%s) needs reviewed and reason -- "+
				"an acceptance with no recorded judgement is just a suppression", path, i, a.key())
		}
	}
	return f.Acceptances, nil
}

// StaleAcceptance is an entry that no longer describes the code.
type StaleAcceptance struct {
	Acceptance
	Matched int
}

// AcceptanceResult is what the report needs to say about the acceptances that
// were applied: how many findings each rule shed, and whether the record is
// still true.
type AcceptanceResult struct {
	// ByRule counts the suppressed findings per rule id.
	ByRule map[string]int
	// Reasons collects the distinct verdicts per rule, in first-seen order, so
	// a report can say WHY a rule went to zero without listing every site.
	Reasons map[string][]string
	// Refs collects the distinct review documents per rule.
	Refs map[string][]string
	// Stale are the entries that matched fewer findings than they claim.
	Stale []StaleAcceptance
}

// Total is the number of findings the acceptances removed.
func (r AcceptanceResult) Total() int {
	n := 0
	for _, c := range r.ByRule {
		n += c
	}
	return n
}

// ApplyAcceptances removes the reviewed findings and reports what it removed.
//
// prefix is the -prefix the findings were qualified with. Acceptances are
// written against the module-relative path, so they survive a report being
// re-run under a different prefix to line up with another tool's paths.
func ApplyAcceptances(findings []Finding, accs []Acceptance, prefix string) ([]Finding, AcceptanceResult) {
	result := AcceptanceResult{
		ByRule:  map[string]int{},
		Reasons: map[string][]string{},
		Refs:    map[string][]string{},
	}
	if len(accs) == 0 {
		return findings, result
	}

	// budget is how many findings each key may still absorb. Summing rather
	// than overwriting means two entries for the same element (a function with
	// two separately reasoned loops) add up instead of the second silently
	// replacing the first.
	budget := map[acceptanceKey]int{}
	byKey := map[acceptanceKey][]Acceptance{}
	for _, a := range accs {
		budget[a.key()] += a.Count
		byKey[a.key()] = append(byKey[a.key()], a)
	}
	claimed := map[acceptanceKey]int{}
	for k, v := range budget {
		claimed[k] = v
	}

	trim := strings.TrimSuffix(prefix, "/")
	open := make([]Finding, 0, len(findings))
	for _, f := range findings {
		rel := f.File
		if trim != "" {
			rel = strings.TrimPrefix(rel, trim+"/")
		}
		k := acceptanceKey{Rule: f.Rule, File: rel, Element: f.Element}
		if budget[k] > 0 {
			budget[k]--
			result.ByRule[f.Rule]++
			for _, a := range byKey[k] {
				result.Reasons[f.Rule] = appendUnique(result.Reasons[f.Rule], a.Reason)
				if a.Ref != "" {
					result.Refs[f.Rule] = appendUnique(result.Refs[f.Rule], a.Ref)
				}
			}
			continue
		}
		open = append(open, f)
	}

	// Whatever budget is left over was claimed against findings that are not
	// there any more.
	for k, left := range budget {
		if left == 0 {
			continue
		}
		for _, a := range byKey[k] {
			result.Stale = append(result.Stale, StaleAcceptance{
				Acceptance: a,
				Matched:    claimed[k] - left,
			})
		}
	}
	sort.Slice(result.Stale, func(i, j int) bool {
		a, b := result.Stale[i], result.Stale[j]
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Element < b.Element
	})

	return open, result
}

// StaleError renders the stale entries as one actionable error, or nil when the
// record still matches the code.
//
// Failing the run is deliberate, and it is the same call the SIG profile makes
// for an unmeasured language. A stale acceptance has exactly two causes and
// both want a human: the site was fixed, so the entry should be deleted and the
// count is now genuinely lower; or the element was renamed, so the entry has
// stopped suppressing anything and the finding it covered is back in the report
// under a new name. Neither is served by a warning nobody reads.
func (r AcceptanceResult) StaleError() error {
	if len(r.Stale) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d acceptance(s) no longer match the code. "+
		"Each was reviewed against a finding that is not there now -- the site was fixed "+
		"(delete the entry) or the element was renamed (update it):", len(r.Stale))
	for _, s := range r.Stale {
		fmt.Fprintf(&b, "\n  %s: claims %d, matched %d  (reviewed %s, %s)",
			s.key(), s.Count, s.Matched, s.Reviewed, s.Ref)
	}
	return fmt.Errorf("%s", b.String())
}

// acceptedCell renders a suppressed count for a report column. A rule with no
// acceptances prints a dash rather than 0, so "nobody has reviewed this" reads
// differently from "reviewed, and nothing was accepted".
func acceptedCell(n int) string {
	if n == 0 {
		return "-"
	}
	return strconv.Itoa(n)
}

// writeAcceptedSummary prints the verdicts behind the ACCEPTED column, so the
// terminal report carries the reasoning and not just the arithmetic.
func writeAcceptedSummary(w io.Writer, r AcceptanceResult) error {
	if r.Total() == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w, "\nAccepted (%d findings reviewed and judged correct as written):\n", r.Total()); err != nil {
		return err
	}
	for _, rule := range ruleOrder {
		n := r.ByRule[rule]
		if n == 0 {
			continue
		}
		if _, err := fmt.Fprintf(w, "  %s: %d\n", rule, n); err != nil {
			return err
		}
		for _, reason := range r.Reasons[rule] {
			if _, err := fmt.Fprintf(w, "    - %s\n", reason); err != nil {
				return err
			}
		}
		for _, ref := range r.Refs[rule] {
			if _, err := fmt.Fprintf(w, "    see %s\n", ref); err != nil {
				return err
			}
		}
	}
	return nil
}

func appendUnique(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}
