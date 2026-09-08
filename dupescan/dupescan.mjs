/**
 * Cross-file duplicate DECLARATION scanner for the TypeScript and Go surfaces.
 *
 *   node dupescan.mjs                  # text report, default roots
 *   node dupescan.mjs --json           # machine-readable
 *   node dupescan.mjs --strict         # exit 1 when anything is found
 *   node dupescan.mjs --root app/utils --root shared
 *   node dupescan.mjs --config path/to/dupescan.json
 *
 * WHY THIS EXISTS, given a repo may already run sonarjs and (on Go) dupl:
 *
 * ESLint parses one file at a time and forgets it, so `sonarjs/no-duplicate-string`
 * means "this literal repeats WITHIN this file". The same string under two names in
 * two files is one occurrence twice, and is invisible. `dupl` and jscpd do look
 * across files, but both need ~6 near-identical LINES.
 *
 * The duplication neither of those reaches is one concept DECLARED twice, usually one
 * line each. The shapes that motivated this tool:
 *
 *   - a session cookie's NAME, under two different const names, in a client tree and
 *     a server tree. Renaming one side keeps compiling, passes every test, and logs
 *     every user out.
 *   - a badge colour palette restated many times, some copies having drifted to fewer
 *     members than the component itself accepts.
 *   - one composable exported by two modules, one store-backed and stale, one
 *     fetching, with auto-import silently binding the stale one.
 *   - one decoder implemented twice, client and server, with different primitives
 *     (`atob` against `Buffer`), so no clone detector could ever match them.
 *
 * A Go tree sits in the same uncovered corner, for the mirror-image reason. `dupl`
 * (golangci-lint) reads Go but needs ~150 tokens; jscpd needs ~6 near-identical lines.
 * A service helper is four to eight lines, so a helper restated across a dozen or more
 * packages is invisible to both. Check 5 covers that shape; nothing about the
 * regex/line design was ever TypeScript specific.
 *
 * Deliberately regex/line based and dependency-free, matching `qualityscan`'s
 * stdlib-only rule: it must run on a branch that does not compile, in about a second,
 * with nothing to install. That costs precision, so every check documents what it
 * over-reports and every finding names its files rather than asserting a verdict.
 */

import fs from 'node:fs'
import path from 'node:path'
import process from 'node:process'

/** Fallback when neither `--root` nor a config file names anything: the working
 *  directory. Which trees are worth scanning is a fact about a repository's layout,
 *  not about duplication, so the real list lives in the scanned repository's config
 *  rather than here.
 *
 *  Scanning everything from the root is the honest fallback -- a tool that silently
 *  scanned nothing would report a clean repository -- but on a monorepo it walks
 *  vendored and build trees that `SKIP_DIRS` does not name and takes minutes, so the
 *  run says what it is doing rather than appearing to hang. */
const DEFAULT_ROOTS = ['.']

/** Never source: generated output and vendored bundles would flood every check. */
const SKIP_DIRS = new Set([
  'node_modules', '.nuxt', '.output', 'dist', 'coverage', 'generated',
  'playwright-report', 'test-results', '.git',
])

/**
 * Trees that are not hand-written, excluded by path rather than by directory name.
 *
 * Empty by default and supplied by config, because these are one repository's
 * generated trees. A Go repository that runs protoc typically names its `gen` tree,
 * any parked twin of it, and a migrations directory whose SQL sits beside a thin
 * embedding shim, matching whatever its clone detector and linter already exclude.
 * A scan including them would report the generated `meta` accessors protoc emits
 * once per message.
 *
 * By path, not by basename, because `SKIP_DIRS` is matched against a bare directory
 * name: adding `gen` there would silently skip any future `gen/` elsewhere in the
 * tree. Patterns are anchored at the repo root and matched against POSIX paths.
 */
const DEFAULT_SKIP_PATHS = []

/** Build the skip-path matcher from config. No patterns means skip nothing, rather
 *  than an empty regex, which would match every path. */
function skipPathMatcher(patterns) {
  if (!patterns || patterns.length === 0) return () => false
  const re = new RegExp(patterns.map(p => `(?:${p})`).join('|'))
  return f => re.test(f)
}

/** Tests are excluded: a fixture named like a production symbol is not a duplicate
 *  declaration, and mock modules restate real shapes by design. On the Go side this
 *  matters more than on the TS side - `adminCtx` and `ctxAs` are each declared in 8
 *  and 7 packages respectively, entirely in `_test.go` files, and they are test
 *  scaffolding rather than production duplication. */
const isTestPath = f => /\.(test|spec)\.(?:[cm]?tsx?|vue)$/.test(f)
  || f.includes('__tests__')
  || f.endsWith('_test.go')

const isScanned = name => /\.[cm]?tsx?$/.test(name) || name.endsWith('.vue') || name.endsWith('.go')

const isGo = f => f.endsWith('.go')

/**
 * The `<script>` content of a single-file component, concatenated.
 *
 * Scanning a `.vue` file whole would read its `<template>` and `<style>` as TypeScript;
 * only the script blocks declare anything. Both blocks are taken (a component may pair
 * `<script setup>` with a plain `<script>` for a name or an option), and `lang` is not
 * filtered on, so a plain-JS SFC still contributes.
 *
 * Regex rather than `vue/compiler-sfc`: this tool resolves no packages, so it keeps
 * working when `node_modules` is absent or the branch does not build. Line
 * numbers are not preserved, which costs nothing here - findings are reported per FILE,
 * since the point is that a declaration exists twice, not where in a file it sits.
 *
 * The palette copies that make this necessary tend to live exactly here: a `Variant`
 * union declared in one component, another in a second, a third inline on a third
 * component's prop. A `.ts`-only scan reports none of them.
 */
function extractVueScript(src) {
  const blocks = [...src.matchAll(/<script[^>]*>([\s\S]*?)<\/script>/g)]
  return blocks.map(m => m[1]).join('\n')
}

const toPosix = p => p.split(path.sep).join('/')

function walk(dir, out = [], skip = () => false) {
  let entries
  try {
    entries = fs.readdirSync(dir, { withFileTypes: true })
  } catch {
    return out // a root that does not exist in this checkout is simply skipped
  }
  for (const e of entries) {
    const full = path.join(dir, e.name)
    if (skip(toPosix(path.relative(process.cwd(), full)))) continue
    if (e.isDirectory()) {
      if (!SKIP_DIRS.has(e.name)) walk(full, out, skip)
    } else if (isScanned(e.name) && !isTestPath(full)) {
      out.push(full)
    }
  }
  return out
}

/** Strip block and line comments so a symbol named only in prose never counts. A
 *  `//` inside a string literal is mangled by this; harmless, because every check
 *  reads declaration heads rather than string bodies. */
function stripComments(src) {
  return src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:"'`\\])\/\/.*$/gm, '$1')
}

// ---------------------------------------------------------------------------
// Check 1 - the same exported SYMBOL NAME declared by more than one module.
//
// Highest yield. Two modules exporting one name means auto-import chooses for you,
// and the wrong choice type-checks whenever the shapes are compatible.
//
// OVER-REPORTS: a same-name-different-domain pair that is genuinely fine (a `Row` or
// `Config` per module). Pure re-exports (`export { X } from './y'`) do not match,
// because this only looks at declaration heads.
//
// `export` IS REQUIRED here, and unlike in Check 2 that is not an oversight. This
// check's claim is "the import site decides which", so it needs there to BE an import
// site: TypeScript scopes an unexported declaration to its module, and two files
// declaring `Translate` or `Api` are not in conflict at all. Dropping the keyword was
// tried and took the run from 63 findings to 1386, of which 1176 were unexported names
// - local type aliases and destructured locals, none of them duplication. A detector
// whose output is mostly ordinary code gets skimmed, which is the same argument that
// keeps Check 5 to unexported Go names and off `New`.
//
// The asymmetry with Check 2 is the point. A duplicated VALUE is one concept restated
// whether or not anyone can import it; a duplicated NAME is only a hazard when
// something resolves it for you.
// ---------------------------------------------------------------------------
const DECL_RE = /^[ \t]*export[ \t]+(?:async[ \t]+)?(?:function|const|let|class|interface|type|enum)[ \t]+([A-Za-z_$][\w$]*)/gm

/**
 * The SECOND and later names of a one-line multi-declarator export.
 *
 * `DECL_RE` captures one name per line, so `export const CHIP_H = 14, ROW_H = 16`
 * exported `ROW_H` invisibly. Both halves of the pattern are needed rather than one
 * cleverer regex: the head is what carries the `export` keyword and the kind.
 *
 * Bounded to a SINGLE LINE and to an initialiser with no bracket in it. A declarator
 * whose value spans lines or contains `(`, `[` or `{` is skipped, because without
 * parsing there is no way to tell `const a = f(x), b = 2` from a comma inside an
 * argument list, an array or an object - and a default parameter in an arrow function
 * (`(t, showTemperature = false) =>`) reads exactly like a second declarator. Skipping
 * those under-reports, which is the right direction: a wrong name here would be
 * attributed to a module that never exported it.
 */
const MULTI_DECL_RE = /^[ \t]*export[ \t]+(?:const|let)[ \t]+[A-Za-z_$][\w$]*[ \t]*=[^,;(\[{\n]*((?:,[ \t]*[A-Za-z_$][\w$]*[ \t]*=[^,;(\[{\n]*)+)$/gm

const TRAILING_NAME_RE = /,[ \t]*([A-Za-z_$][\w$]*)[ \t]*=/g

function checkDuplicateExports(sources) {
  const places = new Map()
  const record = (name, file) => {
    if (!places.has(name)) places.set(name, new Set())
    places.get(name).add(file)
  }
  for (const [file, src] of sources) {
    for (const m of src.matchAll(DECL_RE)) record(m[1], file)
    for (const m of src.matchAll(MULTI_DECL_RE)) {
      for (const t of m[1].matchAll(TRAILING_NAME_RE)) record(t[1], file)
    }
  }
  const findings = []
  for (const [name, files] of places) {
    if (files.size > 1) findings.push({ name, files: [...files].sort() })
  }
  return findings.sort((a, b) => b.files.length - a.files.length || a.name.localeCompare(b.name))
}

// ---------------------------------------------------------------------------
// Check 2 - the same string VALUE behind more than one exported constant.
//
// The cookie-name case: `SESSION_KEY` in app/ and `SESSION_COOKIE` in server/, same
// string, nothing connecting them. Two names for one value drift the moment either
// side is renamed, and nothing fails.
//
// OVER-REPORTS: short or generic values reused coincidentally, which the length floor
// below trims. Reported only when the value appears under two DIFFERENT names or in
// two different files - the same name re-exported is not a finding.
//
// `export` IS OPTIONAL here for the same reason as in Check 1, and it matters more:
// the whole point of this check is a VALUE restated, and a value does not care about
// the keyword. The API paths `/api/v1/widgets` (3 copies), `/api/v1/projects` and
// `/api/v1/access-requests` are each declared behind unexported consts, as is one of
// the two `UNITS_PER_YEAR` copies - every one of them invisible while this was anchored
// to `^export`.
//
// BACKTICKS are accepted alongside `'` and `"`, but only for a template with no `${}`
// in it. With a substitution the text is not a value at all, it is a recipe, and two
// recipes sharing a prefix are not one concept.
// ---------------------------------------------------------------------------
const CONST_STR_RE = /^[ \t]*(?:export[ \t]+)?const[ \t]+([A-Za-z_$][\w$]*)(?:\s*:\s*[^=]+)?\s*=\s*(['"`])((?:(?!\2)[^\\]|\\.)*)\2/gm

/** A backtick literal carrying a `${...}` is a template, not a value. */
const hasSubstitution = (quote, value) => quote === '`' && value.includes('${')

/** Below this, a shared value is far more likely coincidence than one concept. */
const MIN_VALUE_LEN = 4

function checkDuplicateLiteralConsts(sources) {
  const byValue = new Map()
  for (const [file, src] of sources) {
    for (const m of src.matchAll(CONST_STR_RE)) {
      const [, name, quote, value] = m
      if (value.length < MIN_VALUE_LEN || hasSubstitution(quote, value)) continue
      if (!byValue.has(value)) byValue.set(value, [])
      byValue.get(value).push({ file, name })
    }
  }
  const findings = []
  for (const [value, sites] of byValue) {
    const names = new Set(sites.map(s => s.name))
    const files = new Set(sites.map(s => s.file))
    if (sites.length > 1 && (names.size > 1 || files.size > 1)) {
      findings.push({ value, sites: sites.sort((a, b) => a.file.localeCompare(b.file)) })
    }
  }
  return findings.sort((a, b) => b.sites.length - a.sites.length)
}

// ---------------------------------------------------------------------------
// Check 3 - the same STRING-LITERAL UNION declared in more than one place.
//
// The badge-palette case. Matches named aliases AND unions written inline on a field,
// because the nine copies were a mix of both, and keys on the SORTED member set so a
// reordered copy still matches.
//
// OVER-REPORTS: two domains that legitimately share a vocabulary shape. A NARROWER
// copy (six members where the real palette has eight) does NOT appear here at all -
// different set, different key - which is why the report also lists near-misses:
// sets that are a strict subset of a larger one, the shape drift actually takes.
// ---------------------------------------------------------------------------
const UNION_RE = /(?:type\s+([A-Za-z_$][\w$]*)\s*=|([A-Za-z_$][\w$]*)\s*\??\s*:)\s*((?:'[^']+'|"[^"]+")(?:\s*\|\s*(?:'[^']+'|"[^"]+")){2,})/g

const MEMBERS_RE = /'([^']+)'|"([^"]+)"/g

function unionSites(sources) {
  const sites = []
  for (const [file, src] of sources) {
    for (const m of src.matchAll(UNION_RE)) {
      const label = m[1] ?? m[2]
      const members = [...m[3].matchAll(MEMBERS_RE)].map(x => x[1] ?? x[2])
      sites.push({ file, label, members, key: [...members].sort().join('|') })
    }
  }
  return sites
}

function checkDuplicateUnions(sources) {
  const sites = unionSites(sources)
  const byKey = new Map()
  for (const s of sites) {
    if (!byKey.has(s.key)) byKey.set(s.key, [])
    byKey.get(s.key).push(s)
  }
  const exact = []
  for (const [key, group] of byKey) {
    if (group.length > 1) exact.push({ members: group[0].members, key, sites: group })
  }

  // Near-misses: a set strictly contained in a bigger one. This is where a copy that
  // has fallen behind shows up, and the exact-match check cannot see it by design.
  const keys = [...byKey.keys()].map(k => ({ key: k, set: new Set(k.split('|')) }))
  const subsets = []
  for (const a of keys) {
    for (const b of keys) {
      if (a.key === b.key || a.set.size >= b.set.size) continue
      if ([...a.set].every(m => b.set.has(m))) {
        subsets.push({
          smaller: byKey.get(a.key),
          larger: byKey.get(b.key),
          missing: [...b.set].filter(m => !a.set.has(m)),
        })
      }
    }
  }
  return {
    exact: exact.sort((x, y) => y.sites.length - x.sites.length),
    subsets: subsets.sort((x, y) => x.missing.length - y.missing.length),
  }
}

// ---------------------------------------------------------------------------
// Check 4 - interfaces with an identical FIELD-NAME SET.
//
// Two names for one record shape (`OrderTotalsRecord`, declared in `types/entities`
// and in `types/portal/orders`). Structural typing keeps them interchangeable
// until one gains a field, at which point half the app cannot see it.
//
// OVER-REPORTS: small shapes that coincide (`{ id, name }`), which the field floor
// trims. Field extraction is brace-depth based, so a nested object literal inside a
// field type is attributed to its outer field rather than treated as its own record.
// ---------------------------------------------------------------------------
const MIN_FIELDS = 4

/**
 * Field names at brace depth 1 of each `interface X { ... }` or `type X = { ... }`
 * block.
 *
 * BOTH FORMS, because TypeScript treats them as interchangeable and so does every
 * caller: a shape written as an `interface` in one module and as a `type` alias in
 * another is the same record twice, which is the whole claim of this check. Reading
 * only `interface` made the form of the declaration decide whether a duplicate was
 * visible, and eight object-shaped aliases sit in the webapp today.
 *
 * The alias arm requires `= {` with nothing but whitespace between, so a union of
 * object types, a mapped type (`{ [K in T]: ... }`, whose members are not field names)
 * and an intersection all fall out - `MIN_FIELDS` would drop most of them anyway, but
 * not matching them at all keeps the field extractor reading what it was written for.
 */
function collectInterfaces(file, src) {
  const out = []
  const head = /(?:export\s+)?(?:interface\s+([A-Za-z_$][\w$]*)[^{]*|type\s+([A-Za-z_$][\w$]*)\s*=\s*)\{/g
  let m
  while ((m = head.exec(src)) !== null) {
    const name = m[1] ?? m[2]
    let i = m.index + m[0].length
    let depth = 1
    const fields = []
    let line = ''
    // A pending field is flushed by a separator AND by the closing brace. Without the
    // second, the last field of a declaration written on ONE line is dropped, which
    // took `{ id: string; name: string; foo: number; bar: number }` to three fields and
    // under `MIN_FIELDS`. A multi-line declaration hid this completely: its `}` sits on
    // its own line, so the preceding newline had already flushed the field.
    const flush = () => {
      const f = /^\s*(?:readonly\s+)?([A-Za-z_$][\w$]*)\s*\??\s*:/.exec(line)
      if (f) fields.push(f[1])
      line = ''
    }
    while (i < src.length && depth > 0) {
      const ch = src[i]
      if (ch === '{') depth++
      else if (ch === '}') depth--
      if (depth === 0) flush()
      else if (depth === 1 && (ch === '\n' || ch === ';' || ch === ',')) flush()
      else if (depth >= 1) line += ch
      i++
    }
    if (fields.length >= MIN_FIELDS) {
      out.push({ file, name, fields, key: [...new Set(fields)].sort().join('|') })
    }
  }
  return out
}

function checkDuplicateInterfaces(sources) {
  const byKey = new Map()
  for (const [file, src] of sources) {
    for (const s of collectInterfaces(file, src)) {
      if (!byKey.has(s.key)) byKey.set(s.key, [])
      byKey.get(s.key).push(s)
    }
  }
  const findings = []
  for (const [, group] of byKey) {
    // Same NAME in two files is already Check 1's finding; here the interesting case
    // is one shape under two different names, or the same shape in two modules.
    if (group.length > 1) findings.push({ fields: group[0].fields, sites: group })
  }
  return findings.sort((a, b) => b.sites.length - a.sites.length)
}

// ---------------------------------------------------------------------------
// Check 5 - one unexported package-level Go FUNCTION NAME declared in four or more
// packages.
//
// The Go mirror of Check 1, and the reason this tool grew a second surface. Go has no
// `export` keyword and no auto-import, so the TypeScript checks find nothing here; what
// a Go codebase does instead is restate a helper per package, because sibling packages
// cannot see each other's unexported names. Neither `dupl` (150 tokens) nor jscpd
// (6 lines) can see a six-line helper.
//
// UNEXPORTED ONLY, and this is the load-bearing constraint rather than a stylistic one.
// Exported names carry Go's package-qualified conventions: `New` is the constructor
// idiom and is declared in as many packages as a codebase has constructors, which is
// not duplication. Lowering the bar to exported names makes the single loudest finding
// a false positive, which is how a detector gets skimmed.
//
// PACKAGES, not files: Go forbids two package-level functions of one name in a package,
// so the count only moves when a genuinely separate package restates the helper. A
// build-tagged second file in the same package counts once.
//
// METHODS ARE NOT COUNTED. `func (s *Service) withTx(...)` is namespaced by its receiver
// type, and the names that surface when methods are folded in are generic verbs on
// unrelated types - `resolve` on `*Resolver` and on `*boundSet`, `any` on four different
// expand structs. They are parsed only so a method is never mistaken for a package-level
// function of the same name.
//
// OVER-REPORTS: a short helper that is genuinely cheaper to restate than to share, and
// two packages that coincidentally name unrelated helpers alike. The four-package floor
// trims the coincidences; at the time of writing every name it reports is one concept.
// `stripComments` does not touch string literals, so a backtick-quoted block whose text
// happens to begin a line with `func name(` would read as a declaration - no such case
// exists in the module today, and the four-package floor makes one implausible.
// ---------------------------------------------------------------------------

/** `func name(` or `func name[T any](` at column 0, lowercase initial. A method reads
 *  `func (r *T) name(`, so the `(` immediately after `func ` excludes it here.
 *
 *  The type parameter list allows ONE level of nested brackets, which is what a
 *  constraint written over a composite type needs: `decodeSealedJSON[M ~map[string]V,
 *  V any]` is declared in `service/request` today and a `[^\]]*` list stopped at
 *  the `]` of `[string]`, missing the declaration entirely. Two levels
 *  (`[T ~[]map[string]int]`) are still missed; the same under-reporting argument
 *  applies, and no such declaration exists in the module. */
const GO_FUNC_RE = /^func[ \t]+([a-z]\w*)[ \t]*(?:\[(?:[^[\]]|\[[^[\]]*\])*\])?\(/gm

/** Below this a shared helper name is far more likely convention than one concept. */
const MIN_GO_PACKAGES = 4

/** A Go package is a directory, so the file's parent is its package path. */
const goPackageOf = file => file.slice(0, file.lastIndexOf('/'))

/**
 * How many parameters a declaration takes, counting commas directly inside its own
 * parameter list.
 *
 * ARITY rather than types, deliberately. Two copies of one helper routinely name their
 * parameters differently (`hasPath(paths []string, p string)` against
 * `hasPath(fm []string, want string)`), so comparing signature text would mark almost
 * every finding as diverging and the signal would mean nothing. Arity is invariant to
 * naming and needs no Go type parsing, and it errs toward silence: two copies that take
 * the same number of different types look identical here. Under-reporting is the right
 * direction for a heuristic that escalates severity.
 *
 * `i` is the index of the opening paren. Nested parens, brackets and braces are tracked
 * so a `func(a, b int)` parameter contributes one argument rather than two.
 *
 * Returns `null` for a parameter list that never closes, which is what a half-typed
 * declaration on a mid-edit branch looks like. Counting to end-of-file there would
 * invent an arity and could report a set of identical copies as re-implementations.
 */
function goArity(src, i) {
  let depth = 0
  let commas = 0
  let sawContent = false
  for (; i < src.length; i++) {
    const ch = src[i]
    if (ch === '(' || ch === '[' || ch === '{') depth++
    else if (ch === ')' || ch === ']' || ch === '}') {
      depth--
      if (depth === 0) return sawContent ? commas + 1 : 0
    } else if (ch === ',' && depth === 1) commas++
    else if (depth === 1 && !/\s/.test(ch)) sawContent = true
  }
  return null
}

function checkDuplicateGoFuncs(sources) {
  const places = new Map()
  for (const [file, src] of sources) {
    for (const m of src.matchAll(GO_FUNC_RE)) {
      const name = m[1]
      if (!places.has(name)) places.set(name, { files: new Set(), pkgs: new Set(), arities: new Set() })
      const at = places.get(name)
      at.files.add(file)
      at.pkgs.add(goPackageOf(file))
      const arity = goArity(src, m.index + m[0].length - 1)
      if (arity !== null) at.arities.add(arity)
    }
  }
  const findings = []
  for (const [name, at] of places) {
    if (at.pkgs.size < MIN_GO_PACKAGES) continue
    findings.push({
      name,
      packages: at.pkgs.size,
      files: [...at.files].sort(),
      // More than one arity means these are re-implementations of one idea rather than
      // copies of one function - the `decodeExp` shape, on the Go side.
      reimplemented: at.arities.size > 1,
    })
  }
  return findings.sort((a, b) => b.packages - a.packages || a.name.localeCompare(b.name))
}

// ---------------------------------------------------------------------------
// Severity
//
// Not a feeling. Each factor below is a way one of these duplicates actually breaks
// something, so the ranking is "how silently does this fail", not "how many copies are
// there".
//
//   Silent at RUNTIME beats silent at COMPILE time. A duplicated VALUE, such as a
//   session cookie's name, fails with nothing to catch it: rename one side and the
//   server sets a cookie the browser never reads, so every user is logged out while
//   the build, the types and every test stay green. A duplicated TYPE at least has a
//   compiler watching part of it.
//
//   A DRIFTED copy beats an exact one. Two identical unions are a maintenance cost;
//   one that has fallen behind is already wrong, unable to express the members the
//   component it feeds actually renders.
//
//   AUTO-IMPORT scope beats explicit imports. A framework that publishes
//   `composables/*` and `utils/*` globally resolves a twice-exported name for you,
//   silently, and the wrong pick type-checks whenever the shapes are compatible. That
//   is how a bare composable call ends up bound to permanently stale store data.
//
//   CROSSING app/server or app/e2e beats staying inside one tree. Those boundaries
//   cannot import each other, so a copy is the only option and nothing links them.
//
//   Same file is the mildest: still worth fixing, but no other module can be misled.
// ---------------------------------------------------------------------------

/** Dirs Nuxt publishes app-wide (nuxt.config `imports.dirs`, plus Nuxt's own defaults
 *  for the top level of `composables/` and `utils/`). A duplicate name here is chosen
 *  for the caller rather than by them. */
const AUTO_IMPORT_RE = /webapp\/app\/(?:composables\/(?:data|access|forms|ui|features)\/|utils\/(?:api|format|mutations|dom)\/|composables\/[^/]+$|utils\/[^/]+$)/

const treeOf = (f) => {
  if (f.startsWith('api/')) return 'api'
  if (f.includes('/e2e/')) return 'e2e'
  if (f.startsWith('webapp/server/')) return 'server'
  if (f.startsWith('shared/') || f.startsWith('webapp/shared/')) return 'shared'
  if (f.includes('/scripts/')) return 'scripts'
  return 'app'
}

const LEVELS = [[7, 'HIGH'], [4, 'MEDIUM']]
const levelFor = score => LEVELS.find(([min]) => score >= min)?.[1] ?? 'LOW'

/** Score one normalised finding, collecting the reasons so the report can show them. */
function scoreFinding(f) {
  const files = [...new Set(f.sites.map(s => s.file))]
  const trees = [...new Set(files.map(treeOf))]
  const why = []
  let score = 0

  if (f.check === 'value') {
    score += 6
    why.push('a duplicated VALUE fails at runtime with nothing to catch it')
  } else if (f.check === 'subset') {
    // A HYPOTHESIS, not a fact, and scored accordingly: a narrower palette is often
    // deliberate, since a status that must never take one of the colours is a
    // constraint rather than drift. Only escalate when both sides carry the SAME
    // identifier, which is
    // one concept provably diverging rather than two that merely overlap.
    score += 3
    why.push(`inferred drift: the narrower copy cannot express ${f.missing.join(', ')}`)
    const bare = n => n.replace(/ \((?:narrower|wider)\)$/, '')
    const narrow = new Set(f.sites.filter(s => s.name.endsWith('(narrower)')).map(s => bare(s.name)))
    const wide = new Set(f.sites.filter(s => s.name.endsWith('(wider)')).map(s => bare(s.name)))
    // Only a COMPOUND name counts as the same concept. A single generic word (`Size`,
    // `variant`, `Color`, `Side`) is what every component in a library calls its own
    // local scale, and a modal having more sizes than a button is a design decision,
    // not drift. A compound name like `BadgeVariant` asserts a domain, so two of those
    // diverging is a real disagreement.
    const isDomainName = n => /[a-z][A-Z]/.test(n)
    const shared = [...narrow].filter(n => wide.has(n) && isDomainName(n))
    if (shared.length) {
      score += 3
      why.push(`same domain identifier (${shared.join(', ')}) on both sides, so one concept is diverging`)
    }
  } else if (f.check === 'name') {
    score += 3
    why.push('one name, two declarations: the import site decides which')
  } else if (f.check === 'godecl') {
    // Silent in the same way the cookie name was, one level up: nothing links the
    // copies, so a fix applied to one leaves the rest wrong and everything still
    // compiles. Go's own rules guarantee the silence - `internal/service/*` packages
    // cannot see each other's unexported names, so there is no import to grep for.
    score += 3
    why.push('one helper restated per package: a fix to one copy leaves the rest wrong')
    if (f.reimplemented) {
      // Not copies of one function but separate answers to one question, which is the
      // case no clone detector can reach at any threshold: `displayName` is written
      // three ways across four packages.
      score += 2
      why.push('the copies do not even agree on an argument count, so these are re-implementations rather than copies')
    }
    if (files.length >= 8) {
      // Count is the only drift signal this tool has here - it reads declaration heads,
      // never bodies, so it cannot tell an in-sync copy from one that has already
      // diverged (`mustJSON` had, at six). Treat a large set as unreviewable rather
      // than as evidence a small one is safe.
      score += 2
      why.push(`${files.length} packages restate it, past the point the set is reviewable as a whole`)
    }
  } else if (f.check === 'union' || f.check === 'shape') {
    score += 2
    why.push('one vocabulary restated, so a change has to be made twice')
  }

  if (files.length === 1) {
    score -= 2
    why.push('both in ONE file, so no other module can be misled')
  } else {
    if (files.some(x => AUTO_IMPORT_RE.test(x)) && f.check === 'name') {
      score += 3
      why.push('auto-import scope: a bare call binds one of them silently')
    }
    if (trees.length > 1) {
      score += 2
      why.push(`crosses ${trees.join(' / ')}, which cannot import each other`)
    }
    if (files.length >= 4) {
      score += 2
      why.push(`${files.length} copies`)
    } else if (files.length === 3) {
      score += 1
      why.push('3 copies')
    }
  }

  // A shared vocabulary restated locally is worse than two local copies: `types/`
  // and `shared/` exist precisely to be the one declaration.
  if (files.length > 1 && files.some(x => /\/types\//.test(x) || treeOf(x) === 'shared')) {
    score += 1
    why.push('one copy sits in a types/shared module that should own it')
  }

  return { score, level: levelFor(score), why }
}

/** Flatten every check into one comparable list, worst first. */
function rankFindings({ exports_, literals, unions, interfaces, goFuncs = [] }) {
  const out = []
  for (const f of literals) {
    out.push({ check: 'value', title: JSON.stringify(f.value), sites: f.sites })
  }
  for (const f of goFuncs) {
    out.push({
      check: 'godecl',
      title: `${f.name} — declared in ${f.packages} packages`,
      reimplemented: f.reimplemented,
      sites: f.files.map(file => ({ file, name: f.name })),
    })
  }
  for (const f of unions.subsets) {
    out.push({
      check: 'subset',
      title: `missing ${f.missing.join(', ')}`,
      missing: f.missing,
      sites: [...f.smaller.map(s => ({ file: s.file, name: `${s.label ?? '(inline)'} (narrower)` })),
        ...f.larger.map(s => ({ file: s.file, name: `${s.label ?? '(inline)'} (wider)` }))],
    })
  }
  for (const f of exports_) {
    out.push({ check: 'name', title: f.name, sites: f.files.map(file => ({ file, name: f.name })) })
  }
  for (const f of unions.exact) {
    out.push({
      check: 'union',
      title: `${f.members.length} members: ${f.members.join(' | ')}`,
      sites: f.sites.map(s => ({ file: s.file, name: s.label ?? '(inline)' })),
    })
  }
  for (const f of interfaces) {
    out.push({
      check: 'shape',
      title: `{${f.fields.join(', ')}}`,
      sites: f.sites.map(s => ({ file: s.file, name: s.name })),
    })
  }
  for (const f of out) Object.assign(f, scoreFinding(f))
  return out.sort((a, b) => b.score - a.score
    || b.sites.length - a.sites.length
    || a.title.localeCompare(b.title))
}

// ---------------------------------------------------------------------------
// Report
// ---------------------------------------------------------------------------

/**
 * Read a config file naming the roots to scan and the paths to skip.
 *
 * Both are facts about one repository's layout, so they are supplied rather than
 * compiled in. A missing file is not an error: an explicit `--config` that does not
 * exist is, but the default path being absent just means a repository has not
 * written one and the defaults apply.
 */
function loadConfig(configPath, explicit) {
  if (!fs.existsSync(configPath)) {
    if (explicit) {
      console.error(`dupescan: config file not found: ${configPath}`)
      process.exit(2)
    }
    return {}
  }
  try {
    return JSON.parse(fs.readFileSync(configPath, 'utf8'))
  } catch (err) {
    console.error(`dupescan: cannot parse ${configPath}: ${err.message}`)
    process.exit(2)
  }
}

/** Where the scan looks when `--config` is not given: a bare filename in the working
 *  directory. Deliberately not a path into some directory this tool has opinions
 *  about -- where a repository keeps its tooling config is that repository's business,
 *  and a default naming one project's layout is wrong everywhere else. A repository
 *  that files it somewhere else passes `--config`. */
const DEFAULT_CONFIG_PATH = 'dupescan.json'

function parseArgs(argv) {
  const roots = []
  let json = false
  let strict = false
  let configPath = DEFAULT_CONFIG_PATH
  let explicitConfig = false
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === '--json') json = true
    else if (argv[i] === '--strict') strict = true
    else if (argv[i] === '--root') roots.push(argv[++i])
    else if (argv[i] === '--config') { configPath = argv[++i]; explicitConfig = true }
  }
  const cfg = loadConfig(configPath, explicitConfig)
  if (!roots.length && !(Array.isArray(cfg.roots) && cfg.roots.length)) {
    console.error(
      `dupescan: no roots configured (looked in ${configPath}); scanning ${DEFAULT_ROOTS.join(', ')}. `
      + 'Name the trees to scan with --root, or in a config file, to make this faster and narrower.',
    )
  }
  // `--root` wins over the config file, which wins over the fallback: the flag is
  // how someone narrows a single run without editing anything.
  const configured = Array.isArray(cfg.roots) && cfg.roots.length ? cfg.roots : null
  return {
    roots: roots.length ? roots : (configured ?? DEFAULT_ROOTS),
    skipPaths: Array.isArray(cfg.skip_paths) ? cfg.skip_paths : DEFAULT_SKIP_PATHS,
    json,
    strict,
  }
}

function main() {
  const { roots, skipPaths, json, strict } = parseArgs(process.argv.slice(2))
  const skip = skipPathMatcher(skipPaths)
  const files = roots.flatMap(r => walk(r, [], skip))
  const sources = files.map((f) => {
    const raw = fs.readFileSync(f, 'utf8')
    const code = f.endsWith('.vue') ? extractVueScript(raw) : raw
    return [toPosix(path.relative(process.cwd(), f)), stripComments(code)]
  })

  // Checks 1-4 key on TypeScript declaration syntax and Check 5 on Go's, so each is fed
  // only the surface it was written for. Go cannot produce an `export const`, but it
  // does have `interface` and `|`, and feeding it to the TS checks could only ever add
  // noise to them.
  const tsSources = sources.filter(([f]) => !isGo(f))
  const goSources = sources.filter(([f]) => isGo(f))

  const exports_ = checkDuplicateExports(tsSources)
  const literals = checkDuplicateLiteralConsts(tsSources)
  const unions = checkDuplicateUnions(tsSources)
  const interfaces = checkDuplicateInterfaces(tsSources)
  const goFuncs = checkDuplicateGoFuncs(goSources)

  const total = exports_.length + literals.length + unions.exact.length
    + unions.subsets.length + interfaces.length + goFuncs.length

  const ranked = rankFindings({ exports_, literals, unions, interfaces, goFuncs })

  if (json) {
    process.stdout.write(JSON.stringify({ scanned: sources.length, findings: ranked }, null, 2) + '\n')
  } else {
    const CHECK_LABEL = {
      value: 'same VALUE, two names',
      subset: 'DRIFTED copy (subset)',
      name: 'same NAME, two modules',
      godecl: 'same Go helper restated per package',
      union: 'same union restated',
      shape: 'same record shape restated',
    }
    const counts = ranked.reduce((acc, f) => ({ ...acc, [f.level]: (acc[f.level] ?? 0) + 1 }), {})
    const lines = []
    lines.push(`dupescan: ${sources.length} files across ${roots.join(', ')}`)
    lines.push(`${ranked.length} findings — ${['HIGH', 'MEDIUM', 'LOW'].map(l => `${counts[l] ?? 0} ${l}`).join(', ')}`)
    lines.push('worst first; severity is how SILENTLY it fails, not how many copies exist')
    lines.push('')
    for (const f of ranked) {
      lines.push(`[${f.level}] ${CHECK_LABEL[f.check]} — ${f.title}`)
      for (const s of f.sites) lines.push(`      ${s.name}  ${s.file}`)
      lines.push(`      why: ${f.why.join('; ')}`)
      lines.push('')
    }
    process.stdout.write(lines.join('\n'))
  }

  process.exit(strict && total > 0 ? 1 : 0)
}

/** Only scan when invoked as a command. The checks are imported directly by the test,
 *  which feeds them source strings instead of a tree on disk. */
const invokedDirectly = process.argv[1]
  && path.basename(process.argv[1]) === 'dupescan.mjs'
if (invokedDirectly) main()

export {
  checkDuplicateExports,
  skipPathMatcher,
  checkDuplicateLiteralConsts,
  checkDuplicateUnions,
  checkDuplicateInterfaces,
  checkDuplicateGoFuncs,
  extractVueScript,
  isScanned,
  isTestPath,
  rankFindings,
  scoreFinding,
  stripComments,
}
