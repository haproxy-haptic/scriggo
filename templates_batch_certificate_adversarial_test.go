// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"errors"
	"reflect"
	"testing"

	"github.com/haproxy-haptic/scriggo/native"
)

type batchCertificateFunctionStore struct {
	Call  func() string
	Other func() string
}

type batchCertificateNestedStore struct {
	Store batchCertificateFunctionStore
}

type batchCertificateFunctionFactory struct {
	Make func() func() string
}

type batchCertificateReader interface {
	Read() string
}

type batchCertificateMethods struct{}

func (batchCertificateMethods) Read() string  { return "read" }
func (batchCertificateMethods) Write() string { return "write" }

func TestTemplateBatchSafeRejectsEveryUncertifiedCallableShape(t *testing.T) {
	tests := []struct {
		name        string
		source      string
		declaration native.Declaration
	}{
		{
			name:        "escaped function field",
			source:      `{% call := store.Call %}{{ call() }}`,
			declaration: (*batchCertificateFunctionStore)(nil),
		},
		{
			name:        "nested function field",
			source:      `{{ store.Store.Call() }}`,
			declaration: (*batchCertificateNestedStore)(nil),
		},
		{
			name:        "map function value",
			source:      `{{ store.call() }}`,
			declaration: (*map[string]func() string)(nil),
		},
		{
			name:        "interface method",
			source:      `{{ store.Read() }}`,
			declaration: (*batchCertificateReader)(nil),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpl, err := BuildTemplate(Files{"index.txt": []byte(test.source)}, "index.txt", &BuildOptions{
				Globals: native.Declarations{"store": test.declaration},
			})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if tmpl.BatchSafe() {
				t.Fatalf("BatchSafe = true, declarations = %#v", tmpl.UsedNativeDeclarations())
			}
			if err := tmpl.RunBatch(nil); !errors.Is(err, ErrBatchUncertifiedNative) {
				t.Fatalf("empty batch error = %v, want %v", err, ErrBatchUncertifiedNative)
			}
		})
	}
}

func TestTemplateBatchCertificateUsesOnlyReferencedDeclarations(t *testing.T) {
	tmpl, err := BuildTemplate(Files{"index.txt": []byte("safe")}, "index.txt", &BuildOptions{
		Globals: native.Declarations{"unused": func() string { return "ambient" }},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !tmpl.BatchSafe() {
		t.Fatal("BatchSafe = false, want true")
	}
}

func TestTemplateBatchCertificateFindsUnreachableAndGlobbedNativeUse(t *testing.T) {
	tests := []struct {
		name  string
		files Files
	}{
		{
			name: "dead branch",
			files: Files{
				"index.txt": []byte(`{% if false %}{{ ambient() }}{% end %}safe`),
			},
		},
		{
			name: "render glob",
			files: Files{
				"index.txt":       []byte(`{{ render_glob "parts/*.txt" }}`),
				"parts/first.txt": []byte(`{{ ambient() }}`),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpl, err := BuildTemplate(test.files, "index.txt", &BuildOptions{
				Globals: native.Declarations{"ambient": func() string { return "ambient" }},
			})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if tmpl.BatchSafe() {
				t.Fatalf("BatchSafe = true, declarations = %#v", tmpl.UsedNativeDeclarations())
			}
			if err := tmpl.RunBatch(nil); !errors.Is(err, ErrBatchUncertifiedNative) {
				t.Fatalf("empty batch error = %v, want %v", err, ErrBatchUncertifiedNative)
			}
		})
	}
}

func TestTemplateBatchCertificateIsBoundToDeclarationOrigin(t *testing.T) {
	tests := []struct {
		name        string
		source      string
		globals     native.Declarations
		declaration string
	}{
		{
			name:   "same-type variable",
			source: `{{ a.Call() }}{{ b.Call() }}`,
			globals: native.Declarations{
				"a": native.Synchronous((*batchCertificateFunctionStore)(nil), "Call"),
				"b": (*batchCertificateFunctionStore)(nil),
			},
			declaration: "b",
		},
		{
			name:   "escaped same-type variable",
			source: `{% escaped := b %}{% call := escaped.Call %}{{ call() }}`,
			globals: native.Declarations{
				"a": native.Synchronous((*batchCertificateFunctionStore)(nil), "Call"),
				"b": (*batchCertificateFunctionStore)(nil),
			},
			declaration: "b",
		},
		{
			name:   "same-type function result",
			source: `{{ a().Call() }}{{ b().Call() }}`,
			globals: native.Declarations{
				"a": native.Synchronous(func() batchCertificateFunctionStore { return batchCertificateFunctionStore{} }, "Call"),
				"b": native.Synchronous(func() batchCertificateFunctionStore { return batchCertificateFunctionStore{} }),
			},
			declaration: "b",
		},
		{
			name:   "escaped same-type function result",
			source: `{% escaped := b() %}{% call := escaped.Call %}{{ call() }}`,
			globals: native.Declarations{
				"a": native.Synchronous(func() batchCertificateFunctionStore { return batchCertificateFunctionStore{} }, "Call"),
				"b": native.Synchronous(func() batchCertificateFunctionStore { return batchCertificateFunctionStore{} }),
			},
			declaration: "b",
		},
		{
			name:   "same-type declaration",
			source: `{{ B{}.Read() }}`,
			globals: native.Declarations{
				"A": native.Synchronous(reflect.TypeFor[batchCertificateMethods](), "Read"),
				"B": reflect.TypeFor[batchCertificateMethods](),
			},
			declaration: "B",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpl, err := BuildTemplate(Files{"index.txt": []byte(test.source)}, "index.txt", &BuildOptions{
				Globals: test.globals,
			})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if tmpl.BatchSafe() {
				t.Fatal("BatchSafe = true, want false")
			}
			found := false
			for _, callable := range tmpl.UsedNativeCallables() {
				if callable.DeclarationName == test.declaration && !callable.Synchronous {
					found = true
				}
			}
			if !found {
				t.Fatalf("callables = %#v, want unsafe declaration %q", tmpl.UsedNativeCallables(), test.declaration)
			}
		})
	}
}

func TestTemplateBatchCertificateKeepsOriginAcrossFilesAndMacros(t *testing.T) {
	tests := []struct {
		name  string
		files Files
	}{
		{
			name: "macro",
			files: Files{
				"index.txt": []byte(`{% macro M %}{{ ambient.Call() }}{% end %}{{ M() }}`),
			},
		},
		{
			name: "rendered file",
			files: Files{
				"index.txt":   []byte(`{{ render "partial.txt" }}`),
				"partial.txt": []byte(`{{ ambient.Call() }}`),
			},
		},
		{
			name: "imported macro",
			files: Files{
				"index.txt":   []byte(`{% import "library.txt" for M %}{{ M() }}`),
				"library.txt": []byte(`{% macro M %}{{ ambient.Call() }}{% end %}`),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpl, err := BuildTemplate(test.files, "index.txt", &BuildOptions{Globals: native.Declarations{
				"safe":    native.Synchronous((*batchCertificateFunctionStore)(nil), "Call"),
				"ambient": (*batchCertificateFunctionStore)(nil),
			}})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if tmpl.BatchSafe() {
				t.Fatalf("BatchSafe = true, callables = %#v", tmpl.UsedNativeCallables())
			}
			found := false
			for _, callable := range tmpl.UsedNativeCallables() {
				if callable.DeclarationName == "ambient" && !callable.Synchronous {
					found = true
				}
			}
			if !found {
				t.Fatalf("callables = %#v, want unsafe ambient origin", tmpl.UsedNativeCallables())
			}
		})
	}
}

func TestTemplateBatchCertificateAcceptsExactCallableOrigin(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		globals native.Declarations
	}{
		{
			name:   "escaped variable",
			source: `{% escaped := store %}{% call := escaped.Call %}{{ call() }}`,
			globals: native.Declarations{
				"store": native.Synchronous((*batchCertificateFunctionStore)(nil), "Call"),
			},
		},
		{
			name:   "escaped function result",
			source: `{% escaped := makeStore() %}{% call := escaped.Call %}{{ call() }}`,
			globals: native.Declarations{
				"makeStore": native.Synchronous(
					func() batchCertificateFunctionStore { return batchCertificateFunctionStore{} },
					"Call",
				),
			},
		},
		{
			name:   "nested function result",
			source: `{{ makeStore().Store.Call() }}`,
			globals: native.Declarations{
				"makeStore": native.Synchronous(
					func() batchCertificateNestedStore { return batchCertificateNestedStore{} },
					"Store.Call",
				),
			},
		},
		{
			name:   "nested wildcard",
			source: `{{ store.Store.Call() }}`,
			globals: native.Declarations{
				"store": native.Synchronous((*batchCertificateNestedStore)(nil), "*.Call"),
			},
		},
		{
			name:   "map wildcard",
			source: `{{ store.first.Read() }}`,
			globals: native.Declarations{
				"store": native.Synchronous((*map[string]batchCertificateMethods)(nil), "*.Read"),
			},
		},
		{
			name:   "method",
			source: `{{ store.Read() }}`,
			globals: native.Declarations{
				"store": native.Synchronous((*batchCertificateMethods)(nil), "Read"),
			},
		},
		{
			name:   "constructed native type method",
			source: `{{ Store{}.Read() }}`,
			globals: native.Declarations{
				"Store": native.Synchronous(reflect.TypeFor[batchCertificateMethods](), "Read"),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpl, err := BuildTemplate(Files{"index.txt": []byte(test.source)}, "index.txt", &BuildOptions{
				Globals: test.globals,
			})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if !tmpl.BatchSafe() {
				t.Fatalf("BatchSafe = false, callables = %#v", tmpl.UsedNativeCallables())
			}
			for _, callable := range tmpl.UsedNativeCallables() {
				if !callable.Synchronous {
					t.Fatalf("uncertified callable: %#v", callable)
				}
			}
		})
	}
}

func TestTemplateBatchCertificateRejectsNewCallableSurface(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		globals native.Declarations
	}{
		{
			name:   "function field",
			source: `{{ store.Other() }}`,
			globals: native.Declarations{
				"store": native.Synchronous((*batchCertificateFunctionStore)(nil), "Call"),
			},
		},
		{
			name:   "method",
			source: `{{ store.Write() }}`,
			globals: native.Declarations{
				"store": native.Synchronous((*batchCertificateMethods)(nil), "Read"),
			},
		},
		{
			name:   "wildcard only selects named callable",
			source: `{{ store.Store.Other() }}`,
			globals: native.Declarations{
				"store": native.Synchronous((*batchCertificateNestedStore)(nil), "*.Call"),
			},
		},
		{
			name:   "constructed function field",
			source: `{{ Store{Call: func() string { return "ambient" }}.Call() }}`,
			globals: native.Declarations{
				"Store": native.Synchronous(reflect.TypeFor[batchCertificateFunctionStore](), "Call"),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpl, err := BuildTemplate(Files{"index.txt": []byte(test.source)}, "index.txt", &BuildOptions{
				Globals: test.globals,
			})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if tmpl.BatchSafe() {
				t.Fatal("BatchSafe = true, want false")
			}
			if err := tmpl.RunBatch(nil); !errors.Is(err, ErrBatchUncertifiedNative) {
				t.Fatalf("empty batch error = %v, want %v", err, ErrBatchUncertifiedNative)
			}
		})
	}
}

func TestTemplateBatchCertificateRejectsAmbiguousMultipleResults(t *testing.T) {
	pair := func() (batchCertificateFunctionStore, batchCertificateFunctionStore) {
		return batchCertificateFunctionStore{}, batchCertificateFunctionStore{}
	}
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{% first, second := pair() %}{{ first.Call() }}{{ second.Call() }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"pair": native.Synchronous(pair, "Call"),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if tmpl.BatchSafe() {
		t.Fatal("BatchSafe = true, want false")
	}
	for _, callable := range tmpl.UsedNativeCallables() {
		if callable.Synchronous {
			t.Fatalf("ambiguous callable certified: %#v", callable)
		}
	}
}

func TestTemplateBatchCertificateRejectsLoopCarriedProvenanceOverwrite(t *testing.T) {
	safe := &batchCertificateFunctionStore{Call: func() string { return "safe" }}
	ambient := &batchCertificateFunctionStore{Call: func() string { return "ambient" }}
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{% current := safe %}` +
			`{% for iteration := 0; iteration < 2; iteration++ %}` +
			`{% if iteration > 0 %}{% current = ambient %}{% end %}` +
			`{{ current.Call() }}` +
			`{% end %}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"safe":    native.Synchronous(safe, "Call"),
		"ambient": ambient,
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if tmpl.BatchSafe() {
		t.Fatalf("BatchSafe = true, callables = %#v", tmpl.UsedNativeCallables())
	}
	if err := tmpl.RunBatch(nil); !errors.Is(err, ErrBatchUncertifiedNative) {
		t.Fatalf("empty batch error = %v, want %v", err, ErrBatchUncertifiedNative)
	}
}

func TestTemplateBatchCertificateRejectsLoopCarriedOverwriteAfterCall(t *testing.T) {
	safe := &batchCertificateFunctionStore{Call: func() string { return "safe" }}
	makeAmbient := func() batchCertificateFunctionStore {
		return batchCertificateFunctionStore{Call: func() string { return "ambient" }}
	}
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{% current := safe %}` +
			`{% for iteration := 0; iteration < 2; iteration++ %}` +
			`{{ current.Call() }}` +
			`{% current = makeAmbient() %}` +
			`{% end %}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"safe":        native.Synchronous(safe, "Call"),
		"makeAmbient": native.Synchronous(makeAmbient),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if tmpl.BatchSafe() {
		t.Fatalf("BatchSafe = true, callables = %#v", tmpl.UsedNativeCallables())
	}
	if err := tmpl.RunBatch(nil); !errors.Is(err, ErrBatchUncertifiedNative) {
		t.Fatalf("empty batch error = %v, want %v", err, ErrBatchUncertifiedNative)
	}
}

func TestTemplateBatchCertificateAcceptsLoopCarriedCertifiedOverwrite(t *testing.T) {
	first := &batchCertificateFunctionStore{Call: func() string { return "first" }}
	second := &batchCertificateFunctionStore{Call: func() string { return "second" }}
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{% current := first %}` +
			`{% for iteration := 0; iteration < 2; iteration++ %}` +
			`{{ current.Call() }}` +
			`{% current = second %}` +
			`{% end %}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"first":  native.Synchronous(first, "Call"),
		"second": native.Synchronous(second, "Call"),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !tmpl.BatchSafe() {
		t.Fatalf("BatchSafe = false, callables = %#v", tmpl.UsedNativeCallables())
	}
}

func TestTemplateBatchCertificateRejectsFunctionFieldOverwrite(t *testing.T) {
	makeStore := func() batchCertificateFunctionStore {
		return batchCertificateFunctionStore{Call: func() string { return "safe" }}
	}
	ambient := func() string { return "ambient" }
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{% current := makeStore() %}` +
			`{% current.Call = ambient %}` +
			`{{ current.Call() }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"makeStore": native.Synchronous(makeStore, "Call"),
		"ambient":   ambient,
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if tmpl.BatchSafe() {
		t.Fatalf("BatchSafe = true, callables = %#v", tmpl.UsedNativeCallables())
	}
	if err := tmpl.RunBatch(nil); !errors.Is(err, ErrBatchUncertifiedNative) {
		t.Fatalf("empty batch error = %v, want %v", err, ErrBatchUncertifiedNative)
	}
}

func TestTemplateBatchCertificateRejectsScriggoFunctionFieldOverwrite(t *testing.T) {
	makeStore := func() batchCertificateFunctionStore {
		return batchCertificateFunctionStore{Call: func() string { return "safe" }}
	}
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{% current := makeStore() %}` +
			`{% current.Call = func() string { return "ambient" } %}` +
			`{{ current.Call() }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"makeStore": native.Synchronous(makeStore, "Call"),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if tmpl.BatchSafe() {
		t.Fatalf("BatchSafe = true, callables = %#v", tmpl.UsedNativeCallables())
	}
	if err := tmpl.RunBatch(nil); !errors.Is(err, ErrBatchUncertifiedNative) {
		t.Fatalf("empty batch error = %v, want %v", err, ErrBatchUncertifiedNative)
	}
}

func TestTemplateBatchCertificateRejectsMutationThroughPointerAlias(t *testing.T) {
	store := &batchCertificateFunctionStore{Call: func() string { return "safe" }}
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{% alias := store %}` +
			`{% alias.Call = func() string { return "ambient" } %}` +
			`{{ store.Call() }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"store": native.Synchronous(store, "Call"),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if tmpl.BatchSafe() {
		t.Fatalf("BatchSafe = true, callables = %#v", tmpl.UsedNativeCallables())
	}
	if err := tmpl.RunBatch(nil); !errors.Is(err, ErrBatchUncertifiedNative) {
		t.Fatalf("empty batch error = %v, want %v", err, ErrBatchUncertifiedNative)
	}
}

func TestTemplateBatchCertificateRejectsNativeFunctionResults(t *testing.T) {
	makeCall := func() func() string {
		return func() string { return "ambient" }
	}
	tests := []struct {
		name   string
		source string
	}{
		{
			name:   "immediate call",
			source: `{{ makeCall()() }}`,
		},
		{
			name:   "escaped call",
			source: `{% call := makeCall() %}{{ call() }}`,
		},
		{
			name: "passed through Scriggo function",
			source: `{% var invoke = func(call func() string) string { return call() } %}` +
				`{{ invoke(makeCall()) }}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpl, err := BuildTemplate(Files{"index.txt": []byte(test.source)}, "index.txt", &BuildOptions{
				Globals: native.Declarations{"makeCall": native.Synchronous(makeCall)},
			})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if tmpl.BatchSafe() {
				t.Fatalf("BatchSafe = true, callables = %#v", tmpl.UsedNativeCallables())
			}
			if err := tmpl.RunBatch(nil); !errors.Is(err, ErrBatchUncertifiedNative) {
				t.Fatalf("empty batch error = %v, want %v", err, ErrBatchUncertifiedNative)
			}
		})
	}
}

func TestTemplateBatchCertificateAcceptsLocalFunctionWithNativeResultSignature(t *testing.T) {
	makeCall := func() func() string {
		return func() string { return "ambient" }
	}
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{% unused := makeCall() %}` +
			`{% local := func() string { return "safe" } %}` +
			`{{ local() }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"makeCall": native.Synchronous(makeCall),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !tmpl.BatchSafe() {
		t.Fatalf("BatchSafe = false, callables = %#v", tmpl.UsedNativeCallables())
	}
}

func TestTemplateBatchCertificateRejectsFunctionBehindNativeInterface(t *testing.T) {
	makeAny := func() any {
		return func() string { return "ambient" }
	}
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{% value := makeAny() %}` +
			`{% call := value.(func() string) %}` +
			`{{ call() }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"makeAny": native.Synchronous(makeAny),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if tmpl.BatchSafe() {
		t.Fatalf("BatchSafe = true, callables = %#v", tmpl.UsedNativeCallables())
	}
	if err := tmpl.RunBatch(nil); !errors.Is(err, ErrBatchUncertifiedNative) {
		t.Fatalf("empty batch error = %v, want %v", err, ErrBatchUncertifiedNative)
	}
}

func TestTemplateBatchCertificateRejectsFunctionBehindNativeVariableInterface(t *testing.T) {
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{% call := value.(func() string) %}{{ call() }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"value": (*any)(nil),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if tmpl.BatchSafe() {
		t.Fatalf("BatchSafe = true, callables = %#v", tmpl.UsedNativeCallables())
	}
	if err := tmpl.RunBatch(nil); !errors.Is(err, ErrBatchUncertifiedNative) {
		t.Fatalf("empty batch error = %v, want %v", err, ErrBatchUncertifiedNative)
	}
}

func TestTemplateBatchCertificateRejectsFunctionReturnedByCertifiedField(t *testing.T) {
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{{ store.Make()() }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"store": native.Synchronous((*batchCertificateFunctionFactory)(nil), "Make"),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if tmpl.BatchSafe() {
		t.Fatalf("BatchSafe = true, callables = %#v", tmpl.UsedNativeCallables())
	}
	if err := tmpl.RunBatch(nil); !errors.Is(err, ErrBatchUncertifiedNative) {
		t.Fatalf("empty batch error = %v, want %v", err, ErrBatchUncertifiedNative)
	}
}
