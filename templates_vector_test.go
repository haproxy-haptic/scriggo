// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"gitlab.com/haproxy-haptic/scriggo/ast"
	"gitlab.com/haproxy-haptic/scriggo/native"
)

type vectorTestController struct {
	authority *native.VectorAuthority
	calls     []string
	onBegin   func(int)
}

func (controller *vectorTestController) Begin(env native.Env, index int) {
	vectorEnv, ok := env.(native.VectorEnv)
	if !ok {
		env.Stop(errors.New("vector environment is unavailable"))
		return
	}
	if err := vectorEnv.ActivateVectorIndex(controller.authority, index); err != nil {
		env.Stop(err)
		return
	}
	controller.calls = append(controller.calls, "begin:"+strconv.Itoa(index))
	if controller.onBegin != nil {
		controller.onBegin(index)
	}
}

type vectorPointerValue struct {
	Text string
}

type vectorMethodValue struct {
	Text string
}

type vectorResourceCapability struct {
	Read func(native.Env) string
}

type vectorVMNativeLifecycle struct {
	calls       []string
	abortIndex  int
	abortErr    error
	beginErr    error
	finishErr   error
	commitErr   error
	beginPanic  error
	finishPanic error
	commitPanic error
	onAbort     func()
}

func (lifecycle *vectorVMNativeLifecycle) Begin(ctx context.Context, index int) error {
	if ctx == nil {
		return errors.New("nil vector context")
	}
	lifecycle.calls = append(lifecycle.calls, "begin:"+strconv.Itoa(index))
	if lifecycle.beginPanic != nil {
		panic(lifecycle.beginPanic)
	}
	return lifecycle.beginErr
}

func (lifecycle *vectorVMNativeLifecycle) Finish(index int) error {
	lifecycle.calls = append(lifecycle.calls, "finish:"+strconv.Itoa(index))
	if lifecycle.finishPanic != nil {
		panic(lifecycle.finishPanic)
	}
	return lifecycle.finishErr
}

func (lifecycle *vectorVMNativeLifecycle) Commit(index int) error {
	lifecycle.calls = append(lifecycle.calls, "commit:"+strconv.Itoa(index))
	if lifecycle.commitPanic != nil {
		panic(lifecycle.commitPanic)
	}
	return lifecycle.commitErr
}

func (lifecycle *vectorVMNativeLifecycle) Abort(index int, err error) {
	lifecycle.calls = append(lifecycle.calls, "abort:"+strconv.Itoa(index))
	lifecycle.abortIndex = index
	lifecycle.abortErr = err
	if lifecycle.onAbort != nil {
		lifecycle.onAbort()
	}
}

type vectorBenchmarkController struct {
	authority *native.VectorAuthority
}

func (controller *vectorBenchmarkController) Begin(env native.Env, index int) {
	if err := env.(native.VectorEnv).ActivateVectorIndex(controller.authority, index); err != nil {
		env.Stop(err)
	}
}

func (controller *vectorBenchmarkController) End(env native.Env, _ int) {
	if err := env.(native.VectorEnv).RevokeVectorIndex(controller.authority); err != nil {
		env.Stop(err)
	}
}

type vectorBenchmarkLifecycle struct{}

func (*vectorBenchmarkLifecycle) Begin(context.Context, int) error { return nil }
func (*vectorBenchmarkLifecycle) Finish(int) error                 { return nil }
func (*vectorBenchmarkLifecycle) Commit(int) error                 { return nil }
func (*vectorBenchmarkLifecycle) Abort(int, error)                 {}

func (value *vectorMethodValue) Read() string {
	return value.Text
}

func (controller *vectorTestController) End(env native.Env, index int) {
	vectorEnv, ok := env.(native.VectorEnv)
	if !ok {
		env.Stop(errors.New("vector environment is unavailable"))
		return
	}
	if err := vectorEnv.RevokeVectorIndex(controller.authority); err != nil {
		env.Stop(err)
		return
	}
	controller.calls = append(controller.calls, "end:"+strconv.Itoa(index))
}

func vectorTestGlobals(controller **vectorTestController) native.Declarations {
	return native.Declarations{
		"controller": native.Synchronous(controller, "Begin", "End"),
		"indices":    (*[]int)(nil),
		"value":      (*string)(nil),
	}
}

func TestTemplateVectorRunReplacesImportedGlobalSlots(t *testing.T) {
	template, err := BuildTemplate(Files{
		"wrapper.txt": []byte(`{% for index := range indices %}{% controller.Begin(index) %}{{ render "entry.txt" }}{% controller.End(index) %}{% end %}`),
		"entry.txt":   []byte(`{% import "library.txt" for Value %}{{ Value() }}`),
		"library.txt": []byte(`{% macro Value() string %}{{ value }}{% end %}`),
	}, "wrapper.txt", &BuildOptions{Globals: vectorTestGlobals((**vectorTestController)(nil))})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	valueSlots := 0
	for _, global := range template.globals {
		if global.Name == "value" && !global.Value.IsValid() {
			valueSlots++
		}
	}
	if valueSlots == 0 {
		t.Fatal("compiled template has no mutable value slot")
	}

	authority := native.NewVectorAuthority()
	controller := &vectorTestController{authority: authority}
	var output strings.Builder
	err = template.Run(&output, map[string]any{
		"controller": controller,
		"indices":    []int{0, 1},
	}, &RunOptions{Vector: &VectorRunOptions{
		Authority: authority,
		Count:     2,
		Bindings:  map[string]any{"value": []string{"first", "second"}},
		Contexts:  []context.Context{context.Background(), context.Background()},
		VMNative:  true,
	}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := output.String(), "firstsecond"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if got, want := strings.Join(controller.calls, ","), "begin:0,end:0,begin:1,end:1"; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestTemplateVectorBindingsPreserveImportedCapabilityContext(t *testing.T) {
	type contextKey struct{}
	globals := vectorTestGlobals((**vectorTestController)(nil))
	globals["item"] = (*string)(nil)
	globals["invoke"] = native.Synchronous(func(callback func() string) string { return callback() })
	globals["resources"] = native.Synchronous((*vectorResourceCapability)(nil), "Read")
	template, err := BuildTemplate(Files{
		"wrapper.txt": []byte(`{% for index := range indices %}{% controller.Begin(index) %}{{ render "entry.txt" }}{% controller.End(index) %}{% end %}`),
		"entry.txt":   []byte(`{% import "library.txt" for Read %}{{ Read() }}`),
		"library.txt": []byte(`{% macro Read() string %}{{ invoke(func() string { return item + "=" + resources.Read() + ";" }) }}{% end %}`),
	}, "wrapper.txt", &BuildOptions{Globals: globals})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	resources := make([]vectorResourceCapability, 2)
	for index, want := range []string{"first", "second"} {
		want := want
		resources[index].Read = func(env native.Env) string {
			got, _ := env.Context().Value(contextKey{}).(string)
			if got != want {
				env.Stop(fmt.Errorf("resource context = %q, want %q", got, want))
			}
			return want
		}
	}
	authority := native.NewVectorAuthority()
	controller := &vectorTestController{authority: authority}
	var output strings.Builder
	err = template.Run(&output, map[string]any{
		"controller": controller,
		"indices":    []int{0, 1},
	}, &RunOptions{Vector: &VectorRunOptions{
		Authority: authority,
		Count:     2,
		Bindings: map[string]any{
			"item":      []string{"first", "second"},
			"resources": resources,
		},
		Contexts: []context.Context{
			context.WithValue(context.Background(), contextKey{}, "first"),
			context.WithValue(context.Background(), contextKey{}, "second"),
		},
		VMNative: true,
	}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := output.String(), "first=first;second=second;"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestTemplateVMNativeVectorBoundaryPreservesOrderAndContext(t *testing.T) {
	type contextKey struct{}
	boundary := native.NewVectorBoundary()
	globals := native.Declarations{
		"boundary": native.Synchronous((**native.VectorBoundary)(nil), "Begin", "End"),
		"indices":  (*[]int)(nil),
		"item":     (*string)(nil),
		"readContext": native.Synchronous(func(env native.Env) string {
			return env.Context().Value(contextKey{}).(string)
		}),
	}
	template, err := BuildTemplate(Files{
		"wrapper.txt": []byte(`{% for index := range indices %}{% boundary.Begin(index) %}{{ render "entry.txt" }}{% boundary.End(index) %}{% end %}`),
		"entry.txt":   []byte(`{% import "library.txt" for Read %}{{ Read() }}`),
		"library.txt": []byte(`{% macro Read() string %}{{ item }}={{ readContext() }};{% end %}`),
	}, "wrapper.txt", &BuildOptions{Globals: globals})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	authority := native.NewVectorAuthority()
	lifecycle := &vectorVMNativeLifecycle{abortIndex: -1}
	var output strings.Builder
	err = template.Run(&output, map[string]any{
		"boundary": boundary,
		"indices":  []int{0, 1},
	}, &RunOptions{Vector: &VectorRunOptions{
		Authority: authority,
		Count:     2,
		Bindings:  map[string]any{"item": []string{"first", "second"}},
		Contexts: []context.Context{
			context.WithValue(context.Background(), contextKey{}, "first"),
			context.WithValue(context.Background(), contextKey{}, "second"),
		},
		VMNative:  true,
		Boundary:  boundary,
		Lifecycle: lifecycle,
	}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := output.String(), "first=first;second=second;"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if got, want := strings.Join(lifecycle.calls, ","), "begin:0,finish:0,commit:0,begin:1,finish:1,commit:1"; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
	if lifecycle.abortErr != nil || lifecycle.abortIndex != -1 {
		t.Fatalf("abort = (%d, %v), want none", lifecycle.abortIndex, lifecycle.abortErr)
	}
}

func TestTemplateVMNativeVectorBoundaryAbortsAfterRevocation(t *testing.T) {
	tests := []struct {
		name      string
		beginErr  bool
		finishErr bool
		commitErr bool
		panicAt   string
		wantCalls string
	}{
		{name: "begin", beginErr: true, wantCalls: "begin:0,abort:0"},
		{name: "finish", finishErr: true, wantCalls: "begin:0,finish:0,abort:0"},
		{name: "commit", commitErr: true, wantCalls: "begin:0,finish:0,commit:0,abort:0"},
		{name: "begin panic", panicAt: "begin", wantCalls: "begin:0,abort:0"},
		{name: "finish panic", panicAt: "finish", wantCalls: "begin:0,finish:0,abort:0"},
		{name: "commit panic", panicAt: "commit", wantCalls: "begin:0,finish:0,commit:0,abort:0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			boundary := native.NewVectorBoundary()
			var retainedContext context.Context
			captureContext := func(env native.Env) string {
				retainedContext = env.Context()
				return "value"
			}
			template, err := BuildTemplate(Files{
				"index.txt": []byte(`{% boundary.Begin(0) %}{{ captureContext() }}{% boundary.End(0) %}`),
			}, "index.txt", &BuildOptions{Globals: native.Declarations{
				"boundary":       native.Synchronous((**native.VectorBoundary)(nil), "Begin", "End"),
				"captureContext": native.Synchronous(captureContext),
			}})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			failure := errors.New("lifecycle failure")
			lifecycle := &vectorVMNativeLifecycle{abortIndex: -1}
			if test.beginErr {
				lifecycle.beginErr = failure
			}
			if test.finishErr {
				lifecycle.finishErr = failure
			}
			if test.commitErr {
				lifecycle.commitErr = failure
			}
			switch test.panicAt {
			case "begin":
				lifecycle.beginPanic = failure
			case "finish":
				lifecycle.finishPanic = failure
			case "commit":
				lifecycle.commitPanic = failure
			}
			var staleErr error
			lifecycle.onAbort = func() {
				if retainedContext != nil {
					staleErr = retainedContext.Err()
				}
			}
			authority := native.NewVectorAuthority()
			err = template.Run(io.Discard, map[string]any{"boundary": boundary}, &RunOptions{
				Vector: &VectorRunOptions{
					Authority: authority,
					Count:     1,
					Bindings:  map[string]any{},
					Contexts:  []context.Context{context.Background()},
					VMNative:  true,
					Boundary:  boundary,
					Lifecycle: lifecycle,
				},
			})
			if test.panicAt == "" && !errors.Is(err, failure) {
				t.Fatalf("error = %v, want %v", err, failure)
			}
			if test.panicAt != "" && !strings.Contains(err.Error(), failure.Error()) {
				t.Fatalf("panic error = %v, want it to contain %q", err, failure)
			}
			if got := strings.Join(lifecycle.calls, ","); got != test.wantCalls {
				t.Fatalf("calls = %q, want %q", got, test.wantCalls)
			}
			if !test.beginErr && test.panicAt != "begin" && !errors.Is(staleErr, ErrVectorGenerationRevoked) {
				t.Fatalf("retained context error = %v, want %v", staleErr, ErrVectorGenerationRevoked)
			}
			if lifecycle.abortIndex != 0 || test.panicAt == "" && !errors.Is(lifecycle.abortErr, failure) ||
				test.panicAt != "" && !strings.Contains(lifecycle.abortErr.Error(), failure.Error()) {
				t.Fatalf("abort = (%d, %v), want (0, %v)", lifecycle.abortIndex, lifecycle.abortErr, failure)
			}
		})
	}
}

func TestTemplateVMNativeVectorBoundaryRejectsInvalidSequence(t *testing.T) {
	tests := []struct {
		name      string
		source    string
		count     int
		wantCalls string
		wantError error
	}{
		{
			name:      "missing end",
			source:    `{% boundary.Begin(0) %}`,
			count:     1,
			wantCalls: "begin:0,abort:0",
		},
		{
			name:      "end before begin",
			source:    `{% boundary.End(0) %}`,
			count:     1,
			wantCalls: "abort:-1",
			wantError: ErrVectorInvalidConfiguration,
		},
		{
			name:      "duplicate begin",
			source:    `{% boundary.Begin(0) %}{% boundary.Begin(0) %}`,
			count:     1,
			wantCalls: "begin:0,abort:0",
			wantError: ErrVectorInvalidConfiguration,
		},
		{
			name:      "duplicate end",
			source:    `{% boundary.Begin(0) %}{% boundary.End(0) %}{% boundary.End(0) %}`,
			count:     1,
			wantCalls: "begin:0,finish:0,commit:0,abort:-1",
			wantError: ErrVectorInvalidConfiguration,
		},
		{
			name:      "skipped index",
			source:    `{% boundary.Begin(1) %}`,
			count:     2,
			wantCalls: "abort:-1",
			wantError: ErrVectorInvalidConfiguration,
		},
		{
			name:      "foreign boundary",
			source:    `{% foreign.Begin(0) %}`,
			count:     1,
			wantCalls: "abort:-1",
			wantError: ErrVectorInvalidConfiguration,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			boundary := native.NewVectorBoundary()
			foreign := native.NewVectorBoundary()
			template, err := BuildTemplate(Files{
				"index.txt": []byte(test.source),
			}, "index.txt", &BuildOptions{Globals: native.Declarations{
				"boundary": native.Synchronous((**native.VectorBoundary)(nil), "Begin", "End"),
				"foreign":  native.Synchronous((**native.VectorBoundary)(nil), "Begin", "End"),
			}})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			lifecycle := &vectorVMNativeLifecycle{abortIndex: -2}
			err = template.Run(io.Discard, map[string]any{
				"boundary": boundary,
				"foreign":  foreign,
			}, &RunOptions{Vector: &VectorRunOptions{
				Authority: native.NewVectorAuthority(),
				Count:     test.count,
				Bindings:  map[string]any{},
				Contexts:  vectorBackgroundContexts(test.count),
				VMNative:  true,
				Boundary:  boundary,
				Lifecycle: lifecycle,
			}})
			if err == nil {
				t.Fatal("run succeeded")
			}
			if test.wantError != nil && !errors.Is(err, test.wantError) {
				t.Fatalf("error = %v, want %v", err, test.wantError)
			}
			if got := strings.Join(lifecycle.calls, ","); got != test.wantCalls {
				t.Fatalf("calls = %q, want %q", got, test.wantCalls)
			}
			if lifecycle.abortErr == nil {
				t.Fatal("Abort received a nil error")
			}
		})
	}
}

func TestTemplateVMNativeVectorBoundaryRevokesOnCancellationAndPanic(t *testing.T) {
	tests := []struct {
		name      string
		source    string
		itemCtx   func() context.Context
		globals   native.Declarations
		wantCalls string
		wantError error
	}{
		{
			name:   "canceled before begin",
			source: `{% boundary.Begin(0) %}`,
			itemCtx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			wantCalls: "abort:0",
			wantError: context.Canceled,
		},
		{
			name:    "body panic",
			source:  `{% boundary.Begin(0) %}{{ captureContext() }}{% panicBody() %}`,
			itemCtx: context.Background,
			globals: native.Declarations{
				"panicBody": native.Synchronous(func() { panic("body panic") }),
			},
			wantCalls: "begin:0,abort:0",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			boundary := native.NewVectorBoundary()
			var retainedContext context.Context
			globals := native.Declarations{
				"boundary": native.Synchronous((**native.VectorBoundary)(nil), "Begin", "End"),
				"captureContext": native.Synchronous(func(env native.Env) string {
					retainedContext = env.Context()
					return "value"
				}),
			}
			for name, declaration := range test.globals {
				globals[name] = declaration
			}
			template, err := BuildTemplate(Files{"index.txt": []byte(test.source)}, "index.txt", &BuildOptions{
				Globals: globals,
			})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			lifecycle := &vectorVMNativeLifecycle{abortIndex: -1}
			var staleErr error
			lifecycle.onAbort = func() {
				if retainedContext != nil {
					staleErr = retainedContext.Err()
				}
			}
			err = template.Run(io.Discard, map[string]any{"boundary": boundary}, &RunOptions{
				Vector: &VectorRunOptions{
					Authority: native.NewVectorAuthority(),
					Count:     1,
					Bindings:  map[string]any{},
					Contexts:  []context.Context{test.itemCtx()},
					VMNative:  true,
					Boundary:  boundary,
					Lifecycle: lifecycle,
				},
			})
			if err == nil {
				t.Fatal("run succeeded")
			}
			if test.wantError != nil && !errors.Is(err, test.wantError) {
				t.Fatalf("error = %v, want %v", err, test.wantError)
			}
			if got := strings.Join(lifecycle.calls, ","); got != test.wantCalls {
				t.Fatalf("calls = %q, want %q", got, test.wantCalls)
			}
			if retainedContext != nil && !errors.Is(staleErr, ErrVectorGenerationRevoked) {
				t.Fatalf("retained context error = %v, want %v", staleErr, ErrVectorGenerationRevoked)
			}
		})
	}
}

func TestTemplateVMNativeVectorBoundaryAbortsOnPreflightFailure(t *testing.T) {
	boundary := native.NewVectorBoundary()
	template, err := BuildTemplate(Files{"index.txt": []byte(`constant`)}, "index.txt", nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	lifecycle := &vectorVMNativeLifecycle{abortIndex: -2}
	err = template.Run(io.Discard, nil, &RunOptions{
		NativeFunctionTrampolines: []*native.FunctionTrampoline{nil},
		Vector: &VectorRunOptions{
			Authority: native.NewVectorAuthority(), Count: 1, Bindings: map[string]any{},
			Contexts: []context.Context{context.Background()}, VMNative: true,
			Boundary: boundary, Lifecycle: lifecycle,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid nil native function trampoline") {
		t.Fatalf("error = %v, want invalid trampoline", err)
	}
	if got, want := strings.Join(lifecycle.calls, ","), "abort:-1"; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
	if lifecycle.abortErr == nil {
		t.Fatal("Abort received a nil error")
	}
}

func TestTemplateVMNativeVectorBoundaryAbortsOnFatalPanic(t *testing.T) {
	boundary := native.NewVectorBoundary()
	template, err := BuildTemplate(Files{
		"index.txt": []byte(`{% boundary.Begin(0) %}{% fatal() %}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"boundary": native.Synchronous((**native.VectorBoundary)(nil), "Begin", "End"),
		"fatal": native.Synchronous(func(env native.Env) {
			env.Fatal("fatal vector execution")
		}),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	lifecycle := &vectorVMNativeLifecycle{abortIndex: -1}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = template.Run(io.Discard, map[string]any{"boundary": boundary}, &RunOptions{
			Vector: &VectorRunOptions{
				Authority: native.NewVectorAuthority(), Count: 1, Bindings: map[string]any{},
				Contexts: []context.Context{context.Background()}, VMNative: true,
				Boundary: boundary, Lifecycle: lifecycle,
			},
		})
	}()
	if recovered == nil {
		t.Fatal("run did not panic")
	}
	if got, want := strings.Join(lifecycle.calls, ","), "begin:0,abort:0"; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
	if lifecycle.abortErr == nil {
		t.Fatal("Abort received a nil error")
	}
}

func TestTemplateVMNativeVectorBoundaryMatchesLegacy(t *testing.T) {
	const count = 3
	values := []string{"first", "second", "third"}
	contexts := vectorBackgroundContexts(count)
	indices := []int{0, 1, 2}

	legacyAuthority := native.NewVectorAuthority()
	legacyController := &vectorTestController{authority: legacyAuthority}
	legacyTemplate, err := BuildTemplate(Files{
		"index.txt": []byte(`{% for index := range indices %}{% controller.Begin(index) %}{{ value }};{% controller.End(index) %}{% end %}`),
	}, "index.txt", &BuildOptions{Globals: vectorTestGlobals((**vectorTestController)(nil))})
	if err != nil {
		t.Fatalf("build legacy: %v", err)
	}
	var legacyOutput strings.Builder
	err = legacyTemplate.Run(&legacyOutput, map[string]any{
		"controller": legacyController,
		"indices":    indices,
	}, &RunOptions{Vector: &VectorRunOptions{
		Authority: legacyAuthority,
		Count:     count,
		Bindings:  map[string]any{"value": values},
		Contexts:  contexts,
	}})
	if err != nil {
		t.Fatalf("legacy run: %v", err)
	}

	boundary := native.NewVectorBoundary()
	directTemplate, err := BuildTemplate(Files{
		"index.txt": []byte(`{% for index := range indices %}{% boundary.Begin(index) %}{{ value }};{% boundary.End(index) %}{% end %}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"boundary": native.Synchronous((**native.VectorBoundary)(nil), "Begin", "End"),
		"indices":  (*[]int)(nil),
		"value":    (*string)(nil),
	}})
	if err != nil {
		t.Fatalf("build direct: %v", err)
	}
	lifecycle := &vectorVMNativeLifecycle{abortIndex: -1}
	var directOutput strings.Builder
	err = directTemplate.Run(&directOutput, map[string]any{
		"boundary": boundary,
		"indices":  indices,
	}, &RunOptions{Vector: &VectorRunOptions{
		Authority: native.NewVectorAuthority(),
		Count:     count,
		Bindings:  map[string]any{"value": values},
		Contexts:  contexts,
		VMNative:  true,
		Boundary:  boundary,
		Lifecycle: lifecycle,
	}})
	if err != nil {
		t.Fatalf("direct run: %v", err)
	}
	if directOutput.String() != legacyOutput.String() {
		t.Fatalf("direct output = %q, legacy = %q", directOutput.String(), legacyOutput.String())
	}
	if got, want := strings.Join(lifecycle.calls, ","),
		"begin:0,finish:0,commit:0,begin:1,finish:1,commit:1,begin:2,finish:2,commit:2"; got != want {
		t.Fatalf("direct calls = %q, want %q", got, want)
	}
}

func vectorBackgroundContexts(count int) []context.Context {
	contexts := make([]context.Context, count)
	for index := range contexts {
		contexts[index] = context.Background()
	}
	return contexts
}

func TestTemplateVectorRunOwnsExactAndPointerColumns(t *testing.T) {
	globals := vectorTestGlobals((**vectorTestController)(nil))
	globals["record"] = (*vectorPointerValue)(nil)
	template, err := BuildTemplate(Files{
		"wrapper.txt": []byte(`{% for index := range indices %}{% controller.Begin(index) %}{{ value }}:{{ record.Text }};{% controller.End(index) %}{% end %}`),
	}, "wrapper.txt", &BuildOptions{Globals: globals})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	authority := native.NewVectorAuthority()
	values := []string{"first", "second"}
	records := []*vectorPointerValue{{Text: "one"}, {Text: "two"}}
	controller := &vectorTestController{authority: authority}
	controller.onBegin = func(index int) {
		if index == 0 {
			values[1] = "poison"
			records[1] = &vectorPointerValue{Text: "poison"}
		}
	}
	var output strings.Builder
	err = template.Run(&output, map[string]any{
		"controller": controller,
		"indices":    []int{0, 1},
	}, &RunOptions{Vector: &VectorRunOptions{
		Authority: authority,
		Count:     2,
		Bindings: map[string]any{
			"record": records,
			"value":  values,
		},
		Contexts: []context.Context{context.Background(), context.Background()},
	}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := output.String(), "first:one;second:two;"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestTemplateVectorRunRejectsEmptyAndNilPointerColumns(t *testing.T) {
	globals := vectorTestGlobals((**vectorTestController)(nil))
	globals["record"] = (*vectorPointerValue)(nil)
	template, err := BuildTemplate(Files{
		"wrapper.txt": []byte(`{% controller.Begin(0) %}{{ record.Text }}{% controller.End(0) %}`),
	}, "wrapper.txt", &BuildOptions{Globals: globals})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	err = template.Run(io.Discard, map[string]any{
		"controller": &vectorTestController{authority: native.NewVectorAuthority()},
		"indices":    []int{},
	}, &RunOptions{Vector: &VectorRunOptions{
		Authority: native.NewVectorAuthority(),
		Count:     0,
		Bindings:  map[string]any{"record": []*vectorPointerValue{}},
		Contexts:  []context.Context{},
	}})
	if !errors.Is(err, ErrVectorInvalidConfiguration) {
		t.Fatalf("empty error = %v, want %v", err, ErrVectorInvalidConfiguration)
	}

	authority := native.NewVectorAuthority()
	controller := &vectorTestController{authority: authority}
	err = template.Run(io.Discard, map[string]any{
		"controller": controller,
		"indices":    []int{0},
	}, &RunOptions{Vector: &VectorRunOptions{
		Authority: authority,
		Count:     1,
		Bindings:  map[string]any{"record": []*vectorPointerValue{nil}},
		Contexts:  []context.Context{context.Background()},
	}})
	if !errors.Is(err, ErrVectorInvalidConfiguration) {
		t.Fatalf("nil pointer error = %v, want %v", err, ErrVectorInvalidConfiguration)
	}
	if len(controller.calls) != 0 {
		t.Fatalf("controller calls = %v, want none", controller.calls)
	}
}

func TestTemplateVectorRunPrevalidatesColumnsBeforeExecution(t *testing.T) {
	template, err := BuildTemplate(Files{
		"index.txt": []byte(`{% controller.Begin(0) %}{{ value }}{% controller.End(0) %}`),
	}, "index.txt", &BuildOptions{Globals: vectorTestGlobals((**vectorTestController)(nil))})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	authority := native.NewVectorAuthority()
	controller := &vectorTestController{authority: authority}
	err = template.Run(io.Discard, map[string]any{
		"controller": controller,
		"indices":    []int{0},
	}, &RunOptions{Vector: &VectorRunOptions{
		Authority: authority,
		Count:     1,
		Bindings:  map[string]any{"value": []int{1}},
		Contexts:  []context.Context{context.Background()},
	}})
	if !errors.Is(err, ErrVectorInvalidConfiguration) {
		t.Fatalf("error = %v, want %v", err, ErrVectorInvalidConfiguration)
	}
	if len(controller.calls) != 0 {
		t.Fatalf("controller calls = %v, want none", controller.calls)
	}
}

func TestTemplateVectorRunRevokesRetainedCallable(t *testing.T) {
	var retained func() string
	retainOrInvoke := func(value string, callback func() string) string {
		if retained == nil {
			retained = callback
			return value
		}
		return retained()
	}
	globals := vectorTestGlobals((**vectorTestController)(nil))
	globals["retainOrInvoke"] = native.Synchronous(retainOrInvoke)
	template, err := BuildTemplate(Files{
		"wrapper.txt": []byte(`{% for index := range indices %}{% controller.Begin(index) %}{{ render "entry.txt" }}{% controller.End(index) %}{% end %}`),
		"entry.txt":   []byte(`{{ retainOrInvoke(value, func() string { return value }) }}`),
	}, "wrapper.txt", &BuildOptions{Globals: globals})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	authority := native.NewVectorAuthority()
	controller := &vectorTestController{authority: authority}
	err = template.Run(io.Discard, map[string]any{
		"controller": controller,
		"indices":    []int{0, 1},
	}, &RunOptions{Vector: &VectorRunOptions{
		Authority: authority,
		Count:     2,
		Bindings:  map[string]any{"value": []string{"first", "second"}},
		Contexts:  []context.Context{context.Background(), context.Background()},
		VMNative:  true,
	}})
	if !errors.Is(err, ErrVectorGenerationRevoked) {
		t.Fatalf("error = %v, want %v", err, ErrVectorGenerationRevoked)
	}
	if got, want := strings.Join(controller.calls, ","), "begin:0,end:0,begin:1"; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestTemplateVectorRunRevokesRetainedNativeMethodValue(t *testing.T) {
	var retained func() string
	retainOrInvoke := func(callback func() string) string {
		if retained == nil {
			retained = callback
			return "first"
		}
		return retained()
	}
	globals := vectorTestGlobals((**vectorTestController)(nil))
	globals["methodValue"] = native.Synchronous((**vectorMethodValue)(nil), "Read")
	globals["retainOrInvoke"] = native.Synchronous(retainOrInvoke)
	template, err := BuildTemplate(Files{
		"wrapper.txt": []byte(`{% for index := range indices %}{% controller.Begin(index) %}{{ render "entry.txt" }}{% controller.End(index) %}{% end %}`),
		"entry.txt":   []byte(`{{ retainOrInvoke(methodValue.Read) }}`),
	}, "wrapper.txt", &BuildOptions{Globals: globals})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	authority := native.NewVectorAuthority()
	controller := &vectorTestController{authority: authority}
	err = template.Run(io.Discard, map[string]any{
		"controller": controller,
		"indices":    []int{0, 1},
	}, &RunOptions{Vector: &VectorRunOptions{
		Authority: authority,
		Count:     2,
		Bindings: map[string]any{
			"methodValue": []*vectorMethodValue{{Text: "first"}, {Text: "second"}},
		},
		Contexts: []context.Context{context.Background(), context.Background()},
		VMNative: true,
	}})
	if !errors.Is(err, ErrVectorGenerationRevoked) {
		t.Fatalf("error = %v, want %v", err, ErrVectorGenerationRevoked)
	}
	if got, want := strings.Join(controller.calls, ","), "begin:0,end:0,begin:1"; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestTemplateVectorRunRejectsSharedBindingOverlap(t *testing.T) {
	template, err := BuildTemplate(Files{"index.txt": []byte(`{{ value }}`)}, "index.txt", &BuildOptions{
		Globals: native.Declarations{"value": (*string)(nil)},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	err = template.Run(io.Discard, map[string]any{"value": "fixed"}, &RunOptions{
		Vector: &VectorRunOptions{
			Authority: native.NewVectorAuthority(),
			Count:     1,
			Bindings:  map[string]any{"value": []string{"column"}},
			Contexts:  []context.Context{context.Background()},
		},
	})
	if !errors.Is(err, ErrVectorInvalidConfiguration) {
		t.Fatalf("error = %v, want %v", err, ErrVectorInvalidConfiguration)
	}
}

func TestTemplateVectorRunUsesAndRevokesIndexedContexts(t *testing.T) {
	type contextKey struct{}
	var retainedEnv native.Env
	var retainedContext context.Context
	var staleValue any
	var staleErr error
	readContext := func(env native.Env) string {
		if retainedEnv == nil {
			retainedEnv = env
			retainedContext = env.Context()
		} else {
			staleValue = retainedEnv.Context().Value(contextKey{})
			staleErr = retainedContext.Err()
		}
		return env.Context().Value(contextKey{}).(string)
	}
	globals := vectorTestGlobals((**vectorTestController)(nil))
	globals["readContext"] = native.Synchronous(readContext)
	template, err := BuildTemplate(Files{
		"wrapper.txt": []byte(`{% for index := range indices %}{% controller.Begin(index) %}{{ render "entry.txt" }}{% controller.End(index) %}{% end %}`),
		"entry.txt":   []byte(`{{ readContext() }}`),
	}, "wrapper.txt", &BuildOptions{Globals: globals})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	authority := native.NewVectorAuthority()
	controller := &vectorTestController{authority: authority}
	var output strings.Builder
	err = template.Run(&output, map[string]any{
		"controller": controller,
		"indices":    []int{0, 1},
	}, &RunOptions{
		Context: context.WithValue(context.Background(), contextKey{}, "base"),
		Vector: &VectorRunOptions{
			Authority: authority,
			Count:     2,
			Bindings:  map[string]any{},
			Contexts: []context.Context{
				context.WithValue(context.Background(), contextKey{}, "first"),
				context.WithValue(context.Background(), contextKey{}, "second"),
			},
		},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := output.String(), "firstsecond"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if staleValue != nil {
		t.Fatalf("retained environment exposed value %v", staleValue)
	}
	if !errors.Is(staleErr, ErrVectorGenerationRevoked) {
		t.Fatalf("retained context error = %v", staleErr)
	}
}

func TestTemplateVectorRunPrevalidatesIndexedContexts(t *testing.T) {
	template, err := BuildTemplate(Files{
		"index.txt": []byte(`{% controller.Begin(0) %}{{ value }}{% controller.End(0) %}`),
	}, "index.txt", &BuildOptions{Globals: vectorTestGlobals((**vectorTestController)(nil))})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	authority := native.NewVectorAuthority()
	controller := &vectorTestController{authority: authority}
	err = template.Run(io.Discard, map[string]any{
		"controller": controller,
		"indices":    []int{0},
	}, &RunOptions{Vector: &VectorRunOptions{
		Authority: authority,
		Count:     1,
		Bindings:  map[string]any{"value": []string{"value"}},
		Contexts:  []context.Context{nil},
	}})
	if !errors.Is(err, ErrVectorInvalidConfiguration) {
		t.Fatalf("error = %v, want %v", err, ErrVectorInvalidConfiguration)
	}
	if len(controller.calls) != 0 {
		t.Fatalf("controller calls = %v, want none", controller.calls)
	}
}

func TestTemplateVectorRunReinitializesRenderedPackageVariables(t *testing.T) {
	tests := []struct {
		name  string
		files Files
	}{
		{
			name:  "scalar",
			files: Files{"entry.txt": []byte(`{% var Counter int %}{% Counter++ %}{{ Counter }}`)},
		},
		{
			name:  "map",
			files: Files{"entry.txt": []byte(`{% var State = map[string]int{"count": 0} %}{% State["count"]++ %}{{ State["count"] }}`)},
		},
		{
			name:  "slice",
			files: Files{"entry.txt": []byte(`{% var State = []int{0} %}{% State[0]++ %}{{ State[0] }}`)},
		},
		{
			name: "imported macro",
			files: Files{
				"entry.txt":   []byte(`{% import "library.txt" for Next %}{{ Next() }}`),
				"library.txt": []byte(`{% var Counter int %}{% macro Next %}{% Counter++ %}{{ Counter }}{% end %}`),
			},
		},
		{
			name: "captured package variable",
			files: Files{
				"entry.txt": []byte(`{% var Counter int %}{% var Next = func() int { Counter++; return Counter } %}{{ Next() }}`),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.files["wrapper.txt"] = []byte(`{% for index := range indices %}{% controller.Begin(index) %}{{ render "entry.txt" }}{% controller.End(index) %}{% end %}`)
			template, err := BuildTemplate(
				test.files,
				"wrapper.txt",
				&BuildOptions{Globals: vectorTestGlobals((**vectorTestController)(nil))},
			)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			authority := native.NewVectorAuthority()
			controller := &vectorTestController{authority: authority}
			var output strings.Builder
			err = template.Run(&output, map[string]any{
				"controller": controller,
				"indices":    []int{0, 1},
			}, &RunOptions{Vector: &VectorRunOptions{
				Authority: authority,
				Count:     2,
				Bindings:  map[string]any{},
				Contexts:  []context.Context{context.Background(), context.Background()},
			}})
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if got, want := output.String(), "11"; got != want {
				t.Fatalf("output = %q, want %q", got, want)
			}
		})
	}
}

func TestTemplateIsolatedRootRendersReinitializeSharedImportedState(t *testing.T) {
	tests := []struct {
		name    string
		library string
	}{
		{
			name:    "scalar",
			library: `{% var Counter int %}{% macro Next %}{% Counter++ %}{{ Counter }}{% end %}`,
		},
		{
			name:    "map",
			library: `{% var State = map[string]int{"count": 0} %}{% macro Next %}{% State["count"]++ %}{{ State["count"] }}{% end %}`,
		},
		{
			name:    "slice",
			library: `{% var State = []int{0} %}{% macro Next %}{% State[0]++ %}{{ State[0] }}{% end %}`,
		},
		{
			name:    "captured variable",
			library: `{% var Counter int %}{% var Next = func() int { Counter++; return Counter } %}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			template, err := BuildTemplate(Files{
				"wrapper.txt": []byte(`{{ render "a.txt" }}{{ render "b.txt" }}`),
				"a.txt":       []byte(`{% import "library.txt" for Next %}{{ Next() }}`),
				"b.txt":       []byte(`{% import "library.txt" for Next %}{{ Next() }}`),
				"library.txt": []byte(test.library),
			}, "wrapper.txt", &BuildOptions{IsolateRootRenderState: true})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var output strings.Builder
			if err := template.Run(&output, nil, nil); err != nil {
				t.Fatalf("run: %v", err)
			}
			if got, want := output.String(), "11"; got != want {
				t.Fatalf("output = %q, want %q", got, want)
			}
		})
	}
}

func TestTemplateRootRenderStateIsolationIsOptIn(t *testing.T) {
	template, err := BuildTemplate(Files{
		"wrapper.txt": []byte(`{{ render "a.txt" }}{{ render "b.txt" }}`),
		"a.txt":       []byte(`{% import "library.txt" for Next %}{{ Next() }}`),
		"b.txt":       []byte(`{% import "library.txt" for Next %}{{ Next() }}`),
		"library.txt": []byte(`{% var Counter int %}{% macro Next %}{% Counter++ %}{{ Counter }}{% end %}`),
	}, "wrapper.txt", nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var output strings.Builder
	if err := template.Run(&output, nil, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := output.String(), "12"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestTemplateIsolatedRootRenderKeepsNestedState(t *testing.T) {
	template, err := BuildTemplate(Files{
		"wrapper.txt": []byte(`{{ render "entry.txt" }}`),
		"entry.txt":   []byte(`{% import "library.txt" for Next %}{{ Next() }}{{ render "child.txt" }}`),
		"child.txt":   []byte(`{% import "library.txt" for Next %}{{ Next() }}`),
		"library.txt": []byte(`{% var Counter int %}{% macro Next %}{% Counter++ %}{{ Counter }}{% end %}`),
	}, "wrapper.txt", &BuildOptions{IsolateRootRenderState: true})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var output strings.Builder
	if err := template.Run(&output, nil, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := output.String(), "12"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestTemplateIsolatedRootRenderRejectsNestedTargetReuse(t *testing.T) {
	_, err := BuildTemplate(Files{
		"wrapper.txt": []byte(`{{ render "a.txt" }}{{ render "b.txt" }}`),
		"a.txt":       []byte(`{{ render "b.txt" }}`),
		"b.txt":       []byte(`value`),
	}, "wrapper.txt", &BuildOptions{IsolateRootRenderState: true})
	if err == nil || !strings.Contains(err.Error(), "also rendered from a child template") {
		t.Fatalf("error = %v, want nested target rejection", err)
	}
}

func TestTemplateUsedVariablesIdentifyNativeAndTemplateSlots(t *testing.T) {
	template, err := BuildTemplate(Files{
		"index.txt":   []byte(`{% import "library.txt" for Value %}{{ external }}{{ Value() }}`),
		"library.txt": []byte(`{% var Counter int %}{% macro Value %}{{ Counter }}{% end %}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{"external": (*string)(nil)}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	want := []UsedVariable{
		{Path: "library.txt", Name: "Counter"},
		{
			Package:          "main",
			Name:             "external",
			Native:           true,
			FunctionPath:     "index.txt",
			FunctionName:     "@root",
			FunctionPosition: ast.Position{Line: 1, Column: 1},
		},
	}
	if got := template.UsedVariables(); !reflect.DeepEqual(got, want) {
		t.Fatalf("variables = %#v, want %#v", got, want)
	}
}

func TestTemplateUsedVariablesIdentifyRootRenderScopes(t *testing.T) {
	template, err := BuildTemplate(Files{
		"wrapper.txt": []byte(`{{ external }}{{ render "a.txt" }}{{ render "b.txt" }}`),
		"a.txt":       []byte(`{{ external }}`),
		"b.txt":       []byte(`{{ external }}`),
	}, "wrapper.txt", &BuildOptions{
		Globals:                native.Declarations{"external": (*string)(nil)},
		IsolateRootRenderState: true,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	variables := template.UsedVariables()
	paths := make([]string, 0, len(variables))
	for _, variable := range variables {
		if variable.Name != "external" || !variable.Native || variable.FunctionName != "@root" {
			t.Fatalf("variable = %#v, want an authenticated root external slot", variable)
		}
		paths = append(paths, variable.FunctionPath)
	}
	if got, want := paths, []string{"a.txt", "b.txt", "wrapper.txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("function paths = %v, want %v", got, want)
	}
}

func TestTemplateVectorRunContinueStaysInRenderedMacro(t *testing.T) {
	template, err := BuildTemplate(Files{
		"wrapper.txt": []byte(`{% for index := range indices %}{% controller.Begin(index) %}{{ render "entry.txt" }}{% controller.End(index) %}{% end %}`),
		"entry.txt":   []byte(`{% import "library.txt" for Values %}{{ Values() }}`),
		"library.txt": []byte(`{% macro Values %}{%% values := []string{"", "kept"}; for i := 0; i < len(values); i++ { if values[i] == "" { continue }; show values[i] } %%}{% end %}`),
	}, "wrapper.txt", &BuildOptions{Globals: vectorTestGlobals((**vectorTestController)(nil))})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	authority := native.NewVectorAuthority()
	controller := &vectorTestController{authority: authority}
	var output strings.Builder
	err = template.Run(&output, map[string]any{
		"controller": controller,
		"indices":    []int{0, 1},
	}, &RunOptions{Vector: &VectorRunOptions{
		Authority: authority,
		Count:     2,
		Bindings:  map[string]any{},
		Contexts:  []context.Context{context.Background(), context.Background()},
	}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := output.String(), "keptkept"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if got, want := strings.Join(controller.calls, ","), "begin:0,end:0,begin:1,end:1"; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestTemplateVectorRunBreakStaysInRenderedMacro(t *testing.T) {
	template, err := BuildTemplate(Files{
		"wrapper.txt": []byte(`{% for index := 0; index < len(indices); index++ %}{% controller.Begin(index) %}{{ render "entry.txt" }}{% controller.End(index) %}{% end %}`),
		"entry.txt":   []byte(`{% import "library.txt" for Value %}{{ Value() }}`),
		"library.txt": []byte(`{% macro Value %}{%% for _, value := range []string{"kept", "discarded"} { show value; break } %%}{% end %}`),
	}, "wrapper.txt", &BuildOptions{Globals: vectorTestGlobals((**vectorTestController)(nil))})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	authority := native.NewVectorAuthority()
	controller := &vectorTestController{authority: authority}
	var output strings.Builder
	err = template.Run(&output, map[string]any{
		"controller": controller,
		"indices":    []int{0, 1},
	}, &RunOptions{Vector: &VectorRunOptions{
		Authority: authority,
		Count:     2,
		Bindings:  map[string]any{},
		Contexts:  []context.Context{context.Background(), context.Background()},
	}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := output.String(), "keptkept"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if got, want := strings.Join(controller.calls, ","), "begin:0,end:0,begin:1,end:1"; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func BenchmarkTemplateVectorItemBoundary(b *testing.B) {
	const count = 128
	indices := make([]int, count)
	values := make([]string, count)
	for index := range count {
		indices[index] = index
		values[index] = "x"
	}
	contexts := vectorBackgroundContexts(count)

	legacyAuthority := native.NewVectorAuthority()
	legacyTemplate, err := BuildTemplate(Files{
		"index.txt": []byte(`{% for index := range indices %}{% controller.Begin(index) %}{{ value }}{% controller.End(index) %}{% end %}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"controller": native.Synchronous((**vectorBenchmarkController)(nil), "Begin", "End"),
		"indices":    (*[]int)(nil),
		"value":      (*string)(nil),
	}})
	if err != nil {
		b.Fatalf("build legacy: %v", err)
	}
	legacyController := &vectorBenchmarkController{authority: legacyAuthority}
	legacyGlobals := map[string]any{"controller": legacyController, "indices": indices}
	legacyOptions := &RunOptions{Vector: &VectorRunOptions{
		Authority: legacyAuthority,
		Count:     count,
		Bindings:  map[string]any{"value": values},
		Contexts:  contexts,
	}}

	boundary := native.NewVectorBoundary()
	directTemplate, err := BuildTemplate(Files{
		"index.txt": []byte(`{% for index := range indices %}{% boundary.Begin(index) %}{{ value }}{% boundary.End(index) %}{% end %}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"boundary": native.Synchronous((**native.VectorBoundary)(nil), "Begin", "End"),
		"indices":  (*[]int)(nil),
		"value":    (*string)(nil),
	}})
	if err != nil {
		b.Fatalf("build direct: %v", err)
	}
	directGlobals := map[string]any{"boundary": boundary, "indices": indices}
	directOptions := &RunOptions{Vector: &VectorRunOptions{
		Authority: native.NewVectorAuthority(),
		Count:     count,
		Bindings:  map[string]any{"value": values},
		Contexts:  contexts,
		VMNative:  true,
		Boundary:  boundary,
		Lifecycle: &vectorBenchmarkLifecycle{},
	}}

	benchmarks := []struct {
		name     string
		template *Template
		globals  map[string]any
		options  *RunOptions
	}{
		{name: "reflective", template: legacyTemplate, globals: legacyGlobals, options: legacyOptions},
		{name: "VM-native", template: directTemplate, globals: directGlobals, options: directOptions},
	}
	for _, benchmark := range benchmarks {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(count)
			for range b.N {
				if err := benchmark.template.Run(io.Discard, benchmark.globals, benchmark.options); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
