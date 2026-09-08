package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// snap builds a snapshot with one language and the given measures.
func snap(taken string, measures map[string]float64) Snapshot {
	return Snapshot{
		Model: SIGModel, Version: SIGModelVersion, StarTarget: SIGStarTarget, Taken: taken,
		Languages: []LanguageSnapshot{{Language: LanguageGo, Measures: measures}},
	}
}

// movementsByMeasure indexes a comparison by the measure it names.
func movementsByMeasure(moves []Movement) map[string]Movement {
	out := map[string]Movement{}
	for _, m := range moves {
		out[m.Measure] = m
	}
	return out
}

// TestSnapshotRoundTrips holds that a baseline written today can be read back
// tomorrow. It is the only artefact here that has to survive the tool changing
// under it, so the shape is checked rather than assumed.
func TestSnapshotRoundTrips(t *testing.T) {
	want := snap("2026-08-28", map[string]float64{"unit_size > 15": 66.8, "duplication redundant": 8.2})
	want.Languages[0].Entanglement = map[string]float64{"density": 2.6, "violation_degree": 0.06}

	p := filepath.Join(t.TempDir(), "sigprofile.json")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteSnapshot(f, want); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	f.Close()

	// The model's own wording carries `>` in a measure key, and a baseline is
	// read as a diff, so it must not come back as >.
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `\u003e`) {
		t.Errorf("the snapshot escapes its comparison operators:\n%s", body)
	}
	if !strings.Contains(string(body), `"unit_size > 15"`) {
		t.Errorf("the snapshot does not carry the measure key verbatim:\n%s", body)
	}

	got, err := LoadSnapshot(p)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if got == nil {
		t.Fatal("LoadSnapshot returned nothing for a file it wrote")
	}
	if got.Taken != want.Taken || got.Version != want.Version {
		t.Errorf("header = %+v, want %+v", got, want)
	}
	if got.Languages[0].Measures["unit_size > 15"] != 66.8 {
		t.Errorf("measures came back as %+v", got.Languages[0].Measures)
	}
	if got.Languages[0].Entanglement["density"] != 2.6 {
		t.Errorf("entanglement came back as %+v", got.Languages[0].Entanglement)
	}
}

// TestMissingBaselineIsNotSilence holds the degradation. Without a baseline the
// report says so, because "no movement reported" and "nothing moved" are
// different claims and only one of them is true here.
func TestMissingBaselineIsNotSilence(t *testing.T) {
	if got, err := LoadSnapshot(filepath.Join(t.TempDir(), "absent.json")); err != nil || got != nil {
		t.Fatalf("LoadSnapshot of a missing file = %v, %v; want nil, nil", got, err)
	}

	b := &strings.Builder{}
	writeMovement(b, nil, nil)
	out := b.String()
	if !strings.Contains(out, "No baseline") {
		t.Errorf("the report must say there is no baseline, got:\n%s", out)
	}
	if strings.Contains(out, "Nothing measured has changed") {
		t.Error("a missing baseline must not render as nothing having changed")
	}
}

// TestMovementReportsTheDirectionAndTheGaps covers the three cases that matter:
// a figure that moved, one measured for the first time, and one that stopped
// being measured. The last is the dangerous one, because an absent number and an
// improved one look identical in a total.
func TestMovementReportsTheDirectionAndTheGaps(t *testing.T) {
	base := snap("2026-08-28", map[string]float64{
		"unit_size > 15":        66.8,
		"unit_size > 30":        29.3,
		"duplication redundant": 8.2,
	})
	now := snapshotFromMeasures(map[string]float64{
		"unit_size > 15":                64.2, // moved down
		"unit_size > 30":                29.3, // unchanged
		"component_independence hidden": 58.5, // new
	})

	moves := movementsByMeasure(compareSnapshots(&base, now))

	if got := moves["unit_size > 15"]; got.Kind != "moved" || got.Delta() > -2.5 || got.Delta() < -2.7 {
		t.Errorf("unit_size > 15 = %+v (delta %.1f), want moved by -2.6", got, got.Delta())
	}
	if _, unchanged := moves["unit_size > 30"]; unchanged {
		t.Error("an unchanged measure must not be listed as movement")
	}
	if got := moves["component_independence hidden"]; got.Kind != "new" {
		t.Errorf("a measure absent from the baseline = %+v, want kind new", got)
	}
	if got := moves["duplication redundant"]; got.Kind != "gone" || got.Then != 8.2 {
		t.Errorf("a measure that stopped being measured = %+v, want kind gone at 8.2", got)
	}

	b := &strings.Builder{}
	writeMovement(b, &base, compareSnapshots(&base, now))
	out := b.String()
	if !strings.Contains(out, "2026-08-28") {
		t.Errorf("the report must name the baseline it measures from, got:\n%s", out)
	}
	if !strings.Contains(out, "no longer measured") {
		t.Errorf("the report must flag a measure that disappeared, got:\n%s", out)
	}
	if !strings.Contains(out, "-2.6") {
		t.Errorf("the report must carry the size of the change, got:\n%s", out)
	}
}

// snapshotFromMeasures builds the "now" side of a comparison.
func snapshotFromMeasures(measures map[string]float64) Snapshot {
	return Snapshot{Languages: []LanguageSnapshot{{Language: LanguageGo, Measures: measures}}}
}

// TestALanguageDisappearingIsMovement holds the loudest case of all. A surface
// that stops being scanned reports nothing, and nothing reads as fine.
func TestALanguageDisappearingIsMovement(t *testing.T) {
	base := Snapshot{Taken: "2026-08-28", Languages: []LanguageSnapshot{
		{Language: LanguageGo, Measures: map[string]float64{"unit_size > 15": 66.8}},
		{Language: LanguageTypeScript, Measures: map[string]float64{"unit_size > 15": 34.9}},
	}}
	now := snapshotFromMeasures(map[string]float64{"unit_size > 15": 66.8})

	var found bool
	for _, m := range compareSnapshots(&base, now) {
		if m.Language == LanguageTypeScript && m.Kind == "gone" {
			found = true
		}
	}
	if !found {
		t.Error("a language present in the baseline and absent now must be reported as gone")
	}
}

// TestSnapshotRoundsToWhatTheReportPrints keeps the movement table honest
// against the profile beside it. A change too small to alter either printed
// figure is not a change a reader could see, and reporting it would make every
// run look busy.
func TestSnapshotRoundsToWhatTheReportPrints(t *testing.T) {
	r := BuildSIGProfile([]UnitSet{{
		Language: LanguageGo, FilesScanned: 1,
		Units: []Unit{unit(20, 1, 0), unit(10, 1, 0), unit(3, 1, 0)},
	}}, "", defaultModuleCouplingCounts)

	s := snapshotOf(r, "2026-08-28", "")
	for name, v := range s.Languages[0].Measures {
		if v != round1(v) {
			t.Errorf("%s recorded as %v, want it rounded to the one decimal the report prints", name, v)
		}
	}
}

// TestMovementKeepsEachMeasureAtItsOwnScale holds a bug this table had on its
// first real run. The measures span two scales: a tail is a percentage to one
// decimal, an entanglement violation degree is a ratio to three. Printed at one
// decimal, a degree of 0.06 renders as "0.1" and a change of -0.004 as "-0.0",
// which reads as a figure that did not move when it is the only one that did.
func TestMovementKeepsEachMeasureAtItsOwnScale(t *testing.T) {
	base := Snapshot{Taken: "2026-08-28", Languages: []LanguageSnapshot{{
		Language:     LanguageGo,
		Measures:     map[string]float64{"unit_size > 15": 66.8},
		Entanglement: map[string]float64{"violation_degree": 0.06},
	}}}
	now := Snapshot{Languages: []LanguageSnapshot{{
		Language:     LanguageGo,
		Measures:     map[string]float64{"unit_size > 15": 65.2},
		Entanglement: map[string]float64{"violation_degree": 0.056},
	}}}

	b := &strings.Builder{}
	writeMovement(b, &base, compareSnapshots(&base, now))
	out := b.String()

	if !strings.Contains(out, "0.056") {
		t.Errorf("a three-decimal measure was flattened; the table reads:\n%s", out)
	}
	if strings.Contains(out, "| -0 |") || strings.Contains(out, "-0.0 |") {
		t.Errorf("a real change rendered as no change:\n%s", out)
	}
	// And the percentage scale still reads as it always did.
	if !strings.Contains(out, "65.2") || !strings.Contains(out, "-1.6") {
		t.Errorf("the percentage measures lost their own formatting:\n%s", out)
	}
}

// TestSnapshotRecordsEveryCappedFigureAtItsOwnPrecision covers the three
// figures that live outside the property tails: component independence,
// duplication and the two entanglement numbers.
//
// Each is recorded at the precision its own report prints, which is why there
// are three rounding helpers rather than one. A percentage to one decimal and a
// violation degree to three are not the same kind of number, and a single
// precision would either lose the second or make the first report movement a
// reader could not have seen.
//
// The unmeasured language in the fixture is the other half. A language that
// contributed no units has nothing to compare, and recording it would put a row
// of zeroes into the baseline that the next run would read as a real figure
// that got worse.
func TestSnapshotRecordsEveryCappedFigureAtItsOwnPrecision(t *testing.T) {
	r := BuildSIGProfile([]UnitSet{{
		Language: LanguageGo, FilesScanned: 2,
		Units: []Unit{unit(40, 3, 2), unit(10, 1, 0), unit(6, 1, 1)},
	}}, "", defaultModuleCouplingCounts)
	// Figures chosen so each one is only correct at its own precision: 58.46
	// rounds to 58.5 at one decimal, 2.6449 to 2.64 at two, and 0.0564 to 0.056
	// at three but to 0.1 at one.
	r.Languages[0].Independence = &IndependenceProfile{Pct: 58.46, Applies: true}
	r.Languages[0].Duplication = &DuplicationProfile{Pct: 8.24, Applies: true}
	r.Languages[0].Entanglement = &EntanglementProfile{
		Density: 2.6449, ViolationDegree: 0.0564, Lines: 1200,
		Violations: []EntanglementViolation{{Kind: "cyclic"}, {Kind: "transitive"}},
	}
	// TypeScript supplies no units today, so it is in the report and measured
	// nothing.
	r.Languages = append(r.Languages, LanguageProfile{Language: LanguageTypeScript})

	s := snapshotOf(r, "2026-09-06", "b420251")

	if len(s.Languages) != 1 || s.Languages[0].Language != LanguageGo {
		t.Fatalf("snapshot carries %+v; an unmeasured language must not be recorded, because "+
			"its zeroes read as measurements to the next run", s.Languages)
	}
	if s.Taken != "2026-09-06" || s.Rev != "b420251" {
		t.Errorf("header = %+v, want the date and revision the caller supplied", s)
	}

	got := s.Languages[0]
	for _, c := range []struct {
		key  string
		want float64
		why  string
	}{
		{"component_independence hidden", 58.5, "a percentage, at the one decimal the report prints"},
		{"duplication redundant", 8.2, "a percentage, at the one decimal the report prints"},
	} {
		if got.Measures[c.key] != c.want {
			t.Errorf("%s = %v, want %v (%s)", c.key, got.Measures[c.key], c.want, c.why)
		}
	}
	for _, c := range []struct {
		key  string
		want float64
		why  string
	}{
		{"density", 2.64, "a ratio, at two decimals"},
		{"violation_degree", 0.056, "a ratio, at three decimals: one decimal would render it as 0.1"},
		{"lines", 1200, "a count, recorded whole"},
		{"violations", 2, "a count of the violations, not their weight"},
	} {
		if got.Entanglement[c.key] != c.want {
			t.Errorf("entanglement %s = %v, want %v (%s)", c.key, got.Entanglement[c.key], c.want, c.why)
		}
	}

	// The property tails are still in there alongside them, keyed by property
	// and band so the two halves of the snapshot read the same way.
	if len(got.Measures) <= 2 {
		t.Errorf("measures = %+v; the property tails must be recorded beside the "+
			"three standalone figures", got.Measures)
	}
	if got.Files != 2 || got.UnitLOC == 0 {
		t.Errorf("language snapshot = %+v, want the file count and the unit LOC denominator", got)
	}
}

// TestSnapshotOmitsTheOptionalProfilesItWasNotGiven is the negative half of the
// test above. Independence, duplication and entanglement are each nil when the
// run had no component map or no normalised lines, and a nil profile has to
// leave the key out entirely: a 0.0 recorded for a figure nobody measured is
// the best possible score, and the next run would report the real measurement
// as a regression.
func TestSnapshotOmitsTheOptionalProfilesItWasNotGiven(t *testing.T) {
	r := BuildSIGProfile([]UnitSet{{
		Language: LanguageGo, FilesScanned: 1, Units: []Unit{unit(10, 1, 0)},
	}}, "", defaultModuleCouplingCounts)

	s := snapshotOf(r, "2026-09-06", "")

	got := s.Languages[0]
	for _, key := range []string{"component_independence hidden", "duplication redundant"} {
		if _, present := got.Measures[key]; present {
			t.Errorf("%s was recorded for a run that did not measure it: %v", key, got.Measures[key])
		}
	}
	if got.Entanglement != nil {
		t.Errorf("entanglement = %+v for a run with no component graph, want nothing recorded", got.Entanglement)
	}
	if s.Rev != "" {
		t.Errorf("Rev = %q, want it empty when the caller named no revision", s.Rev)
	}
}

// TestLoadSnapshotTellsAbsentFromBroken covers the three ways a baseline fails
// to arrive, because only one of them means "there is no baseline".
//
// No path and no file are both that: the report degrades to saying where the
// code stands. A file that is there and does not parse is not -- it is a
// corrupted or half-written baseline, and swallowing it would silently turn
// every later run's movement section into "no baseline" while the file sat in
// the repository looking fine.
func TestLoadSnapshotTellsAbsentFromBroken(t *testing.T) {
	if got, err := LoadSnapshot(""); got != nil || err != nil {
		t.Errorf("LoadSnapshot(\"\") = %v, %v; want nil, nil: no -baseline flag is not an error", got, err)
	}

	dir := t.TempDir()
	broken := filepath.Join(dir, "sigprofile.json")
	if err := os.WriteFile(broken, []byte("{\"languages\": [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSnapshot(broken)
	if err == nil {
		t.Fatalf("LoadSnapshot of a truncated baseline = %+v, nil; a file that is there and "+
			"does not parse must not read as no baseline", got)
	}
	if !strings.Contains(err.Error(), broken) {
		t.Errorf("error = %q, want it to name the file it could not parse", err)
	}

	// Anything else that stops the read is an error too. A directory where the
	// baseline should be is the readable version of that case.
	if _, err := LoadSnapshot(dir); err == nil {
		t.Error("LoadSnapshot of an unreadable path returned no error")
	}
}

// TestMovementNamesTheBaselineEvenWhenNothingMoved covers the quiet run. "No
// baseline" and "nothing changed since the baseline of 2026-08-28" are the two
// claims the section has to keep apart, and the second one is only checkable if
// it names what it measured against.
func TestMovementNamesTheBaselineEvenWhenNothingMoved(t *testing.T) {
	base := snap("2026-08-28", map[string]float64{"unit_size > 15": 66.8})
	base.Rev = "8db37b1"
	now := snapshotFromMeasures(map[string]float64{"unit_size > 15": 66.8})

	b := &strings.Builder{}
	writeMovement(b, &base, compareSnapshots(&base, now))
	out := b.String()

	if !strings.Contains(out, "Nothing measured has changed since 2026-08-28 (8db37b1)") {
		t.Errorf("a quiet run must name the date and the revision it compared against, got:\n%s", out)
	}
	if strings.Contains(out, "No baseline") {
		t.Errorf("a run with a baseline must not say it has none:\n%s", out)
	}

	// A baseline recorded without a date still gets described rather than
	// leaving the sentence half finished.
	undated := snap("", map[string]float64{"unit_size > 15": 66.8})
	b.Reset()
	writeMovement(b, &undated, nil)
	if !strings.Contains(b.String(), "the recorded baseline") {
		t.Errorf("an undated baseline must still be named, got:\n%s", b.String())
	}
}

// TestMovementKeepsTheSignAndOrdersByLanguage holds the two things that make
// the table readable rather than merely correct.
//
// The sign is the whole point of the change column: a tail that went from 64.2
// to 65.6 got worse, and "1.4" alone says only that it moved. And with two
// languages in one table the rows group by language, because a reader scanning
// for Go's numbers should not be interleaving them with TypeScript's.
func TestMovementKeepsTheSignAndOrdersByLanguage(t *testing.T) {
	base := Snapshot{Taken: "2026-08-28", Languages: []LanguageSnapshot{
		{Language: LanguageGo, Measures: map[string]float64{"unit_size > 15": 64.2}},
		{Language: LanguageTypeScript, Measures: map[string]float64{"unit_size > 15": 34.9}},
	}}
	now := Snapshot{Languages: []LanguageSnapshot{
		{Language: LanguageGo,
			Measures: map[string]float64{"unit_size > 15": 65.6},
			// Absent from the baseline: entanglement was not recorded then.
			Entanglement: map[string]float64{"violation_degree": 0.056}},
		{Language: LanguageTypeScript, Measures: map[string]float64{"unit_size > 15": 33.1}},
	}}

	moves := compareSnapshots(&base, now)
	byMeasure := movementsByMeasure(moves)
	if got := byMeasure["entanglement violation_degree"]; got.Kind != "new" || got.Now != 0.056 {
		t.Errorf("an entanglement figure absent from the baseline = %+v, want kind new at 0.056", got)
	}

	// Go before TypeScript, and within a language by measure name.
	var order []string
	for _, m := range moves {
		order = append(order, m.Language)
	}
	if !slices.IsSorted(order) {
		t.Errorf("movements are ordered %v; the rows must group by language", order)
	}

	b := &strings.Builder{}
	writeMovement(b, &base, moves)
	out := b.String()
	if !strings.Contains(out, "+1.4") {
		t.Errorf("a figure that got worse must carry its sign, got:\n%s", out)
	}
	if !strings.Contains(out, "-1.8") {
		t.Errorf("a figure that improved must keep its sign too, got:\n%s", out)
	}
	if strings.Contains(out, "+-") {
		t.Errorf("the sign was applied twice:\n%s", out)
	}
}
