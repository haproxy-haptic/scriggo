// Copyright 2019 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"slices"
	"testing"
)

func Test_uncapitalize(t *testing.T) {
	cases := map[string]string{
		"name":         "name",
		"Name":         "name",
		"ADSL":         "adsl",
		"ADSLAndOther": "adslAndOther",
		"DoubleWord":   "doubleWord",
		"X":            "x",
		"unExported":   "unExported",
		"AbC":          "abC",
		"Èident":       "èident",
		"È":            "è",
		"ÀÈÈ":          "àèè",
		"àÀÈÒò":        "àÀÈÒò",
	}
	for input, expected := range cases {
		t.Run(input, func(t *testing.T) {
			got := uncapitalize(input)
			if got != expected {
				t.Fatalf("input: %q, expected %q, got %q", input, expected, got)
			}
		})
	}
}

func Test_nextGoVersion(t *testing.T) {
	tests := []struct {
		current string
		want    string
	}{
		{
			current: "go1.15",
			want:    "go1.16",
		},
		{
			current: "go1.8",
			want:    "go1.9",
		},
		{
			current: "go1.9",
			want:    "go1.10",
		},
		{
			current: "go1.20",
			want:    "go1.21",
		},
		{
			current: "go1.1000",
			want:    "go1.1001",
		},
	}
	for _, tt := range tests {
		t.Run(tt.current, func(t *testing.T) {
			if got := nextGoVersion(tt.current); got != tt.want {
				t.Errorf("nextGoVersion(%s) = %v, want %v", tt.current, got, tt.want)
			}
		})
	}
}

func Test_goVersionAtLeast(t *testing.T) {
	tests := []struct {
		version string
		minimum string
		want    bool
	}{
		{version: "go1.25", minimum: "go1.26", want: false},
		{version: "go1.26", minimum: "go1.26", want: true},
		{version: "go1.27", minimum: "go1.26", want: true},
	}
	for _, test := range tests {
		t.Run(test.version, func(t *testing.T) {
			if got := goVersionAtLeast(test.version, test.minimum); got != test.want {
				t.Fatalf("goVersionAtLeast(%q, %q) = %t, want %t", test.version, test.minimum, got, test.want)
			}
		})
	}
}

func Test_stdLibPathsIncludesPackagesIntroducedBeforeCurrentToolchain(t *testing.T) {
	for _, test := range []struct {
		version string
		want    bool
	}{
		{version: "go1.25", want: false},
		{version: "go1.26", want: true},
		{version: "go1.27", want: true},
	} {
		t.Run(test.version, func(t *testing.T) {
			if got := slices.Contains(stdLibPaths(test.version), "crypto/hpke"); got != test.want {
				t.Fatalf("stdLibPaths(%q) contains crypto/hpke = %t, want %t", test.version, got, test.want)
			}
		})
	}
}

func Test_checkPackagePath(t *testing.T) {
	cases := map[string]string{
		`main`:          ``,
		`internal`:      `use of internal package "internal" not allowed`,
		`internal/pkg`:  `use of internal package "internal/pkg" not allowed`,
		`internal/main`: `use of internal package "internal/main" not allowed`,
		`fmt`:           ``,
		`?`:             `invalid path path "?"`,
	}
	for path, want := range cases {
		t.Run(path, func(t *testing.T) {
			got := checkPackagePath(path)
			switch {
			case want == "" && got == nil:
				// Ok.
			case want == "" && got != nil:
				t.Fatalf("path '%s': no error expected, got '%s'", path, got)
			case want != "" && got == nil:
				t.Fatalf("path '%s': error '%s' expected, got nothing", path, want)
			case want != "" && got != nil:
				if want == got.Error() {
					// Ok
				} else {
					t.Fatalf("path: '%s': expecting error '%s', got '%s'", path, want, got)
				}
			}
		})
	}
}

func Test_hasStdlibPrefix(t *testing.T) {
	cases := map[string]bool{
		`main`:         false,
		`fmt`:          true,
		`archive`:      true,
		`archiv`:       false,
		`archive/tar`:  true,
		`testing`:      true,
		`path/to/pkg`:  true,
		`path/to/test`: true,
	}
	for path, want := range cases {
		t.Run(path, func(t *testing.T) {
			got := hasStdlibPrefix(path)
			if got != want {
				t.Fatalf("path '%s': expecting %t, got %t", path, want, got)
			}
		})
	}
}
