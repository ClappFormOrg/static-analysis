# static-analysis

Two repo-quality scanners, neither part of any shipped product.

| Tool | Language | What it answers |
| --- | --- | --- |
| [`qualityscan`](qualityscan/) | Go | Maintainability over a Go module: function size, parameter count, cognitive complexity, dependency volume and span, directory reference cycles, duplication, hardcoded URLs and paths, plus the SIG maintainability profile. Ships four gate commands beside it. |
| [`dupescan`](dupescan/) | Node | One concept **declared twice** across files, over TypeScript and Go: a symbol exported by two modules, one string value behind two constants, one shape restated elsewhere. |

Both are stdlib-only and read their input as source text rather than building it,
so they run on a branch that does not compile, in about a second, with nothing to
install beyond the tool itself.

Neither ships a judgement about any particular codebase. Defaults are the ones
that hold for any Go or TypeScript tree; thresholds, acceptances, component
boundaries and which directories to skip arrive as flags, from files the
consuming repository owns. Two of `qualityscan`'s bands are the exception, and
they are named below.

## Consuming this

### The Go commands

Five binaries, installed and pinned the way `golangci-lint` is:

```
go install github.com/ClappFormOrg/static-analysis/qualityscan@v0.1.0
go install github.com/ClappFormOrg/static-analysis/qualityscan/cmd/covergate@v0.1.0
go install github.com/ClappFormOrg/static-analysis/qualityscan/cmd/ratchetpatch@v0.1.0
go install github.com/ClappFormOrg/static-analysis/qualityscan/cmd/toolgate@v0.1.0
go install github.com/ClappFormOrg/static-analysis/qualityscan/cmd/crosscheck@v0.1.0
```

**Pin, and pin in one file.** The thresholds are part of the tool, so an
unpinned install changes what a gate measures between runs, and a version spelled
in the build and again in a workflow drifts in two directions. Hold it in a
single file the way `toolgate` expects a linter pin to be held, and read it from
there everywhere.

A module rather than an image, deliberately. Three of the five commands cannot
work from inside a container: `ratchetpatch` shells out to `git diff` against a
merge-base and needs real history, `toolgate` interrogates `golangci-lint`,
`actionlint` and `govulncheck` on the host `PATH` and would otherwise report the
container's versions, and `covergate` reads a profile the host produced. An image
serving only the reporting formats is a reasonable addition; one running
everything is not.

### The Node script

`dupescan` is one `.mjs` file with no dependencies, and the module cache is
enough to reach it at a pinned version, with no vendored copy and nothing to
fetch that `go install` above does not already fetch:

```
go mod download github.com/ClappFormOrg/static-analysis@v0.1.0
DUPESCAN="$(go list -m -f '{{.Dir}}' github.com/ClappFormOrg/static-analysis@v0.1.0)/dupescan/dupescan.mjs"
node "$DUPESCAN" --config dupescan.json
```

The module cache is read-only, which is what makes this safe to run from: the
copy cannot drift from the tag.

## Releases

Tags are plain `vX.Y.Z` off `main`; the module is at the repository root, so a
consumer pins one version for all five commands and the script.

A release that moves a default band, adds a rule, or changes what a rule measures
is a change to what every consumer's gate reports, whatever the version number
says. Say so in the release notes, and expect a consumer to re-measure before
taking it.

## Calibration: two bands were fitted against one codebase

Most of `qualityscan`'s bands are the conventional published numbers for these
metrics. Two were fitted against **one large Go codebase**, the first one the
scanner measured:

- **`dependency_span`** is rescaled by about a quarter from the published 30/40,
  because span at package granularity runs below the scale those are set for.
  The rescaling came from that codebase's own distribution.
- **`complexity`** is anchored to `gocognit`'s own `min-complexity` rather than
  to the published 30/50, which are set for "function nesting complexity", a
  different metric whose numbers do not transfer.

Both are reasonable starting points for another Go codebase and neither is a
measurement of one. **Re-fitting takes no code change:** dump the raw
measurements with `qualityscan -root <module> -format metrics -out metrics.csv`,
pick the cut that puts the intended share of rows above it -- a conventional
scale puts roughly the worst 5% in HIGH and the worst 1% in VERY HIGH -- and
write the two bands into the repository's `-config` file. Absent fields keep
their default.

Leaving them where they are is also a defensible choice, as long as it is a
choice: a band that reports nothing looks exactly like a codebase with nothing to
report.

[`qualityscan/README.md`](qualityscan/README.md) documents what every rule
measures, the thresholds, and the SIG properties the profile reports. It carries
no measurements of any particular codebase: the figures a scan produces belong to
whoever ran it.

## Working on the tools

```
go test ./...                                              # the Go suites
go vet ./...
node --test dupescan/dupescan.test.mjs                     # the Node suite

go test ./... -covermode=atomic -coverprofile=cover.out
go run ./qualityscan/cmd/covergate -dir . -profile cover.out -floor qualityscan/.coverage-floor -gate
node --experimental-test-coverage --test-coverage-lines="$(grep -v '^#' dupescan/.coverage-floor | tr -d '[:space:]')" \
  --test dupescan/dupescan.test.mjs
```

CI runs exactly those. Each tool's floor lives in its own `.coverage-floor`, with
the number and the reasoning behind it in the same file.

`QUALITYSCAN_CONFIG_DIR` points the five config-validating tests at a consuming
repository's config directory. Unset, they skip: a checkout of this repository has
no config of its own to validate, and the coverage floor is measured with them
skipping so the number a bare checkout reads is the number the gate holds.
