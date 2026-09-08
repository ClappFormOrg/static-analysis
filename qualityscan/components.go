package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Component independence: the share of code that no other component can reach.
//
// The model classifies every module as hidden or exposed. A module is hidden
// when nothing outside its own component depends on it, and the property is the
// share of lines of code residing in hidden modules. Four stars needs at least
// 93.7%, which is a strong claim: almost all of a system's code should be
// reachable only from inside the component that owns it, so a change can be made
// without leaving that component.
//
// Unlike every other property here, this one cannot be computed from the source
// alone. It needs the component boundary, and the model puts that decision on
// the system owner during scoping rather than on the measurement. Guessing it
// changes the answer by an order of magnitude: measured over api/ with each
// internal package treated as its own component, the Go surface reads about 10%
// hidden, which says nothing about the code and everything about the guess.
//
// So the boundary is data, not a default. ComponentFile records the decision,
// the date it was made and the reason for each component, `-components` supplies
// it, and a file that matches no component fails the run rather than landing in
// a bucket nobody chose. Without the flag the property reports as not measured,
// with the reason, because a made-up boundary produces a real-looking percentage
// and there is no way to tell one from the other after the fact.

// Component is one top-level component: the paths that belong to it, and why.
type Component struct {
	Name string `json:"name"`
	// Paths are prefixes relative to the scan root. The LONGEST matching prefix
	// wins, so a catch-all like "internal" can sit beside "internal/store"
	// without the declaration order deciding the answer.
	Paths []string `json:"paths"`
	// Reason is why these paths are one component. Required: a boundary with no
	// recorded reasoning is a guess that has been written down, and the whole
	// point of this file is that the boundary was decided rather than assumed.
	Reason string `json:"reason"`
}

// ComponentFile is the on-disk shape: what the file is, when the boundary was
// decided, and the components themselves.
type ComponentFile struct {
	// Note is free text describing the file. Ignored by the tool; present so the
	// file explains itself to whoever opens it first.
	Note string `json:"note,omitempty"`
	// Decided is the ISO date the boundary was agreed. Printed in the report, so
	// a reader can weigh a component map that predates half the codebase.
	Decided    string      `json:"decided"`
	Components []Component `json:"components"`
}

// Components is a loaded, validated component map.
type Components struct {
	Decided string
	List    []Component
}

// LoadComponents reads the component map. An empty path returns a zero
// Components, which reports the property as not measured rather than inventing a
// boundary.
func LoadComponents(path string) (Components, error) {
	if path == "" {
		return Components{}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Components{}, err
	}
	var f ComponentFile
	if err := json.Unmarshal(b, &f); err != nil {
		return Components{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if f.Decided == "" {
		return Components{}, fmt.Errorf("%s: needs a decided date -- the component boundary is a "+
			"scoping decision, and an undated one cannot be told apart from a default", path)
	}
	seen := map[string]bool{}
	for i, c := range f.Components {
		if c.Name == "" || len(c.Paths) == 0 {
			return Components{}, fmt.Errorf("%s: component %d needs a name and at least one path", path, i)
		}
		if c.Reason == "" {
			return Components{}, fmt.Errorf("%s: component %q needs a reason -- a boundary with no "+
				"recorded reasoning is a guess written down", path, c.Name)
		}
		if seen[c.Name] {
			return Components{}, fmt.Errorf("%s: component %q is declared twice", path, c.Name)
		}
		seen[c.Name] = true
	}
	if len(f.Components) == 0 {
		return Components{}, fmt.Errorf("%s: no components declared", path)
	}
	return Components{Decided: f.Decided, List: f.Components}, nil
}

// Measured reports whether a boundary was supplied at all.
func (c Components) Measured() bool { return len(c.List) > 0 }

// Of returns the component owning a module, by longest matching prefix. The
// empty string means no component claims it, which is a hard error at the call
// site rather than a silent bucket: an unmapped file is a scoping question, and
// answering it by default is the mistake this whole file exists to avoid.
func (c Components) Of(rel string) string {
	best, bestLen := "", -1
	for _, comp := range c.List {
		for _, p := range comp.Paths {
			p = strings.Trim(p, "/")
			if p == "" {
				continue
			}
			if rel == p || strings.HasPrefix(rel, p+"/") {
				if len(p) > bestLen {
					best, bestLen = comp.Name, len(p)
				}
			}
		}
	}
	return best
}

// IndependenceProfile is the measured property for one language.
type IndependenceProfile struct {
	// Decided is the date the boundary was agreed, carried into the report.
	Decided   string
	HiddenLOC int
	TotalLOC  int
	Pct       float64
	// FloorPct is the model's 4-star minimum. This is the one property with a
	// floor rather than a cap: more hidden code is better.
	FloorPct float64
	Pass     bool
	Applies  bool
	// ExposedLOC is how many lines would have to become hidden to reach the
	// floor. Zero on a passing measurement.
	ExposedLOC int
	Components []ComponentShare
}

// ComponentShare is one component's contribution.
type ComponentShare struct {
	Name       string
	Modules    int
	LOC        int
	Exposed    int
	ExposedPct float64
}

// independenceFloorPct is the 4-star minimum from SIGModel SIGModelVersion,
// section 3.8.
const independenceFloorPct = 93.7

// measureIndependence computes the property over modules that already carry
// their component and their cross-component incoming count.
func measureIndependence(mods []Module, decided string, verdicts bool) *IndependenceProfile {
	total, hidden := 0, 0
	byComponent := map[string]*ComponentShare{}
	for _, m := range mods {
		total += m.LOC
		if m.Hidden() {
			hidden += m.LOC
		}
		s := byComponent[m.Component]
		if s == nil {
			s = &ComponentShare{Name: m.Component}
			byComponent[m.Component] = s
		}
		s.Modules++
		s.LOC += m.LOC
		if !m.Hidden() {
			s.Exposed += m.LOC
		}
	}
	if total == 0 {
		return nil
	}

	p := &IndependenceProfile{
		Decided:   decided,
		HiddenLOC: hidden,
		TotalLOC:  total,
		Pct:       pct(hidden, total),
		FloorPct:  independenceFloorPct,
		Applies:   verdicts,
	}
	p.Pass = p.Pct >= independenceFloorPct
	if !p.Pass {
		// The lines that have to stop being reachable from outside their own
		// component. Held to the same first-order reading as a tail's LOC to
		// move: the denominator does not change when a module stops being
		// exposed, only which side of the split it sits on.
		p.ExposedLOC = int(float64(total)*independenceFloorPct/100) - hidden
	}

	for _, s := range byComponent {
		s.ExposedPct = pct(s.Exposed, s.LOC)
		p.Components = append(p.Components, *s)
	}
	// Worst first: the component leaking the most code is the one to read.
	sort.Slice(p.Components, func(i, j int) bool {
		if p.Components[i].Exposed != p.Components[j].Exposed {
			return p.Components[i].Exposed > p.Components[j].Exposed
		}
		return p.Components[i].Name < p.Components[j].Name
	})
	return p
}
