// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package native

import (
	"reflect"
	"runtime"
	"testing"
	"weak"
)

func TestFunctionTrampolineValueRetainsDirectImplementation(t *testing.T) {
	value, retained := func() (reflect.Value, weak.Pointer[FunctionTrampoline]) {
		trampoline := MakeFunctionTrampoline(
			reflect.TypeFor[func() int](),
			func([]reflect.Value) []reflect.Value {
				return []reflect.Value{reflect.ValueOf(42)}
			},
		)
		return trampoline.Value(), weak.Make(trampoline)
	}()

	runtime.GC()
	runtime.GC()
	trampoline := retained.Value()
	if trampoline == nil {
		t.Fatal("function value did not retain its trampoline")
	}
	if got := trampoline.Call(nil)[0].Int(); got != 42 {
		t.Fatalf("direct result = %d, want 42", got)
	}
	if got := value.Call(nil)[0].Int(); got != 42 {
		t.Fatalf("function result = %d, want 42", got)
	}
	runtime.KeepAlive(value)
}

func TestFunctionTrampolineForUsesExactTargetIdentity(t *testing.T) {
	originalCalls := 0
	target := func(value int) int {
		originalCalls++
		return value + 1
	}
	trampolineCalls := 0
	trampoline := MakeFunctionTrampolineFor(
		reflect.ValueOf(target),
		func(args []reflect.Value) []reflect.Value {
			trampolineCalls++
			return []reflect.Value{reflect.ValueOf(int(args[0].Int()) + 2)}
		},
	)

	if trampoline.Value() != reflect.ValueOf(target) {
		t.Fatal("trampoline target identity changed")
	}
	if got := trampoline.Call([]reflect.Value{reflect.ValueOf(40)})[0].Int(); got != 42 {
		t.Fatalf("direct result = %d, want 42", got)
	}
	if originalCalls != 0 || trampolineCalls != 1 {
		t.Fatalf("calls = original %d, trampoline %d; want 0, 1", originalCalls, trampolineCalls)
	}
}
