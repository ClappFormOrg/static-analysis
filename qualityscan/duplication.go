package main

import (
	"fmt"
	"go/scanner"
	"go/token"
	"sort"
	"strings"
)

// Duplication: the share of lines of code that would not exist if every
// fragment were written once.
//
// The model's definition is exact, so this implements it rather than
// approximating it. A fragment counts when it is at least 6 lines of code long
// and repeats literally, modulo whitespace, in at least one other location. With
// optimal reuse the fragment would occur once, so the redundancy of a fragment
// occurring N times is (N-1) times its length. Four stars needs the redundant
// share of each language to stay at or below 5.6%.
//
// `make duplication-scan` (jscpd) stays, and the two numbers are not comparable:
// jscpd accumulates per detected clone pair over the TOTAL lines of the analysed
// files, blanks and comments included, where this counts occurrences after the
// first over LINES OF CODE. The denominator alone is about a third smaller here.
// A tree can therefore sit either side of the 5.6% cap depending only on which
// definition is applied, and the model's own definition is the one its cap
// belongs to.
//
// Normalisation is by token, not by text. Two lines are equal "modulo
// whitespace" exactly when their token sequences are equal, which makes
// indentation and spacing free by construction and comments free as well, since
// the scanner does not emit them. It also means the lines counted here are the
// same lines countLOCIn counts, so duplication's denominator and the module
// denominator cannot drift apart. TestDuplicationLinesMatchLOC holds that.

// duplicationMinLines is the model's fragment length, in lines of code. Not
// configurable: it is part of the definition the 5.6% cap is calibrated against,
// so a local override would produce a number that is no longer the property.
const duplicationMinLines = 6

// duplicationCapPct is the 4-star cap from SIGModel SIGModelVersion, section 3.2.
const duplicationCapPct = 5.6

// normalisedLines returns one entry per line of Go code, in order, each holding
// that line's token sequence. A blank line and a comment-only line produce no
// entry, exactly as they contribute nothing to countLOCIn.
func normalisedLines(src []byte) []string {
	fset := token.NewFileSet()
	tf := fset.AddFile("", fset.Base(), len(src))
	var s scanner.Scanner
	s.Init(tf, src, nil, 0)

	byLine := map[int]*strings.Builder{}
	var order []int
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		// The semicolon Go's grammar inserts at a line end is not written in the
		// source. Counting it would make a line of pure `}` differ from one
		// written `};`, which is a whitespace-level difference.
		if tok == token.SEMICOLON && lit == "\n" {
			continue
		}
		line := tf.Position(pos).Line
		b := byLine[line]
		if b == nil {
			b = &strings.Builder{}
			byLine[line] = b
			order = append(order, line)
		} else {
			b.WriteByte(' ')
		}
		if lit != "" {
			b.WriteString(lit)
			continue
		}
		b.WriteString(tok.String())
	}

	sort.Ints(order)
	out := make([]string, 0, len(order))
	for _, line := range order {
		out = append(out, byLine[line].String())
	}
	return out
}

// DuplicationProfile is the measured property for one language.
type DuplicationProfile struct {
	MinLines     int
	RedundantLOC int
	TotalLOC     int
	Pct          float64
	CapPct       float64
	Pass         bool
	AtCap        bool
	Applies      bool
	// ExcessLOC is how many redundant lines have to go for the share to reach
	// its cap. Zero when it already does.
	ExcessLOC int
	// Modules carries the per-module redundancy, worst first, so the percentage
	// can be read as a list of places rather than a single number.
	Modules []ModuleDuplication
	// ModulesWithRedundancy is how many modules carry any, so a truncated table
	// can say what it left out.
	ModulesWithRedundancy int
}

// ModuleDuplication is one module's share of the redundancy.
type ModuleDuplication struct {
	File         string
	LOC          int
	RedundantLOC int
	Pct          float64
}

// measureDuplication computes the property over modules carrying their
// normalised lines. It returns nil when no module supplies any, which the report
// states rather than printing 0% against the cap.
//
// The counting rule, and the one detail that decides whether this stays a
// percentage: redundancy is a SET of lines, not a sum over fragments. Every
// window of duplicationMinLines lines is grouped with the windows identical to
// it, and for each group every occurrence after the first has its lines marked.
// Summing fragment lengths instead would double count wherever two clones
// overlap, and a file could then report more redundant lines than it has.
//
// Marking every occurrence but the earliest is what makes this (N-1) times the
// length for a fragment occurring N times. Where two overlapping groups disagree
// about which occurrence is earliest, the union can mark a line that a
// fragment-by-fragment count would not, so the figure is an upper bound in that
// case. It is bounded by the module's own LOC either way, which is the property
// that matters: a percentage that can exceed 100 is not one.
func measureDuplication(mods []Module, verdicts bool) *DuplicationProfile {
	type occurrence struct {
		mod   int
		start int
	}

	// Intern each distinct line once, so window comparison is integer work.
	ids := map[string]int{}
	lines := make([][]int, len(mods))
	total := 0
	supplied := false
	for i, m := range mods {
		if len(m.Lines) > 0 {
			supplied = true
		}
		seq := make([]int, 0, len(m.Lines))
		for _, l := range m.Lines {
			id, ok := ids[l]
			if !ok {
				id = len(ids)
				ids[l] = id
			}
			seq = append(seq, id)
		}
		lines[i] = seq
		total += len(seq)
	}
	if !supplied || total == 0 {
		return nil
	}

	windows := map[string][]occurrence{}
	for i, seq := range lines {
		for start := 0; start+duplicationMinLines <= len(seq); start++ {
			key := windowKey(seq[start : start+duplicationMinLines])
			windows[key] = append(windows[key], occurrence{mod: i, start: start})
		}
	}

	redundant := make([]map[int]bool, len(mods))
	for _, occs := range windows {
		if len(occs) < 2 {
			continue
		}
		sort.Slice(occs, func(a, b int) bool {
			if occs[a].mod != occs[b].mod {
				return occs[a].mod < occs[b].mod
			}
			return occs[a].start < occs[b].start
		})
		// occs[0] is the one occurrence optimal reuse would keep.
		for _, o := range occs[1:] {
			if redundant[o.mod] == nil {
				redundant[o.mod] = map[int]bool{}
			}
			for k := range duplicationMinLines {
				redundant[o.mod][o.start+k] = true
			}
		}
	}

	p := &DuplicationProfile{
		MinLines: duplicationMinLines,
		TotalLOC: total,
		CapPct:   duplicationCapPct,
		Applies:  verdicts,
	}
	for i, m := range mods {
		n := len(redundant[i])
		if n == 0 {
			continue
		}
		p.RedundantLOC += n
		p.ModulesWithRedundancy++
		p.Modules = append(p.Modules, ModuleDuplication{
			File:         m.File,
			LOC:          len(lines[i]),
			RedundantLOC: n,
			Pct:          pct(n, len(lines[i])),
		})
	}
	p.Pct = pct(p.RedundantLOC, total)
	p.Pass = p.Pct <= duplicationCapPct
	p.AtCap = p.Pct <= duplicationCapPct+atCapMargin && p.Pct >= duplicationCapPct-atCapMargin
	if !p.Pass {
		p.ExcessLOC = int(float64(p.RedundantLOC) - duplicationCapPct/100*float64(total) + 0.999999)
	}

	sort.Slice(p.Modules, func(a, b int) bool {
		if p.Modules[a].RedundantLOC != p.Modules[b].RedundantLOC {
			return p.Modules[a].RedundantLOC > p.Modules[b].RedundantLOC
		}
		return p.Modules[a].File < p.Modules[b].File
	})
	return p
}

// windowKey renders a window of interned line ids as a comparison key. Ids
// rather than text keeps the key short, and the separator keeps `1,23` from
// colliding with `12,3`.
func windowKey(ids []int) string {
	b := &strings.Builder{}
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(b, "%d", id)
	}
	return b.String()
}

// duplicationTableLimit is how many modules the report lists. The count it left
// out is printed beside it: a table that silently truncates reads as a complete
// one.
const duplicationTableLimit = 10
