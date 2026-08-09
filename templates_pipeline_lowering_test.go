// Copyright 2025 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"strings"
	"testing"

	"gitlab.com/haproxy-haptic/scriggo/native"
)

// pipelineNatives are stand-ins for the collection helpers a host registers.
// A lowered chain must never reach them; an unlowerable one must, and must
// still produce the same answer. Each records that it ran.
type pipelineNatives struct{ calls int }

func (p *pipelineNatives) decls() native.Declarations {
	return native.Declarations{
		"filter": func(s []int, f func(int) bool) []int {
			p.calls++
			out := []int{}
			for _, v := range s {
				if f(v) {
					out = append(out, v)
				}
			}
			return out
		},
		"reject": func(s []int, f func(int) bool) []int {
			p.calls++
			out := []int{}
			for _, v := range s {
				if !f(v) {
					out = append(out, v)
				}
			}
			return out
		},
		"map": func(s []int, f func(int) string) []string {
			p.calls++
			out := []string{}
			for _, v := range s {
				out = append(out, f(v))
			}
			return out
		},
		"flat_map": func(s []int, f func(int) []int) []int {
			p.calls++
			out := []int{}
			for _, v := range s {
				out = append(out, f(v)...)
			}
			return out
		},
		"unique": func(s []int) []int {
			p.calls++
			seen := map[int]bool{}
			out := []int{}
			for _, v := range s {
				if !seen[v] {
					seen[v] = true
					out = append(out, v)
				}
			}
			return out
		},
		// sorted mirrors a host helper that returns (value, error) — the shape
		// that only type-checks in a pipe, never as a direct call in a
		// single-value context.
		"sorted": func(s []int) ([]int, error) { p.calls++; return s, nil },
		"join":   strings.Join,
	}
}

func TestPipelineLowering(t *testing.T) {
	tests := []struct {
		name       string
		source     string
		want       string
		wantNative int // native calls that must still happen
	}{
		{
			name:   "filter alone",
			source: `{% var src = []int{1,2,3,4,5,6} %}{% var out = src | filter(func(v int) bool { return v % 2 == 0 }) %}{{ len(out) }}`,
			want:   "3",
		},
		{
			name:   "reject alone",
			source: `{% var src = []int{1,2,3,4,5,6} %}{% var out = src | reject(func(v int) bool { return v % 2 == 0 }) %}{{ len(out) }}`,
			want:   "3",
		},
		{
			name: "filter then map fuse into one pass",
			source: `{% var src = []int{1,2,3,4,5,6} %}{% var out = src | filter(func(v int) bool { return v > 3 }) | ` +
				`map(func(v int) string { return "x" }) %}{{ join(out, ",") }}`,
			want: "x,x,x",
		},
		{
			name: "flat_map expands, then filter",
			source: `{% var src = []int{1,2,3,4,5,6} %}{% var out = src | flat_map(func(v int) []int { return []int{v, v} }) | ` +
				`filter(func(v int) bool { return v == 1 }) %}{{ len(out) }}`,
			want: "2",
		},
		{
			name: "map then filter on the mapped value",
			source: `{% var src = []int{1,2,3,4,5,6} %}{% var out = src | map(func(v int) string { return "a" }) %}{{ join(out, "") }}`,
			want:   "aaaaaa",
		},
		{
			name:   "direct call form, not the pipe",
			source: `{% var src = []int{1,2,3,4,5,6} %}{% var out = filter(src, func(v int) bool { return v < 3 }) %}{{ len(out) }}`,
			want:   "2",
		},
		{
			name: "an unlowerable stage keeps the rest lowered",
			source: `{% var src = []int{1,2,3,4,5,6} %}{% var dup = src | flat_map(func(v int) []int { return []int{v, v} }) %}` +
				`{% var out = unique(dup) %}{{ len(out) }}`,
			want:       "6",
			wantNative: 1, // unique only
		},
		{
			name: "a non-literal function argument is not lowered",
			source: `{% var src = []int{1,2,3,4,5,6} %}{% var pred = func(v int) bool { return v > 4 } %}` +
				`{% var out = src | filter(pred) %}{{ len(out) }}`,
			want:       "2",
			wantNative: 1,
		},
		{
			// Re-applying an unlowered tail must keep the pipe spelling: the
			// checker treats `x | f()` specially, and that is what lets a
			// two-result helper sit in a single-value context. Rebuilding it
			// as a direct call fails with "multiple-value in single-value
			// context" — which the chart suite hit and the fork suite did not.
			name: "a two-result tail stage stays a pipe",
			source: `{% var src = []int{1,2,3,4,5,6} %}{% var out = src | filter(func(v int) bool { return v > 2 }) | ` +
				`sorted() %}{{ len(out) }}`,
			want:       "4",
			wantNative: 1,
		},
		{
			name:   "empty source yields empty output",
			source: `{% var out = []int{} | filter(func(v int) bool { return true }) %}{{ len(out) }}`,
			want:   "0",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			natives := &pipelineNatives{}
			fsys := Files{"index.html": []byte(tc.source)}
			tmpl, err := BuildTemplate(fsys, "index.html", &BuildOptions{Globals: natives.decls()})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var out strings.Builder
			if err := tmpl.Run(&out, nil, nil); err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := out.String(); got != tc.want {
				t.Fatalf("output: got %q, want %q", got, tc.want)
			}
			if natives.calls != tc.wantNative {
				t.Fatalf("native helper calls: got %d, want %d — a lowered stage must not "+
					"reach the native helper, and an unlowerable one must",
					natives.calls, tc.wantNative)
			}
		})
	}
}

// TestPipelineLoweringDoesNotAliasSource pins the `[:0:0]` in the accumulator:
// a plain `src[:0]` would share src's backing array, so appending to the
// result would overwrite the source in place.
func TestPipelineLoweringDoesNotAliasSource(t *testing.T) {
	natives := &pipelineNatives{}
	fsys := Files{"index.html": []byte(
		`{% var src = []int{1,2,3,4,5,6} %}{% var out = src | filter(func(v int) bool { return v > 2 }) %}` +
			`{{ len(out) }}:{{ src[0] }}{{ src[1] }}{{ src[2] }}`)}
	tmpl, err := BuildTemplate(fsys, "index.html", &BuildOptions{Globals: natives.decls()})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var out strings.Builder
	if err := tmpl.Run(&out, nil, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := out.String(); got != "4:123" {
		t.Fatalf("source was mutated by the lowered pipeline: got %q, want %q", got, "4:123")
	}
}
