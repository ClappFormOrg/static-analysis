package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// componentMap builds a loaded map without going through disk.
func componentMap(comps ...Component) Components {
	return Components{Decided: "2026-08-27", List: comps}
}

// writeComponents materialises a component file and returns its path.
func writeComponents(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "components.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLongestPrefixOwnsTheModule holds that a catch-all can sit beside a
// specific path without the declaration order deciding the answer. The real map
// depends on this: `shared` claims `internal` and `store` claims
// `internal/store`, and every file under the latter has to land in `store`.
func TestLongestPrefixOwnsTheModule(t *testing.T) {
	comps := componentMap(
		Component{Name: "shared", Paths: []string{"internal"}, Reason: "x"},
		Component{Name: "store", Paths: []string{"internal/store"}, Reason: "x"},
	)

	cases := map[string]string{
		"internal/store/order.go":      "store",
		"internal/store/sub/thing.go":  "store",
		"internal/apperr/apperr.go":    "shared",
		"internal/storedouble/fake.go": "shared", // not a prefix match on internal/store
		"cmd/api/main.go":              "",
		"internal":                     "shared",
	}
	for rel, want := range cases {
		if got := comps.Of(rel); got != want {
			t.Errorf("Of(%q) = %q, want %q", rel, got, want)
		}
	}

	// And the answer must not depend on the order the components were declared.
	reversed := componentMap(comps.List[1], comps.List[0])
	if got := reversed.Of("internal/store/order.go"); got != "store" {
		t.Errorf("with the components declared the other way round, Of() = %q, want store", got)
	}
}

// TestUnmappedFileFailsTheRun holds the rule that makes the component map a
// decision rather than a default. A file nobody placed cannot be classified as
// hidden or exposed, and quietly bucketing it would move the percentage without
// anyone choosing to.
func TestUnmappedFileFailsTheRun(t *testing.T) {
	idx := load(t, map[string]string{
		"a/a.go": "package a\n\n// F is a unit.\nfunc F() int { return 1 }\n",
		"b/b.go": "package b\n\n// G is a unit.\nfunc G() int { return 2 }\n",
	}, DefaultConfig())

	comps := componentMap(Component{Name: "only-a", Paths: []string{"a"}, Reason: "x"})

	_, err := idx.measureModules("", comps)
	if err == nil {
		t.Fatal("a file belonging to no component must fail the run")
	}
	if !strings.Contains(err.Error(), "b/b.go") {
		t.Errorf("error %q does not name the unmapped file", err)
	}
}

// TestComponentFileNeedsItsReasoning holds the two fields that separate a
// recorded decision from a guess someone wrote down.
func TestComponentFileNeedsItsReasoning(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"no decided date": {
			body: `{"components": [{"name": "a", "paths": ["a"], "reason": "x"}]}`,
			want: "decided",
		},
		"component with no reason": {
			body: `{"decided": "2026-08-27", "components": [{"name": "a", "paths": ["a"]}]}`,
			want: "reason",
		},
		"component with no path": {
			body: `{"decided": "2026-08-27", "components": [{"name": "a", "reason": "x"}]}`,
			want: "path",
		},
		"no components at all": {
			body: `{"decided": "2026-08-27", "components": []}`,
			want: "no components",
		},
		"duplicate component": {
			body: `{"decided": "2026-08-27", "components": [
				{"name": "a", "paths": ["a"], "reason": "x"},
				{"name": "a", "paths": ["b"], "reason": "y"}]}`,
			want: "twice",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadComponents(writeComponents(t, tc.body))
			if err == nil {
				t.Fatalf("LoadComponents accepted a file with %s", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestHiddenMeansNoIncomingCrossComponentDependency is the classification the
// property rests on. A module referenced heavily from inside its own component
// is still hidden; one reference from outside is enough to expose it.
func TestHiddenMeansNoIncomingCrossComponentDependency(t *testing.T) {
	idx := load(t, map[string]string{
		// Referenced only by its own component.
		"store/internal_helper.go": `package store

// Helper is used inside the store only.
func Helper() int { return 1 }
`,
		"store/repo.go": `package store

// Repo leans on its sibling, which is coupling inside one component.
func Repo() int { return Helper() }
`,
		// Referenced from another component, so exposed.
		"store/api.go": `package store

// API is what the service calls.
func API() int { return 2 }
`,
		"service/svc.go": `package service

import "example.com/m/store"

// Call reaches across the component boundary.
func Call() int { return store.API() }
`,
	}, DefaultConfig())

	comps := componentMap(
		Component{Name: "store", Paths: []string{"store"}, Reason: "x"},
		Component{Name: "service", Paths: []string{"service"}, Reason: "x"},
	)
	mods := modulesByFile(modules(t, idx, comps))

	helper := mods["store/internal_helper.go"]
	if helper.InModules == 0 {
		t.Fatal("fixture is wrong: the helper should be referenced by its sibling")
	}
	if !helper.Hidden() {
		t.Errorf("a module referenced only from inside its own component is hidden, got %+v", helper)
	}

	api := mods["store/api.go"]
	if api.InCrossComponent != 1 {
		t.Errorf("InCrossComponent = %d, want 1: the service module references it", api.InCrossComponent)
	}
	if api.Hidden() {
		t.Error("a module referenced from another component is exposed, not hidden")
	}
}

// TestIndependenceIsAFloorNotACap holds the direction of the one property that
// compares the other way round from every other in this report: more hidden code
// is better, and the model publishes a minimum rather than a maximum.
func TestIndependenceIsAFloorNotACap(t *testing.T) {
	// 940 of 1000 lines hidden, against a floor of 93.7%.
	mods := []Module{
		{File: "a.go", Component: "a", LOC: 940},
		{File: "b.go", Component: "b", LOC: 60, InCrossComponent: 1},
	}
	p := measureIndependence(mods, "2026-08-27", true)
	if p == nil {
		t.Fatal("measureIndependence returned nothing for a non-empty set")
	}
	if p.FloorPct != 93.7 {
		t.Errorf("FloorPct = %.1f, want 93.7 (v17.0 section 3.8)", p.FloorPct)
	}
	if !p.Pass {
		t.Errorf("94.0%% hidden must pass a 93.7%% floor, got fail")
	}
	if p.ExposedLOC != 0 {
		t.Errorf("ExposedLOC = %d on a passing measurement, want 0", p.ExposedLOC)
	}

	// One more exposed line and it fails, which is the boundary worth pinning.
	mods[0].LOC, mods[1].LOC = 930, 70
	q := measureIndependence(mods, "2026-08-27", true)
	if q.Pass {
		t.Errorf("93.0%% hidden must fail a 93.7%% floor, got pass")
	}
	// 93.7% of 1000 is 937 lines; 930 are hidden, so 7 have to stop being exposed.
	if q.ExposedLOC != 7 {
		t.Errorf("ExposedLOC = %d, want 7: the lines that have to stop being reachable "+
			"from outside their own component", q.ExposedLOC)
	}
}

// TestIndependenceIsAbsentWithoutAComponentMap holds the degradation. A guessed
// boundary produces a percentage that reads exactly like a measured one, so
// without the map the property is reported as absent rather than estimated.
func TestIndependenceIsAbsentWithoutAComponentMap(t *testing.T) {
	l := profileOf(UnitSet{
		Language: LanguageGo,
		Units:    []Unit{unit(10, 1, 0)},
		Modules:  []Module{{File: "a.go", LOC: 10}},
	}, true, defaultModuleCouplingCounts)

	if l.Independence != nil {
		t.Error("component independence must not be measured without a component map")
	}

	b := &strings.Builder{}
	writeIndependence(b, l)
	out := b.String()
	if !strings.Contains(out, "Not measured") {
		t.Errorf("the report must say the property is absent, got:\n%s", out)
	}
	if !strings.Contains(out, "-components") {
		t.Errorf("the report must say how to supply the boundary, got:\n%s", out)
	}
}

// TestIndependenceReportNamesTheDecision holds that the report carries the date
// the boundary was agreed. A component map is a scoping decision, and one made
// before half the code existed should be read differently from a current one.
func TestIndependenceReportNamesTheDecision(t *testing.T) {
	l := profileOf(UnitSet{
		Language:      LanguageGo,
		Units:         []Unit{unit(10, 1, 0)},
		Modules:       []Module{{File: "a.go", LOC: 10, Component: "a"}},
		Components:    "2026-08-27",
		ModuleMetrics: []string{moduleMetricCoupling},
	}, true, defaultModuleCouplingCounts)

	if l.Independence == nil {
		t.Fatal("component independence must be measured when a component map was supplied")
	}
	b := &strings.Builder{}
	writeIndependence(b, l)
	if !strings.Contains(b.String(), "2026-08-27") {
		t.Errorf("the report must name the date the boundary was decided, got:\n%s", b.String())
	}
}
