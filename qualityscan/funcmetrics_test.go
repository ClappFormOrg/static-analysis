package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// firstFunc parses a file body and returns its first function declaration.
func firstFunc(t *testing.T, src string) *ast.FuncDecl {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", "package p\n"+src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			return fd
		}
	}
	t.Fatal("no func declaration in source")
	return nil
}

func TestCountTokensIn(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want int
	}{
		// func f ( ) { } -> 6
		{"empty func", "func f() {}", 6},
		// Comments are not tokens, and the newline-inserted semicolon after
		// `x := 1` is not counted either.
		{"comment is free", "func f() {\n// a comment\nx := 1\n_ = x\n}", 6 + 3 + 3},
		{"explicit semicolon counts", "func f() { a(); b() }", 6 + 3 + 1 + 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := countTokensIn([]byte(tc.src)); got != tc.want {
				t.Errorf("countTokensIn(%q) = %d, want %d", tc.src, got, tc.want)
			}
		})
	}
}

func TestCountLOCIn(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want int
	}{
		{"one liner", "func f() {}", 1},
		// The closing `}` carries a real token, so it counts as a line of code.
		{"four lines", "func f() {\nx := 1\n_ = x\n}", 4},
		{"blank lines are free", "func f() {\n\n\nx := 1\n\n}", 3},
		{"comment-only lines are free", "func f() {\n// explain\n// more\nx := 1\n}", 3},
		{"trailing comment counts its line once", "func f() {\nx := 1 // why\n}", 3},
		{"block comment on its own lines is free", "func f() {\n/*\n a\n*/\nx := 1\n}", 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := countLOCIn([]byte(tc.src)); got != tc.want {
				t.Errorf("countLOCIn(%q) = %d, want %d", tc.src, got, tc.want)
			}
		})
	}
}

func TestMccabeComplexity(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want int
	}{
		{"straight line", "func f() { a(); b() }", 1},
		{"one if", "func f() { if c { x() } }", 2},
		// McCabe does not weight nesting, which is the whole difference from
		// cognitive complexity: two ifs are two decisions however deep they sit.
		{"nested if scores as two flat ifs", "func f() { if c { if d { x() } } }", 3},
		{"else costs nothing", "func f() { if c { } else { } }", 2},
		{"else-if is another decision", "func f() { if a { } else if b { } else { } }", 3},
		{"for", "func f() { for i := 0; i < 3; i++ { } }", 2},
		{"range", "func f() { for range xs { } }", 2},
		// Each operator, not each run: the same line that cognitive complexity
		// charges once, McCabe charges twice.
		{"logical operators count individually", "func f() { if a && b && c { } }", 4},
		{"cases count, default does not", "func f() { switch x { case 1: case 2: default: } }", 3},
		{"select clauses count, its default does not", "func f() { select { case <-ch: default: } }", 2},
		{"type switch cases count", "func f() { switch x.(type) { case int: case string: } }", 3},
		// A Go closure is anonymous, so its branches bill to the enclosing named
		// unit. mccabeComplexity documents why the webapp side differs here.
		{"closure branches bill inward", "func f() { g(func() { if c { } }) }", 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mccabeComplexity(firstFunc(t, tc.src)); got != tc.want {
				t.Errorf("mccabeComplexity = %d, want %d\nsrc: %s", got, tc.want, tc.src)
			}
		})
	}
}

// TestMccabeIgnoresBodylessDeclaration keeps an assembly or cgo stub out of the
// profile rather than scoring it 1.
func TestMccabeIgnoresBodylessDeclaration(t *testing.T) {
	if got := mccabeComplexity(firstFunc(t, "func f(n int) int")); got != 0 {
		t.Errorf("mccabeComplexity of a bodyless declaration = %d, want 0", got)
	}
}

func TestCountParams(t *testing.T) {
	tests := []struct {
		name        string
		src         string
		wantNames   int
		wantByGroup int
	}{
		{"none", "func f() {}", 0, 0},
		{"receiver excluded", "func (s *S) f(a int) {}", 1, 1},
		{"grouped names", "func f(a, b, c int) {}", 3, 1},
		{"mixed", "func f(ctx C, a, b int, opt ...O) {}", 4, 3},
		{"unnamed", "func f(int, string) {}", 2, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fd := firstFunc(t, tc.src)
			if got := countParams(fd, false); got != tc.wantNames {
				t.Errorf("countParams(byGroup=false) = %d, want %d", got, tc.wantNames)
			}
			if got := countParams(fd, true); got != tc.wantByGroup {
				t.Errorf("countParams(byGroup=true) = %d, want %d", got, tc.wantByGroup)
			}
		})
	}
}

func TestCognitiveComplexity(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want int
	}{
		{"straight line", "func f() { a(); b() }", 0},
		{"one if", "func f() { if c { x() } }", 1},
		// The nested `if` costs 1 for itself plus 1 for sitting a level deep.
		{"nested if", "func f() { if c { if d { x() } } }", 1 + 2},
		// An `else if` ladder stays flat: 1 + 1 + 1, not 1 + 2 + 3.
		{"else-if ladder", "func f() { if a { } else if b { } else { } }", 3},
		{"logical run", "func f() { if a && b && c { } }", 1 + 1},
		{"two logical runs", "func f() { if a && b || c { } }", 1 + 2},
		// Parentheses end a run, so the inner `||` is charged on its own.
		{"parens split runs", "func f() { if a && (b || c) { } }", 1 + 1 + 1},
		{"loop with nested if", "func f() { for range xs { if c { } } }", 1 + 2},
		{"switch", "func f() { switch x { case 1: case 2: } }", 1},
		{"closure adds nesting", "func f() { g(func() { if c { } }) }", 2},
		{"labelled break", "func f() { L: for { break L } }", 1 + 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := cognitiveComplexity(firstFunc(t, tc.src)); got != tc.want {
				t.Errorf("cognitiveComplexity = %d, want %d\nsrc: %s", got, tc.want, tc.src)
			}
		})
	}
}
