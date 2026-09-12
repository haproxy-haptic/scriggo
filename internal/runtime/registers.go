// Copyright 2019 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"context"
	"reflect"
)

type registers struct {
	int     []int64
	float   []float64
	string  []string
	general []reflect.Value
}

func (vm *VM) set(r int8, v reflect.Value) {
	k := v.Kind()
	if reflect.Int <= k && k <= reflect.Int64 {
		vm.setInt(r, v.Int())
	} else if reflect.Uint <= k && k <= reflect.Uintptr {
		vm.setInt(r, int64(v.Uint()))
	} else if k == reflect.String {
		vm.setString(r, v.String())
	} else if k == reflect.Float64 || k == reflect.Float32 {
		vm.setFloat(r, v.Float())
	} else {
		vm.setGeneral(r, v)
	}
}

func (vm *VM) int(r int8) int64 {
	if r > 0 {
		return vm.regs.int[vm.fp[0]+Addr(r)]
	}
	return vm.intIndirect(-r)
}

func (vm *VM) intk(r int8, k bool) int64 {
	if k {
		return int64(r)
	}
	if r > 0 {
		return vm.regs.int[vm.fp[0]+Addr(r)]
	}
	return vm.intIndirect(-r)
}

func (vm *VM) intIndirect(r int8) int64 {
	v := vm.regs.general[vm.fp[3]+Addr(r)]
	if v.IsNil() {
		panic(errNilPointer)
	}
	elem := v.Elem()
	k := elem.Kind()
	switch {
	case reflect.Int <= k && k <= reflect.Int64:
		return elem.Int()
	case k == reflect.Bool:
		if elem.Bool() {
			return 1
		}
		return 0
	default:
		return int64(elem.Uint())
	}
}

func (vm *VM) setInt(r int8, i int64) {
	if r > 0 {
		index := vm.fp[0] + Addr(r)
		vm.regs.int[index] = i
		vm.noteHigh(0, index)
		return
	}
	vm.setIntIndirect(-r, i)
}

func (vm *VM) setIntIndirect(r int8, i int64) {
	v := vm.regs.general[vm.fp[3]+Addr(r)]
	vm.observeMutation(MutationSetPointer, v)
	elem := v.Elem()
	k := elem.Kind()
	switch {
	case reflect.Int <= k && k <= reflect.Int64:
		elem.SetInt(i)
	case k == reflect.Bool:
		elem.SetBool(i == 1)
	default:
		elem.SetUint(uint64(i))
	}
}

func (vm *VM) bool(r int8) bool {
	if r > 0 {
		return vm.regs.int[vm.fp[0]+Addr(r)] > 0
	}
	return vm.boolIndirect(-r)
}

func (vm *VM) boolk(r int8, k bool) bool {
	if k {
		return r > 0
	}
	if r > 0 {
		return vm.regs.int[vm.fp[0]+Addr(r)] > 0
	}
	return vm.boolIndirect(-r)
}

func (vm *VM) boolIndirect(r int8) bool {
	v := vm.regs.general[vm.fp[3]+Addr(r)]
	if v.IsNil() {
		panic(errNilPointer)
	}
	return v.Elem().Bool()
}

func (vm *VM) setBool(r int8, b bool) {
	if r > 0 {
		v := int64(0)
		if b {
			v = 1
		}
		index := vm.fp[0] + Addr(r)
		vm.regs.int[index] = v
		vm.noteHigh(0, index)
		return
	}
	vm.setBoolIndirect(-r, b)
}

func (vm *VM) setBoolIndirect(r int8, b bool) {
	v := vm.regs.general[vm.fp[3]+Addr(r)]
	vm.observeMutation(MutationSetPointer, v)
	v.Elem().SetBool(b)
}

func (vm *VM) float(r int8) float64 {
	if r > 0 {
		return vm.regs.float[vm.fp[1]+Addr(r)]
	}
	return vm.floatIndirect(-r)
}

func (vm *VM) floatk(r int8, k bool) float64 {
	if k {
		return float64(r)
	}
	if r > 0 {
		return vm.regs.float[vm.fp[1]+Addr(r)]
	}
	return vm.floatIndirect(-r)
}

func (vm *VM) floatIndirect(r int8) float64 {
	v := vm.regs.general[vm.fp[3]+Addr(r)]
	if v.IsNil() {
		panic(errNilPointer)
	}
	return v.Elem().Float()
}

func (vm *VM) setFloat(r int8, f float64) {
	if r > 0 {
		index := vm.fp[1] + Addr(r)
		vm.regs.float[index] = f
		vm.noteHigh(1, index)
		return
	}
	vm.setFloatIndirect(-r, f)
}

func (vm *VM) setFloatIndirect(r int8, f float64) {
	v := vm.regs.general[vm.fp[3]+Addr(r)]
	vm.observeMutation(MutationSetPointer, v)
	v.Elem().SetFloat(f)
}

func (vm *VM) string(r int8) string {
	if r > 0 {
		return vm.regs.string[vm.fp[2]+Addr(r)]
	}
	return vm.stringIndirect(-r)
}

func (vm *VM) stringk(r int8, k bool) string {
	if k {
		return vm.fn.Values.String[uint8(r)]
	}
	if r > 0 {
		return vm.regs.string[vm.fp[2]+Addr(r)]
	}
	return vm.stringIndirect(-r)
}

func (vm *VM) stringIndirect(r int8) string {
	v := vm.regs.general[vm.fp[3]+Addr(r)]
	if v.IsNil() {
		panic(errNilPointer)
	}
	return v.Elem().String()
}

func (vm *VM) setString(r int8, s string) {
	if r > 0 {
		i := vm.fp[2] + Addr(r)
		vm.regs.string[i] = s
		vm.noteHigh(2, i)
		return
	}
	vm.setStringIndirect(-r, s)
}

func (vm *VM) setStringIndirect(r int8, s string) {
	v := vm.regs.general[vm.fp[3]+Addr(r)]
	vm.observeMutation(MutationSetPointer, v)
	v.Elem().SetString(s)
}

func (vm *VM) general(r int8) reflect.Value {
	if r > 0 {
		return vm.regs.general[vm.fp[3]+Addr(r)]
	}
	return vm.generalIndirect(-r)
}

func (vm *VM) generalk(r int8, k bool) reflect.Value {
	if k {
		return vm.fn.Values.General[uint8(r)]
	}
	if r > 0 {
		return vm.regs.general[vm.fp[3]+Addr(r)]
	}
	return vm.generalIndirect(-r)
}

func (vm *VM) generalIndirect(r int8) reflect.Value {
	v := vm.regs.general[vm.fp[3]+Addr(r)]
	if v.IsNil() {
		panic(errNilPointer)
	}
	elem := v.Elem()
	if elem.Kind() == reflect.Func {
		return reflect.ValueOf(newNativeCallable(elem))
	}
	return elem
}

func (vm *VM) setGeneral(r int8, v reflect.Value) {
	if r > 0 {
		i := vm.fp[3] + Addr(r)
		vm.regs.general[i] = v
		vm.noteHigh(3, i)
		return
	}
	vm.setGeneralIndirect(-r, v)
}

func (vm *VM) setGeneralIndirect(r int8, v reflect.Value) {
	ptr := vm.regs.general[vm.fp[3]+Addr(r)]
	vm.observeMutation(MutationSetPointer, ptr)
	ptr.Elem().Set(v)
}

func (vm *VM) getIntoReflectValue(r int8, v reflect.Value, k bool) registerType {
	return vm.getIntoReflectValueInContext(r, v, k, vm.nativeEnv.ctx)
}

func (vm *VM) getIntoReflectValueInContext(r int8, v reflect.Value, k bool, parentContext context.Context) registerType {
	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(vm.boolk(r, k))
		return intRegister
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(vm.intk(r, k))
		return intRegister
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		v.SetUint(uint64(vm.intk(r, k)))
		return intRegister
	case reflect.Float32, reflect.Float64:
		v.SetFloat(vm.floatk(r, k))
		return floatRegister
	case reflect.String:
		v.SetString(vm.stringk(r, k))
		return stringRegister
	case reflect.Func:
		vm.markBatchGlobalsEscaped()
		v.Set(vm.generalk(r, k).Interface().(*callable).Value(vm.env, parentContext))
		return generalRegister
	case reflect.Interface:
		if g := vm.generalk(r, k); !g.IsValid() { // TODO: check if it is correct.
			if t := v.Type(); t == emptyInterfaceType {
				v.Set(emptyInterfaceNil)
			} else {
				v.Set(reflect.Zero(t))
			}
		} else {
			vm.markBatchValueEscaped(g)
			v.Set(g)
		}
		return generalRegister
	default:
		value := vm.generalk(r, k)
		vm.markBatchValueEscaped(value)
		v.Set(value)
		return generalRegister
	}
}

func (vm *VM) reflectValueInContext(r int8, typ reflect.Type, k bool, parentContext context.Context) (reflect.Value, registerType) {
	var value reflect.Value
	var register registerType
	switch typ.Kind() {
	case reflect.Bool:
		value = reflect.ValueOf(vm.boolk(r, k))
		register = intRegister
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value = reflect.ValueOf(vm.intk(r, k))
		register = intRegister
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		value = reflect.ValueOf(uint64(vm.intk(r, k)))
		register = intRegister
	case reflect.Float32, reflect.Float64:
		value = reflect.ValueOf(vm.floatk(r, k))
		register = floatRegister
	case reflect.String:
		value = reflect.ValueOf(vm.stringk(r, k))
		register = stringRegister
	case reflect.Func:
		vm.markBatchGlobalsEscaped()
		value = vm.generalk(r, k).Interface().(*callable).Value(vm.env, parentContext)
		register = generalRegister
	case reflect.Interface:
		value = vm.generalk(r, k)
		if !value.IsValid() {
			value = reflect.Zero(typ)
		} else {
			vm.markBatchValueEscaped(value)
		}
		return value, generalRegister
	default:
		value = vm.generalk(r, k)
		vm.markBatchValueEscaped(value)
		return value, generalRegister
	}
	if value.Type() != typ {
		value = value.Convert(typ)
	}
	return value, register
}

func (vm *VM) markBatchValueEscaped(value reflect.Value) {
	for value.IsValid() && value.Kind() == reflect.Interface {
		if value.IsNil() {
			return
		}
		value = value.Elem()
	}
	if !value.IsValid() {
		return
	}
	switch value.Kind() {
	case reflect.Func:
		vm.markBatchGlobalsEscaped()
	case reflect.Pointer:
		if callable, ok := value.Interface().(*callable); ok && callable.fn != nil {
			vm.markBatchGlobalsEscaped()
			return
		}
		if vm.state != nil {
			vm.state.globalsEscaped.Store(true)
		}
	}
}

func snapshotValue(v reflect.Value) reflect.Value {
	if v.Kind() != reflect.Array && v.Kind() != reflect.Struct && !v.CanAddr() {
		return v
	}
	copied := reflect.New(v.Type()).Elem()
	copied.Set(v)
	return copied
}

func (vm *VM) setFromReflectValue(r int8, v reflect.Value) registerType {
	switch v.Kind() {
	case reflect.Bool:
		vm.setBool(r, v.Bool())
		return intRegister
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		vm.setInt(r, v.Int())
		return intRegister
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		vm.setInt(r, int64(v.Uint()))
		return intRegister
	case reflect.Float32, reflect.Float64:
		vm.setFloat(r, v.Float())
		return floatRegister
	case reflect.String:
		vm.setString(r, v.String())
		return stringRegister
	case reflect.Func:
		vm.setGeneral(r, reflect.ValueOf(newNativeCallable(v)))
		return generalRegister
	case reflect.Interface:
		vm.setGeneral(r, v.Elem())
		return generalRegister
	case reflect.Array, reflect.Struct:
		vm.setGeneral(r, v)
		return generalRegister
	default:
		vm.setGeneral(r, snapshotValue(v))
		return generalRegister
	}
}

func appendCap(oc, nl int) int {
	if oc == 0 || nl > oc*2 {
		return nl
	}
	if oc < 1024 {
		return oc * 2
	}
	c := oc
	for c < nl {
		c += c / 4
		if c <= 0 {
			c = nl
		}
	}
	return c
}

func (vm *VM) appendSlice(first int8, length int, slice reflect.Value) reflect.Value {
	switch s := slice.Interface().(type) {
	case []int:
		ol := len(s)
		nl := ol + length
		if nl < ol {
			panic("scriggo: slice overflow")
		}
		if c := cap(s); nl <= c {
			s = s[:nl]
		} else {
			old := s
			c = appendCap(c, nl)
			s = make([]int, nl, c)
			copy(s, old)
		}
		t := s[ol:]
		regs := vm.regs.int[vm.fp[0]+Addr(first):]
		for i := range length {
			t[i] = int(regs[i])
		}
		return reflect.ValueOf(s)
	case []byte:
		ol := len(s)
		nl := ol + length
		if nl < ol {
			panic("scriggo: slice overflow")
		}
		if c := cap(s); nl <= c {
			s = s[:nl]
		} else {
			old := s
			c = appendCap(c, nl)
			s = make([]byte, nl, c)
			copy(s, old)
		}
		t := s[ol:]
		r := vm.regs.int[vm.fp[0]+Addr(first):]
		for i := range length {
			t[i] = byte(r[i])
		}
		return reflect.ValueOf(s)
	case []rune:
		ol := len(s)
		nl := ol + length
		if nl < ol {
			panic("scriggo: slice overflow")
		}
		if c := cap(s); nl <= c {
			s = s[:nl]
		} else {
			old := s
			c = appendCap(c, nl)
			s = make([]rune, nl, c)
			copy(s, old)
		}
		t := s[ol:]
		r := vm.regs.int[vm.fp[0]+Addr(first):]
		for i := range length {
			t[i] = rune(r[i])
		}
		return reflect.ValueOf(s)
	case []float64:
		i := int(vm.fp[1] + Addr(first))
		return reflect.ValueOf(append(s, vm.regs.float[i:i+length]...))
	case []string:
		i := int(vm.fp[2] + Addr(first))
		return reflect.ValueOf(append(s, vm.regs.string[i:i+length]...))
	case []interface{}:
		// Fast path for []interface{} ([]any) - the most common generic slice type
		// Avoids reflect.MakeSlice overhead by using native append
		ol := len(s)
		nl := ol + length
		if nl < ol {
			panic("scriggo: slice overflow")
		}
		if c := cap(s); nl <= c {
			s = s[:nl]
		} else {
			old := s
			c = appendCap(c, nl)
			s = make([]interface{}, nl, c)
			copy(s, old)
		}
		regs := vm.regs.general[vm.fp[3]+Addr(first):]
		for i, j := 0, ol; i < length; i, j = i+1, j+1 {
			s[j] = nil
			if regs[i].IsValid() {
				s[j] = regs[i].Interface()
			}
		}
		return reflect.ValueOf(s)
	default:
		ol := slice.Len()
		nl := ol + length
		if nl < ol {
			panic(runtimeError("append: out of memory"))
		}
		if c := slice.Cap(); nl <= c {
			slice = slice.Slice(0, nl)
		} else {
			old := slice
			c = appendCap(c, nl)
			slice = reflect.MakeSlice(slice.Type(), nl, c)
			if ol > 0 {
				reflect.Copy(slice, old)
			}
		}
		switch slice.Type().Elem().Kind() {
		case reflect.Bool:
			regs := vm.regs.int[vm.fp[0]+Addr(first):]
			for i, j := 0, ol; i < length; i, j = i+1, j+1 {
				slice.Index(j).SetBool(regs[i] > 0)
			}
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			regs := vm.regs.int[vm.fp[0]+Addr(first):]
			for i, j := 0, ol; i < length; i, j = i+1, j+1 {
				slice.Index(j).SetInt(regs[i])
			}
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			regs := vm.regs.int[vm.fp[0]+Addr(first):]
			for i, j := 0, ol; i < length; i, j = i+1, j+1 {
				slice.Index(j).SetUint(uint64(regs[i]))
			}
		case reflect.Float32, reflect.Float64:
			regs := vm.regs.float[vm.fp[1]+Addr(first):]
			for i, j := 0, ol; i < length; i, j = i+1, j+1 {
				slice.Index(j).SetFloat(regs[i])
			}
		case reflect.String:
			regs := vm.regs.string[vm.fp[2]+Addr(first):]
			for i, j := 0, ol; i < length; i, j = i+1, j+1 {
				slice.Index(j).SetString(regs[i])
			}
		default:
			regs := vm.regs.general[vm.fp[3]+Addr(first):]
			for i, j := 0, ol; i < length; i, j = i+1, j+1 {
				if regs[i].IsValid() {
					slice.Index(j).Set(regs[i])
				} else {
					slice.Index(j).SetZero()
				}
			}
		}
		return slice
	}
}

// noteHigh records that register i of bank b has been written, so Reset knows
// how much of the bank to clear. Indirect writes go through
// a pointer already held in a register, which setGeneral recorded when it put
// the pointer there.
func (vm *VM) noteHigh(b int, i Addr) {
	if i > vm.high[b] {
		vm.high[b] = i
	}
}

func (vm *VM) noteCopiedRegisters(bank, count int) {
	if count > 0 {
		vm.noteHigh(bank, Addr(count-1))
	}
}
