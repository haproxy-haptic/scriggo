// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gitlab.com/haproxy-haptic/scriggo/internal/runtime"
	"gitlab.com/haproxy-haptic/scriggo/native"
)

type nativeFunctionTrampolineContextKey struct{}

type nativeFunctionTrampolineInterface interface {
	Select(string) (string, bool)
}

type nativeFunctionTrampolineInterfaceValue struct{}

func (nativeFunctionTrampolineInterfaceValue) Select(value string) (string, bool) {
	return "reflect:" + value, true
}

type nativeFunctionTrampolineAlternateInterface interface {
	Select(string) (string, bool)
}

type nativeFunctionTrampolineNilReceiver struct{}

func (receiver *nativeFunctionTrampolineNilReceiver) Select(value string) (string, bool) {
	if receiver == nil {
		return "nil:" + value, true
	}
	return "value:" + value, true
}

type nativeFunctionCallFrameNamedInt int16

type nativeFunctionCallFrameNamedMap map[string]int

type nativeFunctionCallFrameNamedSlice []string

type nativeFunctionCallFramePayload struct {
	Value string
}

var nativeFunctionCallArgumentSliceSink []any

type nativeFunctionTrampolineFixture struct {
	template   *Template
	resources  any
	trampoline *native.FunctionTrampoline
}

func newNativeFunctionTrampolineFixture(
	t testing.TB,
	callback func([]reflect.Value) []reflect.Value,
	source string,
) nativeFunctionTrampolineFixture {
	t.Helper()
	itemType := reflect.StructOf([]reflect.StructField{{Name: "Value", Type: reflect.TypeFor[string]()}})
	functionType := reflect.FuncOf(
		[]reflect.Type{reflect.TypeFor[native.Env](), reflect.TypeFor[string]()},
		[]reflect.Type{reflect.PointerTo(itemType)},
		false,
	)
	trampoline := native.MakeFunctionTrampoline(functionType, callback)
	storeType := reflect.StructOf([]reflect.StructField{{Name: "Get", Type: functionType}})
	resourcesType := reflect.StructOf([]reflect.StructField{{Name: "Store", Type: reflect.PointerTo(storeType)}})
	store := reflect.New(storeType)
	store.Elem().Field(0).Set(trampoline.Value())
	resources := reflect.New(resourcesType)
	resources.Elem().Field(0).Set(store)
	declaration := reflect.Zero(reflect.PointerTo(resourcesType)).Interface()
	template, err := BuildTemplate(Files{"index.txt": []byte(source)}, "index.txt", &BuildOptions{
		Globals: native.Declarations{
			"iterations": (*[]int)(nil),
			"resources":  native.Synchronous(declaration, "Store.Get"),
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return nativeFunctionTrampolineFixture{
		template: template, resources: resources.Interface(), trampoline: trampoline,
	}
}

func newNativeFunctionCallFrameFixture(
	t testing.TB,
	callback func([]reflect.Value) []reflect.Value,
	callFrame func(native.FunctionCallFrame),
	source string,
) nativeFunctionTrampolineFixture {
	t.Helper()
	itemType := reflect.StructOf([]reflect.StructField{{Name: "Value", Type: reflect.TypeFor[string]()}})
	functionType := reflect.FuncOf(
		[]reflect.Type{reflect.TypeFor[native.Env](), reflect.TypeFor[string]()},
		[]reflect.Type{reflect.PointerTo(itemType)},
		false,
	)
	trampoline := native.MakeFunctionTrampolineWithFrame(functionType, callback, callFrame)
	storeType := reflect.StructOf([]reflect.StructField{{Name: "Get", Type: functionType}})
	resourcesType := reflect.StructOf([]reflect.StructField{{Name: "Store", Type: reflect.PointerTo(storeType)}})
	store := reflect.New(storeType)
	store.Elem().Field(0).Set(trampoline.Value())
	resources := reflect.New(resourcesType)
	resources.Elem().Field(0).Set(store)
	declaration := reflect.Zero(reflect.PointerTo(resourcesType)).Interface()
	template, err := BuildTemplate(Files{"index.txt": []byte(source)}, "index.txt", &BuildOptions{
		Globals: native.Declarations{
			"iterations": (*[]int)(nil),
			"resources":  native.Synchronous(declaration, "Store.Get"),
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return nativeFunctionTrampolineFixture{
		template: template, resources: resources.Interface(), trampoline: trampoline,
	}
}

func TestTemplateNativeFunctionTrampolineMatchesReflectiveDispatch(t *testing.T) {
	marker := &struct{}{}
	ctx := context.WithValue(t.Context(), nativeFunctionTrampolineContextKey{}, marker)
	var callbackCalls int
	var itemType reflect.Type
	fixture := newNativeFunctionTrampolineFixture(t, func(args []reflect.Value) []reflect.Value {
		callbackCalls++
		env := args[0].Interface().(native.Env)
		if env.Context().Value(nativeFunctionTrampolineContextKey{}) != marker {
			t.Fatal("native callback received the wrong context")
		}
		if got := args[1].String(); got != "key" {
			t.Fatalf("native argument = %q, want key", got)
		}
		item := reflect.New(itemType)
		item.Elem().Field(0).SetString("value")
		return []reflect.Value{item}
	}, `{{ resources.Store.Get("key").Value }}`)
	itemType = fixture.trampoline.Value().Type().Out(0).Elem()

	for _, test := range []struct {
		name       string
		trampoline bool
	}{
		{name: "reflective"},
		{name: "trampoline", trampoline: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			beforeCalls := 0
			observerCalls := 0
			options := &RunOptions{
				Context: ctx,
				BeforeNativeCall: func(env native.Env) error {
					beforeCalls++
					if env.Context().Value(nativeFunctionTrampolineContextKey{}) != marker {
						t.Fatal("preflight received the wrong context")
					}
					return nil
				},
				ObserveNativeCall: func(native.Env, NativeCall) error {
					observerCalls++
					return nil
				},
			}
			if test.trampoline {
				options.NativeFunctionTrampolines = []*native.FunctionTrampoline{fixture.trampoline}
			}
			var output strings.Builder
			err := fixture.template.Run(&output, map[string]any{"resources": fixture.resources}, options)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := output.String(); got != "value" {
				t.Fatalf("output = %q, want value", got)
			}
			if beforeCalls != 1 {
				t.Fatalf("preflight calls = %d, want 1", beforeCalls)
			}
			if observerCalls != 0 {
				t.Fatalf("observer calls = %d, want 0 for a function field", observerCalls)
			}
		})
	}
	if callbackCalls != 2 {
		t.Fatalf("callback calls = %d, want 2", callbackCalls)
	}
}

func TestTemplateNativeFunctionCallFrameMatchesReflectiveDispatch(t *testing.T) {
	marker := &struct{}{}
	ctx := context.WithValue(t.Context(), nativeFunctionTrampolineContextKey{}, marker)
	var itemType reflect.Type
	legacyCalls := 0
	frameCalls := 0
	newItem := func() reflect.Value {
		item := reflect.New(itemType)
		item.Elem().Field(0).SetString("value")
		return item
	}
	fixture := newNativeFunctionCallFrameFixture(
		t,
		func(args []reflect.Value) []reflect.Value {
			legacyCalls++
			if args[0].Interface().(native.Env).Context().Value(nativeFunctionTrampolineContextKey{}) != marker ||
				args[1].String() != "key" {
				t.Fatal("reflective call received the wrong arguments")
			}
			return []reflect.Value{newItem()}
		},
		func(frame native.FunctionCallFrame) {
			frameCalls++
			if frame.ArgEnv(0).Context().Value(nativeFunctionTrampolineContextKey{}) != marker ||
				frame.ArgString(1) != "key" {
				t.Fatal("frame call received the wrong arguments")
			}
			frame.SetResultValue(0, newItem())
		},
		`{{ resources.Store.Get("key").Value }}`,
	)
	itemType = fixture.trampoline.Value().Type().Out(0).Elem()

	for _, trampolines := range [][]*native.FunctionTrampoline{nil, {fixture.trampoline}} {
		var output strings.Builder
		err := fixture.template.Run(&output, map[string]any{"resources": fixture.resources}, &RunOptions{
			Context:                   ctx,
			NativeFunctionTrampolines: trampolines,
		})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if got := output.String(); got != "value" {
			t.Fatalf("output = %q, want value", got)
		}
	}
	if legacyCalls != 1 || frameCalls != 1 {
		t.Fatalf("calls = legacy %d frame %d, want 1/1", legacyCalls, frameCalls)
	}
}

func TestTemplateNativeFunctionCallFrameSupportsExpandedVariadicArguments(t *testing.T) {
	functionType := reflect.TypeFor[func(native.Env, ...any) string]()
	legacyCalls := 0
	frameCalls := 0
	var retainedArguments native.FunctionCallArguments
	trampoline := native.MakeFunctionTrampolineWithFrame(
		functionType,
		func(args []reflect.Value) []reflect.Value {
			legacyCalls++
			values := args[1].Interface().([]any)
			return []reflect.Value{reflect.ValueOf(values[0].(string) + strconv.Itoa(values[1].(int)))}
		},
		func(frame native.FunctionCallFrame) {
			frameCalls++
			arguments := frame.VariadicArguments()
			retainedArguments = arguments
			if arguments.Len() != 2 {
				t.Fatalf("variadic len = %d, want 2", arguments.Len())
			}
			first := native.FunctionCallArgumentAt[any](arguments, 0).(string)
			second := native.FunctionCallArgumentAt[any](arguments, 1).(int)
			frame.SetResultString(0, first+strconv.Itoa(second))
		},
	)
	template, err := BuildTemplate(Files{"index.txt": []byte(`{{ lookup("key", 42) }}`)}, "index.txt", &BuildOptions{
		Globals: native.Declarations{"lookup": native.Synchronous(trampoline.Value().Interface())},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, trampolines := range [][]*native.FunctionTrampoline{nil, {trampoline}} {
		var output strings.Builder
		err := template.Run(&output, nil, &RunOptions{NativeFunctionTrampolines: trampolines})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if got := output.String(); got != "key42" {
			t.Fatalf("output = %q, want key42", got)
		}
	}
	if legacyCalls != 1 || frameCalls != 1 {
		t.Fatalf("calls = legacy %d frame %d, want 1/1", legacyCalls, frameCalls)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("retained variadic arguments remained active")
			}
		}()
		retainedArguments.Len()
	}()
}

func TestTemplateNativeFunctionCallFrameIsRevokedAfterReturn(t *testing.T) {
	var retained []native.FunctionCallFrame
	var itemType reflect.Type
	fixture := newNativeFunctionCallFrameFixture(
		t,
		func([]reflect.Value) []reflect.Value {
			return []reflect.Value{reflect.New(itemType)}
		},
		func(frame native.FunctionCallFrame) {
			retained = append(retained, frame)
			frame.SetResultValue(0, reflect.New(itemType))
		},
		`{{ resources.Store.Get("key").Value }}`,
	)
	itemType = fixture.trampoline.Value().Type().Out(0).Elem()
	for range 2 {
		if err := fixture.template.Run(io.Discard, map[string]any{"resources": fixture.resources}, &RunOptions{
			NativeFunctionTrampolines: []*native.FunctionTrampoline{fixture.trampoline},
		}); err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	if len(retained) != 2 {
		t.Fatalf("retained frames = %d, want 2", len(retained))
	}
	for index, frame := range retained {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("retained frame %d remained active", index)
				}
			}()
			frame.ArgString(1)
		}()
	}
}

func TestTemplateNativeFunctionCallFrameRequiresEveryResult(t *testing.T) {
	fixture := newNativeFunctionCallFrameFixture(
		t,
		func([]reflect.Value) []reflect.Value {
			return []reflect.Value{reflect.Zero(reflect.TypeFor[*struct{ Value string }]())}
		},
		func(native.FunctionCallFrame) {},
		`{{ resources.Store.Get("key").Value }}`,
	)
	err := fixture.template.Run(io.Discard, map[string]any{"resources": fixture.resources}, &RunOptions{
		NativeFunctionTrampolines: []*native.FunctionTrampoline{fixture.trampoline},
	})
	if err == nil || !strings.Contains(err.Error(), "did not set every result") {
		t.Fatalf("error = %v, want missing result", err)
	}
}

func TestTemplateNativeFunctionCallFramePreservesNamedPrimitives(t *testing.T) {
	functionType := reflect.TypeFor[func(nativeFunctionCallFrameNamedInt) nativeFunctionCallFrameNamedInt]()
	trampoline := native.MakeFunctionTrampolineWithFrame(
		functionType,
		func(args []reflect.Value) []reflect.Value {
			return []reflect.Value{reflect.ValueOf(nativeFunctionCallFrameNamedInt(args[0].Int() + 1))}
		},
		func(frame native.FunctionCallFrame) {
			frame.SetResultInt(0, frame.ArgInt(0)+1)
		},
	)
	template, err := BuildTemplate(Files{"index.txt": []byte(`{{ increment(41) }}`)}, "index.txt", &BuildOptions{
		Globals: native.Declarations{"increment": native.Synchronous(trampoline.Value().Interface())},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, trampolines := range [][]*native.FunctionTrampoline{nil, {trampoline}} {
		var output strings.Builder
		if err := template.Run(&output, nil, &RunOptions{NativeFunctionTrampolines: trampolines}); err != nil {
			t.Fatalf("run: %v", err)
		}
		if got := output.String(); got != "42" {
			t.Fatalf("output = %q, want 42", got)
		}
	}
}

func TestTemplateNativeFunctionCallFramePreservesTypedNilArgumentsAndResults(t *testing.T) {
	probeType := reflect.TypeFor[func(any, *int, map[string]int, []int) bool]()
	probe := native.MakeFunctionTrampolineWithFrame(
		probeType,
		func(args []reflect.Value) []reflect.Value {
			return []reflect.Value{reflect.ValueOf(
				args[0].IsNil() && args[1].IsNil() && args[2].IsNil() && args[3].IsNil(),
			)}
		},
		func(frame native.FunctionCallFrame) {
			frame.SetResultBool(
				0,
				native.FunctionCallFrameArg[any](frame, 0) == nil &&
					native.FunctionCallFrameArg[*int](frame, 1) == nil &&
					native.FunctionCallFrameArg[map[string]int](frame, 2) == nil &&
					native.FunctionCallFrameArg[[]int](frame, 3) == nil,
			)
		},
	)
	nilsType := reflect.TypeFor[func() (any, *int, map[string]int, []int)]()
	nils := native.MakeFunctionTrampolineWithFrame(
		nilsType,
		func([]reflect.Value) []reflect.Value {
			results := make([]reflect.Value, nilsType.NumOut())
			for index := range results {
				results[index] = reflect.Zero(nilsType.Out(index))
			}
			return results
		},
		func(frame native.FunctionCallFrame) {
			for index := range nilsType.NumOut() {
				frame.SetResultZero(index)
			}
		},
	)
	template, err := BuildTemplate(
		Files{"index.txt": []byte(`{{ probe(nil, nil, nil, nil) }}:{% a, p, m, s := nils() %}{{ a == nil }}{{ p == nil }}{{ m == nil }}{{ s == nil }}`)},
		"index.txt",
		&BuildOptions{Globals: native.Declarations{
			"probe": native.Synchronous(probe.Value().Interface()),
			"nils":  native.Synchronous(nils.Value().Interface()),
		}},
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, trampolines := range [][]*native.FunctionTrampoline{nil, {probe, nils}} {
		var output strings.Builder
		if err := template.Run(&output, nil, &RunOptions{NativeFunctionTrampolines: trampolines}); err != nil {
			t.Fatalf("run: %v", err)
		}
		if got := output.String(); got != "true:truetruetruetrue" {
			t.Fatalf("output = %q", got)
		}
	}
}

func TestTemplateNativeFunctionCallFrameTypedAccessPreservesNamedCompositeTypes(t *testing.T) {
	functionType := reflect.TypeFor[func(nativeFunctionCallFrameNamedMap, nativeFunctionCallFrameNamedSlice) string]()
	legacyCalls := 0
	frameCalls := 0
	trampoline := native.MakeFunctionTrampolineWithFrame(
		functionType,
		func(args []reflect.Value) []reflect.Value {
			legacyCalls++
			items := args[0].Interface().(nativeFunctionCallFrameNamedMap)
			labels := args[1].Interface().(nativeFunctionCallFrameNamedSlice)
			return []reflect.Value{reflect.ValueOf(labels[0] + strconv.Itoa(items["key"]))}
		},
		func(frame native.FunctionCallFrame) {
			frameCalls++
			items := native.FunctionCallFrameArg[nativeFunctionCallFrameNamedMap](frame, 0)
			labels := native.FunctionCallFrameArg[nativeFunctionCallFrameNamedSlice](frame, 1)
			frame.SetResultString(0, labels[0]+strconv.Itoa(items["key"]))
		},
	)
	template, err := BuildTemplate(Files{"index.txt": []byte(`{{ probe(items, labels) }}`)}, "index.txt", &BuildOptions{
		Globals: native.Declarations{
			"items":  (*nativeFunctionCallFrameNamedMap)(nil),
			"labels": (*nativeFunctionCallFrameNamedSlice)(nil),
			"probe":  native.Synchronous(trampoline.Value().Interface()),
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	values := map[string]any{
		"items":  nativeFunctionCallFrameNamedMap{"key": 42},
		"labels": nativeFunctionCallFrameNamedSlice{"value"},
	}
	for _, trampolines := range [][]*native.FunctionTrampoline{nil, {trampoline}} {
		var output strings.Builder
		if err := template.Run(&output, values, &RunOptions{NativeFunctionTrampolines: trampolines}); err != nil {
			t.Fatalf("run: %v", err)
		}
		if got := output.String(); got != "value42" {
			t.Fatalf("output = %q, want value42", got)
		}
	}
	if legacyCalls != 1 || frameCalls != 1 {
		t.Fatalf("calls = legacy %d frame %d, want 1/1", legacyCalls, frameCalls)
	}
}

func TestTemplateNativeFunctionCallFrameTypedAccessRejectsStaticTypeMismatch(t *testing.T) {
	functionType := reflect.TypeFor[func(map[string]int) bool]()
	trampoline := native.MakeFunctionTrampolineWithFrame(
		functionType,
		func([]reflect.Value) []reflect.Value { return []reflect.Value{reflect.ValueOf(true)} },
		func(frame native.FunctionCallFrame) {
			native.FunctionCallFrameArg[map[string]string](frame, 0)
			frame.SetResultBool(0, true)
		},
	)
	template, err := BuildTemplate(Files{"index.txt": []byte(`{{ probe(nil) }}`)}, "index.txt", &BuildOptions{
		Globals: native.Declarations{"probe": native.Synchronous(trampoline.Value().Interface())},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	err = template.Run(io.Discard, nil, &RunOptions{NativeFunctionTrampolines: []*native.FunctionTrampoline{trampoline}})
	if err == nil || !strings.Contains(err.Error(), "incompatible static type") {
		t.Fatalf("error = %v, want incompatible static type", err)
	}
}

func TestTemplateNativeFunctionCallFrameTypedAccessPreservesValueAndPointerEscape(t *testing.T) {
	functionType := reflect.TypeFor[func(nativeFunctionCallFramePayload, *nativeFunctionCallFramePayload) string]()
	var retainedValue nativeFunctionCallFramePayload
	var retainedPointer *nativeFunctionCallFramePayload
	trampoline := native.MakeFunctionTrampolineWithFrame(
		functionType,
		func(args []reflect.Value) []reflect.Value {
			return []reflect.Value{reflect.ValueOf(args[0].FieldByName("Value").String())}
		},
		func(frame native.FunctionCallFrame) {
			retainedValue = native.FunctionCallFrameArg[nativeFunctionCallFramePayload](frame, 0)
			retainedPointer = native.FunctionCallFrameArg[*nativeFunctionCallFramePayload](frame, 1)
			frame.SetResultString(0, retainedValue.Value)
		},
	)
	template, err := BuildTemplate(Files{"index.txt": []byte(`{{ retain(value, pointer) }}`)}, "index.txt", &BuildOptions{
		Globals: native.Declarations{
			"pointer": (**nativeFunctionCallFramePayload)(nil),
			"retain":  native.Synchronous(trampoline.Value().Interface()),
			"value":   (*nativeFunctionCallFramePayload)(nil),
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	value := nativeFunctionCallFramePayload{Value: "initial"}
	pointer := &nativeFunctionCallFramePayload{Value: "initial"}
	var output strings.Builder
	if err := template.Run(&output, map[string]any{
		"pointer": pointer,
		"value":   value,
	}, &RunOptions{NativeFunctionTrampolines: []*native.FunctionTrampoline{trampoline}}); err != nil {
		t.Fatalf("run: %v", err)
	}
	value.Value = "changed"
	pointer.Value = "changed"
	if output.String() != "initial" || retainedValue.Value != "initial" || retainedPointer.Value != "changed" {
		t.Fatalf(
			"output/value/pointer = %q/%q/%q, want initial/initial/changed",
			output.String(), retainedValue.Value, retainedPointer.Value,
		)
	}
}

func TestTemplateNativeFunctionCallFrameWideSignatureUsesFallbackLayout(t *testing.T) {
	stringType := reflect.TypeFor[string]()
	inputs := make([]reflect.Type, 9)
	outputs := make([]reflect.Type, 9)
	for index := range inputs {
		inputs[index] = stringType
		outputs[index] = stringType
	}
	functionType := reflect.FuncOf(inputs, outputs, false)
	legacy := func(args []reflect.Value) []reflect.Value { return args }
	trampoline := native.MakeFunctionTrampolineWithFrame(functionType, legacy, func(frame native.FunctionCallFrame) {
		for index := range functionType.NumIn() {
			frame.SetResultString(index, frame.ArgString(index))
		}
	})
	template, err := BuildTemplate(
		Files{"index.txt": []byte(
			`{% a, b, c, d, e, f, g, h, i := wide("a", "b", "c", "d", "e", "f", "g", "h", "i") %}` +
				`{{ a }}{{ b }}{{ c }}{{ d }}{{ e }}{{ f }}{{ g }}{{ h }}{{ i }}`,
		)},
		"index.txt",
		&BuildOptions{Globals: native.Declarations{"wide": native.Synchronous(trampoline.Value().Interface())}},
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var output strings.Builder
	if err := template.Run(&output, nil, &RunOptions{
		NativeFunctionTrampolines: []*native.FunctionTrampoline{trampoline},
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := output.String(); got != "abcdefghi" {
		t.Fatalf("output = %q, want abcdefghi", got)
	}
}

func TestTemplateNativeFunctionCallFrameSupportsUnexpandedVariadicArguments(t *testing.T) {
	functionType := reflect.TypeFor[func(...string) string]()
	trampoline := native.MakeFunctionTrampolineWithFrame(
		functionType,
		func(args []reflect.Value) []reflect.Value {
			return []reflect.Value{reflect.ValueOf(strings.Join(args[0].Interface().([]string), ""))}
		},
		func(frame native.FunctionCallFrame) {
			arguments := frame.VariadicArguments()
			if arguments.Len() != 2 {
				t.Fatalf("variadic len = %d, want 2", arguments.Len())
			}
			frame.SetResultString(0,
				native.FunctionCallArgumentAt[string](arguments, 0)+
					native.FunctionCallArgumentAt[string](arguments, 1),
			)
		},
	)
	template, err := BuildTemplate(Files{"index.txt": []byte(`{{ join(values...) }}`)}, "index.txt", &BuildOptions{
		Globals: native.Declarations{
			"join":   native.Synchronous(trampoline.Value().Interface()),
			"values": (*[]string)(nil),
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, trampolines := range [][]*native.FunctionTrampoline{nil, {trampoline}} {
		var output strings.Builder
		if err := template.Run(&output, map[string]any{"values": []string{"a", "b"}}, &RunOptions{
			NativeFunctionTrampolines: trampolines,
		}); err != nil {
			t.Fatalf("run: %v", err)
		}
		if got := output.String(); got != "ab" {
			t.Fatalf("output = %q, want ab", got)
		}
	}
}

func TestTemplateNativeFunctionCallArgumentsPreserveUnexpandedTypedNil(t *testing.T) {
	functionType := reflect.TypeFor[func(...any) bool]()
	check := func(values []any) bool {
		pointer, ok := values[1].(*int)
		return values[0] == nil && ok && pointer == nil
	}
	trampoline := native.MakeFunctionTrampolineWithFrame(
		functionType,
		func(args []reflect.Value) []reflect.Value {
			return []reflect.Value{reflect.ValueOf(check(args[0].Interface().([]any)))}
		},
		func(frame native.FunctionCallFrame) {
			arguments := frame.VariadicArguments()
			pointer, ok := native.FunctionCallArgumentAt[any](arguments, 1).(*int)
			frame.SetResultBool(
				0,
				native.FunctionCallArgumentAt[any](arguments, 0) == nil && ok && pointer == nil,
			)
		},
	)
	template, err := BuildTemplate(Files{"index.txt": []byte(`{{ probe(values...) }}`)}, "index.txt", &BuildOptions{
		Globals: native.Declarations{
			"probe":  native.Synchronous(trampoline.Value().Interface()),
			"values": (*[]any)(nil),
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var pointer *int
	values := []any{nil, pointer}
	for _, trampolines := range [][]*native.FunctionTrampoline{nil, {trampoline}} {
		var output strings.Builder
		if err := template.Run(&output, map[string]any{"values": values}, &RunOptions{
			NativeFunctionTrampolines: trampolines,
		}); err != nil {
			t.Fatalf("run: %v", err)
		}
		if got := output.String(); got != "true" {
			t.Fatalf("output = %q, want true", got)
		}
	}
}

func TestTemplateNativeFunctionCallFrameSurvivesSynchronousReentry(t *testing.T) {
	innerType := reflect.TypeFor[func() string]()
	inner := native.MakeFunctionTrampolineWithFrame(
		innerType,
		func([]reflect.Value) []reflect.Value { return []reflect.Value{reflect.ValueOf("inner")} },
		func(frame native.FunctionCallFrame) { frame.SetResultString(0, "inner") },
	)
	outerType := reflect.TypeFor[func(func() string) string]()
	outer := native.MakeFunctionTrampolineWithFrame(
		outerType,
		func(args []reflect.Value) []reflect.Value { return args[0].Call(nil) },
		func(frame native.FunctionCallFrame) {
			result := frame.ArgValue(0).Call(nil)[0].String()
			frame.SetResultString(0, result)
		},
	)
	template, err := BuildTemplate(
		Files{"index.txt": []byte(`{{ outer(func() string { return inner() }) }}`)},
		"index.txt",
		&BuildOptions{Globals: native.Declarations{
			"inner": native.Synchronous(inner.Value().Interface()),
			"outer": native.Synchronous(outer.Value().Interface()),
		}},
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var output strings.Builder
	if err := template.Run(&output, nil, &RunOptions{
		NativeFunctionTrampolines: []*native.FunctionTrampoline{inner, outer},
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := output.String(); got != "inner" {
		t.Fatalf("output = %q, want inner", got)
	}
}

func TestTemplateNativeFunctionCallFrameRejectsRetainedFrameDuringLaterCall(t *testing.T) {
	functionType := reflect.TypeFor[func(string) string]()
	var retained native.FunctionCallFrame
	secondActive := make(chan struct{})
	releaseSecond := make(chan struct{})
	calls := 0
	trampoline := native.MakeFunctionTrampolineWithFrame(
		functionType,
		func(args []reflect.Value) []reflect.Value { return args },
		func(frame native.FunctionCallFrame) {
			calls++
			if calls == 1 {
				retained = frame
			} else {
				close(secondActive)
				<-releaseSecond
			}
			frame.SetResultString(0, frame.ArgString(0))
		},
	)
	template, err := BuildTemplate(Files{"index.txt": []byte(`{{ identity("value") }}`)}, "index.txt", &BuildOptions{
		Globals: native.Declarations{"identity": native.Synchronous(trampoline.Value().Interface())},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	run := func() error {
		return template.Run(io.Discard, nil, &RunOptions{
			NativeFunctionTrampolines: []*native.FunctionTrampoline{trampoline},
		})
	}
	if err := run(); err != nil {
		t.Fatalf("first run: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- run() }()
	<-secondActive
	retainedAccepted := false
	func() {
		defer func() {
			if recover() == nil {
				retainedAccepted = true
			}
		}()
		retained.ArgString(0)
	}()
	close(releaseSecond)
	if err := <-done; err != nil {
		t.Fatalf("second run: %v", err)
	}
	if retainedAccepted {
		t.Fatal("retained frame read a later call")
	}
}

func TestTemplateNativeFunctionCallFrameDrainsEveryArgumentAccessBeforeReuse(t *testing.T) {
	functionType := reflect.TypeFor[func(native.Env, bool, int, uint, float64, string, any, ...any) (bool, int, uint, float64, string, any)]()
	var entered chan native.FunctionCallFrame
	var release chan struct{}
	trampoline := native.MakeFunctionTrampolineWithFrame(
		functionType,
		func([]reflect.Value) []reflect.Value {
			return []reflect.Value{
				reflect.ValueOf(true), reflect.ValueOf(1), reflect.ValueOf(uint(2)),
				reflect.ValueOf(3.5), reflect.ValueOf("result"), reflect.ValueOf(any("value")),
			}
		},
		func(frame native.FunctionCallFrame) {
			frame.SetResultBool(0, true)
			frame.SetResultInt(1, 1)
			frame.SetResultUint(2, 2)
			frame.SetResultFloat(3, 3.5)
			frame.SetResultString(4, "result")
			frame.SetResultValue(5, reflect.ValueOf("value"))
			entered <- frame
			<-release
		},
	)
	template, err := BuildTemplate(
		Files{"index.txt": []byte(`{% _, _, _, _, _, _ = probe(true, -4, 5, 1.25, "text", "value", "variadic") %}`)},
		"index.txt",
		&BuildOptions{Globals: native.Declarations{
			"probe": native.Synchronous(trampoline.Value().Interface()),
		}},
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	operations := []func(native.FunctionCallFrame){
		func(frame native.FunctionCallFrame) { _ = frame.Type() },
		func(frame native.FunctionCallFrame) { _ = frame.ArgEnv(0).Context() },
		func(frame native.FunctionCallFrame) { _ = frame.ArgBool(1) },
		func(frame native.FunctionCallFrame) { _ = frame.ArgInt(2) },
		func(frame native.FunctionCallFrame) { _ = frame.ArgUint(3) },
		func(frame native.FunctionCallFrame) { _ = frame.ArgFloat(4) },
		func(frame native.FunctionCallFrame) { _ = frame.ArgString(5) },
		func(frame native.FunctionCallFrame) { _ = frame.ArgValue(6) },
		func(frame native.FunctionCallFrame) { _ = native.FunctionCallFrameArg[any](frame, 6) },
		func(frame native.FunctionCallFrame) {
			arguments := frame.VariadicArguments()
			_ = arguments.Len()
			_ = native.FunctionCallArgumentAt[any](arguments, 0)
		},
	}
	for iteration := range 32 {
		entered = make(chan native.FunctionCallFrame, 1)
		release = make(chan struct{})
		runDone := make(chan error, 1)
		go func() {
			runDone <- template.Run(io.Discard, nil, &RunOptions{
				Context:                   t.Context(),
				NativeFunctionTrampolines: []*native.FunctionTrampoline{trampoline},
			})
		}()
		frame := <-entered
		start := make(chan struct{})
		panics := make(chan any, len(operations))
		var accesses sync.WaitGroup
		for _, operation := range operations {
			accesses.Add(1)
			go func() {
				defer accesses.Done()
				defer func() { panics <- recover() }()
				<-start
				for {
					operation(frame)
				}
			}()
		}
		close(start)
		close(release)
		accesses.Wait()
		close(panics)
		for recovered := range panics {
			if recovered == nil || !strings.Contains(fmt.Sprint(recovered), "revoked") {
				t.Fatalf("iteration %d argument access panic = %v, want revoked", iteration, recovered)
			}
		}
		if err := <-runDone; err != nil {
			t.Fatalf("iteration %d run: %v", iteration, err)
		}
	}
}

func TestTemplateNativeFunctionCallFrameSetResultsRaceRevocation(t *testing.T) {
	tests := []struct {
		name   string
		result reflect.Type
		set    func(native.FunctionCallFrame)
	}{
		{name: "bool", result: reflect.TypeFor[bool](), set: func(frame native.FunctionCallFrame) { frame.SetResultBool(0, true) }},
		{name: "int", result: reflect.TypeFor[int](), set: func(frame native.FunctionCallFrame) { frame.SetResultInt(0, 1) }},
		{name: "uint", result: reflect.TypeFor[uint](), set: func(frame native.FunctionCallFrame) { frame.SetResultUint(0, 1) }},
		{name: "float", result: reflect.TypeFor[float64](), set: func(frame native.FunctionCallFrame) { frame.SetResultFloat(0, 1) }},
		{name: "string", result: reflect.TypeFor[string](), set: func(frame native.FunctionCallFrame) { frame.SetResultString(0, "value") }},
		{name: "value", result: reflect.TypeFor[any](), set: func(frame native.FunctionCallFrame) { frame.SetResultValue(0, reflect.ValueOf("value")) }},
		{name: "zero", result: reflect.TypeFor[any](), set: func(frame native.FunctionCallFrame) { frame.SetResultZero(0) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			functionType := reflect.FuncOf(nil, []reflect.Type{test.result}, false)
			var entered chan native.FunctionCallFrame
			var release chan struct{}
			trampoline := native.MakeFunctionTrampolineWithFrame(
				functionType,
				func([]reflect.Value) []reflect.Value { return []reflect.Value{reflect.Zero(test.result)} },
				func(frame native.FunctionCallFrame) {
					entered <- frame
					<-release
				},
			)
			template, err := BuildTemplate(
				Files{"index.txt": []byte(`{% _ = probe() %}`)},
				"index.txt",
				&BuildOptions{Globals: native.Declarations{
					"probe": native.Synchronous(trampoline.Value().Interface()),
				}},
			)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			for iteration := range 32 {
				entered = make(chan native.FunctionCallFrame, 1)
				release = make(chan struct{})
				runDone := make(chan error, 1)
				go func() {
					runDone <- template.Run(io.Discard, nil, &RunOptions{
						NativeFunctionTrampolines: []*native.FunctionTrampoline{trampoline},
					})
				}()
				frame := <-entered
				start := make(chan struct{})
				setDone := make(chan any, 1)
				go func() {
					defer func() { setDone <- recover() }()
					<-start
					test.set(frame)
				}()
				close(start)
				close(release)
				recovered := <-setDone
				runErr := <-runDone
				if recovered != nil && !strings.Contains(fmt.Sprint(recovered), "revoked") {
					t.Fatalf("iteration %d setter panic = %v, want revoked", iteration, recovered)
				}
				if recovered == nil && runErr != nil {
					t.Fatalf("iteration %d successful setter run: %v", iteration, runErr)
				}
				if recovered != nil && (runErr == nil || !strings.Contains(runErr.Error(), "did not set every result")) {
					t.Fatalf("iteration %d revoked setter error = %v", iteration, runErr)
				}
			}
		})
	}
}

func TestTemplateNativeFunctionCallFramePreservesStopAndRejectsDuplicateResult(t *testing.T) {
	stopErr := errors.New("frame stop")
	stopType := reflect.TypeFor[func(native.Env) string]()
	stop := native.MakeFunctionTrampolineWithFrame(
		stopType,
		func(args []reflect.Value) []reflect.Value {
			args[0].Interface().(native.Env).Stop(stopErr)
			return nil
		},
		func(frame native.FunctionCallFrame) {
			frame.ArgEnv(0).Stop(stopErr)
		},
	)
	stopTemplate, err := BuildTemplate(Files{"index.txt": []byte(`{{ stop() }}`)}, "index.txt", &BuildOptions{
		Globals: native.Declarations{"stop": native.Synchronous(stop.Value().Interface())},
	})
	if err != nil {
		t.Fatalf("build stop: %v", err)
	}
	for _, trampolines := range [][]*native.FunctionTrampoline{nil, {stop}} {
		err := stopTemplate.Run(io.Discard, nil, &RunOptions{NativeFunctionTrampolines: trampolines})
		if !errors.Is(err, stopErr) {
			t.Fatalf("stop error = %v, want %v", err, stopErr)
		}
	}

	duplicateType := reflect.TypeFor[func() string]()
	duplicate := native.MakeFunctionTrampolineWithFrame(
		duplicateType,
		func([]reflect.Value) []reflect.Value { return []reflect.Value{reflect.ValueOf("value")} },
		func(frame native.FunctionCallFrame) {
			frame.SetResultString(0, "first")
			frame.SetResultString(0, "second")
		},
	)
	duplicateTemplate, err := BuildTemplate(Files{"index.txt": []byte(`{{ duplicate() }}`)}, "index.txt", &BuildOptions{
		Globals: native.Declarations{"duplicate": native.Synchronous(duplicate.Value().Interface())},
	})
	if err != nil {
		t.Fatalf("build duplicate: %v", err)
	}
	err = duplicateTemplate.Run(io.Discard, nil, &RunOptions{
		NativeFunctionTrampolines: []*native.FunctionTrampoline{duplicate},
	})
	if err == nil || !strings.Contains(err.Error(), "result was set twice") {
		t.Fatalf("duplicate result error = %v", err)
	}
}

func TestTemplateNativeFunctionTrampolinePreservesPanic(t *testing.T) {
	panicValue := errors.New("native trampoline panic")
	fixture := newNativeFunctionTrampolineFixture(t, func([]reflect.Value) []reflect.Value {
		panic(panicValue)
	}, `{{ resources.Store.Get("key").Value }}`)

	errorsByMode := make([]string, 2)
	for index, trampolines := range [][]*native.FunctionTrampoline{nil, {fixture.trampoline}} {
		err := fixture.template.Run(io.Discard, map[string]any{"resources": fixture.resources}, &RunOptions{
			NativeFunctionTrampolines: trampolines,
		})
		if err == nil {
			t.Fatalf("run %d succeeded, want panic error", index)
		}
		errorsByMode[index] = err.Error()
	}
	if errorsByMode[0] != errorsByMode[1] {
		t.Fatalf("panic errors differ:\nreflective: %s\ntrampoline: %s", errorsByMode[0], errorsByMode[1])
	}
}

func TestTemplateNativeFunctionTrampolineRejectsMalformedResultLikeMakeFunc(t *testing.T) {
	fixture := newNativeFunctionTrampolineFixture(t, func([]reflect.Value) []reflect.Value {
		return nil
	}, `{{ resources.Store.Get("key").Value }}`)

	errorsByMode := make([]string, 2)
	for index, trampolines := range [][]*native.FunctionTrampoline{nil, {fixture.trampoline}} {
		err := fixture.template.Run(io.Discard, map[string]any{"resources": fixture.resources}, &RunOptions{
			NativeFunctionTrampolines: trampolines,
		})
		if err == nil {
			t.Fatalf("run %d succeeded, want malformed result error", index)
		}
		errorsByMode[index] = err.Error()
	}
	if errorsByMode[0] != errorsByMode[1] {
		t.Fatalf("malformed result errors differ:\nreflective: %s\ntrampoline: %s", errorsByMode[0], errorsByMode[1])
	}
}

func TestTemplateNativeFunctionTrampolineRejectsInvalidRegistration(t *testing.T) {
	fixture := newNativeFunctionTrampolineFixture(t, func([]reflect.Value) []reflect.Value {
		return []reflect.Value{reflect.New(reflect.StructOf([]reflect.StructField{{Name: "Value", Type: reflect.TypeFor[string]()}}))}
	}, `constant`)
	err := fixture.template.Run(io.Discard, map[string]any{"resources": fixture.resources}, &RunOptions{
		NativeFunctionTrampolines: []*native.FunctionTrampoline{nil},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid nil native function trampoline") {
		t.Fatalf("error = %v, want invalid trampoline", err)
	}
}

func TestTemplateNativeFunctionTargetTrampolinePreservesMethodGuards(t *testing.T) {
	template, err := BuildTemplate(Files{
		"wrapper.txt": []byte(`{% for index := range indices %}{% controller.Begin(index) %}{{ value }}{% controller.End(index) %}{% end %}`),
	}, "wrapper.txt", &BuildOptions{Globals: vectorTestGlobals((**vectorTestController)(nil))})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	beginCalls := 0
	endCalls := 0
	begin := native.MakeMethodTrampolineWithFrame(
		reflect.TypeFor[*vectorTestController](),
		"Begin",
		func(args []reflect.Value) []reflect.Value {
			beginCalls++
			args[0].Interface().(*vectorTestController).Begin(
				args[1].Interface().(native.Env),
				int(args[2].Int()),
			)
			return nil
		},
		func(frame native.FunctionCallFrame) {
			beginCalls++
			frame.ArgValue(0).Interface().(*vectorTestController).Begin(
				frame.ArgEnv(1),
				int(frame.ArgInt(2)),
			)
		},
	)
	end := native.MakeMethodTrampolineWithFrame(
		reflect.TypeFor[*vectorTestController](),
		"End",
		func(args []reflect.Value) []reflect.Value {
			endCalls++
			args[0].Interface().(*vectorTestController).End(
				args[1].Interface().(native.Env),
				int(args[2].Int()),
			)
			return nil
		},
		func(frame native.FunctionCallFrame) {
			endCalls++
			frame.ArgValue(0).Interface().(*vectorTestController).End(
				frame.ArgEnv(1),
				int(frame.ArgInt(2)),
			)
		},
	)
	authority := native.NewVectorAuthority()
	controller := &vectorTestController{authority: authority}
	preflights := 0
	observed := make([]string, 0, 4)
	var output strings.Builder
	err = template.Run(&output, map[string]any{
		"controller": controller,
		"indices":    []int{0, 1},
	}, &RunOptions{
		BeforeNativeCall: func(native.Env) error {
			preflights++
			return nil
		},
		ObserveNativeCall: func(_ native.Env, call NativeCall) error {
			if mutationTargetIdentity(call.Receiver) != reflect.ValueOf(controller).Pointer() {
				t.Fatalf("receiver = %v, want %p", call.Receiver, controller)
			}
			observed = append(observed, call.Method+":"+strconv.Itoa(call.Line))
			return nil
		},
		NativeFunctionTrampolines: []*native.FunctionTrampoline{begin, end},
		Vector: &VectorRunOptions{
			Authority: authority,
			Count:     2,
			Bindings:  map[string]any{"value": []string{"first", "second"}},
			Contexts:  []context.Context{context.Background(), context.Background()},
		},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := output.String(), "firstsecond"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if beginCalls != 2 || endCalls != 2 || preflights != 4 {
		t.Fatalf("calls = begin %d, end %d, preflight %d; want 2, 2, 4", beginCalls, endCalls, preflights)
	}
	if got, want := strings.Join(observed, ","), "Begin:1,End:1,Begin:1,End:1"; got != want {
		t.Fatalf("observed = %q, want %q", got, want)
	}
	if got, want := strings.Join(controller.calls, ","), "begin:0,end:0,begin:1,end:1"; got != want {
		t.Fatalf("controller calls = %q, want %q", got, want)
	}
}

func TestTemplateNativeFunctionTargetTrampolineObserverRejectsBeforeBody(t *testing.T) {
	template, err := BuildTemplate(Files{
		"wrapper.txt": []byte(`{% controller.Begin(0) %}`),
	}, "wrapper.txt", &BuildOptions{Globals: vectorTestGlobals((**vectorTestController)(nil))})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	trampolineCalls := 0
	begin := native.MakeMethodTrampoline(
		reflect.TypeFor[*vectorTestController](),
		"Begin",
		func([]reflect.Value) []reflect.Value {
			trampolineCalls++
			return nil
		},
	)
	controller := &vectorTestController{authority: native.NewVectorAuthority()}
	err = template.Run(io.Discard, map[string]any{
		"controller": controller,
		"indices":    []int{},
	}, &RunOptions{
		ObserveNativeCall: func(native.Env, NativeCall) error {
			return errNativeCallRejected
		},
		NativeFunctionTrampolines: []*native.FunctionTrampoline{begin},
	})
	if err != errNativeCallRejected {
		t.Fatalf("run error = %T %v, want sentinel", err, err)
	}
	if trampolineCalls != 0 || len(controller.calls) != 0 {
		t.Fatalf("effects = trampolines %d, controller calls %v; want none", trampolineCalls, controller.calls)
	}
}

func TestTemplateNativeFunctionTargetTrampolineRejectsConflictingTargets(t *testing.T) {
	template, err := BuildTemplate(Files{"index.txt": []byte(`constant`)}, "index.txt", nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	target := reflect.TypeFor[*vectorTestController]()
	first := native.MakeMethodTrampoline(target, "Begin", func([]reflect.Value) []reflect.Value { return nil })
	second := native.MakeMethodTrampoline(target, "Begin", func([]reflect.Value) []reflect.Value { return nil })
	err = template.Run(io.Discard, nil, &RunOptions{
		NativeFunctionTrampolines: []*native.FunctionTrampoline{first, second},
	})
	if err == nil || !strings.Contains(err.Error(), "conflicting native method trampolines") {
		t.Fatalf("error = %v, want conflicting targets", err)
	}
}

func TestTemplateNativeFunctionCallFrameSupportsInterfaceMethods(t *testing.T) {
	template, err := BuildTemplate(Files{
		"index.txt": []byte(`{% value, found := selector.Select("key") %}{{ value }}:{{ found }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"selector": native.Synchronous((*nativeFunctionTrampolineInterface)(nil), "Select"),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	frameCalls := 0
	trampoline := native.MakeMethodTrampolineWithFrame(
		reflect.TypeFor[nativeFunctionTrampolineInterface](),
		"Select",
		func(args []reflect.Value) []reflect.Value {
			value, found := args[0].Interface().(nativeFunctionTrampolineInterface).Select(args[1].String())
			return []reflect.Value{reflect.ValueOf(value), reflect.ValueOf(found)}
		},
		func(frame native.FunctionCallFrame) {
			frameCalls++
			value, found := native.FunctionCallFrameReceiver[nativeFunctionTrampolineInterface](frame).Select(
				frame.ArgString(0),
			)
			frame.SetResultString(0, value)
			frame.SetResultBool(1, found)
		},
	)
	var output strings.Builder
	err = template.Run(&output, map[string]any{
		"selector": nativeFunctionTrampolineInterfaceValue{},
	}, &RunOptions{NativeFunctionTrampolines: []*native.FunctionTrampoline{trampoline}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := output.String(); got != "reflect:key:true" || frameCalls != 1 {
		t.Fatalf("output/calls = %q/%d, want reflect:key:true/1", got, frameCalls)
	}
}

func TestTemplateNativeFunctionCallFrameInterfaceMethodMatchesReflectiveGuards(t *testing.T) {
	template, err := BuildTemplate(Files{
		"index.txt": []byte(`{% value, found := selector.Select("key") %}{{ value }}:{{ found }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"selector": native.Synchronous((*nativeFunctionTrampolineInterface)(nil), "Select"),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	trampoline := native.MakeMethodTrampolineWithFrame(
		reflect.TypeFor[nativeFunctionTrampolineInterface](),
		"Select",
		func(args []reflect.Value) []reflect.Value {
			value, found := args[0].Interface().(nativeFunctionTrampolineInterface).Select(args[1].String())
			return []reflect.Value{reflect.ValueOf(value), reflect.ValueOf(found)}
		},
		func(frame native.FunctionCallFrame) {
			value, found := native.FunctionCallFrameReceiver[nativeFunctionTrampolineInterface](frame).Select(
				frame.ArgString(0),
			)
			frame.SetResultString(0, value)
			frame.SetResultBool(1, found)
		},
	)
	marker := &struct{}{}
	ctx := context.WithValue(t.Context(), nativeFunctionTrampolineContextKey{}, marker)
	for _, test := range []struct {
		name        string
		trampolines []*native.FunctionTrampoline
	}{
		{name: "reflective"},
		{name: "authenticated", trampolines: []*native.FunctionTrampoline{trampoline}},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := make([]string, 0, 3)
			var output strings.Builder
			err := template.Run(&output, map[string]any{
				"selector": nativeFunctionTrampolineInterfaceValue{},
			}, &RunOptions{
				Context: ctx,
				BeforeNativeCall: func(env native.Env) error {
					if env.Context().Value(nativeFunctionTrampolineContextKey{}) != marker {
						t.Fatal("preflight received the wrong context")
					}
					events = append(events, "before")
					return nil
				},
				ObserveNativeCall: func(env native.Env, call NativeCall) error {
					if env.Context().Value(nativeFunctionTrampolineContextKey{}) != marker {
						t.Fatal("observer received the wrong context")
					}
					events = append(events, fmt.Sprintf("observe:%s:%t", call.Method, call.MethodValue))
					return nil
				},
				NativeFunctionTrampolines: test.trampolines,
			})
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if output.String() != "reflect:key:true" {
				t.Fatalf("output = %q, want reflect:key:true", output.String())
			}
			if got, want := strings.Join(events, ","), "observe:Select:true,before,observe:Select:false"; got != want {
				t.Fatalf("events = %q, want %q", got, want)
			}
		})
	}
}

func TestTemplateNativeFunctionCallFrameInterfaceMethodRejectsAmbiguousSites(t *testing.T) {
	template, err := BuildTemplate(Files{
		"index.txt": []byte(`{% value, _ := selector.Select("key") %}{{ value }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"selector": native.Synchronous((*nativeFunctionTrampolineInterface)(nil), "Select"),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	makeTrampoline := func(receiver reflect.Type, value string) *native.FunctionTrampoline {
		return native.MakeMethodTrampolineWithFrame(
			receiver,
			"Select",
			func([]reflect.Value) []reflect.Value {
				return []reflect.Value{reflect.ValueOf(value), reflect.ValueOf(true)}
			},
			func(frame native.FunctionCallFrame) {
				frame.SetResultString(0, value)
				frame.SetResultBool(1, true)
			},
		)
	}
	first := makeTrampoline(reflect.TypeFor[nativeFunctionTrampolineInterface](), "first")
	second := makeTrampoline(reflect.TypeFor[nativeFunctionTrampolineAlternateInterface](), "second")
	var output strings.Builder
	err = template.Run(&output, map[string]any{
		"selector": nativeFunctionTrampolineInterfaceValue{},
	}, &RunOptions{NativeFunctionTrampolines: []*native.FunctionTrampoline{first, second}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := output.String(); got != "reflect:key" {
		t.Fatalf("output = %q, want reflective fallback", got)
	}
}

func TestTemplateNativeFunctionCallFrameInterfaceMethodPreservesTypedNilReceiver(t *testing.T) {
	template, err := BuildTemplate(Files{
		"index.txt": []byte(`{% value, _ := selector.Select("key") %}{{ value }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"selector": native.Synchronous((*nativeFunctionTrampolineInterface)(nil), "Select"),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	trampoline := native.MakeMethodTrampolineWithFrame(
		reflect.TypeFor[nativeFunctionTrampolineInterface](),
		"Select",
		func(args []reflect.Value) []reflect.Value {
			value, found := args[0].Interface().(nativeFunctionTrampolineInterface).Select(args[1].String())
			return []reflect.Value{reflect.ValueOf(value), reflect.ValueOf(found)}
		},
		func(frame native.FunctionCallFrame) {
			value, found := native.FunctionCallFrameReceiver[nativeFunctionTrampolineInterface](frame).Select(
				frame.ArgString(0),
			)
			frame.SetResultString(0, value)
			frame.SetResultBool(1, found)
		},
	)
	var receiver *nativeFunctionTrampolineNilReceiver
	for _, test := range []struct {
		name        string
		trampolines []*native.FunctionTrampoline
	}{
		{name: "reflective"},
		{name: "authenticated", trampolines: []*native.FunctionTrampoline{trampoline}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output strings.Builder
			err := template.Run(&output, map[string]any{"selector": receiver}, &RunOptions{
				NativeFunctionTrampolines: test.trampolines,
			})
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := output.String(); got != "nil:key" {
				t.Fatalf("output = %q, want nil:key", got)
			}
		})
	}
}

func BenchmarkTemplateNativeFunctionTrampoline(b *testing.B) {
	var itemType reflect.Type
	legacy := func([]reflect.Value) []reflect.Value {
		item := reflect.New(itemType)
		item.Elem().Field(0).SetString("value")
		return []reflect.Value{item}
	}
	fixture := newNativeFunctionCallFrameFixture(b, legacy, func(frame native.FunctionCallFrame) {
		item := reflect.New(itemType)
		item.Elem().Field(0).SetString("value")
		frame.SetResultValue(0, item)
	}, `{% for range iterations %}{{ resources.Store.Get("key").Value }}{% end %}`)
	itemType = fixture.trampoline.Value().Type().Out(0).Elem()
	valueTrampoline := native.MakeFunctionTrampolineFor(fixture.trampoline.Value(), legacy)
	iterations := make([]int, 128)
	for _, test := range []struct {
		name        string
		template    *Template
		trampolines []*native.FunctionTrampoline
	}{
		{name: "reflective", template: fixture.template},
		{name: "value-trampoline", template: fixture.template, trampolines: []*native.FunctionTrampoline{valueTrampoline}},
		{name: "frame", template: fixture.template, trampolines: []*native.FunctionTrampoline{fixture.trampoline}},
	} {
		b.Run(test.name, func(b *testing.B) {
			options := &RunOptions{NativeFunctionTrampolines: test.trampolines}
			values := map[string]any{"iterations": iterations, "resources": fixture.resources}
			b.ReportAllocs()
			for range b.N {
				if err := test.template.Run(io.Discard, values, options); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkTemplateNativeInterfaceMethodFrame(b *testing.B) {
	files := Files{
		"index.txt": []byte(`{% for range iterations %}{% _, _ = selector.Select("key") %}{% end %}`),
	}
	buildOptions := &BuildOptions{Globals: native.Declarations{
		"iterations": (*[]int)(nil),
		"selector":   native.Synchronous((*nativeFunctionTrampolineInterface)(nil), "Select"),
	}}
	template, err := BuildTemplate(files, "index.txt", buildOptions)
	if err != nil {
		b.Fatalf("build: %v", err)
	}
	trampoline := native.MakeMethodTrampolineWithFrame(
		reflect.TypeFor[nativeFunctionTrampolineInterface](),
		"Select",
		func(args []reflect.Value) []reflect.Value {
			value, found := args[0].Interface().(nativeFunctionTrampolineInterface).Select(args[1].String())
			return []reflect.Value{reflect.ValueOf(value), reflect.ValueOf(found)}
		},
		func(frame native.FunctionCallFrame) {
			frame.SetResultString(0, frame.ArgString(0))
			frame.SetResultBool(1, true)
		},
	)
	general, err := BuildTemplate(files, "index.txt", buildOptions)
	if err != nil {
		b.Fatalf("build general: %v", err)
	}
	replacements := map[runtime.Operation]runtime.Operation{
		runtime.OpMethodValueFrame:      runtime.OpMethodValue,
		runtime.OpCallNativeFrameMethod: runtime.OpCallIndirect,
	}
	for index := range general.fn.Body {
		if replacement, found := replacements[general.fn.Body[index].Op]; found {
			general.fn.Body[index].Op = replacement
		}
	}
	iterations := make([]int, 128)
	values := map[string]any{
		"iterations": iterations,
		"selector":   nativeFunctionTrampolineInterfaceValue{},
	}
	for _, test := range []struct {
		name     string
		template *Template
	}{
		{name: "general", template: general},
		{name: "authenticated", template: template},
	} {
		b.Run(test.name, func(b *testing.B) {
			options := &RunOptions{NativeFunctionTrampolines: []*native.FunctionTrampoline{trampoline}}
			b.ReportAllocs()
			for range b.N {
				if err := test.template.Run(io.Discard, values, options); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkTemplateNativeFunctionCallFrameTypedArgument(b *testing.B) {
	functionType := reflect.TypeFor[func(map[string]int) int]()
	legacy := func(args []reflect.Value) []reflect.Value {
		return []reflect.Value{reflect.ValueOf(args[0].Interface().(map[string]int)["key"])}
	}
	target := reflect.MakeFunc(functionType, legacy)
	interfaceTrampoline := native.MakeFunctionTrampolineForWithFrame(target, legacy, func(frame native.FunctionCallFrame) {
		value := frame.ArgValue(0).Interface().(map[string]int)["key"]
		frame.SetResultInt(0, int64(value))
	})
	typedTrampoline := native.MakeFunctionTrampolineForWithFrame(target, legacy, func(frame native.FunctionCallFrame) {
		value := native.FunctionCallFrameArg[map[string]int](frame, 0)["key"]
		frame.SetResultInt(0, int64(value))
	})
	template, err := BuildTemplate(
		Files{"index.txt": []byte(`{% for range iterations %}{% _ = consume(items) %}{% end %}`)},
		"index.txt",
		&BuildOptions{Globals: native.Declarations{
			"consume":    native.Synchronous(target.Interface()),
			"items":      (*map[string]int)(nil),
			"iterations": (*[]int)(nil),
		}},
	)
	if err != nil {
		b.Fatalf("build: %v", err)
	}
	const calls = 1024
	values := map[string]any{
		"items":      map[string]int{"key": 42},
		"iterations": make([]int, calls),
	}
	for _, benchmark := range []struct {
		name       string
		trampoline *native.FunctionTrampoline
	}{
		{name: "interface", trampoline: interfaceTrampoline},
		{name: "typed", trampoline: typedTrampoline},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			options := &RunOptions{NativeFunctionTrampolines: []*native.FunctionTrampoline{benchmark.trampoline}}
			b.ReportAllocs()
			b.ReportMetric(calls, "calls/op")
			for range b.N {
				if err := template.Run(io.Discard, values, options); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkTemplateNativeFunctionCallFrameWideArguments(b *testing.B) {
	functionType := reflect.TypeFor[func(string, string, string, string, string, string, string) int]()
	legacy := func(args []reflect.Value) []reflect.Value {
		length := 0
		for _, value := range args {
			length += value.Len()
		}
		return []reflect.Value{reflect.ValueOf(length)}
	}
	target := reflect.MakeFunc(functionType, legacy)
	trampoline := native.MakeFunctionTrampolineForWithFrame(target, legacy, func(frame native.FunctionCallFrame) {
		length := 0
		for index := range functionType.NumIn() {
			length += len(frame.ArgString(index))
		}
		frame.SetResultInt(0, int64(length))
	})
	template, err := BuildTemplate(
		Files{"index.txt": []byte(`{% for range iterations %}{% _ = consume("a", "b", "c", "d", "e", "f", "g") %}{% end %}`)},
		"index.txt",
		&BuildOptions{Globals: native.Declarations{
			"consume":    native.Synchronous(target.Interface()),
			"iterations": (*[]int)(nil),
		}},
	)
	if err != nil {
		b.Fatalf("build: %v", err)
	}
	const calls = 1024
	values := map[string]any{"iterations": make([]int, calls)}
	options := &RunOptions{NativeFunctionTrampolines: []*native.FunctionTrampoline{trampoline}}
	b.ReportAllocs()
	b.ReportMetric(calls, "calls/op")
	for range b.N {
		if err := template.Run(io.Discard, values, options); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTemplateNativeFunctionCallArguments(b *testing.B) {
	functionType := reflect.TypeFor[func(...any) int]()
	legacy := func(args []reflect.Value) []reflect.Value {
		values := args[0].Interface().([]any)
		return []reflect.Value{reflect.ValueOf(len(values))}
	}
	target := reflect.MakeFunc(functionType, legacy)
	sliceTrampoline := native.MakeFunctionTrampolineForWithFrame(target, legacy, func(frame native.FunctionCallFrame) {
		count := frame.VariadicLen()
		values := make([]any, count)
		for index := range count {
			values[index] = frame.VariadicValue(index).Interface()
		}
		nativeFunctionCallArgumentSliceSink = values
		frame.SetResultInt(0, int64(len(values)))
		nativeFunctionCallArgumentSliceSink = nil
	})
	viewTrampoline := native.MakeFunctionTrampolineForWithFrame(target, legacy, func(frame native.FunctionCallFrame) {
		arguments := frame.VariadicArguments()
		count := arguments.Len()
		for index := range count {
			_ = native.FunctionCallArgumentAt[any](arguments, index)
		}
		frame.SetResultInt(0, int64(count))
	})
	template, err := BuildTemplate(
		Files{"index.txt": []byte(`{% for range iterations %}{% _ = consume("namespace", "name") %}{% end %}`)},
		"index.txt",
		&BuildOptions{Globals: native.Declarations{
			"consume":    native.Synchronous(target.Interface()),
			"iterations": (*[]int)(nil),
		}},
	)
	if err != nil {
		b.Fatalf("build: %v", err)
	}
	const calls = 1024
	values := map[string]any{"iterations": make([]int, calls)}
	for _, benchmark := range []struct {
		name       string
		trampoline *native.FunctionTrampoline
	}{
		{name: "slice", trampoline: sliceTrampoline},
		{name: "view", trampoline: viewTrampoline},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			options := &RunOptions{NativeFunctionTrampolines: []*native.FunctionTrampoline{benchmark.trampoline}}
			b.ReportAllocs()
			b.ReportMetric(calls, "calls/op")
			for range b.N {
				if err := template.Run(io.Discard, values, options); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
