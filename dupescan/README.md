# dupescan

Finds one concept **declared twice** across files, on the TypeScript **and Go** surfaces.

```
node dupescan.mjs                                  # text report
node dupescan.mjs --strict                         # same, exits 1 when anything is found
node dupescan.mjs --json                           # machine-readable
node dupescan.mjs --root app/utils --root shared
node dupescan.mjs --root internal                  # the Go half alone
node dupescan.mjs --config path/to/dupescan.json

node --test dupescan.test.mjs                      # this tool's tests
```

One file, Node stdlib only, nothing to install. A consuming repository does not vendor
it: `go mod download github.com/ClappFormOrg/static-analysis@<version>` puts the whole
module in the module cache, and `go list -m -f '{{.Dir}}'` prints the directory to run it
out of. See the root [README](../README.md).

Well under a second over a few thousand files. Regex based, so it runs on a branch that
does not compile.

`.ts`, `.tsx`, `.vue` and `.go` are all scanned. For an SFC only the `<script>` blocks are
read (both, when a component pairs `<script setup>` with a plain one); reading a `.vue`
whole would parse its `<template>` and `<style>` as TypeScript. That extraction is regex
too, not `vue/compiler-sfc`, so the tool still resolves no packages. It matters more than
it sounds: including SFCs roughly doubled the union checks' exact matches, because
duplicated palettes and size scales tend to be declared in components rather than in
`.ts` files.

The two surfaces are scanned separately, not together: checks 1-4 key on TypeScript
declaration syntax and check 5 on Go's, so each is fed only the files it was written for.

Excluded on the Go side: `_test.go` (as `.test.ts` is on the other surface), plus
whatever generated and migration trees a repository names in its config. Test exclusion
does more work here than it looks, since test helpers are exactly the kind of thing a
codebase restates per package.

## Configuration

Which trees to read, and which to skip, are facts about a repository's layout rather
than about duplication, so the scanner holds neither. A consuming repository keeps its
answers in a `dupescan.json` of its own, which is read from the working directory
unless `--config` names it somewhere else:

| Key | What it does |
| --- | --- |
| `roots` | Repo-root-relative trees to scan. `--root` overrides it for a single run. |
| `skip_paths` | Regular expressions, anchored at the repo root, for trees that are not hand-written. Matched against POSIX paths, so a nested directory of the same name elsewhere is not swept up. |

With no config and no `--root`, the scan falls back to the whole working directory and
says so on stderr. That is the honest fallback, since a tool that silently scanned
nothing would report a clean repository, but on a monorepo it is slow, so name the roots.

## Why a linter does not cover this

| tool | sees across files? | finds | misses |
| --- | --- | --- | --- |
| `sonarjs/no-duplicate-string` (ESLint) | **no** | a literal repeated 4+ times *within one file* | the same literal once in each of two files |
| `dupl` (golangci-lint, Go only) | yes | ~150 tokens of near-identical Go | anything shorter; the whole frontend |
| jscpd | yes | ~6+ near-identical lines, Go + TS | one-line duplicates; re-implementations that share no tokens |
| **dupescan** | yes | one **declaration** restated, Go + TS: a name, a value, a union, a record shape, a per-package helper | textual clones inside function bodies (that is jscpd's job) |

ESLint parses one file and forgets it. That is not a configuration gap, it is the tool's
boundary, and it is why a cookie name declared once in a client tree and again in a
server tree never shows up in a lint run.

A Go tree sits in the same uncovered corner for the mirror-image reason. A clone
detector can put a Go surface comfortably inside a duplication cap and be accurate,
because a service helper is four to eight lines: under jscpd's 6-line floor and far under
`dupl`'s 150 tokens. Check 5 is that gap closed, and nothing about the regex and
line-oriented design was ever TypeScript specific.

## The five checks

Each check has a test asserting it does **not** fire on a legitimate lookalike. A
detector that reports ordinary code gets skimmed, and a skimmed detector is worth
nothing.

1. **Same exported name in more than one module.** Auto-import then chooses for you,
   and the wrong choice type-checks whenever the shapes are compatible. Unlike check 2
   this one **does** require `export`, and the asymmetry is deliberate: its claim is that
   the import site decides which declaration wins, so there has to be an import site.
   TypeScript scopes an unexported name to its module, and dropping the keyword here
   multiplied the finding count more than twentyfold, almost all of it local type aliases
   and destructured locals rather than duplication.
2. **Same string value behind more than one module-level constant**, exported or not.
   The motivating shape is a session cookie's name declared under two constants:
   renaming either side keeps compiling, passes every test, and logs every user out.
   `export` is not required here, because it says who may *import* the name and this
   check is about the *value*. Backtick literals count too, unless the template carries
   a `${...}`; with a substitution the text is a recipe rather than a value, and two
   recipes sharing a prefix are not one concept.
3. **Same string-literal union declared in more than one place**, keyed on the sorted
   member set so a reordered copy still matches. Also reports a set that is a strict
   **subset** of a larger one, because a copy that has fallen behind cannot match
   exactly. That is how a badge palette ends up with two six-member copies while the
   component accepts eight.
4. **Records with an identical field-name set**, written as an `interface` or as a
   `type X = { ... }` alias. Structural typing keeps two such records interchangeable
   until one gains a field, at which point half the app cannot see it. Both forms are
   read because TypeScript treats them as interchangeable and so does every caller;
   which keyword the author reached for should not decide whether a duplicate is
   visible.
5. **One unexported package-level Go function name declared in four or more packages.**
   The Go mirror of check 1. Go has no `export` keyword and no auto-import, so checks
   1-4 find nothing here; a Go codebase restates a helper per package instead, because
   sibling packages cannot see each other's unexported names.

Three constraints on check 5 do the work, and each is load-bearing rather than
stylistic:

- **Unexported only.** `New` is Go's constructor idiom and is declared in as many
  packages as a codebase has constructors. Counting exported names would make the
  loudest finding in the whole run a false positive, and a detector whose top finding is
  noise gets skimmed.
- **Packages, not files.** Go forbids two package-level functions of one name in a
  package, so the count only moves when a genuinely separate package restates the
  helper. A build-tagged second file counts once.
- **Methods are not counted.** A method is namespaced by its receiver type. Folding
  methods in surfaces generic verbs on unrelated types, so they are parsed only to make
  sure a method is never mistaken for a package-level function of the same name.

## Reading the output

One flat list, worst first, each finding carrying a `why`. Severity is **how silently
the duplicate fails**, not how many copies exist:

| factor | effect | because |
| --- | --- | --- |
| duplicated **value** | strongest | fails at runtime with nothing watching: rename one side of a cookie name and every user is logged out while the build, the types and every test stay green |
| **auto-import** scope | strong | a framework that publishes `composables/*` and `utils/*` app-wide resolves a twice-declared name *for* the caller, so the caller cannot see the choice being made |
| crosses **app / server / e2e** | strong | those trees cannot import each other, so a copy is the only option and nothing links them |
| Go copies at **differing arity** | strong | not copies of one function but separate answers to one question, which no clone detector reaches at any threshold |
| a Go helper in **8+ packages** | strong | past the point the set is reviewable as a whole, so a fix applied to one copy silently leaves the rest wrong |
| a copy sits in **types/** or **shared/** | mild | those modules exist to be the single declaration |
| 3+ copies | mild | scales the cost, not the risk |
| both in **one file** | *demotes* | still worth fixing, but no other module can be misled |

Subset findings are deliberately **inferences** and say so. A narrower palette is often
correct, since a status that must never take one of the colours is a constraint rather
than drift, so they only escalate when the *same compound* identifier appears on both
sides. `StatusBadgeVariant` narrower than `StatusBadgeVariant` is one concept diverging;
`Size` in a button being narrower than `Size` in a modal is a design decision. Getting
that backwards makes a subset model rank a design system behaving normally as its
highest findings.

Findings are evidence, not verdicts. Two modules exporting `Row`, or two domains sharing
a five-colour palette on purpose, are expected to appear; the report names every file so
a reader can dismiss them. What it cannot do is tell you which of two copies is the live
one, which still takes reading the call sites.

Tests are excluded from the scan: a fixture named after a production symbol is not a
duplicate declaration, and mock modules restate real shapes by design.

## Limits worth knowing

- Regex, not a type checker: a union built with `keyof` or a mapped type is invisible,
  and so is a shape declared inline in `defineProps<{...}>()` rather than as an
  `interface`.
- A multi-declarator statement (`export const CHIP_H = 14, ROW_H = 16`) is read only
  when it fits on one line and no initialiser contains a bracket. Without parsing there
  is no telling `const a = f(x), b = 2` from a comma inside an argument list, and an
  arrow function's default parameter (`(t, showTemperature = false) =>`) reads exactly
  like a second declarator, so those are skipped rather than guessed at.
- A Go type-parameter list is read one bracket deep, which covers a constraint over a
  composite type (`[M ~map[string]V, V any]`). Two levels (`[T ~[]map[string]int]`) are
  still missed.
- Check 5 reads declaration *heads*, never bodies, so it cannot tell an in-sync copy
  from one that has already drifted. Copy count is therefore a proxy for attention, not
  evidence that a smaller set is safe.
- Divergence on the Go side is judged by argument **count**, not types. Copies of one
  helper routinely rename their parameters, so comparing signature text would call every
  finding a re-implementation. Arity errs toward silence: two copies taking the same
  number of different types look identical here.
- Interface fields are collected by brace depth, so a nested object type is attributed
  to its outer field rather than treated as its own record.
- SFC line numbers are not preserved by the script extraction. Findings are reported per
  FILE, which is all the checks need: the claim is that a declaration exists twice, not
  where in a file it sits.
- **The subset check is the noisiest section.** A design system legitimately gives each
  component its own size scale, so `Size` declared in five components is expected; what
  is worth reading there is the *missing member* line, which is how a scale that fell
  behind becomes visible. If it ever gets skimmed, cap it rather than delete it.
