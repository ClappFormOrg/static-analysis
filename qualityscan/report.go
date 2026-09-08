package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Rule ids are the conventional names for these checks, so a finding here lines
// up with the same finding from another scanner without a translation table.
// They are also the keys an acceptance file and `disabled_rules` are written
// against, so renaming one is a breaking change for every consumer.
const (
	RuleCyclicReference    = "CYCLIC_REFERENCE"
	RuleFunctionSize       = "FUNCTION_SIZE_RISK"
	RuleParameter          = "PARAMETER_RISK"
	RuleFunctionComplexity = "FUNCTION_COMPLEXITY_RISK"
	RuleDependencyVolume   = "DEPENDENCY_VOLUME_RISK"
	RuleDependencySpan     = "DEPENDENCY_SPAN_RISK"
	RuleHardcodedURL       = "HARDCODED_URL"
	RuleHardcodedPath      = "HARDCODED_PATH"
	RuleCopyrightOrLicense = "COPYRIGHT_OR_LICENSE_NOTICE"
)

// Gap rules. These are real Go concerns that the standard best-practice
// catalogues name but that commercial scanners commonly score zero on, having
// no Go implementation of them. See tier2.go.
const (
	RuleSQLInLoop            = "SQL_IN_LOOP"
	RuleStringConcatInLoop   = "STRING_CONCAT_IN_LOOP"
	RuleNestedLoop           = "NESTED_LOOP"
	RuleEmptyBranch          = "EMPTY_BRANCH"
	RuleDebugStatement       = "DEBUG_STATEMENT"
	RuleUnfinishedWork       = "UNFINISHED_WORK"
	RuleSuppressedWarning    = "SUPPRESSED_WARNING"
	RuleCommentedOutCode     = "COMMENTED_OUT_CODE"
	RuleMissingDoc           = "MISSING_DOC_COMMENT"
	RuleSwitchWithoutDefault = "SWITCH_WITHOUT_DEFAULT"
)

// standardRules are the nine checks a commercial maintainability scan reports,
// in the order the report lists them. They come first because they are the ones
// another tool's output can be lined up against.
var standardRules = []string{
	RuleCyclicReference,
	RuleFunctionSize,
	RuleParameter,
	RuleFunctionComplexity,
	RuleDependencyVolume,
	RuleDependencySpan,
	RuleHardcodedURL,
	RuleHardcodedPath,
	RuleCopyrightOrLicense,
}

// gapRules follow, ordered worst-consequence first: the two that cost query
// time, then the ones that hide a bug, then the ones that cost a reader.
var gapRules = []string{
	RuleSQLInLoop,
	RuleStringConcatInLoop,
	RuleNestedLoop,
	RuleEmptyBranch,
	RuleSwitchWithoutDefault,
	RuleDebugStatement,
	RuleCommentedOutCode,
	RuleUnfinishedWork,
	RuleSuppressedWarning,
	RuleMissingDoc,
}

var ruleOrder = append(append([]string{}, standardRules...), gapRules...)

// reviewRules are the rules whose findings a reviewer can judge correct as
// written, so they are the only ones accepted.json may carry entries for. Every
// other rule states a threshold, and the way past a threshold is to change the
// code. See the doc comment on Acceptance for why the distinction exists.
var reviewRules = []string{
	RuleSQLInLoop,
	RuleNestedLoop,
	RuleSwitchWithoutDefault,
}

// isReviewRule reports whether rule's verdicts may live in accepted.json.
func isReviewRule(rule string) bool {
	return slices.Contains(reviewRules, rule)
}

// Finding is one reported problem, shaped like a row of the report's CSV.
type Finding struct {
	Rule        string `json:"rule"`
	Element     string `json:"element"`
	Description string `json:"description"`
	File        string `json:"filename"`
	Line        int    `json:"line"`
	Severity    string `json:"severity"`
	Value       int    `json:"value,omitempty"`
}

// Run executes every rule and returns the findings, ordered by rule and then by
// descending severity so the worst offender of each rule reads first.
func Run(idx *Index, prefix string) []Finding {
	cfg := idx.Cfg
	var out []Finding

	qualify := func(rel string) string {
		if prefix == "" {
			return rel
		}
		return strings.TrimSuffix(prefix, "/") + "/" + rel
	}

	for _, c := range idx.findCycles() {
		for _, e := range c.Edges {
			out = append(out, Finding{
				Rule:        RuleCyclicReference,
				Element:     e.ToPkg[strings.LastIndex(e.ToPkg, "/")+1:],
				Description: c.Describe(),
				File:        qualify(e.File.Rel),
				Line:        e.Line,
				Severity:    SeverityFinding.String(),
				Value:       len(c.Members),
			})
		}
	}

	for _, m := range idx.measureFuncs() {
		add := func(rule string, band Band, v int, unit string) {
			sev := band.Rate(v)
			if sev == SeverityNone {
				return
			}
			out = append(out, Finding{
				Rule:        rule,
				Element:     m.Element,
				Description: fmt.Sprintf("%s: %d %s", unit, v, band.Describe(v)),
				File:        qualify(m.File.Rel),
				Line:        m.Line,
				Severity:    sev.String(),
				Value:       v,
			})
		}
		add(RuleFunctionSize, cfg.FunctionSize, m.Tokens, "function size in tokens")
		add(RuleParameter, cfg.Parameters, m.Params, "parameters")
		// Deliberately not the conventional "function nesting complexity"
		// wording: the rule id is the standard one so findings line up, but the
		// number underneath is cognitive complexity and the two do not compare.
		// See funcmetrics.go.
		add(RuleFunctionComplexity, cfg.Complexity, m.Complexity, "cognitive complexity")
	}

	for _, d := range idx.measureDeps() {
		add := func(rule string, band Band, v int, unit string) {
			sev := band.Rate(v)
			if sev == SeverityNone {
				return
			}
			out = append(out, Finding{
				Rule:        rule,
				Element:     d.File.Rel[strings.LastIndex(d.File.Rel, "/")+1:],
				Description: fmt.Sprintf("%s: %d %s", unit, v, band.Describe(v)),
				File:        qualify(d.File.Rel),
				Line:        d.Line,
				Severity:    sev.String(),
				Value:       v,
			})
		}
		add(RuleDependencyVolume, cfg.DependencyVolume, d.Volume, "dependency volume")
		add(RuleDependencySpan, cfg.DependencySpan, d.Span, "dependency span")
	}

	for _, l := range idx.findHardcodedURLs() {
		out = append(out, literalFinding(RuleHardcodedURL, l, qualify))
	}
	for _, l := range idx.findHardcodedPaths() {
		out = append(out, literalFinding(RuleHardcodedPath, l, qualify))
	}
	for _, l := range idx.findCopyrightOrLicenseNotices() {
		out = append(out, literalFinding(RuleCopyrightOrLicense, l, qualify))
	}

	for _, f := range idx.tier2Rules() {
		f.File = qualify(f.File)
		out = append(out, f)
	}

	if len(cfg.DisabledRules) > 0 {
		kept := out[:0]
		for _, f := range out {
			if !contains(cfg.DisabledRules, f.Rule) {
				kept = append(kept, f)
			}
		}
		out = kept
	}

	rank := map[string]int{}
	for i, r := range ruleOrder {
		rank[r] = i
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if rank[a.Rule] != rank[b.Rule] {
			return rank[a.Rule] < rank[b.Rule]
		}
		if a.Value != b.Value {
			return a.Value > b.Value
		}
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
	return out
}

func literalFinding(rule string, l Literal, qualify func(string) string) Finding {
	return Finding{
		Rule:        rule,
		Element:     l.Element,
		Description: l.Value,
		File:        qualify(l.File.Rel),
		Line:        l.Line,
		Severity:    SeverityFinding.String(),
	}
}

// WriteCSV emits a sectioned CSV: a bare rule name on its own line, a header
// row, then rows numbered from 1 within the section. That is the layout a
// commercial scan exports, so the two can be diffed directly.
// Rules that found nothing still get a section, because "0 violations" is the
// half of the report that says a check ran.
func WriteCSV(w io.Writer, findings []Finding) error {
	byRule := map[string][]Finding{}
	for _, f := range findings {
		byRule[f.Rule] = append(byRule[f.Rule], f)
	}

	cw := csv.NewWriter(w)
	for _, rule := range ruleOrder {
		if _, err := fmt.Fprintf(w, "%s\n", rule); err != nil {
			return err
		}
		if err := cw.Write([]string{"id", "element", "description", "filename", "line"}); err != nil {
			return err
		}
		for i, f := range byRule[rule] {
			row := []string{
				strconv.Itoa(i + 1), f.Element, f.Description, f.File, strconv.Itoa(f.Line),
			}
			if err := cw.Write(row); err != nil {
				return err
			}
		}
		cw.Flush()
		if err := cw.Error(); err != nil {
			return err
		}
	}
	return nil
}

// WriteText emits a summary table plus the findings, for reading in a terminal.
//
// The ACCEPTED column is not decoration. A rule that reads 0 open / 58 accepted
// is a rule somebody finished, and without the second number that is
// indistinguishable from a rule nobody ever ran.
func WriteText(w io.Writer, findings []Finding, reviewed AcceptanceResult) error {
	byRule := map[string][]Finding{}
	for _, f := range findings {
		byRule[f.Rule] = append(byRule[f.Rule], f)
	}

	if _, err := fmt.Fprintf(w, "%-26s %8s %10s %14s %10s\n",
		"RULE", "OPEN", "HIGH", "VERY HIGH", "ACCEPTED"); err != nil {
		return err
	}
	total := 0
	for _, rule := range ruleOrder {
		list := byRule[rule]
		total += len(list)
		high, veryHigh := 0, 0
		for _, f := range list {
			switch f.Severity {
			case SeverityHigh.String():
				high++
			case SeverityVeryHigh.String():
				veryHigh++
			}
		}
		if _, err := fmt.Fprintf(w, "%-26s %8d %10d %14d %10s\n",
			rule, len(list), high, veryHigh, acceptedCell(reviewed.ByRule[rule])); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "%-26s %8d %10s %14s %10s\n",
		"TOTAL", total, "", "", acceptedCell(reviewed.Total())); err != nil {
		return err
	}

	if err := writeAcceptedSummary(w, reviewed); err != nil {
		return err
	}

	for _, rule := range ruleOrder {
		list := byRule[rule]
		if len(list) == 0 {
			continue
		}
		if _, err := fmt.Fprintf(w, "\n%s (%d)\n", rule, len(list)); err != nil {
			return err
		}
		for _, f := range list {
			if _, err := fmt.Fprintf(w, "  %s:%d  %s  %s\n", f.File, f.Line, f.Element, f.Description); err != nil {
				return err
			}
		}
	}
	return nil
}

// WriteMetrics dumps every raw measurement, threshold-free. This is the format
// to use when re-deriving thresholds: sort a column and pick the cut that puts
// the right number of rows above it. See README.md, "Calibration".
func WriteMetrics(w io.Writer, idx *Index, prefix string) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()

	qualify := func(rel string) string {
		if prefix == "" {
			return rel
		}
		return strings.TrimSuffix(prefix, "/") + "/" + rel
	}

	// volume_local and volume_cross are blank on func rows and split the volume
	// on file rows; see FileDeps. They are the two columns to look at when a
	// dependency-volume ranking looks wrong for a whole package.
	if err := cw.Write([]string{
		"kind", "element", "filename", "line", "tokens_or_volume", "params_or_span",
		"complexity", "volume_local", "volume_cross",
	}); err != nil {
		return err
	}
	for _, m := range idx.measureFuncs() {
		if err := cw.Write([]string{
			"func", m.Element, qualify(m.File.Rel), strconv.Itoa(m.Line),
			strconv.Itoa(m.Tokens), strconv.Itoa(m.Params), strconv.Itoa(m.Complexity), "", "",
		}); err != nil {
			return err
		}
	}
	for _, d := range idx.measureDeps() {
		if err := cw.Write([]string{
			"file", d.File.Rel, qualify(d.File.Rel), strconv.Itoa(d.Line),
			strconv.Itoa(d.Volume), strconv.Itoa(d.Span), "",
			strconv.Itoa(d.VolumeLocal), strconv.Itoa(d.VolumeCross),
		}); err != nil {
			return err
		}
	}
	return cw.Error()
}

// WriteJSON writes the findings as an indented JSON array. A nil slice is
// written as `[]` rather than `null`, so a clean scan and a failed one cannot
// be told apart by a reader that only checks for a list.
func WriteJSON(w io.Writer, findings []Finding) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if findings == nil {
		findings = []Finding{}
	}
	return enc.Encode(findings)
}
