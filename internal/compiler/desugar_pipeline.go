// Copyright (c) 2025 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package compiler

import "gitlab.com/haproxy-haptic/scriggo/ast"

// Compile-time lowering of collection pipelines.
//
// `src | filter(f) | map(g)` normally runs as two native calls, each invoking
// a template closure once per element. That crossing — native code calling
// back into the VM — is the dominant cost of a pipeline: it is what makes one
// run ~1.7x slower than the equivalent hand-written loop even after the VM
// pool removed its allocation.
//
// This pass rewrites such a chain into the loop a human would have written,
// fused into a single pass with no intermediate slices. The closures are
// hoisted and called from inside the loop, so the calls stay within the VM
// instead of crossing the native boundary.
//
// It runs after parsing and before type checking, alongside desugarWildcards.
// That placement means no type information is available — the pass works only
// because a lowerable stage's closure is a literal whose result type is
// written in the source, so the accumulator's element type can be lifted
// straight out of the AST. See ADR-0018 (haptic).
//
// Only stages that are genuinely a loop are lowered. unique / unique_by /
// group_by need a map and sort_by needs a sort; those keep their native
// implementation, and a chain is lowered only up to the first one.
//
// Following Kotlin's `inline fun` rule, a stage is lowered only when its
// function argument is a literal closure at the call site. `filter(x, someVar)`
// cannot be lowered and stays a native call.

// pipelineStage is one lowerable stage of a chain.
type pipelineStage struct {
	kind    string    // "map", "filter", "reject" or "flat_map"; "" when not lowerable
	fn      *ast.Func // the literal closure; nil when not lowerable
	call    *ast.Call // the original call, for re-applying an unlowered stage
	piped   bool      // whether the source arrived through `|` rather than as arg 0
	callPos *ast.Position
}

// lowerableStages are the stages whose semantics are exactly a loop body.
var lowerableStages = map[string]bool{
	"map": true, "filter": true, "reject": true, "flat_map": true,
}

// desugarPipelines rewrites lowerable pipeline chains in a tree.
func desugarPipelines(tree *ast.Tree, comp *compilation) error {
	tree.Nodes = desugarPipelineStmts(tree.Nodes, comp)
	return nil
}

// desugarPipelineStmts walks statements, replacing any whose initialiser is a
// lowerable chain with the statements that implement it. A single input
// statement can expand to several, so the slice is rebuilt rather than mutated
// in place.
func desugarPipelineStmts(nodes []ast.Node, comp *compilation) []ast.Node {
	out := make([]ast.Node, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, desugarPipelineStmt(n, comp)...)
	}
	return out
}

// desugarPipelineStmt returns the replacement statements for n.
func desugarPipelineStmt(n ast.Node, comp *compilation) []ast.Node {
	if n == nil {
		return nil
	}
	switch v := n.(type) {
	case *ast.Var:
		if lowered := lowerVarDeclaration(v, comp); lowered != nil {
			return lowered
		}
	case *ast.Block:
		v.Nodes = desugarPipelineStmts(v.Nodes, comp)
	case *ast.Statements:
		v.Nodes = desugarPipelineStmts(v.Nodes, comp)
	case *ast.For:
		v.Body = desugarPipelineStmts(v.Body, comp)
	case *ast.ForRange:
		v.Body = desugarPipelineStmts(v.Body, comp)
		if v.Else != nil {
			v.Else.Nodes = desugarPipelineStmts(v.Else.Nodes, comp)
		}
		// A chain in the range head is the shape chart code reaches for most
		// — `for _, x := range src | flat_map(…) | reject(…)`. Lowering only
		// var declarations would silently leave it on the native path, which
		// is exactly the sort of "works, but only if you spell it the other
		// way" trap this pass exists to remove. Go evaluates a range
		// expression once, so hoisting it ahead of the loop is equivalent.
		if v.Assignment != nil && len(v.Assignment.Rhs) == 1 {
			if pre, replacement := lowerChainToTemp(v.Assignment.Rhs[0], comp); pre != nil {
				v.Assignment.Rhs[0] = replacement
				return append(pre, v)
			}
		}
	case *ast.ForIn:
		v.Body = desugarPipelineStmts(v.Body, comp)
		if v.Else != nil {
			v.Else.Nodes = desugarPipelineStmts(v.Else.Nodes, comp)
		}
	case *ast.If:
		if v.Then != nil {
			v.Then.Nodes = desugarPipelineStmts(v.Then.Nodes, comp)
		}
		if v.Else != nil {
			if els := desugarPipelineStmt(v.Else, comp); len(els) == 1 {
				v.Else = els[0]
			}
		}
	case *ast.Show:
		// `{{ src | filter(…) }}`. Hoisting one show expression can emit
		// several statements, which is why this walker returns a slice.
		var pre []ast.Node
		for i, e := range v.Expressions {
			if nodes, replacement := lowerChainToTemp(e, comp); nodes != nil {
				pre = append(pre, nodes...)
				v.Expressions[i] = replacement
			}
		}
		if pre != nil {
			return append(pre, v)
		}
	case *ast.Assignment:
		// `x = src | filter(…)` on an already-declared variable.
		if len(v.Lhs) == 1 && len(v.Rhs) == 1 && v.Type == ast.AssignmentSimple {
			if pre, replacement := lowerChainToTemp(v.Rhs[0], comp); pre != nil {
				v.Rhs[0] = replacement
				return append(pre, v)
			}
		}
	case *ast.Func:
		if v.Body != nil {
			v.Body.Nodes = desugarPipelineStmts(v.Body.Nodes, comp)
		}
	}
	return []ast.Node{n}
}

// lowerVarDeclaration rewrites `var x = <chain>` when the chain is lowerable,
// returning the replacement statements, or nil to leave the declaration alone.
//
// Only the single-variable, single-value form is handled. A multi-value
// declaration cannot be a pipeline: every lowerable stage yields one value.
func lowerVarDeclaration(v *ast.Var, comp *compilation) []ast.Node {
	if len(v.Lhs) != 1 || len(v.Rhs) != 1 || v.Type != nil {
		return nil
	}
	source, stages := splitPipeline(v.Rhs[0])

	// Lower the longest run of lowerable stages starting at the source. A
	// chain ending in unique() or sort_by() still gets its map/filter work
	// fused; only the tail stays native. Lowering must start at the source
	// because that is the only point whose input expression is already
	// evaluated — a lowerable stage sitting *above* a native one would have
	// to consume that stage's result, which is exactly what the temp is for.
	n := 0
	for n < len(stages) && stages[n].fn != nil {
		n++
	}
	if n == 0 {
		return nil
	}

	if n == len(stages) {
		return buildLoweredPipeline(v.Lhs[0], source, stages, comp)
	}

	// Lower the prefix into a temporary, then re-apply the remaining stages
	// to it so the native helpers see exactly what they saw before.
	tmp := ast.NewIdentifier(v.Position, comp.generatePipelineName()+"t")
	nodes := buildLoweredPipeline(tmp, source, stages[:n], comp)
	rest := ast.Expression(tmp)
	for _, st := range stages[n:] {
		rest = reapplyStage(st, rest)
	}
	return append(nodes, ast.NewVar(v.Position, v.Lhs, nil, []ast.Expression{rest}))
}

// splitPipeline decomposes an expression into its source and the chain of
// calls applied to it, innermost first. A call that is not a lowerable stage
// is recorded with a nil fn so the caller can stop the lowerable prefix there
// and re-apply the rest.
//
// Both spellings are recognised: `src | filter(f)` (a pipe binary operator
// whose right side is the call) and `filter(src, f)` (a direct call).
func splitPipeline(expr ast.Expression) (ast.Expression, []pipelineStage) {
	var stages []pipelineStage
	for {
		call, source := pipelineCall(expr)
		if call == nil {
			break
		}
		st := pipelineStage{call: call, piped: source != nil, callPos: call.Position}
		if name, ok := call.Func.(*ast.Identifier); ok && lowerableStages[name.Name] {
			if fn := literalClosureArg(call, st.piped); fn != nil {
				st.kind = name.Name
				st.fn = fn
			}
		}
		stages = append([]pipelineStage{st}, stages...)
		if source == nil {
			if len(call.Args) == 0 {
				break
			}
			source = call.Args[0]
		}
		expr = source
	}
	return expr, stages
}

// pipelineCall returns the call at the head of expr and, for the pipe form,
// the expression feeding it. A direct call reports a nil source; its first
// argument is the source instead.
func pipelineCall(expr ast.Expression) (*ast.Call, ast.Expression) {
	switch e := expr.(type) {
	case *ast.BinaryOperator:
		if e.Op != ast.OperatorPipe {
			return nil, nil
		}
		if call, ok := e.Expr2.(*ast.Call); ok {
			return call, e.Expr1
		}
	case *ast.Call:
		if len(e.Args) == 2 {
			return e, nil
		}
	}
	return nil, nil
}

// literalClosureArg returns the stage's function argument when it is a literal
// closure with a body and exactly one parameter and result. Anything else —
// a variable holding a func, a method value — is not lowerable.
func literalClosureArg(call *ast.Call, piped bool) *ast.Func {
	idx := 0
	if !piped {
		idx = 1
	}
	if len(call.Args) != idx+1 {
		return nil
	}
	fn, ok := call.Args[idx].(*ast.Func)
	if !ok || fn.Body == nil || fn.Type == nil {
		return nil
	}
	if len(fn.Type.Parameters) != 1 || len(fn.Type.Result) != 1 {
		return nil
	}
	if fn.Type.Parameters[0].Ident == nil || fn.Type.Result[0].Type == nil {
		return nil
	}
	return fn
}

// reapplyStage rebuilds an unlowered stage over a new source, keeping the
// spelling the author used.
//
// The pipe form must stay a pipe. It is not cosmetic: the checker gives
// `x | f()` its own treatment, which is what lets a two-result helper such as
// sort_by — `(value, error)` — sit in a single-value context. Rewriting it as
// a direct call turns every such chain into "multiple-value f() in
// single-value context".
func reapplyStage(st pipelineStage, source ast.Expression) ast.Expression {
	if st.piped {
		return ast.NewBinaryOperator(st.callPos, ast.OperatorPipe, source, st.call)
	}
	args := make([]ast.Expression, 0, len(st.call.Args))
	args = append(args, source)
	args = append(args, st.call.Args[1:]...)
	return ast.NewCall(st.callPos, st.call.Func, args, st.call.IsVariadic)
}

// lowerChainToTemp lowers the lowerable prefix of expr into statements that
// leave the result in a fresh temporary, returning those statements and the
// expression to use in place of expr. Reports nil when nothing is lowerable.
//
// Every position that can hold a chain — a var initialiser, an assignment, a
// range head — goes through this, so which one an author picked never decides
// whether the chain is lowered.
func lowerChainToTemp(expr ast.Expression, comp *compilation) ([]ast.Node, ast.Expression) {
	source, stages := splitPipeline(expr)
	n := 0
	for n < len(stages) && stages[n].fn != nil {
		n++
	}
	if n == 0 {
		return nil, nil
	}
	tmp := ast.NewIdentifier(exprPos(expr), comp.generatePipelineName()+"t")
	nodes := buildLoweredPipeline(tmp, source, stages[:n], comp)
	rest := ast.Expression(tmp)
	for _, st := range stages[n:] {
		rest = reapplyStage(st, rest)
	}
	return nodes, rest
}

// exprPos returns a position for a synthetic node derived from expr.
func exprPos(expr ast.Expression) *ast.Position {
	if p := expr.Pos(); p != nil {
		return p
	}
	return &ast.Position{}
}
