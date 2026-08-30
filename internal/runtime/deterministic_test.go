// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

type deterministicNamedInt int

func TestDeterministicMapEntriesOrdersSupportedKeys(t *testing.T) {
	keyStruct := func(a int, b string) struct {
		A int
		B string
	} {
		return struct {
			A int
			B string
		}{a, b}
	}
	tests := []struct {
		name  string
		value any
	}{
		{"bool", map[bool]int{true: 1, false: 0}},
		{"int", map[int]int{3: 3, -1: -1, 0: 0}},
		{"int8", map[int8]int{3: 3, -1: -1, 0: 0}},
		{"int16", map[int16]int{3: 3, -1: -1, 0: 0}},
		{"int32", map[int32]int{3: 3, -1: -1, 0: 0}},
		{"int64", map[int64]int{3: 3, -1: -1, 0: 0}},
		{"uint", map[uint]int{3: 3, 1: 1, 0: 0}},
		{"uint8", map[uint8]int{3: 3, 1: 1, 0: 0}},
		{"uint16", map[uint16]int{3: 3, 1: 1, 0: 0}},
		{"uint32", map[uint32]int{3: 3, 1: 1, 0: 0}},
		{"uint64", map[uint64]int{3: 3, 1: 1, 0: 0}},
		{"uintptr", map[uintptr]int{3: 3, 1: 1, 0: 0}},
		{"float32", map[float32]int{3.5: 3, -1.5: -1, 0: 0}},
		{"float64", map[float64]int{3.5: 3, -1.5: -1, 0: 0}},
		{"complex64", map[complex64]int{3 + 1i: 3, -1 + 2i: -1, 3 - 1i: 2}},
		{"complex128", map[complex128]int{3 + 1i: 3, -1 + 2i: -1, 3 - 1i: 2}},
		{"string", map[string]int{"z": 3, "a": 1, "m": 2}},
		{"array", map[[2]int]int{{2, 0}: 2, {1, 9}: 1, {1, 2}: 0}},
		{"struct", map[struct {
			A int
			B string
		}]int{keyStruct(2, "a"): 2, keyStruct(1, "z"): 1, keyStruct(1, "a"): 0}},
		{"named", map[deterministicNamedInt]int{3: 3, -1: -1, 0: 0}},
		{"interface", map[any]int{nil: 0, "z": 1, int64(2): 2, false: 3, [1]int{1}: 4}},
		{"array interface", map[[1]any]int{{"z"}: 1, {int64(2)}: 2, {false}: 3}},
		{"struct interface", map[struct{ Value any }]int{{"z"}: 1, {int64(2)}: 2, {false}: 3}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var baseline string
			for range 100 {
				entries, err := deterministicMapEntries(reflect.ValueOf(test.value))
				if err != nil {
					t.Fatalf("entries: %v", err)
				}
				if len(entries) != reflect.ValueOf(test.value).Len() {
					t.Fatalf("entry count = %d, want %d", len(entries), reflect.ValueOf(test.value).Len())
				}
				for i := 1; i < len(entries); i++ {
					if compareDeterministicValues(entries[i-1].key, entries[i].key) >= 0 {
						t.Fatalf("keys are not strictly ordered at %d: %v, %v", i, entries[i-1].key, entries[i].key)
					}
				}
				var got strings.Builder
				for _, entry := range entries {
					fmt.Fprintf(&got, "%T=%v;", entry.key.Interface(), entry.key.Interface())
				}
				if baseline == "" {
					baseline = got.String()
				} else if got.String() != baseline {
					t.Fatalf("order changed: %q, want %q", got.String(), baseline)
				}
			}
		})
	}
}

func TestDeterministicMapEntriesRejectsUnstableKeys(t *testing.T) {
	value := 1
	channel := make(chan int)
	tests := []struct {
		name  string
		value any
	}{
		{"pointer", map[*int]int{&value: 1}},
		{"channel", map[chan int]int{channel: 1}},
		{"interface pointer", map[any]int{&value: 1}},
		{"array pointer", map[[1]*int]int{{&value}: 1}},
		{"struct channel", map[struct{ C chan int }]int{{channel}: 1}},
		{"float NaN", map[float64]int{math.NaN(): 1}},
		{"complex NaN", map[complex128]int{complex(math.NaN(), 0): 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := deterministicMapEntries(reflect.ValueOf(test.value))
			if !errors.Is(err, ErrDeterministicMapKey) {
				t.Fatalf("error = %v, want %v", err, ErrDeterministicMapKey)
			}
		})
	}
}

func TestValidateDeterministicMapKeyType(t *testing.T) {
	value := 1
	tests := []struct {
		name    string
		typ     reflect.Type
		wantErr bool
	}{
		{"string", reflect.TypeFor[string](), false},
		{"interface", reflect.TypeFor[any](), false},
		{"array", reflect.TypeFor[[2]string](), false},
		{"struct", reflect.TypeFor[struct{ A [2]int }](), false},
		{"pointer", reflect.TypeOf(&value), true},
		{"array pointer", reflect.TypeFor[[1]*int](), true},
		{"struct channel", reflect.TypeFor[struct{ C chan int }](), true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateDeterministicMapKeyType(test.typ)
			if got := errors.Is(err, ErrDeterministicMapKey); got != test.wantErr {
				t.Fatalf("error = %v, errors.Is = %t, want %t", err, got, test.wantErr)
			}
		})
	}
}
