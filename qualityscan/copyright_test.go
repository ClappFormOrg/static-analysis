package main

import "testing"

func TestFindCopyrightOrLicenseNotices(t *testing.T) {
	idx := load(t, map[string]string{
		"a/a.go": `package a

// Copyright (c) 2019 Some Vendor, Inc. All rights reserved.
// SPDX-License-Identifier: MIT

func Pasted() {
	// Licensed under the Apache License, Version 2.0.
	_ = 1
}

// Filter checks what the role gate licenses: the unscoped visibility axis
// below is not a license to skip the tenant scope, so it declines.
func Filter() {}
`,
	}, DefaultConfig())

	notices := idx.findCopyrightOrLicenseNotices()
	if len(notices) != 3 {
		t.Fatalf("got %d notices, want 3 (the copyright mark and the SPDX tag "+
			"ahead of Pasted, plus the license grant inside its body); "+
			"business prose on Filter using \"license\" as a verb must not "+
			"match: %+v", len(notices), notices)
	}

	byElement := map[string]int{}
	for _, n := range notices {
		byElement[n.Element]++
	}
	if byElement["a.go"] != 2 {
		t.Errorf("got %d notices attributed to the file, want 2 (the doc "+
			"comment ahead of Pasted falls outside its declaration range): %+v",
			byElement["a.go"], notices)
	}
	if byElement["Pasted"] != 1 {
		t.Errorf("got %d notices attributed to Pasted, want 1 (the comment "+
			"inside its body): %+v", byElement["Pasted"], notices)
	}
}
