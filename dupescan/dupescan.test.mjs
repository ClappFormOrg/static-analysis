/**
 * Tests for the cross-file duplicate-declaration checks.
 *
 *   node --test dupescan/
 *
 * Node's own runner and `node:assert`, no dependency to install - the scanner is
 * stdlib-only by design and its tests keep that property.
 *
 * Each check is fed source strings rather than a tree on disk, so a case is readable
 * next to its assertion. The cases are drawn from a real duplication
 * pass, plus, for each check, a legitimate lookalike it must NOT report - a detector
 * that fires on ordinary code gets skimmed, and a skimmed detector is worthless
 * (the dead accept-state lint rule that rule replaced is exactly that lesson).
 */

import { test, after } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import {
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
} from './dupescan.mjs'

/** A ranked finding, as `rankFindings` shapes them before scoring. */
const finding = (check, sites, extra = {}) => scoreFinding({
  check,
  title: 't',
  sites: sites.map(([file, name]) => ({ file, name })),
  ...extra,
})

const src = entries => entries.map(([file, text]) => [file, stripComments(text)])

test('duplicate exports: one name declared by two modules is reported', () => {
  const found = checkDuplicateExports(src([
    ['app/utils/api/jwt.ts', 'export function decodeExp(t: string) { return 1 }'],
    ['server/utils/auth-cookies.ts', 'export function decodeExp(t: string) { return 2 }'],
  ]))
  assert.equal(found.length, 1)
  assert.equal(found[0].name, 'decodeExp')
  assert.deepEqual(found[0].files, ['app/utils/api/jwt.ts', 'server/utils/auth-cookies.ts'])
})

test('duplicate exports: a re-export is not a second declaration', () => {
  // The shape F10 ended up in: one declaration in shared/, forwarded by both sides.
  const found = checkDuplicateExports(src([
    ['shared/auth-session.ts', `export const SESSION_COOKIE = 'app-auth-session'`],
    ['app/composables/access/useAuth.ts', 'export { SESSION_COOKIE }'],
    ['server/utils/auth-cookies.ts', 'export { SESSION_COOKIE }'],
  ]))
  assert.deepEqual(found, [])
})

test('duplicate exports: a symbol named only in a comment does not count', () => {
  const found = checkDuplicateExports(src([
    ['a.ts', 'export const realThing = 1'],
    ['b.ts', '// export const realThing = 1 (removed, see a.ts)\nexport const other = 2'],
  ]))
  assert.deepEqual(found, [])
})

test('duplicate literal consts: one value under two names in two files is reported', () => {
  const found = checkDuplicateLiteralConsts(src([
    ['app/composables/access/useAuth.ts', `export const SESSION_KEY = 'app-auth-session'`],
    ['server/utils/auth-cookies.ts', `export const SESSION_COOKIE = 'app-auth-session'`],
  ]))
  assert.equal(found.length, 1)
  assert.equal(found[0].value, 'app-auth-session')
  assert.deepEqual(found[0].sites.map(s => s.name), ['SESSION_KEY', 'SESSION_COOKIE'])
})

test('duplicate literal consts: a short value is not worth reporting', () => {
  // Coincidence far outweighs concept below the length floor.
  const found = checkDuplicateLiteralConsts(src([
    ['a.ts', `export const A = 'id'`],
    ['b.ts', `export const B = 'id'`],
  ]))
  assert.deepEqual(found, [])
})

test('duplicate literal consts: the same name re-exported is not two values', () => {
  const found = checkDuplicateLiteralConsts(src([
    ['shared/k.ts', `export const SESSION_COOKIE = 'app-auth-session'`],
    ['app/re.ts', 'export { SESSION_COOKIE }'],
  ]))
  assert.deepEqual(found, [])
})

test('duplicate literal consts: an unexported const holds a value just as loudly', () => {
  // `export` says who may IMPORT the name; it says nothing about the value, and this
  // check is about the value. `/api/v1/widgets` is written three times in
  // `composables/api/widgets/*` behind unexported consts, and `UNITS_PER_YEAR` sits in
  // `composables/portal/order-scope.ts` exported and in
  // `components/widgets/organisms/reference-year-create.ts` not - each invisible while
  // the pattern was anchored to `^export`.
  const found = checkDuplicateLiteralConsts(src([
    ['app/composables/portal/order-scope.ts', `export const DEFAULT_TOTAL_UNIT = 'UNITS_PER_YEAR'`],
    ['app/components/orders/organisms/reference-year-create.ts', `const DEFAULT_TOTAL_UNIT = 'UNITS_PER_YEAR'`],
  ]))
  assert.equal(found.length, 1)
  assert.equal(found[0].value, 'UNITS_PER_YEAR')
  assert.deepEqual(found[0].sites.map(s => s.file), [
    'app/components/orders/organisms/reference-year-create.ts',
    'app/composables/portal/order-scope.ts',
  ])
})

test('duplicate literal consts: a backtick literal is a value when it has no substitution', () => {
  const found = checkDuplicateLiteralConsts(src([
    ['a.ts', 'export const K = `app-auth-session`'],
    ['b.ts', `const J = 'app-auth-session'`],
  ]))
  assert.equal(found.length, 1)
  assert.equal(found[0].value, 'app-auth-session')
})

test('duplicate literal consts: a template with a substitution is a recipe, not a value', () => {
  // Two templates sharing a prefix are not one concept: what they produce depends on
  // what is interpolated, which this tool cannot see.
  const found = checkDuplicateLiteralConsts(src([
    ['a.ts', 'const A = `/api/v1/widgets/${id}`'],
    ['b.ts', 'const B = `/api/v1/widgets/${id}`'],
  ]))
  assert.deepEqual(found, [])
})

test('duplicate exports: an unexported name is not a second declaration', () => {
  // The asymmetry with the value check above, and it is deliberate. This check's claim
  // is that the IMPORT SITE decides which declaration wins, so it needs an import site
  // to exist. TypeScript scopes an unexported name to its module: `Translate` is a
  // local alias in seven component modules and none of them is in conflict. Dropping
  // the keyword here took the repository run from 63 findings to 1386.
  const found = checkDuplicateExports(src([
    ['app/components/widgets/organisms/grid-connection-options.ts', 'type Translate = (k: string) => string'],
    ['app/components/widgets/organisms/widget-reference-data.ts', 'type Translate = (k: string) => string'],
  ]))
  assert.deepEqual(found, [])
})

test('duplicate exports: every name of a multi-declarator export is declared', () => {
  // One statement, two exports. Reading a name per line exported `ROW_H` invisibly.
  const found = checkDuplicateExports(src([
    ['app/components/orders/charts/a.vue', 'export const CHIP_H = 14, ROW_H = 16'],
    ['app/components/orders/charts/b.vue', 'export const ROW_H = 20'],
  ]))
  assert.equal(found.length, 1)
  assert.equal(found[0].name, 'ROW_H')
})

test('duplicate exports: a default parameter is not a second declarator', () => {
  // `storageColumns` in `widget-reference-data.ts` reads exactly like a two-declarator
  // statement to a comma-splitting regex, and `showTemperature` is a parameter that no
  // module exports. Attributing it to this file would be a finding against source that
  // does not exist.
  const found = checkDuplicateExports(src([
    ['app/components/widgets/organisms/widget-reference-data.ts', 'export const storageColumns = (t: Translate, showTemperature = false) => []'],
    ['app/other.ts', 'export const showTemperature = true'],
  ]))
  assert.deepEqual(found, [])
})

test('duplicate unions: the same member set counts however it is written or ordered', () => {
  const { exact } = checkDuplicateUnions(src([
    ['app/utils/review-state.ts', `export type ReviewBadgeVariant = 'gray' | 'success' | 'violet'`],
    ['app/types/review.ts', `  variant: 'violet' | 'gray' | 'success'`],
  ]))
  assert.equal(exact.length, 1)
  assert.equal(exact[0].sites.length, 2)
  assert.deepEqual(
    exact[0].sites.map(s => s.label).sort(),
    ['ReviewBadgeVariant', 'variant'],
  )
})

test('duplicate unions: a two-member union is too small to be a vocabulary', () => {
  const { exact } = checkDuplicateUnions(src([
    ['a.ts', `type A = 'on' | 'off'`],
    ['b.ts', `type B = 'on' | 'off'`],
  ]))
  assert.deepEqual(exact, [])
})

test('duplicate unions: a narrower copy surfaces as a subset, naming what it lacks', () => {
  // The F2 failure: a copy that fell behind cannot match exactly, so the exact check
  // is blind to precisely the drift that matters.
  const { exact, subsets } = checkDuplicateUnions(src([
    ['app/components/UiBadge.vue.ts', `type Variant = 'gray' | 'success' | 'violet' | 'teal'`],
    ['app/utils/approvals-status.ts', `export type StatusBadgeVariant = 'gray' | 'success' | 'violet'`],
  ]))
  assert.deepEqual(exact, [])
  assert.equal(subsets.length, 1)
  assert.deepEqual(subsets[0].missing, ['teal'])
})

test('duplicate interfaces: one field set under two names is reported', () => {
  const found = checkDuplicateInterfaces(src([
    ['app/types/entities.ts', `export interface OrderTotalsRecord {
      type: string
      amount: number
      unit: string
      unitId?: string
    }`],
    ['app/types/portal/orders.ts', `export interface OrderTotalsRecord {
      type: string
      amount: number
      unit: string
      unitId?: string
    }`],
  ]))
  assert.equal(found.length, 1)
  assert.deepEqual(found[0].sites.map(s => s.file), [
    'app/types/entities.ts',
    'app/types/portal/orders.ts',
  ])
})

test('duplicate interfaces: a small coincidental shape is not reported', () => {
  const found = checkDuplicateInterfaces(src([
    ['a.ts', 'export interface A { id: string\n name: string }'],
    ['b.ts', 'export interface B { id: string\n name: string }'],
  ]))
  assert.deepEqual(found, [])
})

test('duplicate interfaces: a type alias and an interface are the same record', () => {
  // TypeScript treats the two forms as interchangeable and so does every caller, so
  // which keyword the author reached for cannot decide whether a duplicate is visible.
  // The four card-option types sharing `{value, label, icon, description}` are a mix of
  // both forms.
  const found = checkDuplicateInterfaces(src([
    ['app/components/forms/molecules/UiCardSelect.vue', `export type CardSelectOption = {
      value: string
      label: string
      icon: string
      description: string
    }`],
    ['app/types/access.ts', `export interface ApproverTypeOption {
      value: string
      label: string
      icon: string
      description: string
    }`],
  ]))
  assert.equal(found.length, 1)
  assert.deepEqual(found[0].sites.map(s => s.name), ['CardSelectOption', 'ApproverTypeOption'])
})

test('duplicate interfaces: a record written on ONE line keeps its last field', () => {
  // The closing brace has to flush the pending field. Without that the final field is
  // dropped, which silently took a four-field one-liner under MIN_FIELDS and out of the
  // check entirely. A multi-line declaration hid it: its `}` sits on its own line, so
  // the preceding newline had already flushed.
  const found = checkDuplicateInterfaces(src([
    ['a.ts', 'export interface A { id: string; name: string; foo: number; bar: number }'],
    ['b.ts', 'export type B = { id: string; name: string; foo: number; bar: number }'],
  ]))
  assert.equal(found.length, 1)
  assert.deepEqual(found[0].fields, ['id', 'name', 'foo', 'bar'])
})

test('duplicate interfaces: a mapped type is not a record of field names', () => {
  // `{ [K in T]: string }` has no field names to compare - `K` is a binding, not a
  // member - so matching it would key two unrelated maps together.
  const found = checkDuplicateInterfaces(src([
    ['a.ts', 'export type A = { [K in Pathway]: string }'],
    ['b.ts', 'export type B = { [K in Pathway]: string }'],
  ]))
  assert.deepEqual(found, [])
})

test('vue: only script content is read, never the template or styles', () => {
  const sfc = `<script setup lang="ts">
type Variant = 'gray' | 'success' | 'violet'
</script>

<template>
  <span :class="styles">{{ label }}</span>
</template>

<style scoped>
.badge { color: red }
</style>`
  const code = extractVueScript(sfc)
  assert.match(code, /type Variant/)
  assert.doesNotMatch(code, /template|style|scoped/)
})

test('vue: both script blocks contribute', () => {
  // A component may pair `<script setup>` with a plain `<script>` for a name/option.
  const sfc = `<script lang="ts">
export const COMPONENT_NAME = 'UiBadge'
</script>
<script setup lang="ts">
type Variant = 'gray' | 'success'
</script>`
  const code = extractVueScript(sfc)
  assert.match(code, /COMPONENT_NAME/)
  assert.match(code, /type Variant/)
})

test('vue: a template-only component yields nothing rather than throwing', () => {
  assert.equal(extractVueScript('<template><p>hi</p></template>'), '')
})

test('vue: an SFC union is compared against a .ts one - the F2 case', () => {
  // The palette copies lived in SFC script blocks and in .ts files, so a scan that
  // read only .ts reported none of them.
  const { exact } = checkDuplicateUnions(src([
    ['app/components/UiBadge.vue', extractVueScript(`<script setup lang="ts">
type Variant = 'gray' | 'success' | 'violet'
</script>`)],
    ['app/utils/review-state.ts', `export type ReviewBadgeVariant = 'gray' | 'success' | 'violet'`],
  ]))
  assert.equal(exact.length, 1)
  assert.deepEqual(exact[0].sites.map(s => s.file).sort(), [
    'app/components/UiBadge.vue',
    'app/utils/review-state.ts',
  ])
})

test('duplicate interfaces: a nested object type does not become its own record', () => {
  // Brace-depth attribution: `location`'s inner fields belong to `location`.
  const found = checkDuplicateInterfaces(src([
    ['a.ts', `export interface Widget {
      id: string
      name: string
      slug: string
      location: { lat: number; lng: number }
    }`],
  ]))
  assert.deepEqual(found, [])
})

// --- Go: one helper restated per package -------------------------------------
// `internal/service/*` packages cannot see each other's unexported names, so the API
// restates helpers instead of sharing them. The cases below are the real ones from the
// Go-side duplication shapes, plus the lookalikes the check must stay quiet on -
// and on the Go side those matter more, because `New` is declared in 33 packages and
// reporting it would make the loudest finding in the run a false positive.

test('go funcs: one helper restated in four packages is reported, naming every file', () => {
  const found = checkDuplicateGoFuncs(src([
    ['api/internal/service/invoice/helpers.go', 'func decodeCursor(s string) (string, error) {\n\treturn s, nil\n}'],
    ['api/internal/service/widget/helpers.go', 'func decodeCursor(s string) (string, error) {\n\treturn s, nil\n}'],
    ['api/internal/service/order/read.go', 'func decodeCursor(c string) (string, error) {\n\treturn c, nil\n}'],
    ['api/internal/service/comment/comment.go', 'func decodeCursor(v string) (string, error) {\n\treturn v, nil\n}'],
  ]))
  assert.equal(found.length, 1)
  assert.equal(found[0].name, 'decodeCursor')
  assert.equal(found[0].packages, 4)
  assert.deepEqual(found[0].files, [
    'api/internal/service/comment/comment.go',
    'api/internal/service/invoice/helpers.go',
    'api/internal/service/order/read.go',
    'api/internal/service/widget/helpers.go',
  ])
})

test('go funcs: three packages is below the floor', () => {
  // Two or three packages naming a short helper alike is as likely convention as
  // concept. Four is where the API's real cases start.
  const found = checkDuplicateGoFuncs(src([
    ['api/internal/service/a/a.go', 'func codeOf(err error) int { return 0 }'],
    ['api/internal/service/b/b.go', 'func codeOf(err error) int { return 0 }'],
    ['api/internal/service/c/c.go', 'func codeOf(err error) int { return 0 }'],
  ]))
  assert.deepEqual(found, [])
})

test('go funcs: an exported constructor in every package is convention, not duplication', () => {
  // `New` is declared in as many packages as there are constructors. Counting exported names would
  // put Go's constructor idiom at the top of the report, and a detector whose loudest
  // finding is noise gets skimmed.
  const found = checkDuplicateGoFuncs(src([
    ['api/internal/service/invoice/invoice.go', 'func New(db *gorm.DB) *Service { return nil }'],
    ['api/internal/service/widget/widget.go', 'func New(db *gorm.DB) *Service { return nil }'],
    ['api/internal/service/comment/comment.go', 'func New(db *gorm.DB) *Service { return nil }'],
    ['api/internal/service/order/order.go', 'func New(db *gorm.DB) *Service { return nil }'],
    ['api/internal/service/terms/terms.go', 'func New(db *gorm.DB) *Service { return nil }'],
  ]))
  assert.deepEqual(found, [])
})

test('go funcs: a method is not a package-level function of the same name', () => {
  // `withTx` is a method on each package's own `*Service`, so the name is namespaced by
  // its receiver. Folding methods in also surfaces `resolve` on `*Resolver` and on
  // `*boundSet`, which are unrelated.
  const found = checkDuplicateGoFuncs(src([
    ['api/internal/service/invoice/invoice.go', 'func (s *Service) withTx(ctx context.Context) error { return nil }'],
    ['api/internal/service/widget/widget.go', 'func (s *Service) withTx(ctx context.Context) error { return nil }'],
    ['api/internal/service/comment/comment.go', 'func (s *Service) withTx(ctx context.Context) error { return nil }'],
    ['api/internal/service/draft/promote.go', 'func (p *Promoter) withTx(ctx context.Context) error { return nil }'],
  ]))
  assert.deepEqual(found, [])
})

test('go funcs: two files in ONE package are one declaration site', () => {
  // Build-tagged halves of a package are not two packages, and Go forbids a package
  // declaring the same function twice anyway. Four files, two packages, no finding.
  const found = checkDuplicateGoFuncs(src([
    ['api/internal/service/widget/sweep_linux.go', 'func sweep(ctx context.Context) error { return nil }'],
    ['api/internal/service/widget/sweep_windows.go', 'func sweep(ctx context.Context) error { return nil }'],
    ['api/internal/service/comment/sweep_linux.go', 'func sweep(ctx context.Context) error { return nil }'],
    ['api/internal/service/comment/sweep_windows.go', 'func sweep(ctx context.Context) error { return nil }'],
  ]))
  assert.deepEqual(found, [])
})

test('go funcs: copies that disagree on argument count are re-implementations', () => {
  // The real `displayName`: `(*store.User)`, `(first, last string)` and
  // `(store.UserDisplay)`. Three answers to one question, sharing no tokens, so no
  // clone detector reaches it at any threshold - the `decodeExp` shape on the Go side.
  const found = checkDuplicateGoFuncs(src([
    ['api/internal/service/request/access_grants.go', 'func displayName(u *store.User) string { return "" }'],
    ['api/internal/service/identity/notify.go', 'func displayName(first, last string) string { return "" }'],
    ['api/internal/service/account/names.go', 'func displayName(d store.UserDisplay) string { return "" }'],
    ['api/internal/service/widget/names.go', 'func displayName(u *store.User) string { return "" }'],
  ]))
  assert.equal(found.length, 1)
  assert.equal(found[0].reimplemented, true)
})

test('go funcs: renamed parameters at the same arity are copies, not re-implementations', () => {
  // The real `hasPath`, whose copies differ only in what they call their arguments.
  // Comparing signature TEXT would call every finding a re-implementation and the
  // signal would mean nothing.
  const found = checkDuplicateGoFuncs(src([
    ['api/internal/service/invoice/mask.go', 'func hasPath(paths []string, p string) bool { return false }'],
    ['api/internal/service/widget/mask.go', 'func hasPath(fm []string, want string) bool { return false }'],
    ['api/internal/service/comment/mask.go', 'func hasPath(list []string, target string) bool { return false }'],
    ['api/internal/service/order/mask.go', 'func hasPath(fields []string, name string) bool { return false }'],
  ]))
  assert.equal(found.length, 1)
  assert.equal(found[0].reimplemented, false)
})

test('go funcs: generic and zero-argument declarations are counted correctly', () => {
  const found = checkDuplicateGoFuncs(src([
    ['api/internal/service/a/a.go', 'func meta() context.Context { return nil }'],
    ['api/internal/service/b/b.go', 'func meta() context.Context { return nil }'],
    ['api/internal/service/c/c.go', 'func meta() context.Context { return nil }'],
    ['api/internal/service/d/d.go', 'func meta[T any]() context.Context { return nil }'],
  ]))
  assert.equal(found.length, 1)
  assert.equal(found[0].packages, 4)
  assert.equal(found[0].reimplemented, false) // both forms take no arguments
})

test('go funcs: a constraint over a composite type is still a declaration', () => {
  // `decodeSealedJSON[M ~map[string]V, V any]` is declared in `service/request`.
  // A type-parameter list matched as `[^\]]*` stops at the `]` of `[string]`, so the
  // whole declaration was missed rather than mismeasured.
  const found = checkDuplicateGoFuncs(src([
    ['api/internal/service/a/a.go', 'func decodeSealedJSON[M ~map[string]V, V any](j datatypes.JSON) M { return nil }'],
    ['api/internal/service/b/b.go', 'func decodeSealedJSON[M ~map[string]V, V any](j datatypes.JSON) M { return nil }'],
    ['api/internal/service/c/c.go', 'func decodeSealedJSON[M ~map[string]V, V any](j datatypes.JSON) M { return nil }'],
    ['api/internal/service/d/d.go', 'func decodeSealedJSON[M ~map[string]V, V any](j datatypes.JSON) M { return nil }'],
  ]))
  assert.equal(found.length, 1)
  assert.equal(found[0].packages, 4)
  assert.equal(found[0].reimplemented, false)
})

test('go funcs: a nested func parameter is one argument, not two', () => {
  const found = checkDuplicateGoFuncs(src([
    ['api/internal/service/a/a.go', 'func runInTx(fn func(tx *gorm.DB, id uuid.UUID) error) error { return nil }'],
    ['api/internal/service/b/b.go', 'func runInTx(f func(db *gorm.DB, u uuid.UUID) error) error { return nil }'],
    ['api/internal/service/c/c.go', 'func runInTx(cb func(tx *gorm.DB, x uuid.UUID) error) error { return nil }'],
    ['api/internal/service/d/d.go', 'func runInTx(fn func(tx *gorm.DB, id uuid.UUID) error) error { return nil }'],
  ]))
  assert.equal(found[0].reimplemented, false)
})

test('go funcs: a name that only appears in a comment does not count', () => {
  const found = checkDuplicateGoFuncs(src([
    ['api/internal/service/a/a.go', 'func meta() int { return 1 }'],
    ['api/internal/service/b/b.go', 'func meta() int { return 1 }'],
    ['api/internal/service/c/c.go', 'func meta() int { return 1 }'],
    ['api/internal/service/d/d.go', '// func meta() int - moved to svcutil\nfunc other() int { return 1 }'],
  ]))
  assert.deepEqual(found, [])
})

test('go funcs: a file that does not compile is still scanned', () => {
  // The tool resolves no packages and parses nothing, so a half-written branch reports
  // the same as a green one. That is the property that lets it run pre-commit.
  const found = checkDuplicateGoFuncs(src([
    ['api/internal/service/a/a.go', 'func meta() int { return 1 }'],
    ['api/internal/service/b/b.go', 'func meta() int { return 1 }'],
    ['api/internal/service/c/c.go', 'func meta() int { return 1 }'],
    ['api/internal/service/d/d.go', 'import "fmt"\n\nfunc meta() int {\n\tif x == { // unbalanced, mid-edit\n'],
  ]))
  assert.equal(found.length, 1)
  assert.equal(found[0].packages, 4)
})

test('go funcs: a half-typed signature does not invent an argument count', () => {
  // Counting to end-of-file through an unclosed parameter list would give the fourth
  // copy an arity of its own and report four identical helpers as re-implementations.
  const found = checkDuplicateGoFuncs(src([
    ['api/internal/service/a/a.go', 'func meta(ctx context.Context) int { return 1 }'],
    ['api/internal/service/b/b.go', 'func meta(ctx context.Context) int { return 1 }'],
    ['api/internal/service/c/c.go', 'func meta(ctx context.Context) int { return 1 }'],
    ['api/internal/service/d/d.go', 'func meta(ctx context.Context, extra'],
  ]))
  assert.equal(found.length, 1)
  assert.equal(found[0].reimplemented, false)
})

test('go: .go is scanned and _test.go is not', () => {
  // `adminCtx` and `ctxAs` are declared in 8 and 7 packages, all of them `_test.go`.
  // They are test scaffolding, and reporting them would bury the production findings.
  assert.equal(isScanned('helpers.go'), true)
  assert.equal(isTestPath('api/internal/service/widget/widget_test.go'), true)
  assert.equal(isTestPath('api/internal/service/widget/widget.go'), false)
})

// --- severity ---------------------------------------------------------------
// The ranking claims to order by how SILENTLY a duplicate fails. These pin the
// comparisons that claim rests on, because a mis-calibrated severity is worse than
// none: it sends the reader to the wrong finding first and teaches them to ignore the
// label. Every one of these was wrong in a first draft of the model.

test('severity: a duplicated VALUE across app/server is the worst case', () => {
  const value = finding('value', [
    ['webapp/app/composables/access/useAuth.ts', 'SESSION_KEY'],
    ['webapp/server/utils/auth-cookies.ts', 'SESSION_COOKIE'],
  ])
  const union = finding('union', [
    ['webapp/app/types/a.ts', 'A'],
    ['webapp/app/types/b.ts', 'B'],
  ])
  assert.equal(value.level, 'HIGH')
  assert.ok(value.score > union.score)
})

test('severity: an auto-imported name collision outranks one outside that scope', () => {
  const auto = finding('name', [
    ['webapp/app/composables/data/useX.ts', 'useX'],
    ['webapp/app/composables/pages/useY.ts', 'useX'],
  ])
  const explicit = finding('name', [
    ['webapp/app/components/a/A.vue', 'Thing'],
    ['webapp/app/components/b/B.vue', 'Thing'],
  ])
  assert.ok(auto.score > explicit.score)
  assert.match(auto.why.join(' '), /auto-import/)
})

test('severity: two declarations in ONE file rank below the same thing across files', () => {
  const oneFile = finding('union', [
    ['webapp/app/components/media/atoms/UiMap.vue', 'MapVariant'],
    ['webapp/app/components/media/atoms/UiMap.vue', 'variant'],
  ])
  const twoFiles = finding('union', [
    ['webapp/app/components/media/atoms/UiMap.vue', 'MapVariant'],
    ['webapp/app/utils/map-tiles.ts', 'MapVariant'],
  ])
  assert.ok(oneFile.score < twoFiles.score)
  assert.match(oneFile.why.join(' '), /ONE file/)
})

test('severity: the copy-count ladder scales the cost, and only above two copies', () => {
  // Count is the mild factor in the model - it scales what a fix costs rather than
  // how silently the thing fails - so it has to keep the order 2 < 3 < 4 without
  // ever overtaking a factor that is about silence. The pair is deliberately the
  // quiet case: components/ is outside Nuxt's auto-import dirs and every copy is in
  // one tree, so nothing but the count moves between these three.
  const copies = n => Array.from({ length: n }, (_, i) => [`webapp/app/components/c${i}/X.vue`, 'Thing'])
  const two = finding('name', copies(2))
  const three = finding('name', copies(3))
  const four = finding('name', copies(4))
  assert.ok(two.score < three.score)
  assert.ok(three.score < four.score)
  assert.match(three.why.join(' '), /3 copies/)
  assert.match(four.why.join(' '), /4 copies/)
  // Two is the ordinary case a report is full of, so it earns no count reason at all.
  assert.doesNotMatch(two.why.join(' '), /copies/)
  // And the whole ladder stays below the factors that are about silence: four
  // ordinary copies must not outrank one value duplicated across app and server.
  assert.ok(four.score < finding('value', [
    ['webapp/app/composables/access/useAuth.ts', 'SESSION_KEY'],
    ['webapp/server/utils/auth-cookies.ts', 'SESSION_COOKIE'],
  ]).score)
})

test('severity: a generic single-word name is NOT treated as one concept diverging', () => {
  // `Size` in a button and `Size` in a modal: a modal having more sizes is a design
  // decision. Scoring this as drift is what made the first model flood HIGH.
  const generic = finding('subset', [
    ['webapp/app/components/actions/atoms/UiButton.vue', 'Size (narrower)'],
    ['webapp/app/components/overlays/atoms/UiModal.vue', 'Size (wider)'],
  ], { missing: ['xl'] })
  assert.notEqual(generic.level, 'HIGH')
  assert.doesNotMatch(generic.why.join(' '), /same domain identifier/)
})

test('severity: the SAME compound domain name on both sides does count as diverging', () => {
  const domain = finding('subset', [
    ['webapp/app/utils/approvals-status.ts', 'StatusBadgeVariant (narrower)'],
    ['webapp/app/utils/review-state.ts', 'StatusBadgeVariant (wider)'],
  ], { missing: ['teal', 'violet'] })
  assert.match(domain.why.join(' '), /same domain identifier \(StatusBadgeVariant\)/)
  assert.ok(domain.score > finding('subset', [
    ['webapp/app/a.ts', 'Size (narrower)'],
    ['webapp/app/b.ts', 'Size (wider)'],
  ], { missing: ['xl'] }).score)
})

test('severity: a Go helper restated across many packages outranks one restated across few', () => {
  const goSites = n => Array.from({ length: n }, (_, i) => [`api/internal/service/p${i}/x.go`, 'meta'])
  const many = finding('godecl', goSites(21))
  const few = finding('godecl', goSites(4))
  assert.equal(many.level, 'HIGH')
  assert.ok(many.score > few.score)
  assert.match(many.why.join(' '), /21 packages restate it/)
  assert.match(few.why.join(' '), /a fix to one copy leaves the rest wrong/)
})

test('severity: Go copies that disagree on argument count outrank plain copies', () => {
  // Count is otherwise the only drift signal available here - the scan reads declaration
  // heads, never bodies, so it cannot tell an in-sync copy from one that has already
  // diverged. A differing arity is drift the tool can actually see.
  const sites = Array.from({ length: 4 }, (_, i) => [`api/internal/service/p${i}/x.go`, 'displayName'])
  const reimplemented = finding('godecl', sites, { reimplemented: true })
  const copies = finding('godecl', sites)
  assert.ok(reimplemented.score > copies.score)
  assert.match(reimplemented.why.join(' '), /re-implementations rather than copies/)
  assert.doesNotMatch(copies.why.join(' '), /re-implementations/)
})

test('severity: a subset is only ever an inference, and says so', () => {
  const s = finding('subset', [
    ['webapp/app/a.ts', 'AVariant (narrower)'],
    ['webapp/app/b.ts', 'BVariant (wider)'],
  ], { missing: ['x'] })
  assert.match(s.why.join(' '), /inferred/)
})

// ---------------------------------------------------------------------------
// Skip-path matching
// ---------------------------------------------------------------------------
//
// The generated trees used to be a compiled-in regex. They are config now, so the
// matcher has to hold two properties the constant got for free: no patterns must
// skip NOTHING (an empty alternation would match every path and scan an empty
// tree, reporting a clean repo), and several patterns must be independent rather
// than accidentally anchored to each other.

test('skip paths: no patterns skips nothing', () => {
  const skip = skipPathMatcher([])
  assert.equal(skip('api/gen/go/x.go'), false)
  assert.equal(skip('anything/at/all.ts'), false)
})

test('skip paths: an undefined pattern list skips nothing', () => {
  const skip = skipPathMatcher(undefined)
  assert.equal(skip('api/gen/go/x.go'), false)
})

test('skip paths: a pattern matches only what it names', () => {
  const skip = skipPathMatcher(['^api/(?:gen|\.disabled-gen|migrations)(?:/|$)'])
  assert.equal(skip('api/gen/go/x.go'), true)
  assert.equal(skip('api/.disabled-gen/y.go'), true)
  assert.equal(skip('api/migrations/001.sql'), true)
  assert.equal(skip('api/internal/service/widget/widget.go'), false)
  // Anchored at the root, so a nested directory of the same name still scans.
  assert.equal(skip('webapp/app/gen/x.ts'), false)
})

test('skip paths: several patterns are matched independently', () => {
  const skip = skipPathMatcher(['^api/gen(?:/|$)', '^webapp/generated(?:/|$)'])
  assert.equal(skip('api/gen/x.go'), true)
  assert.equal(skip('webapp/generated/x.ts'), true)
  assert.equal(skip('api/internal/x.go'), false)
})

// ---------------------------------------------------------------------------
// Ranking
// ---------------------------------------------------------------------------
//
// Each check returns its own shape, and the report is one flat list. `rankFindings`
// is the seam between the two: it normalises five different result shapes into
// `{ check, title, sites }`, scores each, and orders them. Two things can go wrong
// here that no check-level test can see. A check can be normalised into a shape the
// scorer does not understand, in which case its findings silently score zero and
// sink to the bottom of the report. And the ordering can stop being worst-first,
// which sends the reader to the wrong finding and teaches them to ignore the label.

/** The five checks' own output shapes, as `main` hands them over: one real case each
 *  from the duplication passes. Kept in one place so a test can drop a key to make a
 *  point about it rather than restating the whole set. */
const allChecks = () => ({
  exports_: [{ name: 'decodeExp', files: ['webapp/app/components/a/A.vue', 'webapp/app/components/b/B.vue'] }],
  literals: [{
    value: 'app-auth-session',
    sites: [
      { file: 'webapp/app/composables/access/useAuth.ts', name: 'SESSION_KEY' },
      { file: 'webapp/server/utils/auth-cookies.ts', name: 'SESSION_COOKIE' },
    ],
  }],
  unions: {
    exact: [{
      members: ['gray', 'success', 'violet'],
      key: 'gray|success|violet',
      sites: [
        { file: 'webapp/app/badge.ts', label: 'BadgeVariant' },
        { file: 'webapp/app/alert.ts', label: 'AlertVariant' },
      ],
    }],
    subsets: [{
      smaller: [{ file: 'webapp/app/approvals-status.ts', label: 'StatusBadgeVariant' }],
      larger: [{ file: 'webapp/app/review-state.ts', label: 'StatusBadgeVariant' }],
      missing: ['teal'],
    }],
  },
  interfaces: [{
    fields: ['type', 'amount', 'unit', 'unitId'],
    sites: [
      { file: 'webapp/app/models/entities.ts', name: 'OrderTotalsRecord' },
      { file: 'webapp/app/models/orders.ts', name: 'OrderTotalsRecord' },
    ],
  }],
  goFuncs: [{
    name: 'meta',
    packages: 21,
    files: Array.from({ length: 21 }, (_, i) => `api/internal/service/p${i}/x.go`),
    reimplemented: false,
  }],
})

test('ranking: all five checks reach the list, scored, worst first', () => {
  const ranked = rankFindings(allChecks())
  // Six findings from five checks: the union check contributes exact matches and
  // subsets separately, because a drifted copy is a different claim from a copy.
  assert.equal(ranked.length, 6)
  assert.deepEqual(
    [...new Set(ranked.map(f => f.check))].sort(),
    ['godecl', 'name', 'shape', 'subset', 'union', 'value'],
  )
  // Every entry carries a score, a level and its reasons: a check normalised into a
  // shape `scoreFinding` does not recognise would come back scoreless and sink.
  for (const f of ranked) {
    assert.equal(typeof f.score, 'number')
    assert.ok(['HIGH', 'MEDIUM', 'LOW'].includes(f.level), `${f.check} has no level`)
    assert.ok(f.why.length > 0, `${f.check} gives no reason`)
  }
  // Worst first, which is the whole claim the report's ordering makes.
  const scores = ranked.map(f => f.score)
  assert.deepEqual(scores, [...scores].sort((a, b) => b - a))
  // And the order the model argues for: a runtime-silent value, then a helper
  // restated past the point the set is reviewable, then inferred drift, then a
  // plain name collision outside auto-import scope.
  assert.deepEqual(ranked.slice(0, 4).map(f => f.check), ['value', 'godecl', 'subset', 'name'])
  assert.deepEqual(ranked.slice(0, 2).map(f => f.level), ['HIGH', 'HIGH'])
})

test('ranking: each title carries the evidence, so no finding needs the source to read', () => {
  const by = Object.fromEntries(rankFindings(allChecks()).map(f => [f.check, f]))
  // A value is quoted, so trailing whitespace or an empty string stays visible.
  assert.equal(by.value.title, '"app-auth-session"')
  // The package count, which is the godecl finding's severity in one number.
  assert.match(by.godecl.title, /^meta .* declared in 21 packages$/)
  assert.equal(by.godecl.sites.length, 21)
  // A subset names what the narrower copy cannot express -- the README calls that
  // the line worth reading in the noisiest section of the report.
  assert.equal(by.subset.title, 'missing teal')
  assert.deepEqual(by.subset.sites.map(s => s.name), [
    'StatusBadgeVariant (narrower)',
    'StatusBadgeVariant (wider)',
  ])
  assert.equal(by.union.title, '3 members: gray | success | violet')
  assert.equal(by.shape.title, '{type, amount, unit, unitId}')
  assert.deepEqual(by.name.sites.map(s => s.name), ['decodeExp', 'decodeExp'])
})

test('ranking: a union declared inline on a field is labelled, not blank', () => {
  // `type Variant = ...` has a name; `variant?: 'a' | 'b' | 'c'` written on a prop
  // does not, and half the palette copies were the second kind. A blank column would
  // read as a parse failure rather than as an inline declaration.
  const ranked = rankFindings({
    exports_: [],
    literals: [],
    interfaces: [],
    unions: {
      exact: [{
        members: ['gray', 'success', 'violet'],
        key: 'gray|success|violet',
        sites: [
          { file: 'webapp/app/components/UiBadge.vue', label: undefined },
          { file: 'webapp/app/badge.ts', label: 'BadgeVariant' },
        ],
      }],
      subsets: [],
    },
  })
  assert.deepEqual(ranked[0].sites.map(s => s.name), ['(inline)', 'BadgeVariant'])
})

test('ranking: a repository with no Go surface still ranks its TypeScript findings', () => {
  // `--root webapp/app` is a supported run and produces no Go results at all, so the
  // Go key is simply absent. Ranking must not fall over on it.
  const { goFuncs, ...noGo } = allChecks()
  assert.ok(goFuncs.length > 0) // the key really is the only thing dropped
  const ranked = rankFindings(noGo)
  assert.equal(ranked.length, 5)
  assert.equal(ranked.some(f => f.check === 'godecl'), false)
  assert.equal(ranked[0].check, 'value')
})

test('ranking: equal scores break the tie by copy count, then alphabetically', () => {
  // Two findings of one kind score the same by construction, so without a tie-break
  // the report's order would follow Map insertion and shuffle between runs. The
  // titles here are chosen to sort the WRONG way round, so a pass means copy count
  // really did decide it.
  const union = (members, sites) => ({ members, key: members.join('|'), sites })
  const byCount = rankFindings({
    exports_: [],
    literals: [],
    interfaces: [],
    unions: {
      exact: [
        union(['z1', 'z2', 'z3'], [
          { file: 'webapp/app/x.ts', label: 'X1' },
          { file: 'webapp/app/x.ts', label: 'X2' },
          { file: 'webapp/app/y.ts', label: 'Y' },
        ]),
        union(['a1', 'a2', 'a3'], [
          { file: 'webapp/app/p.ts', label: 'P' },
          { file: 'webapp/app/q.ts', label: 'Q' },
        ]),
      ],
      subsets: [],
    },
  })
  assert.deepEqual(byCount.map(f => f.score), [2, 2])
  assert.deepEqual(byCount.map(f => f.sites.length), [3, 2])

  // With the count equal too, the title decides, so a report of otherwise identical
  // findings is stable rather than arbitrary.
  const byTitle = rankFindings({
    exports_: [],
    literals: [],
    interfaces: [],
    unions: {
      exact: [
        union(['z1', 'z2', 'z3'], [
          { file: 'webapp/app/p.ts', label: 'P' },
          { file: 'webapp/app/q.ts', label: 'Q' },
        ]),
        union(['a1', 'a2', 'a3'], [
          { file: 'webapp/app/r.ts', label: 'R' },
          { file: 'webapp/app/s.ts', label: 'S' },
        ]),
      ],
      subsets: [],
    },
  })
  assert.deepEqual(byTitle.map(f => f.title), [
    '3 members: a1 | a2 | a3',
    '3 members: z1 | z2 | z3',
  ])
})

// ---------------------------------------------------------------------------
// The command: walking a tree, reading config, and the exit code a gate sees
// ---------------------------------------------------------------------------
//
// The checks take source strings, which is why every test above can be read next to
// its case. The other half of the tool cannot be: which files a walk picks up is a
// fact about a directory, and `--strict`'s answer is an exit code rather than a
// value. `walk`, `loadConfig`, `parseArgs` and `main` are deliberately not exported
// either -- the module's export list is the checks, so a unit test feeds them source
// instead of a tree.
//
// So this section runs the scanner as a command over a fixture repository in a temp
// directory and reads what it printed and what it exited with. Nothing is written
// inside this repository, and the fixture is removed afterwards.

const SCANNER = path.join(path.dirname(fileURLToPath(import.meta.url)), 'dupescan.mjs')

/**
 * A miniature of the repository the scanner was written against.
 *
 * One real case per check -- the cookie value under two names, `decodeExp` in two
 * modules, one palette split between a `.vue` and a `.ts`, one Go helper in four
 * packages -- and, next to each, the lookalike the walk must leave out: a vendored
 * tree, a generated tree, a `_test.go` and a `<template>` block. Every exclusion is
 * asserted through a finding that would gain a copy if it failed, rather than through
 * the file count alone, because a count says nothing about WHICH file went missing.
 */
const FIXTURE = {
  'repo/dupescan.json': JSON.stringify({
    roots: ['src'],
    skip_paths: ['^src/generated(?:/|$)'],
  }),

  'repo/src/auth-app.ts': `export const SESSION_KEY = 'app-auth-session'
export function decodeExp(t: string) { return 1 }
`,
  'repo/src/auth-server.ts': `export const SESSION_COOKIE = 'app-auth-session'
export function decodeExp(t: string) { return 2 }
`,

  // The palette, half of it in an SFC. TplVariant exists only so the template below
  // has something to match: were the template read, that pair would be a finding.
  'repo/src/badge.ts': `export type BadgeVariant = 'gray' | 'success' | 'violet'
export type TplVariant = 'tpl-a' | 'tpl-b' | 'tpl-c'
`,
  'repo/src/UiBadge.vue': `<script setup lang="ts">
type Variant = 'gray' | 'success' | 'violet'
</script>
<template>
  <span>variant: 'tpl-a' | 'tpl-b' | 'tpl-c'</span>
</template>
`,

  // Vendored and generated copies of a scanned declaration. Neither may be read:
  // node_modules by directory name, src/generated by the config's skip_paths.
  'repo/src/node_modules/dup.ts': `export function decodeExp(t: string) { return 3 }
`,
  'repo/src/generated/dup.ts': `export function decodeExp(t: string) { return 4 }
`,

  // Check 5: four packages, plus a fifth that is test scaffolding. `adminCtx` and
  // `ctxAs` are the real reason for that exclusion, at 8 and 7 packages each.
  'repo/src/svc/a/a.go': 'package a\n\nfunc meta() int { return 1 }\n',
  'repo/src/svc/b/b.go': 'package b\n\nfunc meta() int { return 1 }\n',
  'repo/src/svc/c/c.go': 'package c\n\nfunc meta() int { return 1 }\n',
  'repo/src/svc/d/d.go': 'package d\n\nfunc meta() int { return 1 }\n',
  'repo/src/svc/e/e_test.go': 'package e\n\nfunc meta() int { return 1 }\n',

  'repo/bad.json': '{ not json',

  // A checkout that has never written a config, to pin the honest fallback.
  'unconfigured/a.ts': `export const A = 'app-auth-session'\n`,
  'unconfigured/b.ts': `export const B = 'app-auth-session'\n`,

  // A configured checkout with nothing to report, for the other half of --strict.
  'clean/dupescan.json': JSON.stringify({ roots: ['src'] }),
  'clean/src/only.ts': `export const ONLY = 'a-single-declaration'\n`,
}

const fixtureRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'dupescan-'))
for (const [rel, body] of Object.entries(FIXTURE)) {
  const full = path.join(fixtureRoot, rel)
  fs.mkdirSync(path.dirname(full), { recursive: true })
  fs.writeFileSync(full, body)
}
after(() => fs.rmSync(fixtureRoot, { recursive: true, force: true }))

/** Run the scanner as a command from one of the fixture checkouts. */
const scan = (checkout, ...args) => spawnSync(
  process.execPath,
  [SCANNER, ...args],
  { cwd: path.join(fixtureRoot, checkout), encoding: 'utf8' },
)

/** The `--json` payload, which is what a machine consumer of the scan reads. */
const scanJson = (checkout, ...args) => {
  const r = scan(checkout, '--json', ...args)
  assert.equal(r.status, 0, `scan exited ${r.status}: ${r.stderr}`)
  return JSON.parse(r.stdout)
}

test('cli: the walk reads the configured roots and reports across both surfaces', () => {
  const out = scanJson('repo')
  // Eight hand-written files: two auth modules, two palette declarations and four Go
  // packages. The vendored, generated and _test.go copies are the other three.
  assert.equal(out.scanned, 8)
  assert.deepEqual(
    out.findings.map(f => f.check).sort(),
    ['godecl', 'name', 'union', 'value'],
  )
})

test('cli: a vendored or generated copy of a declaration is not a second declaration', () => {
  // The negative half, and the one that matters most here: node_modules and a
  // generated tree hold `decodeExp` too. If either were walked the finding would name
  // four files and send a reader to a directory nobody edits.
  const out = scanJson('repo')
  const name = out.findings.find(f => f.check === 'name')
  assert.deepEqual(name.sites.map(s => s.file), ['src/auth-app.ts', 'src/auth-server.ts'])
})

test('cli: a Go helper is counted per package, and _test.go is not a package', () => {
  const out = scanJson('repo')
  const go = out.findings.find(f => f.check === 'godecl')
  assert.match(go.title, /^meta .* declared in 4 packages$/)
  assert.equal(go.sites.some(s => s.file.endsWith('_test.go')), false)
})

test('cli: an SFC contributes its script block and not its template', () => {
  const out = scanJson('repo')
  const union = out.findings.find(f => f.check === 'union')
  assert.deepEqual(union.sites.map(s => s.file).sort(), ['src/UiBadge.vue', 'src/badge.ts'])
  // The template restates TplVariant's members verbatim. Reading the whole file would
  // pair the two and produce a second union finding out of markup.
  assert.equal(out.findings.some(f => /tpl-a/.test(f.title)), false)
})

test('cli: --root overrides the configured roots for one run', () => {
  // The flag is how someone narrows a run without editing anything -- `--root
  // api/internal` for the Go half alone is the documented case.
  const out = scanJson('repo', '--root', 'src/svc')
  assert.equal(out.scanned, 4)
  assert.deepEqual(out.findings.map(f => f.check), ['godecl'])
})

test('cli: a root missing from this checkout is skipped, not fatal', () => {
  // Roots are configured once and checkouts differ, so a sparse or partial one has to
  // scan what it has rather than take the whole run down.
  const out = scanJson('repo', '--root', 'src/svc', '--root', 'not-in-this-checkout')
  assert.equal(out.scanned, 4)
  assert.deepEqual(out.findings.map(f => f.check), ['godecl'])
})

test('cli: the text report names every file and its reason under each finding', () => {
  const r = scan('repo')
  assert.equal(r.status, 0)
  const out = r.stdout
  assert.match(out, /^dupescan: 8 files across src\n/)
  // The header counts by level, so a reader knows how much of the list is worth
  // reading before reading any of it.
  assert.match(out, /\n4 findings . 0 HIGH, 2 MEDIUM, 2 LOW\n/)
  assert.match(out, /severity is how SILENTLY it fails/)
  // Then each finding: its level, the check's label in prose, both sites, the why.
  assert.match(out, /\[MEDIUM\] same VALUE, two names . "app-auth-session"\n/)
  assert.match(out, /\n {6}SESSION_KEY {2}src\/auth-app\.ts\n/)
  assert.match(out, /\n {6}SESSION_COOKIE {2}src\/auth-server\.ts\n/)
  assert.match(out, /\n {6}why: a duplicated VALUE fails at runtime/)
  assert.match(out, /\[LOW\] same NAME, two modules . decodeExp\n/)
  assert.match(out, /\[MEDIUM\] same Go helper restated per package /)
  assert.match(out, /\[LOW\] same union restated /)
})

test('cli: --strict is the gate, and only fails when there is something to fail on', () => {
  // Both halves, because a gate that always fails gets disabled and a gate that never
  // fails is not a gate. The clean checkout is configured exactly the same way.
  assert.equal(scan('repo').status, 0, 'a plain run reports without failing')
  assert.equal(scan('repo', '--strict').status, 1)
  assert.equal(scan('clean', '--strict').status, 0)
  assert.deepEqual(scanJson('clean', '--strict').findings, [])
})

test('cli: a checkout with no config scans everything and says so', () => {
  // The honest fallback. A tool that silently scanned nothing would report a clean
  // repository, so the run still works -- it just warns that it is doing the slow,
  // wide thing, and names where it looked for the config it did not find.
  const r = scan('unconfigured', '--json')
  assert.equal(r.status, 0)
  assert.match(r.stderr, /no roots configured \(looked in dupescan\.json\)/)
  assert.match(r.stderr, /scanning \./)
  const out = JSON.parse(r.stdout)
  assert.equal(out.scanned, 2)
  assert.equal(out.findings[0].title, '"app-auth-session"')
  // And the negative half: a checkout that HAS named its roots gets no warning,
  // otherwise the message is noise on every run and stops being read.
  assert.equal(scan('repo', '--json').stderr, '')
})

test('cli: a config file that cannot be read fails loudly rather than scanning nothing', () => {
  // The dangerous failure for a config-driven scanner is a quiet one: a config that
  // did not load leaves no roots, and a scan of no roots reports a clean repository.
  // Exit 2 is neither 0 nor --strict's 1, so a gate cannot read it as either answer.
  const missing = scan('repo', '--config', 'no-such-config.json')
  assert.equal(missing.status, 2)
  assert.match(missing.stderr, /config file not found: no-such-config\.json/)
  assert.equal(missing.stdout, '')

  const malformed = scan('repo', '--config', 'bad.json')
  assert.equal(malformed.status, 2)
  assert.match(malformed.stderr, /cannot parse bad\.json:/)
  assert.equal(malformed.stdout, '')

  // A missing DEFAULT path is not an error though, only an unconfigured repository.
  // Same absence; the difference is whether someone asked for that exact file.
  assert.equal(scan('unconfigured', '--json').status, 0)
})

test('cli: an explicit --config is read from wherever it points', () => {
  // The unconfigured checkout has no config of its own, so any roots applied to it
  // can only have come from the file named here.
  const cfg = path.join(fixtureRoot, 'repo/dupescan.json')
  const r = scan('unconfigured', '--json', '--config', cfg)
  assert.equal(r.status, 0)
  assert.equal(r.stderr, '')
  // `src` does not exist in this checkout, so the roots were read and applied and
  // found nothing. That is the observable difference from the fallback, which would
  // have scanned the working directory and found the two files sitting in it.
  assert.equal(JSON.parse(r.stdout).scanned, 0)
})
