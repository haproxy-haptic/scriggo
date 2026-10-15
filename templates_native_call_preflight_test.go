// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/haproxy-haptic/scriggo/native"
)

var errNativeCallPreflight = errors.New("native call preflight rejected")

type nativeCallPreflightContextKey struct{}

type nativeCallPreflightMethod struct {
	bodyCalls *atomic.Int64
}

func (value *nativeCallPreflightMethod) Call() string {
	value.bodyCalls.Add(1)
	return "method"
}

type nativeCallPreflightField struct {
	Call func() string
}

type nativeCallPreflightVariadicRetainer interface {
	Fetch(...any) (any, error)
}

type nativeCallPreflightVariadicRetainerImpl struct {
	mu        sync.Mutex
	callbacks []func() string
	bodyCalls atomic.Int64
}

func (retainer *nativeCallPreflightVariadicRetainerImpl) Fetch(arguments ...any) (any, error) {
	retainer.bodyCalls.Add(1)
	if len(arguments) == 1 {
		if callback, ok := arguments[0].(func() string); ok {
			retainer.mu.Lock()
			retainer.callbacks = append(retainer.callbacks, callback)
			retainer.mu.Unlock()
			return "retained", nil
		}
	}
	return "body", nil
}

func (retainer *nativeCallPreflightVariadicRetainerImpl) callback(index int) func() string {
	retainer.mu.Lock()
	defer retainer.mu.Unlock()
	return retainer.callbacks[index]
}

func TestBeforeNativeCallRejectsEveryCallableFormBeforeItsBody(t *testing.T) {
	tests := []struct {
		name          string
		source        string
		declarations  func(*atomic.Int64) native.Declarations
		rejectCall    int64
		wantBodyCalls int64
	}{
		{
			name:   "direct function",
			source: `prefix{{ call() }}suffix`,
			declarations: func(bodyCalls *atomic.Int64) native.Declarations {
				return native.Declarations{"call": func() string {
					bodyCalls.Add(1)
					return "direct"
				}}
			},
			rejectCall: 1,
		},
		{
			name:   "bound method",
			source: `{% call := value.Call %}prefix{{ call() }}suffix`,
			declarations: func(bodyCalls *atomic.Int64) native.Declarations {
				value := &nativeCallPreflightMethod{bodyCalls: bodyCalls}
				return native.Declarations{"value": &value}
			},
			rejectCall: 1,
		},
		{
			name:   "function field",
			source: `prefix{{ value.Call() }}suffix`,
			declarations: func(bodyCalls *atomic.Int64) native.Declarations {
				value := nativeCallPreflightField{Call: func() string {
					bodyCalls.Add(1)
					return "field"
				}}
				return native.Declarations{"value": &value}
			},
			rejectCall: 1,
		},
		{
			name:   "indexed function",
			source: `prefix{{ values.call() }}suffix`,
			declarations: func(bodyCalls *atomic.Int64) native.Declarations {
				values := map[string]func() string{"call": func() string {
					bodyCalls.Add(1)
					return "indexed"
				}}
				return native.Declarations{"values": &values}
			},
			rejectCall: 1,
		},
		{
			name:   "function result",
			source: `prefix{{ factory()() }}suffix`,
			declarations: func(bodyCalls *atomic.Int64) native.Declarations {
				return native.Declarations{"factory": func() func() string {
					bodyCalls.Add(1)
					return func() string {
						bodyCalls.Add(1)
						return "result"
					}
				}}
			},
			rejectCall:    2,
			wantBodyCalls: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var bodyCalls atomic.Int64
			tmpl, err := BuildTemplate(
				Files{"index.txt": []byte(test.source)},
				"index.txt",
				&BuildOptions{Globals: test.declarations(&bodyCalls)},
			)
			if err != nil {
				t.Fatalf("build: %v", err)
			}

			marker := &struct{}{}
			ctx := context.WithValue(context.Background(), nativeCallPreflightContextKey{}, marker)
			var preflights atomic.Int64
			var out strings.Builder
			err = tmpl.Run(&out, nil, &RunOptions{
				Context: ctx,
				BeforeNativeCall: func(env native.Env) error {
					if got := env.Context().Value(nativeCallPreflightContextKey{}); got != marker {
						t.Fatalf("preflight context value = %v, want %p", got, marker)
					}
					if env.CallPath() != "index.txt" || env.CallLine() != 1 {
						t.Fatalf("preflight location = %s:%d, want index.txt:1", env.CallPath(), env.CallLine())
					}
					if preflights.Add(1) == test.rejectCall {
						return errNativeCallPreflight
					}
					return nil
				},
			})
			if err != errNativeCallPreflight {
				t.Fatalf("run error = %T %v, want sentinel", err, err)
			}
			if got := preflights.Load(); got != test.rejectCall {
				t.Fatalf("preflight calls = %d, want %d", got, test.rejectCall)
			}
			if got := bodyCalls.Load(); got != test.wantBodyCalls {
				t.Fatalf("native body calls = %d, want %d", got, test.wantBodyCalls)
			}
			if got := out.String(); got != "prefix" {
				t.Fatalf("output = %q, want prefix", got)
			}
		})
	}
}

func TestBeforeNativeCallRunsBeforeMethodReceiverObserver(t *testing.T) {
	var bodyCalls atomic.Int64
	value := &nativeCallPreflightMethod{bodyCalls: &bodyCalls}
	tmpl, err := BuildTemplate(
		Files{"index.txt": []byte(`{{ value.Call() }}`)},
		"index.txt",
		&BuildOptions{Globals: native.Declarations{"value": &value}},
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	observerCalls := 0
	err = tmpl.Run(&strings.Builder{}, nil, &RunOptions{
		BeforeNativeCall: func(native.Env) error { return errNativeCallPreflight },
		ObserveNativeCall: func(native.Env, NativeCall) error {
			observerCalls++
			return nil
		},
	})
	if err != errNativeCallPreflight {
		t.Fatalf("run error = %T %v, want sentinel", err, err)
	}
	if observerCalls != 0 || bodyCalls.Load() != 0 {
		t.Fatalf("effects: observers=%d bodies=%d, want none", observerCalls, bodyCalls.Load())
	}
}

func TestBeforeNativeCallAppliesToPrograms(t *testing.T) {
	var bodyCalls atomic.Int64
	program, err := Build(Files{
		"main.go": []byte("package main\nimport \"test/native\"\nfunc main() { native.Call() }\n"),
	}, &BuildOptions{Packages: native.Packages{
		"test/native": native.Package{
			Name: "native",
			Declarations: native.Declarations{"Call": func() {
				bodyCalls.Add(1)
			}},
		},
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	preflights := 0
	err = program.Run(&RunOptions{BeforeNativeCall: func(env native.Env) error {
		preflights++
		if env.CallPath() != "main" || env.CallLine() != 3 {
			t.Fatalf("preflight location = %s:%d, want main:3", env.CallPath(), env.CallLine())
		}
		return errNativeCallPreflight
	}})
	if err != errNativeCallPreflight {
		t.Fatalf("run error = %T %v, want sentinel", err, err)
	}
	if preflights != 1 || bodyCalls.Load() != 0 {
		t.Fatalf("effects: preflights=%d bodies=%d", preflights, bodyCalls.Load())
	}
}

func TestBeforeNativeCallRejectsNativeParallelWorkBeforeLaunch(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		allowGo bool
	}{
		{name: "detached go statement", source: `{% go call() %}`, allowGo: true},
		{name: "parallel render", source: `{{ go call() }}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var bodyCalls atomic.Int64
			tmpl, err := BuildTemplate(
				Files{"index.txt": []byte(test.source)},
				"index.txt",
				&BuildOptions{
					AllowGoStmt: test.allowGo,
					Globals: native.Declarations{"call": func() string {
						bodyCalls.Add(1)
						return "called"
					}},
				},
			)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var out strings.Builder
			err = tmpl.Run(&out, nil, &RunOptions{
				BeforeNativeCall: func(native.Env) error { return errNativeCallPreflight },
			})
			if err != errNativeCallPreflight {
				t.Fatalf("run error = %T %v, want sentinel", err, err)
			}
			if bodyCalls.Load() != 0 || out.Len() != 0 {
				t.Fatalf("effects: bodies=%d output=%q, want none", bodyCalls.Load(), out.String())
			}
		})
	}
}

type nativeCallPreflightLease struct {
	active atomic.Bool
}

func TestBeforeNativeCallBatchGenerationRejectsRetainedCallbacks(t *testing.T) {
	var retainedMu sync.Mutex
	var retained []func() string
	retain := func(call func() string) {
		retainedMu.Lock()
		retained = append(retained, call)
		retainedMu.Unlock()
	}
	var bodyCalls [2]atomic.Int64
	tmpl, err := BuildTemplate(
		Files{"index.txt": []byte(`{% retain(call) %}{{ call() }}`)},
		"index.txt",
		&BuildOptions{Globals: native.Declarations{
			"call":   native.Synchronous((*func() string)(nil)),
			"retain": native.Synchronous(retain),
		}},
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !tmpl.BatchSafe() {
		t.Fatalf("template is not batch safe: %#v", tmpl.UsedNativeCallables())
	}

	leases := [2]*nativeCallPreflightLease{{}, {}}
	for _, lease := range leases {
		lease.active.Store(true)
	}
	contexts := [2]context.Context{
		context.WithValue(context.Background(), nativeCallPreflightContextKey{}, leases[0]),
		context.WithValue(context.Background(), nativeCallPreflightContextKey{}, leases[1]),
	}
	preflight := func(env native.Env) error {
		lease, _ := env.Context().Value(nativeCallPreflightContextKey{}).(*nativeCallPreflightLease)
		if lease == nil || !lease.active.Load() {
			return errNativeCallPreflight
		}
		return nil
	}
	var outputs [2]strings.Builder
	err = tmpl.RunBatch([]BatchRun{
		{
			Out: &outputs[0],
			Vars: map[string]any{"call": func() string {
				bodyCalls[0].Add(1)
				return "first"
			}},
			Options: &RunOptions{Context: contexts[0], BeforeNativeCall: preflight},
			After:   func() { leases[0].active.Store(false) },
		},
		{
			Out: &outputs[1],
			Vars: map[string]any{"call": func() string {
				bodyCalls[1].Add(1)
				return "second"
			}},
			Before: func() error {
				retainedMu.Lock()
				first := retained[0]
				retainedMu.Unlock()
				assertNativeCallPreflightPanic(t, first)
				return nil
			},
			Options: &RunOptions{Context: contexts[1], BeforeNativeCall: preflight},
			After:   func() { leases[1].active.Store(false) },
		},
	})
	if err != nil {
		t.Fatalf("run batch: %v", err)
	}
	if outputs[0].String() != "first" || outputs[1].String() != "second" {
		t.Fatalf("outputs = %q, %q", outputs[0].String(), outputs[1].String())
	}
	if bodyCalls[0].Load() != 1 || bodyCalls[1].Load() != 1 {
		t.Fatalf("body calls = %d, %d, want one each", bodyCalls[0].Load(), bodyCalls[1].Load())
	}

	retainedMu.Lock()
	callbacks := append([]func() string(nil), retained...)
	retainedMu.Unlock()
	var wait sync.WaitGroup
	for range 32 {
		for _, callback := range callbacks {
			wait.Add(1)
			go func() {
				defer wait.Done()
				assertNativeCallPreflightPanic(t, callback)
			}()
		}
	}
	wait.Wait()
	if bodyCalls[0].Load() != 1 || bodyCalls[1].Load() != 1 {
		t.Fatalf("retained callback reached body: %d, %d", bodyCalls[0].Load(), bodyCalls[1].Load())
	}
}

func TestBeforeNativeCallBatchGenerationRejectsRetainedVariadicInterfaceCallback(t *testing.T) {
	tmpl, err := BuildTemplate(
		Files{"index.txt": []byte(`{% var _, _ = http.Fetch(func() string { var value, _ = http.Fetch("body"); return value.(string) }) %}`)},
		"index.txt",
		&BuildOptions{Globals: native.Declarations{
			"http": native.Synchronous((*nativeCallPreflightVariadicRetainer)(nil), "Fetch"),
		}},
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	retainer := &nativeCallPreflightVariadicRetainerImpl{}
	leases := [2]*nativeCallPreflightLease{{}, {}}
	for _, lease := range leases {
		lease.active.Store(true)
	}
	contexts := [2]context.Context{
		context.WithValue(context.Background(), nativeCallPreflightContextKey{}, leases[0]),
		context.WithValue(context.Background(), nativeCallPreflightContextKey{}, leases[1]),
	}
	preflight := func(env native.Env) error {
		lease, _ := env.Context().Value(nativeCallPreflightContextKey{}).(*nativeCallPreflightLease)
		if lease == nil || !lease.active.Load() {
			return errNativeCallPreflight
		}
		return nil
	}
	var crossGenerationPanic any
	err = tmpl.RunBatch([]BatchRun{
		{
			Out:     io.Discard,
			Vars:    map[string]any{"http": retainer},
			Options: &RunOptions{Context: contexts[0], BeforeNativeCall: preflight},
			After:   func() { leases[0].active.Store(false) },
		},
		{
			Out:  io.Discard,
			Vars: map[string]any{"http": retainer},
			Before: func() error {
				func() {
					defer func() { crossGenerationPanic = recover() }()
					retainer.callback(0)()
				}()
				return nil
			},
			Options: &RunOptions{Context: contexts[1], BeforeNativeCall: preflight},
			After:   func() { leases[1].active.Store(false) },
		},
	})
	if err != nil {
		t.Fatalf("run batch: %v", err)
	}
	if crossGenerationPanic == nil {
		t.Fatal("retained callback crossed into the next batch generation")
	}
	var afterBatchPanic any
	func() {
		defer func() { afterBatchPanic = recover() }()
		retainer.callback(0)()
	}()
	if afterBatchPanic == nil {
		t.Fatal("retained callback remained usable after the batch")
	}
	if got, want := retainer.bodyCalls.Load(), int64(2); got != want {
		t.Fatalf("native bodies = %d, want %d", got, want)
	}
}

func assertNativeCallPreflightPanic(t *testing.T, call func() string) {
	t.Helper()
	defer func() {
		panicValue := recover()
		if panicValue == nil {
			t.Error("retained callback did not reject the revoked execution")
		} else if got, want := fmt.Sprint(panicValue), "stop: "+errNativeCallPreflight.Error(); got != want {
			t.Errorf("retained callback panic = %q, want %q", got, want)
		}
	}()
	call()
}
