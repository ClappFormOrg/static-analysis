package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// The rules in this file are the ones the vendor's catalogue lists but scores
// zero on for Go, because it has no Go implementation of them -- not because
// the codebase is clean. They are separated from the eight rules in report.go
// for that reason: those reproduce a vendor measurement, these fill a hole in
// it, and only the first group can be diffed against a vendor export.
//
// Rules a golangci-lint config already covers are deliberately absent:
// `Duplicate` is dupl, and the resource open/close family is bodyclose,
// sqlclosecheck and rowserrcheck.

// tier2Rules runs every gap rule and returns their findings unsorted.
func (idx *Index) tier2Rules() []Finding {
	var out []Finding
	out = append(out, idx.loopRules()...)
	out = append(out, idx.emptyBranches()...)
	out = append(out, idx.debugStatements()...)
	out = append(out, idx.switchWithoutDefault()...)
	out = append(out, idx.missingDocComments()...)
	out = append(out, idx.commentRules()...)
	return out
}

func (idx *Index) finding(rule string, f *File, pos token.Pos, element, desc string) Finding {
	return Finding{
		Rule:        rule,
		Element:     element,
		Description: desc,
		File:        f.Rel,
		Line:        idx.Fset.Position(pos).Line,
		Severity:    SeverityFinding.String(),
	}
}

// -- Loop rules ---------------------------------------------------------------

// loopRules reports the three things that turn a cheap loop into an expensive
// one: a query per iteration, a string rebuilt per iteration, and a loop inside
// a loop. All three are found in one walk because all three need the same
// thing -- knowing how deep in loops a statement sits.
func (idx *Index) loopRules() []Finding {
	var out []Finding
	for _, f := range idx.Files {
		for _, d := range f.AST.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			s := &loopScan{
				idx:     idx,
				file:    f,
				element: idx.element(fd),
				strings: stringVars(fd),
				seen:    map[int]bool{},
			}
			s.walk(fd.Body, 0)
			out = append(out, s.out...)
		}
	}
	return out
}

type loopScan struct {
	idx     *Index
	file    *File
	element string
	strings map[string]bool // names in this function known to hold a string
	seen    map[int]bool    // lines already reported for SQL, so a chained query counts once
	cursors []string        // receivers of the enclosing `for x.Next()` loops
	out     []Finding
}

func (s *loopScan) walk(n ast.Node, depth int) {
	ast.Inspect(n, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.FuncLit:
			// A closure is its own loop context: a loop inside a callback
			// passed to a loop is not the same mistake as a doubly nested one.
			s.walk(node.Body, 0)
			return false
		case *ast.ForStmt:
			s.enterLoop(node, node.Body, depth)
			return false
		case *ast.RangeStmt:
			s.enterLoop(node, node.Body, depth)
			return false
		}
		if depth == 0 {
			return true
		}
		switch node := node.(type) {
		case *ast.CallExpr:
			s.checkQuery(node)
		case *ast.AssignStmt:
			s.checkConcat(node)
		}
		return true
	})
}

func (s *loopScan) enterLoop(loop ast.Node, body *ast.BlockStmt, depth int) {
	if depth >= 1 {
		s.out = append(s.out, s.idx.finding(RuleNestedLoop, s.file, loop.Pos(), s.element,
			fmt.Sprintf("loop nested %d deep; the work inside runs the product of both counts", depth+1)))
	}
	if cur := rowCursor(loop); cur != "" {
		s.cursors = append(s.cursors, cur)
		defer func() { s.cursors = s.cursors[:len(s.cursors)-1] }()
	}
	s.walk(body, depth+1)
}

// rowCursor returns the receiver of a `for rows.Next()` loop. Draining a
// result set is one round trip, so the Scan inside such a loop is the correct
// way to read it, not an N+1 -- the SQL rule has to know to leave it alone.
func rowCursor(loop ast.Node) string {
	f, ok := loop.(*ast.ForStmt)
	if !ok {
		return ""
	}
	call, ok := f.Cond.(*ast.CallExpr)
	if !ok {
		return ""
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Next" {
		return ""
	}
	return rootIdent(sel.X)
}

// checkQuery reports a database call made from inside a loop -- the shape of
// every N+1. Chained calls on one line collapse to a single finding, because
// `tx.Where(...).Find(...)` is one query, not two.
func (s *loopScan) checkQuery(c *ast.CallExpr) {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	name := sel.Sel.Name
	cfg := s.idx.Cfg
	if contains(s.cursors, rootIdent(sel.X)) {
		return // draining the result set of a query made outside the loop
	}

	// A chainable builder never executes, so it is never a round trip. SQLMethods
	// already leaves them out for that reason, but the SQLReceivers arm below
	// matches on the RECEIVER and would let `db.Where(...)` or `db.Table(...)`
	// through on the strength of the variable being called `db` -- which is how
	// noteReferenceDataClause, a function that assembles a subquery and returns it
	// unexecuted, came to be reported twice as a query per iteration. Checked
	// before the arms rather than inside the receiver one, so a builder is not a
	// query whichever arm would otherwise claim it.
	if contains(cfg.SQLChainMethods, name) {
		return
	}

	why := ""
	switch {
	case contains(cfg.SQLMethods, name):
		why = "database call"
	case hasAnySuffix(name, cfg.SQLMethodSuffixes):
		why = "transaction-scoped repository call"
	case contains(cfg.SQLReceivers, rootIdent(sel.X)):
		why = "database call"
	default:
		return
	}

	line := s.idx.Fset.Position(c.Pos()).Line
	if s.seen[line] {
		return
	}
	s.seen[line] = true
	s.out = append(s.out, s.idx.finding(RuleSQLInLoop, s.file, c.Pos(), s.element,
		fmt.Sprintf("%s %s() inside a loop; one round trip per iteration", why, name)))
}

// checkConcat reports `s += ...` and `s = s + ...` on a string inside a loop,
// which reallocates the whole string every pass. strings.Builder is the fix.
func (s *loopScan) checkConcat(a *ast.AssignStmt) {
	if len(a.Lhs) != 1 || len(a.Rhs) != 1 {
		return
	}
	target, ok := a.Lhs[0].(*ast.Ident)
	if !ok || !s.strings[target.Name] {
		return
	}
	switch a.Tok {
	case token.ADD_ASSIGN:
	case token.ASSIGN:
		b, ok := a.Rhs[0].(*ast.BinaryExpr)
		if !ok || b.Op != token.ADD || rootIdent(b.X) != target.Name {
			return
		}
	default:
		return
	}
	s.out = append(s.out, s.idx.finding(RuleStringConcatInLoop, s.file, a.Pos(), s.element,
		fmt.Sprintf("string %s rebuilt by concatenation inside a loop; use strings.Builder", target.Name)))
}

// stringVars collects the names in a function that demonstrably hold a string:
// declared `var s string`, or initialised from something that plainly produces
// one. Anything whose type needs real inference is left out, so the concat rule
// under-reports rather than guessing.
func stringVars(fd *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(fd, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.ValueSpec:
			if id, ok := n.Type.(*ast.Ident); ok && id.Name == "string" {
				for _, name := range n.Names {
					out[name.Name] = true
				}
			}
			for i, v := range n.Values {
				if producesString(v) && i < len(n.Names) {
					out[n.Names[i].Name] = true
				}
			}
		case *ast.AssignStmt:
			if n.Tok != token.DEFINE {
				return true
			}
			for i, lhs := range n.Lhs {
				id, ok := lhs.(*ast.Ident)
				if ok && i < len(n.Rhs) && producesString(n.Rhs[i]) {
					out[id.Name] = true
				}
			}
		}
		return true
	})
	return out
}

// stringProducers are calls whose result is a string in every package that
// spells them this way -- fmt.Sprintf, strings.Join, a String() method.
var stringProducers = map[string]bool{
	"Sprint": true, "Sprintf": true, "Sprintln": true,
	"Join": true, "Repeat": true, "String": true,
	"ToLower": true, "ToUpper": true, "TrimSpace": true,
}

func producesString(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.BasicLit:
		return e.Kind == token.STRING
	case *ast.BinaryExpr:
		return e.Op == token.ADD && (producesString(e.X) || producesString(e.Y))
	case *ast.CallExpr:
		switch fun := e.Fun.(type) {
		case *ast.Ident:
			return fun.Name == "string" // a conversion
		case *ast.SelectorExpr:
			return stringProducers[fun.Sel.Name]
		}
	}
	return false
}

// rootIdent unwraps a selector or call chain down to the identifier it starts
// from, so `s.widgets.GetTx(...)` reports `s` and `tx.Where(...).Find` reports
// `tx`.
func rootIdent(e ast.Expr) string {
	for {
		switch x := e.(type) {
		case *ast.Ident:
			return x.Name
		case *ast.SelectorExpr:
			e = x.X
		case *ast.CallExpr:
			e = x.Fun
		case *ast.IndexExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		default:
			return ""
		}
	}
}

// -- Statement rules ----------------------------------------------------------

// emptyBranches reports an `if` with an empty body and no else. It is Go's
// version of the vendor's "empty catch": most of them are `if err != nil {}`,
// a condition someone meant to handle and did not.
func (idx *Index) emptyBranches() []Finding {
	return idx.perFunc(func(f *File, element string, fd *ast.FuncDecl) []Finding {
		var out []Finding
		ast.Inspect(fd, func(n ast.Node) bool {
			s, ok := n.(*ast.IfStmt)
			if ok && s.Else == nil && len(s.Body.List) == 0 {
				out = append(out, idx.finding(RuleEmptyBranch, f, s.Pos(), element,
					"if statement with an empty body and no else; the condition is tested and then ignored"))
			}
			return true
		})
		return out
	})
}

// debugStatements reports writes to stdout and to the standard `log` package.
// The API logs through slog; anything reaching for fmt.Println is a debug print
// that outlived its debugging session.
func (idx *Index) debugStatements() []Finding {
	var out []Finding
	for _, f := range idx.Files {
		if idx.Cfg.excludedFrom(idx.Cfg.DebugStatementExcludeDirs, f.Rel) {
			continue
		}
		stdlog := f.Imports()["log"] == "log"
		for _, d := range f.AST.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			element := idx.element(fd)
			ast.Inspect(fd, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := c.Fun.(type) {
				case *ast.Ident:
					if fun.Name == "print" || fun.Name == "println" {
						out = append(out, idx.finding(RuleDebugStatement, f, c.Pos(), element,
							"builtin "+fun.Name+"() writes to stderr and is not a logger"))
					}
				case *ast.SelectorExpr:
					pkg, ok := fun.X.(*ast.Ident)
					if !ok {
						return true
					}
					switch {
					case pkg.Name == "fmt" && strings.HasPrefix(fun.Sel.Name, "Print"):
						out = append(out, idx.finding(RuleDebugStatement, f, c.Pos(), element,
							"fmt."+fun.Sel.Name+"() writes to stdout instead of the logger"))
					case pkg.Name == "log" && stdlog:
						out = append(out, idx.finding(RuleDebugStatement, f, c.Pos(), element,
							"standard log."+fun.Sel.Name+"() bypasses the structured slog logger"))
					}
				}
				return true
			})
		}
	}
	return out
}

// unparen strips redundant parentheses from an expression. The module carries no
// dependencies on purpose, so this stands in for astutil.Unparen.
func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// clauseEnd is where the switch clause at index i stops, which is the next
// clause's `case` keyword or, for the last one, the switch body's closing brace.
//
// CaseClause.End() cannot answer this: for an empty-bodied arm it is the colon
// itself, so the comment that arm carries falls outside its own node.
func clauseEnd(body *ast.BlockStmt, i int) token.Pos {
	if i+1 < len(body.List) {
		return body.List[i+1].Pos()
	}
	return body.Rbrace
}

// commentIn reports whether the file has a comment between from and to.
func (f *File) commentIn(from, to token.Pos) bool {
	for _, group := range f.AST.Comments {
		if group.Pos() > from && group.End() <= to {
			return true
		}
	}
	return false
}

// switchWithoutDefault reports a switch with no default clause: when a new case
// value appears, such a switch falls through silently rather than failing.
//
// Three shapes are not that, and each is skipped below.
//
// A tagless switch is an if-ladder. A switch on a call result -- `switch
// len(names)` -- is not switching over a vocabulary at all, so no new case value
// can arrive and the statements after the switch are its default. And a switch
// with an arm that names values, does nothing with them, and CARRIES A COMMENT
// has had the fall-through decided in writing: the author listed the values that
// reach the code below and wrote down why, which is the pattern `exhaustive`
// rewards and a default would undo. The comment is what separates that from a
// bare stub arm, which still reports.
//
// What is left is the shape worth reading: every arm does something, and a value
// nobody named reaches none of them. Whether that is a defect still depends on
// whether the tag's value set is closed, which needs the types this scanner
// deliberately does without -- so this is a review rule, and its verdicts live in
// accepted.json alongside the other two.
func (idx *Index) switchWithoutDefault() []Finding {
	return idx.perFunc(func(f *File, element string, fd *ast.FuncDecl) []Finding {
		var out []Finding
		ast.Inspect(fd, func(n ast.Node) bool {
			var body *ast.BlockStmt
			kind := ""
			switch n := n.(type) {
			case *ast.SwitchStmt:
				body, kind = n.Body, "switch"
				// A tagless `switch { case cond: }` is an if-ladder in switch
				// clothing, and an if-ladder needs no else.
				if n.Tag == nil {
					return true
				}
				// `switch len(names)` or `switch f()` switches over a computed
				// result, not over a vocabulary. Nothing can be "added" to it, so
				// the silent-fall-through story does not apply; the statements
				// after the switch are its default.
				if _, call := unparen(n.Tag).(*ast.CallExpr); call {
					return true
				}
			case *ast.TypeSwitchStmt:
				body, kind = n.Body, "type switch"
			default:
				return true
			}
			for i, stmt := range body.List {
				c, ok := stmt.(*ast.CaseClause)
				if !ok {
					continue
				}
				if c.List == nil {
					return true // this is the default clause
				}
				// An arm that names values, does nothing with them, and says why
				// is the fall-through, stated. See the doc comment.
				if len(c.Body) == 0 && f.commentIn(c.Colon, clauseEnd(body, i)) {
					return true
				}
			}
			out = append(out, idx.finding(RuleSwitchWithoutDefault, f, n.Pos(), element,
				kind+" has no default clause; an unhandled value passes through silently"))
			return true
		})
		return out
	})
}

// missingDocComments reports an exported package-level declaration with no doc
// comment. Unexported declarations and methods on unexported types are out of
// scope: the rule is about the surface other packages have to use.
func (idx *Index) missingDocComments() []Finding {
	var out []Finding
	for _, f := range idx.Files {
		base := path.Base(f.Rel)
		for _, d := range f.AST.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() || d.Doc != nil || !exportedReceiver(d) {
					continue
				}
				out = append(out, idx.finding(RuleMissingDoc, f, d.Pos(), idx.element(d),
					"exported "+funcKind(d)+" has no doc comment"))
			case *ast.GenDecl:
				if d.Tok == token.IMPORT {
					continue
				}
				for _, spec := range d.Specs {
					name, doc := specDoc(spec)
					if name == nil || !name.IsExported() || doc != nil || d.Doc != nil {
						continue
					}
					out = append(out, idx.finding(RuleMissingDoc, f, spec.Pos(), name.Name,
						fmt.Sprintf("exported %s %s in %s has no doc comment", d.Tok, name.Name, base)))
				}
			}
		}
	}
	return out
}

func exportedReceiver(fd *ast.FuncDecl) bool {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return true
	}
	t := fd.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	// A generic receiver is `Type[T]`; the name is on the left.
	if idx, ok := t.(*ast.IndexExpr); ok {
		t = idx.X
	}
	id, ok := t.(*ast.Ident)
	return ok && id.IsExported()
}

func funcKind(fd *ast.FuncDecl) string {
	if fd.Recv != nil {
		return "method"
	}
	return "function"
}

func specDoc(spec ast.Spec) (*ast.Ident, *ast.CommentGroup) {
	if spec == nil {
		return nil, nil
	}
	switch s := spec.(type) {
	case *ast.TypeSpec:
		return s.Name, s.Doc
	case *ast.ValueSpec:
		if len(s.Names) > 0 {
			return s.Names[0], s.Doc
		}
	}
	return nil, nil
}

// perFunc runs a per-function rule over every function in the module.
func (idx *Index) perFunc(rule func(*File, string, *ast.FuncDecl) []Finding) []Finding {
	var out []Finding
	for _, f := range idx.Files {
		for _, d := range f.AST.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			out = append(out, rule(f, idx.element(fd), fd)...)
		}
	}
	return out
}

// -- Comment rules ------------------------------------------------------------

var (
	unfinishedPattern = regexp.MustCompile(`\b(TODO|FIXME|XXX|HACK)\b`)
	suppressPattern   = regexp.MustCompile(`^//\s*(nolint|lint:ignore)\b`)

	// A comment is only considered for the commented-out-code rule if it opens
	// like a statement rather than a sentence. Every alternative is anchored:
	// an unanchored `\w\(` matched the SQL fragments quoted in doc comments --
	// `COALESCE(a, b, c)` and `numeric(10,4)` both parse as Go call
	// expressions, which is how they became the rule's only two findings
	// before this was tightened. A qualified call needs a dot before the
	// paren, which is what separates `d.String(...)` from `numeric(...)`.
	codeShapePattern = regexp.MustCompile(
		`:=` +
			`|^(if|for|switch|select|return|var|const|go|defer|func|case|break|continue)\b` +
			`|^[}{]` +
			`|\{$` +
			`|^\w+(\.\w+)+\(` +
			// A plain assignment counts only when its right-hand side is
			// itself code -- a literal, a call, an index. `x = foo.Bar()` is
			// code; `// empty = any`, on a struct field, is a sentence.
			`|^\w+ = ("|` + "`" + `|\w+[.(\[])`)
)

// commentRules reports the three things comments carry that a scan should
// surface: unfinished work, suppressed lint warnings, and code that was
// commented out instead of deleted.
func (idx *Index) commentRules() []Finding {
	var out []Finding
	for _, f := range idx.Files {
		decls := idx.declRanges(f)
		for _, group := range f.AST.Comments {
			element := enclosingElement(decls, group.Pos(), path.Base(f.Rel))

			for _, c := range group.List {
				if unfinishedPattern.MatchString(c.Text) {
					out = append(out, idx.finding(RuleUnfinishedWork, f, c.Pos(), element,
						trimComment(c.Text)))
				}
				if suppressPattern.MatchString(c.Text) {
					out = append(out, idx.finding(RuleSuppressedWarning, f, c.Pos(), element,
						"lint suppression: "+trimComment(c.Text)))
				}
			}

			// Commented-out code is looked for across the whole group first: a
			// block that was commented out line by line only parses when its
			// lines are put back together, and reporting it once at the top is
			// what someone deleting it wants to see.
			if body, ok := commentedOutBlock(group); ok {
				out = append(out, idx.finding(RuleCommentedOutCode, f, group.Pos(), element,
					"commented-out code: "+body))
				continue
			}
			for _, c := range group.List {
				if isCommentedOutCode(c.Text) {
					out = append(out, idx.finding(RuleCommentedOutCode, f, c.Pos(), element,
						"commented-out code: "+trimComment(c.Text)))
				}
			}
		}
	}
	return out
}

// commentedOutBlock reports whether a whole comment group is a block of source
// that was commented out, and summarises it. A group only qualifies if every
// one of its lines clears the same bars a single line has to clear and the
// lines parse together as a function body -- so `if ... {` / `return err` / `}`
// is caught as one finding, while a doc comment that quotes a call is not
// caught at all.
func commentedOutBlock(group *ast.CommentGroup) (string, bool) {
	if len(group.List) < 2 {
		return "", false
	}
	var lines []string
	shaped := false
	for _, c := range group.List {
		body, ok := commentBody(c.Text)
		if !ok {
			return "", false
		}
		if codeShapePattern.MatchString(body) {
			shaped = true
		}
		lines = append(lines, body)
	}
	if !shaped || !parsesAsBody(strings.Join(lines, "\n")) {
		return "", false
	}
	summary := strings.Join(lines, " ")
	if len(summary) > 90 {
		summary = summary[:87] + "..."
	}
	return summary, true
}

// isCommentedOutCode decides whether a `//` comment is source that was
// commented out rather than prose. It has to clear three bars: it must not be
// an indented block (godoc renders those as code samples on purpose, and they
// are meant to be there), it must look like a statement, and it must actually
// parse as one. Prose that happens to mention `if err != nil` fails the third.
func isCommentedOutCode(text string) bool {
	body, ok := commentBody(text)
	return ok && codeShapePattern.MatchString(body) && parsesAsBody(body)
}

// commentBody strips the `//` from a line comment and rejects the kinds of
// comment that are never commented-out code: block comments, compiler
// directives, and the indented blocks godoc renders as deliberate code samples.
func commentBody(text string) (string, bool) {
	if !strings.HasPrefix(text, "//") {
		return "", false
	}
	body := text[2:]
	if strings.HasPrefix(body, "\t") || strings.HasPrefix(body, "    ") {
		return "", false // a godoc code sample
	}
	body = strings.TrimSpace(body)
	if body == "" || strings.HasPrefix(body, "go:") || strings.HasPrefix(body, "nolint") {
		return "", false
	}
	return body, true
}

func parsesAsBody(src string) bool {
	_, err := parser.ParseFile(token.NewFileSet(), "c.go",
		"package p\nfunc _() {\n"+src+"\n}\n", parser.SkipObjectResolution)
	return err == nil
}

func trimComment(text string) string {
	text = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(text, "//"), "/*"))
	text = strings.TrimSpace(strings.TrimSuffix(text, "*/"))
	if len(text) > 90 {
		return text[:87] + "..."
	}
	return text
}

type declRange struct {
	start, end token.Pos
	element    string
}

// declRanges maps the source spans of a file's declarations to the names to
// report them under, so a comment inside a struct is attributed to the struct
// rather than to the file.
func (idx *Index) declRanges(f *File) []declRange {
	var out []declRange
	for _, d := range f.AST.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			out = append(out, declRange{d.Pos(), d.End(), idx.element(d)})
		case *ast.GenDecl:
			if name, _ := specDoc(firstSpec(d)); name != nil {
				out = append(out, declRange{d.Pos(), d.End(), name.Name})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

func firstSpec(d *ast.GenDecl) ast.Spec {
	if len(d.Specs) == 0 {
		return nil
	}
	return d.Specs[0]
}

// enclosingElement returns the innermost declaration containing pos. Ranges are
// sorted by start, so the last match is the innermost one.
func enclosingElement(decls []declRange, pos token.Pos, fallback string) string {
	out := fallback
	for _, r := range decls {
		if pos >= r.start && pos < r.end {
			out = r.element
		}
	}
	return out
}

func contains(list []string, s string) bool {
	return s != "" && slices.Contains(list, s)
}

func hasAnySuffix(s string, suffixes []string) bool {
	for _, suf := range suffixes {
		if suf != "" && s != suf && strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}
