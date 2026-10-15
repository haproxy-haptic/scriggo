// Copyright 2025 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"strings"
	"testing"

	"github.com/haproxy-haptic/scriggo/native"
)

// TestWildcardForRange exercises the wildcard-slice-access fork
// extension end-to-end: parse → desugar → check → emit → run. Each
// case asserts the rendered output of a template that uses `[*]` in
// a for-range expression.
func TestWildcardForRange(t *testing.T) {
	type leaf struct {
		Vals []int
	}
	type mid struct {
		Leaves []leaf
		Label  string
	}
	type root struct {
		Mids []mid
	}

	mkRoot := func() *root {
		return &root{Mids: []mid{
			{Leaves: []leaf{{Vals: []int{10, 20}}, {Vals: []int{30}}}, Label: "a"},
			{Leaves: []leaf{{Vals: []int{40}}}, Label: "b"},
		}}
	}

	tests := []struct {
		name     string
		source   string
		data     *root
		expected string
	}{
		{
			name:     "single wildcard over outer slice's nested int slice",
			source:   `{% for _, m := range x.Mids %}{% for _, v := range m.Leaves[*].Vals %}{{ v }};{% end %}{% end %}`,
			data:     mkRoot(),
			expected: "10;20;30;40;",
		},
		{
			name:     "two wildcards: chained iteration over Mids and Leaves",
			source:   `{% for _, v := range x.Mids[*].Leaves[*].Vals %}{{ v }};{% end %}`,
			data:     mkRoot(),
			expected: "10;20;30;40;",
		},
		{
			name:     "two wildcards, two-var form (i is innermost index)",
			source:   `{% for i, v := range x.Mids[*].Leaves[*].Vals %}{{ i }}={{ v }};{% end %}`,
			data:     mkRoot(),
			expected: "0=10;1=20;0=30;0=40;",
		},
		{
			name:     "continue targets innermost loop",
			source:   `{% for _, v := range x.Mids[*].Leaves[*].Vals %}{% if v == 20 %}{% continue %}{% end %}{{ v }};{% end %}`,
			data:     mkRoot(),
			expected: "10;30;40;",
		},
		{
			name:     "break targets innermost loop (outer continues)",
			source:   `{% for _, v := range x.Mids[*].Leaves[*].Vals %}{% if v == 20 %}{% break %}{% end %}{{ v }};{% end %}`,
			data:     mkRoot(),
			expected: "10;30;40;",
		},
		{
			name:     "empty outer slice yields no output",
			source:   `{% for _, v := range x.Mids[*].Leaves[*].Vals %}{{ v }};{% end %}`,
			data:     &root{Mids: nil},
			expected: "",
		},
		{
			name:     "empty inner slices yield no output",
			source:   `{% for _, v := range x.Mids[*].Leaves[*].Vals %}{{ v }};{% end %}`,
			data:     &root{Mids: []mid{{Leaves: nil}, {Leaves: []leaf{{Vals: nil}}}}},
			expected: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fsys := Files{"index.html": []byte(tc.source)}
			opts := &BuildOptions{
				Globals: native.Declarations{
					"x": (*root)(nil),
				},
			}
			tmpl, err := BuildTemplate(fsys, "index.html", opts)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var out strings.Builder
			if err := tmpl.Run(&out, map[string]any{"x": tc.data}, nil); err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := out.String(); got != tc.expected {
				t.Fatalf("output: got %q, want %q", got, tc.expected)
			}
		})
	}
}

// TestWildcardForRange_RejectsBadUsage asserts the parser surfaces a
// clear error when `[*]` appears in an illegal position.
func TestWildcardForRange_RejectsBadUsage(t *testing.T) {
	type root struct {
		Mids []struct{ Label string }
	}

	tests := []struct {
		name    string
		source  string
		wantSub string // substring of the error message
	}{
		{
			name:    "trailing wildcard",
			source:  `{% for _, v := range x.Mids[*] %}{{ v }}{% end %}`,
			wantSub: "[*] must be followed by a .field accessor",
		},
		{
			name:    "wildcard in show",
			source:  `{{ x.Mids[*].Label }}`,
			wantSub: "[*] only allowed in for-range expressions",
		},
		{
			name:    "wildcard in var assignment",
			source:  `{% var v = x.Mids[*].Label %}{{ v }}`,
			wantSub: "[*] only allowed in for-range expressions",
		},
		{
			name:    "wildcard in if condition",
			source:  `{% if x.Mids[*].Label != "" %}yes{% end %}`,
			wantSub: "[*] only allowed in for-range expressions",
		},
		{
			name:    "wildcard inside call argument",
			source:  `{% var v = len(x.Mids[*].Label) %}{{ v }}`,
			wantSub: "[*] only allowed in for-range expressions",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fsys := Files{"index.html": []byte(tc.source)}
			opts := &BuildOptions{
				Globals: native.Declarations{
					"x": (*root)(nil),
				},
			}
			_, err := BuildTemplate(fsys, "index.html", opts)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q lacks substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestWildcardForRange_ErrorLocations asserts type-checker errors
// triggered by the desugared chain point at a source location that
// relates to the user's original `[*]` site (not synthetic AST
// internals).
func TestWildcardForRange_ErrorLocations(t *testing.T) {
	type leaf struct {
		Vals []int
	}
	type mid struct {
		Leaves []leaf
		Label  string
	}
	type root struct {
		Mids      []mid
		NotASlice int
	}

	tests := []struct {
		name    string
		source  string
		wantSub string
	}{
		{
			name:    "field doesn't exist after [*]",
			source:  `{% for _, v := range x.Mids[*].NoSuchField %}{{ v }}{% end %}`,
			wantSub: "NoSuchField",
		},
		{
			name:    "subject of [*] is not a slice",
			source:  `{% for _, v := range x.NotASlice[*].Foo %}{{ v }}{% end %}`,
			wantSub: "range",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fsys := Files{"index.html": []byte(tc.source)}
			opts := &BuildOptions{
				Globals: native.Declarations{
					"x": (*root)(nil),
				},
			}
			_, err := BuildTemplate(fsys, "index.html", opts)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q lacks substring %q", err.Error(), tc.wantSub)
			}
			// Position prefix is mandatory: `<path>:line:col: ...`.
			if !strings.Contains(err.Error(), ":") {
				t.Fatalf("error %q missing position prefix", err.Error())
			}
		})
	}
}

// TestWildcardForRange_InRenderedSubTree pins the regression that
// uncovered the `render`-sub-tree gap in the desugar pass: the
// pass walks the entry-point Tree's statement nodes but does NOT
// descend into the Tree carried by each `*ast.Render` /
// `*ast.RenderGlob` node (filled in by the parser's expand pass
// for snippets called via `{% render "..." %}` /
// `{{ render "..." }}` / `{{ render_glob "..." }}`).
//
// Without the recursion, a wildcard inside a rendered snippet
// parses correctly, but the WildcardIndex survives past desugar
// and triggers the type-checker's defensive panic
// (`missing case for type *ast.WildcardIndex`) when the
// dependency analyzer walks the AST.
//
// Real-world trigger: the HAPTIC chart's templateSnippets each
// compile as separate Trees that the entry-point templates pull
// in via `render` / `render_glob`. Migrating chart code to use
// `[*]` exposed the gap immediately.
func TestWildcardForRange_InRenderedSubTree(t *testing.T) {
	type leaf struct {
		Vals []int
	}
	type mid struct {
		Leaves []leaf
	}
	type root struct {
		Mids []mid
	}

	tests := []struct {
		name     string
		files    Files
		expected string
	}{
		{
			name: "show-render: wildcard inside rendered snippet",
			files: Files{
				"index.html":   []byte(`{{ render "snippet.html" }}`),
				"snippet.html": []byte(`{% for _, v := range x.Mids[*].Leaves[*].Vals %}{{ v }};{% end %}`),
			},
			expected: "10;20;30;",
		},
		{
			name: "nested-render: wildcard inside a snippet rendered from another snippet",
			files: Files{
				"index.html":   []byte(`{{ render "outer.html" }}`),
				"outer.html":   []byte(`{{ render "inner.html" }}`),
				"inner.html":   []byte(`{% for _, v := range x.Mids[*].Leaves[*].Vals %}{{ v }};{% end %}`),
			},
			expected: "10;20;30;",
		},
		{
			name: "import: wildcard inside an imported macro",
			files: Files{
				"index.html": []byte(`{% import "lib.html" for Show %}{{ Show() }}`),
				"lib.html":   []byte(`{% macro Show() %}{% for _, v := range x.Mids[*].Leaves[*].Vals %}{{ v }};{% end %}{% end %}`),
			},
			expected: "10;20;30;",
		},
	}

	data := &root{Mids: []mid{
		{Leaves: []leaf{{Vals: []int{10, 20}}, {Vals: []int{30}}}},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := &BuildOptions{
				Globals: native.Declarations{
					"x": (*root)(nil),
				},
			}
			tmpl, err := BuildTemplate(tc.files, "index.html", opts)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var out strings.Builder
			if err := tmpl.Run(&out, map[string]any{"x": data}, nil); err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := out.String(); got != tc.expected {
				t.Fatalf("output: got %q, want %q", got, tc.expected)
			}
		})
	}
}
