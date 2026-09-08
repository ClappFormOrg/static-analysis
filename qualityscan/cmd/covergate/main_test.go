package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── Profile parsing ──────────────────────────────────────────────────────

const profileHeader = "mode: atomic\n"

func TestParseProfileSumsStatementsPerFile(t *testing.T) {
	raw := profileHeader +
		"example.com/m/a/one.go:10.1,12.2 3 1\n" +
		"example.com/m/a/one.go:14.1,16.2 2 0\n" +
		"example.com/m/b/two.go:5.1,6.2 4 7\n"

	files, err := parseProfile([]byte(raw))
	if err != nil {
		t.Fatalf("parseProfile: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2", len(files))
	}

	one := files["example.com/m/a/one.go"]
	if one.Statements != 5 || one.Covered != 3 {
		t.Errorf("one.go = %d/%d statements covered, want 3/5", one.Covered, one.Statements)
	}
	if got := one.Package(); got != "example.com/m/a" {
		t.Errorf("one.go package = %q, want %q", got, "example.com/m/a")
	}

	two := files["example.com/m/b/two.go"]
	if two.Statements != 4 || two.Covered != 4 {
		t.Errorf("two.go = %d/%d statements covered, want 4/4", two.Covered, two.Statements)
	}
}

// TestParseProfileDeduplicatesBlocks pins the behaviour that makes the gated
// number independent of how the profile was collected. Under `-coverpkg` one
// file is instrumented into several test binaries and every block appears once
// per binary; counting them all would inflate the denominator by an amount that
// depends on the import graph, so a refactor that changed nothing but who
// imports whom would move the coverage percentage.
func TestParseProfileDeduplicatesBlocks(t *testing.T) {
	raw := profileHeader +
		"example.com/m/a/one.go:10.1,12.2 3 0\n" +
		"example.com/m/a/one.go:14.1,16.2 2 0\n" +
		// The same two blocks again, from a second test binary that did reach
		// the first one.
		"example.com/m/a/one.go:10.1,12.2 3 4\n" +
		"example.com/m/a/one.go:14.1,16.2 2 0\n"

	files, err := parseProfile([]byte(raw))
	if err != nil {
		t.Fatalf("parseProfile: %v", err)
	}
	one := files["example.com/m/a/one.go"]
	if one.Statements != 5 {
		t.Errorf("statements = %d, want 5 (blocks counted once)", one.Statements)
	}
	if one.Covered != 3 {
		t.Errorf("covered = %d, want 3 (a block reached by any binary is covered)", one.Covered)
	}
}

// TestParseProfileDeduplicationIsOrderIndependent is the other half of the same
// property. The blocks arrive in whatever order the toolchain happened to write
// the test binaries out in, so a block one binary reached and another missed
// must come out covered either way; keeping the last count seen would make the
// gated number depend on that ordering.
func TestParseProfileDeduplicationIsOrderIndependent(t *testing.T) {
	const (
		reached = "example.com/m/a/one.go:10.1,12.2 3 4\n"
		missed  = "example.com/m/a/one.go:10.1,12.2 3 0\n"
	)
	for _, tc := range []struct{ name, body string }{
		{"the binary that reached it comes first", reached + missed},
		{"the binary that missed it comes first", missed + reached},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, err := parseProfile([]byte(profileHeader + tc.body))
			if err != nil {
				t.Fatalf("parseProfile: %v", err)
			}
			one := files["example.com/m/a/one.go"]
			if one.Statements != 3 {
				t.Errorf("statements = %d, want 3 (the block counted once)", one.Statements)
			}
			if one.Covered != 3 {
				t.Errorf("covered = %d, want 3 (a block reached by any binary is covered)", one.Covered)
			}
		})
	}
}

// TestParseProfileRejectsCountsItCannotHold covers the gap the shape check
// leaves open. `\d+` matches a number of any length, so a corrupted or
// truncated profile can satisfy the regex and fail only on conversion, and a
// statement count that fell back to zero would shrink the denominator by
// exactly the amount nobody would notice.
func TestParseProfileRejectsCountsItCannotHold(t *testing.T) {
	const huge = "99999999999999999999"
	for _, tc := range []struct{ name, line, want string }{
		{"statement count", "example.com/m/a/one.go:10.1,12.2 " + huge + " 1", "statement count"},
		{"execution count", "example.com/m/a/one.go:10.1,12.2 3 " + huge, "execution count"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseProfile([]byte(profileHeader + tc.line + "\n"))
			if err == nil {
				t.Fatalf("parseProfile(%q) succeeded, want an error", tc.line)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name the %s", err, tc.want)
			}
		})
	}
}

// TestParseProfileRejectsALineItCannotRead pins the scanner's failure. A line
// past the buffer stops the scan, and stopping quietly would drop that block
// and every block after it -- which on a gate reads as a coverage fall with no
// cause and no diff to blame.
func TestParseProfileRejectsALineItCannotRead(t *testing.T) {
	oversized := "example.com/m/" + strings.Repeat("a", 2<<20) + "/one.go:10.1,12.2 3 1\n"

	files, err := parseProfile([]byte(profileHeader + oversized))
	if err == nil {
		t.Fatalf("parseProfile read %d files off an oversized line, want an error", len(files))
	}
}

// TestParseProfileRejectsMalformedLines matters more than it looks. A line this
// tool silently skipped would remove statements from the denominator, and on a
// gate that reads as a coverage figure that quietly stopped describing the
// module.
func TestParseProfileRejectsMalformedLines(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
	}{
		{"no position", "example.com/m/a/one.go 3 1"},
		{"missing count", "example.com/m/a/one.go:10.1,12.2 3"},
		{"trailing junk", "example.com/m/a/one.go:10.1,12.2 3 1 extra"},
		{"prose", "coverage: 50.2% of statements"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseProfile([]byte(profileHeader + tc.line + "\n")); err == nil {
				t.Fatalf("parseProfile(%q) succeeded, want an error", tc.line)
			}
		})
	}
}

// TestParseProfileIgnoresBlankLines keeps whitespace from being a malformed
// line. The strictness above is deliberate, but it has to be strict about
// content rather than about formatting: a profile that picked up a blank line
// on its way through a shell pipeline still describes the same run, and
// refusing it would fail the gate for a reason that is not about coverage.
func TestParseProfileIgnoresBlankLines(t *testing.T) {
	raw := profileHeader +
		"\n" +
		"example.com/m/a/one.go:10.1,12.2 3 1\n" +
		"\n" +
		"example.com/m/a/one.go:14.1,16.2 2 0\n" +
		"\n"

	files, err := parseProfile([]byte(raw))
	if err != nil {
		t.Fatalf("parseProfile: %v", err)
	}
	one := files["example.com/m/a/one.go"]
	if one == nil || one.Statements != 5 || one.Covered != 3 {
		t.Fatalf("one.go = %+v, want 3 of 5 statements covered", one)
	}
}

func TestParseProfileAcceptsAnEmptyBody(t *testing.T) {
	files, err := parseProfile([]byte(profileHeader))
	if err != nil {
		t.Fatalf("parseProfile: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("got %d files, want 0", len(files))
	}
}

// TestPackageOfANameWithNoSeparator covers the shape a Go profile never writes:
// every entry it produces carries an import path. Handing back the whole name
// rather than an empty string is what keeps a report that somehow saw one
// grouped under something a reader can look up.
func TestPackageOfANameWithNoSeparator(t *testing.T) {
	f := &file{Name: "main.go"}
	if got := f.Package(); got != "main.go" {
		t.Errorf("Package() = %q, want %q", got, "main.go")
	}
}

// TestClassifyRejectsFilesOutsideTheModule guards the denominator. A profile
// covering a dependency (which is what `-coverpkg` on a wider pattern would
// produce) would fold third-party statements into a figure that is supposed to
// describe this repository's own tests.
func TestClassifyRejectsFilesOutsideTheModule(t *testing.T) {
	dir := t.TempDir()
	files := map[string]*file{
		"github.com/other/pkg/x.go": {Name: "github.com/other/pkg/x.go", Statements: 1},
	}
	err := classify(files, "example.com/m", dir)
	if err == nil {
		t.Fatal("classify succeeded on a file outside the module, want an error")
	}
	if !strings.Contains(err.Error(), "outside module") {
		t.Errorf("error = %q, want it to say the file is outside the module", err)
	}
}

func TestClassifyMarksGeneratedFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "gen", "api.pb.go"),
		"// Code generated by protoc-gen-go. DO NOT EDIT.\n\npackage gen\n")
	write(t, filepath.Join(dir, "internal", "store.go"),
		"package store\n")

	files := map[string]*file{
		"example.com/m/gen/api.pb.go":     {Name: "example.com/m/gen/api.pb.go"},
		"example.com/m/internal/store.go": {Name: "example.com/m/internal/store.go"},
	}
	if err := classify(files, "example.com/m", dir); err != nil {
		t.Fatalf("classify: %v", err)
	}
	if !files["example.com/m/gen/api.pb.go"].Generated {
		t.Error("gen/api.pb.go was not classified as generated")
	}
	if files["example.com/m/internal/store.go"].Generated {
		t.Error("internal/store.go was classified as generated")
	}
}

func TestModulePath(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "go.mod"), "// a comment\n\nmodule example.com/m\n\ngo 1.26.6\n")

	got, err := modulePath(dir)
	if err != nil {
		t.Fatalf("modulePath: %v", err)
	}
	if got != "example.com/m" {
		t.Errorf("modulePath = %q, want %q", got, "example.com/m")
	}
}

// TestModulePathRejectsAGoModWithout keeps a missing module path an error. It
// is the prefix every profile entry is stripped of, so an empty one would make
// every name look like it sits outside the module -- a confusing way to report
// a go.mod the tool could read but not understand.
func TestModulePathRejectsAGoModWithout(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"no module line at all", "go 1.27.1\n"},
		{"the keyword with no path", "module\n"},
		{"a second word after the path", "module example.com/m // renamed\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, filepath.Join(dir, "go.mod"), tc.content)
			if got, err := modulePath(dir); err == nil {
				t.Fatalf("modulePath read %q out of %q, want an error", got, tc.content)
			}
		})
	}
}

func TestModulePathRequiresTheFile(t *testing.T) {
	if _, err := modulePath(t.TempDir()); err == nil {
		t.Fatal("modulePath succeeded on a directory with no go.mod, want an error")
	}
}

// ─── The floor file ───────────────────────────────────────────────────────

func TestReadFloor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    float64
		wantErr string
	}{
		{
			name:    "number under prose",
			content: "# why this number is what it is\n#\n# more prose\n\n50.0\n",
			want:    50.0,
		},
		{
			name:    "percent sign tolerated",
			content: "62.5%\n",
			want:    62.5,
		},
		{
			name:    "no value",
			content: "# only comments\n",
			wantErr: "exactly one percentage",
		},
		{
			// The shape a careless edit takes: the old floor left above the new
			// one. Taking either silently would be worse than refusing.
			name:    "two values",
			content: "50.0\n55.0\n",
			wantErr: "exactly one percentage",
		},
		{
			name:    "not a number",
			content: "fifty\n",
			wantErr: "not a percentage",
		},
		{
			name:    "out of range",
			content: "150\n",
			wantErr: "not between 0 and 100",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".coverage-floor")
			write(t, path, tc.content)

			got, err := readFloor(path)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("readFloor = %v, want an error mentioning %q", got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("readFloor: %v", err)
			}
			if got != tc.want {
				t.Errorf("readFloor = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestReadFloorRequiresTheFile stops a missing or renamed floor file from
// reading as "no floor to breach".
func TestReadFloorRequiresTheFile(t *testing.T) {
	if _, err := readFloor(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("readFloor succeeded on a missing file, want an error")
	}
}

// ─── The verdict ──────────────────────────────────────────────────────────

// TestSummariseSplitsGeneratedOut is the whole point of the tool in one
// assertion: the same profile is 50% or 25% depending on whether generated code
// counts, and only one of those two numbers can carry a threshold.
func TestSummariseSplitsGeneratedOut(t *testing.T) {
	files := map[string]*file{
		"m/internal/a.go": {Name: "m/internal/a.go", Statements: 100, Covered: 50},
		"m/internal/b.go": {Name: "m/internal/b.go", Statements: 100, Covered: 50},
		"m/gen/x.pb.go":   {Name: "m/gen/x.pb.go", Statements: 300, Generated: true},
		"m/gen/y.pb.go":   {Name: "m/gen/y.pb.go", Statements: 100, Generated: true},
	}

	r := summarise(files, "m", 50, ".coverage-floor")

	if r.Statements != 200 || r.Covered != 100 {
		t.Fatalf("hand-written = %d/%d, want 100/200", r.Covered, r.Statements)
	}
	if got := r.Percent(); got != 50 {
		t.Errorf("percent = %v, want 50", got)
	}
	if r.GeneratedStatements != 400 || r.GeneratedFiles != 2 {
		t.Errorf("generated = %d statements in %d files, want 400 in 2",
			r.GeneratedStatements, r.GeneratedFiles)
	}
	if len(r.GeneratedPackages) != 1 || r.GeneratedPackages[0] != "gen" {
		t.Errorf("generated packages = %v, want [gen]", r.GeneratedPackages)
	}
	// Both hand-written files sit in one package, and generated code must not
	// appear in the per-package listing at all.
	if len(r.Packages) != 1 || r.Packages[0].Path != "internal" {
		t.Fatalf("packages = %v, want one entry for internal", r.Packages)
	}
}

func TestSummarisePackagesOrderedByUncovered(t *testing.T) {
	files := map[string]*file{
		"m/small/a.go": {Name: "m/small/a.go", Statements: 10, Covered: 9},
		"m/big/a.go":   {Name: "m/big/a.go", Statements: 100, Covered: 10},
		"m/mid/a.go":   {Name: "m/mid/a.go", Statements: 50, Covered: 20},
	}
	r := summarise(files, "m", 50, ".coverage-floor")

	want := []string{"big", "mid", "small"}
	if len(r.Packages) != len(want) {
		t.Fatalf("got %d packages, want %d", len(r.Packages), len(want))
	}
	for i, path := range want {
		if r.Packages[i].Path != path {
			t.Errorf("package %d = %q, want %q", i, r.Packages[i].Path, path)
		}
	}
}

// TestSummariseBreaksTiesByPath pins the half of the ordering that has nothing
// to do with coverage. Packages come out of a map, so without the tie-break two
// runs over the same profile would list equally uncovered packages in different
// orders and the two reports could not be diffed against each other.
func TestSummariseBreaksTiesByPath(t *testing.T) {
	// All three owe exactly ten uncovered statements, so only the path decides.
	files := map[string]*file{
		"m/zulu/a.go":  {Name: "m/zulu/a.go", Statements: 10},
		"m/alpha/a.go": {Name: "m/alpha/a.go", Statements: 10},
		"m/mike/a.go":  {Name: "m/mike/a.go", Statements: 10},
	}
	want := []string{"alpha", "mike", "zulu"}

	// Repeated because a map iteration order that happened to be right once is
	// not the property being claimed.
	for range 8 {
		r := summarise(files, "m", 50, ".coverage-floor")
		for i, path := range want {
			if r.Packages[i].Path != path {
				t.Fatalf("package %d = %q, want %q", i, r.Packages[i].Path, path)
			}
		}
	}
}

// TestSummariseNamesTheModuleRoot covers the one package whose path trims to
// nothing. The listing is read as a set of places to go and write a test, and a
// row whose package column is blank is a row nobody can act on.
func TestSummariseNamesTheModuleRoot(t *testing.T) {
	files := map[string]*file{
		"m/main.go":       {Name: "m/main.go", Statements: 40, Covered: 10},
		"m/internal/a.go": {Name: "m/internal/a.go", Statements: 10, Covered: 5},
	}
	r := summarise(files, "m", 50, ".coverage-floor")

	if len(r.Packages) != 2 {
		t.Fatalf("got %d packages, want 2", len(r.Packages))
	}
	// The root has the most uncovered statements, so it sorts first.
	if r.Packages[0].Path != "." {
		t.Errorf("root package is named %q, want %q", r.Packages[0].Path, ".")
	}
	if r.Packages[1].Path != "internal" {
		t.Errorf("second package = %q, want %q", r.Packages[1].Path, "internal")
	}
}

// TestHeadroomAndShortfallAreExact checks the two numbers a developer acts on.
// Percentage points are not actionable -- statements are -- so both have to be
// right rather than roughly right.
func TestHeadroomAndShortfallAreExact(t *testing.T) {
	for _, tc := range []struct {
		name          string
		covered       int
		statements    int
		floor         float64
		wantBreached  bool
		wantHeadroom  int
		wantShortfall int
	}{
		{
			// The measured position on development at f72f9d99.
			name: "the baseline", covered: 18865, statements: 37609, floor: 50,
			wantHeadroom: 121,
		},
		{
			name: "exactly on the floor", covered: 50, statements: 100, floor: 50,
			wantHeadroom: 0,
		},
		{
			name: "one statement below", covered: 49, statements: 100, floor: 50,
			wantBreached: true, wantShortfall: 1,
		},
		{
			name: "well below", covered: 100, statements: 1000, floor: 50,
			wantBreached: true, wantShortfall: 400,
		},
		{
			name: "no floor set", covered: 1, statements: 1000, floor: 0,
			wantHeadroom: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := report{Covered: tc.covered, Statements: tc.statements, Floor: tc.floor}
			if got := r.Breached(); got != tc.wantBreached {
				t.Fatalf("Breached = %v, want %v (%.4f%% vs %.2f%%)",
					got, tc.wantBreached, r.Percent(), r.Floor)
			}
			if got := r.Headroom(); got != tc.wantHeadroom {
				t.Errorf("Headroom = %d, want %d", got, tc.wantHeadroom)
			}
			if got := r.Shortfall(); got != tc.wantShortfall {
				t.Errorf("Shortfall = %d, want %d", got, tc.wantShortfall)
			}
		})
	}
}

// TestHeadroomIsTheRealLimit walks the headroom figure back into the model it
// claims to describe: adding exactly that many uncovered statements must still
// pass, and one more must fail. A headroom that is off by one is worse than no
// headroom, because it is quoted in the CI summary as a budget.
func TestHeadroomIsTheRealLimit(t *testing.T) {
	base := report{Covered: 18865, Statements: 37609, Floor: 50}
	k := base.Headroom()

	atLimit := report{Covered: base.Covered, Statements: base.Statements + k, Floor: base.Floor}
	if atLimit.Breached() {
		t.Errorf("adding %d uncovered statements breached the floor (%.4f%%), want it to hold",
			k, atLimit.Percent())
	}
	overLimit := report{Covered: base.Covered, Statements: base.Statements + k + 1, Floor: base.Floor}
	if !overLimit.Breached() {
		t.Errorf("adding %d uncovered statements held (%.4f%%), want the floor breached",
			k+1, overLimit.Percent())
	}
}

// TestShortfallIsEnough is the mirror: covering exactly the reported shortfall
// must clear the floor.
func TestShortfallIsEnough(t *testing.T) {
	base := report{Covered: 18000, Statements: 37609, Floor: 50}
	if !base.Breached() {
		t.Fatal("the fixture is not below the floor")
	}
	fixed := report{Covered: base.Covered + base.Shortfall(), Statements: base.Statements, Floor: base.Floor}
	if fixed.Breached() {
		t.Errorf("covering the reported %d statements left %.4f%%, still under the floor",
			base.Shortfall(), fixed.Percent())
	}
}

// TestRaiseTo pins the advisory. Its value is entirely in not firing: advice
// that appears on every green build is advice nobody reads, so the baseline
// position -- 50.16% against a 50.0% floor -- must produce silence.
func TestRaiseTo(t *testing.T) {
	for _, tc := range []struct {
		name       string
		covered    int
		statements int
		floor      float64
		want       float64
	}{
		{name: "the baseline, silent", covered: 18865, statements: 37609, floor: 50, want: 0},
		{name: "a tenth of a point, silent", covered: 5011, statements: 10000, floor: 50, want: 0},
		{name: "just under a point, silent", covered: 5099, statements: 10000, floor: 50, want: 0},
		{name: "a point, less the slack", covered: 5100, statements: 10000, floor: 50, want: 50.8},
		{name: "several points", covered: 6234, statements: 10000, floor: 50, want: 62.1},
		{name: "below the floor", covered: 4000, statements: 10000, floor: 50, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := report{Covered: tc.covered, Statements: tc.statements, Floor: tc.floor}
			if got := r.RaiseTo(); got != tc.want {
				t.Errorf("RaiseTo = %v, want %v (at %.4f%%)", got, tc.want, r.Percent())
			}
		})
	}
}

// TestVerdictSaysWhatToDo pins the failure message. The floor's value depends on
// people fixing it rather than deleting it, and a message that only says "50.2
// < 50.5" invites the second.
func TestVerdictSaysWhatToDo(t *testing.T) {
	failing := report{Covered: 100, Statements: 1000, Floor: 50, FloorPath: ".coverage-floor"}
	got := failing.verdict()
	for _, want := range []string{"BELOW FLOOR", "400", ".coverage-floor", "rather than dropping the check"} {
		if !strings.Contains(got, want) {
			t.Errorf("verdict = %q, want it to mention %q", got, want)
		}
	}

	passing := report{Covered: 700, Statements: 1000, Floor: 50, FloorPath: ".coverage-floor"}
	got = passing.verdict()
	if !strings.Contains(got, "OK:") {
		t.Errorf("verdict = %q, want it to start OK", got)
	}
	if !strings.Contains(got, "raised to 69.8%") {
		t.Errorf("verdict = %q, want it to offer the floor it would support", got)
	}

	// The baseline position says nothing about raising, so a passing run is one
	// line rather than two.
	baseline := report{Covered: 18865, Statements: 37609, Floor: 50, FloorPath: ".coverage-floor"}
	if got := baseline.verdict(); strings.Contains(got, "raised to") {
		t.Errorf("verdict = %q, want no raise advice at 0.16pp of headroom", got)
	}
}

// TestReportsQuoteTheSameNumbers keeps the terminal output and the CI job
// summary from disagreeing, which is how a green PR page ends up next to a red
// step.
func TestReportsQuoteTheSameNumbers(t *testing.T) {
	r := report{
		FloorPath: ".coverage-floor", Floor: 50,
		Statements: 37609, Covered: 18865,
		GeneratedStatements: 41159, GeneratedFiles: 65,
		GeneratedPackages: []string{"gen/go/apiv1"},
		Packages:          []pkg{{Path: "internal/store", Statements: 7962, Covered: 1082}},
	}
	text, markdown := r.Text(15), r.Markdown(15)

	for _, want := range []string{"50.16%", "37,609", "18,865", "41,159", "internal/store"} {
		if !strings.Contains(text, want) {
			t.Errorf("text report is missing %q", want)
		}
	}
	for _, want := range []string{"50.16%", "37,609", "41,159", "internal/store"} {
		if !strings.Contains(markdown, want) {
			t.Errorf("markdown report is missing %q", want)
		}
	}
}

// TestReportsHonourTheWorstLimit pins the truncation and the count beside it.
// The listing is a queue of places to write the next test, so a run that shows
// three rows has to say three of how many -- otherwise a reader cannot tell a
// short list from a short module, and a package that fell off the bottom looks
// like a package with no uncovered code.
func TestReportsHonourTheWorstLimit(t *testing.T) {
	r := report{
		FloorPath: ".coverage-floor", Floor: 50,
		Statements: 300, Covered: 150,
		Packages: []pkg{
			{Path: "internal/store", Statements: 100, Covered: 10},
			{Path: "internal/server", Statements: 100, Covered: 50},
			{Path: "internal/tail", Statements: 100, Covered: 90},
		},
	}

	text, markdown := r.Text(2), r.Markdown(2)
	if !strings.Contains(text, "2 of 3 packages") {
		t.Errorf("text report does not say how many packages it left out\n%s", text)
	}
	if !strings.Contains(markdown, "(2 of 3)") {
		t.Errorf("markdown report does not say how many packages it left out\n%s", markdown)
	}
	for _, out := range []string{text, markdown} {
		if strings.Contains(out, "internal/tail") {
			t.Errorf("the third package was listed under a limit of 2\n%s", out)
		}
	}

	// Zero is the no-limit case rather than the show-nothing case, which is what
	// lets a local run ask for the whole listing.
	all := r.Text(0)
	if !strings.Contains(all, "3 of 3 packages") || !strings.Contains(all, "internal/tail") {
		t.Errorf("-worst 0 truncated the listing\n%s", all)
	}
}

// TestReportsOnAnEmptyModuleSayNothingAboutPackages is the shape a profile with
// only generated code produces. Nothing is divided by nothing, and the reports
// have to render rather than print an empty table with a header over it.
func TestReportsOnAnEmptyModuleSayNothingAboutPackages(t *testing.T) {
	r := report{
		FloorPath:           ".coverage-floor",
		GeneratedStatements: 41159, GeneratedFiles: 65,
		GeneratedPackages: []string{"gen/go/apiv1"},
	}
	if got := r.Percent(); got != 0 {
		t.Errorf("percent of nothing = %v, want 0", got)
	}
	for _, out := range []string{r.Text(15), r.Markdown(15)} {
		if strings.Contains(out, "most uncovered statements first") || strings.Contains(out, "| Uncovered |") {
			t.Errorf("a report with no packages printed a table header\n%s", out)
		}
		if !strings.Contains(out, "41,159") {
			t.Errorf("report does not account for the generated statements\n%s", out)
		}
	}
}

// TestPlural exists because the noun is read next to a count that is usually
// one in this repository -- gen is the only generated package most of the
// time -- so the branch that fires on every real run is the one a test would
// most easily leave to the other.
func TestPlural(t *testing.T) {
	for in, want := range map[int]string{0: "0 packages", 1: "1 package", 2: "2 packages"} {
		if got := plural(in, "package"); got != want {
			t.Errorf("plural(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestThousands(t *testing.T) {
	for in, want := range map[int]string{
		0: "0", 5: "5", 999: "999", 1000: "1,000", 37609: "37,609",
		1234567: "1,234,567", -4200: "-4,200",
	} {
		if got := thousands(in); got != want {
			t.Errorf("thousands(%d) = %q, want %q", in, got, want)
		}
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
