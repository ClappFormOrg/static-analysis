package main

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// ruleDoc is what turns a count into a ticket someone can pick up: what the
// rule measures, why it is worth anyone's time, and what finishing looks like.
type ruleDoc struct {
	Title    string
	Why      string
	DoneWhen []string
	Labels   []string
}

// maxListed caps the table in an issue body. A ticket listing 486 rows is not a
// ticket anyone reads; the full set is one `make quality-scan` away, and the
// per-package breakdown underneath is what tells you where to start.
const maxListed = 25

var ruleDocs = map[string]ruleDoc{
	RuleCyclicReference: {
		Title: "Break the directory reference cycle in the API",
		Why: "Go rejects an import cycle between packages, so these compile — but the " +
			"directories still point at each other, and neither can be read, moved or " +
			"extracted without the other. The usual cause is a leaf package living under " +
			"the directory that consumes it.",
		DoneWhen: []string{
			"The packages the cycle runs through are moved so no directory imports a descendant of a directory that imports it.",
			"`make quality-scan` reports 0 for `CYCLIC_REFERENCE`.",
		},
		Labels: []string{"area:api", "type:refactor", "priority:high"},
	},
	RuleFunctionSize: {
		Title: "Split the oversized functions in the API",
		Why: "Function length measured in lexical tokens. A function past the threshold " +
			"does not fit on a screen or in a reviewer's head, and is where the " +
			"complexity and parameter findings cluster.",
		DoneWhen: []string{
			"The very-high-risk functions are decomposed into named helpers with their own tests.",
			"No behaviour change: the existing suite passes untouched.",
		},
		Labels: []string{"area:api", "type:refactor", "priority:medium"},
	},
	RuleParameter: {
		Title: "Reduce the parameter counts on the widest API functions",
		Why: "A long parameter list is easy to call wrongly — two adjacent arguments of " +
			"the same type transpose silently, and the compiler says nothing. An options " +
			"struct makes the call site say what each value is.",
		DoneWhen: []string{
			"Functions over the very-high threshold take a struct, or are split.",
			"Call sites name their arguments rather than relying on position.",
		},
		Labels: []string{"area:api", "type:refactor", "priority:medium"},
	},
	RuleFunctionComplexity: {
		Title: "Flatten the most complex functions in the API",
		Why: "Cognitive complexity: each branch costs one, and each level of nesting costs " +
			"one more. It measures how hard a function is to hold in your head, which is " +
			"why deeply nested code scores far above flat code with the same branch count.",
		DoneWhen: []string{
			"Guard clauses replace nesting, and the distinct decisions move into named helpers.",
			"`gocognit` agrees the scores came down.",
		},
		Labels: []string{"area:api", "type:refactor", "priority:medium"},
	},
	RuleDependencyVolume: {
		Title: "Reduce outgoing coupling in the heaviest API files",
		Why: "How many references a file makes to declarations outside itself. A file at the " +
			"top of this list changes whenever anything it touches changes, which is most things.",
		DoneWhen: []string{
			"The file is split along the seams its dependencies already suggest.",
			"No new package is created purely to move the number.",
		},
		Labels: []string{"area:api", "type:refactor", "priority:medium"},
	},
	RuleDependencySpan: {
		Title: "Reduce the dependency span of the widest API files",
		Why: "How many distinct packages a file reaches into. Volume says a file leans hard " +
			"on its neighbours; span says it leans on a lot of different ones, which is the " +
			"harder problem — it is the file that has to know about everything.",
		DoneWhen: []string{
			"The file talks to fewer packages, by delegating rather than by re-exporting.",
		},
		Labels: []string{"area:api", "type:refactor", "priority:medium"},
	},
	RuleHardcodedURL: {
		Title: "Move hardcoded URLs out of API source",
		Why: "A URL compiled into the binary cannot be changed per environment. Some of these " +
			"are legitimate — XML namespaces and RFC links in documentation are not endpoints — " +
			"so this needs a judgement per finding rather than a sweep.",
		DoneWhen: []string{
			"Every URL that addresses a real service comes from configuration.",
			"The rest are confirmed as namespaces or documentation links and left alone.",
		},
		Labels: []string{"area:api", "type:chore", "priority:medium"},
	},
	RuleHardcodedPath: {
		Title: "Move hardcoded filesystem paths out of API source",
		Why: "A path compiled into the binary ties the process to one filesystem layout, which " +
			"breaks the moment the container image or the test harness differs from the author's machine.",
		DoneWhen: []string{
			"Paths come from configuration, a flag, or an embedded file.",
		},
		Labels: []string{"area:api", "type:chore", "priority:medium"},
	},
	RuleCopyrightOrLicense: {
		Title: "Review third-party copyright and license notices in API source",
		Why: "A real copyright or license notice inside our source usually means a snippet was " +
			"copied in along with whichever terms governed it, which is a provenance and " +
			"licensing question, not a style one.",
		DoneWhen: []string{
			"Each notice is either removed because the code was rewritten, or the snippet's " +
				"origin and license are confirmed compatible and recorded.",
		},
		Labels: []string{"area:api", "type:chore", "priority:high"},
	},

	RuleSQLInLoop: {
		Title: "Remove the per-iteration database calls in the API",
		Why: "A query inside a loop is one round trip per element — the N+1 pattern. It is " +
			"invisible on demo data and is the first thing to fall over on production volume. " +
			"The seed tooling builds a 100× estate (`make seed-loadtest`) precisely so these show up.",
		DoneWhen: []string{
			"Each site is either batched into a single query, or documented as bounded with the bound stated.",
			"The hot paths are checked against the load-test seed, not the demo seed.",
		},
		Labels: []string{"area:api", "type:refactor", "priority:high"},
	},
	RuleStringConcatInLoop: {
		Title: "Replace loop string concatenation with strings.Builder",
		Why: "`s += x` in a loop reallocates and copies the whole string every pass, which is " +
			"quadratic in the output length.",
		DoneWhen: []string{"Each site uses `strings.Builder` or `strings.Join`."},
		Labels:   []string{"area:api", "type:refactor", "priority:medium"},
	},
	RuleNestedLoop: {
		Title: "Review the nested loops in the API",
		Why: "Work inside a nested loop runs the product of both counts. Many of these are " +
			"fine — two short slices stay short — so this is a review pass, not a sweep: the " +
			"ones to fix are those where either bound comes from the database.",
		DoneWhen: []string{
			"Each site is confirmed bounded, or the inner lookup is hoisted into a map built once.",
		},
		Labels: []string{"area:api", "type:refactor", "priority:medium"},
	},
	RuleEmptyBranch: {
		Title: "Handle or delete the empty conditional branches in the API",
		Why: "An `if` with an empty body tests a condition and then ignores it. Most are a " +
			"swallowed error someone meant to come back to.",
		DoneWhen: []string{"Each branch either handles the condition or is deleted."},
		Labels:   []string{"area:api", "type:bug", "priority:medium"},
	},
	RuleSwitchWithoutDefault: {
		Title: "Review the switches with no default clause in the API",
		Why: "A switch with no default falls through silently when a new case value appears. " +
			"On an enum that grows — a status, a role, an asset type — that is a bug that ships " +
			"quietly. A switch already naming every member of a closed enum is not, and telling " +
			"the two apart needs types this scanner does without, so this is a review pass. " +
			"Tagless ladders, a switch over a call result, and an empty arm whose comment states " +
			"the fall-through are excluded already.",
		DoneWhen: []string{
			"Each site either gains a default, or is recorded in `accepted.json` with the reason its value set is closed.",
		},
		Labels: []string{"area:api", "type:bug", "priority:medium"},
	},
	RuleDebugStatement: {
		Title: "Remove debug prints from the API",
		Why: "The API logs through `slog`. Anything reaching for `fmt.Println` or the standard " +
			"`log` package writes outside the structured log and is invisible to the log pipeline.",
		DoneWhen: []string{"Each call is deleted or replaced with the injected `slog.Logger`."},
		Labels:   []string{"area:api", "type:chore", "priority:medium"},
	},
	RuleCommentedOutCode: {
		Title: "Delete the commented-out code in the API",
		Why: "Commented-out code is not documentation. Git remembers it; a reader has to " +
			"decide whether it still matters and cannot tell.",
		DoneWhen: []string{"Each block is deleted, or turned into a comment saying why the live code is the way it is."},
		Labels:   []string{"area:api", "type:chore", "priority:low"},
	},
	RuleUnfinishedWork: {
		Title: "Resolve or ticket the TODOs in the API",
		Why: "A TODO in source is work nobody is tracking. Either it is real, in which case it " +
			"belongs in the tracker, or it is not, in which case it belongs in the bin.",
		DoneWhen: []string{"Each marker is resolved, or replaced by a comment citing a tracker issue."},
		Labels:   []string{"area:api", "type:chore", "priority:low"},
	},
	RuleSuppressedWarning: {
		Title: "Review the lint suppressions in the API",
		Why: "Each `//nolint` is a linter finding someone decided not to fix. That is often " +
			"right, and every one of these carries a reason — this is a periodic audit that " +
			"the reasons still hold, not a demand to remove them.",
		DoneWhen: []string{
			"Each suppression is confirmed still necessary, or removed along with what made it necessary.",
			"Any suppression without a stated reason gets one.",
		},
		Labels: []string{"area:api", "type:chore", "priority:low"},
	},
	RuleMissingDoc: {
		Title: "Document the undocumented exported API declarations",
		Why: "An exported declaration with no doc comment gives the next reader nothing but " +
			"its name. This is a large backlog and is best cleared per package, alongside " +
			"other work in that package, rather than as one sweep.",
		DoneWhen: []string{
			"Exported declarations in the packages listed below carry a doc comment starting with the declared name.",
			"Take this per package; closing it in one PR is not the goal.",
		},
		Labels: []string{"area:api", "type:docs", "priority:low"},
	},
}

// Issue is one proposed tracker issue.
type Issue struct {
	Marker   string // stable identity, embedded in the body for idempotent updates
	Title    string
	Labels   []string
	Body     string
	Findings int
}

// BuildIssues groups findings into one issue per rule. Per rule is the split
// that matches how the work is actually done: each rule is one kind of change
// applied in many places, so one ticket carries one decision and one reviewer's
// context. Rules with no findings produce no issue.
func BuildIssues(findings []Finding, reviewed AcceptanceResult, rev string) []Issue {
	byRule := map[string][]Finding{}
	for _, f := range findings {
		byRule[f.Rule] = append(byRule[f.Rule], f)
	}

	var out []Issue
	for _, rule := range ruleOrder {
		list := byRule[rule]
		accepted := reviewed.ByRule[rule]
		// A rule with nothing open and nothing accepted has no issue to file.
		// A rule with nothing open but findings accepted still gets one, and
		// that is the point of the mechanism: the tracker issue is where the
		// count lives, so a rule somebody finished has to be able to say so in
		// the place people read it. Without this arm the issue keeps its last
		// body -- 58 findings, none of them still open -- for ever.
		if len(list) == 0 && accepted == 0 {
			continue
		}
		doc, ok := ruleDocs[rule]
		if !ok {
			continue
		}
		out = append(out, Issue{
			Marker:   "qualityscan:" + rule,
			Title:    doc.Title,
			Labels:   doc.Labels,
			Body:     issueBody(rule, doc, list, reviewed, rev),
			Findings: len(list),
		})
	}
	return out
}

func issueBody(rule string, doc ruleDoc, list []Finding, reviewed AcceptanceResult, rev string) string {
	var b strings.Builder

	accepted := reviewed.ByRule[rule]

	fmt.Fprintf(&b, "<!-- %s -->\n", "qualityscan:"+rule)
	fmt.Fprintf(&b, "**Rule:** `%s` · **Open:** %d", rule, len(list))
	if accepted > 0 {
		fmt.Fprintf(&b, " · **Reviewed and accepted:** %d", accepted)
	}
	if rev != "" {
		fmt.Fprintf(&b, " · **Scanned at:** `%s`", rev)
	}
	b.WriteString("\n\n")

	// The resolved banner goes above the rule's rationale, because for a reader
	// arriving at a stale-looking ticket the first question is "is there
	// anything left to do here", not "why does this rule exist".
	if len(list) == 0 && accepted > 0 {
		fmt.Fprintf(&b, "> **Nothing open.** All %d finding(s) of this rule have been reviewed "+
			"and judged correct as written. This issue can be closed.\n\n", accepted)
	}

	b.WriteString(doc.Why)

	if accepted > 0 {
		b.WriteString("\n\n### Reviewed and accepted\n\n")
		fmt.Fprintf(&b, "%d finding(s) are not listed below. They were read individually and "+
			"judged correct as written:\n\n", accepted)
		for _, reason := range reviewed.Reasons[rule] {
			fmt.Fprintf(&b, "- %s\n", escapePipes(reason))
		}
		for _, ref := range reviewed.Refs[rule] {
			fmt.Fprintf(&b, "\nFull reasoning, one verdict per site: `%s`.\n", ref)
		}
		b.WriteString("\nThese are recorded in the scan's `accepted.json`. " +
			"The record is count-bounded and keyed on the element, so a NEW finding at " +
			"an accepted site is still reported, and an entry that stops matching fails the scan.\n")
	}

	if len(list) == 0 {
		b.WriteString("\n---\n")
		b.WriteString("_Filed by `qualityscan`. Re-running the publisher updates this issue in place " +
			"rather than opening another; the HTML comment at the top is how it finds this one._\n")
		return b.String()
	}

	b.WriteString("\n### Where\n\n")

	b.WriteString("| File | Element | Detail |\n| --- | --- | --- |\n")
	shown := list
	if len(shown) > maxListed {
		shown = shown[:maxListed]
	}
	for _, f := range shown {
		fmt.Fprintf(&b, "| `%s:%d` | `%s` | %s |\n",
			f.File, f.Line, f.Element, escapePipes(f.Description))
	}
	if len(list) > len(shown) {
		fmt.Fprintf(&b, "\n_%d further findings are not listed. Re-run the scan for the full set._\n",
			len(list)-len(shown))
	}

	if counts := byPackage(list); len(counts) > 1 {
		b.WriteString("\n### By package\n\n| Package | Findings |\n| --- | --- |\n")
		for _, c := range counts {
			fmt.Fprintf(&b, "| `%s` | %d |\n", c.name, c.n)
		}
	}

	b.WriteString("\n### Done when\n\n")
	for _, d := range doc.DoneWhen {
		fmt.Fprintf(&b, "- [ ] %s\n", d)
	}

	b.WriteString("\n---\n")
	b.WriteString("_Filed by `qualityscan`. Re-running the publisher updates this issue in place " +
		"rather than opening another; the HTML comment at the top is how it finds this one._\n")
	return b.String()
}

type pkgCount struct {
	name string
	n    int
}

// byPackage groups findings by directory, worst first, so a ticket says where
// to start rather than only how much there is.
func byPackage(list []Finding) []pkgCount {
	counts := map[string]int{}
	for _, f := range list {
		counts[path.Dir(f.File)]++
	}
	out := make([]pkgCount, 0, len(counts))
	for name, n := range counts {
		out = append(out, pkgCount{name, n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].name < out[j].name
	})
	if len(out) > maxListed {
		out = out[:maxListed]
	}
	return out
}

func escapePipes(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

// WriteMarkdown emits one combined report: the summary table, then every
// proposed issue inline. This is the format to commit alongside the other
// reports, or to read before deciding to publish anything.
func WriteMarkdown(w io.Writer, findings []Issue, all []Finding, reviewed AcceptanceResult, rev string) error {
	var b strings.Builder

	b.WriteString("# API quality scan\n\n")
	if rev != "" {
		fmt.Fprintf(&b, "Commit `%s` · ", rev)
	}
	fmt.Fprintf(&b, "%d open findings across %d rules", len(all), len(findings))
	if n := reviewed.Total(); n > 0 {
		fmt.Fprintf(&b, ", plus %d reviewed and accepted", n)
	}
	b.WriteString(".\n\n")
	b.WriteString("Produced by `qualityscan`; see its README for what each rule measures.\n\n")

	b.WriteString("| Rule | Open | Accepted | Proposed issue |\n| --- | --- | --- | --- |\n")
	for _, iss := range findings {
		rule := strings.TrimPrefix(iss.Marker, "qualityscan:")
		fmt.Fprintf(&b, "| `%s` | %d | %s | %s |\n",
			rule, iss.Findings, acceptedCell(reviewed.ByRule[rule]), iss.Title)
	}

	for _, iss := range findings {
		fmt.Fprintf(&b, "\n---\n\n## %s\n\n", iss.Title)
		fmt.Fprintf(&b, "**Labels:** %s\n\n", strings.Join(iss.Labels, ", "))
		b.WriteString(iss.Body)
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// WriteIssueFiles writes one markdown file per proposed issue into dir, each
// with front matter the publisher reads. Useful for reviewing or editing the
// wording before anything reaches the tracker.
func WriteIssueFiles(dir string, issues []Issue) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for i, iss := range issues {
		name := fmt.Sprintf("%02d-%s.md", i+1,
			strings.ToLower(strings.TrimPrefix(iss.Marker, "qualityscan:")))
		var b strings.Builder
		b.WriteString("---\n")
		fmt.Fprintf(&b, "title: %q\n", iss.Title)
		fmt.Fprintf(&b, "labels: [%s]\n", strings.Join(iss.Labels, ", "))
		fmt.Fprintf(&b, "marker: %s\n", iss.Marker)
		b.WriteString("---\n\n")
		b.WriteString(iss.Body)

		if err := os.WriteFile(filepath.Join(dir, name), []byte(b.String()), 0o644); err != nil {
			return err
		}
	}
	return nil
}
