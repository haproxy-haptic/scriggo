// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"bytes"
	"strings"
	"testing"

	"github.com/haproxy-haptic/scriggo/internal/fstest"
)

// TestForRangeOverMapDoesNotClobberOuterVar pins a regression first
// surfaced by haptic's chart-rendering tests. In a `{%% %%}` block, a
// `for k, v := range m` (where `m` is a map) was clobbering an
// unrelated outer-scope variable declared earlier in the same block.
//
// The shape that triggers it: a scalar var declared before the loop,
// then a `for k, v := range mapValue` whose key register the compiler
// allocates to the same slot as the outer var. The loop body's writes
// to `k` corrupt the outer var.
//
// The corruption only happens for map ranges (slice ranges with a
// blank-key `for _, v := range` are unaffected) and only when at least
// one outer variable is declared *before* the loop.
func TestForRangeOverMapDoesNotClobberOuterVar(t *testing.T) {
	cases := []struct {
		name string
		tmpl string
		want string
	}{
		{
			name: "map[string]any with k,v range",
			tmpl: `{%%
				var outer = "ORIGINAL"
				var sink = map[string]any{}
				var src = map[string]any{"alpha": "1"}
				for k, v := range src {
					sink[k] = v
				}
			%%}{{ outer }}`,
			want: "ORIGINAL",
		},
		{
			name: "map[string]string with k,v range",
			tmpl: `{%%
				var outer = "ORIGINAL"
				var sink = map[string]string{}
				var src = map[string]string{"only": "value"}
				for k, v := range src {
					sink[k] = v
				}
			%%}{{ outer }}`,
			want: "ORIGINAL",
		},
		{
			name: "map range with key only (k := range)",
			tmpl: `{%%
				var outer = "ORIGINAL"
				var src = map[string]any{"only": "value"}
				for k := range src {
					_ = k
				}
			%%}{{ outer }}`,
			want: "ORIGINAL",
		},
		{
			name: "two outer vars before map-range loop",
			tmpl: `{%%
				var outerA = "AAA"
				var outerB = "BBB"
				var sink = map[string]any{}
				var src = map[string]any{"only": "value"}
				for k, v := range src {
					sink[k] = v
				}
			%%}A={{ outerA }} B={{ outerB }}`,
			want: "A=AAA B=BBB",
		},
		{
			name: "outer var declared *after* the loop is unaffected (sanity)",
			tmpl: `{%%
				var sink = map[string]any{}
				var src = map[string]any{"only": "value"}
				for k, v := range src {
					sink[k] = v
				}
				var outer = "ORIGINAL"
			%%}{{ outer }}`,
			want: "ORIGINAL",
		},
		{
			name: "slice range with blank key — does NOT leak (sanity)",
			tmpl: `{%%
				var outer = "ORIGINAL"
				var sink = []string{}
				var src = []string{"value"}
				for _, v := range src {
					sink = append(sink, v)
				}
			%%}{{ outer }}`,
			want: "ORIGINAL",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsys := fstest.Files{"index.html": tc.tmpl}
			prog, err := BuildTemplate(fsys, "index.html", nil)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var buf bytes.Buffer
			if err := prog.Run(&buf, nil, nil); err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := strings.TrimSpace(buf.String()); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
