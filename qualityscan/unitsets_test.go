package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The `-units` contract: what a front end is allowed to claim it measured, and
// what happens to a claim its data does not back.

func writeUnits(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "units.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestModuleMetricClaimsAreChecked holds that a front end cannot assert a
// property it did not compute. Modules supplied for duplication alone carry no
// incoming edges, so binning them for coupling would put every line in the low
// band and print three passes for something nobody measured.
func TestModuleMetricClaimsAreChecked(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"unknown metric": {
			body: `{"language": "TypeScript", "units": [{"loc": 3, "mccabe": 1, "params": 0}],
			        "module_metrics": ["entanglement"]}`,
			want: "unknown module metric",
		},
		"coupling claimed with no modules": {
			body: `{"language": "TypeScript", "units": [{"loc": 3, "mccabe": 1, "params": 0}],
			        "module_metrics": ["coupling"]}`,
			want: "supplies no modules",
		},
		"duplication claimed with no lines": {
			body: `{"language": "TypeScript", "units": [{"loc": 3, "mccabe": 1, "params": 0}],
			        "modules": [{"file": "a.ts", "loc": 10}], "module_metrics": ["duplication"]}`,
			want: "normalised lines",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadUnitSets([]string{writeUnits(t, tc.body)})
			if err == nil {
				t.Fatalf("LoadUnitSets accepted a set with %s", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}

	// A set claiming only what it supplies loads.
	ok := `{"language": "TypeScript", "units": [{"loc": 3, "mccabe": 1, "params": 0}],
	        "modules": [{"file": "a.ts", "loc": 10, "lines": ["a", "b"]}],
	        "module_metrics": ["duplication"]}`
	if _, err := LoadUnitSets([]string{writeUnits(t, ok)}); err != nil {
		t.Errorf("LoadUnitSets rejected a well-formed set: %v", err)
	}
}

// TestSupplyingModulesForDuplicationDoesNotClaimCoupling is the failure the
// contract exists to prevent, checked end to end: a language that supplies
// modules only so duplication can be measured must still report module coupling
// as absent.
func TestSupplyingModulesForDuplicationDoesNotClaimCoupling(t *testing.T) {
	l := profileOf(UnitSet{
		Language:      LanguageTypeScript,
		Units:         []Unit{unit(10, 1, 0)},
		Modules:       []Module{{File: "a.ts", LOC: 6, Lines: []string{"a", "b", "c", "d", "e", "f"}}},
		ModuleMetrics: []string{moduleMetricDuplication},
	}, true, defaultModuleCouplingCounts)

	if l.ModulesMeasured {
		t.Error("module coupling must not be measured from modules supplied for duplication")
	}
	if len(l.ModuleProperties) != 0 {
		t.Errorf("got %d module property profiles, want none", len(l.ModuleProperties))
	}
	if l.Duplication == nil {
		t.Error("duplication must be measured when the set supplies its lines and claims it")
	}
	if l.Independence != nil {
		t.Error("component independence needs resolved edges, so it cannot come from these modules")
	}
}

// TestCombinedClaimsOnlyWhatEveryLanguageMeasured holds the roll-up's honesty.
// One surface measuring coupling and another not means the combined figure
// cannot claim it, or the second surface's unmeasured modules join the first's
// as a pile of zeroes.
func TestCombinedClaimsOnlyWhatEveryLanguageMeasured(t *testing.T) {
	r := BuildSIGProfile([]UnitSet{
		{
			Language: LanguageGo, Units: []Unit{unit(10, 1, 0)}, FilesScanned: 1,
			Modules:       []Module{{File: "a.go", LOC: 10, Lines: []string{"a", "b", "c", "d", "e", "f"}}},
			ModuleMetrics: []string{moduleMetricCoupling, moduleMetricDuplication},
		},
		{
			Language: LanguageTypeScript, Units: []Unit{unit(10, 1, 0)}, FilesScanned: 1,
			Modules:       []Module{{File: "a.ts", LOC: 10, Lines: []string{"x", "y", "z", "p", "q", "r"}}},
			ModuleMetrics: []string{moduleMetricDuplication},
		},
	}, "", defaultModuleCouplingCounts)

	if r.Combined.ModulesMeasured {
		t.Error("the roll-up claims coupling, which only one of its two languages measured")
	}
	if r.Combined.Duplication == nil {
		t.Error("the roll-up should still carry duplication, which both languages measured")
	}
}
