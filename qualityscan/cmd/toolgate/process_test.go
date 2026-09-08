package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// ─── The stub binary ──────────────────────────────────────────────────────
//
// Everything below the parsers is a process boundary: the gate is only worth
// having if it runs the right binary, from the right directory, with the flag
// that tool answers, and reads what comes back off whichever stream it lands
// on. None of that can be exercised against the real golangci-lint -- a machine
// without it would skip the tests, and a machine with the wrong one is the
// situation this command exists to detect.
//
// So the test binary stands in for all three tools. It is copied under each
// tool's name into a directory the test puts on PATH, and TestMain hands
// control to runStub when it sees the marker environment variable, which the
// child inherits. That is the TestHelperProcess pattern from os/exec's own
// tests, minus the -test.run round trip: the stub answers before the testing
// package parses a flag, so it cannot recurse into the suite.

const (
	// stubMarker turns the copied test binary into the stub. It is unset in the
	// parent until a test asks for a stub, so the suite itself runs normally.
	stubMarker = "TOOLGATE_STUB"

	// The banner the stub prints, settable per stream because the tools
	// disagree about which one a version banner belongs on.
	stubStdout = "TOOLGATE_STUB_STDOUT"
	stubStderr = "TOOLGATE_STUB_STDERR"

	// The status the stub exits with. A version flag that failed is not a
	// version flag that printed nothing, and interrogate treats them apart.
	stubExit = "TOOLGATE_STUB_EXIT"

	// Where the stub writes down the directory it was started in and the
	// arguments it was given, which is the only way from here to see what the
	// command asked and where it asked it.
	stubRecord = "TOOLGATE_STUB_RECORD"
)

func TestMain(m *testing.M) {
	if os.Getenv(stubMarker) != "" {
		os.Exit(runStub())
	}
	code := m.Run()
	if stubRoot != "" {
		os.RemoveAll(stubRoot)
	}
	os.Exit(code)
}

// runStub is the whole of the fake tool: say where you were run and with what,
// print the banner you were told to print, exit with the status you were told
// to exit with.
func runStub() int {
	if path := os.Getenv(stubRecord); path != "" {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, "stub: getwd:", err)
			return 3
		}
		lines := append([]string{wd}, os.Args[1:]...)
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "stub: record:", err)
			return 3
		}
	}
	fmt.Fprint(os.Stdout, os.Getenv(stubStdout))
	fmt.Fprint(os.Stderr, os.Getenv(stubStderr))
	code, err := strconv.Atoi(os.Getenv(stubExit))
	if err != nil {
		return 0
	}
	return code
}

// stubRoot holds the copies, and TestMain removes it. It cannot be a t.TempDir:
// the copies are made once for the whole suite and would otherwise be deleted
// out from under every test after the first.
var (
	stubRoot    string
	installOnce = sync.OnceValues(installStubs)
)

func installStubs() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate the test binary: %w", err)
	}
	raw, err := os.ReadFile(self)
	if err != nil {
		return "", fmt.Errorf("read the test binary: %w", err)
	}
	dir, err := os.MkdirTemp("", "toolgate-stubs-")
	if err != nil {
		return "", err
	}
	for _, tool := range []string{"golangci-lint", "actionlint", "govulncheck"} {
		if err := os.WriteFile(filepath.Join(dir, exeName(tool)), raw, 0o755); err != nil {
			return "", fmt.Errorf("copy the test binary to %s: %w", tool, err)
		}
	}
	stubRoot = dir
	return dir, nil
}

// exeName gives the stub the extension the platform's lookup insists on.
// Without .exe, LookPath on Windows walks straight past the file.
func exeName(tool string) string {
	if runtime.GOOS == "windows" {
		return tool + ".exe"
	}
	return tool
}

// stubOnPATH installs the stubs, puts their directory first on PATH and points
// the named tool at the given banner. It returns the path the stub sits at, so
// a test can check that a message names the binary that was actually run.
//
// The directory goes ahead of the existing PATH rather than replacing it, which
// is both what the failure looks like in the wild -- a second copy of the tool
// earlier on PATH -- and what keeps a machine with the real tool installed from
// behaving differently from one without it.
func stubOnPATH(t *testing.T, tool, banner string) string {
	t.Helper()
	dir, err := installOnce()
	if err != nil {
		t.Fatalf("install stub binaries: %v", err)
	}
	t.Setenv(stubMarker, "1")
	t.Setenv(stubStdout, banner)
	t.Setenv(stubStderr, "")
	t.Setenv(stubExit, "0")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return filepath.Join(dir, exeName(tool))
}

// recordFile arranges for the next stub run to write down where it ran and with
// what, and returns the reader for that record.
func recordFile(t *testing.T) func() (dir string, args []string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "record")
	t.Setenv(stubRecord, path)
	return func() (string, []string) {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("the stub left no record, so it was never run: %v", err)
		}
		lines := strings.Split(string(raw), "\n")
		return lines[0], lines[1:]
	}
}

// sameFile answers whether two paths name one file or directory, which string
// equality does not: a temporary directory reaches the child through the
// shortened form Windows hands out and through the symlinked /var on macOS.
func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	fa, err := os.Stat(a)
	if err != nil {
		t.Fatalf("stat %s: %v", a, err)
	}
	fb, err := os.Stat(b)
	if err != nil {
		t.Fatalf("stat %s: %v", b, err)
	}
	return os.SameFile(fa, fb)
}

// capture swaps the two standard streams for files while fn runs. The checks
// print their verdict rather than returning it, and the verdict is the part a
// developer acts on, so it has to be readable from here.
func capture(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	dir := t.TempDir()
	out := openCapture(t, filepath.Join(dir, "stdout"))
	errs := openCapture(t, filepath.Join(dir, "stderr"))

	saveOut, saveErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = out, errs
	defer func() {
		os.Stdout, os.Stderr = saveOut, saveErr
		out.Close()
		errs.Close()
	}()
	fn()

	return readBack(t, out), readBack(t, errs)
}

func openCapture(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create capture file: %v", err)
	}
	return f
}

func readBack(t *testing.T, f *os.File) string {
	t.Helper()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("rewind capture file: %v", err)
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	return string(raw)
}

// ─── Interrogating a binary ───────────────────────────────────────────────

// TestInterrogateReadsBothStreams pins the CombinedOutput decision. golangci-lint
// puts its banner on stdout and other tools put theirs on stderr, and a check
// reading only one of them would report "no version in the output" for a
// perfectly good binary -- which reads as a broken install and sends the reader
// hunting for a second copy that is not there.
func TestInterrogateReadsBothStreams(t *testing.T) {
	for _, tc := range []struct{ name, stdout, stderr string }{
		{"banner on stdout", golangciPinned, ""},
		{"banner on stderr", "", golangciPinned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubOnPATH(t, "golangci-lint", tc.stdout)
			t.Setenv(stubStderr, tc.stderr)

			out, _, err := interrogate("golangci-lint", "", "version")
			if err != nil {
				t.Fatalf("interrogate: %v", err)
			}
			if !strings.Contains(out, "2.12.2") {
				t.Errorf("output = %q, want the banner the binary printed", out)
			}
		})
	}
}

// TestInterrogateKeepsABannerFromAFailingRun is the other half of the same
// decision. Some builds print their version and then exit non-zero, and the
// banner is a usable answer whatever the status; only a run that produced
// nothing at all leaves the check with nothing to compare.
func TestInterrogateKeepsABannerFromAFailingRun(t *testing.T) {
	stubOnPATH(t, "golangci-lint", golangciPinned)
	t.Setenv(stubExit, "1")

	out, _, err := interrogate("golangci-lint", "", "version")
	if err != nil {
		t.Fatalf("interrogate errored on a run that printed a banner: %v", err)
	}
	if !strings.Contains(out, "2.12.2") {
		t.Errorf("output = %q, want the banner the binary printed before failing", out)
	}
}

func TestInterrogateReportsARunThatPrintedNothing(t *testing.T) {
	// Nothing on either stream and a non-zero status: there is no answer to
	// parse, and reporting it here names the binary that failed rather than
	// leaving parseGolangciVersion to complain about an empty banner.
	stubOnPATH(t, "golangci-lint", "")
	t.Setenv(stubExit, "2")

	if out, _, err := interrogate("golangci-lint", "", "version"); err == nil {
		t.Fatalf("interrogate returned %q and no error for a silent failure", out)
	}
}

func TestInterrogateReportsABinaryThatIsNotThere(t *testing.T) {
	_, _, err := interrogate("toolgate-no-such-binary", "", "-version")
	if err == nil {
		t.Fatal("interrogate on an absent binary returned no error")
	}
	if !strings.Contains(err.Error(), "toolgate-no-such-binary") {
		t.Errorf("error = %q, want it to name the binary that could not be run", err)
	}
}

// TestInterrogateResolvesThroughPATH covers the value the mismatch messages
// exist to print. The failure this gate was written for is a second copy of the
// tool earlier on PATH, and "found 2.11.3" stays a puzzle until it is followed
// by which file that was, so the resolved path has to be the one PATH chose and
// not the name the caller passed.
func TestInterrogateResolvesThroughPATH(t *testing.T) {
	want := stubOnPATH(t, "golangci-lint", golangciPinned)

	_, resolved, err := interrogate("golangci-lint", "", "version")
	if err != nil {
		t.Fatalf("interrogate: %v", err)
	}
	if resolved == "golangci-lint" {
		t.Fatal("resolved is the name that was passed in, not the file PATH chose")
	}
	if !sameFile(t, resolved, want) {
		t.Errorf("resolved = %q, want the stub at %q", resolved, want)
	}
}

// TestInterrogateRunsWhereItIsTold pins the difference between the two kinds of
// tool. golangci-lint's version is fixed when it is built, so it is asked from
// wherever the command happens to be running; govulncheck's answer is the
// toolchain it resolves where it is asked, so it has to be asked in the module
// directory. An empty dir means "here", and without it exec would leave the
// child in the parent's directory by accident rather than by decision.
func TestInterrogateRunsWhereItIsTold(t *testing.T) {
	here, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	elsewhere := t.TempDir()

	for _, tc := range []struct{ name, dir, want string }{
		{"an empty dir inherits this process's own", "", here},
		{"a named dir is where the binary runs", elsewhere, elsewhere},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubOnPATH(t, "golangci-lint", golangciPinned)
			read := recordFile(t)

			if _, _, err := interrogate("golangci-lint", tc.dir, "version"); err != nil {
				t.Fatalf("interrogate: %v", err)
			}
			ran, _ := read()
			if !sameFile(t, ran, tc.want) {
				t.Errorf("the binary ran in %q, want %q", ran, tc.want)
			}
		})
	}
}

// ─── golangci-lint, end to end ────────────────────────────────────────────

func TestCheckGolangciLintPasses(t *testing.T) {
	binary := stubOnPATH(t, "golangci-lint", golangciPinned)
	read := recordFile(t)
	pin := writePin(t, "v2.12.2\n")

	var (
		code int
		err  error
	)
	stdout, stderr := capture(t, func() { code, err = checkGolangciLint(binary, pin) })
	if err != nil {
		t.Fatalf("checkGolangciLint: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit %d on a binary that matches the pin, want 0", code)
	}
	if !strings.Contains(stdout, "2.12.2") {
		t.Errorf("stdout = %q, want it to name the version it accepted", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want a passing check to be quiet on stderr", stderr)
	}

	// `golangci-lint version` and not `--version`: the tool answers the
	// subcommand, and the flag gets a usage banner carrying no version.
	if _, args := read(); len(args) != 1 || args[0] != "version" {
		t.Errorf("asked with %q, want [version]", args)
	}
}

// TestCheckGolangciLintFailsOnTheVersionItWasWrittenFor runs the exact pair the
// command exists for: the chocolatey package resolving ahead of the pinned
// install. The status has to be the mismatch code rather than the one for a
// tool that could not be run, because a failing Makefile target is read through
// that number.
func TestCheckGolangciLintFailsOnTheVersionItWasWrittenFor(t *testing.T) {
	binary := stubOnPATH(t, "golangci-lint", golangciChocolatey)
	pin := writePin(t, "v2.12.2\n")

	var (
		code int
		err  error
	)
	_, stderr := capture(t, func() { code, err = checkGolangciLint(binary, pin) })
	if err != nil {
		t.Fatalf("checkGolangciLint errored, want a clean mismatch verdict: %v", err)
	}
	if code != exitMismatch {
		t.Fatalf("exit %d on a version mismatch, want %d", code, exitMismatch)
	}
	for _, want := range []string{"2.11.3", "v2.12.2", filepath.ToSlash(pin), binary, "make lint-api-install"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("mismatch message is missing %q:\n%s", want, stderr)
		}
	}
}

// TestCheckGolangciLintReportsAMissingInstall separates the two ways the gate
// says no. A tool that is not installed needs a different fix from a tool at the
// wrong version, and the message has to carry the install command rather than a
// version comparison against nothing.
func TestCheckGolangciLintReportsAMissingInstall(t *testing.T) {
	pin := writePin(t, "v2.12.2\n")

	_, err := checkGolangciLint("toolgate-no-such-binary", pin)
	if err == nil {
		t.Fatal("checkGolangciLint on an absent binary returned no error")
	}
	for _, want := range []string{"toolgate-no-such-binary", "make lint-api-install", "v2.12.2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q:\n%v", want, err)
		}
	}
}

// TestCheckGolangciLintRefusesAnUnreadablePin keeps a broken pin file from
// reading as "nothing to compare against, carry on". The pin is the whole
// authority here, so a file the check cannot read has to stop it before it runs
// anything.
func TestCheckGolangciLintRefusesAnUnreadablePin(t *testing.T) {
	binary := stubOnPATH(t, "golangci-lint", golangciPinned)
	record := recordFile(t)

	if _, err := checkGolangciLint(binary, writePin(t, "# no version here\n")); err == nil {
		t.Fatal("checkGolangciLint accepted a pin file holding no version")
	}
	if _, err := os.Stat(os.Getenv(stubRecord)); err == nil {
		ran, _ := record()
		t.Errorf("the binary was run from %q despite an unreadable pin", ran)
	}
}

func TestCheckGolangciLintRefusesAnUnreadableBanner(t *testing.T) {
	// A binary answering `version` with something that is not a version banner
	// is not this tool, and saying so beats comparing prose against the pin.
	binary := stubOnPATH(t, "golangci-lint", "usage: golangci-lint <command>\n")

	if code, err := checkGolangciLint(binary, writePin(t, "v2.12.2\n")); err == nil {
		t.Fatalf("checkGolangciLint returned %d and no error for an unreadable banner", code)
	}
}

// ─── actionlint, end to end ───────────────────────────────────────────────

func TestCheckActionlintPasses(t *testing.T) {
	binary := stubOnPATH(t, "actionlint", actionlintBanner)
	read := recordFile(t)

	var (
		code int
		err  error
	)
	stdout, _ := capture(t, func() { code, err = checkActionlint(binary, writePin(t, "v1.7.7\n")) })
	if err != nil {
		t.Fatalf("checkActionlint: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit %d on a binary that matches the pin, want 0", code)
	}
	if !strings.Contains(stdout, "1.7.7") {
		t.Errorf("stdout = %q, want it to name the version it accepted", stdout)
	}

	// actionlint takes a flag where golangci-lint takes a subcommand, and the
	// wrong one of the two gets a usage banner that parses as no version.
	if _, args := read(); len(args) != 1 || args[0] != "-version" {
		t.Errorf("asked with %q, want [-version]", args)
	}
}

func TestCheckActionlintFailsOnAVersionOtherThanThePin(t *testing.T) {
	// Ahead of the pin rather than behind it: actionlint carries its own
	// catalogue of action metadata, so a newer build answers a different
	// question about the same workflows just as a stale one does.
	binary := stubOnPATH(t, "actionlint", "1.7.8\ninstalled by building from source\n")

	var (
		code int
		err  error
	)
	_, stderr := capture(t, func() { code, err = checkActionlint(binary, writePin(t, "v1.7.7\n")) })
	if err != nil {
		t.Fatalf("checkActionlint errored, want a clean mismatch verdict: %v", err)
	}
	if code != exitMismatch {
		t.Fatalf("exit %d on a version mismatch, want %d", code, exitMismatch)
	}
	for _, want := range []string{"1.7.8", "v1.7.7", binary, "make lint-actions-install"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("mismatch message is missing %q:\n%s", want, stderr)
		}
	}
}

func TestCheckActionlintReportsAMissingInstall(t *testing.T) {
	_, err := checkActionlint("toolgate-no-such-binary", writePin(t, "v1.7.7\n"))
	if err == nil {
		t.Fatal("checkActionlint on an absent binary returned no error")
	}
	if !strings.Contains(err.Error(), "make lint-actions-install") {
		t.Errorf("error is missing the install command:\n%v", err)
	}
}

func TestCheckActionlintRefusesAnUnreadablePinAndBanner(t *testing.T) {
	binary := stubOnPATH(t, "actionlint", actionlintBanner)
	if _, err := checkActionlint(binary, writePin(t, "1.7.7 1.7.8\n")); err == nil {
		t.Error("checkActionlint accepted a pin file holding two versions")
	}

	t.Setenv(stubStdout, "Usage: actionlint [FLAGS] [FILES]\n")
	if _, err := checkActionlint(binary, writePin(t, "v1.7.7\n")); err == nil {
		t.Error("checkActionlint accepted a usage banner as a version")
	}
}

// ─── govulncheck, end to end ──────────────────────────────────────────────

// TestCheckGovulncheckPasses covers the floor and the directory together, which
// is the pair that makes this check different from the other two. The Go it
// reports is the toolchain it resolves where it is asked, so asking anywhere
// but the module directory describes a scan other than the one about to run.
func TestCheckGovulncheckPasses(t *testing.T) {
	binary := stubOnPATH(t, "govulncheck", govulncheckBanner)
	read := recordFile(t)
	gomod := writeGoMod(t, "module example.com/m\n\ngo 1.26.6\n")

	var (
		code int
		err  error
	)
	stdout, _ := capture(t, func() { code, err = checkGovulncheck(binary, gomod) })
	if err != nil {
		t.Fatalf("checkGovulncheck: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit %d on a scanner at the directive, want 0", code)
	}
	if !strings.Contains(stdout, "go1.26.6") {
		t.Errorf("stdout = %q, want it to name the Go it accepted", stdout)
	}

	ran, args := read()
	if want := filepath.Dir(gomod); !sameFile(t, ran, want) {
		t.Errorf("the scanner was interrogated in %q, want the module directory %q", ran, want)
	}
	if len(args) != 1 || args[0] != "-version" {
		t.Errorf("asked with %q, want [-version]", args)
	}
}

// TestCheckGovulncheckAcceptsAGoAboveTheDirective is the half that must not
// fire. This check is a floor rather than an equality -- a newer Go analyses the
// tree correctly -- and a gate that refused every developer running ahead of the
// directive would be switched off within the week.
func TestCheckGovulncheckAcceptsAGoAboveTheDirective(t *testing.T) {
	binary := stubOnPATH(t, "govulncheck", "Go: go1.27.1\nScanner: govulncheck@v1.7.0\n")
	gomod := writeGoMod(t, "module example.com/m\n\ngo 1.26.6\n")

	var (
		code int
		err  error
	)
	capture(t, func() { code, err = checkGovulncheck(binary, gomod) })
	if err != nil {
		t.Fatalf("checkGovulncheck: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit %d on a scanner above the directive, want 0", code)
	}
}

// TestCheckGovulncheckRefusesAGoBelowTheDirective is the failure mode the check
// was written for: below the directive govulncheck prints "no vulnerabilities
// found" without analysing anything, so a pass here would report a clean tree
// nobody looked at. ci.yml records the case -- a scanner on Go 1.23 reporting
// clean on a tree Go 1.26.5 found findings in.
func TestCheckGovulncheckRefusesAGoBelowTheDirective(t *testing.T) {
	binary := stubOnPATH(t, "govulncheck", "Go: go1.23.3\nScanner: govulncheck@v1.7.0\n")
	gomod := writeGoMod(t, "module example.com/m\n\ngo 1.26.6\n")

	var (
		code int
		err  error
	)
	_, stderr := capture(t, func() { code, err = checkGovulncheck(binary, gomod) })
	if err != nil {
		t.Fatalf("checkGovulncheck errored, want a clean mismatch verdict: %v", err)
	}
	if code != exitMismatch {
		t.Fatalf("exit %d on a scanner below the directive, want %d", code, exitMismatch)
	}
	for _, want := range []string{"go1.23.3", "go1.26.6", binary, "make vuln-api-install", "GOTOOLCHAIN"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("mismatch message is missing %q:\n%s", want, stderr)
		}
	}
}

func TestCheckGovulncheckReportsAMissingInstall(t *testing.T) {
	gomod := writeGoMod(t, "module example.com/m\n\ngo 1.26.6\n")

	_, err := checkGovulncheck("toolgate-no-such-binary", gomod)
	if err == nil {
		t.Fatal("checkGovulncheck on an absent binary returned no error")
	}
	if !strings.Contains(err.Error(), "make vuln-api-install") {
		t.Errorf("error is missing the install command:\n%v", err)
	}
}

func TestCheckGovulncheckRefusesAGoModWithoutADirective(t *testing.T) {
	binary := stubOnPATH(t, "govulncheck", govulncheckBanner)
	if _, err := checkGovulncheck(binary, writeGoMod(t, "module example.com/m\n")); err == nil {
		t.Error("checkGovulncheck accepted a go.mod carrying no go directive")
	}

	t.Setenv(stubStdout, "Scanner: govulncheck@v1.7.0\n")
	gomod := writeGoMod(t, "module example.com/m\n\ngo 1.26.6\n")
	if _, err := checkGovulncheck(binary, gomod); err == nil {
		t.Error("checkGovulncheck accepted a banner with no Go line")
	}
}

// ─── The flags ────────────────────────────────────────────────────────────

// runWith calls run with a fresh flag set, because run registers its flags on
// the package-level CommandLine and a second registration there would panic.
func runWith(t *testing.T, args ...string) (int, error) {
	t.Helper()
	saveArgs, saveFlags := os.Args, flag.CommandLine
	t.Cleanup(func() { os.Args, flag.CommandLine = saveArgs, saveFlags })

	flag.CommandLine = flag.NewFlagSet("toolgate", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	os.Args = append([]string{"toolgate"}, args...)
	return run()
}

// TestRunRequiresTheArgumentsEachCheckNeeds keeps a half-specified invocation an
// error rather than a pass. Every shape below exits 2 through main, and the
// Makefile reads that as "could not reach an answer" -- which is the honest
// answer, where a 0 would tell the developer their tool was verified when the
// check never ran.
func TestRunRequiresTheArgumentsEachCheckNeeds(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no tool at all", nil, "-tool is required"},
		{"a tool this command does not know", []string{"-tool", "staticcheck"}, "staticcheck"},
		{"golangci-lint without a pin", []string{"-tool", "golangci-lint"}, "-pin"},
		{"actionlint without a pin", []string{"-tool", "actionlint"}, "-pin"},
		{"govulncheck without a go.mod", []string{"-tool", "govulncheck"}, "-gomod"},
		{
			// The pin and the go.mod are not interchangeable: each check reads
			// one of them, and the other is no answer at all.
			name: "govulncheck given a pin instead", args: []string{"-tool", "govulncheck", "-pin", "x"},
			want: "-gomod",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, err := runWith(t, tc.args...)
			if err == nil {
				t.Fatalf("run(%q) returned %d and no error", tc.args, code)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestRunDefaultsTheBinaryToTheToolName covers the default that keeps the
// Makefile targets short: with no -bin the check interrogates whatever PATH
// resolves the tool's own name to, which is precisely the binary the guarded run
// would have used.
func TestRunDefaultsTheBinaryToTheToolName(t *testing.T) {
	want := stubOnPATH(t, "golangci-lint", golangciPinned)
	read := recordFile(t)
	pin := writePin(t, "v2.12.2\n")

	var (
		code int
		err  error
	)
	capture(t, func() { code, err = runWith(t, "-tool", "golangci-lint", "-pin", pin) })
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if _, args := read(); len(args) != 1 || args[0] != "version" {
		t.Errorf("asked with %q, want [version]", args)
	}

	// And the binary it reached was the one PATH answers with, rather than some
	// other copy that happened to print the pinned banner.
	resolved, err := exec.LookPath("golangci-lint")
	if err != nil {
		t.Fatalf("the stub is not the golangci-lint on PATH: %v", err)
	}
	if !sameFile(t, resolved, want) {
		t.Errorf("PATH resolved golangci-lint to %q, want the stub at %q", resolved, want)
	}
}

// TestRunHonoursAnExplicitBinary is the other half. The caller must be able to
// point the check at the same binary it points the guarded run at, or the check
// verifies one tool while the gate runs another -- and with PATH offering
// nothing, only -bin can find this one.
func TestRunHonoursAnExplicitBinary(t *testing.T) {
	binary := stubOnPATH(t, "govulncheck", govulncheckBanner)
	read := recordFile(t)
	gomod := writeGoMod(t, "module example.com/m\n\ngo 1.26.6\n")

	// Nothing on PATH answers to govulncheck now, so a run that reaches the stub
	// reached it through -bin.
	t.Setenv("PATH", t.TempDir())

	var (
		code int
		err  error
	)
	capture(t, func() { code, err = runWith(t, "-tool", "govulncheck", "-bin", binary, "-gomod", gomod) })
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if ran, _ := read(); !sameFile(t, ran, filepath.Dir(gomod)) {
		t.Errorf("the scanner ran in %q, want the module directory", ran)
	}
}

// TestRunReturnsTheMismatchStatus walks the status the Makefile reads all the
// way out through run, rather than only out of the check that produced it.
func TestRunReturnsTheMismatchStatus(t *testing.T) {
	binary := stubOnPATH(t, "actionlint", "1.7.8\n")
	pin := writePin(t, "v1.7.7\n")

	var (
		code int
		err  error
	)
	capture(t, func() { code, err = runWith(t, "-tool", "actionlint", "-bin", binary, "-pin", pin) })
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != exitMismatch {
		t.Errorf("exit %d on a version mismatch, want %d", code, exitMismatch)
	}
}
