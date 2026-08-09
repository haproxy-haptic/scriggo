// Copyright 2025 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"strings"
	"testing"
)

// TestAppendSpreadTypeCheck pins that `append(dst, src...)` requires src to be
// assignable to dst's slice type, as Go does. Checking only element
// assignability let []*T through into []any, where reflect.AppendSlice panics
// at run time — a diagnostic must land at compile time instead.
func TestAppendSpreadTypeCheck(t *testing.T) {
	ok := []struct {
		name   string
		source string
		want   string
	}{
		{
			name:   "same slice type",
			source: `{% var a = []int{1} %}{% var b = []int{2,3} %}{% a = append(a, b...) %}{{ len(a) }}`,
			want:   "3",
		},
		{
			name:   "any into any",
			source: `{% var a = []any{1} %}{% var b = []any{2,3} %}{% a = append(a, b...) %}{{ len(a) }}`,
			want:   "3",
		},
		{
			name:   "string spreads into []byte",
			source: `{% var a = []byte{65} %}{% a = append(a, "BC"...) %}{{ len(a) }}`,
			want:   "3",
		},
	}

	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			tmpl, err := BuildTemplate(Files{"index.html": []byte(tc.source)}, "index.html", nil)
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
		})
	}

	// The element type is assignable but the slice type is not; Go rejects
	// this and so must we, at compile time rather than in reflect.AppendSlice.
	bad := `{% var a = []any{} %}{% var b = []int{1,2} %}{% a = append(a, b...) %}{{ len(a) }}`
	_, err := BuildTemplate(Files{"index.html": []byte(bad)}, "index.html", nil)
	if err == nil {
		t.Fatal("expected []int... into []any to be rejected")
	}
	if !strings.Contains(err.Error(), "in append") {
		t.Fatalf("diagnostic must name append, got: %v", err)
	}
}
