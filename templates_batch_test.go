// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"context"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gitlab.com/haproxy-haptic/scriggo/native"
)

type batchBenchmarkResource struct {
	Name string
}

type batchBenchmarkStore struct {
	GetSingle func(...any) *batchBenchmarkResource
}

type batchObserverTarget struct{}

func (*batchObserverTarget) Read() string { return "read" }

type batchContextKey struct{}

type batchBenchmarkRegexp struct {
	expression *regexp.Regexp
}

func (expression batchBenchmarkRegexp) ReplaceAll(input, replacement string) string {
	return expression.expression.ReplaceAllString(input, replacement)
}

func TestTemplateRunBatchIsolatesGlobalsAndLifecycle(t *testing.T) {
	active := ""
	current := func() string { return active }
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{{ value }}:{{ current() }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"current": native.Synchronous(current),
		"value":   (*string)(nil),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	var first, second strings.Builder
	err = tmpl.RunBatch([]BatchRun{
		{
			Out:  &first,
			Vars: map[string]any{"value": "first"},
			Before: func() error {
				active = "one"
				return nil
			},
			After: func() { active = "" },
		},
		{
			Out:  &second,
			Vars: map[string]any{"value": "second"},
			Before: func() error {
				if active != "" {
					t.Fatalf("prior batch activation leaked: %q", active)
				}
				active = "two"
				return nil
			},
			After: func() { active = "" },
		},
	})
	if err != nil {
		t.Fatalf("run batch: %v", err)
	}
	if got, want := first.String(), "first:one"; got != want {
		t.Fatalf("first output = %q, want %q", got, want)
	}
	if got, want := second.String(), "second:two"; got != want {
		t.Fatalf("second output = %q, want %q", got, want)
	}
	if active != "" {
		t.Fatalf("last batch activation leaked: %q", active)
	}
}

func TestTemplateRunBatchReusesOnlyUnescapedGenerations(t *testing.T) {
	t.Run("pointer initializers preserve aliases", func(t *testing.T) {
		tmpl, err := BuildTemplate(Files{
			"index.txt": []byte(`{% value = value + "!" %}{{ value }}`),
		}, "index.txt", &BuildOptions{Globals: native.Declarations{"value": (*string)(nil)}})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		firstValue, secondValue := "first", "second"
		var first, second strings.Builder
		if err := tmpl.RunBatch([]BatchRun{
			{Out: &first, Vars: map[string]any{"value": &firstValue}},
			{Out: &second, Vars: map[string]any{"value": &secondValue}},
		}); err != nil {
			t.Fatalf("run batch: %v", err)
		}
		if got, want := first.String()+":"+second.String(), "first!:second!"; got != want {
			t.Fatalf("output = %q, want %q", got, want)
		}
		if firstValue != "first!" || secondValue != "second!" {
			t.Fatalf("initializers = %q/%q, want first!/second!", firstValue, secondValue)
		}
	})

	t.Run("escaped addresses retain generations", func(t *testing.T) {
		var retained []*string
		tmpl, err := BuildTemplate(Files{
			"index.txt": []byte(`{% retain(&value) %}{{ value }}`),
		}, "index.txt", &BuildOptions{Globals: native.Declarations{
			"retain": native.Synchronous(func(value *string) { retained = append(retained, value) }),
			"value":  (*string)(nil),
		}})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if err := tmpl.RunBatch([]BatchRun{
			{Out: io.Discard, Vars: map[string]any{"value": "first"}},
			{Out: io.Discard, Vars: map[string]any{"value": "second"}},
		}); err != nil {
			t.Fatalf("run batch: %v", err)
		}
		if len(retained) != 2 || retained[0] == retained[1] || *retained[0] != "first" || *retained[1] != "second" {
			t.Fatalf("retained generations = %#v", retained)
		}
	})

	t.Run("nested renders see each generation", func(t *testing.T) {
		tmpl, err := BuildTemplate(Files{
			"index.txt":  []byte(`{{ render "nested.txt" }}`),
			"nested.txt": []byte(`{{ value }}`),
		}, "index.txt", &BuildOptions{Globals: native.Declarations{"value": (*string)(nil)}})
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
	})
}

func TestTemplateRunBatchStopsAndDeactivatesOnError(t *testing.T) {
	stop := func() string { panic("failed") }
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{{ stop() }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{"stop": native.Synchronous(stop)}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	deactivated := false
	laterActivated := false
	err = tmpl.RunBatch([]BatchRun{
		{Out: &strings.Builder{}, After: func() { deactivated = true }},
		{Out: &strings.Builder{}, Before: func() error { laterActivated = true; return nil }},
	})
	var batchErr *BatchRunError
	if !errors.As(err, &batchErr) || batchErr.Index != 0 {
		t.Fatalf("error = %v, want batch item 0", err)
	}
	if !deactivated {
		t.Fatal("failed batch item was not deactivated")
	}
	if laterActivated {
		t.Fatal("batch continued after the first error")
	}
}

func TestTemplateRunBatchRejectsDetachedWorkBeforeActivation(t *testing.T) {
	touched := false
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{% go touch() %}`),
	}, "index.txt", &BuildOptions{
		AllowGoStmt: true,
		Globals:     native.Declarations{"touch": func() { touched = true }},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	activated := false
	err = tmpl.RunBatch([]BatchRun{{
		Out: io.Discard,
		Before: func() error {
			activated = true
			return nil
		},
	}})
	if !errors.Is(err, ErrBatchDetachedWork) {
		t.Fatalf("error = %v, want %v", err, ErrBatchDetachedWork)
	}
	if activated || touched {
		t.Fatalf("batch executed before rejection: activated=%t touched=%t", activated, touched)
	}
}

func TestTemplateBatchSafeRejectsDetachedWorkRecursively(t *testing.T) {
	tests := []struct {
		name  string
		files Files
	}{
		{
			name: "nested function",
			files: Files{
				"index.txt": []byte(`{% var fn = func() { go touch() } %}safe`),
			},
		},
		{
			name: "rendered file",
			files: Files{
				"index.txt":   []byte(`{{ render "partial.txt" }}`),
				"partial.txt": []byte(`{% go touch() %}`),
			},
		},
		{
			name: "parallel macro",
			files: Files{
				"index.txt": []byte(`{% macro M %}value{% end %}{{ go M() }}`),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpl, err := BuildTemplate(test.files, "index.txt", &BuildOptions{
				AllowGoStmt: true,
				Globals:     native.Declarations{"touch": func() {}},
			})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if tmpl.BatchSafe() {
				t.Fatal("BatchSafe = true, want false")
			}
			if err := tmpl.RunBatch(nil); !errors.Is(err, ErrBatchDetachedWork) {
				t.Fatalf("empty batch error = %v, want %v", err, ErrBatchDetachedWork)
			}
		})
	}
}

func TestTemplateBatchSafeAcceptsNestedSynchronousWork(t *testing.T) {
	tmpl, err := BuildTemplate(Files{
		"index.txt":   []byte(`{% macro M %}{{ render "partial.txt" }}{% end %}{{ M() }}`),
		"partial.txt": []byte(`{% var fn = func() string { return "safe" } %}{{ fn() }}`),
	}, "index.txt", nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !tmpl.BatchSafe() {
		t.Fatal("BatchSafe = false, want true")
	}
	if err := tmpl.RunBatch(nil); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
}

func TestTemplateRunBatchRejectsUncertifiedNativeBeforeExecution(t *testing.T) {
	var retained []func() string
	retain := func(fn func() string) {
		retained = append(retained, fn)
	}
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{% retain(func() string { return value }) %}{{ value }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"retain": retain,
		"value":  (*string)(nil),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if tmpl.BatchSafe() {
		t.Fatal("BatchSafe = true, want false")
	}

	var ordinary strings.Builder
	if err := tmpl.Run(&ordinary, map[string]any{"value": "ordinary"}, nil); err != nil {
		t.Fatalf("ordinary run: %v", err)
	}
	if got, want := ordinary.String(), "ordinary"; got != want {
		t.Fatalf("ordinary output = %q, want %q", got, want)
	}
	if len(retained) != 1 || retained[0]() != "ordinary" {
		t.Fatalf("ordinary retained closures = %d, want one usable closure", len(retained))
	}
	retained = nil

	activated := 0
	deactivated := 0
	var first, second strings.Builder
	err = tmpl.RunBatch([]BatchRun{
		{
			Out:  &first,
			Vars: map[string]any{"value": "first"},
			Before: func() error {
				activated++
				return nil
			},
			After: func() { deactivated++ },
		},
		{
			Out:  &second,
			Vars: map[string]any{"value": "second"},
			Before: func() error {
				activated++
				return nil
			},
			After: func() { deactivated++ },
		},
	})
	if !errors.Is(err, ErrBatchUncertifiedNative) {
		t.Fatalf("error = %v, want %v", err, ErrBatchUncertifiedNative)
	}
	var batchErr *BatchRunError
	if errors.As(err, &batchErr) {
		t.Fatalf("validation error = %#v, want pre-execution error", batchErr)
	}
	if activated != 0 || deactivated != 0 || len(retained) != 0 || first.Len() != 0 || second.Len() != 0 {
		t.Fatalf(
			"batch changed state before rejection: activated=%d deactivated=%d retained=%d output=%q/%q",
			activated, deactivated, len(retained), first.String(), second.String(),
		)
	}
}

func TestTemplateBatchSafeRejectsUncertifiedNativeRecursively(t *testing.T) {
	tests := []struct {
		name  string
		files Files
	}{
		{
			name: "direct call",
			files: Files{
				"index.txt": []byte(`{% call() %}`),
			},
		},
		{
			name: "nested function",
			files: Files{
				"index.txt": []byte(`{% var fn = func() { call() } %}safe`),
			},
		},
		{
			name: "macro",
			files: Files{
				"index.txt": []byte(`{% macro M %}{% call() %}{% end %}safe`),
			},
		},
		{
			name: "rendered file",
			files: Files{
				"index.txt":   []byte(`{{ render "partial.txt" }}`),
				"partial.txt": []byte(`{% call() %}`),
			},
		},
		{
			name: "imported macro",
			files: Files{
				"index.txt":   []byte(`{% import "library.txt" for M %}safe`),
				"library.txt": []byte(`{% macro M %}{% call() %}{% end %}`),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpl, err := BuildTemplate(test.files, "index.txt", &BuildOptions{Globals: native.Declarations{
				"call": func() {},
			}})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if tmpl.BatchSafe() {
				t.Fatal("BatchSafe = true, want false")
			}
			if err := tmpl.RunBatch(nil); !errors.Is(err, ErrBatchUncertifiedNative) {
				t.Fatalf("empty batch error = %v, want %v", err, ErrBatchUncertifiedNative)
			}
		})
	}
}

func TestTemplateBatchSafeRejectsUncertifiedNativeFunctionField(t *testing.T) {
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{{ store.GetSingle("default", "name").Name }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"store": (*batchBenchmarkStore)(nil),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if tmpl.BatchSafe() {
		t.Fatal("BatchSafe = true, want false")
	}
	if err := tmpl.RunBatch(nil); !errors.Is(err, ErrBatchUncertifiedNative) {
		t.Fatalf("empty batch error = %v, want %v", err, ErrBatchUncertifiedNative)
	}
}

func TestTemplateRunBatchAcceptsCertifiedSynchronousCallback(t *testing.T) {
	invoke := func(fn func() string) string { return fn() }
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{{ invoke(func() string { return value }) }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"invoke": native.Synchronous(invoke),
		"value":  (*string)(nil),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !tmpl.BatchSafe() {
		t.Fatal("BatchSafe = false, want true")
	}
	var first, second strings.Builder
	if err := tmpl.RunBatch([]BatchRun{
		{Out: &first, Vars: map[string]any{"value": "first"}},
		{Out: &second, Vars: map[string]any{"value": "second"}},
	}); err != nil {
		t.Fatalf("run batch: %v", err)
	}
	if got, want := first.String(), "first"; got != want {
		t.Fatalf("first output = %q, want %q", got, want)
	}
	if got, want := second.String(), "second"; got != want {
		t.Fatalf("second output = %q, want %q", got, want)
	}
}

func TestTemplateRunBatchGenerationsSurviveBrokenNativeCertificate(t *testing.T) {
	var retained []func() string
	contextValue := func(env native.Env) string {
		value, _ := env.Context().Value(batchContextKey{}).(string)
		return value
	}
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{% retain(func() string { return value + ":" + contextValue() }) %}{{ value }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"contextValue": native.Synchronous(contextValue),
		"retain": native.Synchronous(func(fn func() string) {
			retained = append(retained, fn)
		}),
		"value": (*string)(nil),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var first, second strings.Builder
	err = tmpl.RunBatch([]BatchRun{
		{
			Out:     &first,
			Vars:    map[string]any{"value": "first"},
			Options: &RunOptions{Context: context.WithValue(context.Background(), batchContextKey{}, "one")},
		},
		{
			Out:     &second,
			Vars:    map[string]any{"value": "second"},
			Options: &RunOptions{Context: context.WithValue(context.Background(), batchContextKey{}, "two")},
		},
	})
	if err != nil {
		t.Fatalf("run batch: %v", err)
	}
	if len(retained) != 2 {
		t.Fatalf("retained %d closures, want 2", len(retained))
	}
	if got := retained[0](); got != "first:one" {
		t.Fatalf("first retained closure = %q, want first:one", got)
	}
	if got := retained[1](); got != "second:two" {
		t.Fatalf("second retained closure = %q, want second:two", got)
	}
}

func TestTemplateRunBatchResetsContextAndObservers(t *testing.T) {
	var retainedEnvironments []native.Env
	captureEnvironment := func(env native.Env) {
		retainedEnvironments = append(retainedEnvironments, env)
	}
	contextValue := func(env native.Env) string {
		value, _ := env.Context().Value(batchContextKey{}).(string)
		return value
	}
	invoke := func(fn func() string) string { return fn() }
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{% captureEnvironment() %}{{ contextValue() }}:{{ invoke(func() string { return contextValue() }) }}:{{ target.Read() }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"captureEnvironment": native.Synchronous(captureEnvironment),
		"contextValue":       native.Synchronous(contextValue),
		"invoke":             native.Synchronous(invoke),
		"target":             native.Synchronous((*batchObserverTarget)(nil), "Read"),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	rootOne := context.WithValue(context.Background(), batchContextKey{}, "root-one")
	rootTwo := context.WithValue(context.Background(), batchContextKey{}, "root-two")
	target := &batchObserverTarget{}
	observations := 0
	var first, second strings.Builder
	err = tmpl.RunBatch([]BatchRun{
		{
			Out:  &first,
			Vars: map[string]any{"target": target},
			Options: &RunOptions{
				Context: rootOne,
				ChildContext: func(parent context.Context) context.Context {
					return context.WithValue(parent, batchContextKey{}, "child-one")
				},
				ObserveNativeCall: func(native.Env, NativeCall) error {
					observations++
					return nil
				},
			},
		},
		{
			Out:     &second,
			Vars:    map[string]any{"target": target},
			Options: &RunOptions{Context: rootTwo},
		},
	})
	if err != nil {
		t.Fatalf("run batch: %v", err)
	}
	if got, want := first.String(), "root-one:child-one:read"; got != want {
		t.Fatalf("first output = %q, want %q", got, want)
	}
	if got, want := second.String(), "root-two:root-two:read"; got != want {
		t.Fatalf("second output = %q, want %q", got, want)
	}
	if observations != 1 {
		t.Fatalf("native observations = %d, want 1", observations)
	}
	if len(retainedEnvironments) != 2 || retainedEnvironments[0] == retainedEnvironments[1] {
		t.Fatalf("retained native environments = %#v, want two generations", retainedEnvironments)
	}
	if got := retainedEnvironments[0].Context().Value(batchContextKey{}); got != "root-one" {
		t.Fatalf("first retained native context = %v, want root-one", got)
	}
	if got := retainedEnvironments[1].Context().Value(batchContextKey{}); got != "root-two" {
		t.Fatalf("second retained native context = %v, want root-two", got)
	}
}

func TestTemplateRunBatchCleanAfterFailedExecution(t *testing.T) {
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{% value += "-mutated" %}{% if fail %}{% panic("failed") %}{% end %}{{ value }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"fail":  (*bool)(nil),
		"value": (*string)(nil),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	mutationError := errors.New("mutation rejected")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	failures := []struct {
		name    string
		vars    map[string]any
		options *RunOptions
		want    error
	}{
		{
			name: "panic",
			vars: map[string]any{"fail": true, "value": "poison"},
		},
		{
			name:    "cancellation",
			vars:    map[string]any{"fail": false, "value": "poison"},
			options: &RunOptions{Context: canceled},
			want:    context.Canceled,
		},
		{
			name: "mutation observer",
			vars: map[string]any{"fail": false, "value": "poison"},
			options: &RunOptions{ObserveMutation: func(native.Env, Mutation) error {
				return mutationError
			}},
			want: mutationError,
		},
	}
	for _, failure := range failures {
		t.Run(failure.name, func(t *testing.T) {
			err := tmpl.RunBatch([]BatchRun{{
				Out:     io.Discard,
				Vars:    failure.vars,
				Options: failure.options,
			}})
			var batchErr *BatchRunError
			if !errors.As(err, &batchErr) || batchErr.Index != 0 {
				t.Fatalf("error = %v, want batch item 0", err)
			}
			if failure.want != nil && !errors.Is(err, failure.want) {
				t.Fatalf("error = %v, want %v", err, failure.want)
			}

			var clean strings.Builder
			if err := tmpl.RunBatch([]BatchRun{{
				Out:  &clean,
				Vars: map[string]any{"fail": false, "value": "clean"},
			}}); err != nil {
				t.Fatalf("clean rerun: %v", err)
			}
			if got, want := clean.String(), "clean-mutated"; got != want {
				t.Fatalf("clean output = %q, want %q", got, want)
			}
		})
	}
}

func TestTemplateSynchronousReflectiveFieldCall(t *testing.T) {
	store := batchBenchmarkStore{
		GetSingle: func(keys ...any) *batchBenchmarkResource {
			return &batchBenchmarkResource{Name: keys[1].(string)}
		},
	}
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{{ store.GetSingle("default", "direct").Name }}:{% get := store.GetSingle %}{{ get("default", "escaped").Name }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"store": (*batchBenchmarkStore)(nil),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var output strings.Builder
	if err := tmpl.Run(&output, map[string]any{"store": store}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := output.String(), "direct:escaped"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func BenchmarkTemplateRunBatchReflectiveFieldCall(b *testing.B) {
	store := batchBenchmarkStore{
		GetSingle: func(keys ...any) *batchBenchmarkResource {
			return &batchBenchmarkResource{Name: keys[1].(string)}
		},
	}
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{{ store.GetSingle("default", value).Name }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"store": native.Synchronous((*batchBenchmarkStore)(nil), "GetSingle"),
		"value": (*string)(nil),
	}})
	if err != nil {
		b.Fatalf("build: %v", err)
	}

	const size = 4096
	runs := make([]BatchRun, size)
	for index := range runs {
		runs[index] = BatchRun{
			Out:  io.Discard,
			Vars: map[string]any{"store": store, "value": strconv.Itoa(index)},
		}
	}
	b.ReportAllocs()
	b.ReportMetric(size, "runs/op")
	b.ResetTimer()
	for range b.N {
		if err := tmpl.RunBatch(runs); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTemplateFusedBatchReflectiveFieldCall(b *testing.B) {
	store := batchBenchmarkStore{
		GetSingle: func(keys ...any) *batchBenchmarkResource {
			return &batchBenchmarkResource{Name: keys[1].(string)}
		},
	}
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{%- for _, value := range values -%}{{ store.GetSingle("default", value).Name }}{%- end -%}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"store":  native.Synchronous((*batchBenchmarkStore)(nil), "GetSingle"),
		"values": (*[]string)(nil),
	}})
	if err != nil {
		b.Fatalf("build: %v", err)
	}

	const size = 4096
	values := make([]string, size)
	for index := range values {
		values[index] = strconv.Itoa(index)
	}
	b.ReportAllocs()
	b.ReportMetric(size, "runs/op")
	b.ResetTimer()
	for range b.N {
		if err := tmpl.Run(io.Discard, map[string]any{"store": store, "values": values}, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTemplateRunBatchBeforeNativeCall(b *testing.B) {
	store := batchBenchmarkStore{
		GetSingle: func(keys ...any) *batchBenchmarkResource {
			return &batchBenchmarkResource{Name: keys[1].(string)}
		},
	}
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{{ store.GetSingle("default", value).Name }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"store": native.Synchronous((*batchBenchmarkStore)(nil), "GetSingle"),
		"value": (*string)(nil),
	}})
	if err != nil {
		b.Fatalf("build: %v", err)
	}

	const size = 4096
	for _, benchmark := range []struct {
		name    string
		options *RunOptions
	}{
		{name: "nil"},
		{
			name: "no-op",
			options: &RunOptions{BeforeNativeCall: func(native.Env) error {
				return nil
			}},
		},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			runs := make([]BatchRun, size)
			for index := range runs {
				runs[index] = BatchRun{
					Out:     io.Discard,
					Vars:    map[string]any{"store": store, "value": strconv.Itoa(index)},
					Options: benchmark.options,
				}
			}
			b.ReportAllocs()
			b.ReportMetric(size, "runs/op")
			b.ResetTimer()
			for range b.N {
				if err := tmpl.RunBatch(runs); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkTemplatePostprocess3000(b *testing.B) {
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{{- regexp("\\n([ \\t]*\\n)+").ReplaceAll(input, "\n\n") -}}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"input": (*string)(nil),
		"regexp": native.Synchronous(func(expression string) batchBenchmarkRegexp {
			return batchBenchmarkRegexp{expression: regexp.MustCompile(expression)}
		}, "ReplaceAll"),
	}})
	if err != nil {
		b.Fatalf("build: %v", err)
	}

	const size = 3000
	builders := make([]strings.Builder, size)
	runs := make([]BatchRun, size)
	options := &RunOptions{Context: context.Background()}
	for index := range runs {
		input := "    backend backend-" + strconv.Itoa(index) +
			"\n        server server 10.0.0.1:8080\n\n\n"
		builders[index].Grow(len(input))
		runs[index] = BatchRun{
			Out:     &builders[index],
			Vars:    map[string]any{"input": input},
			Options: options,
		}
	}

	reset := func() {
		for index := range builders {
			builders[index].Reset()
		}
	}
	b.ReportMetric(size, "sections/op")
	b.Run("Run", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			reset()
			for index := range runs {
				if err := tmpl.Run(runs[index].Out, runs[index].Vars, options); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
	b.Run("RunBatch", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			reset()
			if err := tmpl.RunBatch(runs); err != nil {
				b.Fatal(err)
			}
		}
	})
}
