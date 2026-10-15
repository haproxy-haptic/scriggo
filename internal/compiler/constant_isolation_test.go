// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package compiler

import (
	"reflect"
	"testing"

	"github.com/haproxy-haptic/scriggo/ast"
)

func TestConstantIdentifierTypeInfoIsIsolated(t *testing.T) {
	for _, name := range []string{"true", "false", "local"} {
		t.Run(name, func(t *testing.T) {
			tc := newTypechecker(newCompilation(nil), "", checkerOptions{}, nil)
			tc.scopes.Declare("local", &typeInfo{
				Type: boolType, Properties: propertyUntyped, Constant: boolConst(true),
			}, ast.NewIdentifier(nil, "local"), nil)
			declaration, _, _ := tc.scopes.Lookup(name)
			first := tc.checkExpr(ast.NewIdentifier(nil, name))
			second := tc.checkExpr(ast.NewIdentifier(nil, name))
			if first == second || first == declaration || second == declaration {
				t.Fatal("constant expressions share declaration metadata")
			}
			first.setValue(reflect.TypeFor[definedBool]())
			second.setValue(boolType)
			if first.valueType != reflect.TypeFor[definedBool]() || second.valueType != boolType {
				t.Fatal("constant conversions share an encoded type")
			}
			if declaration.Properties&propertyHasValue != 0 || declaration.valueType != nil {
				t.Fatal("constant conversion mutated its declaration")
			}
		})
	}
}

func TestComparisonTypeInfoIsIsolated(t *testing.T) {
	tc := newTypechecker(newCompilation(nil), "", checkerOptions{}, nil)
	tc.scopes.Declare("value", &typeInfo{Type: boolType, Properties: propertyAddressable}, ast.NewIdentifier(nil, "value"), nil)
	checkComparison := func() *typeInfo {
		return tc.checkExpr(ast.NewBinaryOperator(nil, ast.OperatorEqual,
			ast.NewIdentifier(nil, "value"), ast.NewIdentifier(nil, "true")))
	}
	first, second := checkComparison(), checkComparison()
	if first == second {
		t.Fatal("comparison expressions share mutable type information")
	}
	if first.Type != boolType || !first.Untyped() || second.Type != boolType || !second.Untyped() {
		t.Fatal("comparisons must produce untyped booleans")
	}
}
