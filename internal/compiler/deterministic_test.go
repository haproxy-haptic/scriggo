// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package compiler

import (
	"reflect"
	"testing"

	"github.com/haproxy-haptic/scriggo/internal/fstest"
	"github.com/haproxy-haptic/scriggo/internal/runtime"
)

func TestEmitterRecordsRangeTypesAcrossFunctionGraph(t *testing.T) {
	code, err := BuildTemplate(fstest.Files{
		"index.txt":   `{% import "lib.txt" for Imported %}{% macro Local(values map[int]int) %}{% for key := range values %}{{ key }}{% end %}{% end %}{{ Local(map[int]int{2: 2}) }}{{ Imported(map[string]int{"b": 2}) }}{{ render "partial.txt" }}`,
		"lib.txt":     `{% macro Imported(values map[string]int) %}{% for key := range values %}{{ key }}{% end %}{% end %}`,
		"partial.txt": `{% var values any = map[bool]int{true: 1} %}{% for key := range values %}{{ key }}{% end %}`,
	}, "index.txt", Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	found := map[reflect.Kind]int{}
	seen := map[*runtime.Function]struct{}{}
	functions := []*runtime.Function{code.Main}
	for len(functions) > 0 {
		fn := functions[0]
		functions = functions[1:]
		if _, ok := seen[fn]; ok {
			continue
		}
		seen[fn] = struct{}{}
		for addr, typ := range fn.RangeTypes {
			op := fn.Body[addr].Op
			if op != runtime.OpRange && op != runtime.OpRangeInterface && op != runtime.OpRangeString && op != -runtime.OpRangeString {
				t.Fatalf("range type at %d belongs to opcode %v", addr, op)
			}
			found[typ.Kind()]++
		}
		functions = append(functions, fn.Functions...)
	}
	if found[reflect.Map] < 2 {
		t.Fatalf("map range metadata count = %d, want at least 2", found[reflect.Map])
	}
	if found[reflect.Interface] < 1 {
		t.Fatalf("interface range metadata count = %d, want at least 1", found[reflect.Interface])
	}
}
