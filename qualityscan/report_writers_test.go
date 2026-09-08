package main

import (
	"encoding/csv"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// reportFixture is the finding set the four writers are exercised over. Two
// rules carry findings and the other seventeen carry none, because the half of
// each writer worth pinning is what it does with a rule that found nothing.
func reportFixture() []Finding {
	return []Finding{
		{Rule: RuleFunctionSize, Element: "Handler", Value: 900,
			Description: "function size in tokens: 900 (very high risk, [> 750])",
			File:        "handler.go", Line: 12, Severity: SeverityVeryHigh.String()},
		{Rule: RuleFunctionSize, Element: "Middle", Value: 400,
			Description: "function size in tokens: 400 (high risk, [> 300])",
			File:        "handler.go", Line: 80, Severity: SeverityHigh.String()},
		{Rule: RuleNestedLoop, Element: "Sweep",
			// The comma is load-bearing: a description carrying one has to come
			// back out of the CSV as a single field.
			Description: "two nested range loops, over rows and columns",
			File:        "sweep.go", Line: 30, Severity: SeverityFinding.String()},
	}
}

// summaryRow returns the fields of the WriteText summary line for one rule.
func summaryRow(t *testing.T, out, rule string) []string {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && f[0] == rule {
			return f
		}
	}
	t.Fatalf("no summary row for %s in:\n%s", rule, out)
	return nil
}

// sameFields reports whether two rows of cells are identical.
func sameFields(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestCSVKeepsASectionForEveryRule holds the sectioned CSV shape, and
// the reason the empty sections are in it. A rule that found nothing still gets
// its header and its column row: "0 violations" is the half of a report that
// says the check ran, and once the section is dropped a reader cannot tell a
// clean rule from one nobody implemented.
//
// Row numbering is the other thing pinned here. Ids restart at 1 inside each
// section, which is the layout's convention, so a counter that ran on across
// sections would make every section after the first disagree with the export it
// is diffed against.
func TestCSVKeepsASectionForEveryRule(t *testing.T) {
	b := &strings.Builder{}
	if err := WriteCSV(b, reportFixture()); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	out := b.String()

	for _, rule := range ruleOrder {
		if !strings.Contains(out, "\n"+rule+"\n") && !strings.HasPrefix(out, rule+"\n") {
			t.Errorf("no section for %s; a rule that found nothing must still be listed:\n%s", rule, out)
		}
	}

	// The first rule found nothing, so its section is a header immediately
	// followed by the next rule's name, and the two sit in ruleOrder order.
	empty := RuleCyclicReference + "\nid,element,description,filename,line\n" + RuleFunctionSize + "\n"
	if !strings.Contains(out, empty) {
		t.Errorf("an empty section must still carry its column row, and the sections must "+
			"follow ruleOrder; got:\n%s", out)
	}

	for _, want := range []string{
		`1,Handler,"function size in tokens: 900 (very high risk, [> 750])",handler.go,12`,
		`2,Middle,"function size in tokens: 400 (high risk, [> 300])",handler.go,80`,
		// Back to 1, in the second populated section, five sections later.
		`1,Sweep,"two nested range loops, over rows and columns",sweep.go,30`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("CSV missing row %q\ngot:\n%s", want, out)
		}
	}

	// And it is real CSV: a reader that parses rather than greps gets either a
	// bare rule name or a five-cell row, never a description split on its comma.
	r := csv.NewReader(strings.NewReader(out))
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		t.Fatalf("the export does not parse as CSV: %v", err)
	}
	for _, rec := range records {
		if len(rec) != 1 && len(rec) != 5 {
			t.Errorf("record %v has %d fields, want 1 (a rule name) or 5", rec, len(rec))
		}
	}
}

// TestTextReportSeparatesOpenFromAccepted covers the summary table and the
// detail listing under it.
//
// The ACCEPTED column is the point. A rule reading 0 open / 4 accepted is a
// rule somebody finished, and a rule reading 0 open / - is one that never
// fired; those are different facts and the table has to keep them apart. The
// negative half is that a rule with no findings gets no detail section at all,
// so a reader scrolling the findings is not walking past seventeen empty
// headings to reach the two that matter.
func TestTextReportSeparatesOpenFromAccepted(t *testing.T) {
	reviewed := AcceptanceResult{
		ByRule:  map[string]int{RuleNestedLoop: 4},
		Reasons: map[string][]string{RuleNestedLoop: {"both loops are bounded by a two-element enum"}},
		Refs:    map[string][]string{RuleNestedLoop: {"docs/reviews/nested-loops.md"}},
	}

	b := &strings.Builder{}
	if err := WriteText(b, reportFixture(), reviewed); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := b.String()

	// The cells after the rule name are open, high, very high, accepted.
	cases := []struct {
		rule string
		want []string
		why  string
	}{
		{RuleFunctionSize, []string{RuleFunctionSize, "2", "1", "1", "-"},
			"two findings, one at each of the two severities that get their own column"},
		{RuleNestedLoop, []string{RuleNestedLoop, "1", "0", "0", "4"},
			"one open finding and four somebody has already reviewed"},
		// A rule that never fired and was never accepted reads as a dash rather
		// than a zero: the column counts reviews, and there were none to count.
		{RuleCyclicReference, []string{RuleCyclicReference, "0", "0", "0", "-"},
			"a rule that found nothing and was never reviewed"},
		// Fields collapses the two blank severity cells away, so TOTAL is three
		// fields wide: the open count across every rule, then the accepted one.
		{"TOTAL", []string{"TOTAL", "3", "4"}, "the totals of both columns that carry one"},
	}
	for _, c := range cases {
		if got := summaryRow(t, out, c.rule); !sameFields(got, c.want) {
			t.Errorf("%s summary = %v, want %v (%s)", c.rule, got, c.want, c.why)
		}
	}

	// The accepted block says why the rule went quiet, which is what makes the
	// count checkable rather than something a reader has to take on trust.
	for _, want := range []string{
		"Accepted (4 findings reviewed and judged correct as written)",
		"both loops are bounded by a two-element enum",
		"see docs/reviews/nested-loops.md",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the accepted summary is missing %q, got:\n%s", want, out)
		}
	}

	// The detail listing: a heading carrying the count, then every finding
	// under it at file:line.
	for _, want := range []string{
		"\n" + RuleFunctionSize + " (2)\n",
		"  handler.go:12  Handler  function size in tokens: 900",
		"  handler.go:80  Middle  function size in tokens: 400",
		"\n" + RuleNestedLoop + " (1)\n",
		"  sweep.go:30  Sweep  two nested range loops",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the findings listing is missing %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, RuleCyclicReference+" (0)") {
		t.Errorf("a rule with no findings must not get a detail section:\n%s", out)
	}
}

// TestTextReportSaysNothingAboutAcceptanceWhenThereIsNone is the negative half
// of the block above. With nothing accepted the summary is absent entirely,
// because a heading reading "Accepted (0 findings...)" invites a reader to
// believe a review happened.
func TestTextReportSaysNothingAboutAcceptanceWhenThereIsNone(t *testing.T) {
	b := &strings.Builder{}
	if err := WriteText(b, reportFixture(), AcceptanceResult{}); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := b.String()
	if strings.Contains(out, "Accepted (") {
		t.Errorf("an unreviewed scan printed an acceptance summary:\n%s", out)
	}
	if got := summaryRow(t, out, "TOTAL"); !sameFields(got, []string{"TOTAL", "3", "-"}) {
		t.Errorf("TOTAL = %v, want the accepted cell as a dash", got)
	}
	// The findings themselves are untouched by there being no acceptances.
	if !strings.Contains(out, "\n"+RuleFunctionSize+" (2)\n") {
		t.Errorf("the findings listing disappeared with the acceptance summary:\n%s", out)
	}
}

// TestMetricsDumpIsThresholdFree covers the calibration view. Every raw
// measurement goes out with no band applied, because the way a threshold gets
// re-derived is to sort a column and pick the cut that puts the right number of
// rows above it, which needs the rows a threshold would have filtered away.
//
// The two row kinds share one header, so the blank cells are checked as
// deliberately as the filled ones: a func row has no volume to split and a file
// row has no complexity, and a 0 in either place would read as a measured zero.
func TestMetricsDumpIsThresholdFree(t *testing.T) {
	idx := load(t, map[string]string{
		"a/one.go": `package a

import "fmt"

// Run leans on a sibling file and on fmt, so its file row carries both halves
// of the volume split.
func Run(a, b int) {
	fmt.Println(helper(), a, b)
}
`,
		"a/two.go": `package a

func helper() string { return "" }
`,
	}, DefaultConfig())

	b := &strings.Builder{}
	if err := WriteMetrics(b, idx, "example/tool"); err != nil {
		t.Fatalf("WriteMetrics: %v", err)
	}

	records, err := csv.NewReader(strings.NewReader(b.String())).ReadAll()
	if err != nil {
		t.Fatalf("the metrics dump does not parse as CSV: %v", err)
	}
	wantHeader := []string{"kind", "element", "filename", "line", "tokens_or_volume",
		"params_or_span", "complexity", "volume_local", "volume_cross"}
	if !sameFields(records[0], wantHeader) {
		t.Fatalf("header = %v, want %v", records[0], wantHeader)
	}

	rows := map[string][]string{}
	kinds := map[string]int{}
	for _, rec := range records[1:] {
		kinds[rec[0]]++
		rows[rec[0]+":"+rec[1]] = rec
	}
	// Two functions and two files, with neither pass dropping a row: an element
	// missing here is one that cannot appear in a threshold's denominator.
	if kinds["func"] != 2 || kinds["file"] != 2 {
		t.Errorf("dumped %v, want two func rows and two file rows", kinds)
	}

	run, ok := rows["func:Run"]
	if !ok {
		t.Fatalf("no row for Run in %v", rows)
	}
	if want := "example/tool/a/one.go"; run[2] != want {
		t.Errorf("filename = %q, want %q: the -prefix has to be applied so the path in the "+
			"dump is the path from the repository root", run[2], want)
	}
	if run[5] != "2" {
		t.Errorf("params = %q, want 2", run[5])
	}
	if run[7] != "" || run[8] != "" {
		t.Errorf("a func row split a volume it does not have: volume_local=%q volume_cross=%q; "+
			"those two columns belong to the file rows and a 0 here would read as a measurement",
			run[7], run[8])
	}

	one, ok := rows["file:a/one.go"]
	if !ok {
		t.Fatalf("no file row for a/one.go in %v", rows)
	}
	if one[6] != "" {
		t.Errorf("a file row carried complexity %q; complexity is measured per function", one[6])
	}
	// Volume is the sum of the two halves by construction, so the dump is only
	// usable for re-ranking a package if the split adds back up.
	volume, local, cross := cell(t, one[4]), cell(t, one[7]), cell(t, one[8])
	if local+cross != volume {
		t.Errorf("volume %d is not local %d plus cross %d", volume, local, cross)
	}
	if local == 0 || cross == 0 {
		t.Errorf("volume split = %d local / %d cross; the file references both a sibling file "+
			"and an external package, so neither half can be empty", local, cross)
	}
}

// cell reads a metrics cell as the number it is meant to be.
func cell(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("cell %q is not a number: %v", s, err)
	}
	return n
}

// TestJSONCleanScanIsNotNull holds a distinction a downstream reader cannot
// recover on its own. A scan that found nothing and a scan that fell over both
// arrive here with no findings, so the clean one has to be an empty array: a
// reader that only checks for a list would treat `null` as neither.
func TestJSONCleanScanIsNotNull(t *testing.T) {
	b := &strings.Builder{}
	if err := WriteJSON(b, nil); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if got := strings.TrimSpace(b.String()); got != "[]" {
		t.Errorf("a clean scan wrote %q, want []", got)
	}

	b.Reset()
	if err := WriteJSON(b, reportFixture()); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var got []Finding
	if err := json.Unmarshal([]byte(b.String()), &got); err != nil {
		t.Fatalf("the findings do not round-trip: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("read back %d findings, want 3", len(got))
	}
	if got[0] != reportFixture()[0] {
		t.Errorf("first finding came back as %+v, want %+v", got[0], reportFixture()[0])
	}
	// The keys are the CSV's column names rather than the Go field
	// names, because this file is what another tool reads.
	for _, want := range []string{`"filename"`, `"rule"`, `"element"`} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("the JSON does not carry %s:\n%s", want, b.String())
		}
	}
	// A rule that states no number omits the key rather than writing 0, which
	// is what the omitempty is there for: NESTED_LOOP measures nothing, and a
	// 0 would read as a measurement of zero.
	if strings.Contains(b.String(), `"value": 0`) {
		t.Errorf("a rule that measures no number wrote value 0:\n%s", b.String())
	}
}
