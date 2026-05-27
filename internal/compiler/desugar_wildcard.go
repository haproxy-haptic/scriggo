// Copyright (c) 2025 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package compiler

import "gitlab.com/haproxy-haptic/scriggo/ast"

// desugarWildcards rewrites every for-range whose range expression
// contains one or more `[*]` wildcards into a chain of nested
// for-range statements. Each `[*]` produces one synthetic outer
// loop with a fresh `_, $wildN` iterator variable; the trailing
// accessor (whatever follows the last `[*]`) drives the innermost
// loop, which keeps the user's variable bindings, body, and else
// block.
//
// The pass runs after parsing and before type checking.
// WildcardIndex nodes surviving past this pass surface as
// syntax-style errors from the type checker (see
// checker_expressions.go).
func desugarWildcards(tree *ast.Tree, comp *compilation) error {
	tree.Nodes = desugarStmts(tree.Nodes, comp)
	return nil
}

// desugarStmts walks a slice of statement nodes, rewriting each
// in place. The slice is mutated and returned.
func desugarStmts(nodes []ast.Node, comp *compilation) []ast.Node {
	for i, n := range nodes {
		nodes[i] = desugarStmt(n, comp)
	}
	return nodes
}

// desugarStmt rewrites a single statement node and returns it
// (possibly the same pointer, possibly a replacement). For
// for-range nodes whose range expression contains wildcards, the
// returned node is a new synthetic outer ForRange wrapping nested
// inner ForRanges.
func desugarStmt(n ast.Node, comp *compilation) ast.Node {
	if n == nil {
		return nil
	}
	switch v := n.(type) {
	case *ast.ForRange:
		return desugarForRange(v, comp)
	case *ast.For:
		v.Body = desugarStmts(v.Body, comp)
		return v
	case *ast.ForIn:
		v.Body = desugarStmts(v.Body, comp)
		if v.Else != nil {
			v.Else.Nodes = desugarStmts(v.Else.Nodes, comp)
		}
		return v
	case *ast.Block:
		v.Nodes = desugarStmts(v.Nodes, comp)
		return v
	case *ast.If:
		if v.Then != nil {
			v.Then.Nodes = desugarStmts(v.Then.Nodes, comp)
		}
		if v.Else != nil {
			v.Else = desugarStmt(v.Else, comp)
		}
		return v
	case *ast.Switch:
		for _, c := range v.Cases {
			c.Body = desugarStmts(c.Body, comp)
		}
		return v
	case *ast.TypeSwitch:
		for _, c := range v.Cases {
			c.Body = desugarStmts(c.Body, comp)
		}
		return v
	case *ast.Select:
		for _, c := range v.Cases {
			c.Body = desugarStmts(c.Body, comp)
		}
		return v
	case *ast.Func:
		if v.Body != nil {
			v.Body.Nodes = desugarStmts(v.Body.Nodes, comp)
		}
		return v
	}
	return n
}

// desugarForRange handles a single ForRange. If its range
// expression contains a wildcard, it rewrites it into a synthetic
// outer ForRange wrapping the inner (whose RHS still may contain
// shallower wildcards — recursion handles them). If the range
// expression has no wildcard, the body is still recursively
// desugared and the original ForRange is returned.
func desugarForRange(fr *ast.ForRange, comp *compilation) ast.Node {
	if fr.Assignment == nil || len(fr.Assignment.Rhs) == 0 {
		// Range form `for { ... }` shouldn't have nil Assignment,
		// but tolerate it defensively. Recurse into body anyway.
		fr.Body = desugarStmts(fr.Body, comp)
		if fr.Else != nil {
			fr.Else.Nodes = desugarStmts(fr.Else.Nodes, comp)
		}
		return fr
	}
	wild := deepestWildcardInChain(fr.Assignment.Rhs[0])
	if wild == nil {
		// No wildcard in the range expression — preserve the
		// ForRange and recurse into its body / else.
		fr.Body = desugarStmts(fr.Body, comp)
		if fr.Else != nil {
			fr.Else.Nodes = desugarStmts(fr.Else.Nodes, comp)
		}
		return fr
	}

	// Generate the synthetic outer iterator name.
	name := comp.generateWildcardName()

	// Replace the wildcard with the synthetic identifier in
	// the range expression. Position of the new identifier
	// carries the wildcard's source position so type errors
	// surface at the user's `[*]` site.
	syntheticID := ast.NewIdentifier(wild.Position, name)
	newRhs := replaceNode(fr.Assignment.Rhs[0], wild, syntheticID)

	// Build the inner ForRange — same user vars / body / else,
	// new RHS with this wildcard substituted out.
	innerAssign := ast.NewAssignment(
		fr.Assignment.Position,
		fr.Assignment.Lhs,
		fr.Assignment.Type,
		[]ast.Expression{newRhs},
	)
	innerFR := ast.NewForRange(fr.Position, innerAssign, fr.Body, fr.Else)

	// Recurse — the inner's RHS may still contain shallower
	// wildcards. Each recursion strips exactly one wildcard.
	desugaredInner := desugarStmt(innerFR, comp)

	// Build the outer synthetic ForRange.
	outerLhs := []ast.Expression{
		ast.NewIdentifier(wild.Position, "_"),
		ast.NewIdentifier(wild.Position, name),
	}
	outerAssign := ast.NewAssignment(
		wild.Position,
		outerLhs,
		ast.AssignmentDeclaration,
		[]ast.Expression{wild.Expr},
	)
	outerFR := ast.NewForRange(wild.Position, outerAssign, []ast.Node{desugaredInner}, nil)
	return outerFR
}

// deepestWildcardInChain walks down a selector / wildcard chain
// looking for the WildcardIndex closest to the chain's root
// (i.e. the one whose own .Expr contains no further wildcards).
// Returns nil if the chain has no wildcards.
//
// Only descends through `Selector.Expr` and `WildcardIndex.Expr`
// — the chain operators. `Index.Expr` is also chain-passing for
// ordinary indices like `a[0]`, but `[*]` itself stops the
// descent (we've found a wildcard).
func deepestWildcardInChain(e ast.Expression) *ast.WildcardIndex {
	var deepest *ast.WildcardIndex
	cur := e
	for cur != nil {
		switch v := cur.(type) {
		case *ast.WildcardIndex:
			deepest = v
			cur = v.Expr
		case *ast.Selector:
			cur = v.Expr
		case *ast.Index:
			// Ordinary `[i]` index: chain-passing for descent
			// purposes. The sub-expression `i` was rejected at
			// parse time if it contained `[*]` (parser flag
			// cleared inside index expressions), so we only
			// follow .Expr here.
			cur = v.Expr
		default:
			cur = nil
		}
	}
	return deepest
}

// replaceNode walks an expression tree following chain operators
// (.Expr fields) and replaces `target` with `replacement` if
// found. Returns the (possibly new) root.
func replaceNode(root ast.Expression, target *ast.WildcardIndex, replacement ast.Expression) ast.Expression {
	if root == nil {
		return nil
	}
	if w, ok := root.(*ast.WildcardIndex); ok && w == target {
		return replacement
	}
	switch v := root.(type) {
	case *ast.Selector:
		v.Expr = replaceNode(v.Expr, target, replacement)
		return v
	case *ast.WildcardIndex:
		v.Expr = replaceNode(v.Expr, target, replacement)
		return v
	case *ast.Index:
		v.Expr = replaceNode(v.Expr, target, replacement)
		return v
	}
	return root
}
