package main

import (
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixture is one branch's worth of movement, built so the three properties
// come out at three different scores. A single number repeated across all three
// would pass whether or not they are computed independently.
//
//   - order.go: one 90-line unit becomes three 10-line ones. Good for size
//     (90 high-risk lines left, 30 low-risk arrived) and good for interfacing
//     (4 parameters became 2), but the replacements still sit over the
//     complexity boundary at McCabe 7, so only the 60-line drop counts there.
//   - handler.go is new: 40 lines that are over the size and complexity
//     boundaries and under the interfacing one.
//   - util/x.go and the TypeScript file are untouched and must not move
//     anything.
const beforeCSV = `language,element,file,line,loc,mccabe,params,unit_size_category
Go,Big,internal/store/order.go,10,90,12,4,very high
Go,Small,internal/store/order.go,120,8,2,1,low
Go,Stable,internal/util/x.go,3,12,2,1,low
TypeScript,useThing,app/x.ts,3,40,8,3,high
`

const afterCSV = `language,element,file,line,loc,mccabe,params,unit_size_category
Go,BigA,internal/store/order.go,10,10,7,2,low
Go,BigB,internal/store/order.go,25,10,7,2,low
Go,BigC,internal/store/order.go,40,10,7,2,low
Go,Small,internal/store/order.go,120,8,2,1,low
Go,Stable,internal/util/x.go,3,12,2,1,low
Go,NewMess,internal/api/handler.go,5,40,9,1,high
TypeScript,useThing,app/x.ts,3,40,8,3,high
`

func write(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// fixture writes the two inventories and returns their paths.
func fixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	before := filepath.Join(dir, "before.csv")
	after := filepath.Join(dir, "after.csv")
	if err := os.WriteFile(before, []byte(beforeCSV), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(after, []byte(afterCSV), 0o644); err != nil {
		t.Fatal(err)
	}
	return before, after
}

// invoke runs the command the way a shell would, and hands back what it
// printed. The flag set is replaced rather than reset because package-level
// flags can only be registered once and every case here registers them again.
func invoke(t *testing.T, args ...string) (int, string, error) {
	t.Helper()

	savedArgs, savedFlags, savedStdout := os.Args, flag.CommandLine, os.Stdout
	t.Cleanup(func() {
		os.Args, flag.CommandLine, os.Stdout = savedArgs, savedFlags, savedStdout
	})

	flag.CommandLine = flag.NewFlagSet("deltagate", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	os.Args = append([]string{"deltagate"}, args...)

	captured := filepath.Join(t.TempDir(), "stdout")
	f, err := os.Create(captured)
	if err != nil {
		t.Fatalf("capture stdout: %v", err)
	}
	os.Stdout = f

	code, runErr := run()

	os.Stdout = savedStdout
	if err := f.Close(); err != nil {
		t.Fatalf("close capture: %v", err)
	}
	raw, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	return code, string(raw), runErr
}

// TestScoresEachPropertySeparately is the tool's whole claim. One branch
// produces three different verdicts because the three properties draw their
// low-risk boundary in different places, and a reader has to be able to see
// which one the change hurt.
func TestScoresEachPropertySeparately(t *testing.T) {
	before, after := fixture(t)

	code, out, err := invoke(t, "-before", before, "-after", after)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 0 {
		t.Errorf("exit %d, want 0 without -gate", code)
	}

	for _, want := range []string{
		"Unit size", "0.75",
		"Unit complexity", "0.60",
		"Unit interfacing", "1.00",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
}

// TestTextReportStampsTheRevision keeps the range on the report. A score
// pasted into a pull request without the two revisions it was taken between
// says nothing that can be checked later.
func TestTextReportStampsTheRevision(t *testing.T) {
	before, after := fixture(t)

	_, out, err := invoke(t, "-before", before, "-after", after, "-rev", "main..HEAD")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "Delta maintainability (main..HEAD)") {
		t.Errorf("no revision range in the header:\n%s", out)
	}
}

// TestUntouchedLanguageReportsNoScore holds the distinction that keeps a gate
// honest. The TypeScript surface is identical on both sides, and printing 0.00
// for it would fail every floor for a language the branch never opened.
func TestUntouchedLanguageReportsNoScore(t *testing.T) {
	before, after := fixture(t)

	_, out, err := invoke(t, "-before", before, "-after", after, "-format", "json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	var got jsonReport
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("parse json: %v\n%s", err, out)
	}

	var ts *jsonLanguage
	for i := range got.Languages {
		if got.Languages[i].Language == "TypeScript" {
			ts = &got.Languages[i]
		}
	}
	if ts == nil {
		t.Fatalf("no TypeScript entry in %s", out)
	}
	if ts.FilesTouched != 0 {
		t.Errorf("FilesTouched = %d, want 0", ts.FilesTouched)
	}
	for _, p := range ts.Properties {
		if p.Measured {
			t.Errorf("%s reports measured for an untouched language", p.ID)
		}
		// null, not 0: a dashboard averaging this must not fold a silence in
		// as a regression.
		if p.Score != nil {
			t.Errorf("%s score = %v, want null", p.ID, *p.Score)
		}
	}
}

// TestGateFailsOnlyOnAMeasuredBreach pins both halves of the exit code: the
// floor is compared against the properties that moved, and the run stays at 0
// until -gate asks for enforcement.
func TestGateFailsOnlyOnAMeasuredBreach(t *testing.T) {
	before, after := fixture(t)

	tests := []struct {
		name string
		args []string
		want int
	}{
		{"floor under every score passes", []string{"-floor", "0.5", "-gate"}, 0},
		{"floor above unit complexity fails", []string{"-floor", "0.7", "-gate"}, 1},
		{"a breach without -gate is advisory", []string{"-floor", "0.7"}, 0},
		{"no floor is no verdict", []string{"-gate"}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"-before", before, "-after", after}, tt.args...)
			code, out, err := invoke(t, args...)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if code != tt.want {
				t.Errorf("exit %d, want %d:\n%s", code, tt.want, out)
			}
		})
	}
}

// TestAdvisoryRunSaysWhyItPassed keeps the one output a reader could otherwise
// misread: a breach that did not fail the build has to say so on the spot.
func TestAdvisoryRunSaysWhyItPassed(t *testing.T) {
	before, after := fixture(t)

	_, out, err := invoke(t, "-before", before, "-after", after, "-floor", "0.7")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "Below the floor of 0.70") {
		t.Errorf("no breach listed:\n%s", out)
	}
	if !strings.Contains(out, "-gate was not set") {
		t.Errorf("advisory run does not say it exits 0:\n%s", out)
	}
}

// TestColumnsAreFoundByName is why this reads the header rather than counting
// commas. The two inventories can come from different builds of the scanner,
// and a column added between them must move nothing.
func TestColumnsAreFoundByName(t *testing.T) {
	before, _ := fixture(t)
	reordered := write(t, "after.csv",
		`element,loc,file,params,mccabe,language,line,extra_new_column
BigA,10,internal/store/order.go,2,7,Go,10,x
BigB,10,internal/store/order.go,2,7,Go,25,x
BigC,10,internal/store/order.go,2,7,Go,40,x
Small,8,internal/store/order.go,1,2,Go,120,x
Stable,12,internal/util/x.go,1,2,Go,3,x
NewMess,40,internal/api/handler.go,1,9,Go,5,x
useThing,40,app/x.ts,3,8,TypeScript,3,x
`)

	_, out, err := invoke(t, "-before", before, "-after", reordered)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"0.75", "0.60", "1.00"} {
		if !strings.Contains(out, want) {
			t.Errorf("reordered columns changed the answer, %q missing:\n%s", want, out)
		}
	}
}

// TestPathSeparatorsDoNotReadAsARename covers the one difference between a scan
// run on a developer's Windows machine and the same scan in CI. Left alone,
// every file would look deleted and re-added, and the score would collapse
// towards 0.5 with no code having changed.
func TestPathSeparatorsDoNotReadAsARename(t *testing.T) {
	windows := write(t, "before.csv",
		`language,element,file,line,loc,mccabe,params
Go,Stable,internal\util\x.go,3,12,2,1
`)
	posix := write(t, "after.csv",
		`language,element,file,line,loc,mccabe,params
Go,Stable,./internal/util/x.go,3,12,2,1
`)

	_, out, err := invoke(t, "-before", windows, "-after", posix)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "0 files touched") {
		t.Errorf("separator difference read as a change:\n%s", out)
	}
	if strings.Contains(out, "added") {
		t.Errorf("separator difference read as a rename:\n%s", out)
	}
}

// TestWritesToAFileWhenAsked covers -out and the markdown rendering together,
// since the two are only ever used that way: a CI step writing a PR comment.
func TestWritesToAFileWhenAsked(t *testing.T) {
	before, after := fixture(t)
	dest := filepath.Join(t.TempDir(), "delta.md")

	_, _, err := invoke(t, "-before", before, "-after", after,
		"-format", "markdown", "-out", dest, "-rev", "main..HEAD", "-floor", "0.5")
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	raw, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read %s: %v", dest, err)
	}
	body := string(raw)
	for _, want := range []string{
		"## Delta maintainability",
		"`main..HEAD`",
		"| Unit size | 120 | 40 | 160 | 0.75 |",
		"**Every measured property is at or above the floor of 0.50.**",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("markdown is missing %q:\n%s", want, body)
		}
	}
}

// TestReportsWhyItCouldNotRun covers the refusals. Each of these is a way to
// get a clean-looking report out of a run that measured the wrong thing, which
// is the failure this repository treats as the worst one available.
func TestReportsWhyItCouldNotRun(t *testing.T) {
	before, after := fixture(t)
	missingColumn := write(t, "bad.csv", "language,element,file,line,loc,mccabe\nGo,F,a.go,1,10,2\n")
	shortRow := write(t, "short.csv", "language,element,file,line,loc,mccabe,params\nGo,F,a.go\n")
	empty := write(t, "empty.csv", "language,element,file,line,loc,mccabe,params\n")
	headerless := write(t, "headerless.csv", "")
	ragged := write(t, "ragged.csv", "language,element\n\"unterminated\n")

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no inputs", nil, "both -before and -after are required"},
		{"only one input", []string{"-before", before}, "both -before and -after are required"},
		{"floor out of range", []string{"-before", before, "-after", after, "-floor", "1.5"}, "outside the score's range"},
		{"unknown format", []string{"-before", before, "-after", after, "-format", "yaml"}, "unknown -format"},
		{"missing input file", []string{"-before", before, "-after", filepath.Join(t.TempDir(), "nope.csv")}, "nope.csv"},
		{"missing column", []string{"-before", before, "-after", missingColumn}, `no "params" column`},
		{"short row", []string{"-before", before, "-after", shortRow}, "columns, want"},
		{"no header at all", []string{"-before", before, "-after", headerless}, "is empty"},
		{"malformed csv", []string{"-before", before, "-after", ragged}, "read "},
		{"nothing measured on either side", []string{"-before", empty, "-after", empty}, "no units in either"},
		{"unwritable -out", []string{
			"-before", before, "-after", after,
			"-out", filepath.Join(t.TempDir(), "no-such-dir", "delta.txt"),
		}, "delta.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := invoke(t, tt.args...)
			if err == nil {
				t.Fatalf("no error, want one mentioning %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

// TestEmptyBeforeSideIsAValidRun is the first-scan case: there is no baseline
// yet, so every unit reads as new. That has to produce a report rather than an
// error, because it is what the tool does on the commit that introduces it.
func TestEmptyBeforeSideIsAValidRun(t *testing.T) {
	empty := write(t, "empty.csv", "language,element,file,line,loc,mccabe,params\n")
	_, after := fixture(t)

	code, out, err := invoke(t, "-before", empty, "-after", after)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 0 {
		t.Errorf("exit %d, want 0", code)
	}
	if !strings.Contains(out, "Go: 3 files touched (3 added, 0 removed)") {
		t.Errorf("first run does not report every file as added:\n%s", out)
	}
}
