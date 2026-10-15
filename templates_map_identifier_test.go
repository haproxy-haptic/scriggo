// Copyright 2025 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"strings"
	"testing"

	"github.com/haproxy-haptic/scriggo/native"
)

// TestMapAsIdentifier exercises the fork extension that lets `map` name a
// declaration when it is not starting a map type.
//
// A map type always requires `[`, so `map` followed by anything else was
// previously an unconditional syntax error ("unexpected (, expecting ["). The
// extension therefore only accepts programs that did not compile before and
// cannot change the meaning of any that did — which is what makes it safe.
// Templates need it so a collection helper can carry the name every other
// language gives it.
func TestMapAsIdentifier(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		expected string
	}{
		{
			name:     "map names a function",
			source:   `{{ map([]int{1, 2, 3}) }}`,
			expected: "3",
		},
		{
			name:     "map as a pipe stage",
			source:   `{{ []int{1, 2} | map() }}`,
			expected: "2",
		},
		{
			name:     "map type still parses",
			source:   `{% var m = map[string]int{"a": 1} %}{{ m["a"] }}`,
			expected: "1",
		},
		{
			name:     "map type and map function in one expression",
			source:   `{% var m = map[string]int{"a": 1, "b": 2} %}{{ map([]int{len(m)}) }}`,
			expected: "1",
		},
		{
			name:     "map type as a composite-literal value type",
			source:   `{% var m = map[string][]int{"a": []int{1, 2}} %}{{ map(m["a"]) }}`,
			expected: "2",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fsys := Files{"index.html": []byte(tc.source)}
			opts := &BuildOptions{
				Globals: native.Declarations{
					"map": func(s []int) int { return len(s) },
				},
			}
			tmpl, err := BuildTemplate(fsys, "index.html", opts)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var out strings.Builder
			if err := tmpl.Run(&out, nil, nil); err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := out.String(); got != tc.expected {
				t.Fatalf("output: got %q, want %q", got, tc.expected)
			}
		})
	}
}

// TestMapAsIdentifier_UndeclaredIsReported asserts the diagnostic when `map`
// is used as a name nothing declares. Without the extension this said
// "unexpected (, expecting [", which pointed at the map-type grammar rather
// than at the missing declaration.
func TestMapAsIdentifier_UndeclaredIsReported(t *testing.T) {
	fsys := Files{"index.html": []byte(`{{ map(1) }}`)}
	_, err := BuildTemplate(fsys, "index.html", nil)
	if err == nil {
		t.Fatal("expected an error for an undeclared map identifier")
	}
	if !strings.Contains(err.Error(), "undefined: map") {
		t.Fatalf("diagnostic must name the undeclared identifier, got: %v", err)
	}
}

// TestMapTypeErrorsUnchanged pins the map-type diagnostics that must NOT be
// swallowed by the identifier fallback: once `[` is seen, the parser is
// committed to a map type and a malformed one still fails there.
func TestMapTypeErrorsUnchanged(t *testing.T) {
	tests := []struct {
		name   string
		source string
	}{
		{name: "missing value type", source: `{% var m = map[string] %}{{ m }}`},
		{name: "missing closing bracket", source: `{% var m = map[string{} %}{{ m }}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fsys := Files{"index.html": []byte(tc.source)}
			if _, err := BuildTemplate(fsys, "index.html", nil); err == nil {
				t.Fatal("expected a syntax error")
			}
		})
	}
}
