// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"errors"
	"io"
	"reflect"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/haproxy-haptic/scriggo/native"
)

type fatalTestString string

type fatalTestUnformattable struct{}

func (*fatalTestUnformattable) Error() string { panic("must not format the fatal value") }

func TestEnvFatalPreservesValue(t *testing.T) {
	values := []struct {
		name  string
		value any
	}{
		{"string", "some error occurred"},
		{"named string", fatalTestString("fatal")},
		{"integer", 42},
		{"error identity", errors.New("fatal")},
		{"pointer identity", &struct{ N int }{42}},
		{"slice identity", []int{42}},
		{"map identity", map[string]int{"n": 42}},
		{"typed nil", (*int)(nil)},
		{"nil", nil},
		{"unformattable", &fatalTestUnformattable{}},
	}
	paths := []struct {
		name string
		body string
	}{
		{"direct", `{% fatal() %}`},
		{"indirect", `{% var f = fatal %}{% f() %}`},
		{"callback", `{% invoke(func() { defer touched(); fatal() }) %}`},
		{"deferred native", `{% defer fatal() %}`},
		{"deferred callback", `{% defer func() { fatal() }() %}`},
		{"parallel render", `{{ go render "child.txt" }}`},
		{"nested parallel render", `{{ go render "parent.txt" }}`},
		{"parallel native", `{{ go fatal() }}`},
		{"parallel native callback", `{{ go invokeString(func() string { defer touched(); return fatal() }) }}`},
	}
	for _, path := range paths {
		for _, value := range values {
			t.Run(path.name+"/"+value.name, func(t *testing.T) {
				var touched atomic.Bool
				globals := native.Declarations{
					"fatal":        func(env native.Env) string { env.Fatal(value.value); return "" },
					"touched":      func() { touched.Store(true) },
					"invoke":       func(fn func()) { fn() },
					"invokeString": func(fn func() string) string { return fn() },
				}
				tmpl, err := BuildTemplate(Files{
					"index.txt":  []byte(`{% defer func() { recover(); touched() }() %}` + path.body),
					"child.txt":  []byte(`{% defer touched() %}{% fatal() %}`),
					"parent.txt": []byte(`{{ go render "child.txt" }}`),
				}, "index.txt", &BuildOptions{Globals: globals})
				if err != nil {
					t.Fatalf("build: %v", err)
				}
				assertFatalValue(t, value.value, func() error { return tmpl.Run(io.Discard, nil, nil) })
				if touched.Load() {
					t.Fatal("fatal execution ran a deferred function")
				}
			})
		}
	}
}

func TestProgramEnvFatalSkipsDefers(t *testing.T) {
	want := errors.New("program fatal")
	var touched bool
	program, err := Build(Files{"main.go": []byte(`package main
import "fataltest"
func main() {
	defer func() { recover(); fataltest.Touch() }()
	fataltest.Fatal()
	fataltest.Touch()
}`)}, &BuildOptions{Packages: native.Packages{
		"fataltest": native.Package{Name: "fataltest", Declarations: native.Declarations{
			"Fatal": func(env native.Env) { env.Fatal(want) },
			"Touch": func() { touched = true },
		}},
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	assertFatalValue(t, want, func() error { return program.Run(nil) })
	if touched {
		t.Fatal("fatal execution continued or ran a deferred function")
	}
}

func assertFatalValue(t *testing.T, want any, run func() error) {
	t.Helper()
	returned := false
	var got any
	func() {
		defer func() { got = recover() }()
		err := run()
		returned = true
		t.Errorf("run returned %v instead of panicking", err)
	}()
	if returned {
		return
	}
	if want == nil {
		if _, ok := got.(*runtime.PanicNilError); !ok {
			t.Errorf("recovered %T, want *runtime.PanicNilError", got)
		}
		return
	}
	if reflect.TypeOf(got) != reflect.TypeOf(want) {
		t.Errorf("recovered %T, want %T", got, want)
		return
	}
	wantValue, gotValue := reflect.ValueOf(want), reflect.ValueOf(got)
	switch wantValue.Kind() {
	case reflect.Map, reflect.Slice:
		if wantValue.Pointer() != gotValue.Pointer() {
			t.Error("fatal value lost its identity")
		}
	default:
		if got != want {
			t.Error("fatal value changed")
		}
	}
}
