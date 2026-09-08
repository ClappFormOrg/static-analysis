package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The banners below are verbatim output from the two binaries this gate was
// written against, so a change in either tool's wording fails a test here
// rather than silently turning a gate into a no-op.
const (
	// The pinned build, installed by `make lint-api-install`.
	golangciPinned = "golangci-lint has version 2.12.2 built with go1.26.6 from " +
		`(unknown, modified: ?, mod sum: "h1:7+d1uY0bq1MU2UV3R5pW5Q7QWdcoq4naMRXM+gsJKrs=") on (unknown)` + "\n"

	// The chocolatey package, which resolves ahead of ~/go/bin on PATH and is
	// the reason this command exists.
	golangciChocolatey = "golangci-lint has version 2.11.3 built with go1.26.1 from 6008b81b on 2026-03-10T10:25:44Z\n"

	govulncheckBanner = "Go: go1.26.6\n" +
		"Scanner: govulncheck@v1.7.0\n" +
		"DB: https://vuln.go.dev\n" +
		"DB updated: 2026-08-19 17:06:06 +0000 UTC\n"
)

// ─── golangci-lint's banner ───────────────────────────────────────────────

func TestParseGolangciVersion(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want string
	}{
		{"pinned build", golangciPinned, "2.12.2"},
		{"chocolatey build", golangciChocolatey, "2.11.3"},
		{"leading v, should a future release add one", "golangci-lint has version v2.13.0 built with go1.27.0\n", "v2.13.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseGolangciVersion(tc.out)
			if err != nil {
				t.Fatalf("parseGolangciVersion: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestParseGolangciVersionRejectsUnreadableOutput keeps an unparseable banner an
// error rather than an empty string. An empty string would compare unequal to
// the pin and so still fail the gate, but it would fail it with a version
// mismatch message naming no version, which sends the reader looking for a
// second install that is not there.
func TestParseGolangciVersionRejectsUnreadableOutput(t *testing.T) {
	for _, tc := range []struct{ name, out string }{
		{"empty", ""},
		{"a different tool entirely", "usage: golangci-lint <command>\n"},
		{"the words but no number", "golangci-lint has version\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := parseGolangciVersion(tc.out); err == nil {
				t.Fatalf("parsed %q out of %q, want an error", got, tc.out)
			}
		})
	}
}

// TestPinSatisfiedIgnoresTheModuleVersionPrefix pins the one normalisation the
// comparison does. The pin file holds what `go install ...@` consumes (v2.12.2)
// and the binary reports the same version without the v, so a literal compare
// would fail the gate on a correctly installed linter.
func TestPinSatisfiedIgnoresTheModuleVersionPrefix(t *testing.T) {
	for _, tc := range []struct {
		name      string
		got, want string
		ok        bool
	}{
		{"reported bare, pinned with v", "2.12.2", "v2.12.2", true},
		{"both bare", "2.12.2", "2.12.2", true},
		{"both with v", "v2.12.2", "v2.12.2", true},
		{"pin file left a trailing newline", "2.12.2", "v2.12.2\n", true},
		{"behind the pin", "2.11.3", "v2.12.2", false},
		{"ahead of the pin", "2.13.0", "v2.12.2", false},
		{"same minor, different patch", "2.12.1", "v2.12.2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pinSatisfied(tc.got, tc.want); got != tc.ok {
				t.Errorf("pinSatisfied(%q, %q) = %v, want %v", tc.got, tc.want, got, tc.ok)
			}
		})
	}
}

// ─── The pin file ─────────────────────────────────────────────────────────

func TestReadPin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    string
	}{
		{"one line", "v2.12.2\n", "v2.12.2"},
		{"no trailing newline", "v2.12.2", "v2.12.2"},
		{"CRLF, from a checkout that ignored .gitattributes", "v2.12.2\r\n", "v2.12.2"},
		{"surrounding blank lines", "\n\nv2.12.2\n\n", "v2.12.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readPin(writePin(t, tc.content))
			if err != nil {
				t.Fatalf("readPin: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestReadPinRejectsAnythingButAVersion is the grammar half of the single-source
// claim. The Makefile and both workflows read this file with `tr -d`, which
// would splice a comment or a second entry straight into a `go install` argument
// and produce a confusing install failure. Rejecting here means the file cannot
// grow a shape only one of the four readers understands.
func TestReadPinRejectsAnythingButAVersion(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"empty", ""},
		{"whitespace only", "\n  \n"},
		{"a comment above the version", "# bump with ci.yml\nv2.12.2\n"},
		{"two versions", "v2.12.2 v2.11.3\n"},
		{"a version and a trailing word", "v2.12.2 # pinned\n"},
		{"two lines", "v2.12.2\nv2.11.3\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := readPin(writePin(t, tc.content)); err == nil {
				t.Fatalf("read %q out of %q, want an error", got, tc.content)
			}
		})
	}
}

func TestReadPinReportsAMissingFile(t *testing.T) {
	if _, err := readPin(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("readPin on a missing file returned no error")
	}
}

func writePin(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".golangci-version")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write pin fixture: %v", err)
	}
	return path
}

// ─── actionlint's banner ──────────────────────────────────────────────────

// actionlintBanner is what `actionlint -version` prints for a build from
// source. A downloaded release prints the same first line.
const actionlintBanner = `1.7.7
installed by building from source
built with go1.26.6 compiler for linux/amd64
`

func TestParseActionlintVersion(t *testing.T) {
	got, err := parseActionlintVersion(actionlintBanner)
	if err != nil {
		t.Fatalf("parseActionlintVersion: %v", err)
	}
	if got != "1.7.7" {
		t.Errorf("got %q; want 1.7.7", got)
	}
}

func TestParseActionlintVersionReadsTheFirstLineAndNotTheToolchain(t *testing.T) {
	// The banner's third line carries a second version-shaped token, go1.26.6.
	// Reading the file rather than the first line would report the compiler.
	got, err := parseActionlintVersion(actionlintBanner)
	if err != nil {
		t.Fatalf("parseActionlintVersion: %v", err)
	}
	if strings.HasPrefix(got, "go") {
		t.Errorf("got %q, which is the compiler and not actionlint's version", got)
	}
}

func TestParseActionlintVersionAcceptsTheVPrefix(t *testing.T) {
	// The pin file spells the module form. A binary printing it too must not be
	// rejected as unreadable before pinSatisfied gets to normalise either side.
	got, err := parseActionlintVersion("v1.7.7\n")
	if err != nil {
		t.Fatalf("parseActionlintVersion: %v", err)
	}
	if got != "v1.7.7" {
		t.Errorf("got %q; want v1.7.7", got)
	}
}

func TestParseActionlintVersionRejectsUnreadableOutput(t *testing.T) {
	// A binary that is not actionlint answers this flag with something else, and
	// the failure has to name that rather than compare prose against the pin.
	cases := map[string]string{
		"a usage banner": "Usage: actionlint [FLAGS] [FILES]\n",
		"an error":       "actionlint: unknown flag -version\n",
		"nothing":        "",
		"only blanks":    "\n\n",
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseActionlintVersion(out); err == nil {
				t.Fatalf("parseActionlintVersion(%q) returned no error", out)
			}
		})
	}
}

// ─── govulncheck's banner ─────────────────────────────────────────────────

func TestParseGovulncheckGo(t *testing.T) {
	got, err := parseGovulncheckGo(govulncheckBanner)
	if err != nil {
		t.Fatalf("parseGovulncheckGo: %v", err)
	}
	if got != "go1.26.6" {
		t.Errorf("got %q, want %q", got, "go1.26.6")
	}
}

// TestParseGovulncheckGoReadsTheGoLineAndNotTheScanner guards the line the
// comparison depends on. The scanner's own version sits directly beneath the Go
// line and moves independently of it, and reading that one instead would compare
// govulncheck@v1.7.0 against a go directive and reject every install.
func TestParseGovulncheckGoReadsTheGoLineAndNotTheScanner(t *testing.T) {
	out := "Scanner: govulncheck@v1.7.0\nGo: go1.23.3\nDB: https://vuln.go.dev\n"
	got, err := parseGovulncheckGo(out)
	if err != nil {
		t.Fatalf("parseGovulncheckGo: %v", err)
	}
	if got != "go1.23.3" {
		t.Errorf("got %q, want %q", got, "go1.23.3")
	}
}

// TestParseGovulncheckGoRejectsUnreadableOutput matters more here than it does
// for the linter: govulncheck's failure mode is reporting success, so a banner
// this command cannot read must stop the scan rather than wave it through.
func TestParseGovulncheckGoRejectsUnreadableOutput(t *testing.T) {
	for _, tc := range []struct{ name, out string }{
		{"empty", ""},
		{"no Go line", "Scanner: govulncheck@v1.7.0\nDB: https://vuln.go.dev\n"},
		{"unreadable version", "Go: not-a-version\n"},
		{"Go line with nothing after it", "Go:\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := parseGovulncheckGo(tc.out); err == nil {
				t.Fatalf("parsed %q out of %q, want an error", got, tc.out)
			}
		})
	}
}

// ─── The go directive ─────────────────────────────────────────────────────

func TestReadGoDirective(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "directive only",
			content: "module example.com/m\n\ngo 1.26.6\n",
			want:    "go1.26.6",
		},
		{
			// The toolchain line is the trap: it is spelled like the directive,
			// it sits next to it, and it can name a different version. The
			// directive is what govulncheck's analysis is measured against.
			name:    "toolchain line beneath the directive",
			content: "module example.com/m\n\ngo 1.26.6\n\ntoolchain go1.27.1\n",
			want:    "go1.26.6",
		},
		{
			name:    "two-component directive",
			content: "module example.com/m\n\ngo 1.26\n",
			want:    "go1.26",
		},
		{
			name:    "tabs after the keyword",
			content: "module example.com/m\n\ngo\t1.26.6\n",
			want:    "go1.26.6",
		},
		{
			// `go` also appears inside require blocks and in module paths, and
			// only a line that starts with it is the directive.
			name:    "go inside a require block",
			content: "module example.com/m\n\nrequire (\n\tgo.uber.org/zap v1.27.0\n)\n\ngo 1.26.6\n",
			want:    "go1.26.6",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readGoDirective(writeGoMod(t, tc.content))
			if err != nil {
				t.Fatalf("readGoDirective: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadGoDirectiveRejectsAGoModWithout(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"no directive", "module example.com/m\n"},
		{"only a toolchain line", "module example.com/m\n\ntoolchain go1.27.1\n"},
		{"unreadable version", "module example.com/m\n\ngo 1.2.3.4.5\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := readGoDirective(writeGoMod(t, tc.content)); err == nil {
				t.Fatalf("read %q out of %q, want an error", got, tc.content)
			}
		})
	}
}

// TestReadGoDirectiveReportsAMissingGoMod keeps a mistyped -gomod path an error
// rather than a missing floor. There is nothing to compare a scanner against
// without the directive, and the govulncheck check is the one whose failure mode
// is reporting success, so it must stop rather than run unmeasured.
func TestReadGoDirectiveReportsAMissingGoMod(t *testing.T) {
	_, err := readGoDirective(filepath.Join(t.TempDir(), "go.mod"))
	if err == nil {
		t.Fatal("readGoDirective on a missing go.mod returned no error")
	}
	if !strings.Contains(err.Error(), "go.mod") {
		t.Errorf("error = %q, want it to name the file it could not read", err)
	}
}

func writeGoMod(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "go.mod")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write go.mod fixture: %v", err)
	}
	return path
}

// ─── The floor ────────────────────────────────────────────────────────────

// TestGoSatisfies covers the comparison a string compare gets wrong. The patch
// cases are the ones that matter in practice: go1.26.6 against go1.26.10 is
// exactly the shape a Go release cycle produces, and lexically "10" sorts below
// "6", which would pass a scanner four patches stale.
func TestGoSatisfies(t *testing.T) {
	for _, tc := range []struct {
		name      string
		got, want string
		ok        bool
	}{
		{"equal", "go1.26.6", "go1.26.6", true},
		{"newer patch", "go1.26.7", "go1.26.6", true},
		{"newer minor", "go1.27.0", "go1.26.6", true},
		{"double-digit patch above single digit", "go1.26.10", "go1.26.6", true},
		{"older patch", "go1.26.5", "go1.26.6", false},
		{"much older minor", "go1.23.3", "go1.26.6", false},
		{"single-digit patch below double digit", "go1.26.6", "go1.26.10", false},
		{"release candidate is below its release", "go1.27rc1", "go1.27.0", false},
		{"two-component directive met by its first patch", "go1.26.1", "go1.26", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := goSatisfies(tc.got, tc.want); got != tc.ok {
				t.Errorf("goSatisfies(%q, %q) = %v, want %v", tc.got, tc.want, got, tc.ok)
			}
		})
	}
}
