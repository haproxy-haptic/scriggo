// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package compiler

import (
	"io"
	"reflect"
	"testing"

	"github.com/haproxy-haptic/scriggo/ast"
	"github.com/haproxy-haptic/scriggo/native"
)

type checkerTextFragment struct{}

func (checkerTextFragment) WriteTo(io.Writer) (int64, error) { return 0, nil }
func (checkerTextFragment) String() string                   { return "fragment" }
func (checkerTextFragment) JS() native.JS                    { return "fragment" }

func TestCheckShowTextFragmentContext(t *testing.T) {
	types := []reflect.Type{
		reflect.TypeFor[native.TextFragment](),
		reflect.TypeFor[checkerTextFragment](),
		reflect.TypeFor[*checkerTextFragment](),
	}
	for _, typ := range types {
		for ctx := ast.ContextText; ctx <= ast.ContextSpacesCodeBlock; ctx++ {
			err := checkShow(typ, ctx)
			switch ctx {
			case ast.ContextText:
				if err != nil {
					t.Errorf("type %s in context %s: %v", typ, ctx, err)
				}
			default:
				if err == nil {
					t.Errorf("type %s in context %s unexpectedly accepted", typ, ctx)
				}
			}
		}
	}
}
