// Copyright 2019 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"bytes"
	"fmt"
	"io"
	"reflect"
	"testing"

	"github.com/open2b/scriggo/ast"
	"github.com/open2b/scriggo/internal/compiler"
	"github.com/open2b/scriggo/internal/fstest"
)

func TestInitGlobals(t *testing.T) {

	// Test no globals.
	globals := initGlobalVariables([]compiler.Global{}, nil)
	if globals != nil {
		t.Fatalf("expected nil, got %v", globals)
	}

	// Test zero value.
	global := compiler.Global{
		Pkg:  "p",
		Name: "a",
		Type: reflect.TypeOf(0),
	}
	globals = initGlobalVariables([]compiler.Global{global}, nil)
	g := globals[0]
	if g.Kind() != reflect.Int {
		t.Fatalf("unexpected kind %v", g.Kind())
	}
	if g.Interface() != 0 {
		t.Fatalf("unexpected %v, expecting 0", g.Interface())
	}

	// Test pointer value in globals.
	n := 1
	global = compiler.Global{
		Pkg:   "p",
		Name:  "a",
		Type:  reflect.TypeOf(n),
		Value: reflect.ValueOf(&n).Elem(),
	}
	globals = initGlobalVariables([]compiler.Global{global}, nil)
	n = 2
	g = globals[0]
	if g.Kind() != reflect.Int {
		t.Fatalf("unexpected kind %v", g.Kind())
	}
	iface := g.Interface()
	if iface != n {
		t.Fatalf("unexpected %v (type %T), expecting %d (type %T)", iface, iface, n, n)
	}

	// Test pointer value in init.
	global = compiler.Global{
		Pkg:  "main",
		Name: "a",
		Type: reflect.TypeOf(n),
	}
	init := map[string]interface{}{"a": &n}
	globals = initGlobalVariables([]compiler.Global{global}, init)
	if globals == nil {
		t.Fatalf("unexpected %v, expecting nil", globals)
	}
	n = 3
	g = globals[0]
	if g.Kind() != reflect.Int {
		t.Fatalf("unexpected kind %v", g.Kind())
	}
	iface = g.Interface()
	if iface != n {
		t.Fatalf("unexpected %v (type %T), expecting %d (type %T)", iface, iface, n, n)
	}

	// Test non pointer value in init.
	global = compiler.Global{
		Pkg:  "main",
		Name: "a",
		Type: reflect.TypeOf(n),
	}
	init = map[string]interface{}{"a": n}
	globals = initGlobalVariables([]compiler.Global{global}, init)
	if globals == nil {
		t.Fatalf("unexpected %v, expecting nil", globals)
	}
	g = globals[0]
	if g.Kind() != reflect.Int {
		t.Fatalf("unexpected kind %v", g.Kind())
	}
	iface = g.Interface()
	if iface != n {
		t.Fatalf("unexpected %v (type %T), expecting %d (type %T)", iface, iface, n, n)
	}

}

func recoverInitGlobalsPanic(t *testing.T, expected string) {
	got := recover()
	if got == nil {
		t.Fatalf("expecting panic")
	}
	if _, ok := got.(string); !ok {
		panic(got)
	}
	if got.(string) != expected {
		t.Fatalf("unexpected panic %q, expecting panic %q", got, expected)
	}
}

func TestInitGlobalsAlreadyInitializedError(t *testing.T) {
	defer recoverInitGlobalsPanic(t, "variable \"a\" already initialized")
	n := 2
	global := compiler.Global{
		Pkg:   "main",
		Name:  "a",
		Type:  reflect.TypeOf(n),
		Value: reflect.ValueOf(&n).Elem(),
	}
	init := map[string]interface{}{"a": 5}
	_ = initGlobalVariables([]compiler.Global{global}, init)
}

func TestInitGlobalsNilError(t *testing.T) {
	defer recoverInitGlobalsPanic(t, "variable initializer \"a\" cannot be nil")
	global := compiler.Global{
		Pkg:  "main",
		Name: "a",
		Type: reflect.TypeOf(0),
	}
	init := map[string]interface{}{"a": nil}
	_ = initGlobalVariables([]compiler.Global{global}, init)
}

func TestInitGlobalsInvalidTypeError(t *testing.T) {
	defer recoverInitGlobalsPanic(t, "variable initializer \"a\" must have type assignable to int, but have bool")
	global := compiler.Global{
		Pkg:  "main",
		Name: "a",
		Type: reflect.TypeOf(0),
	}
	init := map[string]interface{}{"a": true}
	_ = initGlobalVariables([]compiler.Global{global}, init)
}

func TestInitGlobalsNilPointerError(t *testing.T) {
	defer recoverInitGlobalsPanic(t, "variable initializer \"a\" cannot be a nil pointer")
	global := compiler.Global{
		Pkg:  "main",
		Name: "a",
		Type: reflect.TypeOf(0),
	}
	init := map[string]interface{}{"a": (*int)(nil)}
	_ = initGlobalVariables([]compiler.Global{global}, init)
}

// TestInitGlobalsInterfaceWithConcreteType tests that a concrete type
// implementing an interface is accepted when the global is declared with
// an interface type.
func TestInitGlobalsInterfaceWithConcreteType(t *testing.T) {
	var iw io.Writer
	global := compiler.Global{
		Pkg:  "main",
		Name: "writer",
		Type: reflect.TypeOf(&iw).Elem(), // interface type
	}

	// Pass a concrete *bytes.Buffer which implements io.Writer
	buf := &bytes.Buffer{}
	init := map[string]interface{}{"writer": buf}

	values := initGlobalVariables([]compiler.Global{global}, init)
	if values[0].IsNil() {
		t.Fatal("expected non-nil value")
	}
	// Verify the value is usable as io.Writer
	w, ok := values[0].Interface().(io.Writer)
	if !ok {
		t.Fatal("value should be usable as io.Writer")
	}
	// Verify it's the same underlying buffer
	if _, err := w.Write([]byte("test")); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "test" {
		t.Errorf("expected 'test', got %q", buf.String())
	}
}

// TestInitGlobalsInterfaceWithPointerToConcreteType tests that a pointer to
// a concrete type implementing an interface is accepted and properly dereferenced.
// This handles the case where someone passes **T when *T implements the interface.
func TestInitGlobalsInterfaceWithPointerToConcreteType(t *testing.T) {
	var iw io.Writer
	global := compiler.Global{
		Pkg:  "main",
		Name: "writer",
		Type: reflect.TypeOf(&iw).Elem(), // interface type
	}

	// buf is *bytes.Buffer, &buf is **bytes.Buffer
	buf := &bytes.Buffer{}
	init := map[string]interface{}{"writer": &buf}

	values := initGlobalVariables([]compiler.Global{global}, init)
	if values[0].IsNil() {
		t.Fatal("expected non-nil value")
	}
	// Verify the value is usable as io.Writer
	w, ok := values[0].Interface().(io.Writer)
	if !ok {
		t.Fatal("value should be usable as io.Writer")
	}
	// Verify it's the same underlying buffer (was properly dereferenced)
	if _, err := w.Write([]byte("test")); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "test" {
		t.Errorf("expected 'test', got %q", buf.String())
	}
}

type testFormatFS struct {
	fstest.Files
	format Format
}

func (fsys testFormatFS) Format(_ string) (Format, error) {
	return fsys.format, nil
}

// TestFormatFS tests that BuildTemplate gets the file format by calling the
// Format method of FormatFS.
func TestFormatFS(t *testing.T) {
	fsys := testFormatFS{Files: fstest.Files{"index": ""}}
	for _, format := range []Format{FormatText, FormatHTML, FormatCSS, FormatJS, FormatJSON, FormatMarkdown} {
		fsys.format = format
		options := BuildOptions{
			TreeTransformer: func(tree *ast.Tree) error {
				if tree.Format != ast.Format(format) {
					return fmt.Errorf("expected format %s, got %s", format, tree.Format)
				}
				return nil
			},
		}
		_, err := BuildTemplate(fsys, "index", &options)
		if err != nil {
			t.Fatal(err)
		}
	}
}
