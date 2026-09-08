// Command ratchetpatch narrows the maintainability ratchet to the files a
// change actually changed the CODE of.
//
// A ratchet in CI runs a golangci-lint config's budgets with
// `--new-from-rev=<merge-base> --whole-files`, so
// every file a branch touches is judged in full. That flag is correct and
// load-bearing -- adding branches to the body of an existing function is the
// most common way debt gets created, and hunk scoping waves it through, because
// every complexity linter anchors its finding to the `func` line well above the
// hunk.
//
// The cost is that `--whole-files` cannot tell a code change from a comment
// change. Adding one doc comment to internal/store/order.go surfaces the 11
// findings that file already carried, so the edit that pays down
// MISSING_DOC_COMMENT (#1637, 483 findings, ~31% of the scan total) is charged
// for FUNCTION_SIZE_RISK and PARAMETER_RISK debt it did not create. That is not
// the forcing function working, it is the forcing function pointed backwards:
// the cheapest, most mechanical, zero-design-risk class of remediation in the
// backlog is also the one the gate makes most expensive. #1713 and #1715 both
// hit this.
//
// So this tool computes the diff the ratchet should see. A .go file whose only
// change is comments, blank lines or formatting is dropped from the patch; every
// other changed file stays, and is still judged in full. golangci-lint then runs
// with `--new-from-patch` instead of `--new-from-rev`, which is the same ratchet
// over a smaller file set rather than a weaker one.
//
//	cd api && go run <this> -base "$MERGE_BASE" -out changed.patch
//
// Two comment classes are deliberately NOT exempt, because both change what the
// compiler or the linter does:
//
//   - Directives (`//go:build`, `//go:generate`, `//nolint`, `//lint:ignore`).
//     A `//nolint` is the single most consequential comment in a linted file --
//     it deletes a finding -- and SUPPRESSED_WARNING (#1636) is itself one of
//     the tickets. Waving those through would let a change disable a linter with
//     the gate exempting it for being "comment only".
//   - Anything in a file that does not parse. An unparseable file cannot be
//     compared, so it is included and the normal gate applies.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "ratchetpatch: %v\n", err)
		os.Exit(2)
	}
}

func run() error {
	var (
		base    = flag.String("base", "", "revision to diff against (required); the merge-base the ratchet uses")
		out     = flag.String("out", "", "write the patch here instead of stdout")
		dir     = flag.String("dir", "", "run as if from this directory; the paths in the patch come out relative to it")
		explain = flag.Bool("explain", true, "list the exempted files on stderr")
	)
	flag.Parse()

	if *base == "" {
		return fmt.Errorf("-base is required")
	}

	// -out is resolved before the chdir so it keeps meaning what the caller
	// typed, rather than landing inside -dir.
	outPath := *out
	if outPath != "" {
		abs, err := filepath.Abs(outPath)
		if err != nil {
			return err
		}
		outPath = abs
	}

	// The tool lives in the qualityscan module and the module it reports on is
	// api/, so `go run` cannot already be in the right directory. Everything
	// below depends on the working directory: `git diff --relative` strips the
	// prefix against it, and golangci-lint matches those paths against the
	// directory IT runs in, which must be the same one.
	if *dir != "" {
		if err := os.Chdir(*dir); err != nil {
			return err
		}
	}

	changed, err := changedFiles(*base)
	if err != nil {
		return err
	}

	keep, exempt, err := partition(*base, changed)
	if err != nil {
		return err
	}

	if *explain {
		reportExemptions(exempt, len(changed))
	}

	patch, err := diffFor(*base, keep)
	if err != nil {
		return err
	}

	if outPath == "" {
		_, err = os.Stdout.Write(patch)
		return err
	}
	return os.WriteFile(outPath, patch, 0o644)
}

// change is one path from `git diff --name-status`, relative to the working
// directory rather than the repository root, so the paths in the emitted patch
// line up with the directory golangci-lint runs in.
type change struct {
	Status string
	Path   string
}

// Modified reports whether both sides of this change exist, which is the only
// case where "did the code change" is a question worth asking. An add, a delete
// or a rename is never exempt.
func (c change) Modified() bool { return c.Status == "M" }

func changedFiles(base string) ([]change, error) {
	// --relative scopes the output to the current directory and strips the
	// prefix, so running this from api/ yields `internal/store/widget.go` --
	// which is what golangci-lint, also running from api/, will match against.
	//
	// Two-dot `git diff <base>` rather than three-dot `<base>...HEAD`: it
	// compares base to the WORKING TREE, which is what --new-from-rev does, so
	// a local run over uncommitted edits behaves the same as CI over a pushed
	// branch.
	raw, err := git("diff", "--relative", "--name-status", "-z", base)
	if err != nil {
		return nil, err
	}

	// -z gives NUL-terminated fields: status, path, status, path... Renames and
	// copies carry two paths, which is why this is a hand-rolled scan rather
	// than a line split.
	fields := strings.Split(string(raw), "\x00")
	var out []change
	for i := 0; i < len(fields); i++ {
		status := fields[i]
		if status == "" {
			continue
		}
		letter := status[:1]
		if letter == "R" || letter == "C" {
			// <status>\0<from>\0<to>: the destination is what exists now.
			if i+2 >= len(fields) {
				break
			}
			out = append(out, change{Status: letter, Path: fields[i+2]})
			i += 2
			continue
		}
		if i+1 >= len(fields) {
			break
		}
		out = append(out, change{Status: letter, Path: fields[i+1]})
		i++
	}
	return out, nil
}

// exemption records why a file was dropped, so a run can say what it did rather
// than silently shrinking the gate.
type exemption struct {
	Path   string
	Reason string
}

// partition splits the changed files into the ones the ratchet should judge and
// the ones whose code did not move.
func partition(base string, changed []change) (keep []string, exempt []exemption, err error) {
	for _, c := range changed {
		if c.Status == "D" {
			// A deleted file has nothing left to lint, and naming it in the
			// patch makes golangci-lint look for a file that is not there.
			continue
		}
		if !c.Modified() || !strings.HasSuffix(c.Path, ".go") {
			keep = append(keep, c.Path)
			continue
		}

		before, err := gitShow(base, c.Path)
		if err != nil {
			// The path exists on neither side under this name (most often a
			// rename git reported as a plain modify). Nothing to compare.
			keep = append(keep, c.Path)
			continue
		}
		after, err := os.ReadFile(c.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", c.Path, err)
		}

		same, why := codeUnchanged(before, after)
		if same {
			exempt = append(exempt, exemption{Path: c.Path, Reason: why})
			continue
		}
		keep = append(keep, c.Path)
	}
	return keep, exempt, nil
}

// codeUnchanged reports whether two versions of a Go file differ only in
// comments, blank lines or formatting.
//
// The comparison is made on two projections, and BOTH must match:
//
//   - the source printed from an AST parsed without comments, which normalises
//     away every non-directive comment and all whitespace at once; and
//   - the sorted set of directive comments, which the first projection drops
//     and which change what the compiler and the linters do.
//
// A file that fails to parse is never exempt: `false` here means "judge it",
// which is the safe direction.
func codeUnchanged(before, after []byte) (bool, string) {
	beforeCode, ok1 := stripComments(before)
	afterCode, ok2 := stripComments(after)
	if !ok1 || !ok2 {
		return false, ""
	}
	if beforeCode != afterCode {
		return false, ""
	}

	beforeDirectives, ok1 := directives(before)
	afterDirectives, ok2 := directives(after)
	if !ok1 || !ok2 {
		return false, ""
	}
	if !equalStrings(beforeDirectives, afterDirectives) {
		return false, ""
	}

	if bytes.Equal(before, after) {
		// Reachable when git reports a modify for a mode or attribute change.
		return true, "no content change"
	}
	return true, "comments, blank lines or formatting only"
}

// stripComments renders src through the AST with comments discarded. Parsing
// without parser.ParseComments keeps them out of the tree entirely, so the
// printed result is the code alone, normalised to gofmt spacing.
func stripComments(src []byte) (string, bool) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "src.go", src, 0)
	if err != nil {
		return "", false
	}
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, file); err != nil {
		return "", false
	}
	return buf.String(), true
}

// directives returns the directive comments in src, sorted, so the comparison
// is order-insensitive but membership-sensitive.
func directives(src []byte) ([]string, bool) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "src.go", src, parser.ParseComments)
	if err != nil {
		return nil, false
	}
	var out []string
	for _, group := range file.Comments {
		for _, c := range group.List {
			if isDirective(c.Text) {
				out = append(out, c.Text)
			}
		}
	}
	sort.Strings(out)
	return out, true
}

// isDirective reproduces the rule go/ast applies internally (its own
// isDirective is unexported, or this would call it): a directive is a line
// comment matching `//[a-z0-9]+:[a-z0-9]`, plus the three legacy spaced forms
// `//line `, `//extern ` and `//export `.
//
// The lowercase-only name is what keeps ordinary prose out. `//Deprecated: use
// NewThing` is not a directive, because `D` is not in the set -- which matters
// here, since treating an ordinary marker comment as a directive would gate the
// file for a line nobody compiles.
//
// `//nolint` is added on top of Go's rule. Go does not recognise it (it means
// nothing to the compiler) but golangci-lint does, and a `//nolint` deletes a
// finding -- exactly what this tool must not let through unexamined.
func isDirective(text string) bool {
	if !strings.HasPrefix(text, "//") {
		// A /* */ block comment is never a directive.
		return false
	}
	body := text[2:]

	if body == "nolint" || strings.HasPrefix(body, "nolint:") || strings.HasPrefix(body, "nolint ") {
		return true
	}
	if strings.HasPrefix(body, "line ") || strings.HasPrefix(body, "extern ") ||
		strings.HasPrefix(body, "export ") {
		return true
	}

	colon := strings.Index(body, ":")
	if colon <= 0 || colon+1 >= len(body) {
		return false
	}
	for i := 0; i <= colon+1; i++ {
		if i == colon {
			continue
		}
		b := body[i]
		if !('a' <= b && b <= 'z' || '0' <= b && b <= '9') {
			return false
		}
	}
	return true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// diffFor regenerates the diff limited to the kept paths. Regenerating is what
// makes this safe: slicing sections out of an already-rendered patch would mean
// re-implementing git's format, and a patch this tool got subtly wrong would
// silently narrow the gate rather than fail.
//
// With no paths kept there is nothing to ratchet, and `git diff -- ` with an
// empty pathspec would diff EVERYTHING, so the empty patch is returned directly.
func diffFor(base string, paths []string) ([]byte, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	args := append([]string{"diff", "--relative", base, "--"}, paths...)
	return git(args...)
}

func reportExemptions(exempt []exemption, total int) {
	if len(exempt) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "ratchetpatch: %d of %d changed files exempt from the ratchet (code unchanged):\n",
		len(exempt), total)
	for _, e := range exempt {
		fmt.Fprintf(os.Stderr, "  %s (%s)\n", e.Path, e.Reason)
	}
}

func git(args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func gitShow(rev, path string) ([]byte, error) {
	// `rev:./path` resolves the path relative to the working directory, which is
	// what --relative gave us above.
	return git("show", rev+":./"+path)
}
