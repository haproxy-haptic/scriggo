// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/haproxy-haptic/scriggo/native"
)

var errMutationRejected = errors.New("mutation rejected")

type mutationContextKey struct{}

type mutationNode struct {
	Value int
	Child *mutationNode
}

type mutationNamedMap map[string]string
type mutationNamedSlice []int

type mutationObservation struct {
	Mutation
	Context any
}

type mutationTestCase struct {
	name      string
	source    string
	operation MutationOperation
	globals   native.Declarations
	identity  uintptr
	unchanged func() bool
}

func mutationTargetIdentity(target reflect.Value) uintptr {
	switch target.Kind() {
	case reflect.Chan, reflect.Map, reflect.Pointer, reflect.Slice:
		return target.Pointer()
	default:
		if target.CanAddr() {
			return target.Addr().Pointer()
		}
	}
	return 0
}

func rejectMutation(
	t *testing.T,
	files Files,
	entry string,
	buildOptions *BuildOptions,
	runOptions *RunOptions,
	operation MutationOperation,
	targetIdentity uintptr,
) mutationObservation {
	t.Helper()

	tmpl, err := BuildTemplate(files, entry, buildOptions)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if runOptions == nil {
		runOptions = &RunOptions{}
	}

	var mu sync.Mutex
	var observed Mutation
	var observedContext any
	matched := false
	runOptions.ObserveMutation = func(env native.Env, mutation Mutation) error {
		if mutation.Operation != operation || mutationTargetIdentity(mutation.Target) != targetIdentity {
			return nil
		}
		mu.Lock()
		observed = mutation
		if ctx := env.Context(); ctx != nil {
			observedContext = ctx.Value(mutationContextKey{})
		}
		matched = true
		mu.Unlock()
		return errMutationRejected
	}

	err = tmpl.Run(&strings.Builder{}, nil, runOptions)
	if err != errMutationRejected {
		t.Fatalf("run error = %T %v, want sentinel", err, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !matched {
		t.Fatalf("did not observe %v for target %#x", operation, targetIdentity)
	}
	if observed.Operation != operation {
		t.Fatalf("operation = %v, want %v", observed.Operation, operation)
	}
	if runOptions.Context != nil && observedContext == nil {
		t.Fatal("observer Env lost its execution context")
	}
	return mutationObservation{Mutation: observed, Context: observedContext}
}

func TestMutationObserverSetMapIndex(t *testing.T) {
	t.Run("optimized string map", func(t *testing.T) {
		value := map[string]string{"key": "old"}
		rejectMutation(t, Files{"index.txt": []byte(`{% value["key"] = "new" %}`)}, "index.txt",
			&BuildOptions{Globals: native.Declarations{"value": &value}}, nil,
			MutationSetMapIndex, reflect.ValueOf(value).Pointer())
		if value["key"] != "old" {
			t.Fatalf("map mutated to %q", value["key"])
		}
	})

	t.Run("generic named map", func(t *testing.T) {
		value := mutationNamedMap{"key": "old"}
		rejectMutation(t, Files{"index.txt": []byte(`{% value["key"] = "new" %}`)}, "index.txt",
			&BuildOptions{Globals: native.Declarations{"value": &value}}, nil,
			MutationSetMapIndex, reflect.ValueOf(value).Pointer())
		if value["key"] != "old" {
			t.Fatalf("map mutated to %q", value["key"])
		}
	})

	t.Run("interface held", func(t *testing.T) {
		value := map[string]any{"key": "old"}
		var boxed any = value
		rejectMutation(t, Files{"index.txt": []byte(`{% value := boxed.(map[string]interface{}) %}{% value["key"] = "new" %}`)}, "index.txt",
			&BuildOptions{Globals: native.Declarations{"boxed": &boxed}}, nil,
			MutationSetMapIndex, reflect.ValueOf(value).Pointer())
		if value["key"] != "old" {
			t.Fatalf("map mutated to %v", value["key"])
		}
	})
}

func TestMutationObserverSetIndex(t *testing.T) {
	t.Run("optimized int slice", func(t *testing.T) {
		value := []int{1}
		rejectMutation(t, Files{"index.txt": []byte(`{% value[0] = 2 %}`)}, "index.txt",
			&BuildOptions{Globals: native.Declarations{"value": &value}}, nil,
			MutationSetIndex, reflect.ValueOf(value).Pointer())
		if value[0] != 1 {
			t.Fatalf("slice mutated to %d", value[0])
		}
	})

	t.Run("optimized any slice through interface", func(t *testing.T) {
		value := []any{"old"}
		var boxed any = value
		rejectMutation(t, Files{"index.txt": []byte(`{% value := boxed.([]interface{}) %}{% value[0] = "new" %}`)}, "index.txt",
			&BuildOptions{Globals: native.Declarations{"boxed": &boxed}}, nil,
			MutationSetIndex, reflect.ValueOf(value).Pointer())
		if value[0] != "old" {
			t.Fatalf("slice mutated to %v", value[0])
		}
	})

	t.Run("generic named slice", func(t *testing.T) {
		value := mutationNamedSlice{1}
		rejectMutation(t, Files{"index.txt": []byte(`{% value[0] = 2 %}`)}, "index.txt",
			&BuildOptions{Globals: native.Declarations{"value": &value}}, nil,
			MutationSetIndex, reflect.ValueOf(value).Pointer())
		if value[0] != 1 {
			t.Fatalf("slice mutated to %d", value[0])
		}
	})

}

func TestMutationObserverSetField(t *testing.T) {
	t.Run("addressable struct", func(t *testing.T) {
		value := &mutationNode{Value: 1}
		rejectMutation(t, Files{"index.txt": []byte(`{% value.Value = 2 %}`)}, "index.txt",
			&BuildOptions{Globals: native.Declarations{"value": &value}}, nil,
			MutationSetField, reflect.ValueOf(value).Pointer())
		if value.Value != 1 {
			t.Fatalf("field mutated to %d", value.Value)
		}
	})

	t.Run("nested pointer", func(t *testing.T) {
		child := &mutationNode{Value: 1}
		value := mutationNode{Child: child}
		rejectMutation(t, Files{"index.txt": []byte(`{% value.Child.Value = 2 %}`)}, "index.txt",
			&BuildOptions{Globals: native.Declarations{"value": &value}}, nil,
			MutationSetField, reflect.ValueOf(child).Pointer())
		if child.Value != 1 {
			t.Fatalf("nested field mutated to %d", child.Value)
		}
	})

	t.Run("interface held pointer", func(t *testing.T) {
		value := &mutationNode{Value: 1}
		var boxed any = value
		rejectMutation(t, Files{"index.txt": []byte(`{% value := boxed.(*mutationNode) %}{% value.Value = 2 %}`)}, "index.txt",
			&BuildOptions{Globals: native.Declarations{"boxed": &boxed, "mutationNode": reflect.TypeFor[mutationNode]()}}, nil,
			MutationSetField, reflect.ValueOf(value).Pointer())
		if value.Value != 1 {
			t.Fatalf("interface-held field mutated to %d", value.Value)
		}
	})
}

func TestMutationObserverSetPointer(t *testing.T) {
	intValue := 1
	intPointer := &intValue
	stringValue := "old"
	stringPointer := &stringValue
	boolValue := true
	boolPointer := &boolValue
	floatValue := 1.5
	floatPointer := &floatValue
	generalValue := mutationNode{Value: 1}
	generalPointer := &generalValue
	replacement := mutationNode{Value: 2}

	tests := []mutationTestCase{
		{"int", `{% *pointer = 2 %}`, MutationSetPointer, native.Declarations{"pointer": &intPointer}, reflect.ValueOf(intPointer).Pointer(), func() bool { return intValue == 1 }},
		{"string", `{% *pointer = "new" %}`, MutationSetPointer, native.Declarations{"pointer": &stringPointer}, reflect.ValueOf(stringPointer).Pointer(), func() bool { return stringValue == "old" }},
		{"bool", `{% *pointer = false %}`, MutationSetPointer, native.Declarations{"pointer": &boolPointer}, reflect.ValueOf(boolPointer).Pointer(), func() bool { return boolValue }},
		{"float", `{% *pointer = 2.5 %}`, MutationSetPointer, native.Declarations{"pointer": &floatPointer}, reflect.ValueOf(floatPointer).Pointer(), func() bool { return floatValue == 1.5 }},
		{"general", `{% *pointer = replacement %}`, MutationSetPointer, native.Declarations{"pointer": &generalPointer, "replacement": &replacement}, reflect.ValueOf(generalPointer).Pointer(), func() bool { return generalValue.Value == 1 }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rejectMutation(t, Files{"index.txt": []byte(test.source)}, "index.txt",
				&BuildOptions{Globals: test.globals}, nil, test.operation, test.identity)
			if !test.unchanged() {
				t.Fatal("pointed value changed")
			}
		})
	}

	t.Run("interface held", func(t *testing.T) {
		value := 1
		pointer := &value
		var boxed any = pointer
		rejectMutation(t, Files{"index.txt": []byte(`{% pointer := boxed.(*int) %}{% *pointer = 2 %}`)}, "index.txt",
			&BuildOptions{Globals: native.Declarations{"boxed": &boxed}}, nil,
			MutationSetPointer, reflect.ValueOf(pointer).Pointer())
		if value != 1 {
			t.Fatalf("pointed value changed to %d", value)
		}
	})
}

func TestMutationObserverSetVariable(t *testing.T) {
	value := 1
	rejectMutation(t, Files{"index.txt": []byte(`{% value = 2 %}`)}, "index.txt",
		&BuildOptions{Globals: native.Declarations{"value": &value}}, nil,
		MutationSetVariable, reflect.ValueOf(&value).Pointer())
	if value != 1 {
		t.Fatalf("global mutated to %d", value)
	}

	t.Run("array element persists through global", func(t *testing.T) {
		value := [1]int{1}
		rejectMutation(t, Files{"index.txt": []byte(`{% value[0] = 2 %}`)}, "index.txt",
			&BuildOptions{Globals: native.Declarations{"value": &value}}, nil,
			MutationSetVariable, reflect.ValueOf(&value).Pointer())
		if value[0] != 1 {
			t.Fatalf("array mutated to %d", value[0])
		}
	})
}

func TestMutationObserverRejectsNestedGlobalWrites(t *testing.T) {
	type leaf struct{ Value int }
	type aggregate struct {
		Child leaf
		Items [1]leaf
	}
	for _, source := range []string{
		`{% value.Child.Value = 2 %}`,
		`{% value.Child.Value += 1 %}`,
		`{% value.Items[0].Value = 2 %}`,
		`{% value.Items[0].Value++ %}`,
	} {
		t.Run(source, func(t *testing.T) {
			original := aggregate{Child: leaf{1}, Items: [1]leaf{{1}}}
			value := original
			rejectMutation(t, Files{"index.txt": []byte(source)}, "index.txt",
				&BuildOptions{Globals: native.Declarations{"value": &value}}, nil,
				MutationSetVariable, reflect.ValueOf(&value).Pointer())
			if value != original {
				t.Fatalf("global changed to %#v, want %#v", value, original)
			}
		})
	}
}

func TestMutationObserverBuiltins(t *testing.T) {
	t.Run("delete", func(t *testing.T) {
		value := map[string]int{"key": 1}
		rejectMutation(t, Files{"index.txt": []byte(`{% delete(value, "key") %}`)}, "index.txt",
			&BuildOptions{Globals: native.Declarations{"value": &value}}, nil,
			MutationDelete, reflect.ValueOf(value).Pointer())
		if value["key"] != 1 {
			t.Fatal("delete ran")
		}
	})

	t.Run("clear map", func(t *testing.T) {
		value := map[string]int{"key": 1}
		rejectMutation(t, Files{"index.txt": []byte(`{% clear(value) %}`)}, "index.txt",
			&BuildOptions{Globals: native.Declarations{"value": &value}}, nil,
			MutationClear, reflect.ValueOf(value).Pointer())
		if value["key"] != 1 {
			t.Fatal("clear ran")
		}
	})

	t.Run("clear slice", func(t *testing.T) {
		value := []int{1}
		rejectMutation(t, Files{"index.txt": []byte(`{% clear(value) %}`)}, "index.txt",
			&BuildOptions{Globals: native.Declarations{"value": &value}}, nil,
			MutationClear, reflect.ValueOf(value).Pointer())
		if value[0] != 1 {
			t.Fatal("clear ran")
		}
	})

	t.Run("append elements", func(t *testing.T) {
		value := make([]int, 1, 2)
		value[0] = 1
		rejectMutation(t, Files{"index.txt": []byte(`{% value = append(value, 2) %}`)}, "index.txt",
			&BuildOptions{Globals: native.Declarations{"value": &value}}, nil,
			MutationAppend, reflect.ValueOf(value).Pointer())
		if len(value) != 1 || value[0] != 1 {
			t.Fatalf("append changed slice to %v", value)
		}
	})

	t.Run("append slice", func(t *testing.T) {
		value := make([]int, 1, 3)
		value[0] = 1
		source := []int{2}
		rejectMutation(t, Files{"index.txt": []byte(`{% value = append(value, source...) %}`)}, "index.txt",
			&BuildOptions{Globals: native.Declarations{"value": &value, "source": &source}}, nil,
			MutationAppend, reflect.ValueOf(value).Pointer())
		if len(value) != 1 || value[0] != 1 {
			t.Fatalf("append changed slice to %v", value)
		}
	})

	t.Run("copy", func(t *testing.T) {
		value := []int{1}
		source := []int{2}
		rejectMutation(t, Files{"index.txt": []byte(`{% copy(value, source) %}`)}, "index.txt",
			&BuildOptions{Globals: native.Declarations{"value": &value, "source": &source}}, nil,
			MutationCopy, reflect.ValueOf(value).Pointer())
		if value[0] != 1 {
			t.Fatalf("copy changed slice to %v", value)
		}
	})
}

func TestMutationObserverAppendWithoutBackingStoreWrite(t *testing.T) {
	tests := map[string]string{
		"elements allocate": `{% var result = append(value, 2) %}{{ len(result) }}`,
		"slice allocates":   `{% var result = append(value, source...) %}{{ len(result) }}`,
		"empty append":      `{% var result = append(value, empty...) %}{{ len(result) }}`,
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			value := []int{1}
			appendSource := []int{2}
			empty := []int{}
			tmpl, err := BuildTemplate(Files{"index.txt": []byte(source)}, "index.txt", &BuildOptions{
				Globals: native.Declarations{"value": &value, "source": &appendSource, "empty": &empty},
			})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var operations []MutationOperation
			var out strings.Builder
			err = tmpl.Run(&out, nil, &RunOptions{ObserveMutation: func(_ native.Env, mutation Mutation) error {
				operations = append(operations, mutation.Operation)
				return nil
			}})
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			for _, operation := range operations {
				if operation == MutationAppend {
					t.Fatalf("append was observed without a backing-store write: %v", operations)
				}
			}
			if len(value) != 1 || value[0] != 1 {
				t.Fatalf("source changed to %v", value)
			}
		})
	}
}

func TestMutationObserverCompoundAssignments(t *testing.T) {
	t.Run("map", func(t *testing.T) {
		value := map[string]string{"key": "old"}
		rejectMutation(t, Files{"index.txt": []byte(`{% value["key"] += "new" %}`)}, "index.txt",
			&BuildOptions{Globals: native.Declarations{"value": &value}}, nil,
			MutationSetMapIndex, reflect.ValueOf(value).Pointer())
		if value["key"] != "old" {
			t.Fatalf("map changed to %q", value["key"])
		}
	})

	t.Run("slice increment", func(t *testing.T) {
		value := []int{1}
		rejectMutation(t, Files{"index.txt": []byte(`{% value[0]++ %}`)}, "index.txt",
			&BuildOptions{Globals: native.Declarations{"value": &value}}, nil,
			MutationSetIndex, reflect.ValueOf(value).Pointer())
		if value[0] != 1 {
			t.Fatalf("slice changed to %v", value)
		}
	})

	t.Run("field increment", func(t *testing.T) {
		value := &mutationNode{Value: 1}
		rejectMutation(t, Files{"index.txt": []byte(`{% value.Value++ %}`)}, "index.txt",
			&BuildOptions{Globals: native.Declarations{"value": &value}}, nil,
			MutationSetField, reflect.ValueOf(value).Pointer())
		if value.Value != 1 {
			t.Fatalf("field changed to %d", value.Value)
		}
	})

	t.Run("pointer increment", func(t *testing.T) {
		value := 1
		pointer := &value
		rejectMutation(t, Files{"index.txt": []byte(`{% (*pointer)++ %}`)}, "index.txt",
			&BuildOptions{Globals: native.Declarations{"pointer": &pointer}}, nil,
			MutationSetPointer, reflect.ValueOf(pointer).Pointer())
		if value != 1 {
			t.Fatalf("pointed value changed to %d", value)
		}
	})

	t.Run("global increment", func(t *testing.T) {
		value := 1
		rejectMutation(t, Files{"index.txt": []byte(`{% value++ %}`)}, "index.txt",
			&BuildOptions{Globals: native.Declarations{"value": &value}}, nil,
			MutationSetVariable, reflect.ValueOf(&value).Pointer())
		if value != 1 {
			t.Fatalf("global changed to %d", value)
		}
	})
}

func TestMutationObserverDeferredBuiltins(t *testing.T) {
	dst, src := []int{1}, []int{2}
	deleted := map[string]int{"key": 1}
	cleared := []int{1}
	tests := []mutationTestCase{
		{"copy", `{% defer copy(dst, src) %}`, MutationCopy, native.Declarations{"dst": &dst, "src": &src}, reflect.ValueOf(dst).Pointer(), func() bool { return dst[0] == 1 }},
		{"delete", `{% defer delete(value, "key") %}`, MutationDelete, native.Declarations{"value": &deleted}, reflect.ValueOf(deleted).Pointer(), func() bool { return deleted["key"] == 1 }},
		{"clear", `{% defer clear(value) %}`, MutationClear, native.Declarations{"value": &cleared}, reflect.ValueOf(cleared).Pointer(), func() bool { return cleared[0] == 1 }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rejectMutation(t, Files{"index.txt": []byte(test.source)}, "index.txt",
				&BuildOptions{Globals: test.globals}, nil, test.operation, test.identity)
			if !test.unchanged() {
				t.Fatal("deferred builtin mutated target")
			}
		})
	}
}

func TestMutationObserverChildExecutions(t *testing.T) {
	root := context.WithValue(context.Background(), mutationContextKey{}, "root")
	childOptions := func() *RunOptions {
		return &RunOptions{
			Context: root,
			ChildContext: func(parent context.Context) context.Context {
				value, _ := parent.Value(mutationContextKey{}).(string)
				return context.WithValue(parent, mutationContextKey{}, value+"/child")
			},
		}
	}

	t.Run("native callback", func(t *testing.T) {
		value := map[string]string{"key": "old"}
		invoke := func(fn func()) { fn() }
		observed := rejectMutation(t,
			Files{"index.txt": []byte(`{% invoke(func() { value["key"] = "new" }) %}`)}, "index.txt",
			&BuildOptions{Globals: native.Declarations{"invoke": invoke, "value": &value}}, childOptions(),
			MutationSetMapIndex, reflect.ValueOf(value).Pointer())
		if observed.Target.Pointer() != reflect.ValueOf(value).Pointer() || value["key"] != "old" {
			t.Fatal("callback mutation was not stopped")
		}
		if observed.Context != "root/child" {
			t.Fatalf("observer context = %v, want root/child", observed.Context)
		}
	})

	t.Run("nested macro", func(t *testing.T) {
		value := map[string]string{"key": "old"}
		observed := rejectMutation(t,
			Files{"index.txt": []byte(`{% macro mutate %}{% value["key"] = "new" %}{% end %}{{ mutate() }}`)}, "index.txt",
			&BuildOptions{Globals: native.Declarations{"value": &value}}, childOptions(),
			MutationSetMapIndex, reflect.ValueOf(value).Pointer())
		if value["key"] != "old" {
			t.Fatal("macro mutation was not stopped")
		}
		if observed.Context != "root" {
			t.Fatalf("observer context = %v, want root", observed.Context)
		}
	})

	t.Run("parallel render", func(t *testing.T) {
		value := map[string]string{"key": "old"}
		observed := rejectMutation(t, Files{
			"index.txt": []byte(`{{ go render "child.txt" }}`),
			"child.txt": []byte(`{% value["key"] = "new" %}`),
		}, "index.txt", &BuildOptions{Globals: native.Declarations{"value": &value}}, childOptions(),
			MutationSetMapIndex, reflect.ValueOf(value).Pointer())
		if value["key"] != "old" {
			t.Fatal("parallel mutation was not stopped")
		}
		if observed.Context != "root/child" || observed.Path != "child.txt" {
			t.Fatalf("observer context/path = %v/%q, want root/child and child.txt", observed.Context, observed.Path)
		}
	})

	t.Run("nested parallel render", func(t *testing.T) {
		value := map[string]string{"key": "old"}
		observed := rejectMutation(t, Files{
			"index.txt":  []byte(`{{ go render "parent.txt" }}`),
			"parent.txt": []byte(`{{ go render "child.txt" }}`),
			"child.txt":  []byte(`{% value["key"] = "new" %}`),
		}, "index.txt", &BuildOptions{Globals: native.Declarations{"value": &value}}, childOptions(),
			MutationSetMapIndex, reflect.ValueOf(value).Pointer())
		if value["key"] != "old" {
			t.Fatal("nested parallel mutation was not stopped")
		}
		if observed.Context != "root/child/child" || observed.Path != "child.txt" {
			t.Fatalf("observer context/path = %v/%q, want root/child/child and child.txt", observed.Context, observed.Path)
		}
	})

	t.Run("go copy", func(t *testing.T) {
		dst, src := []int{1}, []int{2}
		observed := rejectMutation(t, Files{"index.txt": []byte(`{% go copy(dst, src) %}`)}, "index.txt",
			&BuildOptions{AllowGoStmt: true, Globals: native.Declarations{"dst": &dst, "src": &src}}, childOptions(),
			MutationCopy, reflect.ValueOf(dst).Pointer())
		if dst[0] != 1 {
			t.Fatal("go copy was not stopped")
		}
		if observed.Context != "root/child" {
			t.Fatalf("observer context = %v, want root/child", observed.Context)
		}
	})

	t.Run("go delete", func(t *testing.T) {
		value := map[string]int{"key": 1}
		observed := rejectMutation(t, Files{"index.txt": []byte(`{% go delete(value, "key") %}`)}, "index.txt",
			&BuildOptions{AllowGoStmt: true, Globals: native.Declarations{"value": &value}}, childOptions(),
			MutationDelete, reflect.ValueOf(value).Pointer())
		if value["key"] != 1 || observed.Context != "root/child" {
			t.Fatalf("go delete result/context = %v/%v", value, observed.Context)
		}
	})

	t.Run("go clear", func(t *testing.T) {
		value := []int{1}
		observed := rejectMutation(t, Files{"index.txt": []byte(`{% go clear(value) %}`)}, "index.txt",
			&BuildOptions{AllowGoStmt: true, Globals: native.Declarations{"value": &value}}, childOptions(),
			MutationClear, reflect.ValueOf(value).Pointer())
		if value[0] != 1 || observed.Context != "root/child" {
			t.Fatalf("go clear result/context = %v/%v", value, observed.Context)
		}
	})
}

func TestMutationObserverRejectsDetachedScriggoGo(t *testing.T) {
	value := map[string]string{"key": "old"}
	started := make(chan struct{}, 1)
	start := func() { started <- struct{}{} }
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{% go func() { start(); value["key"] = "new" }() %}`),
	}, "index.txt", &BuildOptions{AllowGoStmt: true, Globals: native.Declarations{
		"start": start,
		"value": &value,
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	err = tmpl.Run(&strings.Builder{}, nil, &RunOptions{
		ObserveMutation: func(native.Env, Mutation) error { return nil },
	})
	if err != ErrMutationObserverDetachedGo {
		t.Fatalf("run error = %T %v, want %v", err, err, ErrMutationObserverDetachedGo)
	}
	if value["key"] != "old" {
		t.Fatalf("detached child mutated value to %q", value["key"])
	}
	select {
	case <-started:
		t.Fatal("detached child started")
	default:
	}
}

func TestMutationObserverRejectsNamedDetachedScriggoGo(t *testing.T) {
	value := map[string]string{"key": "old"}
	started := make(chan struct{}, 1)
	start := func() { started <- struct{}{} }
	program, err := Build(fstest.MapFS{
		"main.go": {Data: []byte(`package main
import "guard"
func mutate() { guard.Start(); guard.Value["key"] = "new" }
func main() { go mutate() }
`)},
	}, &BuildOptions{AllowGoStmt: true, Packages: native.Packages{
		"guard": native.Package{Name: "guard", Declarations: native.Declarations{
			"Start": start,
			"Value": &value,
		}},
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	err = program.Run(&RunOptions{ObserveMutation: func(native.Env, Mutation) error { return nil }})
	if err != ErrMutationObserverDetachedGo {
		t.Fatalf("run error = %T %v, want %v", err, err, ErrMutationObserverDetachedGo)
	}
	if value["key"] != "old" {
		t.Fatalf("detached child mutated value to %q", value["key"])
	}
	select {
	case <-started:
		t.Fatal("detached child started")
	default:
	}
}

func TestMutationObserverRejectsChannelOperations(t *testing.T) {
	run := func(t *testing.T, source string, globals native.Declarations, buildOptions *BuildOptions) []Mutation {
		t.Helper()
		if buildOptions == nil {
			buildOptions = &BuildOptions{}
		}
		buildOptions.Globals = globals
		tmpl, err := BuildTemplate(Files{"index.txt": []byte(source)}, "index.txt", buildOptions)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		var observed []Mutation
		err = tmpl.Run(&strings.Builder{}, nil, &RunOptions{ObserveMutation: func(_ native.Env, mutation Mutation) error {
			observed = append(observed, mutation)
			return nil
		}})
		if err != ErrMutationObserverChannelOperation {
			t.Fatalf("run error = %T %v, want %v", err, err, ErrMutationObserverChannelOperation)
		}
		return observed
	}

	t.Run("close", func(t *testing.T) {
		ch := make(chan int, 1)
		observed := run(t, `{% close(ch) %}`, native.Declarations{"ch": &ch}, nil)
		if len(observed) != 1 || observed[0].Operation != MutationCloseChannel || mutationTargetIdentity(observed[0].Target) != reflect.ValueOf(ch).Pointer() {
			t.Fatalf("observed = %#v, want channel close", observed)
		}
		close(ch)
	})

	t.Run("send", func(t *testing.T) {
		ch := make(chan int, 1)
		observed := run(t, `{% ch <- 1 %}`, native.Declarations{"ch": &ch}, nil)
		if len(observed) != 1 || observed[0].Operation != MutationSendChannel || mutationTargetIdentity(observed[0].Target) != reflect.ValueOf(ch).Pointer() {
			t.Fatalf("observed = %#v, want channel send", observed)
		}
		if len(ch) != 0 {
			t.Fatal("channel send ran")
		}
	})

	t.Run("receive", func(t *testing.T) {
		ch := make(chan int, 1)
		ch <- 1
		observed := run(t, `{% _ = <-ch %}`, native.Declarations{"ch": &ch}, nil)
		if len(observed) != 1 || observed[0].Operation != MutationReceiveChannel || mutationTargetIdentity(observed[0].Target) != reflect.ValueOf(ch).Pointer() {
			t.Fatalf("observed = %#v, want channel receive", observed)
		}
		if len(ch) != 1 {
			t.Fatal("channel receive ran")
		}
	})

	for _, rangeCase := range []struct {
		name   string
		boxed  bool
		source string
	}{
		{name: "range", source: `{% for value in ch %}{% _ = value %}{% end %}`},
		{name: "range interface held", boxed: true, source: `{% for value in ch %}{% _ = value %}{% end %}`},
	} {
		t.Run(rangeCase.name, func(t *testing.T) {
			ch := make(chan int, 1)
			ch <- 1
			close(ch)
			boxed := any(ch)
			var declaration any = &ch
			if rangeCase.boxed {
				declaration = &boxed
			}
			observed := run(t, rangeCase.source, native.Declarations{"ch": declaration}, nil)
			if len(observed) != 1 || observed[0].Operation != MutationReceiveChannel || mutationTargetIdentity(observed[0].Target) != reflect.ValueOf(ch).Pointer() {
				t.Fatalf("observed = %#v, want ranged channel receive", observed)
			}
			if len(ch) != 1 {
				t.Fatal("channel range received a value")
			}
		})
	}

	t.Run("select preflights every channel", func(t *testing.T) {
		receive := make(chan int, 1)
		receive <- 1
		send := make(chan int, 1)
		observed := run(t,
			`{% select %}{% case <-receive %}{% case send <- 2 %}{% default %}{% end %}`,
			native.Declarations{"receive": &receive, "send": &send}, nil)
		if len(observed) != 2 {
			t.Fatalf("observed %d candidates, want 2", len(observed))
		}
		want := map[uintptr]MutationOperation{
			reflect.ValueOf(receive).Pointer(): MutationReceiveChannel,
			reflect.ValueOf(send).Pointer():    MutationSendChannel,
		}
		for _, mutation := range observed {
			identity := mutationTargetIdentity(mutation.Target)
			if mutation.Operation != want[identity] {
				t.Fatalf("operation for channel %#x = %v, want %v", identity, mutation.Operation, want[identity])
			}
			delete(want, identity)
		}
		if len(want) != 0 || len(receive) != 1 || len(send) != 0 {
			t.Fatalf("select changed channels or skipped candidates: remaining=%v receive=%d send=%d", want, len(receive), len(send))
		}
	})

	t.Run("empty select", func(t *testing.T) {
		observed := run(t, `{% select %}{% end %}`, nil, nil)
		if len(observed) != 1 || observed[0].Operation != MutationSelectChannel || observed[0].Target.IsValid() {
			t.Fatalf("observed = %#v, want empty select", observed)
		}
	})

	for _, statement := range []struct {
		name  string
		src   string
		allow bool
	}{
		{name: "defer close", src: `{% defer close(ch) %}`},
		{name: "go close", src: `{% go close(ch) %}`, allow: true},
	} {
		t.Run(statement.name, func(t *testing.T) {
			ch := make(chan int)
			observed := run(t, statement.src, native.Declarations{"ch": &ch}, &BuildOptions{AllowGoStmt: statement.allow})
			if len(observed) != 1 || observed[0].Operation != MutationCloseChannel {
				t.Fatalf("observed = %#v, want channel close", observed)
			}
			close(ch)
		})
	}
}

func TestMutationObserverNativeBoundary(t *testing.T) {
	value := map[string]string{"key": "old"}
	mutate := func(target map[string]string) { target["key"] = "new" }
	tmpl, err := BuildTemplate(Files{"index.txt": []byte(`{% mutate(value) %}`)}, "index.txt", &BuildOptions{
		Globals: native.Declarations{"mutate": mutate, "value": &value},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	called := false
	err = tmpl.Run(&strings.Builder{}, nil, &RunOptions{ObserveMutation: func(native.Env, Mutation) error {
		called = true
		return fmt.Errorf("unexpected observer call")
	}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if called {
		t.Fatal("native mutation was reported as a template mutation")
	}
	if value["key"] != "new" {
		t.Fatal("native boundary test did not mutate its argument")
	}
}

func TestProgramMutationObserver(t *testing.T) {
	program, err := Build(fstest.MapFS{
		"main.go": {Data: []byte("package main\nvar value map[string]int\nfunc main() { value = make(map[string]int); value[\"key\"] = 1 }\n")},
	}, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	called := false
	err = program.Run(&RunOptions{ObserveMutation: func(_ native.Env, mutation Mutation) error {
		if mutation.Operation == MutationSetMapIndex {
			called = true
			return errMutationRejected
		}
		return nil
	}})
	if err != errMutationRejected {
		t.Fatalf("run error = %T %v, want sentinel", err, err)
	}
	if !called {
		t.Fatal("program mutation was not observed")
	}
}
