// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"strings"
	"testing"

	"github.com/haproxy-haptic/scriggo/native"
)

type capturedNativeState struct{ Value string }

func TestNestedClosureKeepsNativeGlobalIdentity(t *testing.T) {
	for _, body := range []string{
		`return hold(&State)`,
		`var inner = func() string { return hold(&State) }; return inner()`,
		`var middle = func() string { var inner = func() string { return hold(&State) }; return inner() }; return middle()`,
	} {
		t.Run(body, func(t *testing.T) {
			tmpl, err := BuildTemplate(Files{
				"index.txt":   []byte(`{% import "library.txt" for Transform, Outer %}{{ Transform("before") }}{{ Outer() }}{{ Transform("after") }}`),
				"library.txt": []byte(`{% var Transform = func(value string) string { return value + "!" } %}{% var Outer = func() string { ` + body + ` } %}`),
			}, "index.txt", &BuildOptions{Globals: native.Declarations{
				"State": (*capturedNativeState)(nil),
				"hold":  func(state *capturedNativeState) string { return state.Value },
			}})
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range []string{"first", "second"} {
				var output strings.Builder
				if err := tmpl.Run(&output, map[string]any{"State": capturedNativeState{Value: value}}, nil); err != nil {
					t.Fatal(err)
				}
				if got, want := output.String(), "before!"+value+"after!"; got != want {
					t.Fatalf("output = %q, want %q", got, want)
				}
			}
		})
	}
}
