// Copyright (c) 2025 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package compiler

import (
	"strings"
	"testing"

	"github.com/haproxy-haptic/scriggo/ast"
)

// TestParseWildcardIndex_OK exercises the parser's wildcard-slice-access
// extension (`[*]`) in for-range contexts. Each case parses a Scriggo
// template containing a `for ... := range <chain-with-[*]>` statement
// and asserts the resulting AST holds the expected number of
// WildcardIndex nodes in the for-range expression.
func TestParseWildcardIndex_OK(t *testing.T) {
	tests := []struct {
		name          string
		src           string
		wantWildcards int
	}{
		{"single wildcard, single var",
			`{% for x := range y.Foo[*].Bar %}{% end %}`, 1},
		{"single wildcard, two vars",
			`{% for i, x := range y.Foo[*].Bar %}{% end %}`, 1},
		{"two wildcards",
			`{% for x := range y.A[*].B[*].C %}{% end %}`, 2},
		{"three wildcards (arbitrary depth)",
			`{% for x := range y.A[*].B[*].C[*].D %}{% end %}`, 3},
		{"wildcard with intermediate selector chain",
			`{% for x := range y.Outer.Mid[*].Inner.Leaf %}{% end %}`, 1},
		{"parenthesised root preserves wildcard context",
			`{% for x := range (y.Outer)[*].Inner %}{% end %}`, 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tree, _, err := ParseTemplateSource([]byte(tc.src), ast.FormatText, false, false)
			if err != nil {
				t.Fatalf("parse error: %v", err)
			}
			forRange := findFirstForRange(tree)
			if forRange == nil {
				t.Fatalf("no ForRange node in parsed tree")
			}
			got := countWildcards(forRange.Assignment.Rhs[0])
			if got != tc.wantWildcards {
				t.Fatalf("wildcards in range expression: got %d, want %d (src: %q)", got, tc.wantWildcards, tc.src)
			}
		})
	}
}

// TestParseWildcardIndex_Errors asserts the parser rejects `[*]` in
// every illegal position with a clear `:line:column: syntax error:`
// diagnostic.
func TestParseWildcardIndex_Errors(t *testing.T) {
	tests := []struct {
		name   string
		src    string
		errSub string // substring match — line/col vary by surrounding tokens
	}{
		{"trailing wildcard at end of chain",
			`{% for x := range y.Foo[*] %}{% end %}`,
			"[*] must be followed by a .field accessor"},
		{"wildcard in show statement",
			`{{ y.Foo[*].Bar }}`,
			"[*] only allowed in for-range expressions"},
		{"wildcard in var declaration",
			`{% var v = y.Foo[*].Bar %}`,
			"[*] only allowed in for-range expressions"},
		{"wildcard in if condition",
			`{% if y.Foo[*].Bar != nil %}{% end %}`,
			"[*] only allowed in for-range expressions"},
		{"wildcard in call argument",
			`{% var v = len(y.Foo[*].Bar) %}`,
			"[*] only allowed in for-range expressions"},
		{"wildcard in for-in template syntax",
			// `for x in expr` is a template-syntax variant of for-range;
			// the wildcard desugar pass only rewrites ast.ForRange, so
			// [*] in a ForIn range expression would survive into the
			// type checker and produce a confusing error. The parser
			// rejects it at parse time instead — same error path as
			// any other illegal position.
			`{% for x in y.Foo[*].Bar %}{% end %}`,
			"[*] only allowed in for-range expressions"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ParseTemplateSource([]byte(tc.src), ast.FormatText, false, false)
			if err == nil {
				t.Fatalf("expected parse error containing %q, got nil (src: %q)", tc.errSub, tc.src)
			}
			if !strings.Contains(err.Error(), tc.errSub) {
				t.Fatalf("error %q does not contain %q (src: %q)", err.Error(), tc.errSub, tc.src)
			}
			if !strings.HasPrefix(err.Error(), ":") || !strings.Contains(err.Error(), "syntax error:") {
				t.Fatalf("error %q lacks `:<line>:<col>: syntax error:` prefix (src: %q)", err.Error(), tc.src)
			}
		})
	}
}

// findFirstForRange returns the first ForRange node encountered by a
// depth-first walk of the tree, or nil.
func findFirstForRange(n ast.Node) *ast.ForRange {
	if n == nil {
		return nil
	}
	if fr, ok := n.(*ast.ForRange); ok {
		return fr
	}
	switch v := n.(type) {
	case *ast.Tree:
		for _, c := range v.Nodes {
			if fr := findFirstForRange(c); fr != nil {
				return fr
			}
		}
	case *ast.Block:
		for _, c := range v.Nodes {
			if fr := findFirstForRange(c); fr != nil {
				return fr
			}
		}
	}
	return nil
}

// countWildcards walks an expression and counts WildcardIndex nodes.
func countWildcards(e ast.Expression) int {
	n := 0
	var walk func(ast.Expression)
	walk = func(e ast.Expression) {
		if e == nil {
			return
		}
		switch v := e.(type) {
		case *ast.WildcardIndex:
			n++
			walk(v.Expr)
		case *ast.Selector:
			walk(v.Expr)
		case *ast.Index:
			walk(v.Expr)
			walk(v.Index)
		case *ast.UnaryOperator:
			walk(v.Expr)
		case *ast.BinaryOperator:
			walk(v.Expr1)
			walk(v.Expr2)
		case *ast.Call:
			walk(v.Func)
			for _, a := range v.Args {
				walk(a)
			}
		}
	}
	walk(e)
	return n
}
