package main

import (
	"testing"

	"github.com/ClappFormOrg/static-analysis/qualityscan/internal/dmm"
)

// TestDMMLowRiskAlignsWithSIGLowCategory holds the one number the delta model
// and the risk profile share.
//
// internal/dmm splits every unit into low-risk and not, and cmd/deltagate scores
// a change by how lines move across that split. The boundary it splits on is not
// a second opinion: it is the top of this profile's low category, which is the
// same place the first cumulative tail begins. Alves, Ypma and Visser derived
// both from the same benchmark.
//
// Two copies of a threshold drift. The delta model cannot import sigProperties
// -- it is in package main, which nothing can import -- so the numbers are
// spelled twice by necessity, and this test is what makes that safe. A cap
// edited in sigprofile.go without the matching edit in internal/dmm fails here
// rather than quietly scoring pull requests against last year's boundary.
func TestDMMLowRiskAlignsWithSIGLowCategory(t *testing.T) {
	deltaProps := map[string]dmm.Property{}
	for _, p := range dmm.Properties() {
		deltaProps[p.ID] = p
	}

	sig := sigProperties()
	if len(deltaProps) != len(sig) {
		t.Fatalf("delta model covers %d properties, the profile has %d unit properties",
			len(deltaProps), len(sig))
	}

	for _, p := range sig {
		t.Run(p.ID, func(t *testing.T) {
			d, ok := deltaProps[p.ID]
			if !ok {
				t.Fatalf("the delta model has no property %q", p.ID)
			}

			// The low category's upper bound is the last low-risk value.
			if got := p.Categories[0].Max; got != d.LowRiskMax {
				t.Errorf("low category ends at %d, the delta model splits at %d", got, d.LowRiskMax)
			}

			// And the first tail opens immediately above it. This is the half
			// that catches the inclusive/exclusive wording: unit interfacing's
			// tail is worded ">= 3" where size's is "> 15", so a delta boundary
			// copied across from the wrong property fails here.
			tail := p.Tails[0]
			if tail.Holds(d.LowRiskMax) {
				t.Errorf("%d is in the %s tail, so it is not low risk", d.LowRiskMax, tail.Label())
			}
			if !tail.Holds(d.LowRiskMax + 1) {
				t.Errorf("%d is outside the %s tail, so the boundary sits below the tail",
					d.LowRiskMax+1, tail.Label())
			}
		})
	}
}
