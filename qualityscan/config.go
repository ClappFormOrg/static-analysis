package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// Severity is the two risk bands a threshold rule can report. Anything below
// High is not emitted at all: a measurement inside every band is not a finding,
// and a report padded with them buries the ones worth acting on.
type Severity int

const (
	// SeverityNone is the zero value: a measurement inside every band, which
	// is emitted nowhere.
	SeverityNone Severity = iota
	// SeverityHigh is a value over a band's High threshold and at or under its
	// VeryHigh one.
	SeverityHigh
	// SeverityVeryHigh is a value over a band's VeryHigh threshold.
	SeverityVeryHigh
	// SeverityFinding is for the rules that have no numeric band (cycles,
	// hardcoded literals), where the finding is the whole verdict.
	SeverityFinding
)

// String renders the severity as the report and the CSV spell it.
func (s Severity) String() string {
	switch s {
	case SeverityHigh:
		return "high risk"
	case SeverityVeryHigh:
		return "very high risk"
	case SeverityFinding:
		return "finding"
	default:
		return "none"
	}
}

// Band is a two-threshold risk scale. A value strictly greater than High is
// high risk; strictly greater than VeryHigh is very high risk. The bounds are
// exclusive, which is the convention the published ranges use: a size of 806
// tokens reads as "(very high risk, [> 500])" and the high band as "[201 -
// 500]", which is High=200, VeryHigh=500.
type Band struct {
	High     int `json:"high"`
	VeryHigh int `json:"very_high"`
}

// Rate places one measurement in the band. Both bounds are exclusive, matching
// the printed ranges the Band doc comment quotes.
func (b Band) Rate(v int) Severity {
	switch {
	case v > b.VeryHigh:
		return SeverityVeryHigh
	case v > b.High:
		return SeverityHigh
	default:
		return SeverityNone
	}
}

// Describe renders the band the value fell into, in the conventional notation
// for a risk range, so a row reads the same way in every format.
func (b Band) Describe(v int) string {
	switch b.Rate(v) {
	case SeverityVeryHigh:
		return fmt.Sprintf("(very high risk, [> %d])", b.VeryHigh)
	case SeverityHigh:
		return fmt.Sprintf("(high risk, [%d - %d])", b.High+1, b.VeryHigh)
	default:
		return ""
	}
}

// Config holds every threshold and scope decision the scan makes. The defaults
// are the conventional published bands for these metrics, except for Complexity
// and DependencySpan, which are fitted. See README.md for how each was derived.
type Config struct {
	// FunctionSize counts Go lexical tokens in a function declaration,
	// signature and body together.
	FunctionSize Band `json:"function_size"`
	// Parameters counts declared parameters. The receiver is excluded and a
	// variadic parameter counts as one.
	Parameters Band `json:"parameters"`
	// Complexity is cognitive complexity (nesting-weighted). Its bands are not
	// the conventional published ones, which are set for a different metric
	// ("function nesting complexity") whose numbers do not transfer. See
	// README.md, "Complexity".
	Complexity Band `json:"complexity"`
	// DependencyVolume counts, per file, every identifier reference that
	// resolves to a declaration outside that file.
	DependencyVolume Band `json:"dependency_volume"`
	// DependencySpan counts, per file, the distinct targets those references
	// reach.
	DependencySpan Band `json:"dependency_span"`
	// SpanGranularity decides what counts as one target: "package" (the
	// default) folds every package into a single target, so span reads as
	// "how many neighbourhoods does this file touch"; "file" counts each
	// in-module file separately and runs roughly twice as high.
	SpanGranularity string `json:"span_granularity"`
	// ModuleCouplingCounts decides what one incoming dependency is for the
	// SIG module coupling property: "modules" (the default) counts the
	// distinct modules referencing a module, "references" counts every
	// individual reference and runs far higher.
	//
	// The model says "the number of incoming dependencies, such as
	// invocations, per module", which admits both readings. The default is
	// modules because the caps read as a coordination cost -- an edit here
	// reaches N other files -- and a caller referencing a module forty times
	// is still one caller. Both are measured either way; this picks which one
	// the caps are applied to.
	ModuleCouplingCounts string `json:"module_coupling_counts"`

	// SQLMethods are method names that mean "this statement talks to the
	// database". The default is the gorm and database/sql surface.
	SQLMethods []string `json:"sql_methods"`
	// SQLMethodSuffixes catch repository methods by naming convention rather
	// than by name, for a codebase that has one.
	//
	// Empty by default, because a suffix is a house style rather than a fact
	// about Go. A repository that marks every transaction-scoped repository
	// method with one gets an exact reading from it, and the same string means
	// nothing at all in a repository that does not use it. Defaulting one on
	// would silently count unrelated methods as queries elsewhere, which is a
	// wrong measurement rather than a noisy one, so a repository that has such
	// a suffix names it in its own config.
	SQLMethodSuffixes []string `json:"sql_method_suffixes"`
	// SQLReceivers are variable names that hold a database handle, so a call
	// on one is a query whatever the method is called.
	SQLReceivers []string `json:"sql_receivers"`
	// SQLChainMethods are gorm's chainable builders. They assemble a statement
	// and return it; none of them executes, so none is a round trip. They are
	// listed rather than merely omitted from SQLMethods because SQLReceivers
	// matches on the receiver alone and would otherwise count `db.Where(...)`
	// as a query.
	SQLChainMethods []string `json:"sql_chain_methods"`
	// DebugStatementExcludeDirs skip the debug-print rule. `cmd` is excluded
	// by default: writing to stdout is what a CLI is for.
	DebugStatementExcludeDirs []string `json:"debug_statement_exclude_dirs"`
	// DisabledRules are rule ids to drop from the report entirely.
	DisabledRules []string `json:"disabled_rules"`
	// URLAllowlistHosts are hosts whose URLs are identifiers or references
	// rather than addresses: an XML namespace, a link to a specification. A
	// literal pointing at one is not a hardcoded endpoint, so the URL rule
	// skips it. Matching is on label boundaries, so `w3.org` also covers
	// `www.w3.org`. Reserved names (`.test`, `example.com`) are always
	// skipped and need no entry here; see reservedHostSuffixes.
	URLAllowlistHosts []string `json:"url_allowlist_hosts"`

	// ExcludeDirs are path prefixes (relative to the scan root, slash
	// separated) that are skipped entirely.
	ExcludeDirs []string `json:"exclude_dirs"`
	// IncludeTests scans _test.go files too. The default report contains no
	// test findings, so this is off by default.
	IncludeTests bool `json:"include_tests"`
	// SkipGeneratedFiles drops any file carrying the Go toolchain's
	// `// Code generated ... DO NOT EDIT.` marker ahead of its package clause,
	// wherever it sits. ExcludeDirs already covers the generated trees, but a
	// generator that writes beside the code it converts -- enumgen writes
	// `internal/convert/values.gen.go` -- lands inside the scan,
	// and a finding on a file `make codegen-enums` overwrites is a finding
	// nobody can act on. The marker is read by internal/gomarker, shared with
	// the coverage gate so the two cannot disagree about which files are
	// machine-written. On by default; a run comparing this scan against a tool
	// that measures generated code turns it off.
	SkipGeneratedFiles bool `json:"skip_generated_files"`
}

// DefaultConfig is the scan's thresholds: the conventional published numbers
// for each metric, and a linter's own budget where the two measure the same
// thing. Every field carries the reasoning for its own value.
func DefaultConfig() Config {
	return Config{
		FunctionSize: Band{High: 200, VeryHigh: 500},
		Parameters:   Band{High: 4, VeryHigh: 6},
		// Anchored to `gocognit: min-complexity: 20` rather than to the
		// conventional 30/50, which are set for a different metric. Both bounds
		// are exclusive and golangci's is too, so High=20 flags exactly what
		// gocognit flags. VeryHigh is twice that line.
		Complexity:       Band{High: 20, VeryHigh: 40},
		DependencyVolume: Band{High: 110, VeryHigh: 200},
		// Span at package granularity runs about a quarter below the scale the
		// published 30/40 bands are set for, which would leave this rule
		// reporting nothing at all. Rescaled by that quarter. See README.md,
		// "Two bands were fitted rather than taken".
		DependencySpan:       Band{High: 23, VeryHigh: 33},
		SpanGranularity:      "package",
		ModuleCouplingCounts: defaultModuleCouplingCounts,
		SQLMethods: []string{
			// gorm's finishers and raw escape hatches. Chained builders
			// (Where, Joins, Order) are deliberately absent: they do not
			// execute, and including them would report the same query twice.
			"Find", "First", "Last", "Take", "Scan", "Pluck", "Count",
			"Create", "CreateInBatches", "Save", "Update", "Updates",
			"Delete", "FirstOrCreate", "FirstOrInit",
			// database/sql.
			"Exec", "ExecContext", "Query", "QueryContext",
			"QueryRow", "QueryRowContext", "Raw",
		},
		SQLMethodSuffixes: nil,
		SQLReceivers:      []string{"db", "tx", "gdb", "conn", "pool"},
		// gorm's chain methods, plus the session/config ones that also return a
		// *gorm.DB without touching the database.
		SQLChainMethods: []string{
			"Where", "Or", "Not", "Table", "Select", "Omit", "Distinct",
			"Joins", "Preload", "Order", "Limit", "Offset", "Group", "Having",
			"Model", "Clauses", "Unscoped", "WithContext", "Session", "Debug",
			"Attrs", "Assign", "InnerJoins", "MapColumn",
		},
		DebugStatementExcludeDirs: []string{"cmd"},
		URLAllowlistHosts: []string{
			// XML and XHTML namespace URIs. A namespace is an identifier
			// spelled as a URL; nothing ever dereferences it.
			"w3.org",
			// Specification references rendered into documentation pages.
			"rfc-editor.org", "datatracker.ietf.org",
		},
		ExcludeDirs:        defaultExcludeDirs(),
		IncludeTests:       false,
		SkipGeneratedFiles: true,
	}
}

// defaultExcludeDirs is the scanned-code boundary: generated output, vendored
// code and test data. Generated code is also caught by marker wherever it sits
// outside these trees; see SkipGeneratedFiles.
//
// These entries are the ones that hold for any Go repository. What is NOT here
// is the other half of the boundary: a package that exists only to be imported
// by tests. IncludeTests is off by default, but it decides what to scan by
// filename, so it only catches `_test.go`. A package of test doubles is test
// code under any reading except that one, and scanning it applies the policy to
// some test code and not the rest -- so it belongs on this list, by name, in the
// config of the repository that has it, because such a package's name means
// nothing in another project.
//
// Judge a candidate for such an entry by that rule rather than by its name: what
// qualifies a package is that nothing outside a test imports it, not that its
// name contains "test" or "fake". Two consequences are worth knowing before
// adding one, because they are not what the shape of such a package suggests.
// Excluding it moves EVERY SIG denominator, not just the unit ones, because they
// are all counted over the same file index. And the movement is not all one way:
// a package of test doubles pointing at the real implementation carries incoming
// edges no production caller has, so coupling improves when it leaves, while
// duplication can get WORSE, because short doubles whose bodies diverge inside
// the six-line window dilute the redundancy share rather than inflating it. Read
// both as a correction to the measurement rather than as a change in the code.
//
// `exclude_dirs` REPLACES this list when a config sets it, rather than extending
// it, so a repository naming its own test-only packages restates the entries
// below alongside them.
//
// It is a function rather than a package-level slice so a caller mutating the
// returned config cannot change what the next DefaultConfig hands out.
func defaultExcludeDirs() []string {
	return []string{
		"gen",
		"migrations",
		"seed",
		".disabled-gen",
		"vendor",
		"testdata",
	}
}

// LoadConfig overlays a JSON file onto the defaults. Absent fields keep their
// default, so a config that only wants to move one threshold only names that
// one -- except for exclude_dirs, which replaces the default list when present.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	if path == "" {
		return cfg, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// validate rejects a config whose enumerated fields carry a value nothing reads.
//
// A misspelled enum otherwise falls back to its default in silence, and a report
// that quietly measured something other than what its config asked for is worse
// than one that refused to run. Only the fields this covers are checked;
// span_granularity predates the check and is left alone rather than tightened as
// a side effect of adding module coupling.
func (c Config) validate() error {
	switch c.ModuleCouplingCounts {
	case moduleCouplingCountsModules, moduleCouplingCountsReferences:
	default:
		return fmt.Errorf("module_coupling_counts %q: want %q or %q",
			c.ModuleCouplingCounts, moduleCouplingCountsModules, moduleCouplingCountsReferences)
	}
	return nil
}
