// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"io"
	"strings"
	"testing"

	"gitlab.com/haproxy-haptic/scriggo/native"
)

func TestTemplateInternalFunctionGlobalCallsAndReassigns(t *testing.T) {
	tmpl, err := BuildTemplate(Files{
		"index.txt":   []byte(`{% import "library.txt" for Transform %}{{ Transform("a") }}{% Transform = func(value string) string { return value + "?" } %}{{ Transform("b") }}`),
		"library.txt": []byte(`{% var suffix = "!" %}{% var Transform = func(value string) string { return value + suffix } %}`),
	}, "index.txt", nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var output strings.Builder
	if err := tmpl.Run(&output, nil, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := output.String(), "a!b?"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestTemplateInternalFunctionGlobalMaterializesOnlyEscapingValue(t *testing.T) {
	call := func(function func() string) string { return function() }
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{% var Function = func() string { return "value" } %}{{ call(Function) }}{{ Function() }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"call": native.Synchronous(call),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var output strings.Builder
	if err := tmpl.Run(&output, nil, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := output.String(), "valuevalue"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestTemplateInternalFunctionGlobalAddressEscapePreservesMutation(t *testing.T) {
	var retained *func() string
	replace := func(function *func() string) {
		retained = function
		*function = func() string { return "native" }
	}
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{% var Function = func() string { return "scriggo" } %}{% replace(&Function) %}{{ Function() }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"replace": native.Synchronous(replace),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var output strings.Builder
	if err := tmpl.Run(&output, nil, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := output.String(), "native"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if retained == nil || (*retained)() != "native" {
		t.Fatal("escaped function address did not retain the mutated value")
	}
}

func TestTemplateNativeFunctionGlobalRemainsExternallyMutable(t *testing.T) {
	function := func() string { return "first" }
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{% Function = func() string { return "second" } %}{{ Function() }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"Function": &function,
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var output strings.Builder
	if err := tmpl.Run(&output, nil, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := output.String(), "second"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if got := function(); got != "second" {
		t.Fatalf("native global = %q, want second", got)
	}
}

func TestTemplateInternalFunctionGlobalsAreBatchIsolated(t *testing.T) {
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{% var Function = func() string { return value } %}{{ Function() }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"value": (*string)(nil),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var first, second strings.Builder
	if err := tmpl.RunBatch([]BatchRun{
		{Out: &first, Vars: map[string]any{"value": "first"}},
		{Out: &second, Vars: map[string]any{"value": "second"}},
	}); err != nil {
		t.Fatalf("run batch: %v", err)
	}
	if got, want := first.String()+":"+second.String(), "first:second"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func BenchmarkTemplateInternalFunctionGlobal(b *testing.B) {
	for _, internal := range []bool{false, true} {
		name := "reflective"
		if internal {
			name = "internal"
		}
		b.Run(name, func(b *testing.B) {
			tmpl, err := BuildTemplate(Files{
				"index.txt":   []byte(`{% import "library.txt" for Transform %}{% for index := 0; index < 1000; index++ %}{% _ = Transform(index) %}{% end %}`),
				"library.txt": []byte(`{% var Transform = func(value int) int { return value + 1 } %}`),
			}, "index.txt", nil)
			if err != nil {
				b.Fatalf("build: %v", err)
			}
			if !internal {
				tmpl.fn.InternalFunctionGlobals = nil
			}
			b.ReportAllocs()
			for b.Loop() {
				if err := tmpl.Run(io.Discard, nil, nil); err != nil {
					b.Fatalf("run: %v", err)
				}
			}
		})
	}
}
