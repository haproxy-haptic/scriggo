// Copyright 2019 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"reflect"
	"testing"

	"gitlab.com/haproxy-haptic/scriggo/internal/compiler"
)

func TestInitPackageLevelVariables(t *testing.T) {

	// Test no globals.
	globals := initPackageLevelVariables([]compiler.Global{})
	if globals != nil {
		t.Fatalf("expected nil, got %v", globals)
	}

	// Test zero value.
	global := compiler.Global{
		Pkg:  "p",
		Name: "a",
		Type: reflect.TypeFor[int](),
	}
	globals = initPackageLevelVariables([]compiler.Global{global})
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
	globals = initPackageLevelVariables([]compiler.Global{global})
	n = 2
	g = globals[0]
	if g.Kind() != reflect.Int {
		t.Fatalf("unexpected kind %v", g.Kind())
	}
	iface := g.Interface()
	if iface != n {
		t.Fatalf("unexpected %v (type %T), expecting %d (type %T)", iface, iface, n, n)
	}

}

func TestProgramSharedImportedPackageInitializesOnce(t *testing.T) {
	program, err := Build(Files{
		"go.mod": []byte("module example.test/sharedinit\n"),
		"main.go": []byte(`package main

import (
	_ "example.test/sharedinit/a"
	_ "example.test/sharedinit/b"
)

func main() {}
`),
		"a/a.go": []byte(`package a

import "example.test/sharedinit/c"

func init() { c.Value += 10 }
`),
		"b/b.go": []byte(`package b

import "example.test/sharedinit/c"

func init() {
	if c.Value != 52 {
		panic(c.Value)
	}
}
`),
		"c/c.go": []byte(`package c

var Value = 42
`),
	}, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := program.Run(nil); err != nil {
		t.Fatalf("run: %v", err)
	}
}
