// Copyright 2019 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	goruntime "runtime"
	"slices"
	"strings"
	"unicode"

	"gitlab.com/haproxy-haptic/scriggo/ast"
)

// panicErrorWithStack creates a PanicError from a recovered value with Go stack trace.
// Used in parallel render goroutines where we don't have access to VM's Stack method.
func panicErrorWithStack(r interface{}) *PanicError {
	buf := make([]byte, 8192) // Larger buffer for more complete stack traces
	n := goruntime.Stack(buf, false)
	return &PanicError{
		message:    fmt.Sprintf("panic in parallel render: %v", r),
		stackTrace: buf[:n],
	}
}

// panicErrorWithContext creates a PanicError with function context for better debugging.
func panicErrorWithContext(r interface{}, fnName, fnPkg string) *PanicError {
	buf := make([]byte, 8192)
	n := goruntime.Stack(buf, false)
	var msg string
	if fnName != "" {
		msg = fmt.Sprintf("panic in parallel render of %s.%s: %v", fnPkg, fnName, r)
	} else {
		msg = fmt.Sprintf("panic in parallel render: %v", r)
	}
	return &PanicError{
		message:    msg,
		stackTrace: buf[:n],
	}
}

// parallelRenderArgs holds captured argument registers for parallel render goroutines.
// Arguments must be copied before spawning the goroutine since the parent VM continues
// and may overwrite its registers.
type parallelRenderArgs struct {
	ints     []int64
	floats   []float64
	strings  []string
	generals []reflect.Value
}

// parallelRenderArgsPool is a pool for parallelRenderArgs to reduce allocations.
// Slices are allocated at maxRegisterIndex capacity since captureArgs needs up to that size.
var parallelRenderArgsPool = newRetainedPool(256, func() *parallelRenderArgs {
	return &parallelRenderArgs{
		ints:     make([]int64, 0, maxRegisterIndex),
		floats:   make([]float64, 0, maxRegisterIndex),
		strings:  make([]string, 0, maxRegisterIndex),
		generals: make([]reflect.Value, 0, maxRegisterIndex),
	}
})

// putParallelRenderArgs returns a parallelRenderArgs to the pool after resetting it.
func putParallelRenderArgs(args *parallelRenderArgs) {
	// Clear slices to allow GC but keep capacity
	for i := range args.strings {
		args.strings[i] = ""
	}
	for i := range args.generals {
		args.generals[i] = reflect.Value{}
	}
	args.ints = args.ints[:0]
	args.floats = args.floats[:0]
	args.strings = args.strings[:0]
	args.generals = args.generals[:0]
	parallelRenderArgsPool.put(args)
}

// captureArgs extracts arguments from VM registers based on stackShift.
// Used by OpGoRender and OpGoRenderIndirect to prepare arguments for parallel execution.
// The returned *parallelRenderArgs is from the pool and must be returned via putParallelRenderArgs.
func (vm *VM) captureArgs(stackShift StackShift) *parallelRenderArgs {
	for i, shift := range stackShift {
		if shift < 0 || int(shift) > maxRegisterIndex {
			panic(vm.newPanic(fmt.Errorf("invalid stack shift %d in register type %d", shift, i)))
		}
	}
	args := parallelRenderArgsPool.get()
	intLen := maxRegisterIndex - int(stackShift[0])
	floatLen := maxRegisterIndex - int(stackShift[1])
	stringLen := maxRegisterIndex - int(stackShift[2])
	generalLen := maxRegisterIndex - int(stackShift[3])
	args.ints = args.ints[:intLen]
	args.floats = args.floats[:floatLen]
	args.strings = args.strings[:stringLen]
	args.generals = args.generals[:generalLen]
	if intLen > 0 {
		copy(args.ints, vm.regs.int[vm.fp[0]+Addr(stackShift[0]):vm.fp[0]+maxRegisterIndex])
	}
	if floatLen > 0 {
		copy(args.floats, vm.regs.float[vm.fp[1]+Addr(stackShift[1]):vm.fp[1]+maxRegisterIndex])
	}
	if stringLen > 0 {
		copy(args.strings, vm.regs.string[vm.fp[2]+Addr(stackShift[2]):vm.fp[2]+maxRegisterIndex])
	}
	if generalLen > 0 {
		copy(args.generals, vm.regs.general[vm.fp[3]+Addr(stackShift[3]):vm.fp[3]+maxRegisterIndex])
	}
	return args
}

// copyToVM copies captured arguments to a child VM's registers.
func (args *parallelRenderArgs) copyToVM(nvm *VM) {
	copy(nvm.regs.int, args.ints)
	copy(nvm.regs.float, args.floats)
	copy(nvm.regs.string, args.strings)
	copy(nvm.regs.general, args.generals)
	nvm.noteCopiedRegisters(0, len(args.ints))
	nvm.noteCopiedRegisters(1, len(args.floats))
	nvm.noteCopiedRegisters(2, len(args.strings))
	nvm.noteCopiedRegisters(3, len(args.generals))
}

// toReflectValues converts captured arguments to []reflect.Value for native function calls.
// Uses the function type to determine how to interpret each argument.
// Arguments are consumed in order by type (ints, floats, strings, generals).
func (args *parallelRenderArgs) toReflectValues(fnType reflect.Type) []reflect.Value {
	numArgs := fnType.NumIn()
	result := make([]reflect.Value, numArgs)
	intIdx, floatIdx, stringIdx, generalIdx := 0, 0, 0, 0

	for i := 0; i < numArgs; i++ {
		argType := fnType.In(i)
		switch argType.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			if intIdx < len(args.ints) {
				result[i] = reflect.ValueOf(args.ints[intIdx]).Convert(argType)
				intIdx++
			} else {
				result[i] = reflect.Zero(argType)
			}
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			if intIdx < len(args.ints) {
				result[i] = reflect.ValueOf(uint64(args.ints[intIdx])).Convert(argType)
				intIdx++
			} else {
				result[i] = reflect.Zero(argType)
			}
		case reflect.Bool:
			// Booleans are stored in int registers (0 or 1)
			if intIdx < len(args.ints) {
				result[i] = reflect.ValueOf(args.ints[intIdx] != 0)
				intIdx++
			} else {
				result[i] = reflect.Zero(argType)
			}
		case reflect.Float32, reflect.Float64:
			if floatIdx < len(args.floats) {
				result[i] = reflect.ValueOf(args.floats[floatIdx]).Convert(argType)
				floatIdx++
			} else {
				result[i] = reflect.Zero(argType)
			}
		case reflect.String:
			if stringIdx < len(args.strings) {
				result[i] = reflect.ValueOf(args.strings[stringIdx])
				stringIdx++
			} else {
				result[i] = reflect.Zero(argType)
			}
		default:
			// Find the next general value that's assignable to argType.
			// Skip internal runtime values like *callable that may be in the registers.
			found := false
			searchStart := generalIdx
			for j := searchStart; j < len(args.generals); j++ {
				v := args.generals[j]
				if !v.IsValid() {
					continue
				}
				// Direct type match
				if v.Type().AssignableTo(argType) {
					result[i] = v
					generalIdx = j + 1
					found = true
					break
				}
				// For interface types, check if the underlying value is assignable
				if v.Kind() == reflect.Interface && !v.IsNil() {
					elem := v.Elem()
					if elem.Type().AssignableTo(argType) {
						result[i] = elem
						generalIdx = j + 1
						found = true
						break
					}
				}
			}
			// If no matching value found, use zero value
			if !found {
				result[i] = reflect.Zero(argType)
			}
		}
	}
	return result
}

// runScriggoMacroParallel runs a Scriggo function in a goroutine and sends result to future.
// This centralizes the VM setup, argument copying, execution, and cleanup.
func runScriggoMacroParallel(fn *Function, fnVars []reflect.Value, args *parallelRenderArgs,
	future *renderFuture, parentEnv *env, childContext context.Context) {
	// Capture function info for error reporting before any operations
	fnName := fn.Name
	fnPkg := fn.Pkg

	nvm := getPooledVM(parentEnv, childContext)
	nvm.vars = fnVars
	if nvm.vars == nil {
		nvm.vars = parentEnv.globals
	}

	buf := stringBuilderPool.get()
	buf.Reset()
	nvm.setRenderer(buf)

	nvm.fn = fn
	nvm.fp = [4]Addr{0, 0, 0, 0}
	nvm.pc = 0

	args.copyToVM(nvm)
	putParallelRenderArgs(args)

	if fn.NumReg[0] > 0 && Addr(fn.NumReg[0]) > nvm.st[0] {
		nvm.moreIntStack()
	}
	if fn.NumReg[1] > 0 && Addr(fn.NumReg[1]) > nvm.st[1] {
		nvm.moreFloatStack()
	}
	if fn.NumReg[2] > 0 && Addr(fn.NumReg[2]) > nvm.st[2] {
		nvm.moreStringStack()
	}
	if fn.NumReg[3] > 0 && Addr(fn.NumReg[3]) > nvm.st[3] {
		nvm.moreGeneralStack()
	}

	defer func() {
		putPooledVM(nvm)
		putStringBuilder(buf)
		if r := recover(); r != nil {
			switch r.(type) {
			case stopError, envFatalError, *PanicError:
				future.err <- r.(error)
			default:
				future.err <- panicErrorWithContext(r, fnName, fnPkg)
			}
		}
	}()

	if err := nvm.runFunc(fn, nvm.vars); err != nil {
		switch err.(type) {
		case *PanicError, stopError, envFatalError:
			future.err <- err
		default:
			future.err <- stopError{err}
		}
		return
	}
	future.result <- buf.String()
}

// runNativeMacroParallel calls a native function wrapper and sends result to future.
func runNativeMacroParallel(nativeFn *NativeFunction, args []reflect.Value, future *renderFuture) {
	// Capture function info for error reporting
	fnName := nativeFn.name
	fnPkg := nativeFn.pkg

	defer func() {
		if r := recover(); r != nil {
			switch r.(type) {
			case *PanicError, envFatalError:
				future.err <- r.(error)
			default:
				future.err <- panicErrorWithContext(r, fnName, fnPkg)
			}
		}
	}()

	results := nativeFn.value.Call(args)
	if len(results) > 0 && results[0].Kind() == reflect.String {
		future.result <- results[0].String()
	} else {
		future.result <- ""
	}
}

func (vm *VM) runFunc(fn *Function, vars []reflect.Value) error {
	vm.fn = fn
	vm.vars = vars
	ctx := vm.nativeEnv.ctx
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	for {
		err := vm.runRecoverable()
		if err == nil {
			break
		}
		p, ok := err.(*PanicError)
		if !ok {
			return err
		}
		p.next = vm.panic
		vm.panic = p
		if len(vm.calls) == 0 {
			break
		}
		vm.calls = append(vm.calls, callFrame{cl: callable{fn: vm.fn, vars: vm.vars}, renderer: vm.renderer, fp: vm.fp, pc: vm.pc, status: panicked})
		vm.fn = nil
	}
	if currentContext := vm.nativeEnv.ctx; currentContext != nil {
		if err := currentContext.Err(); err != nil {
			return err
		}
	}
	if vm.panic != nil {
		return vm.panic
	}
	return nil
}

func (vm *VM) runRecoverable() (err error) {
	panicking := true
	defer func() {
		if panicking {
			msg := recover()
			err = vm.convertPanic(msg)
		}
	}()
	if vm.fn != nil || vm.nextCall() {
		vm.run()
	}
	panicking = false
	return nil
}

// executeMacro sets up and executes a macro function.
// The format parameter specifies the expected output format (or ReturnString for string return).
func (vm *VM) executeMacro(fn *Function, format int8) {
	call := callFrame{cl: callable{fn: vm.fn, vars: vm.vars}, renderer: vm.renderer, fp: vm.fp, pc: vm.pc + 1, ok: vm.ok, rangesLen: len(vm.ranges), rangesBase: vm.rangesBase}
	off := vm.fn.Body[vm.pc]
	vm.fp[0] += Addr(off.Op)
	if vm.fp[0]+Addr(fn.NumReg[0]) > vm.st[0] {
		vm.moreIntStack()
	}
	vm.fp[1] += Addr(off.A)
	if vm.fp[1]+Addr(fn.NumReg[1]) > vm.st[1] {
		vm.moreFloatStack()
	}
	vm.fp[2] += Addr(off.B)
	if vm.fp[2]+Addr(fn.NumReg[2]) > vm.st[2] {
		vm.moreStringStack()
	}
	vm.fp[3] += Addr(off.C)
	if vm.fp[3]+Addr(fn.NumReg[3]) > vm.st[3] {
		vm.moreGeneralStack()
	}
	if format == ReturnString {
		nr := newRenderer(&strings.Builder{})
		if call.renderer != nil {
			nr.collectSrc = call.renderer.collectSrc // collect the value's source too
		}
		vm.renderer = nr
	} else if ast.Format(format) != fn.Format {
		if fn.Format == ast.FormatMarkdown && ast.Format(format) == ast.FormatHTML {
			if vm.env.conv == nil {
				panic(vm.newPanic(errors.New("no Markdown converter available")))
			}
			vm.renderer = newRenderer(&bytes.Buffer{})
		} else {
			nr := newRenderer(vm.renderer.out)
			nr.retainTextFragments = vm.renderer.retainTextFragments
			vm.renderer = nr
		}
	}
	vm.fn = fn
	vm.vars = vm.env.globals
	vm.calls = append(vm.calls, call)
	vm.rangesBase = len(vm.ranges)
	vm.pc = 0
}

func (vm *VM) run() (Addr, bool) {

	var hasDefaultCase bool
	var startNativeGoroutine bool

	var op Operation
	var a, b, c int8

	done := vm.doneChan

mainLoop:
	for {

		if done != nil {
			select {
			case <-done:
				return vm.stop()
			default:
			}
		}

		in := vm.fn.Body[vm.pc]

		vm.pc++
		op, a, b, c = in.Op, in.A, in.B, in.C

		// If an instruction needs to change the program counter,
		// it must be changed, if possible, at the end of the instruction execution.

		switch op {

		// Add
		case OpAdd, -OpAdd:
			switch a := reflect.Kind(a); a {
			case reflect.Float64:
				vm.setFloat(c, vm.floatk(b, op < 0)+vm.float(c))
			case reflect.Float32:
				vm.setFloat(c, float64(float32(vm.floatk(b, op < 0)+vm.float(c))))
			default:
				v := vm.intk(b, op < 0) + vm.int(c)
				switch a {
				case reflect.Int8:
					v = int64(int8(v))
				case reflect.Int16:
					v = int64(int16(v))
				case reflect.Int32:
					v = int64(int32(v))
				case reflect.Uint8:
					v = int64(uint8(v))
				case reflect.Uint16:
					v = int64(uint16(v))
				case reflect.Uint32:
					v = int64(uint32(v))
				case reflect.Uint64:
					v = int64(uint64(v))
				}
				vm.setInt(c, v)
			}
		case OpAddInt, -OpAddInt:
			vm.setInt(c, vm.int(a)+vm.intk(b, op < 0))
		case OpAddFloat64, -OpAddFloat64:
			vm.setFloat(c, vm.float(a)+vm.floatk(b, op < 0))

		// Addr
		case OpAddr:
			v := vm.general(a)
			switch v.Kind() {
			case reflect.Slice, reflect.Array:
				i := int(vm.int(b))
				vm.setGeneral(c, v.Index(i).Addr())
			case reflect.Struct, reflect.Pointer:
				vm.setGeneral(c, vm.fieldByIndex(v, uint8(b)).Addr())
			}

		// And
		case OpAnd, -OpAnd:
			vm.setInt(c, vm.int(a)&vm.intk(b, op < 0))

		// AndNot
		case OpAndNot, -OpAndNot:
			vm.setInt(c, vm.int(a)&^vm.intk(b, op < 0))

		// Append
		case OpAppend:
			slice := vm.general(c)
			length := int(b - a)
			if length > 0 && length <= slice.Cap()-slice.Len() {
				vm.observeMutation(MutationAppend, slice)
			}
			vm.setGeneral(c, vm.appendSlice(a, length, slice))

		// AppendSlice
		case OpAppendSlice:
			slice := vm.general(c)
			source := vm.general(a)
			if source.Len() > 0 && source.Len() <= slice.Cap()-slice.Len() {
				vm.observeMutation(MutationAppend, slice)
			}
			vm.setGeneral(c, reflect.AppendSlice(slice, source))

		// Assert
		case OpAssert:
			v := vm.general(a)
			t := vm.fn.Types[uint8(b)]
			var ok bool
			if v.IsValid() {
				if w, isScriggoType := t.(ScriggoType); isScriggoType {
					v, ok = w.Unwrap(v)
				} else {
					if t.Kind() == reflect.Interface {
						ok = v.Type().Implements(t)
					} else {
						ok = v.Type() == t
					}
				}
			}
			vm.ok = ok
			if !ok {
				in := vm.fn.Body[vm.pc]
				if in.Op == OpPanic {
					var concrete reflect.Type
					var method string
					if v.IsValid() {
						concrete = v.Type()
						if t.Kind() == reflect.Interface {
							method = missingMethod(concrete, t)
						}
					}
					panic(errTypeAssertion(vm.fn.Types[uint8(in.C)], concrete, t, method, v))
				}
			}
			if c != 0 {
				switch t.Kind() {
				case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
					var n int64
					if ok {
						n = v.Int()
					}
					vm.setInt(c, n)
				case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
					var n int64
					if ok {
						n = int64(v.Uint())
					}
					vm.setInt(c, n)
				case reflect.Float32, reflect.Float64:
					var f float64
					if ok {
						f = v.Float()
					}
					vm.setFloat(c, f)
				case reflect.String:
					var s string
					if ok {
						s = v.String()
					}
					vm.setString(c, s)
				default:
					if w, ok := t.(ScriggoType); ok {
						t = w.GoType()
					}
					rv := reflect.New(t).Elem()
					if ok {
						rv.Set(v)
					}
					vm.setFromReflectValue(c, rv)
				}
			}
			if ok {
				vm.pc++
			}

		// Break
		case OpBreak:
			targetAddr := Addr(decodeUint24(a, b, c))
			// Check if we're breaking from an inline range loop
			// Find the range that this break targets (by matching range address)
			// Only search ranges belonging to current function (from rangesBase onward)
			for i := len(vm.ranges) - 1; i >= vm.rangesBase; i-- {
				if vm.ranges[i].rangeAddr == targetAddr {
					// Found the target range - pop all ranges up to and including it
					// Jump to endAddr which points to the Goto after OpRange (exits the loop)
					// Set vm.ok = true so that for-else's else block is NOT executed
					endAddr := vm.ranges[i].endAddr
					vm.ranges = vm.ranges[:i]
					vm.ok = true
					vm.pc = endAddr
					continue mainLoop
				}
			}
			// Not breaking from an inline range, return as before
			return targetAddr, true

		// Call
		case OpCallFunc:
			call := callFrame{cl: callable{fn: vm.fn, vars: vm.vars}, renderer: vm.renderer, fp: vm.fp, pc: vm.pc + 1, ok: vm.ok, rangesLen: len(vm.ranges), rangesBase: vm.rangesBase}
			fn := vm.fn.Functions[uint8(a)]
			// Profiling: record function call entry.
			if vm.profiler != nil {
				vm.profiler.enterCall(fn.Name, fn.Pkg, fn.File, vm.currentLine(), ProfileCallKindScriggoFunc, "")
			}
			off := vm.fn.Body[vm.pc]
			vm.fp[0] += Addr(off.Op)
			if vm.fp[0]+Addr(fn.NumReg[0]) > vm.st[0] {
				vm.moreIntStack()
			}
			vm.fp[1] += Addr(off.A)
			if vm.fp[1]+Addr(fn.NumReg[1]) > vm.st[1] {
				vm.moreFloatStack()
			}
			vm.fp[2] += Addr(off.B)
			if vm.fp[2]+Addr(fn.NumReg[2]) > vm.st[2] {
				vm.moreStringStack()
			}
			vm.fp[3] += Addr(off.C)
			if vm.fp[3]+Addr(fn.NumReg[3]) > vm.st[3] {
				vm.moreGeneralStack()
			}
			vm.fn = fn
			vm.vars = vm.env.globals
			vm.calls = append(vm.calls, call)
			vm.rangesBase = len(vm.ranges)
			vm.pc = 0
		case OpCallIndirect:
			f := vm.general(a).Interface().(*callable)
			if f.fn == nil {
				off := vm.fn.Body[vm.pc]
				vm.callNative(f.Native(), c, StackShift{int8(off.Op), off.A, off.B, off.C}, startNativeGoroutine)
				done = vm.doneChan
				startNativeGoroutine = false
				vm.pc++
			} else {
				call := callFrame{cl: callable{fn: vm.fn, vars: vm.vars}, renderer: vm.renderer, fp: vm.fp, pc: vm.pc + 1, ok: vm.ok, rangesLen: len(vm.ranges), rangesBase: vm.rangesBase}
				fn := f.fn
				// Profiling: record indirect Scriggo function call entry.
				if vm.profiler != nil {
					kind := ProfileCallKindScriggoFunc
					if fn.Macro {
						kind = ProfileCallKindMacro
					}
					vm.profiler.enterCall(fn.Name, fn.Pkg, fn.File, vm.currentLine(), kind, fn.File)
				}
				off := vm.fn.Body[vm.pc]
				vm.fp[0] += Addr(off.Op)
				if vm.fp[0]+Addr(fn.NumReg[0]) > vm.st[0] {
					vm.moreIntStack()
				}
				vm.fp[1] += Addr(off.A)
				if vm.fp[1]+Addr(fn.NumReg[1]) > vm.st[1] {
					vm.moreFloatStack()
				}
				vm.fp[2] += Addr(off.B)
				if vm.fp[2]+Addr(fn.NumReg[2]) > vm.st[2] {
					vm.moreStringStack()
				}
				vm.fp[3] += Addr(off.C)
				if vm.fp[3]+Addr(fn.NumReg[3]) > vm.st[3] {
					vm.moreGeneralStack()
				}
				if fn.Macro {
					if b == ReturnString {
						nr := newRenderer(&strings.Builder{})
						if call.renderer != nil {
							nr.collectSrc = call.renderer.collectSrc // collect the value's source too
						}
						vm.renderer = nr
					} else if ast.Format(b) != fn.Format {
						if fn.Format == ast.FormatMarkdown && ast.Format(b) == ast.FormatHTML {
							if vm.env.conv == nil {
								panic(&fatalError{env: vm.env, msg: errors.New("no Markdown convert available")})
							}
							vm.renderer = newRenderer(&bytes.Buffer{})
						} else {
							nr := newRenderer(vm.renderer.out)
							nr.retainTextFragments = vm.renderer.retainTextFragments
							vm.renderer = nr
						}
					}
				}
				vm.fn = fn
				vm.vars = f.vars
				vm.calls = append(vm.calls, call)
				vm.rangesBase = len(vm.ranges)
				vm.pc = 0
			}
		case OpCallMacro:
			fn := vm.fn.Functions[uint8(a)]
			// Profiling: record macro call entry.
			if vm.profiler != nil {
				vm.profiler.enterCall(fn.Name, fn.Pkg, fn.File, vm.currentLine(), ProfileCallKindMacro, fn.File)
			}
			vm.executeMacro(fn, b)
		case OpCallNative:
			fn := vm.fn.NativeFunctions[uint8(a)]
			off := vm.fn.Body[vm.pc]
			vm.callNative(fn, c, StackShift{int8(off.Op), off.A, off.B, off.C}, startNativeGoroutine)
			done = vm.doneChan
			startNativeGoroutine = false
			vm.pc++
		case OpCallNativeValue:
			initReflectNativeFunction(&vm.nativeCall, vm.general(a))
			vm.nativeCall.frameCallKind = nativeFrameCallFunctionField
			off := vm.fn.Body[vm.pc]
			vm.callNative(&vm.nativeCall, c, StackShift{int8(off.Op), off.A, off.B, off.C}, false)
			done = vm.doneChan
			vm.pc++
		case OpCallNativeFrameMethod:
			receiver := vm.general(a)
			method := vm.stringk(b, true)
			off := vm.fn.Body[vm.pc]
			shift := StackShift{int8(off.Op), off.A, off.B, off.C}
			expectedType := vm.fn.InstructionInfo[vm.pc-1].FuncType
			if !vm.callNativeMethodFrame(receiver, method, expectedType, c, shift) {
				recordNativeFrameCall(nativeFrameCallInterfaceMethod, false)
				value := receiver.MethodByName(method)
				initReflectNativeFunction(&vm.nativeCall, value)
				vm.nativeCall.SetBoundMethodReceiver(receiver, method)
				vm.callNative(&vm.nativeCall, c, shift, false)
			}
			done = vm.doneChan
			vm.pc++

		// Cap
		case OpCap:
			vm.setInt(c, int64(vm.general(a).Cap()))

		// Case
		case OpCase, -OpCase:
			dir := reflect.SelectDir(a)
			i := len(vm.cases)
			if i == cap(vm.cases) {
				vm.cases = append(vm.cases, reflect.SelectCase{Dir: dir})
			} else {
				vm.cases = vm.cases[:i+1]
				vm.cases[i].Dir = dir
				if dir == reflect.SelectDefault {
					vm.cases[i].Chan = reflect.Value{}
				}
				if dir != reflect.SelectSend {
					vm.cases[i].Send = reflect.Value{}
				}
			}
			switch dir {
			case reflect.SelectSend:
				vm.cases[i].Chan = vm.general(c)
				t := vm.cases[i].Chan.Type().Elem()
				if !vm.cases[i].Send.IsValid() || t != vm.cases[i].Send.Type() {
					vm.cases[i].Send = reflect.New(t).Elem()
				}
				vm.getIntoReflectValue(b, vm.cases[i].Send, op < 0)
			case reflect.SelectRecv:
				vm.cases[i].Chan = vm.general(b)
			}
			vm.pc++

		// Close
		case OpClose:
			ch := vm.general(a)
			vm.rejectChannelOperation(MutationCloseChannel, ch)
			ch.Close()

		// Clear
		case OpClear:
			target := vm.general(a)
			vm.observeMutation(MutationClear, target)
			target.Clear()

		// Complex
		case OpComplex64:
			var re, im float32
			if a > 0 {
				re = float32(vm.float(a))
			}
			if b > 0 {
				im = float32(vm.float(b))
			}
			vm.setGeneral(c, reflect.ValueOf(complex(re, im)))
		case OpComplex128:
			var re, im float64
			if a > 0 {
				re = vm.float(a)
			}
			if b > 0 {
				im = vm.float(b)
			}
			vm.setGeneral(c, reflect.ValueOf(complex(re, im)))

		// Continue
		case OpContinue:
			targetAddr := Addr(decodeUint24(a, b, c))
			// Check if we're in an inline range loop targeting this address
			// Only check ranges belonging to current function
			if len(vm.ranges) > vm.rangesBase {
				rs := &vm.ranges[len(vm.ranges)-1]
				if rs.rangeAddr == targetAddr {
					// Advance to next iteration
					if vm.advanceRangeInline(rs) {
						// More elements - continue the loop
						vm.pc = rs.bodyAddr
						continue mainLoop
					}
					// No more elements - exit the loop
					vm.ranges = vm.ranges[:len(vm.ranges)-1]
					vm.ok = true
					vm.pc = rs.endAddr
					continue mainLoop
				}
			}
			// Not in an inline range, return as before
			return targetAddr, false

		// Convert
		case OpConvert:
			t := vm.fn.Types[uint8(b)]
			switch t.Kind() {
			case reflect.String:
				vm.setString(c, vm.general(a).Convert(t).String())
			default:
				vm.setGeneral(c, vm.general(a).Convert(t))
			}
		case OpConvertInt:
			t := vm.fn.Types[uint8(b)]
			v := vm.int(a)
			switch t.Kind() {
			case reflect.Int:
				vm.setInt(c, int64(int(v)))
			case reflect.Int8:
				vm.setInt(c, int64(int8(v)))
			case reflect.Int16:
				vm.setInt(c, int64(int16(v)))
			case reflect.Int32:
				vm.setInt(c, int64(int32(v)))
			case reflect.Int64:
				vm.setInt(c, v)
			case reflect.Uint, reflect.Uintptr:
				vm.setInt(c, int64(uint(v)))
			case reflect.Uint8:
				vm.setInt(c, int64(uint8(v)))
			case reflect.Uint16:
				vm.setInt(c, int64(uint16(v)))
			case reflect.Uint32:
				vm.setInt(c, int64(uint32(v)))
			case reflect.Uint64:
				vm.setInt(c, v)
			case reflect.Float32:
				vm.setFloat(c, float64(float32(v)))
			case reflect.Float64:
				vm.setFloat(c, float64(v))
			case reflect.String:
				s := string(unicode.ReplacementChar)
				if 0 <= v && v <= unicode.MaxRune {
					s = string(rune(v))
				}
				vm.setString(c, s)
			}
		case OpConvertUint:
			t := vm.fn.Types[uint8(b)]
			v := uint64(vm.int(a))
			switch t.Kind() {
			case reflect.Int:
				vm.setInt(c, int64(int(v)))
			case reflect.Int8:
				vm.setInt(c, int64(int8(v)))
			case reflect.Int16:
				vm.setInt(c, int64(int16(v)))
			case reflect.Int32:
				vm.setInt(c, int64(int32(v)))
			case reflect.Int64:
				vm.setInt(c, int64(v))
			case reflect.Uint, reflect.Uintptr:
				vm.setInt(c, int64(uint(v)))
			case reflect.Uint8:
				vm.setInt(c, int64(uint8(v)))
			case reflect.Uint16:
				vm.setInt(c, int64(uint16(v)))
			case reflect.Uint32:
				vm.setInt(c, int64(uint32(v)))
			case reflect.Uint64:
				vm.setInt(c, int64(v))
			case reflect.Float32:
				vm.setFloat(c, float64(float32(v)))
			case reflect.Float64:
				vm.setFloat(c, float64(v))
			case reflect.String:
				s := string(unicode.ReplacementChar)
				if v <= unicode.MaxRune {
					s = string(rune(v))
				}
				vm.setString(c, s)
			}
		case OpConvertFloat:
			t := vm.fn.Types[uint8(b)]
			v := vm.float(a)
			switch t.Kind() {
			case reflect.Int:
				vm.setInt(c, int64(int(v)))
			case reflect.Int8:
				vm.setInt(c, int64(int8(v)))
			case reflect.Int16:
				vm.setInt(c, int64(int16(v)))
			case reflect.Int32:
				vm.setInt(c, int64(int32(v)))
			case reflect.Int64:
				vm.setInt(c, int64(v))
			case reflect.Uint, reflect.Uintptr:
				vm.setInt(c, int64(uint(v)))
			case reflect.Uint8:
				vm.setInt(c, int64(uint8(v)))
			case reflect.Uint16:
				vm.setInt(c, int64(uint16(v)))
			case reflect.Uint32:
				vm.setInt(c, int64(uint32(v)))
			case reflect.Uint64:
				vm.setInt(c, int64(uint64(v)))
			case reflect.Float32:
				vm.setFloat(c, float64(float32(v)))
			case reflect.Float64:
				vm.setFloat(c, v)
			}
		case OpConvertString:
			t := vm.fn.Types[uint8(b)]
			v := reflect.ValueOf(vm.string(a))
			if t.Kind() == reflect.Slice {
				vm.setGeneral(c, v.Convert(t))
			} else {
				if vm.env.conv != nil {
					var b strings.Builder
					_ = vm.env.conv([]byte(v.String()), &b)
					vm.setString(c, b.String())
				}
			}
		case OpConvertFromInterface:
			// Implicit conversion from interface{} to concrete type
			// Instruction format: A=source register, B=type table index, C=destination register
			t := vm.fn.Types[uint8(b)]
			v := vm.general(a)

			// Handle two nil-like cases that should produce zero values:
			// 1. Invalid reflect.Value (e.g., uninitialized variable)
			// 2. Nil interface value (interface{} containing nil)
			// Note: We check Kind()==Interface before calling IsNil() to avoid panics on non-nillable types.
			if !v.IsValid() || (v.Kind() == reflect.Interface && v.IsNil()) {
				// Set zero value for target type
				// Optimize by only allocating reflect.Zero when needed (default case)
				switch t.Kind() {
				case reflect.String:
					vm.setString(c, "")
				case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
					reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
					vm.setInt(c, 0)
				case reflect.Float32, reflect.Float64:
					vm.setFloat(c, 0)
				case reflect.Bool:
					vm.setInt(c, 0)
				default:
					vm.setGeneral(c, reflect.Zero(t))
				}
				break
			}

			// Get the actual value to convert
			// Unwrap interface if needed
			var elem reflect.Value
			var sourceType reflect.Type
			if v.Kind() == reflect.Interface {
				// Value is wrapped in interface, unwrap it
				// Note: v.IsNil() was already checked above, so v.Elem() is safe here
				elem = v.Elem()
				sourceType = v.Type()
			} else {
				// Value is already concrete (compiler optimized away the interface wrapping)
				elem = v
				sourceType = v.Type()
			}

			// Special case: converting any value to string via fmt.Sprint
			// This enables template string concatenation like: "value: " + data["num"]
			// where data["num"] is interface{} containing a non-string type (int, float64, bool, etc.)
			// Note: We must use fmt.Sprint for all non-string types because Go's reflect.Convert
			// treats int→string as a rune conversion (42 becomes '*'), not decimal string conversion.
			if t.Kind() == reflect.String && elem.Kind() != reflect.String {
				str := fmt.Sprint(elem.Interface())
				vm.setString(c, str)
				break
			}

			// Check convertibility
			if !elem.CanConvert(t) {
				panic(errInterfaceConversion(sourceType, elem.Type(), t, elem))
			}

			// Perform conversion
			converted := elem.Convert(t)

			// Store result in appropriate register
			switch t.Kind() {
			case reflect.String:
				vm.setString(c, converted.String())
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				vm.setInt(c, converted.Int())
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
				vm.setInt(c, int64(converted.Uint()))
			case reflect.Float32, reflect.Float64:
				vm.setFloat(c, converted.Float())
			case reflect.Bool:
				if converted.Bool() {
					vm.setInt(c, 1)
				} else {
					vm.setInt(c, 0)
				}
			default:
				vm.setGeneral(c, converted)
			}

		// Concat
		case OpConcat:
			vm.setString(c, vm.string(a)+vm.string(b))

		// Copy
		case OpCopy:
			dst := vm.general(c)
			vm.observeMutation(MutationCopy, dst)
			n := reflect.Copy(dst, vm.general(a))
			if b != 0 {
				vm.setInt(b, int64(n))
			}

		// Defer
		case OpDefer:
			cl := vm.general(a).Interface().(*callable)
			off := vm.fn.Body[vm.pc]
			arg := vm.fn.Body[vm.pc+1]
			fp := [4]Addr{
				vm.fp[0] + Addr(off.Op),
				vm.fp[1] + Addr(off.A),
				vm.fp[2] + Addr(off.B),
				vm.fp[3] + Addr(off.C),
			}
			vm.swapStack(&vm.fp, &fp, StackShift{int8(arg.Op), arg.A, arg.B, arg.C})
			vm.calls = append(vm.calls, callFrame{cl: *cl, renderer: vm.renderer, fp: fp, pc: 0, status: deferred, numVariadic: c, ok: vm.ok, rangesLen: len(vm.ranges), rangesBase: vm.rangesBase})
			vm.pc += 2

		// Delete
		case OpDelete:
			m := vm.general(a)
			vm.observeMutation(MutationDelete, m)
			k := reflect.New(m.Type().Key()).Elem()
			vm.getIntoReflectValue(b, k, false)
			m.SetMapIndex(k, reflect.Value{})

		// Div
		case OpDiv, -OpDiv:
			switch a := reflect.Kind(a); a {
			case reflect.Float32:
				vm.setFloat(c, float64(float32(vm.float(c))/float32(vm.floatk(b, op < 0))))
			case reflect.Float64:
				vm.setFloat(c, vm.float(c)/vm.floatk(b, op < 0))
			default:
				var bv = vm.intk(b, op < 0)
				var cv = vm.int(c)
				var v int64
				switch a {
				case reflect.Int8:
					v = int64(int8(cv) / int8(bv))
				case reflect.Int16:
					v = int64(int16(cv) / int16(bv))
				case reflect.Int32:
					v = int64(int32(cv) / int32(bv))
				case reflect.Int64:
					v = cv / bv
				case reflect.Uint8:
					v = int64(uint8(cv) / uint8(bv))
				case reflect.Uint16:
					v = int64(uint16(cv) / uint16(bv))
				case reflect.Uint32:
					v = int64(uint32(cv) / uint32(bv))
				case reflect.Uint64:
					v = int64(uint64(cv) / uint64(bv))
				}
				vm.setInt(c, v)
			}
		case OpDivInt, -OpDivInt:
			vm.setInt(c, vm.int(a)/vm.intk(b, op < 0))
		case OpDivFloat64, -OpDivFloat64:
			vm.setFloat(c, vm.float(a)/vm.floatk(b, op < 0))

		// Field with fast path for single-level access (Phase 3 optimization)
		case OpField:
			v := vm.general(a)
			fieldIndexes := vm.fn.FieldIndexes[uint8(b)]
			// Fast path: single-level field access (most common case)
			if len(fieldIndexes) == 1 {
				v = derefPointer(v)
				vm.setFromReflectValue(c, v.Field(fieldIndexes[0]))
			} else {
				// Multi-level field access (embedded structs)
				vm.setFromReflectValue(c, vm.fieldByIndex(v, uint8(b)))
			}
		case OpFieldNativeFunc:
			v := vm.general(a)
			fieldIndexes := vm.fn.FieldIndexes[uint8(b)]
			if len(fieldIndexes) == 1 {
				v = derefPointer(v).Field(fieldIndexes[0])
			} else {
				v = vm.fieldByIndex(v, uint8(b))
			}
			vm.setGeneral(c, v)

		// GetVar
		case OpGetVar:
			index := decodeInt16(a, b)
			v := vm.vars[index]
			if global, ok := vm.internalFunctionGlobal(index); ok &&
				!vm.env.internalFunctionEscaped[global].Load() {
				if function := vm.env.internalFunctionValues[global].Load(); function != nil {
					vm.setGeneral(c, reflect.ValueOf(function))
					break
				}
			}
			if global, ok := vm.vectorGlobal(index); ok {
				var bound bool
				var err error
				v, bound, err = vm.env.vector.valueForGlobal(global)
				if err != nil {
					panic(stopError{err})
				}
				if !bound {
					v = vm.vars[index]
				}
			}
			if v.Kind() == reflect.Array || v.Kind() == reflect.Struct {
				v = snapshotValue(v)
			}
			vm.setFromReflectValue(c, v)

		// GetVarAddr
		case OpGetVarAddr:
			index := decodeInt16(a, b)
			if global, ok := vm.vectorGlobal(index); ok && vm.env.vector.hasGlobal(global) {
				panic(stopError{fmt.Errorf("%w: a vector binding address cannot escape", ErrVectorInvalidConfiguration)})
			}
			vm.markBatchGlobalsEscaped()
			if global, ok := vm.internalFunctionGlobal(index); ok {
				vm.env.internalFunctionEscaped[global].Store(true)
				if function := vm.env.internalFunctionValues[global].Swap(nil); function != nil {
					vm.vars[index].Set(function.Value(vm.env, vm.nativeEnv.ctx))
				}
			}
			ptr := vm.vars[index].Addr()
			vm.setFromReflectValue(c, ptr)

		// Go
		case OpGo:
			if (vm.env.observeMutation != nil || vm.env.observeMutationContext != nil) &&
				vm.detachedScriggoCall() {
				panic(stopError{ErrMutationObserverDetachedGo})
			}
			wasNative := vm.startGoroutine()
			if wasNative {
				startNativeGoroutine = true
			}

		// GoRender - spawn a parallel render operation
		case OpGoRender:
			vm.setSrcCtx(false) // attribute the parallel block to this go-render site
			fn := vm.fn.Functions[uint8(a)]
			off := vm.fn.Body[vm.pc]
			vm.pc++

			future := getRenderFuture()
			eg, err := vm.renderer.addFutureSlot(future)
			if err != nil {
				putRenderFuture(future)
				panic(vm.newPanic(err))
			}

			stackShift := StackShift{int8(off.Op), off.A, off.B, off.C}
			args := vm.captureArgs(stackShift)
			childContext := vm.env.deriveChildContext(vm.nativeEnv.ctx)

			vm.renderer.spawnParallelRender(eg, func(fn *Function, args *parallelRenderArgs, future *renderFuture, parentEnv *env, childContext context.Context) func() {
				return func() {
					runScriggoMacroParallel(fn, parentEnv.globals, args, future, parentEnv, childContext)
				}
			}(fn, args, future, vm.env, childContext))

		// GoRenderIndirect - spawn a parallel render operation for an indirect function call
		case OpGoRenderIndirect:
			vm.setSrcCtx(false) // attribute the parallel block to this go-render site
			f := vm.general(a).Interface().(*callable)
			var nativeFn *NativeFunction
			var childEnv *nativeEnv
			if f.fn == nil {
				nativeFn = f.Native()
				childEnv = vm.newChildNativeEnv()
				vm.beforeNativeCall(childEnv)
			}
			off := vm.fn.Body[vm.pc]
			vm.pc++

			future := getRenderFuture()
			eg, err := vm.renderer.addFutureSlot(future)
			if err != nil {
				putRenderFuture(future)
				panic(vm.newPanic(err))
			}

			stackShift := StackShift{int8(off.Op), off.A, off.B, off.C}
			args := vm.captureArgs(stackShift)

			if f.fn == nil {
				// Native function path: callable was converted to a native wrapper via callable.Value()
				reflectArgs := args.toReflectValues(nativeFn.value.Type())
				putParallelRenderArgs(args)
				for i := range reflectArgs {
					if nativeFn.value.Type().In(i) == envType {
						reflectArgs[i] = reflect.ValueOf(childEnv)
					}
				}
				if receiver := nativeFn.receiver(reflectArgs); receiver.IsValid() {
					vm.observeNativeCall(childEnv, receiver, nativeFn.name, false)
				}

				vm.renderer.spawnParallelRender(eg, func(nativeFn *NativeFunction, args []reflect.Value, future *renderFuture) func() {
					return func() {
						runNativeMacroParallel(nativeFn, args, future)
					}
				}(nativeFn, reflectArgs, future))
			} else {
				// Scriggo function path: callable has a direct function reference
				fn := f.fn
				fnVars := f.vars
				childContext := vm.env.deriveChildContext(vm.nativeEnv.ctx)

				vm.renderer.spawnParallelRender(eg, func(fn *Function, fnVars []reflect.Value, args *parallelRenderArgs, future *renderFuture, parentEnv *env, childContext context.Context) func() {
					return func() {
						runScriggoMacroParallel(fn, fnVars, args, future, parentEnv, childContext)
					}
				}(fn, fnVars, args, future, vm.env, childContext))
			}

		// GoRenderWait - wait for all pending parallel renders and flush output
		case OpGoRenderWait:
			if err := vm.renderer.flushWithFutures(); err != nil {
				switch err.(type) {
				case stopError, envFatalError:
					panic(err)
				}
				panic(vm.newPanic(err))
			}

		// Goto
		case OpGoto:
			vm.pc = Addr(decodeUint24(a, b, c))

		// If
		case OpIf, -OpIf:
			var cond bool
			switch Condition(b) {
			case ConditionOK, ConditionNotOK:
				cond = vm.ok
			case ConditionInterfaceNil, ConditionInterfaceNotNil:
				// An invalid reflect.Value represents a nil interface value.
				cond = !vm.general(a).IsValid()
			case ConditionNil, ConditionNotNil:
				cond = vm.general(a).IsNil()
			case ConditionEqual, ConditionNotEqual:
				x := vm.general(a)
				y := vm.generalk(c, op < 0)
				// Use reflect.Value.Equal() to avoid interface boxing allocation
				cond = x.Equal(y)
			case ConditionInterfaceEqual, ConditionInterfaceNotEqual:
				x := vm.general(a)
				y := vm.generalk(c, op < 0)
				cond = vm.equals(x, y)
			case ConditionContainsElement, ConditionNotContainsElement:
				x := vm.general(a)
				y := vm.generalk(c, op < 0)
				n := x.Len()
				k := x.Type().Elem().Kind()
				if k == reflect.Interface {
					for i := range n {
						if vm.equals(x.Index(i).Elem(), y) {
							cond = true
							break
						}
					}
				} else {
					// Use reflect.Value.Equal() to avoid interface boxing allocation in loop
					for i := range n {
						if x.Index(i).Equal(y) {
							cond = true
							break
						}
					}
				}
			case ConditionContainsKey, ConditionNotContainsKey:
				x := vm.general(a)
				y := vm.generalk(c, op < 0)
				cond = x.MapIndex(y).IsValid()
			case ConditionContainsNil, ConditionNotContainsNil:
				x := vm.general(a)
				if x.Kind() == reflect.Map {
					zero := reflect.Zero(x.Type().Key())
					cond = x.MapIndex(zero).IsValid()
				} else {
					n := x.Len()
					for i := range n {
						if x.Index(i).IsZero() {
							cond = true
							break
						}
					}
				}
			}
			switch Condition(b) {
			case ConditionNotOK, ConditionInterfaceNotNil, ConditionNotNil,
				ConditionNotEqual, ConditionInterfaceNotEqual, ConditionNotContainsSubstring,
				ConditionNotContainsRune, ConditionNotContainsKey, ConditionNotContainsElement,
				ConditionNotContainsNil:
				cond = !cond
			}
			if cond {
				vm.pc++
			}
		case OpIfInt, -OpIfInt:
			var cond bool
			switch Condition(b) {
			case ConditionZero:
				cond = vm.int(a) == 0
			case ConditionNotZero:
				cond = vm.int(a) != 0
			case ConditionLessU, ConditionLessEqualU, ConditionGreaterU, ConditionGreaterEqualU:
				v1 := uint64(vm.int(a))
				v2 := uint64(vm.intk(c, op < 0))
				switch Condition(b) {
				case ConditionLessU:
					cond = v1 < v2
				case ConditionLessEqualU:
					cond = v1 <= v2
				case ConditionGreaterU:
					cond = v1 > v2
				case ConditionGreaterEqualU:
					cond = v1 >= v2
				}
			case ConditionContainsElement, ConditionNotContainsElement:
				x := vm.general(a)
				if s, ok := x.Interface().([]int); ok {
					y := int(vm.intk(c, op < 0))
					if slices.Contains(s, y) {
						cond = true
					}
				} else {
					n := x.Len()
					k := x.Type().Elem().Kind()
					switch {
					case k == reflect.Bool:
						y := vm.boolk(c, op < 0)
						for i := range n {
							if cond = x.Index(i).Bool() == y; cond {
								break
							}
						}
					case reflect.Int <= k && k <= reflect.Int64:
						y := vm.intk(c, op < 0)
						for i := range n {
							if cond = x.Index(i).Int() == y; cond {
								break
							}
						}
					default:
						y := uint64(vm.intk(c, op < 0))
						for i := range n {
							if cond = x.Index(i).Uint() == y; cond {
								break
							}
						}
					}
				}
				if Condition(b) == ConditionNotContainsElement {
					cond = !cond
				}
			case ConditionContainsKey, ConditionNotContainsKey:
				x := vm.general(a)
				if x.Len() > 0 {
					v := reflect.New(x.Type().Key()).Elem()
					_ = vm.getIntoReflectValue(c, v, op < 0)
					cond = x.MapIndex(v).IsValid()
				}
				if Condition(b) == ConditionNotContainsKey {
					cond = !cond
				}
			case ConditionContainsRune, ConditionNotContainsRune:
				x := vm.string(a)
				if x != "" {
					y := vm.intk(c, op < 0)
					cond = strings.ContainsRune(x, rune(y))
				}
				if Condition(b) == ConditionNotContainsRune {
					cond = !cond
				}
			default:
				v1 := vm.int(a)
				v2 := vm.intk(c, op < 0)
				switch Condition(b) {
				case ConditionEqual:
					cond = v1 == v2
				case ConditionNotEqual:
					cond = v1 != v2
				case ConditionLess:
					cond = v1 < v2
				case ConditionLessEqual:
					cond = v1 <= v2
				case ConditionGreater:
					cond = v1 > v2
				case ConditionGreaterEqual:
					cond = v1 >= v2
				}
			}
			if cond {
				vm.pc++
			}
		case OpIfFloat, -OpIfFloat:
			var cond bool
			switch bb := Condition(b); bb {
			case ConditionContainsElement, ConditionNotContainsElement:
				x := vm.general(a)
				y := vm.floatk(c, op < 0)
				if s, ok := x.Interface().([]float64); ok {
					if slices.Contains(s, y) {
						cond = true
					}
				} else {
					n := x.Len()
					for i := range n {
						if x.Index(i).Float() == y {
							cond = true
							break
						}
					}
				}
				if bb == ConditionNotContainsElement {
					cond = !cond
				}
			case ConditionContainsKey, ConditionNotContainsKey:
				x := vm.general(a)
				if x.Len() > 0 {
					v := reflect.New(x.Type().Key()).Elem()
					_ = vm.getIntoReflectValue(c, v, op < 0)
					cond = x.MapIndex(v).IsValid()
				}
				if bb == ConditionNotContainsKey {
					cond = !cond
				}
			default:
				x := vm.float(a)
				y := vm.floatk(c, op < 0)
				switch bb {
				case ConditionEqual:
					cond = x == y
				case ConditionNotEqual:
					cond = x != y
				case ConditionLess:
					cond = x < y
				case ConditionLessEqual:
					cond = x <= y
				case ConditionGreater:
					cond = x > y
				case ConditionGreaterEqual:
					cond = x >= y
				}
			}
			if cond {
				vm.pc++
			}
		case OpIfString, -OpIfString:
			var cond bool
			bb := Condition(b)
			if bb <= ConditionGreaterEqual {
				v1 := vm.string(a)
				v2 := vm.stringk(c, op < 0)
				switch bb {
				case ConditionEqual:
					cond = v1 == v2
				case ConditionNotEqual:
					cond = v1 != v2
				case ConditionLess:
					cond = v1 < v2
				case ConditionLessEqual:
					cond = v1 <= v2
				case ConditionGreater:
					cond = v1 > v2
				case ConditionGreaterEqual:
					cond = v1 >= v2
				}
			} else if bb <= ConditionLenGreaterEqual {
				v1 := vm.string(a)
				v2 := int(vm.intk(c, op < 0))
				switch bb {
				case ConditionLenEqual:
					cond = len(v1) == v2
				case ConditionLenNotEqual:
					cond = len(v1) != v2
				case ConditionLenLess:
					cond = len(v1) < v2
				case ConditionLenLessEqual:
					cond = len(v1) <= v2
				case ConditionLenGreater:
					cond = len(v1) > v2
				case ConditionLenGreaterEqual:
					cond = len(v1) >= v2
				}
			} else if bb == ConditionContainsSubstring || bb == ConditionNotContainsSubstring {
				x := vm.string(a)
				y := vm.stringk(c, op < 0)
				cond = strings.Contains(x, y)
				if bb == ConditionNotContainsSubstring {
					cond = !cond
				}
			} else if bb == ConditionContainsElement || bb == ConditionNotContainsElement {
				x := vm.general(a)
				n := x.Len()
				if n > 0 {
					y := vm.stringk(c, op < 0)
					for i := range n {
						if x.Index(i).String() == y {
							cond = true
							break
						}
					}
				}
				if bb == ConditionNotContainsElement {
					cond = !cond
				}
			} else {
				// ConditionContainsKey, ConditionNotContainsKey
				x := vm.general(a)
				if x.Len() > 0 {
					v := reflect.New(x.Type().Key()).Elem()
					_ = vm.getIntoReflectValue(c, v, op < 0)
					cond = x.MapIndex(v).IsValid()
				}
				if bb == ConditionNotContainsKey {
					cond = !cond
				}
			}
			if cond {
				vm.pc++
			}

		// Index
		case OpIndex, -OpIndex:
			// TODO: OpIndex and -OpIndex currently return the reference to the
			// element returned by the indexing operation, so the implementation
			// is the same as OpIndexRef and -OpIndexRef. This is going to
			// change in a future commit.
			v := vm.general(a)
			i := int(vm.intk(b, op < 0))
			vm.setFromReflectValue(c, v.Index(i))
		case OpIndexString, -OpIndexString:
			vm.setInt(c, int64(vm.string(a)[int(vm.intk(b, op < 0))]))
		case OpIndexRef, -OpIndexRef:
			v := vm.general(a)
			i := int(vm.intk(b, op < 0))
			vm.setFromReflectValue(c, v.Index(i))

		// Len
		case OpLen:
			var length int
			if registerType(a) == stringRegister {
				length = len(vm.string(b))
			} else {
				length = vm.general(b).Len()
			}
			vm.setInt(c, int64(length))

		// Load
		case OpLoad:
			t, i := decodeValueIndex(a, b)
			switch t {
			case intRegister:
				vm.setInt(c, vm.fn.Values.Int[i])
			case floatRegister:
				vm.setFloat(c, vm.fn.Values.Float[i])
			case stringRegister:
				vm.setString(c, vm.fn.Values.String[i])
			case generalRegister:
				vm.setGeneral(c, vm.fn.Values.General[i])
			}

		// LoadFunc
		case OpLoadFunc:
			if a == 1 {
				fn := callable{}
				fn.native = vm.fn.NativeFunctions[uint8(b)]
				vm.setGeneral(c, reflect.ValueOf(&fn))
			} else {
				vm.markBatchGlobalsEscaped()
				fn := vm.fn.Functions[uint8(b)]
				var vars []reflect.Value
				if fn.VarRefs != nil {
					vars = make([]reflect.Value, len(fn.VarRefs))
					for i, ref := range fn.VarRefs {
						if ref < 0 {
							// Calling Elem() is necessary because the general
							// register contains an indirect value, that is
							// stored as a pointer to a value.
							vars[i] = vm.general(int8(-ref)).Elem()
						} else {
							vars[i] = vm.vars[ref]
						}
					}
				} else {
					vars = vm.env.globals
				}
				vm.setGeneral(c, reflect.ValueOf(&callable{fn: fn, vars: vars}))
			}

		// MakeArray
		case OpMakeArray:
			t := vm.fn.Types[uint8(b)]
			vm.setGeneral(c, reflect.New(t).Elem())

		// MakeChan
		case OpMakeChan, -OpMakeChan:
			typ := vm.fn.Types[uint8(a)]
			buffer := int(vm.intk(b, op < 0))
			var ch reflect.Value
			if typ.ChanDir() == reflect.BothDir {
				ch = reflect.MakeChan(typ, buffer)
			} else {
				t := reflect.ChanOf(reflect.BothDir, typ.Elem())
				ch = reflect.MakeChan(t, buffer).Convert(typ)
			}
			vm.setGeneral(c, ch)

		// MakeMap
		case OpMakeMap, -OpMakeMap:
			typ := vm.fn.Types[uint8(a)]
			n := int(vm.intk(b, op < 0))
			if n > 0 {
				vm.setGeneral(c, reflect.MakeMapWithSize(typ, n))
			} else {
				vm.setGeneral(c, reflect.MakeMap(typ))
			}

		// MakeSlice
		case OpMakeSlice:
			typ := vm.fn.Types[uint8(a)]
			var len, cap int
			if b > 0 {
				next := vm.fn.Body[vm.pc]
				lenIsConst := (b & (1 << 1)) != 0
				len = int(vm.intk(next.A, lenIsConst))
				capIsConst := (b & (1 << 2)) != 0
				cap = int(vm.intk(next.B, capIsConst))
			}
			vm.setGeneral(c, reflect.MakeSlice(typ, len, cap))
			if b > 0 {
				vm.pc++
			}

		// MakeStruct
		case OpMakeStruct:
			t := vm.fn.Types[uint8(b)]
			vm.setGeneral(c, reflect.New(t).Elem())

		// MapIndex with inline fast paths for common types (Phase 3 optimization)
		case OpMapIndex, -OpMapIndex:
			m := vm.general(a)
			t := m.Type()

			// Fast path: try common map types to avoid reflection overhead
			// Only for non-constant keys (op > 0) with string key type
			handled := false
			if t.Key().Kind() == reflect.String && op > 0 {
				key := vm.string(b)
				switch t.Elem().Kind() {
				case reflect.String:
					if mapVal, ok := m.Interface().(map[string]string); ok {
						val, exists := mapVal[key]
						vm.ok = exists
						vm.setString(c, val)
						handled = true
					}
				case reflect.Int:
					if mapVal, ok := m.Interface().(map[string]int); ok {
						val, exists := mapVal[key]
						vm.ok = exists
						vm.setInt(c, int64(val))
						handled = true
					}
				case reflect.Int64:
					if mapVal, ok := m.Interface().(map[string]int64); ok {
						val, exists := mapVal[key]
						vm.ok = exists
						vm.setInt(c, val)
						handled = true
					}
				case reflect.Bool:
					if mapVal, ok := m.Interface().(map[string]bool); ok {
						val, exists := mapVal[key]
						vm.ok = exists
						vm.setBool(c, val)
						handled = true
					}
				}
			}
			// Generic path with reflection (for non-string keys, constant keys, or named types)
			if !handled {
				k := reflect.New(t.Key()).Elem()
				vm.getIntoReflectValue(b, k, op < 0)
				elem := reflect.New(t.Elem()).Elem()
				index := m.MapIndex(k)
				vm.ok = index.IsValid()
				if vm.ok {
					elem.Set(index)
				}
				vm.setFromReflectValue(c, elem)
			}
		case OpMapIndexAny, -OpMapIndexAny:
			m := vm.general(a)
			t := m.Type()
			// Type must be 'map[K]E' where K is a string type or the 'interface{}' type and E is any type.
			if k := t.Key(); t.Name() != "" || k.Kind() != reflect.String && k != emptyInterfaceType {
				panic(runtimeError("invalid operation: type " + t.String() + " does not support key indexing"))
			}
			k := reflect.ValueOf(vm.stringk(b, op < 0))
			index := m.MapIndex(k)
			elem := reflect.New(emptyInterfaceType).Elem()
			vm.ok = index.IsValid()
			if vm.ok {
				elem.Set(index)
			}
			vm.setFromReflectValue(c, elem)

		// MethodValue
		case OpMethodValue, OpMethodValueFrame:
			receiver := vm.general(a)
			if !receiver.IsValid() {
				panic(errNilPointer)
			}
			method := vm.stringk(b, true)
			var nativeEnv *nativeEnv
			if vm.env.observeNativeCall != nil {
				nativeEnv = vm.nativeEnvForExposure()
			}
			vm.observeNativeCallWithContext(vm.nativeEnv.ctx, nativeEnv, receiver, method, true)
			if op == OpMethodValueFrame {
				vm.setGeneral(c, receiver)
				break
			}
			value := receiver.MethodByName(method)
			vm.setGeneral(c, reflect.ValueOf(newNativeMethodCallable(value, receiver, method)))

		// Move
		case OpMove, -OpMove:
			switch registerType(a) {
			case intRegister:
				vm.setInt(c, vm.intk(b, op < 0))
			case floatRegister:
				vm.setFloat(c, vm.floatk(b, op < 0))
			case stringRegister:
				vm.setString(c, vm.stringk(b, op < 0))
			case generalRegister:
				vm.setGeneral(c, snapshotValue(vm.generalk(b, op < 0)))
			}

		// Mul
		case OpMul, -OpMul:
			switch a := reflect.Kind(a); a {
			case reflect.Float32:
				vm.setFloat(c, float64(float32(vm.float(c)*vm.floatk(b, op < 0))))
			case reflect.Float64:
				vm.setFloat(c, vm.float(c)*vm.floatk(b, op < 0))
			default:
				v := vm.int(c) * vm.intk(b, op < 0)
				switch a {
				case reflect.Int8:
					v = int64(int8(v))
				case reflect.Int16:
					v = int64(int16(v))
				case reflect.Int32:
					v = int64(int32(v))
				case reflect.Uint8:
					v = int64(uint8(v))
				case reflect.Uint16:
					v = int64(uint16(v))
				case reflect.Uint32:
					v = int64(uint32(v))
				case reflect.Uint64:
					v = int64(uint64(v))
				}
				vm.setInt(c, v)
			}
		case OpMulInt, -OpMulInt:
			vm.setInt(c, vm.int(a)*vm.intk(b, op < 0))
		case OpMulFloat64, -OpMulFloat64:
			vm.setFloat(c, vm.float(a)*vm.floatk(b, op < 0))

		// Neg
		case OpNeg:
			switch a := reflect.Kind(a); a {
			case reflect.Float32:
				vm.setFloat(c, float64(-float32(vm.float(b))))
			case reflect.Float64:
				vm.setFloat(c, -vm.float(b))
			default:
				v := vm.int(b)
				switch a {
				case reflect.Int8:
					v = int64(-int8(v))
				case reflect.Int16:
					v = int64(-int16(v))
				case reflect.Int32:
					v = int64(-int32(v))
				case reflect.Int64:
					v = -v
				case reflect.Uint8:
					v = int64(-uint8(v))
				case reflect.Uint16:
					v = int64(-uint16(v))
				case reflect.Uint32:
					v = int64(-uint32(v))
				case reflect.Uint64:
					v = int64(-uint64(v))
				}
				vm.setInt(c, v)
			}

		// New
		case OpNew:
			t := vm.fn.Types[uint8(b)]
			vm.setGeneral(c, reflect.New(t))

		// Or
		case OpOr, -OpOr:
			vm.setInt(c, vm.int(a)|vm.intk(b, op < 0))

		// Panic
		case OpPanic:
			rv := vm.general(a)
			if rv.IsValid() {
				panic(rv.Interface())
			} else {
				panic(new(goruntime.PanicNilError))
			}

		// Print
		case OpPrint:
			rv := vm.general(a)
			if rv.IsValid() {
				vm.env.doPrint(rv.Interface())
			} else {
				vm.env.doPrint(nil)
			}

		// Range - inline execution version
		// Instead of recursive vm.run() calls, we push state onto vm.ranges
		// and execute the loop body inline within the same run() call.
		case OpRange:
			endAddress := vm.pc
			rangeAddress := endAddress - 1
			bodyAddress := endAddress + 1
			v := vm.general(a)
			if vm.env.deterministic && v.Kind() == reflect.Map {
				entries, err := deterministicMapEntries(v)
				if err != nil {
					panic(stopError{err})
				}
				for _, entry := range entries {
					if b != 0 {
						vm.setFromReflectValue(b, entry.key)
					}
					if c != 0 {
						vm.setFromReflectValue(c, entry.value)
					}
					vm.pc = bodyAddress
					addr, breakOut := vm.run()
					if addr != rangeAddress {
						return addr, breakOut
					}
					if breakOut {
						break
					}
				}
				vm.ok = vm.pc != endAddress
				vm.pc = endAddress
				continue mainLoop
			}
			switch s := v.Interface().(type) {
			case []int:
				if len(s) == 0 {
					vm.ok = false
					vm.pc = endAddress
					continue mainLoop
				}
				// Set up first iteration
				if b != 0 {
					vm.setInt(b, 0)
				}
				if c != 0 {
					vm.setInt(c, int64(s[0]))
				}
				// Push range state for subsequent iterations
				vm.ranges = append(vm.ranges, rangeState{
					sliceInt:  s,
					index:     0,
					rangeAddr: rangeAddress,
					bodyAddr:  bodyAddress,
					endAddr:   endAddress,
					indexReg:  b,
					elemReg:   c,
					rangeType: rangeSliceInt,
				})
				vm.pc = bodyAddress
				continue mainLoop

			case []byte:
				if len(s) == 0 {
					vm.ok = false
					vm.pc = endAddress
					continue mainLoop
				}
				if b != 0 {
					vm.setInt(b, 0)
				}
				if c != 0 {
					vm.setInt(c, int64(s[0]))
				}
				vm.ranges = append(vm.ranges, rangeState{
					sliceByte: s,
					index:     0,
					rangeAddr: rangeAddress,
					bodyAddr:  bodyAddress,
					endAddr:   endAddress,
					indexReg:  b,
					elemReg:   c,
					rangeType: rangeSliceByte,
				})
				vm.pc = bodyAddress
				continue mainLoop

			case []rune:
				if len(s) == 0 {
					vm.ok = false
					vm.pc = endAddress
					continue mainLoop
				}
				if b != 0 {
					vm.setInt(b, 0)
				}
				if c != 0 {
					vm.setInt(c, int64(s[0]))
				}
				vm.ranges = append(vm.ranges, rangeState{
					sliceRune: s,
					index:     0,
					rangeAddr: rangeAddress,
					bodyAddr:  bodyAddress,
					endAddr:   endAddress,
					indexReg:  b,
					elemReg:   c,
					rangeType: rangeSliceRune,
				})
				vm.pc = bodyAddress
				continue mainLoop

			case []float64:
				if len(s) == 0 {
					vm.ok = false
					vm.pc = endAddress
					continue mainLoop
				}
				if b != 0 {
					vm.setInt(b, 0)
				}
				if c != 0 {
					vm.setFloat(c, s[0])
				}
				vm.ranges = append(vm.ranges, rangeState{
					sliceFloat: s,
					index:      0,
					rangeAddr:  rangeAddress,
					bodyAddr:   bodyAddress,
					endAddr:    endAddress,
					indexReg:   b,
					elemReg:    c,
					rangeType:  rangeSliceFloat64,
				})
				vm.pc = bodyAddress
				continue mainLoop

			case []string:
				if len(s) == 0 {
					vm.ok = false
					vm.pc = endAddress
					continue mainLoop
				}
				if b != 0 {
					vm.setInt(b, 0)
				}
				if c != 0 {
					vm.setString(c, s[0])
				}
				vm.ranges = append(vm.ranges, rangeState{
					sliceString: s,
					index:       0,
					rangeAddr:   rangeAddress,
					bodyAddr:    bodyAddress,
					endAddr:     endAddress,
					indexReg:    b,
					elemReg:     c,
					rangeType:   rangeSliceString,
				})
				vm.pc = bodyAddress
				continue mainLoop

			case []any:
				if len(s) == 0 {
					vm.ok = false
					vm.pc = endAddress
					continue mainLoop
				}
				if b != 0 {
					vm.setInt(b, 0)
				}
				if c != 0 {
					vm.setGeneral(c, reflect.ValueOf(s[0]))
				}
				vm.ranges = append(vm.ranges, rangeState{
					sliceIface: s,
					index:      0,
					rangeAddr:  rangeAddress,
					bodyAddr:   bodyAddress,
					endAddr:    endAddress,
					indexReg:   b,
					elemReg:    c,
					rangeType:  rangeSliceInterface,
				})
				vm.pc = bodyAddress
				continue mainLoop

			case map[string]int:
				// Use native Go map iteration - faster than reflection
				for i, v := range s {
					if b != 0 {
						vm.setString(b, i)
					}
					if c != 0 {
						vm.setInt(c, int64(v))
					}
					vm.pc = bodyAddress
					addr, breakOut := vm.run()
					if addr != rangeAddress {
						return addr, breakOut
					}
					if breakOut {
						break
					}
				}
				vm.ok = vm.pc != endAddress
				vm.pc = endAddress

			case map[string]bool:
				for i, v := range s {
					if b != 0 {
						vm.setString(b, i)
					}
					if c != 0 {
						vm.setBool(c, v)
					}
					vm.pc = bodyAddress
					addr, breakOut := vm.run()
					if addr != rangeAddress {
						return addr, breakOut
					}
					if breakOut {
						break
					}
				}
				vm.ok = vm.pc != endAddress
				vm.pc = endAddress

			case map[string]string:
				for i, v := range s {
					if b != 0 {
						vm.setString(b, i)
					}
					if c != 0 {
						vm.setString(c, v)
					}
					vm.pc = bodyAddress
					addr, breakOut := vm.run()
					if addr != rangeAddress {
						return addr, breakOut
					}
					if breakOut {
						break
					}
				}
				vm.ok = vm.pc != endAddress
				vm.pc = endAddress

			case map[string]any:
				for i, v := range s {
					if b != 0 {
						vm.setString(b, i)
					}
					if c != 0 {
						vm.setGeneral(c, reflect.ValueOf(v))
					}
					vm.pc = bodyAddress
					addr, breakOut := vm.run()
					if addr != rangeAddress {
						return addr, breakOut
					}
					if breakOut {
						break
					}
				}
				vm.ok = vm.pc != endAddress
				vm.pc = endAddress

			case map[string]float64:
				for i, v := range s {
					if b != 0 {
						vm.setString(b, i)
					}
					if c != 0 {
						vm.setFloat(c, v)
					}
					vm.pc = bodyAddress
					addr, breakOut := vm.run()
					if addr != rangeAddress {
						return addr, breakOut
					}
					if breakOut {
						break
					}
				}
				vm.ok = vm.pc != endAddress
				vm.pc = endAddress

			case map[int]string:
				for i, v := range s {
					if b != 0 {
						vm.setInt(b, int64(i))
					}
					if c != 0 {
						vm.setString(c, v)
					}
					vm.pc = bodyAddress
					addr, breakOut := vm.run()
					if addr != rangeAddress {
						return addr, breakOut
					}
					if breakOut {
						break
					}
				}
				vm.ok = vm.pc != endAddress
				vm.pc = endAddress

			case map[int]interface{}:
				for i, v := range s {
					if b != 0 {
						vm.setInt(b, int64(i))
					}
					if c != 0 {
						vm.setGeneral(c, reflect.ValueOf(v))
					}
					vm.pc = bodyAddress
					addr, breakOut := vm.run()
					if addr != rangeAddress {
						return addr, breakOut
					}
					if breakOut {
						break
					}
				}
				vm.ok = vm.pc != endAddress
				vm.pc = endAddress

			default:
				switch kind := v.Kind(); kind {
				case reflect.Map:
					// Use recursive approach for generic maps
					iter := v.MapRange()
					for iter.Next() {
						if b != 0 {
							vm.setFromReflectValue(b, iter.Key())
						}
						if c != 0 {
							vm.setFromReflectValue(c, iter.Value())
						}
						vm.pc = bodyAddress
						addr, breakOut := vm.run()
						if addr != rangeAddress {
							return addr, breakOut
						}
						if breakOut {
							break
						}
					}
					vm.ok = vm.pc != endAddress
					vm.pc = endAddress

				case reflect.Chan:
					// Channels still use recursive approach due to blocking semantics
					vm.rejectChannelOperation(MutationReceiveChannel, v)
					var u reflect.Value
					var ok bool
					for {
						if done == nil {
							u, ok = v.Recv()
						} else {
							var chosen int
							cas := reflect.SelectCase{Dir: reflect.SelectRecv, Chan: v}
							vm.cases = append(vm.cases, cas, vm.doneCase)
							chosen, u, ok = reflect.Select(vm.cases)
							if chosen == 1 {
								return vm.stop()
							}
							vm.cases = vm.cases[:0]
						}
						if !ok {
							break
						}
						if b != 0 {
							vm.setFromReflectValue(b, u)
						}
						vm.pc = bodyAddress
						addr, breakOut := vm.run()
						if addr != rangeAddress {
							return addr, breakOut
						}
						if breakOut {
							break
						}
					}
					vm.ok = vm.pc != endAddress
					vm.pc = endAddress

				case reflect.String:
					str := v.String()
					if len(str) == 0 {
						vm.ok = false
						vm.pc = endAddress
						continue mainLoop
					}
					// Decode first rune
					r, size := decodeRuneInString(str)
					if b != 0 {
						vm.setInt(b, 0)
					}
					if c != 0 {
						vm.setInt(c, int64(r))
					}
					vm.ranges = append(vm.ranges, rangeState{
						strVal:    str,
						bytePos:   size,
						rangeAddr: rangeAddress,
						bodyAddr:  bodyAddress,
						endAddr:   endAddress,
						indexReg:  b,
						elemReg:   c,
						rangeType: rangeString,
					})
					vm.pc = bodyAddress
					continue mainLoop

				default:
					if kind == reflect.Pointer {
						v = v.Elem()
					}
					length := v.Len()
					if length == 0 {
						vm.ok = false
						vm.pc = endAddress
						continue mainLoop
					}
					if b != 0 {
						vm.setInt(b, 0)
					}
					if c != 0 {
						vm.setFromReflectValue(c, v.Index(0))
					}
					vm.ranges = append(vm.ranges, rangeState{
						reflectVal: v,
						index:      0,
						length:     length,
						rangeAddr:  rangeAddress,
						bodyAddr:   bodyAddress,
						endAddr:    endAddress,
						indexReg:   b,
						elemReg:    c,
						rangeType:  rangeSliceReflect,
					})
					vm.pc = bodyAddress
					continue mainLoop
				}
			}

		// RangeInterface - range over interface{} typed expressions
		// Uses reflection since concrete type is unknown at compile time.
		// Loop variable registers are always general registers (interface{} type),
		// so we use setGeneral directly instead of setFromReflectValue.
		case OpRangeInterface:
			endAddress := vm.pc
			rangeAddress := endAddress - 1
			bodyAddress := endAddress + 1
			v := vm.general(a)
			// Handle nil/invalid interface - iterate zero times
			if !v.IsValid() {
				vm.ok = false
				vm.pc = endAddress
				continue
			}
			switch kind := v.Kind(); kind {
			case reflect.Map:
				if vm.env.deterministic {
					entries, err := deterministicMapEntries(v)
					if err != nil {
						panic(stopError{err})
					}
					for _, entry := range entries {
						if b != 0 {
							vm.setGeneral(b, entry.key)
						}
						if c != 0 {
							vm.setGeneral(c, entry.value)
						}
						vm.pc = bodyAddress
						addr, breakOut := vm.run()
						if addr != rangeAddress {
							return addr, breakOut
						}
						if breakOut {
							break
						}
					}
					break
				}
				iter := v.MapRange()
				for iter.Next() {
					if b != 0 {
						vm.setGeneral(b, iter.Key())
					}
					if c != 0 {
						vm.setGeneral(c, iter.Value())
					}
					vm.pc = bodyAddress
					addr, breakOut := vm.run()
					if addr != rangeAddress {
						return addr, breakOut
					}
					if breakOut {
						break
					}
				}
			case reflect.Chan:
				vm.rejectChannelOperation(MutationReceiveChannel, v)
				var u reflect.Value
				var ok bool
				for {
					if done == nil {
						u, ok = v.Recv()
					} else {
						var chosen int
						cas := reflect.SelectCase{Dir: reflect.SelectRecv, Chan: v}
						vm.cases = append(vm.cases, cas, vm.doneCase)
						chosen, u, ok = reflect.Select(vm.cases)
						if chosen == 1 {
							return vm.stop()
						}
						vm.cases = vm.cases[:0]
					}
					if !ok {
						break
					}
					if b != 0 {
						vm.setGeneral(b, u)
					}
					vm.pc = bodyAddress
					addr, breakOut := vm.run()
					if addr != rangeAddress {
						return addr, breakOut
					}
					if breakOut {
						break
					}
				}
			case reflect.String:
				for i, r := range v.String() {
					if b != 0 {
						vm.setGeneral(b, reflect.ValueOf(i))
					}
					if c != 0 {
						vm.setGeneral(c, reflect.ValueOf(r))
					}
					vm.pc = bodyAddress
					addr, breakOut := vm.run()
					if addr != rangeAddress {
						return addr, breakOut
					}
					if breakOut {
						break
					}
				}
			case reflect.Slice, reflect.Array:
				length := v.Len()
				for i := 0; i < length; i++ {
					if b != 0 {
						vm.setGeneral(b, reflect.ValueOf(i))
					}
					if c != 0 {
						vm.setGeneral(c, v.Index(i))
					}
					vm.pc = bodyAddress
					addr, breakOut := vm.run()
					if addr != rangeAddress {
						return addr, breakOut
					}
					if breakOut {
						break
					}
				}
			case reflect.Ptr:
				elem := v.Elem()
				if elem.Kind() != reflect.Array {
					panic(runtimeError("cannot range over " + v.Type().String()))
				}
				length := elem.Len()
				for i := 0; i < length; i++ {
					if b != 0 {
						vm.setGeneral(b, reflect.ValueOf(i))
					}
					if c != 0 {
						vm.setGeneral(c, elem.Index(i))
					}
					vm.pc = bodyAddress
					addr, breakOut := vm.run()
					if addr != rangeAddress {
						return addr, breakOut
					}
					if breakOut {
						break
					}
				}
			default:
				panic(runtimeError("cannot range over " + v.Type().String()))
			}
			vm.ok = vm.pc != endAddress
			vm.pc = endAddress

		// RangeString
		case OpRangeString, -OpRangeString:
			endAddress := vm.pc
			rangeAddress := endAddress - 1
			bodyAddress := endAddress + 1
			s := vm.stringk(a, op < 0)
			for i, e := range s {
				if b != 0 {
					vm.setInt(b, int64(i))
				}
				if c != 0 {
					vm.setInt(c, int64(e))
				}
				vm.pc = bodyAddress
				addr, breakOut := vm.run()
				if addr != rangeAddress {
					return addr, breakOut
				}
				if breakOut {
					break
				}
			}
			vm.ok = vm.pc != endAddress
			vm.pc = endAddress

		// RealImag
		case OpRealImag, -OpRealImag:
			v := vm.generalk(a, op < 0)
			cmpx := v.Complex()
			re, im := real(cmpx), imag(cmpx)
			if b > 0 {
				vm.setFloat(b, re)
			}
			if c > 0 {
				vm.setFloat(c, im)
			}

		// Receive
		case OpReceive:
			ch := vm.general(a)
			vm.rejectChannelOperation(MutationReceiveChannel, ch)
			var v reflect.Value
			if done == nil {
				v, vm.ok = ch.Recv()
			} else {
				var chosen int
				cas := reflect.SelectCase{Dir: reflect.SelectRecv, Chan: ch}
				vm.cases = append(vm.cases, cas, vm.doneCase)
				chosen, v, vm.ok = reflect.Select(vm.cases)
				if chosen == 1 {
					return vm.stop()
				}
				vm.cases = vm.cases[:0]
			}
			if c != 0 {
				vm.setFromReflectValue(c, v)
			}
			if b != 0 {
				vm.setBool(b, vm.ok)
			}

		// Recover
		case OpRecover:
			var msg reflect.Value
			last := len(vm.calls) - 1
			if a > 0 {
				// Recover down the stack.
				if vm.calls[last].status == panicked {
					last = -1
				} else {
					last--
				}
			}
			for i := last; i >= 0; i-- {
				switch vm.calls[i].status {
				case deferred:
					continue
				case panicked:
					vm.calls[i].status = recovered
					vm.panic.recovered = true
					msg = reflect.ValueOf(vm.panic.message)
				}
				break
			}
			if c != 0 {
				vm.setGeneral(c, msg)
			}

		// Rem
		case OpRem, -OpRem:
			bv := vm.intk(b, op < 0)
			cv := vm.int(c)
			var v int64
			switch reflect.Kind(a) {
			case reflect.Int8:
				v = int64(int8(cv) % int8(bv))
			case reflect.Int16:
				v = int64(int16(cv) % int16(bv))
			case reflect.Int32:
				v = int64(int32(cv) % int32(bv))
			case reflect.Int64:
				v = cv % bv
			case reflect.Uint8:
				v = int64(uint8(cv) % uint8(bv))
			case reflect.Uint16:
				v = int64(uint16(cv) % uint16(bv))
			case reflect.Uint32:
				v = int64(uint32(cv) % uint32(bv))
			case reflect.Uint64:
				v = int64(uint64(cv) % uint64(bv))
			}
			vm.setInt(c, v)
		case OpRemInt, -OpRemInt:
			vm.setInt(c, vm.int(a)%vm.intk(b, op < 0))

		// Return
		case OpReturn:
			i := len(vm.calls) - 1
			if i == -1 {
				return maxUint32, false
			}
			call := vm.calls[i]
			fn := call.cl.fn
			if call.status == started {
				// Profiling: record function/macro return.
				if vm.profiler != nil {
					vm.profiler.exitCall()
				}
				if vm.fn.Macro {
					if call.renderer != vm.renderer {
						b := fn.Body[call.pc-2].B
						if b == ReturnString {
							out := vm.renderer.Out().(*strings.Builder)
							s := out.String()
							vm.stashValueSource(call.renderer, vm.renderer, s)
							vm.setString(1, s)
						} else if vm.fn.Format == ast.FormatMarkdown && ast.Format(b) == ast.FormatHTML {
							out := vm.renderer.Out().(*bytes.Buffer)
							err := vm.env.conv(out.Bytes(), call.renderer.out)
							if err != nil {
								panic(&fatalError{env: vm.env, msg: err})
							}
						}
					}
					vm.renderer = call.renderer
				} else if regs := vm.fn.FinalRegs; regs != nil {
					vm.finalize(vm.fn.FinalRegs)
				}
				vm.calls = vm.calls[:i]
				vm.fp = call.fp
				vm.fn = fn
				vm.vars = call.cl.vars
				vm.pc = call.pc
				vm.ok = call.ok
				vm.ranges = vm.ranges[:call.rangesLen]
				vm.rangesBase = call.rangesBase
			} else if !vm.nextCall() {
				return maxUint32, false
			}

		// Select
		case OpSelect:
			numCase := len(vm.cases)
			if vm.env.observeMutation != nil || vm.env.observeMutationContext != nil {
				var nativeEnv *nativeEnv
				if vm.env.observeMutation != nil {
					nativeEnv = vm.nativeEnvForExposure()
				}
				ctx := vm.nativeEnv.ctx
				var observerErr error
				observedChannel := false
				for _, candidate := range vm.cases {
					var operation MutationOperation
					switch candidate.Dir {
					case reflect.SelectSend:
						operation = MutationSendChannel
					case reflect.SelectRecv:
						operation = MutationReceiveChannel
					default:
						continue
					}
					observedChannel = true
					if err := vm.notifyMutation(ctx, nativeEnv, operation, candidate.Chan); err != nil && observerErr == nil {
						observerErr = err
					}
				}
				if !observedChannel {
					observerErr = vm.notifyMutation(ctx, nativeEnv, MutationSelectChannel, reflect.Value{})
				}
				if observerErr != nil {
					panic(stopError{observerErr})
				}
				panic(stopError{ErrMutationObserverChannelOperation})
			}
			var chosen int
			var recv reflect.Value
			var recvOK bool
			if done == nil || hasDefaultCase {
				chosen, recv, recvOK = reflect.Select(vm.cases)
			} else {
				vm.cases = append(vm.cases, vm.doneCase)
				chosen, recv, recvOK = reflect.Select(vm.cases)
				if chosen == numCase {
					return vm.stop()
				}
			}
			step := numCase - chosen
			var pc Addr
			if step > 0 {
				pc = vm.pc - 2*Addr(step)
				if vm.cases[chosen].Dir == reflect.SelectRecv {
					r := vm.fn.Body[pc-1].C
					if r != 0 {
						vm.setFromReflectValue(r, recv)
					}
					vm.ok = recvOK
				}
			}
			for _, c := range vm.cases {
				if c.Dir != reflect.SelectDefault {
					if c.Dir == reflect.SelectSend {
						zero := reflect.Zero(c.Chan.Type().Elem())
						c.Send.Set(zero)
					}
					c.Chan = reflect.Value{}
				}
			}
			hasDefaultCase = false
			vm.cases = vm.cases[:0]
			vm.pc = pc

		// Send
		case OpSend, -OpSend:
			ch := vm.generalk(c, op < 0)
			vm.rejectChannelOperation(MutationSendChannel, ch)
			elemType := ch.Type().Elem()
			v := reflect.New(elemType).Elem()
			vm.getIntoReflectValue(a, v, op < 0)
			if done == nil {
				ch.Send(v)
			} else {
				cas := reflect.SelectCase{Dir: reflect.SelectSend, Chan: ch, Send: v}
				vm.cases = append(vm.cases, cas, vm.doneCase)
				chosen, _, _ := reflect.Select(vm.cases)
				if chosen == 1 {
					return vm.stop()
				}
				vm.cases = vm.cases[:0]
			}

		// SetField
		case OpSetField, -OpSetField:
			v := vm.general(b)
			vm.observeMutation(MutationSetField, vm.fieldMutationTarget(v, uint8(c)))
			vm.getIntoReflectValue(a, vm.fieldByIndex(v, uint8(c)), op < 0)

		// SetMap
		case OpSetMap, -OpSetMap:
			mv := vm.general(b)
			vm.observeMutation(MutationSetMapIndex, mv)
			switch m := mv.Interface().(type) {
			case map[string]string:
				k := vm.string(c)
				v := vm.stringk(a, op < 0)
				m[k] = v
			case map[string]int:
				k := vm.string(c)
				v := vm.intk(a, op < 0)
				m[k] = int(v)
			case map[string]bool:
				k := vm.string(c)
				v := vm.boolk(a, op < 0)
				m[k] = v
			case map[string]struct{}:
				m[vm.string(c)] = struct{}{}
			case map[string]any:
				k := vm.string(c)
				rv := vm.generalk(a, op < 0)
				if rv.IsValid() {
					m[k] = rv.Interface()
				} else {
					m[k] = nil
				}
			case map[int]int:
				k := vm.int(c)
				v := vm.intk(a, op < 0)
				m[int(k)] = int(v)
			case map[int]bool:
				k := vm.int(c)
				v := vm.boolk(a, op < 0)
				m[int(k)] = v
			case map[int]string:
				k := vm.int(c)
				v := vm.stringk(a, op < 0)
				m[int(k)] = v
			case map[int]struct{}:
				m[int(vm.int(c))] = struct{}{}
			default:
				t := mv.Type()
				k := reflect.New(t.Key()).Elem()
				vm.getIntoReflectValue(c, k, false)
				v := reflect.New(t.Elem()).Elem()
				vm.getIntoReflectValue(a, v, op < 0)
				mv.SetMapIndex(k, v)
			}

		// SetSlice
		case OpSetSlice, -OpSetSlice:
			i := vm.int(c)
			sv := vm.general(b)
			vm.observeMutation(MutationSetIndex, sv)
			switch s := sv.Interface().(type) {
			case []int:
				s[i] = int(vm.intk(a, op < 0))
			case []rune:
				s[i] = rune(vm.intk(a, op < 0))
			case []byte:
				s[i] = byte(vm.intk(a, op < 0))
			case []float64:
				s[i] = vm.floatk(a, op < 0)
			case []string:
				s[i] = vm.stringk(a, op < 0)
			case []any:
				rv := vm.generalk(a, op < 0)
				if rv.IsValid() {
					s[i] = rv.Interface()
				} else {
					s[i] = nil
				}
			default:
				v := sv.Index(int(i))
				vm.getIntoReflectValue(a, v, op < 0)
			}

		// SetVar
		case OpSetVar, -OpSetVar:
			index := decodeInt16(b, c)
			if global, ok := vm.vectorGlobal(index); ok && vm.env.vector.hasGlobal(global) {
				panic(stopError{fmt.Errorf("%w: a vector binding cannot be mutated", ErrVectorInvalidConfiguration)})
			}
			v := vm.vars[index]
			vm.observeMutation(MutationSetVariable, v)
			if global, ok := vm.internalFunctionGlobal(index); ok &&
				!vm.env.internalFunctionEscaped[global].Load() {
				value := vm.generalk(a, op < 0)
				if value.IsValid() {
					if function, callableValue := value.Interface().(*callable); callableValue {
						v.SetZero()
						vm.env.internalFunctionValues[global].Store(function)
						break
					}
				}
			}
			vm.getIntoReflectValue(a, v, op < 0)

		// Shl
		case OpShl, -OpShl:
			v := vm.int(c) << uint(vm.intk(b, op < 0))
			switch reflect.Kind(a) {
			case reflect.Int8:
				v = int64(int8(v))
			case reflect.Int16:
				v = int64(int16(v))
			case reflect.Int32:
				v = int64(int32(v))
			case reflect.Uint8:
				v = int64(uint8(v))
			case reflect.Uint16:
				v = int64(uint16(v))
			case reflect.Uint32:
				v = int64(uint32(v))
			case reflect.Uint64:
				v = int64(uint64(v))
			}
			vm.setInt(c, v)
		case OpShlInt, -OpShlInt:
			vm.setInt(c, vm.int(a)<<uint(vm.intk(b, op < 0)))

		// Shr
		case OpShr, -OpShr:
			bv := uint(vm.intk(b, op < 0))
			cv := vm.int(c)
			var v int64
			if reflect.Kind(a) < reflect.Uint8 {
				v = cv >> bv
			} else {
				v = int64(uint64(cv) >> bv)
			}
			vm.setInt(c, v)
		case OpShrInt, -OpShrInt:
			vm.setInt(c, vm.int(a)>>uint(vm.intk(b, op < 0)))

		// Show
		case OpShow:
			vm.setSrcCtxShow()
			t := vm.fn.Types[uint8(a)]
			st, ok := t.(ScriggoType)
			if ok {
				t = st.GoType()
			}
			// Fast paths for basic types - avoid reflect.New allocation
			// Only use fast path for unnamed types (e.g., plain int, string)
			// Named types like native.HTML need to go through reflection to preserve type info
			if st == nil && t.Name() == t.Kind().String() {
				var err error
				ctx := Context(c)
				switch t.Kind() {
				case reflect.Int:
					err = vm.renderer.Show(vm.nativeEnvForExposure(), int(vm.intk(b, op < 0)), ctx)
					if err != nil {
						panic(outError{err})
					}
					continue mainLoop
				case reflect.Int64:
					err = vm.renderer.Show(vm.nativeEnvForExposure(), vm.intk(b, op < 0), ctx)
					if err != nil {
						panic(outError{err})
					}
					continue mainLoop
				case reflect.Int32:
					err = vm.renderer.Show(vm.nativeEnvForExposure(), int32(vm.intk(b, op < 0)), ctx)
					if err != nil {
						panic(outError{err})
					}
					continue mainLoop
				case reflect.String:
					err = vm.renderer.Show(vm.nativeEnvForExposure(), vm.stringk(b, op < 0), ctx)
					if err != nil {
						panic(outError{err})
					}
					continue mainLoop
				case reflect.Bool:
					err = vm.renderer.Show(vm.nativeEnvForExposure(), vm.boolk(b, op < 0), ctx)
					if err != nil {
						panic(outError{err})
					}
					continue mainLoop
				case reflect.Float64:
					err = vm.renderer.Show(vm.nativeEnvForExposure(), vm.floatk(b, op < 0), ctx)
					if err != nil {
						panic(outError{err})
					}
					continue mainLoop
				case reflect.Float32:
					err = vm.renderer.Show(vm.nativeEnvForExposure(), float32(vm.floatk(b, op < 0)), ctx)
					if err != nil {
						panic(outError{err})
					}
					continue mainLoop
				}
			}
			// Fallback: use reflection for complex types
			rv := reflect.New(t).Elem()
			vm.getIntoReflectValue(b, rv, op < 0)
			if st != nil {
				rv = st.Wrap(rv)
			}
			var v any
			if rv.IsValid() {
				v = rv.Interface()
			}
			err := vm.renderer.Show(vm.nativeEnvForExposure(), v, Context(c))
			if err != nil {
				panic(outError{err})
			}

		// Super-instruction: OpFieldShow (fused OpField + OpShow)
		// Encoding: Op=OpFieldShow, A=struct_reg, B=field_index, C=context
		// Next instruction word: Op=type_index (other fields unused)
		case OpFieldShow:
			vm.setSrcCtxShow()
			v := vm.general(a)
			// Fast path for single-level field access (same optimization as OpField)
			fieldIndexes := vm.fn.FieldIndexes[uint8(b)]
			var field reflect.Value
			if len(fieldIndexes) == 1 {
				v = derefPointer(v)
				field = v.Field(fieldIndexes[0])
			} else {
				field = vm.fieldByIndex(v, uint8(b))
			}
			// Read type index from next instruction
			typeIdx := uint8(vm.fn.Body[vm.pc].Op)
			vm.pc++
			t := vm.fn.Types[typeIdx]
			st, ok := t.(ScriggoType)
			if ok {
				t = st.GoType()
			}
			// Get field value and show it
			var val interface{}
			if field.IsValid() {
				if st != nil {
					field = st.Wrap(field)
				}
				val = field.Interface()
			}
			if err := vm.renderer.Show(vm.nativeEnvForExposure(), val, Context(c)); err != nil {
				panic(outError{err})
			}

		// Super-instruction: OpMapIndexShow (fused OpMapIndex + OpShow)
		// Encoding: Op=OpMapIndexShow, A=map_reg, B=key_reg, C=context
		// Next instruction word: Op=type_index (other fields unused)
		case OpMapIndexShow, -OpMapIndexShow:
			vm.setSrcCtxShow()
			m := vm.general(a)
			t := m.Type()
			k := reflect.New(t.Key()).Elem()
			vm.getIntoReflectValue(b, k, op < 0)
			index := m.MapIndex(k)
			// Read type index from next instruction
			typeIdx := uint8(vm.fn.Body[vm.pc].Op)
			vm.pc++
			showType := vm.fn.Types[typeIdx]
			st, ok := showType.(ScriggoType)
			if ok {
				showType = st.GoType()
			}
			// Get element value and show it
			var val interface{}
			if index.IsValid() {
				elem := reflect.New(t.Elem()).Elem()
				elem.Set(index)
				if st != nil {
					elem = st.Wrap(elem)
				}
				val = elem.Interface()
			}
			if err := vm.renderer.Show(vm.nativeEnvForExposure(), val, Context(c)); err != nil {
				panic(outError{err})
			}

		// Specialized map index handlers (Phase 3 - Bytecode Specialization)
		// These are direct-dispatch handlers that skip type checking for known types.
		case OpMapIndexStringString:
			// Specialized: map[string]string - no reflection for key/value
			m := vm.general(a)
			key := vm.string(b)
			if mapVal, ok := m.Interface().(map[string]string); ok {
				val, exists := mapVal[key]
				vm.ok = exists
				vm.setString(c, val)
			} else {
				vm.mapIndexStringKeyFallback(m, key, c)
			}

		case OpMapIndexStringInt:
			// Specialized: map[string]int - no reflection for key/value
			m := vm.general(a)
			key := vm.string(b)
			if mapVal, ok := m.Interface().(map[string]int); ok {
				val, exists := mapVal[key]
				vm.ok = exists
				vm.setInt(c, int64(val))
			} else if mapVal, ok := m.Interface().(map[string]int64); ok {
				val, exists := mapVal[key]
				vm.ok = exists
				vm.setInt(c, val)
			} else {
				vm.mapIndexStringKeyFallback(m, key, c)
			}

		case OpMapIndexStringBool:
			// Specialized: map[string]bool - no reflection for key/value
			m := vm.general(a)
			key := vm.string(b)
			if mapVal, ok := m.Interface().(map[string]bool); ok {
				val, exists := mapVal[key]
				vm.ok = exists
				vm.setBool(c, val)
			} else {
				vm.mapIndexStringKeyFallback(m, key, c)
			}

		case OpFieldDirect:
			// Specialized: direct single-field access for single-level FieldIndexes entries.
			// Uses same encoding as OpField: b is index into FieldIndexes.
			// Only used when FieldIndexes[b] has exactly one element.
			// NOTE: This opcode is currently not emitted - it's infrastructure for future
			// compile-time specialization when the compiler can detect single-level access.
			v := derefPointer(vm.general(a))
			fieldIdx := vm.fn.FieldIndexes[uint8(b)][0]
			vm.setFromReflectValue(c, v.Field(fieldIdx))

		// Slice
		case OpSlice:
			var i1, i2, i3 int
			s := vm.general(a)
			next := vm.fn.Body[vm.pc]
			i1 = int(vm.intk(next.A, b&1 != 0))
			if k := b&2 != 0; k && next.B == -1 {
				i2 = s.Len()
			} else {
				i2 = int(vm.intk(next.B, k))
			}
			if k := b&4 != 0; k && next.C == -1 {
				i3 = s.Cap()
			} else {
				i3 = int(vm.intk(next.C, k))
			}
			s = s.Slice3(i1, i2, i3)
			vm.setGeneral(c, s)
			vm.pc++
		case OpStringSlice:
			var i1, i2 int
			s := vm.string(a)
			next := vm.fn.Body[vm.pc]
			i1 = int(vm.intk(next.A, b&1 != 0))
			if k := b&2 != 0; k && next.B == -1 {
				i2 = len(s)
			} else {
				i2 = int(vm.intk(next.B, k))
			}
			vm.setString(c, s[i1:i2])
			vm.pc++

		// Sub
		case OpSub, -OpSub:
			switch a := reflect.Kind(a); a {
			case reflect.Float32:
				vm.setFloat(c, float64(float32(vm.float(c)-vm.floatk(b, op < 0))))
			case reflect.Float64:
				vm.setFloat(c, vm.float(c)-vm.floatk(b, op < 0))
			default:
				v := vm.int(c) - vm.intk(b, op < 0)
				switch a {
				case reflect.Int8:
					v = int64(int8(v))
				case reflect.Int16:
					v = int64(int16(v))
				case reflect.Int32:
					v = int64(int32(v))
				case reflect.Uint8:
					v = int64(uint8(v))
				case reflect.Uint16:
					v = int64(uint16(v))
				case reflect.Uint32:
					v = int64(uint32(v))
				case reflect.Uint64:
					v = int64(uint64(v))
				}
				vm.setInt(c, v)
			}
		case OpSubInt, -OpSubInt:
			vm.setInt(c, vm.int(a)-vm.intk(b, op < 0))
		case OpSubFloat64, -OpSubFloat64:
			vm.setFloat(c, vm.float(a)-vm.floatk(b, op < 0))

		// SubInv
		case OpSubInv, -OpSubInv:
			switch a := reflect.Kind(a); a {
			case reflect.Float32:
				vm.setFloat(c, float64(float32(vm.floatk(b, op < 0)-vm.float(c))))
			case reflect.Float64:
				vm.setFloat(c, vm.floatk(b, op < 0)-vm.float(c))
			default:
				v := vm.intk(b, op < 0) - vm.int(c)
				switch a {
				case reflect.Int8:
					v = int64(int8(v))
				case reflect.Int16:
					v = int64(int16(v))
				case reflect.Int32:
					v = int64(int32(v))
				case reflect.Uint8:
					v = int64(uint8(v))
				case reflect.Uint16:
					v = int64(uint16(v))
				case reflect.Uint32:
					v = int64(uint32(v))
				case reflect.Uint64:
					v = int64(uint64(v))
				}
				vm.setInt(c, v)
			}
		case OpSubInvInt, -OpSubInvInt:
			vm.setInt(c, vm.intk(b, op < 0)-vm.int(a))
		case OpSubInvFloat64, -OpSubInvFloat64:
			vm.setFloat(c, vm.float(b)-vm.floatk(a, op < 0))

		// TailCall
		case OpTailCall:
			vm.calls = append(vm.calls, callFrame{cl: callable{fn: vm.fn, vars: vm.vars}, pc: vm.pc, status: tailed})
			if a != CurrentFunction {
				var fn *Function
				if a == 0 {
					closure := vm.general(b).Interface().(*callable)
					fn = closure.fn
					vm.vars = closure.vars
				} else {
					fn = vm.fn.Functions[uint8(b)]
					vm.vars = vm.env.globals
				}
				if vm.fp[0]+Addr(fn.NumReg[0]) > vm.st[0] {
					vm.moreIntStack()
				}
				if vm.fp[1]+Addr(fn.NumReg[1]) > vm.st[1] {
					vm.moreFloatStack()
				}
				if vm.fp[2]+Addr(fn.NumReg[2]) > vm.st[2] {
					vm.moreStringStack()
				}
				if vm.fp[3]+Addr(fn.NumReg[3]) > vm.st[3] {
					vm.moreGeneralStack()
				}
				vm.fn = fn
			}
			vm.pc = 0

		// Text
		case OpText:
			vm.setSrcCtx(true)
			txt := vm.fn.Text[decodeUint16(a, b)]
			inURL, isSet := c > 0, c == 2
			err := vm.renderer.Text(txt, inURL, isSet)
			if err != nil {
				panic(outError{err})
			}

		// Typify
		case OpTypify, -OpTypify:
			t := vm.fn.Types[uint8(a)]
			st, ok := t.(ScriggoType)
			if ok {
				t = st.GoType()
			}
			v, _ := vm.reflectValueInContext(b, t, op < 0, vm.nativeEnv.ctx)
			if st != nil {
				v = st.Wrap(v)
			}
			vm.setGeneral(c, v)

		// Xor
		case OpXor, -OpXor:
			vm.setInt(c, vm.int(a)^vm.intk(b, op < 0))

		// Zero
		case OpZero:
			not := false
			if a >= 10 {
				a -= 10
				not = true
			}
			var zero bool
			switch registerType(a) {
			case intRegister:
				zero = vm.int(b) == 0
			case floatRegister:
				zero = vm.float(b) == 0
			case stringRegister:
				zero = vm.string(b) == ""
			case generalRegister:
				rv := vm.general(b)
				zero = !rv.IsValid() || rv.IsZero()
				if zero {
					break
				}
				switch rv.Kind() {
				case reflect.Slice, reflect.Map:
					zero = rv.Len() == 0
				case reflect.Pointer:
					if rv.Elem().Kind() != reflect.Struct {
						break
					}
					fallthrough
				case reflect.Struct:
					switch v := rv.Interface().(type) {
					case *callable:
						zero = v.fn == nil
					case interface{ IsTrue() bool }:
						zero = !v.IsTrue()
					}
				}
			}
			if not {
				zero = !zero
			}
			vm.setBool(c, zero)
		}

	}
}
