// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/haproxy-haptic/scriggo/native"
)

var errNativeCallRejected = errors.New("native call rejected")

type nativeCallValue struct {
	Value string
}

func (value *nativeCallValue) Mutate(replacement string) {
	value.Value = replacement
}

func (*nativeCallValue) Render(text string) string {
	return text
}

type nativeCallMutator interface {
	Mutate(string)
}

type nativeMethodTest struct {
	name         string
	source       string
	declarations func(*nativeCallValue) native.Declarations
	methodValue  bool
}

func TestNativeCallObserverRejectsMethodReceivers(t *testing.T) {
	tests := []nativeMethodTest{
		{
			name:   "direct concrete method",
			source: `{% value.Mutate("changed") %}`,
			declarations: func(value *nativeCallValue) native.Declarations {
				return native.Declarations{"value": &value}
			},
		},
		{
			name:   "bound method value",
			source: `{% var mutate = value.Mutate %}{% mutate("changed") %}`,
			declarations: func(value *nativeCallValue) native.Declarations {
				return native.Declarations{"value": &value}
			},
			methodValue: true,
		},
		{
			name:   "interface method",
			source: `{% target.Mutate("changed") %}`,
			declarations: func(value *nativeCallValue) native.Declarations {
				var target nativeCallMutator = value
				return native.Declarations{"target": &target}
			},
			methodValue: true,
		},
		{
			name:   "concrete method expression",
			source: `{% (*Value).Mutate(value, "changed") %}`,
			declarations: func(value *nativeCallValue) native.Declarations {
				return native.Declarations{
					"Value": reflect.TypeFor[nativeCallValue](),
					"value": &value,
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { runRejectedNativeMethod(t, test) })
	}
}

func runRejectedNativeMethod(t *testing.T, test nativeMethodTest) {
	t.Helper()
	value := &nativeCallValue{Value: "original"}
	tmpl, err := BuildTemplate(
		Files{"index.txt": []byte(test.source)},
		"index.txt",
		&BuildOptions{Globals: test.declarations(value)},
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	called := false
	err = tmpl.Run(&strings.Builder{}, nil, &RunOptions{
		ObserveNativeCall: func(_ native.Env, call NativeCall) error {
			called = true
			assertNativeMethod(t, call, value, test.methodValue)
			return errNativeCallRejected
		},
	})
	if err != errNativeCallRejected {
		t.Fatalf("run error = %T %v, want sentinel", err, err)
	}
	if !called {
		t.Fatal("native call was not observed")
	}
	if value.Value != "original" {
		t.Fatalf("value = %q, want original", value.Value)
	}
}

func assertNativeMethod(t *testing.T, call NativeCall, value *nativeCallValue, methodValue bool) {
	t.Helper()
	if mutationTargetIdentity(call.Receiver) != reflect.ValueOf(value).Pointer() {
		t.Fatalf("receiver = %v, want %p", call.Receiver, value)
	}
	if call.Method != "Mutate" {
		t.Fatalf("method = %q, want Mutate", call.Method)
	}
	if call.MethodValue != methodValue {
		t.Fatalf("method value = %v, want %v", call.MethodValue, methodValue)
	}
	if call.Path != "index.txt" || call.Line != 1 {
		t.Fatalf("location = %s:%d, want index.txt:1", call.Path, call.Line)
	}
}

type nativeMethodCallbackTest struct {
	name         string
	source       string
	invoke       any
	methodValue  bool
	invokeBefore bool
}

func TestNativeCallObserverRejectsMethodValueBeforeNativeCallback(t *testing.T) {
	tests := []nativeMethodCallbackTest{
		{
			name:        "bound method value",
			source:      `{% invoke(value.Mutate, "changed") %}`,
			invoke:      func(method func(string), replacement string) { method(replacement) },
			methodValue: true,
		},
		{
			name:   "method expression",
			source: `{% invoke((*Value).Mutate, value, "changed") %}`,
			invoke: func(method func(*nativeCallValue, string), value *nativeCallValue, replacement string) {
				method(value, replacement)
			},
			invokeBefore: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { runRejectedNativeMethodCallback(t, test) })
	}
}

func runRejectedNativeMethodCallback(t *testing.T, test nativeMethodCallbackTest) {
	t.Helper()
	value := &nativeCallValue{Value: "original"}
	invocations := 0
	invoke := reflect.MakeFunc(reflect.TypeOf(test.invoke), func(args []reflect.Value) []reflect.Value {
		invocations++
		reflect.ValueOf(test.invoke).Call(args)
		return nil
	}).Interface()
	tmpl, err := BuildTemplate(
		Files{"index.txt": []byte(test.source)},
		"index.txt",
		&BuildOptions{Globals: native.Declarations{
			"Value":  reflect.TypeFor[nativeCallValue](),
			"invoke": invoke,
			"value":  &value,
		}},
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	err = tmpl.Run(&strings.Builder{}, nil, &RunOptions{
		ObserveNativeCall: func(_ native.Env, call NativeCall) error {
			if call.MethodValue != test.methodValue || call.Method != "Mutate" {
				t.Fatalf("call = %#v, want Mutate method value %v", call, test.methodValue)
			}
			return errNativeCallRejected
		},
	})
	if err != errNativeCallRejected {
		t.Fatalf("run error = %T %v, want sentinel", err, err)
	}
	wantInvocations := 0
	if test.invokeBefore {
		wantInvocations = 1
	}
	if invocations != wantInvocations {
		t.Fatalf("native callback invocations = %d, want %d", invocations, wantInvocations)
	}
	if value.Value != "original" {
		t.Fatalf("value = %q, want original", value.Value)
	}
}

func TestNativeCallObserverLeavesFunctionsAndAllowedMethodsUsable(t *testing.T) {
	value := &nativeCallValue{Value: "original"}
	pure := func(input string) string { return "pure:" + input }
	tmpl, err := BuildTemplate(
		Files{"index.txt": []byte(`{{ pure(value.Value) }}{% value.Mutate("changed") %}`)},
		"index.txt",
		&BuildOptions{Globals: native.Declarations{"pure": pure, "value": &value}},
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var observed []NativeCall
	var out strings.Builder
	err = tmpl.Run(&out, nil, &RunOptions{
		ObserveNativeCall: func(_ native.Env, call NativeCall) error {
			observed = append(observed, call)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.String() != "pure:original" {
		t.Fatalf("output = %q, want pure:original", out.String())
	}
	if len(observed) != 1 || observed[0].Method != "Mutate" {
		t.Fatalf("observed = %#v, want one Mutate call", observed)
	}
	if value.Value != "changed" {
		t.Fatalf("value = %q, want changed", value.Value)
	}
}

func TestNativeCallObserverParallelBoundMethodValue(t *testing.T) {
	value := &nativeCallValue{}
	invoke := func(method func(string) string, text string) string { return method(text) }
	tmpl, err := BuildTemplate(
		Files{"index.txt": []byte(`{% var method = value.Render %}{{ go invoke(method, "a") }}{{ go invoke(method, "b") }}`)},
		"index.txt",
		&BuildOptions{Globals: native.Declarations{"invoke": invoke, "value": &value}},
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var out strings.Builder
	err = tmpl.Run(&out, nil, &RunOptions{
		ObserveNativeCall: func(_ native.Env, call NativeCall) error {
			if call.Method != "Render" {
				t.Fatalf("method = %q, want Render", call.Method)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.String() != "ab" {
		t.Fatalf("output = %q, want ab", out.String())
	}
}
