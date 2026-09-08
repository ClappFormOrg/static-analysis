# qualityscan

A local implementation of the best-practice maintainability checks a commercial
static analysis product runs over a Go tree, so they can be run on any commit,
in CI, and without uploading the source anywhere.

Stdlib only, no configuration of its own, about 0.3s over a 100k-line module. It
reads the tree as source text rather than building it, so it works on a branch
that does not compile.

## Install

```
go install github.com/ClappFormOrg/static-analysis/qualityscan@latest
go install github.com/ClappFormOrg/static-analysis/qualityscan/cmd/covergate@latest
go install github.com/ClappFormOrg/static-analysis/qualityscan/cmd/ratchetpatch@latest
go install github.com/ClappFormOrg/static-analysis/qualityscan/cmd/toolgate@latest
go install github.com/ClappFormOrg/static-analysis/qualityscan/cmd/crosscheck@latest
```

Pin a released version rather than `@latest` in anything that gates a merge: the
thresholds are part of the tool, so an unpinned install changes what a gate
measures between runs. See the root [README](../README.md) for the version a
consuming repository should hold in one file.

## Run

```
qualityscan -root . -format text                    # summary table plus every finding
qualityscan -root . -format csv -out scan.csv       # sectioned CSV, for diffing against another scanner
qualityscan -root . -format markdown -out scan.md   # one markdown write-up
qualityscan -root . -format sigprofile              # the SIG maintainability profile
qualityscan -root . -format metrics -out m.csv      # raw measurements, for re-fitting thresholds
```

`-root` takes one module. A repository with several Go modules runs the scanner
once per module, because the denominators every ratio is built from are counted
over one module's file index.

The judgements about a particular repository arrive as flags, and a consuming
repository keeps them in files of its own:

```
qualityscan -root . -config qualityscan.json -accepted accepted.json -format text
```

## The other four commands

`ratchetpatch` narrows a `golangci-lint --new-from-patch --whole-files` run to
the files a branch changed the *code* of, by emitting the diff with comment-only
`.go` files removed. `--whole-files` cannot tell a doc comment from a branch, so
without it the edit that pays down a doc-comment backlog is charged for every
unrelated finding in the file it touched. Directive comments (`//go:build`,
`//nolint`) count as code, so a change cannot disable a linter and be waved
through as "comment only".

```
ratchetpatch -base <merge-base> -dir . -out .ratchet.patch
```

`covergate` makes a coverage floor expressible on a module that carries
generated code. Generated output sits at 0% and can be a large share of a
module, which drags the raw profile well below what the hand-written half
measures and moves it whenever the generator runs, so nothing can be gated on
it. The command drops every file carrying Go's `Code generated ... DO NOT EDIT.`
marker, reports the remainder by package, and compares the total to a floor held
in one file.

```
covergate -dir . -profile cover.out            # report
covergate -dir . -profile cover.out -gate      # gate on .coverage-floor
```

`toolgate` asks a pinned binary what version it is and fails when that disagrees
with the version a repository pins, so a gate cannot silently run a different
linter than CI does.

```
toolgate -tool golangci-lint -pin .golangci-version
toolgate -tool govulncheck -gomod go.mod
toolgate -tool actionlint -pin .github/.actionlint-version
```

`crosscheck` compares the scanner's unit measurements against
[lizard](https://github.com/terryyin/lizard)'s, so a wrong reading in either
shows up as a disagreement rather than as a number nobody questions. Every
decision this tool makes about what a unit is is pinned by a test, but those
tests are written against the same reading of the model as the code, so a wrong
reading would pass both. A second implementation by different people is the
check that catches it. Expect agreement on the overwhelming majority and a tail
where the difference is explainable; the value is in noticing a *new* divergence
after a change to the measurement.

```
crosscheck -ours sigunits.csv -theirs lizard-go.csv
```

## What the scanner ships, and what the repository does

The scanner ships **no configuration of its own**. Its defaults are the ones
that hold for any Go codebase; every judgement about a particular one is passed
in. That split is what lets the tool be lifted into another project without
carrying the first one's verdicts, and it is worth keeping when adding a
setting: if the right value depends on a repository's layout, naming or history,
its default is "off" and the repository names it.

A consuming repository keeps its answers in files of its own, one per flag, and
is expected to hold them together in a directory rather than scattering them:

| Flag | What it decides |
| --- | --- |
| `-config` | Thresholds and scope: which method-name conventions mean "this is a query", and which directories are excluded from the scan. |
| `-accepted` | Reviewed `NESTED_LOOP` / `SQL_IN_LOOP` / `SWITCH_WITHOUT_DEFAULT` sites, with the verdict and the write-up. |
| `-components` | The top-level component boundary for the SIG properties. |
| `-deviations` | Recorded, argued departures from the SIG caps. |

Two defaults are deliberately empty because they are conventions rather than
facts about Go, and defaulting them on would mismeasure every other repository:

- **`sql_method_suffixes`.** A repository that suffixes every
  transaction-scoped method gets a reliable query marker out of that suffix, and
  the same string is meaningless in a project that does not use it.
- **Test-only packages in `exclude_dirs`.** `include_tests` filters by
  filename, so a package that exists only to be imported by tests is scanned in
  full unless named. Which packages those are is a fact about one tree. Note
  that `exclude_dirs` *replaces* the default list rather than extending it, so a
  config setting it restates the generic entries alongside its own.

A consumer that wants its own config files validated, that they parse and that
every entry still matches something, points `QUALITYSCAN_CONFIG_DIR` at the
directory holding them and runs this module's tests. Unset, those tests skip: a
checkout of the tool on its own has nothing to validate.

### Two bands were fitted rather than taken

Most default bands are the conventional published numbers for these metrics.
Two are not, and both were fitted against one large Go codebase:

- **`dependency_span`** is rescaled by about a quarter from the published bands,
  because span at package granularity runs below the scale those are set for.
- **`complexity`** is anchored to `gocognit`'s own threshold rather than to the
  published 30/50, which are set for "function nesting complexity", a different
  metric whose numbers do not transfer. See "Complexity" below.

Both are reasonable starting points for another Go codebase and neither is a
measurement of one. **Re-fitting takes no code change:** dump the raw
measurements with `-format metrics`, pick the cut that puts the intended share
of rows above it (a conventional scale puts roughly the worst 5% in HIGH and the
worst 1% in VERY HIGH), and write the two bands into the repository's `-config`
file. Absent fields keep their default.

Leaving them where they are is also a defensible choice, as long as it is a
choice: a band that reports nothing looks exactly like a codebase with nothing
to report. `TestEveryRuleStillFires` guards the tool's own defaults against
that, not a consumer's overrides.

## What each rule measures

| Rule | Measured over | Definition |
| --- | --- | --- |
| `CYCLIC_REFERENCE` | import | A directory-level reference cycle. |
| `FUNCTION_SIZE_RISK` | function | Go lexical tokens in the declaration, signature and body together. |
| `PARAMETER_RISK` | function | Declared parameters, receiver excluded, variadic counts one. |
| `FUNCTION_COMPLEXITY_RISK` | function | Cognitive complexity (Campbell/SonarSource). Shares the standard rule id but not the metric usually reported under it; see below. |
| `DEPENDENCY_VOLUME_RISK` | file | Every identifier occurrence resolving outside the file. |
| `DEPENDENCY_SPAN_RISK` | file | Distinct targets those references reach. |
| `HARDCODED_URL` | literal | String literal containing `scheme://host`, unless the host is an identifier or a reference rather than an address. |
| `HARDCODED_PATH` | literal | String literal addressing the filesystem. |
| `COPYRIGHT_OR_LICENSE_NOTICE` | comment | A genuine copyright mark, SPDX tag, "all rights reserved" line, or named open-source license grant, not the word "license" used as a verb. |

And the gap rules, which a commercial scan commonly scores zero on for Go:

| Rule | Measured over | Definition |
| --- | --- | --- |
| `SQL_IN_LOOP` | statement | A database call inside a loop body. |
| `STRING_CONCAT_IN_LOOP` | statement | `s += x` on a string inside a loop. |
| `NESTED_LOOP` | loop | A loop inside another loop, closures excluded. |
| `EMPTY_BRANCH` | statement | An `if` with an empty body and no else. |
| `SWITCH_WITHOUT_DEFAULT` | statement | A switch on a value with no default clause. |
| `DEBUG_STATEMENT` | call | `fmt.Print*`, `print`/`println`, or the standard `log` package. |
| `COMMENTED_OUT_CODE` | comment | A comment that parses as Go and is shaped like a statement. |
| `UNFINISHED_WORK` | comment | `TODO`, `FIXME`, `XXX`, `HACK`. |
| `SUPPRESSED_WARNING` | comment | `//nolint` and `//lint:ignore`. |
| `MISSING_DOC_COMMENT` | declaration | An exported package-level declaration with no doc comment. |

Four of these need more than a line.

**Cycles.** Go rejects an import cycle between packages at compile time, so
checking the package graph proves nothing, it is always acyclic. The cycle that
*can* exist is between directories, and the usual shape is a leaf package parked
under the directory that consumes it: a transport directory imports every
package under a service directory, while each of those imports a helper sitting
back under the transport directory. Neither import is a package cycle, but the
two directories point at each other, so neither can be read, moved or extracted
without the other. Hoisting the leaf out to its own directory is the fix. The
scanner builds the graph over directories truncated to a fixed depth, for every
depth in turn, and reports the strongly connected components. Shallower cycles
are the more serious ones, so their edges are claimed first and never reported
twice. One finding per package-to-package edge, not per import statement: many
files importing the same package across a cycle is one architectural fact.

**Dependency volume and span.** Volume is how *much* a file leans on the rest of
the codebase; span is how *many* distinct places it leans on. Both high is a
file that knows about everything. Resolution is syntactic, not type-checked: a
qualified `pkg.Sym` goes through the file's import table, and a bare identifier
goes through the package's cross-file declaration index (Go has no cross-file
scoping inside a package, so that index is exact). What it cannot resolve is a
selector on a value, since `x.Foo()` needs `x`'s type, so method and field
accesses are not counted.

That makes the number a lower bound, and not a uniform one, so it ranks files
against each other better within a layer than across layers: a file leaning on
same-package declarations resolves exactly, a file leaning on injected
dependencies barely resolves at all. `-format metrics` splits the count into
`volume_local` and `volume_cross` so the skew can be seen rather than guessed
at.

Span counts one target per *package* by default, so it reads as "how many
neighbourhoods does this file touch". Set `"span_granularity": "file"` to count
each in-module file separately; that runs roughly 2.5x higher.

**Complexity.** This rule carries the standard id but not the metric usually
reported under it, so the two numbers must not be compared. "Function nesting
complexity" makes cognitive complexity the obvious reading, but the scores do
not reconcile: on a flat dispatch of switches the Campbell/SonarSource
definition charges once per switch rather than once per case, and the other
metric runs well above it on functions containing no switch at all, so the extra
weight is not switch fan-out either. Its formula is not published, so
reproducing it would mean guessing from a handful of data points and giving up
the cross-check against `gocognit`. The rule instead tracks a metric that can be
defined, at `gocognit`'s own threshold.

**Hardcoded paths.** The implementation requires a Windows drive prefix, a `~/`
prefix, a known filesystem root (`/etc/`, `/opt/`, `/var/`, and so on), an
absolute path whose last segment has a file extension, or a relative path with a
separator and an extension. HTTP routes, gRPC method names and MIME types are
excluded by name, because in a server codebase they otherwise drown the rule.
Note that an escaped backslash inside SQL, as in an `ESCAPE '\\'` clause, reads
as a Windows path to a looser implementation and is a common false positive
elsewhere.

**Copyright and license notices.** The implementation requires a standalone
`Copyright` mark followed by `(c)`, the copyright sign or a year, an
`SPDX-License-Identifier` tag, an "all rights reserved" line, or a grant naming
a specific open-source license (`Licensed under the MIT/Apache/BSD/GNU/...`).
Those are the parts of a real notice that ordinary prose about permissions does
not happen to contain, which is what keeps the word "license" used as an
ordinary verb out of the results.

## Thresholds

Defaults are in `config.go`. They are the conventional published numbers for
each metric, except for the two marked below:

| Rule | high | very high | source |
| --- | --- | --- | --- |
| Function size (tokens) | > 200 | > 500 | published |
| Parameters | > 4 | > 6 | published |
| Complexity | > 20 | > 40 | `gocognit`'s own threshold |
| Dependency volume | > 110 | > 200 | published |
| Dependency span | > 23 | > 33 | published bands rescaled to package granularity |

A published threshold is only meaningful if the number underneath it is on the
same scale, and for those two it is not. Left at the published bands, both
reported nothing at all.
`TestEveryRuleStillFires` fails when a default moves out of reach of a fixture
built to trip it, because a rule that matches nothing looks exactly like a rule
with nothing to match.

Override any of them with `-config`; absent fields keep their default. Any rule
can be switched off with `"disabled_rules": ["MISSING_DOC_COMMENT"]`, which is
useful for the large backlog rules once they have a ticket.

`gen/`, `migrations/`, `seed/`, `.disabled-gen/`, `vendor/` and `testdata/` are
skipped by default. `exclude_dirs` in a `-config` file *replaces* this list
rather than adding to it.

`HARDCODED_URL` skips hosts that are identifiers or references rather than
addresses, because those are the findings the rule cannot act on: moving an XML
namespace or a link to a specification into configuration would be wrong, not an
improvement. Two groups are skipped:

- Names RFC 2606 and RFC 6761 reserve so fixtures have hosts guaranteed never to
  resolve: anything under `.test`, `.example`, `.invalid`, `.localhost`, or
  `example.com`/`.net`/`.org`. These are always skipped. Bare `localhost` is
  not, since that is a real local service and hardcoding one is worth reporting.
- Hosts named in `"url_allowlist_hosts"`, which defaults to `w3.org`,
  `rfc-editor.org` and `datatracker.ietf.org`. Matching is on label boundaries,
  so `w3.org` covers `www.w3.org` but not `w3.org.example-cdn.nl`.

Everything else is reported. A URL that addresses a service the scanned codebase
actually talks to belongs in configuration, read from the environment at startup
rather than compiled in.

## Reviewed findings (`accepted.json`)

Most rules answer "is this over the threshold", and the remedy is to change the
code. Two do not: `NESTED_LOOP` and `SQL_IN_LOOP` flag a *shape*, and whether
that shape is a defect depends on where the bounds come from. A loop over a
grouped map is not a product; a query inside a loop over a four-member
vocabulary is not an N+1. The remedy for those is a judgement, made once.

`accepted.json` is where that judgement lives, and `-accepted` applies it.
Without it the judgement evaporates: the scan goes on reporting the same count,
the tracker issue goes on repeating it, and every later pass re-derives the same
verdicts. A count that cannot go down is not a metric, it is a tax.

Three properties stop this being a blanket suppression, and each is pinned by a
test in `accepted_test.go`:

- **Count-bounded.** An entry covers a stated number of findings at a site, not
  the site. Add a second loop to a function accepted for one and the extra is
  reported: the reviewer judged the loop that was there.
- **Keyed on the element, never the line.** Line numbers move constantly, and a
  line-keyed suppression slides onto its neighbour.
- **Stale loudly.** An entry matching fewer findings than it claims means the
  code moved out from under the review, whether fixed, renamed or deleted, and
  the run **exits non-zero** until the record is brought back in line.

Every entry needs a `reviewed` date, a `reason` and a `ref` to the write-up
carrying the per-site reasoning. `LoadAcceptances` rejects one without them,
because an acceptance with no recorded judgement is just a suppression.

Only the two review rules may carry acceptances. The threshold rules are fixed
by changing the code, not by judging it, and a test enforces that.

## The SIG risk-profile distribution

Everything else in this tool reports **violation counts against one gate
threshold**. That is not what the SIG maintainability model measures, and
reading a gate count as a SIG measure is a mistake worth guarding against.

The model bins every unit into four risk categories and caps the share of code
**volume** in each. A count cannot answer it: forty one-line units at very high
complexity are a rounding error next to one 900-line unit, and a count ranks the
forty as forty times worse.

Source of record: **SIG/TUViT Evaluation Criteria Trusted Product
Maintainability, Guidance for Producers, v17.0 (2025-03-12)**.

| Property | Metric | Risk categories | 4-star caps (share of LOC) |
| --- | --- | --- | --- |
| Unit size | lines of code | 1-15 / 16-30 / 31-60 / > 60 | `> 15` <= 47.1%, `> 30` <= 23.1%, `> 60` <= 8.3% |
| Unit complexity | McCabe | 1-5 / 6-10 / 11-25 / > 25 | `> 5` <= 20.2%, `> 10` <= 7.3%, `> 25` <= 1.1% |
| Unit interfacing | parameters | 0-2 / 3-4 / 5-6 / >= 7 | `>= 3` <= 15.0%, `>= 5` <= 3.3%, `>= 7` <= 0.9% |
| Module coupling | incoming dependencies | 0-10 / 11-20 / 21-50 / > 50 | `> 10` <= 10.0%, `> 20` <= 5.6%, `> 50` <= 1.9% |
| Component independence | hidden LOC | hidden / exposed | `>= 93.7%` hidden |
| Duplication | redundant LOC | duplicated / not | `<= 5.6%` redundant |
| Component entanglement | density and violation degree | | `<= 0.077`, **not evaluable locally** |
| Volume | rebuild value | | `<= 3.9` person-years, **not computable for Go** |

All eight properties are measured. Six carry a verdict; the last two carry
figures and an explicit refusal to judge, for the reasons below.

**Four stars is the target, not a requirement.** Pass and fail are the model's
own eligibility wording, and nothing here blocks on them. A fail is a distance
to close, which is what the "LOC to move" column exists to quantify: it reports
the lines that have to leave a failing tail for it to reach its cap, with the
denominator held fixed. That is an estimate rather than an identity, since
splitting one 90-line unit into three 30-line ones moves 90 lines out of the
`> 60` tail and leaves the denominator where it was.

The first three are shares of **unit** LOC; module coupling is a share of
**module** LOC, which includes the lines residing in no unit. The two
denominators reconcile exactly (module LOC = unit LOC + non-unit LOC) and
`TestModuleLOCReconcilesWithUnitLOC` holds them that way.

Five details are easy to get quietly wrong, so each is pinned by a test:

- **The tails are cumulative and nested.** A 70-line unit counts in all three
  unit-size tails. They do not sum to 100%.
- **Interfacing is worded inclusively** (`>= 3`) where size and complexity are
  exclusive (`> 15`). That one word is why its categories break at 3/5/7.
- **The caps apply per language.** Each language gets a verdict; the combined
  roll-up is informational and carries none.
- **A language with no units is "not measured", not 0%.** A zero denominator
  reported as 0% would print a row of passes for a surface nobody scanned. The
  run exits non-zero.
- **Comparison is at full precision, printed to one decimal.** A tail of 47.14%
  fails a 47.1% cap even though the two render identically.

### Two surfaces, one model

`-root` takes one Go module, so another language is measured by a front end of
the consuming repository's own and handed in through `-units`:

```json
{ "language": "TypeScript", "root": "webapp", "non_unit_loc": 29927,
  "files_scanned": 889,
  "units": [{ "element": "useThing", "file": "app/x.ts", "line": 3,
              "loc": 9, "mccabe": 2, "params": 1 }],
  "modules": [{ "file": "app/x.ts", "loc": 40,
                "lines": ["import { ref } from 'vue'", "const state = ref ( 0 )"] }],
  "module_metrics": ["duplication"] }
```

`module_metrics` is the load-bearing field. A front end declares which
module-level properties it actually measured, and the profile reports anything
unclaimed as **not measured**, with the reason, rather than as 0%. Without it,
modules supplied so duplication can be measured would also be binned for
coupling, where they carry no incoming edges: every line would land in the low
band and three caps would print three passes for a property nobody computed.

`LoadUnitSets` rejects an unknown metric, a `coupling` claim with no modules,
and a `duplication` claim where no module carries its lines, so a front end
cannot assert a property its data does not back.

The front end only measures. Binning, caps and arithmetic live here, so there is
one implementation to be right about and one test suite covering every surface.
A malformed or unattributable `-units` file is a hard error, because a language
that silently vanishes reports nothing, and nothing reads as fine.

**Unit definitions differ between languages, on purpose.** The model's unit is
the smallest *named* piece of executable code. In Go a function literal is
anonymous even when assigned to a variable, so its lines and branches bill to
the enclosing declaration. In TypeScript `const f = () => {}` is simply how a
function is declared, so a name-bound arrow is its own unit and only genuinely
anonymous callables bill inward. Getting that wrong collapses whole module
bodies into one "unit" and makes extracting a helper unable to improve the
number.

### The baseline

A scorecard answers two questions: where the code stands, and what has moved.
The profile computes the first. The second used to need a person reading an older
report and recalling the difference, which is the part that decays.

So the numbers are committed. `-format sigjson` writes every figure that carries
a cap or a floor, keyed by language and measure, and `-baseline` makes the report
open with a **Movement** section computed against it. Three rules keep that
honest:

- **The baseline moves deliberately.** Writing it is its own command; generating
  a report never does. A baseline updated on every run would make the report a
  function of when it last ran rather than of the source.
- **A missing baseline is stated, not assumed.** Without one the report says it
  cannot tell you what moved, because "no movement reported" and "nothing moved"
  are different claims.
- **A measure that stops being measured is listed as such**, never dropped. An
  absent number and an improved one look identical in a total.

Comparison is at the precision the report prints, so a change no reader could
have seen is not reported as one.

The scanner reads no clock: `-taken` supplies the baseline's date. Its output
stays a function of its inputs, which is what makes two runs over one tree
byte-identical and a diff between two runs mean something.

### Recorded deviations (`deviations.json`)

Everything above answers "where does the code stand". One thing it cannot answer
is "and is anyone going to do something about it". For most failing tails the
answer is implicit: the LOC-to-move column is the size of the job, and the job is
worth doing. For some it is not, and a scorecard that cannot say so leaves every
later reader to re-derive the same conclusion and re-argue the same trade.

`deviations.json` is where that decision lives, keyed by language, property and
the single tail it covers. The report renders it directly beneath the property
it explains.

A deviation **never changes a number or a verdict**. The tail still measures what
it measures, still prints `fail`, still carries its LOC to move; the deviation is
prose underneath. That is the whole distinction from a suppression, and
`TestDeviationDoesNotChangeTheVerdict` pins it. Hiding the figure would leave a
reader unable to tell a considered deviation from a property nobody had looked
at.

Three rules keep it a decision rather than a licence, each pinned by a test in
`deviations_test.go`:

- **One tail, never the property.** An entry covers `>= 3` and says so. The
  `>= 5` and `>= 7` tiers keep their caps and their verdicts, so the deviation
  cannot quietly widen to excuse a unit that gets genuinely wide later.
- **Reasoning is required.** `LoadDeviations` rejects an entry with no
  `reasoning`, `summary`, `decided` date or `ref`, because a conclusion with no
  argument is a suppression with a date on it.
- **A typo fails the run.** An entry naming a property that does not exist would
  never match, and the report would print a bare `fail` with the reasoning
  sitting unused on disk, so an unknown property ID is a hard error.

### Component independence needs a boundary

A module is **hidden** when nothing outside its own component depends on it, and
the property is the share of lines of code residing in hidden modules. Four
stars needs at least 93.7%. It is the one property with a floor rather than a
cap, so its verdict compares the other way round.

It is also the only property that cannot be computed from the source alone. It
needs the top-level component boundary, and the model makes that the system
owner's scoping decision, not the measurement's. The answer moves the number by
an order of magnitude: treating each internal package as its own component
measures the directory layout rather than the architecture.

So the boundary is data. `components.json` records it, `-components` supplies it,
and four rules keep it a decision rather than a default:

- **A component needs a reason**, and the file needs a `decided` date. The
  loader rejects either being absent, and the report prints the date so a map
  agreed before half the code existed can be read as one.
- **A file matching no component fails the run**, naming the files. An unmapped
  module cannot be classified, and bucketing it silently would move the
  percentage without anyone choosing to.
- **The longest matching prefix wins**, so a `shared` component can claim
  `internal` while a `store` component claims `internal/store`, and the
  declaration order decides nothing.
- **Without `-components` the property is absent, not estimated.** A guessed
  boundary produces a percentage that reads exactly like a measured one.

### Module coupling, and which direction it points

The three unit properties and the two dependency rules all describe what a file
leans on. Module coupling is the other direction: how many other modules lean on
this one, which is what decides whether a change to it is safe. A module fifty
others reference cannot be renamed, moved or reshaped without fifty edits,
however few dependencies of its own it has.

It costs no new analysis. `depResolver` already resolves a qualified `pkg.Sym`
down to the file *declaring* `Sym`, so `measureDeps` has been computing exactly
these edges and discarding their direction. `measureModules` inverts them.

Two things to know before reading the number:

- **It is a lower bound, and not a uniform one.** A selector on a value cannot
  be resolved without types, so a module reached only through an injected
  interface collects no incoming edge from its caller. A module that scores high
  is high; a module that scores low may only be invisible.
- **"Incoming dependency" has two readings and the config picks one.** The model
  says "the number of incoming dependencies, such as invocations, per module",
  which is either the distinct modules referencing this one or every individual
  reference. The default is distinct modules, because the caps then read as a
  coordination cost and a caller that references a module forty times is still
  one caller. `"module_coupling_counts": "references"` switches it, both numbers
  are measured either way, and the report names which reading it applied. An
  unknown value fails the run rather than falling back.

### Duplication, and why it is not jscpd's number

The model's definition is exact, so this implements it rather than approximating
it: a fragment counts when at least 6 lines of code repeat literally, modulo
whitespace, in at least one other location, and the redundancy of a fragment
occurring N times is (N-1) times its length. Four stars needs each language at or
below 5.6% redundant.

Normalisation is by token, not by text. Two lines are equal modulo whitespace
exactly when their token sequences are equal, which makes indentation free by
construction and comments free as well, since the scanner does not emit them. A
trailing comment therefore does not break a match, and a comment-only line is not
part of a fragment at all, which follows from fragments being measured in lines
of *code*. It also means the lines compared here are exactly the lines
`countLOCIn` counts, so the numerator and denominator cannot drift apart;
`TestNormalisedLinesMatchLOC` holds that.

Redundancy is tracked as a set of lines, not a sum over fragments. Overlapping
clones otherwise charge a line twice and a module can report more redundant lines
than it has, at which point the percentage has stopped being one. Repetition
inside a single file counts, because the model says "in at least one other
location", not in another file.

**The figure is not comparable with jscpd's, and the gap is large.** Three
differences, all in the same direction:

| | this tool | jscpd |
| --- | --- | --- |
| Denominator | lines of code | every line of the analysed files, blanks and comments included |
| Fragment floor | 6 lines, no token floor | 6 lines **and** 50 tokens by default |
| Redundancy of N occurrences | `(N-1) x length`, lines counted once | accumulated per detected clone pair |

The token floor is most of it: a short repeated block of six lines is a fragment
to the model and invisible to jscpd's default. Lower jscpd's token floor and put
its percentage on a LOC denominator and the two agree. jscpd stays useful as an
independent cross-check and as a clone browser: it reports which fragments match
which, where this reports how much. The 5.6% cap belongs to the model's
definition, so that is the number this tool measures against.

### Component entanglement, and the verdict it cannot print

Component independence asks how much code is reachable from outside its own
component. Entanglement asks about the shape of the reaching: how many
communication lines the component graph has, and how many of them are lines a
reasonable architecture would not have.

The model specifies this one in unusual detail, so it is implemented rather than
approximated. A communication line is a directed pair of components weighted by
the dependencies it carries; communication density is lines over *connected*
components; communication violation degree is the summed weight of the
violations over the total weight of all lines. Violations are checked in a fixed
order and a line can be marked as only one type:

| Violation | Weighs | Because |
| --- | --- | --- |
| Direct cycle | the lighter of the two lines | removing that one breaks the cycle for the least effort |
| Indirect cycle | the lightest line in the cycle | same rule, on the graph the direct pass left behind |
| Transitive | the line's own weight | it bypasses a path already going the same way |

A transitive line has to satisfy all three of the model's criteria: an indirect
path exists, the target has an outgoing line of its own (a component with none is
a library, and depending on a library directly is what libraries are for), and
the direct line is the lightest on every path between the two. Each rule is
pinned by a test built from the guidance's own figures.

**What is not implemented is the published figure.** The final score normalises
both measurements "by dividing by the maximal values for each in our benchmark",
and those maxima are not in the document. So the report prints density,
violation degree and the violations behind them, and no verdict against the
0.077 cap, with that stated in the output. A number computed without the
constants would be a different metric wearing the model's threshold.

### Volume, reported and never graded

The model estimates a product's rebuild value in person-years from its lines of
code, normalised per language, and caps it at 3.9 person-years for four stars.
Two things stop that being a verdict here. The published conversion table names
seven languages and Go is not one of them, so a Go product total is missing its
largest term. And the cap is on one figure for the whole system, not one per
language, so a per-language verdict would be a different measurement wearing this
one's threshold.

SIG's own 2026 quality model removed the property outright, on the grounds that
system size is a risk indicator with no action attached. The criteria this tool
measures against still carry it, so it is reported as a figure: lines of code per
language, and a rebuild value wherever the table supports one.

## Filing issues

The scan groups its findings into one tracker issue per rule and files them
through the `gh` CLI. Per rule is the split that matches how the work gets done:
each rule is one kind of change applied in many places, so one ticket carries one
decision and one reviewer's context. Rules that found nothing produce no issue.

Each body opens with an HTML comment naming its rule:

```html
<!-- qualityscan:SQL_IN_LOOP -->
```

That marker is what makes publishing **idempotent**. A publish run reads the
existing issues, matches on the marker, and edits the issue it finds rather than
opening a second one. Three details matter:

- Matching reads the issue list and compares bodies locally rather than using
  `gh issue list --search`. GitHub's search index lags writes by seconds to
  minutes, so a search-based check would cheerfully duplicate an issue opened
  moments earlier.
- An issue someone has **closed** is left closed. Re-opening it on every scan
  would make the tool argue with the people using it. `-reopen` overrides that.
- Labels are added on update, never removed, so a re-triage is not overruled by
  the next scan.

Publishing writes to a shared tracker, so `-publish` is never the default:
`-plan` prints what it would create or update and writes nothing.
`-format markdown` writes the same content as one file, the summary table then
the full body of every issue that would be filed. `-issue-dir` writes one file
per issue with YAML front matter instead, for editing the wording first.

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-root` | `.` | Module directory to scan; must contain `go.mod`. |
| `-format` | `text` | `text`, `csv`, `json`, `markdown`, `metrics`, `sigprofile`, `sigunits`, `sigmodules`, or `sigjson`. |
| `-out` | stdout | Write to a file instead. |
| `-config` | none | JSON threshold overrides. |
| `-accepted` | none | JSON file of reviewed findings to treat as accepted. |
| `-components` | none | JSON file naming the top-level components. |
| `-deviations` | none | JSON file of recorded deviations from the SIG caps. |
| `-baseline` | none | JSON snapshot of a previous run, to report movement against. |
| `-taken` | none | Date to stamp on a `-format sigjson` snapshot. |
| `-units` | none | JSON file of externally measured units to fold into the SIG profile. Repeatable, one per language. |
| `-prefix` | none | Prepended to reported filenames, to match an external report's paths. |
| `-tests` | off | Include `_test.go`. |
| `-fail-on` | `0` | Exit 1 when the finding count exceeds this. `0` disables. |
| `-rev` | none | Commit stamped on the report and on each issue body. |
| `-issue-dir` | none | Write one markdown file per proposed issue into this directory. |
| `-plan` | off | Show what `-publish` would create or update, writing nothing. |
| `-publish` | off | Create or update tracker issues via `gh`. |
| `-repo` | inferred | `owner/name` to publish to. |
| `-reopen` | off | Also update issues someone has closed. |

`csv` is a sectioned layout, a bare rule name, a header row, then rows numbered
from 1 within the section. That is what a commercial scan exports, so the two
can be diffed directly. Rules that found nothing still get a section, because "0
violations" is the half of the report that says the check ran.
