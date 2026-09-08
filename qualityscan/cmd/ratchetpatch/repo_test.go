package main

// Everything in this file runs against a real git repository built under
// t.TempDir. The half of the tool below codeUnchanged is not a computation over
// strings, it is a set of questions about what `git diff -z` prints and which
// directory the process is standing in, and a fake would only pin this file's
// idea of git's output rather than git's. The repository is small enough that
// the whole file runs in well under a second.

import (
	"bytes"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The three versions of one file that every case below is assembled from:
// the base, the same code carrying a doc comment (the #1637 remediation shape
// this tool exists to let through), and the same signature with a changed body
// (what it must never let through).
const (
	srcPlain = `package p

func Add(a, b int) int {
	return a + b
}
`
	srcCommented = `package p

// Add returns the sum of a and b.
func Add(a, b int) int {
	return a + b
}
`
	srcChanged = `package p

func Add(a, b int) int {
	return a - b
}
`
)

// ─── The fixture repository ───────────────────────────────────────────────

// newRepo writes files into a fresh repository and commits them, returning the
// directory and the revision of that commit -- the merge-base a real run is
// handed.
func newRepo(t *testing.T, files map[string]string) (dir, base string) {
	t.Helper()
	dir = t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")

	// The fixture must not inherit the machine's git configuration. autocrlf is
	// the setting that would actually corrupt a result here: on Windows it
	// rewrites line endings on checkout, and line endings are exactly what the
	// "blank lines and formatting only" judgement is about. The identity is set
	// because a machine that has not configured one cannot commit at all, and
	// signing is turned off because a machine that has cannot commit
	// unattended.
	for _, kv := range [][2]string{
		{"user.name", "ratchetpatch tests"},
		{"user.email", "tests@example.invalid"},
		{"commit.gpgsign", "false"},
		{"core.autocrlf", "false"},
		{"core.safecrlf", "false"},
	} {
		gitIn(t, dir, "config", kv[0], kv[1])
	}

	writeFiles(t, dir, files)
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "base")
	return dir, strings.TrimSpace(gitIn(t, dir, "rev-parse", "HEAD"))
}

// writeFiles lays down (or overwrites) files named with forward slashes,
// creating the directories they need.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out)
}

// ─── changedFiles ─────────────────────────────────────────────────────────

// Each status letter takes a different path through the hand-rolled -z scan,
// and the rename branch is the one that can silently name the wrong file: R and
// C carry two paths, and reading the first would put the pre-rename name into
// the patch -- a path that is not on disk, so golangci-lint would lint nothing
// and the ratchet would pass.
func TestChangedFilesReadsEveryStatusShape(t *testing.T) {
	// Each file gets its own declaration. Identical content would let git pair
	// the delete with the add as a rename, which says more about the fixture
	// than about the scan being tested.
	decl := func(name string) string { return "package p\n\nfunc " + name + "() {}\n" }

	dir, base := newRepo(t, map[string]string{
		"edited.go": decl("Edited"),
		"gone.go":   decl("Gone"),
		"moved.go":  decl("Moved"),
		"steady.go": decl("Steady"),
		"notes.md":  "notes\n",
	})
	t.Chdir(dir)

	writeFiles(t, dir, map[string]string{
		"edited.go": decl("Edited") + "\nfunc Second() {}\n",
		"added.go":  decl("Added"),
	})
	gitIn(t, dir, "add", "added.go")
	gitIn(t, dir, "rm", "-q", "gone.go")
	gitIn(t, dir, "mv", "moved.go", "elsewhere.go")

	got, err := changedFiles(base)
	if err != nil {
		t.Fatalf("changedFiles: %v", err)
	}
	status := map[string]string{}
	for _, c := range got {
		status[c.Path] = c.Status
	}

	want := map[string]string{
		"edited.go": "M",
		"added.go":  "A",
		"gone.go":   "D",
		// git reports this one as R100; only the letter is kept, because the
		// similarity score is not something any caller here asks about.
		"elsewhere.go": "R",
	}
	for path, letter := range want {
		if status[path] != letter {
			t.Errorf("%s reported as %q, want %q", path, status[path], letter)
		}
	}
	// The rename under its source name, and the two files nobody touched, must
	// not be in the list at all. A ratchet judging a file the branch did not
	// change is the failure this whole tool is a response to.
	for _, absent := range []string{"moved.go", "steady.go", "notes.md"} {
		if letter, ok := status[absent]; ok {
			t.Errorf("%s appeared in the change list as %q", absent, letter)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d changes %v, want %d", len(got), status, len(want))
	}
}

// The paths have to come out relative to the directory the tool was pointed at,
// because golangci-lint matches them against the directory IT runs in. A run
// from api/ that emitted repository-root paths would produce a patch matching
// nothing, and a ratchet matching nothing passes everything.
func TestChangedFilesScopesPathsToTheWorkingDirectory(t *testing.T) {
	dir, base := newRepo(t, map[string]string{
		"api/internal/store/widget.go": srcPlain,
		"tools/other.go":               srcPlain,
	})
	writeFiles(t, dir, map[string]string{
		"api/internal/store/widget.go": srcChanged,
		"tools/other.go":               srcChanged,
	})
	t.Chdir(filepath.Join(dir, "api"))

	got, err := changedFiles(base)
	if err != nil {
		t.Fatalf("changedFiles: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %v, want only the file below api/", got)
	}
	if got[0].Path != "internal/store/widget.go" {
		t.Errorf("path = %q, want it stripped of the api/ prefix", got[0].Path)
	}
}

// An unresolvable base is what a mis-set CI variable looks like, and it has to
// stop the run. Reporting no changes instead would produce an empty patch, and
// an empty patch is a ratchet that waves everything through.
func TestChangedFilesReportsAnUnresolvableBase(t *testing.T) {
	dir, _ := newRepo(t, map[string]string{"a.go": srcPlain})
	t.Chdir(dir)

	if got, err := changedFiles("nosuchrev"); err == nil {
		t.Fatalf("changedFiles on an unresolvable revision returned %v and no error", got)
	}
}

// Only a modify has two sides to compare, which is what makes "did the code
// change" answerable. Treating any other status as modified would offer an
// exemption to a file the ratchet has never judged in its current state.
func TestModifiedIsOnlyTheModifyStatus(t *testing.T) {
	for status, want := range map[string]bool{
		"M": true,
		"A": false,
		"D": false,
		"R": false,
		"C": false,
		"T": false,
	} {
		if got := (change{Status: status, Path: "a.go"}).Modified(); got != want {
			t.Errorf("change{Status: %q}.Modified() = %v, want %v", status, got, want)
		}
	}
}

// ─── partition ────────────────────────────────────────────────────────────

// The one exemption partition is allowed to make is a modified .go file whose
// code did not move. Every other row here is a legitimate lookalike that must
// stay in front of the ratchet.
func TestPartitionExemptsOnlyUnchangedGoCode(t *testing.T) {
	dir, base := newRepo(t, map[string]string{
		"doc.go":    srcPlain,
		"code.go":   srcPlain,
		"README.md": "before\n",
		"gone.go":   srcPlain,
	})
	t.Chdir(dir)
	writeFiles(t, dir, map[string]string{
		"doc.go":    srcCommented,
		"code.go":   srcChanged,
		"README.md": "after\n",
		"added.go":  srcCommented,
	})

	keep, exempt, err := partition(base, []change{
		{Status: "M", Path: "doc.go"},
		{Status: "M", Path: "code.go"},
		// Not a .go file: nothing here can parse it, so nothing here can say
		// its content is only comments.
		{Status: "M", Path: "README.md"},
		{Status: "D", Path: "gone.go"},
		// An add has no before side to compare against, whatever it contains.
		{Status: "A", Path: "added.go"},
	})
	if err != nil {
		t.Fatalf("partition: %v", err)
	}

	wantKeep := []string{"code.go", "README.md", "added.go"}
	if !equalStrings(keep, wantKeep) {
		t.Errorf("keep = %v, want %v", keep, wantKeep)
	}
	if len(exempt) != 1 || exempt[0].Path != "doc.go" {
		t.Fatalf("exempt = %v, want only doc.go", exempt)
	}
	if exempt[0].Reason != "comments, blank lines or formatting only" {
		t.Errorf("reason = %q, want the formatting-only reason", exempt[0].Reason)
	}
	// A deleted file belongs on neither list: naming it in the patch sends
	// golangci-lint after a file that is not there, and naming it as an
	// exemption claims a judgement nobody made.
	for _, e := range exempt {
		if e.Path == "gone.go" {
			t.Error("the deleted file was reported as exempt")
		}
	}
}

// A modify over a file with no version at the base is most often a rename git
// declined to detect. There is nothing to compare, so the file is kept -- the
// safe direction -- rather than exempted for want of a before side.
func TestPartitionKeepsAFileWithNoVersionAtTheBase(t *testing.T) {
	dir, base := newRepo(t, map[string]string{"a.go": srcPlain})
	t.Chdir(dir)
	writeFiles(t, dir, map[string]string{"newcomer.go": srcCommented})

	keep, exempt, err := partition(base, []change{{Status: "M", Path: "newcomer.go"}})
	if err != nil {
		t.Fatalf("partition: %v", err)
	}
	if !equalStrings(keep, []string{"newcomer.go"}) {
		t.Errorf("keep = %v, want the file with no base version kept", keep)
	}
	if len(exempt) != 0 {
		t.Errorf("exempt = %v, want nothing exempted on a comparison that could not be made", exempt)
	}
}

// A file that exists at the base and not in the working tree, reported as a
// modify, is a shape the tool cannot reason about, and it has to fail loudly.
// Carrying on would quietly drop the file from both lists and so from the gate.
func TestPartitionFailsWhenTheWorkingTreeCopyIsUnreadable(t *testing.T) {
	dir, base := newRepo(t, map[string]string{"a.go": srcPlain})
	t.Chdir(dir)
	if err := os.Remove(filepath.Join(dir, "a.go")); err != nil {
		t.Fatalf("remove the working tree copy: %v", err)
	}

	_, _, err := partition(base, []change{{Status: "M", Path: "a.go"}})
	if err == nil {
		t.Fatal("partition succeeded with the file missing from the working tree")
	}
	if !strings.Contains(err.Error(), "a.go") {
		t.Errorf("error = %q, want it to name the file it could not read", err)
	}
}

// git reports a modify for a mode or attribute change too, and the two versions
// are then byte-identical. That is still an exemption, but it is reported with
// its own reason: "no content change" and "formatting only" send a reader
// looking in different places.
func TestPartitionNamesTheReasonForAnIdenticalFile(t *testing.T) {
	dir, base := newRepo(t, map[string]string{"a.go": srcPlain})
	t.Chdir(dir)

	keep, exempt, err := partition(base, []change{{Status: "M", Path: "a.go"}})
	if err != nil {
		t.Fatalf("partition: %v", err)
	}
	if len(keep) != 0 {
		t.Errorf("keep = %v, want nothing kept", keep)
	}
	if len(exempt) != 1 || exempt[0].Reason != "no content change" {
		t.Fatalf("exempt = %v, want one entry reading \"no content change\"", exempt)
	}
}

// ─── diffFor ──────────────────────────────────────────────────────────────

// The patch is regenerated from the kept paths, so what has to hold is that it
// covers those and nothing else. A patch that still carried the exempted file
// would put every finding already in that file back in front of the ratchet,
// which is the whole cost this tool exists to remove.
func TestDiffForCoversOnlyTheKeptPaths(t *testing.T) {
	dir, base := newRepo(t, map[string]string{"kept.go": srcPlain, "dropped.go": srcPlain})
	t.Chdir(dir)
	writeFiles(t, dir, map[string]string{"kept.go": srcChanged, "dropped.go": srcChanged})

	patch, err := diffFor(base, []string{"kept.go"})
	if err != nil {
		t.Fatalf("diffFor: %v", err)
	}
	if !strings.Contains(string(patch), "kept.go") {
		t.Errorf("patch does not mention the kept file:\n%s", patch)
	}
	if strings.Contains(string(patch), "dropped.go") {
		t.Errorf("patch still covers the dropped file:\n%s", patch)
	}
}

// With nothing kept there is nothing to ratchet, and the empty patch has to be
// produced without asking git: `git diff <base> --` with an empty pathspec
// diffs the entire tree, so the one case that means "judge no files" would hand
// the gate every changed file instead.
func TestDiffForWithNoPathsDoesNotDiffTheWholeTree(t *testing.T) {
	dir, base := newRepo(t, map[string]string{"a.go": srcPlain})
	t.Chdir(dir)
	writeFiles(t, dir, map[string]string{"a.go": srcChanged})

	patch, err := diffFor(base, nil)
	if err != nil {
		t.Fatalf("diffFor: %v", err)
	}
	if len(patch) != 0 {
		t.Fatalf("an empty keep list produced a %d-byte patch:\n%s", len(patch), patch)
	}
}

// ─── git and gitShow ──────────────────────────────────────────────────────

// gitShow has to read the version at the base and not the file on disk.
// Comparing the working tree against itself would find every file identical and
// exempt the whole branch.
func TestGitShowReadsTheBaseVersionNotTheWorkingTree(t *testing.T) {
	dir, base := newRepo(t, map[string]string{"a.go": srcPlain})
	t.Chdir(dir)
	writeFiles(t, dir, map[string]string{"a.go": srcChanged})

	got, err := gitShow(base, "a.go")
	if err != nil {
		t.Fatalf("gitShow: %v", err)
	}
	if string(got) != srcPlain {
		t.Errorf("gitShow returned:\n%s\nwant the committed version:\n%s", got, srcPlain)
	}
}

// The `rev:./path` spelling is what makes the relative paths --relative
// produces resolvable. `rev:path` is read from the repository root, so a run
// from api/ would look for api/internal/... at the top of the tree, miss, and
// keep every file it could not compare.
func TestGitShowResolvesRelativeToTheWorkingDirectory(t *testing.T) {
	dir, base := newRepo(t, map[string]string{"api/internal/store/widget.go": srcPlain})
	t.Chdir(filepath.Join(dir, "api"))

	got, err := gitShow(base, "internal/store/widget.go")
	if err != nil {
		t.Fatalf("gitShow from a subdirectory: %v", err)
	}
	if string(got) != srcPlain {
		t.Errorf("gitShow returned %q, want the committed source", got)
	}
}

// A git failure has to carry git's own diagnosis. The command runs with stderr
// captured rather than inherited, so an error that dropped it would leave the
// caller holding an exit status and no reason -- and every caller of git here
// returns straight out to the CI log.
func TestGitReportsTheCommandAndItsStderr(t *testing.T) {
	dir, _ := newRepo(t, map[string]string{"a.go": srcPlain})
	t.Chdir(dir)

	out, err := git("show", "nosuchrev:./a.go")
	if err == nil {
		t.Fatalf("git show of an unresolvable revision returned %q and no error", out)
	}
	msg := err.Error()
	if !strings.Contains(msg, "show nosuchrev:./a.go") {
		t.Errorf("error = %q, want it to name the command that failed", msg)
	}
	if !strings.Contains(msg, "fatal:") {
		t.Errorf("error = %q, want git's own stderr in it", msg)
	}
}

// ─── reportExemptions ─────────────────────────────────────────────────────

func TestReportExemptionsNamesEachFileAndItsReason(t *testing.T) {
	out := captureStderr(t, func() {
		reportExemptions([]exemption{
			{Path: "internal/store/order.go", Reason: "comments, blank lines or formatting only"},
			{Path: "internal/store/widget.go", Reason: "no content change"},
		}, 5)
	})
	for _, want := range []string{
		"2 of 5",
		"internal/store/order.go",
		"comments, blank lines or formatting only",
		"internal/store/widget.go",
		"no content change",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
}

// The negative half, and the one that keeps the report worth reading. A branch
// that exempted nothing narrowed nothing, and a "0 of 12 exempt" header on every
// ordinary run is how a reader learns to skip the line that matters.
func TestReportExemptionsSaysNothingWhenNothingWasExempt(t *testing.T) {
	if out := captureStderr(t, func() { reportExemptions(nil, 12) }); out != "" {
		t.Errorf("reported %q for an empty exemption list", out)
	}
}

// ─── run ──────────────────────────────────────────────────────────────────

// The shape the CI step depends on, end to end: the comment-only file is gone
// from the patch, the file whose code moved is still in it, and the narrowing is
// announced rather than done in silence.
func TestRunEmitsAPatchWithoutTheExemptedFile(t *testing.T) {
	dir, base := newRepo(t, map[string]string{"doc.go": srcPlain, "code.go": srcPlain})
	writeFiles(t, dir, map[string]string{"doc.go": srcCommented, "code.go": srcChanged})
	t.Chdir(t.TempDir())

	var err error
	var patch string
	report := captureStderr(t, func() {
		patch = captureStdout(t, func() { err = runWith(t, "-base", base, "-dir", dir) })
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(patch, "code.go") {
		t.Errorf("patch does not cover the file whose code changed:\n%s", patch)
	}
	if strings.Contains(patch, "doc.go") {
		t.Errorf("patch still covers the comment-only file:\n%s", patch)
	}
	if !strings.Contains(report, "doc.go") {
		t.Errorf("stderr does not name the exempted file:\n%s", report)
	}
}

// -out is resolved before the chdir, so a relative path keeps meaning what the
// caller typed. CI writes the patch beside the workspace and then runs
// golangci-lint from api/; resolving it after the chdir would drop the file
// inside api/ where the next step does not look for it.
func TestRunResolvesOutBeforeChangingDirectory(t *testing.T) {
	dir, base := newRepo(t, map[string]string{"code.go": srcPlain})
	writeFiles(t, dir, map[string]string{"code.go": srcChanged})
	work := t.TempDir()
	t.Chdir(work)

	if err := runWith(t, "-base", base, "-dir", dir, "-out", "changed.patch", "-explain=false"); err != nil {
		t.Fatalf("run: %v", err)
	}
	patch, err := os.ReadFile(filepath.Join(work, "changed.patch"))
	if err != nil {
		t.Fatalf("the patch was not written beside the caller: %v", err)
	}
	if !strings.Contains(string(patch), "code.go") {
		t.Errorf("patch does not cover the changed file:\n%s", patch)
	}
	if _, err := os.Stat(filepath.Join(dir, "changed.patch")); err == nil {
		t.Error("the patch landed inside -dir, so a relative -out no longer means what the caller typed")
	}
}

// -explain is what keeps a narrowed gate visible. Off it must be silent, on it
// must name the file it dropped: a gate that shrinks without saying so reads
// exactly like one that is not running.
func TestRunExplainControlsTheExemptionReport(t *testing.T) {
	dir, base := newRepo(t, map[string]string{"doc.go": srcPlain})
	writeFiles(t, dir, map[string]string{"doc.go": srcCommented})

	for _, tc := range []struct {
		name   string
		flag   string
		report bool
	}{
		{"on by default", "", true},
		{"explicitly on", "-explain=true", true},
		{"off", "-explain=false", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := t.TempDir()
			t.Chdir(work)
			args := []string{"-base", base, "-dir", dir, "-out", filepath.Join(work, "p.patch")}
			if tc.flag != "" {
				args = append(args, tc.flag)
			}

			var err error
			out := captureStderr(t, func() { err = runWith(t, args...) })
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := strings.Contains(out, "doc.go"); got != tc.report {
				t.Errorf("named the exempted file = %v, want %v; stderr was %q", got, tc.report, out)
			}
		})
	}
}

// Every failure below has to come back as an error rather than an empty patch.
// A run that reported success having produced nothing would be indistinguishable
// from a branch with no code changes, and the ratchet would pass.
func TestRunReportsFailures(t *testing.T) {
	dir, base := newRepo(t, map[string]string{"a.go": srcPlain})

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "no base",
			args: nil,
			want: "-base",
		},
		{
			// The merge-base a mis-set CI variable produces.
			name: "an unresolvable base",
			args: []string{"-base", "nosuchrev", "-dir", dir},
		},
		{
			name: "a -dir that is not there",
			args: []string{"-base", base, "-dir", filepath.Join(dir, "absent")},
		},
		{
			name: "an -out under a directory that is not there",
			args: []string{"-base", base, "-dir", dir, "-out", filepath.Join(dir, "absent", "p.patch")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())

			var err error
			_ = captureStderr(t, func() {
				_ = captureStdout(t, func() { err = runWith(t, tc.args...) })
			})
			if err == nil {
				t.Fatal("run returned no error")
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// runWith drives run() the way the process does, through os.Args and the
// package-level FlagSet.
func runWith(t *testing.T, args ...string) error {
	t.Helper()
	oldArgs, oldFlags := os.Args, flag.CommandLine
	t.Cleanup(func() { os.Args, flag.CommandLine = oldArgs, oldFlags })

	// run registers its flags on the process-wide FlagSet, so a second call in
	// the same binary would panic on a redefined flag. ContinueOnError rather
	// than the real ExitOnError: a future flag bug should fail a test, not take
	// the test binary down with it.
	flag.CommandLine = flag.NewFlagSet("ratchetpatch", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	os.Args = append([]string{"ratchetpatch"}, args...)
	return run()
}

func captureStdout(t *testing.T, fn func()) string { return capture(t, &os.Stdout, fn) }
func captureStderr(t *testing.T, fn func()) string { return capture(t, &os.Stderr, fn) }

// capture swaps one of the process's standard streams for a pipe while fn runs.
// Those streams are package variables in os, so this is only safe because Go
// runs a package's tests one at a time unless they ask otherwise, and nothing
// here calls t.Parallel. The reader runs in its own goroutine so a report longer
// than the pipe buffer cannot deadlock the test.
func capture(t *testing.T, stream **os.File, fn func()) (out string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	read := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		_ = r.Close()
		read <- buf.String()
	}()

	orig := *stream
	*stream = w
	// Deferred rather than written after fn, so that a t.Fatal inside fn
	// restores the stream on its way out instead of leaving the rest of the
	// package writing into a closed pipe.
	defer func() {
		*stream = orig
		_ = w.Close()
		out = <-read
	}()
	fn()
	return ""
}
