package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func scanIssues(t *testing.T, files map[string]string) []Issue {
	t.Helper()
	idx := load(t, files, DefaultConfig())
	return BuildIssues(Run(idx, ""), AcceptanceResult{}, "abc1234")
}

func TestBuildIssuesOnePerRuleWithFindings(t *testing.T) {
	issues := scanIssues(t, map[string]string{"a/a.go": `package a

// Undocumented has a doc comment, so it is not a finding.
func Undocumented(ids []int) {
	for _, id := range ids {
		db.Where("id = ?", id).Find(&row)
	}
}

func NoDoc() {}
`})

	got := map[string]Issue{}
	for _, iss := range issues {
		if _, dup := got[iss.Marker]; dup {
			t.Fatalf("two issues share the marker %q", iss.Marker)
		}
		got[iss.Marker] = iss
	}

	for _, marker := range []string{"qualityscan:SQL_IN_LOOP", "qualityscan:MISSING_DOC_COMMENT"} {
		if _, ok := got[marker]; !ok {
			t.Errorf("no issue for %s; got %v", marker, keys(got))
		}
	}
	// Rules that found nothing must not produce an empty ticket.
	for _, marker := range []string{"qualityscan:NESTED_LOOP", "qualityscan:CYCLIC_REFERENCE"} {
		if _, ok := got[marker]; ok {
			t.Errorf("issue raised for %s, which found nothing", marker)
		}
	}
}

// The marker is the whole basis of idempotent publishing: it has to appear in
// the body verbatim, or a second publish opens a duplicate instead of updating.
func TestIssueBodyCarriesItsMarker(t *testing.T) {
	for _, iss := range scanIssues(t, map[string]string{"a/a.go": "package a\n\nfunc NoDoc() {}\n"}) {
		if !strings.Contains(iss.Body, iss.Marker) {
			t.Errorf("issue %q body does not contain its marker %q", iss.Title, iss.Marker)
		}
		if !strings.Contains(iss.Body, "<!-- "+iss.Marker+" -->") {
			t.Errorf("issue %q marker is not in an HTML comment, so it would render in the body", iss.Title)
		}
		if iss.Title == "" || len(iss.Labels) == 0 {
			t.Errorf("issue %+v is missing a title or labels", iss)
		}
	}
}

func TestEveryRuleHasIssueDocumentation(t *testing.T) {
	for _, rule := range ruleOrder {
		doc, ok := ruleDocs[rule]
		if !ok {
			t.Errorf("rule %s has no ruleDocs entry, so it can never become an issue", rule)
			continue
		}
		if doc.Title == "" || doc.Why == "" || len(doc.DoneWhen) == 0 || len(doc.Labels) == 0 {
			t.Errorf("rule %s has an incomplete ruleDocs entry: %+v", rule, doc)
		}
	}
}

func TestIssueBodyTruncatesLongFindingLists(t *testing.T) {
	var src strings.Builder
	src.WriteString("package a\n")
	for i := range maxListed + 10 {
		src.WriteString("func NoDoc")
		src.WriteString(string(rune('A' + i%26)))
		src.WriteString(string(rune('a' + i/26)))
		src.WriteString("() {}\n")
	}

	for _, iss := range scanIssues(t, map[string]string{"a/a.go": src.String()}) {
		if iss.Marker != "qualityscan:MISSING_DOC_COMMENT" {
			continue
		}
		rows := strings.Count(iss.Body, "\n| `a/a.go:")
		if rows != maxListed {
			t.Errorf("listed %d findings, want the %d cap", rows, maxListed)
		}
		if !strings.Contains(iss.Body, "further findings are not listed") {
			t.Error("truncated list does not say it was truncated")
		}
	}
}

func TestWriteIssueFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "issues")
	issues := scanIssues(t, map[string]string{"a/a.go": "package a\n\nfunc NoDoc() {}\n"})
	if err := WriteIssueFiles(dir, issues); err != nil {
		t.Fatalf("WriteIssueFiles: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(issues) {
		t.Fatalf("wrote %d files for %d issues", len(entries), len(issues))
	}

	body, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"title:", "labels:", "marker:"} {
		if !strings.Contains(string(body), field) {
			t.Errorf("%s has no %s front matter:\n%s", entries[0].Name(), field, body)
		}
	}
}

func TestWriteMarkdownIncludesEveryIssue(t *testing.T) {
	files := map[string]string{"a/a.go": "package a\n\nfunc NoDoc() {}\n"}
	idx := load(t, files, DefaultConfig())
	findings := Run(idx, "")
	issues := BuildIssues(findings, AcceptanceResult{}, "abc1234")

	var buf bytes.Buffer
	if err := WriteMarkdown(&buf, issues, findings, AcceptanceResult{}, "abc1234"); err != nil {
		t.Fatalf("WriteMarkdown: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "abc1234") {
		t.Error("report does not stamp the revision it was produced from")
	}
	for _, iss := range issues {
		if !strings.Contains(out, iss.Title) {
			t.Errorf("report omits issue %q", iss.Title)
		}
	}
}

func TestMatchByMarker(t *testing.T) {
	existing := []existingIssue{
		{Number: 7, Body: "prose\n<!-- qualityscan:SQL_IN_LOOP -->\nmore", State: "OPEN"},
		{Number: 9, Body: "unrelated issue", State: "OPEN"},
	}
	if got, ok := matchByMarker(existing, "qualityscan:SQL_IN_LOOP"); !ok || got.Number != 7 {
		t.Errorf("matched %+v (ok=%v), want issue 7", got, ok)
	}
	if _, ok := matchByMarker(existing, "qualityscan:NESTED_LOOP"); ok {
		t.Error("matched an issue that carries no such marker")
	}
}

func keys(m map[string]Issue) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
