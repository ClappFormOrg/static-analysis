// Command crosscheck compares the scanner's unit measurements against
// lizard's, as an independent reading of the same source.
//
// Nothing else verifies the unit inventory. The profile's tests pin every
// decision it makes, but they are written against the same reading of the model
// as the code, so a wrong reading passes both. lizard is a different
// implementation by different people over Go, TypeScript and Vue, which makes it
// the one available oracle for unit size, McCabe and parameter counts.
//
//	make sig-crosscheck
//
// It is advisory and never a gate. Where the two differ, ours is usually right:
// lizard's Go front end is not AST-based, and two of the differences below are
// deliberate design decisions of this tool rather than defects. The value is in
// noticing a NEW divergence, which is why the summary is a handful of numbers
// rather than a list of every function.
//
// Two systematic differences to expect, both explainable:
//
//   - Function literals. The model's unit is the smallest NAMED piece of
//     executable code, so a Go closure's lines and branches bill to the
//     declaration enclosing it. lizard reports the closure as its own entry, so
//     it finds units we do not, and our enclosing function measures larger than
//     its.
//   - Scan boundary. This tool excludes tests, generated output and
//     internal/testhelpers; lizard is pointed at the same boundary by the
//     Makefile, but a mismatch there shows up here as a pile of one-sided
//     functions rather than as a metric disagreement.
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

// measurement is one function as one of the two tools measured it.
type measurement struct {
	File   string
	Line   int
	Name   string
	LOC    int
	McCabe int
	Params int
}

func (m measurement) key() string { return m.File + ":" + strconv.Itoa(m.Line) }

func main() {
	ours := flag.String("ours", "", "sigunits CSV written by -format sigunits")
	theirs := flag.String("theirs", "", "lizard CSV written by `lizard --csv`")
	language := flag.String("language", "Go", "language column to compare from the sigunits CSV")
	worst := flag.Int("worst", 5, "how many of the widest divergences to print per metric")
	flag.Parse()

	if err := run(*ours, *theirs, *language, *worst); err != nil {
		fmt.Fprintf(os.Stderr, "crosscheck: %v\n", err)
		os.Exit(2)
	}
}

func run(oursPath, theirsPath, language string, worst int) error {
	if oursPath == "" || theirsPath == "" {
		return fmt.Errorf("both -ours and -theirs are required")
	}
	ours, err := readOurs(oursPath, language)
	if err != nil {
		return err
	}
	them, anonymous, err := readLizard(theirsPath)
	if err != nil {
		return err
	}
	// A run that matched nothing looks exactly like a run that agreed on
	// nothing, so it fails rather than printing a clean-looking zero.
	if len(ours) == 0 {
		return fmt.Errorf("no %s units in %s", language, oursPath)
	}
	if len(them) == 0 {
		return fmt.Errorf("no functions in %s", theirsPath)
	}

	report(os.Stdout, ours, them, anonymous, worst)
	return nil
}

// readOurs loads the sigunits CSV, keeping one language.
func readOurs(path, language string) (map[string]measurement, error) {
	rows, err := readCSV(path)
	if err != nil {
		return nil, err
	}
	out := map[string]measurement{}
	for i, r := range rows {
		// language,element,file,line,loc,mccabe,params,...
		if i == 0 || len(r) < 7 || r[0] != language {
			continue
		}
		m := measurement{
			File: normalisePath(r[2]), Name: r[1],
			Line: atoi(r[3]), LOC: atoi(r[4]), McCabe: atoi(r[5]), Params: atoi(r[6]),
		}
		out[m.key()] = m
	}
	return out, nil
}

// readLizard loads lizard's CSV, which carries no header, and separates the
// entries with no name: those are function literals, which this tool bills to
// the declaration enclosing them rather than counting as units.
func readLizard(path string) (map[string]measurement, int, error) {
	rows, err := readCSV(path)
	if err != nil {
		return nil, 0, err
	}
	out := map[string]measurement{}
	anonymous := 0
	for _, r := range rows {
		// nloc,ccn,token,param,length,location,file,name,long_name,start,end
		if len(r) < 11 {
			continue
		}
		if strings.TrimSpace(r[7]) == "" {
			anonymous++
			continue
		}
		m := measurement{
			File: normalisePath(r[6]), Name: r[7], Line: atoi(r[9]),
			LOC: atoi(r[0]), McCabe: atoi(r[1]), Params: atoi(r[3]),
		}
		out[m.key()] = m
	}
	return out, anonymous, nil
}

// normalisePath makes the two tools' paths comparable. lizard emits the
// separator of the platform it ran on, which on Windows means one path can carry
// both.
func normalisePath(p string) string {
	return strings.TrimPrefix(strings.ReplaceAll(p, "\\", "/"), "./")
}

func readCSV(path string) ([][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	return r.ReadAll()
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// divergence is one function the two tools measured differently.
type divergence struct {
	m     measurement
	ours  int
	their int
	ratio float64
}

// report prints the comparison: what each tool saw, and how far apart they are
// where they saw the same function.
func report(w io.Writer, ours, them map[string]measurement, anonymous, worst int) {
	var shared []string
	for k := range ours {
		if _, ok := them[k]; ok {
			shared = append(shared, k)
		}
	}
	sort.Strings(shared)

	fmt.Fprintf(w, "Units: %d ours, %d theirs, %d matched on file and line.\n", len(ours), len(them), len(shared))
	fmt.Fprintf(w, "lizard also reported %d unnamed callables, which this tool bills to the "+
		"declaration enclosing them rather than counting as units.\n\n", anonymous)

	metrics := []struct {
		name string
		get  func(measurement) int
	}{
		{"lines of code", func(m measurement) int { return m.LOC }},
		{"McCabe", func(m measurement) int { return m.McCabe }},
		{"parameters", func(m measurement) int { return m.Params }},
	}

	for _, metric := range metrics {
		var (
			ratios []float64
			diffs  []divergence
			equal  int
		)
		for _, k := range shared {
			a, b := metric.get(ours[k]), metric.get(them[k])
			if a == b {
				equal++
			}
			if b == 0 {
				// A ratio against zero says nothing, and parameters are legitimately
				// zero, so those are counted as agreeing or not and left out of the
				// distribution.
				continue
			}
			ratio := float64(a) / float64(b)
			ratios = append(ratios, ratio)
			if a != b {
				diffs = append(diffs, divergence{m: ours[k], ours: a, their: b, ratio: ratio})
			}
		}

		fmt.Fprintf(w, "## %s\n\n", metric.name)
		fmt.Fprintf(w, "  identical on %d of %d matched units (%.1f%%)\n", equal, len(shared),
			float64(equal)/float64(max(len(shared), 1))*100)
		if len(ratios) > 0 {
			fmt.Fprintf(w, "  median ratio ours/theirs %.3f, outside a 0.75-1.33 band: %d\n",
				median(ratios), outsideBand(ratios))
		}

		sort.Slice(diffs, func(i, j int) bool {
			return math.Abs(math.Log(diffs[i].ratio)) > math.Abs(math.Log(diffs[j].ratio))
		})
		for i, d := range diffs {
			if i >= worst {
				fmt.Fprintf(w, "  ... and %d more\n", len(diffs)-worst)
				break
			}
			fmt.Fprintf(w, "  %s:%d %s: ours %d, theirs %d\n",
				d.m.File, d.m.Line, d.m.Name, d.ours, d.their)
		}
		fmt.Fprintln(w)
	}
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}

// outsideBand counts the ratios outside the 0.75-1.33 band the README uses for
// the threshold calibration, so the two comparisons read the same way.
func outsideBand(v []float64) int {
	n := 0
	for _, r := range v {
		if r < 0.75 || r > 1.33 {
			n++
		}
	}
	return n
}
