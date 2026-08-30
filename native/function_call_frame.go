// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package native

import "reflect"

// FunctionCallFrameAccess connects a FunctionCallFrame to one active runtime call.
type FunctionCallFrameAccess interface {
	FunctionCallFrameValid(uint64) bool
	FunctionCallFrameReceiver(uint64) reflect.Value
	FunctionCallFrameEnv(uint64, int) Env
	FunctionCallFrameBool(uint64, int, int) bool
	FunctionCallFrameInt(uint64, int, int) int64
	FunctionCallFrameUint(uint64, int, int) uint64
	FunctionCallFrameFloat(uint64, int, int) float64
	FunctionCallFrameString(uint64, int, int) string
	FunctionCallFrameValue(uint64, int, int, reflect.Type) reflect.Value
	FunctionCallFrameSliceLen(uint64, int) int
	FunctionCallFrameSliceValue(uint64, int, int, reflect.Type) reflect.Value
	FunctionCallFrameSetBool(uint64, int, bool)
	FunctionCallFrameSetInt(uint64, int, int64)
	FunctionCallFrameSetUint(uint64, int, uint64)
	FunctionCallFrameSetFloat(uint64, int, float64)
	FunctionCallFrameSetString(uint64, int, string)
	FunctionCallFrameSetValue(uint64, int, reflect.Value)
}

// FunctionCallFrame provides allocation-free access to one native call's VM registers.
type FunctionCallFrame struct {
	access        FunctionCallFrameAccess
	functionType  reflect.Type
	generation    uint64
	variadicCount int
}

// FunctionCallArguments provides revoked-frame-checked access to variadic arguments.
type FunctionCallArguments struct {
	frame FunctionCallFrame
	count int
}

func newFunctionCallFrame(
	access FunctionCallFrameAccess,
	functionType reflect.Type,
	generation uint64,
	variadicCount int,
) FunctionCallFrame {
	return FunctionCallFrame{
		access:        access,
		functionType:  functionType,
		generation:    generation,
		variadicCount: variadicCount,
	}
}

func (frame FunctionCallFrame) valid() bool {
	return frame.access != nil && frame.functionType != nil && frame.functionType.Kind() == reflect.Func &&
		frame.generation != 0 && frame.access.FunctionCallFrameValid(frame.generation)
}

func (frame FunctionCallFrame) requireValid() {
	if !frame.valid() {
		panic("native: function call frame is revoked")
	}
}

func (frame FunctionCallFrame) argumentType(index int) reflect.Type {
	frame.requireValid()
	if index < 0 || index >= frame.functionType.NumIn() {
		panic("native: function call argument index is out of range")
	}
	if frame.functionType.IsVariadic() && frame.variadicCount >= 0 && index == frame.functionType.NumIn()-1 {
		panic("native: expanded variadic argument has no fixed value")
	}
	return frame.functionType.In(index)
}

func (frame FunctionCallFrame) variadicType(index int) reflect.Type {
	frame.requireValid()
	if !frame.functionType.IsVariadic() || frame.variadicCount < 0 {
		panic("native: function call does not expand variadic arguments")
	}
	if index < 0 || index >= frame.variadicCount {
		panic("native: variadic argument index is out of range")
	}
	return frame.functionType.In(frame.functionType.NumIn() - 1).Elem()
}

func (frame FunctionCallFrame) resultType(index int) reflect.Type {
	frame.requireValid()
	if index < 0 || index >= frame.functionType.NumOut() {
		panic("native: function call result index is out of range")
	}
	return frame.functionType.Out(index)
}

// Type returns the exact function type invoked by this frame.
func (frame FunctionCallFrame) Type() reflect.Type {
	frame.requireValid()
	return frame.functionType
}

// Receiver returns the bound receiver of a method-value call.
func (frame FunctionCallFrame) Receiver() reflect.Value {
	frame.requireValid()
	return frame.access.FunctionCallFrameReceiver(frame.generation)
}

// VariadicLen returns the number of expanded variadic arguments, or -1 when a slice was passed directly.
func (frame FunctionCallFrame) VariadicLen() int {
	frame.requireValid()
	if !frame.functionType.IsVariadic() {
		panic("native: function call is not variadic")
	}
	return frame.variadicCount
}

// VariadicArguments returns a view over expanded or directly passed variadic arguments.
func (frame FunctionCallFrame) VariadicArguments() FunctionCallArguments {
	return FunctionCallArguments{frame: frame, count: frame.VariadicLen()}
}

// Len returns the number of arguments in the view.
func (arguments FunctionCallArguments) Len() int {
	arguments.frame.requireValid()
	if arguments.count >= 0 {
		return arguments.count
	}
	return arguments.frame.access.FunctionCallFrameSliceLen(
		arguments.frame.generation,
		arguments.frame.functionType.NumIn()-1,
	)
}

// Value returns an argument as a reflect.Value of the variadic element type.
func (arguments FunctionCallArguments) Value(index int) reflect.Value {
	arguments.frame.requireValid()
	elementType := arguments.frame.functionType.In(arguments.frame.functionType.NumIn() - 1).Elem()
	return arguments.value(index, elementType)
}

func (arguments FunctionCallArguments) value(index int, elementType reflect.Type) reflect.Value {
	if arguments.count >= 0 {
		if index < 0 || index >= arguments.count {
			panic("native: variadic argument index is out of range")
		}
		return arguments.frame.access.FunctionCallFrameValue(
			arguments.frame.generation,
			arguments.frame.functionType.NumIn()-1,
			index,
			elementType,
		)
	}
	return arguments.frame.access.FunctionCallFrameSliceValue(
		arguments.frame.generation,
		arguments.frame.functionType.NumIn()-1,
		index,
		elementType,
	)
}

// FunctionCallArgumentAt returns a variadic-view argument with its exact element type.
func FunctionCallArgumentAt[T any](arguments FunctionCallArguments, index int) T {
	arguments.frame.requireValid()
	elementType := arguments.frame.functionType.In(arguments.frame.functionType.NumIn() - 1).Elem()
	if elementType != reflect.TypeFor[T]() {
		panic("native: variadic argument has an incompatible static type")
	}
	return functionCallFrameTypeAssert[T](arguments.value(index, elementType), "variadic argument")
}

// ArgEnv returns an Env argument.
func (frame FunctionCallFrame) ArgEnv(index int) Env {
	if frame.argumentType(index) != reflect.TypeFor[Env]() {
		panic("native: function call argument is not an Env")
	}
	return frame.access.FunctionCallFrameEnv(frame.generation, index)
}

// ArgBool returns a bool argument.
func (frame FunctionCallFrame) ArgBool(index int) bool {
	if frame.argumentType(index).Kind() != reflect.Bool {
		panic("native: function call argument is not a bool")
	}
	return frame.access.FunctionCallFrameBool(frame.generation, index, -1)
}

// ArgInt returns a signed integer argument.
func (frame FunctionCallFrame) ArgInt(index int) int64 {
	kind := frame.argumentType(index).Kind()
	if kind < reflect.Int || kind > reflect.Int64 {
		panic("native: function call argument is not a signed integer")
	}
	return frame.access.FunctionCallFrameInt(frame.generation, index, -1)
}

// ArgUint returns an unsigned integer argument.
func (frame FunctionCallFrame) ArgUint(index int) uint64 {
	kind := frame.argumentType(index).Kind()
	if kind < reflect.Uint || kind > reflect.Uintptr {
		panic("native: function call argument is not an unsigned integer")
	}
	return frame.access.FunctionCallFrameUint(frame.generation, index, -1)
}

// ArgFloat returns a floating-point argument.
func (frame FunctionCallFrame) ArgFloat(index int) float64 {
	kind := frame.argumentType(index).Kind()
	if kind != reflect.Float32 && kind != reflect.Float64 {
		panic("native: function call argument is not a float")
	}
	return frame.access.FunctionCallFrameFloat(frame.generation, index, -1)
}

// ArgString returns a string argument.
func (frame FunctionCallFrame) ArgString(index int) string {
	if frame.argumentType(index).Kind() != reflect.String {
		panic("native: function call argument is not a string")
	}
	return frame.access.FunctionCallFrameString(frame.generation, index, -1)
}

// ArgValue returns an argument as a reflect.Value of its declared type.
func (frame FunctionCallFrame) ArgValue(index int) reflect.Value {
	typ := frame.argumentType(index)
	return frame.access.FunctionCallFrameValue(frame.generation, index, -1, typ)
}

// FunctionCallFrameArg returns an argument with its exact declared type.
func FunctionCallFrameArg[T any](frame FunctionCallFrame, index int) T {
	typ := frame.argumentType(index)
	if typ != reflect.TypeFor[T]() {
		panic("native: function call argument has an incompatible static type")
	}
	value := frame.access.FunctionCallFrameValue(frame.generation, index, -1, typ)
	return functionCallFrameTypeAssert[T](value, "argument")
}

// VariadicBool returns an expanded bool argument.
func (frame FunctionCallFrame) VariadicBool(index int) bool {
	if frame.variadicType(index).Kind() != reflect.Bool {
		panic("native: variadic argument is not a bool")
	}
	return frame.access.FunctionCallFrameBool(frame.generation, frame.functionType.NumIn()-1, index)
}

// VariadicInt returns an expanded signed integer argument.
func (frame FunctionCallFrame) VariadicInt(index int) int64 {
	kind := frame.variadicType(index).Kind()
	if kind < reflect.Int || kind > reflect.Int64 {
		panic("native: variadic argument is not a signed integer")
	}
	return frame.access.FunctionCallFrameInt(frame.generation, frame.functionType.NumIn()-1, index)
}

// VariadicUint returns an expanded unsigned integer argument.
func (frame FunctionCallFrame) VariadicUint(index int) uint64 {
	kind := frame.variadicType(index).Kind()
	if kind < reflect.Uint || kind > reflect.Uintptr {
		panic("native: variadic argument is not an unsigned integer")
	}
	return frame.access.FunctionCallFrameUint(frame.generation, frame.functionType.NumIn()-1, index)
}

// VariadicFloat returns an expanded floating-point argument.
func (frame FunctionCallFrame) VariadicFloat(index int) float64 {
	kind := frame.variadicType(index).Kind()
	if kind != reflect.Float32 && kind != reflect.Float64 {
		panic("native: variadic argument is not a float")
	}
	return frame.access.FunctionCallFrameFloat(frame.generation, frame.functionType.NumIn()-1, index)
}

// VariadicString returns an expanded string argument.
func (frame FunctionCallFrame) VariadicString(index int) string {
	if frame.variadicType(index).Kind() != reflect.String {
		panic("native: variadic argument is not a string")
	}
	return frame.access.FunctionCallFrameString(frame.generation, frame.functionType.NumIn()-1, index)
}

// VariadicValue returns an expanded argument as a reflect.Value of its declared element type.
func (frame FunctionCallFrame) VariadicValue(index int) reflect.Value {
	typ := frame.variadicType(index)
	return frame.access.FunctionCallFrameValue(
		frame.generation,
		frame.functionType.NumIn()-1,
		index,
		typ,
	)
}

// FunctionCallFrameVariadicArg returns an expanded variadic argument with its exact element type.
func FunctionCallFrameVariadicArg[T any](frame FunctionCallFrame, index int) T {
	typ := frame.variadicType(index)
	if typ != reflect.TypeFor[T]() {
		panic("native: variadic argument has an incompatible static type")
	}
	value := frame.access.FunctionCallFrameValue(
		frame.generation,
		frame.functionType.NumIn()-1,
		index,
		typ,
	)
	return functionCallFrameTypeAssert[T](value, "variadic argument")
}

// FunctionCallFrameReceiver returns the receiver assignable to T.
func FunctionCallFrameReceiver[T any](frame FunctionCallFrame) T {
	value := frame.Receiver()
	return functionCallFrameTypeAssert[T](value, "receiver")
}

func functionCallFrameTypeAssert[T any](value reflect.Value, kind string) T {
	if value.IsValid() && value.Kind() == reflect.Interface && value.IsNil() {
		var zero T
		return zero
	}
	result, ok := reflect.TypeAssert[T](value)
	if !ok {
		panic("native: function call " + kind + " has an incompatible dynamic type")
	}
	return result
}

// SetResultBool stores a bool result.
func (frame FunctionCallFrame) SetResultBool(index int, value bool) {
	if frame.resultType(index).Kind() != reflect.Bool {
		panic("native: function call result is not a bool")
	}
	frame.access.FunctionCallFrameSetBool(frame.generation, index, value)
}

// SetResultInt stores a signed integer result.
func (frame FunctionCallFrame) SetResultInt(index int, value int64) {
	kind := frame.resultType(index).Kind()
	if kind < reflect.Int || kind > reflect.Int64 {
		panic("native: function call result is not a signed integer")
	}
	frame.access.FunctionCallFrameSetInt(frame.generation, index, value)
}

// SetResultUint stores an unsigned integer result.
func (frame FunctionCallFrame) SetResultUint(index int, value uint64) {
	kind := frame.resultType(index).Kind()
	if kind < reflect.Uint || kind > reflect.Uintptr {
		panic("native: function call result is not an unsigned integer")
	}
	frame.access.FunctionCallFrameSetUint(frame.generation, index, value)
}

// SetResultFloat stores a floating-point result.
func (frame FunctionCallFrame) SetResultFloat(index int, value float64) {
	kind := frame.resultType(index).Kind()
	if kind != reflect.Float32 && kind != reflect.Float64 {
		panic("native: function call result is not a float")
	}
	frame.access.FunctionCallFrameSetFloat(frame.generation, index, value)
}

// SetResultString stores a string result.
func (frame FunctionCallFrame) SetResultString(index int, value string) {
	if frame.resultType(index).Kind() != reflect.String {
		panic("native: function call result is not a string")
	}
	frame.access.FunctionCallFrameSetString(frame.generation, index, value)
}

// SetResultValue stores a result assignable to its declared type.
func (frame FunctionCallFrame) SetResultValue(index int, value reflect.Value) {
	typ := frame.resultType(index)
	if !value.IsValid() {
		panic("native: function call result is invalid")
	}
	if !value.Type().AssignableTo(typ) {
		panic("native: function call result has an incompatible type")
	}
	frame.access.FunctionCallFrameSetValue(frame.generation, index, value)
}

// SetResultZero stores the zero value of a result's declared type.
func (frame FunctionCallFrame) SetResultZero(index int) {
	frame.SetResultValue(index, reflect.Zero(frame.resultType(index)))
}
