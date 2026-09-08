package main

import (
	"sort"
	"strings"
	"testing"
)

// rulesIn runs the gap rules over a synthetic module and returns the elements
// each rule fired on, so a test can assert on shape rather than on line
// numbers.
func rulesIn(t *testing.T, files map[string]string) map[string][]string {
	t.Helper()
	return rulesInWith(t, files, DefaultConfig())
}

// rulesInWith is rulesIn for a case that has to configure something the
// defaults deliberately leave off, such as a repository's method-name
// convention.
func rulesInWith(t *testing.T, files map[string]string, cfg Config) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, f := range load(t, files, cfg).tier2Rules() {
		out[f.Rule] = append(out[f.Rule], f.Element)
	}
	for _, v := range out {
		sort.Strings(v)
	}
	return out
}

func want(t *testing.T, got map[string][]string, rule string, elements ...string) {
	t.Helper()
	sort.Strings(elements)
	g, w := strings.Join(got[rule], ","), strings.Join(elements, ",")
	if g != w {
		t.Errorf("%s fired on [%s], want [%s]", rule, g, w)
	}
}

func TestSQLInLoop(t *testing.T) {
	// The Tx suffix is a naming convention, not a fact about Go, so the scanner
	// defaults to no suffixes and the repository that uses one declares it. The
	// RepoInLoop case below is here to prove the suffix arm of the matcher
	// works, so it configures the convention it is testing.
	cfg := DefaultConfig()
	cfg.SQLMethodSuffixes = []string{"Tx"}

	got := rulesInWith(t, map[string]string{"a/a.go": `package a

func NPlusOne(ids []int) {
	for _, id := range ids {
		db.Where("id = ?", id).Find(&row)
	}
}

// DrainRows is the correct shape: one query, then a Scan per row of the result
// set it already fetched.
func DrainRows() {
	rows, _ := db.Query("select 1")
	for rows.Next() {
		rows.Scan(&x)
	}
}

// Batched is one query for the whole slice.
func Batched(ids []int) {
	db.Where("id IN ?", ids).Find(&rows)
	for range ids {
		total++
	}
}

// RepoInLoop is caught by the Tx suffix convention, not by a method name.
func (s *Service) RepoInLoop(ids []int) {
	for range ids {
		s.widgets.CreateTx(ctx, tx, &p)
	}
}

// Chained is one query written across several builder calls, so it is one
// finding, not four.
func Chained(ids []int) {
	for range ids {
		db.Where("a").Order("b").Limit(1).Find(&row)
	}
}
`}, cfg)

	want(t, got, RuleSQLInLoop, "NPlusOne", "(*Service).RepoInLoop", "Chained")
}

// The suffix arm is off unless a repository asks for it. Without this, the
// default would count any method ending in the configured suffix as a query in
// every project that scans with the tool, which is a wrong measurement rather
// than a noisy one -- `CommitTx`, `BeginTx` and any domain method ending in the
// same letters would all read as round trips.
func TestSQLInLoopSuffixIsOptIn(t *testing.T) {
	files := map[string]string{"a/a.go": `package a

func (s *Service) RepoInLoop(ids []int) {
	for range ids {
		s.widgets.CreateTx(ctx, tx, &p)
	}
}
`}

	if got := rulesIn(t, files); len(got[RuleSQLInLoop]) != 0 {
		t.Errorf("SQL_IN_LOOP fired on %v with no suffix configured; the convention is opt-in", got[RuleSQLInLoop])
	}

	cfg := DefaultConfig()
	cfg.SQLMethodSuffixes = []string{"Tx"}
	want(t, rulesInWith(t, files, cfg), RuleSQLInLoop, "(*Service).RepoInLoop")
}

// A builder chain that is never finished executes nothing, so it is not a round
// trip however many times the loop runs. The receiver arm of the matcher is what
// makes this worth pinning: it counts any call on a variable named `db`, which
// caught internal/store/commentReferenceDataClause -- a function that assembles a
// subquery, returns it, and touches the database never -- twice.
func TestSQLInLoopIgnoresUnfinishedBuilderChains(t *testing.T) {
	got := rulesIn(t, map[string]string{"a/a.go": `package a

// BuildClause assembles a query per arm and returns it unexecuted. The arms are
// a source constant and nothing here runs a statement.
func BuildClause(arms []arm, id int) *gorm.DB {
	var q *gorm.DB
	for _, a := range arms {
		sub := db.Table(a.table).Select("id").Where("widget_id = ?", id)
		q = db.Where("target_id IN (?)", sub)
	}
	return q
}

// Executed is the same shape with a finisher on the end, so it stays a finding.
func Executed(arms []arm, id int) {
	for _, a := range arms {
		db.Table(a.table).Where("widget_id = ?", id).Find(&rows)
	}
}
`})

	want(t, got, RuleSQLInLoop, "Executed")
}

func TestStringConcatInLoop(t *testing.T) {
	got := rulesIn(t, map[string]string{"a/a.go": `package a

func Quadratic(parts []string) string {
	out := ""
	for _, p := range parts {
		out += p
	}
	return out
}

func LongForm(parts []string) string {
	var out string
	for _, p := range parts {
		out = out + p
	}
	return out
}

// FromSprintf still holds a string, even though no literal is in sight.
func FromSprintf(parts []string) string {
	out := fmt.Sprintf("%d items: ", len(parts))
	for _, p := range parts {
		out += p
	}
	return out
}

// Counting is arithmetic, not concatenation.
func Counting(parts []string) int {
	n := 0
	for range parts {
		n += 1
	}
	return n
}

// Outside the loop is fine: it runs once.
func Outside(parts []string) string {
	out := ""
	if len(parts) > 0 {
		out += parts[0]
	}
	return out
}
`})

	want(t, got, RuleStringConcatInLoop, "Quadratic", "LongForm", "FromSprintf")
}

func TestNestedLoop(t *testing.T) {
	got := rulesIn(t, map[string]string{"a/a.go": `package a

func Nested(rows, cols []int) {
	for range rows {
		for range cols {
			work()
		}
	}
}

func Sequential(rows, cols []int) {
	for range rows {
		work()
	}
	for range cols {
		work()
	}
}

// A loop inside a callback is not the same mistake as a doubly nested one:
// the closure may well run once.
func InClosure(rows []int) {
	for range rows {
		each(func() {
			for range rows {
				work()
			}
		})
	}
}
`})

	want(t, got, RuleNestedLoop, "Nested")
}

func TestEmptyBranchAndSwitchDefault(t *testing.T) {
	got := rulesIn(t, map[string]string{"a/a.go": `package a

func Swallowed() {
	if err := do(); err != nil {
	}
}

func Handled() {
	if err := do(); err != nil {
		return
	}
	if x {
	} else {
		y()
	}
}

func NoDefault(k Kind) {
	switch k {
	case KindA:
	case KindB:
	}
}

func WithDefault(k Kind) {
	switch k {
	case KindA:
	default:
	}
}

// A tagless switch is an if-ladder, and an if-ladder needs no else.
func IfLadder(n int) {
	switch {
	case n > 0:
	case n < 0:
	}
}

// A switch over a call result has no vocabulary for a new value to join, and the
// statements after it are its default.
func CallTag(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return "many"
}

// An empty arm carrying a comment is the fall-through, decided in writing.
func StatedFallThrough(k Kind) string {
	switch k {
	case KindA:
		return "a"
	case KindB:
		// Says nothing about the answer, so it joins the ladder below rather
		// than answering here.
	}
	return "ladder"
}

// The same arm without the comment is a bare stub, and still reports.
func BareStub(k Kind) string {
	switch k {
	case KindA:
		return "a"
	case KindB:
	}
	return "ladder"
}
`})

	want(t, got, RuleEmptyBranch, "Swallowed")
	// NoDefault's and BareStub's empty arms carry no comment saying the
	// fall-through was meant, which is still the shape the rule is for.
	// CallTag and StatedFallThrough are the two new skips.
	want(t, got, RuleSwitchWithoutDefault, "NoDefault", "BareStub")
}

func TestDebugStatements(t *testing.T) {
	got := rulesIn(t, map[string]string{
		"a/a.go": `package a

import (
	"fmt"
	"log"
	"log/slog"
)

func Debug() {
	fmt.Println("here")
	log.Printf("here")
	println("here")
}

func Proper(l *slog.Logger) {
	l.Info("here")
	_ = fmt.Sprintf("not a print")
}
`,
		// cmd/ writes to stdout on purpose; that is what a CLI is.
		"cmd/tool/main.go": "package main\n\nimport \"fmt\"\n\nfunc Report() { fmt.Println(\"ok\") }\n",
	})

	if n := len(got[RuleDebugStatement]); n != 3 {
		t.Errorf("got %d debug findings, want 3 (all in a/a.go): %v", n, got[RuleDebugStatement])
	}
	for _, e := range got[RuleDebugStatement] {
		if e != "Debug" {
			t.Errorf("debug finding on %q, want only Debug", e)
		}
	}
}

func TestCommentRules(t *testing.T) {
	got := rulesIn(t, map[string]string{"a/a.go": `package a

type Filter struct {
	// TODO: drop this once the webapp stops sending it.
	Legacy string
	Kind   Kind // empty = any
	// COALESCE(year_of_implementation, planned_execute_year)
	Year int
}

func Suppressed() {
	//nolint:gosec // G115: bounded by the column type
	_ = 1
}

func Stale() {
	// x := compute()
	// if err != nil {
	// return err
	// }
	run()
}

// Prose mentions code without being code: if the caller passes nil, for
// example, the result is empty.
func Documented() {}
`})

	want(t, got, RuleUnfinishedWork, "Filter")
	want(t, got, RuleSuppressedWarning, "Suppressed")
	// One finding for the whole commented-out block, not one per line -- and
	// nothing for the `empty = any` field comment or the quoted SQL, which are
	// what the shape test exists to keep out.
	want(t, got, RuleCommentedOutCode, "Stale")
}

func TestMissingDocComment(t *testing.T) {
	got := rulesIn(t, map[string]string{"a/a.go": `package a

// Documented is fine.
func Documented() {}

func Undocumented() {}

func unexported() {}

// Thing is fine.
type Thing struct{}

type Bare struct{}

// Method is fine.
func (t *Thing) Method() {}

func (t *Thing) Undocumented() {}

// A method on an unexported type is not part of the package surface.
func (h *hidden) Exported() {}

// Const is fine.
const Const = 1

const Bare2 = 2
`})

	want(t, got, RuleMissingDoc,
		"Undocumented", "Bare", "(*Thing).Undocumented", "Bare2")
}

func TestDisabledRules(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DisabledRules = []string{RuleMissingDoc, RuleNestedLoop}

	idx := load(t, map[string]string{"a/a.go": `package a

func Undocumented(rows, cols []int) {
	for range rows {
		for range cols {
		}
	}
}
`}, cfg)

	for _, f := range Run(idx, "") {
		if f.Rule == RuleMissingDoc || f.Rule == RuleNestedLoop {
			t.Errorf("disabled rule %s still reported: %+v", f.Rule, f)
		}
	}
}
