package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func acc(rule, file, element string, count int) Acceptance {
	return Acceptance{
		Rule: rule, File: file, Element: element, Count: count,
		Reviewed: "2026-08-11", Reason: "bounded", Ref: "reviews/r.md",
	}
}

func nested(file, element string, line int) Finding {
	return Finding{
		Rule: RuleNestedLoop, File: file, Element: element, Line: line,
		Severity: SeverityFinding.String(),
	}
}

func TestApplyAcceptancesRemovesReviewedFindings(t *testing.T) {
	findings := []Finding{
		nested("internal/a/a.go", "Walk", 10),
		nested("internal/b/b.go", "Scan", 20),
	}
	open, res := ApplyAcceptances(findings, []Acceptance{
		acc(RuleNestedLoop, "internal/a/a.go", "Walk", 1),
	}, "")

	if len(open) != 1 || open[0].Element != "Scan" {
		t.Fatalf("open = %+v, want just Scan", open)
	}
	if res.ByRule[RuleNestedLoop] != 1 || res.Total() != 1 {
		t.Fatalf("accepted = %d, want 1", res.Total())
	}
	if len(res.Stale) != 0 {
		t.Fatalf("unexpected stale: %+v", res.Stale)
	}
}

// The property that stops an acceptance becoming a blanket suppression. A
// reviewer judged the loops that were in the function when they read it; a loop
// added afterwards has been judged by nobody.
func TestApplyAcceptancesIsCountBounded(t *testing.T) {
	findings := []Finding{
		nested("internal/a/a.go", "Walk", 10),
		nested("internal/a/a.go", "Walk", 40), // added after the review
	}
	open, res := ApplyAcceptances(findings, []Acceptance{
		acc(RuleNestedLoop, "internal/a/a.go", "Walk", 1),
	}, "")

	if len(open) != 1 {
		t.Fatalf("open = %d findings, want 1: the second loop was never reviewed", len(open))
	}
	if res.ByRule[RuleNestedLoop] != 1 {
		t.Fatalf("accepted = %d, want 1", res.ByRule[RuleNestedLoop])
	}
	if len(res.Stale) != 0 {
		t.Fatalf("a fully-consumed acceptance is not stale: %+v", res.Stale)
	}
}

// Two entries for the same element add up rather than the second replacing the
// first, so a function with two separately reasoned loops can carry two
// verdicts.
func TestApplyAcceptancesSumsEntriesForTheSameElement(t *testing.T) {
	findings := []Finding{
		nested("internal/a/a.go", "Walk", 10),
		nested("internal/a/a.go", "Walk", 40),
	}
	a1 := acc(RuleNestedLoop, "internal/a/a.go", "Walk", 1)
	a2 := acc(RuleNestedLoop, "internal/a/a.go", "Walk", 1)
	a2.Reason = "a different reason"
	open, res := ApplyAcceptances(findings, []Acceptance{a1, a2}, "")

	if len(open) != 0 {
		t.Fatalf("open = %+v, want none", open)
	}
	if got := res.Reasons[RuleNestedLoop]; len(got) != 2 {
		t.Fatalf("reasons = %v, want both recorded", got)
	}
}

func TestApplyAcceptancesReportsStale(t *testing.T) {
	findings := []Finding{nested("internal/a/a.go", "Walk", 10)}
	_, res := ApplyAcceptances(findings, []Acceptance{
		acc(RuleNestedLoop, "internal/a/a.go", "Walk", 1),
		acc(RuleNestedLoop, "internal/a/a.go", "Renamed", 1),
		acc(RuleNestedLoop, "internal/gone/gone.go", "Deleted", 3),
	}, "")

	if len(res.Stale) != 2 {
		t.Fatalf("stale = %+v, want the renamed and the deleted entry", res.Stale)
	}
	err := res.StaleError()
	if err == nil {
		t.Fatal("StaleError = nil, want an error: a suppression nobody maintains goes quietly wrong")
	}
	for _, want := range []string{"Renamed", "Deleted", "claims 3, matched 0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("StaleError missing %q:\n%s", want, err)
		}
	}
}

// A partly-consumed acceptance is stale too: it claims more than the code has,
// which means the review record has drifted even though something still matches.
func TestApplyAcceptancesPartialMatchIsStale(t *testing.T) {
	findings := []Finding{nested("internal/a/a.go", "Walk", 10)}
	_, res := ApplyAcceptances(findings, []Acceptance{
		acc(RuleNestedLoop, "internal/a/a.go", "Walk", 3),
	}, "")

	if len(res.Stale) != 1 || res.Stale[0].Matched != 1 {
		t.Fatalf("stale = %+v, want one entry that matched 1 of 3", res.Stale)
	}
}

// Acceptances are written against the module-relative path so a run under
// -prefix (the layout used to line up with another tool) still matches them.
func TestApplyAcceptancesIgnoresThePrefix(t *testing.T) {
	findings := []Finding{nested("internal/a/a.go", "Walk", 10)}
	open, res := ApplyAcceptances(findings, []Acceptance{
		acc(RuleNestedLoop, "internal/a/a.go", "Walk", 1),
	}, "api")

	if len(open) != 0 {
		t.Fatalf("open = %+v, want none: the prefix should not defeat the match", open)
	}
	if res.Total() != 1 {
		t.Fatalf("accepted = %d, want 1", res.Total())
	}
}

// An acceptance for one rule must not absorb a different rule's finding at the
// same element -- a function can be a fine nested loop and a bad N+1 at once.
func TestApplyAcceptancesIsPerRule(t *testing.T) {
	findings := []Finding{
		{Rule: RuleSQLInLoop, File: "internal/a/a.go", Element: "Walk", Line: 10},
	}
	open, _ := ApplyAcceptances(findings, []Acceptance{
		acc(RuleNestedLoop, "internal/a/a.go", "Walk", 1),
	}, "")

	if len(open) != 1 {
		t.Fatal("a NESTED_LOOP acceptance swallowed a SQL_IN_LOOP finding")
	}
}

func TestLoadAcceptancesRejectsAnEntryWithNoJudgement(t *testing.T) {
	tests := map[string]string{
		"no reason": `{"acceptances":[
			{"rule":"NESTED_LOOP","file":"a.go","element":"F","count":1,"reviewed":"2026-08-11"}]}`,
		"no reviewed date": `{"acceptances":[
			{"rule":"NESTED_LOOP","file":"a.go","element":"F","count":1,"reason":"bounded"}]}`,
		"no count": `{"acceptances":[
			{"rule":"NESTED_LOOP","file":"a.go","element":"F","reviewed":"2026-08-11","reason":"bounded"}]}`,
		"no element": `{"acceptances":[
			{"rule":"NESTED_LOOP","file":"a.go","count":1,"reviewed":"2026-08-11","reason":"bounded"}]}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "accepted.json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadAcceptances(path); err == nil {
				t.Fatal("LoadAcceptances accepted an entry carrying no recorded judgement")
			}
		})
	}
}

func TestLoadAcceptancesEmptyPathIsNoAcceptances(t *testing.T) {
	got, err := LoadAcceptances("")
	if err != nil || got != nil {
		t.Fatalf("LoadAcceptances(%q) = %v, %v; want nil, nil", "", got, err)
	}
}

// The report has to be able to say "reviewed, and nothing was accepted"
// differently from "nobody has looked at this".
func TestAcceptedCellDistinguishesZeroFromUnreviewed(t *testing.T) {
	if got := acceptedCell(0); got != "-" {
		t.Errorf("acceptedCell(0) = %q, want %q", got, "-")
	}
	if got := acceptedCell(58); got != "58" {
		t.Errorf("acceptedCell(58) = %q, want %q", got, "58")
	}
}

// A rule with nothing open and findings accepted still produces an issue, so
// the tracker ticket can be updated to say there is nothing left to do. Without
// it the issue keeps its last body for ever and the ticket cannot be closed on
// evidence.
func TestBuildIssuesEmitsAResolvedIssueForAFullyAcceptedRule(t *testing.T) {
	res := AcceptanceResult{
		ByRule:  map[string]int{RuleNestedLoop: 58},
		Reasons: map[string][]string{RuleNestedLoop: {"flat traversal"}},
		Refs:    map[string][]string{RuleNestedLoop: {"reviews/r.md"}},
	}
	issues := BuildIssues(nil, res, "abc1234")

	if len(issues) != 1 {
		t.Fatalf("issues = %d, want 1", len(issues))
	}
	if issues[0].Findings != 0 {
		t.Errorf("Findings = %d, want 0", issues[0].Findings)
	}
	for _, want := range []string{"Nothing open", "can be closed", "flat traversal", "reviews/r.md"} {
		if !strings.Contains(issues[0].Body, want) {
			t.Errorf("body missing %q:\n%s", want, issues[0].Body)
		}
	}
}

func TestBuildIssuesSkipsARuleWithNothingOpenAndNothingAccepted(t *testing.T) {
	if issues := BuildIssues(nil, AcceptanceResult{}, "abc1234"); len(issues) != 0 {
		t.Fatalf("issues = %d, want none", len(issues))
	}
}

func TestWriteTextShowsTheAcceptedColumnAndTheReasons(t *testing.T) {
	res := AcceptanceResult{
		ByRule:  map[string]int{RuleNestedLoop: 58},
		Reasons: map[string][]string{RuleNestedLoop: {"flat traversal"}},
		Refs:    map[string][]string{RuleNestedLoop: {"reviews/r.md"}},
	}
	var buf bytes.Buffer
	if err := WriteText(&buf, nil, res); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"ACCEPTED", "NESTED_LOOP", "58", "flat traversal", "reviews/r.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("WriteText output missing %q:\n%s", want, out)
		}
	}
}

// The file the repository actually ships has to load, satisfy the schema, and
// still describe the code. The stale check is what makes the last part true, and
// it only runs when something reads the file -- so this test reads it.
func TestShippedAcceptanceFileIsValid(t *testing.T) {
	accs, err := LoadAcceptances(configFile(t, "accepted.json"))
	if err != nil {
		t.Fatalf("accepted.json: %v", err)
	}
	if len(accs) == 0 {
		t.Fatal("accepted.json carries no entries")
	}
	for _, a := range accs {
		if !isReviewRule(a.Rule) {
			t.Errorf("%s: only a review rule (%v) may carry acceptances; the "+
				"threshold rules are fixed by changing the code, not by judging it",
				a.key(), reviewRules)
		}
		// Deliberately not a required directory prefix. Where a repository files
		// its review write-ups is its own layout decision, and the check that
		// actually matters -- that the ref resolves to a file on disk -- is made
		// by TestShippedAcceptanceRefsResolveToAWriteUp below. All this asks is that the ref is
		// a relative path to a write-up rather than a bare word or a URL.
		if a.Ref == "" || strings.HasPrefix(a.Ref, "/") || strings.Contains(a.Ref, "://") {
			t.Errorf("%s: ref %q is not a repository-relative path", a.key(), a.Ref)
		}
		if !strings.Contains(a.Ref, "/") || !strings.HasSuffix(a.Ref, ".md") {
			t.Errorf("%s: ref %q does not point at a review write-up", a.key(), a.Ref)
		}
	}
}

// refPath resolves a ref recorded in accepted.json to a path on disk.
// Refs are written relative to the repository root because that is where a
// reader opens them from.
func refPath(t *testing.T, ref string) string {
	t.Helper()
	return filepath.Join(repoRoot(t), filepath.FromSlash(ref))
}

// Every ref has to resolve to a write-up that is actually there.
//
// TestShippedAcceptanceFileIsValid checks the prefix, which catches a ref
// pointing somewhere else entirely and nothing else. A ref naming a file that
// was renamed, never committed, or simply mistyped passes it, and the entry then
// carries a verdict whose reasoning cannot be read -- which is the same failure
// as the stale entries this file was reconciled for on 2026-08-20, one level up:
// the record drifts from what it describes and nothing says so.
func TestShippedAcceptanceRefsResolveToAWriteUp(t *testing.T) {
	accs, err := LoadAcceptances(configFile(t, "accepted.json"))
	if err != nil {
		t.Fatalf("accepted.json: %v", err)
	}

	seen := map[string]bool{}
	for _, a := range accs {
		if seen[a.Ref] {
			continue
		}
		seen[a.Ref] = true
		if _, statErr := os.Stat(refPath(t, a.Ref)); statErr != nil {
			t.Errorf("%s: ref %q names no write-up: %v", a.key(), a.Ref, statErr)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no refs were checked")
	}

	// The negative half, so the loop above cannot pass by resolving everything
	// to the same directory or by silently not resolving at all.
	if _, statErr := os.Stat(refPath(t, "reviews/no-such-review.md")); statErr == nil {
		t.Fatal("a ref naming a file that does not exist resolved; the check is vacuous")
	}
}
