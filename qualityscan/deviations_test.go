package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDeviations materialises a deviation file and returns its path.
func writeDeviations(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "deviations.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// validDeviation is a complete entry, for tests that want to remove exactly one
// field and see the loader reject it.
const validDeviation = `{"deviations":[{
  "language":"Go","property":"unit_interfacing","tail":">= 3","decided":"2026-09-06",
  "measured":"44.1% against a 15.0% cap","ref":"https://example.invalid/1",
  "summary":"ctx is counted as a parameter",
  "reasoning":["no unit reaches 5 parameters"],
  "still_gated":"the >= 5 and >= 7 tiers keep their caps"}]}`

// TestDeviationLoadsAndLooksUpByTail is the happy path: a well-formed entry
// loads, and it is found by the exact language, property and tail label the
// report renders with.
func TestDeviationLoadsAndLooksUpByTail(t *testing.T) {
	devs, err := LoadDeviations(writeDeviations(t, validDeviation))
	if err != nil {
		t.Fatal(err)
	}
	d, ok := devs.For("Go", "unit_interfacing", ">= 3")
	if !ok {
		t.Fatal("the deviation did not come back for the tail it declares")
	}
	if d.Summary != "ctx is counted as a parameter" {
		t.Errorf("summary = %q", d.Summary)
	}
	if !devs.Any("Go") {
		t.Error("Any(Go) = false with a Go deviation loaded")
	}
	if devs.Any("TypeScript") {
		t.Error("Any(TypeScript) = true with no TypeScript deviation")
	}
}

// TestDeviationCoversOneTailOnly is the property that stops a deviation on the
// moderate band from quietly excusing the whole measure. The real entry covers
// `>= 3`; the tiers above it must not resolve to it, because a future unit at 5
// parameters is exactly what the record promises is still reported.
func TestDeviationCoversOneTailOnly(t *testing.T) {
	devs, err := LoadDeviations(writeDeviations(t, validDeviation))
	if err != nil {
		t.Fatal(err)
	}
	for _, tail := range []string{">= 5", ">= 7", "> 3"} {
		if _, ok := devs.For("Go", "unit_interfacing", tail); ok {
			t.Errorf("the `>= 3` deviation also answered for %q", tail)
		}
	}
	// And it does not leak across languages or across properties.
	if _, ok := devs.For("TypeScript", "unit_interfacing", ">= 3"); ok {
		t.Error("the Go deviation answered for TypeScript")
	}
	if _, ok := devs.For("Go", "unit_size", ">= 3"); ok {
		t.Error("the unit_interfacing deviation answered for unit_size")
	}
}

// TestDeviationNeedsItsReasoning holds the line between a deviation and a
// suppression. Each of these fields is what makes the record reviewable, and an
// entry missing one is rejected rather than loaded with a blank.
func TestDeviationNeedsItsReasoning(t *testing.T) {
	cases := map[string]string{
		"reasoning": `{"deviations":[{"language":"Go","property":"unit_interfacing","tail":">= 3",
		  "decided":"2026-09-06","summary":"s","ref":"r"}]}`,
		"summary": `{"deviations":[{"language":"Go","property":"unit_interfacing","tail":">= 3",
		  "decided":"2026-09-06","reasoning":["r"],"ref":"r"}]}`,
		"decided": `{"deviations":[{"language":"Go","property":"unit_interfacing","tail":">= 3",
		  "summary":"s","reasoning":["r"],"ref":"r"}]}`,
		"ref": `{"deviations":[{"language":"Go","property":"unit_interfacing","tail":">= 3",
		  "decided":"2026-09-06","summary":"s","reasoning":["r"]}]}`,
		"tail": `{"deviations":[{"language":"Go","property":"unit_interfacing",
		  "decided":"2026-09-06","summary":"s","reasoning":["r"],"ref":"r"}]}`,
		"language": `{"deviations":[{"property":"unit_interfacing","tail":">= 3",
		  "decided":"2026-09-06","summary":"s","reasoning":["r"],"ref":"r"}]}`,
	}
	for missing, body := range cases {
		if _, err := LoadDeviations(writeDeviations(t, body)); err == nil {
			t.Errorf("an entry with no %s loaded; it should be rejected", missing)
		}
	}

	// An empty reasoning list is the same failure as an absent one: a conclusion
	// with no argument.
	empty := `{"deviations":[{"language":"Go","property":"unit_interfacing","tail":">= 3",
	  "decided":"2026-09-06","summary":"s","reasoning":[],"ref":"r"}]}`
	if _, err := LoadDeviations(writeDeviations(t, empty)); err == nil {
		t.Error("an entry with an empty reasoning list loaded")
	}
}

// TestUnknownPropertyFailsTheRun holds that a typo cannot render nothing in
// silence. A deviation naming a property that does not exist would simply never
// match, and the report would print a bare `fail` with the reasoning sitting
// unused on disk.
func TestUnknownPropertyFailsTheRun(t *testing.T) {
	body := `{"deviations":[{"language":"Go","property":"unit_interfacng","tail":">= 3",
	  "decided":"2026-09-06","summary":"s","reasoning":["r"],"ref":"r"}]}`
	_, err := LoadDeviations(writeDeviations(t, body))
	if err == nil {
		t.Fatal("a deviation naming an unknown property loaded")
	}
	if !strings.Contains(err.Error(), "unknown property") {
		t.Errorf("error = %v, want it to name the unknown property", err)
	}
}

// TestDuplicateDeviationFailsTheRun keeps one tail from carrying two records,
// where the one that rendered would depend on map order.
func TestDuplicateDeviationFailsTheRun(t *testing.T) {
	body := `{"deviations":[
	  {"language":"Go","property":"unit_interfacing","tail":">= 3","decided":"2026-09-06",
	   "summary":"first","reasoning":["r"],"ref":"r"},
	  {"language":"Go","property":"unit_interfacing","tail":">= 3","decided":"2026-09-06",
	   "summary":"second","reasoning":["r"],"ref":"r"}]}`
	if _, err := LoadDeviations(writeDeviations(t, body)); err == nil {
		t.Error("the same tail loaded twice")
	}
}

// TestNoDeviationFileRendersNothing holds that the file is optional. A report
// produced without one is a report where every failing tail is simply failing.
func TestNoDeviationFileRendersNothing(t *testing.T) {
	devs, err := LoadDeviations("")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := devs.For("Go", "unit_interfacing", ">= 3"); ok {
		t.Error("an empty set answered a lookup")
	}
	if devs.Any("Go") {
		t.Error("an empty set reported a deviation")
	}
}

// TestDeviationDoesNotChangeTheVerdict is the property that separates this from
// a suppression, and the one the ticket that introduced it asked for by name.
// The tail still measures what it measures and still prints `fail`; the
// deviation is additional prose beneath it.
func TestDeviationDoesNotChangeTheVerdict(t *testing.T) {
	// Two units at 4 parameters put the whole of the measured LOC in the `>= 3`
	// tail, well over the 15.0% cap.
	units := []Unit{unit(10, 1, 4), unit(10, 1, 4)}
	p := profileFor(t, "unit_interfacing", units)

	var moderate *TailShare
	for _, c := range p.Categories {
		if c.Tail != nil && c.Tail.Label == ">= 3" {
			moderate = c.Tail
		}
	}
	if moderate == nil {
		t.Fatal("no `>= 3` tail in the profile")
	}
	if moderate.Pass {
		t.Fatal("the fixture was supposed to fail its cap")
	}

	devs, err := LoadDeviations(writeDeviations(t, validDeviation))
	if err != nil {
		t.Fatal(err)
	}

	b := &strings.Builder{}
	writeProperty(b, p, "Go", devs)
	out := b.String()

	// The verdict, the measured share and the LOC-to-move all survive.
	if !strings.Contains(out, "fail") {
		t.Error("the deviation removed the `fail` verdict from the table")
	}
	if !strings.Contains(out, "100.0%") {
		t.Error("the deviation changed the measured share")
	}
	// And the reasoning is rendered beneath it, after the table.
	if !strings.Contains(out, "Permanent deviation") {
		t.Error("the deviation did not render")
	}
	if strings.Index(out, "Permanent deviation") < strings.Index(out, "fail") {
		t.Error("the deviation rendered above the verdict it explains")
	}

	// Without the record, the same profile renders the same verdict and no prose.
	plain := &strings.Builder{}
	writeProperty(plain, p, "Go", Deviations{})
	if !strings.Contains(plain.String(), "fail") {
		t.Error("the verdict depends on the deviation file")
	}
	if strings.Contains(plain.String(), "Permanent deviation") {
		t.Error("a deviation rendered with no deviation file loaded")
	}
}

// TestCommittedDeviationsLoad holds the checked-in record to the same rules as
// any other, so a hand-edit that drops the reasoning fails the build rather than
// silently rendering less.
func TestCommittedDeviationsLoad(t *testing.T) {
	devs, err := LoadDeviations(configFile(t, "deviations.json"))
	if err != nil {
		t.Fatal(err)
	}
	d, ok := devs.For("Go", "unit_interfacing", ">= 3")
	if !ok {
		t.Fatal("the recorded unit_interfacing deviation is not loading")
	}
	if d.StillGated == "" {
		t.Error("the recorded deviation does not say what it leaves gated")
	}
	// The tiers it promises are still gated must not carry a record of their own.
	for _, tail := range []string{">= 5", ">= 7"} {
		if _, ok := devs.For("Go", "unit_interfacing", tail); ok {
			t.Errorf("%s carries a deviation, which the record says it does not", tail)
		}
	}
}
