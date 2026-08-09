// Copyright 2025 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"strconv"
	"strings"
	"testing"

	"gitlab.com/haproxy-haptic/scriggo/native"
)

// pipelineNatives are stand-ins for the collection helpers a host registers.
// A lowered chain must never reach them; an unlowerable one must, and must
// still produce the same answer. Each records that it ran.
type pipelineNatives struct{ calls, elems int }

func (p *pipelineNatives) decls() native.Declarations {
	return native.Declarations{
		"filter": func(s []int, f func(int) bool) []int {
			p.calls++
			p.elems += len(s)
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
			p.elems += len(s)
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
			p.elems += len(s)
			out := []string{}
			for _, v := range s {
				out = append(out, f(v))
			}
			return out
		},
		"flat_map": func(s []int, f func(int) []int) []int {
			p.calls++
			p.elems += len(s)
			out := []int{}
			for _, v := range s {
				out = append(out, f(v)...)
			}
			return out
		},
		"unique": func(s []int) []int {
			p.calls++
			p.elems += len(s)
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
		"sorted": func(s []int) ([]int, error) { p.calls++; p.elems += len(s); return s, nil },
		"join":   strings.Join,
		"join_ints": func(s []int) string {
			parts := make([]string, 0, len(s))
			for _, v := range s {
				parts = append(parts, strconv.Itoa(v))
			}
			return strings.Join(parts, ",")
		},
	}
}

func TestPipelineLowering(t *testing.T) {
	tests := []struct {
		name       string
		source     string
		want       string
		wantNative int // elements that must still flow through a native helper
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
			wantNative: 12, // unique() sees all 12 expanded elements
		},
		{
			name: "a non-literal function argument is not lowered",
			source: `{% var src = []int{1,2,3,4,5,6} %}{% var pred = func(v int) bool { return v > 4 } %}` +
				`{% var out = src | filter(pred) %}{{ len(out) }}`,
			want:       "2",
			wantNative: 6, // filter(pred): a variable, not a literal, so not lowered
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
			wantNative: 4, // sorted() is a two-result tail, never lowered
		},
		{
			// The position a chain sits in must not decide whether it is
			// lowered. A range head is what chart code reaches for most, and
			// lowering only var initialisers left it silently on the native
			// path — a "works, but only if you spell it the other way" trap.
			name: "a chain in a for-range head is lowered",
			source: `{% var src = []int{1,2,3,4,5,6} %}{% var n = 0 %}` +
				`{% for _, v := range src | filter(func(v int) bool { return v > 4 }) %}{% n = n + v %}{% end %}{{ n }}`,
			want: "11",
		},
		{
			name: "a chain assigned to an existing variable is lowered",
			source: `{% var src = []int{1,2,3,4,5,6} %}{% var out []int %}` +
				`{% out = src | filter(func(v int) bool { return v > 4 }) %}{{ len(out) }}`,
			want: "2",
		},
		{
			name: "a chain shown directly is lowered",
			source: `{% var src = []int{1,2,3,4,5,6} %}` +
				`{{ src | filter(func(v int) bool { return v > 4 }) | join_ints() }}`,
			want: "5,6",
		},
		{
			// Deliberately NOT lowered. Hoisting a chain out of an argument
			// list would evaluate it before the other arguments, which is a
			// semantic change this pass has no way to prove safe. The stage
			// falls back to the native helper — correct, just not fused.
			name: "a chain nested in a call argument stays native",
			source: `{% var src = []int{1,2,3,4,5,6} %}` +
				`{{ len(filter(src, func(v int) bool { return v > 4 })) }}`,
			want:       "2",
			wantNative: 6, // nested in a call argument, so left native
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
			if natives.elems != tc.wantNative {
				t.Fatalf("elements through a native helper: got %d, want %d — a lowered "+
					"stage feeds none, an unlowered one feeds every element. (Seeding a "+
					"map/flat_map accumulator does call the native, but on an empty "+
					"slice, so it moves no elements.)",
					natives.elems, tc.wantNative)
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
