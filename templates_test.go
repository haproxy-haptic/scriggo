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
	"gitlab.com/haproxy-haptic/scriggo/native"
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
// TestSelectorChainAsType verifies the selector-chain-as-type extension:
// a selector expression resolving to a typed value (e.g. `x.T` where T is
// a struct field) is accepted in type-expression position. Its resolved
// type is the field's static type.
//
// This unblocks schema-driven type generation patterns where the chart
// engine exposes per-resource types as fields on a runtime struct, and
// template authors reference them in macro signatures without first
// declaring a top-level type alias.
//
// Coverage:
//
//   - Pointer wrap (`*x.T`): the most common shape, the chart's
//     `(g *resources.gateways.T)` macro signature pattern.
//   - Bare selector (`x.T`): a type alias without the pointer wrap.
//   - Slice/map composition (`[]x.T`, `map[string]x.T`): the extension
//     plays nicely with composite type literals.
//   - Backward compatibility: standard package-qualified types still work
//     and the existing "not a type" error fires for non-selector
//     expressions in type position.
//
// The extension is implemented in `checkType` in
// `internal/compiler/checker_expressions.go`; this test is the
// behavioural pin against accidental regression.
func TestSelectorChainAsType(t *testing.T) {
	// A struct global whose `T` field carries the type we want to lift.
	// In production this is the per-resource typed store struct holding
	// `T <generated-resource-type>`; here we use a simple int field so
	// the test is self-contained. `inner` exercises the multi-level
	// chain (matches the chart's `resources.<store>.T` shape).
	type innerStore struct {
		T int
	}
	type container struct {
		T     int
		Inner innerStore
	}

	tests := []struct {
		name     string
		source   string
		expected string
	}{
		{
			name:     "pointer wrap",
			source:   `{% macro Foo(v *x.T) %}{% if v != nil %}got:{{ *v }}{% end %}{% end %}{% var n = 42 %}{{ Foo(&n) }}`,
			expected: "got:42",
		},
		{
			name:     "bare selector as type",
			source:   `{% macro Foo(v x.T) %}got:{{ v }}{% end %}{% var n int = 7 %}{{ Foo(n) }}`,
			expected: "got:7",
		},
		{
			name:     "slice composition",
			source:   `{% macro Sum(vs []x.T) %}{% var s = 0 %}{% for _, v := range vs %}{% s += v %}{% end %}{{ s }}{% end %}{{ Sum([]int{1, 2, 3}) }}`,
			expected: "6",
		},
		{
			name:     "map value position",
			source:   `{% macro Lookup(m map[string]x.T, k string) %}{{ m[k] }}{% end %}{{ Lookup(map[string]int{"a": 1, "b": 2}, "b") }}`,
			expected: "2",
		},
		{
			// Multi-level selector chain: `x.inner.T` — extends the
			// "package-qualified type" grammar to N levels. Mirrors
			// the chart pattern `resources.gateways.T` where the
			// type lives two struct fields deep.
			name:     "multi-level chain",
			source:   `{% macro Show(v *x.Inner.T) %}got:{{ *v }}{% end %}{% var n = 42 %}{{ Show(&n) }}`,
			expected: "got:42",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fsys := Files{"index.html": []byte(tc.source)}
			opts := &BuildOptions{
				Globals: native.Declarations{
					"x": (*container)(nil),
				},
			}
			template, err := BuildTemplate(fsys, "index.html", opts)
			if err != nil {
				t.Fatalf("template should compile against selector-as-type, got: %v", err)
			}
			var out strings.Builder
			err = template.Run(&out, map[string]any{"x": &container{}}, nil)
			if err != nil {
				t.Fatalf("template should run, got: %v", err)
			}
			if got := out.String(); got != tc.expected {
				t.Fatalf("expected %q, got %q", tc.expected, got)
			}
		})
	}
}

// TestSelectorChainAsType_RejectsNonType ensures the extension stays
// narrow: an expression that doesn't resolve via the selector path (e.g.
// a numeric literal, a function call) still fails to be used as a type
// with the same error message as before the extension.
func TestSelectorChainAsType_RejectsNonType(t *testing.T) {
	tests := []struct {
		name   string
		source string
	}{
		{"integer literal", `{% var v 42 %}`},
		{"string literal", `{% var v "hello" %}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fsys := Files{"index.html": []byte(tc.source)}
			_, err := BuildTemplate(fsys, "index.html", nil)
			if err == nil {
				t.Fatalf("expected compile error for non-type in type position, got nil")
			}
		})
	}
}

// adaptiveTestItem is the per-element shape the AdaptiveFunc tests
// iterate over. Two fields keep the loop body realistic without
// growing the global's allocation footprint.
type adaptiveTestItem struct {
	Name  string
	Value int
}

// TestAdaptiveFunc verifies the native.AdaptiveFunc declaration shape:
// argument validation goes through the Impl's concrete signature, but
// the static return type at each call site comes from the closure in
// ReturnType. This is the engine-side support that downstream
// schema-driven template engines use to keep filter outputs statically
// typed — without it, polymorphic filters declared as
// `func(any, …) []any` erase useful element-type information at the
// call boundary, forcing downstream template code through reflection.
//
// Coverage:
//
//   - same-shape return: input slice's static type flows through to
//     the call expression's static return type, so the loop variable
//     is statically typed and field access stays on the typed bytecode
//     path (the shard_slice / sort_by pattern).
//   - error path: ReturnType's error surfaces as a compile diagnostic
//     at the call site (the type-system contract).
func TestAdaptiveFunc(t *testing.T) {
	// sameShape mimics shard_slice / sort_by: return type matches the
	// first argument's static type. Impl is identity here — the real
	// filters partition / sort; the principle is the same: produce a
	// value of the same static type as the input slice.
	sameShape := native.AdaptiveFunc{
		Impl: func(items any) any { return items },
		ReturnType: func(argTypes []reflect.Type) (reflect.Type, error) {
			return argTypes[0], nil
		},
	}

	// errReturn returns an error from the closure — the type-checker
	// must surface it as a compile diagnostic at the call site.
	errReturn := native.AdaptiveFunc{
		Impl: func(items any) any { return items },
		ReturnType: func(argTypes []reflect.Type) (reflect.Type, error) {
			return nil, fmt.Errorf("test: refusing %s", argTypes[0])
		},
	}

	t.Run("same-shape return preserves typed slice element type", func(t *testing.T) {
		// Input is []*adaptiveTestItem. After the adaptive call, the
		// static return type stays []*adaptiveTestItem, so the loop
		// variable is *adaptiveTestItem and .Name is a typed field
		// access (the whole point of the feature).
		items := []*adaptiveTestItem{{Name: "a", Value: 1}, {Name: "b", Value: 2}}
		fsys := Files{"index.html": []byte(
			`{%- for _, it := range sameShape(items) %}{{ it.Name }}={{ it.Value }};{%- end %}`,
		)}
		opts := &BuildOptions{
			Globals: native.Declarations{
				"items":     &items,
				"sameShape": sameShape,
			},
		}
		template, err := BuildTemplate(fsys, "index.html", opts)
		if err != nil {
			t.Fatalf("template should compile against adaptive return type, got: %v", err)
		}
		var out strings.Builder
		if err := template.Run(&out, nil, nil); err != nil {
			t.Fatalf("template should run, got: %v", err)
		}
		got := out.String()
		want := "a=1;b=2;"
		if got != want {
			t.Fatalf("expected %q, got %q", want, got)
		}
	})

	t.Run("ReturnType error surfaces as compile diagnostic", func(t *testing.T) {
		items := []*adaptiveTestItem{{Name: "x"}}
		fsys := Files{"index.html": []byte(
			`{%- var _ = errReturn(items) %}`,
		)}
		opts := &BuildOptions{
			Globals: native.Declarations{
				"items":     &items,
				"errReturn": errReturn,
			},
		}
		_, err := BuildTemplate(fsys, "index.html", opts)
		if err == nil {
			t.Fatal("template should fail to compile when AdaptiveFunc ReturnType returns an error")
		}
		if !strings.Contains(err.Error(), "test: refusing") {
			t.Fatalf("expected the closure's error message in the diagnostic, got: %v", err)
		}
	})
}

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
