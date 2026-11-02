// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo_test

import (
	"strings"
	"testing"

	"github.com/haproxy-haptic/scriggo"
	"github.com/haproxy-haptic/scriggo/native"
)

// TestImportedVarInitializedForEveryImporter covers a file that exports a
// package-level variable and is imported both by a file rendered directly and
// by a file rendered from inside a macro of another imported file.
func TestImportedVarInitializedForEveryImporter(t *testing.T) {
	fsys := scriggo.Files{
		"main.txt":   []byte(`{% import "chain.txt" for Chain %}{{ render "direct.txt" }}|{{ Chain() }}`),
		"chain.txt":  []byte(`{% macro Chain() %}{{ render "nested.txt" }}{% end %}`),
		"direct.txt": []byte(`{% import "util.txt" for F %}{{ F() }}`),
		"nested.txt": []byte(`{% import "util.txt" for F %}{{ F() }}`),
		"util.txt":   []byte(`{% var F = func() string { return "f" } %}`),
	}
	template, err := scriggo.BuildTemplate(fsys, "main.txt", nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var out strings.Builder
	if err := template.Run(&out, nil, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := out.String(); got != "f|f" {
		t.Fatalf("got %q, want %q", got, "f|f")
	}
}

// TestImportedVarInitializationOrder pins both run orders. The import site
// emitted first (inside Chain) initializes whenever it runs; the other one
// initializes only while nothing has.
func TestImportedVarInitializationOrder(t *testing.T) {
	for name, test := range map[string]struct {
		main  string
		want  string
		inits int
	}{
		"direct first": {`{% import "chain.txt" for Chain %}{{ render "direct.txt" }}|{{ Chain() }}`, "1|2", 2},
		"macro first":  {`{% import "chain.txt" for Chain %}{{ Chain() }}|{{ render "direct.txt" }}`, "1|1", 1},
	} {
		t.Run(name, func(t *testing.T) {
			inits := 0
			fsys := scriggo.Files{
				"main.txt":   []byte(test.main),
				"chain.txt":  []byte(`{% macro Chain() %}{{ render "nested.txt" }}{% end %}`),
				"direct.txt": []byte(`{% import "util.txt" for N %}{{ N }}`),
				"nested.txt": []byte(`{% import "util.txt" for N %}{{ N }}`),
				"util.txt":   []byte(`{% var N = count() %}`),
			}
			template, err := scriggo.BuildTemplate(fsys, "main.txt", &scriggo.BuildOptions{
				Globals: native.Declarations{"count": func() int { inits++; return inits }},
			})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var out strings.Builder
			if err := template.Run(&out, nil, nil); err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := out.String(); got != test.want || inits != test.inits {
				t.Fatalf("got %q after %d initializations, want %q after %d", got, inits, test.want, test.inits)
			}
		})
	}
}
