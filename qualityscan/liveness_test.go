package main

import (
	"fmt"
	"strings"
	"testing"
)

// TestEveryRuleStillFires is the guard against a rule going quiet.
//
// A threshold rule that stops matching does not fail anything: it reports zero,
// the summary table prints a zero, and a zero reads as good news. That is how
// FUNCTION_COMPLEXITY_RISK and DEPENDENCY_SPAN_RISK came to sit at zero across
// a whole module for a full release cycle while another tool's output was naming
// sixteen findings between them -- the thresholds had been set from the
// the conventional printed bands for metrics that turned out not to be on that
// scale, so nothing in the codebase could reach them.
//
// The fixture below is built to trip every rule at the DEFAULT thresholds, so
// moving a default past what the fixture can reach fails here. That is the
// point: it makes "this threshold is now unreachable" a test failure rather
// than a quiet zero. When a default legitimately moves, move the fixture with
// it rather than exempting the rule.
func TestEveryRuleStillFires(t *testing.T) {
	idx := load(t, livenessFixture(), DefaultConfig())

	counts := map[string]int{}
	for _, f := range Run(idx, "") {
		counts[f.Rule]++
	}

	for _, rule := range ruleOrder {
		if counts[rule] == 0 {
			t.Errorf("%s fired on nothing; either the rule is broken or its default threshold is out of reach", rule)
		}
	}
}

// livenessFixture is a module written to be bad in every way the scanner knows
// how to name, one rule at a time. It is not meant to be realistic and it is
// not meant to compile -- the scanner parses rather than builds.
func livenessFixture() map[string]string {
	files := map[string]string{
		// CYCLIC_REFERENCE: server and service point at each other through a
		// leaf package parked under server/.
		"server/wiring.go": `package server

import "example.com/m/service"

// New builds the server.
func New() { service.New() }
`,
		"server/interceptors/auth.go": `package interceptors

// Caller reads the caller id.
func Caller() string { return "" }
`,
		"service/svc.go": `package service

import "example.com/m/server/interceptors"

// New builds the service.
func New() string { return interceptors.Caller() }
`,

		// PARAMETER_RISK: more than the default four.
		"a/params.go": `package a

// Wide takes more parameters than anyone can pass in the right order.
func Wide(a, b, c, d, e, f int) {}
`,

		// HARDCODED_URL and HARDCODED_PATH. The host must be neither
		// allowlisted nor reserved, and the path must look like a filesystem.
		//
		// The leading comment also trips COPYRIGHT_OR_LICENSE_NOTICE: a real
		// notice, not the word "license" used as a verb.
		"a/literals.go": `package a

// Copyright (c) 2020 Some Vendor, Inc. All rights reserved.
const (
	// Endpoint is an address, so it belongs in configuration.
	Endpoint = "https://payments.internal-vendor.io/v1/charge"
	// ConfigFile is a filesystem path.
	ConfigFile = "/etc/svc/api.conf"
)
`,

		// The loop family, the branch family and the comment family.
		"a/tier2.go": `package a

import "strings"

// Loops trips the three loop rules at once: a query per iteration, a string
// built by repeated concatenation, and a loop inside a loop.
func Loops(ids []int, rows [][]string) string {
	var s string
	for _, id := range ids {
		db.Where("id = ?", id).Find(&row)
		for range rows {
			s += "x"
		}
	}
	return strings.TrimSpace(s)
}

// Branches trips the empty branch and the defaultless switch.
func Branches(n int) {
	if n > 0 {
	}
	switch n {
	case 1:
		println("one")
	case 2:
		println("two")
	}
}

// Debug writes to stdout from outside cmd/.
func Debug() {
	fmt.Println("here")
}

// Comments carries the two comment rules and a suppression.
//
//nolint:gocognit // pinned so SUPPRESSED_WARNING has something to find
func Comments() {
	// TODO: finish this
	// total := compute(a, b)
	return
}
`,

		// MISSING_DOC_COMMENT: exported, package level, undocumented.
		"a/undocumented.go": `package a

type Undocumented struct{}
`,
	}

	// FUNCTION_SIZE_RISK and FUNCTION_COMPLEXITY_RISK. Both scale with depth,
	// so generating them keeps the fixture honest about which threshold it is
	// clearing instead of hiding it in a wall of hand-written statements.
	files["a/heavy.go"] = "package a\n\n" +
		"// Deep nests ifs until cognitive complexity clears the default band.\n" +
		nestedIfs("Deep", 7) +
		"\n// Long is past the default token threshold.\n" +
		longFunc("Long", 90)

	// DEPENDENCY_VOLUME_RISK and DEPENDENCY_SPAN_RISK. Span counts one target
	// per package, so clearing it needs genuinely many packages; volume then
	// follows from calling into each of them repeatedly.
	const pkgs, callsEach = 30, 5
	var imports, body strings.Builder
	for i := range pkgs {
		fmt.Fprintf(&imports, "\t\"example.com/m/dep/p%d\"\n", i)
		for range callsEach {
			fmt.Fprintf(&body, "\t_ = p%d.F()\n", i)
		}
		files[fmt.Sprintf("dep/p%d/p.go", i)] = fmt.Sprintf(
			"package p%d\n\n// F is a target.\nfunc F() int { return 0 }\n", i)
	}
	files["hub/hub.go"] = fmt.Sprintf(
		"package hub\n\nimport (\n%s)\n\n// Hub touches every package there is.\nfunc Hub() {\n%s}\n",
		imports.String(), body.String())

	return files
}

// nestedIfs renders a function of cognitive complexity 1+2+...+depth, which is
// the shape the metric is designed to punish.
func nestedIfs(name string, depth int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "func %s(n int) int {\n\ttotal := 0\n", name)
	for i := range depth {
		fmt.Fprintf(&b, "%sif n > %d {\n", strings.Repeat("\t", i+1), i)
	}
	fmt.Fprintf(&b, "%stotal++\n", strings.Repeat("\t", depth+1))
	for i := depth - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "%s}\n", strings.Repeat("\t", i+1))
	}
	b.WriteString("\treturn total\n}\n")
	return b.String()
}

// longFunc renders a function of roughly 6 tokens per statement, with no
// branching, so it clears the size threshold without touching any other rule.
func longFunc(name string, stmts int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "func %s() int {\n\ttotal := 0\n", name)
	for i := range stmts {
		fmt.Fprintf(&b, "\ttotal += %d * 2\n", i)
	}
	b.WriteString("\treturn total\n}\n")
	return b.String()
}
