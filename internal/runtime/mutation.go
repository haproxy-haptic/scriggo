// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"context"
	"errors"
	"reflect"

	"github.com/haproxy-haptic/scriggo/native"
)

// ErrMutationObserverDetachedGo is returned before a detached Scriggo
// goroutine starts because its observer error cannot be joined by Run.
var ErrMutationObserverDetachedGo = errors.New("scriggo: mutation observer cannot guard a detached Scriggo go statement")

// ErrMutationObserverChannelOperation is returned before template code uses a
// channel while a mutation observer is installed.
var ErrMutationObserverChannelOperation = errors.New("scriggo: mutation observer cannot guard a channel operation")

// MutationOperation identifies a template-language mutation.
type MutationOperation uint8

const (
	MutationSetVariable MutationOperation = iota + 1
	MutationSetPointer
	MutationSetField
	MutationSetMapIndex
	MutationSetIndex
	MutationDelete
	MutationClear
	MutationAppend
	MutationCopy
	MutationCloseChannel
	MutationSendChannel
	MutationReceiveChannel
	MutationSelectChannel
)

// Mutation describes storage immediately before the template mutates it.
type Mutation struct {
	Operation MutationOperation
	Target    reflect.Value
	Path      string
	Line      int
}

// MutationObserver is called synchronously before a template mutation.
type MutationObserver func(native.Env, Mutation) error

// MutationContextObserver is called synchronously before a template mutation.
type MutationContextObserver func(context.Context, Mutation) error

func (vm *VM) observeMutation(operation MutationOperation, target reflect.Value) {
	var nativeEnv *nativeEnv
	if vm.env.observeMutation != nil {
		nativeEnv = vm.nativeEnvForExposure()
	}
	vm.observeMutationWithContext(vm.nativeEnv.ctx, nativeEnv, operation, target)
}

func (vm *VM) observeMutationWithEnv(nativeEnv *nativeEnv, operation MutationOperation, target reflect.Value) {
	ctx := vm.nativeEnv.ctx
	if nativeEnv != nil {
		ctx = nativeEnv.ctx
	}
	vm.observeMutationWithContext(ctx, nativeEnv, operation, target)
}

func (vm *VM) observeMutationWithContext(
	ctx context.Context,
	nativeEnv *nativeEnv,
	operation MutationOperation,
	target reflect.Value,
) {
	if err := vm.notifyMutation(ctx, nativeEnv, operation, target); err != nil {
		panic(stopError{err})
	}
}

func (vm *VM) notifyMutation(
	ctx context.Context,
	nativeEnv *nativeEnv,
	operation MutationOperation,
	target reflect.Value,
) error {
	contextObserver := vm.env.observeMutationContext
	observer := vm.env.observeMutation
	if contextObserver == nil && observer == nil {
		return nil
	}
	mutation := Mutation{Operation: operation, Target: target}
	if info, ok := vm.fn.InstructionInfo[vm.pc-1]; ok {
		mutation.Path = info.Path
		mutation.Line = info.Position.Line
	}
	if contextObserver != nil {
		if err := contextObserver(ctx, mutation); err != nil {
			return err
		}
	}
	if observer != nil {
		vm.markBatchEnvironmentEscaped()
		return observer(nativeEnv, mutation)
	}
	return nil
}

func (vm *VM) rejectChannelOperation(operation MutationOperation, target reflect.Value) {
	var nativeEnv *nativeEnv
	if vm.env.observeMutation != nil {
		nativeEnv = vm.nativeEnvForExposure()
	}
	vm.rejectChannelOperationWithContext(vm.nativeEnv.ctx, nativeEnv, operation, target)
}

func (vm *VM) rejectChannelOperationWithEnv(nativeEnv *nativeEnv, operation MutationOperation, target reflect.Value) {
	ctx := vm.nativeEnv.ctx
	if nativeEnv != nil {
		ctx = nativeEnv.ctx
	}
	vm.rejectChannelOperationWithContext(ctx, nativeEnv, operation, target)
}

func (vm *VM) rejectChannelOperationWithContext(
	ctx context.Context,
	nativeEnv *nativeEnv,
	operation MutationOperation,
	target reflect.Value,
) {
	if vm.env.observeMutation == nil && vm.env.observeMutationContext == nil {
		return
	}
	if err := vm.notifyMutation(ctx, nativeEnv, operation, target); err != nil {
		panic(stopError{err})
	}
	panic(stopError{ErrMutationObserverChannelOperation})
}
