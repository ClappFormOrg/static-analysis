package main

import (
	"go/ast"
	"sort"
	"strings"
)

// FileDeps is the outgoing coupling of one file.
//
// Volume counts every identifier occurrence that resolves to a declaration
// outside the file -- how much this file leans on the rest of the codebase.
// Span counts the distinct places those references land: one target per
// in-module file, one per external package. Volume without span is a file that
// talks a lot to few neighbours; both high is a file that knows about
// everything.
// VolumeLocal and VolumeCross split Volume by where the reference lands:
// local is a sibling file in the same package, cross is anything else. Volume
// is their sum. The split exists because the two halves are measured with very
// different fidelity -- see measureDeps -- and mixing them biases the ranking
// by architectural layer.
type FileDeps struct {
	File        *File
	Line        int
	Volume      int
	VolumeLocal int
	VolumeCross int
	Span        int
	Targets     []string
}

// measureDeps computes outgoing coupling for every file.
//
// Resolution is syntactic, not type-checked: a qualified `pkg.Sym` is resolved
// through the file's import table, and a bare identifier is resolved against
// the package's cross-file declaration index (Go has no cross-file scoping
// inside a package, so that index is exact). What it cannot resolve is a
// selector on a value -- `x.Foo()` needs x's type -- so method and field
// accesses are not counted.
//
// That makes the number a lower bound, but not a uniform one, and the
// difference matters when ranking files against each other. The two halves of
// the count have different fidelity: sibling-file references resolve exactly,
// while cross-package work reached through a method on a value does not
// resolve at all. A file that leans on same-package declarations therefore
// scores closer to its true weight than one that leans on injected
// dependencies, which is why a persistence layer outranks a service layer here
// by more than a type-checked implementation would. VolumeLocal and VolumeCross are
// reported separately by `-format metrics` so that skew is visible rather than
// something to rediscover.
//
// Counting sibling-file references is deliberate and was checked against
// another tool's output rather than assumed: dropping them moves the median
// agreement materially further away and widens the spread.
func (idx *Index) measureDeps() []FileDeps {
	out := make([]FileDeps, 0, len(idx.Files))
	for _, f := range idx.Files {
		r := newDepResolver(idx, f)
		r.run()

		counted := r.coarse
		if idx.Cfg.SpanGranularity == "file" {
			counted = r.fine
		}
		targets := make([]string, 0, len(counted))
		for t := range counted {
			targets = append(targets, t)
		}
		sort.Strings(targets)

		out = append(out, FileDeps{
			File: f,
			// The convention is to anchor a file-level finding to a line inside the
			// file rather than to line 1; the package clause is the one line
			// every Go file has and is where the imports that drive the score
			// begin.
			Line:        idx.Fset.Position(f.AST.Name.Pos()).Line,
			Volume:      r.volumeLocal + r.volumeCross,
			VolumeLocal: r.volumeLocal,
			VolumeCross: r.volumeCross,
			Span:        len(targets),
			Targets:     targets,
		})
	}
	return out
}

// newDepResolver prepares the reference resolver for one file. Both
// directions of coupling are measured through it -- measureDeps reads the
// outgoing side, measureModules inverts it for the incoming side -- so it is
// constructed in one place and neither can drift from the other.
func newDepResolver(idx *Index, f *File) *depResolver {
	return &depResolver{
		idx:     idx,
		file:    f,
		imports: f.Imports(),
		fine:    map[string]int{},
		coarse:  map[string]int{},
	}
}

type depResolver struct {
	idx     *Index
	file    *File
	imports map[string]string
	local   map[string]bool // names bound inside the declaration being walked
	// volumeLocal counts references landing on a sibling file in this
	// package, volumeCross everything else. Kept apart because only the
	// second is coupling this file could shed by importing less.
	volumeLocal int
	volumeCross int
	// fine counts one target per in-module file; coarse folds each package
	// into a single target. Span is reported at whichever granularity the
	// config asks for -- see Config.SpanGranularity.
	fine   map[string]int
	coarse map[string]int
}

func (r *depResolver) run() {
	for _, d := range r.file.AST.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			r.local = localNames(fd)
			r.walk(fd)
			continue
		}
		r.local = nil
		r.walk(d)
	}
}

func (r *depResolver) record(fine, coarse string, sibling bool) {
	if sibling {
		r.volumeLocal++
	} else {
		r.volumeCross++
	}
	r.fine[fine]++
	r.coarse[coarse]++
}

func (r *depResolver) walk(n ast.Node) {
	ast.Inspect(n, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.SelectorExpr:
			if id, ok := node.X.(*ast.Ident); ok {
				if p, ok := r.imports[id.Name]; ok && !r.local[id.Name] {
					r.record(r.resolveQualified(p, node.Sel.Name), p, false)
					return false
				}
			}
			// Not a package qualifier, so `Sel` is a field or method on a
			// value and is unresolvable without types. Walk the receiver
			// expression only.
			r.walk(node.X)
			return false

		case *ast.KeyValueExpr:
			// A bare identifier key is a struct field name in the
			// overwhelming majority of literals, not a reference.
			if _, isIdent := node.Key.(*ast.Ident); !isIdent {
				r.walk(node.Key)
			}
			r.walk(node.Value)
			return false

		case *ast.LabeledStmt:
			r.walk(node.Stmt)
			return false

		case *ast.BranchStmt:
			return false // the label is not a reference

		case *ast.Field:
			// Names here declare, they do not reference.
			r.walk(node.Type)
			return false

		case *ast.Ident:
			if r.local[node.Name] {
				return false
			}
			if target, ok := r.file.Pkg.Decls[node.Name]; ok && target != r.file {
				// A sibling file is its own target at both granularities:
				// folding it into "my own package" would erase the coupling
				// entirely, and it is real coupling.
				r.record(target.Rel, target.Rel, true)
			}
			return false
		}
		return true
	})
}

// resolveQualified turns `pkg.Sym` into the target the reference lands on. For
// an in-module package that is the specific file declaring Sym; for anything
// else -- stdlib, third party, or a symbol this scanner did not index, such as
// a method promoted from an embedded type -- it is the package as a whole.
func (r *depResolver) resolveQualified(importPath, sym string) string {
	if !strings.HasPrefix(importPath, r.idx.ModulePath) {
		return importPath
	}
	pkg := r.idx.Pkgs[importPath]
	if pkg == nil {
		return importPath
	}
	if target, ok := pkg.Decls[sym]; ok {
		return target.Rel
	}
	return importPath
}

// localNames collects every name bound inside a function: receiver, parameters,
// named results, `:=` definitions, local var/const/type declarations, range and
// type-switch bindings, and closure signatures. Block scoping is flattened,
// which can only ever hide a reference, never invent one.
func localNames(fd *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	addFieldList(out, fd.Recv)
	addFieldList(out, fd.Type.Params)
	addFieldList(out, fd.Type.Results)

	// An assembly or cgo stub is a declaration with no body. Its signature still
	// binds names, which is why the three calls above run first, but ast.Inspect
	// panics on a nil node rather than treating it as an empty tree.
	if fd.Body == nil {
		return out
	}

	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if n.Tok.String() == ":=" {
				addIdents(out, n.Lhs)
			}
		case *ast.RangeStmt:
			if n.Tok.String() == ":=" {
				addIdents(out, []ast.Expr{n.Key, n.Value})
			}
		case *ast.GenDecl:
			for _, spec := range n.Specs {
				switch s := spec.(type) {
				case *ast.ValueSpec:
					for _, id := range s.Names {
						out[id.Name] = true
					}
				case *ast.TypeSpec:
					out[s.Name.Name] = true
				}
			}
		case *ast.FuncLit:
			addFieldList(out, n.Type.Params)
			addFieldList(out, n.Type.Results)
		}
		return true
	})
	return out
}

func addFieldList(out map[string]bool, fl *ast.FieldList) {
	if fl == nil {
		return
	}
	for _, f := range fl.List {
		for _, id := range f.Names {
			if id.Name != "_" {
				out[id.Name] = true
			}
		}
	}
}

func addIdents(out map[string]bool, exprs []ast.Expr) {
	for _, e := range exprs {
		if id, ok := e.(*ast.Ident); ok && id.Name != "_" {
			out[id.Name] = true
		}
	}
}
