// Copyright 2019 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"gitlab.com/haproxy-haptic/scriggo/ast"
	"gitlab.com/haproxy-haptic/scriggo/native"
)

const NoVariadicArgs = -1
const CurrentFunction = -1
const ReturnString = -1

const maxUint32 = 1<<31 - 1

const stackSize = 512

// maxRegisterIndex is the maximum register index in the instruction encoding.
// Registers are indexed 0-127 in each type (int, float, string, general).
const maxRegisterIndex = 127

var envType = reflect.TypeFor[native.Env]()
var emptyInterfaceType = reflect.TypeFor[any]()
var emptyInterfaceNil = reflect.ValueOf(&[]any{nil}[0]).Elem()

// vmPool is a pool of VMs for reuse in parallel render operations.
// Uses chanPool instead of sync.Pool to survive GC eviction cycles.
// Capacity 128 covers peak usage (~114 VMs per render: 6 sharded ops × 19 shards).
var vmPool = newRetainedPool(128, func() *VM {
	return create(nil)
})

// getPooledVM gets a VM from the pool and initializes it with the given env.
func getPooledVM(env *env, ctx context.Context) *VM {
	vm := vmPool.get()
	vm.env = env
	if env != nil {
		vm.nativeEnv = &nativeEnv{env: env, ctx: ctx}
		vm.envArg = reflect.ValueOf(vm.nativeEnv)
		vm.setExecutionContext(ctx)
	}
	return vm
}

// maxPooledStackSize is the maximum register bank size allowed in pooled VMs.
// If a VM's stacks grow beyond this (e.g. from deep recursion), they are
// reallocated at the default stackSize on pool return to prevent memory bloat.
const maxPooledStackSize = 2 * stackSize

// putPooledVM returns a VM to the pool after resetting it.
//
// maxPooledFrames bounds the frame slices a pooled VM may carry: small ones are
// cleared and kept, oversized ones dropped, so a VM that once ran a deep render
// does not pay a large memclr for every shallow call it later serves.
const maxPooledFrames = 32

// releaseFrames drops every reference a frame slice holds, keeping the
// allocation only while it is small enough to be worth keeping.
func releaseFrames[T any](s []T) []T {
	if s == nil {
		return nil
	}
	if cap(s) > maxPooledFrames {
		return nil
	}
	clear(s[:cap(s)])
	return s[:0]
}

func clearWrittenRegisters[T any](registers []T, high Addr) {
	end := min(int(high)+1, len(registers))
	clear(registers[:end])
}

func putPooledVM(vm *VM) {
	// Cap oversized stacks to prevent a single deep-recursion call from
	// permanently inflating all pooled VMs.
	if len(vm.regs.int) > maxPooledStackSize {
		vm.regs.int = make([]int64, stackSize)
	} else {
		clear(vm.regs.int)
	}
	if len(vm.regs.float) > maxPooledStackSize {
		vm.regs.float = make([]float64, stackSize)
	} else {
		clear(vm.regs.float)
	}
	// Clear the whole bank rather than up to the high-water mark. The mark only
	// covers registers written through paths that call noteHigh; a bulk copy
	// into a pooled VM's banks does not, so the tail keeps reflect.Values from
	// the previous render — and one of those is the render's globals.
	if len(vm.regs.string) > maxPooledStackSize {
		vm.regs.string = make([]string, stackSize)
	} else {
		clear(vm.regs.string)
	}
	if len(vm.regs.general) > maxPooledStackSize {
		vm.regs.general = make([]reflect.Value, stackSize)
	} else {
		clear(vm.regs.general)
	}

	// Clear other references
	vm.fn = nil
	vm.vars = nil
	vm.renderer = nil
	vm.rootRender.reset(nil)
	vm.env = nil
	vm.nativeEnv = nil
	vm.envArg = reflect.Value{}
	vm.state = nil
	vm.doneChan = nil
	vm.doneCase = reflect.SelectCase{}
	vm.panic = nil
	vm.profiler = nil
	clear(vm.nativeArgs[:])
	vm.nativeCall = NativeFunction{}

	// Reset stack pointers to full capacity
	vm.st[0] = Addr(len(vm.regs.int))
	vm.st[1] = Addr(len(vm.regs.float))
	vm.st[2] = Addr(len(vm.regs.string))
	vm.st[3] = Addr(len(vm.regs.general))
	vm.fp = [4]Addr{0, 0, 0, 0}
	vm.high = [4]Addr{0, 0, 0, 0}
	vm.pc = 0
	vm.ok = false

	// Release, don't just truncate. A rangeState pins the collection it was
	// iterating and a callFrame pins that render's globals and renderer, so an
	// element left inside a truncated slice's capacity keeps a whole render's
	// object graph reachable for as long as the VM sits in the pool — which is
	// forever, since the pool is deliberately not GC-evictable.
	vm.calls = releaseFrames(vm.calls)
	vm.cases = releaseFrames(vm.cases)
	vm.ranges = releaseFrames(vm.ranges)
	// rangesBase indexes into vm.ranges, which was just truncated. Leaving it
	// set makes the next user of this VM address the range stack from a base
	// that no longer exists. A run that unwinds normally resets it itself, so
	// only a run that ends early — a panicking render — puts a VM back dirty.
	vm.rangesBase = 0

	vmPool.put(vm)
}

// ClearVMPool drains the VM pool, allowing accumulated VMs to be garbage collected.
// Call this to release memory from high-concurrency spikes.
// Thread-safe: can be called from any goroutine.
func ClearVMPool() {
	vmPool.drain()
}

// renderFuturePool is a pool of renderFutures for reuse.
// The channels are buffered with size 1 to allow non-blocking sends from
// the parallel render goroutine, ensuring the goroutine can complete and
// return to the pool without blocking on the receiver.
var renderFuturePool = newRetainedPool(256, func() *renderFuture {
	return &renderFuture{
		result: make(chan string, 1), // Buffered for non-blocking send
		err:    make(chan error, 1),  // Buffered for non-blocking send
	}
})

// getRenderFuture gets a renderFuture from the pool.
func getRenderFuture() *renderFuture {
	return renderFuturePool.get()
}

// putRenderFuture returns a renderFuture to the pool after draining channels.
func putRenderFuture(f *renderFuture) {
	// Drain channels (non-blocking) to reset state
	select {
	case <-f.result:
	default:
	}
	select {
	case <-f.err:
	default:
	}
	renderFuturePool.put(f)
}

// Converter is implemented by format converters.
type Converter = func(src []byte, out io.Writer) error

// A TypeOfFunc function returns a type of a value.
type TypeOfFunc func(reflect.Value) reflect.Type

// A ScriggoType represents a type compiled by Scriggo from a type definition
// or a composite type literal with at least one element with a Scriggo type.
type ScriggoType interface {
	reflect.Type

	// Wrap wraps a value with a Scriggo type putting into a proxy that exposes
	// methods to Go.
	Wrap(reflect.Value) reflect.Value

	// Unwrap unwraps a value that has been read from Go. If the value given as
	// parameter can be unwrapped using the unwrapper's type, the unwrapped
	// value is returned and the method returns true.
	Unwrap(reflect.Value) (reflect.Value, bool)

	// GoType returns the Go type of a Scriggo type. Note that the
	// implementation of the reflect.Type returned by GoType is the
	// implementation of the package 'reflect', so it's safe to pass the
	// returned value to reflect functions and methods as argument.
	GoType() reflect.Type
}

type StackShift [4]int8

type Instruction struct {
	Op      Operation
	A, B, C int8
}

func decodeInt16(a, b int8) int16 {
	return int16(int(a)<<8 | int(uint8(b)))
}

func decodeUint16(a, b int8) uint16 {
	return uint16(uint8(a))<<8 | uint16(uint8(b))
}

func decodeUint24(a, b, c int8) uint32 {
	return uint32(uint8(a))<<16 | uint32(uint8(b))<<8 | uint32(uint8(c))
}

func decodeValueIndex(a, b int8) (t registerType, i int) {
	return registerType(uint8(a) >> 6), int(decodeUint16(a, b) &^ (3 << 14))
}

// rangeState holds the state for a currently executing inline range loop.
// This allows range loops to execute without recursive vm.run() calls.
type rangeState struct {
	// Iterator state - only one of these is used depending on collection type
	sliceInt    []int
	sliceByte   []byte
	sliceRune   []rune
	sliceFloat  []float64
	sliceString []string
	sliceIface  []interface{}
	mapIter     *reflect.MapIter
	reflectVal  reflect.Value // for generic slice/array iteration
	strVal      string        // for string iteration

	// Current position
	index int
	// For strings, byte position
	bytePos int
	// Collection length (for slices/arrays)
	length int

	// Loop control addresses
	rangeAddr Addr // address of the range instruction (for continue matching)
	bodyAddr  Addr // address of loop body start
	endAddr   Addr // address after loop (where to jump on break or completion)

	// Register info for loop variables
	indexReg int8 // register for index (0 if unused)
	elemReg  int8 // register for element (0 if unused)

	// Collection type info
	kind      reflect.Kind
	elemKind  reflect.Kind
	rangeType rangeType
}

// rangeType indicates what type of range iteration is being performed
type rangeType int8

const (
	rangeSliceInt rangeType = iota
	rangeSliceByte
	rangeSliceRune
	rangeSliceFloat64
	rangeSliceString
	rangeSliceInterface
	rangeMapStringInt
	rangeMapStringBool
	rangeMapStringString
	rangeMapStringInterface
	rangeMapReflect
	rangeSliceReflect
	rangeArrayReflect
	rangeString
	rangeChan
)

// Specialization states for bytecode quickening (Phase 3)
const (
	specStateUnspecialized uint32 = iota // Not yet specialized
	specStateInProgress                  // Specialization in progress
	specStateSpecialized                 // Successfully specialized
	specStateDeoptimized                 // Deoptimized due to type mismatch
)

// SpecializationMetadata holds runtime type information for instruction specialization.
// This is a sidecar structure that doesn't modify the original instruction,
// ensuring concurrent safety for multiple VMs executing the same bytecode.
type SpecializationMetadata struct {
	state    uint32       // Atomic: specialization state (see specState* constants)
	keyKind  reflect.Kind // For map specialization: key type kind
	elemKind reflect.Kind // For map specialization: element type kind
	fieldIdx int          // For field specialization: direct field index (-1 if nested)
}

// renderFuture holds the result of a parallel render operation.
type renderFuture struct {
	result chan string // receives rendered output
	err    chan error  // receives error if any
}

// VM represents a Scriggo virtual machine.
type VM struct {
	fp                      [4]Addr              // frame pointers.
	st                      [4]Addr              // stack tops.
	pc                      Addr                 // program counter.
	ok                      bool                 // ok flag.
	regs                    registers            // registers.
	fn                      *Function            // running function.
	vars                    []reflect.Value      // global and closure variables.
	env                     *env                 // execution environment.
	nativeEnv               *nativeEnv           // native-facing execution environment.
	envArg                  reflect.Value        // execution environment as argument.
	renderer                *renderer            // renderer
	calls                   []callFrame          // call stack frame.
	cases                   []reflect.SelectCase // select cases.
	ranges                  []rangeState         // inline range loop state stack.
	high                    [4]Addr              // highest register index written; see Reset.
	rangesBase              int                  // base index for current function's ranges.
	panic                   *PanicError          // panic.
	main                    bool                 // reports whether this VM is executing the main goroutine.
	profiler                *profiler            // profiler for execution timing (nil when disabled).
	doneChan                <-chan struct{}
	doneCase                reflect.SelectCase
	state                   *executionState
	rootRender              renderer
	nativeArgs              [8]reflect.Value
	nativeCall              NativeFunction
	exposedNativeEnv        *nativeEnv
	exposedVectorGeneration uint64
	exposedVectorLease      *vectorInvocationLease
}

type executionState struct {
	env            env
	nativeEnv      nativeEnv
	envEscaped     atomic.Bool
	globalsEscaped atomic.Bool
}

func (vm *VM) vectorGlobal(index int16) (int16, bool) {
	if vm.env == nil || vm.env.vector == nil || !vm.env.vector.vmNative {
		return -1, false
	}
	if vm.fn.VarRefs == nil {
		return index, true
	}
	if index < 0 || int(index) >= len(vm.fn.GlobalVarRefs) {
		return -1, false
	}
	global := vm.fn.GlobalVarRefs[index]
	return global, global >= 0
}

func (vm *VM) internalFunctionGlobal(index int16) (int, bool) {
	if vm.env == nil {
		return -1, false
	}
	global := int(index)
	if vm.fn.VarRefs != nil {
		if index < 0 || int(index) >= len(vm.fn.GlobalVarRefs) {
			return -1, false
		}
		global = int(vm.fn.GlobalVarRefs[index])
	}
	if global < 0 || global >= len(vm.env.internalFunctionGlobals) ||
		!vm.env.internalFunctionGlobals[global] {
		return -1, false
	}
	return global, true
}

func (vm *VM) installInternalFunctionGlobals(globals []bool) {
	vm.env.internalFunctionGlobals = globals
	if len(globals) == 0 {
		vm.env.internalFunctionValues = nil
		vm.env.internalFunctionEscaped = nil
		return
	}
	vm.env.internalFunctionValues = make([]atomic.Pointer[callable], len(globals))
	vm.env.internalFunctionEscaped = make([]atomic.Bool, len(globals))
}

// NewVM returns a new virtual machine.
func NewVM() *VM {
	vm := create(nil)
	vm.main = true
	vm.installExecutionState()
	return vm
}

func (vm *VM) installExecutionState() {
	state := &executionState{}
	state.nativeEnv.env = &state.env
	vm.state = state
	vm.env = &state.env
	vm.nativeEnv = &state.nativeEnv
	vm.envArg = reflect.ValueOf(vm.nativeEnv)
}

// Reset resets a virtual machine so that it is ready for a new call to Run.
func (vm *VM) Reset() {
	vm.reset(false)
}

// ResetBatch resets a virtual machine after a certified batch execution.
func (vm *VM) ResetBatch() {
	vm.reset(true)
}

func (vm *VM) reset(batch bool) {
	if len(vm.regs.int) > maxPooledStackSize {
		vm.regs.int = make([]int64, stackSize)
	} else {
		clearWrittenRegisters(vm.regs.int, vm.high[0])
	}
	if len(vm.regs.float) > maxPooledStackSize {
		vm.regs.float = make([]float64, stackSize)
	} else {
		clearWrittenRegisters(vm.regs.float, vm.high[1])
	}
	if len(vm.regs.string) > maxPooledStackSize {
		vm.regs.string = make([]string, stackSize)
	} else {
		clearWrittenRegisters(vm.regs.string, vm.high[2])
	}
	if len(vm.regs.general) > maxPooledStackSize {
		vm.regs.general = make([]reflect.Value, stackSize)
	} else {
		clearWrittenRegisters(vm.regs.general, vm.high[3])
	}
	vm.fp = [4]Addr{0, 0, 0, 0}
	vm.st[0] = Addr(len(vm.regs.int))
	vm.st[1] = Addr(len(vm.regs.float))
	vm.st[2] = Addr(len(vm.regs.string))
	vm.st[3] = Addr(len(vm.regs.general))
	vm.pc = 0
	vm.ok = false
	vm.high = [4]Addr{0, 0, 0, 0}
	vm.fn = nil
	vm.vars = nil
	if batch && vm.state != nil && !vm.state.envEscaped.Load() {
		vm.resetExecutionState()
	} else {
		vm.installExecutionState()
	}
	vm.doneChan = nil
	vm.doneCase = reflect.SelectCase{}
	vm.rootRender.reset(nil)
	vm.renderer = nil
	vm.calls = releaseFrames(vm.calls)
	vm.cases = releaseFrames(vm.cases)
	vm.ranges = releaseFrames(vm.ranges)
	vm.rangesBase = 0
	vm.panic = nil
	vm.profiler = nil
	clear(vm.nativeArgs[:])
	vm.nativeCall = NativeFunction{}
	vm.exposedNativeEnv = nil
	vm.exposedVectorGeneration = 0
	vm.exposedVectorLease = nil
}

func (vm *VM) resetExecutionState() {
	state := vm.state
	state.env = env{}
	state.nativeEnv.mu.Lock()
	state.nativeEnv.env = &state.env
	state.nativeEnv.ctx = nil
	state.nativeEnv.vector = nil
	state.nativeEnv.vectorGeneration = 0
	state.nativeEnv.vectorLease = nil
	state.nativeEnv.callPath = ""
	state.nativeEnv.callLine = 0
	state.nativeEnv.mu.Unlock()
	state.envEscaped.Store(false)
	state.globalsEscaped.Store(false)
	vm.env = &state.env
	vm.nativeEnv = &state.nativeEnv
	vm.envArg = reflect.ValueOf(vm.nativeEnv)
}

// BatchGlobalsEscaped reports whether the current batch generation must remain isolated.
func (vm *VM) BatchGlobalsEscaped() bool {
	return vm.state != nil && vm.state.globalsEscaped.Load()
}

func (vm *VM) markBatchEnvironmentEscaped() {
	if vm.state != nil {
		vm.state.envEscaped.Store(true)
	}
}

func (vm *VM) markBatchGlobalsEscaped() {
	if vm.state != nil {
		vm.state.envEscaped.Store(true)
		vm.state.globalsEscaped.Store(true)
	}
}

// stop is called in the vm.run method to stop the execution.
func (vm *VM) stop() (Addr, bool) {
	const maxAddr = 1<<32 - 1
	return maxAddr, false
}

// advanceRangeInline advances the iterator for an inline range loop and sets
// the loop variables. Returns true if there are more elements, false if iteration
// is complete.
func (vm *VM) advanceRangeInline(rs *rangeState) bool {
	rs.index++

	switch rs.rangeType {
	case rangeSliceInt:
		if rs.index >= len(rs.sliceInt) {
			return false
		}
		if rs.indexReg != 0 {
			vm.setInt(rs.indexReg, int64(rs.index))
		}
		if rs.elemReg != 0 {
			vm.setInt(rs.elemReg, int64(rs.sliceInt[rs.index]))
		}
		return true

	case rangeSliceByte:
		if rs.index >= len(rs.sliceByte) {
			return false
		}
		if rs.indexReg != 0 {
			vm.setInt(rs.indexReg, int64(rs.index))
		}
		if rs.elemReg != 0 {
			vm.setInt(rs.elemReg, int64(rs.sliceByte[rs.index]))
		}
		return true

	case rangeSliceRune:
		if rs.index >= len(rs.sliceRune) {
			return false
		}
		if rs.indexReg != 0 {
			vm.setInt(rs.indexReg, int64(rs.index))
		}
		if rs.elemReg != 0 {
			vm.setInt(rs.elemReg, int64(rs.sliceRune[rs.index]))
		}
		return true

	case rangeSliceFloat64:
		if rs.index >= len(rs.sliceFloat) {
			return false
		}
		if rs.indexReg != 0 {
			vm.setInt(rs.indexReg, int64(rs.index))
		}
		if rs.elemReg != 0 {
			vm.setFloat(rs.elemReg, rs.sliceFloat[rs.index])
		}
		return true

	case rangeSliceString:
		if rs.index >= len(rs.sliceString) {
			return false
		}
		if rs.indexReg != 0 {
			vm.setInt(rs.indexReg, int64(rs.index))
		}
		if rs.elemReg != 0 {
			vm.setString(rs.elemReg, rs.sliceString[rs.index])
		}
		return true

	case rangeSliceInterface:
		if rs.index >= len(rs.sliceIface) {
			return false
		}
		if rs.indexReg != 0 {
			vm.setInt(rs.indexReg, int64(rs.index))
		}
		if rs.elemReg != 0 {
			vm.setGeneral(rs.elemReg, reflect.ValueOf(rs.sliceIface[rs.index]))
		}
		return true

	case rangeMapStringInt:
		if !rs.mapIter.Next() {
			return false
		}
		if rs.indexReg != 0 {
			vm.setString(rs.indexReg, rs.mapIter.Key().String())
		}
		if rs.elemReg != 0 {
			vm.setInt(rs.elemReg, rs.mapIter.Value().Int())
		}
		return true

	case rangeMapStringBool:
		if !rs.mapIter.Next() {
			return false
		}
		if rs.indexReg != 0 {
			vm.setString(rs.indexReg, rs.mapIter.Key().String())
		}
		if rs.elemReg != 0 {
			vm.setBool(rs.elemReg, rs.mapIter.Value().Bool())
		}
		return true

	case rangeMapStringString:
		if !rs.mapIter.Next() {
			return false
		}
		if rs.indexReg != 0 {
			vm.setString(rs.indexReg, rs.mapIter.Key().String())
		}
		if rs.elemReg != 0 {
			vm.setString(rs.elemReg, rs.mapIter.Value().String())
		}
		return true

	case rangeMapStringInterface:
		if !rs.mapIter.Next() {
			return false
		}
		if rs.indexReg != 0 {
			vm.setString(rs.indexReg, rs.mapIter.Key().String())
		}
		if rs.elemReg != 0 {
			vm.setGeneral(rs.elemReg, reflect.ValueOf(rs.mapIter.Value().Interface()))
		}
		return true

	case rangeMapReflect:
		if !rs.mapIter.Next() {
			return false
		}
		if rs.indexReg != 0 {
			vm.setFromReflectValue(rs.indexReg, rs.mapIter.Key())
		}
		if rs.elemReg != 0 {
			vm.setFromReflectValue(rs.elemReg, rs.mapIter.Value())
		}
		return true

	case rangeSliceReflect, rangeArrayReflect:
		if rs.index >= rs.length {
			return false
		}
		if rs.indexReg != 0 {
			vm.setInt(rs.indexReg, int64(rs.index))
		}
		if rs.elemReg != 0 {
			vm.setFromReflectValue(rs.elemReg, rs.reflectVal.Index(rs.index))
		}
		return true

	case rangeString:
		if rs.bytePos >= len(rs.strVal) {
			return false
		}
		// Decode the next rune
		r, size := decodeRuneInString(rs.strVal[rs.bytePos:])
		if rs.indexReg != 0 {
			vm.setInt(rs.indexReg, int64(rs.bytePos))
		}
		if rs.elemReg != 0 {
			vm.setInt(rs.elemReg, int64(r))
		}
		rs.bytePos += size
		return true

	default:
		// Unsupported range type - this shouldn't happen for inline ranges
		return false
	}
}

// decodeRuneInString decodes a single UTF-8 rune from a string.
// Returns the rune and the number of bytes consumed.
func decodeRuneInString(s string) (rune, int) {
	if len(s) == 0 {
		return 0, 0
	}
	// Fast path for ASCII
	if s[0] < 0x80 {
		return rune(s[0]), 1
	}
	// For multi-byte sequences, manually decode UTF-8
	b := s[0]
	var r rune
	var size int
	switch {
	case b&0xE0 == 0xC0:
		if len(s) < 2 {
			return 0xFFFD, 1
		}
		r = rune(b&0x1F)<<6 | rune(s[1]&0x3F)
		size = 2
	case b&0xF0 == 0xE0:
		if len(s) < 3 {
			return 0xFFFD, 1
		}
		r = rune(b&0x0F)<<12 | rune(s[1]&0x3F)<<6 | rune(s[2]&0x3F)
		size = 3
	case b&0xF8 == 0xF0:
		if len(s) < 4 {
			return 0xFFFD, 1
		}
		r = rune(b&0x07)<<18 | rune(s[1]&0x3F)<<12 | rune(s[2]&0x3F)<<6 | rune(s[3]&0x3F)
		size = 4
	default:
		return 0xFFFD, 1
	}
	return r, size
}

// Run starts the execution of the function fn with the given global variables
// and waits for it to complete.
//
// During the execution if a panic occurs and has not been recovered, by
// default Run panics with the panic message.
//
// If a context has been set and the context is canceled, Run returns
// as soon as possible with the error returned by the Err method of the
// context.
func (vm *VM) Run(fn *Function, typeof TypeOfFunc, globals []reflect.Value) error {
	if vm.env.deterministic {
		if err := validateDeterministicProgram(fn); err != nil {
			return err
		}
	}
	if typeof == nil {
		typeof = typeOfFunc
	}
	vm.env.typeof = typeof
	vm.env.globals = globals
	vm.installInternalFunctionGlobals(fn.InternalFunctionGlobals)
	if vm.renderer != nil {
		vm.renderer.retainTextFragments = fn.Format == ast.FormatText
	}
	err := vm.runFunc(fn, globals)
	if err != nil {
		switch e := err.(type) {
		case *PanicError:
			if outErr, ok := e.message.(outError); ok {
				err = outErr.err
			}
		case *fatalError:
			panic(e.Error())
		case stopError:
			err = e.err
		}
		return err
	}
	return nil
}

// SetContext sets the context.
//
// SetContext must not be called after vm has been started.
func (vm *VM) SetContext(ctx context.Context) {
	vm.env.ctx = ctx
	vm.setExecutionContext(ctx)
}

func (vm *VM) setExecutionContext(ctx context.Context) {
	vm.nativeEnv.ctx = ctx
	if ctx != nil {
		if done := ctx.Done(); done != nil {
			vm.doneChan = done
			vm.doneCase = reflect.SelectCase{
				Dir:  reflect.SelectRecv,
				Chan: reflect.ValueOf(done),
			}
			return
		}
	}
	vm.doneChan = nil
	vm.doneCase = reflect.SelectCase{}
}

// SetChildContext sets the function used to derive contexts for child
// executions. It must not be called after vm has been started.
func (vm *VM) SetChildContext(fn func(context.Context) context.Context) {
	vm.env.childContext = fn
}

// SetMutationObserver sets the function called before template mutations.
// It must not be called after vm has been started.
func (vm *VM) SetMutationObserver(observer MutationObserver) {
	vm.env.observeMutation = observer
}

// SetMutationContextObserver sets a synchronous context-only mutation observer.
func (vm *VM) SetMutationContextObserver(observer MutationContextObserver) {
	vm.env.observeMutationContext = observer
}

// SetNativeCallObserver sets the function called before native method access.
// It must not be called after vm has been started.
func (vm *VM) SetNativeCallObserver(observer NativeCallObserver) {
	vm.env.observeNativeCall = observer
}

// SetNativeCallContextObserver sets a synchronous context-only native method observer.
func (vm *VM) SetNativeCallContextObserver(observer NativeCallContextObserver) {
	vm.env.observeNativeCallContext = observer
}

// SetBeforeNativeCall sets the function called before every native invocation.
// It must not be called after vm has been started.
func (vm *VM) SetBeforeNativeCall(before BeforeNativeCall) {
	vm.env.beforeNativeCall = before
}

// SetBeforeNativeCallContext sets a synchronous context-only native call hook.
func (vm *VM) SetBeforeNativeCallContext(before BeforeNativeCallContext) {
	vm.env.beforeNativeCallContext = before
}

// SetNativeFunctionTrampolines installs direct invocation paths for exact
// native function values.
func (vm *VM) SetNativeFunctionTrampolines(trampolines []*native.FunctionTrampoline) error {
	if len(trampolines) == 0 {
		vm.env.nativeTrampolines = nil
		vm.env.methodTrampolines = nil
		return nil
	}
	nativeTrampolines, methodTrampolines, err := mergeNativeFunctionTrampolines(nil, nil, trampolines)
	if err != nil {
		return err
	}
	vm.env.nativeTrampolines = nativeTrampolines
	vm.env.methodTrampolines = methodTrampolines
	return nil
}

func mergeNativeFunctionTrampolines(
	baseNative map[reflect.Value]nativeFunctionCallSite,
	baseMethods map[nativeMethodTrampolineKey]nativeFunctionCallSite,
	trampolines []*native.FunctionTrampoline,
) (map[reflect.Value]nativeFunctionCallSite, map[nativeMethodTrampolineKey]nativeFunctionCallSite, error) {
	nativeTrampolines := make(map[reflect.Value]nativeFunctionCallSite, len(baseNative)+len(trampolines))
	methodTrampolines := make(map[nativeMethodTrampolineKey]nativeFunctionCallSite, len(baseMethods))
	for identity, site := range baseNative {
		nativeTrampolines[identity] = site
	}
	for identity, site := range baseMethods {
		methodTrampolines[identity] = site
	}
	for _, trampoline := range trampolines {
		if trampoline == nil {
			return nil, nil, errors.New("invalid nil native function trampoline")
		}
		site, valid := makeNativeFunctionCallSite(trampoline)
		if !valid {
			return nil, nil, errors.New("invalid native function trampoline")
		}
		if receiver, method, isMethod := trampoline.MethodTarget(); isMethod {
			key := nativeMethodTrampolineKey{
				receiver: receiver,
				method:   method,
				function: trampoline.Value().Type(),
			}
			if previous, duplicate := methodTrampolines[key]; duplicate && previous.trampoline != trampoline {
				return nil, nil, errors.New("conflicting native method trampolines")
			}
			methodTrampolines[key] = site
			continue
		}
		identity := comparableNativeFunction(trampoline.Value())
		if !identity.IsValid() {
			return nil, nil, errors.New("invalid native function trampoline")
		}
		if previous, duplicate := nativeTrampolines[identity]; duplicate && previous.trampoline != trampoline {
			return nil, nil, errors.New("conflicting native function trampolines")
		}
		nativeTrampolines[identity] = site
	}
	return nativeTrampolines, methodTrampolines, nil
}

// SetDeterministic enables deterministic execution checks and map iteration.
// It must not be called after vm has been started.
func (vm *VM) SetDeterministic(enabled bool) {
	vm.env.deterministic = enabled
}

// SetVectorState installs the state used by authenticated vector controllers.
func (vm *VM) SetVectorState(state *VectorState) {
	vm.env.vector = state
	state.setContextSwitch(func(ctx context.Context) {
		if ctx == nil {
			ctx = vm.env.ctx
		}
		vm.setExecutionContext(ctx)
	})
}

// SetRenderer sets template output and markdown converter.
//
// SetRenderer must not be called after vm has been started.
func (vm *VM) SetRenderer(out io.Writer, conv Converter) {
	vm.setRenderer(out)
	vm.env.conv = conv
}

func (vm *VM) setRenderer(out io.Writer) {
	vm.rootRender.reset(out)
	vm.renderer = &vm.rootRender
}

// SetMaxParallelRenders sets the maximum number of concurrent "go render" operations.
// If n is zero or negative, the default (2*GOMAXPROCS) is used.
//
// SetMaxParallelRenders must be called after SetRenderer and before vm has been started.
func (vm *VM) SetMaxParallelRenders(n int) {
	if vm.renderer != nil {
		vm.renderer.maxParallelRenders = n
	}
}

// SetCollectSourceMap enables recording of the output-to-source line map.
// Must be called after SetRenderer and before vm has been started.
func (vm *VM) SetCollectSourceMap(on bool) {
	if vm.renderer != nil {
		vm.renderer.collectSrc = on
	}
}

// SourceSpans returns the output-ordered source map collected during the run,
// or nil if collection was not enabled.
func (vm *VM) SourceSpans() []SourceSpan {
	if vm.renderer == nil {
		return nil
	}
	return vm.renderer.collectSpans()
}

// setSrcCtx records, for the instruction currently executing (at pc-1), the
// template include stack of the output it is about to write (innermost first:
// the current template, then each render/include caller). isText marks literal
// template text. A no-op unless source-map collection is enabled.
func (vm *VM) setSrcCtx(isText bool) {
	if !vm.renderer.collectSrc {
		return
	}
	vm.renderer.curFrames = vm.captureFrames(vm.pc - 1)
	vm.renderer.curIsText = isText
	vm.renderer.consumeNext = false // only a show consumes a pending value source
}

// setSrcCtxShow is setSrcCtx for a show opcode: it also flags the next write to
// consume a pending rendered-to-string value's per-line source, if any.
func (vm *VM) setSrcCtxShow() {
	if !vm.renderer.collectSrc {
		return
	}
	vm.renderer.curFrames = vm.captureFrames(vm.pc - 1)
	vm.renderer.curIsText = false
	vm.renderer.consumeNext = len(vm.renderer.pendingValueSources) > 0
}

// captureFrames walks the call stack (current frame at instruction ppc, then its
// callers) and returns the include stack, innermost first. Mirrors the frame
// walk in Stack. Frames whose template path is empty/internal are kept as-is;
// the consumer filters to known templates.
func (vm *VM) captureFrames(ppc Addr) []SourceFrame {
	frames := make([]SourceFrame, 0, len(vm.calls)+1)
	if info, ok := vm.fn.InstructionInfo[ppc]; ok {
		frames = append(frames, SourceFrame{Path: info.Path, Line: info.Position.Line})
	} else {
		frames = append(frames, SourceFrame{})
	}
	for i := len(vm.calls) - 1; i >= 0; i-- {
		call := vm.calls[i]
		var cppc Addr
		if call.status == tailed {
			cppc = call.pc - 1
		} else {
			cppc = call.pc - 2
		}
		if info, ok := call.cl.fn.InstructionInfo[cppc]; ok {
			frames = append(frames, SourceFrame{Path: info.Path, Line: info.Position.Line})
		}
	}
	return frames
}

// stashValueSource records, on the parent renderer, the per-line include stack of
// a just-completed rendered-to-string value so the show that outputs it can
// attribute per line. buf is the value string; r is the sub-render's renderer.
func (vm *VM) stashValueSource(parent *renderer, r *renderer, buf string) {
	if parent == nil || !parent.collectSrc {
		return
	}
	parent.pendingValueSources = append(parent.pendingValueSources, perLineFrames(buf, r.collectSpans()))
}

// SetPrint sets the "print" builtin function.
//
// SetPrint must not be called after vm has been started.
func (vm *VM) SetPrint(p PrintFunc) {
	vm.env.print = p
}

// SetProfiling enables or disables profiling. When enabled, the VM will
// collect timing data for function calls, macros, and includes.
//
// SetProfiling must not be called after vm has been started.
func (vm *VM) SetProfiling(enabled bool) {
	if enabled {
		vm.profiler = newProfiler()
	} else {
		vm.profiler = nil
	}
}

// GetProfileData returns the collected profiling data, or nil if profiling
// was not enabled.
func (vm *VM) GetProfileData() *ProfileData {
	if vm.profiler == nil {
		return nil
	}
	return vm.profiler.getData()
}

// currentLine returns the source line number for the current instruction,
// or 0 if not available. Used for profiling instrumentation.
func (vm *VM) currentLine() int {
	if info, ok := vm.fn.InstructionInfo[vm.pc-1]; ok {
		return info.Position.Line
	}
	return 0
}

// Stack returns the current stack trace.
func (vm *VM) Stack(buf []byte, all bool) int {
	// TODO(marco): implement all == true
	if len(buf) == 0 {
		return 0
	}
	b := buf[0:0:len(buf)]
	write := func(s string) {
		n := copy(b[len(b):cap(b)], s)
		b = b[:len(b)+n]
	}
	write("scriggo goroutine 1 [running]:")
	size := len(vm.calls)
	for i := size; i >= 0; i-- {
		var fn *Function
		var ppc Addr
		if i == size {
			fn = vm.fn
			ppc = vm.pc - 1
		} else {
			call := vm.calls[i]
			fn = call.cl.fn
			if call.status == tailed {
				ppc = call.pc - 1
			} else {
				ppc = call.pc - 2
			}
		}
		write("\n")
		write(packageName(fn.Pkg))
		write(".")
		write(fn.Name)
		write("()\n\t")
		if fn.File != "" {
			write(fn.File)
		} else {
			write("???")
		}
		write(":")
		if info, ok := fn.InstructionInfo[ppc]; ok {
			write(strconv.Itoa(info.Position.Line))
		} else {
			write("???")
		}
		if len(b) == len(buf) {
			break
		}
	}
	return len(b)
}

// generalToInterface safely converts a general register value to interface{}.
// Zero/invalid reflect.Values (representing nil interfaces) return nil.
func generalToInterface(v reflect.Value) interface{} {
	if !v.IsValid() {
		return nil
	}
	return v.Interface()
}

// prepareEnvCallPath sets the call path on the VM's env for native functions
// that accept native.Env. This must be called before invoking such functions
// so error messages include the correct source location.
func (vm *VM) prepareEnvCallPathFor(nativeEnv *nativeEnv) {
	vm.markBatchEnvironmentEscaped()
	if vm.main {
		info := vm.fn.InstructionInfo[vm.pc-1]
		nativeEnv.mu.Lock()
		nativeEnv.callPath = info.Path
		nativeEnv.callLine = info.Position.Line
		nativeEnv.mu.Unlock()
	}
}

func (vm *VM) prepareEnvCallPath() {
	vm.prepareEnvCallPathFor(vm.nativeEnv)
}

func (vm *VM) nativeEnvForExposure() *nativeEnv {
	if vm.env.vector == nil {
		return vm.nativeEnv
	}
	generation := vm.nativeEnv.vectorGeneration
	ctx := vm.nativeEnv.ctx
	if vm.nativeEnv.vectorLease != nil {
		if vm.exposedNativeEnv != nil && vm.exposedVectorGeneration == generation &&
			vm.exposedVectorLease == vm.nativeEnv.vectorLease {
			return vm.exposedNativeEnv
		}
		vm.exposedNativeEnv = &nativeEnv{
			env:              vm.env,
			ctx:              ctx,
			vector:           vm.env.vector,
			vectorGeneration: generation,
			vectorLease:      vm.nativeEnv.vectorLease,
		}
		vm.exposedVectorGeneration = generation
		vm.exposedVectorLease = vm.nativeEnv.vectorLease
		return vm.exposedNativeEnv
	}
	generation, ctx = vm.env.vector.captureContext()
	if ctx == nil {
		ctx = vm.nativeEnv.ctx
	}
	if vm.exposedNativeEnv != nil && vm.exposedVectorGeneration == generation &&
		vm.exposedVectorLease == nil {
		return vm.exposedNativeEnv
	}
	vm.exposedNativeEnv = &nativeEnv{
		env:              vm.env,
		ctx:              ctx,
		vector:           vm.env.vector,
		vectorGeneration: generation,
	}
	vm.exposedVectorGeneration = generation
	vm.exposedVectorLease = nil
	return vm.exposedNativeEnv
}

func (vm *VM) newChildNativeEnv() *nativeEnv {
	return &nativeEnv{
		env: vm.env,
		ctx: vm.env.deriveChildContext(vm.nativeEnv.ctx),
	}
}

// callNative calls a native function. numVariadic is the number of variadic
// arguments, shift is the stack shift and asGoroutine reports whether the
// function must be started as a goroutine.
//
// When callNative is called, vm.pc must be the address of the call
// instruction plus one.
func (vm *VM) callNative(fn *NativeFunction, numVariadic int8, shift StackShift, asGoroutine bool) {

	if fn.value.IsNil() {
		panic(errNilPointer)
	}
	if !asGoroutine && vm.callVMNativeVectorBoundary(fn, shift) {
		return
	}
	if !asGoroutine && vm.callVMNativeVectorFiberBoundary(fn, shift) {
		return
	}
	callContext := vm.nativeEnv.ctx
	needsEnvironment := asGoroutine || nativeFunctionAcceptsEnvironment(fn.value.Type()) ||
		vm.env.beforeNativeCall != nil ||
		fn.mutationOperation != 0 && vm.env.observeMutation != nil ||
		(fn.boundReceiver.IsValid() || fn.methodReceiver >= 0) && vm.env.observeNativeCall != nil
	var callEnv *nativeEnv
	if asGoroutine {
		callEnv = vm.newChildNativeEnv()
		callContext = callEnv.ctx
	} else if needsEnvironment {
		callEnv = vm.nativeEnvForExposure()
		callContext = callEnv.ctx
	}
	vm.beforeNativeCallWithContext(callContext, callEnv)

	// Profiling: record native function call entry (skip for goroutines since we don't wait).
	profileNative := vm.profiler != nil && !asGoroutine
	if profileNative {
		vm.profiler.enterCall(fn.name, fn.pkg, "", vm.currentLine(), ProfileCallKindNativeFunc, "")
	}

	// Make a copy of the frame pointer.
	fp := vm.fp

	// Shift the frame pointer.
	vm.fp[0] += Addr(shift[0])
	vm.fp[1] += Addr(shift[1])
	vm.fp[2] += Addr(shift[2])
	vm.fp[3] += Addr(shift[3])
	if fn.mutationOperation != 0 {
		if fn.mutationOperation == MutationCloseChannel {
			vm.rejectChannelOperationWithContext(callContext, callEnv, fn.mutationOperation, vm.general(1))
		} else {
			vm.observeMutationWithContext(callContext, callEnv, fn.mutationOperation, vm.general(1))
		}
	}
	if (fn.boundReceiver.IsValid() || fn.methodReceiver >= 0) && !fn.reflectCall {
		panic("native method unexpectedly uses the direct call path")
	}
	if !asGoroutine {
		if site, found := vm.functionCallFrameSite(fn); found {
			recordNativeFrameCall(fn.frameCallKind, true)
			vm.callNativeFunctionFrame(fn, site, numVariadic, callContext, callEnv)
			vm.fp = fp
			if profileNative {
				vm.profiler.exitCall()
			}
			return
		}
	}
	recordNativeFrameCall(fn.frameCallKind, false)

	// Call the function without the reflect.
	if !fn.reflectCall {
		if asGoroutine {
			childEnv := callEnv
			switch f := fn.function.(type) {
			case func(string) int:
				go f(vm.string(1))
			case func(string) string:
				go f(vm.string(2))
			case func(string, string) int:
				go f(vm.string(1), vm.string(2))
			case func(string, int) string:
				go f(vm.string(2), int(vm.int(1)))
			case func(string, string) bool:
				go f(vm.string(1), vm.string(2))
			case func(interface{}) bool:
				go f(generalToInterface(vm.general(1)))
			case func(interface{}) string:
				go f(generalToInterface(vm.general(1)))
			case func(interface{}, interface{}) interface{}:
				go f(generalToInterface(vm.general(2)), generalToInterface(vm.general(3)))
			case func(interface{}, interface{}) bool:
				go f(generalToInterface(vm.general(1)), generalToInterface(vm.general(2)))
			case func(interface{}, string) string:
				go f(generalToInterface(vm.general(1)), vm.string(2))
			case func(interface{}, ...string) interface{}:
				obj := generalToInterface(vm.general(2))
				var strs []string
				if numVariadic > 0 {
					strs = make([]string, int(numVariadic))
					for j := 0; j < int(numVariadic); j++ {
						strs[j] = vm.string(int8(j + 1))
					}
				}
				go f(obj, strs...)
			// Phase 1: Simple single/multi-param signatures.
			case func(interface{}) int:
				go f(generalToInterface(vm.general(1)))
			case func(interface{}) []string:
				go f(generalToInterface(vm.general(2)))
			case func(interface{}) []interface{}:
				go f(generalToInterface(vm.general(2)))
			case func(interface{}, interface{}) string:
				go f(generalToInterface(vm.general(1)), generalToInterface(vm.general(2)))
			case func(interface{}, interface{}) []string:
				go f(generalToInterface(vm.general(2)), generalToInterface(vm.general(3)))
			case func(interface{}, interface{}) []interface{}:
				go f(generalToInterface(vm.general(2)), generalToInterface(vm.general(3)))
			case func(interface{}, interface{}, interface{}) string:
				go f(generalToInterface(vm.general(1)), generalToInterface(vm.general(2)), generalToInterface(vm.general(3)))
			case func(float64) float64:
				go f(vm.float(2))
			case func(int) []int:
				go f(int(vm.int(1)))
			case func(interface{}, string) []string:
				go f(generalToInterface(vm.general(2)), vm.string(1))
			case func(interface{}, int, int) []interface{}:
				go f(generalToInterface(vm.general(2)), int(vm.int(1)), int(vm.int(2)))
			case func(string, string) string:
				go f(vm.string(2), vm.string(3))
			// Phase 2: Variadic signatures.
			case func(...interface{}) string:
				var args []interface{}
				if numVariadic > 0 {
					args = make([]interface{}, int(numVariadic))
					for j := 0; j < int(numVariadic); j++ {
						args[j] = generalToInterface(vm.general(int8(j + 1)))
					}
				}
				go f(args...)
			case func(string, ...interface{}) string:
				s := vm.string(2)
				var args []interface{}
				if numVariadic > 0 {
					args = make([]interface{}, int(numVariadic))
					for j := 0; j < int(numVariadic); j++ {
						args[j] = generalToInterface(vm.general(int8(j + 1)))
					}
				}
				go f(s, args...)
			case func(interface{}, string, ...interface{}) []interface{}:
				obj := generalToInterface(vm.general(2))
				attr := vm.string(1)
				var args []interface{}
				if numVariadic > 0 {
					args = make([]interface{}, int(numVariadic))
					for j := 0; j < int(numVariadic); j++ {
						args[j] = generalToInterface(vm.general(int8(j + 3)))
					}
				}
				go f(obj, attr, args...)
			// Phase 3: Error-return signatures.
			case func(interface{}) (float64, error):
				go f(generalToInterface(vm.general(2)))
			case func(interface{}) (string, error):
				go f(generalToInterface(vm.general(2)))
			case func(interface{}, ...interface{}) (string, error):
				arg := generalToInterface(vm.general(2))
				var args []interface{}
				if numVariadic > 0 {
					args = make([]interface{}, int(numVariadic))
					for j := 0; j < int(numVariadic); j++ {
						args[j] = generalToInterface(vm.general(int8(j + 3)))
					}
				}
				go f(arg, args...)
			case func([]interface{}, []string) ([]interface{}, error):
				var items []interface{}
				if v := vm.general(3); v.IsValid() {
					items = v.Interface().([]interface{})
				}
				var criteria []string
				if v := vm.general(4); v.IsValid() {
					criteria = v.Interface().([]string)
				}
				go f(items, criteria)
			// Phase 4: native.Env signatures.
			case func(native.Env, ...interface{}) bool:
				vm.prepareEnvCallPathFor(childEnv)
				var args []interface{}
				if numVariadic > 0 {
					args = make([]interface{}, int(numVariadic))
					for j := 0; j < int(numVariadic); j++ {
						args[j] = generalToInterface(vm.general(int8(j + 1)))
					}
				}
				go f(childEnv, args...)
			case func(native.Env, string) string:
				vm.prepareEnvCallPathFor(childEnv)
				go f(childEnv, vm.string(2))
			case func(native.Env, interface{}, interface{}) bool:
				vm.prepareEnvCallPathFor(childEnv)
				go f(childEnv, generalToInterface(vm.general(1)), generalToInterface(vm.general(2)))
			default:
				panic("unexpected")
			}
		} else {
			switch f := fn.function.(type) {
			case func(string) int:
				vm.setInt(1, int64(f(vm.string(1))))
			case func(string) string:
				vm.setString(1, f(vm.string(2)))
			case func(string, string) int:
				vm.setInt(1, int64(f(vm.string(1), vm.string(2))))
			case func(string, int) string:
				vm.setString(1, f(vm.string(2), int(vm.int(1))))
			case func(string, string) bool:
				vm.setBool(1, f(vm.string(1), vm.string(2)))
			case func(interface{}) bool:
				vm.setBool(1, f(generalToInterface(vm.general(1))))
			case func(interface{}) string:
				vm.setString(1, f(generalToInterface(vm.general(1))))
			case func(interface{}, interface{}) interface{}:
				vm.setGeneral(1, reflect.ValueOf(f(generalToInterface(vm.general(2)), generalToInterface(vm.general(3)))))
			case func(interface{}, interface{}) bool:
				vm.setBool(1, f(generalToInterface(vm.general(1)), generalToInterface(vm.general(2))))
			case func(interface{}, string) string:
				vm.setString(1, f(generalToInterface(vm.general(1)), vm.string(2)))
			case func(interface{}, ...string) interface{}:
				obj := generalToInterface(vm.general(2))
				if numVariadic == NoVariadicArgs || numVariadic == 0 {
					vm.setGeneral(1, reflect.ValueOf(f(obj)))
				} else {
					strs := make([]string, int(numVariadic))
					for j := 0; j < int(numVariadic); j++ {
						strs[j] = vm.string(int8(j + 1))
					}
					vm.setGeneral(1, reflect.ValueOf(f(obj, strs...)))
				}
			// Phase 1: Simple single/multi-param signatures.
			case func(interface{}) int:
				vm.setInt(1, int64(f(generalToInterface(vm.general(1)))))
			case func(interface{}) []string:
				vm.setGeneral(1, reflect.ValueOf(f(generalToInterface(vm.general(2)))))
			case func(interface{}) []interface{}:
				vm.setGeneral(1, reflect.ValueOf(f(generalToInterface(vm.general(2)))))
			case func(interface{}, interface{}) string:
				vm.setString(1, f(generalToInterface(vm.general(1)), generalToInterface(vm.general(2))))
			case func(interface{}, interface{}) []string:
				vm.setGeneral(1, reflect.ValueOf(f(generalToInterface(vm.general(2)), generalToInterface(vm.general(3)))))
			case func(interface{}, interface{}) []interface{}:
				vm.setGeneral(1, reflect.ValueOf(f(generalToInterface(vm.general(2)), generalToInterface(vm.general(3)))))
			case func(interface{}, interface{}, interface{}) string:
				vm.setString(1, f(generalToInterface(vm.general(1)), generalToInterface(vm.general(2)), generalToInterface(vm.general(3))))
			case func(float64) float64:
				vm.setFloat(1, f(vm.float(2)))
			case func(int) []int:
				vm.setGeneral(1, reflect.ValueOf(f(int(vm.int(1)))))
			case func(interface{}, string) []string:
				vm.setGeneral(1, reflect.ValueOf(f(generalToInterface(vm.general(2)), vm.string(1))))
			case func(interface{}, int, int) []interface{}:
				vm.setGeneral(1, reflect.ValueOf(f(generalToInterface(vm.general(2)), int(vm.int(1)), int(vm.int(2)))))
			case func(string, string) string:
				vm.setString(1, f(vm.string(2), vm.string(3)))
			// Phase 2: Variadic signatures.
			case func(...interface{}) string:
				if numVariadic == NoVariadicArgs || numVariadic == 0 {
					vm.setString(1, f())
				} else {
					args := make([]interface{}, int(numVariadic))
					for j := 0; j < int(numVariadic); j++ {
						args[j] = generalToInterface(vm.general(int8(j + 1)))
					}
					vm.setString(1, f(args...))
				}
			case func(string, ...interface{}) string:
				s := vm.string(2)
				if numVariadic == NoVariadicArgs || numVariadic == 0 {
					vm.setString(1, f(s))
				} else {
					args := make([]interface{}, int(numVariadic))
					for j := 0; j < int(numVariadic); j++ {
						args[j] = generalToInterface(vm.general(int8(j + 1)))
					}
					vm.setString(1, f(s, args...))
				}
			case func(interface{}, string, ...interface{}) []interface{}:
				obj := generalToInterface(vm.general(2))
				attr := vm.string(1)
				if numVariadic == NoVariadicArgs || numVariadic == 0 {
					vm.setGeneral(1, reflect.ValueOf(f(obj, attr)))
				} else {
					args := make([]interface{}, int(numVariadic))
					for j := 0; j < int(numVariadic); j++ {
						args[j] = generalToInterface(vm.general(int8(j + 3)))
					}
					vm.setGeneral(1, reflect.ValueOf(f(obj, attr, args...)))
				}
			// Phase 3: Error-return signatures.
			case func(interface{}) (float64, error):
				result, err := f(generalToInterface(vm.general(2)))
				vm.setFloat(1, result)
				if err != nil {
					vm.setGeneral(1, reflect.ValueOf(err))
				} else {
					vm.setGeneral(1, reflect.Value{})
				}
			case func(interface{}) (string, error):
				result, err := f(generalToInterface(vm.general(2)))
				vm.setString(1, result)
				if err != nil {
					vm.setGeneral(1, reflect.ValueOf(err))
				} else {
					vm.setGeneral(1, reflect.Value{})
				}
			case func(interface{}, ...interface{}) (string, error):
				arg := generalToInterface(vm.general(2))
				var s string
				var err error
				if numVariadic == NoVariadicArgs || numVariadic == 0 {
					s, err = f(arg)
				} else {
					args := make([]interface{}, int(numVariadic))
					for j := 0; j < int(numVariadic); j++ {
						args[j] = generalToInterface(vm.general(int8(j + 3)))
					}
					s, err = f(arg, args...)
				}
				vm.setString(1, s)
				if err != nil {
					vm.setGeneral(1, reflect.ValueOf(err))
				} else {
					vm.setGeneral(1, reflect.Value{})
				}
			case func([]interface{}, []string) ([]interface{}, error):
				var items []interface{}
				if v := vm.general(3); v.IsValid() {
					items = v.Interface().([]interface{})
				}
				var criteria []string
				if v := vm.general(4); v.IsValid() {
					criteria = v.Interface().([]string)
				}
				result, err := f(items, criteria)
				vm.setGeneral(1, reflect.ValueOf(result))
				if err != nil {
					vm.setGeneral(2, reflect.ValueOf(err))
				} else {
					vm.setGeneral(2, reflect.Value{})
				}
			// Phase 4: native.Env signatures.
			case func(native.Env, ...interface{}) bool:
				vm.prepareEnvCallPathFor(callEnv)
				if numVariadic == NoVariadicArgs || numVariadic == 0 {
					vm.setBool(1, f(callEnv))
				} else {
					args := make([]interface{}, int(numVariadic))
					for j := 0; j < int(numVariadic); j++ {
						args[j] = generalToInterface(vm.general(int8(j + 1)))
					}
					vm.setBool(1, f(callEnv, args...))
				}
			case func(native.Env, string) string:
				vm.prepareEnvCallPathFor(callEnv)
				vm.setString(1, f(callEnv, vm.string(2)))
			case func(native.Env, interface{}, interface{}) bool:
				vm.prepareEnvCallPathFor(callEnv)
				vm.setBool(1, f(callEnv, generalToInterface(vm.general(1)), generalToInterface(vm.general(2))))
			default:
				panic("unexpected")
			}
		}
		vm.fp = fp
		// Profiling: record native function call exit.
		if profileNative {
			vm.profiler.exitCall()
		}
		return
	}

	// Call the function with reflect.
	var args []reflect.Value

	typ := fn.value.Type()
	nunIn := typ.NumIn()
	variadic := typ.IsVariadic()
	var nativeEnvArg reflect.Value
	if callEnv != nil {
		nativeEnvArg = reflect.ValueOf(callEnv)
		if !asGoroutine && vm.env.vector == nil {
			nativeEnvArg = vm.envArg
		}
	}

	if nunIn > 0 {

		// Shift the frame pointer.
		vm.fp[0] += Addr(fn.outOff[0])
		vm.fp[1] += Addr(fn.outOff[1])
		vm.fp[2] += Addr(fn.outOff[2])
		vm.fp[3] += Addr(fn.outOff[3])

		if !asGoroutine && nunIn <= len(vm.nativeArgs) {
			args = vm.nativeArgs[:nunIn]
			clear(args)
			defer clear(args)
		} else {
			args = make([]reflect.Value, nunIn)
		}
		// Prepare the arguments.
		lastNonVariadic := nunIn
		if variadic && numVariadic != NoVariadicArgs {
			lastNonVariadic--
		}
		for i := range nunIn {
			if i < lastNonVariadic {
				if i < 2 && typ.In(i) == envType {
					vm.prepareEnvCallPathFor(callEnv)
					args[i] = nativeEnvArg
				} else {
					value, r := vm.reflectValueInContext(1, typ.In(i), false, callContext)
					args[i] = value
					vm.fp[r]++
				}
			} else {
				sliceType := typ.In(i)
				slice := reflect.MakeSlice(sliceType, int(numVariadic), int(numVariadic))
				k := sliceType.Elem().Kind()
				switch k {
				case reflect.Bool:
					for j := 0; j < int(numVariadic); j++ {
						slice.Index(j).SetBool(vm.bool(int8(j + 1)))
					}
				case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
					for j := 0; j < int(numVariadic); j++ {
						slice.Index(j).SetInt(vm.int(int8(j + 1)))
					}
				case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
					for j := 0; j < int(numVariadic); j++ {
						slice.Index(j).SetUint(uint64(vm.int(int8(j + 1))))
					}
				case reflect.Float32, reflect.Float64:
					for j := 0; j < int(numVariadic); j++ {
						slice.Index(j).SetFloat(vm.float(int8(j + 1)))
					}
				case reflect.Func:
					for j := 0; j < int(numVariadic); j++ {
						vm.markBatchGlobalsEscaped()
						f := vm.general(int8(j + 1)).Interface().(*callable)
						slice.Index(j).Set(f.Value(vm.env, callContext))
					}
				case reflect.String:
					for j := 0; j < int(numVariadic); j++ {
						slice.Index(j).SetString(vm.string(int8(j + 1)))
					}
				case reflect.Interface:
					for j := 0; j < int(numVariadic); j++ {
						if v := vm.general(int8(j + 1)); !v.IsValid() {
							if t := slice.Index(j).Type(); t == emptyInterfaceType {
								slice.Index(j).Set(emptyInterfaceNil)
							} else {
								slice.Index(j).Set(reflect.Zero(t))
							}
						} else {
							slice.Index(j).Set(v)
						}
					}
				default:
					for j := 0; j < int(numVariadic); j++ {
						slice.Index(j).Set(vm.general(int8(j + 1)))
					}
				}
				args[i] = slice
			}
		}

		// Shift the frame pointer.
		vm.fp[0] = fp[0] + Addr(shift[0])
		vm.fp[1] = fp[1] + Addr(shift[1])
		vm.fp[2] = fp[2] + Addr(shift[2])
		vm.fp[3] = fp[3] + Addr(shift[3])

	}
	receiver := fn.receiver(args)
	if receiver.IsValid() {
		vm.observeNativeCallWithContext(callContext, callEnv, receiver, fn.name, false)
	}

	if asGoroutine {

		// Start a goroutine.
		if variadic {
			go fn.value.CallSlice(args)
		} else {
			go fn.value.Call(args)
		}

	} else {

		// Call the function and get the results.
		var out []reflect.Value
		var methodSite nativeFunctionCallSite
		var methodSiteFound bool
		if receiver.IsValid() {
			methodSite, methodSiteFound = vm.env.methodTrampolines[nativeMethodTrampolineKey{
				receiver: receiver.Type(),
				method:   fn.name,
				function: fn.value.Type(),
			}]
		}
		if methodSiteFound {
			out = methodSite.trampoline.Call(args)
		} else if site, found := vm.env.nativeTrampolines[fn.identity]; found {
			out = site.trampoline.Call(args)
		} else if variadic {
			out = fn.value.CallSlice(args)
		} else {
			out = fn.value.Call(args)
		}
		for _, arg := range out {
			r := vm.setFromReflectValue(1, arg)
			vm.fp[r]++
		}

		// args slice is not pooled — it will be collected by GC.

	}

	vm.fp = fp // Restore the frame pointer.

	// Profiling: record native function call exit.
	if profileNative {
		vm.profiler.exitCall()
	}

	return
}

var vectorBoundaryFunctionType = reflect.TypeFor[func(*native.VectorBoundary, int)]()
var vectorFiberBoundaryFunctionType = reflect.TypeFor[func(*native.VectorFiberBoundary, int, int)]()

func (vm *VM) callVMNativeVectorBoundary(fn *NativeFunction, shift StackShift) bool {
	state := vm.env.vector
	if state == nil || !state.vmNative || state.VMNativeBoundary() == nil ||
		fn.boundReceiver.IsValid() || fn.methodReceiver != 0 || fn.value.Type() != vectorBoundaryFunctionType ||
		fn.name != "Begin" && fn.name != "End" {
		return false
	}
	fp := vm.fp
	for register := range vm.fp {
		vm.fp[register] += Addr(shift[register])
	}
	defer func() { vm.fp = fp }()
	receiver := vm.general(1)
	if !receiver.IsValid() || receiver.Type() != reflect.TypeFor[*native.VectorBoundary]() {
		panic(stopError{ErrVectorInvalidConfiguration})
	}
	boundary := receiver.Interface().(*native.VectorBoundary)
	if boundary != state.VMNativeBoundary() {
		panic(stopError{ErrVectorInvalidConfiguration})
	}
	index := int(vm.int(1))
	var err error
	if fn.name == "Begin" {
		err = state.BeginVMNative(boundary, index)
	} else {
		err = state.EndVMNative(boundary, index)
	}
	vm.syncVMNativeVectorContext(state)
	if err != nil {
		panic(stopError{err})
	}
	return true
}

func (vm *VM) callVMNativeVectorFiberBoundary(fn *NativeFunction, shift StackShift) bool {
	state := vm.env.vector
	if state == nil || !state.vmNative || state.VMNativeFiberBoundary() == nil ||
		fn.boundReceiver.IsValid() || fn.methodReceiver != 0 || fn.value.Type() != vectorFiberBoundaryFunctionType ||
		fn.name != "BeginChild" && fn.name != "EndChild" {
		return false
	}
	fp := vm.fp
	for register := range vm.fp {
		vm.fp[register] += Addr(shift[register])
	}
	defer func() { vm.fp = fp }()
	receiver := vm.general(1)
	if !receiver.IsValid() || receiver.Type() != reflect.TypeFor[*native.VectorFiberBoundary]() {
		panic(stopError{ErrVectorInvalidConfiguration})
	}
	boundary := receiver.Interface().(*native.VectorFiberBoundary)
	if boundary != state.VMNativeFiberBoundary() {
		panic(stopError{ErrVectorInvalidConfiguration})
	}
	source := int(vm.int(1))
	child := int(vm.int(2))
	var err error
	if fn.name == "BeginChild" {
		err = state.BeginFiberVMNative(boundary, source, child)
	} else {
		err = state.EndFiberVMNative(boundary, source, child)
	}
	if err != nil {
		panic(stopError{err})
	}
	vm.syncVMNativeVectorContext(state)
	return true
}

func (vm *VM) syncVMNativeVectorContext(state *VectorState) {
	generation, ctx := state.captureContext()
	if generation == 0 || ctx == nil {
		vm.nativeEnv.vector = nil
		vm.nativeEnv.vectorGeneration = 0
		vm.nativeEnv.vectorLease = nil
		vm.exposedNativeEnv = nil
		vm.exposedVectorGeneration = 0
		vm.exposedVectorLease = nil
		return
	}
	vm.nativeEnv.vector = state
	vm.nativeEnv.vectorGeneration = generation
	vm.nativeEnv.vectorLease = nil
	vm.setExecutionContext(ctx)
}

// equals reports whether x and y are equal.
// It panics if x and y are not comparable.
//
// If x and y are not interfaces and are guaranteed to have the same type,
// x.Interface() == y.Interface() can be used as a faster alternative.
func (vm *VM) equals(x, y reflect.Value) bool {
	if x.IsValid() != y.IsValid() {
		return false
	}
	if !x.IsValid() {
		return true
	}
	tx := vm.env.typeof(x)
	ty := vm.env.typeof(y)
	if tx != ty {
		return false
	}
	if t, ok := tx.(ScriggoType); ok {
		if !tx.Comparable() {
			panic("runtime error: comparing uncomparable type " + tx.String())
		}
		x, _ = t.Unwrap(x)
		y, _ = t.Unwrap(y)
	}
	// Use reflect.Value.Equal() to avoid interface boxing allocation
	return x.Equal(y)
}

// fieldByIndex returns the field of the struct s at index i.
// s can be a pointer to a struct value.
//
// It panics with errNilPointer if s represents a nil pointer or accessing
// to a nil embedded struct.
func (vm *VM) fieldByIndex(s reflect.Value, i uint8) reflect.Value {
	v := s
	for _, x := range vm.fn.FieldIndexes[i] {
		if v.Kind() == reflect.Pointer {
			if v.IsNil() {
				panic(errNilPointer)
			}
			v = v.Elem()
		}
		v = v.Field(x)
	}
	return v
}

func (vm *VM) fieldMutationTarget(s reflect.Value, i uint8) reflect.Value {
	v := s
	target := s
	indexes := vm.fn.FieldIndexes[i]
	for n, index := range indexes {
		if v.Kind() == reflect.Pointer {
			if v.IsNil() {
				panic(errNilPointer)
			}
			target = v
			v = v.Elem()
		}
		if n == len(indexes)-1 {
			return target
		}
		v = v.Field(index)
	}
	return target
}

// mapIndexStringKeyFallback performs a generic map index operation for maps with string keys.
// Used as fallback when specialized fast paths don't match (e.g., named types).
// Sets vm.ok to indicate whether the key was found.
func (vm *VM) mapIndexStringKeyFallback(m reflect.Value, key string, c int8) {
	t := m.Type()
	k := reflect.New(t.Key()).Elem()
	k.SetString(key)
	elem := reflect.New(t.Elem()).Elem()
	index := m.MapIndex(k)
	vm.ok = index.IsValid()
	if vm.ok {
		elem.Set(index)
	}
	vm.setFromReflectValue(c, elem)
}

// derefPointer dereferences a pointer value, panicking if nil.
// Returns the dereferenced value, or the original if not a pointer.
func derefPointer(v reflect.Value) reflect.Value {
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			panic(errNilPointer)
		}
		return v.Elem()
	}
	return v
}

func (vm *VM) finalize(regs [][2]int8) {
	for _, reg := range regs {
		vm.setFromReflectValue(reg[1], vm.generalIndirect(reg[0]))
	}
}

func (vm *VM) moreIntStack() {
	top := len(vm.regs.int) * 2
	stack := make([]int64, top)
	copy(stack, vm.regs.int)
	vm.regs.int = stack
	vm.st[0] = Addr(top)
}

func (vm *VM) moreFloatStack() {
	top := len(vm.regs.float) * 2
	stack := make([]float64, top)
	copy(stack, vm.regs.float)
	vm.regs.float = stack
	vm.st[1] = Addr(top)
}

func (vm *VM) moreStringStack() {
	top := len(vm.regs.string) * 2
	stack := make([]string, top)
	copy(stack, vm.regs.string)
	vm.regs.string = stack
	vm.st[2] = Addr(top)
}

func (vm *VM) moreGeneralStack() {
	top := len(vm.regs.general) * 2
	stack := make([]reflect.Value, top)
	copy(stack, vm.regs.general)
	vm.regs.general = stack
	vm.st[3] = Addr(top)
}

func (vm *VM) nextCall() bool {
	for i := len(vm.calls) - 1; i >= 0; i-- {
		call := vm.calls[i]
		switch call.status {
		case started:
			// A call is returned, continue with the previous call.
		case tailed:
			// A tail call is returned, continue with the previous call.
			continue
		case deferred:
			// A call, that has deferred calls, is returned, its first
			// deferred call will be executed.
			current := callFrame{cl: callable{fn: vm.fn}, renderer: vm.renderer, fp: vm.fp, status: returned}
			vm.swapStack(&call.fp, &current.fp, current.cl.fn.NumReg)
			vm.calls[i] = current
			i++
		case returned, recovered:
			// A deferred call is returned. If there is another deferred
			// call, it will be executed, otherwise the previous call will be
			// finalized.
			if i > 0 {
				prev := vm.calls[i-1]
				if prev.status == deferred {
					vm.swapStack(&prev.fp, &call.fp, call.cl.fn.NumReg)
					call, vm.calls[i-1] = prev, call
					break
				}
			}
			if regs := call.cl.fn.FinalRegs; regs != nil {
				vm.fp = call.fp
				vm.finalize(regs)
			}
			if call.status == recovered {
				numPanicked := 0
				for _, c := range vm.calls {
					if c.status == panicked {
						numPanicked++
					}
				}
				num := 0
				for p := vm.panic; p != nil; p = p.next {
					num++
				}
				for p := vm.panic; num > numPanicked; num-- {
					p = p.next
					vm.panic = p
				}
			}
			continue
		case panicked:
			// A call is panicked, the first deferred call in the call stack,
			// if there is one, will be executed.
			for i = i - 1; i >= 0; i-- {
				call = vm.calls[i]
				if call.status == deferred {
					vm.calls[i] = vm.calls[i+1]
					vm.calls[i].status = panicked
					if call.cl.fn != nil {
						i++
					}
					break
				}
			}
		}
		if i >= 0 {
			if call.cl.fn != nil {
				vm.calls = vm.calls[:i]
				vm.fp = call.fp
				vm.pc = call.pc
				vm.fn = call.cl.fn
				vm.vars = call.cl.vars
				vm.renderer = call.renderer
				vm.rangesBase = call.rangesLen
				return true
			}
			vm.fp = call.fp
			vm.callNative(call.cl.Native(), call.numVariadic, StackShift{}, false)
		}
	}
	return false
}

// create creates a new virtual machine with the execution environment env.
func create(env *env) *VM {
	vm := &VM{
		st: [4]Addr{stackSize, stackSize, stackSize, stackSize},
		regs: registers{
			int:     make([]int64, stackSize),
			float:   make([]float64, stackSize),
			string:  make([]string, stackSize),
			general: make([]reflect.Value, stackSize),
		},
	}
	if env != nil {
		vm.env = env
		vm.nativeEnv = &nativeEnv{env: env, ctx: env.ctx}
		vm.envArg = reflect.ValueOf(vm.nativeEnv)
		vm.setExecutionContext(env.ctx)
	}
	return vm
}

// startGoroutine starts a new goroutine to execute a function call at program
// counter pc. If the function is native, returns true.
func (vm *VM) startGoroutine() bool {
	var fn *Function
	var vars []reflect.Value
	call := vm.fn.Body[vm.pc]
	switch call.Op {
	case OpCallFunc:
		fn = vm.fn.Functions[uint8(call.A)]
		vars = vm.env.globals
	case OpCallIndirect:
		f := vm.general(call.A).Interface().(*callable)
		if f.fn == nil {
			if f.native.value.IsNil() {
				panic(errors.New("fatal error: go of nil func value"))
			}
			return true
		}
		fn = f.fn
		vars = f.vars
	default:
		return true
	}
	nvm := getPooledVM(vm.env, vm.env.deriveChildContext(vm.nativeEnv.ctx))
	vm.pc++
	off := vm.fn.Body[vm.pc]
	nvm.noteCopiedRegisters(0, copy(nvm.regs.int, vm.regs.int[vm.fp[0]+Addr(off.Op):vm.fp[0]+127]))
	nvm.noteCopiedRegisters(1, copy(nvm.regs.float, vm.regs.float[vm.fp[1]+Addr(off.A):vm.fp[1]+127]))
	nvm.noteCopiedRegisters(2, copy(nvm.regs.string, vm.regs.string[vm.fp[2]+Addr(off.B):vm.fp[2]+127]))
	nvm.noteCopiedRegisters(3, copy(nvm.regs.general, vm.regs.general[vm.fp[3]+Addr(off.C):vm.fp[3]+127]))
	go func() {
		defer putPooledVM(nvm)
		nvm.runFunc(fn, vars)
	}()
	vm.pc++
	return false
}

func (vm *VM) detachedScriggoCall() bool {
	call := vm.fn.Body[vm.pc]
	switch call.Op {
	case OpCallFunc:
		return true
	case OpCallIndirect:
		return vm.general(call.A).Interface().(*callable).fn != nil
	}
	return false
}

// swapStack swaps the stacks pointed by a and b. bSize is the size of the
// stack pointed by b. The stacks must be consecutive and a must precede b.
//
// A stack can have zero size, so a and b can point to the same index.
func (vm *VM) swapStack(a, b *[4]Addr, bSize StackShift) {

	// Swap int registers.
	as := b[0] - a[0]
	bs := Addr(bSize[0])
	if as > 0 && bs > 0 {
		tot := as + bs
		if a[0]+tot+bs > vm.st[0] {
			vm.moreIntStack()
		}
		vm.noteHigh(0, a[0]+tot+bs)
		s := vm.regs.int[a[0]+1:]
		copy(s[bs:], s[:tot])
		copy(s, s[tot:tot+bs])
	}
	b[0] = a[0]
	a[0] += bs

	// Swap float registers.
	as = b[1] - a[1]
	bs = Addr(bSize[1])
	if as > 0 && bs > 0 {
		tot := as + bs
		if a[1]+tot+bs > vm.st[1] {
			vm.moreFloatStack()
		}
		vm.noteHigh(1, a[1]+tot+bs)
		s := vm.regs.float[a[1]+1:]
		copy(s[bs:], s[:tot])
		copy(s, s[tot:tot+bs])
	}
	b[1] = a[1]
	a[1] += bs

	// Swap string registers.
	as = b[2] - a[2]
	bs = Addr(bSize[2])
	if as > 0 && bs > 0 {
		tot := as + bs
		if a[2]+tot+bs > vm.st[2] {
			vm.moreStringStack()
		}
		vm.noteHigh(2, a[2]+tot+bs)
		s := vm.regs.string[a[2]+1:]
		copy(s[bs:], s[:tot])
		copy(s, s[tot:tot+bs])
	}
	b[2] = a[2]
	a[2] += bs

	// Swap general registers.
	as = b[3] - a[3]
	bs = Addr(bSize[3])
	if as > 0 && bs > 0 {
		tot := as + bs
		if a[3]+tot+bs > vm.st[3] {
			vm.moreGeneralStack()
		}
		vm.noteHigh(3, a[3]+tot+bs)
		s := vm.regs.general[a[3]+1:]
		copy(s[bs:], s[:tot])
		copy(s, s[tot:tot+bs])
	}
	b[3] = a[3]
	a[3] += bs

}

type Registers struct {
	Int     []int64
	Float   []float64
	String  []string
	General []reflect.Value
}

type NativeFunction struct {
	pkg               string            // package.
	name              string            // name.
	function          any               // value.
	outOff            [4]int8           // offset of out arguments.
	value             reflect.Value     // reflect value.
	identity          reflect.Value     // comparable function identity.
	reflectCall       bool              // reports whether it can be called only with reflect.
	mutationOperation MutationOperation // compiler-generated mutating builtin.
	methodReceiver    int               // index of a method-expression receiver; -1 for a function.
	boundReceiver     reflect.Value     // receiver captured by a method value.
	frameLayout       nativeFunctionFrameLayout
	frameCallKind     nativeFrameCallKind
}

// NewNativeFunction returns a new native function given its package and name
// and the function value or its reflect value. pkg and name can be empty
// strings.
func NewNativeFunction(pkg, name string, function any) *NativeFunction {
	fn := &NativeFunction{}
	initNativeFunction(fn, pkg, name, function)
	return fn
}

func initNativeFunction(fn *NativeFunction, pkg, name string, function any) {
	*fn = NativeFunction{
		pkg:            pkg,
		name:           name,
		methodReceiver: -1,
	}
	if value, ok := function.(reflect.Value); ok {
		fn.value = value
	} else {
		fn.function = function
		fn.value = reflect.ValueOf(function)
	}
	typ := fn.value.Type()
	fn.identity = comparableNativeFunction(fn.value)
	nOut := typ.NumOut()
	for i := range nOut {
		k := typ.Out(i).Kind()
		fn.outOff[kindToType[k]]++
	}
	fn.frameLayout = makeNativeFunctionFrameLayout(typ)
	if NativeFunctionUsesReflection(typ) {
		fn.reflectCall = true
		return
	}
	if fn.function == nil {
		fn.function = fn.value.Interface()
	}
}

func initReflectNativeFunction(fn *NativeFunction, value reflect.Value) {
	*fn = NativeFunction{
		value:          value,
		identity:       comparableNativeFunction(value),
		reflectCall:    true,
		methodReceiver: -1,
	}
	for index := range value.Type().NumOut() {
		fn.outOff[kindToType[value.Type().Out(index).Kind()]]++
	}
	fn.frameLayout = makeNativeFunctionFrameLayout(value.Type())
}

func comparableNativeFunction(value reflect.Value) reflect.Value {
	if !value.IsValid() || value.Kind() != reflect.Func || !value.CanInterface() {
		return reflect.Value{}
	}
	return reflect.ValueOf(value.Interface())
}

func nativeFunctionAcceptsEnvironment(functionType reflect.Type) bool {
	for index := 0; index < min(functionType.NumIn(), 2); index++ {
		if functionType.In(index) == envType {
			return true
		}
	}
	return false
}

// NativeFunctionUsesReflection reports whether a function type uses the
// reflective native call path.
func NativeFunctionUsesReflection(typ reflect.Type) bool {
	switch typ {
	case reflect.TypeFor[func(string) int](),
		reflect.TypeFor[func(string) string](),
		reflect.TypeFor[func(string, string) int](),
		reflect.TypeFor[func(string, int) string](),
		reflect.TypeFor[func(string, string) bool](),
		reflect.TypeFor[func(interface{}) bool](),
		reflect.TypeFor[func(interface{}) string](),
		reflect.TypeFor[func(interface{}, interface{}) interface{}](),
		reflect.TypeFor[func(interface{}, interface{}) bool](),
		reflect.TypeFor[func(interface{}, string) string](),
		reflect.TypeFor[func(interface{}, ...string) interface{}](),
		reflect.TypeFor[func(interface{}) int](),
		reflect.TypeFor[func(interface{}) []string](),
		reflect.TypeFor[func(interface{}) []interface{}](),
		reflect.TypeFor[func(interface{}, interface{}) string](),
		reflect.TypeFor[func(interface{}, interface{}) []string](),
		reflect.TypeFor[func(interface{}, interface{}) []interface{}](),
		reflect.TypeFor[func(interface{}, interface{}, interface{}) string](),
		reflect.TypeFor[func(float64) float64](),
		reflect.TypeFor[func(int) []int](),
		reflect.TypeFor[func(interface{}, string) []string](),
		reflect.TypeFor[func(interface{}, int, int) []interface{}](),
		reflect.TypeFor[func(string, string) string](),
		reflect.TypeFor[func(...interface{}) string](),
		reflect.TypeFor[func(string, ...interface{}) string](),
		reflect.TypeFor[func(interface{}, string, ...interface{}) []interface{}](),
		reflect.TypeFor[func(interface{}) (float64, error)](),
		reflect.TypeFor[func(interface{}) (string, error)](),
		reflect.TypeFor[func(interface{}, ...interface{}) (string, error)](),
		reflect.TypeFor[func([]interface{}, []string) ([]interface{}, error)](),
		reflect.TypeFor[func(native.Env, ...interface{}) bool](),
		reflect.TypeFor[func(native.Env, string) string](),
		reflect.TypeFor[func(native.Env, interface{}, interface{}) bool]():
		return false
	}
	return true
}

func (fn *NativeFunction) Package() string {
	return fn.pkg
}

func (fn *NativeFunction) Name() string {
	return fn.name
}

func (fn *NativeFunction) Func() any {
	return fn.function
}

// SetMutationOperation marks a compiler-generated mutating builtin wrapper.
func (fn *NativeFunction) SetMutationOperation(operation MutationOperation) {
	fn.mutationOperation = operation
}

// SetMethodReceiver marks an argument as the receiver of a native method.
func (fn *NativeFunction) SetMethodReceiver(index int) {
	fn.methodReceiver = index
	fn.reflectCall = true
}

// SetBoundMethodReceiver marks the receiver captured by a native method value.
func (fn *NativeFunction) SetBoundMethodReceiver(receiver reflect.Value, method string) {
	fn.boundReceiver = receiver
	fn.name = method
	fn.reflectCall = true
}

func (fn *NativeFunction) receiver(args []reflect.Value) reflect.Value {
	if fn.boundReceiver.IsValid() {
		return fn.boundReceiver
	}
	if fn.methodReceiver >= 0 && fn.methodReceiver < len(args) {
		return args[fn.methodReceiver]
	}
	return reflect.Value{}
}

// Function represents a function.
type Function struct {
	Pkg    string
	Name   string
	File   string
	Pos    *Position // position of the function declaration.
	Type   reflect.Type
	Parent *Function

	// VarRefs refers to the non-local variables referenced in this function.
	//
	// If VarRefs is nil, it means that the non-local variables are the global
	// variables passed through the env.
	//
	// Otherwise, for each var Ref in VarRefs:
	//
	// * if Ref >= 0, Ref refers to the non-local variable at index Ref of the
	//   currently executing function
	//
	// * if Ref < 0, it refers to the general register -Ref of the currently
	//   executing function, which will contain the value of that variable
	//   (indirectly).
	VarRefs []int16

	// GlobalVarRefs maps VarRefs entries to canonical globals. A negative value
	// identifies a function-local capture.
	GlobalVarRefs []int16

	// InternalFunctionGlobals identifies Scriggo-owned function slots that remain callable inside the VM.
	InternalFunctionGlobals []bool

	Types           []reflect.Type
	NumReg          [4]int8
	FinalRegs       [][2]int8 // [indirect -> return parameter registers]
	Macro           bool
	Format          ast.Format
	Values          Registers
	FieldIndexes    [][]int
	Functions       []*Function
	NativeFunctions []*NativeFunction
	Body            []Instruction
	Text            [][]byte
	InstructionInfo map[Addr]InstructionInfo
	// RangeTypes maps range instruction addresses to their static operand type.
	RangeTypes map[Addr]reflect.Type
	UsesPrint  bool

	// Specialization metadata for bytecode quickening (Phase 3).
	// Lazily allocated on first specialization attempt.
	// Each entry corresponds to an instruction in Body at the same index.
	specializations []SpecializationMetadata
	specInit        sync.Once

	deterministicValidation sync.Once
	deterministicErr        error
	batchValidation         sync.Once
	batchErr                error
	BatchUnsafeNative       string
}

// ensureSpecializations lazily allocates the specializations slice.
// Safe for concurrent calls from multiple VMs.
func (fn *Function) ensureSpecializations() {
	fn.specInit.Do(func() {
		fn.specializations = make([]SpecializationMetadata, len(fn.Body))
	})
}

// GetSpecialization returns the specialization metadata for instruction at addr.
// Returns nil if specializations haven't been allocated yet.
func (fn *Function) GetSpecialization(addr Addr) *SpecializationMetadata {
	if fn.specializations == nil || int(addr) >= len(fn.specializations) {
		return nil
	}
	return &fn.specializations[addr]
}

// TrySpecialize attempts to specialize an instruction at addr.
// Returns the specialized opcode if successful, or 0 if already specialized/failed.
// Uses atomic CAS to ensure only one goroutine performs specialization.
func (fn *Function) TrySpecialize(addr Addr, specialize func(*SpecializationMetadata) Operation) Operation {
	fn.ensureSpecializations()

	spec := &fn.specializations[addr]

	// Try to transition from unspecialized to in-progress
	if !atomic.CompareAndSwapUint32(&spec.state, specStateUnspecialized, specStateInProgress) {
		// Already being specialized or already done
		return 0
	}

	// Perform specialization
	newOp := specialize(spec)

	if newOp != 0 {
		atomic.StoreUint32(&spec.state, specStateSpecialized)
	} else {
		atomic.StoreUint32(&spec.state, specStateDeoptimized)
	}

	return newOp
}

// Position represents a source position.
type Position struct {
	Line   int // line starting from 1
	Column int // column in characters starting from 1
	Start  int // index of the first byte
	End    int // index of the last byte
}

func (p Position) String() string {
	return strconv.Itoa(p.Line) + ":" + strconv.Itoa(p.Column)
}

// InstructionInfo represents a set of information associated to a given
// instruction. None of the fields below is mandatory.
type InstructionInfo struct {
	Position    Position        // position of the instruction in the source code.
	Path        string          // path of the source code where the instruction is located in.
	OperandKind [3]reflect.Kind // kind of operands A, B and C.
	FuncType    reflect.Type    // type of the function that is called; only for call instructions.
}

type Addr uint32

type callStatus int8

const (
	started callStatus = iota
	tailed
	returned
	deferred
	panicked
	recovered
)

// If the size of callFrame changes, update the constant CallFrameSize.
type callFrame struct {
	cl          callable   // callable.
	renderer    *renderer  // renderer
	fp          [4]Addr    // frame pointers.
	pc          Addr       // program counter.
	status      callStatus // status.
	numVariadic int8       // number of variadic arguments.
	ok          bool       // saved vm.ok for restoration after call.
	rangesLen   int        // saved len(vm.ranges) for cleanup after call.
	rangesBase  int        // saved vm.rangesBase for restoration after call.
}

type callable struct {
	value  reflect.Value   // reflect value.
	fn     *Function       // function, if it is a Scriggo function.
	native *NativeFunction // native function.
	vars   []reflect.Value // non-local (global and closure) variables.
}

type nativeCallable struct {
	callable callable
	native   NativeFunction
}

func newNativeCallable(value reflect.Value) *callable {
	c := &nativeCallable{}
	initNativeFunction(&c.native, "", "", value)
	c.callable.native = &c.native
	return &c.callable
}

func newNativeMethodCallable(value, receiver reflect.Value, name string) *callable {
	c := &nativeCallable{}
	initNativeFunction(&c.native, "", name, value)
	c.native.SetBoundMethodReceiver(receiver, name)
	c.callable.native = &c.native
	return &c.callable
}

// Native returns the native function of a callable.
func (c *callable) Native() *NativeFunction {
	if c.native != nil {
		return c.native
	}
	c.native = NewNativeFunction("", "", c.value)
	return c.native
}

// Value returns a reflect.Value of a callable, so it can be called from a
// native code and passed to a native code.
func (c *callable) Value(env *env, parentContext context.Context) reflect.Value {
	var vectorGeneration uint64
	if env.vector != nil {
		vectorGeneration = env.vector.capture()
	}
	if c.native != nil {
		fn := c.native
		method := fn.boundReceiver.IsValid() || fn.methodReceiver >= 0
		before := env.beforeNativeCall
		beforeContext := env.beforeNativeCallContext
		observer := env.observeNativeCall
		observerContext := env.observeNativeCallContext
		if before != nil || beforeContext != nil ||
			method && (observer != nil || observerContext != nil) || env.vector != nil {
			return reflect.MakeFunc(fn.value.Type(), func(args []reflect.Value) []reflect.Value {
				callContext := parentContext
				var lease *vectorInvocationLease
				release := func() {}
				if env.vector != nil {
					var err error
					lease, callContext, release, err = acquireVectorInvocation(
						env.vector,
						vectorGeneration,
						parentContext,
					)
					if err != nil {
						panic(stopError{err})
					}
					defer release()
					callContext = &vectorInvocationContext{Context: callContext, lease: lease}
				}
				var callEnv *nativeEnv
				if before != nil || method && observer != nil {
					callEnv = &nativeEnv{
						env:              env,
						ctx:              callContext,
						vector:           env.vector,
						vectorGeneration: vectorGeneration,
						vectorLease:      lease,
					}
				}
				if beforeContext != nil {
					runBeforeNativeCallContext(beforeContext, callContext)
				}
				if before != nil {
					runBeforeNativeCall(before, callEnv)
				}
				if method && (observer != nil || observerContext != nil) {
					receiver := fn.receiver(args)
					call := NativeCall{
						Receiver: receiver,
						Method:   fn.name,
					}
					if observerContext != nil {
						if err := observerContext(callContext, call); err != nil {
							panic(stopError{err})
						}
					}
					if observer != nil {
						if err := observer(callEnv, call); err != nil {
							panic(stopError{err})
						}
					}
				}
				if fn.value.Type().IsVariadic() {
					return fn.value.CallSlice(args)
				}
				return fn.value.Call(args)
			})
		}
		return fn.value
	}
	if c.value.IsValid() && env.vector == nil && (c.fn == nil || env.childContext == nil) {
		return c.value
	}
	// It is a Scriggo function.
	fn := c.fn
	vars := c.vars
	value := reflect.MakeFunc(fn.Type, func(args []reflect.Value) []reflect.Value {
		callContext := parentContext
		var lease *vectorInvocationLease
		release := func() {}
		if env.vector != nil {
			var err error
			lease, callContext, release, err = acquireVectorInvocation(
				env.vector,
				vectorGeneration,
				parentContext,
			)
			if err != nil {
				panic(stopError{err})
			}
			defer release()
			callContext = &vectorInvocationContext{
				Context: env.deriveChildContext(callContext),
				lease:   lease,
			}
		} else {
			callContext = env.deriveChildContext(callContext)
		}
		// Pooled, not create(): a fresh VM allocates four register banks of
		// stackSize entries (~28 KB), and this path runs once per call from
		// native code. A native helper that invokes a template closure per
		// slice element therefore paid that on every element — measured at
		// ~32 KB and ~5.6 µs per call, which made an element-wise pipeline
		// ~9x slower and ~100x hungrier than the equivalent template loop.
		// The deferred put must survive a panic: runFunc panics to propagate
		// template errors, and leaking the VM would defeat the pool exactly
		// when a render is already failing.
		nvm := getPooledVM(env, callContext)
		if lease != nil {
			nvm.nativeEnv.vector = env.vector
			nvm.nativeEnv.vectorGeneration = vectorGeneration
			nvm.nativeEnv.vectorLease = lease
		}
		defer putPooledVM(nvm)
		if fn.Macro {
			nvm.setRenderer(&strings.Builder{})
		}
		nOut := fn.Type.NumOut()
		results := make([]reflect.Value, nOut)
		var r = [4]int8{1, 1, 1, 1}
		for i := range nOut {
			typ := fn.Type.Out(i)
			results[i] = reflect.New(typ).Elem()
			t := kindToType[typ.Kind()]
			r[t]++
		}
		for _, arg := range args {
			t := kindToType[arg.Kind()]
			nvm.setFromReflectValue(r[t], arg)
			r[t]++
		}
		err := nvm.runFunc(fn, vars)
		if err != nil {
			if ctx := nvm.nativeEnv.ctx; ctx != nil && err == ctx.Err() {
				panic(stopError{err})
			}
			if p, ok := err.(*PanicError); ok {
				var msg string
				for ; p != nil; p = p.next {
					msg = "\n" + msg
					if p.recovered {
						msg = " [recovered]" + msg
					}
					msg = p.String() + msg
					if p.next != nil {
						msg = "\tpanic: " + msg
					}
				}
				err = &fatalError{msg: msg}
			}
			panic(err)
		}
		if fn.Macro {
			b := nvm.renderer.Out().(*strings.Builder)
			nvm.setString(1, b.String())
		}
		r = [4]int8{1, 1, 1, 1}
		for _, result := range results {
			t := kindToType[result.Kind()]
			nvm.getIntoReflectValue(r[t], result, false)
			r[t]++
		}
		return results
	})
	if env.vector == nil && env.childContext == nil {
		c.value = value
	}
	return value
}

func packageName(pkg string) string {
	for i := len(pkg) - 1; i >= 0; i-- {
		if pkg[i] == '/' {
			return pkg[i+1:]
		}
	}
	return pkg
}

type registerType int8

const (
	intRegister registerType = iota
	floatRegister
	stringRegister
	generalRegister
)

// kindToType maps a reflect kind to the corresponding internal register type.
var kindToType = [...]registerType{
	reflect.Float32:       floatRegister,
	reflect.Float64:       floatRegister,
	reflect.Complex64:     generalRegister,
	reflect.Complex128:    generalRegister,
	reflect.Array:         generalRegister,
	reflect.Chan:          generalRegister,
	reflect.Func:          generalRegister,
	reflect.Interface:     generalRegister,
	reflect.Map:           generalRegister,
	reflect.Pointer:       generalRegister,
	reflect.Slice:         generalRegister,
	reflect.String:        stringRegister,
	reflect.Struct:        generalRegister,
	reflect.UnsafePointer: generalRegister,
}

type Condition int8

const (
	ConditionZero                 Condition = iota // x == 0
	ConditionNotZero                               // x != 0
	ConditionEqual                                 // x == y
	ConditionNotEqual                              // x != y
	ConditionInterfaceEqual                        // x == y
	ConditionInterfaceNotEqual                     // x != y
	ConditionLess                                  // x <  y
	ConditionLessEqual                             // x <= y
	ConditionGreater                               // x >  y
	ConditionGreaterEqual                          // x >= y
	ConditionLessU                                 // x <  y (unsigned)
	ConditionLessEqualU                            // x <= y (unsigned)
	ConditionGreaterU                              // x >  y (unsigned)
	ConditionGreaterEqualU                         // x >= y (unsigned)
	ConditionLenEqual                              // len(x) == y
	ConditionLenNotEqual                           // len(x) != y
	ConditionLenLess                               // len(x) <  y
	ConditionLenLessEqual                          // len(x) <= y
	ConditionLenGreater                            // len(x) >  y
	ConditionLenGreaterEqual                       // len(x) >= y
	ConditionInterfaceNil                          // x == nil
	ConditionInterfaceNotNil                       // x != nil
	ConditionNil                                   // x == nil
	ConditionNotNil                                // x != nil
	ConditionContainsSubstring                     // x contains y
	ConditionContainsRune                          // x contains y
	ConditionContainsElement                       // x contains y
	ConditionContainsKey                           // x contains y
	ConditionContainsNil                           // x contains nil
	ConditionNotContainsSubstring                  // x not contains y
	ConditionNotContainsRune                       // x not contains y
	ConditionNotContainsElement                    // x not contains y
	ConditionNotContainsKey                        // x not contains y
	ConditionNotContainsNil                        // x not contains nil
	ConditionOK                                    // [vm.ok]
	ConditionNotOK                                 // ![vm.ok]
)

type Operation int8

const (
	OpNone Operation = iota

	OpAdd
	OpAddInt
	OpAddFloat64

	OpAddr

	OpAnd

	OpAndNot

	OpAssert

	OpAppend

	OpAppendSlice

	OpBreak

	OpCallFunc
	OpCallIndirect
	OpCallMacro
	OpCallNative

	OpCap

	OpCase

	OpClose

	OpClear

	OpComplex64
	OpComplex128

	OpConcat

	OpContinue

	OpConvert
	OpConvertInt
	OpConvertUint
	OpConvertFloat
	OpConvertString
	OpConvertFromInterface

	OpCopy

	OpDefer

	OpDelete

	OpDiv
	OpDivInt
	OpDivFloat64

	OpField

	OpGetVar

	OpGetVarAddr

	OpGo

	OpGoRender
	OpGoRenderIndirect

	OpGoRenderWait

	OpGoto

	OpIf
	OpIfInt
	OpIfFloat
	OpIfString

	OpIndex
	OpIndexString

	OpIndexRef

	OpLen

	OpLoad

	OpLoadFunc

	OpMakeArray

	OpMakeChan

	OpMakeMap

	OpMakeSlice

	OpMakeStruct

	OpMapIndex
	OpMapIndexAny

	OpMethodValue

	OpMove

	OpMul
	OpMulInt
	OpMulFloat64

	OpNeg

	OpNew

	OpOr

	OpPanic

	OpPrint

	OpRange

	OpRangeInterface

	OpRangeString

	OpRealImag

	OpReceive

	OpRecover

	OpRem
	OpRemInt

	OpReturn

	OpSelect

	OpSend

	OpSetField

	OpSetMap

	OpSetSlice

	OpSetVar

	OpShl
	OpShlInt

	OpShow

	OpShr
	OpShrInt

	OpSlice

	OpStringSlice

	OpSub
	OpSubInt
	OpSubFloat64

	OpSubInv
	OpSubInvInt
	OpSubInvFloat64

	OpTailCall

	OpText

	OpTypify

	OpXor

	OpZero

	// Super-instructions: fused opcodes for common patterns
	OpFieldShow    // Fused: OpField + OpShow (field access followed by display)
	OpMapIndexShow // Fused: OpMapIndex + OpShow (map access followed by display)

	// Specialized opcodes for common type patterns (Phase 3 - Bytecode Specialization)
	OpMapIndexStringString // Specialized: map[string]string index
	OpMapIndexStringInt    // Specialized: map[string]int index
	OpMapIndexStringBool   // Specialized: map[string]bool index
	OpFieldDirect          // Specialized: direct single-level struct field access
	OpCallNativeValue      // Compiler-certified synchronous native function value call
	OpCallNativeFrameMethod
	OpFieldNativeFunc // Loads a function field without allocating a callable wrapper
	OpMethodValueFrame
)
