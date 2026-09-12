// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type appendNilAnySlice []any

func TestAppendNilGeneralRegister(t *testing.T) {
	for _, sliceType := range []reflect.Type{
		reflect.TypeFor[[]any](), reflect.TypeFor[appendNilAnySlice](), reflect.TypeFor[[]io.Reader](),
	} {
		for _, capacity := range []int{1, 4} {
			t.Run(sliceType.String()+"/"+strconv.Itoa(capacity), func(t *testing.T) {
				vm := NewVM()
				vm.fp[3] = 4
				var typedNil *strings.Reader
				value := strings.NewReader("retained")
				vm.setGeneral(2, reflect.Value{})
				vm.setGeneral(3, reflect.ValueOf(typedNil))
				vm.setGeneral(4, reflect.ValueOf(value))
				backing := reflect.MakeSlice(sliceType, capacity, capacity)
				for i := range capacity {
					backing.Index(i).Set(reflect.ValueOf(value))
				}
				got := vm.appendSlice(2, 3, backing.Slice(0, 1))
				want := reflect.MakeSlice(sliceType, 4, 4)
				want.Index(0).Set(reflect.ValueOf(value))
				want.Index(2).Set(reflect.ValueOf(typedNil))
				want.Index(3).Set(reflect.ValueOf(value))
				if !reflect.DeepEqual(got.Interface(), want.Interface()) {
					t.Fatalf("append = %#v, want %#v", got.Interface(), want.Interface())
				}
				if capacity == 4 && !backing.Index(1).IsNil() {
					t.Fatal("nil append did not clear the reused backing element")
				}
			})
		}
	}
}
