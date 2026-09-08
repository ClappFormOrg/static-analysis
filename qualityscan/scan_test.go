package main

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeModule materialises files (path -> contents, slash separated) as a Go
// module rooted at a temp directory and returns that directory.
func writeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	all := map[string]string{"go.mod": "module example.com/m\n\ngo 1.26\n"}
	maps.Copy(all, files)
	for name, body := range all {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// units measures a fixture's Go surface with no component map, which is what a
// test wants unless it is testing the component map itself.
func units(t *testing.T, idx *Index) UnitSet {
	t.Helper()
	set, err := idx.goUnits("", Components{})
	if err != nil {
		t.Fatalf("goUnits: %v", err)
	}
	return set
}

// measured runs the module-level pass over a fixture under the given component
// map, and returns the modules together with the dependencies between them.
func measured(t *testing.T, idx *Index, comps Components) ModuleSet {
	t.Helper()
	set, err := idx.measureModules("", comps)
	if err != nil {
		t.Fatalf("measureModules: %v", err)
	}
	return set
}

// modules is measured() when only the modules matter.
func modules(t *testing.T, idx *Index, comps Components) []Module {
	t.Helper()
	return measured(t, idx, comps).Modules
}

func load(t *testing.T, files map[string]string, cfg Config) *Index {
	t.Helper()
	idx, err := LoadIndex(writeModule(t, files), cfg)
	if err != nil {
		t.Fatalf("LoadIndex: %v", err)
	}
	return idx
}

func TestDependencyMetrics(t *testing.T) {
	idx := load(t, map[string]string{
		"a/one.go": `package a

import (
	"fmt"
	"example.com/m/b"
)

func Run() {
	fmt.Println(helper())
	fmt.Println(b.Do(), b.Do2())
}
`,
		"a/two.go": `package a

func helper() string { return "" }
`,
		"b/b.go": `package b

func Do() string  { return "" }
func Do2() string { return "" }
`,
	}, DefaultConfig())

	var one FileDeps
	for _, d := range idx.measureDeps() {
		if d.File.Rel == "a/one.go" {
			one = d
		}
	}

	// Two fmt.Println, one helper(), two b.* calls.
	if want := 5; one.Volume != want {
		t.Errorf("volume = %d, want %d (targets %v)", one.Volume, want, one.Targets)
	}
	// At package granularity: fmt, example.com/m/b, and the sibling a/two.go.
	if want := 3; one.Span != want {
		t.Errorf("span = %d, want %d (targets %v)", one.Span, want, one.Targets)
	}
}

func TestDependencyMetricsIgnoresLocalShadowsAndFieldNames(t *testing.T) {
	idx := load(t, map[string]string{
		"a/one.go": `package a

type T struct{ helper string }

func Run() {
	// A local named helper shadows the package-level one; neither this
	// binding nor the struct field key below is a reference to it.
	helper := "x"
	_ = T{helper: helper}
}
`,
		"a/two.go": `package a

func helper() string { return "" }
`,
	}, DefaultConfig())

	for _, d := range idx.measureDeps() {
		if d.File.Rel == "a/one.go" && d.Volume != 0 {
			t.Errorf("volume = %d, want 0 (targets %v)", d.Volume, d.Targets)
		}
	}
}

func TestFindCyclesFoldsSubdirectoriesIntoParents(t *testing.T) {
	// server -> service is a plain import; service -> server/interceptors is
	// not a package cycle and Go compiles it happily, but the two directories
	// still point at each other.
	idx := load(t, map[string]string{
		"server/wiring.go": `package server

import "example.com/m/service"

func New() { service.New() }
`,
		"server/interceptors/auth.go": `package interceptors

func Caller() string { return "" }
`,
		"service/svc.go": `package service

import "example.com/m/server/interceptors"

func New() string { return interceptors.Caller() }
`,
	}, DefaultConfig())

	cycles := idx.findCycles()
	if len(cycles) != 1 {
		t.Fatalf("got %d cycles, want 1: %+v", len(cycles), cycles)
	}
	if got, want := strings.Join(cycles[0].Members, ","), "server,service"; got != want {
		t.Errorf("members = %q, want %q", got, want)
	}
	if len(cycles[0].Edges) != 2 {
		t.Errorf("got %d edges, want 2 (one per package pair): %+v", len(cycles[0].Edges), cycles[0].Edges)
	}
}

func TestFindCyclesQuietOnAcyclicTree(t *testing.T) {
	idx := load(t, map[string]string{
		"server/s.go":  "package server\n\nimport \"example.com/m/service\"\n\nfunc New() { service.New() }\n",
		"service/x.go": "package service\n\nimport \"example.com/m/store\"\n\nfunc New() { store.Get() }\n",
		"store/x.go":   "package store\n\nfunc Get() {}\n",
	}, DefaultConfig())

	if got := idx.findCycles(); len(got) != 0 {
		t.Errorf("got %d cycles on an acyclic tree, want 0: %+v", len(got), got)
	}
}

func TestExcludedDirsAreNotScanned(t *testing.T) {
	cfg := DefaultConfig()
	idx := load(t, map[string]string{
		"gen/generated.go": "package gen\n\nfunc G() {}\n",
		"a/a.go":           "package a\n\nfunc A() {}\n",
	}, cfg)

	for _, f := range idx.Files {
		if strings.HasPrefix(f.Rel, "gen/") {
			t.Errorf("scanned excluded file %s", f.Rel)
		}
	}
	if len(idx.Files) != 1 {
		t.Errorf("scanned %d files, want 1", len(idx.Files))
	}
}

func TestTestFilesExcludedByDefault(t *testing.T) {
	files := map[string]string{
		"a/a.go":      "package a\n\nfunc A() {}\n",
		"a/a_test.go": "package a\n\nfunc TestA() {}\n",
	}
	if got := len(load(t, files, DefaultConfig()).Files); got != 1 {
		t.Errorf("default scan read %d files, want 1", got)
	}

	cfg := DefaultConfig()
	cfg.IncludeTests = true
	if got := len(load(t, files, cfg).Files); got != 2 {
		t.Errorf("-tests scan read %d files, want 2", got)
	}
}

// TestTestOnlyHelperPackageIsExcluded covers the half of the "no test findings"
// policy that IncludeTests cannot reach. IncludeTests filters by filename, so a
// package that exists only to be imported by tests is scanned in full despite
// carrying no production code — which is how a container path inside a
// testcontainers fixture came to be reported as a hardcoded path (#1631).
//
// Both such packages are covered here rather than one each: `internal/storedouble`
// meets the same rule as `internal/testhelpers` — nothing outside a test imports
// it — and reaches the boundary by the same mechanism, so a change that breaks
// one breaks the other.
//
// The negative half matters as much as the positive one: the entries are path
// prefixes, so `internal/testhelpersextra` and `internal/storedoubleextra` must not
// be swept up by them.
//
// The two package names are configured here rather than taken from the defaults.
// The scanner defaults to no test-only exclusions at all: a package that only
// tests import is a fact about one repository's layout, so naming them belongs
// to the consuming project. What is under test is the prefix mechanism, which is
// the scanner's.
func TestTestOnlyHelperPackageIsExcluded(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ExcludeDirs = append(cfg.ExcludeDirs, "internal/testhelpers", "internal/storedouble")

	idx := load(t, map[string]string{
		"internal/testhelpers/idp.go":        "//go:build integration\n\npackage testhelpers\n\nconst importDir = \"/opt/idp/data/import\"\n",
		"internal/testhelpersextra/extra.go": "package testhelpersextra\n\nconst importDir = \"/opt/idp/data/import\"\n",
		// No build tag, unlike testhelpers: storedouble is ordinary Go that only
		// tests import, so this entry is the only thing that catches it.
		"internal/storedouble/widget.go":     "package storedouble\n\nconst importDir = \"/opt/idp/data/import\"\n",
		"internal/storedoubleextra/extra.go": "package storedoubleextra\n\nconst importDir = \"/opt/idp/data/import\"\n",
		"internal/service/widget/widget.go":  "package widget\n\nfunc P() {}\n",
	}, cfg)

	var scanned []string
	for _, f := range idx.Files {
		scanned = append(scanned, f.Rel)
	}
	for _, rel := range scanned {
		for _, excluded := range []string{"internal/testhelpers/", "internal/storedouble/"} {
			if strings.HasPrefix(rel, excluded) {
				t.Errorf("scanned test-only package file %s; it should be excluded like a _test.go file", rel)
			}
		}
	}
	if len(scanned) != 3 {
		t.Errorf("scanned %v, want the three files outside the test-only packages", scanned)
	}

	// The exclusion must not silence the same literal in production code. Two
	// of the four copies are excluded, and the two `...extra` ones remain.
	if got := len(idx.findHardcodedPaths()); got != 2 {
		t.Errorf("got %d path findings, want 2 (only the excluded copies disappear)", got)
	}
}

// TestGeneratedFilesAreNotScanned covers the other half of the boundary. The
// exclude list draws it by directory, which misses a generator that writes
// beside the code it converts: `internal/convert/values.gen.go`
// carries the marker but sits in no `gen/` directory, and `make codegen-enums`
// overwrites whatever a finding there asked someone to change.
//
// Matching the marker rather than the filename is what makes the next such file
// free, so the cases below pin the convention rather than the name: the marker
// counts only ahead of the package clause, and only in the exact form the Go
// toolchain fixes.
func TestGeneratedFilesAreNotScanned(t *testing.T) {
	files := map[string]string{
		"conv/converters.gen.go": "// Code generated by tools/enumgen. DO NOT EDIT.\n\npackage conv\n\nfunc C() {}\n",
		// The marker under a build constraint, which is where a generator that
		// emits tagged output puts it.
		"tagged/tagged.gen.go": "//go:build !slim\n\n// Code generated by tools/other. DO NOT EDIT.\n\npackage tagged\n\nfunc T() {}\n",
		// Hand-written code that talks ABOUT the convention. The marker sits
		// after the package clause, so it says nothing about this file.
		"gene/emit.go": "package gene\n\n// Emit writes the header below into every file it produces.\n//\n// Code generated by tools/gene. DO NOT EDIT.\nfunc Emit() {}\n",
		// Near-misses on the marker's own text: no trailing period, and lower
		// case. Neither is the convention, so neither hides a file.
		"nearmiss/a.go": "// Code generated by hand. DO NOT EDIT\n\npackage nearmiss\n\nfunc A() {}\n",
		"nearmiss/b.go": "// code generated by hand. do not edit.\n\npackage nearmiss\n\nfunc B() {}\n",
	}

	var scanned []string
	for _, f := range load(t, files, DefaultConfig()).Files {
		scanned = append(scanned, f.Rel)
	}
	slices.Sort(scanned)
	want := []string{"gene/emit.go", "nearmiss/a.go", "nearmiss/b.go"}
	if !slices.Equal(scanned, want) {
		t.Errorf("scanned %v, want %v", scanned, want)
	}

	// The vendor-parity run measures generated files because the vendor did, so
	// the skip has to be something a config can turn back off.
	cfg := DefaultConfig()
	cfg.SkipGeneratedFiles = false
	if got := len(load(t, files, cfg).Files); got != len(files) {
		t.Errorf("skip_generated_files=false read %d files, want %d", got, len(files))
	}
}

// TestVendorParityConfigMeasuresGeneratedFiles is the calibration guard for the
// marker rule, matching the one below it for exclude_dirs. A parity run is
// compared against a vendor export that had no generated-code rule and measured
// those files, so a parity run that skipped them would report the boundary
// difference as a findings difference.
func TestVendorParityConfigMeasuresGeneratedFiles(t *testing.T) {
	cfg, err := LoadConfig(configFile(t, "vendor-parity.json"))
	if err != nil {
		t.Fatalf("LoadConfig(vendor-parity.json): %v", err)
	}
	if cfg.SkipGeneratedFiles {
		t.Error("vendor-parity.json skips generated files; the vendor scanned them, so the parity run must too")
	}
}

// TestVendorParityConfigPinsItsOwnExclusions guards the calibration table in
// README.md. `exclude_dirs` replaces the default list rather than extending it,
// so a parity run inherits every later addition to the default set unless it
// names its own — and each such addition would silently restate the vendor
// comparison as a disagreement it is not.
func TestVendorParityConfigPinsItsOwnExclusions(t *testing.T) {
	cfg, err := LoadConfig(configFile(t, "vendor-parity.json"))
	if err != nil {
		t.Fatalf("LoadConfig(vendor-parity.json): %v", err)
	}
	if len(cfg.ExcludeDirs) == 0 {
		t.Fatal("vendor-parity.json sets no exclude_dirs, so it inherits the defaults")
	}
	for _, e := range cfg.ExcludeDirs {
		if e == "internal/testhelpers" {
			t.Error("vendor-parity.json excludes internal/testhelpers; the vendor scanned it, so the parity run must too")
		}
	}
}

func TestRunReportsAgainstThresholds(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Parameters = Band{High: 2, VeryHigh: 3}

	idx := load(t, map[string]string{
		"a/a.go": `package a

func Small(a int)                    {}
func Wide(a, b, c int)               {}
func VeryWide(a, b, c, d, e, f int)  {}
`,
	}, cfg)

	var got []Finding
	for _, f := range Run(idx, "repo") {
		if f.Rule == RuleParameter {
			got = append(got, f)
		}
	}
	if len(got) != 2 {
		t.Fatalf("got %d parameter findings, want 2: %+v", len(got), got)
	}
	if got[0].Element != "VeryWide" || got[0].Severity != SeverityVeryHigh.String() {
		t.Errorf("worst finding = %+v, want VeryWide at very high risk", got[0])
	}
	if want := "parameters: 6 (very high risk, [> 3])"; got[0].Description != want {
		t.Errorf("description = %q, want %q", got[0].Description, want)
	}
	if want := "repo/a/a.go"; got[0].File != want {
		t.Errorf("-prefix not applied: file = %q, want %q", got[0].File, want)
	}
}
