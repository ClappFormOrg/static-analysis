// Package gomarker recognises Go's generated-code marker.
//
// It exists because two things in this module need the same answer and must not
// answer it differently. The coverage gate excludes generated files from its
// denominator; the scan excludes them from the scanned-code boundary. Both are
// "is this file machine-written", and a file that counted as generated for one
// and hand-written for the other would make the two reports disagree about the
// same tree for a reason nobody could see from either of them.
package gomarker

import (
	"bufio"
	"bytes"
	"regexp"
	"strings"
)

// marker is the line every generated Go file is expected to carry, as specified
// at golang.org/s/generatedcode and implemented by go/ast's (unexported)
// generator check. `.*` is deliberately permissive about which tool claims
// authorship; the load-bearing halves are the fixed prefix and the
// `DO NOT EDIT.` suffix.
var marker = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)

// IsGenerated reports whether src carries the generated-code marker in the
// region where it counts: before the first non-comment, non-blank line.
//
// The position requirement is the whole point. A file can perfectly well
// contain the string `// Code generated ... DO NOT EDIT.` inside a function, in
// a test fixture for this very tool, or in a doc comment describing the
// convention, and none of those make the file generated. Reading only the
// preamble means a hand-written file cannot exempt itself from the coverage
// floor, or from the scan, by mentioning the marker in passing.
//
// It reads bytes rather than an AST so the answer is available before a parse,
// which is what lets the scanner drop a generated file without paying to parse
// it. Bytes are also all the coverage gate has: it works from a profile and
// never builds a syntax tree at all.
func IsGenerated(src []byte) bool {
	scanner := bufio.NewScanner(bytes.NewReader(src))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "//") && !strings.HasPrefix(trimmed, "/*") {
			// The package clause, or a build constraint's successor -- either
			// way, the preamble is over.
			return false
		}
		if marker.MatchString(line) {
			return true
		}
	}
	return false
}
