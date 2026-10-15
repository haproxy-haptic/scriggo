// Copyright (c) 2025 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package compiler

import "github.com/haproxy-haptic/scriggo/ast"

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
	case *ast.Statements:
		// `{%% ... %%}` multi-line statement blocks. Parser nests
		// for-range / for / etc. statements inside the Nodes slice;
		// recurse so wildcards inside survive into the type checker.
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
	case *ast.Render:
		// `render "path"` carries an expanded sub-tree (filled
		// in by the parser's expand pass). Recurse so wildcards
		// inside rendered snippets get desugared.
		if v.Tree != nil {
			v.Tree.Nodes = desugarStmts(v.Tree.Nodes, comp)
		}
		return v
	case *ast.Import:
		// `import "path" for X` carries an expanded sub-tree
		// holding the imported file's macro / var declarations.
		// Recurse so wildcards inside imported macros get
		// desugared.
		if v.Tree != nil {
			v.Tree.Nodes = desugarStmts(v.Tree.Nodes, comp)
		}
		return v
	case *ast.Extends:
		// `extends "path"` carries an expanded sub-tree of the
		// parent layout. Recurse so wildcards in the parent's
		// macros get desugared.
		if v.Tree != nil {
			v.Tree.Nodes = desugarStmts(v.Tree.Nodes, comp)
		}
		return v
	case *ast.RenderGlob:
		// render_glob expands at parse-time into a slice of
		// Render nodes, each with its own sub-tree.
		for _, r := range v.Renders {
			if r != nil && r.Tree != nil {
				r.Tree.Nodes = desugarStmts(r.Tree.Nodes, comp)
			}
		}
		return v
	case *ast.Show:
		// `{{ render "..." }}` parses as a Show whose Expressions
		// include Render / RenderGlob nodes. Statement-level
		// recursion alone misses those sub-trees.
		for _, e := range v.Expressions {
			desugarExprSubTrees(e, comp)
		}
		return v
	case *ast.Var:
		for _, e := range v.Rhs {
			desugarExprSubTrees(e, comp)
		}
		return v
	case *ast.Assignment:
		for _, e := range v.Rhs {
			desugarExprSubTrees(e, comp)
		}
		return v
	}
	return n
}

// desugarExprSubTrees walks an expression looking for Render /
// RenderGlob nodes and recurses into their sub-trees. Render is an
// Expression node (it appears in show-statement and
// assignment-RHS positions), so statement-level recursion alone
// misses them.
func desugarExprSubTrees(e ast.Node, comp *compilation) {
	if e == nil {
		return
	}
	switch v := e.(type) {
	case *ast.Render:
		if v.Tree != nil {
			v.Tree.Nodes = desugarStmts(v.Tree.Nodes, comp)
		}
	case *ast.RenderGlob:
		for _, r := range v.Renders {
			if r != nil && r.Tree != nil {
				r.Tree.Nodes = desugarStmts(r.Tree.Nodes, comp)
			}
		}
	}
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
