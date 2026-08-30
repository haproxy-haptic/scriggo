// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"reflect"
	"sort"
	"testing"

	"gitlab.com/haproxy-haptic/scriggo/native"
)

type usedNativeDeclarationItem struct {
	Value int
}

func TestTemplateUsedNativeDeclarationsReportsExactReferencedBindings(t *testing.T) {
	direct := func() string { return "direct" }
	nested := func() string { return "nested" }
	unused := func() string { return "unused" }
	text := "text"
	tmpl, err := BuildTemplate(Files{
		"index.txt":   []byte(`{% macro M %}{{ direct() }}{% end %}{{ M() }}{{ render "partial.txt" }}{% item := Item{Value: Count} %}{{ item.Value }}:{{ text }}`),
		"partial.txt": []byte(`{{ helpers.Nested() }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"Count":  native.UntypedNumericConst("7"),
		"Item":   reflect.TypeFor[usedNativeDeclarationItem](),
		"direct": native.Synchronous(direct),
		"helpers": native.Package{Name: "helpers", Declarations: native.Declarations{
			"Nested": nested,
		}},
		"text":   &text,
		"unused": unused,
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	declarations := tmpl.UsedNativeDeclarations()
	if !sort.SliceIsSorted(declarations, func(i, j int) bool {
		if declarations[i].Package != declarations[j].Package {
			return declarations[i].Package < declarations[j].Package
		}
		if declarations[i].Name != declarations[j].Name {
			return declarations[i].Name < declarations[j].Name
		}
		return declarations[i].Kind < declarations[j].Kind
	}) {
		t.Fatalf("declarations are not sorted: %#v", declarations)
	}
	byName := make(map[string]UsedNativeDeclaration, len(declarations))
	for _, declaration := range declarations {
		byName[declaration.Package+"."+declaration.Name] = declaration
	}
	if len(byName) != 5 {
		t.Fatalf("declarations = %#v, want five referenced bindings", declarations)
	}
	assertUsedNativeFunction(t, byName["main.direct"], direct, NativeDeclarationFunction, true)
	assertUsedNativeFunction(t, byName["helpers.Nested"], nested, NativeDeclarationFunction, false)
	assertUsedNativeDeclaration(t, byName["main.Count"], native.UntypedNumericConst("7"), NativeDeclarationConstant)
	assertUsedNativeDeclaration(t, byName["main.Item"], reflect.TypeFor[usedNativeDeclarationItem](), NativeDeclarationType)
	assertUsedNativeDeclaration(t, byName["main.text"], &text, NativeDeclarationVariable)
	if _, ok := byName["main.unused"]; ok {
		t.Fatal("unused declaration was reported")
	}

	declarations[0].SynchronousMembers = append(declarations[0].SynchronousMembers, "poison")
	for _, declaration := range tmpl.UsedNativeDeclarations() {
		for _, member := range declaration.SynchronousMembers {
			if member == "poison" {
				t.Fatal("caller mutation changed compiled declaration metadata")
			}
		}
	}
}

func TestTemplateUsedNativeDeclarationsDistinguishesShadowedUniverseName(t *testing.T) {
	customLen := func(string) int { return 41 }
	shadowed, err := BuildTemplate(Files{
		"index.txt": []byte(`{{ len("value") }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"len": native.Synchronous(customLen),
	}})
	if err != nil {
		t.Fatalf("build shadowed: %v", err)
	}
	declarations := shadowed.UsedNativeDeclarations()
	if len(declarations) != 1 {
		t.Fatalf("shadowed declarations = %#v, want one", declarations)
	}
	assertUsedNativeFunction(t, declarations[0], customLen, NativeDeclarationFunction, true)
	if declarations[0].Package != "main" || declarations[0].Name != "len" {
		t.Fatalf("shadowed declaration = %#v, want main.len", declarations[0])
	}

	builtin, err := BuildTemplate(Files{
		"index.txt": []byte(`{{ len("value") }}`),
	}, "index.txt", nil)
	if err != nil {
		t.Fatalf("build builtin: %v", err)
	}
	if declarations := builtin.UsedNativeDeclarations(); len(declarations) != 0 {
		t.Fatalf("builtin declarations = %#v, want none", declarations)
	}
}

func TestTemplateUsedNativeCallablesReportsDeclarationAndMemberPath(t *testing.T) {
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{{ store.Store.Call() }}{{ store.Store.Other() }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"store": native.Synchronous((*batchCertificateNestedStore)(nil), "*.Call"),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	callables := tmpl.UsedNativeCallables()
	if len(callables) != 2 {
		t.Fatalf("callables = %#v, want two", callables)
	}
	byMember := make(map[string]UsedNativeCallable, len(callables))
	for _, callable := range callables {
		byMember[callable.MemberPath] = callable
		if callable.Package != "main" || callable.DeclarationName != "store" {
			t.Fatalf("callable origin = %#v, want main.store", callable)
		}
		if reflect.TypeOf(callable.Declaration) != reflect.TypeOf((*batchCertificateNestedStore)(nil)) {
			t.Fatalf("callable declaration = %T, want *batchCertificateNestedStore", callable.Declaration)
		}
		if callable.Kind != NativeCallableFunctionField || callable.Receiver != reflect.TypeFor[batchCertificateFunctionStore]() {
			t.Fatalf("callable static identity = %#v", callable)
		}
	}
	if !byMember["Store.Call"].Synchronous {
		t.Fatalf("Store.Call = %#v, want certified", byMember["Store.Call"])
	}
	if byMember["Store.Other"].Synchronous {
		t.Fatalf("Store.Other = %#v, want uncertified", byMember["Store.Other"])
	}
}

func TestTemplateUsedNativeCallablesReportsFunctionResult(t *testing.T) {
	makeCall := func() func() string {
		return func() string { return "ambient" }
	}
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{% call := makeCall() %}{{ call() }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"makeCall": native.Synchronous(makeCall),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	callables := tmpl.UsedNativeCallables()
	if len(callables) != 1 {
		t.Fatalf("callables = %#v, want one", callables)
	}
	callable := callables[0]
	if callable.Kind != NativeCallableFunctionResult || callable.Package != "main" ||
		callable.DeclarationName != "makeCall" || callable.MemberPath != "<result>" || callable.Synchronous {
		t.Fatalf("callable = %#v, want unsafe main.makeCall function result", callable)
	}
	if reflect.ValueOf(callable.Declaration).Pointer() != reflect.ValueOf(makeCall).Pointer() {
		t.Fatalf("callable declaration = %v, want exact makeCall", callable.Declaration)
	}
}

func assertUsedNativeFunction(
	t *testing.T,
	got UsedNativeDeclaration,
	want native.Declaration,
	kind NativeDeclarationKind,
	synchronous bool,
) {
	t.Helper()
	if got.Kind != kind || got.Synchronous != synchronous {
		t.Fatalf("declaration = %#v, want kind %d synchronous %t", got, kind, synchronous)
	}
	if got.Declaration == nil || reflect.ValueOf(got.Declaration).Pointer() != reflect.ValueOf(want).Pointer() {
		t.Fatalf("declaration function = %v, want exact function %v", got.Declaration, want)
	}
}

func assertUsedNativeDeclaration(
	t *testing.T,
	got UsedNativeDeclaration,
	want native.Declaration,
	kind NativeDeclarationKind,
) {
	t.Helper()
	if got.Kind != kind || !reflect.DeepEqual(got.Declaration, want) {
		t.Fatalf("declaration = %#v, want kind %d exact value %#v", got, kind, want)
	}
}
