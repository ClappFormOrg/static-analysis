package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

// The snapshot: what the profile measured, in a form the next run can diff
// against.
//
// A scorecard answers two questions.
// Where does the codebase stand, which the profile computes, and what has moved
// since last time, which until now a person answered by reading two reports and
// remembering. The second question is the one that decays: it needs the previous
// numbers, and a written-down answer cannot be checked without them.
//
// So the numbers are committed alongside the report. `make sig-baseline` writes
// this file deliberately, `make sig-profile` reads it and renders the movement,
// and neither needs anyone to recall anything. A baseline that moves on every
// generation would break the drift check -- the report has to stay a function of
// its inputs -- which is why writing one is its own command rather than a side
// effect of reporting.

// Snapshot is one run's measurements, keyed so a later run can line them up.
type Snapshot struct {
	Model      string `json:"model"`
	Version    string `json:"version"`
	StarTarget int    `json:"star_target"`
	// Taken is the date the baseline was recorded, supplied by the caller. The
	// scanner reads no clock: its output is a function of its inputs, and a
	// timestamp it invented would make two runs over one tree differ.
	Taken string `json:"taken,omitempty"`
	// Rev is the commit the baseline describes, when the caller names one.
	Rev       string             `json:"rev,omitempty"`
	Languages []LanguageSnapshot `json:"languages"`
}

// LanguageSnapshot is one language's measured figures.
type LanguageSnapshot struct {
	Language string `json:"language"`
	Files    int    `json:"files_scanned"`
	UnitLOC  int    `json:"unit_loc"`
	// Measures is every figure that carries a cap or a floor, keyed by a stable
	// name: "unit_size > 15", "duplication", "component_independence". A key
	// present in one snapshot and not the other is reported as such rather than
	// compared against zero.
	Measures map[string]float64 `json:"measures"`
	// Entanglement carries the two figures the model would combine. They have
	// no cap here (see entanglement.go), so they are recorded to be watched
	// rather than to be judged.
	Entanglement map[string]float64 `json:"entanglement,omitempty"`
}

// snapshotOf reduces a report to the numbers worth diffing.
func snapshotOf(r SIGReport, taken, rev string) Snapshot {
	s := Snapshot{
		Model: SIGModel, Version: SIGModelVersion, StarTarget: SIGStarTarget,
		Taken: taken, Rev: rev,
	}
	for _, l := range r.Languages {
		if !l.Measured {
			continue
		}
		ls := LanguageSnapshot{
			Language: l.Language, Files: l.FilesScanned, UnitLOC: l.UnitLOC,
			Measures: map[string]float64{},
		}
		for _, p := range append(append([]PropertyProfile{}, l.Properties...), l.ModuleProperties...) {
			for _, c := range p.Categories {
				if c.Tail != nil {
					ls.Measures[p.ID+" "+c.Tail.Label] = round1(c.Tail.Pct)
				}
			}
		}
		if l.Independence != nil {
			ls.Measures["component_independence hidden"] = round1(l.Independence.Pct)
		}
		if l.Duplication != nil {
			ls.Measures["duplication redundant"] = round1(l.Duplication.Pct)
		}
		if l.Entanglement != nil {
			ls.Entanglement = map[string]float64{
				"density":          round2(l.Entanglement.Density),
				"violation_degree": round3(l.Entanglement.ViolationDegree),
				"lines":            float64(l.Entanglement.Lines),
				"violations":       float64(len(l.Entanglement.Violations)),
			}
		}
		s.Languages = append(s.Languages, ls)
	}
	sort.Slice(s.Languages, func(i, j int) bool { return s.Languages[i].Language < s.Languages[j].Language })
	return s
}

// Rounding to the precision the report prints, so a movement is one a reader
// could have seen. Comparing at full precision would report a change of
// 0.04pp that both reports render identically.
func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// WriteSnapshot renders the snapshot as JSON, indented so a diff between two
// commits of the file is readable line by line.
func WriteSnapshot(w io.Writer, s Snapshot) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	// The measure keys carry > and >= from the model's own wording, and the
	// default encoder escapes those to \u003e. This file is committed and read
	// as a diff, so it stays legible.
	enc.SetEscapeHTML(false)
	return enc.Encode(s)
}

// LoadSnapshot reads a baseline. An empty path returns nil, which the report
// states rather than treating as "nothing moved".
func LoadSnapshot(path string) (*Snapshot, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse baseline %s: %w", path, err)
	}
	return &s, nil
}

// Movement is one figure that changed between two snapshots.
type Movement struct {
	Language string
	Measure  string
	Then     float64
	Now      float64
	// Kind is "moved", "new" (measured now, absent from the baseline) or
	// "gone" (in the baseline, not measured now). The last two matter: a
	// property that stopped being measured must not read as an improvement.
	Kind string
}

// Delta is the change, and is meaningless for a new or gone measure.
func (m Movement) Delta() float64 { return m.Now - m.Then }

// compareSnapshots lines up two runs by language and measure name.
func compareSnapshots(base *Snapshot, now Snapshot) []Movement {
	if base == nil {
		return nil
	}
	baseByLang := map[string]LanguageSnapshot{}
	for _, l := range base.Languages {
		baseByLang[l.Language] = l
	}

	var out []Movement
	for _, l := range now.Languages {
		b, had := baseByLang[l.Language]
		for name, value := range l.Measures {
			was, ok := b.Measures[name]
			switch {
			case !had || !ok:
				out = append(out, Movement{Language: l.Language, Measure: name, Now: value, Kind: "new"})
			case was != value:
				out = append(out, Movement{Language: l.Language, Measure: name, Then: was, Now: value, Kind: "moved"})
			}
		}
		for name, was := range b.Measures {
			if _, still := l.Measures[name]; !still {
				out = append(out, Movement{Language: l.Language, Measure: name, Then: was, Kind: "gone"})
			}
		}
		for name, value := range l.Entanglement {
			was, ok := b.Entanglement[name]
			switch {
			case !ok:
				out = append(out, Movement{Language: l.Language, Measure: "entanglement " + name, Now: value, Kind: "new"})
			case was != value:
				out = append(out, Movement{Language: l.Language, Measure: "entanglement " + name, Then: was, Now: value, Kind: "moved"})
			}
		}
	}
	// A language in the baseline and gone now is the loudest possible change.
	for _, b := range base.Languages {
		found := false
		for _, l := range now.Languages {
			if l.Language == b.Language {
				found = true
			}
		}
		if !found {
			out = append(out, Movement{Language: b.Language, Measure: "the whole surface", Kind: "gone"})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Language != out[j].Language {
			return out[i].Language < out[j].Language
		}
		return out[i].Measure < out[j].Measure
	})
	return out
}

// measureText prints a figure at the precision it was recorded with. The
// measures here span two scales -- a tail is a percentage to one decimal, a
// violation degree is a ratio to three -- and one fixed format flatters the
// second into uselessness: 0.060 renders as "0.1" and a change of -0.004 as
// "-0.0", which reads as a figure that did not move.
func measureText(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// deltaText keeps the sign, which is the whole point of the column.
func deltaText(v float64) string {
	// Rounded away from float noise: 66.8 - 29.3 in binary is not exactly 37.5,
	// and a movement table is not the place to show it.
	v = math.Round(v*1000) / 1000
	if v > 0 {
		return "+" + strconv.FormatFloat(v, 'g', -1, 64)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// writeMovement renders what changed since the baseline, or says why it cannot.
func writeMovement(b *strings.Builder, base *Snapshot, moves []Movement) {
	b.WriteString("## Movement\n\n")
	if base == nil {
		b.WriteString("**No baseline.** `make sig-baseline` records one, and every later report " +
			"then says what moved since it. Without one this report can only say where the code " +
			"stands, not which way it is going, and the difference is not something a reader " +
			"should have to reconstruct from an older copy.\n\n")
		return
	}

	since := base.Taken
	if since == "" {
		since = "the recorded baseline"
	}
	if base.Rev != "" {
		since += " (" + base.Rev + ")"
	}

	if len(moves) == 0 {
		fmt.Fprintf(b, "Nothing measured has changed since %s.\n\n", since)
		return
	}

	fmt.Fprintf(b, "Against the baseline of %s:\n\n", since)
	b.WriteString("| Language | Measure | Then | Now | Change |\n|---|---|---|---|---|\n")
	for _, m := range moves {
		switch m.Kind {
		case "new":
			fmt.Fprintf(b, "| %s | %s | not measured | %s | newly measured |\n",
				m.Language, m.Measure, measureText(m.Now))
		case "gone":
			fmt.Fprintf(b, "| %s | %s | %s | not measured | **no longer measured** |\n",
				m.Language, m.Measure, measureText(m.Then))
		default:
			fmt.Fprintf(b, "| %s | %s | %s | %s | %s |\n",
				m.Language, m.Measure, measureText(m.Then), measureText(m.Now), deltaText(m.Delta()))
		}
	}
	b.WriteString("\nA measure that stopped being measured is listed as such rather than left " +
		"out, because an absent number and an improved one look identical in a total.\n\n")
}
