// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"bytes"
	"strings"
	"testing"

	"gitlab.com/haproxy-haptic/scriggo/internal/fstest"
)

// TestShowAutoDerefPointer pins the auto-deref-in-show feature: when
// `{{ expr }}` evaluates to a pointer type, the renderer dereferences
// it before dispatching to the context-specific shower so chart code
// can write `{{ gateway.Metadata.Generation }}` against the typegen
// tristate `*int64` pointer (issue #52) without `tostring(deref(...))`
// boilerplate. A nil pointer renders as empty (no output).
func TestShowAutoDerefPointer(t *testing.T) {
	cases := []struct {
		name string
		tmpl string
		want string
	}{
		{
			name: "*int with value 42",
			tmpl: `{%- var x = 42 -%}{%- var p = &x -%}{{ p }}`,
			want: "42",
		},
		{
			name: "*int with zero value",
			tmpl: `{%- var x = 0 -%}{%- var p = &x -%}{{ p }}`,
			want: "0",
		},
		{
			name: "*string with non-empty value",
			tmpl: `{%- var x = "hello" -%}{%- var p = &x -%}{{ p }}`,
			want: "hello",
		},
		{
			name: "*string empty",
			tmpl: `{%- var x = "" -%}{%- var p = &x -%}{{ p }}`,
			want: "",
		},
		{
			name: "*bool true",
			tmpl: `{%- var x = true -%}{%- var p = &x -%}{{ p }}`,
			want: "true",
		},
		{
			name: "*bool false",
			tmpl: `{%- var x = false -%}{%- var p = &x -%}{{ p }}`,
			want: "false",
		},
		{
			name: "*float64",
			tmpl: `{%- var x = 3.14 -%}{%- var p = &x -%}{{ p }}`,
			want: "3.14",
		},
		{
			name: "nil pointer renders as empty",
			tmpl: `{%- var p *int = nil -%}{{ p }}`,
			want: "",
		},
		{
			name: "pointer reassigned to nil renders as empty",
			tmpl: `{%- var x = 42 -%}{%- var p = &x -%}{%- p = nil -%}{{ p }}`,
			want: "",
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
