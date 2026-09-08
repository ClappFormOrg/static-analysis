package main

import "testing"

// The whole gate turns on codeUnchanged: a false positive here silently exempts
// a real code change from the ratchet, which is the one failure mode that would
// make this tool worse than not having it. Every case below is a shape that has
// actually shown up in a remediation branch.
func TestCodeUnchanged(t *testing.T) {
	const base = `package p

func Add(a, b int) int {
	return a + b
}
`

	tests := []struct {
		name string
		src  string
		want bool
	}{
		{
			name: "identical",
			src:  base,
			want: true,
		},
		{
			name: "doc comment added -- the #1637 remediation shape",
			src: `package p

// Add returns the sum of a and b.
func Add(a, b int) int {
	return a + b
}
`,
			want: true,
		},
		{
			name: "inline comment added",
			src: `package p

func Add(a, b int) int {
	return a + b // the sum
}
`,
			want: true,
		},
		{
			name: "block comment added",
			src: `package p

/*
Add is a function.
*/
func Add(a, b int) int {
	return a + b
}
`,
			want: true,
		},
		{
			name: "comment removed",
			src: `package p
func Add(a, b int) int {
	return a + b
}
`,
			want: true,
		},
		{
			name: "blank lines and indentation only",
			src:  "package p\n\n\n\nfunc Add(a, b int) int {\n        return a + b\n}\n",
			want: true,
		},
		{
			name: "body changed",
			src: `package p

func Add(a, b int) int {
	return a - b
}
`,
			want: false,
		},
		{
			name: "branch added -- what --whole-files exists to catch",
			src: `package p

func Add(a, b int) int {
	if a < 0 {
		return 0
	}
	return a + b
}
`,
			want: false,
		},
		{
			name: "signature widened",
			src: `package p

func Add(a, b, c int) int {
	return a + b
}
`,
			want: false,
		},
		{
			name: "declaration added",
			src: `package p

func Add(a, b int) int {
	return a + b
}

func Sub(a, b int) int { return a - b }
`,
			want: false,
		},
		{
			name: "nolint added is never comment-only",
			src: `package p

//nolint:gocyclo // pre-existing
func Add(a, b int) int {
	return a + b
}
`,
			want: false,
		},
		{
			name: "nolint without a linter list is still a directive",
			src: `package p

//nolint
func Add(a, b int) int {
	return a + b
}
`,
			want: false,
		},
		{
			name: "go:generate added is never comment-only",
			src: `package p

//go:generate stringer -type=T

func Add(a, b int) int {
	return a + b
}
`,
			want: false,
		},
		{
			name: "unparseable is never exempt",
			src: `package p

func Add(a, b int) int {
`,
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := codeUnchanged([]byte(base), []byte(tc.src))
			if got != tc.want {
				t.Fatalf("codeUnchanged = %v, want %v", got, tc.want)
			}
			// The relation is symmetric: removing a comment must be judged the
			// same way adding one is, or a revert would be gated differently
			// from the change it reverts.
			if back, _ := codeUnchanged([]byte(tc.src), []byte(base)); back != tc.want {
				t.Fatalf("reversed: codeUnchanged = %v, want %v", back, tc.want)
			}
		})
	}
}

// A build tag change alters which files compile, so it is a code change even
// though it is spelled as a comment. Kept separate from the table above because
// it needs a different base: the tag has to be above the package clause.
func TestCodeUnchangedBuildTag(t *testing.T) {
	const without = `package p

func F() {}
`
	const with = `//go:build integration

package p

func F() {}
`
	if got, _ := codeUnchanged([]byte(without), []byte(with)); got {
		t.Fatal("adding a //go:build tag was treated as a comment-only change")
	}
}

// Swapping which linter a //nolint suppresses leaves the number of directives
// unchanged, so the comparison has to look at their text and not just count
// them. This is the shape that would move a suppression off a linter that was
// already quiet and onto one that was failing, and have the gate wave it
// through as a comment edit.
func TestCodeUnchangedSwappedDirective(t *testing.T) {
	const before = `package p

//nolint:gocyclo // pre-existing
func Add(a, b int) int {
	return a + b
}
`
	const after = `package p

//nolint:dupl // pre-existing
func Add(a, b int) int {
	return a + b
}
`
	if got, _ := codeUnchanged([]byte(before), []byte(after)); got {
		t.Error("swapping the suppressed linter was treated as a comment-only change")
	}

	// The legitimate lookalike. The directive set is sorted before it is
	// compared, so moving one directive above another changes nothing the
	// compiler or the linter sees, and must stay exemptible -- reordering the
	// comment block above a declaration is a normal part of the #1637 edit.
	const twoDirectives = `package p

//go:generate stringer -type=T
//nolint:dupl
func Add(a, b int) int {
	return a + b
}
`
	const reordered = `package p

//nolint:dupl
//go:generate stringer -type=T
func Add(a, b int) int {
	return a + b
}
`
	if got, _ := codeUnchanged([]byte(twoDirectives), []byte(reordered)); !got {
		t.Error("reordering two unchanged directives was treated as a code change")
	}
}

// directives has to report failure on a file it cannot parse rather than hand
// back an empty set. An empty set compares equal to another empty set, so a
// silent failure on one side would exempt a file on the strength of a
// comparison that never happened.
func TestDirectivesRejectsAnUnparseableFile(t *testing.T) {
	const good = `package p

//go:build integration

//nolint:dupl
func F() {}
`
	got, ok := directives([]byte(good))
	if !ok {
		t.Fatal("directives failed on a file that parses")
	}
	if len(got) != 2 {
		t.Fatalf("directives = %v, want both the build tag and the nolint", got)
	}

	// The same file with its closing brace removed. Nothing about the
	// directives changed; the parse is what fails.
	if got, ok := directives([]byte("package p\n\n//nolint:dupl\nfunc F() {\n")); ok {
		t.Errorf("directives parsed an unparseable file and returned %v", got)
	}
}

func TestIsDirective(t *testing.T) {
	tests := map[string]bool{
		"//go:build integration": true,
		"//go:generate mockgen":  true,
		"//nolint":               true,
		"//nolint:gocyclo,dupl":  true,
		"//nolint // why":        true,
		"//lint:ignore SA1019":   true,
		"//line foo.go:1":        true, // one of Go's three legacy spaced forms
		"// Add returns a sum.":  false,
		"//Add returns a sum.":   false, // prose without a colon
		"// go:build":            false, // spaced, so not a directive
		"/* go:build */":         false,
		"//":                     false,
		// Uppercase is not in Go's directive name set, which is what keeps
		// ordinary annotations out. A TODO must stay exemptible.
		"//TODO: fix this": false,
		"//FIXME: later":   false,
	}
	for text, want := range tests {
		if got := isDirective(text); got != want {
			t.Errorf("isDirective(%q) = %v, want %v", text, got, want)
		}
	}
}
