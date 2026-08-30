// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build scriggo_native_frame_stats

package scriggo

import (
	"io"
	"reflect"
	"testing"

	"gitlab.com/haproxy-haptic/scriggo/native"
)

func TestNativeFrameCallStatisticsCountAuthenticatedAndFallbackCalls(t *testing.T) {
	var itemType reflect.Type
	fixture := newNativeFunctionCallFrameFixture(t, func([]reflect.Value) []reflect.Value {
		return []reflect.Value{reflect.New(itemType)}
	}, func(frame native.FunctionCallFrame) {
		frame.SetResultValue(0, reflect.New(itemType))
	}, `{{ resources.Store.Get("key").Value }}`)
	itemType = fixture.trampoline.Value().Type().Out(0).Elem()
	methodTemplate, err := BuildTemplate(Files{
		"index.txt": []byte(`{% _, _ = selector.Select("key") %}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"selector": native.Synchronous((*nativeFunctionTrampolineInterface)(nil), "Select"),
	}})
	if err != nil {
		t.Fatalf("build method: %v", err)
	}
	method := native.MakeMethodTrampolineWithFrame(
		reflect.TypeFor[nativeFunctionTrampolineInterface](),
		"Select",
		func([]reflect.Value) []reflect.Value {
			return []reflect.Value{reflect.ValueOf("value"), reflect.ValueOf(true)}
		},
		func(frame native.FunctionCallFrame) {
			frame.SetResultString(0, "value")
			frame.SetResultBool(1, true)
		},
	)

	ResetNativeFrameCallStatistics()
	for _, trampolines := range [][]*native.FunctionTrampoline{nil, {fixture.trampoline}} {
		if err := fixture.template.Run(io.Discard, map[string]any{"resources": fixture.resources}, &RunOptions{
			NativeFunctionTrampolines: trampolines,
		}); err != nil {
			t.Fatalf("run function field: %v", err)
		}
	}
	for _, trampolines := range [][]*native.FunctionTrampoline{nil, {method}} {
		if err := methodTemplate.Run(io.Discard, map[string]any{
			"selector": nativeFunctionTrampolineInterfaceValue{},
		}, &RunOptions{NativeFunctionTrampolines: trampolines}); err != nil {
			t.Fatalf("run method: %v", err)
		}
	}
	if got, want := ReadNativeFrameCallStatistics(), (NativeFrameCallStatistics{
		FunctionFieldAuthenticated:   1,
		FunctionFieldFallback:        1,
		InterfaceMethodAuthenticated: 1,
		InterfaceMethodFallback:      1,
	}); got != want {
		t.Fatalf("statistics = %+v, want %+v", got, want)
	}
}
