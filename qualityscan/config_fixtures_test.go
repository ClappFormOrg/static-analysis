package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The scanner ships no configuration of its own. Acceptances, components,
// deviations and the vendor-parity thresholds are judgements about ONE
// codebase, so they live in the repository being scanned rather than beside
// the code that reads them -- otherwise every consumer of this tool inherits
// another project's verdicts, and the tool cannot be extracted without taking
// them along.
//
// The tests that check those files are still worth running, because a
// hand-edit that breaks the schema should fail a build rather than surface as
// a confusing scan. They find the files through QUALITYSCAN_CONFIG_DIR, which
// the consuming repository sets (this one does, in the test-tools target).
// Unset, they skip: a checkout of the tool on its own has nothing to validate,
// and a skipped test says so where a hardcoded relative path would just fail.
const configDirEnv = "QUALITYSCAN_CONFIG_DIR"

// configDir returns the directory holding the scanned repository's quality
// configuration, or skips the test when the environment does not name one.
func configDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv(configDirEnv)
	if dir == "" {
		t.Skipf("%s is unset; nothing to validate against", configDirEnv)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("%s=%q: %v", configDirEnv, dir, err)
	}
	return dir
}

// configFile resolves one file inside that directory.
func configFile(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(configDir(t), name)
}

// repoRoot is the directory the config directory's refs are written relative
// to. Refs like `reviews/2026-08-08-nested-loop-review.md` are written
// from the repository root because that is where a reader opens them, so the
// parent of the config directory is what resolves them.
func repoRoot(t *testing.T) string {
	t.Helper()
	return filepath.Dir(configDir(t))
}
