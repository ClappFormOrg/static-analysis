package main

import (
	"fmt"
	"go/ast"
	"go/printer"
	"go/scanner"
	"go/token"
	"strings"
)

// FuncMetrics are the per-function measurements. The first three are the ones
// the vendor scan reports; LOC and McCabe are the SIG model's own metrics and
// are measured alongside them rather than instead of them, because the two sets
// are not interchangeable -- see sigprofile.go.
type FuncMetrics struct {
	Element    string
	File       *File
	Line       int
	Tokens     int
	Params     int
	Complexity int
	// LOC is the SIG unit-size metric: distinct lines of the declaration that
	// carry at least one non-comment token. The vendor measures size in tokens
	// instead, which is why both are here.
	LOC int
	// McCabe is cyclomatic complexity, which is what the SIG model means by
	// unit complexity. Complexity above is cognitive complexity, a different
	// metric with a different scale; neither substitutes for the other.
	McCabe int
	// HasBody is false for a declaration with no Go body -- an assembly or cgo
	// stub. It is still measured, because the vendor-parity rules have always
	// counted it, but it is not a unit of *executable* code, so the SIG profile
	// leaves it out rather than padding the denominator with it.
	HasBody bool
}

// measureFuncs computes the per-function metrics for every top-level function
// and method in the index. Function literals are measured as part of their
// enclosing function, which is how the vendor attributes them too -- every
// element name in the export is a named function.
func (idx *Index) measureFuncs() []FuncMetrics {
	var out []FuncMetrics
	for _, f := range idx.Files {
		for _, d := range f.AST.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			out = append(out, FuncMetrics{
				Element:    idx.element(fd),
				File:       f,
				Line:       idx.Fset.Position(fd.Pos()).Line,
				Tokens:     idx.countTokens(f, fd),
				Params:     countParams(fd, idx.Cfg.CountParameterGroups),
				Complexity: cognitiveComplexity(fd),
				LOC:        idx.countLOC(f, fd),
				McCabe:     mccabeComplexity(fd),
				HasBody:    fd.Body != nil,
			})
		}
	}
	return out
}

// element renders a function the way it is written in Go: `Name` for a plain
// function, `(*Recv).Name` for a method. The vendor export renders methods with
// the receiver in the parameter slot, which reads as though the receiver were
// the argument list. Ours is unambiguous and greppable.
func (idx *Index) element(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	var b strings.Builder
	if err := printer.Fprint(&b, idx.Fset, fd.Recv.List[0].Type); err != nil {
		return fd.Name.Name
	}
	return fmt.Sprintf("(%s).%s", b.String(), fd.Name.Name)
}

// countTokens counts Go lexical tokens across the whole declaration, signature
// and body together. Comments are not tokens and doc comments sit before
// FuncDecl.Pos(), so neither is counted. Semicolons the scanner inserts at line
// ends are dropped: they are an artefact of Go's grammar, not something written
// in the source, and counting them would make the metric track line count.
func (idx *Index) countTokens(f *File, fd *ast.FuncDecl) int {
	start := idx.Fset.Position(fd.Pos()).Offset
	end := idx.Fset.Position(fd.End()).Offset
	if start < 0 || end > len(f.Src) || start >= end {
		return 0
	}
	return countTokensIn(f.Src[start:end])
}

func countTokensIn(src []byte) int {
	fset := token.NewFileSet()
	tf := fset.AddFile("", fset.Base(), len(src))
	var s scanner.Scanner
	// A nil error handler with mode 0 makes the scanner skip comments and
	// swallow the lexical errors a truncated fragment can produce.
	s.Init(tf, src, nil, 0)

	n := 0
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			return n
		}
		if tok == token.SEMICOLON && lit == "\n" {
			continue
		}
		n++
	}
}

// countLOC counts the lines of code in a declaration, which is what the SIG
// model measures unit size in. See countLOCIn for the counting rule.
func (idx *Index) countLOC(f *File, fd *ast.FuncDecl) int {
	start := idx.Fset.Position(fd.Pos()).Offset
	end := idx.Fset.Position(fd.End()).Offset
	if start < 0 || end > len(f.Src) || start >= end {
		return 0
	}
	return countLOCIn(f.Src[start:end])
}

// countLOCIn counts distinct source lines carrying at least one non-comment
// token. A blank line and a comment-only line are both free; a line holding
// code and a trailing comment counts once.
//
// Counting tokens rather than newlines is what makes this the same measurement
// on both surfaces: the webapp emitter runs the equivalent pass over the
// TypeScript scanner, so a Go LOC and a TypeScript LOC mean the same thing and
// the combined roll-up in sigprofile.go is adding like to like.
func countLOCIn(src []byte) int {
	fset := token.NewFileSet()
	tf := fset.AddFile("", fset.Base(), len(src))
	var s scanner.Scanner
	// Mode 0 does not emit comments, so a comment-only line produces no token
	// and never reaches the set. A nil handler swallows the lexical errors a
	// truncated fragment can produce.
	s.Init(tf, src, nil, 0)

	lines := map[int]bool{}
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			return len(lines)
		}
		// The semicolon Go's grammar inserts at a line end is not written in
		// the source, and counting it would make a line of pure `}` register.
		if tok == token.SEMICOLON && lit == "\n" {
			continue
		}
		lines[tf.Position(pos).Line] = true
	}
}

// mccabeComplexity is the McCabe cyclomatic complexity number: one, plus one
// per decision point. This is what the SIG model means by unit complexity.
//
// The definition is deliberately gocyclo's -- `if`, `for`, `range`, a non-default
// `case`, a non-default `select` clause, and each `&&`/`||` -- because a
// repository running golangci-lint typically already has gocyclo, with cyclop
// alongside it computing the same metric in lockstep. Two
// tools computing the same number from the same source is a free cross-check,
// which is the same reason cognitiveComplexity above is anchored to gocognit.
//
// A function literal is not charged as a branch, but the branches inside it are
// charged to the function that encloses it. In Go a closure is anonymous, and
// the SIG model's unit is the smallest *named* piece of executable code, so
// billing it inward is what the model asks for. The webapp emitter does not do
// this for a named arrow function, and sigprofile.go explains why the two
// surfaces differ there.
func mccabeComplexity(fd *ast.FuncDecl) int {
	if fd.Body == nil {
		return 0
	}
	n := 1
	ast.Inspect(fd, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt:
			n++
		case *ast.CaseClause:
			// `default` carries no expressions and adds no path of its own.
			if node.List != nil {
				n++
			}
		case *ast.CommClause:
			// The `default` of a select, likewise.
			if node.Comm != nil {
				n++
			}
		case *ast.BinaryExpr:
			// Each operator, not each run: `a && b && c` is two decisions to
			// McCabe where cognitive complexity charges the run once.
			if node.Op == token.LAND || node.Op == token.LOR {
				n++
			}
		}
		return true
	})
	return n
}

// countParams counts declared parameters. The receiver is excluded -- the
// vendor counted `NewPromoter`'s eight parameters and it has no receiver, and a
// receiver is not something a caller passes -- and a variadic counts as one.
//
// byGroup counts declaration groups instead of names, so `a, b int` is one
// rather than two. Names is the default because it is what a caller has to
// supply at the call site; grouping is a way of writing the signature, not a
// property of it.
func countParams(fd *ast.FuncDecl, byGroup bool) int {
	if fd.Type.Params == nil {
		return 0
	}
	n := 0
	for _, field := range fd.Type.Params.List {
		switch {
		case byGroup, len(field.Names) == 0:
			n++ // one group, or one unnamed parameter
		default:
			n += len(field.Names)
		}
	}
	return n
}

// cognitiveComplexity implements the Campbell/SonarSource cognitive complexity
// metric: every control-flow structure costs one, structures nested inside
// others cost one more per level of nesting, and each run of `&&`/`||` costs
// one.
//
// This is the same definition gocognit implements, so a repository running both
// can cross-check one against the other. The thresholds in config.go are
// gocognit's for that reason.
//
// It is NOT the vendor's "function nesting complexity", which an earlier
// version of this file assumed on the strength of the name. On a flat dispatch
// of switches the vendor scores several times what this returns, and this one is
// right by the spec: the metric charges once per switch rather than once per
// case. The vendor also runs well above this on functions containing no switch
// at all, so its extra weight is not switch fan-out either. Reproducing it would
// mean guessing at an undocumented formula from a handful of data points and
// giving up the gocognit cross-check, so this rule tracks a metric that can be
// defined instead of one it could only imitate. What the vendor catches and this
// does not is breadth, and a function broad enough to score that high there
// trips FUNCTION_SIZE_RISK here anyway.
// A declaration with no body -- an assembly or cgo stub -- scores 0 and must be
// rejected here rather than in walkBody. walkBody's `n == nil` guard cannot
// catch it: a nil *ast.BlockStmt boxed into an ast.Node is an interface with a
// type and a nil value, which does not compare equal to nil, so ast.Inspect gets
// through the guard and panics on the nil pointer.
func cognitiveComplexity(fd *ast.FuncDecl) int {
	if fd.Body == nil {
		return 0
	}
	v := &cognitiveVisitor{name: fd.Name.Name, counted: map[ast.Node]bool{}}
	v.walkBody(fd.Body, 0)
	return v.total
}

type cognitiveVisitor struct {
	name    string
	total   int
	counted map[ast.Node]bool
}

func (v *cognitiveVisitor) walkBody(n ast.Node, nesting int) {
	if n == nil {
		return
	}
	ast.Inspect(n, func(node ast.Node) bool {
		if node == nil {
			return false
		}
		switch node := node.(type) {
		case *ast.IfStmt:
			v.visitIf(node, nesting)
			return false
		case *ast.SwitchStmt:
			v.inc(1 + nesting)
			v.cond(node.Tag, nesting)
			v.children(node.Body, nesting+1)
			return false
		case *ast.TypeSwitchStmt:
			v.inc(1 + nesting)
			v.children(node.Body, nesting+1)
			return false
		case *ast.SelectStmt:
			v.inc(1 + nesting)
			v.children(node.Body, nesting+1)
			return false
		case *ast.ForStmt:
			v.inc(1 + nesting)
			v.cond(node.Cond, nesting)
			v.walkBody(node.Body, nesting+1)
			return false
		case *ast.RangeStmt:
			v.inc(1 + nesting)
			v.walkBody(node.Body, nesting+1)
			return false
		case *ast.FuncLit:
			// A closure adds a nesting level but is not itself a branch.
			v.walkBody(node.Body, nesting+1)
			return false
		case *ast.BranchStmt:
			// `break`/`continue`/`goto` to a label is a non-local jump.
			if node.Label != nil {
				v.inc(1)
			}
			return true
		case *ast.BinaryExpr:
			v.logicalRun(node)
			return true
		case *ast.CallExpr:
			if isCallTo(node, v.name) {
				v.inc(1) // direct recursion
			}
			return true
		}
		return true
	})
}

// visitIf charges the `if` at the current nesting level and walks its body one
// level deeper. An `else if` chains at the same level rather than nesting: the
// reader follows it as one flat ladder, which is the whole point of the metric.
func (v *cognitiveVisitor) visitIf(n *ast.IfStmt, nesting int) {
	v.inc(1 + nesting)
	v.cond(n.Cond, nesting)
	v.walkBody(n.Body, nesting+1)

	switch e := n.Else.(type) {
	case *ast.IfStmt:
		v.inc(1)
		v.cond(e.Cond, nesting)
		v.walkBody(e.Body, nesting+1)
		v.elseTail(e.Else, nesting)
	case *ast.BlockStmt:
		v.inc(1)
		v.walkBody(e, nesting+1)
	}
}

func (v *cognitiveVisitor) elseTail(else_ ast.Stmt, nesting int) {
	switch e := else_.(type) {
	case *ast.IfStmt:
		v.inc(1)
		v.cond(e.Cond, nesting)
		v.walkBody(e.Body, nesting+1)
		v.elseTail(e.Else, nesting)
	case *ast.BlockStmt:
		v.inc(1)
		v.walkBody(e, nesting+1)
	}
}

// cond walks an expression for logical runs and nested closures without
// charging it as a branch of its own.
func (v *cognitiveVisitor) cond(e ast.Expr, nesting int) {
	if e == nil {
		return
	}
	v.walkBody(e, nesting)
}

// children walks the statements of a switch/select body at the given nesting,
// without re-charging the switch itself.
func (v *cognitiveVisitor) children(b *ast.BlockStmt, nesting int) {
	if b == nil {
		return
	}
	for _, stmt := range b.List {
		switch c := stmt.(type) {
		case *ast.CaseClause:
			for _, e := range c.List {
				v.cond(e, nesting)
			}
			for _, s := range c.Body {
				v.walkBody(s, nesting)
			}
		case *ast.CommClause:
			for _, s := range c.Body {
				v.walkBody(s, nesting)
			}
		default:
			v.walkBody(stmt, nesting)
		}
	}
}

// logicalRun charges one point per run of the same logical operator. `a && b &&
// c` is one run and costs one; `a && b || c` is two and costs two. Parentheses
// end a run -- the inner expression is visited separately and charged on its
// own -- because that is exactly the reading cost parentheses remove.
func (v *cognitiveVisitor) logicalRun(n *ast.BinaryExpr) {
	if !isLogical(n.Op) || v.counted[n] {
		return
	}
	var ops []token.Token
	var flatten func(e ast.Expr)
	flatten = func(e ast.Expr) {
		b, ok := e.(*ast.BinaryExpr)
		if !ok || !isLogical(b.Op) {
			return
		}
		v.counted[b] = true
		flatten(b.X)
		ops = append(ops, b.Op)
		flatten(b.Y)
	}
	flatten(n)

	runs := 0
	var prev token.Token
	for _, op := range ops {
		if op != prev {
			runs++
			prev = op
		}
	}
	v.inc(runs)
}

func (v *cognitiveVisitor) inc(n int) { v.total += n }

func isLogical(op token.Token) bool { return op == token.LAND || op == token.LOR }

func isCallTo(c *ast.CallExpr, name string) bool {
	id, ok := c.Fun.(*ast.Ident)
	if ok {
		return id.Name == name
	}
	sel, ok := c.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == name
}
