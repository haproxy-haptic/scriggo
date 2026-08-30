// Copyright 2019 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"

	"gitlab.com/haproxy-haptic/scriggo/native"
)

type PrintFunc func(any)

// Context represents a context in Show and Text instructions.
type Context byte

type env struct {
	ctx                      context.Context // root context.
	childContext             func(context.Context) context.Context
	globals                  []reflect.Value // global variables.
	print                    PrintFunc       // custom print builtin.
	typeof                   TypeOfFunc      // typeof function.
	conv                     Converter       // Markdown converter
	observeMutation          MutationObserver
	observeMutationContext   MutationContextObserver
	observeNativeCall        NativeCallObserver
	observeNativeCallContext NativeCallContextObserver
	beforeNativeCall         BeforeNativeCall
	beforeNativeCallContext  BeforeNativeCallContext
	nativeTrampolines        map[reflect.Value]nativeFunctionCallSite
	methodTrampolines        map[nativeMethodTrampolineKey]nativeFunctionCallSite
	deterministic            bool
	vector                   *VectorState
	internalFunctionGlobals  []bool
	internalFunctionValues   []atomic.Pointer[callable]
	internalFunctionEscaped  []atomic.Bool
}

// nativeEnv is the native-facing view of an execution environment.
// Runtime state stays in env; each VM carries its execution context.
type nativeEnv struct {
	env              *env
	ctx              context.Context
	vector           *VectorState
	vectorGeneration uint64
	vectorLease      *vectorInvocationLease

	mu       sync.Mutex
	callPath string
	callLine int
}

func (env *env) deriveChildContext(parent context.Context) context.Context {
	if env.childContext == nil {
		return parent
	}
	return env.childContext(parent)
}

func (env *nativeEnv) CallPath() string {
	release := env.acquireVectorGeneration()
	defer release()
	env.mu.Lock()
	callPath := env.callPath
	env.mu.Unlock()
	return callPath
}

func (env *nativeEnv) CallLine() int {
	release := env.acquireVectorGeneration()
	defer release()
	env.mu.Lock()
	callLine := env.callLine
	env.mu.Unlock()
	return callLine
}

func (env *nativeEnv) Context() context.Context {
	return env.ctx
}

func (env *nativeEnv) Fatal(v any) {
	release := env.acquireVectorGeneration()
	defer release()
	panic(&fatalError{env: env.env, msg: v})
}

func (env *nativeEnv) MarkdownConverter() Converter {
	release := env.acquireVectorGeneration()
	defer release()
	return env.env.conv
}

func (env *nativeEnv) Print(args ...any) {
	release := env.acquireVectorGeneration()
	defer release()
	for _, arg := range args {
		env.env.doPrint(arg)
	}
}

func (env *nativeEnv) Println(args ...any) {
	release := env.acquireVectorGeneration()
	defer release()
	for i, arg := range args {
		if i > 0 {
			env.env.doPrint(" ")
		}
		env.env.doPrint(arg)
	}
	env.env.doPrint("\n")
}

func (env *nativeEnv) Stop(err error) {
	release := env.acquireVectorGeneration()
	defer release()
	panic(stopError{err})
}

func (env *nativeEnv) TypeOf(v reflect.Value) reflect.Type {
	release := env.acquireVectorGeneration()
	defer release()
	return env.env.typeof(v)
}

func (env *nativeEnv) acquireVectorGeneration() func() {
	if env == nil || env.vector == nil || env.vectorGeneration == 0 {
		return func() {}
	}
	if env.vectorLease != nil {
		if err := env.vectorLease.acquireUse(); err != nil {
			panic(stopError{err})
		}
		return env.vectorLease.releaseUse
	}
	if err := env.vector.acquire(env.vectorGeneration); err != nil {
		panic(stopError{err})
	}
	return env.vector.release
}

func (env *nativeEnv) ActivateVectorIndex(authority *native.VectorAuthority, index int) error {
	if env == nil || env.env == nil || env.env.vector == nil {
		return ErrVectorInvalidConfiguration
	}
	if env.vectorGeneration != 0 {
		return ErrVectorInvalidConfiguration
	}
	if err := env.env.vector.Activate(authority, index); err != nil {
		return err
	}
	generation, ctx := env.env.vector.captureContext()
	if generation == 0 || ctx == nil {
		return ErrVectorInvalidConfiguration
	}
	env.vector = env.env.vector
	env.vectorGeneration = generation
	env.ctx = ctx
	return nil
}

func (env *nativeEnv) RevokeVectorIndex(authority *native.VectorAuthority) error {
	if env == nil || env.env == nil || env.env.vector == nil {
		return ErrVectorInvalidConfiguration
	}
	if env.vector != env.env.vector || env.vectorGeneration == 0 ||
		env.vector.capture() != env.vectorGeneration {
		return ErrVectorGenerationRevoked
	}
	return env.env.vector.Revoke(authority)
}

func (env *nativeEnv) LoadVectorRange(
	authority *native.VectorAuthority,
	start int,
	bindings map[string]any,
	contexts []context.Context,
	trampolines []*native.FunctionTrampoline,
) error {
	if env == nil || env.env == nil || env.env.vector == nil || env.vectorGeneration != 0 {
		return ErrVectorInvalidConfiguration
	}
	if len(trampolines) == 0 {
		return env.env.vector.LoadRange(authority, start, bindings, contexts)
	}
	nativeTrampolines, methodTrampolines, err := mergeNativeFunctionTrampolines(
		env.env.nativeTrampolines,
		env.env.methodTrampolines,
		trampolines,
	)
	if err != nil {
		return err
	}
	if err := env.env.vector.LoadRange(authority, start, bindings, contexts); err != nil {
		return err
	}
	env.env.nativeTrampolines = nativeTrampolines
	env.env.methodTrampolines = methodTrampolines
	return nil
}

func (env *nativeEnv) LoadVectorRangeOwned(
	authority *native.VectorAuthority,
	start int,
	bindings map[string]any,
	contexts []context.Context,
	trampolines []*native.FunctionTrampoline,
) error {
	if env == nil || env.env == nil || env.env.vector == nil || env.vectorGeneration != 0 {
		return ErrVectorInvalidConfiguration
	}
	if len(trampolines) == 0 {
		return env.env.vector.LoadOwnedRange(authority, start, bindings, contexts)
	}
	nativeTrampolines, methodTrampolines, err := mergeNativeFunctionTrampolines(
		env.env.nativeTrampolines,
		env.env.methodTrampolines,
		trampolines,
	)
	if err != nil {
		return err
	}
	if err := env.env.vector.LoadOwnedRange(authority, start, bindings, contexts); err != nil {
		return err
	}
	env.env.nativeTrampolines = nativeTrampolines
	env.env.methodTrampolines = methodTrampolines
	return nil
}

func typeOfFunc(v reflect.Value) reflect.Type {
	return v.Type()
}

func (env *env) doPrint(arg any) {
	if env.print != nil {
		env.print(arg)
		return
	}
	r := reflect.ValueOf(arg)
	switch r.Kind() {
	case reflect.Invalid, reflect.Array, reflect.Func, reflect.Struct:
		print(hex(reflect.ValueOf(&arg).Elem().InterfaceData()[1]))
	case reflect.Bool:
		print(r.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		print(r.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		print(r.Uint())
	case reflect.Float32, reflect.Float64:
		print(r.Float())
	case reflect.Complex64, reflect.Complex128:
		print(r.Complex())
	case reflect.Chan, reflect.Map, reflect.UnsafePointer:
		print(hex(r.Pointer()))
	case reflect.Interface, reflect.Pointer:
		print(arg)
	case reflect.Slice:
		print("[", r.Len(), "/", r.Cap(), "]", hex(r.Pointer()))
	case reflect.String:
		print(r.String())
	}
}
