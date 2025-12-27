// Copyright 2024 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
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
