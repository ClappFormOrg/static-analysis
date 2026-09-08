package main

import (
	"strings"
	"testing"
)

// TestVolumeTableIsV17 pins the model's own conversion table. These are the
// figures a rebuild value is computed from, so an edited row would change a
// number that looks like it came from the source document.
func TestVolumeTableIsV17(t *testing.T) {
	const source = "SIG/TUViT Evaluation Criteria Trusted Product Maintainability, " +
		"Guidance for Producers, v17.0 (2025-03-12), section 3.1"

	want := map[string]int{
		"C++": 32000, "C#": 39000, "Java": 35100, "JavaScript": 36300,
		"Python": 25000, "Ruby": 24600, "TypeScript": 31600,
	}
	got := volumeLinesPerBudget()
	if len(got) != len(want) {
		t.Fatalf("the table has %d languages, want %d (%s)", len(got), len(want), source)
	}
	for lang, lines := range want {
		if got[lang] != lines {
			t.Errorf("%s = %d lines per %.1f person-years, want %d (%s)",
				lang, got[lang], volumeYears, lines, source)
		}
	}
	if volumeYears != 3.9 {
		t.Errorf("volumeYears = %.1f, want 3.9 (%s)", volumeYears, source)
	}

	// Go's absence is the reason this property carries no verdict, so it is
	// asserted rather than left as an accident of the table.
	if _, rated := got[LanguageGo]; rated {
		t.Error("the source table has no Go row; adding one here would invent a conversion")
	}
}

// TestVolumeComputesARebuildValueOnlyWhereThereIsAFactor holds the split between
// what the table supports and what it does not.
func TestVolumeComputesARebuildValueOnlyWhereThereIsAFactor(t *testing.T) {
	shares := measureVolume([]LanguageProfile{
		{Language: LanguageTypeScript, Measured: true, FilesScanned: 2, UnitLOC: 30000, NonUnitLOC: 1600},
		{Language: LanguageGo, Measured: true, FilesScanned: 3, UnitLOC: 90000, NonUnitLOC: 10000},
		{Language: "Nothing", FilesScanned: 4},
	})

	if len(shares) != 2 {
		t.Fatalf("got %d shares, want 2: an unmeasured language has no size to report", len(shares))
	}
	if shares[0].Language != LanguageGo {
		t.Errorf("shares are ordered %s first, want the largest language first", shares[0].Language)
	}

	ts := shares[1]
	if ts.Rated() != true {
		t.Fatal("TypeScript is in the model's table and must carry a rebuild value")
	}
	// 31,600 lines is exactly the 3.9-person-year figure for TypeScript.
	if ts.Years < 3.89 || ts.Years > 3.91 {
		t.Errorf("31,600 TypeScript lines = %.2f person-years, want 3.9", ts.Years)
	}
	if shares[0].Rated() || shares[0].Years != 0 {
		t.Errorf("Go has no published factor, so it must carry no rebuild value, got %.2f", shares[0].Years)
	}
}

// TestVolumePrintsNoVerdict is the point of reporting this property at all. The
// cap is on one figure for the whole product, and the product's largest language
// has no conversion, so any verdict here would be invented.
func TestVolumePrintsNoVerdict(t *testing.T) {
	b := &strings.Builder{}
	writeVolume(b, measureVolume([]LanguageProfile{
		{Language: LanguageGo, Measured: true, FilesScanned: 3, UnitLOC: 90000, NonUnitLOC: 10000},
		{Language: LanguageTypeScript, Measured: true, FilesScanned: 2, UnitLOC: 30000, NonUnitLOC: 1600},
	}))
	out := b.String()

	for _, forbidden := range []string{"Verdict: pass", "Verdict: fail", "| pass |", "| fail |"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("the report renders %q for a property it cannot judge:\n%s", forbidden, out)
		}
	}
	if !strings.Contains(out, "No verdict") {
		t.Errorf("the report must say there is no verdict, got:\n%s", out)
	}
	if !strings.Contains(out, "Go") || !strings.Contains(out, "not computable") {
		t.Errorf("the report must name the language with no conversion, got:\n%s", out)
	}
	if !strings.Contains(out, "2026") {
		t.Errorf("the report must note that SIG's own 2026 model dropped this property, got:\n%s", out)
	}
}
