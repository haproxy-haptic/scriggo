// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// ErrDeterministicPrint rejects language-level output outside the renderer.
var ErrDeterministicPrint = errors.New("print and println are disabled in deterministic execution")

// ErrDeterministicMapKey rejects map keys without a stable value order.
var ErrDeterministicMapKey = errors.New("map key cannot be ordered deterministically")

// ErrDeterministicGo rejects detached goroutines and parallel rendering.
var ErrDeterministicGo = errors.New("goroutines and parallel rendering are disabled in deterministic execution")

var ErrBatchDetachedWork = errors.New("goroutines and parallel rendering are disabled in batch execution")

var ErrBatchUncertifiedNative = errors.New("native declaration is not certified for batch execution")

type deterministicMapEntry struct {
	key   reflect.Value
	value reflect.Value
}

func validateDeterministicProgram(main *Function) error {
	return ValidateDeterministicProgram(main)
}

func ValidateDeterministicProgram(main *Function) error {
	main.deterministicValidation.Do(func() {
		main.deterministicErr = deterministicProgramError(main)
	})
	return main.deterministicErr
}

func ValidateBatchProgram(main *Function) error {
	main.batchValidation.Do(func() {
		main.batchErr = batchProgramError(main)
	})
	return main.batchErr
}

func batchProgramError(main *Function) error {
	seen := map[*Function]struct{}{}
	functions := []*Function{main}
	for len(functions) > 0 {
		fn := functions[0]
		functions = functions[1:]
		if _, ok := seen[fn]; ok {
			continue
		}
		seen[fn] = struct{}{}
		for addr := 0; addr < len(fn.Body); addr++ {
			in := fn.Body[addr]
			switch in.Op {
			case OpGo, OpGoRender, OpGoRenderIndirect:
				return ErrBatchDetachedWork
			}
			addr += instructionOperandWords(in)
		}
		functions = append(functions, fn.Functions...)
	}
	if main.BatchUnsafeNative != "" {
		return fmt.Errorf("%w: %s", ErrBatchUncertifiedNative, main.BatchUnsafeNative)
	}
	return nil
}

func deterministicProgramError(main *Function) error {
	seen := map[*Function]struct{}{}
	functions := []*Function{main}
	for len(functions) > 0 {
		fn := functions[0]
		functions = functions[1:]
		if _, ok := seen[fn]; ok {
			continue
		}
		seen[fn] = struct{}{}
		if fn.UsesPrint {
			return ErrDeterministicPrint
		}
		for addr := 0; addr < len(fn.Body); addr++ {
			in := fn.Body[addr]
			info := fn.InstructionInfo[Addr(addr)]
			path := info.Path
			if path == "" {
				path = fn.File
			}
			switch in.Op {
			case OpPrint:
				return fmt.Errorf("%w: %s:%s:%s at %d", ErrDeterministicPrint, path, info.Position.String(), fn.Name, addr)
			case OpGo, OpGoRender, OpGoRenderIndirect:
				return fmt.Errorf(
					"%w: %s:%s:%s at %d",
					ErrDeterministicGo,
					path,
					info.Position.String(),
					fn.Name,
					addr,
				)
			}
			if typ, ok := fn.RangeTypes[Addr(addr)]; ok && typ.Kind() == reflect.Map {
				if err := validateDeterministicMapKeyType(typ.Key()); err != nil {
					return err
				}
			}
			addr += instructionOperandWords(in)
		}
		functions = append(functions, fn.Functions...)
	}
	return nil
}

func instructionOperandWords(in Instruction) int {
	switch in.Op {
	case OpCallFunc, OpCallMacro, OpGoRender, OpCallIndirect, OpCallNative, OpCallNativeValue,
		OpCallNativeFrameMethod,
		OpTailCall, OpSlice, OpStringSlice:
		return 1
	case OpDefer:
		return 2
	case OpMakeSlice:
		if in.B > 0 {
			return 1
		}
	}
	return 0
}

func validateDeterministicMapKeyType(typ reflect.Type) error {
	switch typ.Kind() {
	case reflect.Interface:
		return nil
	case reflect.Array:
		return validateDeterministicMapKeyType(typ.Elem())
	case reflect.Struct:
		for i := range typ.NumField() {
			if err := validateDeterministicMapKeyType(typ.Field(i).Type); err != nil {
				return err
			}
		}
		return nil
	case reflect.Pointer, reflect.UnsafePointer, reflect.Chan:
		return fmt.Errorf("%w: %s contains %s identity", ErrDeterministicMapKey, typ, typ.Kind())
	default:
		return nil
	}
}

func deterministicMapEntries(v reflect.Value) ([]deterministicMapEntry, error) {
	entries := make([]deterministicMapEntry, 0, v.Len())
	iter := v.MapRange()
	for iter.Next() {
		key := iter.Key()
		if err := validateDeterministicMapKey(key); err != nil {
			return nil, err
		}
		entries = append(entries, deterministicMapEntry{key: key, value: iter.Value()})
	}
	sort.Slice(entries, func(i, j int) bool {
		return compareDeterministicValues(entries[i].key, entries[j].key) < 0
	})
	for i := 1; i < len(entries); i++ {
		if compareDeterministicValues(entries[i-1].key, entries[i].key) == 0 &&
			!entries[i-1].key.Equal(entries[i].key) {
			return nil, fmt.Errorf("%w: distinct %s keys have the same order", ErrDeterministicMapKey, v.Type().Key())
		}
	}
	return entries, nil
}

func validateDeterministicMapKey(v reflect.Value) error {
	if !v.IsValid() {
		return nil
	}
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return validateDeterministicMapKey(v.Elem())
	case reflect.Array:
		for i := range v.Len() {
			if err := validateDeterministicMapKey(v.Index(i)); err != nil {
				return err
			}
		}
		return nil
	case reflect.Struct:
		for i := range v.NumField() {
			if err := validateDeterministicMapKey(v.Field(i)); err != nil {
				return err
			}
		}
		return nil
	case reflect.Float32, reflect.Float64:
		if math.IsNaN(v.Float()) {
			return fmt.Errorf("%w: %s contains NaN", ErrDeterministicMapKey, v.Type())
		}
		return nil
	case reflect.Complex64, reflect.Complex128:
		value := v.Complex()
		if math.IsNaN(real(value)) || math.IsNaN(imag(value)) {
			return fmt.Errorf("%w: %s contains NaN", ErrDeterministicMapKey, v.Type())
		}
		return nil
	case reflect.Pointer, reflect.UnsafePointer, reflect.Chan:
		return fmt.Errorf("%w: %s uses %s identity", ErrDeterministicMapKey, v.Type(), v.Kind())
	default:
		return nil
	}
}

func compareDeterministicValues(a, b reflect.Value) int {
	if !a.IsValid() {
		if !b.IsValid() {
			return 0
		}
		return -1
	}
	if a.Kind() == reflect.Interface {
		if a.IsNil() {
			if b.IsNil() {
				return 0
			}
			return -1
		}
		if b.IsNil() {
			return 1
		}
		a, b = a.Elem(), b.Elem()
		if order := cmp.Compare(deterministicTypeID(a.Type()), deterministicTypeID(b.Type())); order != 0 {
			return order
		}
	}
	switch a.Kind() {
	case reflect.Bool:
		if a.Bool() == b.Bool() {
			return 0
		}
		if !a.Bool() {
			return -1
		}
		return 1
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return cmp.Compare(a.Int(), b.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return cmp.Compare(a.Uint(), b.Uint())
	case reflect.Float32, reflect.Float64:
		return cmp.Compare(a.Float(), b.Float())
	case reflect.Complex64, reflect.Complex128:
		av, bv := a.Complex(), b.Complex()
		if order := cmp.Compare(real(av), real(bv)); order != 0 {
			return order
		}
		return cmp.Compare(imag(av), imag(bv))
	case reflect.String:
		return cmp.Compare(a.String(), b.String())
	case reflect.Array:
		for i := range a.Len() {
			if order := compareDeterministicValues(a.Index(i), b.Index(i)); order != 0 {
				return order
			}
		}
		return 0
	case reflect.Struct:
		for i := range a.NumField() {
			if order := compareDeterministicValues(a.Field(i), b.Field(i)); order != 0 {
				return order
			}
		}
		return 0
	default:
		panic("unsupported deterministic map key " + a.Type().String())
	}
}

func deterministicTypeID(typ reflect.Type) string {
	var id strings.Builder
	appendDeterministicTypeID(&id, typ)
	return id.String()
}

func appendDeterministicTypeID(id *strings.Builder, typ reflect.Type) {
	if typ.Name() != "" {
		id.WriteByte('N')
		appendDeterministicTypePart(id, typ.PkgPath())
		appendDeterministicTypePart(id, typ.Name())
		return
	}
	id.WriteByte('K')
	id.WriteString(strconv.Itoa(int(typ.Kind())))
	id.WriteByte(':')
	switch typ.Kind() {
	case reflect.Array:
		id.WriteString(strconv.Itoa(typ.Len()))
		id.WriteByte(':')
		appendDeterministicTypeID(id, typ.Elem())
	case reflect.Struct:
		id.WriteString(strconv.Itoa(typ.NumField()))
		id.WriteByte(':')
		for i := range typ.NumField() {
			field := typ.Field(i)
			appendDeterministicTypePart(id, field.Name)
			appendDeterministicTypePart(id, field.PkgPath)
			appendDeterministicTypePart(id, string(field.Tag))
			if field.Anonymous {
				id.WriteByte('1')
			} else {
				id.WriteByte('0')
			}
			appendDeterministicTypeID(id, field.Type)
		}
	}
}

func appendDeterministicTypePart(id *strings.Builder, value string) {
	id.WriteString(strconv.Itoa(len(value)))
	id.WriteByte(':')
	id.WriteString(value)
}
