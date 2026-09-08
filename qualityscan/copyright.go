package main

import (
	"path"
	"regexp"
)

// copyrightPattern matches a genuine copyright or license notice: a
// standalone "Copyright" mark, an SPDX tag, a line reserving every right to
// the author, or a grant naming a specific open-source license. It is
// deliberately not "contains the word license": business-logic comments use
// "license"/"licenses" as an ordinary verb, describing what some rule or role
// permits, and a keyword match reports every one of those as a notice. A
// precise pattern reports zero on that prose and still catches the thing the
// rule exists to prevent, a header pasted in along with the code under it.
//
// Deliberately not written out below: the reserved-rights phrase itself,
// spelled the way a real notice spells it, would make this comment a finding
// of its own rule.
var copyrightPattern = regexp.MustCompile(
	`(?i)\bcopyright\b\s*(\(c\)|©|\d{4})` +
		`|\bSPDX-License-Identifier\b` +
		`|\ball rights reserved\b` +
		`|\blicensed under (the )?(MIT|Apache|BSD|GNU|GPL|LGPL|MPL|ISC)\b`,
)

// findCopyrightOrLicenseNotices reports a comment carrying a real copyright
// or license notice: the kind that arrives copied in from somewhere else,
// rather than prose authored in place.
func (idx *Index) findCopyrightOrLicenseNotices() []Literal {
	var out []Literal
	for _, f := range idx.Files {
		decls := idx.declRanges(f)
		for _, group := range f.AST.Comments {
			element := enclosingElement(decls, group.Pos(), path.Base(f.Rel))
			for _, c := range group.List {
				if copyrightPattern.MatchString(c.Text) {
					out = append(out, Literal{
						Element: element,
						File:    f,
						Line:    idx.Fset.Position(c.Pos()).Line,
						Value:   trimComment(c.Text),
					})
				}
			}
		}
	}
	return out
}
