// Command deltagate scores what a change did to maintainability, rather than
// where the code stands.
//
// The scanner and the SIG profile both answer the standing question: how much
// of this tree sits over a threshold today. That is the right question for a
// scorecard and the wrong one for a pull request. A branch adding four lines to
// a file that already carried nine findings inherits all nine, and a gate
// reading the standing number tells the author to pay down somebody else's
// debt before their own change can land. cmd/ratchetpatch exists because of
// exactly that failure and narrows the blast radius to files whose code
// actually moved; it still judges those files in full.
//
// This command measures the change itself. It reads the unit inventory at two
// revisions, bins every unit as low-risk or not on the SIG low-category
// boundary, and reports the share of the moved lines that went the right way,
// per language and per property. See internal/dmm for the model, where it comes
// from, and the two shapes that distort it.
//
//	qualityscan -root . -format sigunits -out before.csv   # at the merge base
//	qualityscan -root . -format sigunits -out after.csv    # at HEAD
//	deltagate -before before.csv -after after.csv -floor 0.7 -gate
//
// Both inputs are `-format sigunits` output, which is the CSV cmd/crosscheck
// already reads, so a language handed to the scanner through -units is scored
// here too with no further plumbing.
//
// Exit status: 0 when the report was produced and (under -gate) every measured
// property met the floor, 1 when one did not, 2 on a usage or input error.
package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/ClappFormOrg/static-analysis/qualityscan/internal/dmm"
)

func main() {
	code, err := run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "deltagate: %v\n", err)
		os.Exit(2)
	}
	os.Exit(code)
}

func run() (int, error) {
	var (
		before = flag.String("before", "", "sigunits CSV measured at the base revision")
		after  = flag.String("after", "", "sigunits CSV measured at the revision under review")
		floor  = flag.Float64("floor", 0, "lowest acceptable score, 0 to 1")
		gate   = flag.Bool("gate", false, "exit 1 when a measured property is below the floor, rather than only reporting it")
		format = flag.String("format", "text", "output format: text, markdown, or json")
		out    = flag.String("out", "", "write to this file instead of stdout")
		rev    = flag.String("rev", "", "revision range to stamp on the report")
	)
	flag.Parse()

	if *before == "" || *after == "" {
		return 0, fmt.Errorf("both -before and -after are required")
	}
	if *floor < 0 || *floor > 1 {
		return 0, fmt.Errorf("-floor %v is outside the score's range of 0 to 1", *floor)
	}

	report, err := compare(*before, *after)
	if err != nil {
		return 0, err
	}

	w := os.Stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return 0, err
		}
		defer f.Close()
		w = f
	}

	if err := writeReport(w, *format, report, *rev, *floor, *gate); err != nil {
		return 0, err
	}

	if *gate && len(breaches(report, *floor)) > 0 {
		return 1, nil
	}
	return 0, nil
}

// compare loads both inventories and runs the model over them.
func compare(beforePath, afterPath string) (dmm.Report, error) {
	before, err := readSIGUnits(beforePath)
	if err != nil {
		return dmm.Report{}, err
	}
	after, err := readSIGUnits(afterPath)
	if err != nil {
		return dmm.Report{}, err
	}
	// Two empty inventories produce a report of nothing that reads as a clean
	// pass, which is the failure the profile's unmeasured-language check exists
	// to stop one level up. Here it almost always means the sigunits runs were
	// pointed at the wrong paths.
	if len(before) == 0 && len(after) == 0 {
		return dmm.Report{}, fmt.Errorf("no units in either %s or %s", beforePath, afterPath)
	}
	return dmm.Compare(before, after), nil
}

// writeReport renders the comparison in the requested format.
func writeReport(w io.Writer, format string, r dmm.Report, rev string, floor float64, gate bool) error {
	switch format {
	case "text":
		return writeText(w, r, rev, floor, gate)
	case "markdown":
		return writeMarkdown(w, r, rev, floor, gate)
	case "json":
		return writeJSON(w, r, floor)
	}
	return fmt.Errorf("unknown -format %q (want text, markdown, or json)", format)
}

// breach is one property that came in under the floor.
type breach struct {
	Language string
	Property string
	Score    float64
}

// breaches lists every measured property below the floor. An unmeasured
// property is never a breach: a change that touched nothing this property
// measures has not regressed it, and failing on it would make the gate fire on
// the emptiest changes.
func breaches(r dmm.Report, floor float64) []breach {
	var out []breach
	for _, lang := range r.Languages {
		for _, p := range lang.Properties {
			if p.Measured && p.Score < floor {
				out = append(out, breach{Language: lang.Language, Property: p.Name, Score: p.Score})
			}
		}
	}
	return out
}

// readSIGUnits loads a `-format sigunits` CSV.
//
// The header is required rather than assumed, and the columns are located by
// name. crosscheck reads the same file by fixed position and this does not, for
// one reason: that tool compares two measurements of the same run, where a
// column shift shows up immediately as every function diverging, and this one
// compares two runs that may have been produced by different versions of the
// scanner, where a silently shifted column would move the score instead.
func readSIGUnits(path string) ([]dmm.Unit, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s is empty", path)
	}

	col := map[string]int{}
	for i, name := range rows[0] {
		col[strings.TrimSpace(name)] = i
	}
	for _, want := range []string{"language", "element", "file", "line", "loc", "mccabe", "params"} {
		if _, ok := col[want]; !ok {
			return nil, fmt.Errorf("%s: no %q column; -before and -after want `qualityscan -format sigunits` output", path, want)
		}
	}

	var units []dmm.Unit
	for i, row := range rows[1:] {
		if len(row) < len(col) {
			return nil, fmt.Errorf("%s line %d: %d columns, want %d", path, i+2, len(row), len(col))
		}
		units = append(units, dmm.Unit{
			Language: row[col["language"]],
			File:     normalisePath(row[col["file"]]),
			Element:  row[col["element"]],
			Line:     atoi(row[col["line"]]),
			LOC:      atoi(row[col["loc"]]),
			McCabe:   atoi(row[col["mccabe"]]),
			Params:   atoi(row[col["params"]]),
		})
	}
	return units, nil
}

// normalisePath makes the two revisions' paths comparable. A scan run on
// Windows and one run in CI emit different separators for the same file, and a
// path that differs only in its separator would read as a rename.
func normalisePath(p string) string {
	return strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(p), "\\", "/"), "./")
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// scoreText renders a score, or the reason there is not one. A property nobody
// measured prints a dash and never a number, so no reader has to work out
// whether a 0.00 means harm or silence.
func scoreText(p dmm.PropertyResult) string {
	if !p.Measured {
		return "not touched"
	}
	return fmt.Sprintf("%.2f", p.Score)
}

func writeText(w io.Writer, r dmm.Report, rev string, floor float64, gate bool) error {
	b := &strings.Builder{}
	b.WriteString("Delta maintainability")
	if rev != "" {
		fmt.Fprintf(b, " (%s)", rev)
	}
	b.WriteString("\n\nShare of the lines this change moved that went the right way, per language\n")
	b.WriteString("and per property. 1.00 is all improvement, 0.00 all regression.\n")

	for _, lang := range r.Languages {
		fmt.Fprintf(b, "\n%s: %d files touched", lang.Language, lang.FilesTouched)
		if lang.FilesAdded > 0 || lang.FilesRemoved > 0 {
			fmt.Fprintf(b, " (%d added, %d removed)", lang.FilesAdded, lang.FilesRemoved)
		}
		b.WriteString("\n")
		fmt.Fprintf(b, "  %-18s %8s %8s %8s  %s\n", "property", "good", "bad", "churn", "score")
		for _, p := range lang.Properties {
			fmt.Fprintf(b, "  %-18s %8d %8d %8d  %s\n", p.Name, p.Good, p.Bad, p.Churn(), scoreText(p))
		}
	}

	writeVerdict(b, r, floor, gate, "")
	_, err := io.WriteString(w, b.String())
	return err
}

func writeMarkdown(w io.Writer, r dmm.Report, rev string, floor float64, gate bool) error {
	b := &strings.Builder{}
	b.WriteString("## Delta maintainability\n\n")
	if rev != "" {
		fmt.Fprintf(b, "Revision range: `%s`\n\n", rev)
	}
	b.WriteString("Share of the lines this change moved that went the right way. " +
		"1.00 is all improvement, 0.00 all regression.\n")

	for _, lang := range r.Languages {
		fmt.Fprintf(b, "\n### %s\n\n%d files touched", lang.Language, lang.FilesTouched)
		if lang.FilesAdded > 0 || lang.FilesRemoved > 0 {
			fmt.Fprintf(b, " (%d added, %d removed)", lang.FilesAdded, lang.FilesRemoved)
		}
		b.WriteString("\n\n| Property | Good | Bad | Churn | Score |\n| --- | ---: | ---: | ---: | ---: |\n")
		for _, p := range lang.Properties {
			fmt.Fprintf(b, "| %s | %d | %d | %d | %s |\n", p.Name, p.Good, p.Bad, p.Churn(), scoreText(p))
		}
	}

	writeVerdict(b, r, floor, gate, "**")
	_, err := io.WriteString(w, b.String())
	return err
}

// writeVerdict appends the floor comparison. emph wraps the headline so the
// markdown renderer gets bold and the terminal does not get asterisks.
func writeVerdict(b *strings.Builder, r dmm.Report, floor float64, gate bool, emph string) {
	if floor <= 0 {
		return
	}
	failed := breaches(r, floor)
	if len(failed) == 0 {
		fmt.Fprintf(b, "\n%sEvery measured property is at or above the floor of %.2f.%s\n", emph, floor, emph)
		return
	}
	fmt.Fprintf(b, "\n%sBelow the floor of %.2f:%s\n", emph, floor, emph)
	for _, f := range failed {
		fmt.Fprintf(b, "  %s %s: %.2f\n", f.Language, f.Property, f.Score)
	}
	if !gate {
		b.WriteString("\nAdvisory: -gate was not set, so this run exits 0.\n")
	}
}

// jsonProperty is the wire shape. dmm.PropertyResult embeds a func field, which
// cannot be marshalled, so the report is projected rather than encoded
// directly.
type jsonProperty struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Good     int      `json:"good"`
	Bad      int      `json:"bad"`
	Churn    int      `json:"churn"`
	Score    *float64 `json:"score"`
	Measured bool     `json:"measured"`
}

type jsonLanguage struct {
	Language     string         `json:"language"`
	FilesTouched int            `json:"files_touched"`
	FilesAdded   int            `json:"files_added"`
	FilesRemoved int            `json:"files_removed"`
	Properties   []jsonProperty `json:"properties"`
}

type jsonReport struct {
	Floor     float64        `json:"floor"`
	Breaches  []breach       `json:"breaches"`
	Languages []jsonLanguage `json:"languages"`
}

func writeJSON(w io.Writer, r dmm.Report, floor float64) error {
	out := jsonReport{Floor: floor, Breaches: breaches(r, floor), Languages: []jsonLanguage{}}
	for _, lang := range r.Languages {
		jl := jsonLanguage{
			Language:     lang.Language,
			FilesTouched: lang.FilesTouched,
			FilesAdded:   lang.FilesAdded,
			FilesRemoved: lang.FilesRemoved,
			Properties:   []jsonProperty{},
		}
		for _, p := range lang.Properties {
			jp := jsonProperty{
				ID: p.ID, Name: p.Name, Good: p.Good, Bad: p.Bad,
				Churn: p.Churn(), Measured: p.Measured,
			}
			// null rather than 0 for a property nobody measured: a consumer
			// reading this into a dashboard must not average a silence in as a
			// zero.
			if p.Measured {
				score := p.Score
				jp.Score = &score
			}
			jl.Properties = append(jl.Properties, jp)
		}
		out.Languages = append(out.Languages, jl)
	}
	if out.Breaches == nil {
		out.Breaches = []breach{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
