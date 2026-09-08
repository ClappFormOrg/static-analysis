package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Module coupling: the model's fourth system property, and the first
// measurement in this tool that points INWARDS.
//
// DEPENDENCY_VOLUME_RISK and DEPENDENCY_SPAN_RISK both describe what a file
// leans on. Nothing described what leans on a file, which is the direction that
// decides whether a change to it is safe: a module fifty other modules reference
// cannot be renamed, moved or reshaped without fifty edits, however few
// dependencies of its own it has.
//
// The measurement is free of new analysis. depResolver already resolves a
// qualified `pkg.Sym` down to the file DECLARING Sym, and a bare identifier
// through the package's cross-file declaration index, so measureDeps has been
// computing exactly these edges all along and discarding their direction. This
// file inverts them.
//
// The known lower bound, which is the same one measureDeps documents with its
// sign flipped: a selector on a value cannot be resolved without types, so a
// module reached only through an injected interface collects no incoming edge
// from its caller. internal/store is reached that way from internal/service, so
// its exposure reads low here. A file that scores high is therefore high; a file
// that scores low may only be invisible.

// moduleMetricCoupling and moduleMetricDuplication are the module-level
// properties a front end can declare it measured. See UnitSet.ModuleMetrics.
const (
	moduleMetricCoupling    = "coupling"
	moduleMetricDuplication = "duplication"
)

// Module is one file measured as the model's module. The model defines a module
// as "a delimited group of declarations", which "generally corresponds to a
// file", and in Go it is exactly a file.
type Module struct {
	File string `json:"file"`
	// LOC is every line of code in the file, in a unit or not. Module coupling
	// is a property of the whole module, so unlike the three unit properties it
	// does not exclude the imports, type declarations and package-level vars
	// that reside in no unit.
	LOC int `json:"loc"`
	// InModules counts the DISTINCT modules that reference this one. This is
	// what the profile bins by default: the model's bands (above 10, above 20,
	// above 50) read as "how many other files would an edit here reach", and a
	// caller that references a module forty times is still one caller to
	// coordinate with.
	InModules int `json:"in_modules"`
	// InRefs counts every individual reference instead. The model's wording,
	// "the number of incoming dependencies, such as invocations", admits both
	// readings, so both are measured and `module_coupling_counts` selects which
	// one the caps are applied to. Reporting only one would hide that the choice
	// was made.
	InRefs int `json:"in_refs"`
	// Component is the top-level component this module belongs to, or the
	// empty string when no component map was supplied. It comes from the
	// map in components.json, never from the directory tree.
	Component string `json:"component,omitempty"`
	// InCrossComponent counts the distinct modules in OTHER components that
	// reference this one. It is what classifies a module as hidden or
	// exposed, and it is always at most InModules.
	InCrossComponent int `json:"in_cross_component"`
	// Lines are this module's lines of code, normalised so that two lines
	// equal modulo whitespace and comments render identically. Duplication is
	// measured over them. Optional in the JSON contract: a language whose
	// front end does not supply them has duplication reported as not
	// measured, the way a language supplying no modules has module coupling
	// reported as not measured.
	Lines []string `json:"lines,omitempty"`
}

// Hidden reports whether nothing outside this module's own component depends
// on it, which is the model's classification for component independence.
func (m Module) Hidden() bool { return m.InCrossComponent == 0 }

// ModuleDependency is one module depending on another, carrying the component
// each sits in. The component graph is built from these: an edge between two
// components is every dependency crossing between them, collapsed.
type ModuleDependency struct {
	From          string
	To            string
	FromComponent string
	ToComponent   string
	// Refs is how many individual references this dependency carries.
	Refs int
}

// ModuleSet is what one measurement pass over the modules produces: the
// modules themselves, and the dependencies between them. The second is kept
// rather than folded into the first because component entanglement needs the
// edges, not the per-module counts.
type ModuleSet struct {
	Modules      []Module
	Dependencies []ModuleDependency
}

// ModuleProperty is a property measured over modules.
type ModuleProperty struct {
	Property
	Measure func(Module) int
}

// elements reduces modules to the pairs the binner needs.
func (p ModuleProperty) elements(mods []Module) []Element {
	out := make([]Element, 0, len(mods))
	for _, m := range mods {
		out = append(out, Element{Value: p.Measure(m), LOC: m.LOC})
	}
	return out
}

// moduleProperties are the module-level properties of SIGModel SIGModelVersion.
//
// The published caps are on the share of lines of code residing in modules with
// more than 10, more than 20 and more than 50 incoming dependencies, so the
// bands break at 11, 21 and 51. The low band starts at 0 rather than 1, because
// a module nothing references is a real and healthy measurement, and dropping it
// would shrink the denominator every other percentage is a share of.
func moduleProperties(counts string) []ModuleProperty {
	metric := "incoming dependencies per module"
	if counts == moduleCouplingCountsReferences {
		metric = "incoming references per module"
	}
	return []ModuleProperty{{
		Property: Property{
			ID:          "module_coupling",
			Name:        "Module coupling",
			Metric:      metric,
			SubChars:    "Modularity, Analysability, Modifiability",
			ElementNoun: "Modules",
			Categories: []Category{
				{"low", 0, 10}, {"moderate", 11, 20}, {"high", 21, 50}, {"very high", 51, unbounded},
			},
			Tails: []Tail{
				{Threshold: 10, CapPct: 10.0},
				{Threshold: 20, CapPct: 5.6},
				{Threshold: 50, CapPct: 1.9},
			},
		},
		Measure: func(m Module) int { return m.Incoming(counts) },
	}}
}

// Incoming returns the count the named convention asks for. An unknown name
// cannot reach here -- LoadConfig rejects one -- so it falls back to the default
// rather than to zero, which would read as an uncoupled module.
func (m Module) Incoming(counts string) int {
	if counts == moduleCouplingCountsReferences {
		return m.InRefs
	}
	return m.InModules
}

// measureModules measures every file in the index as a module.
//
// It runs its own resolver pass rather than sharing measureDeps'. The two are
// called from different formats -- measureDeps serves the threshold rules,
// this serves the SIG profile -- so sharing would cost a pass in whichever
// format needed only one of them, and the resolver is the cheap half of a scan
// that takes about a third of a second over the whole API.
func (idx *Index) measureModules(prefix string, comps Components) (ModuleSet, error) {
	isModule := make(map[string]bool, len(idx.Files))
	for _, f := range idx.Files {
		isModule[f.Rel] = true
	}

	// The component of every module, resolved once. An unmapped file fails the
	// run: the map is a scoping decision, so a file nobody placed is a question
	// for a person, and bucketing it silently would move the percentage.
	component := make(map[string]string, len(idx.Files))
	if comps.Measured() {
		var unmapped []string
		for _, f := range idx.Files {
			name := comps.Of(f.Rel)
			if name == "" {
				unmapped = append(unmapped, f.Rel)
				continue
			}
			component[f.Rel] = name
		}
		if len(unmapped) > 0 {
			shown := unmapped
			if len(shown) > 10 {
				shown = shown[:10]
			}
			return ModuleSet{}, fmt.Errorf("%d file(s) belong to no component, starting with %s: "+
				"add them to the component map or widen a path there, because a module with no "+
				"component cannot be classified as hidden or exposed",
				len(unmapped), strings.Join(shown, ", "))
		}
	}

	inModules := map[string]map[string]bool{}
	inCross := map[string]map[string]bool{}
	inRefs := map[string]int{}
	var deps []ModuleDependency
	for _, f := range idx.Files {
		r := newDepResolver(idx, f)
		r.run()
		for target, n := range r.fine {
			// A target outside the index is a stdlib or third-party package,
			// which is outgoing coupling only: it has no module here to charge.
			if !isModule[target] || target == f.Rel {
				continue
			}
			if inModules[target] == nil {
				inModules[target] = map[string]bool{}
			}
			inModules[target][f.Rel] = true
			inRefs[target] += n
			deps = append(deps, ModuleDependency{
				From: qualifyPath(prefix, f.Rel), To: qualifyPath(prefix, target),
				FromComponent: component[f.Rel], ToComponent: component[target], Refs: n,
			})
			if comps.Measured() && component[target] != component[f.Rel] {
				if inCross[target] == nil {
					inCross[target] = map[string]bool{}
				}
				inCross[target][f.Rel] = true
			}
		}
	}

	out := make([]Module, 0, len(idx.Files))
	for _, f := range idx.Files {
		out = append(out, Module{
			File:             qualifyPath(prefix, f.Rel),
			LOC:              countLOCIn(f.Src),
			InModules:        len(inModules[f.Rel]),
			InRefs:           inRefs[f.Rel],
			Component:        component[f.Rel],
			InCrossComponent: len(inCross[f.Rel]),
			Lines:            normalisedLines(f.Src),
		})
	}
	// Sorted so two runs over the same tree produce byte-identical output and a
	// diff between runs is signal rather than map ordering.
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	sort.Slice(deps, func(i, j int) bool {
		if deps[i].From != deps[j].From {
			return deps[i].From < deps[j].From
		}
		return deps[i].To < deps[j].To
	})
	return ModuleSet{Modules: out, Dependencies: deps}, nil
}

// qualifyPath prefixes a scan-root-relative path with the reported prefix, so a
// path in the report is the path from the repository root.
func qualifyPath(prefix, rel string) string {
	if prefix == "" {
		return rel
	}
	return strings.TrimSuffix(prefix, "/") + "/" + rel
}

// WriteSIGModules dumps every measured module with the counts behind its
// coupling band and its hidden/exposed classification. It is the audit view for
// the two module-level properties, in the same spirit as WriteSIGUnits: a
// percentage in the report can be re-derived from it, and the exposed modules a
// component leaks are a list rather than a summary.
func WriteSIGModules(w io.Writer, sets []UnitSet) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()

	props := moduleProperties(defaultModuleCouplingCounts)
	if err := cw.Write([]string{
		"language", "file", "component", "loc", "in_modules", "in_refs",
		"in_cross_component", "hidden", "module_coupling_category",
	}); err != nil {
		return err
	}
	for _, s := range sets {
		for _, m := range s.Modules {
			row := []string{
				s.Language, m.File, m.Component, strconv.Itoa(m.LOC),
				strconv.Itoa(m.InModules), strconv.Itoa(m.InRefs),
				strconv.Itoa(m.InCrossComponent), strconv.FormatBool(m.Hidden()),
			}
			for _, p := range props {
				row = append(row, categoryOf(p.Property, p.Measure(m)))
			}
			if err := cw.Write(row); err != nil {
				return err
			}
		}
	}
	return cw.Error()
}
