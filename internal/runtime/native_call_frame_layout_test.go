// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"reflect"
	"testing"

	"gitlab.com/haproxy-haptic/scriggo/native"
)

func TestNativeFunctionFrameLayoutMatchesFallback(t *testing.T) {
	tests := []struct {
		name          string
		functionType  reflect.Type
		variadicCount int8
	}{
		{
			name:         "mixed registers",
			functionType: reflect.TypeFor[func(native.Env, string, int, any, float64, string) (any, string, int, float64)](),
		},
		{
			name:          "expanded variadic",
			functionType:  reflect.TypeFor[func(native.Env, string, ...any) (string, any)](),
			variadicCount: 3,
		},
		{
			name:          "unexpanded variadic",
			functionType:  reflect.TypeFor[func(native.Env, string, ...any) (string, any)](),
			variadicCount: NoVariadicArgs,
		},
		{
			name:         "wide fallback",
			functionType: reflect.TypeFor[func(string, string, string, string, string, string, string, string, string) (string, string, string, string, string, string, string, string, string)](),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			layout := makeNativeFunctionFrameLayout(test.functionType)
			fast := nativeFunctionCallState{
				functionType:  test.functionType,
				frameLayout:   &layout,
				input:         [4]Addr{11, 22, 33, 44},
				output:        [4]Addr{55, 66, 77, 88},
				variadicCount: test.variadicCount,
			}
			fallback := fast
			fallback.frameLayout = nil
			last := test.functionType.NumIn()
			if test.functionType.IsVariadic() && test.variadicCount != NoVariadicArgs {
				last--
			}
			for index := range last {
				if test.functionType.In(index) == envType {
					continue
				}
				assertNativeFunctionFrameArgumentLocation(t, &fast, &fallback, index, -1)
			}
			if test.functionType.IsVariadic() {
				variadicIndex := test.functionType.NumIn() - 1
				if test.variadicCount == NoVariadicArgs {
					assertNativeFunctionFrameArgumentLocation(t, &fast, &fallback, variadicIndex, -1)
				} else {
					for index := range int(test.variadicCount) {
						assertNativeFunctionFrameArgumentLocation(t, &fast, &fallback, variadicIndex, index)
					}
				}
			}
			for index := range test.functionType.NumOut() {
				fastRegister, fastAddress := functionCallResult(&fast, index)
				fallbackRegister, fallbackAddress := functionCallResult(&fallback, index)
				if fastRegister != fallbackRegister || fastAddress != fallbackAddress {
					t.Fatalf(
						"result %d location = %d/%d, want %d/%d",
						index, fastRegister, fastAddress, fallbackRegister, fallbackAddress,
					)
				}
			}
		})
	}
}

func assertNativeFunctionFrameArgumentLocation(
	t *testing.T,
	fast *nativeFunctionCallState,
	fallback *nativeFunctionCallState,
	argumentIndex int,
	variadicIndex int,
) {
	t.Helper()
	fastRegister, fastAddress := functionCallArgument(fast, argumentIndex, variadicIndex)
	fallbackRegister, fallbackAddress := functionCallArgument(fallback, argumentIndex, variadicIndex)
	if fastRegister != fallbackRegister || fastAddress != fallbackAddress {
		t.Fatalf(
			"argument %d/%d location = %d/%d, want %d/%d",
			argumentIndex, variadicIndex, fastRegister, fastAddress, fallbackRegister, fallbackAddress,
		)
	}
}

func BenchmarkNativeFunctionFrameArgumentLocation(b *testing.B) {
	functionType := reflect.TypeFor[func(string, string, string, string, string, string, string) int]()
	layout := makeNativeFunctionFrameLayout(functionType)
	for _, benchmark := range []struct {
		name   string
		layout *nativeFunctionFrameLayout
	}{
		{name: "fallback"},
		{name: "precomputed", layout: &layout},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			state := nativeFunctionCallState{
				functionType: functionType,
				frameLayout:  benchmark.layout,
				input:        [4]Addr{11, 22, 33, 44},
			}
			var address Addr
			b.ReportAllocs()
			for range b.N {
				for index := range functionType.NumIn() {
					_, address = functionCallArgument(&state, index, -1)
				}
			}
			if address == 0 {
				b.Fatal("zero address")
			}
		})
	}
}
