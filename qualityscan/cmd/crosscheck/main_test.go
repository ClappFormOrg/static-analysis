package main

import (
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const oursCSV = `language,element,file,line,loc,mccabe,params,unit_size_category
Go,Encode,api/internal/pagination/cursor.go,80,13,4,0,low
Go,Decode,api/internal/pagination/cursor.go,98,17,7,2,moderate
Go,OnlyOurs,api/internal/pagination/cursor.go,140,9,3,1,low
TypeScript,useThing,app/x.ts,3,9,2,1,low
`

// lizard writes no header, and on Windows emits a path carrying both separators.
const theirsCSV = `13,4,79,0,13,"Encode@80-92@api/internal/pagination\cursor.go","api/internal/pagination\cursor.go","Encode","(c Cursor)Encode",80,92
34,9,120,2,17,"Decode@98-114@api/internal/pagination\cursor.go","api/internal/pagination\cursor.go","Decode","Decode token , expectOrderBy string",98,114
9,3,50,1,9,"@200-208@api/internal/pagination\cursor.go","api/internal/pagination\cursor.go",""," ",200,208
5,1,20,0,5,"OnlyTheirs@300-304@api/internal/pagination\cursor.go","api/internal/pagination\cursor.go","OnlyTheirs","OnlyTheirs",300,304
`

// TestMatchesAcrossPathSeparatorsAndReportsBothSides holds the mechanics the
// comparison rests on: the two tools' paths line up despite lizard emitting
// Windows separators, each side's total is reported, and lizard's unnamed
// callables are counted separately rather than read as units we missed.
func TestMatchesAcrossPathSeparatorsAndReportsBothSides(t *testing.T) {
	ours, err := readOurs(write(t, "ours.csv", oursCSV), "Go")
	if err != nil {
		t.Fatal(err)
	}
	if len(ours) != 3 {
		t.Fatalf("read %d Go units, want 3 (the TypeScript row is another language)", len(ours))
	}

	them, anonymous, err := readLizard(write(t, "theirs.csv", theirsCSV))
	if err != nil {
		t.Fatal(err)
	}
	if anonymous != 1 {
		t.Errorf("counted %d unnamed callables, want 1", anonymous)
	}
	if _, ok := them["api/internal/pagination/cursor.go:80"]; !ok {
		t.Errorf("a Windows-separated path did not normalise; keys are %v", keys(them))
	}

	b := &strings.Builder{}
	report(b, ours, them, anonymous, 5)
	out := b.String()

	for _, want := range []string{
		"3 ours",              // our inventory
		"3 theirs",            // theirs, with the unnamed one excluded
		"2 matched",           // Encode and Decode
		"1 unnamed callables", // billed inward here, so not a miss
		"ours 17, theirs 34",  // the divergence, both sides shown
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not carry %q:\n%s", want, out)
		}
	}
}

// TestEmptyInputFails holds the rule the scanner applies to itself: a comparison
// that matched nothing looks exactly like one that found no disagreement, so it
// fails rather than printing a clean-looking zero.
func TestEmptyInputFails(t *testing.T) {
	empty := write(t, "empty.csv", "language,element,file,line,loc,mccabe,params\n")
	theirs := write(t, "theirs.csv", theirsCSV)

	if err := run(empty, theirs, "Go", 5); err == nil {
		t.Error("a sigunits CSV with no rows must fail the comparison")
	}
	if err := run(write(t, "ours.csv", oursCSV), write(t, "none.csv", ""), "Go", 5); err == nil {
		t.Error("a lizard CSV with no rows must fail the comparison")
	}
}

func keys(m map[string]measurement) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ─── The entry path ───────────────────────────────────────────────────────

// captureStdout swaps os.Stdout for the duration of fn. Both run and main write
// the report to the real one, so a test that did not do this would push the
// comparison into the test binary's own output.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	defer func() { os.Stdout = old }()

	path := filepath.Join(t.TempDir(), "stdout")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = f
	fn()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// runMain drives main in this process, so the flag wiring is exercised rather
// than assumed: the make recipe spells -ours and -theirs out by name, and a
// rename on either side would leave the target comparing nothing. Only inputs
// that succeed may be passed -- main ends a failed comparison with os.Exit,
// which would take the test binary with it. The other half is covered out of
// process by TestMainExitsTwoOnAFailedComparison.
func runMain(t *testing.T, args ...string) string {
	t.Helper()
	oldArgs, oldFlags := os.Args, flag.CommandLine
	t.Cleanup(func() { os.Args, flag.CommandLine = oldArgs, oldFlags })

	// A fresh flag set per call: main registers its flags on the global one, and
	// registering -ours a second time over the test binary's set panics.
	flag.CommandLine = flag.NewFlagSet(args[0], flag.ContinueOnError)
	os.Args = args
	return captureStdout(t, main)
}

// TestMainComparesTheFilesItIsGiven pins the flags the make recipe passes.
// -language is the one carrying a default, and that default is what keeps a Go
// comparison from folding in the webapp's units, which lizard was never pointed
// at and which would therefore read as a pile of functions it missed.
func TestMainComparesTheFilesItIsGiven(t *testing.T) {
	ours, theirs := write(t, "ours.csv", oursCSV), write(t, "theirs.csv", theirsCSV)

	out := runMain(t, "crosscheck", "-ours", ours, "-theirs", theirs)
	if !strings.Contains(out, "Units: 3 ours, 3 theirs, 2 matched") {
		t.Errorf("the default run did not compare the two files:\n%s", out)
	}

	// The same pair read as TypeScript: one unit on our side, and nothing of
	// lizard's to line it up against. A zero here is honest rather than a
	// failure, because both files did carry rows.
	out = runMain(t, "crosscheck", "-ours", ours, "-theirs", theirs, "-language", "TypeScript")
	if !strings.Contains(out, "Units: 1 ours, 3 theirs, 0 matched") {
		t.Errorf("-language was not applied:\n%s", out)
	}
}

// TestMainExitsTwoOnAFailedComparison pins the exit code the make recipe reads.
// The comparison is advisory and never a gate, but a run that could not read
// its inputs has to stop the target rather than let the previous report stand
// as current.
func TestMainExitsTwoOnAFailedComparison(t *testing.T) {
	if os.Getenv("CROSSCHECK_TEST_SUBPROCESS") == "1" {
		// The child, invoked with neither -ours nor -theirs.
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestMainExitsTwoOnAFailedComparison")
	cmd.Env = append(os.Environ(), "CROSSCHECK_TEST_SUBPROCESS=1")
	out, err := cmd.CombinedOutput()

	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("the child ended with %v, want a non-zero exit:\n%s", err, out)
	}
	if exit.ExitCode() != 2 {
		t.Errorf("exit code %d, want 2:\n%s", exit.ExitCode(), out)
	}
	if !strings.Contains(string(out), "crosscheck:") {
		t.Errorf("the failure was not explained on stderr:\n%s", out)
	}
}

// ─── What run refuses ─────────────────────────────────────────────────────

// TestRunRequiresBothPaths keeps a half-configured invocation from reading as a
// finished comparison. An unset make variable expands to the empty string,
// which is exactly what an omitted flag leaves behind, so the check has to fire
// on one missing path as well as on two.
func TestRunRequiresBothPaths(t *testing.T) {
	ours, theirs := write(t, "ours.csv", oursCSV), write(t, "theirs.csv", theirsCSV)

	for _, tc := range []struct {
		name         string
		ours, theirs string
		wantErr      bool
	}{
		{name: "neither", wantErr: true},
		{name: "only ours", ours: ours, wantErr: true},
		{name: "only theirs", theirs: theirs, wantErr: true},
		{name: "both, the legitimate case", ours: ours, theirs: theirs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			out := captureStdout(t, func() { err = run(tc.ours, tc.theirs, "Go", 5) })

			if tc.wantErr {
				if err == nil {
					t.Fatalf("run succeeded on ours=%q theirs=%q, want an error", tc.ours, tc.theirs)
				}
				if !strings.Contains(err.Error(), "-ours") || !strings.Contains(err.Error(), "-theirs") {
					t.Errorf("error = %q, want it to name both flags", err)
				}
				if out != "" {
					t.Errorf("a rejected run still printed a report:\n%s", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if !strings.Contains(out, "Units: 3 ours, 3 theirs, 2 matched") {
				t.Errorf("a configured run printed no comparison:\n%s", out)
			}
		})
	}
}

// TestRunSurfacesAnUnreadableInput is the rule of TestEmptyInputFails one step
// earlier. A file that is absent or torn has to stop the run, because the
// alternative -- an empty side read as zero units -- is indistinguishable from
// two tools that agreed on everything.
func TestRunSurfacesAnUnreadableInput(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "never-written.csv")
	ours, theirs := write(t, "ours.csv", oursCSV), write(t, "theirs.csv", theirsCSV)
	// A quoted field that never closes: the shape a truncated redirect leaves
	// on disk when a lizard run is killed part-way through.
	torn := write(t, "torn.csv", "13,4,79,0,13,\"unterminated\n")

	for _, tc := range []struct {
		name, ours, theirs, want string
	}{
		{name: "ours absent", ours: absent, theirs: theirs, want: "never-written.csv"},
		{name: "theirs absent", ours: ours, theirs: absent, want: "never-written.csv"},
		{name: "ours torn", ours: torn, theirs: theirs},
		{name: "theirs torn", ours: ours, theirs: torn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			out := captureStdout(t, func() { err = run(tc.ours, tc.theirs, "Go", 5) })
			if err == nil {
				t.Fatalf("run succeeded, want an error; it printed:\n%s", out)
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name the file it could not read", err)
			}
		})
	}
}

// ─── What the two readers keep ────────────────────────────────────────────

// TestReadOursKeepsOneLanguageAndWholeRows holds both halves of the sigunits
// filter. The comparison is per language because lizard is pointed at one
// language at a time, and a row too short to carry a measurement is not a unit
// this tool can compare -- reading one as a unit would put a phantom in our
// total that nothing on lizard's side could ever match.
func TestReadOursKeepsOneLanguageAndWholeRows(t *testing.T) {
	path := write(t, "ours.csv", oursCSV+"Go,Truncated,api/x.go,1,2\n")

	goUnits, err := readOurs(path, "Go")
	if err != nil {
		t.Fatal(err)
	}
	if len(goUnits) != 3 {
		t.Errorf("read %d Go units, want 3; keys are %v", len(goUnits), keys(goUnits))
	}
	if _, ok := goUnits["api/x.go:1"]; ok {
		t.Error("a row with no mccabe or params column was read as a unit")
	}

	// The lookalike: the rows the Go pass skips are real units under their own
	// language, so the filter has to be selecting rather than discarding.
	tsUnits, err := readOurs(path, "TypeScript")
	if err != nil {
		t.Fatal(err)
	}
	if len(tsUnits) != 1 {
		t.Errorf("read %d TypeScript units, want 1; keys are %v", len(tsUnits), keys(tsUnits))
	}
}

// TestReadLizardIgnoresRowsItCannotRead covers the other side of the same
// judgement. lizard's CSV has no header and eleven columns, and a shorter row
// is not a function it measured, so it must neither be counted as a unit nor
// mistaken for one of the unnamed callables.
func TestReadLizardIgnoresRowsItCannotRead(t *testing.T) {
	ragged := "5,1,20,0,5\n" + theirsCSV

	them, anonymous, err := readLizard(write(t, "ragged.csv", ragged))
	if err != nil {
		t.Fatalf("readLizard: %v", err)
	}
	if len(them) != 3 {
		t.Errorf("read %d functions, want the 3 whole rows; keys are %v", len(them), keys(them))
	}
	if anonymous != 1 {
		t.Errorf("counted %d unnamed callables, want 1 -- the short row is not one", anonymous)
	}
}

// ─── The summary numbers ──────────────────────────────────────────────────

// TestReportSummarisesEachMetricSeparately pins the figures the README quotes
// for this calibration. Each metric is its own distribution: units can agree on
// McCabe while disagreeing on lines, and folding them together would hide
// exactly the movement this tool exists to notice.
func TestReportSummarisesEachMetricSeparately(t *testing.T) {
	ours, them := map[string]measurement{}, map[string]measurement{}
	line := 10
	for _, tc := range []struct {
		name             string
		ourLOC, theirLOC int
	}{
		{"Agrees", 10, 10},
		{"AgreesToo", 20, 20},
		{"AgreesAgain", 30, 30},
		{"HalfTheirReading", 10, 20},
		{"TwiceTheirReading", 40, 20},
	} {
		o := measurement{File: "api/x.go", Line: line, Name: tc.name, LOC: tc.ourLOC, McCabe: 3}
		theirs := o
		theirs.LOC = tc.theirLOC
		ours[o.key()], them[theirs.key()] = o, theirs
		line += 10
	}

	b := &strings.Builder{}
	report(b, ours, them, 0, 5)
	out := b.String()

	for _, want := range []string{
		// Three of the five agree on lines, and the two that do not sit at 0.5
		// and 2.0, so the median is still 1.000 and both fall outside the band.
		"identical on 3 of 5 matched units (60.0%)",
		"median ratio ours/theirs 1.000, outside a 0.75-1.33 band: 2",
		// McCabe is identical throughout, which is the shape a healthy run has.
		"identical on 5 of 5 matched units (100.0%)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not carry %q:\n%s", want, out)
		}
	}

	// Every function here takes no parameters, which is legitimate rather than a
	// disagreement. A ratio against zero says nothing, so the units count as
	// agreeing and the distribution line is left off rather than printed as a
	// median over an empty set.
	at := strings.Index(out, "## parameters")
	if at < 0 {
		t.Fatalf("report has no parameters section:\n%s", out)
	}
	if params := out[at:]; strings.Contains(params, "median ratio") {
		t.Errorf("the parameters section quoted a median with no counterparts:\n%s", params)
	}
}

// TestWorstCapsTheListingAndKeepsTheWidest pins the ordering the cap rests on.
// The summary is a handful of numbers precisely so that it gets read, but a cap
// that kept the wrong entries would spend those lines on the two tools
// quibbling and drop the divergence worth looking at. The order is by how far
// apart the readings are, not by how large the function is.
func TestWorstCapsTheListingAndKeepsTheWidest(t *testing.T) {
	ours, them := map[string]measurement{}, map[string]measurement{}
	line := 10
	for _, tc := range []struct {
		name             string
		ourLOC, theirLOC int
	}{
		{"BarelyApart", 12, 10},   // 1.2
		{"SomewhatApart", 15, 10}, // 1.5
		{"Halved", 5, 10},         // 0.5
		{"Quadrupled", 40, 10},    // 4.0
	} {
		o := measurement{File: "api/x.go", Line: line, Name: tc.name, LOC: tc.ourLOC, McCabe: 3}
		theirs := o
		theirs.LOC = tc.theirLOC
		ours[o.key()], them[theirs.key()] = o, theirs
		line += 10
	}

	b := &strings.Builder{}
	report(b, ours, them, 0, 2)
	capped := b.String()

	if !strings.Contains(capped, "api/x.go:40 Quadrupled: ours 40, theirs 10") {
		t.Errorf("the widest divergence was not listed:\n%s", capped)
	}
	if !strings.Contains(capped, "api/x.go:30 Halved: ours 5, theirs 10") {
		t.Errorf("the second widest was not listed:\n%s", capped)
	}
	// A quarter of their reading is further from agreement than half again as
	// much, so the two kept are these and the listing runs widest first.
	if strings.Index(capped, "Quadrupled") > strings.Index(capped, "Halved") {
		t.Errorf("a 0.5 ratio was ranked above a 4.0 one:\n%s", capped)
	}
	for _, unwanted := range []string{"BarelyApart", "SomewhatApart"} {
		if strings.Contains(capped, unwanted) {
			t.Errorf("%s was listed past the cap of 2:\n%s", unwanted, capped)
		}
	}
	if !strings.Contains(capped, "... and 2 more") {
		t.Errorf("the report did not say how many it withheld:\n%s", capped)
	}

	// The lookalike: a cap wider than the divergences prints all of them and
	// says nothing about withholding, so that line stays a signal rather than
	// decoration on every run.
	b = &strings.Builder{}
	report(b, ours, them, 0, 10)
	full := b.String()

	for _, want := range []string{"BarelyApart", "SomewhatApart", "Halved", "Quadrupled"} {
		if !strings.Contains(full, want) {
			t.Errorf("an uncapped report is missing %s:\n%s", want, full)
		}
	}
	if strings.Contains(full, "... and") {
		t.Errorf("an uncapped report claimed to withhold entries:\n%s", full)
	}
}
