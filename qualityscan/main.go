// Command qualityscan reports the best-practice checks an external static
// analysis vendor runs over a Go tree, computed locally from the source so they
// can be run on any commit, in CI, and without an upload.
//
//	qualityscan -root api -format text
//	qualityscan -root . -format csv -out scan.csv
//
// See README.md for what each rule measures and how its thresholds were
// derived from the vendor's own export.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "qualityscan: %v\n", err)
		os.Exit(2)
	}
}

func run() error {
	var units unitFiles
	flag.Var(&units, "units", "JSON file of externally measured units to fold into the SIG profile; repeatable")

	var (
		root       = flag.String("root", ".", "module directory to scan (must contain go.mod)")
		format     = flag.String("format", "text", "output format: text, csv, json, markdown, metrics, sigprofile, sigunits, sigmodules, or sigjson")
		out        = flag.String("out", "", "write to this file instead of stdout")
		configPath = flag.String("config", "", "JSON file overriding the default thresholds")
		accepted   = flag.String("accepted", "", "JSON file of reviewed findings to treat as accepted; see accepted.go")
		components = flag.String("components", "", "JSON file naming the top-level components; see components.go")
		deviations = flag.String("deviations", "", "JSON file of recorded deviations from the SIG caps; see deviations.go")
		baseline   = flag.String("baseline", "", "JSON snapshot of a previous run, to report movement against; see snapshot.go")
		taken      = flag.String("taken", "", "date to stamp on a -format sigjson snapshot (the scanner reads no clock)")
		prefix     = flag.String("prefix", "", "path prefix to prepend to reported filenames")
		tests      = flag.Bool("tests", false, "include _test.go files")
		failOn     = flag.Int("fail-on", 0, "exit 1 when the finding count exceeds this (0 disables)")
		issueDir   = flag.String("issue-dir", "", "write one markdown file per proposed issue into this directory")
		rev        = flag.String("rev", "", "commit or revision to stamp on the report")
		publish    = flag.Bool("publish", false, "create or update tracker issues via the gh CLI (without this, a publish run is a dry run)")
		plan       = flag.Bool("plan", false, "show what -publish would create or update, without writing anything")
		repo       = flag.String("repo", "", "owner/name to publish to; defaults to whatever gh infers")
		reopen     = flag.Bool("reopen", false, "also update issues someone has closed")
	)
	flag.Parse()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if *tests {
		cfg.IncludeTests = true
	}

	acceptances, err := LoadAcceptances(*accepted)
	if err != nil {
		return err
	}

	comps, err := LoadComponents(*components)
	if err != nil {
		return err
	}

	devs, err := LoadDeviations(*deviations)
	if err != nil {
		return err
	}

	idx, err := LoadIndex(*root, cfg)
	if err != nil {
		return err
	}
	findings := Run(idx, *prefix)
	// Reviewed sites come out before anything downstream counts them, so the
	// report, the CSV, the issue bodies and -fail-on all agree on one number.
	findings, reviewed := ApplyAcceptances(findings, acceptances, *prefix)
	issues := BuildIssues(findings, reviewed, *rev)

	w := os.Stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}

	// Publishing writes to the tracker, so it replaces the report rather than
	// following it: a run that files issues should say what it filed, not bury
	// that under 1600 lines of findings.
	if *publish || *plan {
		return Publish(w, issues, PublishOptions{
			Repo:   *repo,
			DryRun: !*publish,
			Reopen: *reopen,
		})
	}

	if *issueDir != "" {
		if err := WriteIssueFiles(*issueDir, issues); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %d issue files to %s\n", len(issues), *issueDir)
	}

	switch *format {
	case "text":
		err = WriteText(w, findings, reviewed)
	case "csv":
		err = WriteCSV(w, findings)
	case "json":
		err = WriteJSON(w, findings)
	case "metrics":
		err = WriteMetrics(w, idx, *prefix)
	case "markdown":
		err = WriteMarkdown(w, issues, findings, reviewed, *rev)
	case "sigprofile", "sigunits", "sigmodules", "sigjson":
		err = writeSIG(w, idx, *format, *prefix, *rev, units, comps, devs, *baseline, *taken)
	default:
		return fmt.Errorf("unknown -format %q (want text, csv, json, markdown, metrics, sigprofile, sigunits, sigmodules, or sigjson)", *format)
	}
	if err != nil {
		return err
	}

	if *failOn > 0 && len(findings) > *failOn {
		return fmt.Errorf("%d findings exceeds the -fail-on budget of %d", len(findings), *failOn)
	}
	// Reported after the output is written, for the same reason the SIG
	// profile's unmeasured-language check is: the findings are still worth
	// reading, and a reader who only sees an exit code cannot tell what rotted.
	return reviewed.StaleError()
}

// unitFiles collects a repeatable -units flag, one measurement file per language.
type unitFiles []string

func (u *unitFiles) String() string { return strings.Join(*u, ",") }

func (u *unitFiles) Set(v string) error {
	*u = append(*u, v)
	return nil
}

// writeSIG renders the SIG risk-profile distribution over the Go surface plus any
// language handed in through -units.
//
// The report is written before the unmeasured-language check fails the run: a
// profile that covered one surface and not the other is still worth reading, and
// a reader who only sees an exit code cannot tell which surface went missing.
func writeSIG(w io.Writer, idx *Index, format, prefix, rev string, paths unitFiles, comps Components,
	devs Deviations, baseline, taken string,
) error {
	external, err := LoadUnitSets(paths)
	if err != nil {
		return err
	}
	go_, err := idx.goUnits(prefix, comps)
	if err != nil {
		return err
	}
	sets := append([]UnitSet{go_}, external...)

	if format == "sigunits" {
		return WriteSIGUnits(w, sets)
	}
	if format == "sigmodules" {
		return WriteSIGModules(w, sets)
	}

	report := BuildSIGProfile(sets, rev, idx.Cfg.ModuleCouplingCounts)
	if format == "sigjson" {
		return WriteSnapshot(w, snapshotOf(report, taken, rev))
	}

	base, err := LoadSnapshot(baseline)
	if err != nil {
		return err
	}

	if err := WriteSIGProfile(w, report, base, devs); err != nil {
		return err
	}
	if missing := report.Unmeasured(); len(missing) > 0 {
		return fmt.Errorf("no units measured for %s: a language with no units has no denominator, "+
			"and reporting it as 0%% would read as a pass against every cap",
			strings.Join(missing, ", "))
	}
	return nil
}
