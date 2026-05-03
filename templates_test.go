// Copyright 2019 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"reflect"
	"strings"
	"testing"

	"gitlab.com/haproxy-haptic/scriggo/ast"
	"gitlab.com/haproxy-haptic/scriggo/ast/astutil"
	"gitlab.com/haproxy-haptic/scriggo/internal/compiler"
	"gitlab.com/haproxy-haptic/scriggo/internal/fstest"
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
		Type: reflect.TypeFor[int](),
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
		Type:  reflect.TypeFor[int](),
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
		Type: reflect.TypeFor[int](),
	}
	init := map[string]any{"a": &n}
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
		Type: reflect.TypeFor[int](),
	}
	init = map[string]any{"a": n}
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
		Type:  reflect.TypeFor[int](),
		Value: reflect.ValueOf(&n).Elem(),
	}
	init := map[string]any{"a": 5}
	_ = initGlobalVariables([]compiler.Global{global}, init)
}

func TestInitGlobalsNilError(t *testing.T) {
	defer recoverInitGlobalsPanic(t, "variable initializer \"a\" cannot be nil")
	global := compiler.Global{
		Pkg:  "main",
		Name: "a",
		Type: reflect.TypeFor[int](),
	}
	init := map[string]any{"a": nil}
	_ = initGlobalVariables([]compiler.Global{global}, init)
}

func TestInitGlobalsInvalidTypeError(t *testing.T) {
	defer recoverInitGlobalsPanic(t, "variable initializer \"a\" must have type assignable to int, but have bool")
	global := compiler.Global{
		Pkg:  "main",
		Name: "a",
		Type: reflect.TypeFor[int](),
	}
	init := map[string]any{"a": true}
	_ = initGlobalVariables([]compiler.Global{global}, init)
}

func TestInitGlobalsNilPointerError(t *testing.T) {
	defer recoverInitGlobalsPanic(t, "variable initializer \"a\" cannot be a nil pointer")
	global := compiler.Global{
		Pkg:  "main",
		Name: "a",
		Type: reflect.TypeFor[int](),
	}
	init := map[string]any{"a": (*int)(nil)}
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
			ExpandedTransformer: func(tree *ast.Tree) error {
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

func TestTemplateMainFormatWithExtends(t *testing.T) {
	fsys := fstest.Files{
		"index.md":    `{% extends "layout.html" %}`,
		"layout.html": `<h1>Title</h1>`,
	}
	template, err := BuildTemplate(fsys, "index.md", nil)
	if err != nil {
		t.Fatal(err)
	}
	if template.fn.Format != ast.FormatHTML {
		t.Fatalf("expected main format %s, got %s", ast.FormatHTML, template.fn.Format)
	}
}

func TestTemplateFormat(t *testing.T) {
	tests := map[string]struct {
		fsys     fs.FS
		name     string
		expected Format
	}{
		"Text": {
			fsys:     fstest.Files{"index.txt": `hello`},
			name:     "index.txt",
			expected: FormatText,
		},
		"HTML": {
			fsys:     fstest.Files{"index.html": `<h1>hello</h1>`},
			name:     "index.html",
			expected: FormatHTML,
		},
		"Markdown": {
			fsys:     fstest.Files{"index.md": `# hello`},
			name:     "index.md",
			expected: FormatMarkdown,
		},
		"MarkdownExtendsHTML": {
			fsys: fstest.Files{
				"index.md":    `{% extends "layout.html" %}`,
				"layout.html": `<h1>hello</h1>`,
			},
			name:     "index.md",
			expected: FormatHTML,
		},
		"MultipleExtendsUseFinalFormat": {
			fsys: fstest.Files{
				"index.md":    `{% extends "level1.html" %}`,
				"level1.html": `{% extends "level2.html" %}`,
				"level2.html": `<h1>hello</h1>`,
			},
			name:     "index.md",
			expected: FormatHTML,
		},
		"FormatFS": {
			fsys:     testFormatFS{Files: fstest.Files{"index": `hello`}, format: FormatJSON},
			name:     "index",
			expected: FormatJSON,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			template, err := BuildTemplate(tc.fsys, tc.name, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := template.Format(); got != tc.expected {
				t.Fatalf("expected %s, got %s", tc.expected, got)
			}
		})
	}
}

// TestUnexpandedTransformer ensures the transformer runs before expansion.
func TestUnexpandedTransformer(t *testing.T) {
	fsys := fstest.Files{
		"index.html": `before {{ render "a.html" }} after`,
		"a.html":     "A",
		"b.html":     "B",
	}
	options := BuildOptions{
		UnexpandedTransformer: func(tree *ast.Tree) error {
			if tree.Path != "index.html" {
				return nil
			}
			var render *ast.Render
			astutil.Inspect(tree, func(node ast.Node) bool {
				if render == nil {
					render, _ = node.(*ast.Render)
				}
				return render == nil
			})
			if render == nil {
				return fmt.Errorf("expected render node, got none")
			}
			render.Path = "b.html"
			return nil
		},
	}
	template, err := BuildTemplate(fsys, "index.html", &options)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	var out strings.Builder
	err = template.Run(&out, nil, nil)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	const expected = "before B after"
	if out.String() != expected {
		t.Fatalf("expected %q, got %q", expected, out.String())
	}
}
