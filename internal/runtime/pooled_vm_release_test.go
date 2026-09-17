// Copyright 2019 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"context"
	"io"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"weak"

	"gitlab.com/haproxy-haptic/scriggo/native"
)

func TestPutPooledVMReleasesExposedVectorEnvironment(t *testing.T) {
	for _, leased := range []bool{false, true} {
		name := "unleased"
		if leased {
			name = "leased"
		}
		t.Run(name, func(t *testing.T) {
			vm, payload := pooledVectorPayload(t, leased)
			runtime.GC()
			if payload.Value() != nil {
				t.Fatal("idle pooled VM retains the previous vector invocation")
			}
			runtime.KeepAlive(vm)
		})
	}
}

func pooledVectorPayload(t *testing.T, leased bool) (*VM, weak.Pointer[[1024]byte]) {
	t.Helper()
	type contextKey struct{}
	payload := new([1024]byte)
	ctx := context.WithValue(context.Background(), contextKey{}, payload)
	authority := native.NewVectorAuthority()
	state, err := NewVectorState(authority, 1, nil, []context.Context{ctx})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Activate(authority, 0); err != nil {
		t.Fatal(err)
	}
	environment := &env{ctx: ctx, vector: state}
	vm := getPooledVM(environment, ctx)
	vm.nativeEnv.vectorGeneration = state.capture()
	if leased {
		lease, err := state.acquireInvocation(state.capture())
		if err != nil {
			t.Fatal(err)
		}
		vm.nativeEnv.vectorLease = lease
	}
	exposed := vm.nativeEnvForExposure()
	if exposed.Context().Value(contextKey{}) != payload {
		t.Fatal("native call did not receive the invocation context")
	}
	if leased {
		vm.nativeEnv.vectorLease.close()
	}
	state.Close()
	putPooledVM(vm)
	if exposed.env != environment || environment.ctx != ctx {
		t.Fatal("pool return mutated an escaped native environment")
	}
	return vm, weak.Make(payload)
}

// A VM returned to the pool must hold nothing from the render it just ran.
//
// The pool is deliberately not GC-evictable, so anything a released VM still
// points at stays reachable for the process lifetime. Truncating the frame
// stacks with [:0] leaves the elements inside the capacity: a rangeState pins
// the collection it was iterating and a callFrame pins that render's renderer.
// Measured on a controller rendering an HAProxy configuration, that was tens of
// megabytes of parsed configuration held per pooled VM.
func TestPutPooledVMReleasesFrames(t *testing.T) {
	vm := NewVM()

	// A render deep enough to grow the stacks, then unwound: len is back to 0
	// while cap still holds the elements. This is the state putPooledVM sees.
	vm.calls = append(vm.calls, callFrame{renderer: &renderer{}}, callFrame{renderer: &renderer{}})
	vm.ranges = append(vm.ranges, rangeState{sliceIface: []interface{}{"held"}})
	vm.cases = append(vm.cases, reflect.SelectCase{})
	vm.calls = vm.calls[:0]
	vm.ranges = vm.ranges[:0]
	vm.cases = vm.cases[:0]

	putPooledVM(vm)

	assertCleared(t, "calls", vm.calls, func(f callFrame) bool { return f.renderer != nil })
	assertCleared(t, "ranges", vm.ranges, func(r rangeState) bool { return r.sliceIface != nil })
	assertCleared(t, "cases", vm.cases, func(c reflect.SelectCase) bool { return c.Chan.IsValid() })
}

// assertCleared reports any element still holding a reference inside the
// slice's capacity — the region a bare truncation leaves untouched.
func assertCleared[T any](t *testing.T, name string, s []T, holds func(T) bool) {
	t.Helper()
	if len(s) != 0 {
		t.Errorf("%s: len is %d, want 0", name, len(s))
	}
	full := s[:cap(s)]
	for i := range full {
		if holds(full[i]) {
			t.Errorf("%s[%d] still holds a reference from the previous render; "+
				"the pool is not GC-evictable, so this keeps that render alive forever", name, i)
		}
	}
}

// The register banks are cleared to their high-water mark, but not every write
// path records one: a bulk copy into a pooled VM's banks bypasses noteHigh, so
// clearing only to the mark leaves live reflect.Values in the tail — among them
// the render's globals.
func TestPutPooledVMClearsRegistersBeyondHighWaterMark(t *testing.T) {
	vm := NewVM()

	const beyond = 40
	if len(vm.regs.general) <= beyond || len(vm.regs.string) <= beyond {
		t.Skipf("register banks too small for this test: general=%d string=%d",
			len(vm.regs.general), len(vm.regs.string))
	}

	// Written without noteHigh, exactly as the bulk-copy paths do.
	vm.regs.general[beyond] = reflect.ValueOf(&struct{ payload [64]byte }{})
	vm.regs.string[beyond] = "held by the previous render"
	vm.high = [4]Addr{0, 0, 0, 0}

	putPooledVM(vm)

	if vm.regs.general[beyond].IsValid() {
		t.Error("general register beyond the high-water mark still holds a value; " +
			"a bulk copy writes there without noteHigh, so the mark cannot bound the clear")
	}
	if vm.regs.string[beyond] != "" {
		t.Error("string register beyond the high-water mark still holds a value")
	}
}

func TestResetReleasesExecutionState(t *testing.T) {
	vm := NewVM()
	oldState := vm.state
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	vm.SetContext(ctx)
	vm.SetChildContext(func(parent context.Context) context.Context { return parent })
	vm.SetMutationObserver(func(native.Env, Mutation) error { return nil })
	vm.SetNativeCallObserver(func(native.Env, NativeCall) error { return nil })
	vm.SetBeforeNativeCall(func(native.Env) error { return nil })
	vm.SetDeterministic(true)
	vm.SetPrint(func(any) {})
	vm.SetRenderer(io.Discard, func([]byte, io.Writer) error { return nil })
	vm.SetProfiling(true)
	vm.env.globals = []reflect.Value{reflect.ValueOf("held")}
	vm.env.typeof = func(value reflect.Value) reflect.Type { return value.Type() }
	vm.nativeEnv.callPath = "held"
	vm.nativeEnv.callLine = 42
	vm.rootRender.pendingBuf = &strings.Builder{}
	vm.rootRender.pendingSpans = []SourceSpan{{Length: 1}}
	vm.fn = &Function{Name: "held"}
	vm.vars = []reflect.Value{reflect.ValueOf("held")}
	vm.regs.int[1] = 41
	vm.regs.float[1] = 42
	vm.regs.string[1] = "held"
	vm.regs.general[1] = reflect.ValueOf(&struct{ payload [64]byte }{})
	vm.calls = append(vm.calls, callFrame{renderer: &renderer{}})
	vm.ranges = append(vm.ranges, rangeState{sliceIface: []any{"held"}})
	vm.cases = append(vm.cases, reflect.SelectCase{Chan: reflect.ValueOf(make(chan struct{}))})
	vm.calls = vm.calls[:0]
	vm.ranges = vm.ranges[:0]
	vm.cases = vm.cases[:0]
	vm.high = [4]Addr{1, 1, 1, 1}
	vm.rangesBase = 9
	vm.panic = &PanicError{message: "held"}
	vm.nativeArgs[0] = reflect.ValueOf("held")
	vm.nativeCall.value = reflect.ValueOf(func() {})

	vm.Reset()

	if vm.state == oldState || vm.env == &oldState.env || vm.nativeEnv == &oldState.nativeEnv {
		t.Fatal("Reset reused an execution environment reachable by an escaped closure")
	}
	if oldState.env.ctx != ctx || oldState.nativeEnv.ctx != ctx {
		t.Fatal("Reset mutated the prior execution environment")
	}
	if vm.env != &vm.state.env || vm.nativeEnv != &vm.state.nativeEnv || vm.nativeEnv.env != vm.env {
		t.Fatal("Reset installed an inconsistent execution environment")
	}
	if got := vm.envArg.Interface(); got != vm.nativeEnv {
		t.Fatalf("Reset environment argument = %p, want %p", got, vm.nativeEnv)
	}
	if vm.env.ctx != nil || vm.env.childContext != nil || vm.env.globals != nil || vm.env.print != nil ||
		vm.env.typeof != nil || vm.env.conv != nil || vm.env.observeMutation != nil ||
		vm.env.observeMutationContext != nil || vm.env.observeNativeCall != nil ||
		vm.env.observeNativeCallContext != nil || vm.env.beforeNativeCall != nil ||
		vm.env.beforeNativeCallContext != nil || vm.env.deterministic {
		t.Fatal("Reset retained execution environment options")
	}
	if vm.nativeEnv.ctx != nil || vm.nativeEnv.callPath != "" || vm.nativeEnv.callLine != 0 {
		t.Fatal("Reset retained native execution environment state")
	}
	if vm.regs.int[1] != 0 || vm.regs.float[1] != 0 || vm.regs.string[1] != "" || vm.regs.general[1].IsValid() {
		t.Fatal("Reset retained register data from the previous execution")
	}
	if vm.high != [4]Addr{} {
		t.Fatalf("Reset retained high-water marks: %v", vm.high)
	}
	assertCleared(t, "calls", vm.calls, func(f callFrame) bool { return f.renderer != nil })
	assertCleared(t, "ranges", vm.ranges, func(r rangeState) bool { return r.sliceIface != nil })
	assertCleared(t, "cases", vm.cases, func(c reflect.SelectCase) bool { return c.Chan.IsValid() })
	if vm.fn != nil || vm.vars != nil || vm.renderer != nil || vm.rootRender.out != nil ||
		vm.rootRender.pendingBuf != nil || vm.rootRender.pendingSpans != nil || vm.rangesBase != 0 {
		t.Fatal("Reset retained VM or renderer execution state")
	}
	if vm.doneChan != nil || vm.doneCase.Chan.IsValid() || vm.panic != nil || vm.profiler != nil {
		t.Fatal("Reset retained cancellation, panic, or profiler state")
	}
	for index, value := range vm.nativeArgs {
		if value.IsValid() {
			t.Fatalf("Reset retained native argument %d", index)
		}
	}
	if vm.nativeCall.value.IsValid() {
		t.Fatal("Reset retained a native function value")
	}
}

// Reset is what drops a Builder's buffer, and it must happen on the way in to
// the pool. Resetting only on the way out leaves every idle pooled builder
// holding the byte array of the largest output it ever produced.
func TestPutStringBuilderReturnsAnEmptyBuilder(t *testing.T) {
	b := stringBuilderPool.get()
	b.WriteString("rendered output that must not stay reachable")

	putStringBuilder(b)

	if b.Len() != 0 {
		t.Errorf("builder returned to the pool with %d bytes still in it", b.Len())
	}
	runtime.KeepAlive(b)
}
