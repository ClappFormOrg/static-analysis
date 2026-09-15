package dmm

import (
	"math"
	"testing"
)

// unit builds a Go unit with the three measurements the model reads.
func unit(file string, loc, mccabe, params int) Unit {
	return Unit{Language: "Go", File: file, Element: "F", Line: 1, LOC: loc, McCabe: mccabe, Params: params}
}

// scoreOf pulls one property's score out of a single-language report.
func scoreOf(t *testing.T, r Report, id string) (float64, bool) {
	t.Helper()
	if len(r.Languages) != 1 {
		t.Fatalf("want 1 language, got %d", len(r.Languages))
	}
	for _, p := range r.Languages[0].Properties {
		if p.ID == id {
			return p.Score, p.Measured
		}
	}
	t.Fatalf("no property %q in report", id)
	return 0, false
}

func wantScore(t *testing.T, r Report, id string, want float64) {
	t.Helper()
	got, ok := scoreOf(t, r, id)
	if !ok {
		t.Fatalf("%s: not measured, want %.2f", id, want)
	}
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %.4f, want %.4f", id, got, want)
	}
}

// TestDirectionOfChangeDecidesTheScore holds the four movements the model is
// built out of. Each is run on its own so a sign error in one cannot be hidden
// by another.
func TestDirectionOfChangeDecidesTheScore(t *testing.T) {
	low := unit("a.go", 10, 2, 1)
	high := unit("a.go", 50, 20, 5)

	tests := []struct {
		name   string
		before []Unit
		after  []Unit
		want   float64
	}{
		{"adding low-risk code is entirely good", nil, []Unit{low}, 1.0},
		{"adding high-risk code is entirely bad", nil, []Unit{high}, 0.0},
		{"removing high-risk code is entirely good", []Unit{high}, nil, 1.0},
		{"removing low-risk code is entirely bad", []Unit{low}, nil, 0.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Compare(tt.before, tt.after)
			for _, p := range Properties() {
				wantScore(t, r, p.ID, tt.want)
			}
		})
	}
}

// TestUntouchedChangeIsNotMeasuredRatherThanZero is the distinction a gate
// depends on. A branch that moved nothing this property measures has no score,
// and both available numbers are wrong: 0.0 fails a threshold for a change that
// did no harm, and 1.0 passes one for a change nobody measured.
func TestUntouchedChangeIsNotMeasuredRatherThanZero(t *testing.T) {
	same := []Unit{unit("a.go", 10, 2, 1)}

	r := Compare(same, same)
	for _, p := range Properties() {
		if _, ok := scoreOf(t, r, p.ID); ok {
			t.Errorf("%s: measured, want not measured for an unchanged tree", p.ID)
		}
	}
	if got := r.Languages[0].FilesTouched; got != 0 {
		t.Errorf("FilesTouched = %d, want 0", got)
	}
}

// TestPropertiesAreScoredIndependently pins the case that reads as a bug and is
// not one. Shrinking a 50-line unit to 10 lines is entirely good for unit size,
// and entirely bad for unit complexity: the unit was already under the
// complexity boundary, so all 50 of its lines counted as low-risk complexity,
// and 40 of them have now gone. The model charges every departure of low-risk
// code, whichever property made it low-risk.
func TestPropertiesAreScoredIndependently(t *testing.T) {
	before := []Unit{unit("a.go", 50, 3, 1)}
	after := []Unit{unit("a.go", 10, 3, 1)}

	r := Compare(before, after)
	wantScore(t, r, "unit_size", 1.0)
	wantScore(t, r, "unit_complexity", 0.0)
	wantScore(t, r, "unit_interfacing", 0.0)
}

// TestExtractionScoresWell is the movement the model exists to reward: one
// oversized unit replaced by three that are each under the boundary. Every line
// leaves the high-risk half and arrives in the low-risk one, so both halves of
// the delta point the same way.
func TestExtractionScoresWell(t *testing.T) {
	before := []Unit{unit("a.go", 90, 3, 1)}
	after := []Unit{unit("a.go", 10, 3, 1), unit("a.go", 10, 3, 1), unit("a.go", 10, 3, 1)}

	r := Compare(before, after)
	wantScore(t, r, "unit_size", 1.0)

	got := r.Languages[0].Properties[0].Delta
	// 30 low-risk lines arrived, 90 high-risk lines left.
	if got.Good != 120 || got.Bad != 0 {
		t.Errorf("delta = %+v, want {Good:120 Bad:0}", got)
	}
}

// TestInterfacingBoundaryIsInclusive holds the one-word difference that decides
// where this property's low category ends. The model opens unit interfacing's
// moderate band at "3 or more parameters" where unit size opens at "more than
// 15 lines", so two parameters is the last low-risk value and three is not.
func TestInterfacingBoundaryIsInclusive(t *testing.T) {
	r := Compare(nil, []Unit{unit("a.go", 10, 2, 2)})
	wantScore(t, r, "unit_interfacing", 1.0)

	r = Compare(nil, []Unit{unit("a.go", 10, 2, 3)})
	wantScore(t, r, "unit_interfacing", 0.0)
}

// TestLanguagesAreScoredSeparately keeps a large surface's churn out of a small
// one's verdict. The Go change is entirely good and the TypeScript change
// entirely bad, and neither may move the other.
func TestLanguagesAreScoredSeparately(t *testing.T) {
	before := []Unit{{Language: "Go", File: "a.go", LOC: 50, McCabe: 20, Params: 5}}
	after := []Unit{{Language: "TypeScript", File: "x.ts", LOC: 50, McCabe: 20, Params: 5}}

	r := Compare(before, after)
	if len(r.Languages) != 2 {
		t.Fatalf("want 2 languages, got %d", len(r.Languages))
	}
	if r.Languages[0].Language != "Go" || r.Languages[1].Language != "TypeScript" {
		t.Fatalf("languages not sorted: %q, %q", r.Languages[0].Language, r.Languages[1].Language)
	}
	for _, p := range r.Languages[0].Properties {
		if p.Score != 1.0 {
			t.Errorf("Go %s = %.2f, want 1.00", p.ID, p.Score)
		}
	}
	for _, p := range r.Languages[1].Properties {
		if p.Score != 0.0 {
			t.Errorf("TypeScript %s = %.2f, want 0.00", p.ID, p.Score)
		}
	}
}

// TestRenameLandsAtAHalfAndIsVisibleInTheCounts is the known distortion. The
// same clean file at a new path books equal good and bad, so the score says
// 0.5 for a change that altered nothing. The added and removed counts are what
// let a reader recognise it, which is why they are reported rather than folded
// into FilesTouched.
func TestRenameLandsAtAHalfAndIsVisibleInTheCounts(t *testing.T) {
	before := []Unit{unit("old.go", 200, 2, 1)}
	after := []Unit{unit("new.go", 200, 2, 1)}

	r := Compare(before, after)
	wantScore(t, r, "unit_size", 0.5)

	lang := r.Languages[0]
	if lang.FilesAdded != 1 || lang.FilesRemoved != 1 {
		t.Errorf("added/removed = %d/%d, want 1/1", lang.FilesAdded, lang.FilesRemoved)
	}
	if lang.FilesTouched != 2 {
		t.Errorf("FilesTouched = %d, want 2", lang.FilesTouched)
	}
}

// TestUnchangedFilesDoNotDiluteTheScore is what makes taking two whole
// inventories equivalent to reading a diff. The untouched file is far larger
// than the changed one and must not move the answer.
func TestUnchangedFilesDoNotDiluteTheScore(t *testing.T) {
	untouched := unit("big.go", 900, 2, 1)
	before := []Unit{untouched}
	after := []Unit{untouched, unit("new.go", 10, 2, 1)}

	r := Compare(before, after)
	wantScore(t, r, "unit_size", 1.0)
	if got := r.Languages[0].FilesTouched; got != 1 {
		t.Errorf("FilesTouched = %d, want 1", got)
	}
}

// TestChurnCountsBothDirections guards the accessor the report divides by.
func TestChurnCountsBothDirections(t *testing.T) {
	d := Delta{Good: 3, Bad: 4}
	if got := d.Churn(); got != 7 {
		t.Errorf("Churn() = %d, want 7", got)
	}
	if _, ok := (Delta{}).Score(); ok {
		t.Error("empty delta reports a score")
	}
}
