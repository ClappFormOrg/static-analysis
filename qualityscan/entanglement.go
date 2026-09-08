package main

import (
	"fmt"
	"sort"
	"strings"
)

// Component entanglement: how tangled the graph BETWEEN components is.
//
// Component independence asks how much code is reachable from outside its own
// component. This asks about the shape of the reaching: how many communication
// lines there are, and how many of them are lines a reasonable architecture
// would not have.
//
// The model is unusually specific here, so this implements what it specifies:
//
//   - A communication line is a directed pair of components carrying at least
//     one dependency, and its weight is the number of dependencies it carries.
//   - Communication density is lines divided by CONNECTED components. A
//     component with no line at either end does not participate and is excluded,
//     which stops a pile of isolated components flattering the number.
//   - Communication violation degree is the summed weight of the violations
//     over the total weight of all lines.
//   - Violations are checked in order -- direct cycles, then indirect cycles,
//     then transitive dependencies -- and a line can be marked as only one type.
//
// WHAT THIS DOES NOT DO is produce the model's published figure. The final score
// normalises density and violation degree "by dividing by the maximal values for
// each in our benchmark", and those maxima are not in the document. A number
// computed without them is a different metric wearing the model's threshold, so
// this reports the two inputs and the violations behind them, and prints no
// verdict against the 0.077 cap. That absence is stated in the report, with the
// reason, so nobody restores the verdict by guessing the constants.

// ComponentEdge is one communication line: every dependency from one component
// to another, collapsed into a single weighted edge.
type ComponentEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Weight is the number of module-to-module dependencies the line carries,
	// counted the same way module coupling counts an incoming dependency: one
	// per distinct pair of modules. Two modules referencing each other forty
	// times are still one dependency to sever.
	Weight int `json:"weight"`
}

// String renders the edge as `from -> to`, the form the entanglement tables use.
func (e ComponentEdge) String() string { return e.From + " -> " + e.To }

// EntanglementViolation is one communication line the model judges unintended,
// with the weight that judgement carries.
type EntanglementViolation struct {
	// Kind is cyclic, indirect cyclic or transitive.
	Kind string
	Edge ComponentEdge
	// Weight is what the violation contributes to the violation degree. For a
	// cycle it is the lightest line involved, since that is the line whose
	// removal breaks the cycle for the least effort; for a transitive
	// dependency it is the line's own weight.
	Weight int
	// Detail says which lines the violation involves, for the report.
	Detail string
}

// EntanglementProfile is the measured property for one language.
type EntanglementProfile struct {
	Edges []ComponentEdge
	// Connected is the number of components with at least one line at either
	// end. It is the denominator of Density.
	Connected       int
	Lines           int
	Density         float64
	TotalWeight     int
	ViolationWeight int
	ViolationDegree float64
	Violations      []EntanglementViolation
}

// Violation kinds, in the order the model checks them.
const (
	violationCyclic         = "cyclic"
	violationIndirectCyclic = "indirect cyclic"
	violationTransitive     = "transitive"
)

// entanglementCap is the model's 4-star threshold on the combined score. It is
// quoted here so the report can say what cannot be evaluated and why, NOT to be
// compared against anything this file computes.
const entanglementCap = 0.077

// componentEdges collapses module-level dependencies into the component graph.
// Edges within one component are not communication lines: they are what a
// component is for.
func componentEdges(deps []ModuleDependency) []ComponentEdge {
	weight := map[[2]string]int{}
	for _, d := range deps {
		if d.FromComponent == "" || d.ToComponent == "" || d.FromComponent == d.ToComponent {
			continue
		}
		weight[[2]string{d.FromComponent, d.ToComponent}]++
	}
	out := make([]ComponentEdge, 0, len(weight))
	for k, w := range weight {
		out = append(out, ComponentEdge{From: k[0], To: k[1], Weight: w})
	}
	sortEdges(out)
	return out
}

func sortEdges(edges []ComponentEdge) {
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].To < edges[j].To
	})
}

// measureEntanglement computes the two figures and the violations behind them.
func measureEntanglement(edges []ComponentEdge) *EntanglementProfile {
	if len(edges) == 0 {
		return nil
	}
	sorted := append([]ComponentEdge(nil), edges...)
	sortEdges(sorted)

	p := &EntanglementProfile{Edges: sorted, Lines: len(sorted)}
	connected := map[string]bool{}
	for _, e := range sorted {
		p.TotalWeight += e.Weight
		connected[e.From] = true
		connected[e.To] = true
	}
	p.Connected = len(connected)
	if p.Connected > 0 {
		p.Density = float64(p.Lines) / float64(p.Connected)
	}

	g := newEdgeGraph(sorted)
	p.Violations = append(p.Violations, g.directCycles()...)
	p.Violations = append(p.Violations, g.indirectCycles()...)
	p.Violations = append(p.Violations, g.transitives()...)

	for _, v := range p.Violations {
		p.ViolationWeight += v.Weight
	}
	if p.TotalWeight > 0 {
		p.ViolationDegree = float64(p.ViolationWeight) / float64(p.TotalWeight)
	}
	return p
}

// edgeGraph is the component graph during violation detection. `live` drops to
// false as a line is claimed, because the model allows a line to be marked as
// only one type of violation, and because the indirect-cycle pass runs on the
// graph the direct-cycle pass left behind.
type edgeGraph struct {
	edges []ComponentEdge
	live  []bool
	// all is every line, live or not, for the questions that are about the
	// architecture rather than about the remaining graph -- specifically
	// whether a component has any outgoing line at all.
	all []ComponentEdge
}

func newEdgeGraph(edges []ComponentEdge) *edgeGraph {
	g := &edgeGraph{edges: edges, live: make([]bool, len(edges)), all: edges}
	for i := range g.live {
		g.live[i] = true
	}
	return g
}

func (g *edgeGraph) index(from, to string) int {
	for i, e := range g.edges {
		if e.From == from && e.To == to {
			return i
		}
	}
	return -1
}

// directCycles finds pairs of components that depend on each other.
//
// The violation weighs the LIGHTEST of the two lines, because removing that one
// breaks the cycle for what is most likely the least effort, and that line is
// the one claimed. When both weigh the same, both are claimed: the model removes
// both before looking for indirect cycles, since either could be the unintended
// one.
func (g *edgeGraph) directCycles() []EntanglementViolation {
	var out []EntanglementViolation
	for i, e := range g.edges {
		if !g.live[i] {
			continue
		}
		j := g.index(e.To, e.From)
		if j < 0 || !g.live[j] || j < i {
			continue
		}
		back := g.edges[j]
		weight := min(e.Weight, back.Weight)

		lighter := e
		switch {
		case e.Weight < back.Weight:
			g.live[i] = false
		case back.Weight < e.Weight:
			lighter = back
			g.live[j] = false
		default:
			g.live[i], g.live[j] = false, false
		}
		out = append(out, EntanglementViolation{
			Kind: violationCyclic, Edge: lighter, Weight: weight,
			Detail: fmt.Sprintf("%s (%d) and %s (%d) depend on each other",
				e, e.Weight, back, back.Weight),
		})
	}
	return out
}

// indirectCycles finds cycles through three or more components in what the
// direct pass left behind, one at a time: the lightest line of a cycle is
// claimed, weighs the violation, and leaves the graph, and the search repeats
// until nothing cyclic remains.
//
// The model calls these less severe than direct cycles for a real reason -- a
// change to B reaches A but a change to A does not automatically reach B -- and
// treats them identically for weighting, which is what this does.
func (g *edgeGraph) indirectCycles() []EntanglementViolation {
	var out []EntanglementViolation
	for {
		cycle := g.findCycle()
		if len(cycle) == 0 {
			return out
		}
		lightest := cycle[0]
		for _, i := range cycle {
			if g.edges[i].Weight < g.edges[lightest].Weight {
				lightest = i
			}
		}
		g.live[lightest] = false

		names := make([]string, 0, len(cycle))
		for _, i := range cycle {
			names = append(names, g.edges[i].String())
		}
		sort.Strings(names)
		out = append(out, EntanglementViolation{
			Kind: violationIndirectCyclic, Edge: g.edges[lightest], Weight: g.edges[lightest].Weight,
			Detail: "cycle through " + strings.Join(names, ", "),
		})
	}
}

// findCycle returns the edge indices of one cycle in the live graph, or nil.
// Depth-first, taking components and edges in sorted order so the cycle found is
// the same on every run over the same graph.
func (g *edgeGraph) findCycle() []int {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	colour := map[string]int{}
	var stack []int
	var found []int

	var visit func(node string) bool
	visit = func(node string) bool {
		colour[node] = grey
		for i, e := range g.edges {
			if !g.live[i] || e.From != node {
				continue
			}
			stack = append(stack, i)
			switch colour[e.To] {
			case grey:
				// Back edge: the cycle is the tail of the stack from the edge
				// that first entered e.To.
				for k, idx := range stack {
					if g.edges[idx].From == e.To {
						found = append([]int(nil), stack[k:]...)
						return true
					}
				}
				found = append([]int(nil), stack...)
				return true
			case white:
				if visit(e.To) {
					return true
				}
			}
			stack = stack[:len(stack)-1]
		}
		colour[node] = black
		return false
	}

	for _, node := range g.nodes() {
		if colour[node] == white {
			stack = stack[:0]
			if visit(node) {
				return found
			}
		}
	}
	return nil
}

// nodes lists every component in the graph, sorted.
func (g *edgeGraph) nodes() []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range g.all {
		for _, n := range []string{e.From, e.To} {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out
}

// transitives finds a direct line that bypasses a path already going the same
// way. All three of the model's criteria have to hold:
//
//   - the line is transitive: an indirect path runs from its source to its
//     target as well;
//   - its target has at least one outgoing line of its own, because a component
//     with none is a library, and depending directly on a library is what
//     libraries are for;
//   - the line is the lightest of every line on every path between the two, so
//     what it bypasses really is the normal flow rather than the exception.
func (g *edgeGraph) transitives() []EntanglementViolation {
	var out []EntanglementViolation
	for i, e := range g.edges {
		if !g.live[i] {
			continue
		}
		if !g.hasOutgoing(e.To) {
			continue
		}
		onPaths := g.linesOnPathsBetween(e.From, e.To, i)
		if len(onPaths) == 0 {
			continue
		}
		lightest := onPaths[0].Weight
		for _, o := range onPaths {
			lightest = min(lightest, o.Weight)
		}
		if e.Weight > lightest {
			continue
		}
		g.live[i] = false
		out = append(out, EntanglementViolation{
			Kind: violationTransitive, Edge: e, Weight: e.Weight,
			Detail: fmt.Sprintf("%s (%d) duplicates an indirect path whose lightest line is %d",
				e, e.Weight, lightest),
		})
	}
	return out
}

// hasOutgoing reports whether a component depends on anything at all, over the
// whole graph rather than the remaining one: whether a component is a leaf is a
// fact about the architecture, not about which lines a previous pass claimed.
func (g *edgeGraph) hasOutgoing(node string) bool {
	for _, e := range g.all {
		if e.From == node {
			return true
		}
	}
	return false
}

// linesOnPathsBetween returns every live line lying on some path from -> to that
// does not use the line at skip. A line lies on such a path when `from` reaches
// its source and its target reaches `to`.
func (g *edgeGraph) linesOnPathsBetween(from, to string, skip int) []ComponentEdge {
	forward := g.reachable(from, skip, false)
	backward := g.reachable(to, skip, true)

	var out []ComponentEdge
	for i, e := range g.edges {
		if i == skip || !g.live[i] {
			continue
		}
		if forward[e.From] && backward[e.To] {
			out = append(out, e)
		}
	}
	return out
}

// reachable returns the components reachable from start, following lines
// forwards or backwards, ignoring the line at skip.
func (g *edgeGraph) reachable(start string, skip int, reverse bool) map[string]bool {
	seen := map[string]bool{start: true}
	queue := []string{start}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		for i, e := range g.edges {
			if i == skip || !g.live[i] {
				continue
			}
			src, dst := e.From, e.To
			if reverse {
				src, dst = e.To, e.From
			}
			if src == node && !seen[dst] {
				seen[dst] = true
				queue = append(queue, dst)
			}
		}
	}
	return seen
}
