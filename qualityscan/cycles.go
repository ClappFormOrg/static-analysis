package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// importEdge is one in-module import, remembered with the place it is written
// so a cycle finding can point at a line someone can go and delete.
type importEdge struct {
	FromPkg string // importing package's import path
	ToPkg   string // imported package's import path
	File    *File
	Line    int
}

// Cycle is one directory-level reference cycle: a set of directories that
// depend on each other, directly or transitively.
type Cycle struct {
	Depth   int      // directory depth the cycle appears at
	Members []string // the directories in the cycle
	Edges   []importEdge
}

// findCycles reports reference cycles in the directory tree.
//
// Go rejects an import cycle between packages at compile time, so the package
// graph is always acyclic and checking it proves nothing. The cycle that *can*
// exist is between directories, and the usual shape is a leaf package parked
// under the directory that consumes it: `internal/server` imports every package
// under `internal/service`, while each of those imports a helper that happens to
// sit at `internal/server/<helper>`. Neither import is a package cycle, but the
// two directories still point at each other, so neither can be read, moved or
// extracted without the other. The fix is to hoist the leaf out to its own
// directory, which is the shape a directory-level cycle usually takes and was
// resolved.
//
// The graph is therefore built over directories truncated to a fixed depth, for
// every depth in turn, and the strongly connected components of each are
// reported. A cycle found at a shallow depth is the more serious one, so edges
// are claimed shallowest-first and never reported twice.
func (idx *Index) findCycles() []Cycle {
	edges := idx.importEdges()

	maxDepth := 0
	for _, pkg := range idx.Pkgs {
		if d := dirDepth(pkg.Dir); d > maxDepth {
			maxDepth = d
		}
	}

	claimed := map[[2]string]bool{}
	var out []Cycle

	for depth := 1; depth <= maxDepth; depth++ {
		graph := map[string]map[string]bool{}
		byNodePair := map[[2]string][]importEdge{}

		for _, e := range edges {
			from := idx.foldedDir(e.FromPkg, depth)
			to := idx.foldedDir(e.ToPkg, depth)
			if from == to || from == "" || to == "" {
				continue
			}
			if graph[from] == nil {
				graph[from] = map[string]bool{}
			}
			graph[from][to] = true
			byNodePair[[2]string{from, to}] = append(byNodePair[[2]string{from, to}], e)
		}

		for _, comp := range stronglyConnected(graph) {
			member := map[string]bool{}
			for _, m := range comp {
				member[m] = true
			}

			var cyc []importEdge
			seen := map[[2]string]bool{}
			for pair, list := range byNodePair {
				if !member[pair[0]] || !member[pair[1]] {
					continue
				}
				// One finding per package-to-package edge, not per import
				// statement: 175 files importing the same package across a
				// cycle is one architectural fact, not 175 of them.
				for _, e := range list {
					key := [2]string{e.FromPkg, e.ToPkg}
					if seen[key] || claimed[key] {
						continue
					}
					seen[key] = true
					claimed[key] = true
					cyc = append(cyc, e)
				}
			}
			if len(cyc) == 0 {
				continue
			}
			sort.Slice(cyc, func(i, j int) bool {
				if cyc[i].File.Rel != cyc[j].File.Rel {
					return cyc[i].File.Rel < cyc[j].File.Rel
				}
				return cyc[i].Line < cyc[j].Line
			})
			sort.Strings(comp)
			out = append(out, Cycle{Depth: depth, Members: comp, Edges: cyc})
		}
	}
	return out
}

func (idx *Index) importEdges() []importEdge {
	var out []importEdge
	for _, f := range idx.Files {
		for _, imp := range f.AST.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil || !strings.HasPrefix(p, idx.ModulePath) {
				continue
			}
			if idx.Pkgs[p] == nil {
				continue // excluded from the scan (generated code, migrations)
			}
			out = append(out, importEdge{
				FromPkg: f.Pkg.ImportPath,
				ToPkg:   p,
				File:    f,
				Line:    idx.Fset.Position(imp.Pos()).Line,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File.Rel != out[j].File.Rel {
			return out[i].File.Rel < out[j].File.Rel
		}
		return out[i].Line < out[j].Line
	})
	return out
}

// foldedDir truncates a package's directory to the given depth, which is how a
// subdirectory gets folded into its parent.
func (idx *Index) foldedDir(importPath string, depth int) string {
	pkg := idx.Pkgs[importPath]
	if pkg == nil || pkg.Dir == "." {
		return ""
	}
	parts := strings.Split(pkg.Dir, "/")
	if len(parts) > depth {
		parts = parts[:depth]
	}
	return strings.Join(parts, "/")
}

func dirDepth(dir string) int {
	if dir == "." || dir == "" {
		return 0
	}
	return len(strings.Split(dir, "/"))
}

// stronglyConnected returns Tarjan's strongly connected components of size two
// or more. A single node is only a component if it has a self-edge, and the
// caller has already dropped those.
func stronglyConnected(graph map[string]map[string]bool) [][]string {
	var (
		index   = map[string]int{}
		lowlink = map[string]int{}
		onStack = map[string]bool{}
		stack   []string
		next    int
		out     [][]string
	)

	nodes := make([]string, 0, len(graph))
	for n := range graph {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)

	var strongConnect func(v string)
	strongConnect = func(v string) {
		index[v] = next
		lowlink[v] = next
		next++
		stack = append(stack, v)
		onStack[v] = true

		succ := make([]string, 0, len(graph[v]))
		for w := range graph[v] {
			succ = append(succ, w)
		}
		sort.Strings(succ)

		for _, w := range succ {
			switch {
			case !visited(index, w):
				strongConnect(w)
				lowlink[v] = min(lowlink[v], lowlink[w])
			case onStack[w]:
				lowlink[v] = min(lowlink[v], index[w])
			}
		}

		if lowlink[v] == index[v] {
			var comp []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			if len(comp) > 1 {
				out = append(out, comp)
			}
		}
	}

	for _, n := range nodes {
		if !visited(index, n) {
			strongConnect(n)
		}
	}
	return out
}

// visited distinguishes "never indexed" from "indexed as 0", which a plain map
// lookup cannot.
func visited(index map[string]int, n string) bool {
	_, ok := index[n]
	return ok
}

// Describe renders the cycle as the one-line finding text, naming every
// directory on the loop so the reader can see where to cut it.
func (c Cycle) Describe() string {
	return fmt.Sprintf("import is part of a %d-directory reference cycle: %s",
		len(c.Members), strings.Join(c.Members, " <-> "))
}
