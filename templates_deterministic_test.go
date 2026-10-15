// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/haproxy-haptic/scriggo/native"
)

func runDeterministicTemplate(t *testing.T, files Files, globals native.Declarations) (string, error) {
	t.Helper()
	tmpl, err := BuildTemplate(files, "index.txt", &BuildOptions{Globals: globals})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var out strings.Builder
	err = tmpl.Run(&out, nil, &RunOptions{Deterministic: true})
	return out.String(), err
}

func TestDeterministicTemplateMapRange(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "string",
			src:  `{% var values = map[string]int{"z": 3, "a": 1, "m": 2} %}{% for key, value := range values %}{{ key }}={{ value }};{% end %}`,
			want: "a=1;m=2;z=3;",
		},
		{
			name: "integer",
			src:  `{% var values = map[int]string{7: "p", -2: "n", 0: "z"} %}{% for key := range values %}{{ key }};{% end %}`,
			want: "-2;0;7;",
		},
		{
			name: "boolean",
			src:  `{% var values = map[bool]string{true: "t", false: "f"} %}{% for key := range values %}{{ key }};{% end %}`,
			want: "false;true;",
		},
		{
			name: "array",
			src:  `{% var values = map[[2]int]string{{2, 0}: "b", {1, 9}: "a9", {1, 2}: "a2"} %}{% for key := range values %}{{ key[0] }}.{{ key[1] }};{% end %}`,
			want: "1.2;1.9;2.0;",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpl, err := BuildTemplate(Files{"index.txt": []byte(test.src)}, "index.txt", nil)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			for range 100 {
				var out strings.Builder
				if err := tmpl.Run(&out, nil, &RunOptions{Deterministic: true}); err != nil {
					t.Fatalf("run: %v", err)
				}
				if got := out.String(); got != test.want {
					t.Fatalf("output = %q, want %q", got, test.want)
				}
			}
		})
	}
}

func TestDeterministicTemplateConcurrentRuns(t *testing.T) {
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{% var values = map[string]int{"z": 3, "a": 1, "m": 2} %}{% for key := range values %}{{ key }}{% end %}`),
	}, "index.txt", nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	const runs = 64
	errs := make(chan error, runs)
	var wait sync.WaitGroup
	for range runs {
		wait.Add(1)
		go func() {
			defer wait.Done()
			var out strings.Builder
			if err := tmpl.Run(&out, nil, &RunOptions{Deterministic: true}); err != nil {
				errs <- err
				return
			}
			if got, want := out.String(), "amz"; got != want {
				errs <- errors.New("output = " + got + ", want " + want)
			}
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestDeterministicTemplateInterfaceMapRange(t *testing.T) {
	values := any(map[any]string{nil: "nil", "z": "z", "a": "a"})
	label := func(value any) string {
		if value == nil {
			return "nil"
		}
		return value.(string)
	}
	got, err := runDeterministicTemplate(t, Files{
		"index.txt": []byte(`{% for key, value := range values %}{{ label(key) }}={{ value }};{% end %}`),
	}, native.Declarations{"label": label, "values": &values})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if want := "nil=nil;a=a;z=z;"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestDeterministicMapRangeAcrossTemplatePaths(t *testing.T) {
	tests := []struct {
		name  string
		files Files
		want  string
	}{
		{
			name: "nested macro",
			files: Files{
				"index.txt": []byte(`{% macro Keys(values map[string]int) %}{% for key := range values %}{{ key }}{% end %}{% end %}{{ Keys(map[string]int{"b": 2, "a": 1}) }}`),
			},
			want: "ab",
		},
		{
			name: "imported macro",
			files: Files{
				"index.txt": []byte(`{% import "keys.txt" for Keys %}{{ Keys(map[string]int{"b": 2, "a": 1}) }}`),
				"keys.txt":  []byte(`{% macro Keys(values map[string]int) %}{% for key := range values %}{{ key }}{% end %}{% end %}`),
			},
			want: "ab",
		},
		{
			name: "rendered template",
			files: Files{
				"index.txt": []byte(`{{ render "keys.txt" }}`),
				"keys.txt":  []byte(`{% var values = map[string]int{"b": 2, "a": 1} %}{% for key := range values %}{{ key }}{% end %}`),
			},
			want: "ab",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpl, err := BuildTemplate(test.files, "index.txt", nil)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			for range 50 {
				var out strings.Builder
				if err := tmpl.Run(&out, nil, &RunOptions{Deterministic: true}); err != nil {
					t.Fatalf("run: %v", err)
				}
				if got := out.String(); got != test.want {
					t.Fatalf("output = %q, want %q", got, test.want)
				}
			}
		})
	}
}

func parallelTemplateCases() []struct {
	name  string
	files Files
} {
	return []struct {
		name  string
		files Files
	}{
		{
			name: "direct macro",
			files: Files{
				"index.txt": []byte(`{% macro P() string %}parallel{% end %}prefix{% touch() %}{{ go P() }}`),
			},
		},
		{
			name: "indirect macro",
			files: Files{
				"index.txt": []byte(`{% macro P() string %}parallel{% end %}{% var f interface{} = P %}prefix{% touch() %}{{ go f.(func() string)() }}`),
			},
		},
		{
			name: "render",
			files: Files{
				"index.txt": []byte(`prefix{% touch() %}{{ go render "child.txt" }}`),
				"child.txt": []byte(`parallel`),
			},
		},
		{
			name: "render glob",
			files: Files{
				"index.txt":          []byte(`prefix{% touch() %}{{ go render_glob "child-*.txt" }}`),
				"child-parallel.txt": []byte(`parallel`),
			},
		},
		{
			name: "imported macro body",
			files: Files{
				"index.txt": []byte(`{% import "library.txt" for Outer %}prefix{% touch() %}{{ Outer() }}`),
				"library.txt": []byte(`{% macro Inner() string %}parallel{% end %}
{% macro Outer() string %}{{ go Inner() }}{% end %}`),
			},
		},
		{
			name: "transitively imported macro body",
			files: Files{
				"index.txt":  []byte(`{% import "middle.txt" for Outer %}prefix{% touch() %}{{ Outer() }}`),
				"middle.txt": []byte(`{% import "leaf.txt" for Inner %}{% macro Outer() string %}{{ Inner() }}{% end %}`),
				"leaf.txt": []byte(`{% macro Parallel() string %}parallel{% end %}
{% macro Inner() string %}{{ go Parallel() }}{% end %}`),
			},
		},
		{
			name: "rendered template body",
			files: Files{
				"index.txt": []byte(`prefix{% touch() %}{{ render "child.txt" }}`),
				"child.txt": []byte(`{% macro P() string %}parallel{% end %}{{ go P() }}`),
			},
		},
	}
}

func TestDeterministicRejectsParallelRenderingBeforeExecution(t *testing.T) {
	for _, test := range parallelTemplateCases() {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			tmpl, err := BuildTemplate(test.files, "index.txt", &BuildOptions{
				Globals: native.Declarations{"touch": func() { calls++ }},
			})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var out strings.Builder
			err = tmpl.Run(&out, nil, &RunOptions{Deterministic: true})
			if !errors.Is(err, ErrDeterministicGo) {
				t.Fatalf("run error = %v, want %v", err, ErrDeterministicGo)
			}
			if calls != 0 || out.Len() != 0 {
				t.Fatalf("effects: calls=%d output=%q, want none", calls, out.String())
			}
		})
	}
}

func TestTemplateDeterministicSafeValidatesFunctionGraph(t *testing.T) {
	value := 1
	values := map[*int]string{&value: "value"}
	tests := []struct {
		name    string
		files   Files
		globals native.Declarations
		want    error
	}{
		{
			name:  "nested print",
			files: Files{"index.txt": []byte(`{% var fn = func() { print("hidden") } %}safe`)},
			want:  ErrDeterministicPrint,
		},
		{
			name: "rendered unstable map range",
			files: Files{
				"index.txt":   []byte(`{{ render "partial.txt" }}`),
				"partial.txt": []byte(`{% for key := range values %}{{ key }}{% end %}`),
			},
			globals: native.Declarations{"values": &values},
			want:    ErrDeterministicMapKey,
		},
		{
			name:  "parallel macro",
			files: Files{"index.txt": []byte(`{% macro M %}value{% end %}{{ go M() }}`)},
			want:  ErrDeterministicGo,
		},
		{
			name: "nested synchronous work",
			files: Files{
				"index.txt":   []byte(`{% macro M %}{{ render "partial.txt" }}{% end %}{{ M() }}`),
				"partial.txt": []byte(`{% var fn = func() string { return "safe" } %}{{ fn() }}`),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpl, err := BuildTemplate(test.files, "index.txt", &BuildOptions{Globals: test.globals})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			err = tmpl.DeterministicSafe()
			if test.want == nil {
				if err != nil {
					t.Fatalf("DeterministicSafe: %v", err)
				}
				return
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("DeterministicSafe error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestDefaultExecutionStillAllowsParallelRendering(t *testing.T) {
	for _, test := range parallelTemplateCases() {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			tmpl, err := BuildTemplate(test.files, "index.txt", &BuildOptions{
				Globals: native.Declarations{"touch": func() { calls++ }},
			})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var out strings.Builder
			if err := tmpl.Run(&out, nil, nil); err != nil {
				t.Fatalf("run: %v", err)
			}
			if got, want := out.String(), "prefixparallel"; got != want {
				t.Fatalf("output = %q, want %q", got, want)
			}
			if calls != 1 {
				t.Fatalf("touch calls = %d, want 1", calls)
			}
		})
	}
}

func TestDeterministicRejectsDetachedGoBeforeExecution(t *testing.T) {
	calls := 0
	started := make(chan struct{})
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`prefix{% touch() %}{% go signal() %}`),
	}, "index.txt", &BuildOptions{
		AllowGoStmt: true,
		Globals: native.Declarations{
			"signal": func() { close(started) },
			"touch":  func() { calls++ },
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var out strings.Builder
	err = tmpl.Run(&out, nil, &RunOptions{Deterministic: true})
	if !errors.Is(err, ErrDeterministicGo) {
		t.Fatalf("run error = %v, want %v", err, ErrDeterministicGo)
	}
	select {
	case <-started:
		t.Fatal("goroutine started during deterministic execution")
	default:
	}
	if calls != 0 || out.Len() != 0 {
		t.Fatalf("effects: calls=%d output=%q, want none", calls, out.String())
	}

	if err := tmpl.Run(&out, nil, nil); err != nil {
		t.Fatalf("default run: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("default execution did not start goroutine")
	}
	if got, want := out.String(), "prefix"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if calls != 1 {
		t.Fatalf("touch calls = %d, want 1", calls)
	}
}

func TestDeterministicRejectsPrintBeforeExecution(t *testing.T) {
	tests := []struct {
		name  string
		files Files
	}{
		{
			name:  "print argument",
			files: Files{"index.txt": []byte(`prefix{% print(touch()) %}`)},
		},
		{
			name:  "println argument",
			files: Files{"index.txt": []byte(`prefix{% println(touch()) %}`)},
		},
		{
			name:  "deferred print",
			files: Files{"index.txt": []byte(`prefix{% defer print(touch()) %}`)},
		},
		{
			name:  "deferred println",
			files: Files{"index.txt": []byte(`prefix{% defer println(touch()) %}`)},
		},
		{
			name: "nested macro",
			files: Files{
				"index.txt": []byte(`prefix{% macro P %}{% print(touch()) %}{% end %}{{ P() }}`),
			},
		},
		{
			name: "imported macro",
			files: Files{
				"index.txt": []byte(`prefix{% import "print.txt" for P %}{{ P() }}`),
				"print.txt": []byte(`{% macro P %}{% print(touch()) %}{% end %}`),
			},
		},
		{
			name: "rendered template",
			files: Files{
				"index.txt": []byte(`prefix{{ render "print.txt" }}`),
				"print.txt": []byte(`{% print(touch()) %}`),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			prints := 0
			touch := func() string {
				calls++
				return "touched"
			}
			tmpl, err := BuildTemplate(test.files, "index.txt", &BuildOptions{Globals: native.Declarations{"touch": touch}})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var out strings.Builder
			err = tmpl.Run(&out, nil, &RunOptions{
				Deterministic: true,
				Print: func(any) {
					prints++
				},
			})
			if !errors.Is(err, ErrDeterministicPrint) {
				t.Fatalf("run error = %v, want %v", err, ErrDeterministicPrint)
			}
			if calls != 0 || prints != 0 || out.Len() != 0 {
				t.Fatalf("effects: calls=%d prints=%d output=%q, want none", calls, prints, out.String())
			}
		})
	}
}

func TestDefaultExecutionStillAllowsPrint(t *testing.T) {
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`before{% print("a") %}{% println("b") %}after`),
	}, "index.txt", nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var out strings.Builder
	var printed strings.Builder
	err = tmpl.Run(&out, nil, &RunOptions{Print: func(value any) {
		printed.WriteString(value.(string))
	}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := out.String(), "beforeafter"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if got, want := printed.String(), "ab\n"; got != want {
		t.Fatalf("printed = %q, want %q", got, want)
	}
}

func TestDeterministicProgramRejectsPrintBeforeExecution(t *testing.T) {
	tests := map[string]string{
		"direct": `func hidden() { print("hidden") }
func main() { hidden() }`,
		"deferred print":   `func main() { defer print("hidden") }`,
		"deferred println": `func main() { defer println("hidden") }`,
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			program, err := Build(Files{
				"main.go": []byte("package main\n" + source),
			}, nil)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			prints := 0
			err = program.Run(&RunOptions{
				Deterministic: true,
				Print: func(any) {
					prints++
				},
			})
			if !errors.Is(err, ErrDeterministicPrint) {
				t.Fatalf("run error = %v, want %v", err, ErrDeterministicPrint)
			}
			if prints != 0 {
				t.Fatalf("print callback called %d times, want 0", prints)
			}
		})
	}
}

func TestDeterministicRejectsStaticUnstableMapKeyBeforeExecution(t *testing.T) {
	value := 1
	values := map[*int]string{&value: "value"}
	calls := 0
	touch := func() { calls++ }
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`prefix{% touch() %}{% for key := range values %}{{ key }}{% end %}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"touch":  touch,
		"values": &values,
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var out strings.Builder
	err = tmpl.Run(&out, nil, &RunOptions{Deterministic: true})
	if !errors.Is(err, ErrDeterministicMapKey) {
		t.Fatalf("run error = %v, want %v", err, ErrDeterministicMapKey)
	}
	if calls != 0 || out.Len() != 0 {
		t.Fatalf("effects: calls=%d output=%q, want none", calls, out.String())
	}
}

func TestDefaultExecutionStillAllowsIdentityMapKeys(t *testing.T) {
	value := 1
	values := map[*int]string{&value: "value"}
	bodyCalls := 0
	body := func() { bodyCalls++ }
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{% for key := range values %}{% body() %}{% _ = key %}{% end %}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"body":   body,
		"values": &values,
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := tmpl.Run(&strings.Builder{}, nil, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if bodyCalls != 1 {
		t.Fatalf("loop body called %d times, want 1", bodyCalls)
	}
}

func TestDeterministicRejectsDynamicUnstableMapKeys(t *testing.T) {
	value := 1
	tests := []struct {
		name  string
		value any
	}{
		{"pointer", map[any]string{&value: "pointer"}},
		{"NaN", map[any]string{math.NaN(): "NaN"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := test.value
			bodyCalls := 0
			body := func() { bodyCalls++ }
			tmpl, err := BuildTemplate(Files{
				"index.txt": []byte(`{% for key := range values %}{% body() %}{{ key }}{% end %}`),
			}, "index.txt", &BuildOptions{Globals: native.Declarations{
				"body":   body,
				"values": &values,
			}})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			err = tmpl.Run(&strings.Builder{}, nil, &RunOptions{Deterministic: true})
			if !errors.Is(err, ErrDeterministicMapKey) {
				t.Fatalf("run error = %v, want %v", err, ErrDeterministicMapKey)
			}
			if bodyCalls != 0 {
				t.Fatalf("loop body called %d times, want 0", bodyCalls)
			}
		})
	}
}
