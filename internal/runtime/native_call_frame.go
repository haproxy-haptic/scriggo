// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"context"
	"reflect"
	goruntime "runtime"
	"sync"
	"sync/atomic"
	"time"

	"gitlab.com/haproxy-haptic/scriggo/native"
)

const (
	nativeFunctionFrameInlineInputs  = 8
	nativeFunctionFrameInlineOutputs = 8
)

type nativeFunctionFrameLocation struct {
	register registerType
	offset   int8
}

type nativeFunctionFrameLayout struct {
	inputs       [nativeFunctionFrameInlineInputs]nativeFunctionFrameLocation
	outputs      [nativeFunctionFrameInlineOutputs]nativeFunctionFrameLocation
	variadic     nativeFunctionFrameLocation
	inputsValid  bool
	outputsValid bool
}

type nativeFunctionCallSite struct {
	trampoline *native.FunctionTrampoline
	frame      *native.FunctionCallFrameSite
	metadata   *nativeFunctionFrameMetadata
}

type nativeFunctionFrameMetadata struct {
	functionType reflect.Type
	frameLayout  nativeFunctionFrameLayout
	outOff       [4]int8
}

var nativeFunctionFrameMetadataCache sync.Map

func makeNativeFunctionCallSite(trampoline *native.FunctionTrampoline) (nativeFunctionCallSite, bool) {
	if trampoline == nil {
		return nativeFunctionCallSite{}, false
	}
	value := trampoline.Value()
	if !value.IsValid() || value.Kind() != reflect.Func || value.IsNil() {
		return nativeFunctionCallSite{}, false
	}
	functionType := value.Type()
	metadata := nativeFunctionFrameMetadataFor(functionType)
	return nativeFunctionCallSite{
		trampoline: trampoline,
		frame:      trampoline.FunctionCallFrameSite(),
		metadata:   metadata,
	}, true
}

func nativeFunctionFrameMetadataFor(functionType reflect.Type) *nativeFunctionFrameMetadata {
	if cached, ok := nativeFunctionFrameMetadataCache.Load(functionType); ok {
		return cached.(*nativeFunctionFrameMetadata)
	}
	metadata := &nativeFunctionFrameMetadata{
		functionType: functionType,
		frameLayout:  makeNativeFunctionFrameLayout(functionType),
	}
	for index := range functionType.NumOut() {
		metadata.outOff[kindToType[functionType.Out(index).Kind()]]++
	}
	actual, _ := nativeFunctionFrameMetadataCache.LoadOrStore(functionType, metadata)
	return actual.(*nativeFunctionFrameMetadata)
}

func makeNativeFunctionFrameLayout(functionType reflect.Type) nativeFunctionFrameLayout {
	layout := nativeFunctionFrameLayout{
		inputsValid:  functionType.NumIn() <= nativeFunctionFrameInlineInputs,
		outputsValid: functionType.NumOut() <= nativeFunctionFrameInlineOutputs,
	}
	if layout.inputsValid {
		offsets := [4]int8{1, 1, 1, 1}
		lastFixed := functionType.NumIn()
		if functionType.IsVariadic() {
			lastFixed--
		}
		for index := range lastFixed {
			typ := functionType.In(index)
			if typ == envType {
				continue
			}
			register := kindToType[typ.Kind()]
			layout.inputs[index] = nativeFunctionFrameLocation{register: register, offset: offsets[register]}
			offsets[register]++
		}
		if functionType.IsVariadic() {
			variadicIndex := functionType.NumIn() - 1
			sliceRegister := kindToType[functionType.In(variadicIndex).Kind()]
			layout.inputs[variadicIndex] = nativeFunctionFrameLocation{
				register: sliceRegister,
				offset:   offsets[sliceRegister],
			}
			elementRegister := kindToType[functionType.In(variadicIndex).Elem().Kind()]
			layout.variadic = nativeFunctionFrameLocation{
				register: elementRegister,
				offset:   offsets[elementRegister],
			}
		}
	}
	if layout.outputsValid {
		offsets := [4]int8{1, 1, 1, 1}
		for index := range functionType.NumOut() {
			register := kindToType[functionType.Out(index).Kind()]
			layout.outputs[index] = nativeFunctionFrameLocation{register: register, offset: offsets[register]}
			offsets[register]++
		}
	}
	return layout
}

type nativeFunctionCallState struct {
	resultMu      sync.Mutex
	active        atomic.Bool
	generation    atomic.Uint64
	inFlight      atomic.Int64
	vm            *VM
	functionType  reflect.Type
	frameLayout   *nativeFunctionFrameLayout
	input         [4]Addr
	output        [4]Addr
	callContext   context.Context
	callEnv       *nativeEnv
	receiver      reflect.Value
	variadicCount int8
	results       uint64
}

var nativeFunctionCallStatePool sync.Pool
var nativeFunctionCallGeneration atomic.Uint64

func (vm *VM) beginFunctionCallFrame(
	site nativeFunctionCallSite,
	numVariadic int8,
	callContext context.Context,
	callEnv *nativeEnv,
) (*nativeFunctionCallState, uint64) {
	state, _ := nativeFunctionCallStatePool.Get().(*nativeFunctionCallState)
	if state == nil {
		state = &nativeFunctionCallState{}
	}
	generation := nativeFunctionCallGeneration.Add(1)
	if generation == 0 {
		generation = nativeFunctionCallGeneration.Add(1)
	}
	state.vm = vm
	state.functionType = site.metadata.functionType
	state.frameLayout = &site.metadata.frameLayout
	state.output = vm.fp
	state.input = vm.fp
	for register := range state.input {
		state.input[register] += Addr(site.metadata.outOff[register])
	}
	state.callContext = callContext
	state.callEnv = callEnv
	state.receiver = reflect.Value{}
	state.variadicCount = numVariadic
	state.results = 0
	state.inFlight.Store(0)
	state.generation.Store(generation)
	state.active.Store(true)
	return state, generation
}

// Draining a frame waits for accessors that are mid-call. Every accessor pairs
// acquire with a deferred release, so under normal control flow the count is
// already zero or falls within a few scheduler yields.
//
// The wait is bounded: an accessor reached from a goroutine that blocks before
// releasing would otherwise spin this thread for the life of the process, with
// no diagnostic. Failing loudly names the defect instead of hanging on it.
const (
	nativeFunctionCallFrameDrainYields = 1024
	nativeFunctionCallFrameDrainSleeps = 1000
	nativeFunctionCallFrameDrainSleep  = time.Millisecond
)

func drainNativeFunctionCallFrame(state *nativeFunctionCallState) {
	for yields := 0; state.inFlight.Load() != 0; yields++ {
		switch {
		case yields < nativeFunctionCallFrameDrainYields:
			goruntime.Gosched()
		case yields < nativeFunctionCallFrameDrainYields+nativeFunctionCallFrameDrainSleeps:
			time.Sleep(nativeFunctionCallFrameDrainSleep)
		default:
			panic("runtime: native function call frame still has accessors in flight after " +
				nativeFunctionCallFrameDrainSleep.String() +
				"-paced draining; a frame value was retained past its call")
		}
	}
}

func (state *nativeFunctionCallState) end(generation uint64) uint64 {
	if generation == 0 || state.generation.Load() != generation ||
		!state.active.CompareAndSwap(true, false) {
		panic("runtime: native function call frame was revoked")
	}
	drainNativeFunctionCallFrame(state)
	state.resultMu.Lock()
	results := state.results
	state.vm = nil
	state.functionType = nil
	state.frameLayout = nil
	state.callContext = nil
	state.callEnv = nil
	state.receiver = reflect.Value{}
	state.variadicCount = 0
	state.results = 0
	state.generation.Store(0)
	state.resultMu.Unlock()
	nativeFunctionCallStatePool.Put(state)
	return results
}

func (state *nativeFunctionCallState) acquire(generation uint64) {
	if generation == 0 || !state.active.Load() || state.generation.Load() != generation {
		panic("native: function call frame is revoked")
	}
	state.inFlight.Add(1)
	if !state.active.Load() || state.generation.Load() != generation {
		state.inFlight.Add(-1)
		panic("native: function call frame is revoked")
	}
}

func (state *nativeFunctionCallState) release() {
	state.inFlight.Add(-1)
}

func (state *nativeFunctionCallState) FunctionCallFrameReceiver(generation uint64) reflect.Value {
	state.acquire(generation)
	defer state.release()
	return state.receiver
}

func (state *nativeFunctionCallState) setReceiver(generation uint64, receiver reflect.Value) {
	if !state.active.Load() || state.generation.Load() != generation {
		panic("native: function call frame is revoked")
	}
	state.receiver = receiver
}

func (state *nativeFunctionCallState) FunctionCallFrameValid(generation uint64) bool {
	return generation != 0 && state.active.Load() && state.generation.Load() == generation
}

func functionCallArgument(
	state *nativeFunctionCallState,
	argumentIndex int,
	variadicIndex int,
) (registerType, Addr) {
	typ := state.functionType
	if state.frameLayout != nil && state.frameLayout.inputsValid && typ.In(argumentIndex) != envType {
		if typ.IsVariadic() && state.variadicCount != NoVariadicArgs && argumentIndex == typ.NumIn()-1 {
			location := state.frameLayout.variadic
			return location.register, state.input[location.register] + Addr(location.offset) + Addr(variadicIndex)
		}
		location := state.frameLayout.inputs[argumentIndex]
		return location.register, state.input[location.register] + Addr(location.offset)
	}
	argumentType := typ.In(argumentIndex)
	lastNonVariadic := typ.NumIn()
	if typ.IsVariadic() && state.variadicCount != NoVariadicArgs {
		lastNonVariadic--
		if argumentIndex == lastNonVariadic {
			argumentType = argumentType.Elem()
		}
	}
	register := kindToType[argumentType.Kind()]
	offset := Addr(1)
	for index := 0; index < min(argumentIndex, lastNonVariadic); index++ {
		if typ.In(index) != envType && kindToType[typ.In(index).Kind()] == register {
			offset++
		}
	}
	if variadicIndex >= 0 {
		offset += Addr(variadicIndex)
	}
	return register, state.input[register] + offset
}

func functionCallResult(state *nativeFunctionCallState, resultIndex int) (registerType, Addr) {
	if state.frameLayout != nil && state.frameLayout.outputsValid {
		location := state.frameLayout.outputs[resultIndex]
		return location.register, state.output[location.register] + Addr(location.offset)
	}
	resultType := state.functionType.Out(resultIndex)
	register := kindToType[resultType.Kind()]
	offset := Addr(1)
	for index := 0; index < resultIndex; index++ {
		if kindToType[state.functionType.Out(index).Kind()] == register {
			offset++
		}
	}
	return register, state.output[register] + offset
}

func (state *nativeFunctionCallState) FunctionCallFrameEnv(generation uint64, argumentIndex int) native.Env {
	state.acquire(generation)
	defer state.release()
	if state.functionType.In(argumentIndex) != envType || state.callEnv == nil {
		panic("native: function call environment is unavailable")
	}
	state.vm.prepareEnvCallPathFor(state.callEnv)
	return state.callEnv
}

func (state *nativeFunctionCallState) FunctionCallFrameBool(generation uint64, argumentIndex int, variadicIndex int) bool {
	state.acquire(generation)
	defer state.release()
	_, address := functionCallArgument(state, argumentIndex, variadicIndex)
	return state.vm.regs.int[address] > 0
}

func (state *nativeFunctionCallState) FunctionCallFrameInt(generation uint64, argumentIndex int, variadicIndex int) int64 {
	state.acquire(generation)
	defer state.release()
	_, address := functionCallArgument(state, argumentIndex, variadicIndex)
	return state.vm.regs.int[address]
}

func (state *nativeFunctionCallState) FunctionCallFrameUint(generation uint64, argumentIndex int, variadicIndex int) uint64 {
	return uint64(state.FunctionCallFrameInt(generation, argumentIndex, variadicIndex))
}

func (state *nativeFunctionCallState) FunctionCallFrameFloat(generation uint64, argumentIndex int, variadicIndex int) float64 {
	state.acquire(generation)
	defer state.release()
	_, address := functionCallArgument(state, argumentIndex, variadicIndex)
	return state.vm.regs.float[address]
}

func (state *nativeFunctionCallState) FunctionCallFrameString(generation uint64, argumentIndex int, variadicIndex int) string {
	state.acquire(generation)
	defer state.release()
	_, address := functionCallArgument(state, argumentIndex, variadicIndex)
	return state.vm.regs.string[address]
}

func (state *nativeFunctionCallState) FunctionCallFrameValue(
	generation uint64,
	argumentIndex int,
	variadicIndex int,
	typ reflect.Type,
) reflect.Value {
	state.acquire(generation)
	defer state.release()
	register, address := functionCallArgument(state, argumentIndex, variadicIndex)
	vm := state.vm
	var value reflect.Value
	switch register {
	case intRegister:
		switch typ.Kind() {
		case reflect.Bool:
			value = reflect.ValueOf(vm.regs.int[address] > 0)
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			value = reflect.ValueOf(vm.regs.int[address])
		default:
			value = reflect.ValueOf(uint64(vm.regs.int[address]))
		}
	case floatRegister:
		value = reflect.ValueOf(vm.regs.float[address])
	case stringRegister:
		value = reflect.ValueOf(vm.regs.string[address])
	case generalRegister:
		value = vm.regs.general[address]
		if typ.Kind() == reflect.Func {
			vm.markBatchGlobalsEscaped()
			return value.Interface().(*callable).Value(vm.env, state.callContext)
		}
		if !value.IsValid() && typ.Kind() == reflect.Interface {
			return reflect.Zero(typ)
		}
		vm.markBatchValueEscaped(value)
		return value
	}
	if value.Type() != typ {
		value = value.Convert(typ)
	}
	return value
}

func (state *nativeFunctionCallState) FunctionCallFrameSliceLen(
	generation uint64,
	argumentIndex int,
) int {
	state.acquire(generation)
	defer state.release()
	_, address := functionCallArgument(state, argumentIndex, -1)
	return state.vm.regs.general[address].Len()
}

func (state *nativeFunctionCallState) FunctionCallFrameSliceValue(
	generation uint64,
	argumentIndex int,
	index int,
	elementType reflect.Type,
) reflect.Value {
	state.acquire(generation)
	defer state.release()
	_, address := functionCallArgument(state, argumentIndex, -1)
	slice := state.vm.regs.general[address]
	if index < 0 || index >= slice.Len() {
		panic("native: variadic argument index is out of range")
	}
	value := slice.Index(index)
	if elementType.Kind() == reflect.Interface {
		if value.IsNil() {
			return reflect.Zero(elementType)
		}
		value = value.Elem()
	}
	state.vm.markBatchValueEscaped(value)
	return value
}

func (state *nativeFunctionCallState) lockResult(index int) (*VM, registerType, Addr) {
	state.resultMu.Lock()
	register, address := functionCallResult(state, index)
	mask := uint64(1) << index
	if state.results&mask != 0 {
		state.resultMu.Unlock()
		panic("native: function call result was set twice")
	}
	state.results |= mask
	return state.vm, register, address
}

func (state *nativeFunctionCallState) FunctionCallFrameSetBool(generation uint64, index int, value bool) {
	state.acquire(generation)
	defer state.release()
	vm, register, address := state.lockResult(index)
	defer state.resultMu.Unlock()
	if value {
		vm.regs.int[address] = 1
	} else {
		vm.regs.int[address] = 0
	}
	vm.noteHigh(int(register), address)
}

func (state *nativeFunctionCallState) FunctionCallFrameSetInt(generation uint64, index int, value int64) {
	state.acquire(generation)
	defer state.release()
	vm, register, address := state.lockResult(index)
	defer state.resultMu.Unlock()
	vm.regs.int[address] = value
	vm.noteHigh(int(register), address)
}

func (state *nativeFunctionCallState) FunctionCallFrameSetUint(generation uint64, index int, value uint64) {
	state.FunctionCallFrameSetInt(generation, index, int64(value))
}

func (state *nativeFunctionCallState) FunctionCallFrameSetFloat(generation uint64, index int, value float64) {
	state.acquire(generation)
	defer state.release()
	vm, register, address := state.lockResult(index)
	defer state.resultMu.Unlock()
	vm.regs.float[address] = value
	vm.noteHigh(int(register), address)
}

func (state *nativeFunctionCallState) FunctionCallFrameSetString(generation uint64, index int, value string) {
	state.acquire(generation)
	defer state.release()
	vm, register, address := state.lockResult(index)
	defer state.resultMu.Unlock()
	vm.regs.string[address] = value
	vm.noteHigh(int(register), address)
}

func (state *nativeFunctionCallState) FunctionCallFrameSetValue(generation uint64, index int, value reflect.Value) {
	state.acquire(generation)
	defer state.release()
	vm, register, address := state.lockResult(index)
	defer state.resultMu.Unlock()
	resultType := state.functionType.Out(index)
	switch register {
	case intRegister:
		switch resultType.Kind() {
		case reflect.Bool:
			if value.Bool() {
				vm.regs.int[address] = 1
			} else {
				vm.regs.int[address] = 0
			}
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			vm.regs.int[address] = value.Int()
		default:
			vm.regs.int[address] = int64(value.Uint())
		}
	case floatRegister:
		vm.regs.float[address] = value.Float()
	case stringRegister:
		vm.regs.string[address] = value.String()
	case generalRegister:
		if resultType.Kind() == reflect.Interface {
			if value.Kind() == reflect.Interface {
				if value.IsNil() {
					vm.regs.general[address] = reflect.Value{}
				} else {
					vm.regs.general[address] = value.Elem()
				}
			} else {
				vm.regs.general[address] = value
			}
		} else if resultType.Kind() == reflect.Func {
			vm.regs.general[address] = reflect.ValueOf(newNativeCallable(value))
		} else {
			vm.regs.general[address] = value
		}
	}
	vm.noteHigh(int(register), address)
}

func functionCallResultMask(count int) uint64 {
	if count == 64 {
		return ^uint64(0)
	}
	return uint64(1)<<count - 1
}

func (vm *VM) functionCallFrameSite(fn *NativeFunction) (nativeFunctionCallSite, bool) {
	var site nativeFunctionCallSite
	var found bool
	if fn.boundReceiver.IsValid() {
		key := nativeMethodTrampolineKey{
			receiver: fn.boundReceiver.Type(),
			method:   fn.name,
			function: fn.value.Type(),
		}
		site, found = vm.env.methodTrampolines[key]
		if !found {
			site, found = interfaceMethodFrameCallSite(vm.env.methodTrampolines, key)
		}
	} else if fn.methodReceiver >= 0 && fn.methodReceiver < fn.value.Type().NumIn() {
		receiverType := fn.value.Type().In(fn.methodReceiver)
		site, found = vm.env.methodTrampolines[nativeMethodTrampolineKey{
			receiver: receiverType,
			method:   fn.name,
			function: fn.value.Type(),
		}]
	} else {
		site, found = vm.env.nativeTrampolines[fn.identity]
	}
	if !found || site.frame == nil || site.metadata == nil ||
		site.metadata.functionType != fn.value.Type() || site.metadata.functionType.NumOut() > 64 {
		return nativeFunctionCallSite{}, false
	}
	return site, true
}

func interfaceMethodFrameCallSite(
	sites map[nativeMethodTrampolineKey]nativeFunctionCallSite,
	call nativeMethodTrampolineKey,
) (nativeFunctionCallSite, bool) {
	var matched nativeFunctionCallSite
	found := false
	for key, site := range sites {
		if key.receiver.Kind() != reflect.Interface || key.method != call.method ||
			key.function != call.function || !call.receiver.Implements(key.receiver) {
			continue
		}
		if found && matched.trampoline != site.trampoline {
			return nativeFunctionCallSite{}, false
		}
		matched = site
		found = true
	}
	return matched, found
}

func (vm *VM) methodFrameCallSite(
	receiver reflect.Value,
	method string,
	expectedType reflect.Type,
) (nativeFunctionCallSite, bool) {
	if !receiver.IsValid() || method == "" || expectedType == nil || expectedType.Kind() != reflect.Func {
		return nativeFunctionCallSite{}, false
	}
	key := nativeMethodTrampolineKey{
		receiver: receiver.Type(),
		method:   method,
		function: expectedType,
	}
	site, found := vm.env.methodTrampolines[key]
	if !found {
		site, found = interfaceMethodFrameCallSite(vm.env.methodTrampolines, key)
	}
	if !found || site.frame == nil || site.metadata == nil ||
		site.metadata.functionType != expectedType || expectedType.NumOut() > 64 {
		return nativeFunctionCallSite{}, false
	}
	return site, true
}

func (vm *VM) callNativeMethodFrame(
	receiver reflect.Value,
	method string,
	expectedType reflect.Type,
	numVariadic int8,
	shift StackShift,
) bool {
	site, found := vm.methodFrameCallSite(receiver, method, expectedType)
	if !found {
		return false
	}
	recordNativeFrameCall(nativeFrameCallInterfaceMethod, true)
	callContext := vm.nativeEnv.ctx
	needsEnvironment := nativeFunctionAcceptsEnvironment(expectedType) || vm.env.beforeNativeCall != nil ||
		vm.env.observeNativeCall != nil
	var callEnv *nativeEnv
	if needsEnvironment {
		callEnv = vm.nativeEnvForExposure()
		callContext = callEnv.ctx
	}
	vm.beforeNativeCallWithContext(callContext, callEnv)

	profileNative := vm.profiler != nil
	if profileNative {
		vm.profiler.enterCall(method, "", "", vm.currentLine(), ProfileCallKindNativeFunc, "")
	}

	fp := vm.fp
	for register := range vm.fp {
		vm.fp[register] += Addr(shift[register])
	}
	fn := NativeFunction{name: method, methodReceiver: -1, boundReceiver: receiver}
	vm.callNativeFunctionFrame(&fn, site, numVariadic, callContext, callEnv)
	vm.fp = fp
	if profileNative {
		vm.profiler.exitCall()
	}
	return true
}

func (vm *VM) callNativeFunctionFrame(
	fn *NativeFunction,
	site nativeFunctionCallSite,
	numVariadic int8,
	callContext context.Context,
	callEnv *nativeEnv,
) {
	state, generation := vm.beginFunctionCallFrame(site, numVariadic, callContext, callEnv)
	ended := false
	defer func() {
		if !ended {
			state.end(generation)
		}
	}()
	var receiver reflect.Value
	if fn != nil && fn.boundReceiver.IsValid() {
		receiver = fn.boundReceiver
	} else if fn != nil && fn.methodReceiver >= 0 {
		receiver = state.FunctionCallFrameValue(
			generation,
			fn.methodReceiver,
			-1,
			site.metadata.functionType.In(fn.methodReceiver),
		)
	}
	state.setReceiver(generation, receiver)
	if receiver.IsValid() {
		vm.observeNativeCallWithContext(callContext, callEnv, receiver, fn.name, false)
	}
	site.frame.Invoke(state, generation, int(numVariadic))
	results := state.end(generation)
	ended = true
	if results != functionCallResultMask(site.metadata.functionType.NumOut()) {
		panic("native: function call frame did not set every result")
	}
}
