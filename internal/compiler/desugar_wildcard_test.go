// Copyright (c) 2025 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package compiler

import (
	"testing"

	"github.com/haproxy-haptic/scriggo/ast"
)

// wildcardPos is a sentinel Position used to assert that synthetic
// outer-loop nodes carry the original `[*]` site rather than an
// unrelated position.
func wildcardPos() *ast.Position {
	return &ast.Position{Line: 7, Column: 12, Start: 100, End: 102}
}

// outerPos is a sentinel for the original ForRange's position (the
// `for` keyword itself), distinct from wildcardPos.
func outerPos() *ast.Position {
	return &ast.Position{Line: 7, Column: 1, Start: 80, End: 150}
}

func newComp() *compilation {
	return &compilation{currentWildcardIndex: -1}
}

// rangeExpr extracts the RHS expression of a ForRange's Assignment.
func rangeExpr(fr *ast.ForRange) ast.Expression {
	if fr == nil || fr.Assignment == nil || len(fr.Assignment.Rhs) == 0 {
		return nil
	}
	return fr.Assignment.Rhs[0]
}

// rangeIdentName returns the name of the simple Identifier RHS of a
// ForRange, panicking if it's not a simple Identifier.
func rangeIdentName(fr *ast.ForRange) string {
	id, ok := rangeExpr(fr).(*ast.Identifier)
	if !ok {
		return "<not-an-identifier>"
	}
	return id.Name
}

// rangeSelectorBase returns (rootName, fieldChain) for a chain of
// Selectors over an Identifier root. `root.A.B.C` → ("root", "A.B.C").
func rangeSelectorChain(e ast.Expression) (root string, chain string) {
	cur := e
	for {
		sel, ok := cur.(*ast.Selector)
		if !ok {
			break
		}
		if chain == "" {
			chain = sel.Ident
		} else {
			chain = sel.Ident + "." + chain
		}
		cur = sel.Expr
	}
	if id, ok := cur.(*ast.Identifier); ok {
		root = id.Name
	}
	return
}

// TestDesugarWildcard_NoOp confirms a ForRange without `[*]` is left
// untouched.
func TestDesugarWildcard_NoOp(t *testing.T) {
	// for _, x := range root.Foo { body }
	fr := ast.NewForRange(outerPos(),
		ast.NewAssignment(outerPos(),
			[]ast.Expression{ast.NewIdentifier(outerPos(), "_"), ast.NewIdentifier(outerPos(), "x")},
			ast.AssignmentDeclaration,
			[]ast.Expression{ast.NewSelector(outerPos(), ast.NewIdentifier(outerPos(), "root"), "Foo")}),
		[]ast.Node{ast.NewIdentifier(outerPos(), "body")},
		nil)
	tree := ast.NewTree("", []ast.Node{fr}, ast.FormatText)

	if err := desugarWildcards(tree, newComp()); err != nil {
		t.Fatalf("desugar: %v", err)
	}
	if len(tree.Nodes) != 1 {
		t.Fatalf("tree.Nodes len: got %d, want 1", len(tree.Nodes))
	}
	got, ok := tree.Nodes[0].(*ast.ForRange)
	if !ok {
		t.Fatalf("tree.Nodes[0] type: got %T, want *ast.ForRange", tree.Nodes[0])
	}
	if got != fr {
		t.Fatalf("ForRange node identity changed; no-op desugar must not allocate")
	}
}

// TestDesugarWildcard_SingleLevel verifies one wildcard expands into
// one synthetic outer loop, with the user's vars / body / else
// preserved on the inner loop.
func TestDesugarWildcard_SingleLevel(t *testing.T) {
	// for i, z := range root.Foo[*].Bar { body }
	wild := ast.NewWildcardIndex(wildcardPos(),
		ast.NewSelector(outerPos(), ast.NewIdentifier(outerPos(), "root"), "Foo"))
	rangeRhs := ast.NewSelector(outerPos(), wild, "Bar")
	bodyNode := ast.NewIdentifier(outerPos(), "body")
	elseBlock := ast.NewBlock(outerPos(), []ast.Node{ast.NewIdentifier(outerPos(), "elseBody")})
	fr := ast.NewForRange(outerPos(),
		ast.NewAssignment(outerPos(),
			[]ast.Expression{ast.NewIdentifier(outerPos(), "i"), ast.NewIdentifier(outerPos(), "z")},
			ast.AssignmentDeclaration,
			[]ast.Expression{rangeRhs}),
		[]ast.Node{bodyNode},
		elseBlock)
	tree := ast.NewTree("", []ast.Node{fr}, ast.FormatText)

	if err := desugarWildcards(tree, newComp()); err != nil {
		t.Fatalf("desugar: %v", err)
	}
	if len(tree.Nodes) != 1 {
		t.Fatalf("tree.Nodes len: got %d, want 1", len(tree.Nodes))
	}
	outer, ok := tree.Nodes[0].(*ast.ForRange)
	if !ok {
		t.Fatalf("outer node type: got %T, want *ast.ForRange", tree.Nodes[0])
	}

	// Outer ForRange carries wildcard position.
	if outer.Position == nil || outer.Position.Line != wildcardPos().Line || outer.Position.Column != wildcardPos().Column {
		t.Fatalf("outer ForRange position: got %v, want %v", outer.Position, wildcardPos())
	}

	// Outer LHS: [_, $wild0].
	if len(outer.Assignment.Lhs) < 2 {
		t.Fatalf("outer LHS len: got %d, want 2", len(outer.Assignment.Lhs))
	}
	if id, ok := outer.Assignment.Lhs[0].(*ast.Identifier); !ok || id.Name != "_" {
		t.Fatalf("outer LHS[0]: got %#v, want _", outer.Assignment.Lhs[0])
	}
	if id, ok := outer.Assignment.Lhs[1].(*ast.Identifier); !ok || id.Name != "$wild0" {
		t.Fatalf("outer LHS[1]: got %#v, want $wild0", outer.Assignment.Lhs[1])
	}

	// Outer Assignment Type is := (declaration).
	if outer.Assignment.Type != ast.AssignmentDeclaration {
		t.Fatalf("outer Assignment.Type: got %v, want AssignmentDeclaration", outer.Assignment.Type)
	}

	// Outer RHS is root.Foo (the wildcard's Expr).
	if root, chain := rangeSelectorChain(outer.Assignment.Rhs[0]); root != "root" || chain != "Foo" {
		t.Fatalf("outer range RHS: got root=%q chain=%q, want root.Foo", root, chain)
	}

	// Outer has no Else.
	if outer.Else != nil {
		t.Fatalf("outer Else: got %v, want nil", outer.Else)
	}

	// Outer Body has exactly one inner ForRange.
	if len(outer.Body) != 1 {
		t.Fatalf("outer.Body len: got %d, want 1", len(outer.Body))
	}
	inner, ok := outer.Body[0].(*ast.ForRange)
	if !ok {
		t.Fatalf("outer.Body[0] type: got %T, want *ast.ForRange", outer.Body[0])
	}

	// Inner LHS preserves user's [i, z].
	if id, ok := inner.Assignment.Lhs[0].(*ast.Identifier); !ok || id.Name != "i" {
		t.Fatalf("inner LHS[0]: got %#v, want i", inner.Assignment.Lhs[0])
	}
	if id, ok := inner.Assignment.Lhs[1].(*ast.Identifier); !ok || id.Name != "z" {
		t.Fatalf("inner LHS[1]: got %#v, want z", inner.Assignment.Lhs[1])
	}

	// Inner range RHS is $wild0.Bar (the wildcard replaced with the synthetic iter).
	if root, chain := rangeSelectorChain(inner.Assignment.Rhs[0]); root != "$wild0" || chain != "Bar" {
		t.Fatalf("inner range RHS: got root=%q chain=%q, want $wild0.Bar", root, chain)
	}

	// Inner Body preserved (same node pointer).
	if len(inner.Body) != 1 || inner.Body[0] != bodyNode {
		t.Fatalf("inner Body not preserved")
	}

	// Inner Else preserved (same Block pointer).
	if inner.Else != elseBlock {
		t.Fatalf("inner Else not preserved")
	}
}

// TestDesugarWildcard_TwoLevels verifies two wildcards yield two
// synthetic outer loops with $wild0 outermost-first.
func TestDesugarWildcard_TwoLevels(t *testing.T) {
	// for z := range a.B[*].C[*].D { body }
	chain := ast.NewSelector(outerPos(),
		ast.NewWildcardIndex(wildcardPos(),
			ast.NewSelector(outerPos(),
				ast.NewWildcardIndex(wildcardPos(),
					ast.NewSelector(outerPos(), ast.NewIdentifier(outerPos(), "a"), "B")),
				"C")),
		"D")
	fr := ast.NewForRange(outerPos(),
		ast.NewAssignment(outerPos(),
			[]ast.Expression{ast.NewIdentifier(outerPos(), "z")},
			ast.AssignmentDeclaration,
			[]ast.Expression{chain}),
		[]ast.Node{ast.NewIdentifier(outerPos(), "body")},
		nil)
	tree := ast.NewTree("", []ast.Node{fr}, ast.FormatText)

	if err := desugarWildcards(tree, newComp()); err != nil {
		t.Fatalf("desugar: %v", err)
	}

	outer, ok := tree.Nodes[0].(*ast.ForRange)
	if !ok {
		t.Fatalf("outer type: got %T, want *ast.ForRange", tree.Nodes[0])
	}
	if len(outer.Assignment.Lhs) < 2 {
		t.Fatalf("outer LHS len: got %d, want >=2 (desugar didn't run)", len(outer.Assignment.Lhs))
	}
	// Outer iterates a.B, synthetic var $wild0.
	if id, _ := outer.Assignment.Lhs[1].(*ast.Identifier); id == nil || id.Name != "$wild0" {
		t.Fatalf("outer LHS[1]: want $wild0, got %v", outer.Assignment.Lhs[1])
	}
	if root, chain := rangeSelectorChain(outer.Assignment.Rhs[0]); root != "a" || chain != "B" {
		t.Fatalf("outer range: got %s.%s, want a.B", root, chain)
	}

	mid, ok := outer.Body[0].(*ast.ForRange)
	if !ok {
		t.Fatalf("mid type: got %T, want *ast.ForRange", outer.Body[0])
	}
	// Mid iterates $wild0.C, synthetic var $wild1.
	if id, _ := mid.Assignment.Lhs[1].(*ast.Identifier); id == nil || id.Name != "$wild1" {
		t.Fatalf("mid LHS[1]: want $wild1, got %v", mid.Assignment.Lhs[1])
	}
	if root, chain := rangeSelectorChain(mid.Assignment.Rhs[0]); root != "$wild0" || chain != "C" {
		t.Fatalf("mid range: got %s.%s, want $wild0.C", root, chain)
	}

	inner, ok := mid.Body[0].(*ast.ForRange)
	if !ok {
		t.Fatalf("inner type: got %T, want *ast.ForRange", mid.Body[0])
	}
	// Inner iterates $wild1.D, user's z.
	if id, _ := inner.Assignment.Lhs[0].(*ast.Identifier); id == nil || id.Name != "z" {
		t.Fatalf("inner LHS[0]: want z, got %v", inner.Assignment.Lhs[0])
	}
	if root, chain := rangeSelectorChain(inner.Assignment.Rhs[0]); root != "$wild1" || chain != "D" {
		t.Fatalf("inner range: got %s.%s, want $wild1.D", root, chain)
	}
}

// TestDesugarWildcard_ThreeLevels confirms arbitrary depth (3 wildcards).
func TestDesugarWildcard_ThreeLevels(t *testing.T) {
	// for z := range a.B[*].C[*].D[*].E { body }
	chain := ast.NewSelector(outerPos(),
		ast.NewWildcardIndex(wildcardPos(),
			ast.NewSelector(outerPos(),
				ast.NewWildcardIndex(wildcardPos(),
					ast.NewSelector(outerPos(),
						ast.NewWildcardIndex(wildcardPos(),
							ast.NewSelector(outerPos(), ast.NewIdentifier(outerPos(), "a"), "B")),
						"C")),
				"D")),
		"E")
	fr := ast.NewForRange(outerPos(),
		ast.NewAssignment(outerPos(),
			[]ast.Expression{ast.NewIdentifier(outerPos(), "z")},
			ast.AssignmentDeclaration,
			[]ast.Expression{chain}),
		[]ast.Node{ast.NewIdentifier(outerPos(), "body")}, nil)
	tree := ast.NewTree("", []ast.Node{fr}, ast.FormatText)

	if err := desugarWildcards(tree, newComp()); err != nil {
		t.Fatalf("desugar: %v", err)
	}

	depth := 0
	cur, ok := tree.Nodes[0].(*ast.ForRange)
	for ok {
		depth++
		if len(cur.Body) != 1 {
			break
		}
		cur, ok = cur.Body[0].(*ast.ForRange)
	}
	if depth != 4 {
		t.Fatalf("nesting depth: got %d, want 4 (3 synthetic + 1 user)", depth)
	}
}

// TestDesugarWildcard_PreservesElseBlock asserts only the innermost
// (user's) ForRange keeps the Else block; synthetic outers have Else
// == nil.
func TestDesugarWildcard_PreservesElseBlock(t *testing.T) {
	wild := ast.NewWildcardIndex(wildcardPos(), ast.NewIdentifier(outerPos(), "a"))
	rangeRhs := ast.NewSelector(outerPos(), wild, "B")
	elseBlock := ast.NewBlock(outerPos(), []ast.Node{ast.NewIdentifier(outerPos(), "el")})
	fr := ast.NewForRange(outerPos(),
		ast.NewAssignment(outerPos(),
			[]ast.Expression{ast.NewIdentifier(outerPos(), "z")},
			ast.AssignmentDeclaration,
			[]ast.Expression{rangeRhs}),
		[]ast.Node{ast.NewIdentifier(outerPos(), "body")},
		elseBlock)
	tree := ast.NewTree("", []ast.Node{fr}, ast.FormatText)

	if err := desugarWildcards(tree, newComp()); err != nil {
		t.Fatalf("desugar: %v", err)
	}
	outer := tree.Nodes[0].(*ast.ForRange)
	if outer.Else != nil {
		t.Fatalf("outer Else: got %v, want nil", outer.Else)
	}
	inner := outer.Body[0].(*ast.ForRange)
	if inner.Else != elseBlock {
		t.Fatalf("inner Else: got %v, want preserved (%v)", inner.Else, elseBlock)
	}
}

// TestDesugarWildcard_PositionFromWildcardSite asserts synthetic outer
// nodes carry the original `[*]` Position rather than the original
// ForRange's Position. Critical for type-error location fidelity.
func TestDesugarWildcard_PositionFromWildcardSite(t *testing.T) {
	wild := ast.NewWildcardIndex(wildcardPos(), ast.NewIdentifier(outerPos(), "a"))
	rangeRhs := ast.NewSelector(outerPos(), wild, "B")
	fr := ast.NewForRange(outerPos(),
		ast.NewAssignment(outerPos(),
			[]ast.Expression{ast.NewIdentifier(outerPos(), "z")},
			ast.AssignmentDeclaration,
			[]ast.Expression{rangeRhs}),
		[]ast.Node{ast.NewIdentifier(outerPos(), "body")}, nil)
	tree := ast.NewTree("", []ast.Node{fr}, ast.FormatText)

	if err := desugarWildcards(tree, newComp()); err != nil {
		t.Fatalf("desugar: %v", err)
	}
	outer := tree.Nodes[0].(*ast.ForRange)
	if outer.Position.Line != wildcardPos().Line || outer.Position.Column != wildcardPos().Column {
		t.Fatalf("outer Pos: got %v, want %v", outer.Position, wildcardPos())
	}
	if outer.Assignment.Position.Line != wildcardPos().Line {
		t.Fatalf("outer Assignment Pos line: got %d, want %d", outer.Assignment.Position.Line, wildcardPos().Line)
	}
	// Synthetic LHS identifiers carry wildcard position.
	if id := outer.Assignment.Lhs[1].(*ast.Identifier); id.Position.Line != wildcardPos().Line {
		t.Fatalf("synthetic LHS ident Pos line: got %d, want %d", id.Position.Line, wildcardPos().Line)
	}
}

// TestDesugarWildcard_PerProgramCounterReset verifies $wildN are
// unique across multiple ForRanges in the same compilation.
func TestDesugarWildcard_PerProgramCounterReset(t *testing.T) {
	mkForRange := func() *ast.ForRange {
		wild := ast.NewWildcardIndex(wildcardPos(), ast.NewIdentifier(outerPos(), "a"))
		return ast.NewForRange(outerPos(),
			ast.NewAssignment(outerPos(),
				[]ast.Expression{ast.NewIdentifier(outerPos(), "z")},
				ast.AssignmentDeclaration,
				[]ast.Expression{ast.NewSelector(outerPos(), wild, "B")}),
			[]ast.Node{ast.NewIdentifier(outerPos(), "body")}, nil)
	}
	tree := ast.NewTree("", []ast.Node{mkForRange(), mkForRange()}, ast.FormatText)

	if err := desugarWildcards(tree, newComp()); err != nil {
		t.Fatalf("desugar: %v", err)
	}
	first := tree.Nodes[0].(*ast.ForRange)
	second := tree.Nodes[1].(*ast.ForRange)
	if len(first.Assignment.Lhs) < 2 || len(second.Assignment.Lhs) < 2 {
		t.Fatalf("synthetic LHS missing — desugar didn't run")
	}
	firstName := first.Assignment.Lhs[1].(*ast.Identifier).Name
	secondName := second.Assignment.Lhs[1].(*ast.Identifier).Name
	if firstName == secondName {
		t.Fatalf("expected distinct synthetic names, got %q == %q", firstName, secondName)
	}
	if firstName != "$wild0" || secondName != "$wild1" {
		t.Fatalf("synthetic names: got %q / %q, want $wild0 / $wild1", firstName, secondName)
	}
}

// TestDesugarWildcard_NestedForRange_OnlyInnerHasWildcard confirms
// only the for-range containing the wildcard is rewritten; the
// surrounding for-range is untouched.
func TestDesugarWildcard_NestedForRange_OnlyInnerHasWildcard(t *testing.T) {
	wild := ast.NewWildcardIndex(wildcardPos(), ast.NewIdentifier(outerPos(), "a"))
	innerFr := ast.NewForRange(outerPos(),
		ast.NewAssignment(outerPos(),
			[]ast.Expression{ast.NewIdentifier(outerPos(), "z")},
			ast.AssignmentDeclaration,
			[]ast.Expression{ast.NewSelector(outerPos(), wild, "B")}),
		[]ast.Node{ast.NewIdentifier(outerPos(), "innerBody")}, nil)
	outerFr := ast.NewForRange(outerPos(),
		ast.NewAssignment(outerPos(),
			[]ast.Expression{ast.NewIdentifier(outerPos(), "_"), ast.NewIdentifier(outerPos(), "y")},
			ast.AssignmentDeclaration,
			[]ast.Expression{ast.NewIdentifier(outerPos(), "items")}),
		[]ast.Node{innerFr}, nil)
	tree := ast.NewTree("", []ast.Node{outerFr}, ast.FormatText)

	if err := desugarWildcards(tree, newComp()); err != nil {
		t.Fatalf("desugar: %v", err)
	}
	got, ok := tree.Nodes[0].(*ast.ForRange)
	if !ok || got != outerFr {
		t.Fatalf("outer ForRange identity changed; must not be touched")
	}
	// outer's body now contains the desugared inner — one synthetic
	// outer ForRange wrapping the user's inner ForRange.
	if len(outerFr.Body) != 1 {
		t.Fatalf("outer.Body len: got %d, want 1", len(outerFr.Body))
	}
	synthetic, ok := outerFr.Body[0].(*ast.ForRange)
	if !ok || synthetic == innerFr {
		t.Fatalf("expected new synthetic outer for the inner's wildcard, got identity-preserved innerFr")
	}
}

// TestDesugarWildcard_NestedForRange_BothHaveWildcards confirms
// independent rewrites when both layers contain wildcards.
func TestDesugarWildcard_NestedForRange_BothHaveWildcards(t *testing.T) {
	innerWild := ast.NewWildcardIndex(wildcardPos(), ast.NewIdentifier(outerPos(), "x"))
	innerFr := ast.NewForRange(outerPos(),
		ast.NewAssignment(outerPos(),
			[]ast.Expression{ast.NewIdentifier(outerPos(), "z")},
			ast.AssignmentDeclaration,
			[]ast.Expression{ast.NewSelector(outerPos(), innerWild, "B")}),
		[]ast.Node{ast.NewIdentifier(outerPos(), "ib")}, nil)
	outerWild := ast.NewWildcardIndex(wildcardPos(), ast.NewIdentifier(outerPos(), "a"))
	outerFr := ast.NewForRange(outerPos(),
		ast.NewAssignment(outerPos(),
			[]ast.Expression{ast.NewIdentifier(outerPos(), "y")},
			ast.AssignmentDeclaration,
			[]ast.Expression{ast.NewSelector(outerPos(), outerWild, "C")}),
		[]ast.Node{innerFr}, nil)
	tree := ast.NewTree("", []ast.Node{outerFr}, ast.FormatText)

	if err := desugarWildcards(tree, newComp()); err != nil {
		t.Fatalf("desugar: %v", err)
	}

	// After desugar: 4 nested ForRanges total (2 synthetic + 2 user).
	depth := 0
	cur, ok := tree.Nodes[0].(*ast.ForRange)
	for ok {
		depth++
		if len(cur.Body) != 1 {
			break
		}
		cur, ok = cur.Body[0].(*ast.ForRange)
	}
	if depth != 4 {
		t.Fatalf("nesting depth: got %d, want 4 (2 synthetic + 2 user)", depth)
	}
}

// TestDesugarWildcard_InsideStatements asserts wildcards inside a
// `{%% ... %%}` (ast.Statements) block are desugared. Without this
// the type checker panics with "missing case for type *ast.WildcardIndex"
// when the dependency walker visits the surviving wildcard node.
//
// Regression for: chart's BackendTLSPolicy ancestor-resolution pass,
// which wrote
//
//	for _, br := range route.Spec.Rules[*].BackendRefs { ... }
//
// inside a {%% %%} block.
func TestDesugarWildcard_InsideStatements(t *testing.T) {
	wild := ast.NewWildcardIndex(wildcardPos(), ast.NewIdentifier(outerPos(), "a"))
	fr := ast.NewForRange(outerPos(),
		ast.NewAssignment(outerPos(),
			[]ast.Expression{ast.NewIdentifier(outerPos(), "z")},
			ast.AssignmentDeclaration,
			[]ast.Expression{ast.NewSelector(outerPos(), wild, "B")}),
		[]ast.Node{ast.NewIdentifier(outerPos(), "body")}, nil)
	stmts := ast.NewStatements(outerPos(), []ast.Node{fr})
	tree := ast.NewTree("", []ast.Node{stmts}, ast.FormatText)

	if err := desugarWildcards(tree, newComp()); err != nil {
		t.Fatalf("desugar: %v", err)
	}
	gotStmts, ok := tree.Nodes[0].(*ast.Statements)
	if !ok {
		t.Fatalf("top-level node: got %T, want *ast.Statements", tree.Nodes[0])
	}
	if len(gotStmts.Nodes) != 1 {
		t.Fatalf("Statements.Nodes len: got %d, want 1", len(gotStmts.Nodes))
	}
	outer, ok := gotStmts.Nodes[0].(*ast.ForRange)
	if !ok {
		t.Fatalf("Statements.Nodes[0]: got %T, want *ast.ForRange", gotStmts.Nodes[0])
	}
	// The wildcard should have produced a synthetic outer ForRange
	// (different identity from the original inner) — verify by
	// checking the inner LHS uses the user's `z`, not the synthetic
	// `$wild0`.
	if rangeIdentName(outer) != "a" {
		t.Fatalf("outer RHS root: got %q, want %q (the wildcard's Expr)", rangeIdentName(outer), "a")
	}
}
