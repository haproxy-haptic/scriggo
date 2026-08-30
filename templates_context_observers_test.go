// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"gitlab.com/haproxy-haptic/scriggo/native"
)

type contextObserverTestKey struct{}

type contextObserverMethod struct {
	calls int
}

func (method *contextObserverMethod) Invoke() string {
	method.calls++
	return "invoked"
}

func TestBeforeNativeCallContextRejectsBeforeBody(t *testing.T) {
	var bodyCalls int
	template, err := BuildTemplate(Files{"index.txt": []byte(`{{ call() }}`)}, "index.txt", &BuildOptions{
		Globals: native.Declarations{"call": func() string {
			bodyCalls++
			return "body"
		}},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	marker := &struct{}{}
	ctx := context.WithValue(t.Context(), contextObserverTestKey{}, marker)
	hookCalls := 0
	err = template.Run(io.Discard, nil, &RunOptions{
		Context: ctx,
		BeforeNativeCallContext: func(actual context.Context) error {
			hookCalls++
			if actual.Value(contextObserverTestKey{}) != marker {
				t.Fatal("context hook received the wrong execution context")
			}
			return errNativeCallPreflight
		},
	})
	if !errors.Is(err, errNativeCallPreflight) {
		t.Fatalf("run error = %v, want sentinel", err)
	}
	if hookCalls != 1 || bodyCalls != 0 {
		t.Fatalf("calls = hooks %d, bodies %d; want 1, 0", hookCalls, bodyCalls)
	}
}

func TestNativeCallContextObserverPreservesReceiverAndLocation(t *testing.T) {
	method := &contextObserverMethod{}
	template, err := BuildTemplate(Files{"index.txt": []byte(`{{ method.Invoke() }}`)}, "index.txt", &BuildOptions{
		Globals: native.Declarations{"method": &method},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	marker := &struct{}{}
	ctx := context.WithValue(t.Context(), contextObserverTestKey{}, marker)
	observerCalls := 0
	err = template.Run(io.Discard, nil, &RunOptions{
		Context: ctx,
		ObserveNativeCallContext: func(actual context.Context, call NativeCall) error {
			observerCalls++
			if actual.Value(contextObserverTestKey{}) != marker {
				t.Fatal("context observer received the wrong execution context")
			}
			if mutationTargetIdentity(call.Receiver) != reflect.ValueOf(method).Pointer() ||
				call.Method != "Invoke" || call.MethodValue || call.Path != "index.txt" || call.Line != 1 {
				t.Fatalf("call = %#v, want direct Invoke at index.txt:1", call)
			}
			return errNativeCallRejected
		},
	})
	if !errors.Is(err, errNativeCallRejected) {
		t.Fatalf("run error = %v, want sentinel", err)
	}
	if observerCalls != 1 || method.calls != 0 {
		t.Fatalf("calls = observers %d, methods %d; want 1, 0", observerCalls, method.calls)
	}
}

func TestMutationContextObserverRejectsBeforeMutation(t *testing.T) {
	value := map[string]string{"key": "original"}
	template, err := BuildTemplate(Files{"index.txt": []byte(`{% value["key"] = "changed" %}`)}, "index.txt", &BuildOptions{
		Globals: native.Declarations{"value": &value},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	marker := &struct{}{}
	ctx := context.WithValue(t.Context(), contextObserverTestKey{}, marker)
	observerCalls := 0
	err = template.Run(io.Discard, nil, &RunOptions{
		Context: ctx,
		ObserveMutationContext: func(actual context.Context, mutation Mutation) error {
			observerCalls++
			if actual.Value(contextObserverTestKey{}) != marker {
				t.Fatal("mutation observer received the wrong execution context")
			}
			if mutation.Operation != MutationSetMapIndex || mutation.Target.Pointer() != reflect.ValueOf(value).Pointer() ||
				mutation.Path != "index.txt" || mutation.Line != 1 {
				t.Fatalf("mutation = %#v, want map assignment at index.txt:1", mutation)
			}
			return errNativeCallRejected
		},
	})
	if !errors.Is(err, errNativeCallRejected) {
		t.Fatalf("run error = %v, want sentinel", err)
	}
	if observerCalls != 1 || value["key"] != "original" {
		t.Fatalf("effects = observers %d, value %q; want 1, original", observerCalls, value["key"])
	}
}

func TestContextObserversRunBeforeEnvironmentObservers(t *testing.T) {
	method := &contextObserverMethod{}
	value := map[string]string{"key": "original"}
	template, err := BuildTemplate(Files{"index.txt": []byte(`{{ method.Invoke() }}{% value["key"] = "changed" %}`)}, "index.txt", &BuildOptions{
		Globals: native.Declarations{"method": &method, "value": &value},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	marker := &struct{}{}
	ctx := context.WithValue(t.Context(), contextObserverTestKey{}, marker)
	var calls []string
	err = template.Run(io.Discard, nil, &RunOptions{
		Context: ctx,
		BeforeNativeCallContext: func(actual context.Context) error {
			if actual.Value(contextObserverTestKey{}) != marker {
				t.Fatal("preflight context does not match the execution")
			}
			calls = append(calls, "before-context")
			return nil
		},
		BeforeNativeCall: func(env native.Env) error {
			if env.Context().Value(contextObserverTestKey{}) != marker || env.CallPath() != "index.txt" || env.CallLine() != 1 {
				t.Fatalf("preflight environment = %s:%d", env.CallPath(), env.CallLine())
			}
			calls = append(calls, "before-environment")
			return nil
		},
		ObserveNativeCallContext: func(actual context.Context, _ NativeCall) error {
			if actual.Value(contextObserverTestKey{}) != marker {
				t.Fatal("native call context does not match the execution")
			}
			calls = append(calls, "native-context")
			return nil
		},
		ObserveNativeCall: func(env native.Env, _ NativeCall) error {
			if env.Context().Value(contextObserverTestKey{}) != marker {
				t.Fatal("native call environment does not match the execution")
			}
			calls = append(calls, "native-environment")
			return nil
		},
		ObserveMutationContext: func(actual context.Context, _ Mutation) error {
			if actual.Value(contextObserverTestKey{}) != marker {
				t.Fatal("mutation context does not match the execution")
			}
			calls = append(calls, "mutation-context")
			return nil
		},
		ObserveMutation: func(env native.Env, _ Mutation) error {
			if env.Context().Value(contextObserverTestKey{}) != marker {
				t.Fatal("mutation environment does not match the execution")
			}
			calls = append(calls, "mutation-environment")
			return errNativeCallRejected
		},
	})
	if !errors.Is(err, errNativeCallRejected) {
		t.Fatalf("run error = %v, want sentinel", err)
	}
	want := []string{
		"before-context", "before-environment", "native-context", "native-environment",
		"mutation-context", "mutation-environment",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("observer calls = %v, want %v", calls, want)
	}
	if method.calls != 1 || value["key"] != "original" {
		t.Fatalf("effects = method %d, value %q; want 1, original", method.calls, value["key"])
	}
}

func TestMutationContextObserverPreservesDetachedAndChannelSafety(t *testing.T) {
	t.Run("detached go", func(t *testing.T) {
		value := map[string]string{"key": "original"}
		started := false
		template, err := BuildTemplate(Files{"index.txt": []byte(`{% go func() { start(); value["key"] = "changed" }() %}`)}, "index.txt", &BuildOptions{
			AllowGoStmt: true,
			Globals: native.Declarations{
				"start": func() { started = true },
				"value": &value,
			},
		})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		err = template.Run(io.Discard, nil, &RunOptions{
			ObserveMutationContext: func(context.Context, Mutation) error { return nil },
		})
		if !errors.Is(err, ErrMutationObserverDetachedGo) {
			t.Fatalf("run error = %v, want %v", err, ErrMutationObserverDetachedGo)
		}
		if started || value["key"] != "original" {
			t.Fatalf("effects = started %t, value %q; want false, original", started, value["key"])
		}
	})

	t.Run("channel send", func(t *testing.T) {
		channel := make(chan int, 1)
		template, err := BuildTemplate(Files{"index.txt": []byte(`{% channel <- 1 %}`)}, "index.txt", &BuildOptions{
			Globals: native.Declarations{"channel": &channel},
		})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		var observed Mutation
		err = template.Run(io.Discard, nil, &RunOptions{
			ObserveMutationContext: func(_ context.Context, mutation Mutation) error {
				observed = mutation
				return nil
			},
		})
		if !errors.Is(err, ErrMutationObserverChannelOperation) {
			t.Fatalf("run error = %v, want %v", err, ErrMutationObserverChannelOperation)
		}
		if observed.Operation != MutationSendChannel || mutationTargetIdentity(observed.Target) != reflect.ValueOf(channel).Pointer() {
			t.Fatalf("mutation = %#v, want channel send", observed)
		}
		if len(channel) != 0 {
			t.Fatal("channel send ran")
		}
	})
}

func BenchmarkTemplateVectorContextHooks(b *testing.B) {
	const itemCount = 64
	const callsPerItem = 8
	globals := vectorTestGlobals((**vectorTestController)(nil))
	globals["call"] = func(string) {}
	globals["iterations"] = (*[]int)(nil)
	template, err := BuildTemplate(Files{
		"wrapper.txt": []byte(`{% for index := range indices %}{% controller.Begin(index) %}{% for range iterations %}{% call(value) %}{% end %}{% controller.End(index) %}{% end %}`),
	}, "wrapper.txt", &BuildOptions{Globals: globals})
	if err != nil {
		b.Fatalf("build: %v", err)
	}
	indices := make([]int, itemCount)
	values := make([]string, itemCount)
	contexts := make([]context.Context, itemCount)
	for index := range itemCount {
		indices[index] = index
		values[index] = "value"
		contexts[index] = context.Background()
	}
	iterations := make([]int, callsPerItem)
	begin := native.MakeMethodTrampoline(
		reflect.TypeFor[*vectorTestController](),
		"Begin",
		func(args []reflect.Value) []reflect.Value {
			args[0].Interface().(*vectorTestController).Begin(args[1].Interface().(native.Env), int(args[2].Int()))
			return nil
		},
	)
	end := native.MakeMethodTrampoline(
		reflect.TypeFor[*vectorTestController](),
		"End",
		func(args []reflect.Value) []reflect.Value {
			args[0].Interface().(*vectorTestController).End(args[1].Interface().(native.Env), int(args[2].Int()))
			return nil
		},
	)
	for _, test := range []struct {
		name    string
		options func(*RunOptions)
	}{
		{name: "environment", options: func(options *RunOptions) {
			options.BeforeNativeCall = func(env native.Env) error {
				_ = env.Context()
				return nil
			}
		}},
		{name: "context", options: func(options *RunOptions) {
			options.BeforeNativeCallContext = func(ctx context.Context) error {
				_ = ctx
				return nil
			}
		}},
	} {
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				authority := native.NewVectorAuthority()
				controller := &vectorTestController{authority: authority}
				options := &RunOptions{
					NativeFunctionTrampolines: []*native.FunctionTrampoline{begin, end},
					Vector: &VectorRunOptions{
						Authority: authority,
						Count:     itemCount,
						Bindings:  map[string]any{"value": values},
						Contexts:  contexts,
					},
				}
				test.options(options)
				if runErr := template.Run(io.Discard, map[string]any{
					"controller": controller,
					"indices":    indices,
					"iterations": iterations,
				}, options); runErr != nil {
					b.Fatal(runErr)
				}
				if len(controller.calls) != itemCount*2 {
					b.Fatalf("controller calls = %d, want %d", len(controller.calls), itemCount*2)
				}
			}
		})
	}
}
