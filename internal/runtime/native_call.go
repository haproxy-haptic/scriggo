// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"context"
	"reflect"

	"gitlab.com/haproxy-haptic/scriggo/native"
)

type NativeCall struct {
	Receiver    reflect.Value
	Method      string
	MethodValue bool
	Path        string
	Line        int
}

type NativeCallObserver func(native.Env, NativeCall) error

type NativeCallContextObserver func(context.Context, NativeCall) error

type BeforeNativeCall func(native.Env) error

type BeforeNativeCallContext func(context.Context) error

type nativeMethodTrampolineKey struct {
	receiver reflect.Type
	method   string
	function reflect.Type
}

func runBeforeNativeCall(before BeforeNativeCall, nativeEnv *nativeEnv) {
	if err := before(nativeEnv); err != nil {
		panic(stopError{err})
	}
}

func runBeforeNativeCallContext(before BeforeNativeCallContext, ctx context.Context) {
	if err := before(ctx); err != nil {
		panic(stopError{err})
	}
}

func (vm *VM) beforeNativeCall(nativeEnv *nativeEnv) {
	ctx := vm.nativeEnv.ctx
	if nativeEnv != nil {
		ctx = nativeEnv.ctx
	}
	vm.beforeNativeCallWithContext(ctx, nativeEnv)
}

func (vm *VM) beforeNativeCallWithContext(ctx context.Context, nativeEnv *nativeEnv) {
	if before := vm.env.beforeNativeCallContext; before != nil {
		runBeforeNativeCallContext(before, ctx)
	}
	if before := vm.env.beforeNativeCall; before != nil {
		vm.prepareEnvCallPathFor(nativeEnv)
		runBeforeNativeCall(before, nativeEnv)
	}
}

func (vm *VM) observeNativeCall(nativeEnv *nativeEnv, receiver reflect.Value, method string, methodValue bool) {
	ctx := vm.nativeEnv.ctx
	if nativeEnv != nil {
		ctx = nativeEnv.ctx
	}
	vm.observeNativeCallWithContext(ctx, nativeEnv, receiver, method, methodValue)
}

func (vm *VM) observeNativeCallWithContext(
	ctx context.Context,
	nativeEnv *nativeEnv,
	receiver reflect.Value,
	method string,
	methodValue bool,
) {
	contextObserver := vm.env.observeNativeCallContext
	observer := vm.env.observeNativeCall
	if contextObserver == nil && observer == nil {
		return
	}
	call := NativeCall{Receiver: receiver, Method: method, MethodValue: methodValue}
	if info, ok := vm.fn.InstructionInfo[vm.pc-1]; ok {
		call.Path = info.Path
		call.Line = info.Position.Line
	}
	if contextObserver != nil {
		if err := contextObserver(ctx, call); err != nil {
			panic(stopError{err})
		}
	}
	if observer != nil {
		vm.markBatchEnvironmentEscaped()
		if err := observer(nativeEnv, call); err != nil {
			panic(stopError{err})
		}
	}
}
