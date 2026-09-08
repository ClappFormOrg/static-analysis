// Command covergate turns a Go coverage profile into a number that can carry a
// threshold, and then enforces one.
//
// The raw profile cannot. `go test ./... -coverprofile` instruments every
// package in the module, generated output included. A tree carrying a large
// protoc surface sits at 0% over a big share of the module's statements, which
// drags the module total far below what the hand-written half measures, so any
// threshold set on the raw figure would be measuring how much protobuf the
// service happens to have generated this month. Regenerate a fat service and
// the number falls with no test having changed. Nothing can be gated on that.
//
// So this tool splits the profile in two, reports the hand-written half, and
// compares it to a floor:
//
//	covergate -dir . -profile cover.out -gate
//
// Generated code is identified by Go's own marker, `// Code generated ... DO
// NOT EDIT.` before the package clause (golang.org/s/generatedcode), not by a
// hardcoded gen/ path. That choice pays off the first time a second generator
// lands outside the directory the list names: its output is excluded on the day
// it appears without anyone editing anything. It cuts both ways rather than
// being a way to hide uncovered code, since a well-covered generated tree
// leaves the denominator too. It is also the same rule golangci-lint applies,
// so a file exempt from lint for being generated is
// exempt from coverage for the same reason. Every excluded package is named in
// the report, so widening the exclusion is visible on the PR that does it
// rather than only in the total.
//
// The marker itself is read by internal/gomarker, shared with the scan so the
// coverage denominator and the scanned-code boundary cannot disagree about
// which files are machine-written.
//
// Exit status: 0 when the report was produced and (under -gate) the floor
// holds, 1 when the floor is breached, 2 when the tool could not run.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ClappFormOrg/static-analysis/qualityscan/internal/gomarker"
)

// exitBelowFloor is returned to the shell when the measurement succeeded and
// the answer was "too low". Distinct from the exit 2 used for tool failure, so
// a caller can tell a coverage regression from a broken invocation.
const exitBelowFloor = 1

func main() {
	code, err := run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "covergate: %v\n", err)
		os.Exit(2)
	}
	os.Exit(code)
}

// defaultFloorFile is the floor's home, relative to -dir. One file, read by
// both `make cover-api-check` and CI, so the threshold is never spelled twice
// and cannot drift between what a developer runs and what blocks a merge.
const defaultFloorFile = ".coverage-floor"

func run() (int, error) {
	var (
		dir     = flag.String("dir", "", "module root the profile was produced from (required)")
		profile = flag.String("profile", "", "coverage profile to read (required)")
		floor   = flag.String("floor", "", "file holding the minimum percentage (default <dir>/"+defaultFloorFile+")")
		gate    = flag.Bool("gate", false, "exit 1 when coverage is below the floor, rather than only reporting it")
		summary = flag.String("summary", "", "append a markdown report to this file, e.g. $GITHUB_STEP_SUMMARY")
		worst   = flag.Int("worst", 15, "how many packages to list, ordered by uncovered statements")
	)
	flag.Parse()

	if *dir == "" || *profile == "" {
		return 0, fmt.Errorf("-dir and -profile are both required")
	}

	floorPath := *floor
	if floorPath == "" {
		floorPath = filepath.Join(*dir, defaultFloorFile)
	}
	minimum, err := readFloor(floorPath)
	if err != nil {
		return 0, err
	}

	module, err := modulePath(*dir)
	if err != nil {
		return 0, err
	}

	raw, err := os.ReadFile(*profile)
	if err != nil {
		return 0, fmt.Errorf("read profile: %w", err)
	}
	files, err := parseProfile(raw)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", filepath.Clean(*profile), err)
	}
	if len(files) == 0 {
		return 0, fmt.Errorf("%s covers no files; was the profile written by a run that compiled anything?",
			filepath.Clean(*profile))
	}

	if err := classify(files, module, *dir); err != nil {
		return 0, err
	}

	rep := summarise(files, module, minimum, floorPath)

	fmt.Print(rep.Text(*worst))
	if *summary != "" {
		if err := appendFile(*summary, rep.Markdown(*worst)); err != nil {
			return 0, fmt.Errorf("write summary: %w", err)
		}
	}

	if *gate && rep.Breached() {
		return exitBelowFloor, nil
	}
	return 0, nil
}

// ─── The profile ──────────────────────────────────────────────────────────

// file is one source file's contribution to the profile. Name is the profile's
// own spelling of it -- import path joined to the base name -- because that is
// the only identity the profile carries and it is what the package grouping is
// derived from.
type file struct {
	Name       string
	Generated  bool
	Statements int
	Covered    int
}

// Package returns the import path the file belongs to, which the profile
// records only as the leading part of Name.
func (f *file) Package() string {
	if i := strings.LastIndex(f.Name, "/"); i >= 0 {
		return f.Name[:i]
	}
	return f.Name
}

// blockLine matches one profile entry: `<name>:<startLine>.<startCol>,<endLine>.<endCol> <numStmt> <count>`.
// Anchored at both ends so a malformed line is reported rather than silently
// contributing nothing, which on a gate would read as a coverage drop with no
// cause.
var blockLine = regexp.MustCompile(`^(.+):(\d+\.\d+,\d+\.\d+) (\d+) (\d+)$`)

// parseProfile reads a Go coverage profile into per-file totals.
//
// Blocks are deduplicated on their file-and-position key, keeping the highest
// execution count. A profile from a plain `go test ./...` has no duplicates --
// each package is instrumented once -- but `-coverpkg` makes one file appear in
// several test binaries, and counting those blocks twice would inflate both
// halves of the ratio by an amount that depends on which packages happen to
// import each other. Deduplicating makes the number independent of how the
// profile was collected.
func parseProfile(raw []byte) (map[string]*file, error) {
	files := make(map[string]*file)
	seen := make(map[string]int)

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	// Profile lines are short, but a generated file can carry long import
	// paths; the default 64 KiB is ample and this only guards against a
	// pathological line stalling the scan silently.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		if text == "" {
			continue
		}
		if line == 1 && strings.HasPrefix(text, "mode:") {
			continue
		}

		m := blockLine.FindStringSubmatch(text)
		if m == nil {
			return nil, fmt.Errorf("line %d is not a coverage block: %q", line, text)
		}
		name, position, stmtText, countText := m[1], m[2], m[3], m[4]

		statements, err := strconv.Atoi(stmtText)
		if err != nil {
			return nil, fmt.Errorf("line %d: statement count %q: %w", line, stmtText, err)
		}
		count, err := strconv.Atoi(countText)
		if err != nil {
			return nil, fmt.Errorf("line %d: execution count %q: %w", line, countText, err)
		}

		key := name + ":" + position
		if previous, dup := seen[key]; dup {
			// Same block from a second test binary. It contributes no new
			// statements; it can only tell us the block was reached after all.
			if count > 0 && previous == 0 {
				files[name].Covered += statements
			}
			if count > previous {
				seen[key] = count
			}
			continue
		}
		seen[key] = count

		f := files[name]
		if f == nil {
			f = &file{Name: name}
			files[name] = f
		}
		f.Statements += statements
		if count > 0 {
			f.Covered += statements
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return files, nil
}

// classify resolves every file in the profile back to disk and records whether
// it is generated.
//
// A name that does not sit under the module path is an error rather than a
// silent pass-through. It would mean the profile covers a dependency, which
// only happens under `-coverpkg`, and quietly folding third-party code into the
// denominator would move the gated number for reasons that have nothing to do
// with the scanned module's own tests.
func classify(files map[string]*file, module, dir string) error {
	prefix := module + "/"
	for _, f := range files {
		relative, ok := strings.CutPrefix(f.Name, prefix)
		if !ok {
			return fmt.Errorf("%s is outside module %s; the profile covers code this floor does not describe",
				f.Name, module)
		}
		path := filepath.Join(dir, filepath.FromSlash(relative))
		src, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s (from profile entry %s): %w", path, f.Name, err)
		}
		f.Generated = gomarker.IsGenerated(src)
	}
	return nil
}

// modulePath reads the module path out of go.mod, so the profile's import paths
// can be turned back into files without the caller having to repeat it.
func modulePath(dir string) (string, error) {
	path := filepath.Join(dir, "go.mod")
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[0] == "module" {
			return fields[1], nil
		}
	}
	return "", fmt.Errorf("%s declares no module path", path)
}

// ─── The floor ────────────────────────────────────────────────────────────

// readFloor reads the single percentage out of the floor file, ignoring `#`
// comments and blank lines. The file is mostly prose explaining the number, and
// keeping the number in that prose -- rather than in a Makefile variable and a
// workflow input -- is what stops the local gate and the CI gate from
// disagreeing.
func readFloor(path string) (float64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read floor: %w", err)
	}

	var values []string
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		values = append(values, line)
	}
	if len(values) != 1 {
		return 0, fmt.Errorf("%s must hold exactly one percentage, found %d non-comment lines",
			filepath.Clean(path), len(values))
	}

	value, err := strconv.ParseFloat(strings.TrimSuffix(values[0], "%"), 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a percentage: %w", filepath.Clean(path), values[0], err)
	}
	if value < 0 || value > 100 {
		return 0, fmt.Errorf("%s: %g is not between 0 and 100", filepath.Clean(path), value)
	}
	return value, nil
}

// ─── The report ───────────────────────────────────────────────────────────

// pkg is one package's hand-written totals. Generated packages are reported
// separately and never appear here.
//
// Path is the package directory relative to the module root -- `internal/store`
// rather than the full import path -- because the report is read as a list of
// places to go and write a test, and fifty characters of repeated module prefix
// in every row is fifty characters of nothing.
type pkg struct {
	Path       string
	Statements int
	Covered    int
}

func (p pkg) Uncovered() int   { return p.Statements - p.Covered }
func (p pkg) Percent() float64 { return percent(p.Covered, p.Statements) }

// report is everything a run measured, separated from how it is rendered so the
// text and markdown forms cannot quote different numbers.
type report struct {
	FloorPath string
	Floor     float64

	Statements int
	Covered    int
	Packages   []pkg

	GeneratedStatements int
	GeneratedFiles      int
	GeneratedPackages   []string
}

func (r report) Percent() float64 { return percent(r.Covered, r.Statements) }

// Breached reports whether the measured coverage is below the floor.
func (r report) Breached() bool { return r.Percent() < r.Floor }

// Headroom is how many further uncovered statements the module can absorb
// before the floor is breached: the largest k for which covered/(total+k) still
// clears the floor. Expressed in statements rather than percentage points
// because that is the unit the next pull request adds code in.
//
// Zero floor means unbounded, so it is reported as zero headroom rather than as
// an infinity nobody asked about.
func (r report) Headroom() int {
	if r.Floor <= 0 || r.Breached() {
		return 0
	}
	return int(math.Floor(float64(r.Covered)*100/r.Floor)) - r.Statements
}

// Shortfall is how many currently-uncovered statements would have to be covered
// to reach the floor, holding the total fixed. The actionable half of a failure
// message: "write tests for this many statements", not "find 0.31 percentage
// points somewhere".
func (r report) Shortfall() int {
	if !r.Breached() {
		return 0
	}
	return int(math.Ceil(r.Floor/100*float64(r.Statements))) - r.Covered
}

// raiseAdvice is the headroom, in percentage points, at which a run starts
// suggesting a higher floor, and the slack it leaves when it does.
//
// Suggesting a raise on any headroom at all would fire on the very first run,
// where a floor is normally set a fraction under what the tree already measures,
// and advice that appears on every green build is advice nobody reads. A whole
// percentage point is a few hundred statements on a module of any size: enough
// that a real body of tests landed, rather than a rounding wobble.
const (
	raiseAdviceThreshold = 1.0
	raiseAdviceSlack     = 0.2
)

// RaiseTo is the floor this run would support, rounded down to a tenth of a
// point and holding raiseAdviceSlack back, or 0 when there is not yet enough
// headroom to be worth moving. Advisory only: a run never fails for being too
// good, but leaving the floor untouched while coverage climbs is how a ratchet
// stops ratcheting.
func (r report) RaiseTo() float64 {
	if r.Breached() || r.Percent()-r.Floor < raiseAdviceThreshold {
		return 0
	}
	candidate := math.Floor((r.Percent()-raiseAdviceSlack)*10) / 10
	if candidate <= r.Floor {
		return 0
	}
	return candidate
}

func summarise(files map[string]*file, module string, floor float64, floorPath string) report {
	r := report{FloorPath: floorPath, Floor: floor}

	// The module root itself is "." rather than an empty string, so a package
	// at the top of the module still names something in the report.
	relative := func(importPath string) string {
		trimmed := strings.TrimPrefix(strings.TrimPrefix(importPath, module), "/")
		if trimmed == "" {
			return "."
		}
		return trimmed
	}

	byPackage := make(map[string]*pkg)
	generated := make(map[string]bool)

	for _, f := range files {
		path := relative(f.Package())
		if f.Generated {
			r.GeneratedStatements += f.Statements
			r.GeneratedFiles++
			generated[path] = true
			continue
		}
		r.Statements += f.Statements
		r.Covered += f.Covered

		p := byPackage[path]
		if p == nil {
			p = &pkg{Path: path}
			byPackage[path] = p
		}
		p.Statements += f.Statements
		p.Covered += f.Covered
	}

	for _, p := range byPackage {
		r.Packages = append(r.Packages, *p)
	}
	// Most uncovered statements first: the ordering that says where the next
	// test is worth writing. Path breaks ties so the report is stable across
	// runs and two runs can be diffed.
	sort.Slice(r.Packages, func(i, j int) bool {
		if r.Packages[i].Uncovered() != r.Packages[j].Uncovered() {
			return r.Packages[i].Uncovered() > r.Packages[j].Uncovered()
		}
		return r.Packages[i].Path < r.Packages[j].Path
	})

	for path := range generated {
		r.GeneratedPackages = append(r.GeneratedPackages, path)
	}
	sort.Strings(r.GeneratedPackages)

	return r
}

// verdict is the one line a reader takes away, in both output formats.
func (r report) verdict() string {
	if r.Breached() {
		return fmt.Sprintf("BELOW FLOOR: %.2f%% is under the %.2f%% floor in %s. "+
			"Covering %s more statements clears it. If the drop is deliberate -- code whose tests "+
			"live in the integration suite, say -- move the floor in that file and say why, "+
			"rather than dropping the check.",
			r.Percent(), r.Floor, filepath.ToSlash(r.FloorPath), thousands(r.Shortfall()))
	}
	line := fmt.Sprintf("OK: %.2f%% clears the %.2f%% floor, with room for %s more uncovered statements.",
		r.Percent(), r.Floor, thousands(r.Headroom()))
	if to := r.RaiseTo(); to > 0 {
		line += fmt.Sprintf("\nThe floor could be raised to %.1f%% in %s.", to, filepath.ToSlash(r.FloorPath))
	}
	return line
}

func (r report) Text(worst int) string {
	var b strings.Builder

	fmt.Fprintf(&b, "\nHand-written statement coverage: %.2f%% (%s of %s statements)\n",
		r.Percent(), thousands(r.Covered), thousands(r.Statements))
	fmt.Fprintf(&b, "Floor:                           %.2f%% (%s)\n", r.Floor, filepath.ToSlash(r.FloorPath))
	fmt.Fprintf(&b, "Excluded as generated:           %s statements in %s files, %s\n",
		thousands(r.GeneratedStatements), thousands(r.GeneratedFiles),
		plural(len(r.GeneratedPackages), "package"))
	for _, path := range r.GeneratedPackages {
		fmt.Fprintf(&b, "                                 %s\n", path)
	}

	shown := r.Packages
	if worst > 0 && len(shown) > worst {
		shown = shown[:worst]
	}
	if len(shown) > 0 {
		fmt.Fprintf(&b, "\n%d of %d packages, most uncovered statements first:\n\n",
			len(shown), len(r.Packages))
		fmt.Fprintf(&b, "  %9s  %7s  %6s  %s\n", "uncovered", "total", "cov", "package")
		for _, p := range shown {
			fmt.Fprintf(&b, "  %9s  %7s  %5.1f%%  %s\n",
				thousands(p.Uncovered()), thousands(p.Statements), p.Percent(), p.Path)
		}
	}

	fmt.Fprintf(&b, "\n%s\n", r.verdict())
	return b.String()
}

func (r report) Markdown(worst int) string {
	var b strings.Builder

	b.WriteString("## API unit coverage\n\n")
	fmt.Fprintf(&b, "**%.2f%%** of %s hand-written statements are covered, against a floor of **%.2f%%**.\n\n",
		r.Percent(), thousands(r.Statements), r.Floor)
	fmt.Fprintf(&b, "%s\n\n", r.verdict())

	shown := r.Packages
	if worst > 0 && len(shown) > worst {
		shown = shown[:worst]
	}
	if len(shown) > 0 {
		fmt.Fprintf(&b, "| Uncovered | Total | Cov | Package (%d of %d) |\n", len(shown), len(r.Packages))
		b.WriteString("| --- | --- | --- | --- |\n")
		for _, p := range shown {
			fmt.Fprintf(&b, "| %s | %s | %.1f%% | `%s` |\n",
				thousands(p.Uncovered()), thousands(p.Statements), p.Percent(), p.Path)
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "_%s statements of generated code excluded (%s files carrying `Code generated ... DO NOT EDIT.`, "+
		"in %s: %s). The floor lives in `%s`._\n\n",
		thousands(r.GeneratedStatements), thousands(r.GeneratedFiles),
		plural(len(r.GeneratedPackages), "package"),
		strings.Join(r.GeneratedPackages, ", "), filepath.ToSlash(r.FloorPath))

	return b.String()
}

// ─── Small helpers ────────────────────────────────────────────────────────

func percent(covered, total int) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(covered) / float64(total)
}

// plural renders a count with its noun. Only the regular -s case arises here.
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// thousands groups digits so five-figure statement counts can be compared at a
// glance. The counts here run to tens of thousands and are read side by side in
// a table, which is exactly where an unseparated 41159 gets misread as 4115.
func thousands(n int) string {
	s := strconv.Itoa(n)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	return sign + strings.Join(append([]string{s}, parts...), ",")
}

// appendFile adds to a file rather than replacing it: the usual destination is
// $GITHUB_STEP_SUMMARY, which several steps in the same job write to.
func appendFile(path, content string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
