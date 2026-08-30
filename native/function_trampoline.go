// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package native

import (
	"fmt"
	"reflect"
)

// FunctionTrampoline associates an exact function value with a direct implementation.
type FunctionTrampoline struct {
	value          reflect.Value
	call           func([]reflect.Value) []reflect.Value
	callFrame      func(FunctionCallFrame)
	frameSite      *FunctionCallFrameSite
	seal           *FunctionTrampoline
	result         []reflect.Type
	methodReceiver reflect.Type
	methodName     string
}

// FunctionCallFrameSite is an immutable, pre-authenticated frame invocation target.
type FunctionCallFrameSite struct {
	functionType reflect.Type
	call         func(FunctionCallFrame)
	seal         *FunctionCallFrameSite
}

// MakeFunctionTrampolineWithFrame adds a VM-register call path to a function trampoline.
func MakeFunctionTrampolineWithFrame(
	typ reflect.Type,
	call func([]reflect.Value) []reflect.Value,
	callFrame func(FunctionCallFrame),
) *FunctionTrampoline {
	if callFrame == nil {
		panic("reflect: call of MakeFunctionTrampolineWithFrame with invalid arguments")
	}
	trampoline := MakeFunctionTrampoline(typ, call)
	trampoline.callFrame = callFrame
	trampoline.frameSite = newFunctionCallFrameSite(typ, callFrame)
	return trampoline
}

// MakeFunctionTrampoline creates a function value and a direct invocation path for it.
func MakeFunctionTrampoline(
	typ reflect.Type,
	call func([]reflect.Value) []reflect.Value,
) *FunctionTrampoline {
	if typ == nil || typ.Kind() != reflect.Func || call == nil {
		panic("reflect: call of MakeFunctionTrampoline with invalid arguments")
	}
	trampoline := newFunctionTrampoline(typ, call)
	trampoline.value = reflect.MakeFunc(typ, func(args []reflect.Value) []reflect.Value {
		if trampoline.seal != trampoline {
			panic("reflect: invalid native function trampoline")
		}
		return trampoline.call(args)
	})
	return trampoline
}

// MakeFunctionTrampolineFor creates a direct invocation path for an existing function value.
func MakeFunctionTrampolineFor(
	value reflect.Value,
	call func([]reflect.Value) []reflect.Value,
) *FunctionTrampoline {
	if !value.IsValid() || value.Kind() != reflect.Func || value.IsNil() ||
		!value.CanInterface() || call == nil {
		panic("reflect: call of MakeFunctionTrampolineFor with invalid arguments")
	}
	trampoline := newFunctionTrampoline(value.Type(), call)
	trampoline.value = reflect.ValueOf(value.Interface())
	return trampoline
}

// MakeFunctionTrampolineForWithFrame adds a VM-register call path for an existing function value.
func MakeFunctionTrampolineForWithFrame(
	value reflect.Value,
	call func([]reflect.Value) []reflect.Value,
	callFrame func(FunctionCallFrame),
) *FunctionTrampoline {
	if callFrame == nil {
		panic("reflect: call of MakeFunctionTrampolineForWithFrame with invalid arguments")
	}
	trampoline := MakeFunctionTrampolineFor(value, call)
	trampoline.callFrame = callFrame
	trampoline.frameSite = newFunctionCallFrameSite(value.Type(), callFrame)
	return trampoline
}

// MakeMethodTrampoline creates a direct invocation path for an exact method target.
func MakeMethodTrampoline(
	receiver reflect.Type,
	methodName string,
	call func([]reflect.Value) []reflect.Value,
) *FunctionTrampoline {
	if receiver == nil || methodName == "" || call == nil {
		panic("reflect: call of MakeMethodTrampoline with invalid arguments")
	}
	method, found := receiver.MethodByName(methodName)
	if !found || method.Type == nil || method.Type.Kind() != reflect.Func {
		panic("reflect: call of MakeMethodTrampoline with invalid method")
	}
	trampoline := newFunctionTrampoline(method.Type, call)
	if method.Func.IsValid() {
		trampoline.value = reflect.ValueOf(method.Func.Interface())
	} else {
		trampoline.value = reflect.MakeFunc(method.Type, call)
	}
	trampoline.methodReceiver = receiver
	trampoline.methodName = methodName
	return trampoline
}

// MakeMethodTrampolineWithFrame adds a VM-register call path for an exact method target.
func MakeMethodTrampolineWithFrame(
	receiver reflect.Type,
	methodName string,
	call func([]reflect.Value) []reflect.Value,
	callFrame func(FunctionCallFrame),
) *FunctionTrampoline {
	if callFrame == nil {
		panic("reflect: call of MakeMethodTrampolineWithFrame with invalid arguments")
	}
	trampoline := MakeMethodTrampoline(receiver, methodName, call)
	trampoline.callFrame = callFrame
	trampoline.frameSite = newFunctionCallFrameSite(trampoline.value.Type(), callFrame)
	return trampoline
}

func newFunctionCallFrameSite(
	functionType reflect.Type,
	call func(FunctionCallFrame),
) *FunctionCallFrameSite {
	site := &FunctionCallFrameSite{functionType: functionType, call: call}
	site.seal = site
	return site
}

func newFunctionTrampoline(
	typ reflect.Type,
	call func([]reflect.Value) []reflect.Value,
) *FunctionTrampoline {
	trampoline := &FunctionTrampoline{
		call:   call,
		result: make([]reflect.Type, typ.NumOut()),
	}
	for index := range typ.NumOut() {
		trampoline.result[index] = typ.Out(index)
	}
	trampoline.seal = trampoline
	return trampoline
}

// Value returns the function value represented by trampoline.
func (trampoline *FunctionTrampoline) Value() reflect.Value {
	if !trampoline.valid() {
		return reflect.Value{}
	}
	return trampoline.value
}

// Call invokes the implementation without reflect.Value.Call.
func (trampoline *FunctionTrampoline) Call(args []reflect.Value) []reflect.Value {
	if !trampoline.valid() {
		panic("reflect: invalid native function trampoline")
	}
	results := trampoline.call(args)
	if len(results) != len(trampoline.result) {
		panic("reflect: wrong return count from function created by MakeFunc")
	}
	for index, result := range results {
		if !result.IsValid() {
			panic("reflect: function created by MakeFunc using closure returned zero Value")
		}
		if !result.Type().AssignableTo(trampoline.result[index]) {
			panic(fmt.Sprintf(
				"reflect.MakeFunc: value of type %s is not assignable to type %s",
				result.Type(),
				trampoline.result[index],
			))
		}
	}
	return results
}

// SupportsFunctionCallFrame reports whether trampoline has a VM-register call path.
func (trampoline *FunctionTrampoline) SupportsFunctionCallFrame() bool {
	return trampoline.valid() && trampoline.callFrame != nil && trampoline.frameSite.valid()
}

// FunctionCallFrameSite returns the immutable frame target after validating the trampoline.
func (trampoline *FunctionTrampoline) FunctionCallFrameSite() *FunctionCallFrameSite {
	if !trampoline.SupportsFunctionCallFrame() {
		return nil
	}
	return trampoline.frameSite
}

// Type returns the exact function type accepted by the site.
func (site *FunctionCallFrameSite) Type() reflect.Type {
	if !site.valid() {
		return nil
	}
	return site.functionType
}

// Invoke binds the active runtime access and calls the authenticated frame implementation.
func (site *FunctionCallFrameSite) Invoke(
	access FunctionCallFrameAccess,
	generation uint64,
	variadicCount int,
) {
	if !site.valid() || access == nil || generation == 0 {
		panic("native: invalid function call frame site")
	}
	site.call(newFunctionCallFrame(access, site.functionType, generation, variadicCount))
}

func (site *FunctionCallFrameSite) valid() bool {
	return site != nil && site.seal == site && site.functionType != nil &&
		site.functionType.Kind() == reflect.Func && site.call != nil
}

// NewFunctionCallFrame binds an active runtime call to this trampoline's exact type.
func (trampoline *FunctionTrampoline) NewFunctionCallFrame(
	access FunctionCallFrameAccess,
	generation uint64,
	variadicCount int,
) FunctionCallFrame {
	if !trampoline.SupportsFunctionCallFrame() || access == nil || generation == 0 {
		panic("native: invalid function call frame")
	}
	return newFunctionCallFrame(access, trampoline.value.Type(), generation, variadicCount)
}

// CallFrame invokes the VM-register implementation of trampoline.
func (trampoline *FunctionTrampoline) CallFrame(frame FunctionCallFrame) {
	if !trampoline.SupportsFunctionCallFrame() || !frame.valid() ||
		frame.functionType != trampoline.value.Type() {
		panic("native: invalid function call frame")
	}
	trampoline.callFrame(frame)
}

// MethodTarget reports the exact method receiver and name for a method trampoline.
func (trampoline *FunctionTrampoline) MethodTarget() (reflect.Type, string, bool) {
	if !trampoline.valid() || trampoline.methodReceiver == nil || trampoline.methodName == "" {
		return nil, "", false
	}
	return trampoline.methodReceiver, trampoline.methodName, true
}

func (trampoline *FunctionTrampoline) valid() bool {
	return trampoline != nil && trampoline.seal == trampoline &&
		trampoline.value.IsValid() && trampoline.value.Kind() == reflect.Func &&
		!trampoline.value.IsNil() && trampoline.value.CanInterface() &&
		trampoline.call != nil && len(trampoline.result) == trampoline.value.Type().NumOut()
}
