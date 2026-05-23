// Copyright 2024 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"gitlab.com/haproxy-haptic/scriggo"
	"gitlab.com/haproxy-haptic/scriggo/native"
)

// templateBenchCase defines a template benchmark test case.
type templateBenchCase struct {
	name    string
	sources scriggo.Files
	globals native.Declarations
}

// noopFunc is a no-op function for benchmarking overhead.
func noopFunc() string { return "" }

// benchItem is a small struct used by the typed-vs-untyped access
// benchmark cases. Two fields exercises a representative shape — one
// string, one int — without growing the per-item allocation cost so
// much that the iteration loop dominates field-access timing.
type benchItem struct {
	Name  string
	Value int
}

// reflectField mimics the dig/dig-like generic field accessor a
// schema-driven template would use against a value of unknown static
// type. Reflection-based; this is the slow path the typed-access
// cases will be measured against.
func reflectField(item any, name string) any {
	v := reflect.ValueOf(item)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return nil
	}
	f := v.FieldByName(name)
	if !f.IsValid() {
		return nil
	}
	return f.Interface()
}

// generateBenchItems produces n items used as the global slice for
// the field-access benchmark cases.
func generateBenchItems(n int) []*benchItem {
	items := make([]*benchItem, n)
	for i := range items {
		items[i] = &benchItem{Name: fmt.Sprintf("item-%d", i), Value: i}
	}
	return items
}

// fieldAccessBenchSize is the slice length used by the field-access
// cases. Big enough to make per-element work dominate per-render
// overhead; small enough to keep b.N iterations cheap.
const fieldAccessBenchSize = 100

// generateTypedFieldAccessTemplate produces a template that iterates
// a typed `[]*benchItem` global and emits the .Name and .Value fields
// directly. This is the best-case typed-bytecode path — Scriggo
// dispatches field access at the bytecode level, no reflection at
// render time.
func generateTypedFieldAccessTemplate() []byte {
	return []byte(`{%- for _, it := range items %}{{ it.Name }}={{ it.Value }};{%- end %}`)
}

// generateAssertFieldAccessTemplate produces a template that iterates
// an `[]any` global containing `*benchItem` values. Each element is
// type-asserted before field access. Scriggo's type-assertion is
// O(1) with one pointer-compare; this measures the per-element
// boxing cost relative to the typed path.
func generateAssertFieldAccessTemplate() []byte {
	return []byte(`{%- for _, it := range items %}{%- var bi = it.(*benchItem) %}{{ bi.Name }}={{ bi.Value }};{%- end %}`)
}

// generateReflectFieldAccessTemplate produces a template that
// iterates an `[]any` global and calls the reflection-based
// `reflectField` helper for each field — the moral equivalent of
// `dig(item, "name")` in chart libraries that operate on
// filter-output `[]any` slices. This is the cost generics aim to
// eliminate by making filter outputs statically typed.
func generateReflectFieldAccessTemplate() []byte {
	return []byte(`{%- for _, it := range items %}{{ reflectField(it, "Name") }}={{ reflectField(it, "Value") }};{%- end %}`)
}

// templateBenchCases defines the benchmark cases for template rendering.
var templateBenchCases = []templateBenchCase{
	{
		name: "RenderSequential10",
		sources: scriggo.Files{
			"index.txt":   generateSequentialRenders(10),
			"partial.txt": []byte(`X`),
		},
	},
	{
		name: "RenderSequential100",
		sources: scriggo.Files{
			"index.txt":   generateSequentialRenders(100),
			"partial.txt": []byte(`X`),
		},
	},
	{
		name: "RenderSequential1000",
		sources: scriggo.Files{
			"index.txt":   generateSequentialRenders(1000),
			"partial.txt": []byte(`X`),
		},
	},
	{
		name: "GoRenderParallel10",
		sources: scriggo.Files{
			"index.txt":   generateParallelRenders(10),
			"partial.txt": []byte(`X`),
		},
	},
	{
		name: "GoRenderParallel100",
		sources: scriggo.Files{
			"index.txt":   generateParallelRenders(100),
			"partial.txt": []byte(`X`),
		},
	},
	{
		name: "GoRenderParallel1000",
		sources: scriggo.Files{
			"index.txt":   generateParallelRenders(1000),
			"partial.txt": []byte(`X`),
		},
	},
	{
		name: "GoRenderParallel10000",
		sources: scriggo.Files{
			"index.txt":   generateParallelRenders(10000),
			"partial.txt": []byte(`X`),
		},
	},
	{
		name: "RenderWithWorkSequential100",
		sources: scriggo.Files{
			"index.txt":   generateSequentialRendersWithWork(100),
			"partial.txt": []byte(`{{ noop() }}`),
		},
		globals: native.Declarations{
			"noop": noopFunc,
		},
	},
	{
		name: "GoRenderWithWorkParallel100",
		sources: scriggo.Files{
			"index.txt":   generateParallelRendersWithWork(100),
			"partial.txt": []byte(`{{ noop() }}`),
		},
		globals: native.Declarations{
			"noop": noopFunc,
		},
	},
	{
		name: "GoRenderWithWorkParallel1000",
		sources: scriggo.Files{
			"index.txt":   generateParallelRendersWithWork(1000),
			"partial.txt": []byte(`{{ noop() }}`),
		},
		globals: native.Declarations{
			"noop": noopFunc,
		},
	},

	// Field-access benchmarks. Establish the baseline gap between
	// typed-bytecode field access, type-assertion + field access on
	// an []any slice, and reflection-based access on an []any slice.
	// These will be re-run after the generics phases land to verify
	// the predicted speedup: filter outputs become statically typed,
	// templates can switch from reflection (the slow path) to typed
	// access (the fast path) without changing chart semantics.
	{
		name: "TypedFieldAccess100",
		sources: scriggo.Files{
			"index.txt": generateTypedFieldAccessTemplate(),
		},
		globals: native.Declarations{
			"items": (func() *[]*benchItem { items := generateBenchItems(fieldAccessBenchSize); return &items })(),
		},
	},
	{
		name: "AssertFieldAccess100",
		sources: scriggo.Files{
			"index.txt": generateAssertFieldAccessTemplate(),
		},
		globals: native.Declarations{
			"items":     (func() *[]any { items := make([]any, fieldAccessBenchSize); for i, it := range generateBenchItems(fieldAccessBenchSize) { items[i] = it }; return &items })(),
			"benchItem": reflect.TypeOf(benchItem{}),
		},
	},
	{
		name: "ReflectFieldAccess100",
		sources: scriggo.Files{
			"index.txt": generateReflectFieldAccessTemplate(),
		},
		globals: native.Declarations{
			"items":        (func() *[]any { items := make([]any, fieldAccessBenchSize); for i, it := range generateBenchItems(fieldAccessBenchSize) { items[i] = it }; return &items })(),
			"reflectField": reflectField,
		},
	},
}

// generateSequentialRenders generates a template with n sequential render calls.
func generateSequentialRenders(n int) []byte {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteString(`{{ render "partial.txt" }}`)
	}
	return []byte(sb.String())
}

// generateParallelRenders generates a template with n parallel go render calls.
func generateParallelRenders(n int) []byte {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteString(`{{ go render "partial.txt" }}`)
	}
	return []byte(sb.String())
}

// generateSequentialRendersWithWork generates a template with n sequential renders
// where each partial does some work (calls a function).
func generateSequentialRendersWithWork(n int) []byte {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteString(`{{ render "partial.txt" }}`)
	}
	return []byte(sb.String())
}

// generateParallelRendersWithWork generates a template with n parallel renders
// where each partial does some work (calls a function).
func generateParallelRendersWithWork(n int) []byte {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteString(`{{ go render "partial.txt" }}`)
	}
	return []byte(sb.String())
}

func BenchmarkTemplateRender(b *testing.B) {
	for _, tc := range templateBenchCases {
		// Build the template once outside the benchmark loop
		opts := &scriggo.BuildOptions{}
		if tc.globals != nil {
			opts.Globals = tc.globals
		}
		template, err := scriggo.BuildTemplate(tc.sources, "index.txt", opts)
		if err != nil {
			b.Fatalf("%s: build error: %v", tc.name, err)
		}

		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				var out strings.Builder
				err := template.Run(&out, nil, nil)
				if err != nil {
					b.Fatalf("run error: %v", err)
				}
			}
		})
	}
}
