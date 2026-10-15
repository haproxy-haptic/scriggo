// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"testing"

	"github.com/haproxy-haptic/scriggo/native"
)

func TestVectorStatePrevalidatesEverySlotBeforeMutation(t *testing.T) {
	authority := native.NewVectorAuthority()
	first := "unchanged"
	second := 42
	_, err := NewVectorState(authority, 1, []VectorBinding{{
		Name:   "value",
		Column: reflect.ValueOf([]string{"replacement"}),
		Slots: []reflect.Value{
			reflect.ValueOf(&first).Elem(),
			reflect.ValueOf(&second).Elem(),
		},
	}}, []context.Context{context.Background()})
	if !errors.Is(err, ErrVectorInvalidConfiguration) {
		t.Fatalf("error = %v, want %v", err, ErrVectorInvalidConfiguration)
	}
	if first != "unchanged" || second != 42 {
		t.Fatalf("slots changed to %q/%d", first, second)
	}
}

func TestVectorStateAuthenticatesAndOrdersActivation(t *testing.T) {
	authority := native.NewVectorAuthority()
	other := native.NewVectorAuthority()
	value := "initial"
	state, err := NewVectorState(authority, 2, []VectorBinding{{
		Name:   "value",
		Column: reflect.ValueOf([]string{"first", "second"}),
		Slots:  []reflect.Value{reflect.ValueOf(&value).Elem()},
	}}, []context.Context{context.Background(), context.Background()})
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	if err := state.Activate(other, 0); !errors.Is(err, ErrVectorInvalidConfiguration) {
		t.Fatalf("foreign activation error = %v", err)
	}
	if value != "initial" {
		t.Fatalf("foreign activation changed value to %q", value)
	}
	if err := state.Activate(authority, 0); err != nil {
		t.Fatalf("activate first: %v", err)
	}
	if value != "first" {
		t.Fatalf("value = %q, want first", value)
	}
	if err := state.Activate(authority, 1); err == nil {
		t.Fatal("second activation while active succeeded")
	}
	if value != "first" {
		t.Fatalf("failed activation changed value to %q", value)
	}
	if err := state.Revoke(authority); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := state.Activate(authority, 1); err != nil {
		t.Fatalf("activate second: %v", err)
	}
	if value != "second" {
		t.Fatalf("value = %q, want second", value)
	}
}

func TestVectorStateReplacesEveryDuplicateSlot(t *testing.T) {
	authority := native.NewVectorAuthority()
	first := "first-old"
	second := "second-old"
	state, err := NewVectorState(authority, 1, []VectorBinding{{
		Name:   "value",
		Column: reflect.ValueOf([]string{"replacement"}),
		Slots: []reflect.Value{
			reflect.ValueOf(&first).Elem(),
			reflect.ValueOf(&second).Elem(),
		},
	}}, []context.Context{context.Background()})
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	if err := state.Activate(authority, 0); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if first != "replacement" || second != "replacement" {
		t.Fatalf("slots = %q/%q, want replacement/replacement", first, second)
	}
}

func TestDeferredVectorStateLoadsOwnedRangesSequentially(t *testing.T) {
	type contextKey struct{}
	authority := native.NewVectorAuthority()
	value := "unchanged"
	state, err := NewDeferredVectorState(authority, 2, []VectorBinding{{
		Name:  "value",
		Type:  reflect.TypeFor[string](),
		Slots: []reflect.Value{reflect.ValueOf(&value).Elem()},
	}})
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	if err := state.Activate(authority, 0); err == nil {
		t.Fatal("unloaded activation succeeded")
	}
	values := []string{"first"}
	contexts := []context.Context{context.WithValue(context.Background(), contextKey{}, "first")}
	if err := state.LoadRange(authority, 0, map[string]any{"value": values}, contexts); err != nil {
		t.Fatalf("load first range: %v", err)
	}
	values[0] = "poison"
	contexts[0] = context.WithValue(context.Background(), contextKey{}, "poison")
	if err := state.Activate(authority, 0); err != nil {
		t.Fatalf("activate first: %v", err)
	}
	if value != "first" {
		t.Fatalf("first value = %q, want owned first", value)
	}
	_, activeContext := state.captureContext()
	if got := activeContext.Value(contextKey{}); got != "first" {
		t.Fatalf("first context value = %v, want owned first", got)
	}
	if err := state.LoadRange(authority, 1, map[string]any{"value": []string{"second"}}, []context.Context{context.Background()}); !errors.Is(err, ErrVectorInvalidConfiguration) {
		t.Fatalf("active load error = %v, want %v", err, ErrVectorInvalidConfiguration)
	}
	if err := state.Revoke(authority); err != nil {
		t.Fatalf("revoke first: %v", err)
	}
	if err := state.LoadRange(authority, 1, map[string]any{"value": []string{"second"}}, []context.Context{context.Background()}); err != nil {
		t.Fatalf("load second range: %v", err)
	}
	if err := state.Activate(authority, 1); err != nil {
		t.Fatalf("activate second: %v", err)
	}
	if value != "second" {
		t.Fatalf("second value = %q, want second", value)
	}
}

func TestDeferredVectorStatePrevalidatesRangeBeforePublication(t *testing.T) {
	authority := native.NewVectorAuthority()
	first := "first-old"
	second := 42
	state, err := NewDeferredVectorState(authority, 1, []VectorBinding{
		{Name: "first", Type: reflect.TypeFor[string](), Slots: []reflect.Value{reflect.ValueOf(&first).Elem()}},
		{Name: "second", Type: reflect.TypeFor[int](), Slots: []reflect.Value{reflect.ValueOf(&second).Elem()}},
	})
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	err = state.LoadRange(authority, 0, map[string]any{
		"first":  []string{"first-new"},
		"second": []string{"invalid"},
	}, []context.Context{context.Background()})
	if !errors.Is(err, ErrVectorInvalidConfiguration) {
		t.Fatalf("invalid range error = %v, want %v", err, ErrVectorInvalidConfiguration)
	}
	if state.loaded != 0 || len(state.ranges) != 0 || first != "first-old" || second != 42 {
		t.Fatalf("invalid range published state: loaded=%d ranges=%d slots=%q/%d", state.loaded, len(state.ranges), first, second)
	}
	if err := state.LoadRange(authority, 0, map[string]any{
		"first":  []string{"first-new"},
		"second": []int{43},
	}, []context.Context{context.Background()}); err != nil {
		t.Fatalf("valid range: %v", err)
	}
	if err := state.Activate(authority, 0); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if first != "first-new" || second != 43 {
		t.Fatalf("slots = %q/%d, want first-new/43", first, second)
	}
}

func TestDeferredVectorStateTakesExactOwnedRangeWithoutCopy(t *testing.T) {
	type contextKey struct{}
	authority := native.NewVectorAuthority()
	value := "unchanged"
	state, err := NewDeferredVectorState(authority, 1, []VectorBinding{{
		Name:  "value",
		Type:  reflect.TypeFor[string](),
		Slots: []reflect.Value{reflect.ValueOf(&value).Elem()},
	}})
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	values := []string{"owned"}
	contexts := []context.Context{context.WithValue(context.Background(), contextKey{}, "owned")}
	if err := state.LoadOwnedRange(
		authority,
		0,
		map[string]any{"value": values},
		contexts,
	); err != nil {
		t.Fatalf("load owned range: %v", err)
	}
	if got, want := state.ranges[0].columns[0].Pointer(), reflect.ValueOf(values).Pointer(); got != want {
		t.Fatalf("owned column pointer = %#x, want transferred %#x", got, want)
	}
	if got, want := reflect.ValueOf(state.ranges[0].contexts).Pointer(), reflect.ValueOf(contexts).Pointer(); got != want {
		t.Fatalf("owned context pointer = %#x, want transferred %#x", got, want)
	}
	if err := state.Activate(authority, 0); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if value != "owned" {
		t.Fatalf("value = %q, want owned", value)
	}
	_, activeContext := state.captureContext()
	if got := activeContext.Value(contextKey{}); got != "owned" {
		t.Fatalf("context = %v, want owned", got)
	}
}

func TestDeferredVectorStateRejectsIndirectOwnedRangeAtomically(t *testing.T) {
	authority := native.NewVectorAuthority()
	value := "unchanged"
	state, err := NewDeferredVectorState(authority, 1, []VectorBinding{{
		Name:  "value",
		Type:  reflect.TypeFor[string](),
		Slots: []reflect.Value{reflect.ValueOf(&value).Elem()},
	}})
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	indirect := "owned"
	err = state.LoadOwnedRange(
		authority,
		0,
		map[string]any{"value": []*string{&indirect}},
		[]context.Context{context.Background()},
	)
	if !errors.Is(err, ErrVectorInvalidConfiguration) {
		t.Fatalf("indirect owned range error = %v, want %v", err, ErrVectorInvalidConfiguration)
	}
	if state.loaded != 0 || len(state.ranges) != 0 || value != "unchanged" {
		t.Fatalf("rejected owned range published state: loaded=%d ranges=%d value=%q", state.loaded, len(state.ranges), value)
	}
}

func TestDeferredVectorStateRejectsForeignAndReplayedRanges(t *testing.T) {
	authority := native.NewVectorAuthority()
	state, err := NewDeferredVectorState(authority, 2, []VectorBinding{{
		Name:  "value",
		Type:  reflect.TypeFor[string](),
		Slots: []reflect.Value{reflect.ValueOf(new(string)).Elem()},
	}})
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	load := func(authority *native.VectorAuthority, start int) error {
		return state.LoadRange(
			authority,
			start,
			map[string]any{"value": []string{"value"}},
			[]context.Context{context.Background()},
		)
	}
	if err := load(native.NewVectorAuthority(), 0); !errors.Is(err, ErrVectorInvalidConfiguration) {
		t.Fatalf("foreign load error = %v, want %v", err, ErrVectorInvalidConfiguration)
	}
	if err := load(authority, 1); !errors.Is(err, ErrVectorInvalidConfiguration) {
		t.Fatalf("out-of-order load error = %v, want %v", err, ErrVectorInvalidConfiguration)
	}
	if err := load(authority, 0); err != nil {
		t.Fatalf("first load: %v", err)
	}
	if err := load(authority, 0); !errors.Is(err, ErrVectorInvalidConfiguration) {
		t.Fatalf("replayed load error = %v, want %v", err, ErrVectorInvalidConfiguration)
	}
}

func TestNativeEnvLoadVectorRangePreservesTrampolineMapsWithoutAdditions(t *testing.T) {
	authority := native.NewVectorAuthority()
	value := "unchanged"
	state, err := NewDeferredVectorState(authority, 1, []VectorBinding{{
		Name:  "value",
		Type:  reflect.TypeFor[string](),
		Slots: []reflect.Value{reflect.ValueOf(&value).Elem()},
	}})
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	nativeTrampolines := make(map[reflect.Value]nativeFunctionCallSite)
	methodTrampolines := make(map[nativeMethodTrampolineKey]nativeFunctionCallSite)
	runtimeEnv := &env{
		vector:            state,
		nativeTrampolines: nativeTrampolines,
		methodTrampolines: methodTrampolines,
	}
	callEnv := &nativeEnv{env: runtimeEnv}
	err = callEnv.LoadVectorRange(
		authority,
		0,
		map[string]any{"value": []string{"replacement"}},
		[]context.Context{context.Background()},
		nil,
	)
	if err != nil {
		t.Fatalf("load range: %v", err)
	}
	nativeTrampolines[reflect.ValueOf(func() {})] = nativeFunctionCallSite{}
	methodTrampolines[nativeMethodTrampolineKey{method: "marker"}] = nativeFunctionCallSite{}
	if len(runtimeEnv.nativeTrampolines) != 1 || len(runtimeEnv.methodTrampolines) != 1 {
		t.Fatal("empty trampoline load replaced the installed maps")
	}
}

func TestVectorStateRevokeWaitsForInvocation(t *testing.T) {
	authority := native.NewVectorAuthority()
	state, err := NewVectorState(authority, 1, nil, []context.Context{context.Background()})
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	if err := state.Activate(authority, 0); err != nil {
		t.Fatalf("activate: %v", err)
	}
	generation := state.capture()
	if err := state.acquire(generation); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- state.Revoke(authority)
	}()
	for {
		state.mu.Lock()
		revoking := state.revoking
		state.mu.Unlock()
		if revoking {
			break
		}
		runtime.Gosched()
	}
	select {
	case err := <-done:
		t.Fatalf("revoke completed before invocation release: %v", err)
	default:
	}
	state.release()
	if err := <-done; err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := state.acquire(generation); !errors.Is(err, ErrVectorGenerationRevoked) {
		t.Fatalf("old generation error = %v", err)
	}
}

func TestVectorStateRevokeDrainsAuthenticatedInvocationContext(t *testing.T) {
	type contextKey struct{}
	authority := native.NewVectorAuthority()
	state, err := NewVectorState(authority, 1, nil, []context.Context{
		context.WithValue(context.Background(), contextKey{}, "value"),
	})
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	if err := state.Activate(authority, 0); err != nil {
		t.Fatalf("activate: %v", err)
	}
	generation := state.capture()
	lease, err := state.acquireInvocation(generation)
	if err != nil {
		t.Fatalf("acquire invocation: %v", err)
	}
	ctx := &vectorInvocationContext{Context: lease.ctx, lease: lease}
	done := make(chan error, 1)
	go func() {
		done <- state.Revoke(authority)
	}()
	for {
		state.mu.Lock()
		revoking := state.revoking
		state.mu.Unlock()
		if revoking {
			break
		}
		runtime.Gosched()
	}
	if got := ctx.Value(contextKey{}); got != "value" {
		t.Fatalf("in-flight context value = %v, want value", got)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("in-flight context error = %v", err)
	}
	if _, err := state.acquireInvocation(generation); !errors.Is(err, ErrVectorGenerationRevoked) {
		t.Fatalf("new invocation error = %v, want %v", err, ErrVectorGenerationRevoked)
	}
	lease.close()
	if err := <-done; err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got := ctx.Value(contextKey{}); got != nil {
		t.Fatalf("revoked invocation context exposed %v", got)
	}
}

func TestVectorStateRevokesIndexedContext(t *testing.T) {
	type contextKey struct{}
	authority := native.NewVectorAuthority()
	state, err := NewVectorState(
		authority,
		2,
		nil,
		[]context.Context{
			context.WithValue(context.Background(), contextKey{}, "first"),
			context.WithValue(context.Background(), contextKey{}, "second"),
		},
	)
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	if err := state.Activate(authority, 0); err != nil {
		t.Fatalf("activate first: %v", err)
	}
	_, first := state.captureContext()
	if got := first.Value(contextKey{}); got != "first" {
		t.Fatalf("first context value = %v", got)
	}
	if err := state.Revoke(authority); err != nil {
		t.Fatalf("revoke first: %v", err)
	}
	if got := first.Value(contextKey{}); got != nil {
		t.Fatalf("revoked context value = %v, want nil", got)
	}
	if err := first.Err(); !errors.Is(err, ErrVectorGenerationRevoked) {
		t.Fatalf("revoked context error = %v", err)
	}
	select {
	case <-first.Done():
	default:
		t.Fatal("revoked context Done is open")
	}
	if err := state.Activate(authority, 1); err != nil {
		t.Fatalf("activate second: %v", err)
	}
	_, second := state.captureContext()
	if got := second.Value(contextKey{}); got != "second" {
		t.Fatalf("second context value = %v", got)
	}
	if got := first.Value(contextKey{}); got != nil {
		t.Fatalf("first context exposed second value %v", got)
	}
}

// failingFiberLifecycle refuses a child transition without panicking, which is
// what a lifecycle does when it cannot open the child at all.
type failingFiberLifecycle struct{ err error }

func (l failingFiberLifecycle) BeginChild(int, int) (context.Context, error) { return nil, l.err }
func (l failingFiberLifecycle) EndChild(int, int) error                      { return l.err }

// A fiber transition revokes the active context before handing control to the
// lifecycle. When the lifecycle then returns a plain error, the state used to
// keep revoking set with no context installed, so every later call answered
// ErrVectorInvalidConfiguration — a stuck state that only looked harmless
// because the VM turns these errors into a panic that Template.run converts
// into CloseWithError. A direct caller got no such rescue.
func TestVectorStateDoesNotStayHalfTransitionedWhenAFiberFails(t *testing.T) {
	authority := native.NewVectorAuthority()
	boundary := native.NewVectorFiberBoundary()
	failure := errors.New("child refused")
	value := "initial"

	state, err := NewVMNativeVectorState(authority, 1, []VectorBinding{{
		Name:          "value",
		Column:        reflect.ValueOf([]string{"first"}),
		Slots:         []reflect.Value{reflect.ValueOf(&value).Elem()},
		GlobalIndexes: []int{0},
	}}, []context.Context{context.Background()})
	if err != nil {
		t.Fatalf("new vm-native state: %v", err)
	}
	t.Cleanup(func() { state.Close() })

	if err := state.SetVMNativeFiberBoundary(boundary, failingFiberLifecycle{err: failure}); err != nil {
		t.Fatalf("set fiber boundary: %v", err)
	}
	if err := state.Activate(authority, 0); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if err := state.BeginFiberVMNative(boundary, 0, 1); !errors.Is(err, failure) {
		t.Fatalf("begin fiber error = %v, want %v", err, failure)
	}

	state.mu.Lock()
	revoking, fiberActive, aborted := state.revoking, state.fiberActive, state.aborted
	state.mu.Unlock()
	if revoking {
		t.Error("revoking stayed set after the fiber failed: the state is stuck")
	}
	if fiberActive >= 0 {
		t.Errorf("fiberActive = %d, want none after the fiber failed", fiberActive)
	}
	if aborted {
		t.Error("the state must not self-abort here: CloseWithError only notifies the outer " +
			"lifecycle while !aborted, so doing so would swallow the Abort for the active row")
	}
}
