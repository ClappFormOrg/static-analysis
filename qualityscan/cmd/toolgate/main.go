// Command toolgate refuses a local quality gate when the binary about to run it
// is not the binary the merge gate runs.
//
// Two local checks were answering off the wrong tool. `make lint-api-gate` calls
// golangci-lint off PATH, and on a machine carrying the chocolatey package PATH
// resolves to 2.11.3 while CI installs the pin, so the developer's run and the
// run that blocks the merge disagree about the same tree. govulncheck is worse
// than merely different: built with a Go below the module's `go` directive it
// prints "no vulnerabilities found" while skipping the analysis entirely, so
// the wrong tool reports success.
//
// Both checks are the same shape -- ask the binary what it is, compare that to
// what the repository pins, exit non-zero on a disagreement -- so they live in
// one command:
//
//	toolgate -tool golangci-lint -pin .golangci-version
//	toolgate -tool govulncheck -gomod api/go.mod
//	toolgate -tool actionlint -pin .github/.actionlint-version
//
// The pin file is meant to be the single source every reader consults -- the
// build, the workflow that installs the tool, the cache warmer -- on the same
// reasoning as covergate's .coverage-floor: a version spelled in three places
// drifts in three directions.
//
// -bin points the check at a specific binary. The caller must pass the same
// value to the check and to the run it guards, or the check verifies one tool
// and the gate runs another.
//
// Exit status: 0 when the binary matches, 1 when it does not, 2 when the tool
// could not reach an answer.
package main

import (
	"flag"
	"fmt"
	"go/version"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// exitMismatch is returned to the shell when the interrogation succeeded and
// the answer was "not the pinned tool". Distinct from the exit 2 used for a
// binary that could not be run at all, so a failed Makefile target tells a
// version skew apart from a missing install.
const exitMismatch = 1

func main() {
	code, err := run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "toolgate: %v\n", err)
		os.Exit(2)
	}
	os.Exit(code)
}

func run() (int, error) {
	var (
		tool  = flag.String("tool", "", "check to run: golangci-lint, actionlint or govulncheck (required)")
		bin   = flag.String("bin", "", "binary to interrogate (default: -tool's name, resolved on PATH)")
		pin   = flag.String("pin", "", "file holding the pinned version (required by -tool golangci-lint and -tool actionlint)")
		gomod = flag.String("gomod", "", "go.mod whose go directive govulncheck must have been built with (required by -tool govulncheck)")
	)
	flag.Parse()

	binary := *bin
	if binary == "" {
		binary = *tool
	}

	switch *tool {
	case "golangci-lint":
		if *pin == "" {
			return 0, fmt.Errorf("-tool golangci-lint needs -pin")
		}
		return checkGolangciLint(binary, *pin)
	case "actionlint":
		if *pin == "" {
			return 0, fmt.Errorf("-tool actionlint needs -pin")
		}
		return checkActionlint(binary, *pin)
	case "govulncheck":
		if *gomod == "" {
			return 0, fmt.Errorf("-tool govulncheck needs -gomod")
		}
		return checkGovulncheck(binary, *gomod)
	case "":
		return 0, fmt.Errorf("-tool is required (golangci-lint, actionlint or govulncheck)")
	default:
		return 0, fmt.Errorf("unknown -tool %q; expected golangci-lint, actionlint or govulncheck", *tool)
	}
}

// --- golangci-lint: exact equality with the pin ---------------------------

// checkGolangciLint compares the version golangci-lint reports against the pin
// file. Equality, not a floor: a newer linter reports different findings from
// the one CI installs, so a developer running ahead of the pin is as misled as
// one running behind it. A suppression audit turns on exactly this: one patch
// release can put findings under a threshold the next keeps above it.
func checkGolangciLint(binary, pinPath string) (int, error) {
	want, err := readPin(pinPath)
	if err != nil {
		return 0, err
	}

	// No directory: golangci-lint's version is fixed when it is built and does
	// not move with the module it is pointed at.
	out, resolved, err := interrogate(binary, "", "version")
	if err != nil {
		return 0, fmt.Errorf("%w\ninstall the pinned %s with: make lint-api-install", err, want)
	}

	got, err := parseGolangciVersion(out)
	if err != nil {
		return 0, err
	}

	if pinSatisfied(got, want) {
		fmt.Printf("golangci-lint %s, matching the pin in %s\n", got, filepath.ToSlash(pinPath))
		return 0, nil
	}

	fmt.Fprintf(os.Stderr, mismatchLint, got, want, filepath.ToSlash(pinPath), resolved)
	return exitMismatch, nil
}

// mismatchLint names the version found, the version pinned, and the file the
// pin came from. It also names the binary: the case this gate exists for is a
// second copy of the tool earlier on PATH, and "found 2.11.3" stays a puzzle
// until it is followed by which file that was.
const mismatchLint = `golangci-lint version disagrees with the pin.
  found:  %s
  pinned: %s  (%s)
  binary: %s

The pinned build is the one that gates the merge, so a run at any other version
answers a different question from CI. Install the pin with "make lint-api-install",
then put its install directory ahead of every other golangci-lint on PATH.
`

// golangciVersionRE pulls the version out of `golangci-lint version`, which
// reports it mid-sentence and without a leading v:
//
//	golangci-lint has version 2.12.2 built with go1.26.6 from ... on ...
var golangciVersionRE = regexp.MustCompile(`has version\s+(\S+)`)

// parseGolangciVersion returns the version golangci-lint reports for itself.
func parseGolangciVersion(out string) (string, error) {
	m := golangciVersionRE.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("no version in `golangci-lint version` output:\n%s", strings.TrimSpace(out))
	}
	return m[1], nil
}

// readPin reads the pinned version out of the pin file. The whole file is the
// version: one bare token, no comments, no second line. Four readers consume
// this file -- this check, `make lint-api-install`, ci.yml and cache-warm.yml --
// and the last three read it with `tr -d`, so a grammar only one of them
// understands would reintroduce exactly the drift the file exists to remove.
// Anything richer than a single token is rejected rather than half-read.
func readPin(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read pin: %w", err)
	}
	pin := strings.TrimSpace(string(raw))
	if pin == "" {
		return "", fmt.Errorf("%s holds no version", filepath.ToSlash(path))
	}
	if strings.ContainsAny(pin, " \t\r\n") {
		return "", fmt.Errorf("%s: expected one bare version and nothing else, got %q",
			filepath.ToSlash(path), pin)
	}
	return pin, nil
}

// pinSatisfied reports whether the version the linter named for itself is the
// pinned one. Equality in both directions: ahead of the pin is as wrong as
// behind it, because either way the findings differ from the ones CI produces.
func pinSatisfied(got, want string) bool {
	return normalise(got) == normalise(want)
}

// normalise makes the pin's module form (v2.12.2) comparable to the form the
// binary prints (2.12.2), so the pin file stays the string `go install` itself
// consumes while still comparing equal to what the tool reports.
func normalise(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

// --- actionlint: exact equality with the pin ------------------------------

// checkActionlint compares the version actionlint reports against the pin file.
// Equality for the reason golangci-lint takes equality: actionlint carries its
// own catalogue of action metadata and its own shellcheck integration, so a
// version other than the one the merge gate installs answers a different
// question about the same workflows.
//
// The shellcheck it drives is deliberately not pinned. It comes from the runner
// image in CI and from PATH locally, and its findings on .github/workflows are
// suppressed in place at the three sites that raise them, so a shellcheck bump
// moves nothing this gate protects.
func checkActionlint(binary, pinPath string) (int, error) {
	want, err := readPin(pinPath)
	if err != nil {
		return 0, err
	}

	// No directory: actionlint's version is fixed when it is built and does not
	// move with the workflows it is pointed at.
	out, resolved, err := interrogate(binary, "", "-version")
	if err != nil {
		return 0, fmt.Errorf("%w\ninstall the pinned %s with: make lint-actions-install", err, want)
	}

	got, err := parseActionlintVersion(out)
	if err != nil {
		return 0, err
	}

	if pinSatisfied(got, want) {
		fmt.Printf("actionlint %s, matching the pin in %s\n", got, filepath.ToSlash(pinPath))
		return 0, nil
	}

	fmt.Fprintf(os.Stderr, mismatchActions, got, want, filepath.ToSlash(pinPath), resolved)
	return exitMismatch, nil
}

// mismatchActions names the version found, the version pinned and the binary,
// on the same reasoning as mismatchLint: the case this gate exists for is a
// second copy of the tool earlier on PATH.
const mismatchActions = `actionlint version disagrees with the pin.
  found:  %s
  pinned: %s  (%s)
  binary: %s

The pinned build is the one that gates the merge, so a run at any other version
answers a different question from CI. Install the pin with
"make lint-actions-install", then put its install directory ahead of every other
actionlint on PATH.
`

// parseActionlintVersion returns the version actionlint reports for itself.
// `actionlint -version` prints the version alone on the first line, then how it
// was installed and what compiled it:
//
//	1.7.7
//	installed by building from source
//	built with go1.26.6 compiler for linux/amd64
//
// A downloaded release prints the same first line and different lines under it,
// so the first non-empty line is the whole answer.
func parseActionlintVersion(out string) (string, error) {
	for line := range strings.SplitSeq(out, "\n") {
		got := strings.TrimSpace(line)
		if got == "" {
			continue
		}
		if !actionlintVersionRE.MatchString(got) {
			return "", fmt.Errorf("`actionlint -version` reported an unreadable version %q", got)
		}
		return got, nil
	}
	return "", fmt.Errorf("`actionlint -version` printed nothing")
}

// actionlintVersionRE matches the bare or v-prefixed semver actionlint prints,
// and nothing else. Anchored, so a line of prose is reported as unreadable
// rather than half-read into a version that then fails the comparison for the
// wrong reason.
var actionlintVersionRE = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// --- govulncheck: at or above the module's go directive -------------------

// checkGovulncheck compares the Go govulncheck reports against the module's own
// go directive. A floor rather than equality: a newer Go analyses the tree
// correctly, an older one reports "no vulnerabilities found" while skipping the
// analysis, and only the second is worth refusing. ci.yml records verifying that
// failure mode -- a scanner running Go 1.23 reported clean on a tree Go 1.26.5
// found findings in -- and gets the right toolchain from setup-go reading the
// same directive this check reads.
func checkGovulncheck(binary, gomodPath string) (int, error) {
	want, err := readGoDirective(gomodPath)
	if err != nil {
		return 0, err
	}

	// Interrogated from the module directory, because the Go it reports is the
	// toolchain it resolves there and not one baked in at build time. Asked from
	// the repository root -- which is not a module -- the same binary answered
	// go1.23.3, and asked from api/ it answered go1.26.6, Go's own toolchain
	// switching having read the directive. The scan runs in api/, so that is
	// where the question has to be put for the answer to describe the scan.
	out, resolved, err := interrogate(binary, filepath.Dir(gomodPath), "-version")
	if err != nil {
		return 0, fmt.Errorf("%w\nbuild it with %s using: make vuln-api-install", err, want)
	}

	got, err := parseGovulncheckGo(out)
	if err != nil {
		return 0, err
	}

	if goSatisfies(got, want) {
		fmt.Printf("govulncheck reports Go %s, at or above %s's go directive (%s)\n",
			got, filepath.ToSlash(gomodPath), want)
		return 0, nil
	}

	fmt.Fprintf(os.Stderr, mismatchVuln, got, want, filepath.ToSlash(gomodPath), resolved)
	return exitMismatch, nil
}

// mismatchVuln states what the scanner would have done had the gate let it run,
// because a passing govulncheck is indistinguishable from a skipped one at the
// terminal and that is the whole hazard.
const mismatchVuln = `govulncheck would scan with a Go below the module's directive.
  reports Go: %s
  required:   %s  (go directive in %s)
  binary:     %s

Below the module's go directive govulncheck prints "no vulnerabilities found"
without analysing anything, so this run would report a clean tree it never
looked at. The Go it reports is the one it resolves where it scans, so check
that GOTOOLCHAIN is not pinned below the directive, then reinstall the scanner
with "make vuln-api-install".
`

// goSatisfies reports whether the Go govulncheck named is at or above the
// module's directive. go/version rather than a string compare, which gets
// go1.26.10 wrong against go1.26.9 and orders a release candidate above the
// release it precedes.
func goSatisfies(got, want string) bool {
	return version.Compare(got, want) >= 0
}

// parseGovulncheckGo pulls the Go version out of `govulncheck -version`, whose
// first line names the toolchain that built it:
//
//	Go: go1.26.6
//	Scanner: govulncheck@v1.7.0
func parseGovulncheckGo(out string) (string, error) {
	for line := range strings.SplitSeq(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Go:")
		if !ok {
			continue
		}
		got := strings.TrimSpace(rest)
		if !version.IsValid(got) {
			return "", fmt.Errorf("`govulncheck -version` reported an unreadable Go version %q", got)
		}
		return got, nil
	}
	return "", fmt.Errorf("no `Go:` line in `govulncheck -version` output:\n%s", strings.TrimSpace(out))
}

// goDirectiveRE matches the go directive and not the toolchain line beneath it.
// The directive is what govulncheck's analysis is measured against and what
// setup-go resolves from in CI, so it is the one this check reads.
var goDirectiveRE = regexp.MustCompile(`(?m)^go[ \t]+([0-9]\S*)`)

// readGoDirective returns the module's go directive in the form go/version
// compares, e.g. go1.26.6.
func readGoDirective(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read go.mod: %w", err)
	}
	m := goDirectiveRE.FindSubmatch(raw)
	if m == nil {
		return "", fmt.Errorf("%s has no go directive", filepath.ToSlash(path))
	}
	got := "go" + string(m[1])
	if !version.IsValid(got) {
		return "", fmt.Errorf("%s declares an unreadable go directive %q", filepath.ToSlash(path), string(m[1]))
	}
	return got, nil
}

// --- Running the binary --------------------------------------------------

// interrogate runs the binary's version flag from dir and returns its output
// alongside the path PATH actually resolved to. An empty dir inherits this
// process's own, for a tool whose answer does not depend on where it is asked.
func interrogate(binary, dir, versionArg string) (out, resolved string, err error) {
	resolved, lookErr := exec.LookPath(binary)
	if lookErr != nil {
		return "", "", fmt.Errorf("%s is not runnable: %w", binary, lookErr)
	}

	// CombinedOutput because the two tools differ on which stream the banner
	// goes to, and a non-zero status still leaves a parseable banner behind.
	cmd := exec.Command(resolved, versionArg)
	cmd.Dir = dir
	raw, runErr := cmd.CombinedOutput()
	if len(raw) == 0 && runErr != nil {
		return "", resolved, fmt.Errorf("`%s %s` failed: %w", filepath.ToSlash(resolved), versionArg, runErr)
	}
	return string(raw), resolved, nil
}
